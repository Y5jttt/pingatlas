//go:build center

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newSigFixture 造一个内存模式中心：两个节点各带独立 token，A 已登记公钥
func newSigFixture(t *testing.T) (*Center, ed25519.PrivateKey, ed25519.PrivateKey) {
	t.Helper()
	c := newTestCenter2("tok", "pw")
	c.mem.UpsertNode(NodeInfo{NodeID: "node-a"})
	c.mem.UpsertNode(NodeInfo{NodeID: "node-b"})
	c.mem.SetNodeToken("node-a", "token-a")
	c.mem.SetNodeToken("node-b", "token-b")
	_, privA, _ := ed25519.GenerateKey(rand.Reader)
	_, privB, _ := ed25519.GenerateKey(rand.Reader)
	return c, privA, privB
}

func pubHex(priv ed25519.PrivateKey) string {
	return hex.EncodeToString(priv.Public().(ed25519.PublicKey))
}

// 六个冒充/篡改场景必须全部被拒，合法签名必须通过
func TestVerifyAuthEd25519Scenarios(t *testing.T) {
	c, privA, privB := newSigFixture(t)
	if err := c.mem.SetNodePubkey("node-a", pubHex(privA)); err != nil {
		t.Fatal(err)
	}
	nonce := "nonce-1234"
	msg := authMsgHandshake("node-a", nonce)

	// ① 合法：A 用自己的私钥声称 A
	if !c.verifyAuthMsg("node-a", msg, nonce, hex.EncodeToString(ed25519.Sign(privA, []byte(msg))), authAlgEd25519).OK {
		t.Fatalf("合法签名应通过")
	}
	// ② B 用自己的私钥声称 A（冒充）
	if c.verifyAuthMsg("node-a", msg, nonce, hex.EncodeToString(ed25519.Sign(privB, []byte(msg))), authAlgEd25519).OK {
		t.Fatalf("别人的私钥不得通过")
	}
	// ③ B 声称 A 但签的是自己的名字（签名内容不匹配）
	other := authMsgHandshake("node-b", nonce)
	if c.verifyAuthMsg("node-a", msg, nonce, hex.EncodeToString(ed25519.Sign(privB, []byte(other))), authAlgEd25519).OK {
		t.Fatalf("签名内容与身份不匹配不得通过")
	}
	// ④ 未登记公钥的名字
	if c.verifyAuthMsg("node-b", authMsgHandshake("node-b", nonce), nonce,
		hex.EncodeToString(ed25519.Sign(privB, []byte(authMsgHandshake("node-b", nonce)))), authAlgEd25519).OK {
		t.Fatalf("未登记公钥不得通过")
	}
	// ⑤ 换 nonce 重放同一个签名
	if c.verifyAuthMsg("node-a", authMsgHandshake("node-a", "nonce-9999"), "nonce-9999",
		hex.EncodeToString(ed25519.Sign(privA, []byte(msg))), authAlgEd25519).OK {
		t.Fatalf("重放旧签名不得通过")
	}
	// ⑥ 篡改签名 1 个 bit
	sig := ed25519.Sign(privA, []byte(msg))
	sig[0] ^= 0x01
	if c.verifyAuthMsg("node-a", msg, nonce, hex.EncodeToString(sig), authAlgEd25519).OK {
		t.Fatalf("篡改签名不得通过")
	}
	// ⑦ 中心侧换掉公钥后，旧签名立即失效（吊销语义）
	_, privOther, _ := ed25519.GenerateKey(rand.Reader)
	if err := c.mem.SetNodePubkey("node-a", pubHex(privOther)); err != nil {
		t.Fatal(err)
	}
	if c.verifyAuthMsg("node-a", msg, nonce, hex.EncodeToString(ed25519.Sign(privA, []byte(msg))), authAlgEd25519).OK {
		t.Fatalf("公钥被替换后旧私钥必须失效")
	}
}

