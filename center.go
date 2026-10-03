//go:build center

package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- 中心端 HTTP 服务 ----------

// Center 聚合存储层（DB 或内存），端点分两组：
//   地图页（与 上游管理面板 同形）：
//     /                      全国延迟地图（ECharts）
//     /api/mapping.json      省级聚合延迟（按运营商）
//     /api/mapping_detail.json  省/运营商/城市/IP 全量明细
//     /api/province_history.json  省内各 IP 历史曲线
//   管理与节点：
//     /admin/                上游管理面板 管理面板（Vue3 SPA，admin-dist 构建产物）
//     /api/admin/<action>    管理面板全部 action（pwd=Token 鉴权）
//     /agent/heartbeat /agent/report /agent/config   节点协议
//     /api/nodes /api/history /api/targets(+POST)    管理 API
type Center struct {
	conf     CenterConf
	confPath string
	db       *CenterDB
	mem      *MemStore
	hub      *Hub // WS 长连接在线表（/agent/ws）
	lim      *failLimiter

	mu       sync.Mutex // 保护 conf（AdminPwd/Token 热更新读写同锁）
	pwdMu    sync.Mutex // 串行化改密码的"读-改-写"，避免并发改密码时内存与文件不一致
	lastSeen map[string]time.Time

	tgtMu     sync.Mutex // 保护目标表缓存
	tgtCache  map[string]struct{}
	tgtCacheAt time.Time

	cmdMu sync.Mutex              // 远程指令回执跟踪：每节点最近一条
	cmds  map[string]*cmdState
}

// cmdState 远程指令下发状态（三态：待确认/已完成/超时）
type cmdState struct {
	Action    string    `json:"action"`
	SentAt    time.Time `json:"sent_at"`
	Ver       string    `json:"ver,omitempty"`
	Done      bool      `json:"done"`
	DoneAt    time.Time `json:"done_at,omitempty"`
	Dead      bool      `json:"dead"`     // 超时未确认
	PrevBootTs int64    `json:"-"`        // 下发时的节点进程启动时刻（重启回执判据，不外传）
}

//go:embed frontend
//go:embed frontend/admin-dist/assets/_*
var frontendFS embed.FS

func runCenter(confPath, listenFlag, dbFlag string) {
	c := loadCenterConf(confPath, listenFlag, dbFlag)
	ctr := &Center{conf: c, confPath: confPath, lastSeen: map[string]time.Time{},
		lim: newFailLimiter(), cmds: map[string]*cmdState{}}

	if c.DB != "" {
		db, err := OpenCenterDB(c.DB)
		if err != nil {
			fatal("center: 数据库连接失败: %v", err)
		}
		ctr.db = db
		log.Printf("center: 已连接数据库")
	} else {
		ctr.mem = NewMemStore()
		log.Printf("center: 内存模式（无 DB，重启数据清空；正式部署请配置 DB）")
	}
	ctr.hub = NewHub(ctr)
	go ctr.lim.janitor()   // 定期回收限流记录，避免 map 无界增长
	go ctr.monitorLoop()   // 定期把明细体积/行数/压缩状态打进日志（唯一的自我观测）

	sub, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		fatal("center: 内嵌前端资源错误: %v", err)
	}
	staticServer := http.FileServer(http.FS(sub))

	mux := http.NewServeMux()
	// 节点协议（HTTP 回退路；鉴权全走 X-PingAtlas-Auth 头，URL 不再带 token）
	mux.HandleFunc("GET /agent/ws", ctr.hub.ServeWS) // WS 长连接（优先通道，质询-应答握手）
	mux.HandleFunc("POST /agent/heartbeat", ctr.handleHeartbeat)
	mux.HandleFunc("POST /agent/report", ctr.handleReport)
	mux.HandleFunc("POST /agent/config", ctr.handleConfig)
	// 一次性安装码兑换（install.sh 拿 code 换 token，token 从此不进 URL/日志）
	mux.HandleFunc("POST /agent/redeem", ctr.handleRedeem)
	// 节点登记/替换自己的 Ed25519 公钥（身份取自鉴权结果，不取请求体）
	mux.HandleFunc("POST /agent/pubkey", ctr.handlePubkey)
	// 一键安装：下载自身二进制 + 动态生成安装脚本
	mux.HandleFunc("GET /agent/download", ctr.handleDownload)
	mux.HandleFunc("GET /agent/install.sh", ctr.handleInstallScript)
	// 管理 API
	mux.HandleFunc("GET /api/nodes", ctr.handleNodes)
	mux.HandleFunc("GET /api/history", ctr.handleHistory)
	mux.HandleFunc("GET /api/targets", ctr.handleTargetsGet)
	mux.HandleFunc("POST /api/targets", ctr.handleTargetUpsert)
	// 管理面板 API（对齐上游管理面板的 action 集合）
	ctr.registerAdminAPI(mux)
	// 地图页 API（与 上游管理面板 同形）
	mux.HandleFunc("GET /api/mapping.json", ctr.handleMapping)
	mux.HandleFunc("GET /api/mapping_detail.json", ctr.handleMappingDetail)
	mux.HandleFunc("GET /api/province_history.json", ctr.handleProvinceHistory)
	mux.HandleFunc(matrixPath, ctr.handleMatrix) // 首页矩阵：目标×节点 + 运营商/地区汇总 + 节点卡片
	// 静态前端
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticServer))

	// 浏览器会默认请求 /favicon.ico（此前未注册，日志里一直有 404）
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		b, err := fs.ReadFile(sub, "favicon.ico")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/x-icon")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		b, _ := fs.ReadFile(sub, "index.html")
		_, _ = w.Write(b)
	})
	// 管理面板 SPA（admin-dist 构建产物 + history 路由 fallback）
	adminSub, err := fs.Sub(sub, "admin-dist")
	if err != nil {
		fatal("center: 内嵌 admin 资源错误: %v", err)
	}
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusMovedPermanently)
	})
	mux.Handle("GET /admin/", http.StripPrefix("/admin/", spaHandler(adminSub)))

	log.Printf("center: 监听 %s", c.Listen)
	if err := http.ListenAndServe(c.Listen, mux); err != nil {
		fatal("center: 监听失败: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (c *Center) touch(name string) {
	c.mu.Lock()
	c.lastSeen[name] = time.Now()
	c.mu.Unlock()
}

// nodeAliveByID 快路径：有活的长连接，或最近 2 分钟有内存心跳。不查库。
func (c *Center) nodeAliveByID(name string) bool {
	if c.hub != nil && c.hub.IsOnline(name) {
		return true
	}
	c.mu.Lock()
	t, ok := c.lastSeen[name]
	c.mu.Unlock()
	return ok && time.Since(t) < 2*time.Minute
}

// nodeAlive 用**调用方已经取到的** DB 行做兜底。
// 单独抽出来是因为 adminNodeStatus 已经握着 ListNodes 的结果：旧写法在循环里调 isOnline，
// 会为每个节点再查一次库（N 个节点 N 次 SELECT，DB 抖动时能把面板拖到 ctx 超时）。
func (c *Center) nodeAlive(ni NodeInfo) bool {
	if c.nodeAliveByID(ni.NodeID) {
		return true
	}
	return time.Since(ni.LastSeen) < 2*time.Minute
}

// isOnline 只有节点名时的判定（内存无记录且 DB 模式 → 回落查库一次）。
// center 重启后内存 lastSeen 是空的，缺这条回落会把所有节点都显示成离线，
// 直到下一次心跳到达（交接文档 §6.2 那条）。
func (c *Center) isOnline(name string) bool {
	if c.nodeAliveByID(name) {
		return true
	}
	if c.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if ni, err := c.db.GetNode(ctx, name); err == nil {
			return time.Since(ni.LastSeen) < 2*time.Minute
		}
	}
	return false
}

