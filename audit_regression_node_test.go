//go:build node

package main

// 第二轮审计修复的节点端回归测试。
//
// 重点：P0（时钟回拨自愈误触发）原先的"回归测试"是假绿 ——
// 它在 InsertRound(t2) **之后**才调用检测函数，那一刻 MaxLogtime()=t2 > 水位，
// 旧判据（MaxLogtime() <= 水位）本来就是假，把旧判据换回去测试照样通过。
// 这里补上真正的现场：水位与缓冲最大 logtime 相等时调用检测，必须不动水位。

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// P0/P2：水位 == 缓冲最大 logtime（稳态）时不得判成时钟回拨。
// 同分钟重启（进程在第 N 分钟重启，启动后立刻跑的那一轮 logtime 仍等于水位）
// 是这条判据最容易踩的边界：旧实现用 `roundLogtime > wm`，等号会被当成回拨。
func TestSameMinuteRoundIsNotClockRewind(t *testing.T) {
	s, done := newTestStore(t)
	defer done()

	base := time.Now().Unix()/60*60 - 600
	if err := s.InsertRound([]RoundResult{mkResult(base, "a")}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.SetWatermark(base); err != nil {
		t.Fatalf("set wm: %v", err)
	}
	// 现场就位：缓冲最大 logtime == 水位（B7 只删 24 小时前的行，所以必然如此）
	if maxLT, ok := s.MaxLogtime(); !ok || maxLT != base {
		t.Fatalf("测试前提不成立：MaxLogtime=%d(ok=%v) 水位=%d", maxLT, ok, base)
	}

	n := &Node{store: s}
	n.checkClockRewind(base) // 同分钟重启后的第一轮：没有回拨

	if got := s.GetWatermark(); got != base {
		t.Fatalf("logtime==水位 被判成回拨，水位被回退到 %d（期望 %d）", got, base)
	}
	// 真正的回拨（严格早于水位）仍然必须被纠正
	n.checkClockRewind(base - 300)
	if got := s.GetWatermark(); got >= base {
		t.Fatalf("真回拨未被纠正：水位 %d 应小于 %d", got, base)
	}
	if got := s.GetWatermark(); got > base-300 {
		t.Fatalf("回退量应按实际幅度算：期望 ≤ %d，实际 %d", base-300, got)
	}
}

// P2：水位只进不退。
// HTTP 上报循环与 WS 会话可能各持一批数据，较旧的那批后回来时会把水位写回较小值
// （重复上报 + 清理延迟）。主动回退只允许走 RewindWatermark（时钟回拨自愈）。
func TestWatermarkNeverGoesBackwards(t *testing.T) {
	s, done := newTestStore(t)
	defer done()

	base := time.Now().Unix()/60*60 - 600
	if err := s.SetWatermark(base + 120); err != nil {
		t.Fatalf("set wm: %v", err)
	}
	if err := s.SetWatermark(base); err != nil {
		t.Fatalf("set wm2: %v", err)
	}
	if got := s.GetWatermark(); got != base+120 {
		t.Fatalf("SetWatermark 不得回退水位：期望 %d，实际 %d", base+120, got)
	}
	if err := s.RewindWatermark(60); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	if got := s.GetWatermark(); got != base+60 {
		t.Fatalf("RewindWatermark 应能主动回退：期望 %d，实际 %d", base+60, got)
	}
}

// P2：管理员清空目标表后，节点回写的是 `"targets": []`。
// 判定必须看"targets 键是否存在"，而不是"列表是否非空"，
// 否则重启会回退到 config.json 里遗留的旧 Chinamap，继续探测已删目标。
func TestPersistedEmptyTargetsBeatsLegacyChinamap(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")

	withEmpty := `{"Name":"n1","Chinamap":[{"Name":"old","Ips":["9.9.9.9"]}],"targets":[],"targets_ver":7}`
	if err := os.WriteFile(cfg, []byte(withEmpty), 0o600); err != nil {
		t.Fatal(err)
	}
	c := loadNodeConf(cfg)
	if len(c.Targets) != 0 {
		t.Fatalf("持久化的空目标表必须优先于 legacy Chinamap，实际 %+v", c.Targets)
	}
	if c.TargetsVer != 7 {
		t.Fatalf("targets_ver 应一并保留，实际 %d", c.TargetsVer)
	}

	// 没有 targets 键（老配置）时才回退 Chinamap
	legacy := `{"Name":"n1","Chinamap":[{"Name":"old","Ips":["9.9.9.9"]}]}`
	if err := os.WriteFile(cfg, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	c2 := loadNodeConf(cfg)
	if len(c2.Targets) != 1 || c2.Targets[0].IP != "9.9.9.9" {
		t.Fatalf("无 targets 键时应回退 Chinamap，实际 %+v", c2.Targets)
	}

	// 非空持久化列表优先于 Chinamap
	both := `{"Name":"n1","Chinamap":[{"Name":"old","Ips":["9.9.9.9"]}],"targets":[{"alias":"a","ip":"1.1.1.1"}],"targets_ver":9}`
	if err := os.WriteFile(cfg, []byte(both), 0o600); err != nil {
		t.Fatal(err)
	}
	c3 := loadNodeConf(cfg)
	if len(c3.Targets) != 1 || c3.Targets[0].IP != "1.1.1.1" || c3.TargetsVer != 9 {
		t.Fatalf("持久化列表应优先：%+v v%d", c3.Targets, c3.TargetsVer)
	}
}
