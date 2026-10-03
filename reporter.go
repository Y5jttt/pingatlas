//go:build node

package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ---------- 节点端运行时编排 ----------

// Node 单二进制 node 角色：探测 -> SQLite -> 上报 -> 心跳 -> 热配置
type Node struct {
	conf     atomic.Value // NodeConf
	engine   *Engine
	store    *NodeStore
	client   *http.Client // 探测/上报/心跳用，短超时（15s）
	dlClient *http.Client // 二进制下载专用，长超时（自升级 10-17MB，P1-5）
	confPath string       // config.json 路径（远程目标回写用）
	wsUp     atomic.Bool  // WS 长连接在线时，HTTP 上报/心跳/拉配置静默

	clockOff atomic.Int64 // 节点时钟偏移估算（中心 server_ts - 本地时刻，毫秒）
	bootTs   atomic.Int64 // 进程启动时刻：中心据此区分"真重启"与"网络抖动重连"

	remoteVer int64 // 已应用的远程配置版本
	verMu     sync.Mutex

	// Ed25519 身份（私钥永不外发；nil = 只用 HMAC 兼容通道）
	priv     ed25519.PrivateKey
	keyFP    string
	sigReady atomic.Bool // 公钥已在中心登记 → 走签名鉴权
}

// checkClockRewind 时钟回拨自愈（A2）——判据是「**本轮探测自己的 logtime** ≤ 水位」。
//
// 为什么不能用「缓冲里最新 logtime ≤ 水位」：
// 稳态下该条件恒成立。探测→上报→ack 推进水位到 maxLT 后，当轮那些 logtime==水位 的
// 确认行仍留在缓冲里（B7 的 24h 保留策略只删水位前 24 小时的行），于是
// MaxLogtime()==水位，每次检查都判成回拨 → 每 30s（HTTP 路）或每次重连（WS 路）
// 回退 2 小时 → 356 目标×120 分钟≈4.3 万行反复重传，磁盘数周写满。
//
// 真正的时钟回拨只有一个可观测特征：**刚写进去的整分钟数据落在水位之前**。
// 检查点必须紧跟 roundLoop 的 InsertRound —— 那里的 logtime 来自
// engine.RunRound 开头的 time.Now()，与节点自身时钟严格同源；
// 而上报/会话启动处的水位是「上一次成功上报的进度」，与当前时钟无关。
//
// 回退量取 (水位 - 本轮 logtime) + 余量，而非固定 2 小时：刚好够把回拨窗口内的
// 数据重新纳入 LoadBatch 范围即可，回退太多会让中心重复收历史。
// 正常 NTP 校准下不会触发；install.sh 已配置 ntp.aliyun.com 定期同步。
func (n *Node) checkClockRewind(roundLogtime int64) {
	wm := n.store.GetWatermark()
	if wm == 0 {
		return // 从未上报过，没有水位概念，谈不上回拨
	}
	// 必须用 >=：logtime 被截断到整分钟，"水位 == 本轮 logtime" 是**稳态的一部分**
	// （上报被 ack 后水位恰好等于最近一轮的整分钟）。只有本轮 logtime **严格早于**
	// 水位才是真的时钟回拨。反例：节点在第 N 分钟被重启，启动后立刻跑的那一轮
	// logtime 仍等于水位，旧判据会误判成回拨、回退 60 秒并触发一轮重传（实测复现）。
	if roundLogtime >= wm {
		return // 本轮数据不早于水位：正常
	}
	rewind := wm - roundLogtime + 60
	if rewind > maxClockRewind {
		rewind = maxClockRewind
	}
	log.Printf("node: 检测到时钟回拨（本轮 logtime=%s ≤ 水位 %s），回退水位 %s 触发重报",
		time.Unix(roundLogtime, 0).Format("01-02 15:04"), time.Unix(wm, 0).Format("01-02 15:04"),
		time.Duration(rewind)*time.Second)
	if err := n.store.RewindWatermark(rewind); err != nil {
		log.Printf("node: 水位回退失败: %v", err)
	}
}

// maxClockRewind 水位回退上限，防止时钟被恶意/意外设到远古时把水位拖到 0 导致全量重传
const maxClockRewind = 6 * 3600

