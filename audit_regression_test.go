//go:build center

package main

// 第二轮审计修复的回归测试（每一条都对应一个已确认的缺陷）。
//
// 与 fault_test.go 的分工：那边是原有故障场景测试，这里是本轮修复新增的锁。
// 每个用例都刻意断言「旧实现会失败」的那个点，而不是只断言新实现的形状。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// P0：POST /api/targets 只带 alias+ip 时不能抹掉地图维度。
// 旧实现把请求体直接 upsert（province=EXCLUDED.province），
// 空维度目标会被 /api/mapping*.json 与省级曲线整体跳过 ⇒ 目标静默消失，
// 而面板又没有"编辑维度"入口，只能删掉重加（alias 变化、历史劈裂）。
func TestTargetUpsertPreservesMapMeta(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	c.mem.UpsertTarget(TargetRow{Alias: "web", IP: "1.1.1.1", Province: "北京", City: "北京", Telecom: "ctcc"})

	post := func(body map[string]string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/targets", bytes.NewReader(raw))
		req.Header.Set("X-Admin-Pwd", "pw")
		w := httptest.NewRecorder()
		c.handleTargetUpsert(w, req)
		return w
	}

	if w := post(map[string]string{"alias": "web", "ip": "1.1.1.1"}); w.Code != 200 {
		t.Fatalf("upsert 失败：%d %s", w.Code, w.Body.String())
	}
	got := c.mem.ListTargets()
	if len(got) != 1 {
		t.Fatalf("目标数期望 1，实际 %d", len(got))
	}
	if got[0].Province != "北京" || got[0].City != "北京" || got[0].Telecom != "ctcc" {
		t.Fatalf("只改 IP 抹掉了地图维度：%+v", got[0])
	}

	// 显式给出的维度必须生效（合并 ≠ 一律沿用旧值）
	if w := post(map[string]string{"alias": "web", "ip": "1.1.1.1", "province": "上海", "telecom": "cucc"}); w.Code != 200 {
		t.Fatalf("upsert2 失败：%d %s", w.Code, w.Body.String())
	}
	got = c.mem.ListTargets()
	if got[0].Province != "上海" || got[0].Telecom != "cucc" || got[0].City != "北京" {
		t.Fatalf("显式维度未生效 / 未保留其余维度：%+v", got[0])
	}
}

// P0：限流键不能被 X-Forwarded-For 伪造。
// 旧实现无条件取 XFF 第一跳 ⇒ 攻击者每次换个 XFF 就是一把新锁（防爆破失效），
// 反过来还能用真管理员的 IP 把人锁在门外（DoS）。
func TestRealIPIgnoresUntrustedXFF(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	mk := func(remote, xff string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/admin/stats", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	if got := c.realIP(mk("203.0.113.9:5000", "1.2.3.4")); got != "203.0.113.9" {
		t.Fatalf("直连对端不可信时 XFF 必须被忽略，实际 %q", got)
	}
	if got := c.realIP(mk("127.0.0.1:5000", "9.9.9.9, 198.51.100.7")); got != "198.51.100.7" {
		t.Fatalf("可信代理应取 XFF 末跳（首跳是攻击者可写的），实际 %q", got)
	}
	if got := c.realIP(mk("127.0.0.1:5000", "9.9.9.9, not-an-ip")); got != "127.0.0.1" {
		t.Fatalf("末跳非法时应退回直连对端，实际 %q", got)
	}

	// 反复失败 + 每次伪造不同 XFF：锁必须落在同一个真实来源上
	locked := "admin:203.0.113.9"
	for i := 0; i < bruteMaxFails; i++ {
		w := httptest.NewRecorder()
		r := mk("203.0.113.9:5000", fmt.Sprintf("10.0.0.%d", i))
		r.Header.Set("X-Admin-Pwd", "wrong-password")
		if c.requireAdmin(w, r) {
			t.Fatalf("错误密码不应通过")
		}
	}
	if c.lim.allowed(locked) {
		t.Fatalf("同一真实来源累计失败后必须被锁定（伪造 XFF 不应换到新锁）")
	}
	// 另一个真实来源不受影响
	if !c.lim.allowed("admin:198.51.100.7") {
		t.Fatalf("其它来源不应被连带锁定")
	}
}

