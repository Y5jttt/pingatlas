//go:build center

package main

// 故障场景与逻辑回归测试：
//   A1  越权：body 里的 Name 覆盖鉴权头 name → 必须被忽略
//   B4  非法目标行被拒（并计入 rejected）
//   C档 buildRows fail-closed：目标表读不出来时整批拒收，且 ack 必须回失败
//   P1-1 POST /api/targets 必须校验管理密码 + bump 版本 + 失效缓存 + 查 IP 唯一性
//   P1-3 目标表被清空是合法终态（Clear=true），节点应能应用空列表
//   A3  目标版本号自增（删非最新目标也必须变化）
//   A4  IP 唯一性：已被别的 alias 占用的 IP 不能再添加
//   ack 回显 seq（B2 的前提）
//   B5  重连只 touch，不清 version/targets
//   C4  restart 回执靠 boot_ts 变化，网络抖动重连不算重启

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestCenter2(tok, apwd string) *Center {
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

func authed(t *testing.T, c *Center, srvURL, path, name, tok string, payload interface{}) *http.Response {
	t.Helper()
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", srvURL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ts := time.Now().Unix()
	// nonce 传原始 ts，name 由 verifyMAC 内部统一拼成 name|ts（P1-9）
	req.Header.Set("X-PingAtlas-Auth", fmt.Sprintf("%s:%d:%s", name, ts, hmacToken(tok, fmt.Sprintf("%s|%d", name, ts))))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	return resp
}

// A1：body.Name 伪造成别人的节点名，落库必须记成鉴权头的名字
func TestSpoofedNodeNameRejected(t *testing.T) {
	tok := "tok-spoof"
	c := newTestCenter2(tok, "pw")
	c.mem.UpsertTarget(TargetRow{Alias: "1.2.3.4", IP: "1.2.3.4"})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /agent/report", c.handleReport)
	mux.HandleFunc("POST /agent/heartbeat", c.handleHeartbeat)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	items := []reportItem{{
		Target: "1.2.3.4", Logtime: time.Now().Format(time.RFC3339),
		AvgDelay: 5, SendPk: 20, RecvPk: 20,
	}}
	// body 里自称 "victim-node"，但 HMAC 头是 attacker-node
	resp := authed(t, c, srv.URL, "/agent/report", "attacker-node", tok,
		reportReq{Name: "victim-node", Seq: 7, Items: items})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("report: want 200 got %d", resp.StatusCode)
	}
	rows := c.mem.QueryHistory("", "", 50)
	if len(rows) != 1 {
		t.Fatalf("expect 1 row, got %d", len(rows))
	}
	if got := rows[0]["node_id"].(string); got != "attacker-node" {
		t.Fatalf("越权未被阻止：落库 node_id = %s，期望 attacker-node", got)
	}
	if ack := readAck(t, resp); ack.Seq != 7 {
		t.Fatalf("ack 未回显 seq：%d", ack.Seq)
	}
}

func readAck(t *testing.T, resp *http.Response) reportResp {
	t.Helper()
	var rr reportResp
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		t.Fatalf("decode ack: %v", err)
	}
	return rr
}

// B4：目标不在表里的行被拒收，并如实回报 rejected 数
func TestRejectUnknownTarget(t *testing.T) {
	tok := "tok-reject"
	c := newTestCenter2(tok, "pw")
	c.mem.UpsertTarget(TargetRow{Alias: "1.2.3.4", IP: "1.2.3.4"})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /agent/report", c.handleReport)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	lt := time.Now().Format(time.RFC3339)
	items := []reportItem{
		{Target: "1.2.3.4", Logtime: lt, AvgDelay: 5, SendPk: 20, RecvPk: 20},
		{Target: "6.6.6.6", Logtime: lt, AvgDelay: 9, SendPk: 20, RecvPk: 20}, // 不在目标表
		{Target: "7.7.7.7", Logtime: "not-a-time", AvgDelay: 1},              // 时间格式坏
	}
	resp := authed(t, c, srv.URL, "/agent/report", "n1", tok, reportReq{Seq: 3, Items: items})
	defer resp.Body.Close()
	ack := readAck(t, resp)
	if !ack.OK {
		t.Fatalf("ack should be OK, got %+v", ack)
	}
	if ack.Rejected != 2 {
		t.Fatalf("rejected 应为 2，实际 %d", ack.Rejected)
	}
	if ack.Count != 1 {
		t.Fatalf("count 应为 1，实际 %d", ack.Count)
	}
}