// decodeBody 解析 JSON 请求体，带 1MB 上限（D6：否则 json.Decoder 可被打爆）
func decodeBody(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "err": "bad json"})
		return false
	}
	return true
}

func (c *Center) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	// 与 handleReport 一致：先鉴权再解析 body
	name, ok := c.authHeader(r)
	if !ok {
		writeJSON(w, 403, map[string]interface{}{"ok": false, "err": "auth failed"})
		return
	}
	var req heartbeatReq
	if !decodeBody(w, r, &req) {
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	c.registerNode(name, ip, req.Version, req.Targets, req.ClockOffsetMs, req.BootTs)
	if req.Backlog > 0 {
		log.Printf("center: 节点 %s 积压 %d 行待补传", name, req.Backlog)
	}
	c.markCmdHeartbeat(name, req.Version, req.BootTs)
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

func (c *Center) handleReport(w http.ResponseWriter, r *http.Request) {
	// 先鉴权、再解析 body：未鉴权请求不应该让中心解码最多 1MB 的 JSON
	name, ok := c.authHeader(r)
	if !ok {
		writeJSON(w, 403, map[string]interface{}{"ok": false, "err": "auth failed"})
		return
	}
	var req reportReq
	if !decodeBody(w, r, &req) {
		return
	}
	// 与 WS 路同一个上限：HTTP 兜底路原先只有 1MB body 限制（约可塞 7 千行），
	// 比 WS 的 5000 行更宽 ⇒ 限制可被绕过；且超大批次会长时间占住落库路径。
	if len(req.Items) > maxReportItems {
		log.Printf("center: 节点 %s HTTP 单批行数超限 %d > %d，拒收（水位不动，数据留在节点）",
			name, len(req.Items), maxReportItems)
		writeJSON(w, 413, map[string]interface{}{"ok": false, "seq": req.Seq,
			"err": fmt.Sprintf("批次过大(%d 行，上限 %d)", len(req.Items), maxReportItems)})
		return
	}
	c.touch(name)
	rows, rejected, err := c.buildRows(name, req.Items)
	if err != nil {
		// fail-closed：目标表读不出来就不能确认这批数据合法，回失败让节点保留数据重推
		log.Printf("center: 目标校验失败(%s, %d 行): %v", name, len(req.Items), err)
		writeJSON(w, 503, map[string]interface{}{"ok": false, "seq": req.Seq, "err": err.Error()})
		return
	}
	n, err := c.insertRows(rows)
	if err != nil {
		log.Printf("center: 落库失败(%s, %d 行): %v", name, len(rows), err)
		writeJSON(w, 500, map[string]interface{}{"ok": false, "seq": req.Seq, "err": err.Error()})
		return
	}
	if rejected > 0 {
		log.Printf("center: 节点 %s 上报中拒收 %d 行（目标不在当前目标表）", name, rejected)
	}
	writeJSON(w, 200, reportResp{OK: true, Seq: req.Seq, Count: n,
		Rejected: rejected, ServerTs: time.Now().UnixMilli()})
}

func (c *Center) handleConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := c.authHeader(r); !ok {
		writeJSON(w, 403, map[string]interface{}{"ok": false, "err": "auth failed"})
		return
	}
	cfg, err := c.currentConfig()
	if err != nil {
		// 读表失败：明确报错而不是回空列表，否则节点会把「异常」当成「清空」而停摆
		writeJSON(w, 503, map[string]interface{}{"ok": false, "err": err.Error()})
		return
	}
	writeJSON(w, 200, cfg)
}