// P2：install.sh 与一键安装命令不得把客户端可控字符串注入 shell。
// Go 的 %q 只加双引号并转义 " 与 \，双引号内的 $() / 反引号照样会展开。
func TestInstallScriptRejectsShellInjection(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent/install.sh", c.handleInstallScript)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	get := func(url string) (int, string) {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("get %s: %v", url, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// name 里带命令替换
	if code, _ := get(srv.URL + "/agent/install.sh?name=" + urlQueryEscape("$(id)") + "&code=abc123"); code != 400 {
		t.Fatalf("含 shell 元字符的 name 必须 400，实际 %d", code)
	}
	// 反引号形式
	if code, _ := get(srv.URL + "/agent/install.sh?name=" + urlQueryEscape("`id`") + "&code=abc123"); code != 400 {
		t.Fatalf("反引号 name 必须 400，实际 %d", code)
	}
	// 非 hex 的 code
	if code, _ := get(srv.URL + "/agent/install.sh?name=n1&code=" + urlQueryEscape("x;rm -rf /")); code != 400 {
		t.Fatalf("非法 code 必须 400，实际 %d", code)
	}
	// 非法 Host 头
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/agent/install.sh?name=n1&code=abc123", nil)
	req.Host = "evil.example.com;$(id)"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("host-inject get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("非法 Host 必须 400，实际 %d", resp.StatusCode)
	}

	// 正常参数必须能生成脚本，且 CENTER 指向请求 Host
	code, body := get(srv.URL + "/agent/install.sh?name=n1&code=abc123")
	if code != 200 || !strings.Contains(body, "CENTER=") || !strings.Contains(body, "NAME=\"n1\"") {
		t.Fatalf("正常参数应生成脚本：code=%d body=%.160s", code, body)
	}
}

// P2：内存模式的幂等必须与 DB 模式的主键语义一致，否则重传会重复计点。
func TestMemStoreInsertIdempotent(t *testing.T) {
	m := NewMemStore()
	row := PingRow{Logtime: time.Unix(1700000000, 0), Target: "t1", NodeID: "n1",
		AvgDelay: 5, SendPk: 20, RecvPk: 20}
	if n := m.InsertRows([]PingRow{row}); n != 1 {
		t.Fatalf("首次插入应计数 1，实际 %d", n)
	}
	if n := m.InsertRows([]PingRow{row}); n != 0 {
		t.Fatalf("重传必须被幂等丢弃，实际计数 %d", n)
	}
	if got := len(m.QueryHistory("", "", 10)); got != 1 {
		t.Fatalf("重传不应产生第二行，实际 %d 行", got)
	}
}

// P2：内存模式存的是无偏移的墙钟字符串，窗口判断必须用 epoch，
// 否则节点与中心时区不同时整段数据会错位（实测：10 分钟前的数据被当成窗口内最新值）。
//
// 注意：本用例会临时改写全局 time.Local，**不能加 t.Parallel()**（会与其它用例互相干扰）。
func TestMemStoreWindowUsesEpochNotWallClock(t *testing.T) {
	old := time.Local
	time.Local = time.UTC
	defer func() { time.Local = old }()

	m := NewMemStore()
	nodeZone := time.FixedZone("CST", 8*3600)
	stale := time.Now().Add(-10 * time.Minute).In(nodeZone) // 实际是 10 分钟前的数据
	m.InsertRows([]PingRow{{Logtime: stale, Target: "t1", NodeID: "n1", AvgDelay: 42, SendPk: 20, RecvPk: 20}})
	if _, shown := m.LatestSnapshot(5*time.Minute, "")["t1"]; shown {
		t.Fatalf("10 分钟前的数据不得落进 5 分钟窗口（时区错位会让窗口整体平移）")
	}
	// 真正的新数据必须可见
	fresh := time.Now().Add(-time.Minute)
	m.InsertRows([]PingRow{{Logtime: fresh, Target: "t2", NodeID: "n1", AvgDelay: 7, SendPk: 20, RecvPk: 20}})
	if _, shown := m.LatestSnapshot(5*time.Minute, "")["t2"]; !shown {
		t.Fatalf("1 分钟前的数据必须在窗口内")
	}
}

// P1：省级曲线必须把超时（avgdelay<=0）排除出平均值，整桶无有效样本给 null。
// 旧实现把 0 混进均值 ⇒ 一条全丢包的链路画出贴近 0ms 的"健康曲线"。
func TestMemProvinceHistorySkipsTimeout(t *testing.T) {
	m := NewMemStore()
	m.UpsertTarget(TargetRow{Alias: "t1", IP: "1.1.1.1", Province: "广东", Telecom: "ctcc"})
	now := time.Now().Truncate(time.Minute)
	m.InsertRows([]PingRow{{Logtime: now, Target: "t1", NodeID: "n1", AvgDelay: 0, SendPk: 20, RecvPk: 0, LossPk: 20}})
	ips := m.ProvinceHistory("广东", 1, "")
	if len(ips) != 1 || len(ips[0].History) != 1 {
		t.Fatalf("应有 1 个目标 1 个点，实际 %+v", ips)
	}
	if ips[0].History[0] != nil {
		t.Fatalf("全超时的点必须是 null（前端画断点），实际 %v", *ips[0].History[0])
	}
	m.InsertRows([]PingRow{{Logtime: now.Add(time.Minute), Target: "t1", NodeID: "n1", AvgDelay: 12.5, SendPk: 20, RecvPk: 20}})
	ips = m.ProvinceHistory("广东", 1, "")
	if len(ips[0].History) != 2 || ips[0].History[1] == nil || *ips[0].History[1] != 12.5 {
		t.Fatalf("有回包的点应为 12.5，实际 %+v", ips[0].History)
	}
}