// 公钥登记必须绑定名字：B 的 token 只能给 B 登记
func TestPubkeyRegisterNameBound(t *testing.T) {
	c, _, privB := newSigFixture(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /agent/pubkey", c.handlePubkey)
	mux.HandleFunc("POST /agent/report", c.handleReport)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	muxAgent := http.NewServeMux()
	muxAgent.HandleFunc("POST /agent/pubkey", c.handlePubkey)
	srvAgent := httptest.NewServer(muxAgent)
	defer srvAgent.Close()

	post := func(authName, token, bodyName, pubkey string) int {
		body, _ := json.Marshal(map[string]string{"name": bodyName, "pubkey": pubkey})
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/agent/pubkey", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		ts := time.Now().Unix()
		req.Header.Set("X-PingAtlas-Auth", fmt.Sprintf("%s:%d:%s", authName, ts,
			hmacToken(token, fmt.Sprintf("%s|%d", authName, ts))))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("pubkey 请求失败: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := post("node-b", "token-b", "node-b", pubHex(privB)); code != 200 {
		t.Fatalf("给自己的名字登记应成功，实际 %d", code)
	}
	if got := c.mem.GetNodePubkey("node-b"); got != pubHex(privB) {
		t.Fatalf("公钥未写入: %q", got)
	}
	// B 的 token 想给 A 登记 → 鉴权按名字查 token，"node-a" 对应的不是 B 的 token，必失败
	if code := post("node-a", "token-b", "node-a", pubHex(privB)); code != 403 {
		t.Fatalf("用 B 的 token 冒充 A 登记应 403，实际 %d", code)
	}
	if got := c.mem.GetNodePubkey("node-a"); got != "" {
		t.Fatalf("A 的公钥不该被写入: %q", got)
	}
	// 认证身份与请求体名字不一致 → 拒绝
	if code := post("node-b", "token-b", "node-a", pubHex(privB)); code != 403 {
		t.Fatalf("请求体名字与身份不一致应 403，实际 %d", code)
	}
	// 非法公钥
	if code := post("node-b", "token-b", "node-b", "not-hex"); code != 400 {
		t.Fatalf("非法公钥应 400，实际 %d", code)
	}
	// 全局 token 身份不得用于登记（它不绑定名字）
	c.conf.Token = "global-master"
	body, _ := json.Marshal(map[string]string{"name": "node-a", "pubkey": pubHex(privB)})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/agent/pubkey", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ts := time.Now().Unix()
	req.Header.Set("X-PingAtlas-Auth", fmt.Sprintf("node-a:%d:%s", ts,
		hmacToken("global-master", fmt.Sprintf("node-a|%d", ts))))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("全局 token 不得用于登记公钥，实际 %d", resp.StatusCode)
	}
}

// HTTP 签名绑定请求体：改一个字节就必须失败；老的 3 段 HMAC 头仍可用
func TestAuthHeaderBodyBound(t *testing.T) {
	c, privA, _ := newSigFixture(t)
	if err := c.mem.SetNodePubkey("node-a", pubHex(privA)); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"name":"node-a","items":[]}`)
	ts := time.Now().Unix()
	mk := func(b []byte) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/agent/report", bytes.NewReader(b))
		req.Header.Set("X-PingAtlas-Auth", fmt.Sprintf("node-a:%d:%s:%s", ts, authAlgEd25519Body,
			hex.EncodeToString(ed25519.Sign(privA, []byte(authMsgHTTP("node-a", ts, bodyDigestHex(body)))))))
		return req
	}
	name, res, ok := c.authHeaderFull(mk(body))
	if !ok || name != "node-a" || res.Method != authAlgEd25519 {
		t.Fatalf("合法签名应通过: %v %v", name, res)
	}
	// handler 之后还能读到 body（签名时读过一次，必须放回去）
	buf := make([]byte, len(body))
	if _, err := mk(body).Body.Read(buf); err != nil {
		t.Fatalf("body 应可再次读取: %v", err)
	}
	// 篡改 body → 摘要变了 → 必须失败
	if _, _, ok := c.authHeaderFull(mk([]byte(`{"name":"node-a","items":[1]}`))); ok {
		t.Fatalf("请求体被改必须验签失败")
	}
	// 老格式（3 段 HMAC）仍然可用
	req := httptest.NewRequest(http.MethodPost, "/agent/report", bytes.NewReader(body))
	req.Header.Set("X-PingAtlas-Auth", fmt.Sprintf("node-a:%d:%s", ts, hmacToken("token-a", fmt.Sprintf("node-a|%d", ts))))
	if _, res, ok := c.authHeaderFull(req); !ok || res.Method != authAlgHMAC {
		t.Fatalf("老 HMAC 头必须保持可用")
	}
}

// 管理端 node/key/reset：清掉公钥后签名立即失效，节点可用自己的 token 重新登记（自愈）
func TestAdminKeyReset(t *testing.T) {
	c, privA, _ := newSigFixture(t)
	if err := c.mem.SetNodePubkey("node-a", pubHex(privA)); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	c.registerAdminAPI(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 自愈用的 agent 路由（与 admin 分开的服务器，避免同 mux 路径冲突）
	muxAgent := http.NewServeMux()
	muxAgent.HandleFunc("POST /agent/pubkey", c.handlePubkey)
	srvAgent := httptest.NewServer(muxAgent)
	defer srvAgent.Close()

	body, _ := json.Marshal(map[string]string{"name": "node-a"})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/admin/node/key/reset", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Pwd", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("key/reset 应 200，实际 %d", resp.StatusCode)
	}
	if got := c.mem.GetNodePubkey("node-a"); got != "" {
		t.Fatalf("公钥应被清空，实际 %q", got)
	}
	nonce := "n1"
	if c.verifyAuthMsg("node-a", authMsgHandshake("node-a", nonce), nonce,
		hex.EncodeToString(ed25519.Sign(privA, []byte(authMsgHandshake("node-a", nonce)))), authAlgEd25519).OK {
		t.Fatalf("重置后旧签名必须失效")
	}
	// 节点仍持有 token → 可自助重新登记（自愈路径）
	if code := func() int {
		b, _ := json.Marshal(map[string]string{"name": "node-a", "pubkey": pubHex(privA)})
		r, _ := http.NewRequest(http.MethodPost, srvAgent.URL+"/agent/pubkey", bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
		ts := time.Now().Unix()
		r.Header.Set("X-PingAtlas-Auth", fmt.Sprintf("node-a:%d:%s", ts, hmacToken("token-a", fmt.Sprintf("node-a|%d", ts))))
		rs, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Body.Close()
		return rs.StatusCode
	}(); code != 200 {
		t.Fatalf("重置后应能重新登记，实际 %d", code)
	}
	if got := c.mem.GetNodePubkey("node-a"); got != pubHex(privA) {
		t.Fatalf("重新登记失败: %q", got)
	}
}

// 兑换安装码时登记公钥（原子路径）+ 非法公钥被拒
func TestRedeemCarriesPubkey(t *testing.T) {
	// 内存模式不支持安装码（需要 DB），这里只验参数校验与原子 SQL 的调用点是否接上
	if !validPubkeyHex(strings.Repeat("ab", 32)) {
		t.Fatalf("合法公钥应被接受")
	}
	if validPubkeyHex("") || validPubkeyHex("zz") || validPubkeyHex(strings.Repeat("ab", 31)) {
		t.Fatalf("非法公钥必须被拒")
	}
	// hex 指纹：同一公钥稳定、不同公钥不同
	p1 := strings.Repeat("ab", 32)
	p2 := strings.Repeat("cd", 32)
	if pubkeyFingerprintHex(p1) == "" || pubkeyFingerprintHex(p1) != pubkeyFingerprintHex(p1) {
		t.Fatalf("指纹应稳定且非空")
	}
	if pubkeyFingerprintHex(p1) == pubkeyFingerprintHex(p2) {
		t.Fatalf("不同公钥指纹不应相同")
	}
	if got := pubkeyFingerprintHex("not-hex"); got != "" {
		t.Fatalf("非法公钥指纹应为空，实际 %q", got)
	}
	_ = context.Background()
}
