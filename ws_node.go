//go:build node

package main

// ---------- 节点端 WebSocket 长连接 ----------
//
// wsLoop 拨号 -> wsSession 干活 -> 断开退避重连（1s→2s→…→30s，稳定 30s 以上归零）。
// WS 在线时 HTTP 上报/心跳/拉配置三循环静默（wsUp 标志）；连不上自动回退 HTTP 老路。
// 可靠性锚点不变：report 发出后水位不动，收到 ack 才推进；单批在途，ack 即补发下一批。

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	wsNodeReadTimeout = 70 * time.Second // 中心 30s 一 ping，两拍 + 余量
	wsNodeWriteTmo    = 10 * time.Second
	wsReportInterval  = 30 * time.Second
	wsHBInterval      = 60 * time.Second
	wsAckTimeout      = 90 * time.Second // 在途批次等 ack 的上限，超时主动断连让 HTTP 兜底顶上（B1/B2）
)

// wsURL http(s):// 自动转 ws(s)://
func wsURL(center string) string {
	switch {
	case strings.HasPrefix(center, "https://"):
		return "wss://" + strings.TrimPrefix(center, "https://")
	case strings.HasPrefix(center, "http://"):
		return "ws://" + strings.TrimPrefix(center, "http://")
	default:
		return "ws://" + center
	}
}

func (n *Node) wsLoop() {
	backoff := time.Second
	for {
		c := n.conf.Load().(NodeConf)
		if c.Center == "" {
			time.Sleep(30 * time.Second) // 纯本地模式，无中心可连
			continue
		}
		url := wsURL(c.Center) + "/agent/ws?name=" + urlQueryEscape(c.Name) // token 不进 URL
		// 还有 token 且本进程尚未确认公钥可用时，先尽力登记公钥（走 HMAC 证明身份）
		if n.priv != nil && !n.sigReady.Load() && c.Token != "" {
			n.registerPubkey(c)
		}
		started := time.Now()
		conn, used, err := n.dialAndAuth(url, c)
		if err != nil {
			log.Printf("node: WS 握手失败(%v)，%s 后重试", err, backoff.Round(time.Millisecond))
			// 签名不再被接受（例如中心侧公钥被重置/清空 token 后未登记）→ 下轮重新登记
			n.sigReady.Store(false)
			time.Sleep(backoff)
			backoff = minDur(backoff*2, 30*time.Second)
			continue
		}
		n.sigReady.Store(used == authAlgEd25519)
		if backoff != time.Second {
			log.Printf("node: 长连接已恢复")
		} else {
			log.Printf("node: 长连接已建立 -> %s (%s 鉴权通过)", url, algLabel(used))
		}
		n.wsSession(conn)
		if time.Since(started) > 30*time.Second {
			backoff = time.Second // 稳定过一段时间，视为闪断，快速重连
		}
		log.Printf("node: 长连接断开，%s 后重连", backoff.Round(time.Millisecond))
		time.Sleep(backoff)
		backoff = minDur(backoff*2, 30*time.Second)
	}
}

// dialAndAuth 连接并完成质询-应答握手，返回实际生效的鉴权方式。
//
// 顺序：**有私钥就先试 Ed25519 签名**（这是主路径，token 被清空后也照样能用），
// 失败且手上还有 token 时再退回 HMAC（兼容尚未登记公钥的节点）。
// 这样"清空库里 token"不会让已登记公钥的节点失联，而没登记公钥的节点也仍能上线。
func (n *Node) dialAndAuth(url string, c NodeConf) (*websocket.Conn, string, error) {
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return nil, "", err
	}
	var sigErr error
	if n.priv != nil {
		if sigErr = wsHandshake(conn, c.Name, c.Token, n.priv, true); sigErr == nil {
			return conn, authAlgEd25519, nil
		}
		conn.Close()
		if c.Token == "" {
			return nil, "", sigErr // 没有 token 可退，直接把签名失败原因报上去
		}
		conn, _, err = websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			return nil, "", err
		}
	}
	if err := wsHandshake(conn, c.Name, c.Token, n.priv, false); err != nil {
		conn.Close()
		return nil, "", err
	}
	return conn, authAlgHMAC, nil
}

