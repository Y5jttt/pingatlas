//go:build center

package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// adminVersion 管理面板展示版本号（App.vue 侧边栏 v{{ ver }}）
const adminVersion = "2.0.0"

// unitCenter / unitNode systemd 单元名（logs / restart / node restart 用）
const (
	unitCenter = "pingatlas-center.service"
	unitNode   = "pingatlas-node.service"
)

// registerAdminAPI 挂载上游管理面板 全部 /api/admin action。
// 鉴权：X-Admin-Pwd 请求头（custom.js fetch 拦截自动迁移 ?pwd=；兼容 query 兜底）
// 与 AdminPwd 比对 —— 管理密码与节点 token 彻底分离，改密码不再牵连节点鉴权。
// 防爆破：按真实 IP（X-Forwarded-For）5 次失败锁 30 分钟。
func (c *Center) registerAdminAPI(mux *http.ServeMux) {
	mux.HandleFunc("/api/admin/", c.handleAdmin)
}

func (c *Center) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if !c.requireAdmin(w, r) {
		return
	}
	action := strings.TrimPrefix(r.URL.Path, "/api/admin/")
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// GET 类
	switch action {
	case "stats":
		c.adminStats(ctx, w)
		return
	case "targets":
		c.adminTargets(ctx, w)
		return
	case "targets/history":
		c.adminTargetHistory(ctx, w, r)
		return
	case "central/status":
		writeJSON(w, 200, map[string]interface{}{"url": "", "last_check": ""})
		return
	case "extraconfig":
		writeJSON(w, 200, map[string]interface{}{"keepRaw": false, "rawPath": ""})
		return
	case "config":
		port, _ := strconv.Atoi(portOf(c.conf.Listen))
		writeJSON(w, 200, map[string]interface{}{
			"Port": port, "Name": "pingatlas", "Ver": adminVersion,
			"Base":       map[string]interface{}{"Archive": 0, "Timeout": 3},
			"Alert":      map[string]interface{}{},
			"Authiplist": "",
		})
		return
	case "node/status":
		c.adminNodeStatus(ctx, w)
		return
	case "geo/options":
		c.adminGeoOptions(ctx, w)
		return
	case "nodecompare":
		c.adminNodeCompare(ctx, w, r)
		return
	case "alerts":
		q := r.URL.Query()
		writeJSON(w, 200, map[string]interface{}{
			"alerts": []interface{}{}, "total": 0,
			"hours": atoiDefault(q.Get("hours"), 48), "limit": atoiDefault(q.Get("limit"), 300),
		})
		return
	case "logs":
		c.adminLogs(w, r)
		return
	}

	// POST 类
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]interface{}{"error": "method not allowed"})
		return
	}
	var body map[string]interface{}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // D6：请求体上限 1MB
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch action {
	case "targets/add":
		c.adminTargetAdd(ctx, w, body)
	case "targets/del":
		c.adminTargetDel(ctx, w, body)
	case "targets/replace":
		c.adminTargetReplace(ctx, w, body)
	case "scan/csegment":
		c.adminScanCSegment(ctx, w, body)
	case "tool/ping":
		c.adminToolPing(w, body)
	case "node/sync":
		writeJSON(w, 200, map[string]interface{}{"message": "pingatlas 节点每 60 秒自动热加载配置，无需手动同步"})
	case "node/add":
		c.adminNodeAdd(ctx, w, r, body)
	case "node/key/reset":
		c.adminNodeKeyReset(ctx, w, body)
	case "node/edit":
		c.adminNodeEdit(ctx, w, body)
	case "node/update":
		c.adminNodeUpdate(ctx, w, body)
	case "node/del":
		c.adminNodeDel(ctx, w, body)
	case "node/restart":
		c.adminNodeRestart(ctx, w, body)
	case "node/cmdstatus":
		c.adminCmdStatus(ctx, w, body)
	case "central/seturl":
		writeJSON(w, 200, map[string]interface{}{"message": "pingatlas 为去中心化架构，无需中央服务器"})
	case "central/check":
		writeJSON(w, 200, map[string]interface{}{"has_changes": false})
	case "central/apply":
		writeJSON(w, 200, map[string]interface{}{"message": "pingatlas 无需中央同步"})
	case "extraconfig/save":
		writeJSON(w, 200, map[string]interface{}{})
	case "config/save":
		c.adminConfigSave(ctx, w, body)
	case "mailtest":
		writeJSON(w, 200, map[string]interface{}{"error": "pingatlas 暂未实现邮件告警"})
	case "transfer":
		writeJSON(w, 200, map[string]interface{}{"error": "pingatlas 明细永久保留于 TimescaleDB（7 天后自动压缩），无需归档迁移"})
	case "restart":
		c.adminRestart(w)
	default:
		writeJSON(w, 404, map[string]interface{}{"error": "unknown action: " + action})
	}
}

// ---------- 存储通用 helper ----------

// requireAdmin 统一的管理鉴权 + 防爆破（/api/admin/* 与需要管理权限的读接口共用）。
// 返回 false 表示已经写过响应，调用方直接 return。
// 限流键用 realIP：直连对端不是可信代理时不采信 X-Forwarded-For，伪造 XFF 无法换新锁。
func (c *Center) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	key := "admin:" + c.realIP(r)
	if !c.lim.allowed(key) {
		writeJSON(w, 429, map[string]interface{}{"error": "失败次数过多，已锁定 30 分钟"})
		return false
	}
	pwd := r.Header.Get("X-Admin-Pwd")
	if pwd == "" {
		pwd = r.URL.Query().Get("pwd") // 兼容未改造的调用方（旧版 SPA 会拼 ?pwd=）
	}
	ap := c.adminPwd()
	if pwd == "" || ap == "" || subtle.ConstantTimeCompare([]byte(pwd), []byte(ap)) != 1 {
		c.lim.fail(key, bruteMaxFails, bruteLockFor)
		writeJSON(w, 401, map[string]interface{}{"error": "unauthorized"})
		return false
	}
	c.lim.reset(key)
	return true
}

// validNodeName 节点名白名单：字母/数字/-/_，最长 64。
// 它同时是一键安装脚本里的 shell 变量值，必须严格（见 publicBase 的说明）。
func validNodeName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, ch := range name {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' {
			continue
		}
		return false
	}
	return true
}

