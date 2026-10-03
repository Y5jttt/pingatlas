//go:build center

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newMatrixFixture 构造两台节点（bj/xm）+ 三个目标（福建电信双节点、广东移动一节点超时、北京联通单节点）
func newMatrixFixture(t *testing.T) (*Center, *httptest.Server) {
	t.Helper()
	c := newTestCenter2("tok", "pw")
	c.conf.NodeGeo = map[string]NodeGeo{
		"node-bj": {Label: "北京一号"},
		"node-xm": {Label: "厦门一号"},
	}
	c.conf.SelfNode = "node-bj"
	c.registerNode("node-bj", "10.0.0.1", "0.2.0", 3, 0, 1)
	c.registerNode("node-xm", "10.0.0.2", "0.2.0", 3, 0, 2)
	c.mem.UpsertTarget(TargetRow{Alias: "t-fj", IP: "1.1.1.1", Province: "福建", City: "福州", Telecom: "ctcc"})
	c.mem.UpsertTarget(TargetRow{Alias: "t-gd", IP: "2.2.2.2", Province: "广东", City: "广州", Telecom: "cmcc"})
	c.mem.UpsertTarget(TargetRow{Alias: "t-bj", IP: "3.3.3.3", Province: "北京", City: "北京", Telecom: "cucc"})
	lt := time.Now().Truncate(time.Minute)
	c.mem.InsertRows([]PingRow{
		{Logtime: lt, Target: "t-fj", NodeID: "node-bj", AvgDelay: 40, SendPk: 20, RecvPk: 20},
		{Logtime: lt, Target: "t-fj", NodeID: "node-xm", AvgDelay: 6.6, SendPk: 20, RecvPk: 20},
		{Logtime: lt, Target: "t-gd", NodeID: "node-bj", AvgDelay: 0, SendPk: 20, RecvPk: 0, LossPk: 20}, // 超时
		{Logtime: lt, Target: "t-gd", NodeID: "node-xm", AvgDelay: 25.5, SendPk: 20, RecvPk: 20},
		{Logtime: lt, Target: "t-bj", NodeID: "node-bj", AvgDelay: 12, SendPk: 20, RecvPk: 20},
	})
	mux := http.NewServeMux()
	mux.HandleFunc(matrixPath, c.handleMatrix)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return c, srv
}

func getMatrix(t *testing.T, srv *httptest.Server, q string) (int, MatrixResp) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/matrix.json" + q)
	if err != nil {
		t.Fatalf("get matrix%s: %v", q, err)
	}
	defer resp.Body.Close()
	var m MatrixResp
	if resp.StatusCode == 200 {
		if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
			t.Fatalf("decode matrix%s: %v", q, err)
		}
	}
	return resp.StatusCode, m
}

func rowOf(m MatrixResp, alias string) *MatrixRow {
	for i := range m.Rows {
		if m.Rows[i].Alias == alias {
			return &m.Rows[i]
		}
	}
	return nil
}

func groupOf(gs []GroupStat, key string) *GroupStat {
	for i := range gs {
		if gs[i].Key == key {
			return &gs[i]
		}
	}
	return nil
}

