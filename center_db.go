//go:build center

package main

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PingRow 中心端落库行（logtime 为节点上报的整分钟时刻）
type PingRow struct {
	Logtime  time.Time
	Target   string
	NodeID   string
	MaxDelay float64
	MinDelay float64
	AvgDelay float64
	SendPk   int
	RecvPk   int
	LossPk   int
}

// NodeInfo 节点注册信息
type NodeInfo struct {
	NodeID        string    `json:"node_id"`
	IP            string    `json:"ip"`
	Version       string    `json:"version"`
	Targets       int       `json:"targets"`
	LastSeen      time.Time `json:"last_seen"`
	ClockOffsetMs int64     `json:"clock_offset_ms"` // 节点时钟偏移估算（毫秒）
	BootTs        int64     `json:"boot_ts"`        // 节点进程启动时刻（重启回执判据）
	// 新增节点时由中心按"地区+运营商"自动拼出的显示名（不再需要改 center.json）
	Label    string `json:"label,omitempty"`
	Province string `json:"province,omitempty"`
	City     string `json:"city,omitempty"`
	Telecom  string `json:"telecom,omitempty"`
	// Pubkey 该节点登记的公钥（hex）；空 = 尚未登记（此时只能走 HMAC 兼容通道）
	Pubkey string `json:"pubkey,omitempty"`
	// PubkeyFP 公钥指纹（SHA-256 前 16 位），供面板人工核对
	PubkeyFP string `json:"pubkey_fp,omitempty"`
	// Visible 是否在前台（首页）展示：false = 隐藏（仍照常探测/上报/入库）
	Visible bool `json:"visible"`
}

// TargetRow target_alias 行：alias 是历史归属名，换 IP 只改 current_ip。
// province/city/telecom 是全国地图聚合维度（telecom: ctcc/cucc/cmcc）。
type TargetRow struct {
	Alias    string `json:"alias"`
	IP       string `json:"ip"`
	Province string `json:"province,omitempty"`
	City     string `json:"city,omitempty"`
	Telecom  string `json:"telecom,omitempty"`
	Updated  string `json:"updated,omitempty"`
}

// SnapRow 最近窗口单目标快照
type SnapRow struct {
	Avg     float64
	Send    int
	Recv    int
	Loss    int
	Logtime time.Time
}

// HistIP 单目标历史序列（桶采样后）
type HistIP struct {
	Alias   string     `json:"-"`
	IP      string     `json:"ip"`
	City    string     `json:"city"`
	Telecom string     `json:"telecom"`
	Times   []string   `json:"times"`
	History []*float64 `json:"history"` // null = 该时间桶没有有效样本（全是超时/无数据）
	Loss    []float64  `json:"loss"`
}

// CenterDB TimescaleDB / 普通 PG 自适应存储层
type CenterDB struct {
	pool  *pgxpool.Pool
	hasTS bool

	// listTargetsErr 仅测试用：注入 ListTargets 故障，验证 buildRows 的 fail-closed 行为。
	// 生产恒为 nil —— 没有它就无法在不真的弄坏 PG 的前提下测「读表失败」这条路径。
	listTargetsErr error
}

// OpenCenterDB 连接 PG 并检测 TimescaleDB，初始化 schema。
func OpenCenterDB(dsn string) (*CenterDB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 5 // 2C2G 机器，收敛连接数
	cfg.MaxConnLifetime = time.Hour
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	var ext int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM pg_extension WHERE extname='timescaledb'`).Scan(&ext)
	c := &CenterDB{pool: pool, hasTS: ext > 0}
	if err := c.ensureSchema(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return c, nil
}

// ensureSchema 建表。pinglog 主键 (target, node_id, logtime) 兼作幂等去重键，
// 明细永久保留：TimescaleDB 模式下按天 chunk + 7 天后列存压缩（~90%），
// 不建聚合表、不设保留删除策略；仅保留一个 logtime 索引服务时间范围查询
//（压缩 chunk 自带 min/max 元数据裁剪，不受此索引影响）。
func (c *CenterDB) ensureSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS pinglog (
			logtime    TIMESTAMPTZ NOT NULL,
			target     TEXT        NOT NULL,
			node_id    TEXT        NOT NULL,
			maxdelay   REAL,
			mindelay   REAL,
			avgdelay   REAL,
			sendpk     INT,
			revcpk     INT,
			losspk     INT,
			received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (target, node_id, logtime)
		)`,
		`CREATE TABLE IF NOT EXISTS node (
			node_id   TEXT PRIMARY KEY,
			ip        TEXT,
			version   TEXT,
			targets   INT DEFAULT 0,
			token     TEXT DEFAULT '',
			last_seen TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`ALTER TABLE node ADD COLUMN IF NOT EXISTS token TEXT DEFAULT ''`,
		`CREATE TABLE IF NOT EXISTS target_alias (
			alias      TEXT PRIMARY KEY,
			current_ip TEXT NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		// 全国地图维度列（增量升级已部署库）
		`ALTER TABLE target_alias ADD COLUMN IF NOT EXISTS province TEXT DEFAULT ''`,
		`ALTER TABLE target_alias ADD COLUMN IF NOT EXISTS city TEXT DEFAULT ''`,
		`ALTER TABLE target_alias ADD COLUMN IF NOT EXISTS telecom TEXT DEFAULT ''`,
		// 一次性安装码 + 时钟偏移 + 进程启动时刻（增量升级）
		`ALTER TABLE node ADD COLUMN IF NOT EXISTS install_code TEXT DEFAULT ''`,
		`ALTER TABLE node ADD COLUMN IF NOT EXISTS code_expire TIMESTAMPTZ`,
		`ALTER TABLE node ADD COLUMN IF NOT EXISTS clock_offset_ms BIGINT DEFAULT 0`,
		`ALTER TABLE node ADD COLUMN IF NOT EXISTS boot_ts BIGINT DEFAULT 0`,
		// 节点显示名与归属（新增节点时按"地区+运营商"自动写入，首页直接用）
		`ALTER TABLE node ADD COLUMN IF NOT EXISTS label TEXT DEFAULT ''`,
		`ALTER TABLE node ADD COLUMN IF NOT EXISTS province TEXT DEFAULT ''`,
		`ALTER TABLE node ADD COLUMN IF NOT EXISTS city TEXT DEFAULT ''`,
		`ALTER TABLE node ADD COLUMN IF NOT EXISTS telecom TEXT DEFAULT ''`,
		// Ed25519 公钥（节点的身份凭据；中心只存公钥，私钥永不出节点）
		`ALTER TABLE node ADD COLUMN IF NOT EXISTS pubkey TEXT DEFAULT ''`,
		// 前台是否展示（隐藏只影响首页展示，节点照常探测与上报，历史数据保留）
		`ALTER TABLE node ADD COLUMN IF NOT EXISTS visible BOOLEAN NOT NULL DEFAULT TRUE`,
		// 目标表版本号自增计数器（A3：原先用 max(updated_at) 猜，删非最新目标时版本不变，
		// 离线节点会拒绝加载新配置、继续探测已删目标）
		`CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v BIGINT DEFAULT 0)`,
		`INSERT INTO meta (k, v) VALUES ('targets_version', 1) ON CONFLICT (k) DO NOTHING`,
		// 地图页/快照按时间过滤的索引（主键先导列是 target，LatestSnapshot 走不了它）
		`CREATE INDEX IF NOT EXISTS idx_pinglog_logtime ON pinglog (logtime DESC)`,
	}
	for _, q := range stmts {
		if _, err := c.pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("建表失败: %w", err)
		}
	}
	// current_ip 唯一：A4 的「先查后写」在并发下会漏（两个 alias 指向同一 IP，
	// 节点重复探测、历史劈裂，之后一次 targets/del 会把两条都删掉）。唯一索引做最终兜底。
	// 线上库若已有重复数据，建索引会失败 —— 这里**不中断启动**，但要高声告警并给出排查 SQL。
	if _, err := c.pool.Exec(ctx,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_target_alias_current_ip ON target_alias (current_ip)`); err != nil {
		log.Printf("centerdb: !!! 建立 current_ip 唯一索引失败（目标表里可能已有重复 IP）: %v", err)
		log.Printf("centerdb: !!! 排查：SELECT current_ip, count(*), string_agg(alias,',') FROM target_alias GROUP BY 1 HAVING count(*)>1;")
	} else {
		log.Printf("centerdb: current_ip 唯一索引就绪")
	}
	if c.hasTS {
		if _, err := c.pool.Exec(ctx,
			`SELECT create_hypertable('pinglog', 'logtime',
				chunk_time_interval => INTERVAL '1 day', if_not_exists => TRUE,
				migrate_data => TRUE)`); err != nil {
			return fmt.Errorf("创建 hypertable 失败: %w", err)
		}
		if _, err := c.pool.Exec(ctx,
			`ALTER TABLE pinglog SET (timescaledb.compress,
				timescaledb.compress_segmentby = 'target',
				timescaledb.compress_orderby = 'logtime DESC')`); err != nil {
			// 不中断启动（老版本语法差异），但必须吼出来：压缩没开 = 明细按原始体积增长，
			// README 承诺的"压缩后 22GB/年"会变成 ~180GB/年，50G 盘几个月就满。
			log.Printf("centerdb: !!! 启用压缩失败：明细将不做列存压缩（磁盘增长约快 10 倍）: %v", err)
			log.Printf("centerdb: !!! 请核对 TimescaleDB 版本与主键/segmentby 是否匹配（自查 SQL 见 README）")
		} else {
			if _, err := c.pool.Exec(ctx,
				`SELECT add_compression_policy('pinglog', INTERVAL '7 days', if_not_exists => TRUE)`); err != nil {
				log.Printf("centerdb: !!! 压缩策略设置失败（等于不会自动压缩，磁盘会持续高速增长）: %v", err)
			} else {
				c.verifyCompression(ctx)
			}
		}
		log.Printf("centerdb: TimescaleDB 模式（按天 chunk + 7 天压缩, 明细永久保留）")
	} else {
		log.Printf("centerdb: 普通 PostgreSQL 模式（未检测到 TimescaleDB, 明细永久保留）")
	}
	return nil
}

// verifyCompression 启动时确认压缩真的生效，并把结论打进日志。
// 交接文档的 P0 就是"无人观测的慢故障"，而压缩是唯一的防磁盘写满手段：
// 它的失败原本只打一行普通日志，且当时库里只有 7 个 chunk —— 7 天策略一次都还没触发过，
// 等于这条路径在生产上从未被验证。
func (c *CenterDB) verifyCompression(ctx context.Context) {
	var n int
	if err := c.pool.QueryRow(ctx,
		`SELECT count(*) FROM timescaledb_information.compression_settings WHERE hypertable_name='pinglog'`).Scan(&n); err != nil {
		log.Printf("centerdb: 压缩状态自查失败（不影响运行）: %v", err)
		return
	}
	if n == 0 {
		log.Printf("centerdb: !!! 压缩未生效（compression_settings 里没有 pinglog）：明细不做列存压缩，磁盘会持续高速增长")
		return
	}
	var chunks, compressed int
	_ = c.pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE is_compressed)
		FROM timescaledb_information.chunks WHERE hypertable_name='pinglog'`).Scan(&chunks, &compressed)
	log.Printf("centerdb: 压缩已启用（chunk %d 个，已压缩 %d 个；未满 7 天的 chunk 不压缩属正常）", chunks, compressed)
}