// validInstallCode 一次性安装码：十六进制串。
func validInstallCode(code string) bool {
	if code == "" || len(code) > 64 {
		return false
	}
	for _, ch := range code {
		if ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' || ch >= 'A' && ch <= 'F' {
			continue
		}
		return false
	}
	return true
}

// validHost 只接受 hostname[:port] 或 [IPv6][:port]，
// 拒绝换行/引号/`$`/反引号等一切 shell 元字符。
func validHost(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	hostPart, portPart := h, ""
	bracketed := strings.HasPrefix(h, "[")
	if bracketed {
		end := strings.Index(h, "]")
		if end < 0 {
			return false
		}
		hostPart = h[1:end]
		switch rest := h[end+1:]; {
		case rest == "":
		case strings.HasPrefix(rest, ":"):
			portPart = rest[1:]
		default:
			return false
		}
	} else if i := strings.LastIndex(h, ":"); i >= 0 && strings.Count(h, ":") == 1 {
		hostPart, portPart = h[:i], h[i+1:]
	}
	if portPart != "" {
		if n, err := strconv.Atoi(portPart); err != nil || n <= 0 || n > 65535 {
			return false
		}
	}
	if hostPart == "" {
		return false
	}
	if net.ParseIP(hostPart) != nil {
		// 裸 IPv6（不在方括号里）拼出的 URL 有歧义（分不清最后一段是不是端口），只接受带括号的
		if strings.Contains(hostPart, ":") && !bracketed {
			return false
		}
		return true
	}
	if bracketed {
		return false // 方括号只能包 IPv6 字面量
	}
	for _, ch := range hostPart {
		if ch == '.' || ch == '-' || ch == '_' || (ch >= '0' && ch <= '9') ||
			(ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') {
			continue
		}
		return false
	}
	return true
}

// publicBase 由请求推导对外 base URL（一键安装命令、install.sh 里的 CENTER）。
//
// scheme 与 Host 都是客户端可控的头部，而它们的下游是「让运维在 root 下 bash 一段
// 命令」：Go 的 %q 只会加双引号并转义 " 与 \，**挡不住双引号内的 $() 与反引号**。
// 所以这里必须先白名单化，再由调用方拼进脚本。
func publicBase(r *http.Request) (string, error) {
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme != "https" && scheme != "http" {
		scheme = "https" // 生产入口恒为 443/TLS；非法值一律退回默认
	}
	if !validHost(r.Host) {
		return "", fmt.Errorf("非法 Host 头: %q", r.Host)
	}
	return scheme + "://" + r.Host, nil
}

func (c *Center) getTargets(ctx context.Context) ([]TargetRow, error) {
	if c.db != nil {
		return c.db.ListTargets(ctx)
	}
	return c.mem.ListTargets(), nil
}

func portOf(listen string) string {
	if _, p, err := net.SplitHostPort(listen); err == nil && p != "" {
		return p
	}
	return "18991"
}

// resolveAlias IP -> alias 解析（DB/内存自适应）；解析不到时原样返回，
// 让后续查询走"目标已删除 ⇒ 返回空"的既有语义。
func (c *Center) resolveAlias(ctx context.Context, ipOrAlias string) (string, error) {
	if c.db != nil {
		return c.db.ResolveAlias(ctx, ipOrAlias)
	}
	if a := c.mem.ResolveAlias(ipOrAlias); a != "" {
		return a, nil
	}
	return ipOrAlias, nil
}

// ---------- /stats /targets /targets/* ----------

func (c *Center) adminStats(ctx context.Context, w http.ResponseWriter) {
	targets, err := c.getTargets(ctx)
	if err != nil {
		writeJSON(w, 500, map[string]interface{}{"error": err.Error()})
		return
	}
	provs := map[string]bool{}
	for _, t := range targets {
		if t.Province != "" {
			provs[t.Province] = true
		}
	}
	writeJSON(w, 200, map[string]interface{}{
		"total_targets": len(targets),
		"province_cnt":  len(provs),
		"port":          portOf(c.conf.Listen),
		"version":       adminVersion,
	})
}

func (c *Center) adminTargets(ctx context.Context, w http.ResponseWriter) {
	targets, err := c.getTargets(ctx)
	if err != nil {
		writeJSON(w, 500, map[string]interface{}{"error": err.Error()})
		return
	}
	// 聚合 (province, operator, city) -> ips[]，与原版 Target 形状一致
	type grp struct {
		Province string   `json:"province"`
		Operator string   `json:"operator"`
		City     string   `json:"city"`
		IPs      []string `json:"ips"`
	}
	idx := map[string]*grp{}
	for _, t := range targets {
		key := t.Province + "|" + t.Telecom + "|" + t.City
		g, ok := idx[key]
		if !ok {
			g = &grp{Province: t.Province, Operator: t.Telecom, City: t.City}
			idx[key] = g
		}
		g.IPs = append(g.IPs, t.IP)
	}
	out := make([]*grp, 0, len(idx))
	for _, g := range idx {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Province != out[j].Province {
			return out[i].Province < out[j].Province
		}
		if out[i].Operator != out[j].Operator {
			return out[i].Operator < out[j].Operator
		}
		return out[i].City < out[j].City
	})
	writeJSON(w, 200, out)
}

func adminStr(body map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := body[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func (c *Center) adminTargetAdd(ctx context.Context, w http.ResponseWriter, body map[string]interface{}) {
	ip := adminStr(body, "ip")
	if ip == "" {
		writeJSON(w, 400, map[string]interface{}{"error": "缺少 ip"})
		return
	}
	if net.ParseIP(ip) == nil || net.ParseIP(ip).To4() == nil {
		writeJSON(w, 400, map[string]interface{}{"error": "IP 格式无效（需 IPv4）"})
		return
	}
	// A4：探测目标的唯一身份是 IP。对一个已 replace 成 2.2.2.2 的目标再点"添加 2.2.2.2"，
	// 会新建 alias=2.2.2.2 的行、而老行 current_ip 也是 2.2.2.2 ⇒ 同 IP 探测两次、历史劈成两半。
	var holder string
	var inUse bool
	var err error
	if c.db != nil {
		holder, inUse, err = c.db.IPInUse(ctx, ip, "")
	} else {
		holder, inUse = c.mem.IPInUse(ip, "")
	}
	if err != nil {
		writeJSON(w, 500, map[string]interface{}{"error": err.Error()})
		return
	}
	if inUse {
		writeJSON(w, 400, map[string]interface{}{
			"error": fmt.Sprintf("IP %s 已在目标列表中（归属 %s），请直接编辑该条目的省份/城市/运营商，或先删除", ip, holder)})
		return
	}
	row := TargetRow{
		Alias:    ip, // alias=ip：换 IP 只替换 current_ip，历史不断档
		IP:       ip,
		Province: adminStr(body, "province"),
		City:     adminStr(body, "city"),
		Telecom:  adminStr(body, "operator", "telecom"),
	}
	if c.db != nil {
		err = c.db.UpsertTarget(ctx, row)
	} else {
		c.mem.UpsertTarget(row)
	}
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, ErrIPInUse) {
			code = http.StatusBadRequest
		}
		writeJSON(w, code, map[string]interface{}{"error": err.Error()})
		return
	}
	c.bumpTargetsVersion(ctx) // 版本号自增，删/改非最新目标也能让离线节点感知（A3）
	c.invalidateTargetCache()
	c.hub.PushConfig() // 新增生效，在线节点秒级热加载
	writeJSON(w, 200, map[string]interface{}{})
}

