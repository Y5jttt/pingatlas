//go:build node

package main

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// NodeStore 节点端 SQLite 缓冲：先落本地、后上报，水位推进保证不丢不重。
// pending 主键 (target, logtime)：同一分钟同一目标重跑会覆盖（OR REPLACE），
// 上报用 (target, logtime) 作为幂等键，中心端 ON CONFLICT DO NOTHING 双保险。
type NodeStore struct {
	db *sql.DB
}

// OpenNodeStore 打开（必要时创建）SQLite 缓冲库，启用 WAL。
func OpenNodeStore(path string) (*NodeStore, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=auto_vacuum(INCREMENTAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite 单写者，串行最稳
	s := &NodeStore{db: db}
	if err := s.ensureSchema(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *NodeStore) ensureSchema() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS pending (
			logtime  INTEGER NOT NULL,
			target   TEXT NOT NULL,
			maxdelay REAL, mindelay REAL, avgdelay REAL,
			sendpk   INTEGER, revcpk INTEGER, losspk INTEGER,
			PRIMARY KEY (target, logtime)
		)`,
		`CREATE TABLE IF NOT EXISTS kv (
			k TEXT PRIMARY KEY,
			v TEXT
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// InsertRound 一轮结果批量写入（单事务）
func (s *NodeStore) InsertRound(rs []RoundResult) error {
	if len(rs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.Prepare(`INSERT OR REPLACE INTO pending
		(logtime, target, maxdelay, mindelay, avgdelay, sendpk, revcpk, losspk)
		VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, r := range rs {
		if _, err := st.Exec(r.Logtime, r.Target, r.MaxDelay, r.MinDelay, r.AvgDelay,
			r.SendPk, r.RecvPk, r.LossPk); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadBatch 取水位之后的待上报数据（按 logtime 升序，limit 截断）
func (s *NodeStore) LoadBatch(watermark int64, limit int) ([]RoundResult, error) {
	rows, err := s.db.Query(`SELECT logtime, target, maxdelay, mindelay, avgdelay, sendpk, revcpk, losspk
		FROM pending WHERE logtime > ? ORDER BY logtime ASC LIMIT ?`, watermark, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoundResult
	for rows.Next() {
		var r RoundResult
		if err := rows.Scan(&r.Logtime, &r.Target, &r.MaxDelay, &r.MinDelay, &r.AvgDelay,
			&r.SendPk, &r.RecvPk, &r.LossPk); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetWatermark 已成功上报到的最大 logtime（unix 秒），无则 0
func (s *NodeStore) GetWatermark() int64 {
	var v sql.NullString
	_ = s.db.QueryRow(`SELECT v FROM kv WHERE k='watermark'`).Scan(&v)
	if !v.Valid {
		return 0
	}
	var n int64
	fmt.Sscanf(v.String, "%d", &n)
	return n
}

// SetWatermark 推进水位（仅在上报成功后调用）。**只进不退**：
// HTTP 上报循环与 WS 会话可能各持一批数据，较旧的那批后回来时会把水位写回较小值
// （重复上报 + 清理延迟）。真正的回退只允许走 RewindWatermark（时钟回拨自愈）。
func (s *NodeStore) SetWatermark(v int64) error {
	if cur := s.GetWatermark(); v <= cur {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO kv (k, v) VALUES ('watermark', ?)
		ON CONFLICT (k) DO UPDATE SET v=excluded.v`, fmt.Sprintf("%d", v))
	return err
}

// CleanupConfirmed 只删除「已确认上报且早于 keepSec 秒」的缓冲行。
// 水位之前的行按定义都已落库中心（ack 推进水位是唯一凭据），删它们绝对安全；
// 水位之后的行一律保留 —— 即使中心长时间不可达也不丢数据（B7）。
// 返回 (删除行数, 未确认积压行数, 错误)
func (s *NodeStore) CleanupConfirmed(keepSec int64) (int64, int64, error) {
	wm := s.GetWatermark()
	cut := wm - keepSec
	var pending int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pending WHERE logtime > ?`, wm).Scan(&pending); err != nil {
		return 0, 0, err
	}
	if cut <= 0 {
		return 0, pending, nil // 水位太年轻，什么都不删
	}
	res, err := s.db.Exec(`DELETE FROM pending WHERE logtime <= ?`, cut)
	if err != nil {
		return 0, pending, err
	}
	n, _ := res.RowsAffected()
	return n, pending, nil
}

// IncrementalVacuum 回收已删除的空闲页（配合 auto_vacuum(INCREMENTAL)）。
// 没有它，DELETE 之后 .db 文件不会缩小，节点会长期占用历史峰值磁盘。
// 注意 auto_vacuum 只对**新建**的库生效，老库上这条语句是空操作（安全）。
func (s *NodeStore) IncrementalVacuum() error {
	_, err := s.db.Exec(`PRAGMA incremental_vacuum`)
	return err
}

// RewindWatermark 时钟回拨应急：把水位回退 rewindSec 秒，触发该窗口内的数据重报。
// 中心 pinglog 主键 (target,node_id,logtime) + ON CONFLICT DO NOTHING 保证重报幂等。
func (s *NodeStore) RewindWatermark(rewindSec int64) error {
	nw := s.GetWatermark() - rewindSec
	if nw < 0 {
		nw = 0
	}
	_, err := s.db.Exec(`INSERT INTO kv (k, v) VALUES ('watermark', ?)
		ON CONFLICT (k) DO UPDATE SET v=excluded.v`, fmt.Sprintf("%d", nw))
	return err
}

// MaxLogtime 缓冲里最新的 logtime
func (s *NodeStore) MaxLogtime() (int64, bool) {
	var v sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(logtime) FROM pending`).Scan(&v); err != nil || !v.Valid {
		return 0, false
	}
	return v.Int64, true
}

// Count 当前缓冲行数（诊断用）
func (s *NodeStore) Count() int64 {
	var n int64
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM pending`).Scan(&n)
	return n
}

// CountAfter 水位之后的未确认行数（真·积压，心跳上报用）
func (s *NodeStore) CountAfter(watermark int64) int64 {
	var n int64
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM pending WHERE logtime > ?`, watermark).Scan(&n)
	return n
}

// Close 关闭
func (s *NodeStore) Close() error {
	return s.db.Close()
}
