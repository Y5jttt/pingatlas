//go:build node

package main

import (
	crand "crypto/rand"
	"encoding/binary"
	"log"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// ---------- 设计要点（仿 agent 高速探测机制）----------
// 1. 常驻 RAW socket，收发解耦：发送 goroutine 只管发，接收 goroutine 收到回包
//    按 (icmp_id, seq) 查"在飞表"记账，天然支持高并发目标。
// 2. icmp_id 固定 0x5350("SP")，seq 全局原子递增 16 位；payload 前 3 字节 magic
//    "SP2" + 4 字节轮次 nonce，双重校验防止串包/上一轮残留应答污染本轮统计。
// 3. 错误报文（type 3 不可达 / type 11 超时）内嵌原始请求，解析出 seq 后照样
//    记账——目标主机明确回绝的也会从在飞表里移除，不占超时等待。
// 4. 后台 sweeper 定期清理超时在飞项，防泄漏。
// 5. 一轮 = 每目标 20 包、间隔 50ms，发出后等 3s 收尾再聚合，输出与旧 fping
//    版本完全相同的 8 字段（maxdelay/mindelay/avgdelay/sendpk/revcpk/losspk）。

const (
	icmpID       = 0x5350 // "SP"
	magic0       = 0x53   // 'S'
	magic1       = 0x50   // 'P'
	magic2       = 0x32   // '2'
	inflightTTL  = 10 * time.Second
	defaultCount = 20
)

// RoundResult 一轮单目标的聚合结果（字段名沿用旧版 MySQL 表）
type RoundResult struct {
	Logtime  int64   // unix 秒（整分钟对齐）
	Target   string  // alias
	MaxDelay float64 // ms
	MinDelay float64 // ms
	AvgDelay float64 // ms
	SendPk   int
	RecvPk   int
	LossPk   int
}

type inflight struct {
	acc    *roundAcc
	nonce  uint32 // 发出时的轮次 nonce，应答比对防串包/跨轮残留
	dst    net.IP // 期望的应答源地址（只认这个源的 echo reply）
	sentAt time.Time
}

type roundAcc struct {
	target  string
	nonce   uint32
	sent    int
	recv    int
	min     float64
	max     float64
	sum     float64
	unreach int // type3 目标不可达次数
	expired int // type11 传输超时次数
	mu      sync.Mutex
}

func (a *roundAcc) record(rtt float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recv++
	a.sum += rtt
	if rtt < a.min {
		a.min = rtt
	}
	if rtt > a.max {
		a.max = rtt
	}
}

func (a *roundAcc) finish(logtime int64) RoundResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := RoundResult{
		Logtime: logtime, Target: a.target,
		SendPk: a.sent, RecvPk: a.recv, LossPk: a.sent - a.recv,
	}
	if a.recv > 0 {
		r.MinDelay = round2(a.min)
		r.MaxDelay = round2(a.max)
		r.AvgDelay = round2(a.sum / float64(a.recv))
	} else {
		r.MinDelay = 0
		r.MaxDelay = 0
		r.AvgDelay = 0
	}
	return r
}


// Engine 常驻 ICMP 引擎
type Engine struct {
	conn     *icmp.PacketConn
	seq      atomic.Uint32
	inflight sync.Map // uint32(seq) -> *inflight
}

// unknownAddrOnce 让源地址类型告警只打一次（避免刷日志）
var unknownAddrOnce sync.Once