// P1：矩阵接口 —— 目标×节点取值、最优/最差/差异、超时与无数据的区分、运营商/地区汇总、节点卡片。
func TestMatrixAPICore(t *testing.T) {
	_, srv := newMatrixFixture(t)
	code, m := getMatrix(t, srv, "")
	if code != 200 {
		t.Fatalf("HTTP %d", code)
	}
	if m.Scope != "best" || m.Window != "5m" || m.Total != 3 {
		t.Fatalf("scope/window/total 异常: %+v", m)
	}

	// 双节点目标：best 取更优、worst 取更差、spread 为差值
	fj := rowOf(m, "t-fj")
	if fj == nil {
		t.Fatalf("缺少 t-fj 行：%+v", m.Rows)
	}
	if fj.Cells["node-bj"].Delay != 40 || fj.Cells["node-xm"].Delay != 6.6 {
		t.Fatalf("单元格取值错误: %+v", fj.Cells)
	}
	if fj.Best != 6.6 || fj.BestNode != "node-xm" || fj.Worst != 40 || fj.WorstNode != "node-bj" {
		t.Fatalf("best/worst 错误: best=%v/%s worst=%v/%s", fj.Best, fj.BestNode, fj.Worst, fj.WorstNode)
	}
	if fj.Spread != 33.4 {
		t.Fatalf("spread 应为 33.4，实际 %v", fj.Spread)
	}
	if fj.Grade != "A" {
		t.Fatalf("6.6ms/0%% 应为 A，实际 %s", fj.Grade)
	}
	if fj.Missing != 0 {
		t.Fatalf("两节点都有数据，missing 应为 0，实际 %d", fj.Missing)
	}

	// 一节点超时：超时值不得参与最优；丢包按 100% 记；missing 仍为 0
	gd := rowOf(m, "t-gd")
	if gd == nil {
		t.Fatalf("缺少 t-gd 行")
	}
	if gd.Cells["node-bj"].Delay != 0 {
		t.Fatalf("超时应记为 0，实际 %v", gd.Cells["node-bj"].Delay)
	}
	if gd.Cells["node-bj"].Loss != 100 {
		t.Fatalf("全丢应记 100%%，实际 %v", gd.Cells["node-bj"].Loss)
	}
	if gd.Best != 25.5 || gd.BestNode != "node-xm" {
		t.Fatalf("最优应跳过超时行取 25.5，实际 %v/%s", gd.Best, gd.BestNode)
	}
	if gd.Spread != -1 {
		t.Fatalf("有一侧超时时 spread 应为 -1，实际 %v", gd.Spread)
	}

	// 单节点有数据 = 另一节点缺失 ⇒ 同样"不可比较"
	bj := rowOf(m, "t-bj")
	if bj.Missing != 1 {
		t.Fatalf("t-bj 只上报 1 台，missing 应为 1，实际 %d", bj.Missing)
	}
	if bj.Spread != -1 {
		t.Fatalf("缺一台时 spread 应为 -1（不可比较），实际 %v", bj.Spread)
	}
	if bj.Grade != "A" {
		t.Fatalf("12ms/0%% 应为 A，实际 %s", bj.Grade)
	}

	// ② 运营商汇总：最快/最慢必须带节点名与地点（跨节点取），并带上被探测 IP 的运营商
	ctcc := groupOf(m.ByTelecom, "ctcc")
	if ctcc == nil || ctcc.Fastest.Node != "node-xm" || ctcc.Fastest.Value != 6.6 || ctcc.Fastest.Where != "福建福州" {
		t.Fatalf("电信最快项错误: %+v", ctcc)
	}
	if ctcc.Fastest.Telecom != "ctcc" || ctcc.Slowest.Telecom != "ctcc" {
		t.Fatalf("最快/最慢未带被探测 IP 的运营商: fast=%q slow=%q", ctcc.Fastest.Telecom, ctcc.Slowest.Telecom)
	}
	if ctcc.Slowest.Node != "node-bj" || ctcc.Slowest.Value != 40 {
		t.Fatalf("电信最慢项错误: %+v", ctcc.Slowest)
	}
	if ctcc.Targets != 1 {
		t.Fatalf("电信目标数应为 1，实际 %d", ctcc.Targets)
	}
	// ③ 地区汇总：福建归华东
	hd := groupOf(m.ByRegion, "华东")
	if hd == nil || hd.Fastest.Value != 6.6 {
		t.Fatalf("华东汇总错误: %+v", hd)
	}
	// 地图省份聚合 = 展示值的最小值
	if m.ProvinceAgg["福建"] != 6.6 || m.ProvinceAgg["广东"] != 25.5 || m.ProvinceAgg["北京"] != 12 {
		t.Fatalf("省份聚合错误: %+v", m.ProvinceAgg)
	}

	// ① 节点卡片：显示名/本机标记/坐标/spark 长度
	if len(m.Nodes) != 2 {
		t.Fatalf("节点卡片应 2 张，实际 %d", len(m.Nodes))
	}
	var bjCard, xmCard *NodeCard
	for i := range m.Nodes {
		switch m.Nodes[i].ID {
		case "node-bj":
			bjCard = &m.Nodes[i]
		case "node-xm":
			xmCard = &m.Nodes[i]
		}
	}
	if bjCard == nil || xmCard == nil {
		t.Fatalf("节点卡片 id 异常: %+v", m.Nodes)
	}
	if bjCard.Label != "北京一号" || !bjCard.Self {
		t.Fatalf("本机节点卡片错误: %+v", bjCard)
	}
	if xmCard.Label != "厦门一号" || xmCard.Self {
		t.Fatalf("厦门节点卡片错误: %+v", xmCard)
	}
	if len(bjCard.Spark) != 24 {
		t.Fatalf("spark 应为 24 点，实际 %d", len(bjCard.Spark))
	}
	if !bjCard.Online {
		t.Fatalf("刚注册的节点应在线")
	}
	if bjCard.Avg <= 0 {
		t.Fatalf("节点窗口均值应 > 0，实际 %v", bjCard.Avg)
	}
}