// A3：目标表任何增删改都必须让版本号变化（否则离线节点拒绝加载，继续探测已删目标）
//
// 旧版本只调 mem.UpsertTarget 再自己 Bump 一次 —— 等于在断言测试自己：把 handler
// 里的 bump 全删掉它依然通过（变异测试实测确认）。现在改走真实管理入口，
// 并且明确断言「内存存储不会偷偷自增」（与 DB 模式语义一致）。
func TestTargetsVersionBumps(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	v0 := c.mem.TargetsVersion()
	c.mem.UpsertTarget(TargetRow{Alias: "1.1.1.1", IP: "1.1.1.1"})
	if v1 := c.mem.TargetsVersion(); v1 != v0 {
		t.Fatalf("UpsertTarget 不应自行推进版本号（版本只由 BumpTargetsVersion 推进）：%d -> %d", v0, v1)
	}

	// 真实添加路径必须自增
	addBody := map[string]interface{}{"ip": "9.9.9.9", "province": "广东", "city": "深圳", "operator": "ctcc"}
	vAdd := c.mem.TargetsVersion()
	w := httptest.NewRecorder()
	c.adminTargetAdd(context.Background(), w, addBody)
	if w.Code != 200 {
		t.Fatalf("targets/add 失败：%d %s", w.Code, w.Body.String())
	}
	if v := c.mem.TargetsVersion(); v <= vAdd {
		t.Fatalf("targets/add 后版本号未变：%d -> %d", vAdd, v)
	}

	// 真实删除路径也必须自增
	vDel := c.mem.TargetsVersion()
	w2 := httptest.NewRecorder()
	c.adminTargetDel(context.Background(), w2, map[string]interface{}{"ip": "9.9.9.9"})
	if w2.Code != 200 {
		t.Fatalf("targets/del 失败：%d %s", w2.Code, w2.Body.String())
	}
	if v := c.mem.TargetsVersion(); v <= vDel {
		t.Fatalf("targets/del 后版本号未变：%d -> %d", vDel, v)
	}
}

// A4：同一个 IP 不能挂在两个 alias 下
func TestIPUniqueness(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	// 两个不同 alias，各有自己的 IP
	c.mem.UpsertTarget(TargetRow{Alias: "1.1.1.1", IP: "1.1.1.1"})
	c.mem.UpsertTarget(TargetRow{Alias: "3.3.3.3", IP: "3.3.3.3"})

	// 场景：把 3.3.3.3 换成 1.1.1.1（已被 alias=1.1.1.1 占用）必须被拒
	if _, err := c.mem.ReplaceTargetIP("3.3.3.3", "1.1.1.1"); err == nil {
		t.Fatalf("replace 到别的 alias 已占用的 IP 应报错")
	}
	// 换到空闲 IP 应成功
	if n, err := c.mem.ReplaceTargetIP("3.3.3.3", "5.5.5.5"); err != nil || n != 1 {
		t.Fatalf("replace 到空闲 IP 应成功：n=%d err=%v", n, err)
	}
	// IPInUse：自身 alias 排除
	if holder, inUse := c.mem.IPInUse("1.1.1.1", ""); !inUse || holder != "1.1.1.1" {
		t.Fatalf("IPInUse 判错：holder=%q inUse=%v", holder, inUse)
	}
	if _, inUse := c.mem.IPInUse("1.1.1.1", "1.1.1.1"); inUse {
		t.Fatalf("自身 alias 不应被判为占用")
	}
	// 未注册的 IP 判为未占用
	if _, inUse := c.mem.IPInUse("9.9.9.9", ""); inUse {
		t.Fatalf("9.9.9.9 未注册，不应判为占用")
	}
}

