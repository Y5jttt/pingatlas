//go:build center

package main

// ---------- 中心端 WebSocket 长连接 Hub ----------
//
// 挂在 GET /agent/ws，质询-应答 HMAC 鉴权（URL 无 token）。
// 帧协议（JSON 文本帧）：
//   ↓ challenge {type,nonce}                           连接建立后中心立即发（一次性随机数）
//   ↑ auth      {type,name,mac}                        mac=HMAC-SHA256(token,nonce)，10s 内
//   ↑ heartbeat {type,targets,backlog,version,clock_offset_ms,boot_ts}  每 60s + 连接建立立即一帧
//   ↑ report    {type,seq,items[]}                     每 30s，单批在途，seq 供 ack 回显
//   ↓ ack       {type,seq,ok,count,rejected,err,server_ts}  落库回执（seq 原样回显，水位推进凭据）
//   ↓ config    {type,version,targets[]}               目标表变更秒级推送 / 重连立即推送
//   ↓ cmd       {type,action[,ver,sha256]}             远程指令（restart | update）
//
// 可靠性不变：节点收到同 seq 的 ack 才推水位，落库幂等，重推无害；
// ack 超过 90s 未回，节点主动断连降级到 HTTP 兜底，数据不丢。

import (
	"context"
	crypto_rand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	wsPingInterval = 30 * time.Second
	wsReadTimeout  = 65 * time.Second // 两个 ping 周期 + 余量，无 pong 无数据即判死
	wsWriteTimeout = 10 * time.Second

	// maxWSMessageBytes 单帧上限，与 HTTP 路的 MaxBytesReader(1MB) 对齐。
	// 旧实现完全没有上限：一个被攻陷的节点发一个 GB 级帧就能把中心 OOM。
	maxWSMessageBytes = 1 << 20
	// maxReportItems 单批行数上限（节点实际每批 2000 行）。
	// HTTP 路靠 1MB 请求体间接限制，WS 路原先没有任何限制 —— 一次几万行就能让
	// CopyFrom 与 buildRows 长时间占住读循环。
	maxReportItems = 5000
	// maxConcurrentHandshakes 同时在途的握手数上限（全局，防放大）。
	maxConcurrentHandshakes = 128
	// maxHandshakesPerIP 同一来源 IP 的同时在途握手数上限。
	// 只做全局计数时，单台机器用几十条空连接就能把全局配额占满，把合法节点挡在门外
	// （每条连接最多占用 wsHandshakeTimeout）；按 IP 限并发才是真正的止血点。
	maxHandshakesPerIP = 4
	// wsHandshakeTimeout 单次握手的读超时。缩短它直接压缩"占坑"成本：
	// 全局 128 个坑 × 5 秒，攻击者需要约 26 条新连接/秒才能持续打满。
	wsHandshakeTimeout = 5 * time.Second
)


var wsUpgrader = websocket.Upgrader{ReadBufferSize: 4096, WriteBufferSize: 4096}

// Hub 按节点名管理在线连接；同名新连接踢旧连接（半开僵尸清理）。
type Hub struct {
	c     *Center
	mu    sync.Mutex
	conns map[string]*wsConn
	hsSem chan struct{} // 全局握手并发闸门（只覆盖 Upgrade→鉴权完成这段窗口）

	hsMu   sync.Mutex
	hsByIP map[string]int // 每来源 IP 的在途握手数
}

type wsConn struct {
	name string
	conn *websocket.Conn
	wmu  sync.Mutex // 串行化 WriteMessage（WriteControl 本身并发安全）
}

func NewHub(c *Center) *Hub {
	return &Hub{c: c, conns: map[string]*wsConn{}, hsSem: make(chan struct{}, maxConcurrentHandshakes),
		hsByIP: map[string]int{}}
}

// acquireHandshake 占用一次握手配额（先全局、再按 IP）。false 表示应拒绝本次连接。
func (h *Hub) acquireHandshake(ip string) bool {
	select {
	case h.hsSem <- struct{}{}:
	default:
		return false
	}
	h.hsMu.Lock()
	if h.hsByIP[ip] >= maxHandshakesPerIP {
		h.hsMu.Unlock()
		<-h.hsSem
		return false
	}
	h.hsByIP[ip]++
	h.hsMu.Unlock()
	return true
}

// releaseHandshake 归还配额。计数归零即删除 key，避免 map 随来源无界增长。
func (h *Hub) releaseHandshake(ip string) {
	h.hsMu.Lock()
	if n := h.hsByIP[ip]; n <= 1 {
		delete(h.hsByIP, ip)
	} else {
		h.hsByIP[ip] = n - 1
	}
	h.hsMu.Unlock()
	<-h.hsSem
}