// randUint32 用密码学随机数作为本轮 nonce。
// 旧实现是自增计数器（1,2,3…），而应答匹配只看 id/seq/magic/nonce：
// **被探测的目标本身**就能在收到的 echo request 里读到 nonce，进而推算后续 seq，
// 于是可以伪造任意其它目标的应答与 RTT（测绘数据的可信度直接失效）。
func randUint32() uint32 {
	var b [4]byte
	if _, err := crand.Read(b[:]); err != nil {
		return uint32(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint32(b[:])
}

// NewEngine 创建引擎并绑定 RAW socket。srcIP 为空绑定 0.0.0.0。
func NewEngine(srcIP string) (*Engine, error) {
	addr := srcIP
	if addr == "" {
		addr = "0.0.0.0"
	}
	conn, err := icmp.ListenPacket("ip4:icmp", addr)
	if err != nil {
		return nil, err
	}
	e := &Engine{conn: conn}
	go e.recvLoop()
	go e.sweepLoop()
	return e, nil
}

// Start 保持接口占位（recv/sweep 已在 NewEngine 启动）
func (e *Engine) Start() {}

// RunRound 执行一轮探测并返回全部目标结果（阻塞直至收尾超时）
func (e *Engine) RunRound(targets []Target, count int, gap, waitAfter time.Duration, maxConc int) []RoundResult {
	if count <= 0 {
		count = defaultCount
	}
	if gap <= 0 {
		gap = 50 * time.Millisecond
	}
	if maxConc <= 0 {
		maxConc = 128
	}
	logtime := time.Now().Unix() / 60 * 60 // 整分钟
	nonce := randUint32()

	results := make([]RoundResult, 0, len(targets))
	var (
		wg     sync.WaitGroup
		resMu  sync.Mutex
		sem    = make(chan struct{}, maxConc)
	)
	for _, tg := range targets {
		tg := tg
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			acc := &roundAcc{target: tg.Alias, nonce: nonce, min: math.MaxFloat64}
			e.probeTarget(tg.IP, acc, count, gap, nonce)
			time.Sleep(waitAfter) // 给最后一个包留应答窗口
			resMu.Lock()
			results = append(results, acc.finish(logtime))
			resMu.Unlock()
		}()
	}
	wg.Wait()
	return results
}

// probeTarget 向单个目标连发 count 个 echo，间隔 gap
func (e *Engine) probeTarget(ipStr string, acc *roundAcc, count int, gap time.Duration, nonce uint32) {
	ip := net.ParseIP(ipStr)
	if ip == nil || ip.To4() == nil {
		return
	}
	dst := &net.IPAddr{IP: ip.To4()}
	payload := make([]byte, 8)
	payload[0], payload[1], payload[2] = magic0, magic1, magic2
	binary.BigEndian.PutUint32(payload[4:8], nonce)

	for i := 0; i < count; i++ {
		seq := int(e.seq.Add(1) & 0xffff)
		if seq == 0 { // 跳过 0，避免与空值混淆
			seq = int(e.seq.Add(1) & 0xffff)
		}
		m := icmp.Message{
			Type: ipv4.ICMPTypeEcho, Code: 0,
			Body: &icmp.Echo{ID: icmpID, Seq: seq, Data: payload},
		}
		wb, err := m.Marshal(nil)
		if err != nil {
			continue
		}
		// 先入在飞表再发包：否则极速应答（<1µs）会在 Store 之前到达而被丢弃、被误记为丢包。
		// 写失败（极少见）时下一轮 sweep 会清掉，不影响记账正确性。
		e.inflight.Store(uint32(seq), &inflight{acc: acc, nonce: nonce, dst: dst.IP, sentAt: time.Now()})
		if _, err := e.conn.WriteTo(wb, dst); err != nil {
			e.inflight.Delete(uint32(seq))
			time.Sleep(gap)
			continue
		}
		acc.mu.Lock()
		acc.sent++
		acc.mu.Unlock()
		time.Sleep(gap)
	}
}

// recvLoop 接收应答并记账（收发解耦的核心）
func (e *Engine) recvLoop() {
	buf := make([]byte, 1600)
	for {
		n, src, err := e.conn.ReadFrom(buf)
		if err != nil {
			log.Printf("engine: recv error: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		srcIP := ipOf(src)
		now := time.Now()
		m, err := icmp.ParseMessage(1 /* ICMPv4 */, buf[:n])
		if err != nil {
			continue
		}
		switch m.Type {
		case ipv4.ICMPTypeEchoReply:
			echo, ok := m.Body.(*icmp.Echo)
			if !ok || echo.ID != icmpID {
				continue
			}
			// magic+nonce 双重校验：payload 前 3 字节 magic + 4 字节轮次 nonce
			// 与在飞项不一致 = 上一轮残留应答（seq 回绕撞号），忽略且不清在飞项
			if len(echo.Data) < 8 || echo.Data[0] != magic0 || echo.Data[1] != magic1 || echo.Data[2] != magic2 {
				continue
			}
			if v, ok := e.inflight.Load(uint32(echo.Seq)); ok {
				inf := v.(*inflight)
				if binary.BigEndian.Uint32(echo.Data[4:8]) != inf.nonce {
					continue
				}
				// 源地址必须就是我们探测的那个目标。raw socket 上不做这层校验的话，
				// 任何能猜中 (seq, nonce) 的主机都能替任意目标"报平安"并伪造 RTT。
				if !ipEqual(inf.dst, srcIP) {
					continue
				}
				if v2, ok2 := e.inflight.LoadAndDelete(uint32(echo.Seq)); ok2 {
					inf2 := v2.(*inflight)
					inf2.acc.record(now.Sub(inf2.sentAt).Seconds() * 1000)
				}
			}
		case ipv4.ICMPTypeDestinationUnreachable, ipv4.ICMPTypeTimeExceeded:
			// 注意：这类报文来自路径上的路由器，源地址本来就不是目标本身，
			// 所以这里**不做**源地址校验，只靠 nonce + magic 防串包。
			var data []byte
			switch b := m.Body.(type) {
			case *icmp.DstUnreach:
				data = b.Data
			case *icmp.TimeExceeded:
				data = b.Data
			default:
				continue
			}
			seq, nonce, ok := parseEmbeddedSeq(data)
			if !ok {
				continue
			}
			if v, ok := e.inflight.Load(uint32(seq)); ok {
				inf := v.(*inflight)
				if nonce != inf.nonce {
					continue
				}
				if v2, ok2 := e.inflight.LoadAndDelete(uint32(seq)); ok2 {
					acc := v2.(*inflight).acc
					acc.mu.Lock()
					if m.Type == ipv4.ICMPTypeDestinationUnreachable {
						acc.unreach++
					} else {
						acc.expired++
					}
					acc.mu.Unlock()
				}
			}
		default:
			// 其他类型（重定向/参数问题等）不处理
		}
	}
}

// warnUnknownAddr 首次遇到无法归一化的源地址类型时告警一次。
// 返回 nil 会让源地址校验把所有回包判成"源不匹配"，表现为**全丢包**（极难排查），
// 所以这里必须留下痕迹而不是静默。
func warnUnknownAddr(a net.Addr) {
	unknownAddrOnce.Do(func() {
		log.Printf("engine: 警告 无法把源地址类型 %T(%v) 解析成 IP，回包可能被全部丢弃，请检查平台兼容性", a, a)
	})
}

// ipOf 把 ReadFrom 返回的地址归一成 net.IP（raw socket 下通常是 *net.IPAddr）。
func ipOf(a net.Addr) net.IP {
	switch v := a.(type) {
	case *net.IPAddr:
		return v.IP
	case *net.UDPAddr:
		return v.IP
	case *net.TCPAddr:
		return v.IP
	}
	if a == nil {
		return nil
	}
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		host = a.String()
	}
	ip := net.ParseIP(host)
	if ip == nil {
		warnUnknownAddr(a)
	}
	return ip
}

// ipEqual 比较两个地址是否指向同一主机（按 4 字节归一比较，兼容 IPv4-mapped 写法）。
func ipEqual(a, b net.IP) bool {
	if a == nil || b == nil {
		return false
	}
	a4, b4 := a.To4(), b.To4()
	if a4 != nil && b4 != nil {
		return a4.Equal(b4)
	}
	return a.Equal(b)
}

// parseEmbeddedSeq 从错误报文内嵌的"原始 IP 包 + 内嵌 ICMP"中提取 seq 与轮次 nonce
func parseEmbeddedSeq(data []byte) (int, uint32, bool) {
	if len(data) < 20+8 {
		return 0, 0, false
	}
	ihl := int(data[0]&0x0f) * 4
	if ihl < 20 || len(data) < ihl+8+8 {
		return 0, 0, false
	}
	inner, err := icmp.ParseMessage(1, data[ihl:ihl+8+8])
	if err != nil {
		return 0, 0, false
	}
	echo, ok := inner.Body.(*icmp.Echo)
	if !ok || echo.ID != icmpID {
		return 0, 0, false
	}
	if len(echo.Data) < 8 || echo.Data[0] != magic0 || echo.Data[1] != magic1 || echo.Data[2] != magic2 {
		return 0, 0, false
	}
	return echo.Seq, binary.BigEndian.Uint32(echo.Data[4:8]), true
}

// sweepLoop 后台清理超过 TTL 的在飞项（目标不回包的场景）
func (e *Engine) sweepLoop() {
	for {
		time.Sleep(5 * time.Second)
		cutoff := time.Now().Add(-inflightTTL)
		e.inflight.Range(func(key, value interface{}) bool {
			inf := value.(*inflight)
			if inf.sentAt.Before(cutoff) {
				e.inflight.Delete(key)
			}
			return true
		})
	}
}

// Close 关闭 socket
func (e *Engine) Close() error {
	return e.conn.Close()
}