func (c *Center) adminTargetDel(ctx context.Context, w http.ResponseWriter, body map[string]interface{}) {
	ip := adminStr(body, "ip")
	if ip == "" {
		writeJSON(w, 400, map[string]interface{}{"error": "缺少 ip"})
		return
	}
	var n int64
	var err error
	if c.db != nil {
		n, err = c.db.DeleteTargetByIP(ctx, ip)
	} else {
		n = c.mem.DeleteTargetByIP(ip)
	}
	if err != nil {
		writeJSON(w, 500, map[string]interface{}{"error": err.Error()})
		return
	}
	if n == 0 {
		writeJSON(w, 404, map[string]interface{}{"error": "未找到该 IP 的目标"})
		return
	}
	c.bumpTargetsVersion(ctx)
	c.invalidateTargetCache()
	c.hub.PushConfig() // 删除生效
	writeJSON(w, 200, map[string]interface{}{"deleted": n})
}

// adminTargetReplace 换 IP。
// inherit=true（面板默认勾选）：alias 不变仅更新 current_ip，历史数据自然衔接；
// inherit=false：alias 一并换成新 IP，旧 alias 行删除 —— 面板上的"不继承历史"，
// 旧实现在后端完全没读这个字段，复选框是装饰品。
func (c *Center) adminTargetReplace(ctx context.Context, w http.ResponseWriter, body map[string]interface{}) {
	oldIP := adminStr(body, "oldIP", "old_ip")
	newIP := adminStr(body, "newIP", "new_ip")
	if oldIP == "" || newIP == "" {
		writeJSON(w, 400, map[string]interface{}{"error": "缺少 oldIP / newIP"})
		return
	}
	if net.ParseIP(newIP) == nil || net.ParseIP(newIP).To4() == nil {
		writeJSON(w, 400, map[string]interface{}{"error": "新 IP 格式无效（需 IPv4）"})
		return
	}
	inherit := true
	if v, ok := body["inherit"].(bool); ok {
		inherit = v
	}
	var n int64
	var err error
	switch {
	case !inherit && c.db != nil:
		n, err = c.db.RenameTargetAlias(ctx, oldIP, newIP)
	case !inherit:
		n, err = c.mem.RenameTargetAlias(oldIP, newIP)
	case c.db != nil:
		n, err = c.db.ReplaceTargetIP(ctx, oldIP, newIP)
	default:
		n, err = c.mem.ReplaceTargetIP(oldIP, newIP)
	}
	if err != nil {
		code := http.StatusInternalServerError
		switch {
		case errors.Is(err, ErrIPInUse):
			code = http.StatusBadRequest
		case errors.Is(err, ErrTargetNotFound):
			code = http.StatusNotFound
		}
		writeJSON(w, code, map[string]interface{}{"error": err.Error()})
		return
	}
	if n == 0 {
		writeJSON(w, 404, map[string]interface{}{"error": "未找到 oldIP 对应的目标"})
		return
	}
	c.bumpTargetsVersion(ctx)
	c.invalidateTargetCache()
	c.hub.PushConfig() // 换 IP 生效
	if inherit {
		writeJSON(w, 200, map[string]interface{}{"replaced": n})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"replaced": n, "inherit": false})
}

func (c *Center) adminTargetHistory(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	ip := r.URL.Query().Get("ip")
	hours := atoiDefault(r.URL.Query().Get("hours"), 24)
	if hours <= 0 || hours > 720 {
		hours = 24
	}
	if ip == "" {
		writeJSON(w, 400, map[string]interface{}{"error": "缺少 ip"})
		return
	}
	var data []map[string]interface{}
	var err error
	// P1-4：pinglog.target 存的是 alias，而前端传的是当前 IP。两者在
	// targets/replace 之后会分叉，必须先解析再查，否则换过 IP 的目标历史永久空白。
	alias, aerr := c.resolveAlias(ctx, ip)
	if aerr != nil {
		writeJSON(w, 500, map[string]interface{}{"error": aerr.Error()})
		return
	}
	if c.db != nil {
		data, err = c.db.TargetIPHistory(ctx, alias, hours)
		if err != nil {
			writeJSON(w, 500, map[string]interface{}{"error": err.Error()})
			return
		}
	} else {
		data = c.mem.TargetIPHistory(alias, hours)
	}
	if data == nil {
		data = []map[string]interface{}{}
	}
	writeJSON(w, 200, map[string]interface{}{"data": data})
}

// ---------- /node/* ----------