// ServeWS 升级连接 → 质询-应答鉴权 → 读写循环，连接关闭后返回。
// 握手：升级后中心发 challenge{nonce}，节点须在 10s 内回 auth{name,mac=HMAC(token,nonce)}，
// 失败/超时即断开 —— URL 里没有 token，每次连接的线上凭据都不同（防重放）。
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	// 握手闸门：全局上限 + 每 IP 上限；鉴权完成即归还（不限制长连接数量）。
	// 维度说明：只做全局限流时，单点几十条空连接就能把配额占满、挡住合法节点。
	hsIP := h.c.realIP(r)
	if !h.acquireHandshake(hsIP) {
		http.Error(w, "too many concurrent handshakes", http.StatusServiceUnavailable)
		return
	}
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		h.releaseHandshake(hsIP)
		return
	}
	conn.SetReadLimit(maxWSMessageBytes) // 与 HTTP 路的 1MB 上限对齐
	name, method, ok := wsHandshake(h.c, conn)
	h.releaseHandshake(hsIP) // 闸门只覆盖握手窗口，不限制长连接数量
	if !ok {
		conn.Close()
		return
	}
	wc := &wsConn{name: name, conn: conn}

	h.mu.Lock()
	if old, ok := h.conns[name]; ok {
		_ = old.conn.Close() // 旧连接的读循环负责清场
	}
	h.conns[name] = wc
	h.mu.Unlock()
	log.Printf("center: 节点 %s WS 已连接 (%s, %s 鉴权通过)", name, r.RemoteAddr, method)

	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	h.c.touchNode(name, ip) // 只更新 ip/last_seen，不清 version/targets/clock_offset（B5）
	h.pushTo(wc)             // 重连立即对齐配置

	defer func() {
		h.mu.Lock()
		if h.conns[wc.name] == wc {
			delete(h.conns, wc.name)
		}
		h.mu.Unlock()
		conn.Close()
		log.Printf("center: 节点 %s WS 断开", wc.name)
	}()

	conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
		return nil
	})

	go func() { // ping 保活循环
		t := time.NewTicker(wsPingInterval)
		defer t.Stop()
		for range t.C {
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteTimeout)); err != nil {
				return
			}
		}
	}()

	for {
		mt, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.TextMessage {
			continue
		}
		conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
		h.dispatch(wc, raw)
	}
}

// wsHandshake 质询-应答鉴权：发 challenge，等 auth{mac}，verifyMAC 通过返回节点名。
// 全部状态为局部变量，多条并发连接互不干扰。
func wsHandshake(c *Center, conn *websocket.Conn) (string, string, bool) {
	nonce := make([]byte, 16)
	if _, err := crypto_rand.Read(nonce); err != nil {
		return "", "", false
	}
	nonceHex := hex.EncodeToString(nonce)
	if raw, err := json.Marshal(wsChallenge{Type: "challenge", Nonce: nonceHex}); err == nil {
		_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
		if err := conn.WriteMessage(websocket.TextMessage, raw); err != nil {
			return "", "", false
		}
	}
	_ = conn.SetReadDeadline(time.Now().Add(wsHandshakeTimeout))
	mt, raw, err := conn.ReadMessage()
	if err != nil || mt != websocket.TextMessage {
		return "", "", false
	}
	var au wsAuth
	if json.Unmarshal(raw, &au) != nil || au.Type != "auth" || au.Name == "" {
		return "", "", false
	}
	// 兼容：老节点只发 mac 字段（= HMAC）；新节点发 alg + cred（cred 可为签名）
	cred, alg := au.Cred, au.Alg
	if cred == "" {
		cred = au.MAC
	}
	if alg == "" {
		alg = authAlgHMAC
	}
	if !c.verifyAuthMsg(au.Name, authMsgHandshake(au.Name, nonceHex), nonceHex, cred, alg).OK {
		log.Printf("center: WS 握手鉴权失败 name=%s alg=%s ip=%s", au.Name, alg, conn.RemoteAddr())
		return "", "", false
	}
	return au.Name, algLabel(alg), true
}