// LogStorageReport 采集一次存储健康快照（体积/行数/滞后/压缩状态）并写日志。
// 由 center 的 monitorLoop 每 6 小时调用一次。
func (c *CenterDB) LogStorageReport(ctx context.Context) {
	const parentSQL = `SELECT pg_size_pretty(pg_total_relation_size('pinglog')),
		(SELECT count(*) FROM pinglog), (SELECT max(logtime) FROM pinglog)`
	// 体积口径必须用 hypertable_size：hypertable 的父表本身几乎是空的
	// （线上实测 parent=24kB vs hypertable=684MB），只查父表会让这条自观测日志
	// 永远显示一个"很小"的假数字 —— 恰好掩盖了它要盯的增长。
	const tsSQL = `SELECT pg_size_pretty(coalesce(hypertable_size('pinglog'), pg_total_relation_size('pinglog'))),
		(SELECT count(*) FROM pinglog), (SELECT max(logtime) FROM pinglog)`

	var size string
	var rows int64
	var newest sql.NullTime
	fallback := false
	sizeSQL := parentSQL
	if c.hasTS {
		sizeSQL = tsSQL
	}
	if err := c.pool.QueryRow(ctx, sizeSQL).Scan(&size, &rows, &newest); err != nil {
		if !c.hasTS {
			log.Printf("centerdb: 存储自查失败: %v", err)
			return
		}
		// 版本差异导致 hypertable_size 不可用时退回父表口径，至少保证日志能出
		if err2 := c.pool.QueryRow(ctx, parentSQL).Scan(&size, &rows, &newest); err2 != nil {
			log.Printf("centerdb: 存储自查失败: %v", err2)
			return
		}
		fallback = true
	}
	msg := fmt.Sprintf("centerdb: 存储自查 体积=%s", size)
	if fallback {
		msg += "(父表口径, 不含 chunk)"
	}
	msg += fmt.Sprintf(" 行数=%d", rows)
	if newest.Valid {
		msg += fmt.Sprintf(" 最新数据=%s(滞后 %s)", newest.Time.Format("01-02 15:04"),
			time.Since(newest.Time).Round(time.Minute))
	}
	var chunks, compressed int
	var oldest sql.NullTime
	tsOK := false
	if c.hasTS {
		if err := c.pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE is_compressed), min(range_start)
			FROM timescaledb_information.chunks WHERE hypertable_name='pinglog'`).
			Scan(&chunks, &compressed, &oldest); err == nil {
			tsOK = true
			msg += fmt.Sprintf(" chunk=%d 已压缩=%d", chunks, compressed)
		}
	}
	log.Print(msg)
	// 最早的 chunk 已超过策略窗口（7 天）却一个都没压缩 ⇒ 压缩策略没生效。
	if tsOK && compressed == 0 && oldest.Valid && time.Since(oldest.Time) > 8*24*time.Hour {
		log.Printf("centerdb: !!! 最早 chunk 已有 %s 但没有任何 chunk 被压缩：压缩策略未生效，磁盘会持续高速增长，请立即排查",
			time.Since(oldest.Time).Round(time.Hour))
	}
}

// InsertRows 批量幂等落库。
// 性能：原先逐行 tx.Exec，2000 行 = 2000 次网络往返，跨云 RTT 30ms 直接打满 20s 超时（B3）。
// 现在 CopyFrom 单次把整批推进临时表，再一条 INSERT...SELECT...ON CONFLICT DO NOTHING 入正式表 ——
// 两次往返搞定任意批量，同时保留 (target,node_id,logtime) 幂等语义。
// 返回实际新插入行数。
func (c *CenterDB) InsertRows(ctx context.Context, rows []PingRow) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	const stage = "pinglog_stage"
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE IF NOT EXISTS `+stage+` (
		logtime TIMESTAMPTZ, target TEXT, node_id TEXT,
		maxdelay REAL, mindelay REAL, avgdelay REAL,
		sendpk INT, revcpk INT, losspk INT) ON COMMIT DROP`); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `TRUNCATE `+stage); err != nil {
		return 0, err
	}
	cnt, err := tx.CopyFrom(ctx, pgx.Identifier{stage},
		[]string{"logtime", "target", "node_id", "maxdelay", "mindelay", "avgdelay", "sendpk", "revcpk", "losspk"},
		pgx.CopyFromSlice(len(rows), func(i int) ([]interface{}, error) {
			r := rows[i]
			return []interface{}{r.Logtime, r.Target, r.NodeID, r.MaxDelay, r.MinDelay,
				r.AvgDelay, r.SendPk, r.RecvPk, r.LossPk}, nil
		}))
	if err != nil {
		return 0, err
	}
	if cnt == 0 {
		return 0, tx.Commit(ctx)
	}
	ct, err := tx.Exec(ctx, `INSERT INTO pinglog
		(logtime, target, node_id, maxdelay, mindelay, avgdelay, sendpk, revcpk, losspk)
		SELECT logtime, target, node_id, maxdelay, mindelay, avgdelay, sendpk, revcpk, losspk
		FROM `+stage+`
		ON CONFLICT (target, node_id, logtime) DO NOTHING`)
	if err != nil {
		return 0, err
	}
	return ct.RowsAffected(), tx.Commit(ctx)
}

