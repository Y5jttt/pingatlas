//go:build center

package main

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// /api/matrix.json —— "目标 × 节点"矩阵 + 运营商/地区汇总 + 节点卡片
//
// 设计要点（与 /api/mapping.json 共用同一套口径）：
//   * 数值约定：-1 = 该窗口内没有任何数据（无数据）；0 = 20 个包一个没回（超时）。
//     两者在前端是不同颜色（灰 / 红），不能混。
//   * window=5m/15m/1h ⇒ **实时口径**：取窗口内每个 (目标,节点) 的最新一行；
//     window=24h/7d    ⇒ **历史口径**：取窗口内每个 (目标,节点) 的有效样本均值。
//   * ?node=xxx 只统计该节点（与 mapping 的 ?node= 校验规则一致，未知节点 400）。
//   * 表格过滤（province/telecom/only/q）只影响 rows；②③ 汇总面板始终基于
//     "当前窗口+当前节点口径下的全部目标"，否则过滤一改面板数字就跳。
//   * 汇总面板里的"最快/最慢"是**跨节点**取的（形如"最快: 广东中山电信 9ms"），
//     这正是多节点监控最该被看见的东西。
// ============================================================================

// matrixCell 目标在某节点上的展示值
type matrixCell struct {
	Avg  float64
	Min  float64
	Max  float64
	Send int
	Recv int
	Loss int
	Log  time.Time
}

