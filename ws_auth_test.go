//go:build center

package main

// 本地集成测试：WS 质询-应答握手 + HTTP 头鉴权 + 管理端防爆破。
// 运行：go test -tags center -run 'TestWSHandshake|TestHTTPAuth|TestAdminBrute' -v

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func newTestCenter(tok, apwd string) *Center {
	c := &Center{
		conf:     CenterConf{Token: tok, AdminPwd: apwd},
		lastSeen: map[string]time.Time{},
		lim:      newFailLimiter(),
		cmds:     map[string]*cmdState{},
	}
	c.mem = NewMemStore()
	c.hub = NewHub(c)
	return c
}

func TestWSHandshake(t *testing.T) {
	tok := "tok-ws-test-123"
	c := newTestCenter(tok, "adminpw")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent/ws", c.hub.ServeWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/agent/ws"

	// ① 正确 MAC：握手通过并收到 config 推送
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read challenge: %v", err)
	}
	var ch wsChallenge
	if json.Unmarshal(raw, &ch) != nil || ch.Type != "challenge" || ch.Nonce == "" {
		t.Fatalf("bad challenge: %s", raw)
	}
	_ = conn.WriteMessage(websocket.TextMessage,
		mustJSON(wsAuth{Type: "auth", Name: "probe-node", MAC: hmacToken(tok, "probe-node|"+ch.Nonce)}))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err = conn.ReadMessage()
	if err != nil {
		t.Fatalf("post-auth read: %v", err)
	}
	var cf wsConfig
	if json.Unmarshal(raw, &cf) != nil || cf.Type != "config" {
		t.Fatalf("expected config frame after auth, got: %s", raw)
	}
	conn.Close()

	// ② 错误 MAC：握手被拒
	conn2, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial2: %v", err)
	}
	defer conn2.Close()
	_, raw, _ = conn2.ReadMessage()
	var ch2 wsChallenge
	_ = json.Unmarshal(raw, &ch2)
	_ = conn2.WriteMessage(websocket.TextMessage,
		mustJSON(wsAuth{Type: "auth", Name: "probe-node", MAC: hmacToken("wrong-token", "probe-node|"+ch2.Nonce)}))
	_ = conn2.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err = conn2.ReadMessage(); err == nil {
		t.Fatalf("wrong MAC should be rejected, but got a frame")
	}
}

// P1-9 回归：全局 token 不能用来冒充别的节点。
//
// 旧实现的全局 token 分支是 `HMAC(gt, nonce)`，与 name 无关 —— 拿到全局 token
// 就能以任意节点名通过校验、往别人的 node_id 写数据，A1 的越权修复形同虚设。
// 现在全局 token 必须签进 name（HMAC(gt, name|nonce)）。
func TestGlobalTokenCannotImpersonate(t *testing.T) {
	gt := "global-token-secret"
	c := newTestCenter(gt, "adminpw")
	const nonce = "fixed-nonce"

	// 攻击场景：手里只有全局 token，却想冒充 victim-node。
	// 关键在于 —— 签名里的 name 和声称的 name 必须是同一个。
	// 旧实现 `HMAC(gt, nonce)` 完全不含 name，所以拿 gt 签一次即可冒充任何人。
	// 现在用「自己的名字」签出来的 MAC，拿去声称别人必须失败：
	if c.verifyMAC("victim-node", nonce, hmacToken(gt, "probe-node|"+nonce)) {
		t.Fatalf("用 probe-node 的签名冒充 victim-node 必须失败：全局 token 未绑定 name")
	}
	// 以本名签名、以本名声称则通过（全局 token 的正常用法）
	if !c.verifyMAC("probe-node", nonce, hmacToken(gt, "probe-node|"+nonce)) {
		t.Fatalf("全局 token 以本名签名应通过")
	}
	// 旧格式（裸 nonce、不含 name）对全局 token 必须不再被接受 ——
	// 这正是旧实现的口径，保留它等于漏洞还在
	if c.verifyMAC("probe-node", nonce, hmacToken(gt, nonce)) {
		t.Fatalf("全局 token 的裸 nonce 形式（不绑定 name）必须被拒绝")
	}
	// 无全局 token 时不得有任何配置 token 造成的误放行
	cEmpty := newTestCenter("", "adminpw")
	if cEmpty.verifyMAC("probe-node", nonce, hmacToken("", "probe-node|"+nonce)) {
		t.Fatalf("未配置全局 token 时不应通过校验")
	}
}

func TestHTTPAuth(t *testing.T) {
	tok := "tok-http-test-456"
	c := newTestCenter(tok, "adminpw")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /agent/heartbeat", c.handleHeartbeat)
	mux.HandleFunc("POST /agent/report", c.handleReport)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	do := func(mac string, ts int64) int {
		body, _ := json.Marshal(heartbeatReq{Name: "probe-node", Version: Version, Targets: 3})
		req, _ := http.NewRequest("POST", srv.URL+"/agent/heartbeat", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-PingAtlas-Auth", fmt.Sprintf("probe-node:%d:%s", ts, mac))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return -1
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	now := time.Now().Unix()
	if got := do(hmacToken(tok, fmt.Sprintf("probe-node|%d", now)), now); got != 200 {
		t.Fatalf("valid header auth: want 200 got %d", got)
	}
	if got := do(hmacToken("bad", fmt.Sprintf("probe-node|%d", now)), now); got != 403 {
		t.Fatalf("invalid mac: want 403 got %d", got)
	}
	old := now - 3600
	if got := do(hmacToken(tok, fmt.Sprintf("probe-node|%d", old)), old); got != 403 {
		t.Fatalf("stale ts replay: want 403 got %d", got)
	}
	// report 同头鉴权
	items := []reportItem{{Target: "1.2.3.4", Logtime: time.Now().Format(time.RFC3339), AvgDelay: 1.5, SendPk: 20, RecvPk: 20, LossPk: 0}}
	body, _ := json.Marshal(reportReq{Name: "probe-node", Items: items})
	req, _ := http.NewRequest("POST", srv.URL+"/agent/report", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-PingAtlas-Auth", fmt.Sprintf("probe-node:%d:%s", now, hmacToken(tok, fmt.Sprintf("probe-node|%d", now))))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	defer resp.Body.Close()
	var rr reportResp
	_ = json.NewDecoder(resp.Body).Decode(&rr)
	if resp.StatusCode != 200 || !rr.OK || rr.ServerTs == 0 {
		t.Fatalf("report with header auth failed: %d %+v", resp.StatusCode, rr)
	}
}

func TestAdminBrute(t *testing.T) {
	c := newTestCenter("tok", "rightpw")
	mux := http.NewServeMux()
	c.registerAdminAPI(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	call := func(pwd string) int {
		req, _ := http.NewRequest("GET", srv.URL+"/api/admin/stats", nil)
		req.Header.Set("X-Admin-Pwd", pwd)
		req.Header.Set("X-Forwarded-For", "203.0.113.7") // 模拟同一攻击 IP
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return -1
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	for i := 0; i < bruteMaxFails; i++ {
		if got := call("wrong" + fmt.Sprint(i)); got != 401 {
			t.Fatalf("attempt %d: want 401 got %d", i, got)
		}
	}
	if got := call("rightpw"); got != 429 {
		t.Fatalf("locked IP with right pwd: want 429 got %d", got)
	}
	// 其他 IP 不受影响
	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/stats", nil)
	req.Header.Set("X-Admin-Pwd", "rightpw")
	req.Header.Set("X-Forwarded-For", "198.51.100.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("other ip: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("other ip should pass, got %d", resp.StatusCode)
	}
}