// QueryHistory 查询明细（node/target 传空表示不过滤）。
// 时间戳在 Go 侧格式化：旧实现用 SQL 的 to_char()，输出的是 **PG 会话时区**的墙钟，
// 而同一份数据的其它接口由 Go 侧 Format 输出（进程时区），两个页面能差好几个小时。
func (c *CenterDB) QueryHistory(ctx context.Context, node, target string, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 || limit > 5000 {
		limit = 200
	}
	rows, err := c.pool.Query(ctx, `SELECT logtime, target, node_id,
		maxdelay, mindelay, avgdelay, sendpk, revcpk, losspk
		FROM pinglog WHERE ($1='' OR node_id=$1) AND ($2='' OR target=$2)
		ORDER BY logtime DESC LIMIT $3`, node, target, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]interface{}
	for rows.Next() {
		var lt time.Time
		var tg, nid string
		var maxd, mind, avgd float64
		var send, recv, loss int
		if err := rows.Scan(&lt, &tg, &nid, &maxd, &mind, &avgd, &send, &recv, &loss); err != nil {
			return nil, err
		}
		out = append(out, map[string]interface{}{
			"logtime": lt.Format("2006-01-02 15:04:05"), "target": tg, "node_id": nid,
			"maxdelay": maxd, "mindelay": mind, "avgdelay": avgd,
			"sendpk": send, "revcpk": recv, "losspk": loss,
		})
	}
	return out, rows.Err()
}

// UpsertNode 心跳落库（含时钟偏移与进程启动时刻）
func (c *CenterDB) UpsertNode(ctx context.Context, ni NodeInfo) error {
	_, err := c.pool.Exec(ctx, `INSERT INTO node (node_id, ip, version, targets, clock_offset_ms, boot_ts, last_seen)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (node_id) DO UPDATE SET ip=EXCLUDED.ip, version=EXCLUDED.version,
			targets=EXCLUDED.targets, clock_offset_ms=EXCLUDED.clock_offset_ms,
			boot_ts=EXCLUDED.boot_ts, last_seen=EXCLUDED.last_seen`,
		ni.NodeID, ni.IP, ni.Version, ni.Targets, ni.ClockOffsetMs, ni.BootTs, ni.LastSeen)
	return err
}

// TouchNode 只刷新 ip 与 last_seen（B5：重连时绝不能清空 version/targets/clock_offset/boot_ts）
func (c *CenterDB) TouchNode(ctx context.Context, name, ip string) error {
	_, err := c.pool.Exec(ctx, `UPDATE node SET ip=$2, last_seen=now() WHERE node_id=$1`, name, ip)
	return err
}

// GetNode 取单个节点
func (c *CenterDB) GetNode(ctx context.Context, name string) (NodeInfo, error) {
	var ni NodeInfo
	err := c.pool.QueryRow(ctx, `SELECT node_id, coalesce(ip,''), coalesce(version,''),
		coalesce(targets,0), last_seen, coalesce(clock_offset_ms,0), coalesce(boot_ts,0)
		FROM node WHERE node_id=$1`, name).
		Scan(&ni.NodeID, &ni.IP, &ni.Version, &ni.Targets, &ni.LastSeen, &ni.ClockOffsetMs, &ni.BootTs)
	return ni, err
}

// NodeExists 节点名是否已注册（拒绝重名，防静默覆盖 token）
func (c *CenterDB) NodeExists(ctx context.Context, name string) (bool, error) {
	var n int
	err := c.pool.QueryRow(ctx, `SELECT count(*) FROM node WHERE node_id=$1`, name).Scan(&n)
	return n > 0, err
}

// DeleteNode 删除节点注册信息（历史明细 pinglog 不动）。
func (c *CenterDB) DeleteNode(ctx context.Context, name string) (int64, error) {
	ct, err := c.pool.Exec(ctx, `DELETE FROM node WHERE node_id=$1`, name)
	if err != nil {
		return 0, err
	}
	return ct.RowsAffected(), nil
}

// GetNodeToken 取节点独立 token（供 HMAC 校验；取不到=无独立 token）
func (c *CenterDB) GetNodeToken(ctx context.Context, name string) (string, error) {
	var tok string
	err := c.pool.QueryRow(ctx, `SELECT token FROM node WHERE node_id=$1 AND token<>''`, name).Scan(&tok)
	if err != nil {
		return "", err
	}
	return tok, nil
}

// NodeCreate 新增节点时写入的字段（显示名由中心按"地区+运营商"拼好）
type NodeCreate struct {
	NodeID   string
	Token    string
	Code     string
	Expire   time.Time
	Label    string
	Province string
	City     string
	Telecom  string
}

// CreateNode 新增节点注册记录
func (c *CenterDB) CreateNode(ctx context.Context, n NodeCreate) error {
	_, err := c.pool.Exec(ctx, `INSERT INTO node (node_id, token, install_code, code_expire, last_seen,
		label, province, city, telecom)
		VALUES ($1,$2,$3,$4,now(),$5,$6,$7,$8)`,
		n.NodeID, n.Token, n.Code, n.Expire, n.Label, n.Province, n.City, n.Telecom)
	return err
}

// NodeInstallInfo 兑换安装码所需信息
type NodeInstallInfo struct {
	Token      string
	Code       string
	CodeExpire time.Time
}

// RedeemInstallCode 校验并**一次性消费**安装码，返回 token。
// 用一条 UPDATE ... WHERE install_code=$2 AND (未过期) 完成「校验 + 作废」，
// 天然原子：并发兑换只有一个能拿到 token（D2：兑换即作废）。
// pubkey 非空时**在同一条语句里**写入公钥 —— 消费安装码与登记公钥必须一起成功/失败，
// 否则会出现"码没了、公钥没写上"导致节点永远登不上。
func (c *CenterDB) RedeemInstallCode(ctx context.Context, name, code, pubkey string) (string, error) {
	var token string
	err := c.pool.QueryRow(ctx, `UPDATE node SET install_code='', code_expire=NULL,
		pubkey = CASE WHEN $3='' THEN pubkey ELSE $3 END
		WHERE node_id=$1 AND install_code=$2 AND token<>''
		  AND (code_expire IS NULL OR code_expire > now())
		RETURNING token`, name, code, pubkey).Scan(&token)
	return token, err
}