// handleRedeem 一次性安装码兑换：install.sh 拿 {name, code} 换 token。
// 30 分钟过期、**兑换成功即作废**（D2：否则 URL 泄露后 30 分钟内可无限换出 token）；
// 同 IP 20 次失败锁 30 分钟。
func (c *Center) handleRedeem(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string `json:"name"`
		Code   string `json:"code"`
		Pubkey string `json:"pubkey,omitempty"` // 新节点：兑换安装码时一并登记公钥
	}
	if !decodeBody(w, r, &req) || req.Name == "" || req.Code == "" {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "err": "bad json"})
		return
	}
	// 公钥格式合法才接受（非法就当作没带，避免把垃圾写进库）
	if req.Pubkey != "" && !validPubkeyHex(req.Pubkey) {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "err": "pubkey 必须是 32 字节 Ed25519 公钥的 hex"})
		return
	}
	key := "redeem:" + c.realIP(r)
	if !c.lim.allowed(key) {
		writeJSON(w, 429, map[string]interface{}{"ok": false, "err": "尝试过于频繁，请 30 分钟后再试"})
		return
	}
	if c.db == nil {
		writeJSON(w, 500, map[string]interface{}{"ok": false, "err": "安装码需要 DB 模式"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	inf, err := c.db.RedeemInstallCode(ctx, req.Name, req.Code, req.Pubkey)
	if err != nil || inf == "" {
		c.lim.fail(key, redeemMaxTry, bruteLockFor)
		log.Printf("center: 安装码兑换失败 name=%s ip=%s", req.Name, c.realIP(r))
		writeJSON(w, 403, map[string]interface{}{"ok": false, "err": "安装码无效/已过期/已使用"})
		return
	}
	c.lim.reset(key)
	log.Printf("center: 安装码兑换成功并作废 name=%s ip=%s", req.Name, c.realIP(r))
	writeJSON(w, 200, map[string]interface{}{"ok": true, "token": inf})
}

// handlePubkey 节点登记/替换自己的 Ed25519 公钥。
//
// 身份完全由请求头里的凭据决定（HMAC 会按"它声称的名字"查该名字的 token；签名则用
// 该名字已登记的公钥），并且**只允许写到那个通过校验的名字下** —— 名字取自鉴权结果，
// 绝不取请求体，否则持有合法 token 的节点就能给别人的名字换钥匙（越权）。
//
// 拒绝全局 token 身份：全局 token 不绑定节点名（对任何名字都成立），拿它来登记公钥
// 等于"谁都能给任何节点换钥匙"，必须挡掉。
func (c *Center) handlePubkey(w http.ResponseWriter, r *http.Request) {
	name, res, ok := c.authHeaderFull(r)
	if !ok {
		writeJSON(w, 403, map[string]interface{}{"ok": false, "err": "auth failed"})
		return
	}
	if res.UsedGlobalToken {
		writeJSON(w, 403, map[string]interface{}{"ok": false,
			"err": "全局 token 不能用于登记公钥（它不绑定节点名）；请使用该节点的独立 token"})
		return
	}
	var req struct {
		Name   string `json:"name"`
		Pubkey string `json:"pubkey"`
	}
	if !decodeBody(w, r, &req) || req.Pubkey == "" {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "err": "bad json"})
		return
	}
	if req.Name != "" && req.Name != name {
		writeJSON(w, 403, map[string]interface{}{"ok": false, "err": "请求体里的 name 与鉴权身份不一致"})
		return
	}
	if !validPubkeyHex(req.Pubkey) {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "err": "pubkey 必须是 32 字节 Ed25519 公钥的 hex"})
		return
	}
	fp := pubkeyFingerprintHex(req.Pubkey)
	// 替换成另一把密钥时留一条醒目日志（换钥匙是敏感操作，便于事后追溯）
	old := ""
	if c.db != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if o, err := c.db.GetNodePubkey(ctx, name); err == nil {
			old = o
		}
	} else if c.mem != nil {
		old = c.mem.GetNodePubkey(name)
	}
	if old != "" && old != req.Pubkey {
		log.Printf("center: 节点 %s 公钥被替换（旧指纹=%s 新指纹=%s）", name, pubkeyFingerprintHex(old), fp)
	}
	var err error
	if c.db != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		err = c.db.SetNodePubkey(ctx, name, req.Pubkey)
	} else {
		err = c.mem.SetNodePubkey(name, req.Pubkey)
	}
	if err != nil {
		writeJSON(w, 404, map[string]interface{}{"ok": false, "err": err.Error()})
		return
	}
	log.Printf("center: 节点 %s 登记公钥 指纹=%s", name, fp)
	writeJSON(w, 200, map[string]interface{}{"ok": true, "name": name, "fingerprint": fp, "alg": authAlgEd25519})
}