// authReq 给节点 HTTP 请求挂 X-PingAtlas-Auth 头。
//   - 有私钥就签名：name:ts:ed25519b:签名("name|ts|sha256(body)")（公钥已登记时唯一可用的方式）
//   - 没私钥才退回兼容模式：name:ts:hmac(token, "name|ts")
//
// 签名版本顺带把**请求体摘要**签进去：老的 HMAC 只签时间戳，请求体仅靠 TLS 保护。
func (n *Node) authReq(c NodeConf, req *http.Request, body []byte) {
	ts := time.Now().Unix()
	if priv := n.priv; priv != nil {
		req.Header.Set("X-PingAtlas-Auth", fmt.Sprintf("%s:%d:%s:%s",
			c.Name, ts, authAlgEd25519Body, signHTTP(priv, c.Name, ts, body)))
		return
	}
	req.Header.Set("X-PingAtlas-Auth", fmt.Sprintf("%s:%d:%s", c.Name, ts, hmacToken(c.Token, fmt.Sprintf("%s|%d", c.Name, ts))))
}

// registerPubkey 用现有 HMAC 凭据把本节点公钥登记到中心（幂等）。
// 新装节点、以及"中心把公钥重置了"的自愈场景都走这里；失败不影响 HMAC 兼容通道。
func (n *Node) registerPubkey(c NodeConf) bool {
	if n.priv == nil || c.Center == "" {
		return false
	}
	pub := hex.EncodeToString(n.priv.Public().(ed25519.PublicKey))
	body, _ := json.Marshal(map[string]string{"name": c.Name, "pubkey": pub})
	req, err := http.NewRequest(http.MethodPost, c.Center+"/agent/pubkey", bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	ts := time.Now().Unix()
	req.Header.Set("X-PingAtlas-Auth", fmt.Sprintf("%s:%d:%s", c.Name, ts,
		hmacToken(c.Token, fmt.Sprintf("%s|%d", c.Name, ts))))
	resp, err := n.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 403 且没有 token = 中心已清空 token（正常终态）：签名通道照常工作，不必每次启动都告警
		if resp.StatusCode != http.StatusForbidden || c.Token != "" {
			log.Printf("node: 登记公钥失败 status=%d（继续用现有通道）", resp.StatusCode)
		}
		return false
	}
	n.sigReady.Store(true)
	log.Printf("node: 公钥已登记到中心 指纹=%s（此后用 Ed25519 签名鉴权）", n.keyFP)
	return true
}
func runNode(confPath string) {
	if confPath == "" {
		confPath = "config.json"
	}
	c := loadNodeConf(confPath)
	log.Printf("node: 名字=%s 模式=%s 本地目标=%d",
		c.Name, map[bool]string{true: "center", false: "本地"}[c.Center != ""], len(c.Targets))

	// Ed25519 身份：首启生成私钥（0600，临时文件 + fsync + rename）；
	// 若配置里记录过指纹但文件不在了 → 判定"密钥丢失"，明确报错并拒绝启动，
	// 绝不静默生成一个新身份（那样中心会认为节点掉线，而真正的身份已被人顶替或丢失）。
	keyPath := c.KeyPath
	if keyPath == "" {
		keyPath = defaultKeyPath
	}
	priv, keyFP, kerr := loadOrCreateNodeKey(keyPath, c.PubkeyFP, c.KeyRegen)
	if kerr != nil {
		fatal("node: %v", kerr)
	}
	log.Printf("node: 身份公钥指纹=%s（私钥 %s）", keyFP, keyPath)
	if c.PubkeyFP != keyFP {
		if err := saveNodePubkeyFP(confPath, keyFP); err != nil {
			log.Printf("node: 指纹写回配置失败（不影响本次运行）: %v", err)
		}
	}

	engine, err := NewEngine(c.SrcIP)
	if err != nil {
		fatal("node: ICMP 引擎初始化失败(需要 root/CAP_NET_RAW): %v", err)
	}
	// SQLite 绝对路径（D1）：相对路径在 systemd 下 cwd=/ 会把库落到 /pingatlas_node.db
	dbPath := c.DBPath
	if dbPath == "" {
		dbPath = "/var/lib/pingatlas/pingatlas_node.db"
	}
	store, err := OpenNodeStore(dbPath)
	if err != nil {
		fatal("node: SQLite 打开失败: %v", err)
	}
	n := &Node{
		engine:   engine,
		store:    store,
		client:   &http.Client{Timeout: 15 * time.Second},
		// P1-5：自升级要下载 10-17MB 二进制，复用 15s 超时的 client 几乎必然超时。
		// 单独给一个宽松超时的 client，只用于下载，不影响探测/上报的时效性。
		dlClient: &http.Client{Timeout: 10 * time.Minute},
		confPath: confPath,
		priv:     priv,
		keyFP:    keyFP,
	}
	n.bootTs.Store(time.Now().Unix())
	n.conf.Store(c)
	if c.TargetsVer > 0 {
		log.Printf("node: 使用已持久化的远程目标 v%d 共 %d 个", c.TargetsVer, len(c.Targets))
	}

	go n.roundLoop()
	go n.reportLoop()
	go n.heartbeatLoop()
	go n.configLoop()
	go n.wsLoop()
	go n.cleanupLoop()

	select {} // 常驻
}