// C4：restart 回执必须靠 boot_ts 变化，网络抖动重连（boot_ts 不变）不算重启成功
func TestRestartReceiptNeedsBootTsChange(t *testing.T) {
	c := newTestCenter2("tok", "pw")

	// 节点心跳上报 boot_ts=1000
	c.registerNode("n1", "1.1.1.1", Version, 5, 0, 1000)
	// 下发 restart：记录此时的 boot_ts 作为 prev
	c.noteCmd("n1", "restart", "")
	if c.cmdSnapshot("n1").Done {
		t.Fatalf("下发后不应立即完成")
	}
	// 网络抖动重连，boot_ts 不变 → 仍是待确认
	c.markCmdHeartbeat("n1", Version, 1000)
	if c.cmdSnapshot("n1").Done {
		t.Fatalf("boot_ts 未变的重连不应被判为重启完成")
	}
	// 进程真重启：boot_ts 变了 → 完成
	c.markCmdHeartbeat("n1", Version, 2000)
	if s := c.cmdSnapshot("n1"); !s.Done {
		t.Fatalf("boot_ts 变化后应判为重启完成")
	}
}

// B5：重连只刷新 ip/last_seen，不清空节点表里的 version/targets/clock_offset
func TestTouchNodeKeepsFields(t *testing.T) {
	tok := "tok-touch"
	c := newTestCenter2(tok, "pw")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent/ws", c.hub.ServeWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 直接验 registerNode → touchNode 的字段保留语义。
	// 注意：内存模式原先没有 TouchNode 实现、touchNode 在 db==nil 时整段跳过，
	// 这个测试因此天然通过（假绿）。现在 mem.TouchNode 存在，断言才有意义 ——
	// 所以必须同时断言 ip 真的被刷新，否则空实现也能过。
	c.registerNode("n1", "10.0.0.1", "0.2.0", 470, 12345, 999)
	before := c.mem.ListNodes()[0]
	c.touchNode("n1", "10.0.0.2")
	after := c.mem.ListNodes()[0]
	if after.IP != "10.0.0.2" {
		t.Fatalf("touchNode 应刷新 ip：%q", after.IP)
	}
	if after.Version != "0.2.0" || after.Targets != 470 || after.ClockOffsetMs != 12345 || after.BootTs != 999 {
		t.Fatalf("touchNode 抹掉了节点字段：before=%+v after=%+v", before, after)
	}
}

