//go:build node

package main

// 节点端存储层回归测试：
//   A2  时钟回拨自愈：判据是「本轮探测 logtime ≤ 水位」，回退量按实际回拨幅度算
//   P0  稳态回归：水位 == 缓冲最大 logtime（B7 的 24h 保留策略必然如此）时
//       **绝不能**判成时钟回拨 —— 旧实现正是在这里恒误触发，导致每 30s 回退
//       2 小时、反复重传 4.3 万行，磁盘数周写满。
//   B7  清理只删水位之前的行；未确认行一律保留（中心宕机 24h 也不丢数据）

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*NodeStore, func()) {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenNodeStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s, func() { s.Close() }
}

func mkResult(lt int64, target string) RoundResult {
	return RoundResult{Logtime: lt, Target: target, AvgDelay: 10, SendPk: 20, RecvPk: 20}
}

// P0 回归：稳态下不得触发时钟回拨自愈。
//
// 复现旧 bug 的确切现场（B7 与 A2 互相冲突导致）：
//   1. 一轮探测写入 T1，上报成功 → 水位 = T1
//   2. B7 的清理策略只删「水位 - 24h 之前」的行，所以 logtime==T1 的**已确认行
//      仍然留在缓冲里** ⇒ MaxLogtime() == 水位
//   3. 下一轮探测写入 T2 = T1 + 60s
//   4. 旧判据「缓冲最大 logtime ≤ 水位」在第 3 步之后依然成立 → 误判回拨
//
// 正确判据只看**本轮 logtime**（T2 > 水位 ⇒ 正常），水位与缓冲内容无关。
func TestNoRewindInSteadyState(t *testing.T) {
	s, done := newTestStore(t)
	defer done()

	base := time.Now().Unix()/60*60 - 600
	t1 := base
	// 第一轮：写入 + 上报成功 → 水位推到 T1
	if err := s.InsertRound([]RoundResult{mkResult(t1, "a"), mkResult(t1, "b")}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.SetWatermark(t1); err != nil {
		t.Fatalf("set wm: %v", err)
	}
	// 关键前提：缓冲里确实还留着 logtime==水位 的已确认行（这正是 B7 的行为）
	if maxLT, ok := s.MaxLogtime(); !ok || maxLT != t1 {
		t.Fatalf("测试前提不成立：缓冲最大 logtime=%d(ok=%v) 应等于水位 %d", maxLT, ok, t1)
	}

	// ① 先在**旧 bug 的确切现场**调用检测：此刻 MaxLogtime()==水位，
	//    旧判据（缓冲最大 logtime ≤ 水位）在这里必然成立 —— 这一步才是真正的 P0 锁。
	//    （旧版本测试先插入 t2 再检测，那一刻 MaxLogtime=t2>水位，旧判据本来就是假，
	//      所以把旧判据换回去测试照样通过；变异测试实测 MISSED。）
	n := &Node{store: s}
	n.checkClockRewind(t1)
	if got := s.GetWatermark(); got != t1 {
		t.Fatalf("稳态（MaxLogtime==水位）被判成时钟回拨：水位 %d 被改成 %d", t1, got)
	}

	// ② 然后再验证正常推进一分钟的情形
	t2 := t1 + 60
	if err := s.InsertRound([]RoundResult{mkResult(t2, "a"), mkResult(t2, "b")}); err != nil {
		t.Fatalf("insert2: %v", err)
	}
	n.checkClockRewind(t2)

	if got := s.GetWatermark(); got != t1 {
		t.Fatalf("稳态下水位被改动了：期望保持 %d，实际 %d（P0：误判为时钟回拨）", t1, got)
	}
	// 且本轮数据仍应可正常上报
	if batch, _ := s.LoadBatch(s.GetWatermark(), 100); len(batch) != 2 {
		t.Fatalf("稳态下应能取到本轮 2 行待上报，实际 %d 行", len(batch))
	}
}

// A2：真回拨必须被识别并回退，且回退量按实际幅度算（不是固定 2 小时）。
func TestWatermarkRewindOnClockJump(t *testing.T) {
	s, done := newTestStore(t)
	defer done()

	// 水位在 T1（模拟之前已成功上报到 T1）
	t1 := time.Now().Unix()/60*60 - 3600
	if err := s.InsertRound([]RoundResult{mkResult(t1, "a")}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.SetWatermark(t1); err != nil {
		t.Fatalf("set wm: %v", err)
	}

	// 时钟被回拨 10 分钟：新一轮数据的 logtime 比水位早 600s
	t2 := t1 - 600
	if err := s.InsertRound([]RoundResult{mkResult(t2, "a")}); err != nil {
		t.Fatalf("insert2: %v", err)
	}
	// 取不到批次 —— 这正是数据静默堆在本地、永不上报的现场
	if batch, _ := s.LoadBatch(s.GetWatermark(), 100); len(batch) != 0 {
		t.Fatalf("回拨后本应取不到批次（数据卡在水位之前），实际取到 %d 行", len(batch))
	}

	n := &Node{store: s}
	n.checkClockRewind(t2)

	wm := s.GetWatermark()
	if wm >= t1 {
		t.Fatalf("回退后水位应小于原水位 %d，实际 %d", t1, wm)
	}
	// 回退量 = (水位 - 本轮logtime) + 60 = 600 + 60 = 660
	if want := t1 - 660; wm != want {
		t.Fatalf("回退量应按实际回拨幅度计算：期望水位 %d，实际 %d", want, wm)
	}
	// 回退后必须能重新取到这批卡住的数据
	if batch, _ := s.LoadBatch(wm, 100); len(batch) == 0 {
		t.Fatalf("回退水位后应能重新取到待上报数据")
	}
}

// P0 边界：回拨量必须有上限，否则时钟被设到远古时水位被拖到 0，导致全量重传。
func TestRewindCapped(t *testing.T) {
	s, done := newTestStore(t)
	defer done()

	t1 := time.Now().Unix()/60*60
	if err := s.InsertRound([]RoundResult{mkResult(t1, "a")}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.SetWatermark(t1); err != nil {
		t.Fatalf("set wm: %v", err)
	}
	// 时钟被设回 1970 年
	n := &Node{store: s}
	n.checkClockRewind(60)

	if wm := s.GetWatermark(); wm < t1-maxClockRewind {
		t.Fatalf("回退量应被 maxClockRewind(%ds) 封顶：水位 %d 低于下限 %d",
			int(maxClockRewind), wm, t1-maxClockRewind)
	}
}

func TestCleanupOnlyConfirmed(t *testing.T) {
	s, done := newTestStore(t)
	defer done()

	base := time.Now().Unix()/60*60 - 7200
	// 三批：T-48h / T-24h / T-1h，水位只推进到 T-24h（= 已确认到 T-24h）
	old1, old2, fresh := base, base+86400, base+86400+3600
	for _, lt := range []int64{old1, old2, fresh} {
		if err := s.InsertRound([]RoundResult{mkResult(lt, "t")}); err != nil {
			t.Fatalf("insert %d: %v", lt, err)
		}
	}
	if err := s.SetWatermark(old2); err != nil {
		t.Fatalf("set wm: %v", err)
	}

	// 保留窗口 1h：水位之前 1h 以上才删 ⇒ old1 应删，old2/fresh 保留
	del, pending, err := s.CleanupConfirmed(3600)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if del != 1 {
		t.Fatalf("应只删 1 行（old1），实际 %d", del)
	}
	if pending != 1 {
		t.Fatalf("未确认积压应为 1 行（fresh），实际 %d", pending)
	}
	rows, _ := s.LoadBatch(0, 100)
	for _, r := range rows {
		if r.Logtime == old1 {
			t.Fatalf("old1 未被删除，清理条件有误")
		}
	}
	// 关键：fresh（未确认）绝不能被删 —— 中心宕机再久也不丢数据
	if cnt := s.Count(); cnt != 2 {
		t.Fatalf("删除后应剩 2 行（old2 + fresh），实际 %d", cnt)
	}
}

func TestLoadBatchIdempotentKey(t *testing.T) {
	s, done := newTestStore(t)
	defer done()

	base := time.Now().Unix()/60*60 - 600
	// 同一 (target, logtime) 重跑两轮 → OR REPLACE 只留一行
	if err := s.InsertRound([]RoundResult{mkResult(base, "x")}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.InsertRound([]RoundResult{mkResult(base, "x")}); err != nil {
		t.Fatalf("reinsert: %v", err)
	}
	if n := s.Count(); n != 1 {
		t.Fatalf("重跑同分钟同目标应覆盖为 1 行，实际 %d", n)
	}
}

func TestConfigRewriteKeeps0600(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	os.WriteFile(cfg, []byte(`{"Name":"n1","Center":{"Endpoint":"https://x","Token":"secret"}}`), 0o600)

	if err := saveNodeTargets(cfg, []Target{{Alias: "a", IP: "1.1.1.1"}}, 9); err != nil {
		t.Fatalf("save: %v", err)
	}
	st, err := os.Stat(cfg)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// 测试名字里的 0600 必须真的被断言：旧版本只查了内容（`_ = st`），
	// 把 0o600 改成 0o644 测试照样通过（变异测试实测 MISSED）。
	// Windows 的权限模型不同（Go 只映射只读位），所以只在类 Unix 平台断言 ——
	// 也就是说这条锁在 Windows 开发机上是**空转**的，必须靠 Linux CI 才真正生效。
	if runtime.GOOS != "windows" {
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Fatalf("回写后权限应为 0600，实际 %o", perm)
		}
	}
	b, _ := os.ReadFile(cfg)
	if !contains(string(b), "targets_ver") || !contains(string(b), "1.1.1.1") {
		t.Fatalf("回写内容不对: %s", string(b))
	}
	// 其余键必须原样保留（Token 不能被回写吃掉）
	if !contains(string(b), "secret") || !contains(string(b), "Endpoint") {
		t.Fatalf("回写不应丢失其它配置键: %s", string(b))
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