// P2：面板的"继承历史"复选框原先后端完全不读（装饰品）。
// inherit=false 时必须连 alias 一起换掉，历史才不会挂到新 IP 上。
func TestTargetReplaceHonoursInheritFlag(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	c.mem.UpsertTarget(TargetRow{Alias: "web", IP: "1.1.1.1", Province: "广东", City: "深圳", Telecom: "ctcc"})

	w := httptest.NewRecorder()
	c.adminTargetReplace(context.Background(), w, map[string]interface{}{
		"oldIP": "1.1.1.1", "newIP": "2.2.2.2", "inherit": false,
	})
	if w.Code != 200 {
		t.Fatalf("replace 失败：%d %s", w.Code, w.Body.String())
	}
	got := c.mem.ListTargets()
	if len(got) != 1 || got[0].Alias != "2.2.2.2" || got[0].IP != "2.2.2.2" {
		t.Fatalf("inherit=false 时 alias 应一并换成新 IP：%+v", got)
	}
	if got[0].Province != "广东" || got[0].City != "深圳" || got[0].Telecom != "ctcc" {
		t.Fatalf("换 alias 不能丢维度：%+v", got[0])
	}

	// 默认（inherit 缺省 / true）保留 alias，历史不断档
	c.mem.UpsertTarget(TargetRow{Alias: "web2", IP: "3.3.3.3", Province: "广东"})
	w2 := httptest.NewRecorder()
	c.adminTargetReplace(context.Background(), w2, map[string]interface{}{"oldIP": "3.3.3.3", "newIP": "4.4.4.4"})
	if w2.Code != 200 {
		t.Fatalf("replace2 失败：%d %s", w2.Code, w2.Body.String())
	}
	found := false
	for _, tr := range c.mem.ListTargets() {
		if tr.Alias == "web2" && tr.IP == "4.4.4.4" {
			found = true
		}
	}
	if !found {
		t.Fatalf("默认应保留 alias（历史不断档）：%+v", c.mem.ListTargets())
	}

	// P2-3：IP 冲突是业务错误，必须回 400（旧实现/未分类时会回 500）
	c.mem.UpsertTarget(TargetRow{Alias: "other", IP: "5.5.5.5"})
	w3 := httptest.NewRecorder()
	c.adminTargetReplace(context.Background(), w3, map[string]interface{}{"oldIP": "4.4.4.4", "newIP": "5.5.5.5"})
	if w3.Code != http.StatusBadRequest {
		t.Fatalf("目标 IP 冲突必须回 400，实际 %d（%s）", w3.Code, w3.Body.String())
	}
}

// P2：/api/nodes 与 /api/history 含节点 IP/明细，原先完全公开。
func TestReadAPIsRequireAdmin(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/nodes", c.handleNodes)
	mux.HandleFunc("GET /api/history", c.handleHistory)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, p := range []string{"/api/nodes", "/api/history"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatalf("get %s: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("%s 无密码必须 401，实际 %d", p, resp.StatusCode)
		}
		req, _ := http.NewRequest(http.MethodGet, srv.URL+p, nil)
		req.Header.Set("X-Admin-Pwd", "pw")
		resp2, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("get2 %s: %v", p, err)
		}
		resp2.Body.Close()
		if resp2.StatusCode != 200 {
			t.Fatalf("%s 带正确密码必须 200，实际 %d", p, resp2.StatusCode)
		}
	}
}