// dispatch 分发上行帧
func (h *Hub) dispatch(wc *wsConn, raw []byte) {
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return
	}
	switch probe.Type {
	case "heartbeat":
		var hb wsHeartbeat
		if json.Unmarshal(raw, &hb) != nil {
			return
		}
		ip, _, _ := net.SplitHostPort(wc.conn.RemoteAddr().String())
		h.c.registerNode(wc.name, ip, hb.Version, hb.Targets, hb.ClockOffsetMs, hb.BootTs)
		if hb.Backlog > 0 {
			log.Printf("center: 节点 %s 积压 %d 行待补传", wc.name, hb.Backlog)
		}
		h.c.markCmdHeartbeat(wc.name, hb.Version, hb.BootTs)
	case "report":
		var rep wsReport
		if json.Unmarshal(raw, &rep) != nil {
			h.sendTo(wc, wsAck{Type: "ack", Seq: rep.Seq, OK: false, Err: "bad json"})
			return
		}
		if len(rep.Items) > maxReportItems {
			// 回失败 ack（不是 OK+rejected）：节点不推进水位，数据留在本地
			log.Printf("center: 节点 %s 单批行数超限 %d > %d，拒收", wc.name, len(rep.Items), maxReportItems)
			h.sendTo(wc, wsAck{Type: "ack", Seq: rep.Seq, OK: false,
				Err: fmt.Sprintf("批次过大(%d 行，上限 %d)", len(rep.Items), maxReportItems)})
			return
		}
		rows, rejected, err := h.c.buildRows(wc.name, rep.Items)
		if err != nil {
			// 目标表读不出来 => 无法判定这批数据合法性 => 回失败的 ack，
			// 节点不推进水位、下轮原样重推。绝不能回 OK（那会让节点以为已落库）。
			log.Printf("center: WS 目标校验失败(%s, %d 行): %v", wc.name, len(rep.Items), err)
			h.sendTo(wc, wsAck{Type: "ack", Seq: rep.Seq, OK: false, Err: err.Error()})
			return
		}
		n, err := h.c.insertRows(rows)
		if err != nil {
			log.Printf("center: WS 落库失败(%s, %d 行): %v", wc.name, len(rep.Items), err)
			h.sendTo(wc, wsAck{Type: "ack", Seq: rep.Seq, OK: false, Err: err.Error()})
			return
		}
		if rejected > 0 {
			log.Printf("center: 节点 %s 上报中拒收 %d 行（目标不在当前目标表）", wc.name, rejected)
		}
		h.sendTo(wc, wsAck{Type: "ack", Seq: rep.Seq, OK: true, Count: n,
			Rejected: rejected, ServerTs: time.Now().UnixMilli()})
	}
}

// sendTo 发送数据帧；失败即关连接（读循环清场，节点自动重连）
func (h *Hub) sendTo(wc *wsConn, v interface{}) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	wc.wmu.Lock()
	defer wc.wmu.Unlock()
	wc.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	if err := wc.conn.WriteMessage(websocket.TextMessage, raw); err != nil {
		_ = wc.conn.Close()
	}
}

func (h *Hub) pushTo(wc *wsConn) {
	cfg, err := h.c.currentConfig()
	if err != nil {
		log.Printf("center: 读取目标表失败，本次不推送配置给 %s: %v", wc.name, err)
		return
	}
	h.sendTo(wc, wsConfig{Type: "config", Version: cfg.Version, Targets: cfg.Targets, Clear: cfg.Clear})
}

// PushConfig 目标表变更后向所有在线节点秒推配置
func (h *Hub) PushConfig() {
	h.mu.Lock()
	conns := make([]*wsConn, 0, len(h.conns))
	for _, wc := range h.conns {
		conns = append(conns, wc)
	}
	h.mu.Unlock()
	if len(conns) == 0 {
		return
	}
	cfg, err := h.c.currentConfig()
	if err != nil {
		return
	}
	for _, wc := range conns {
		h.sendTo(wc, wsConfig{Type: "config", Version: cfg.Version, Targets: cfg.Targets, Clear: cfg.Clear})
	}
	log.Printf("center: 配置 v%d（目标 %d 个）已推送至 %d 个在线节点", cfg.Version, len(cfg.Targets), len(conns))
}

// SendCmd 向指定在线节点下发指令并登记回执跟踪；不在线返回 false。
// update 指令携带目标版本号，节点自升级完成后以心跳版本号作为回执。
func (h *Hub) SendCmd(name, action, ver, sha string) bool {
	h.mu.Lock()
	wc, ok := h.conns[name]
	h.mu.Unlock()
	if !ok {
		return false
	}
	h.c.noteCmd(name, action, ver)
	h.sendTo(wc, wsCmd{Type: "cmd", Action: action, Ver: ver, Sha256: sha})
	return true
}

// IsOnline 节点是否有活的长连接
func (h *Hub) IsOnline(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.conns[name]
	return ok
}