// D6：超过 1MB 的请求体被拒
func TestBodySizeLimit(t *testing.T) {
	tok := "tok-bigbody"
	c := newTestCenter2(tok, "pw")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /agent/report", c.handleReport)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	big := make([]byte, 1<<20+4096)
	for i := range big {
		big[i] = 'x'
	}
	body, _ := json.Marshal(reportReq{Name: "n1", Items: []reportItem{{
		Target: "1.2.3.4", Logtime: string(big),
	}}})
	req, _ := http.NewRequest("POST", srv.URL+"/agent/report", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ts := time.Now().Unix()
	req.Header.Set("X-PingAtlas-Auth", fmt.Sprintf("n1:%d:%s", ts, hmacToken(tok, fmt.Sprintf("n1|%d", ts))))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("超大请求体应被 400 拒绝，实际 %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// C 档修复：buildRows 必须 fail-closed
// ---------------------------------------------------------------------------

// 目标表读不出来时，整批拒收且 ack 必须回 OK=false。
//
// 旧实现是 `if len(valid) > 0 {校验}` + knownTargets 吞掉 ListTargets 错误：
// PG 抖一下 → valid 为空 → 校验整段跳过 → 任意节点都能写入任意 target 行。
// 更糟的是若只回 rejected 而 OK=true，节点会照常推进水位，这批数据被静默丢弃。
func TestBuildRowsFailClosedOnTargetReadError(t *testing.T) {
	tok := "tok-failclosed"
	c := newTestCenter2(tok, "pw")
	// 模拟「DB 模式 + 读目标表失败」：pool 为 nil，靠 listTargetsErr 短路，不会真连库
	c.db = &CenterDB{listTargetsErr: fmt.Errorf("模拟 PG 抖动")}
	c.mem = nil
	mux := http.NewServeMux()
	mux.HandleFunc("POST /agent/report", c.handleReport)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	items := []reportItem{{
		Target: "6.6.6.6", Logtime: time.Now().Format(time.RFC3339),
		AvgDelay: 5, SendPk: 20, RecvPk: 20,
	}}
	resp := authed(t, c, srv.URL, "/agent/report", "n1", tok, reportReq{Seq: 9, Items: items})
	defer resp.Body.Close()
	ack := readAck(t, resp)
	if ack.OK {
		t.Fatalf("目标表读失败时 ack 必须为 OK=false（否则节点会推进水位丢数据），实际 OK=true count=%d", ack.Count)
	}
	if ack.Seq != 9 {
		t.Fatalf("失败回执也必须回显 seq，期望 9 实际 %d", ack.Seq)
	}
}

// 目标表为空是合法状态：此时一切上报都非法，但 ack 仍应是 OK=true（正常拒收，不是故障）。
// 与上一条区分：读失败 = 故障（OK=false），表为空 = 业务拒收（OK=true + rejected=N）。
func TestBuildRowsEmptyTargetTableRejectsAll(t *testing.T) {
	tok := "tok-emptytbl"
	c := newTestCenter2(tok, "pw")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /agent/report", c.handleReport)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	// c.mem 初始为空 —— 目标表确实没有任何目标

	items := []reportItem{
		{Target: "1.1.1.1", Logtime: time.Now().Format(time.RFC3339), AvgDelay: 5, SendPk: 20, RecvPk: 20},
		{Target: "2.2.2.2", Logtime: time.Now().Format(time.RFC3339), AvgDelay: 5, SendPk: 20, RecvPk: 20},
	}
	resp := authed(t, c, srv.URL, "/agent/report", "n1", tok, reportReq{Seq: 1, Items: items})
	defer resp.Body.Close()
	ack := readAck(t, resp)
	if !ack.OK {
		t.Fatalf("目标表为空属业务拒收，ack 应为 OK=true，实际 false（err=%s）", ack.Err)
	}
	if ack.Rejected != 2 {
		t.Fatalf("空表下所有行都应被拒收，期望 rejected=2 实际 %d", ack.Rejected)
	}
}

// ---------------------------------------------------------------------------
// P1-1：POST /api/targets 无鉴权 + 不 bump 版本
// ---------------------------------------------------------------------------

func newTargetUpsertServer(t *testing.T, c *Center) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/targets", c.handleTargetUpsert)
	srv := httptest.NewServer(mux)
	return srv
}

func postTarget(t *testing.T, srvURL, pwd, alias, ip string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(TargetRow{Alias: alias, IP: ip})
	req, _ := http.NewRequest("POST", srvURL+"/api/targets", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if pwd != "" {
		req.Header.Set("X-Admin-Pwd", pwd)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post target: %v", err)
	}
	return resp
}

// 无管理密码必须被拒：公网 443 暴露时这是目标表的完整写权限。
func TestTargetUpsertRequiresAdminPwd(t *testing.T) {
	c := newTestCenter2("tok", "correct-pwd")
	srv := newTargetUpsertServer(t, c)
	defer srv.Close()

	for _, pwd := range []string{"", "wrong-pwd"} {
		resp := postTarget(t, srv.URL, pwd, "1.1.1.1", "1.1.1.1")
		code := resp.StatusCode
		resp.Body.Close()
		if code != 401 {
			t.Fatalf("密码 %q 应被 401 拒绝，实际 %d", pwd, code)
		}
	}
	// 目标表必须还是空的
	if len(c.mem.ListTargets()) != 0 {
		t.Fatalf("未鉴权的请求不得写入任何目标，实际写了 %d 条", len(c.mem.ListTargets()))
	}
}

// 写入成功必须 bump 版本号，否则节点侧 applyRemoteConfig 判「版本未变」直接忽略推送（静默失败）。
func TestTargetUpsertBumpsVersion(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	srv := newTargetUpsertServer(t, c)
	defer srv.Close()

	v0 := c.mem.TargetsVersion()
	resp := postTarget(t, srv.URL, "pw", "1.1.1.1", "1.1.1.1")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("合法写入应成功，实际 %d", resp.StatusCode)
	}
	v1 := c.mem.TargetsVersion()
	if v1 <= v0 {
		t.Fatalf("写入目标后版本号必须自增：%d -> %d（不 bump 会导致节点静默忽略推送）", v0, v1)
	}
}

// 写入后必须失效目标缓存，否则 5 秒内 buildRows 会把新目标当非法行拒收。
func TestTargetUpsertInvalidatesCache(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	srv := newTargetUpsertServer(t, c)
	defer srv.Close()

	// 先让缓存被填上（旧内容：只有 1.1.1.1）
	c.mem.UpsertTarget(TargetRow{Alias: "1.1.1.1", IP: "1.1.1.1"})
	if _, err := c.knownTargets(); err != nil {
		t.Fatalf("knownTargets: %v", err)
	}
	// 通过接口新增一个完全不同的目标
	resp := postTarget(t, srv.URL, "pw", "9.9.9.9", "9.9.9.9")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("写入失败 status=%d", resp.StatusCode)
	}
	// 立即读：9.9.9.9 必须已在集合内（缓存已失效）
	set, err := c.knownTargets()
	if err != nil {
		t.Fatalf("knownTargets: %v", err)
	}
	if _, ok := set["9.9.9.9"]; !ok {
		t.Fatalf("新目标未进目标集合（缓存未失效），5 秒内它的上报会被当成非法行拒收")
	}
}

// 必须查 IP 唯一性（A4），否则同一 IP 挂两个 alias，历史数据劈成两半。
func TestTargetUpsertChecksIPUniqueness(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	srv := newTargetUpsertServer(t, c)
	defer srv.Close()

	resp := postTarget(t, srv.URL, "pw", "1.1.1.1", "1.1.1.1")
	resp.Body.Close()
	// 换一个 alias，但用已被占用的 IP → 必须被拒
	resp2 := postTarget(t, srv.URL, "pw", "other-alias", "1.1.1.1")
	defer resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Fatalf("IP 已被占用时应 400 拒绝，实际 %d", resp2.StatusCode)
	}
	if len(c.mem.ListTargets()) != 1 {
		t.Fatalf("被拒的写入不得落库，目标数期望 1 实际 %d", len(c.mem.ListTargets()))
	}
}