// roundLoop 每分钟一轮探测，结果写 SQLite。
// 启动后先立即跑一轮（便于冒烟测试），之后对齐整分钟。
func (n *Node) roundLoop() {
	first := true
	for {
		if !first {
			now := time.Now()
			time.Sleep(time.Until(now.Truncate(time.Minute).Add(time.Minute)))
		}
		first = false
		c := n.conf.Load().(NodeConf)
		if len(c.Targets) == 0 {
			log.Printf("node: 目标列表为空，跳过本轮")
			continue
		}
		start := time.Now()
		rs := n.engine.RunRound(c.Targets, defaultCount, 50*time.Millisecond, 3*time.Second, 128)
		if err := n.insertRoundWithRetry(rs); err != nil {
			// 这一分钟的探测结果没能落盘 —— 明确说清是"丢数据"，别让它看起来像普通告警
			log.Printf("node: !!! 本轮探测结果写入 SQLite 连续失败，该分钟数据已丢失: %v", err)
			continue
		}
		// 时钟回拨检测必须紧跟写入：本轮 logtime 与节点时钟同源，是唯一可靠的判据。
		// 若本轮整分钟数据落在水位之下，说明时钟往回跳了，这批数据永远取不出批次
		// （LoadBatch 只取 > 水位），必须回退水位才能重报。
		if len(rs) > 0 {
			n.checkClockRewind(rs[0].Logtime)
		}
		var alive int
		for _, r := range rs {
			if r.RecvPk > 0 {
				alive++
			}
		}
		log.Printf("node: 本轮完成 目标=%d 存活=%d 耗时=%s 缓冲=%d",
			len(rs), alive, time.Since(start).Round(time.Millisecond), n.store.Count())
	}
}

// insertRoundWithRetry 写本地缓冲，短暂锁冲突/IO 抖动时重试；连续失败才放弃。
// 旧实现失败即 continue，一轮探测结果就这样永久消失（只有一行不明所以的日志）。
func (n *Node) insertRoundWithRetry(rs []RoundResult) error {
	var err error
	for i := 0; i < 3; i++ {
		if err = n.store.InsertRound(rs); err == nil {
			return nil
		}
		log.Printf("node: SQLite 写入失败(第 %d 次重试): %v", i+1, err)
		time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
	}
	return err
}

