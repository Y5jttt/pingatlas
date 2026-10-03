#!/usr/bin/env bash
# pingatlas 备份：PostgreSQL 全库（pg_dump -Fc）+ center.json + 本机节点私钥（若存在）
#
# 用法：
#   backup.sh                 完整备份（含探测明细表数据）
#   backup.sh --config-only   只备份配置类表（不含 pinglog 明细，体积小很多）
#
# 可用环境变量覆盖：PINGATLAS_DIR / PINGATLAS_BACKUP_DIR / PINGATLAS_KEEP_DAYS / PINGATLAS_KEEP_WEEKS
set -Eeuo pipefail

APP_DIR="${PINGATLAS_DIR:-/opt/pingatlas}"
DEST="${PINGATLAS_BACKUP_DIR:-$APP_DIR/backups}"
KEEP_DAYS="${PINGATLAS_KEEP_DAYS:-14}"      # 普通备份保留天数
KEEP_WEEKS="${PINGATLAS_KEEP_WEEKS:-8}"     # 周日备份额外保留周数
LOG="$DEST/backup.log"

CONFIG_ONLY=0
[[ "${1:-}" == "--config-only" ]] && CONFIG_ONLY=1

mkdir -p "$DEST"
chmod 700 "$DEST"
log() { printf '%s %s\n' "$(date '+%F %T')" "$*" | tee -a "$LOG"; }
die() { log "错误: $*"; exit 1; }

STAMP="$(date +%Y%m%d-%H%M%S)"
DOW="$(date +%u)"                      # 7 = 周日
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

log "=== 备份开始 $STAMP（config_only=$CONFIG_ONLY）==="

# 1) 数据库连接串：与中心用同一份配置，避免口令写进脚本
[[ -f "$APP_DIR/center.json" ]] || die "找不到 $APP_DIR/center.json"
PGURL="$(python3 - "$APP_DIR/center.json" <<'PY'
import json, sys
print(json.load(open(sys.argv[1])).get("DB", ""))
PY
)"
[[ -n "$PGURL" ]] || die "center.json 里的 DB 字段为空（当前可能是内存模式，没有可备份的库）"
case "$PGURL" in *pingatlas*|*postgres*) ;; *) log "警告: DB 不是常见形态，仍按原样使用" ;; esac

# 2) 库体积与关键行数，写进 MANIFEST 便于日后核对
q() { psql "$PGURL" -tAc "$1" 2>/dev/null || echo '-'; }
DBNAME="$(q 'select current_database()')"
DBSIZE="$(q 'select pg_size_pretty(pg_database_size(current_database()))')"
ROWS_TARGET="$(q 'select count(*) from target_alias')"
ROWS_NODE="$(q 'select count(*) from node')"
ROWS_PING="$(q 'select count(*) from pinglog')"
log "库=$DBNAME 体积=$DBSIZE 目标=$ROWS_TARGET 节点=$ROWS_NODE 明细行=$ROWS_PING"

# 3) 导出
DUMP="$WORK/db.dump"
if [[ $CONFIG_ONLY -eq 1 ]]; then
  pg_dump "$PGURL" -Fc --exclude-table-data=pinglog -f "$DUMP" || die "pg_dump 失败"
else
  pg_dump "$PGURL" -Fc -f "$DUMP" || die "pg_dump 失败"
fi
log "已导出 $(du -h "$DUMP" | cut -f1)（$DUMP）"

# 4) 配置文件与节点私钥
cp -p "$APP_DIR/center.json" "$WORK/center.json"
[[ -f /etc/pingatlas/node.key ]] && cp -p /etc/pingatlas/node.key "$WORK/node.key" && log "已含本机节点私钥"

# 5) 清单
SP2VER="$(sha256sum "$APP_DIR/pingatlas-center" 2>/dev/null | cut -c1-16)"   # 用二进制 sha256 前 16 位标识版本
cat > "$WORK/MANIFEST.txt" <<EOF
备份时间: $(date '+%F %T %z')
主机: $(hostname)
中心二进制: sha256:$SP2VER
模式: $([[ $CONFIG_ONLY -eq 1 ]] && echo 仅配置（不含 pinglog 明细） || echo 完整)
数据库: $DBNAME  体积: $DBSIZE
关键行数: target_alias=$ROWS_TARGET node=$ROWS_NODE pinglog=$ROWS_PING
包含: db.dump center.json $([[ -f /etc/pingatlas/node.key ]] && echo node.key)
恢复: deploy/restore.sh <本归档>   （默认恢复到临时库，不动生产）
EOF

# 6) 打包（600 权限：里面有口令与私钥）
ARCHIVE="$DEST/pingatlas-backup-$STAMP.tar.gz"
tar -czf "$ARCHIVE" -C "$WORK" . || die "打包失败"
chmod 600 "$ARCHIVE"
( cd "$DEST" && sha256sum "$(basename "$ARCHIVE")" > "$(basename "$ARCHIVE").sha256" )
log "已生成 $(basename "$ARCHIVE") $(du -h "$ARCHIVE" | cut -f1)"

# 7) 保留策略：周日备份按"周"保留，其余按"天"保留
now=$(date +%s)
pruned=0
while IFS= read -r f; do
  [[ -n "$f" ]] || continue
  mtime=$(stat -c %Y "$f")
  age_days=$(( (now - mtime) / 86400 ))
  dow=$(date -d "@$mtime" +%u)                 # 备份当天是周几（7=周日）
  keep=$KEEP_DAYS
  [[ "$dow" == "7" ]] && keep=$((KEEP_WEEKS * 7))
  if (( age_days > keep )); then
    rm -f "$f" "$f.sha256"
    log "清理 $(basename "$f")（${age_days} 天前，阈值 ${keep} 天）"
    pruned=$((pruned + 1))
  fi
done < <(find "$DEST" -maxdepth 1 -name 'pingatlas-backup-*.tar.gz' | sort)
[[ $pruned -eq 0 ]] && log "无需清理"
log "当前保留 $(find "$DEST" -maxdepth 1 -name 'pingatlas-backup-*.tar.gz' | wc -l) 份，占用 $(du -sh "$DEST" | cut -f1)"
# 日志自截断，避免无限增长
tail -n 500 "$LOG" > "$LOG.tmp" && mv "$LOG.tmp" "$LOG"