// GetNodePubkey 取节点登记的公钥（hex）；未登记返回空串
func (c *CenterDB) GetNodePubkey(ctx context.Context, name string) (string, error) {
	var pk string
	err := c.pool.QueryRow(ctx, `SELECT coalesce(pubkey,'') FROM node WHERE node_id=$1`, name).Scan(&pk)
	if err != nil {
		return "", err
	}
	return pk, nil
}

// SetNodePubkey 写入/替换节点公钥（登记与换钥匙都走这里）
func (c *CenterDB) SetNodePubkey(ctx context.Context, name, pubkey string) error {
	ct, err := c.pool.Exec(ctx, `UPDATE node SET pubkey=$2 WHERE node_id=$1`, name, pubkey)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("节点不存在: %s", name)
	}
	return nil
}

// ListNodes 节点列表
func (c *CenterDB) ListNodes(ctx context.Context) ([]NodeInfo, error) {
	rows, err := c.pool.Query(ctx, `SELECT node_id, coalesce(ip,''), coalesce(version,''),
		coalesce(targets,0), last_seen, coalesce(clock_offset_ms,0), coalesce(boot_ts,0),
		coalesce(label,''), coalesce(province,''), coalesce(city,''), coalesce(telecom,''),
		coalesce(pubkey,''), coalesce(visible,true)
		FROM node ORDER BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeInfo
	for rows.Next() {
		var ni NodeInfo
		if err := rows.Scan(&ni.NodeID, &ni.IP, &ni.Version, &ni.Targets, &ni.LastSeen,
			&ni.ClockOffsetMs, &ni.BootTs, &ni.Label, &ni.Province, &ni.City, &ni.Telecom, &ni.Pubkey,
			&ni.Visible); err != nil {
			return nil, err
		}
		ni.PubkeyFP = pubkeyFingerprintHex(ni.Pubkey)
		out = append(out, ni)
	}
	return out, rows.Err()
}

// SetNodeMeta 更新节点的展示元信息（显示名与归属），不改身份、不影响采集
func (c *CenterDB) SetNodeMeta(ctx context.Context, name, label, province, city, telecom string) error {
	ct, err := c.pool.Exec(ctx, `UPDATE node SET label=$2, province=$3, city=$4, telecom=$5
		WHERE node_id=$1`, name, label, province, city, telecom)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("节点不存在: %s", name)
	}
	return nil
}

// SetNodeVisible 设置前台是否展示（隐藏时节点照常探测/上报，历史保留）
func (c *CenterDB) SetNodeVisible(ctx context.Context, name string, visible bool) error {
	ct, err := c.pool.Exec(ctx, `UPDATE node SET visible=$2 WHERE node_id=$1`, name, visible)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("节点不存在: %s", name)
	}
	return nil
}

// pubkeyFingerprintHex 由 hex 公钥算指纹（非法/为空时返回空串，绝不 panic）
func pubkeyFingerprintHex(pubHex string) string {
	pub, err := hex.DecodeString(pubHex)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return ""
	}
	return pubkeyFingerprint(pub)
}

// NodeLabels 只取"节点 id → 显示名"，用于首页把节点名换成中文（每请求一次，行数=节点数，可忽略）
func (c *CenterDB) NodeLabels(ctx context.Context) (map[string]string, error) {
	rows, err := c.pool.Query(ctx, `SELECT node_id, coalesce(label,'') FROM node`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, l string
		if err := rows.Scan(&id, &l); err != nil {
			return nil, err
		}
		if l != "" {
			out[id] = l
		}
	}
	return out, rows.Err()
}

// ProvinceCities 目标表里出现过的 省 → 市 列表（后台"新增节点"的地区下拉用：
// 城市列表跟着实际监测目标走，不写死一份可能过期的城市表）
func (c *CenterDB) ProvinceCities(ctx context.Context) (map[string][]string, error) {
	rows, err := c.pool.Query(ctx, `SELECT DISTINCT province, city FROM target_alias
		WHERE coalesce(province,'')<>'' AND coalesce(city,'')<>'' ORDER BY province, city`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var p, city string
		if err := rows.Scan(&p, &city); err != nil {
			return nil, err
		}
		out[p] = append(out[p], city)
	}
	return out, rows.Err()
}

// BumpTargetsVersion 目标表任何增删改后自增版本号并返回新值（A3）。
// 删掉非最新的那条目标时 max(updated_at) 不变，旧算法会让离线节点拒绝加载新配置。
func (c *CenterDB) BumpTargetsVersion(ctx context.Context) (int64, error) {
	var v int64
	err := c.pool.QueryRow(ctx, `UPDATE meta SET v = v + 1 WHERE k='targets_version' RETURNING v`).Scan(&v)
	if err != nil {
		return 0, err
	}
	return v, nil
}

// TargetsVersion 当前目标配置版本号
func (c *CenterDB) TargetsVersion(ctx context.Context) (int64, error) {
	var v int64
	err := c.pool.QueryRow(ctx, `SELECT v FROM meta WHERE k='targets_version'`).Scan(&v)
	return v, err
}

// IPInUse 当前 IP 是否已被**其他** alias 占用（excluding 传入的 alias 自身）
func (c *CenterDB) IPInUse(ctx context.Context, ip, excludingAlias string) (string, bool, error) {
	var alias string
	err := c.pool.QueryRow(ctx, `SELECT alias FROM target_alias
		WHERE current_ip=$1 AND alias<>$2 LIMIT 1`, ip, excludingAlias).Scan(&alias)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return alias, true, nil
}

// isUniqueViolation 判断是否唯一约束冲突（23505），用于把并发写的竞态翻译成人话。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ErrIPInUse IP 已被其它目标占用：这是**业务冲突**，调用方应回 4xx 而不是 5xx。
// 旧实现把它和 PG 故障混在一起写 500，把前端的排障方向带偏。
var ErrIPInUse = errors.New("IP 已被其它目标占用")

// ErrTargetNotFound 目标不存在（oldIP 之类查不到）。
var ErrTargetNotFound = errors.New("目标不存在")

// UpsertTarget 添加/更新目标（alias 不变换 IP：历史数据自然衔接），带地图维度
func (c *CenterDB) UpsertTarget(ctx context.Context, t TargetRow) error {
	_, err := c.pool.Exec(ctx, `INSERT INTO target_alias (alias, current_ip, province, city, telecom, updated_at)
		VALUES ($1,$2,$3,$4,$5,now())
		ON CONFLICT (alias) DO UPDATE SET current_ip=EXCLUDED.current_ip,
			province=EXCLUDED.province, city=EXCLUDED.city, telecom=EXCLUDED.telecom, updated_at=now()`,
		t.Alias, t.IP, t.Province, t.City, t.Telecom)
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: %s", ErrIPInUse, t.IP)
	}
	return err
}

// RenameTargetAlias 「不继承历史」的换 IP：alias 一并换成新 IP，旧 alias 行删除。
// pinglog 里旧 alias 的历史不会再挂到这个目标上（面板上表现为历史从零开始），
// province/city/telecom 维度保留。
func (c *CenterDB) RenameTargetAlias(ctx context.Context, oldIP, newIP string) (int64, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var alias, province, city, telecom string
	err = tx.QueryRow(ctx, `SELECT alias, coalesce(province,''), coalesce(city,''), coalesce(telecom,'')
		FROM target_alias WHERE current_ip=$1 FOR UPDATE`, oldIP).
		Scan(&alias, &province, &city, &telecom)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var holder string
	err = tx.QueryRow(ctx, `SELECT alias FROM target_alias WHERE current_ip=$1 OR alias=$1 LIMIT 1`, newIP).Scan(&holder)
	if err == nil {
		return 0, fmt.Errorf("%w: 新 IP %s 已被 alias=%s 占用", ErrIPInUse, newIP, holder)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM target_alias WHERE alias=$1`, alias); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO target_alias (alias, current_ip, province, city, telecom, updated_at)
		VALUES ($1,$2,$3,$4,$5,now())`, newIP, newIP, province, city, telecom); err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("%w: 新 IP %s", ErrIPInUse, newIP)
		}
		return 0, err
	}
	return 1, tx.Commit(ctx)
}

// ResolveAlias 把「前端传来的 IP」解析成 pinglog.target 实际存的 alias。
//
// P1-4 修复：pinglog 的 target 列存的是 **alias**（历史归属名），而管理面板
// 传进来的是**当前 IP**。alias==ip 只在「从未换过 IP」时成立；一旦执行过
// targets/replace（alias 不变、只改 current_ip），两者就分叉了：
//   - TargetIPHistory / NodeCompare 用 `WHERE target=$1` 传当前 IP ⇒ 查不到任何行
//   - 而 DeleteTargetByIP 用 `WHERE current_ip=$1` 传当前 IP ⇒ 能删掉
// 同一份前端数据，一个接口删得掉、另一个接口查不出 ⇒ 换过 IP 的目标历史与
// 节点对比曲线永久空白。
//
// 解析顺序：先按 current_ip 精确匹配（覆盖 alias!=ip 的分叉情况），
// 未命中再按 alias 匹配（覆盖未换 IP 的老数据，也让本函数幂等可反复调用）。
func (c *CenterDB) ResolveAlias(ctx context.Context, ipOrAlias string) (string, error) {
	var alias string
	err := c.pool.QueryRow(ctx,
		`SELECT alias FROM target_alias WHERE current_ip=$1 OR alias=$1 LIMIT 1`, ipOrAlias).Scan(&alias)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil // 目标已删除：交由调用方按"查不到"处理
	}
	return alias, err
}

// ResolveAlias 内存版
func (m *MemStore) ResolveAlias(ipOrAlias string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for alias, t := range m.targets {
		if t.IP == ipOrAlias {
			return alias
		}
	}
	for alias := range m.targets {
		if alias == ipOrAlias {
			return alias
		}
	}
	return ""
}

// ListTargets 全部目标（带维度）
func (c *CenterDB) ListTargets(ctx context.Context) ([]TargetRow, error) {
	if c.listTargetsErr != nil {
		return nil, c.listTargetsErr
	}
	rows, err := c.pool.Query(ctx, `SELECT alias, current_ip, coalesce(province,''), coalesce(city,''),
		coalesce(telecom,''), to_char(updated_at,'YYYY-MM-DD HH24:MI') FROM target_alias ORDER BY alias`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TargetRow
	for rows.Next() {
		var t TargetRow
		if err := rows.Scan(&t.Alias, &t.IP, &t.Province, &t.City, &t.Telecom, &t.Updated); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// LatestSnapshot 最近 window 内每个 target 的最新一条记录。
//
// nodeID == "" ⇒ **全网最优口径**：取该 target 最新 logtime 的那一行，多节点并列时取更优值
// （优先非超时，再取平均延迟更小的）；nodeID != "" ⇒ 只取该节点的数据（单节点视角）。
//
// 取值规则（确定性，不再随机）：
//   1. 取该 target 最新的 logtime（被节点截断到整分钟，多节点并列是常态）；
//   2. 并列时优先 avgdelay > 0 的行（超时行排最后），再取平均延迟最小的；
//   3. 仍并列则按 node_id 排序，保证同一份数据每次返回同一行。
//
// 口径差异必须让调用方可见：全网最优会随节点数增加而偏低（多一台网络位置好的机器，
// 数字就集体下降），所以 /api/mapping.json 会回 scope/node 字段说明当前口径。
// 超时（avgdelay<=0）仍然会呈现（全是超时行时返回 0），不会被藏起来。
func (c *CenterDB) LatestSnapshot(ctx context.Context, window time.Duration, nodeID string) (map[string]SnapRow, error) {
	rows, err := c.pool.Query(ctx, `SELECT DISTINCT ON (target)
		target, avgdelay, sendpk, revcpk, losspk, logtime
		FROM pinglog WHERE logtime > now() - make_interval(secs => $1)
		  AND ($2 = '' OR node_id = $2)
		ORDER BY target, logtime DESC, (avgdelay <= 0) ASC, avgdelay ASC, node_id ASC`,
		int(window.Seconds()), nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]SnapRow{}
	for rows.Next() {
		var tg string
		var r SnapRow
		if err := rows.Scan(&tg, &r.Avg, &r.Send, &r.Recv, &r.Loss, &r.Logtime); err != nil {
			return nil, err
		}
		out[tg] = r
	}
	return out, rows.Err()
}

// bucketSeconds 把采样粒度换算成秒。
// **不再使用 TimescaleDB 的 time_bucket()**：本文件同时支持「普通 PostgreSQL 模式」
// （见 ensureSchema 的 else 分支），而 time_bucket 是扩展函数，普通 PG 下直接
// 报 42883 function does not exist，ProvinceHistory / NodeCompare 必 500。
// 固定间隔（1/5/15 分钟）的 time_bucket 与「按 epoch 对齐下取整」等价
// （TimescaleDB 的原点 2000-01-01 UTC 对 900 秒整除），所以这里用纯 SQL 表达，
// 两种模式都能跑。
func bucketSeconds(hours int) int {
	switch {
	case hours > 24:
		return 900 // 15 分钟
	case hours > 6:
		return 300 // 5 分钟
	default:
		return 60 // 1 分钟
	}
}

// ProvinceHistory 按省拉历史（桶采样控制数据量），返回每目标一条序列。
//
// 超时（avgdelay<=0，即 20 个包一个没回）**不参与平均**：它是"无限大延迟"的证据，
// 把它当 0 混进均值会让一条彻底断掉的链路画出贴近 0ms 的"健康曲线"
// （NodeCompare 早就过滤了，这里漏了）。整桶都没有有效样本时该点返回 null，
// 前端画断点而不是画到 0。
func (c *CenterDB) ProvinceHistory(ctx context.Context, province string, hours int, nodeID string) ([]HistIP, error) {
	secs := bucketSeconds(hours)
	rows, err := c.pool.Query(ctx, `SELECT t.alias, t.current_ip, coalesce(t.city,''), coalesce(t.telecom,''),
		to_timestamp(floor(extract(epoch from p.logtime) / $1) * $1) AS b,
		avg(p.avgdelay) FILTER (WHERE p.avgdelay > 0) AS d,
		avg(coalesce(p.losspk,0)::float * 100 / GREATEST(coalesce(p.sendpk,0), 1)) AS l
		FROM target_alias t
		JOIN pinglog p ON p.target = t.alias AND p.logtime > now() - make_interval(hours => $2)
		WHERE t.province = $3 AND ($4 = '' OR p.node_id = $4)
		GROUP BY 1,2,3,4,b ORDER BY 1, b`, secs, hours, province, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	idx := map[string]int{}
	var out []HistIP
	for rows.Next() {
		var alias string
		var b time.Time
		var d sql.NullFloat64
		var l float64
		var h HistIP
		if err := rows.Scan(&alias, &h.IP, &h.City, &h.Telecom, &b, &d, &l); err != nil {
			return nil, err
		}
		i, ok := idx[alias]
		if !ok {
			i = len(out)
			idx[alias] = i
			h.Alias = alias
			h.Times = []string{}
			h.History = []*float64{}
			h.Loss = []float64{}
			out = append(out, h)
		}
		out[i].Times = append(out[i].Times, b.Format("2006-01-02 15:04"))
		if d.Valid {
			v := round2(d.Float64)
			out[i].History = append(out[i].History, &v)
		} else {
			out[i].History = append(out[i].History, nil)
		}
		out[i].Loss = append(out[i].Loss, round2(l))
	}
	return out, rows.Err()
}

// HasTS 是否 TimescaleDB
func (c *CenterDB) HasTS() bool { return c.hasTS }

// Close 关闭连接池
func (c *CenterDB) Close() { c.pool.Close() }

// DeleteTargetByIP 按 current_ip 删除目标（管理面板 targets/del）
func (c *CenterDB) DeleteTargetByIP(ctx context.Context, ip string) (int64, error) {
	ct, err := c.pool.Exec(ctx, `DELETE FROM target_alias WHERE current_ip=$1`, ip)
	if err != nil {
		return 0, err
	}
	return ct.RowsAffected(), nil
}

// ReplaceTargetIP 换 IP：alias 不变仅更新 current_ip，历史数据自然衔接。
// 新 IP 已被其他 alias 占用时拒绝，避免两条 alias 指向同一探测地址（A4）。
func (c *CenterDB) ReplaceTargetIP(ctx context.Context, oldIP, newIP string) (int64, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var holder string
	err = tx.QueryRow(ctx, `SELECT alias FROM target_alias WHERE current_ip=$1 LIMIT 1`, newIP).Scan(&holder)
	if err == nil {
		return 0, fmt.Errorf("%w: 新 IP %s 已被 alias=%s 占用", ErrIPInUse, newIP, holder)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	ct, err := tx.Exec(ctx, `UPDATE target_alias SET current_ip=$2, updated_at=now() WHERE current_ip=$1`,
		oldIP, newIP)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("%w: 新 IP %s", ErrIPInUse, newIP)
		}
		return 0, err
	}
	if ct.RowsAffected() == 0 {
		return 0, fmt.Errorf("%w: current_ip=%s", ErrTargetNotFound, oldIP)
	}
	return ct.RowsAffected(), tx.Commit(ctx)
}

// TargetIPHistory 单目标（ip=alias）历史序列，升序；管理面板 targets/history 用。
// LIMIT 兜住内存：hours 最大 720，20 个节点 × 30 天 × 1 分钟 ≈ 86 万行，
// 无上限时会一次性载入内存并 JSON 编码（单个管理请求能把中心打爆）。
func (c *CenterDB) TargetIPHistory(ctx context.Context, ip string, hours int) ([]map[string]interface{}, error) {
	rows, err := c.pool.Query(ctx, `SELECT logtime, avgdelay, sendpk, losspk
		FROM pinglog WHERE target=$1 AND logtime > now() - make_interval(hours => $2)
		ORDER BY logtime LIMIT 20000`, ip, hours)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]interface{}
	for rows.Next() {
		var lt time.Time
		var avg float64
		var send, loss int
		if err := rows.Scan(&lt, &avg, &send, &loss); err != nil {
			return nil, err
		}
		out = append(out, map[string]interface{}{"time": lt.Format("2006-01-02 15:04:05"), "avgdelay": avg, "sendpk": send, "losspk": loss})
	}
	return out, rows.Err()
}

// NodeCompareRow / NodeComparePoint 节点对比序列（管理面板 nodecompare 用）
type NodeCompareRow struct {
	Name   string             `json:"name"`
	Points []NodeComparePoint `json:"points"`
}

type NodeComparePoint struct {
	T int64   `json:"t"` // unix 秒
	D float64 `json:"d"` // 平均延迟 ms
}

// NodeCompare 按节点分组的时间桶平均延迟（仅统计有回包的桶）。
// 同样不用 time_bucket()：普通 PG 模式要能跑（见 bucketSeconds）。
func (c *CenterDB) NodeCompare(ctx context.Context, target string, hours int) ([]NodeCompareRow, error) {
	secs := bucketSeconds(hours)
	rows, err := c.pool.Query(ctx, `
		SELECT node_id, (floor(extract(epoch from logtime) / $1) * $1)::bigint AS t, avg(avgdelay) AS d
		FROM pinglog
		WHERE target=$2 AND logtime > now() - make_interval(hours => $3) AND avgdelay > 0
		GROUP BY node_id, t ORDER BY node_id, t`, secs, target, hours)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	idx := map[string]int{}
	var out []NodeCompareRow
	for rows.Next() {
		var name string
		var p NodeComparePoint
		if err := rows.Scan(&name, &p.T, &p.D); err != nil {
			return nil, err
		}
		i, ok := idx[name]
		if !ok {
			i = len(out)
			idx[name] = i
			out = append(out, NodeCompareRow{Name: name, Points: []NodeComparePoint{}})
		}
		out[i].Points = append(out[i].Points, p)
	}
	return out, rows.Err()
}

// ---------- 内存模式（无 DB 时 / 测试用）----------

// MemStore 内存存储：同样语义，重启即清空
// memKeepRows 内存模式保留的明细行数（环形窗口）
const memKeepRows = 5000

type MemStore struct {
	mu      sync.Mutex
	nodes   map[string]NodeInfo
	targets map[string]TargetRow
	tokens  map[string]string // 每节点独立 token（内存模式的鉴权用）
	hidden  map[string]bool   // 前台隐藏的节点（不在集合里 = 可见）
	latest  []map[string]interface{}
	seen    map[string]struct{} // (target,node_id,logtime) 幂等去重，对齐 DB 模式的主键
	trims   int                 // 累计裁剪行数，用于决定何时整理底层数组
	ver     int64
}

func NewMemStore() *MemStore {
	return &MemStore{nodes: map[string]NodeInfo{}, targets: map[string]TargetRow{},
		tokens: map[string]string{}, hidden: map[string]bool{}, seen: map[string]struct{}{}, ver: 1}
}

// SetNodeToken / GetNodeToken 内存版的每节点独立 token（供鉴权与公钥登记测试使用）
func (m *MemStore) SetNodeToken(name, token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[name] = token
}

func (m *MemStore) GetNodeToken(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tokens[name]
}

// evictKeys 逐出被裁掉那批行的去重键（调用方持锁）。
// 旧实现每次裁剪都重建整张 5000 项的去重表 ⇒ 稳态下每次插入变成 O(5000)（整体 O(n²)）；
// 按"被裁掉的行"逐出则是 O(drop)，稳态 drop=1。
func (m *MemStore) evictKeys(dropped []map[string]interface{}) {
	for _, r := range dropped {
		delete(m.seen, rowKey(r))
	}
}

func rowKey(r map[string]interface{}) string {
	var ts int64
	switch v := r["logtime_unix"].(type) {
	case int64:
		ts = v
	case int:
		ts = int64(v)
	}
	return fmt.Sprintf("%v\x00%v\x00%d", r["target"], r["node_id"], ts)
}

// rowTime 取一行的时间。优先用 epoch（与节点时钟同源的绝对时刻）；
// 没有时回退解析墙钟字符串。
//
// 为什么必须用 epoch：内存模式存的是 `Format("2006-01-02 15:04:05")` —— 一个
// **不带时区偏移**的墙钟字面量。旧实现读回时按 time.Local 解释，于是节点在
// +08:00、中心在 UTC 时，一条 10 分钟前的数据会被当成"5 分钟窗口内的最新数据"
// （实测复现）；反过来时区落后的节点数据会被整个窗口过滤掉、地图上看不到它。
func rowTime(r map[string]interface{}) (time.Time, bool) {
	if v, ok := r["logtime_unix"].(int64); ok {
		return time.Unix(v, 0), true
	}
	s, ok := r["logtime"].(string)
	if !ok {
		return time.Time{}, false
	}
	lt, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return lt, true
}

// BumpTargetsVersion 内存版：自增版本号
func (m *MemStore) BumpTargetsVersion() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ver++
	return m.ver
}

// IPInUse 内存版：IP 是否被其他 alias 占用
func (m *MemStore) IPInUse(ip, excludingAlias string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for alias, t := range m.targets {
		if t.IP == ip && alias != excludingAlias {
			return alias, true
		}
	}
	return "", false
}

func (m *MemStore) UpsertNode(ni NodeInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nodes[ni.NodeID] = ni
}

// NodeExists 内存版：节点名是否已注册（与 DB 版语义一致）
func (m *MemStore) NodeExists(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.nodes[name]
	return ok
}

// CreateNode 内存版：新增节点注册记录（含显示名/归属），已存在返回错误
func (m *MemStore) CreateNode(n NodeCreate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.nodes[n.NodeID]; ok {
		return fmt.Errorf("node exists")
	}
	m.nodes[n.NodeID] = NodeInfo{
		NodeID: n.NodeID, LastSeen: time.Now(),
		Label: n.Label, Province: n.Province, City: n.City, Telecom: n.Telecom,
	}
	return nil
}

// SetNodePubkey 内存版：写入/替换节点公钥
func (m *MemStore) SetNodePubkey(name, pubkey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ni, ok := m.nodes[name]
	if !ok {
		return fmt.Errorf("节点不存在: %s", name)
	}
	ni.Pubkey = pubkey
	m.nodes[name] = ni
	return nil
}

// SetNodeMeta 内存版：更新显示名与归属
func (m *MemStore) SetNodeMeta(name, label, province, city, telecom string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ni, ok := m.nodes[name]
	if !ok {
		return fmt.Errorf("节点不存在: %s", name)
	}
	ni.Label, ni.Province, ni.City, ni.Telecom = label, province, city, telecom
	m.nodes[name] = ni
	return nil
}

// SetNodeVisible 内存版：设置前台是否展示
func (m *MemStore) SetNodeVisible(name string, visible bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.nodes[name]; !ok {
		return fmt.Errorf("节点不存在: %s", name)
	}
	if visible {
		delete(m.hidden, name)
	} else {
		m.hidden[name] = true
	}
	return nil
}

// GetNodeInfo 内存版：取单个节点
func (m *MemStore) GetNodeInfo(name string) (NodeInfo, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ni, ok := m.nodes[name]
	if ok {
		ni.Visible = !m.hidden[name]
	}
	return ni, ok
}

// visibleNodes 只保留前台可见的节点（隐藏的照常采集，只是不上首页）
func visibleNodes(list []NodeInfo) []NodeInfo {
	out := make([]NodeInfo, 0, len(list))
	for _, n := range list {
		if n.Visible {
			out = append(out, n)
		}
	}
	return out
}

// GetNodePubkey 内存版：取公钥（未登记返回空串）
func (m *MemStore) GetNodePubkey(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nodes[name].Pubkey
}

// NodeLabels 内存版：节点 id → 显示名
func (m *MemStore) NodeLabels() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for id, ni := range m.nodes {
		if strings.TrimSpace(ni.Label) != "" {
			out[id] = ni.Label
		}
	}
	return out
}

// ProvinceCities 内存版：目标表里出现过的 省 → 市 列表（后台"新增节点"下拉用）
func (m *MemStore) ProvinceCities() map[string][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]map[string]bool{}
	for _, t := range m.targets {
		if t.Province == "" || t.City == "" {
			continue
		}
		if seen[t.Province] == nil {
			seen[t.Province] = map[string]bool{}
		}
		seen[t.Province][t.City] = true
	}
	out := map[string][]string{}
	for p, cs := range seen {
		list := make([]string, 0, len(cs))
		for c := range cs {
			list = append(list, c)
		}
		sort.Strings(list)
		out[p] = list
	}
	return out
}

// TouchNode 只刷新 ip 与 last_seen，保留 version/targets/clock_offset/boot_ts（B5 的内存版）。
// 之前内存模式没有这个方法、touchNode 在 db==nil 时什么都不做，
// 于是 TestTouchNodeKeepsFields 天然通过（假绿）—— B5 的真实修复其实在 SQL 侧。
func (m *MemStore) TouchNode(name, ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ni, ok := m.nodes[name]
	if !ok {
		return
	}
	ni.IP = ip
	ni.LastSeen = time.Now()
	m.nodes[name] = ni
}

// DeleteNode 删除节点注册信息（内存版）
func (m *MemStore) DeleteNode(name string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.nodes[name]; !ok {
		return 0
	}
	delete(m.nodes, name)
	return 1
}

func (m *MemStore) ListNodes() []NodeInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]NodeInfo, 0, len(m.nodes))
	for _, ni := range m.nodes {
		ni.PubkeyFP = pubkeyFingerprintHex(ni.Pubkey)
		// 内存版用"隐藏集合"表达：不在集合里就是可见（避免零值把节点误判为隐藏）
		ni.Visible = !m.hidden[ni.NodeID]
		out = append(out, ni)
	}
	return out
}

func (m *MemStore) InsertRows(rows []PingRow) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, r := range rows {
		row := map[string]interface{}{
			"logtime": r.Logtime.Format("2006-01-02 15:04:05"),
			// epoch 与显示字符串并存：窗口判断/排序一律用 epoch（见 rowTime）
			"logtime_unix": r.Logtime.Unix(),
			"target":       r.Target, "node_id": r.NodeID,
			"maxdelay": r.MaxDelay, "mindelay": r.MinDelay, "avgdelay": r.AvgDelay,
			"sendpk": r.SendPk, "revcpk": r.RecvPk, "losspk": r.LossPk,
		}
		// 幂等：DB 模式靠 (target,node_id,logtime) 主键 + ON CONFLICT DO NOTHING，
		// 内存模式旧实现直接 append，节点重传会把历史/曲线重复计点。
		key := rowKey(row)
		if _, dup := m.seen[key]; dup {
			continue
		}
		m.seen[key] = struct{}{}
		m.latest = append(m.latest, row)
		n++
	}
	if len(m.latest) > memKeepRows {
		drop := len(m.latest) - memKeepRows
		m.evictKeys(m.latest[:drop]) // 被裁掉的行必须移出去重集合，否则重传会被误判为重复而丢弃
		m.latest = m.latest[drop:]
		// 截断后的切片仍指向原底层数组头部；累计裁掉一屏就整理一次，避免长期占住内存
		m.trims += drop
		if m.trims >= memKeepRows {
			cp := make([]map[string]interface{}, len(m.latest))
			copy(cp, m.latest)
			m.latest = cp
			m.trims = 0
		}
	}
	return n
}

func (m *MemStore) QueryHistory(node, target string, limit int) []map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 || limit > 5000 {
		limit = 200
	}
	var out []map[string]interface{}
	for i := len(m.latest) - 1; i >= 0 && len(out) < limit; i-- {
		r := m.latest[i]
		if node != "" && r["node_id"] != node {
			continue
		}
		if target != "" && r["target"] != target {
			continue
		}
		out = append(out, r)
	}
	return out
}

func (m *MemStore) UpsertTarget(t TargetRow) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t.Updated = time.Now().Format("2006-01-02 15:04")
	m.targets[t.Alias] = t
	// 版本号不在这里自增：与 DB 模式保持一致，只有 BumpTargetsVersion 推进它。
	// （旧实现在这里偷偷 ++，导致 TestTargetUpsertBumpsVersion 即使删掉 handler 的
	//   bump 也依然通过 —— 变异测试确认过的假绿。）
}

func (m *MemStore) ListTargets() []TargetRow {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TargetRow, 0, len(m.targets))
	for _, t := range m.targets {
		out = append(out, t)
	}
	return out
}

func (m *MemStore) TargetsVersion() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ver
}

// betterSnap 同一 logtime 内谁"更优"：优先非超时，再取平均延迟更小的。
// 与 SQL 侧 `(avgdelay <= 0) ASC, avgdelay ASC` 的语义保持一致。
func betterSnap(a, b SnapRow) bool {
	if (a.Avg > 0) != (b.Avg > 0) {
		return a.Avg > 0
	}
	return a.Avg < b.Avg
}

// LatestSnapshot 内存版：从 latest 环形缓冲取每 target 最新一条。
// nodeID 为空 = 全网最优（并列取更优值，与 DB 版一致）；非空 = 只取该节点。
func (m *MemStore) LatestSnapshot(window time.Duration, nodeID string) map[string]SnapRow {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]SnapRow{}
	cutoff := time.Now().Add(-window)
	for i := len(m.latest) - 1; i >= 0; i-- {
		r := m.latest[i]
		if nodeID != "" && r["node_id"] != nodeID {
			continue
		}
		tg := r["target"].(string)
		lt, okLT := rowTime(r)
		if !okLT || lt.Before(cutoff) {
			continue
		}
		cand := SnapRow{
			Avg: r["avgdelay"].(float64), Send: r["sendpk"].(int),
			Recv: r["revcpk"].(int), Loss: r["losspk"].(int), Logtime: lt,
		}
		prev, ok := out[tg]
		if !ok {
			out[tg] = cand
			continue
		}
		// 倒序遍历：先到的是更新的一行；只有同一 logtime 才按"取更优"比较
		if cand.Logtime.Equal(prev.Logtime) && betterSnap(cand, prev) {
			out[tg] = cand
		}
	}
	return out
}

// ProvinceHistory 内存版：从 latest 过滤省份（无桶采样，数据本来就少）。
// nodeID 为空 = 全部节点混合（与 DB 版的 avg 口径一致）；非空 = 只看该节点。
func (m *MemStore) ProvinceHistory(province string, hours int, nodeID string) []HistIP {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := map[string]int{}
	var out []HistIP
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour)
	for _, r := range m.latest {
		if nodeID != "" && r["node_id"] != nodeID {
			continue
		}
		tg := r["target"].(string)
		lt, okLT := rowTime(r)
		if !okLT || lt.Before(cutoff) {
			continue
		}
		t, ok := m.targets[tg]
		if !ok || t.Province != province {
			continue
		}
		i, ok2 := idx[tg]
		if !ok2 {
			i = len(out)
			idx[tg] = i
			out = append(out, HistIP{Alias: tg, IP: t.IP, City: t.City, Telecom: t.Telecom,
				Times: []string{}, History: []*float64{}, Loss: []float64{}})
		}
		out[i].Times = append(out[i].Times, lt.Format("2006-01-02 15:04"))
		// 与 DB 版一致：超时（avgdelay<=0）不参与平均，整桶无有效样本时给 null（前端画断点）
		if avg := r["avgdelay"].(float64); avg > 0 {
			v := avg
			out[i].History = append(out[i].History, &v)
		} else {
			out[i].History = append(out[i].History, nil)
		}
		out[i].Loss = append(out[i].Loss, float64(r["losspk"].(int))*100/float64(max1(r["sendpk"].(int))))
	}
	return out
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// DeleteTargetByIP 内存版：按 current_ip 删除
func (m *MemStore) DeleteTargetByIP(ip string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for alias, t := range m.targets {
		if t.IP == ip {
			delete(m.targets, alias)
			n++
		}
	}
	return n
}

// ReplaceTargetIP 内存版：alias 不变换 IP
// ReplaceTargetIP 内存版
func (m *MemStore) ReplaceTargetIP(oldIP, newIP string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for alias, t := range m.targets {
		if t.IP == newIP && alias != oldIP {
			return 0, fmt.Errorf("%w: 新 IP %s 已被 alias=%s 占用", ErrIPInUse, newIP, alias)
		}
	}
	var n int64
	for alias, t := range m.targets {
		if t.IP == oldIP {
			t.IP = newIP
			t.Updated = time.Now().Format("2006-01-02 15:04")
			m.targets[alias] = t
			n++
		}
	}
	return n, nil
}

// RenameTargetAlias 内存版：alias 一并换成新 IP（"不继承历史"），维度保留。
func (m *MemStore) RenameTargetAlias(oldIP, newIP string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for alias, t := range m.targets {
		if t.IP == newIP || alias == newIP {
			return 0, fmt.Errorf("%w: 新 IP %s 已被 alias=%s 占用", ErrIPInUse, newIP, alias)
		}
	}
	for alias, t := range m.targets {
		if t.IP == oldIP {
			delete(m.targets, alias)
			t.Alias = newIP
			t.IP = newIP
			t.Updated = time.Now().Format("2006-01-02 15:04")
			m.targets[newIP] = t
			return 1, nil
		}
	}
	return 0, nil
}

// TargetIPHistory 内存版
func (m *MemStore) TargetIPHistory(ip string, hours int) []map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour)
	var out []map[string]interface{}
	for _, r := range m.latest {
		if r["target"].(string) != ip {
			continue
		}
		lt, okLT := rowTime(r)
		if !okLT || lt.Before(cutoff) {
			continue
		}
		out = append(out, map[string]interface{}{
			"time": lt.Format("2006-01-02 15:04:05"), "avgdelay": r["avgdelay"],
			"sendpk": r["sendpk"], "losspk": r["losspk"],
		})
	}
	return out
}

// NodeCompare 内存版：按 node_id 分桶（1 分钟）
func (m *MemStore) NodeCompare(target string, hours int) []NodeCompareRow {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour)
	buckets := map[string]map[int64][]float64{}
	for _, r := range m.latest {
		if r["target"].(string) != target || r["avgdelay"].(float64) <= 0 {
			continue
		}
		lt, okLT := rowTime(r)
		if !okLT || lt.Before(cutoff) {
			continue
		}
		nid := r["node_id"].(string)
		b := lt.Unix() / 60 * 60
		if buckets[nid] == nil {
			buckets[nid] = map[int64][]float64{}
		}
		buckets[nid][b] = append(buckets[nid][b], r["avgdelay"].(float64))
	}
	names := make([]string, 0, len(buckets))
	for n := range buckets {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]NodeCompareRow, 0, len(names))
	for _, n := range names {
		row := NodeCompareRow{Name: n, Points: []NodeComparePoint{}}
		for b, ds := range buckets[n] {
			sum := 0.0
			for _, d := range ds {
				sum += d
			}
			row.Points = append(row.Points, NodeComparePoint{T: b, D: round2(sum / float64(len(ds)))})
		}
		sort.Slice(row.Points, func(i, j int) bool { return row.Points[i].T < row.Points[j].T })
		out = append(out, row)
	}
	return out
}