// adminNodeStatus center 自身作为 self 节点排在首位，其余为心跳注册的探测节点
func (c *Center) adminNodeStatus(ctx context.Context, w http.ResponseWriter) {
	var nodes []NodeInfo
	if c.db != nil {
		// 不再吞掉 DB 错误：PG 抖动时旧实现返回空列表，面板显示"只有中心自己在线"，
		// 运维会以为探测节点全挂了。
		list, err := c.db.ListNodes(ctx)
		if err != nil {
			writeJSON(w, 500, map[string]interface{}{"error": "读取节点表失败: " + err.Error()})
			return
		}
		nodes = list
	} else {
		nodes = c.mem.ListNodes()
	}
	type item struct {
		Name      string `json:"name"`
		Host      string `json:"host"`
		Port      int    `json:"port"`
		Online    bool   `json:"online"`
		Self      bool   `json:"self"`
		PingAtlas bool   `json:"pingatlas"`
		Label     string `json:"label,omitempty"`
		// Display 该节点最终显示名（数据库 label → center.json NodeGeo → 节点 id），
		// 面板直接显示这个即可，不必自己再实现一遍优先级。
		Display string `json:"display,omitempty"`
		// Province/City/Telecom 新增节点时填的归属信息（面板"节点管理"里展示）
		Province string `json:"province,omitempty"`
		City     string `json:"city,omitempty"`
		Telecom  string `json:"telecom,omitempty"`
		PubkeyFP string `json:"pubkey_fp,omitempty"`
		Auth     string `json:"auth,omitempty"`
		// Visible 前台是否展示（面板可切换；隐藏的节点仍照常采集）
		Visible bool `json:"visible"`
	}
	// 显示名按"数据库 label → NodeGeo 配置 → 节点 id"解析（与首页同一套规则）
	dbLabels := map[string]string{}
	for _, n := range nodes {
		if strings.TrimSpace(n.Label) != "" {
			dbLabels[n.NodeID] = n.Label
		}
	}
	out := []item{{
		Name: "center(" + c.conf.Listen + ")", Host: "127.0.0.1",
		Port: atoiDefault(portOf(c.conf.Listen), 18991), Online: true, Self: true, PingAtlas: true,
		Display: "中心", Auth: "center", Visible: true,
	}}
	for _, n := range nodes {
		authMode := "hmac"
		if n.PubkeyFP != "" {
			authMode = "ed25519"
		}
		out = append(out, item{
			Name: n.NodeID, Host: n.IP, Port: 0,
			Label: n.Label, Display: c.nodeLabel(dbLabels, n.NodeID),
			Province: n.Province, City: n.City, Telecom: n.Telecom,
			PubkeyFP: n.PubkeyFP, Auth: authMode, Visible: n.Visible,
			Online:    c.nodeAlive(n), // 用已取到的 DB 行兜底，不再逐节点回查（D5）
			Self:      false, PingAtlas: true,
		})
	}
	writeJSON(w, 200, map[string]interface{}{"nodes": out, "mode": "central"})
}

// adminNodeAdd 注册新节点：独立 token + 一次性安装码（30 分钟有效、首次上线作废），
// 返回一键安装命令 —— URL 只带 name+code，真 token 由脚本当场兑换，不进任何日志。
//
// 两种用法：
//  1) 只给 地区 + 运营商（推荐）：`{"province":"四川","city":"成都","telecom":"tencent"}`
//     → 节点 id 随机生成（n-xxxxxxxxxxxx），显示名自动拼成"成都腾讯"并写进数据库，
//     首页立刻显示中文名，**不需要改 center.json、不需要重启中心**；
//  2) 老用法：只给 `{"name":"..."}` → 显示名留空，退回 NodeGeo 配置或节点 id。
//
// 重名节点直接拒绝（旧行为是静默覆盖 token，会让旧节点失联）。
func (c *Center) adminNodeAdd(ctx context.Context, w http.ResponseWriter, r *http.Request, body map[string]interface{}) {
	name := strings.TrimSpace(adminStr(body, "name"))
	province := strings.TrimSpace(adminStr(body, "province"))
	city := strings.TrimSpace(adminStr(body, "city"))
	telecom := strings.TrimSpace(adminStr(body, "telecom"))
	label := strings.TrimSpace(adminStr(body, "label"))

	// 地区/运营商：都允许自由填写（面板就是两个输入框）
	//   - province 传了就必须是全国省份列表里的值（老调用方/脚本用）；
	//     面板只填"地区"时把它放在 city 里，province 留空即跳过这项校验。
	//   - telecom 既接受固定取值（ctcc 等映射成"电信/联通/…"），也接受任意文本
	//     （例如"腾讯云"/"移动BGP"），自由文本会原样拼进显示名。
	if province != "" && !validProvince(province) {
		writeJSON(w, 400, map[string]interface{}{"error": "省名不在全国省份列表内: " + province})
		return
	}
	if len([]rune(telecom)) > 24 {
		writeJSON(w, 400, map[string]interface{}{"error": "运营商文本过长（≤24 字）"})
		return
	}
	if len([]rune(city)) > 24 {
		writeJSON(w, 400, map[string]interface{}{"error": "地区文本过长（≤24 字）"})
		return
	}

	if name == "" {
		// 新流程：名字随机生成，但必须知道是哪个地区哪种线路，否则显示名没法拼
		if city == "" || telecom == "" {
			writeJSON(w, 400, map[string]interface{}{"error": "请给出 name，或同时给出 city(地区) + telecom(运营商)"})
			return
		}
		id, err := randomNodeID()
		if err != nil {
			writeJSON(w, 500, map[string]interface{}{"error": err.Error()})
			return
		}
		name = id
	} else if !validNodeName(name) {
		writeJSON(w, 400, map[string]interface{}{"error": "节点名仅限字母/数字/-/_，最长 64"})
		return
	}
	if label == "" && city != "" && telecom != "" {
		label = composeNodeLabel(city, telecom)
	}
	if len([]rune(label)) > 48 {
		writeJSON(w, 400, map[string]interface{}{"error": "显示名过长（≤48 字）"})
		return
	}

	// 存在性检查（DB / 内存两条路径都支持，内存模式便于本地开发和测试）
	exists := false
	if c.db != nil {
		e, eerr := c.db.NodeExists(ctx, name)
		if eerr != nil {
			writeJSON(w, 500, map[string]interface{}{"error": "读取节点表失败: " + eerr.Error()})
			return
		}
		exists = e
	} else {
		exists = c.mem.NodeExists(name)
	}
	if exists {
		writeJSON(w, 400, map[string]interface{}{"error": "节点名已存在（如需重装请先调用 node/del 删除，或换一个名字）"})
		return
	}
	traw := make([]byte, 16)
	if _, err := rand.Read(traw); err != nil {
		writeJSON(w, 500, map[string]interface{}{"error": err.Error()})
		return
	}
	token := hex.EncodeToString(traw)
	craw := make([]byte, 6)
	if _, err := rand.Read(craw); err != nil {
		writeJSON(w, 500, map[string]interface{}{"error": err.Error()})
		return
	}
	code := hex.EncodeToString(craw)
	expire := time.Now().Add(30 * time.Minute)
	nc := NodeCreate{
		NodeID: name, Token: token, Code: code, Expire: expire,
		Label: label, Province: province, City: city, Telecom: telecom,
	}
	if c.db != nil {
		if err := c.db.CreateNode(ctx, nc); err != nil {
			writeJSON(w, 500, map[string]interface{}{"error": err.Error()})
			return
		}
	} else if err := c.mem.CreateNode(nc); err != nil {
		writeJSON(w, 500, map[string]interface{}{"error": err.Error()})
		return
	}
	// 对外 base URL：白名单化 scheme 与 Host 后才允许拼进 shell 命令（见 publicBase）
	base, berr := publicBase(r)
	if berr != nil {
		writeJSON(w, 400, map[string]interface{}{"error": berr.Error()})
		return
	}
	// name 已过 validNodeName、code 是 hex、base 已校验 ⇒ 这条命令里不会出现
	// $()、反引号或引号，%q 的双引号在这里是安全的。
	cmd := fmt.Sprintf("curl -fsSL %q | bash",
		base+"/agent/install.sh?name="+urlQueryEscape(name)+"&code="+urlQueryEscape(code))
	writeJSON(w, 200, map[string]interface{}{
		"ok": true, "name": name, "label": label, "province": province, "city": city, "telecom": telecom,
		"token": token, "install_code": code,
		"code_expire": expire.Format("2006-01-02 15:04:05"), "install_cmd": cmd,
	})
}