// P2：WS 上行单批行数上限。超限必须回**失败 ack**（节点不推水位、数据留在本地），
// 不能回 OK —— 回 OK 等于让节点把这批数据丢掉。
func TestWSReportItemCapRejectsBatch(t *testing.T) {
	tok := "tok-ws-cap"
	c := newTestCenter2(tok, "pw")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent/ws", c.hub.ServeWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/agent/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read challenge: %v", err)
	}
	var ch wsChallenge
	if err := json.Unmarshal(raw, &ch); err != nil || ch.Nonce == "" {
		t.Fatalf("bad challenge: %s", raw)
	}
	if err := conn.WriteMessage(websocket.TextMessage,
		mustJSON(wsAuth{Type: "auth", Name: "n1", MAC: hmacToken(tok, "n1|"+ch.Nonce)})); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	readConfigFrame(t, conn)

	items := make([]reportItem, maxReportItems+1)
	for i := range items {
		items[i] = reportItem{Target: "t1", Logtime: time.Now().Format(time.RFC3339), SendPk: 20, RecvPk: 20}
	}
	if err := conn.WriteMessage(websocket.TextMessage,
		mustJSON(wsReport{Type: "report", Seq: 7, Items: items})); err != nil {
		t.Fatalf("write report: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, raw2, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	var ack wsAck
	if err := json.Unmarshal(raw2, &ack); err != nil {
		t.Fatalf("bad ack: %s", raw2)
	}
	if ack.OK {
		t.Fatalf("超限批次必须回失败 ack，实际 %+v", ack)
	}
	if ack.Seq != 7 {
		t.Fatalf("失败 ack 必须回显 seq（否则节点认不出是哪一批）：%+v", ack)
	}
}

// 交接文档 §6.2：报错文案让运维"先删除再重装"，但删除接口原先根本不存在
// （节点名用尽 / token 泄露只能进 PG 手删）。
func TestAdminNodeDel(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	c.registerNode("n1", "10.0.0.1", Version, 3, 0, 111)

	w := httptest.NewRecorder()
	c.adminNodeDel(context.Background(), w, map[string]interface{}{"name": "n1"})
	if w.Code != 200 {
		t.Fatalf("删除节点失败：%d %s", w.Code, w.Body.String())
	}
	if n := len(c.mem.ListNodes()); n != 0 {
		t.Fatalf("节点应已删除，实际还剩 %d 个", n)
	}
	if c.isOnline("n1") {
		t.Fatalf("删除后不应再显示在线")
	}

	w2 := httptest.NewRecorder()
	c.adminNodeDel(context.Background(), w2, map[string]interface{}{"name": "nope"})
	if w2.Code != 404 {
		t.Fatalf("删除不存在的节点应 404，实际 %d", w2.Code)
	}
	w3 := httptest.NewRecorder()
	c.adminNodeDel(context.Background(), w3, map[string]interface{}{})
	if w3.Code != 400 {
		t.Fatalf("缺 name 应 400，实际 %d", w3.Code)
	}
}

// 交接文档 §6.2：删掉节点后 /api/nodes 的在线判定必须一致。
// 注意：本用例只覆盖**内存路径**（mem 模式 db==nil）；DB 回落分支需要真实 PG，
// 见 center_db_integration_test.go。名字不再暗示它覆盖了 DB 分支。
func TestIsOnlineMemoryPaths(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	if c.isOnline("ghost") {
		t.Fatalf("未知节点不应在线")
	}
	c.registerNode("n1", "10.0.0.1", Version, 3, 0, 1)
	if !c.isOnline("n1") {
		t.Fatalf("刚心跳过的节点应在线")
	}
	// 内存态被清掉（模拟 center 重启）后，DB 分支才是兜底；内存模式下应判离线
	c.mu.Lock()
	delete(c.lastSeen, "n1")
	c.mu.Unlock()
	if c.isOnline("n1") {
		t.Fatalf("内存模式（无 DB）下清掉 lastSeen 应判离线")
	}
}

// custom.js 摘 pwd 时不能把 `?` 一起吃掉（旧实现产出 /api/admin/logs&type=error → 405）。
// 这里直接验 Go 侧的 action 解析与 type 参数。
func TestAdminLogsActionParsing(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	mux := http.NewServeMux()
	c.registerAdminAPI(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 正确形式：pwd 在前，type 在后
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/admin/logs?pwd=pw&type=error", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("logs?pwd=..&type=.. 必须 200（action=logs、type=error），实际 %d", resp.StatusCode)
	}

	// 反面：旧 custom.js 会把 URL 改写成 /api/admin/logs&type=error（没有 ?），
	// 那种形式必须被识别为未知 action，而不是 200 —— 本测试锁定"客户端必须修好"。
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/admin/logs&type=error", nil)
	req2.Header.Set("X-Admin-Pwd", "pw")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("logs2: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode == 200 {
		t.Fatalf("被改写坏的 URL 不应工作（若不 200 说明 custom.js 的修复是必需的）")
	}
}

// P1-1 回归：install.sh 必须在**下载与兑换都成功之后**才停旧服务。
// 旧顺序（先停后下）在下载 404 / 安装码失效时会让在线节点直接停摆，且没有回滚。
// 这里直接把脚本渲染出来断言语句顺序 —— 不需要 Linux 机器就能锁住。
func TestInstallScriptStopsServiceAfterDownload(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	req := httptest.NewRequest(http.MethodGet, "/agent/install.sh?name=n1&code=abc123", nil)
	w := httptest.NewRecorder()
	c.handleInstallScript(w, req)
	if w.Code != 200 {
		t.Fatalf("应生成安装脚本：%d %s", w.Code, w.Body.String())
	}
	s := w.Body.String()

	iBackup := strings.Index(s, "config.json.bak")
	iDownload := strings.Index(s, "/agent/download")
	iRedeem := strings.Index(s, "/agent/redeem")
	iStop := strings.Index(s, "systemctl stop pingatlas-node")
	for name, idx := range map[string]int{"备份": iBackup, "下载": iDownload, "兑换": iRedeem, "停服": iStop} {
		if idx < 0 {
			t.Fatalf("脚本里缺少 %s 语句（测试前提不成立）", name)
		}
	}
	if iStop < iDownload || iStop < iRedeem {
		t.Fatalf("停旧服务必须排在下载(%d)/兑换(%d)之后，实际 stop@%d —— 否则下载失败会让在线节点停摆",
			iDownload, iRedeem, iStop)
	}
	if iBackup > iDownload {
		t.Fatalf("配置备份应排在下载之前（保证下载失败也留有旧配置），实际 backup@%d download@%d", iBackup, iDownload)
	}
	if !strings.Contains(s, "trap rollback EXIT") {
		t.Fatalf("缺少失败回滚 trap：停服后脚本异常退出会把节点留在停止状态")
	}
	if n := strings.Count(s, "ROLLBACK=0"); n < 3 {
		t.Fatalf("ROLLBACK=0 应出现在初始化与两个 init 分支（≥3 处），实际 %d 处", n)
	}
	// logrotate 的 heredoc 结束符必须顶格，否则 cat 会一直吃到文件尾
	terminated := false
	for _, line := range strings.Split(s, "\n") {
		if line == "LOGROTATE" {
			terminated = true
		}
	}
	if !terminated {
		t.Fatalf("logrotate heredoc 结束符未顶格（整段脚本会失效）")
	}
}

// P1-4 回归：HTTP 兜底上报路必须有与 WS 相同的单批行数上限，
// 且超限时必须回失败（节点不推进水位、数据留在本地）。
func TestHTTPReportItemCapRejected(t *testing.T) {
	tok := "tok-http-cap"
	c := newTestCenter2(tok, "pw")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /agent/report", c.handleReport)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	items := make([]reportItem, maxReportItems+1)
	for i := range items {
		items[i] = reportItem{Target: "t1", Logtime: time.Now().Format(time.RFC3339), SendPk: 20, RecvPk: 20}
	}
	resp := authed(t, c, srv.URL, "/agent/report", "n1", tok, reportReq{Name: "n1", Seq: 3, Items: items})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限批次必须 413，实际 %d", resp.StatusCode)
	}
	if n := len(c.mem.QueryHistory("", "", 10)); n != 0 {
		t.Fatalf("超限批次不得落库，实际写了 %d 行", n)
	}
}