// ---------------------------------------------------------------------------
// P1-3：目标表清空是合法终态
// ---------------------------------------------------------------------------

// currentConfig 在表为空时必须标 Clear=true —— 否则节点把「清空」当异常忽略，
// 会永远继续探测已删除的目标。
func TestCurrentConfigMarksClearWhenEmpty(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	cfg, err := c.currentConfig()
	if err != nil {
		t.Fatalf("currentConfig: %v", err)
	}
	if !cfg.Clear {
		t.Fatalf("目标表为空时 Clear 必须为 true，否则清空目标这个操作永远推不下去")
	}
	if len(cfg.Targets) != 0 {
		t.Fatalf("Targets 应为空，实际 %d", len(cfg.Targets))
	}

	// 有目标时 Clear 必须为 false
	c.mem.UpsertTarget(TargetRow{Alias: "1.1.1.1", IP: "1.1.1.1"})
	cfg2, err := c.currentConfig()
	if err != nil {
		t.Fatalf("currentConfig2: %v", err)
	}
	if cfg2.Clear {
		t.Fatalf("有目标时 Clear 不应为 true")
	}
	if len(cfg2.Targets) != 1 {
		t.Fatalf("Targets 应有 1 个，实际 %d", len(cfg2.Targets))
	}
}

// 读表失败时 currentConfig 必须返回 error，绝不能把「异常」表达成「空列表」——
// 否则一次 PG 抖动就会把所有在线节点的目标清空、全部停摆。
func TestCurrentConfigErrorOnReadFailure(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	c.db = &CenterDB{listTargetsErr: fmt.Errorf("模拟 PG 抖动")}
	c.mem = nil
	if _, err := c.currentConfig(); err == nil {
		t.Fatalf("读表失败时 currentConfig 必须返回 error")
	}
}