// 新增节点用到的取值表 —— 省名复用地图分区表 provinceRegion（全国 34 个，避免写出
// "四川成都" 这种省市混写的值），运营商用固定 7 个取值。
var nodeTelSuffix = map[string]string{
	"ctcc": "电信", "cucc": "联通", "cmcc": "移动",
	"tencent": "腾讯", "aliyun": "阿里", "huawei": "华为", "other": "节点",
}

func validProvince(p string) bool { _, ok := provinceRegion[p]; return ok }

func validNodeTelecom(t string) bool { _, ok := nodeTelSuffix[t]; return ok }

// composeNodeLabel 显示名 = 地区 + 运营商。
// 运营商是固定取值（ctcc 等）时用简称（"成都"+"tencent" → "成都腾讯"）；
// 自由文本（"腾讯云"/"移动BGP"）则原样拼接（"成都"+"腾讯云" → "成都腾讯云"）。
func composeNodeLabel(city, telecom string) string {
	city = strings.TrimSpace(city)
	t := strings.TrimSpace(telecom)
	if suf, ok := nodeTelSuffix[t]; ok {
		return city + suf
	}
	return city + t
}

// randomNodeID 随机节点 id：n- + 12 位十六进制（节点名只需在系统内唯一、可读性无关）
func randomNodeID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "n-" + hex.EncodeToString(b), nil
}

// normalizeCityName 把目标表里可能带 AS 后缀的城市值收敛成纯城市名
// （历史数据里有 "乐山AS4809" 这种写法；城市下拉不该出现 AS 号）。
func normalizeCityName(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "AS"); i > 0 {
		// AS 后面必须全是数字才算后缀，避免误伤"巴西AS"这类正常词（虽然城市名极少出现）
		tail := s[i+2:]
		if tail != "" && strings.Trim(tail, "0123456789") == "" {
			s = strings.TrimSpace(s[:i])
		}
	}
	return s
}

// adminGeoOptions 后台"新增节点"表单的选项：省份固定列表 + 省→市（取自实际监测目标）+ 运营商
func (c *Center) adminGeoOptions(ctx context.Context, w http.ResponseWriter) {
	provs := make([]string, 0, len(provinceRegion))
	for p := range provinceRegion {
		provs = append(provs, p)
	}
	sort.Strings(provs)
	var pc map[string][]string
	if c.db != nil {
		pc, _ = c.db.ProvinceCities(ctx)
	} else {
		pc = c.mem.ProvinceCities()
	}
	if pc == nil {
		pc = map[string][]string{}
	}
	// 城市名归一化 + 去重（目标表里同一城市可能有多条、且带 AS 后缀）
	cities := make(map[string][]string, len(pc))
	for prov, list := range pc {
		seen := map[string]bool{}
		out := make([]string, 0, len(list))
		for _, raw := range list {
			c := normalizeCityName(raw)
			if c == "" || seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, c)
		}
		sort.Strings(out)
		cities[prov] = out
	}
	type telOpt struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	}
	tels := make([]telOpt, 0, len(nodeTelSuffix))
	for k, v := range nodeTelSuffix {
		tels = append(tels, telOpt{Key: k, Name: v})
	}
	sort.Slice(tels, func(i, j int) bool { return tels[i].Key < tels[j].Key })
	writeJSON(w, 200, map[string]interface{}{
		"ok": true, "provinces": provs, "cities": cities, "telecoms": tels,
	})
}

// adminNodeUpdate 让指定节点自升级到目标版本（走长连接下发 cmd{update,ver,sha256}）。
// sha256 必须由管理员提供：/agent/download 给的是"中心旁边那份二进制"，只有校验和
// 才能确认节点拿到的是本次要发布的那个文件（update_node.go 里也是这么强制的）。
func (c *Center) adminNodeUpdate(ctx context.Context, w http.ResponseWriter, body map[string]interface{}) {
	name := strings.TrimSpace(adminStr(body, "name"))
	ver := strings.TrimSpace(adminStr(body, "ver"))
	sha := strings.TrimSpace(adminStr(body, "sha256"))
	if name == "" || ver == "" || sha == "" {
		writeJSON(w, 400, map[string]interface{}{"error": "需要 name + ver + sha256"})
		return
	}
	if c.hub == nil || !c.hub.SendCmd(name, "update", ver, sha) {
		writeJSON(w, 409, map[string]interface{}{"error": "节点当前没有在线长连接，无法下发升级指令"})
		return
	}
	log.Printf("center: 已向节点 %s 下发自升级指令 -> %s", name, ver)
	writeJSON(w, 200, map[string]interface{}{"ok": true, "name": name, "ver": ver,
		"message": "升级指令已下发：节点会下载、校验 sha256、原子替换后重启；以心跳版本号为准"})
}

