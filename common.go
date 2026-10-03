package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
)

// 共享类型与工具（node / center 两个构建都包含）

// Version 构建版本号：节点靠它判断是否需要自升级（中心下发 cmd{update,ver,sha256}），
// 所以每次发布新二进制都必须改这里。
const Version = "0.3.1"

// Target 一个探测目标：alias 是历史数据归属名（换 IP 不改历史），ip 是当前探测地址。
// 它是节点/中心之间唯一的共同数据契约之一（config 帧、/agent/config 线格式）。
type Target struct {
	Alias string `json:"alias"`
	IP    string `json:"ip"`
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// hmacToken token 永不上线：线上只传 HMAC-SHA256(token, nonce)，
// nonce 一次性（WS 质询）或时间戳（HTTP 头），同一 token 每次发出的凭据都不同。
func hmacToken(token, nonce string) string {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte(nonce))
	return hex.EncodeToString(m.Sum(nil))
}

// ---- 节点凭据（对称 HMAC 与 Ed25519 签名并存，便于平滑迁移）----
const (
	// authAlgHMAC 老的共享密钥方式（对称：中心也持有密钥）
	authAlgHMAC = "hmac-sha256"
	// authAlgEd25519 WS 握手的签名方式（非对称：中心只存公钥）
	authAlgEd25519 = "ed25519"
	// authAlgEd25519Body HTTP 上报的签名方式：签名覆盖请求体摘要（防篡改）
	authAlgEd25519Body = "ed25519b"
)

// authMsgHandshake 握手签名的消息：把节点名与一次性 nonce 一起签进去，
// 于是"换名字"或"重放旧签名"都无法通过。
func authMsgHandshake(name, nonce string) string { return name + "|" + nonce }

// algLabel 日志里好读的鉴权方式名（中心与节点都会打日志，故放共享文件）
func algLabel(alg string) string {
	if alg == authAlgEd25519 || alg == authAlgEd25519Body {
		return "Ed25519 签名"
	}
	return "HMAC"
}

// authMsgHTTP HTTP 请求签名的消息：名字 + 时间戳 + 请求体摘要。
// 比老的 HMAC 方案多绑定了请求体（老方案只签时间戳，请求体只靠 TLS 保护）。
func authMsgHTTP(name string, ts int64, bodyDigest string) string {
	return fmt.Sprintf("%s|%d|%s", name, ts, bodyDigest)
}

// bodyDigestHex 请求体摘要（空体也要算，保持确定性）
func bodyDigestHex(body []byte) string {
	s := sha256.Sum256(body)
	return hex.EncodeToString(s[:])
}

// pubkeyFingerprint 公钥指纹：对**原始 32 字节**公钥算 SHA-256，取前 16 位十六进制。
// 只用于人工核对（面板 vs 节点日志），校验逻辑一律用完整公钥。
func pubkeyFingerprint(pub []byte) string {
	s := sha256.Sum256(pub)
	return hex.EncodeToString(s[:])[:16]
}

// ---- 节点/中心共享的 WS 线协议 ----
//
// 握手：连接建立后中心先发 challenge（一次性 nonce），节点回 auth（HMAC），通过才算连上。
// 上行 heartbeat(60s 含 backlog/时钟偏移) / report(30s 单批在途)；下行 ack(先落库后回执) /
// config(秒推) / cmd(restart | update{ver,sha256})。

type reportItem struct {
	Target   string  `json:"target"`
	Logtime  string  `json:"logtime"` // RFC3339
	MaxDelay float64 `json:"maxdelay"`
	MinDelay float64 `json:"mindelay"`
	AvgDelay float64 `json:"avgdelay"`
	SendPk   int     `json:"sendpk"`
	RecvPk   int     `json:"revcpk"`
	LossPk   int     `json:"losspk"`
}

type wsChallenge struct {
	Type  string `json:"type"`
	Nonce string `json:"nonce"`
}