// reportLoop 每 30s 上报一批：水位之后的 pending -> POST /agent/report
// 上报成功才推进水位；失败不动水位，下轮原样重推（幂等落库保证无害）。
func (n *Node) reportLoop() {
	seq := int64(0)
	for {
		time.Sleep(30 * time.Second)
		c := n.conf.Load().(NodeConf)
		if c.Center == "" || n.wsUp.Load() {
			continue // 纯本地模式，或 WS 长连接已接管上报
		}
		wm := n.store.GetWatermark()
		batch, err := n.store.LoadBatch(wm, 2000)
		if err != nil {
			log.Printf("node: 读缓冲失败: %v", err)
			continue
		}
		if len(batch) == 0 {
			continue
		}
		items := make([]reportItem, 0, len(batch))
		maxLT := wm
		for _, r := range batch {
			items = append(items, reportItem{
				Target:   r.Target,
				Logtime:  time.Unix(r.Logtime, 0).Format(time.RFC3339),
				MaxDelay: r.MaxDelay, MinDelay: r.MinDelay, AvgDelay: r.AvgDelay,
				SendPk: r.SendPk, RecvPk: r.RecvPk, LossPk: r.LossPk,
			})
			if r.Logtime > maxLT {
				maxLT = r.Logtime
			}
		}
		seq++
		body, _ := json.Marshal(reportReq{Name: c.Name, Seq: seq, Items: items})
		req, err := http.NewRequest(http.MethodPost, c.Center+"/agent/report", bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		n.authReq(c, req, body)
		resp, err := n.client.Do(req)
		if err != nil {
			log.Printf("node: 上报失败(水位不动): %v", err)
			continue
		}
		var rr reportResp
		err = json.NewDecoder(resp.Body).Decode(&rr)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || !rr.OK {
			log.Printf("node: 上报被拒 status=%d err=%s(水位不动)", resp.StatusCode, rr.Err)
			continue
		}
		if rr.ServerTs > 0 {
			n.clockOff.Store(rr.ServerTs - time.Now().UnixMilli())
		}
		if err := n.store.SetWatermark(maxLT); err != nil {
			log.Printf("node: 水位推进失败: %v", err)
			continue
		}
		if rr.Rejected > 0 {
			log.Printf("node: 中心拒收 %d 行（目标已从中心目标表移除）", rr.Rejected)
		}
		log.Printf("node: 上报成功 %d 行(批次%d), 水位 -> %s", rr.Count, seq, time.Unix(maxLT, 0).Format("01-02 15:04"))
	}
}

// heartbeatLoop 每 60s 心跳，中心以此判活（HTTP 回退路同样带积压与时钟偏移）
func (n *Node) heartbeatLoop() {
	for {
		c := n.conf.Load().(NodeConf)
		if c.Center != "" && !n.wsUp.Load() {
			body, _ := json.Marshal(heartbeatReq{
				Name: c.Name, Version: Version, Targets: len(c.Targets),
				Backlog:       int(n.store.CountAfter(n.store.GetWatermark())),
				ClockOffsetMs: n.clockOff.Load(),
				BootTs:        n.bootTs.Load(),
			})
			req, err := http.NewRequest(http.MethodPost, c.Center+"/agent/heartbeat", bytes.NewReader(body))
			if err == nil {
				req.Header.Set("Content-Type", "application/json")
				n.authReq(c, req, body)
				if resp, err := n.client.Do(req); err == nil {
					resp.Body.Close()
				}
			}
		}
		time.Sleep(60 * time.Second)
	}
}

// configLoop 每 60s 拉中心配置（POST + 头鉴权，URL 不带 token），
// 版本变化则原子替换目标列表（热加载，不重启）
func (n *Node) configLoop() {
	for {
		c := n.conf.Load().(NodeConf)
		if c.Center != "" && !n.wsUp.Load() { // WS 在线时配置由中心秒推，不再轮询
			req, err := http.NewRequest(http.MethodPost, c.Center+"/agent/config", nil)
			if err == nil {
				n.authReq(c, req, nil) // 配置拉取无请求体
				if resp, err := n.client.Do(req); err == nil {
					var cr configResp
					derr := json.NewDecoder(resp.Body).Decode(&cr)
					resp.Body.Close()
					if derr == nil && resp.StatusCode == http.StatusOK && (len(cr.Targets) > 0 || cr.Clear) {
						n.applyRemoteConfig(c, cr)
					}
				}
			}
		}
		time.Sleep(60 * time.Second)
	}
}

// applyRemoteConfig 远程目标列表热加载 + 持久化回写
func (n *Node) applyRemoteConfig(c NodeConf, cr configResp) {
	n.verMu.Lock()
	changed := cr.Version != n.remoteVer
	if changed {
		n.remoteVer = cr.Version
	}
	n.verMu.Unlock()
	if !changed {
		return
	}
	nc := c
	nc.Targets = cr.Targets
	nc.TargetsVer = cr.Version
	n.conf.Store(nc)
	if err := saveNodeTargets(n.confPath, cr.Targets, cr.Version); err != nil {
		log.Printf("node: 目标列表回写 config.json 失败(内存已生效): %v", err)
	} else {
		log.Printf("node: 远程配置已热加载 v%d 目标=%d (已回写 config.json)", cr.Version, len(cr.Targets))
	}
}

// backlogWarnRows 未确认积压超过此行数则告警（不删数据，只报警）
const backlogWarnRows = 200000

// cleanupLoop 每小时清理「已确认上报且早于 24h」的本地缓冲。
// 只删水位之前的行（已确认落库中心）；水位之后的未确认数据一律保留，
// 中心长时间不可达也不会丢数据（B7）。未确认积压过大时告警提示人工介入。
func (n *Node) cleanupLoop() {
	for {
		time.Sleep(time.Hour)
		del, pending, err := n.store.CleanupConfirmed(24 * 3600)
		if err != nil {
			log.Printf("node: 清理缓冲失败: %v", err)
			continue
		}
		if del > 0 {
			log.Printf("node: 清理已确认历史缓冲 %d 行（未确认积压 %d 行保留）", del, pending)
			// 顺手把空闲页还给文件系统，避免 SQLite 长期只涨不缩
			if err := n.store.IncrementalVacuum(); err != nil {
				log.Printf("node: incremental_vacuum 失败（不影响采集）: %v", err)
			}
		}
		if pending > backlogWarnRows {
			log.Printf("node: 警告 未确认积压 %d 行（>%d），本地 SQLite 持续增长，请检查中心连通性", pending, backlogWarnRows)
		}
	}
}