// adminNodeEdit 修改节点的展示信息：显示名 / 地区 / 运营商 / 前台是否展示。
//
// 不动的东西（有意为之）：
//   - **节点 id**：它是 pinglog 历史行的主键，改名会让历史断档；要换个名字请用 node/add 建新节点。
//   - **身份公钥**：换钥匙走 node/key/reset。
//   - 采集：隐藏只是一条展示开关，节点照常探测、上报、入库。
func (c *Center) adminNodeEdit(ctx context.Context, w http.ResponseWriter, body map[string]interface{}) {
	name := strings.TrimSpace(adminStr(body, "name"))
	if name == "" {
		writeJSON(w, 400, map[string]interface{}{"error": "节点名不能为空"})
		return
	}
	var cur NodeInfo
	var found bool
	if c.db != nil {
		list, err := c.db.ListNodes(ctx)
		if err != nil {
			writeJSON(w, 500, map[string]interface{}{"error": "读取节点表失败: " + err.Error()})
			return
		}
		for _, n := range list {
			if n.NodeID == name {
				cur, found = n, true
				break
			}
		}
	} else {
		cur, found = c.mem.GetNodeInfo(name)
	}
	if !found {
		writeJSON(w, 404, map[string]interface{}{"error": "节点不存在: " + name})
		return
	}

	label, province, city, telecom := cur.Label, cur.Province, cur.City, cur.Telecom
	_, labelGiven := body["label"]
	if labelGiven {
		label = strings.TrimSpace(adminStr(body, "label"))
	}
	if _, has := body["province"]; has {
		province = strings.TrimSpace(adminStr(body, "province"))
	}
	if _, has := body["city"]; has {
		city = strings.TrimSpace(adminStr(body, "city"))
	}
	if _, has := body["telecom"]; has {
		telecom = strings.TrimSpace(adminStr(body, "telecom"))
	}
	// 没显式给显示名、但地区或运营商变了 → 按"地区+运营商"重算（与新增节点同一套规则）
	if !labelGiven && city != "" && telecom != "" && (city != cur.City || telecom != cur.Telecom) {
		label = composeNodeLabel(city, telecom)
	}
	for _, f := range []struct {
		v   string
		max int
		nm  string
	}{{label, 48, "显示名"}, {province, 24, "地区"}, {city, 24, "地区"}, {telecom, 24, "运营商"}} {
		if len([]rune(f.v)) > f.max {
			writeJSON(w, 400, map[string]interface{}{"error": f.nm + "过长（≤" + strconv.Itoa(f.max) + " 字）"})
			return
		}
	}

	var err error
	if c.db != nil {
		err = c.db.SetNodeMeta(ctx, name, label, province, city, telecom)
	} else {
		err = c.mem.SetNodeMeta(name, label, province, city, telecom)
	}
	if err != nil {
		writeJSON(w, 500, map[string]interface{}{"error": err.Error()})
		return
	}

	visible := cur.Visible
	if v, has := body["visible"]; has {
		switch t := v.(type) {
		case bool:
			visible = t
		case string:
			visible = t == "true" || t == "1"
		case float64:
			visible = t != 0
		}
		if c.db != nil {
			err = c.db.SetNodeVisible(ctx, name, visible)
		} else {
			err = c.mem.SetNodeVisible(name, visible)
		}
		if err != nil {
			writeJSON(w, 500, map[string]interface{}{"error": err.Error()})
			return
		}
	}
	log.Printf("center: 管理员修改节点 %s 显示名=%q 地区=%q 运营商=%q 前台展示=%v",
		name, label, city, telecom, visible)
	writeJSON(w, 200, map[string]interface{}{
		"ok": true, "name": name, "label": label, "province": province, "city": city,
		"telecom": telecom, "visible": visible,
		"display":  c.nodeLabel(map[string]string{name: label}, name),
		"message":  "已保存（隐藏只影响首页展示，节点仍照常采集）",
	})
}

// adminNodeKeyReset 清空某节点的公钥（换钥匙/密钥丢失时的出口）。
//
// 清空后：该节点的签名立即失效；节点若仍持有自己的独立 token，下次启动会用 HMAC
// 通道自助重新登记公钥（自愈）；若 token 也已被清理，则必须走"重新生成安装码"的人工流程。
// 这是个敏感操作（等于允许该名字重新绑定身份），所以只走管理密码鉴权的接口。
func (c *Center) adminNodeKeyReset(ctx context.Context, w http.ResponseWriter, body map[string]interface{}) {
	name := strings.TrimSpace(adminStr(body, "name"))
	if name == "" {
		writeJSON(w, 400, map[string]interface{}{"error": "节点名不能为空"})
		return
	}
	old := ""
	if c.db != nil {
		if o, err := c.db.GetNodePubkey(ctx, name); err == nil {
			old = o
		}
		if err := c.db.SetNodePubkey(ctx, name, ""); err != nil {
			writeJSON(w, 500, map[string]interface{}{"error": err.Error()})
			return
		}
	} else {
		old = c.mem.GetNodePubkey(name)
		if err := c.mem.SetNodePubkey(name, ""); err != nil {
			writeJSON(w, 404, map[string]interface{}{"error": err.Error()})
			return
		}
	}
	log.Printf("center: 管理员清空节点 %s 的公钥（旧指纹=%s）", name, pubkeyFingerprintHex(old))
	writeJSON(w, 200, map[string]interface{}{"ok": true, "name": name,
		"message": "公钥已清空：该节点的签名立即失效；节点下次启动会用独立 token 重新登记"})
}

// adminNodeDel 删除节点（节点名用尽 / token 泄露 / 需要重装时）。
// 交接文档 §6.2 指出的缺口：报错文案让运维"先删除"，但接口不存在，只能进 PG 手删。
// 注意：旧配置里用全局 token 的节点删掉记录后仍能用全局 token 连上
// （全局 token 是共享的万能钥匙），独立 token 的节点则立即失效。
func (c *Center) adminNodeDel(ctx context.Context, w http.ResponseWriter, body map[string]interface{}) {
	name := strings.TrimSpace(adminStr(body, "name"))
	if name == "" {
		writeJSON(w, 400, map[string]interface{}{"error": "缺少 name"})
		return
	}
	var n int64
	var err error
	if c.db != nil {
		n, err = c.db.DeleteNode(ctx, name)
	} else {
		n = c.mem.DeleteNode(name)
	}
	if err != nil {
		writeJSON(w, 500, map[string]interface{}{"error": err.Error()})
		return
	}
	if n == 0 {
		writeJSON(w, 404, map[string]interface{}{"error": "节点不存在"})
		return
	}
	if c.hub != nil {
		c.hub.Disconnect(name) // 立刻断开在线连接，别等节点自己发现
	}
	c.mu.Lock()
	delete(c.lastSeen, name)
	c.mu.Unlock()
	log.Printf("center: 节点 %s 已删除（历史明细保留）", name)
	writeJSON(w, 200, map[string]interface{}{"ok": true, "deleted": n})
}

