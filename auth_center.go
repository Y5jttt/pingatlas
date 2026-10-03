//go:build center

package main

// ---------- 鉴权与限流（center 端） ----------
//
// 节点鉴权：HMAC-SHA256 质询-应答。token 永不出现在 URL / 报文里：
//   WS：    连接后中心发 challenge{nonce}，节点回 auth{mac=HMAC(token,nonce)}
//   HTTP：  请求头 X-PingAtlas-Auth: name:ts:mac，mac=HMAC(token, name|ts)，ts 允许 ±180s
// nonce 每次不同 / ts 每秒不同 ⇒ 线上凭据每次都不一样；HMAC 单向 ⇒ 抓包推不出 token。
//
// 管理面板：X-Admin-Pwd 请求头（custom.js 的 fetch 拦截自动从 ?pwd= 迁移），
// 按 X-Forwarded-For 真实 IP 计失败次数，5 次失败锁 30 分钟。

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	xffAuthWindow = 180 * time.Second // HTTP 时间戳容忍窗口（覆盖分钟级时钟漂移）
	bruteMaxFails = 5
	bruteLockFor  = 30 * time.Minute
	// maxAgentBodyBytes 节点上报请求体上限（与 handler 里的 MaxBytesReader 对齐）
	maxAgentBodyBytes = 1 << 20
	redeemMaxTry  = 20 // 安装码兑换：同一 IP 20 次失败锁 30 分钟
)

// failLimiter 简单失败计数锁定器（内存态，center 重启清零）
type failLimiter struct {
	mu sync.Mutex
	m  map[string]*failState
}

type failState struct {
	fails int
	until time.Time
	seen  time.Time // 最后一次失败时刻，janitor 据此回收
}

func newFailLimiter() *failLimiter {
	return &failLimiter{m: map[string]*failState{}}
}

// allowed 是否放行（未锁定 true）
func (l *failLimiter) allowed(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.m[key]
	if !ok {
		return true
	}
	return time.Now().After(s.until)
}

func (l *failLimiter) fail(key string, max int, lock time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.m[key]
	if s == nil {
		s = &failState{}
		l.m[key] = s
	}
	s.fails++
	s.seen = time.Now()
	if s.fails >= max {
		s.until = time.Now().Add(lock)
		s.fails = 0 // 锁定期满后重新计数
	}
}

func (l *failLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, key)
}

// janitor 定期回收失败记录。限流键来自客户端，没有回收就会随攻击流量无界增长
// （攻击者每次换一个源 IP / XFF 就多一个 key）。
//
// 已知取舍：计数阶段（还没触发锁定）的记录只保留一小时，因此"每小时 ≤4 次失败"
// 的慢速爆破可以长期不触发锁定（≈96 次/天）。对强口令不构成威胁；
// 若要更严，把这一小时的窗口提到 24h（代价是内存里多留一批计数）。
func (l *failLimiter) janitor() {
	for {
		time.Sleep(10 * time.Minute)
		now := time.Now()
		l.mu.Lock()
		for k, s := range l.m {
			switch {
			case !s.until.IsZero() && now.After(s.until):
				delete(l.m, k) // 锁定期已过，无需继续占位
			case !s.seen.IsZero() && now.Sub(s.seen) > time.Hour:
				delete(l.m, k) // 计数阶段闲置超过一小时
			}
		}
		l.mu.Unlock()
	}
}

// defaultTrustedProxies 默认可信代理：回环地址。
// center 与 nginx 同机部署时直连对端恒为 127.0.0.1；此时才值得采信 XFF。
var defaultTrustedProxies = []string{"127.0.0.0/8", "::1/128"}