// handleDownload 把节点二进制发给安装脚本（center 可执行文件同目录下的 pingatlas-node）。
// 支持 ?arch=arm64 取 arm64 版本（文件名 pingatlas-node-arm64，需与主二进制放同目录）。
func (c *Center) handleDownload(w http.ResponseWriter, r *http.Request) {
	exe, err := os.Executable()
	if err != nil {
		http.Error(w, "binary unavailable", 500)
		return
	}
	dir := filepath.Dir(exe)
	nodeBin := filepath.Join(dir, "pingatlas-node")
	if r.URL.Query().Get("arch") == "arm64" {
		alt := filepath.Join(dir, "pingatlas-node-arm64")
		if _, err := os.Stat(alt); err == nil {
			nodeBin = alt
		} else {
			http.Error(w, "arm64 build not found next to center binary", 404)
			return
		}
	}
	if _, err := os.Stat(nodeBin); err != nil {
		http.Error(w, "node binary not found next to center binary", 500)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="pingatlas"`)
	http.ServeFile(w, r, nodeBin)
}

// handleInstallScript 动态生成一键安装脚本：
//   curl -fsSL "https://host/agent/install.sh?name=X&code=Y" | bash
// URL 只携带节点名与一次性安装码（30 分钟有效、首次上线作废），脚本运行时
// 先 POST /agent/redeem 兑换真 token 再写配置 —— token 不进 URL/nginx 日志/history。
// 中心地址按请求推断（nginx 反代时依赖 X-Forwarded-Proto，缺省 https）。
func (c *Center) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	code := r.URL.Query().Get("code")
	// 这两个值会被 %q 拼进一段让运维在 root 下执行的 bash 脚本，必须先白名单化：
	// Go 的 %q 只加双引号并转义 " 与 \，**双引号内的 $() 和反引号依然会展开**。
	// adminNodeAdd 生成 URL 时用的是同一套校验，这里不能只信上游。
	if !validNodeName(name) || !validInstallCode(code) {
		http.Error(w, "invalid name/code", 400)
		return
	}
	// scheme 与 Host 同样客户端可控，走白名单 helper
	base, berr := publicBase(r)
	if berr != nil {
		http.Error(w, berr.Error(), 400)
		return
	}
	center := base

	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	b.WriteString("set -e\n")
	b.WriteString(fmt.Sprintf("CENTER=%q\n", center))
	b.WriteString(fmt.Sprintf("NAME=%q\n", name))
	b.WriteString(fmt.Sprintf("CODE=%q\n", code))
	b.WriteString(`
echo "== pingatlas 节点安装：$NAME =="

# ---------- 环境探测 ----------
have() { command -v "$1" >/dev/null 2>&1; }
fetch() { # fetch <url> <out>：curl 优先，wget 兜底
  if have curl; then curl -fsSL "$1" -o "$2"
  elif have wget; then wget -qO "$2" "$1"
  else return 127; fi
}

[ "$(id -u)" = 0 ] || { echo "请用 root 运行"; exit 1; }
[ "$(uname -s)" = "Linux" ] || { echo "仅支持 Linux，当前 $(uname -s)"; exit 1; }

# 架构：x86_64 与 aarch64 都支持，按架构向中心要对应二进制
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  GOARCH=amd64 ;;
  aarch64) GOARCH=arm64 ;;
  *) echo "不支持的架构：$ARCH（支持 x86_64 / aarch64）"; exit 1 ;;
esac

# 下载工具：curl/wget 都没有时，按发行版包管理器自动装 curl
if ! have curl && ! have wget; then
  echo "-- 未找到 curl/wget，尝试用包管理器安装 curl ..."
  if have apt-get; then apt-get update -y && apt-get install -y curl ca-certificates
  elif have dnf; then dnf install -y curl ca-certificates
  elif have yum; then yum install -y curl ca-certificates
  elif have apk; then apk add --no-cache curl ca-certificates
  elif have pacman; then pacman -Sy --noconfirm curl ca-certificates
  elif have zypper; then zypper --non-interactive install curl ca-certificates
  else echo "无法识别包管理器，请手动安装 curl 后重试"; exit 1; fi
fi

# init 系统探测：systemd 或 OpenRC（Alpine/Gentoo），其余明确报错
INIT=unknown
if [ -d /run/systemd/system ] && have systemctl; then INIT=systemd
elif have rc-service; then INIT=openrc
fi
[ "$INIT" != "unknown" ] || { echo "未识别的 init 系统（非 systemd / OpenRC），无法自动安装服务"; exit 1; }
echo "-- 环境：Linux $ARCH ($GOARCH) / init=$INIT / 下载器=$(have curl && echo curl || echo wget)"

# ---------- 下载二进制与写配置 ----------
mkdir -p /etc/pingatlas /var/lib/pingatlas

# 幂等：重复执行本脚本时备份旧配置（旧实现会直接覆盖 config.json 与同名 unit）
if [ -f /etc/pingatlas/config.json ]; then
  cp -a /etc/pingatlas/config.json "/etc/pingatlas/config.json.bak.$(date +%Y%m%d%H%M%S)"
  echo "-- 旧配置已备份到 /etc/pingatlas/config.json.bak.*"
fi

# 失败回滚：只在"已经停掉旧服务"之后脚本异常退出时，才把原服务拉起来。
# 顺序很关键 —— 先停服务再下载的话，下载 404 或安装码失效会让在线节点直接停摆。
ROLLBACK=0
rollback() {
  if [ "$ROLLBACK" = "1" ]; then
    echo "-- 安装未完成，恢复原有服务 ..."
    if [ "$INIT" = systemd ]; then systemctl start pingatlas-node 2>/dev/null || true
    else rc-service pingatlas-node start 2>/dev/null || true; fi
  fi
}
trap rollback EXIT

echo "-- 下载二进制（约 10-17MB）..."
fetch "$CENTER/agent/download?arch=$GOARCH" /usr/local/bin/pingatlas
chmod 755 /usr/local/bin/pingatlas

# 兑换 Token：一次性安装码 -> 真 token（token 不出现在任何 URL/日志/history）
echo "-- 用一次性安装码兑换 Token ..."
REDEEM_BODY='{"name":"'"$NAME"'","code":"'"$CODE"'"}'
if have curl; then
  RESP=$(curl -fsSL -X POST -H "Content-Type: application/json" --data "$REDEEM_BODY" "$CENTER/agent/redeem" 2>/dev/null) || RESP=""