// adminNodeRestart 远程重启：优先按 name（node_id）定位，兼容旧前端按列表下标 idx。
func (c *Center) adminNodeRestart(ctx context.Context, w http.ResponseWriter, body map[string]interface{}) {
	if v, ok := body["idx"].(float64); ok && int(v) == 0 && adminStr(body, "name") == "" {
		writeJSON(w, 200, map[string]interface{}{"message": "重启指令已发送，请稍候刷新页面"})
		restartUnitAsync(unitCenter)
		return
	}
	name := adminStr(body, "name")
	if name == "" { // 兼容：旧前端只有 idx>0 的下标定位
		if v, ok := body["idx"].(float64); ok {
			idx := int(v)
			var nodes []NodeInfo
			if c.db != nil {
				list, err := c.db.ListNodes(ctx)
				if err != nil {
					writeJSON(w, 500, map[string]interface{}{"error": "读取节点表失败: " + err.Error()})
					return
				}
				nodes = list
			} else {
				nodes = c.mem.ListNodes()
			}
			if idx >= 1 && idx <= len(nodes) {
				name = nodes[idx-1].NodeID
			}
		}
	}
	if name != "" && c.hub != nil && c.hub.SendCmd(name, "restart", "", "") {
		writeJSON(w, 200, map[string]interface{}{"message": "重启指令已通过长连接下发至 " + name + "，节点重连后自动确认"})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"error": "节点不在线（无长连接），无法远程重启"})
}

// adminCmdStatus 查询节点最近一条远程指令的回执状态（三态）
func (c *Center) adminCmdStatus(ctx context.Context, w http.ResponseWriter, body map[string]interface{}) {
	name := adminStr(body, "name")
	if name == "" {
		writeJSON(w, 400, map[string]interface{}{"error": "缺少 name"})
		return
	}
	_ = ctx
	if s := c.cmdSnapshot(name); s != nil {
		writeJSON(w, 200, map[string]interface{}{"ok": true, "cmd": s})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true, "cmd": nil})
}

// restartUnitAsync 异步 systemd 重启（Start+Wait 释放子进程，不留僵尸）
func restartUnitAsync(unit string) {
	go func() {
		time.Sleep(300 * time.Millisecond)
		cmd := exec.Command("systemctl", "restart", unit)
		if err := cmd.Start(); err == nil {
			_ = cmd.Wait()
		}
	}()
}

func (c *Center) adminNodeCompare(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	hours := atoiDefault(r.URL.Query().Get("hours"), 24)
	if hours <= 0 || hours > 720 {
		hours = 24
	}
	if target == "" {
		writeJSON(w, 400, map[string]interface{}{"error": "缺少 target"})
		return
	}
	var rows []NodeCompareRow
	var err error
	// P1-4：同 TargetIPHistory，target 列是 alias，先解析当前 IP
	alias, aerr := c.resolveAlias(ctx, target)
	if aerr != nil {
		writeJSON(w, 500, map[string]interface{}{"error": aerr.Error()})
		return
	}
	if c.db != nil {
		rows, err = c.db.NodeCompare(ctx, alias, hours)
		if err != nil {
			writeJSON(w, 500, map[string]interface{}{"error": err.Error()})
			return
		}
	} else {
		rows = c.mem.NodeCompare(alias, hours)
	}
	if rows == nil {
		rows = []NodeCompareRow{}
	}
	writeJSON(w, 200, map[string]interface{}{"nodes": rows})
}

// ---------- /config/* /logs /restart ----------

// adminConfigSave 仅实现管理密码修改（写回 center.json AdminPwd 字段并热生效），
// 不碰节点鉴权用的 Token 字段 —— 改密码不再影响任何节点。
// 其余项（端口/名字/邮件）pingatlas 由配置文件与 systemd 管理，前端接受保存动作。
func (c *Center) adminConfigSave(ctx context.Context, w http.ResponseWriter, body map[string]interface{}) {
	newPwd := adminStr(body, "password")
	if newPwd != "" {
		if err := c.saveAdminPwd(newPwd); err != nil {
			writeJSON(w, 500, map[string]interface{}{"error": "写入配置失败: " + err.Error()})
			return
		}
	}
	writeJSON(w, 200, map[string]interface{}{})
}