// realIP 取真实客户端 IP（限流键 / 安全日志用）。
//
// 安全前提：X-Forwarded-For 是客户端可以随便写的头部。旧实现无条件取它的**第一跳**，
// 于是攻击者每次换一个 XFF 值就是一把新锁 —— 「5 次失败锁 30 分钟」完全失效，
// 反过来用真管理员的 IP 发 5 次错密码还能把人锁在门外（DoS）。
//
// 现在的规则：
//   - 直连对端不是可信代理 ⇒ 只认 RemoteAddr，XFF 一律忽略（伪造无效）；
//   - 直连对端是可信代理 ⇒ 取 XFF 的**最后一跳**。nginx 的
//     $proxy_add_x_forwarded_for 会把真实对端 IP 追加在列表末尾，而开头的值
//     正是攻击者塞进来的。
//
// 可信代理默认为回环；跨机反代可在 center.json 的 TrustedProxies 里追加 IP/CIDR。
func (c *Center) realIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !c.isTrustedProxy(host) {
		return host
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return host
	}
	parts := strings.Split(xff, ",")
	last := strings.TrimSpace(parts[len(parts)-1])
	if net.ParseIP(last) == nil {
		return host // 末跳不是合法 IP，退回直连对端
	}
	return last
}

func (c *Center) isTrustedProxy(ip string) bool {
	p := net.ParseIP(ip)
	if p == nil {
		return false
	}
	c.mu.Lock()
	extra := append([]string(nil), c.conf.TrustedProxies...)
	c.mu.Unlock()
	for _, s := range append(append([]string(nil), defaultTrustedProxies...), extra...) {
		if strings.Contains(s, "/") {
			if _, n, err := net.ParseCIDR(s); err == nil && n.Contains(p) {
				return true
			}
			continue
		}
		if q := net.ParseIP(s); q != nil && q.Equal(p) {
			return true
		}
	}
	return false
}

// verifyMAC 校验节点身份。nonce 是**原始**随机量/时间戳，函数内部统一拼成
// `name|nonce` 再签名 —— 调用方不要预先把 name 拼进 nonce，否则会双重绑定。
//
// P1-9 修复：全局 token 分支原先**不绑定 name**（`HMAC(gt, nonce)`）——
// 拿到全局 token 就能以任意节点名通过校验、往别人的 node_id 写数据，
// A1 的越权修复在它面前完全失效。现在两条路都必须签进 name。
//
// 两条路的区别：
//   - 全局 token：只接受 `name|nonce`。它是"万能钥匙"，不绑定 name 就等于
//     任意节点身份，必须严格。
//   - 节点独立 token：name 来自 token 所属的那行记录（GetNodeToken(name)），
//     本就无法冒充别人，故额外兼容裸 `nonce`，让升级窗口内新旧节点能共存。
// verifyMAC 兼容入口：HMAC 校验（消息绑定 name|nonce，兼容裸 nonce）
func (c *Center) verifyMAC(name, nonce, mac string) bool {
	ok, _ := c.verifyHMACOf(name, authMsgHandshake(name, nonce), nonce, mac)
	return ok
}

// validPubkeyHex 公钥格式校验：32 字节的 hex（Ed25519）
func validPubkeyHex(s string) bool {
	b, err := hex.DecodeString(strings.TrimSpace(s))
	return err == nil && len(b) == ed25519.PublicKeySize
}

// authResult 鉴权结果：是否通过、用了哪种方式、是否依赖了"全局万能钥匙"。
// UsedGlobalToken 很重要：全局 token 不绑定节点名，凡是"把某样东西写到某个名字下"
// 的操作（例如登记公钥）都必须拒绝这种身份。
type authResult struct {
	OK              bool
	Method          string // hmac-sha256 | ed25519
	UsedGlobalToken bool
}

// verifyHMACOf 校验 HMAC 凭据：先比"消息绑定"的，再（可选）比裸值以兼容升级窗口。
// 返回 (是否通过, 是否用了全局 token)。
func (c *Center) verifyHMACOf(name, msg, bare, mac string) (bool, bool) {
	if name == "" || msg == "" || mac == "" {
		return false, false
	}
	if gt := c.globalToken(); gt != "" &&
		subtle.ConstantTimeCompare([]byte(hmacToken(gt, msg)), []byte(mac)) == 1 {
		return true, true
	}
	var tok string
	if c.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		t, err := c.db.GetNodeToken(ctx, name)
		if err != nil {
			return false, false
		}
		tok = t
	} else if c.mem != nil {
		tok = c.mem.GetNodeToken(name)
	}
	if tok == "" {
		return false, false
	}
	if subtle.ConstantTimeCompare([]byte(hmacToken(tok, msg)), []byte(mac)) == 1 {
		return true, false
	}
	if bare != "" && subtle.ConstantTimeCompare([]byte(hmacToken(tok, bare)), []byte(mac)) == 1 {
		return true, false
	}
	return false, false
}