// P1-4 回归：鉴权必须排在做 body 解析之前。
// 修复前 handleReport/handleHeartbeat 先 decodeBody 再看鉴权头 ⇒ 未鉴权 + 非法 JSON 返回 400，
// 等于任何人都能让中心解析最多 1MB 的 JSON。
func TestReportAuthBeforeBodyParse(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /agent/report", c.handleReport)
	mux.HandleFunc("POST /agent/heartbeat", c.handleHeartbeat)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, p := range []string{"/agent/report", "/agent/heartbeat"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+p, strings.NewReader("not-json"))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post %s: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s 未鉴权必须 403（先鉴权后解析；修复前是 400），实际 %d", p, resp.StatusCode)
		}
	}
}

// P1-3 回归：同一来源 IP 的在途握手数必须受限。
// 只做全局计数时，单台机器几十条"连上但不发 auth"的连接就能把配额占满，
// 把合法节点挡在 503 外面；按 IP 限并发才是止血点。
func TestWSHandshakePerIPLimit(t *testing.T) {
	c := newTestCenter2("tok-ws-hs", "pw")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent/ws", c.hub.ServeWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/agent/ws"

	var held []*websocket.Conn
	defer func() {
		for _, cn := range held {
			cn.Close()
		}
	}()
	for i := 0; i < maxHandshakesPerIP; i++ {
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("第 %d 条握手连接本应被接受（它只是不发 auth）：%v", i+1, err)
		}
		held = append(held, conn)
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, _, err := conn.ReadMessage(); err != nil { // 收到 challenge = 服务端确实停在握手窗口
			t.Fatalf("第 %d 条连接未收到 challenge: %v", i+1, err)
		}
	}
	// 所有连接都来自 127.0.0.1，因此必然命中同一个 per-IP 计数
	if conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil); err == nil {
		conn.Close()
		t.Fatalf("超过每 IP 并发握手上限(%d)后必须拒绝新建连接", maxHandshakesPerIP)
	}
}

// P1-3 回归：握手配额必须在鉴权完成后立刻归还 —— 长连接不占握手配额。
// 若忘记归还，第 maxHandshakesPerIP+1 条长连接就会建不起来（这正是"闸门覆盖了整条连接
// 生命周期"的经典写法错误）。
func TestWSHandshakeSlotsReleasedAfterAuth(t *testing.T) {
	tok := "tok-ws-rel"
	c := newTestCenter2(tok, "pw")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent/ws", c.hub.ServeWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/agent/ws"

	var held []*websocket.Conn
	defer func() {
		for _, cn := range held {
			cn.Close()
		}
	}()
	for i := 0; i < maxHandshakesPerIP*2; i++ {
		name := fmt.Sprintf("n%d", i+1)
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("第 %d 条长连接本应成功（握手配额未归还？）：%v", i+1, err)
		}
		held = append(held, conn)
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("第 %d 条读 challenge: %v", i+1, err)
		}
		var ch wsChallenge
		if json.Unmarshal(raw, &ch) != nil || ch.Nonce == "" {
			t.Fatalf("第 %d 条 challenge 非法: %s", i+1, raw)
		}
		if err := conn.WriteMessage(websocket.TextMessage,
			mustJSON(wsAuth{Type: "auth", Name: name, MAC: hmacToken(tok, name+"|"+ch.Nonce)})); err != nil {
			t.Fatalf("第 %d 条写 auth: %v", i+1, err)
		}
		readConfigFrame(t, conn) // config 帧 = 已通过鉴权
	}
}