// wsHandshake 中心先发 challenge，节点回应答；私钥与 token 本体都永不上线。
//   - useSig=true 且持有私钥 → 回 Ed25519 签名（中心只存公钥，无法被冒充）
//   - 否则 → 回 HMAC(token, "name|nonce")（兼容通道）
func wsHandshake(conn *websocket.Conn, name, token string, priv ed25519.PrivateKey, useSig bool) error {
	if name == "" || (token == "" && priv == nil) {
		return errNoToken
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return err
	}
	var ch wsChallenge
	if json.Unmarshal(raw, &ch) != nil || ch.Type != "challenge" || ch.Nonce == "" {
		return errBadChallenge
	}
	_ = conn.SetWriteDeadline(time.Now().Add(wsNodeWriteTmo))
	// P1-9：nonce 必须带 name —— HMAC 的全局 token 分支按 HMAC(全局token, name|nonce)
	// 校验，若这里只回裸 nonce，握手会失败。签名模式同样把 name 签进去（防换名）。
	if useSig && priv != nil {
		return conn.WriteMessage(websocket.TextMessage,
			mustJSON(wsAuth{Type: "auth", Name: name, Alg: authAlgEd25519,
				Cred: signHandshake(priv, name, ch.Nonce)}))
	}
	return conn.WriteMessage(websocket.TextMessage,
		mustJSON(wsAuth{Type: "auth", Name: name, MAC: hmacToken(token, name+"|"+ch.Nonce),
			Alg: authAlgHMAC}))
}