// verifySigOf 用"该名字登记的公钥"验签。中心自己没有任何可用于冒充的密钥。
func (c *Center) verifySigOf(name, msg, sigHex string) bool {
	if name == "" || msg == "" || sigHex == "" {
		return false
	}
	var pubHex string
	if c.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		p, err := c.db.GetNodePubkey(ctx, name)
		if err != nil {
			return false
		}
		pubHex = p
	} else if c.mem != nil {
		pubHex = c.mem.GetNodePubkey(name)
	}
	pub, err := hex.DecodeString(pubHex)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false // 未登记公钥 → 只能用 HMAC 兼容通道
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub), []byte(msg), sig)
}

// verifyAuthMsg 统一入口：按 alg 选验签或 HMAC。alg 为空按 HMAC 处理（兼容老节点）。
func (c *Center) verifyAuthMsg(name, msg, bare, cred, alg string) authResult {
	switch alg {
	case authAlgEd25519, authAlgEd25519Body:
		return authResult{OK: c.verifySigOf(name, msg, cred), Method: authAlgEd25519}
	default:
		ok, usedGlobal := c.verifyHMACOf(name, msg, bare, cred)
		return authResult{OK: ok, Method: authAlgHMAC, UsedGlobalToken: usedGlobal}
	}
}

// authHeader 校验节点 HTTP 请求头，返回**校验通过的节点名**与鉴权细节。
// handler 必须用这个 name，绝不能用 body 里的自报名字，否则持有合法 token 的节点
// 可以冒充他人把数据写进别人的 node_id（A1 越权）。
//
// 两种头格式：
//
//	X-PingAtlas-Auth: name:ts:mac                 ← 老格式：HMAC(token, "name|ts")
//	X-PingAtlas-Auth: name:ts:alg:cred            ← 新格式：alg=ed25519b 时 cred 是对
//	                                          "name|ts|sha256(body)" 的签名（顺带绑定了请求体）
func (c *Center) authHeaderFull(r *http.Request) (string, authResult, bool) {
	h := r.Header.Get("X-PingAtlas-Auth")
	if h == "" {
		h = r.Header.Get("X-SP2-Auth") // 兼容未升级的旧节点（迁移期）
	}
	parts := strings.SplitN(h, ":", 4)
	var name, tsStr, alg, cred string
	switch len(parts) {
	case 3:
		name, tsStr, cred = parts[0], parts[1], parts[2]
		alg = authAlgHMAC
	case 4:
		name, tsStr, alg, cred = parts[0], parts[1], parts[2], parts[3]
	default:
		return "", authResult{}, false
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil || name == "" {
		return "", authResult{}, false
	}
	if d := time.Since(time.Unix(ts, 0)); d > xffAuthWindow || d < -xffAuthWindow {
		return "", authResult{}, false // 时钟偏差超窗口拒绝；配合 ack server_ts 的偏移观测可发现此类节点
	}
	switch alg {
	case authAlgEd25519Body:
		// 签名覆盖请求体：必须先把 body 读出来算摘要，再放回去给 handler 解析
		body, rerr := io.ReadAll(io.LimitReader(r.Body, maxAgentBodyBytes+1))
		if rerr != nil || len(body) > maxAgentBodyBytes {
			return "", authResult{}, false
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		res := c.verifyAuthMsg(name, authMsgHTTP(name, ts, bodyDigestHex(body)), "", cred, alg)
		if !res.OK {
			return "", res, false
		}
		return name, res, true
	default:
		res := c.verifyAuthMsg(name, authMsgHandshake(name, tsStr), tsStr, cred, alg)
		if !res.OK {
			return "", res, false
		}
		return name, res, true
	}
}

// authHeader 兼容旧调用点：只关心"通过与否 + 节点名"
func (c *Center) authHeader(r *http.Request) (string, bool) {
	name, _, ok := c.authHeaderFull(r)
	return name, ok
}

// globalToken 读全局 token（B7：与写侧同锁，消除 data race）
func (c *Center) globalToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conf.Token
}

// adminPwd 读管理密码（与节点 token 彻底分离；老配置由 loadCenterConf 一次性迁移）
func (c *Center) adminPwd() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conf.AdminPwd
}