// cellJSON 出参：delay/loss 用 -1 表示无数据
type cellJSON struct {
	Delay   float64 `json:"delay"`
	Loss    float64 `json:"loss"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	Send    int     `json:"send"`
	Recv    int     `json:"recv"`
	Logtime string  `json:"logtime"`
}

// MatrixRow 结果表一行（一个目标）
type MatrixRow struct {
	Alias     string              `json:"alias"`
	IP        string              `json:"ip"`
	Province  string              `json:"province"`
	City      string              `json:"city"`
	Telecom   string              `json:"telecom"`
	Cells     map[string]cellJSON `json:"cells"`
	Best      float64             `json:"best"`       // -1 无数据
	BestNode  string              `json:"best_node"`  // 节点 id
	Worst     float64             `json:"worst"`      // -1 无数据
	WorstNode string              `json:"worst_node"` // 节点 id
	Spread    float64             `json:"spread"`     // 节点间差异；-1 = 不可比较（有节点超时/无数据/未覆盖）
	Grade     string              `json:"grade"`      // A/B/C/D/-（基于展示值）
	Missing   int                 `json:"missing"`    // 本窗口内没有上报的节点数（节点掉线/未覆盖）
}

// NodeGeo 节点的展示信息（center.json 的 NodeGeo 配置）。
// 只用于"中文显示名"：地图上不再标注探测节点位置，所以不需要经纬度，
// 新增节点也不必再手填坐标（旧配置里残留的 lng/lat 会被忽略）。
type NodeGeo struct {
	Label string `json:"label"`
}

// NodeCard 右侧"探测节点卡片"
type NodeCard struct {
	ID      string    `json:"id"`
	Label   string    `json:"label"`
	Online  bool      `json:"online"`
	Self    bool      `json:"self"` // 中心自己托管的那台（center.json 的 SelfNode）
	Version string    `json:"version"`
	Last    string    `json:"last"`
	Avg     float64   `json:"avg"`     // 窗口内有效样本均值
	Loss    float64   `json:"loss"`    // 窗口内丢包率 %
	Targets int       `json:"targets"` // 窗口内有数据的去重目标数
	Spark   []float64 `json:"spark"`   // 近 24h 每小时均值（-1 = 无数据）
}

// GroupExtreme "最快/最慢"极端点（带节点名与地点，便于一眼定位）
type GroupExtreme struct {
	Node    string  `json:"node"`
	Label   string  `json:"label"`
	Value   float64 `json:"value"`
	Where   string  `json:"where"`   // 省+市，如 "福建福州"
	Telecom string  `json:"telecom"` // 被探测 IP 的运营商（ctcc/cucc/cmcc）
}

// GroupStat ②运营商汇总 / ③地区汇总的一行
type GroupStat struct {
	Key     string       `json:"key"`
	Label   string       `json:"label"`
	Fastest GroupExtreme `json:"fastest"`
	Slowest GroupExtreme `json:"slowest"`
	Avg     float64      `json:"avg"`
	Loss    float64      `json:"loss"`
	Targets int          `json:"targets"`
}

// MatrixResp /api/matrix.json 响应
type MatrixResp struct {
	AsOf        string             `json:"as_of"`
	Scope       string             `json:"scope"` // best | node
	Node        string             `json:"node"`
	Window      string             `json:"window"`
	Nodes       []NodeCard         `json:"nodes"`
	Rows        []MatrixRow        `json:"rows"`
	Total       int                `json:"total"` // 过滤前的目标总数
	ByTelecom   []GroupStat        `json:"byTelecom"`
	ByRegion    []GroupStat        `json:"byRegion"`
	ProvinceAgg map[string]float64 `json:"provinceAgg"` // 省 -> 展示值（地图用）
}

// provinceRegion 大区映射（华东/华南/… 汇总口径）
var provinceRegion = map[string]string{
	"北京": "华北", "天津": "华北", "河北": "华北", "山西": "华北", "内蒙古": "华北",
	"上海": "华东", "江苏": "华东", "浙江": "华东", "安徽": "华东", "福建": "华东",
	"江西": "华东", "山东": "华东",
	"河南": "华中", "湖北": "华中", "湖南": "华中",
	"广东": "华南", "广西": "华南", "海南": "华南",
	"重庆": "西南", "四川": "西南", "贵州": "西南", "云南": "西南", "西藏": "西南",
	"陕西": "西北", "甘肃": "西北", "青海": "西北", "宁夏": "西北", "新疆": "西北",
	"辽宁": "东北", "吉林": "东北", "黑龙江": "东北",
	"香港": "港澳台", "澳门": "港澳台", "台湾": "港澳台",
}

var regionOrder = []string{"华东", "华南", "华中", "华北", "西南", "西北", "东北", "港澳台"}

var telOrder = []string{"ctcc", "cucc", "cmcc"}

// gradeOf 质量评级：基于"当前展示值"（全网最优或指定节点的值）
func gradeOf(delay, loss float64) string {
	if delay < 0 {
		return "-"
	}
	if delay == 0 || delay >= 2000 {
		return "D"
	}
	switch {
	case delay <= 50 && loss < 1:
		return "A"
	case delay <= 100 && loss < 5:
		return "B"
	case delay <= 200 && loss < 20:
		return "C"
	default:
		return "D"
	}
}

// windowSpec 窗口解析：24h/7d 走历史均值口径
type windowSpec struct {
	Key string
	Dur time.Duration
	Avg bool
}

func parseWindow(s string) windowSpec {
	switch strings.TrimSpace(s) {
	case "15m":
		return windowSpec{"15m", 15 * time.Minute, false}
	case "1h":
		return windowSpec{"1h", time.Hour, false}
	case "24h":
		return windowSpec{"24h", 24 * time.Hour, true}
	case "7d":
		return windowSpec{"7d", 168 * time.Hour, true}
	default:
		return windowSpec{"5m", 5 * time.Minute, false}
	}
}

// ---------- DB 查询 ----------

// MatrixCells 取窗口内每个 (目标,节点) 的展示值。
// avgMode=false：取最新一行；avgMode=true：取窗口内有效样本均值（历史口径）。
func (c *CenterDB) MatrixCells(ctx context.Context, w windowSpec, nodeID string) (map[string]map[string]matrixCell, error) {
	out := map[string]map[string]matrixCell{}
	if !w.Avg {
		rows, err := c.pool.Query(ctx, `SELECT DISTINCT ON (target, node_id)
			target, node_id, avgdelay, mindelay, maxdelay, sendpk, revcpk, losspk, logtime
			FROM pinglog WHERE logtime > now() - make_interval(secs => $1)
			  AND ($2 = '' OR node_id = $2)
			ORDER BY target, node_id, logtime DESC, (avgdelay <= 0) ASC, avgdelay ASC`,
			int(w.Dur.Seconds()), nodeID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var tg, nid string
			var cell matrixCell
			if err := rows.Scan(&tg, &nid, &cell.Avg, &cell.Min, &cell.Max, &cell.Send, &cell.Recv, &cell.Loss, &cell.Log); err != nil {
				return nil, err
			}
			putCell(out, tg, nid, cell)
		}
		return out, rows.Err()
	}

	// 历史口径：有效样本（avgdelay>0）才参与延迟平均，避免超时把曲线拉到 0
	rows, err := c.pool.Query(ctx, `SELECT target, node_id,
			avg(avgdelay) FILTER (WHERE avgdelay > 0) AS d,
			min(mindelay) FILTER (WHERE avgdelay > 0) AS mn,
			max(maxdelay) AS mx,
			avg(sendpk)::float AS s, avg(revcpk)::float AS r, avg(losspk)::float AS l,
			max(logtime) AS lt
			FROM pinglog WHERE logtime > now() - make_interval(secs => $1)
			  AND ($2 = '' OR node_id = $2)
			GROUP BY target, node_id`, int(w.Dur.Seconds()), nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var tg, nid string
		var d, mn, s, r, l *float64
		var mx float64
		var lt time.Time
		if err := rows.Scan(&tg, &nid, &d, &mn, &mx, &s, &r, &l, &lt); err != nil {
			return nil, err
		}
		// d == NULL 表示窗口内全是超时行 ⇒ 展示为超时(0)，而不是"无数据"
		cell := matrixCell{Avg: fv(d), Min: fv(mn), Max: mx, Log: lt}
		cell.Send = int(fv(s) + 0.5)
		cell.Recv = int(fv(r) + 0.5)
		cell.Loss = int(fv(l) + 0.5)
		putCell(out, tg, nid, cell)
	}
	return out, rows.Err()
}

func fv(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func putCell(m map[string]map[string]matrixCell, target, node string, cell matrixCell) {
	if m[target] == nil {
		m[target] = map[string]matrixCell{}
	}
	m[target][node] = cell
}

// NodeAggStat 单节点窗口内聚合
type NodeAggStat struct {
	Avg     float64
	Loss    float64
	Targets int
}

// NodeWindowAgg 每个节点在窗口内的整体表现（节点卡片用）
func (c *CenterDB) NodeWindowAgg(ctx context.Context, w windowSpec) (map[string]NodeAggStat, error) {
	rows, err := c.pool.Query(ctx, `SELECT node_id,
			avg(avgdelay) FILTER (WHERE avgdelay > 0) AS d,
			avg(losspk)::float AS l, avg(sendpk)::float AS s,
			count(DISTINCT target) AS tg
			FROM pinglog WHERE logtime > now() - make_interval(secs => $1)
			GROUP BY node_id`, int(w.Dur.Seconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]NodeAggStat{}
	for rows.Next() {
		var nid string
		var d, l, s *float64
		var tg int
		if err := rows.Scan(&nid, &d, &l, &s, &tg); err != nil {
			return nil, err
		}
		send := fv(s)
		loss := 0.0
		if send >= 1 {
			loss = fv(l) * 100 / send
		}
		out[nid] = NodeAggStat{Avg: round2(fv(d)), Loss: round2(loss), Targets: tg}
	}
	return out, rows.Err()
}

// NodeHourlySpark 近 hours 小时每节点逐小时均值（密集数组，-1 = 无数据）
func (c *CenterDB) NodeHourlySpark(ctx context.Context, hours int) (map[string][]float64, error) {
	rows, err := c.pool.Query(ctx, `SELECT node_id,
			to_timestamp(floor(extract(epoch from logtime) / 3600) * 3600) AS h,
			avg(avgdelay) FILTER (WHERE avgdelay > 0) AS d
			FROM pinglog WHERE logtime > now() - make_interval(hours => $1)
			GROUP BY 1, 2 ORDER BY 1, 2`, hours)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]float64{}
	for rows.Next() {
		var nid string
		var h time.Time
		var d *float64
		if err := rows.Scan(&nid, &h, &d); err != nil {
			return nil, err
		}
		if out[nid] == nil {
			out[nid] = make([]float64, hours)
			for i := range out[nid] {
				out[nid][i] = -1
			}
		}
		// 用"距当前的小时偏移"归位，保证返回的数组右端=当前小时
		off := int(time.Since(h).Hours())
		idx := hours - 1 - off
		if idx < 0 || idx >= hours {
			continue
		}
		if d != nil {
			out[nid][idx] = round2(*d)
		}
	}
	return out, rows.Err()
}

// ---------- 内存版镜像（无 DB 时供测试与单机模式）----------

// MatrixCells 内存版：从 latest 环形缓冲取最新一行 / 均值
func (m *MemStore) MatrixCells(w windowSpec, nodeID string) map[string]map[string]matrixCell {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]map[string]matrixCell{}
	cutoff := time.Now().Add(-w.Dur)
	sum := map[string]map[string][]matrixCell{}
	for _, r := range m.latest {
		if nodeID != "" && r["node_id"] != nodeID {
			continue
		}
		lt, ok := rowTime(r)
		if !ok || lt.Before(cutoff) {
			continue
		}
		tg := r["target"].(string)
		nid := r["node_id"].(string)
		cell := matrixCell{
			Avg: r["avgdelay"].(float64), Send: r["sendpk"].(int),
			Recv: r["revcpk"].(int), Loss: r["losspk"].(int), Log: lt,
		}
		cell.Min, cell.Max = cell.Avg, cell.Avg
		if w.Avg {
			if sum[tg] == nil {
				sum[tg] = map[string][]matrixCell{}
			}
			sum[tg][nid] = append(sum[tg][nid], cell)
			continue
		}
		prev, ok2 := out[tg][nid]
		if !ok2 {
			putCell(out, tg, nid, cell)
			continue
		}
		// 倒序遍历，先到的是更新的一行；同一分钟才比"更优"
		if cell.Log.Equal(prev.Log) && betterMatrixCell(cell, prev) {
			out[tg][nid] = cell
		}
	}
	for tg, byNode := range sum {
		for nid, cells := range byNode {
			var acc matrixCell
			nValid, nSend, nLoss := 0, 0, 0
			for _, c := range cells {
				nSend += c.Send
				nLoss += c.Loss
				if c.Avg > 0 {
					acc.Avg += c.Avg
					nValid++
				}
				if c.Max > acc.Max {
					acc.Max = c.Max
				}
				if c.Log.After(acc.Log) {
					acc.Log = c.Log
				}
			}
			if nValid > 0 {
				acc.Avg = round2(acc.Avg / float64(nValid))
			}
			acc.Send, acc.Loss = nSend, nLoss
			acc.Min = acc.Avg
			putCell(out, tg, nid, acc)
		}
	}
	return out
}

func betterMatrixCell(a, b matrixCell) bool {
	if (a.Avg > 0) != (b.Avg > 0) {
		return a.Avg > 0
	}
	return a.Avg < b.Avg
}

// NodeWindowAgg 内存版
func (m *MemStore) NodeWindowAgg(w windowSpec) map[string]NodeAggStat {
	m.mu.Lock()
	defer m.mu.Unlock()
	type acc struct {
		sumDelay float64
		nValid   int
		send     int
		loss     int
		targets  map[string]struct{}
	}
	tmp := map[string]*acc{}
	cutoff := time.Now().Add(-w.Dur)
	for _, r := range m.latest {
		lt, ok := rowTime(r)
		if !ok || lt.Before(cutoff) {
			continue
		}
		nid := r["node_id"].(string)
		a := tmp[nid]
		if a == nil {
			a = &acc{targets: map[string]struct{}{}}
			tmp[nid] = a
		}
		if d := r["avgdelay"].(float64); d > 0 {
			a.sumDelay += d
			a.nValid++
		}
		a.send += r["sendpk"].(int)
		a.loss += r["losspk"].(int)
		a.targets[r["target"].(string)] = struct{}{}
	}
	out := map[string]NodeAggStat{}
	for nid, a := range tmp {
		st := NodeAggStat{Targets: len(a.targets)}
		if a.nValid > 0 {
			st.Avg = round2(a.sumDelay / float64(a.nValid))
		}
		if a.send >= 1 {
			st.Loss = round2(float64(a.loss) * 100 / float64(a.send))
		}
		out[nid] = st
	}
	return out
}

// NodeHourlySpark 内存版
func (m *MemStore) NodeHourlySpark(hours int) map[string][]float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	type key struct {
		nid string
		idx int
	}
	sum := map[key]float64{}
	cnt := map[key]int{}
	out := map[string][]float64{}
	ensure := func(nid string) {
		if out[nid] == nil {
			out[nid] = make([]float64, hours)
			for i := range out[nid] {
				out[nid][i] = -1
			}
		}
	}
	for _, r := range m.latest {
		lt, ok := rowTime(r)
		if !ok {
			continue
		}
		off := int(time.Since(lt).Hours())
		idx := hours - 1 - off
		if idx < 0 || idx >= hours {
			continue
		}
		d := r["avgdelay"].(float64)
		if d <= 0 {
			continue
		}
		nid := r["node_id"].(string)
		ensure(nid)
		k := key{nid, idx}
		sum[k] += d
		cnt[k]++
	}
	for k, s := range sum {
		out[k.nid][k.idx] = round2(s / float64(cnt[k]))
	}
	return out
}

// ---------- 汇总面板 ----------

// 收集 (行, 节点, 值) 用于跨节点取最快/最慢
type statPoint struct {
	node    string
	value   float64
	where   string
	telecom string
}

func buildGroupStat(key, label string, pts []statPoint, avgSum float64, avgN int, lossSum float64, lossN int, targets int) GroupStat {
	st := GroupStat{Key: key, Label: label, Targets: targets}
	if avgN > 0 {
		st.Avg = round2(avgSum / float64(avgN))
	}
	if lossN > 0 {
		st.Loss = round2(lossSum / float64(lossN))
	}
	best, worst := -1.0, -1.0
	for _, p := range pts {
		if p.value <= 0 || p.value >= 2000 {
			continue // 无数据/超时都不参与"最快最慢"
		}
		if best < 0 || p.value < best {
			best = p.value
			st.Fastest = GroupExtreme{Node: p.node, Value: round2(p.value), Where: p.where, Telecom: p.telecom}
		}
		if p.value > worst {
			worst = p.value
			st.Slowest = GroupExtreme{Node: p.node, Value: round2(p.value), Where: p.where, Telecom: p.telecom}
		}
	}
	return st
}

// ---------- HTTP handler ----------

// sparkCache 24h 逐小时聚合是"较重"查询（1M 行级），用 55s TTL 缓存，
// 避免前端 10s 轮询把它变成常态化全表聚合。内存模式不走缓存。
var sparkCache struct {
	mu   sync.Mutex
	at   time.Time
	data map[string][]float64
}

func (c *Center) nodeCards(ctx context.Context, w windowSpec) ([]NodeCard, error) {
	var infos []NodeInfo
	if c.db != nil {
		list, err := c.db.ListNodes(ctx)
		if err != nil {
			return nil, err
		}
		infos = list
	} else {
		infos = c.mem.ListNodes()
	}
	// 前台只展示"可见"的节点：隐藏的节点照常探测/上报/入库，只是不出现在首页
	infos = visibleNodes(infos)
	var agg map[string]NodeAggStat
	var err error
	if c.db != nil {
		agg, err = c.db.NodeWindowAgg(ctx, w)
	} else {
		agg = c.mem.NodeWindowAgg(w)
	}
	if err != nil {
		return nil, err
	}

	spark := map[string][]float64{}
	if c.db != nil {
		sparkCache.mu.Lock()
		if time.Since(sparkCache.at) < 55*time.Second && sparkCache.data != nil {
			spark = sparkCache.data
		}
		sparkCache.mu.Unlock()
		if spark == nil {
			s, serr := c.db.NodeHourlySpark(ctx, 24)
			if serr == nil {
				spark = s
				sparkCache.mu.Lock()
				sparkCache.at, sparkCache.data = time.Now(), s
				sparkCache.mu.Unlock()
			}
		}
	} else {
		spark = c.mem.NodeHourlySpark(24)
	}

	now := time.Now()
	// 显示名优先用数据库里存的（新增节点时按"地区+运营商"自动写入）
	dbLabels := map[string]string{}
	for _, n := range infos {
		if strings.TrimSpace(n.Label) != "" {
			dbLabels[n.NodeID] = n.Label
		}
	}
	cards := make([]NodeCard, 0, len(infos))
	for _, n := range infos {
		card := NodeCard{
			ID:      n.NodeID,
			Label:   c.nodeLabel(dbLabels, n.NodeID),
			Online:  now.Sub(n.LastSeen) < 3*time.Minute,
			Self:    c.conf.SelfNode != "" && n.NodeID == c.conf.SelfNode,
			Version: n.Version,
			Targets: n.Targets,
		}
		if !n.LastSeen.IsZero() {
			card.Last = n.LastSeen.Format("15:04:05")
		}
		if a, ok := agg[n.NodeID]; ok {
			card.Avg, card.Loss = a.Avg, a.Loss
			if a.Targets > 0 {
				card.Targets = a.Targets
			}
		}
		if s, ok := spark[n.NodeID]; ok {
			card.Spark = s
		} else {
			card.Spark = make([]float64, 24)
			for i := range card.Spark {
				card.Spark[i] = -1
			}
		}
		cards = append(cards, card)
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].ID < cards[j].ID })
	return cards, nil
}

// nodeLabel 节点显示名，优先级：
//  1. 数据库里的 label —— 新增节点时按"地区+运营商"自动拼好（推荐路径，改完即时生效）；
//  2. center.json 的 NodeGeo.Label —— 兼容老部署（手动配置的时代）；
//  3. 节点 id —— 兜底。
//
// dbLabels 为 nil 时自动跳过第 1 步（某些调用点没有数据库上下文）。
func (c *Center) nodeLabel(dbLabels map[string]string, id string) string {
	if dbLabels != nil {
		if l := strings.TrimSpace(dbLabels[id]); l != "" {
			return l
		}
	}
	if g, ok := c.conf.NodeGeo[id]; ok && strings.TrimSpace(g.Label) != "" {
		return g.Label
	}
	return id
}

// nodeLabelMap 取数据库里的"节点 id → 显示名"；DB 不可用或出错时返回 nil（调用方自动降级）
func (c *Center) nodeLabelMap(ctx context.Context) map[string]string {
	if c.db != nil {
		m, err := c.db.NodeLabels(ctx)
		if err == nil {
			return m
		}
		return nil
	}
	return c.mem.NodeLabels()
}

// （nodeGeo 已移除：地图不再标注探测节点位置，坐标配置随之作废）

// handleMatrix GET /api/matrix.json
func (c *Center) handleMatrix(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	node, known, nerr := c.snapshotNode(ctx, r)
	if nerr != nil {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "err": nerr.Error()})
		return
	}
	ws := parseWindow(r.URL.Query().Get("window"))

	var targets []TargetRow
	var cells map[string]map[string]matrixCell
	var err error
	if c.db != nil {
		if targets, err = c.db.ListTargets(ctx); err != nil {
			writeJSON(w, 500, map[string]interface{}{"ok": false, "err": err.Error()})
			return
		}
		if cells, err = c.db.MatrixCells(ctx, ws, node); err != nil {
			writeJSON(w, 500, map[string]interface{}{"ok": false, "err": err.Error()})
			return
		}
	} else {
		targets = c.mem.ListTargets()
		cells = c.mem.MatrixCells(ws, node)
	}

	// ---- 组装行（保留全量用于面板，过滤后的用于 rows）----
	all := make([]MatrixRow, 0, len(targets))
	provAgg := map[string]float64{}
	var newest time.Time
	for _, t := range targets {
		row := MatrixRow{
			Alias: t.Alias, IP: t.IP, Province: t.Province, City: t.City, Telecom: t.Telecom,
			Cells: map[string]cellJSON{}, Best: -1, Worst: -1, Spread: -1,
		}
		byNode := cells[t.Alias]
		for nid, cell := range byNode {
			delay := cell.Avg
			loss := -1.0
			if cell.Send >= 1 {
				loss = round2(float64(cell.Loss) * 100 / float64(cell.Send))
			}
			if delay > 0 && delay < 2000 { // 只让有效值参与最优/最差
				if row.Best < 0 || delay < row.Best {
					row.Best, row.BestNode = round2(delay), nid
				}
				if delay > row.Worst {
					row.Worst, row.WorstNode = round2(delay), nid
				}
			}
			row.Cells[nid] = cellJSON{
				Delay: round2(delay), Loss: loss,
				Min: round2(cell.Min), Max: round2(cell.Max),
				Send: cell.Send, Recv: cell.Recv,
				Logtime: cell.Log.Format("15:04"),
			}
			if cell.Log.After(newest) {
				newest = cell.Log
			}
		}
		// 期望上报的节点数：单节点口径 1 台 / 全网口径全部节点
		expect := len(known)
		if node != "" {
			expect = 1
		}
		if row.Missing = expect - len(row.Cells); row.Missing < 0 {
			row.Missing = 0
		}
		// spread 只在"所有期望节点都给出了有效值"时才有意义：
		// 否则 6.6ms 与"超时"混在一起算差值是假的（会被当成两台一样快）。
		valid := 0
		for _, c := range row.Cells {
			if c.Delay > 0 && c.Delay < 2000 {
				valid++
			}
		}
		if expect >= 2 && valid == expect {
			row.Spread = round2(row.Worst - row.Best)
		}
		row.Grade = gradeOf(row.Best, row.cellLoss(row.BestNode))
		// 展示值 = 全网最优口径下的 Best；单节点口径下就是该节点的值
		shown := row.Best
		if node != "" {
			shown = row.Cells[node].Delay
			row.Grade = gradeOf(shown, row.Cells[node].Loss)
		}
		if shown > 0 && shown < 2000 {
			if cur, ok := provAgg[t.Province]; !ok || shown < cur {
				provAgg[t.Province] = shown
			}
		}
		all = append(all, row)
	}

	// ---- 面板：②运营商 / ③地区（基于全量，不受表格过滤影响）----
	telPts := map[string][]statPoint{}
	telSum := map[string]float64{}
	telN := map[string]int{}
	telLoss := map[string]float64{}
	telLossN := map[string]int{}
	telTargets := map[string]int{}
	regPts := map[string][]statPoint{}
	regSum := map[string]float64{}
	regN := map[string]int{}
	regLoss := map[string]float64{}
	regLossN := map[string]int{}
	regTargets := map[string]int{}

	for _, row := range all {
		shown := row.Best
		shownLoss := row.cellLoss(row.BestNode)
		if node != "" {
			shown, shownLoss = row.Cells[node].Delay, row.Cells[node].Loss
		}
		where := row.Province + row.City
		if row.Province == row.City { // 直辖市别拼成"北京北京"
			where = row.Province
		}
		// 每个节点各贡献一个点（"最快/最慢"是跨节点的）
		for nid, c := range row.Cells {
			if c.Delay <= 0 || c.Delay >= 2000 {
				continue
			}
			p := statPoint{node: nid, value: c.Delay, where: where, telecom: row.Telecom}
			telPts[row.Telecom] = append(telPts[row.Telecom], p)
			if r := provinceRegion[row.Province]; r != "" {
				regPts[r] = append(regPts[r], p)
			}
		}
		if shown > 0 && shown < 2000 {
			telSum[row.Telecom] += shown
			telN[row.Telecom]++
			reg := provinceRegion[row.Province]
			regSum[reg] += shown
			regN[reg]++
		}
		if shownLoss >= 0 {
			telLoss[row.Telecom] += shownLoss
			telLossN[row.Telecom]++
			reg := provinceRegion[row.Province]
			regLoss[reg] += shownLoss
			regLossN[reg]++
		}
		telTargets[row.Telecom]++
		regTargets[provinceRegion[row.Province]]++
	}

	byTel := make([]GroupStat, 0, 3)
	for _, tel := range telOrder {
		byTel = append(byTel, buildGroupStat(tel, TEL_LABELS[tel], telPts[tel],
			telSum[tel], telN[tel], telLoss[tel], telLossN[tel], telTargets[tel]))
	}
	byReg := make([]GroupStat, 0, len(regionOrder))
	for _, reg := range regionOrder {
		if regTargets[reg] == 0 {
			continue
		}
		byReg = append(byReg, buildGroupStat(reg, reg, regPts[reg],
			regSum[reg], regN[reg], regLoss[reg], regLossN[reg], regTargets[reg]))
	}
	// 汇总里的节点显示名：buildGroupStat 只拿得到 node id，这里补上给外部消费者看的名字
	dbLabels := c.nodeLabelMap(ctx)
	fillLabels := func(gs []GroupStat) {
		for i := range gs {
			gs[i].Fastest.Label = c.nodeLabel(dbLabels, gs[i].Fastest.Node)
			gs[i].Slowest.Label = c.nodeLabel(dbLabels, gs[i].Slowest.Node)
		}
	}
	fillLabels(byTel)
	fillLabels(byReg)

	// ---- 表格过滤（只影响 rows）----
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	provF := strings.TrimSpace(r.URL.Query().Get("province"))
	telF := strings.TrimSpace(r.URL.Query().Get("telecom"))
	only := strings.TrimSpace(r.URL.Query().Get("only"))
	filtered := make([]MatrixRow, 0, len(all))
	for _, row := range all {
		if provF != "" && row.Province != provF {
			continue
		}
		if telF != "" && row.Telecom != telF {
			continue
		}
		if q != "" && !strings.Contains(row.Alias+row.IP+row.City+row.Province+row.Telecom, q) {
			continue
		}
		if only == "diff" && row.Spread <= 0 {
			continue
		}
		if only == "timeout" {
			// "任一节点异常"：某节点超时、或某节点本窗口根本没上报（节点掉线/未覆盖）
			if row.Missing == 0 {
				bad := false
				for _, c := range row.Cells {
					if c.Delay <= 0 || c.Delay >= 2000 {
						bad = true
						break
					}
				}
				if !bad {
					continue
				}
			}
		}
		filtered = append(filtered, row)
	}

	// ---- 排序（默认按节点间差异降序：多节点最有价值的信号排最上面）----
	sortKey := strings.TrimSpace(r.URL.Query().Get("sort"))
	order := strings.TrimSpace(r.URL.Query().Get("order"))
	if sortKey == "" {
		// 默认：全网口径按"节点间差异"降序（多节点最有价值的信号排最上面）；
		// 单节点口径没有可比性，改按延迟升序。
		if node != "" {
			sortKey, order = "delay", "asc"
		} else {
			sortKey, order = "spread", "desc"
		}
	}
	less := func(a, b MatrixRow) bool {
		switch sortKey {
		case "loss":
			la, lb := a.cellLoss(a.displayNode(node)), b.cellLoss(b.displayNode(node))
			return la > lb
		case "province":
			if a.Province != b.Province {
				return a.Province < b.Province
			}
			return a.City < b.City
		case "spread":
			return a.Spread > b.Spread
		default: // delay
			da, db := a.displayDelay(node), b.displayDelay(node)
			if da < 0 {
				da = 1e9
			}
			if db < 0 {
				db = 1e9
			}
			return da < db
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if order == "asc" && sortKey != "province" {
			return less(filtered[j], filtered[i])
		}
		return less(filtered[i], filtered[j])
	})

	total := len(filtered)
	limit := atoiDefault(r.URL.Query().Get("limit"), 0)
	offset := atoiDefault(r.URL.Query().Get("offset"), 0)
	if offset > 0 && offset < len(filtered) {
		filtered = filtered[offset:]
	} else if offset >= len(filtered) {
		filtered = nil
	}
	if limit > 0 && limit < len(filtered) {
		filtered = filtered[:limit]
	}

	cards, err := c.nodeCards(ctx, ws)
	if err != nil {
		writeJSON(w, 500, map[string]interface{}{"ok": false, "err": err.Error()})
		return
	}

	asOf := newest
	if asOf.IsZero() {
		asOf = time.Now()
	}
	scope := "best"
	if node != "" {
		scope = "node"
	}
	writeJSON(w, 200, MatrixResp{
		AsOf: asOf.Format("2006-01-02 15:04"), Scope: scope, Node: node, Window: ws.Key,
		Nodes: cards, Rows: filtered, Total: total,
		ByTelecom: byTel, ByRegion: byReg, ProvinceAgg: provAgg,
	})
}

func (r MatrixRow) displayNode(scope string) string {
	if scope != "" {
		return scope
	}
	return r.BestNode
}

func (r MatrixRow) displayDelay(scope string) float64 {
	if scope != "" {
		return r.Cells[scope].Delay
	}
	return r.Best
}

func (r MatrixRow) cellLoss(node string) float64 {
	if c, ok := r.Cells[node]; ok {
		return c.Loss
	}
	return -1
}

// TEL_LABELS 与前端 TEL_NAMES 保持一致
var TEL_LABELS = map[string]string{"ctcc": "电信", "cucc": "联通", "cmcc": "移动"}

// 供 main 注册路由时引用，避免拼错
const matrixPath = "GET /api/matrix.json"