type wsAuth struct {
	Type string `json:"type"`
	Name string `json:"name"`
	MAC  string `json:"mac,omitempty"`  // 旧字段：HMAC-SHA256 凭据（兼容老节点）
	Alg  string `json:"alg,omitempty"`  // hmac-sha256 | ed25519
	Cred string `json:"cred,omitempty"` // 新字段：按 Alg 解释的凭据（HMAC 串或签名）
}

type wsHeartbeat struct {
	Type          string `json:"type"`
	Targets       int    `json:"targets"`
	Backlog       int    `json:"backlog"`                   // SQLite 未确认行数（上行堵了的早期信号）
	Version       string `json:"version"`
	ClockOffsetMs int64  `json:"clock_offset_ms,omitempty"` // 节点估算的时钟偏移（server_ts - 节点时刻）
	BootTs        int64  `json:"boot_ts,omitempty"`         // 节点进程启动时刻：重启回执的判据（网络抖动重连 boot_ts 不变）
}

type wsReport struct {
	Type  string       `json:"type"`
	Seq   int64        `json:"seq"` // 批次号：ack 原样回显，节点据此识别"我那批到没到"
	Items []reportItem `json:"items"`
}

type wsAck struct {
	Type     string `json:"type"`
	Seq      int64  `json:"seq"`
	OK       bool   `json:"ok"`
	Count    int64  `json:"count"`
	Rejected int    `json:"rejected,omitempty"` // 被中心丢弃的非法行数（目标已从表里删除等）
	Err      string `json:"err,omitempty"`
	ServerTs int64  `json:"server_ts,omitempty"` // 中心回执时刻（unix 毫秒），节点借此估算时钟偏移
}

type wsConfig struct {
	Type    string   `json:"type"`
	Version int64    `json:"version"`
	Targets []Target `json:"targets"`
	Clear   bool     `json:"clear,omitempty"` // 目标表被清空是合法状态（见 configResp.Clear）
}

type wsCmd struct {
	Type   string `json:"type"`
	Action string `json:"action"`        // restart | update
	Ver    string `json:"ver,omitempty"` // update：目标版本
	Sha256 string `json:"sha256,omitempty"` // update：二进制校验和
}

// /agent/config 与 config 帧共用的响应结构
//
// Clear 区分两种「空列表」语义，这是 P1-3 的关键：
//   - Clear=false + Targets=[] => 异常/未就绪，节点应保持原有目标不变
//   - Clear=true  + Targets=[] => 管理员确实清空了目标表，节点必须停止探测
// 旧实现两边都要求 len(Targets)>0 才应用，导致「目标表被清空」这个合法终态
// 永远推不下去 —— 节点会继续探测早已删除的目标，且每轮数据都被中心按
// 「不在目标表内」拒收，Backlog 恒不归零。
type configResp struct {
	Version int64    `json:"version"`
	Targets []Target `json:"targets"`
	Clear   bool     `json:"clear,omitempty"`
}
func urlQueryEscape(s string) string {
	var b bytes.Buffer
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' || ch == '.' {
			b.WriteByte(ch)
		} else {
			b.WriteString("%")
			const hex = "0123456789ABCDEF"
			b.WriteByte(hex[ch>>4])
			b.WriteByte(hex[ch&0x0f])
		}
	}
	return b.String()
}
func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// mustJSON 小工具：序列化失败只可能因非法字段，崩溃即 bug
func mustJSON(v interface{}) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}
type reportReq struct {
	Name  string       `json:"name"`
	Seq   int64        `json:"seq"`
	Items []reportItem `json:"items"`
}

type heartbeatReq struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	Targets       int    `json:"targets"`
	Backlog       int    `json:"backlog"`
	ClockOffsetMs int64  `json:"clock_offset_ms,omitempty"`
	BootTs        int64  `json:"boot_ts,omitempty"`
}

type reportResp struct {
	OK       bool  `json:"ok"`
	Seq      int64 `json:"seq"`
	Count    int64 `json:"count"`
	Rejected int   `json:"rejected,omitempty"`
	Err      string `json:"err,omitempty"`
	ServerTs int64 `json:"server_ts,omitempty"`
}