// P1-3：全局握手配额本体的边界。顺序握手不会争用配额，所以必须直接单元测试闸门：
// 配额用尽后要拒绝，归还后要能再取（这条也覆盖了 maxConcurrentHandshakes 的取值语义）。
func TestHubGlobalHandshakeCap(t *testing.T) {
	// 设计下限：这个值过小会让合法节点在建连高峰期被 503 挡住（历史值是 32，
	// 攻击者约 3 条新连接/秒就能打满）。这里把"不能调回去"也锁住。
	if maxConcurrentHandshakes < 64 {
		t.Fatalf("全局握手配额不应低于 64（当前 %d）", maxConcurrentHandshakes)
	}
	if maxHandshakesPerIP < 2 || maxHandshakesPerIP > 16 {
		t.Fatalf("每 IP 握手配额应在 2..16 之间（当前 %d）", maxHandshakesPerIP)
	}
	if wsHandshakeTimeout > 10*time.Second {
		t.Fatalf("握手读超时应 ≤10s（当前 %s）：占坑成本与它成正比", wsHandshakeTimeout)
	}

	h := NewHub(newTestCenter2("t", "p"))
	ips := make([]string, 0, maxConcurrentHandshakes)
	for i := 0; i < maxConcurrentHandshakes; i++ {
		ip := fmt.Sprintf("10.%d.%d.%d", i/65536, (i/256)%256, i%256)
		if !h.acquireHandshake(ip) {
			t.Fatalf("第 %d 个全局配额本应可用", i+1)
		}
		ips = append(ips, ip)
	}
	if h.acquireHandshake("192.0.2.99") {
		t.Fatalf("全局配额用尽后必须拒绝新握手")
	}
	h.releaseHandshake(ips[0])
	if !h.acquireHandshake("192.0.2.99") {
		t.Fatalf("归还一个配额后应能再获取")
	}
}

// P1-5 回归：被环形窗口裁掉的行，其去重键必须同时逐出；
// 否则这些历史行重传时会被当成"重复"静默丢弃（等于丢数据）。
func TestMemStoreEvictsTrimmedKeys(t *testing.T) {
	m := NewMemStore()
	base := time.Unix(1700000000, 0)
	mk := func(i int) PingRow {
		return PingRow{Logtime: base.Add(time.Duration(i) * time.Minute), Target: "t1", NodeID: "n1",
			AvgDelay: 5, SendPk: 20, RecvPk: 20}
	}
	for i := 0; i < memKeepRows+1; i++ {
		m.InsertRows([]PingRow{mk(i)})
	}
	if n := m.InsertRows([]PingRow{mk(0)}); n != 1 {
		t.Fatalf("被裁掉的历史行重传时应重新入库（去重键残留会静默丢弃），实际 n=%d", n)
	}
}

// P1-5 性能口径：稳态（窗口已满）下的单次插入必须是 O(1) 量级。
// 旧实现在每次裁剪时重建整张 5000 项去重表 ⇒ O(5000)/次。
func BenchmarkMemStoreInsert(b *testing.B) {
	m := NewMemStore()
	base := time.Unix(1700000000, 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.InsertRows([]PingRow{{Logtime: base.Add(time.Duration(i) * time.Second), Target: "t",
			NodeID: "n", AvgDelay: 1, SendPk: 20, RecvPk: 20}})
	}
}

