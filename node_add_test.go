//go:build center

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// addNodeReq 调一次 /api/admin/node/add，返回状态码与解析后的响应体
func addNodeReq(t *testing.T, c *Center, srv *httptest.Server, body map[string]interface{}) (int, map[string]interface{}) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/admin/node/add", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Pwd", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("node/add: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// 新流程：只给 地区 + 运营商 → 随机 id + 自动拼显示名 + 一条安装命令
func TestAdminNodeAddAutoIDAndLabel(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	mux := http.NewServeMux()
	c.registerAdminAPI(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	code, out := addNodeReq(t, c, srv, map[string]interface{}{
		"province": "四川", "city": "成都", "telecom": "tencent",
	})
	if code != 200 {
		t.Fatalf("只给地区+运营商必须成功，实际 %d: %v", code, out)
	}
	name, _ := out["name"].(string)
	if !regexp.MustCompile(`^n-[0-9a-f]{12}$`).MatchString(name) {
		t.Fatalf("节点 id 应为 n-+12位十六进制，实际 %q", name)
	}
	if out["label"] != "成都腾讯" {
		t.Fatalf("显示名应为「成都腾讯」，实际 %v", out["label"])
	}
	if out["province"] != "四川" || out["city"] != "成都" || out["telecom"] != "tencent" {
		t.Fatalf("归属字段未回显: %v", out)
	}
	cmd, _ := out["install_cmd"].(string)
	if !strings.Contains(cmd, "/agent/install.sh?name="+name+"&code=") || !strings.HasPrefix(cmd, "curl -fsSL ") {
		t.Fatalf("安装命令不对: %q", cmd)
	}
	if !c.mem.NodeExists(name) {
		t.Fatalf("节点应已注册: %s", name)
	}
	// 显示名落库后，首页取名字的路径必须能拿到它（这就是"不用改 center.json"的关键）
	m := c.nodeLabelMap(context.Background())
	if m[name] != "成都腾讯" {
		t.Fatalf("nodeLabelMap 未返回自动拼的显示名: %v", m)
	}
	if got := c.nodeLabel(m, name); got != "成都腾讯" {
		t.Fatalf("nodeLabel 未用数据库显示名，得到 %q", got)
	}
	// 再补一个：显式 label 覆盖自动拼接
	code2, out2 := addNodeReq(t, c, srv, map[string]interface{}{
		"province": "四川", "city": "成都", "telecom": "aliyun", "label": "成都阿里云BGP",
	})
	if code2 != 200 || out2["label"] != "成都阿里云BGP" {
		t.Fatalf("显式 label 应覆盖自动拼接: %d %v", code2, out2)
	}
}

// 参数校验：非法省名 / 非法运营商 / 既没名字也没地区运营商 / 重名
func TestAdminNodeAddValidation(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	mux := http.NewServeMux()
	c.registerAdminAPI(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cases := []struct {
		name string
		body map[string]interface{}
	}{
		{"省名不存在", map[string]interface{}{"province": "巴蜀", "city": "成都", "telecom": "tencent"}},
		{"既无名字也无地区运营商", map[string]interface{}{}},
		{"只有地区没有运营商", map[string]interface{}{"city": "成都"}},
		{"名字含非法字符", map[string]interface{}{"name": "成都节点"}},
		{"地区文本过长", map[string]interface{}{"city": strings.Repeat("城", 25), "telecom": "tencent"}},
		{"运营商文本过长", map[string]interface{}{"city": "成都", "telecom": strings.Repeat("运", 25)}},
	}
	for _, tc := range cases {
		if code, _ := addNodeReq(t, c, srv, tc.body); code != 400 {
			t.Fatalf("%s: 应 400，实际 %d", tc.name, code)
		}
	}

	// 运营商允许自由文本（面板就是两个输入框，用户写中文）：显示名原样拼接
	if code, out := addNodeReq(t, c, srv, map[string]interface{}{"city": "成都", "telecom": "腾讯云"}); code != 200 {
		t.Fatalf("自由文本运营商应被接受，实际 %d %v", code, out)
	} else if out["label"] != "成都腾讯云" {
		t.Fatalf("自由文本应原样拼进显示名，实际 %v", out["label"])
	}
	// 固定取值仍走简称映射
	if code, out := addNodeReq(t, c, srv, map[string]interface{}{"city": "厦门", "telecom": "ctcc"}); code != 200 {
		t.Fatalf("固定取值应被接受，实际 %d", code)
	} else if out["label"] != "厦门电信" {
		t.Fatalf("固定取值应映射成简称，实际 %v", out["label"])
	}

	// 重名必须被拒（旧实现会静默覆盖 token，导致旧节点失联）
	if code, _ := addNodeReq(t, c, srv, map[string]interface{}{"name": "dup-node"}); code != 200 {
		t.Fatalf("首次注册应成功，实际 %d", code)
	}
	if code, _ := addNodeReq(t, c, srv, map[string]interface{}{"name": "dup-node"}); code != 400 {
		t.Fatalf("重名应 400，实际 %d", code)
	}

	// 老用法（只给名字）仍然可用，显示名留空 → 退回 NodeGeo / 节点 id
	c.conf.NodeGeo = map[string]NodeGeo{"old-node": {Label: "老配置名"}}
	if code, _ := addNodeReq(t, c, srv, map[string]interface{}{"name": "old-node"}); code != 200 {
		t.Fatalf("老用法应成功，实际 %d", code)
	}
	m := c.nodeLabelMap(context.Background())
	if got := c.nodeLabel(m, "old-node"); got != "老配置名" {
		t.Fatalf("显示名应退回 NodeGeo，得到 %q", got)
	}
	if got := c.nodeLabel(m, "never-seen"); got != "never-seen" {
		t.Fatalf("两者都没有时应退回节点 id，得到 %q", got)
	}
}

// 显示名优先级：数据库 label > center.json NodeGeo.Label > 节点 id
func TestNodeLabelPriority(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	c.conf.NodeGeo = map[string]NodeGeo{"n1": {Label: "配置里的名字"}}
	if got := c.nodeLabel(nil, "n1"); got != "配置里的名字" {
		t.Fatalf("无数据库时应退回配置，得到 %q", got)
	}
	m := map[string]string{"n1": "数据库里的名字"}
	if got := c.nodeLabel(m, "n1"); got != "数据库里的名字" {
		t.Fatalf("数据库显示名应优先于配置，得到 %q", got)
	}
	if got := c.nodeLabel(m, "n2"); got != "n2" {
		t.Fatalf("都没有应退回 id，得到 %q", got)
	}
	// 空串不能当成有效显示名（否则会把节点名显示成空白）
	if got := c.nodeLabel(map[string]string{"n1": "   "}, "n1"); got != "配置里的名字" {
		t.Fatalf("空白显示名应被忽略，得到 %q", got)
	}
}

// 全局 Token 的三种配置语义：
//   - 键缺失     → 随机生成并写回配置（便利，兼容单机快速上手）
//   - 写了空串   → 明确关闭"万能钥匙"，只接受各节点独立 token（本次新增）
//   - 有值       → 老行为，兼容尚未换独立 token 的节点
func TestLoadCenterConfTokenSemantics(t *testing.T) {
	dir := t.TempDir()

	// 1) 键缺失 → 生成并写回
	p1 := filepath.Join(dir, "c1.json")
	if err := os.WriteFile(p1, []byte(`{"Listen":":18991","DB":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c1 := loadCenterConf(p1, "", "")
	if c1.Token == "" || len(c1.Token) != 32 {
		t.Fatalf("键缺失时应随机生成 32 位 token，得到 %q", c1.Token)
	}
	b, _ := os.ReadFile(p1)
	if !strings.Contains(string(b), c1.Token) {
		t.Fatalf("生成的 token 应写回配置文件")
	}

	// 2) 显式空串 → 不生成、不启用全局分支，且不覆盖已设的 AdminPwd
	p2 := filepath.Join(dir, "c2.json")
	if err := os.WriteFile(p2, []byte(`{"Listen":":18991","Token":"","AdminPwd":"separate-pw"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c2 := loadCenterConf(p2, "", "")
	if c2.Token != "" {
		t.Fatalf("Token 显式留空时不得再生成，得到 %q", c2.Token)
	}
	if c2.AdminPwd != "separate-pw" {
		t.Fatalf("显式留空时不应把 Token 迁移成 AdminPwd，得到 %q", c2.AdminPwd)
	}
	b2, _ := os.ReadFile(p2)
	if strings.Contains(string(b2), `"Token": "`) && !strings.Contains(string(b2), `"Token": ""`) {
		t.Fatalf("配置文件不应被写入新 token")
	}

	// 3) 老配置（有 Token 无 AdminPwd）→ 仍按老规则迁移，保证升级不锁死面板
	p3 := filepath.Join(dir, "c3.json")
	if err := os.WriteFile(p3, []byte(`{"Listen":":18991","Token":"legacytoken12345"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c3 := loadCenterConf(p3, "", "")
	if c3.AdminPwd != "legacytoken12345" {
		t.Fatalf("老配置应把 Token 迁移成 AdminPwd，得到 %q", c3.AdminPwd)
	}
	if c3.Token != "legacytoken12345" {
		t.Fatalf("老配置的全局 Token 应保持不变，得到 %q", c3.Token)
	}
}

// 管理面板「节点管理」要显示归属信息：node/status 必须带 province/city/telecom + 显示名
func TestAdminNodeStatusShowsGeo(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	mux := http.NewServeMux()
	c.registerAdminAPI(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 新流程建两个节点：一个自由文本、一个固定取值
	if code, _ := addNodeReq(t, c, srv, map[string]interface{}{"city": "成都", "telecom": "腾讯云"}); code != 200 {
		t.Fatalf("建节点失败: %d", code)
	}
	if code, _ := addNodeReq(t, c, srv, map[string]interface{}{"city": "厦门", "telecom": "ctcc"}); code != 200 {
		t.Fatalf("建节点失败: %d", code)
	}
	// 再加一个老节点（没有归属信息，但有 NodeGeo 显示名）
	c.conf.NodeGeo = map[string]NodeGeo{"legacy-1": {Label: "北京一号"}}
	c.mem.UpsertNode(NodeInfo{NodeID: "legacy-1"})

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/admin/node/status", nil)
	req.Header.Set("X-Admin-Pwd", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Nodes []struct {
			Name     string `json:"name"`
			Display  string `json:"display"`
			Province string `json:"province"`
			City     string `json:"city"`
			Telecom  string `json:"telecom"`
			Auth     string `json:"auth"`
		} `json:"nodes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	byName := map[string]int{}
	for i, n := range out.Nodes {
		byName[n.Name] = i
	}
	find := func(city, telecom, wantDisplay string) {
		t.Helper()
		for _, n := range out.Nodes {
			if n.City != city || n.Telecom != telecom {
				continue
			}
			if n.Display != wantDisplay {
				t.Fatalf("显示名应为 %q，实际 %q", wantDisplay, n.Display)
			}
			return
		}
		t.Fatalf("没找到 city=%s telecom=%s 的节点", city, telecom)
	}
	find("成都", "腾讯云", "成都腾讯云")
	find("厦门", "ctcc", "厦门电信")
	// 老节点：city/telecom 为空，但显示名走 NodeGeo
	if i, ok := byName["legacy-1"]; !ok {
		t.Fatalf("老节点应在列表里")
	} else if out.Nodes[i].Display != "北京一号" {
		t.Fatalf("老节点显示名应退回 NodeGeo，实际 %q", out.Nodes[i].Display)
	}
	// 中心自己那条也要有 display
	if i, ok := byName["center("+c.conf.Listen+")"]; ok && out.Nodes[i].Display != "中心" {
		t.Fatalf("中心条目 display 应为「中心」，实际 %q", out.Nodes[i].Display)
	}
}

// 改名 / 改归属 / 前台显示开关
func TestAdminNodeEdit(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	mux := http.NewServeMux()
	c.registerAdminAPI(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, created := addNodeReq(t, c, srv, map[string]interface{}{"city": "成都", "telecom": "腾讯云"})
	name, _ := created["name"].(string)
	if name == "" {
		t.Fatalf("建节点失败: %v", created)
	}

	edit := func(body map[string]interface{}) (int, map[string]interface{}) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/admin/node/edit", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Admin-Pwd", "pw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("node/edit: %v", err)
		}
		defer resp.Body.Close()
		var out map[string]interface{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	status := func() map[string]map[string]interface{} {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/admin/node/status", nil)
		req.Header.Set("X-Admin-Pwd", "pw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var d struct {
			Nodes []map[string]interface{} `json:"nodes"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&d)
		m := map[string]map[string]interface{}{}
		for _, n := range d.Nodes {
			if s, _ := n["name"].(string); s != "" {
				m[s] = n
			}
		}
		return m
	}

	// ① 显式改显示名
	if code, out := edit(map[string]interface{}{"name": name, "label": "成都腾讯云-机房A"}); code != 200 {
		t.Fatalf("改名应 200，实际 %d %v", code, out)
	} else if out["label"] != "成都腾讯云-机房A" {
		t.Fatalf("显示名应为新值，实际 %v", out["label"])
	}
	if got := status()[name]["display"]; got != "成都腾讯云-机房A" {
		t.Fatalf("status 里显示名应是新值，实际 %v", got)
	}
	if got := status()[name]["city"]; got != "成都" {
		t.Fatalf("没改的字段应保持原值，实际 %v", got)
	}

	// ② 只改地区/运营商 → 显示名自动重算
	if code, out := edit(map[string]interface{}{"name": name, "city": "绵阳", "telecom": "ctcc"}); code != 200 {
		t.Fatalf("改归属应 200，实际 %d", code)
	} else if out["label"] != "绵阳电信" {
		t.Fatalf("显示名应按新归属重算为「绵阳电信」，实际 %v", out["label"])
	}

	// ③ 前台隐藏：节点仍在（照常采集），但首页节点列表里没有它
	if code, out := edit(map[string]interface{}{"name": name, "visible": false}); code != 200 {
		t.Fatalf("隐藏应 200，实际 %d", code)
	} else if out["visible"] != false {
		t.Fatalf("visible 应为 false，实际 %v", out["visible"])
	}
	ids := c.knownNodeIDs(context.Background())
	for _, id := range ids {
		if id == name {
			t.Fatalf("隐藏的节点不应出现在首页节点列表里: %v", ids)
		}
	}
	if _, ok := status()[name]; !ok {
		t.Fatalf("面板仍应能看到隐藏的节点")
	}
	// 再显示回来
	if code, _ := edit(map[string]interface{}{"name": name, "visible": true}); code != 200 {
		t.Fatalf("恢复显示应 200，实际 %d", code)
	}
	found := false
	for _, id := range c.knownNodeIDs(context.Background()) {
		if id == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("恢复后应重新出现在首页节点列表里")
	}

	// ④ 边界：不存在的节点 / 过长的显示名
	if code, _ := edit(map[string]interface{}{"name": "no-such-node", "label": "x"}); code != 404 {
		t.Fatalf("不存在的节点应 404，实际 %d", code)
	}
	if code, _ := edit(map[string]interface{}{"name": name, "label": strings.Repeat("长", 49)}); code != 400 {
		t.Fatalf("过长显示名应 400，实际 %d", code)
	}
}

// 表单选项：省份固定列表 + 省→市（来自目标表）+ 运营商取值
func TestAdminGeoOptions(t *testing.T) {
	c := newTestCenter2("tok", "pw")
	c.mem.UpsertTarget(TargetRow{Alias: "t1", IP: "1.1.1.1", Province: "四川", City: "成都", Telecom: "ctcc"})
	c.mem.UpsertTarget(TargetRow{Alias: "t2", IP: "1.1.1.2", Province: "四川", City: "乐山AS4809", Telecom: "ctcc"})
	c.mem.UpsertTarget(TargetRow{Alias: "t3", IP: "1.1.1.3", Province: "四川", City: "成都", Telecom: "cucc"})
	c.mem.UpsertTarget(TargetRow{Alias: "t4", IP: "1.1.1.4", Province: "福建", City: "福州", Telecom: "cucc"})
	mux := http.NewServeMux()
	c.registerAdminAPI(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/admin/geo/options", nil)
	req.Header.Set("X-Admin-Pwd", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("geo/options: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("geo/options 应 200，实际 %d", resp.StatusCode)
	}
	var out struct {
		Provinces []string            `json:"provinces"`
		Cities    map[string][]string `json:"cities"`
		Telecoms  []struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"telecoms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if len(out.Provinces) != len(provinceRegion) || len(out.Provinces) < 30 {
		t.Fatalf("省份列表应为全国 %d 项，实际 %d", len(provinceRegion), len(out.Provinces))
	}
	if len(out.Telecoms) != len(nodeTelSuffix) {
		t.Fatalf("运营商取值应为 %d 项，实际 %d", len(nodeTelSuffix), len(out.Telecoms))
	}
	sc := out.Cities["四川"]
	// 城市名要归一化（去掉 AS4809 后缀）并去重、排序
	if len(sc) != 2 || sc[0] != "乐山" || sc[1] != "成都" {
		t.Fatalf("四川城市列表应归一化为有序去重的纯城市名: %v", sc)
	}
	for _, c := range sc {
		if strings.Contains(c, "AS") {
			t.Fatalf("城市候选不应带 AS 后缀: %v", sc)
		}
	}
}