// Disconnect 主动断开某节点的长连接（删除节点时用；读循环负责清场）。
func (h *Hub) Disconnect(name string) bool {
	h.mu.Lock()
	wc, ok := h.conns[name]
	h.mu.Unlock()
	if !ok {
		return false
	}
	_ = wc.conn.Close()
	return true
}

// ---------- 共享给 HTTP 与 WS 两条路的落库/注册逻辑 ----------

// registerNode 心跳即更新节点表（版本/目标数/时钟偏移/进程启动时刻）
func (c *Center) registerNode(name, ip, version string, targets int, offsetMs, bootTs int64) {
	c.touch(name)
	ni := NodeInfo{NodeID: name, IP: ip, Version: version, Targets: targets,
		LastSeen: time.Now(), ClockOffsetMs: offsetMs, BootTs: bootTs}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if c.db != nil {
		_ = c.db.UpsertNode(ctx, ni)
	} else {
		c.mem.UpsertNode(ni)
	}
}

// touchNode 长连接建立时只刷新 ip 与 last_seen。
// 关键：绝不能把 version/targets/clock_offset 清空 —— 每次重连都清会让
// update 回执（比对版本号）被这两次写入搅乱，也会让后台看到节点信息闪烁（B5）。
func (c *Center) touchNode(name, ip string) {
	c.touch(name)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if c.db != nil {
		_ = c.db.TouchNode(ctx, name, ip)
	} else {
		c.mem.TouchNode(name, ip)
	}
}

// ---------- 远程指令回执跟踪（三态：待确认 / 已完成 / 超时） ----------

const cmdTimeout = 10 * time.Minute

// noteCmd 下发指令时登记：记下节点当前 boot_ts，重启后心跳带来新 boot_ts 即确认为真重启
func (c *Center) noteCmd(name, action, ver string) {
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()
	prevBoot := int64(0)
	if ni, err := c.nodeInfo(name); err == nil {
		prevBoot = ni.BootTs
	}
	c.cmds[name] = &cmdState{Action: action, SentAt: time.Now(), Ver: ver, PrevBootTs: prevBoot}
}

// markCmdHeartbeat 心跳回执判定：
//   restart：boot_ts 变了 = 进程真的重启过（网络抖动重连 boot_ts 不变，不再假阳性，C4）
//   update ：心跳版本号等于目标版本 = 升级到位
// 超时统一标 dead。
func (c *Center) markCmdHeartbeat(name, ver string, bootTs int64) {
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()
	s := c.cmds[name]
	if s == nil {
		return
	}
	if !s.Done {
		switch s.Action {
		case "restart":
			if bootTs > 0 && s.PrevBootTs > 0 && bootTs != s.PrevBootTs {
				s.Done = true
				s.DoneAt = time.Now()
			}
		case "update":
			if ver != "" && s.Ver != "" && ver == s.Ver {
				s.Done = true
				s.DoneAt = time.Now()
			}
		}
	}
	if !s.Done && !s.Dead && time.Since(s.SentAt) > cmdTimeout {
		s.Dead = true
	}
}

// cmdSnapshot 取某节点的最近指令状态（超时未确认标 dead）
func (c *Center) cmdSnapshot(name string) *cmdState {
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()
	s := c.cmds[name]
	if s == nil {
		return nil
	}
	cp := *s
	if !cp.Done && !cp.Dead && time.Since(cp.SentAt) > cmdTimeout {
		cp.Dead = true
	}
	return &cp
}

// buildRows 上报项 -> 落库行，并过滤掉「不在当前目标表内」的 target（B4：
// 被攻陷或配置漂移的节点不能往库里注入任意垃圾行）。
//
// **fail-closed**（C 档修复）：目标表读不出来时必须整批拒收，不能放行。
// 原实现是 `if len(valid) > 0 {校验}` + knownTargets 吞掉 ListTargets 错误，
// 于是 PG 抖动 5 秒内（或目标被删空时）valid 为空 => 校验被整段跳过，
// 任意节点都能写入任意 target 行。
//
// 关键：读表失败必须返回 error 让调用方回**失败的 ack**。若只回 rejected 计数
// 而 ack.OK=true，节点会照常推进水位，这批数据就被静默丢弃了 —— 那比拒收更糟。
// 返回 (有效行, 被拒行数, error)
func (c *Center) buildRows(name string, items []reportItem) ([]PingRow, int, error) {
	valid, err := c.knownTargets()
	if err != nil {
		return nil, 0, err
	}
	// 目标表为空是合法状态（管理员可能清空目标），此时一切上报都是非法的。
	// 注意不能沿用「空集合 = 不校验」的旧语义。
	rows := make([]PingRow, 0, len(items))
	rejected := 0
	for _, it := range items {
		lt, err := time.Parse(time.RFC3339, it.Logtime)
		if err != nil {
			rejected++
			continue
		}
		if _, ok := valid[it.Target]; !ok {
			rejected++
			continue
		}
		rows = append(rows, PingRow{
			Logtime: lt, Target: it.Target, NodeID: name,
			MaxDelay: it.MaxDelay, MinDelay: it.MinDelay, AvgDelay: it.AvgDelay,
			SendPk: it.SendPk, RecvPk: it.RecvPk, LossPk: it.LossPk,
		})
	}
	return rows, rejected, nil
}