// P1：过滤与口径（province/telecom/only/q/node/window/未知节点）
func TestMatrixFiltersAndScope(t *testing.T) {
	_, srv := newMatrixFixture(t)

	if _, m := getMatrix(t, srv, "?province=福建"); len(m.Rows) != 1 || m.Rows[0].Alias != "t-fj" {
		t.Fatalf("按省过滤失败: %+v", m.Rows)
	}
	if _, m := getMatrix(t, srv, "?telecom=cucc"); len(m.Rows) != 1 || m.Rows[0].Alias != "t-bj" {
		t.Fatalf("按运营商过滤失败: %+v", m.Rows)
	}
	if _, m := getMatrix(t, srv, "?q=福州"); len(m.Rows) != 1 || m.Rows[0].Alias != "t-fj" {
		t.Fatalf("关键词过滤失败: %+v", m.Rows)
	}
	// only=diff：只有"两节点都有有效值且不同"的行
	if _, m := getMatrix(t, srv, "?only=diff"); len(m.Rows) != 1 || m.Rows[0].Alias != "t-fj" {
		t.Fatalf("only=diff 失败: %+v", m.Rows)
	}
	// only=timeout：任一节点超时或无数据 → t-gd(超时) 与 t-bj(缺一台)
	_, m := getMatrix(t, srv, "?only=timeout")
	if len(m.Rows) != 2 {
		t.Fatalf("only=timeout 应命中 2 行，实际 %d: %+v", len(m.Rows), m.Rows)
	}
	for _, r := range m.Rows {
		if r.Alias == "t-fj" {
			t.Fatalf("t-fj 两节点都正常，不应出现在超时筛选里")
		}
	}

	// 单节点口径：单元格只剩该节点，展示值/评级基于该节点
	_, m = getMatrix(t, srv, "?node=node-bj")
	if m.Scope != "node" || m.Node != "node-bj" {
		t.Fatalf("口径字段错误: %+v", m)
	}
	fj := rowOf(m, "t-fj")
	if len(fj.Cells) != 1 || fj.Cells["node-bj"].Delay != 40 {
		t.Fatalf("单节点口径单元格错误: %+v", fj.Cells)
	}
	if fj.Missing != 0 {
		t.Fatalf("单节点口径 missing 应为 0，实际 %d", fj.Missing)
	}
	if fj.Grade != "A" {
		t.Fatalf("40ms/0%% 应为 A，实际 %s", fj.Grade)
	}
	// 面板也只反映该节点：电信最快=北京 40
	if ctcc := groupOf(m.ByTelecom, "ctcc"); ctcc == nil || ctcc.Fastest.Node != "node-bj" || ctcc.Fastest.Value != 40 {
		t.Fatalf("单节点口径下汇总未跟随: %+v", ctcc)
	}

	// 未知节点必须 400
	if code, _ := getMatrix(t, srv, "?node=ghost"); code != 400 {
		t.Fatalf("未知节点应 400，实际 %d", code)
	}

	// 历史口径（24h 均值）不应报错且仍返回三行
	code, m := getMatrix(t, srv, "?window=24h")
	if code != 200 || m.Window != "24h" || len(m.Rows) != 3 {
		t.Fatalf("24h 口径异常: %d %s %d 行", code, m.Window, len(m.Rows))
	}
	if r := rowOf(m, "t-fj"); r == nil || r.Cells["node-xm"].Delay <= 0 {
		t.Fatalf("24h 均值口径取值异常: %+v", r)
	}

	// 排序：默认按差异降序，t-fj(33.4) 必须排第一
	if _, m := getMatrix(t, srv, ""); m.Rows[0].Alias != "t-fj" {
		t.Fatalf("默认排序应按 spread 降序，实际首行 %s", m.Rows[0].Alias)
	}
	// 分页
	if _, m := getMatrix(t, srv, "?limit=1&offset=1"); len(m.Rows) != 1 || m.Total != 3 {
		t.Fatalf("分页错误: total=%d rows=%d", m.Total, len(m.Rows))
	}
}

// 质量评级边界（改一处阈值必须让这里红）
func TestMatrixGrade(t *testing.T) {
	cases := []struct {
		delay, loss float64
		want        string
	}{
		{-1, -1, "-"}, {0, 100, "D"}, {2000, 0, "D"},
		{50, 0.9, "A"}, {50, 1, "B"}, {100, 4.9, "B"}, {100, 5, "C"},
		{200, 19, "C"}, {200, 20, "D"}, {39.09, 0, "A"}, {250, 0, "D"},
	}
	for _, tc := range cases {
		if got := gradeOf(tc.delay, tc.loss); got != tc.want {
			t.Fatalf("gradeOf(%v,%v)=%s，期望 %s", tc.delay, tc.loss, got, tc.want)
		}
	}
}