// P2-2 回归：改密码必须"先落盘、成功后才改内存"。
// 旧实现先改内存 ⇒ 写盘失败时接口回 500，但运行中的密码已经变了（旧密码立即失效、
// 重启后又变回去）。这里用"父目录不存在"制造必然的写盘失败来锁定顺序。
func TestSaveAdminPwdOrderingAndValidation(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "center.json")
	if err := os.WriteFile(cfg, []byte(`{"Listen":":18991","Token":"tok","AdminPwd":"oldpw"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newTestCenter2("tok", "oldpw")
	c.confPath = cfg

	if err := c.saveAdminPwd("newpw123"); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if c.adminPwd() != "newpw123" {
		t.Fatalf("内存未更新: %q", c.adminPwd())
	}
	if b, _ := os.ReadFile(cfg); !strings.Contains(string(b), `"AdminPwd": "newpw123"`) {
		t.Fatalf("文件未更新: %s", string(b))
	}

	// 长度校验：被拒的修改不得影响内存
	if err := c.saveAdminPwd("12345"); err == nil {
		t.Fatalf("短密码必须被拒绝")
	}
	if c.adminPwd() != "newpw123" {
		t.Fatalf("被拒的修改影响了内存: %q", c.adminPwd())
	}

	// 写盘必然失败：内存必须保持不变（旧实现会在这里把密码改掉）
	c.confPath = filepath.Join(dir, "nope", "center.json")
	if err := c.saveAdminPwd("anotherone"); err == nil {
		t.Fatalf("写盘必然失败，应返回错误")
	}
	if c.adminPwd() != "newpw123" {
		t.Fatalf("写盘失败后内存被改成了 %q（旧实现的行为）", c.adminPwd())
	}
}

// readConfigFrame 读到 config 帧为止（跳过其它帧）。
// 直接"读一帧就当它是 config"会让人为的帧顺序假设变成脆弱测试。
func readConfigFrame(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("等待 config 帧超时")
		}
		_ = conn.SetReadDeadline(deadline)
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("读 config 帧失败: %v", err)
		}
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &probe) == nil && probe.Type == "config" {
			return
		}
	}
}

// P3-2 回归：host 白名单既要挡住 shell 元字符，也要支持 IPv6 字面量与端口。
func TestValidHostAcceptAndReject(t *testing.T) {
	accept := []string{
		"example.com", "example.com:443", "wss.example.com", "10.0.0.1", "127.0.0.1:18991",
		"[::1]", "[::1]:18991", "[2001:db8::1]:443", "a-b_c.example",
	}
	reject := []string{
		"", "evil;$(id)", "$(id)", "`id`", "a b", "example.com\x00", "example.com\nX",
		"[::1", "[::1]x", "[zzz]", "::1", // 裸 IPv6 拼 URL 有歧义，必须带方括号
		"example.com:0", "example.com:70000", "example.com:abc", strings.Repeat("a", 254),
	}
	for _, h := range accept {
		if !validHost(h) {
			t.Fatalf("应接受 host %q", h)
		}
	}
	for _, h := range reject {
		if validHost(h) {
			t.Fatalf("应拒绝 host %q", h)
		}
	}

	// 端到端：IPv6 Host 能生成脚本，注入型 Host 被拒
	c := newTestCenter2("tok", "pw")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agent/install.sh?name=n1&code=abc123", nil)
	req.Host = "[::1]:18991"
	c.handleInstallScript(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `CENTER="https://[::1]:18991"`) {
		t.Fatalf("IPv6 Host 应能生成脚本：%d %.120s", w.Code, w.Body.String())
	}
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/agent/install.sh?name=n1&code=abc123", nil)
	req2.Host = "evil;$(id)"
	c.handleInstallScript(w2, req2)
	if w2.Code != 400 {
		t.Fatalf("注入型 Host 必须 400，实际 %d", w2.Code)
	}
}

// P1-3 回归：单帧读上限必须真的生效 —— 超限帧要导致断连，而不是被继续读进内存。
// （旧实现完全没有上限：一个被攻陷的节点发一个 GB 级帧就能把中心打爆。）
func TestWSReadLimitClosesOversizedFrame(t *testing.T) {
	tok := "tok-ws-limit"
	c := newTestCenter2(tok, "pw")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent/ws", c.hub.ServeWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/agent/ws"

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read challenge: %v", err)
	}
	var ch wsChallenge
	if json.Unmarshal(raw, &ch) != nil || ch.Nonce == "" {
		t.Fatalf("bad challenge: %s", raw)
	}
	if err := conn.WriteMessage(websocket.TextMessage,
		mustJSON(wsAuth{Type: "auth", Name: "n1", MAC: hmacToken(tok, "n1|"+ch.Nonce)})); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	readConfigFrame(t, conn)

	big := strings.Repeat("a", maxWSMessageBytes+4096)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(big)); err != nil {
		t.Fatalf("write big frame: %v", err)
	}
	// 注意：**读超时也是 err != nil**，不能把"没等到断连"误判成"已断连"——
	// 上一版就是这么写的，去掉 SetReadLimit 之后它照样绿（变异测试抓到）。
	for {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue // 其它帧（如数据中心下发的 ping 对客户端不可见），继续等
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatalf("超过单帧上限(%d 字节)的消息必须导致断连，实际连接仍然开着（读超时）", maxWSMessageBytes)
		}
		return // 收到 close 帧或连接被断开 = 上限生效
	}
}

// D：首页口径可切换 —— 不传 ?node= 是"全网最优"（并列取更优值），传 ?node= 只看该节点，
// 未知节点 400；且地图与明细必须同一份快照。没有这层标注时，界面上根本看不出数字来自哪台。
func TestMappingSnapshotScope(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	c.mem.UpsertTarget(TargetRow{Alias: "t1", IP: "203.0.113.1", Province: "福建", City: "福州", Telecom: "ctcc"})
	c.registerNode("node-a", "10.0.0.1", "0.2.0", 1, 0, 1)
	c.registerNode("node-b", "10.0.0.2", "0.2.0", 1, 0, 2)
	lt := time.Now().Truncate(time.Minute)
	c.mem.InsertRows([]PingRow{
		{Logtime: lt, Target: "t1", NodeID: "node-a", AvgDelay: 40, SendPk: 20, RecvPk: 20},
		{Logtime: lt, Target: "t1", NodeID: "node-b", AvgDelay: 12, SendPk: 20, RecvPk: 20},
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/mapping.json", c.handleMapping)
	mux.HandleFunc("GET /api/mapping_detail.json", c.handleMappingDetail)
	mux.HandleFunc("GET /api/province_history.json", c.handleProvinceHistory)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	type pv struct {
		Name  string  `json:"name"`
		Value float64 `json:"value"`
	}
	type mapResp struct {
		Scope    string             `json:"scope"`
		Node     string             `json:"node"`
		Nodes    []string           `json:"nodes"`
		AvgDelay map[string][]pv    `json:"avgdelay"`
	}
	getMap := func(q string) (int, mapResp) {
		resp, err := http.Get(srv.URL + "/api/mapping.json" + q)
		if err != nil {
			t.Fatalf("get mapping%s: %v", q, err)
		}
		defer resp.Body.Close()
		var m mapResp
		if resp.StatusCode == 200 {
			if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
				t.Fatalf("decode mapping%s: %v", q, err)
			}
		}
		return resp.StatusCode, m
	}
	fujian := func(m mapResp) float64 {
		for _, x := range m.AvgDelay["ctcc"] {
			if x.Name == "福建" {
				return x.Value
			}
		}
		return -1
	}

	code, m := getMap("")
	if code != 200 || m.Scope != "best" || m.Node != "" {
		t.Fatalf("默认应为 best 口径：%d scope=%q node=%q", code, m.Scope, m.Node)
	}
	if len(m.Nodes) != 2 {
		t.Fatalf("响应应带节点列表，实际 %v", m.Nodes)
	}
	if got := fujian(m); got != 12 {
		t.Fatalf("best 口径应取两台里更优的 12，实际 %v", got)
	}
	if code, m = getMap("?node=node-a"); code != 200 || m.Scope != "node" || m.Node != "node-a" {
		t.Fatalf("指定节点口径错误：%d %+v", code, m)
	}
	if got := fujian(m); got != 40 {
		t.Fatalf("node-a 口径应为 40，实际 %v", got)
	}
	if _, m = getMap("?node=node-b"); fujian(m) != 12 {
		t.Fatalf("node-b 口径应为 12，实际 %v", fujian(m))
	}
	if code, _ = getMap("?node=ghost"); code != 400 {
		t.Fatalf("未知节点必须 400（否则拼错会静默显示空地图），实际 %d", code)
	}

	// 明细必须与地图同一口径
	resp, err := http.Get(srv.URL + "/api/mapping_detail.json?node=node-a")
	if err != nil {
		t.Fatalf("get detail: %v", err)
	}
	defer resp.Body.Close()
	var det map[string]map[string]map[string][]struct {
		IP    string  `json:"ip"`
		Delay float64 `json:"delay"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&det); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if v := det["福建"]["ctcc"]["福州"][0].Delay; v != 40 {
		t.Fatalf("明细应跟随 ?node= 口径（期望 40），实际 %v", v)
	}

	// 省级历史同样接受 ?node=
	resp2, err := http.Get(srv.URL + "/api/province_history.json?province=福建&node=node-a")
	if err != nil {
		t.Fatalf("get province history: %v", err)
	}
	defer resp2.Body.Close()
	var ph struct {
		Node string  `json:"node"`
		IPs  []HistIP `json:"ips"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&ph); err != nil {
		t.Fatalf("decode province history: %v", err)
	}
	if ph.Node != "node-a" || len(ph.IPs) != 1 {
		t.Fatalf("省级历史应只含 node-a 的数据：node=%q ips=%d", ph.Node, len(ph.IPs))
	}
	if ph.IPs[0].History[0] == nil || *ph.IPs[0].History[0] != 40 {
		t.Fatalf("node-a 的省级曲线应为 40，实际 %+v", ph.IPs[0].History)
	}
}

// P2-2 不变式：并发改密码后，内存里的密码必须与文件里的一致（避免"重启后回退"）。
func TestSaveAdminPwdConcurrentConsistency(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "center.json")
	if err := os.WriteFile(cfg, []byte(`{"AdminPwd":"startpw"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newTestCenter2("tok", "startpw")
	c.confPath = cfg

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = c.saveAdminPwd(fmt.Sprintf("pw-%d-xxxxx", i))
		}(i)
	}
	wg.Wait()

	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("文件被写坏: %v", err)
	}
	onDisk, _ := m["AdminPwd"].(string)
	if onDisk != c.adminPwd() {
		t.Fatalf("内存=%q 与文件=%q 不一致（重启后密码会回退）", c.adminPwd(), onDisk)
	}
}