// /agent/config 在读表失败时必须回 503，不能回空列表。
func TestHandleConfig503OnReadFailure(t *testing.T) {
	tok := "tok-cfgfail"
	c := newTestCenter2(tok, "pw")
	c.db = &CenterDB{listTargetsErr: fmt.Errorf("模拟 PG 抖动")}
	c.mem = nil
	mux := http.NewServeMux()
	mux.HandleFunc("POST /agent/config", c.handleConfig)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/agent/config", nil)
	ts := time.Now().Unix()
	req.Header.Set("X-PingAtlas-Auth", fmt.Sprintf("n1:%d:%s", ts, hmacToken(tok, fmt.Sprintf("n1|%d", ts))))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("读表失败应回 503（节点据此保持原有目标），实际 %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// P1-4：pinglog.target 是 alias，但前端传的是当前 IP
// ---------------------------------------------------------------------------

// 换过 IP 的目标（alias 不变、current_ip 已改）必须仍能按当前 IP 查到历史。
// 旧实现直接 `WHERE target=<当前IP>` ⇒ 换 IP 后历史与节点对比永久空白，
// 而同一份数据 DeleteTargetByIP 却能删掉 —— 删得掉、查不出的错位。
func TestResolveAliasAfterIPReplace(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	ctx := context.Background()

	// 初始：alias=1.1.1.1，current_ip=1.1.1.1
	c.mem.UpsertTarget(TargetRow{Alias: "1.1.1.1", IP: "1.1.1.1", Province: "广东"})
	// 执行 replace：alias 保持不变，只改 current_ip
	if n, err := c.mem.ReplaceTargetIP("1.1.1.1", "8.8.8.8"); err != nil || n != 1 {
		t.Fatalf("replace 失败: n=%d err=%v", n, err)
	}

	// 前端拿到的是新 IP 8.8.8.8，必须能解析回 alias=1.1.1.1
	alias, err := c.resolveAlias(ctx, "8.8.8.8")
	if err != nil {
		t.Fatalf("resolveAlias: %v", err)
	}
	if alias != "1.1.1.1" {
		t.Fatalf("当前 IP 8.8.8.8 应解析回 alias 1.1.1.1，实际 %q", alias)
	}
	// 用解析出的 alias 查历史才有数据（用当前 IP 查则为空）
	c.mem.InsertRows([]PingRow{{
		Logtime: time.Now().Truncate(time.Minute), Target: "1.1.1.1", NodeID: "n1", AvgDelay: 12,
	}})
	byIP := c.mem.TargetIPHistory("8.8.8.8", 24)
	byAlias := c.mem.TargetIPHistory(alias, 24)
	if len(byIP) != 0 {
		t.Fatalf("前提不成立：用当前 IP 直查本应有 0 行")
	}
	if len(byAlias) != 1 {
		t.Fatalf("换 IP 后按 alias 应查到 1 行历史，实际 %d 行（面板曲线会空白）", len(byAlias))
	}
}

// 未换过 IP 的老数据（alias==ip）也必须能解析，且解析幂等。
func TestResolveAliasUnchangedIP(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	ctx := context.Background()
	c.mem.UpsertTarget(TargetRow{Alias: "1.1.1.1", IP: "1.1.1.1"})

	for _, in := range []string{"1.1.1.1"} {
		a1, err := c.resolveAlias(ctx, in)
		if err != nil || a1 != "1.1.1.1" {
			t.Fatalf("未换 IP 时 %s 应解析为自身，实际 %q err=%v", in, a1, err)
		}
		// 幂等：拿解析结果再解析一次，结果不变
		a2, err := c.resolveAlias(ctx, a1)
		if err != nil || a2 != a1 {
			t.Fatalf("解析不幂等：%q -> %q", a1, a2)
		}
	}
}