// saveAdminPwd 热更新管理密码并持久化到 center.json 的 AdminPwd 键。
//
// 顺序很关键：**先落盘、成功后才改内存**。旧实现先把 c.conf.AdminPwd 换掉再去写文件，
// 文件写失败（只读挂载/ENOSPC）时接口回 500"写入配置失败"，但运行中的进程已经在用新密码
// —— 运维以为没改成，实际旧密码立刻失效，重启后又变回去。
//
// A5：tmp+fsync+rename 原子写，并显式 chmod 0600（os.OpenFile 的 perm 对已存在的
// 0644 文件不生效）。
func (c *Center) saveAdminPwd(newPwd string) error {
	// 串行化整个"读-改-写"：否则两个并发改密码会交错成
	// A写文件 → B写文件 → B改内存 → A改内存 ⇒ 内存=A、文件=B，重启后密码回退。
	c.pwdMu.Lock()
	defer c.pwdMu.Unlock()

	newPwd = strings.TrimSpace(newPwd)
	if len(newPwd) < 6 {
		return fmt.Errorf("密码至少 6 位")
	}
	if len(newPwd) > 128 {
		return fmt.Errorf("密码过长（最多 128 字节）")
	}
	if c.confPath == "" {
		return fmt.Errorf("未指定配置文件路径")
	}
	raw, err := os.ReadFile(c.confPath)
	if err != nil {
		return err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	m["AdminPwd"] = newPwd
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.confPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(out); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil { // 已存在文件也不会被 OpenFile 收紧，显式 chmod
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, c.confPath); err != nil {
		os.Remove(tmp)
		return err
	}
	// 落盘成功之后才生效
	c.mu.Lock()
	c.conf.AdminPwd = newPwd
	c.mu.Unlock()
	return nil
}

func (c *Center) adminLogs(w http.ResponseWriter, r *http.Request) {
	unit := unitCenter
	args := []string{"-u", unit, "-n", "300", "--no-pager", "-o", "short"}
	if r.URL.Query().Get("type") == "error" {
		args = append(args, "-p", "err..emerg")
	}
	out, err := exec.Command("journalctl", args...).Output()
	if err != nil {
		writeJSON(w, 200, map[string]interface{}{"content": "(无法读取 journalctl: " + err.Error() + ")"})
		return
	}
	content := string(out)
	if strings.TrimSpace(content) == "" {
		content = "(暂无日志)"
	}
	writeJSON(w, 200, map[string]interface{}{"content": content})
}

func (c *Center) adminRestart(w http.ResponseWriter) {
	writeJSON(w, 200, map[string]interface{}{"message": "重启指令已发送"})
	restartUnitAsync(unitCenter)
}

// ---------- /tool/ping /scan/csegment（系统 ping 实现，center 无需 CAP_NET_RAW）----------

func (c *Center) adminToolPing(w http.ResponseWriter, body map[string]interface{}) {
	host := adminStr(body, "host")
	if host == "" {
		writeJSON(w, 400, map[string]interface{}{"error": "缺少 host"})
		return
	}
	ip := host
	if net.ParseIP(host) == nil {
		addrs, err := net.LookupHost(host)
		if err != nil {
			writeJSON(w, 200, map[string]interface{}{"error": "域名解析失败: " + err.Error()})
			return
		}
		ip = ""
		for _, a := range addrs {
			if net.ParseIP(a).To4() != nil {
				ip = a
				break
			}
		}
		if ip == "" {
			writeJSON(w, 200, map[string]interface{}{"error": "未解析到 IPv4 地址"})
			return
		}
	}
	out, err := exec.Command("ping", "-c", "10", "-i", "0.2", "-W", "2", ip).CombinedOutput()
	raw := string(out)
	res := map[string]interface{}{"host": host, "ip": ip}
	if err != nil && !strings.Contains(raw, "rtt") {
		res["error"] = "目标不可达"
		res["raw"] = raw
		writeJSON(w, 200, res)
		return
	}
	res["loss"] = fmt.Sprintf("%d", parsePingLoss(raw))
	res["min"], res["avg"], res["max"] = parsePingRTT(raw)
	res["raw"] = raw
	writeJSON(w, 200, res)
}

func parsePingLoss(raw string) int {
	for _, line := range strings.Split(raw, "\n") {
		i := strings.Index(line, "% packet loss")
		if i < 0 {
			continue
		}
		// 从 "% packet loss" 往前回溯取数字（避免误抓 "10 packets transmitted" 的 10）
		j := i
		for j > 0 && ((line[j-1] >= '0' && line[j-1] <= '9') || line[j-1] == '.') {
			j--
		}
		f := strings.TrimSpace(line[j:i])
		if f != "" {
			if n, err := strconv.ParseFloat(f, 64); err == nil {
				return int(n + 0.5)
			}
		}
	}
	return 100
}

func parsePingRTT(raw string) (min, avg, max float64) {
	for _, line := range strings.Split(raw, "\n") {
		if !strings.Contains(line, "rtt") && !strings.Contains(line, "round-trip") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		parts := strings.Split(strings.TrimSpace(line[eq+1:]), "/")
		if len(parts) >= 3 {
			min, _ = strconv.ParseFloat(parts[0], 64)
			avg, _ = strconv.ParseFloat(parts[1], 64)
			max, _ = strconv.ParseFloat(parts[2], 64)
		}
		break
	}
	return round2(min), round2(avg), round2(max)
}

// adminScanCSegment 扫描 /24：254 个地址并发 ping（-c1 -W1），返回存活 IP 列表
func (c *Center) adminScanCSegment(ctx context.Context, w http.ResponseWriter, body map[string]interface{}) {
	ip := adminStr(body, "ip")
	if ip == "" {
		writeJSON(w, 400, map[string]interface{}{"error": "缺少 ip"})
		return
	}
	base := net.ParseIP(ip).To4()
	if base == nil {
		writeJSON(w, 400, map[string]interface{}{"error": "IP 格式无效"})
		return
	}
	prefix := fmt.Sprintf("%d.%d.%d.", base[0], base[1], base[2])
	const last = 254
	results := make([]string, last)
	var firstErrs []string // 诊断：记录前几个失败原因
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 64)
	// D4：用 CommandContext，ctx 超时/断开时真正杀掉 ping 子进程。
	// 原来 cmd.Run() 只靠 ctx 返回，254 个 ping 进程会继续在后台跑完。
	sctx, cancelScan := context.WithCancel(ctx)
	defer cancelScan()
	for i := 1; i <= last; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			target := prefix + strconv.Itoa(i)
			cmd := exec.CommandContext(sctx, "ping", "-c", "1", "-W", "1", target)
			if err := cmd.Run(); err == nil {
				results[i-1] = target
			} else {
				mu.Lock()
				if len(firstErrs) < 3 {
					firstErrs = append(firstErrs, target+": "+err.Error())
				}
				mu.Unlock()
			}
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		writeJSON(w, 500, map[string]interface{}{"error": "扫描超时"})
		return
	}
	alive := []string{}
	for _, r := range results {
		if r != "" {
			alive = append(alive, r)
		}
	}
	resp := map[string]interface{}{"results": alive}
	if len(alive) == 0 && len(firstErrs) > 0 {
		resp["debug_first_errors"] = firstErrs
	}
	writeJSON(w, 200, resp)
}

// ---------- /admin SPA 静态服务（原版 admin-dist 构建产物，history 路由 fallback）----------

// spaHandler 先按文件精确匹配（相对 admin-dist 根的路径），未命中回 index.html
// （Vue Router history 模式：/admin/dashboard 等路由刷新时也要出壳页）
func spaHandler(fsys fs.FS) http.Handler {
	fileServer := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean(r.URL.Path)
		if p == "." || p == "/" {
			p = "index.html"
		}
		if st, err := fs.Stat(fsys, p); err != nil || st.IsDir() {
			b, err := fs.ReadFile(fsys, "index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(b)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}