elif have wget; then
  RESP=$(wget -qO- --header="Content-Type: application/json" --post-data="$REDEEM_BODY" "$CENTER/agent/redeem" 2>/dev/null) || RESP=""
fi
TOKEN=$(printf '%s' "$RESP" | sed -n 's/.*"token"[: ]*"\([^"]*\)".*/\1/p')
[ -n "$TOKEN" ] || { echo "安装码兑换失败（无效/已过期/已使用）：$RESP"; exit 1; }

# 下载与安装码兑换都已成功，此刻才停旧服务（停早了会让失败安装把在线节点拖停）
if [ "$INIT" = systemd ] && have systemctl; then
  systemctl stop pingatlas-node 2>/dev/null || true
  ROLLBACK=1
elif have rc-service; then
  rc-service pingatlas-node stop 2>/dev/null || true
  ROLLBACK=1
fi

echo "-- 写配置 /etc/pingatlas/config.json"
cat > /etc/pingatlas/config.json <<JSON
{
  "Name": "$NAME",
  "Center": { "Endpoint": "$CENTER", "Token": "$TOKEN" },
  "DBPath": "/var/lib/pingatlas/pingatlas_node.db"
}
JSON
chmod 600 /etc/pingatlas/config.json

# ---------- 时间同步：向阿里云 NTP 对时（logs 时间戳的可靠性前提） ----------
# 探测数据以节点本地时钟打 logtime，幂等键也含 logtime；时钟大幅漂移会让数据错位。
# timesyncd / chrony 自身就是周期性重同步，装上即持续校时，无需另写定时任务。
echo "-- 配置 NTP 对时（ntp.aliyun.com）..."
if [ -d /run/systemd/system ] && systemctl list-unit-files systemd-timesyncd.service >/dev/null 2>&1; then
  mkdir -p /etc/systemd
  printf '[Time]\nNTP=ntp.aliyun.com\nFallbackNTP=ntp1.aliyun.com\n' > /etc/systemd/timesyncd.conf
  systemctl enable --now systemd-timesyncd 2>/dev/null || true
elif have chronyd; then
  :
else
  if have apt-get; then apt-get install -y chrony >/dev/null 2>&1 || true
  elif have dnf; then dnf install -y chrony >/dev/null 2>&1 || true
  elif have yum; then yum install -y chrony >/dev/null 2>&1 || true
  elif have apk; then apk add --no-cache chrony >/dev/null 2>&1 || true
  elif have pacman; then pacman -Sy --noconfirm chrony >/dev/null 2>&1 || true
  elif have zypper; then zypper --non-interactive install chrony >/dev/null 2>&1 || true
  fi
fi
if have chronyd; then
  printf 'server ntp.aliyun.com iburst\nserver ntp1.aliyun.com iburst\n' > /etc/chrony.conf 2>/dev/null || true
  if [ "$INIT" = systemd ]; then systemctl enable --now chronyd 2>/dev/null || true
  else rc-service chronyd start 2>/dev/null || true; fi
fi
# Alpine/OpenRC 无 timesyncd/chrony 时退回 busybox ntpd 一次性对时
if ! have chronyd && [ "$INIT" = openrc ]; then
  (ntpd -q -s ntp.aliyun.com 2>/dev/null || busybox ntpd -q -s ntp.aliyun.com 2>/dev/null) || true
fi
echo "   当前时间：$(date '+%F %T %Z')"

# ---------- 按 init 系统装服务 ----------
if [ "$INIT" = systemd ]; then
  echo "-- 写 systemd 服务"
  cat > /etc/systemd/system/pingatlas-node.service <<'UNIT'
[Unit]
Description=pingatlas node
After=network-online.target
Wants=network-online.target

[Service]
WorkingDirectory=/var/lib/pingatlas
ExecStart=/usr/local/bin/pingatlas --conf /etc/pingatlas/config.json
Restart=always
RestartSec=3
AmbientCapabilities=CAP_NET_RAW
NoNewPrivileges=true
MemoryMax=300M

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable --now pingatlas-node
  ROLLBACK=0
  sleep 1
  systemctl --no-pager --lines=5 status pingatlas-node || true
  echo "== 完成：节点 $NAME 已启动，日志 journalctl -u pingatlas-node -f =="
else
  echo "-- 写 OpenRC 服务（Alpine/Gentoo）"
  cat > /etc/init.d/pingatlas-node <<'OPENRC'
#!/sbin/openrc-run
name="pingatlas node"
command="/usr/local/bin/pingatlas"
command_args="--conf /etc/pingatlas/config.json"
command_user="root:root"
directory="/var/lib/pingatlas"
supervisor=supervise-daemon
pidfile="/run/pingatlas-node.pid"
stdout_log="/var/log/pingatlas-node.log"
stderr_log="/var/log/pingatlas-node.log"

depend() {
  need net
}
OPENRC
  chmod 755 /etc/init.d/pingatlas-node
  # OpenRC 分支的 stdout/stderr 直接落文件，没有 logrotate 会无限增长
  cat > /etc/logrotate.d/pingatlas-node <<'LOGROTATE'
/var/log/pingatlas-node.log {
  weekly
  rotate 4
  compress
  missingok
  notifempty
  copytruncate
}
LOGROTATE
  rc-update add pingatlas-node default
  rc-service pingatlas-node start
  ROLLBACK=0
  sleep 1
  rc-service pingatlas-node status || true
  echo "== 完成：节点 $NAME 已启动，日志 tail -f /var/log/pingatlas-node.log =="