// wsSession 一条连接的完整会话，返回即连接已死
func (n *Node) wsSession(conn *websocket.Conn) {
	defer conn.Close()
	n.wsUp.Store(true)
	defer n.wsUp.Store(false)

	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }
	defer stop()

	ackCh := make(chan wsAck, 1)
	var wmu sync.Mutex
	send := func(v interface{}) error {
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		wmu.Lock()
		defer wmu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(wsNodeWriteTmo))
		return conn.WriteMessage(websocket.TextMessage, raw)
	}

	// 读循环：ack / config / cmd
	go func() {
		defer stop()
		conn.SetReadDeadline(time.Now().Add(wsNodeReadTimeout))
		conn.SetPongHandler(func(string) error {
			conn.SetReadDeadline(time.Now().Add(wsNodeReadTimeout))
			return nil
		})
		for {
			mt, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.TextMessage {
				continue
			}
			conn.SetReadDeadline(time.Now().Add(wsNodeReadTimeout))
			var probe struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(raw, &probe) != nil {
				continue
			}
			switch probe.Type {
			case "ack":
				var a wsAck
				if json.Unmarshal(raw, &a) == nil {
					if a.ServerTs > 0 { // 时钟偏移观测：server_ts - 节点本地时刻
						n.clockOff.Store(a.ServerTs - time.Now().UnixMilli())
					}
					select {
					case ackCh <- a:
					default: // 只认最新一笔回执
					}
				}
			case "config":
				var cf wsConfig
				if json.Unmarshal(raw, &cf) != nil {
					continue
				}
				// P1-3：空目标表是合法终态（管理员清空）。Clear 为 true 时必须应用，
				// 否则节点会永远继续探测已删目标。仅当「非空但 Clear=false」之外
				// 的异常空包才忽略。
				if len(cf.Targets) == 0 && !cf.Clear {
					continue
				}
				n.verMu.Lock()
				changed := cf.Version != n.remoteVer
				if changed {
					n.remoteVer = cf.Version
				}
				n.verMu.Unlock()
				if changed {
					cur := n.conf.Load().(NodeConf)
					nc := cur
					nc.Targets = cf.Targets
					nc.TargetsVer = cf.Version
					n.conf.Store(nc)
					if err := saveNodeTargets(n.confPath, cf.Targets, cf.Version); err != nil {
						log.Printf("node: 目标列表回写 config.json 失败(内存已生效): %v", err)
					} else {
						log.Printf("node: WS 推送配置已热加载 v%d 目标=%d (已回写 config.json)", cf.Version, len(cf.Targets))
					}
				}
			case "cmd":
				var cm wsCmd
				if json.Unmarshal(raw, &cm) != nil {
					continue
				}
				switch cm.Action {
				case "restart":
					log.Printf("node: 收到远程重启指令，进程退出等待 systemd 拉起")
					os.Exit(0)
				case "update":
					go n.selfUpdate(cm.Ver, cm.Sha256) // 拉新二进制 → 校验 → 原子替换 → 重启
				}
			}
		}
	}()

	// 连接建立：立即心跳（让中心秒标在线）+ 立即发第一批（有积压就排空）
	_ = send(n.newHeartbeat())

	sendTick := time.NewTicker(wsReportInterval)
	defer sendTick.Stop()
	hbTick := time.NewTicker(wsHBInterval)
	defer hbTick.Stop()

	var pendingMax int64  // 在途批的最大 logtime；ack 到达前不新发
	var pendingSeq int64  // 在途批的批次号（ack 回显比对）
	var pendingAt  time.Time
	batchSeq := int64(0)
	trySend := func() {
		wm := n.store.GetWatermark()
		batch, err := n.store.LoadBatch(wm, 2000)
		if err != nil || len(batch) == 0 {
			return
		}
		items := make([]reportItem, 0, len(batch))
		maxLT := wm
		for _, r := range batch {
			items = append(items, reportItem{
				Target: r.Target, Logtime: time.Unix(r.Logtime, 0).Format(time.RFC3339),
				MaxDelay: r.MaxDelay, MinDelay: r.MinDelay, AvgDelay: r.AvgDelay,
				SendPk: r.SendPk, RecvPk: r.RecvPk, LossPk: r.LossPk,
			})
			if r.Logtime > maxLT {
				maxLT = r.Logtime
			}
		}
		batchSeq++
		if err := send(wsReport{Type: "report", Seq: batchSeq, Items: items}); err != nil {
			log.Printf("node: WS 发送失败: %v", err)
			stop()
			return
		}
		pendingMax, pendingSeq, pendingAt = maxLT, batchSeq, time.Now()
	}
	trySend()

	ackTick := time.NewTicker(15 * time.Second)
	defer ackTick.Stop()

	for {
		select {
		case <-done:
			return
		case a := <-ackCh:
			if a.OK {
				if pendingMax > 0 && (a.Seq == 0 || a.Seq == pendingSeq) {
					if err := n.store.SetWatermark(pendingMax); err != nil {
						log.Printf("node: 水位推进失败: %v", err)
					} else {
						log.Printf("node: WS 上报成功 %d 行(批次%d), 水位 -> %s", a.Count, pendingSeq,
							time.Unix(pendingMax, 0).Format("01-02 15:04"))
					}
					pendingMax = 0
					trySend() // 立即补发下一批，按 RTT 排空积压
				}
			} else if pendingMax > 0 && (a.Seq == 0 || a.Seq == pendingSeq) {
				log.Printf("node: WS 上报被拒(%s)，水位不动", a.Err)
				pendingMax = 0 // 等下个 tick 重试同一批
			}
		case <-ackTick.C:
			// B2：ack 迟迟不来（中心 DB 卡住 / ack 在 TLS 层丢失）时不能无限卡住上报，
			// 主动断开让 HTTP 兜底路径接管，节点数据不断流。
			if pendingMax > 0 && time.Since(pendingAt) > wsAckTimeout {
				log.Printf("node: 批次 %d 等待 ack 超过 %s，主动断连降级（数据仍在本地缓冲，水位未推进）",
					pendingSeq, wsAckTimeout)
				return
			}
		case <-sendTick.C:
			if pendingMax == 0 {
				trySend()
			}
		case <-hbTick.C:
			if err := send(n.newHeartbeat()); err != nil {
				return
			}
		}
	}
}

// newHeartbeat 心跳帧：带积压水位、节点估算的时钟偏移、进程启动时刻
func (n *Node) newHeartbeat() wsHeartbeat {
	c := n.conf.Load().(NodeConf)
	return wsHeartbeat{
		Type:          "heartbeat",
		Targets:       len(c.Targets),
		Backlog:       int(n.store.CountAfter(n.store.GetWatermark())),
		Version:       Version,
		ClockOffsetMs: n.clockOff.Load(),
		BootTs:        n.bootTs.Load(),
	}
}

var (
	errNoToken      = errors.New("node: 缺少 token，无法完成 HMAC 握手")
	errBadChallenge = errors.New("node: 收到非法质询帧")
)

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