// bumpTargetsVersion 目标表变更后自增版本号并推送（A3）
func (c *Center) bumpTargetsVersion(ctx context.Context) {
	if c.db != nil {
		if v, err := c.db.BumpTargetsVersion(ctx); err == nil {
			log.Printf("center: 目标配置版本 -> v%d", v)
		} else {
			log.Printf("center: 版本号自增失败: %v", err)
		}
		return
	}
	c.mem.BumpTargetsVersion()
}

// invalidateTargetCache 目标表变了，丢弃 5s 的目标集合缓存（否则新目标会被当成非法行拒收）
func (c *Center) invalidateTargetCache() {
	c.tgtMu.Lock()
	c.tgtCache = nil
	c.tgtMu.Unlock()
}

// knownTargets 当前目标表 alias 集合（带 5s 缓存，避免每批上报都查库）。
// **查库失败必须返回 error**（C 档修复）：原实现 `targets, _ = c.db.ListTargets(ctx)`
// 吞掉错误后返回一个空集合，buildRows 里的「空集合 = 不校验」语义就把它变成了
// 整个上报通道的 fail-open 开关 —— PG 抖一下，所有节点都能写任意 target 行。
func (c *Center) knownTargets() (map[string]struct{}, error) {
	c.tgtMu.Lock()
	defer c.tgtMu.Unlock()
	if c.tgtCache != nil && time.Since(c.tgtCacheAt) < 5*time.Second {
		return c.tgtCache, nil
	}
	set := map[string]struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var targets []TargetRow
	if c.db != nil {
		var err error
		targets, err = c.db.ListTargets(ctx)
		if err != nil {
			return nil, fmt.Errorf("读取目标表失败: %w", err)
		}
	} else {
		targets = c.mem.ListTargets()
	}
	for _, t := range targets {
		set[t.Alias] = struct{}{}
	}
	c.tgtCache = set
	c.tgtCacheAt = time.Now()
	return set, nil
}

// nodeInfo 取单个节点信息（供指令回执记录 boot_ts）
func (c *Center) nodeInfo(name string) (NodeInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if c.db != nil {
		return c.db.GetNode(ctx, name)
	}
	for _, ni := range c.mem.ListNodes() {
		if ni.NodeID == name {
			return ni, nil
		}
	}
	return NodeInfo{}, fmt.Errorf("节点 %s 未注册", name)
}

// insertRows 批量幂等落库，返回实际新增行数（ack 的 count 就用它）
func (c *Center) insertRows(rows []PingRow) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if c.db != nil {
		return c.db.InsertRows(ctx, rows)
	}
	return c.mem.InsertRows(rows), nil
}

// currentConfig 当前目标表 + 版本号。
// 读表失败时返回 error（调用方不得推送）；读成功但表为空是合法状态，返回 Clear=true。
// 绝不能把「读失败」和「表为空」都表达成空 Targets —— 前者会让节点清空目标停摆，
// 后者才是管理员的意图。
func (c *Center) currentConfig() (configResp, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var targets []TargetRow
	var ver int64
	if c.db != nil {
		var err error
		targets, err = c.db.ListTargets(ctx)
		if err != nil {
			return configResp{}, fmt.Errorf("读取目标表失败: %w", err)
		}
		ver, err = c.db.TargetsVersion(ctx)
		if err != nil {
			return configResp{}, fmt.Errorf("读取目标版本失败: %w", err)
		}
	} else {
		targets = c.mem.ListTargets()
		ver = c.mem.TargetsVersion()
	}
	ts := make([]Target, 0, len(targets))
	for _, t := range targets {
		ts = append(ts, Target{Alias: t.Alias, IP: t.IP})
	}
	return configResp{Version: ver, Targets: ts, Clear: len(ts) == 0}, nil
}