fi
`)
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// handleNodes 节点列表。含节点 IP/版本等内部信息，且面板自己走 admin 的 node/status，
// 所以这里要求管理鉴权（旧实现完全公开）。
func (c *Center) handleNodes(w http.ResponseWriter, r *http.Request) {
	if !c.requireAdmin(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var nodes []NodeInfo
	if c.db != nil {
		nodes, _ = c.db.ListNodes(ctx)
	} else {
		nodes = c.mem.ListNodes()
	}
	c.mu.Lock()
	now := time.Now()
	type nodeOut struct {
		NodeInfo
		Alive       bool   `json:"alive"`
		ClockState  string `json:"clock_state"` // ok | warn(>30s) | bad(>5min)
		ClockOffset int64  `json:"clock_offset_ms"`
	}
	out := make([]nodeOut, 0, len(nodes))
	for _, ni := range nodes {
		alive := now.Sub(ni.LastSeen) < 2*time.Minute
		if t, ok := c.lastSeen[ni.NodeID]; ok {
			alive = now.Sub(t) < 2*time.Minute
		}
		out = append(out, nodeOut{NodeInfo: ni, Alive: alive,
			ClockState: clockState(ni.ClockOffsetMs), ClockOffset: ni.ClockOffsetMs})
	}
	c.mu.Unlock()
	writeJSON(w, 200, map[string]interface{}{"ok": true, "nodes": out})
}

// clockState 时钟偏移分级：>30s 标黄提醒查 NTP，>5 分钟标红
func clockState(offsetMs int64) string {
	a := offsetMs
	if a < 0 {
		a = -a
	}
	switch {
	case a > 5*60_000:
		return "bad"
	case a > 30_000:
		return "warn"
	default:
		return "ok"
	}
}

// handleHistory 原始明细查询（最多 5000 行，含 node_id/target 维度），
// 面板前端不使用它 ⇒ 要求管理鉴权。
func (c *Center) handleHistory(w http.ResponseWriter, r *http.Request) {
	if !c.requireAdmin(w, r) {
		return
	}
	q := r.URL.Query()
	node := q.Get("node")
	target := q.Get("target")
	limit := atoiDefault(q.Get("limit"), 200)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var rows []map[string]interface{}
	var err error
	if c.db != nil {
		rows, err = c.db.QueryHistory(ctx, node, target, limit)
		if err != nil {
			writeJSON(w, 500, map[string]interface{}{"ok": false, "err": err.Error()})
			return
		}
	} else {
		rows = c.mem.QueryHistory(node, target, limit)
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true, "rows": rows})
}

func (c *Center) handleTargetsGet(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var targets []TargetRow
	if c.db != nil {
		targets, _ = c.db.ListTargets(ctx)
	} else {
		targets = c.mem.ListTargets()
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true, "targets": targets})
}

// findTarget 按 alias（其次按当前 IP）找一条已有目标。
// 用途：POST /api/targets 的请求体常常只带 alias+ip（README 就是这么写的），
// 必须把库里已有的地图维度合并进来，不能直接覆盖。
func (c *Center) findTarget(ctx context.Context, aliasOrIP string) (TargetRow, bool) {
	var targets []TargetRow
	if c.db != nil {
		list, err := c.db.ListTargets(ctx)
		if err != nil {
			return TargetRow{}, false
		}
		targets = list
	} else {
		targets = c.mem.ListTargets()
	}
	for _, t := range targets {
		if t.Alias == aliasOrIP {
			return t, true
		}
	}
	for _, t := range targets {
		if t.IP == aliasOrIP {
			return t, true
		}
	}
	return TargetRow{}, false
}

// handleTargetUpsert 添加/更新目标：alias 不变只改 IP（历史不断档），带地图维度
//
// P1-1 修复：此入口原先**完全没有鉴权**，且漏掉了目标变更后的三个必要动作。
// 现补齐：管理密码校验 + 版本号自增 + 缓存失效 + IP 唯一性检查。
// 缺任何一项的后果：
//   - 无鉴权 ⇒ 任何人都能改目标表（公网 443 暴露时是完整写权限漏洞）
//   - 不 bump ⇒ 节点侧 applyRemoteConfig 判「版本未变」直接忽略推送，**静默失败**
//   - 不失效缓存 ⇒ 5 秒内新目标被 buildRows 当非法行拒收，节点上报被吞
//   - 不查唯一性 ⇒ 绕过 A4，同一 IP 挂在两个 alias 下，历史劈成两半
//
// P0 修复（第五件事）：**合并已有维度**。UpsertTarget 的 SQL 是
// `province=EXCLUDED.province, city=..., telecom=...`，请求体没带这三个字段时
// 会被写成空串 ⇒ 目标立刻从 /api/mapping*.json 与省级曲线上消失（这两处都跳过
// province/telecom 为空的目标），而接口返回 200、无任何日志；面板又没有「编辑维度」
// 动作，唯一出路是删掉重加（alias 变成 IP，历史永久劈裂）。
// 语义：请求里**显式给了**的字段为准，没给的沿用库里的旧值。
func (c *Center) handleTargetUpsert(w http.ResponseWriter, r *http.Request) {
	ap := c.adminPwd()
	if ap == "" {
		writeJSON(w, 500, map[string]interface{}{"ok": false, "err": "未配置管理密码，拒绝写入"})
		return
	}
	pwd := r.Header.Get("X-Admin-Pwd")
	if pwd == "" {
		pwd = r.URL.Query().Get("pwd")
	}
	if pwd == "" || subtle.ConstantTimeCompare([]byte(pwd), []byte(ap)) != 1 {
		writeJSON(w, 401, map[string]interface{}{"ok": false, "err": "unauthorized"})
		return
	}
	var req TargetRow
	if !decodeBody(w, r, &req) {
		return
	}
	req.Alias = strings.TrimSpace(req.Alias)
	req.IP = strings.TrimSpace(req.IP)
	if req.Alias == "" || req.IP == "" {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "err": "需要 alias 和 ip"})
		return
	}
	if net.ParseIP(req.IP) == nil || net.ParseIP(req.IP).To4() == nil {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "err": "IP 格式无效（需 IPv4）"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	// A4：同一 IP 不能被两个 alias 占用（excluding 自身，upsert 同一 alias 应放行）
	var holder string
	var inUse bool
	var err error
	if c.db != nil {
		holder, inUse, err = c.db.IPInUse(ctx, req.IP, req.Alias)
	} else {
		holder, inUse = c.mem.IPInUse(req.IP, req.Alias)
	}
	if err != nil {
		writeJSON(w, 500, map[string]interface{}{"ok": false, "err": err.Error()})
		return
	}
	if inUse {
		writeJSON(w, 400, map[string]interface{}{"ok": false,
			"err": fmt.Sprintf("IP %s 已在目标列表中（归属 %s）", req.IP, holder)})
		return
	}
	// P0：合并已有维度，避免「只改 IP」把 province/city/telecom 抹成空串。
	if prev, ok := c.findTarget(ctx, req.Alias); ok {
		if req.Province == "" {
			req.Province = prev.Province
		}
		if req.City == "" {
			req.City = prev.City
		}
		if req.Telecom == "" {
			req.Telecom = prev.Telecom
		}
	}
	if c.db != nil {
		if err := c.db.UpsertTarget(ctx, req); err != nil {
			// IP 冲突是业务错误（400），只有真正的数据库故障才是 500
			code := http.StatusInternalServerError
			if errors.Is(err, ErrIPInUse) {
				code = http.StatusBadRequest
			}
			writeJSON(w, code, map[string]interface{}{"ok": false, "err": err.Error()})
			return
		}
	} else {
		c.mem.UpsertTarget(req)
	}
	c.bumpTargetsVersion(ctx)      // 版本号自增，否则节点判「未变化」忽略推送
	c.invalidateTargetCache()      // 立刻失效，否则新目标 5 秒内被当成非法行拒收
	c.hub.PushConfig()             // 目标表已变，向在线节点秒推
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

// ---------- 地图页 API（与 上游管理面板 逐字段同形）----------

// knownNodeIDs 当前注册的节点名（可选 ?node= 的校验依据，也回给前端做下拉）。
// 只返回"前台可见"的节点：隐藏的节点不应出现在首页下拉里，用 ?node= 指定它也会被判为未知。
func (c *Center) knownNodeIDs(ctx context.Context) []string {
	var nodes []NodeInfo
	if c.db != nil {
		list, err := c.db.ListNodes(ctx)
		if err != nil {
			return nil
		}
		nodes = list
	} else {
		nodes = c.mem.ListNodes()
	}
	nodes = visibleNodes(nodes)
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.NodeID)
	}
	sort.Strings(ids)
	return ids
}

// snapshotNode 解析 ?node= 参数：空 = 全网最优口径；非空必须是已注册节点（避免拼错后
// 静默显示成空地图）。返回 (节点名, 节点列表, error)。
func (c *Center) snapshotNode(ctx context.Context, r *http.Request) (string, []string, error) {
	ids := c.knownNodeIDs(ctx)
	node := strings.TrimSpace(r.URL.Query().Get("node"))
	if node == "" {
		return "", ids, nil
	}
	for _, id := range ids {
		if id == node {
			return node, ids, nil
		}
	}
	return "", ids, fmt.Errorf("未知节点 %q", node)
}

// mappingInputs 统一取最新窗口快照 + 目标维度。nodeID 为空 = 全网最优，非空 = 单节点。
func (c *Center) mappingInputs(ctx context.Context, nodeID string) ([]TargetRow, map[string]SnapRow, time.Time, error) {
	var targets []TargetRow
	var snaps map[string]SnapRow
	var err error
	if c.db != nil {
		targets, err = c.db.ListTargets(ctx)
		if err != nil {
			return nil, nil, time.Time{}, err
		}
		snaps, err = c.db.LatestSnapshot(ctx, 5*time.Minute, nodeID)
		if err != nil {
			return nil, nil, time.Time{}, err
		}
	} else {
		targets = c.mem.ListTargets()
		snaps = c.mem.LatestSnapshot(5*time.Minute, nodeID)
	}
	var newest time.Time
	for _, s := range snaps {
		if s.Logtime.After(newest) {
			newest = s.Logtime
		}
	}
	return targets, snaps, newest, nil
}

// handleMapping 输出 {text, subtext, scope, node, nodes, avgdelay:{ctcc:...,cucc:...,cmcc:...}}
// 省级延迟 = 该省该运营商下所有目标"当前显示值"的最小值（与原版"取城市最小值"一致）；
// 目标无数据/超时(avgdelay<=0)不参与最小值竞争。
//
// 口径由 ?node= 决定：
//   不传        → scope=best：每个目标取"全网最优"（多节点并列时取更优值）
//   ?node=xxx   → scope=node：只看该节点（单节点视角）
// 两种口径的数值会有明显差异，所以响应里显式回 scope/node/nodes，前端据此标注。
func (c *Center) handleMapping(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	node, ids, nerr := c.snapshotNode(ctx, r)
	if nerr != nil {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "err": nerr.Error()})
		return
	}
	targets, snaps, newest, err := c.mappingInputs(ctx, node)
	if err != nil {
		writeJSON(w, 500, map[string]interface{}{"ok": false, "err": err.Error()})
		return
	}
	type pv struct {
		name  string
		value float64
	}
	best := map[string]*pv{} // prov+tel -> best
	for _, t := range targets {
		if t.Province == "" || t.Telecom == "" {
			continue
		}
		s, ok := snaps[t.Alias]
		if !ok || s.Avg <= 0 || s.Avg >= 2000 {
			continue
		}
		key := t.Province + "|" + t.Telecom
		if cur, ok := best[key]; !ok || s.Avg < cur.value {
			best[key] = &pv{name: t.Province, value: round2(s.Avg)}
		}
	}
	// 按运营商分组组装
	byTel := map[string][]map[string]interface{}{"ctcc": {}, "cucc": {}, "cmcc": {}}
	for key, p := range best {
		var tel string
		for _, cand := range []string{"ctcc", "cucc", "cmcc"} {
			if len(key) > len(cand)+1 && key[len(key)-len(cand)-1:] == "|"+cand {
				tel = cand
			}
		}
		if tel == "" {
			continue
		}
		byTel[tel] = append(byTel[tel], map[string]interface{}{"name": p.name, "value": p.value})
	}
	scope := "best"
	if node != "" {
		scope = "node"
	}
	writeJSON(w, 200, map[string]interface{}{
		"text":     "PingAtlas 全国延迟监控",
		"subtext":  "最后检测 " + newest.Format("2006-01-02 15:04"),
		"scope":    scope, // best=全网最优 / node=指定节点
		"node":     node,  // scope=node 时为节点名
		"nodes":    ids,   // 供前端做节点下拉
		"avgdelay": byTel,
	})
}

// handleMappingDetail 输出 {省:{tel:{城市:[{ip,delay,loss}]}}}
// 与 handleMapping 同一份快照、同一 ?node= 口径（保证地图数字与明细数字一致）。
func (c *Center) handleMappingDetail(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	node, _, nerr := c.snapshotNode(ctx, r)
	if nerr != nil {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "err": nerr.Error()})
		return
	}
	targets, snaps, _, err := c.mappingInputs(ctx, node)
	if err != nil {
		writeJSON(w, 500, map[string]interface{}{"ok": false, "err": err.Error()})
		return
	}
	out := map[string]interface{}{}
	for _, t := range targets {
		if t.Province == "" || t.Telecom == "" {
			continue
		}
		// -1 = 该目标在窗口内没有任何数据，与"超时"（20 个包一个没回，avgdelay=0）区分开。
		// 旧实现在无快照时补 delay=0/loss=0，前端把它当"超时"计入"坏"，
		// 于是刚加进来的目标还没探测一次就把面板标红。
		delay := -1.0
		loss := -1.0
		if s, ok := snaps[t.Alias]; ok {
			delay = s.Avg
			loss = 0
			if s.Send > 0 {
				loss = round2(float64(s.Loss) * 100 / float64(s.Send))
			}
		}
		prov, ok := out[t.Province].(map[string]interface{})
		if !ok {
			prov = map[string]interface{}{}
			out[t.Province] = prov
		}
		telMap, ok := prov[t.Telecom].(map[string]interface{})
		if !ok {
			telMap = map[string]interface{}{}
			prov[t.Telecom] = telMap
		}
		city := t.City
		if city == "" {
			city = "-"
		}
		list, ok := telMap[city].([]map[string]interface{})
		if !ok {
			list = []map[string]interface{}{}
		}
		list = append(list, map[string]interface{}{"ip": t.IP, "delay": delay, "loss": loss})
		telMap[city] = list
	}
	writeJSON(w, 200, out)
}

// handleProvinceHistory 输出 {ips:[{ip,telecom,city,history,loss,times}]}
func (c *Center) handleProvinceHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	province := q.Get("province")
	hours := atoiDefault(q.Get("hours"), 24)
	if province == "" {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "err": "需要 province"})
		return
	}
	if hours <= 0 || hours > 168 {
		hours = 24
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	// 与地图同一口径：?node= 指定单节点，不传则混合所有节点（按时间桶平均）
	node, _, nerr := c.snapshotNode(ctx, r)
	if nerr != nil {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "err": nerr.Error()})
		return
	}
	var ips []HistIP
	var err error
	if c.db != nil {
		ips, err = c.db.ProvinceHistory(ctx, province, hours, node)
		if err != nil {
			writeJSON(w, 500, map[string]interface{}{"ok": false, "err": err.Error()})
			return
		}
	} else {
		ips = c.mem.ProvinceHistory(province, hours, node)
	}
	writeJSON(w, 200, map[string]interface{}{"ips": ips, "node": node})
}

// monitorLoop 每 6 小时把「明细表体积 / 行数 / 压缩状态」写进日志。
// 加它的理由：压缩是这套系统唯一的"防磁盘写满"手段，而它的失败路径原本只打一行
// 普通日志、没有任何观测手段 —— 交接文档里 P0 的根因正是"无人观测的慢故障"。
func (c *Center) monitorLoop() {
	for {
		time.Sleep(6 * time.Hour)
		if c.db == nil {
			continue // 内存模式没有可观测的存储
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		c.db.LogStorageReport(ctx)
		cancel()
	}
}

func atoiDefault(s string, def int) int {
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return def
		}
		n = n*10 + int(ch-'0')
		if n > 100000 {
			return def
		}
	}
	if s == "" {
		return def
	}
	return n
}
