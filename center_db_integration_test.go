//go:build center && integration

package main

// 真实 PostgreSQL 的集成测试。**默认不参与编译**，需要显式开启并指向一个**测试库**：
//
//	PINGATLAS_TEST_DSN='postgres://user:pw@127.0.0.1:5432/pingatlas_test' \
//	  go test -tags "center integration" -count=1 -run TestIntegration ./...
//
// 警告：OpenCenterDB 会执行建表/建索引（ensureSchema），**绝不要指向生产库**。
//
// 存在意义：中心端最关键的 SQL（快照/历史/节点对比/省级曲线/存储自查）过去只能靠人工
// 上服务器敲命令验证；这些查询已在线上 PG 16.15 + TimescaleDB 2.30.1 用只读会话验证过，
// 这里把它们固化成可重复执行的用例。

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *CenterDB {
	t.Helper()
	dsn := os.Getenv("PINGATLAS_TEST_DSN")
	if dsn == "" {
		t.Skip("PINGATLAS_TEST_DSN 未设置，跳过 PG 集成测试")
	}
	if v := os.Getenv("SP2_TEST_ALLOW_WRITE"); v != "yes" {
		t.Skip("为避免误伤生产库：需要 SP2_TEST_ALLOW_WRITE=yes 才运行（ensureSchema 会建表建索引）")
	}
	db, err := OpenCenterDB(dsn)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// 只读类查询：语法与聚合语义必须在真实 PG 上跑得通。
func TestIntegrationReadQueries(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := db.LatestSnapshot(ctx, 5*time.Minute, ""); err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if _, err := db.QueryHistory(ctx, "", "", 5); err != nil {
		t.Fatalf("QueryHistory: %v", err)
	}
	// 口径过滤的 DB 分支：未知节点应返回空集而不是报错（SQL 参数绑定与语法）
	if s, err := db.LatestSnapshot(ctx, 5*time.Minute, "no-such-node"); err != nil {
		t.Fatalf("LatestSnapshot(node): %v", err)
	} else if len(s) != 0 {
		t.Fatalf("未知节点应无数据，实际 %d 条", len(s))
	}
	if _, err := db.ProvinceHistory(ctx, "不存在的省", 24, "no-such-node"); err != nil {
		t.Fatalf("ProvinceHistory(node): %v", err)
	}
	targets, err := db.ListTargets(ctx)
	if err != nil {
		t.Fatalf("ListTargets: %v", err)
	}
	if len(targets) == 0 {
		t.Log("测试库没有目标，跳过依赖目标的查询")
	} else {
		if _, err := db.TargetIPHistory(ctx, targets[0].Alias, 24); err != nil {
			t.Fatalf("TargetIPHistory: %v", err)
		}
		if _, err := db.NodeCompare(ctx, targets[0].Alias, 24); err != nil {
			t.Fatalf("NodeCompare: %v", err)
		}
		if p := targets[0].Province; p != "" {
			ips, err := db.ProvinceHistory(ctx, p, 24, "")
			if err != nil {
				t.Fatalf("ProvinceHistory: %v", err)
			}
			// 历史数组里允许 null（整桶无有效样本），但类型必须是 []*float64
			for _, ip := range ips {
				if len(ip.History) != len(ip.Times) {
					t.Fatalf("history/times 长度不一致：%d vs %d", len(ip.History), len(ip.Times))
				}
			}
		}
	}
	db.LogStorageReport(ctx) // 体积口径必须是 hypertable_size（不能报错）
}

// 写入类：幂等语义 + 唯一索引 + 节点删除，全部用带前缀的临时数据并在结束时清理。
func TestIntegrationWritePaths(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	alias := "sp2-itest-target"
	ip := "203.0.113.77"
	nodeID := "sp2-itest-node"
	lt := time.Now().Truncate(time.Minute)

	t.Cleanup(func() {
		_, _ = db.pool.Exec(context.Background(), `DELETE FROM pinglog WHERE target=$1`, alias)
		_, _ = db.pool.Exec(context.Background(), `DELETE FROM target_alias WHERE alias=$1 OR current_ip=$2`, alias, ip)
		_, _ = db.pool.Exec(context.Background(), `DELETE FROM node WHERE node_id=$1`, nodeID)
	})

	// 目标 upsert（含唯一索引路径）
	if err := db.UpsertTarget(ctx, TargetRow{Alias: alias, IP: ip, Province: "测试", City: "测试", Telecom: "ctcc"}); err != nil {
		t.Fatalf("UpsertTarget: %v", err)
	}
	// 同 IP 换 alias 必须被唯一索引拦下并翻译成 ErrIPInUse
	if err := db.UpsertTarget(ctx, TargetRow{Alias: alias + "-dup", IP: ip}); err == nil {
		t.Fatalf("同 IP 的另一 alias 必须被拒绝")
	} else if !errors.Is(err, ErrIPInUse) {
		t.Fatalf("应返回 ErrIPInUse，实际 %v", err)
	}

	// 落库幂等：同一 (target,node_id,logtime) 插两次只算一行
	row := PingRow{Logtime: lt, Target: alias, NodeID: nodeID, AvgDelay: 12.5, SendPk: 20, RecvPk: 20}
	n1, err := db.InsertRows(ctx, []PingRow{row})
	if err != nil {
		t.Fatalf("InsertRows: %v", err)
	}
	n2, err := db.InsertRows(ctx, []PingRow{row})
	if err != nil {
		t.Fatalf("InsertRows(dup): %v", err)
	}
	if n1+n2 != 1 {
		t.Fatalf("ON CONFLICT 幂等失效：第一次 %d 行、重传 %d 行", n1, n2)
	}

	// 快照必须能看到这行，且延迟正确
	snaps, err := db.LatestSnapshot(ctx, 10*time.Minute, "")
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if s, ok := snaps[alias]; !ok || s.Avg <= 0 {
		t.Fatalf("快照应包含刚写入的目标：%+v", snaps[alias])
	}

	// 节点表：写入 → 在线判定回落 → 删除
	if err := db.UpsertNode(ctx, NodeInfo{NodeID: nodeID, IP: "127.0.0.1", Version: "itest",
		Targets: 1, LastSeen: time.Now()}); err != nil {
		t.Fatalf("UpsertNode: %v", err)
	}
	c := &Center{db: db, mem: NewMemStore(), lim: newFailLimiter(),
		lastSeen: map[string]time.Time{}, cmds: map[string]*cmdState{}}
	if !c.isOnline(nodeID) {
		t.Fatalf("内存无记录时应回落到 DB 的 last_seen 判定在线（isOnline 的 DB 分支）")
	}
	if n, err := db.DeleteNode(ctx, nodeID); err != nil || n != 1 {
		t.Fatalf("DeleteNode: n=%d err=%v", n, err)
	}
}
