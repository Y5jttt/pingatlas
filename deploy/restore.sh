#!/usr/bin/env bash
# pingatlas 恢复：默认恢复到【临时库】做校验，绝不动生产库；只有显式 --prod 才写生产。
#
# 用法：
#   restore.sh <归档.tar.gz>                          # 恢复到临时库 sp2_restore_<时间戳>
#   restore.sh <归档.tar.gz> --db 指定库名            # 恢复到指定库（新建/覆盖该库）
#   restore.sh <归档.tar.gz> --prod --yes            # 恢复到生产库（危险，需双确认）
#   restore.sh <归档.tar.gz> --drop-scratch           # 校验完顺手删掉临时库
set -Eeuo pipefail

APP_DIR="${PINGATLAS_DIR:-/opt/pingatlas}"
ARCHIVE="${1:-}"
[[ -n "$ARCHIVE" && -f "$ARCHIVE" ]] || { echo "用法: restore.sh <归档.tar.gz> [--db 名字] [--prod --yes] [--drop-scratch]"; exit 2; }
shift
TARGET_DB=""; PROD=0; ASSUME_YES=0; DROP_SCRATCH=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --db) TARGET_DB="${2:-}"; shift 2 ;;
    --prod) PROD=1; shift ;;
    --yes) ASSUME_YES=1; shift ;;
    --drop-scratch) DROP_SCRATCH=1; shift ;;
    *) echo "未知参数: $1"; exit 2 ;;
  esac
done

log() { printf '%s %s\n' "$(date '+%F %T')" "$*"; }
die() { log "错误: $*"; exit 1; }

# 1) 校验完整性
if [[ -f "$ARCHIVE.sha256" ]]; then
  ( cd "$(dirname "$ARCHIVE")" && sha256sum -c "$(basename "$ARCHIVE").sha256" ) || die "校验和不匹配，归档可能损坏"
  log "校验和 OK"
else
  log "警告: 没有 .sha256，跳过校验"
fi

WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT
tar -xzf "$ARCHIVE" -C "$WORK" || die "解包失败"
[[ -f "$WORK/db.dump" ]] || die "归档里没有 db.dump"
[[ -f "$WORK/MANIFEST.txt" ]] && { log "归档清单："; sed 's/^/    /' "$WORK/MANIFEST.txt"; }

# 2) 连接串：优先用归档里的 center.json（与备份时一致），否则用现场的
CONF="$WORK/center.json"; [[ -f "$CONF" ]] || CONF="$APP_DIR/center.json"
PGURL="$(python3 - "$CONF" <<'PY'
import json, sys
print(json.load(open(sys.argv[1])).get("DB", ""))
PY
)"
[[ -n "$PGURL" ]] || die "取不到数据库连接串"

if [[ $PROD -eq 1 ]]; then
  [[ $ASSUME_YES -eq 1 ]] || die "恢复生产库需要同时给 --prod --yes"
  TARGET_DB="${TARGET_DB:-$(python3 -c 'import sys,urllib.parse as u;print(u.urlparse(sys.argv[1]).path.lstrip("/"))' "$PGURL")}"
  log "!! 即将覆盖生产库 $TARGET_DB（5 秒后开始，Ctrl-C 可中止）"
  sleep 5
else
  TARGET_DB="${TARGET_DB:-sp2_restore_$(date +%m%d%H%M)}"
  log "恢复到临时库 $TARGET_DB（不动生产）"
fi

APP_USER="$(python3 -c 'import sys,urllib.parse as u;print(u.urlparse(sys.argv[1]).username or "")' "$PGURL")"
TARGET_URL="$(python3 - "$PGURL" "$TARGET_DB" <<'PY'
import sys, urllib.parse as u
p = u.urlparse(sys.argv[1])
print(u.urlunparse(p._replace(path="/" + sys.argv[2])))
PY
)"
ADMIN_URL="$(python3 - "$PGURL" <<'PY'
import sys, urllib.parse as u
p = u.urlparse(sys.argv[1]); print(u.urlunparse(p._replace(path="/postgres")))
PY
)"

# 3) 建库 + 恢复
if psql "$ADMIN_URL" -tAc "select 1 from pg_database where datname='$TARGET_DB'" | grep -q 1; then
  log "库 $TARGET_DB 已存在，先清空其中的对象"
  psql "$TARGET_URL" -q -c 'drop schema public cascade; create schema public;' || die "清空失败"
else
  # 应用账号一般没有 CREATEDB 权限：先自己试，失败就借本机 postgres 超级用户
  if createdb -T template0 "$TARGET_URL" 2>/dev/null; then
    log "已创建库 $TARGET_DB"
  elif sudo -n -u postgres createdb -T template0 -O "$APP_USER" "$TARGET_DB" 2>/dev/null; then
    log "已用 postgres 超级用户建库 $TARGET_DB（owner=$APP_USER）"
  else
    die "建库失败：应用账号无 CREATEDB 权限，sudo -u postgres 也不可用"
  fi
  # 预建扩展：CREATE EXTENSION 需要超级用户，先建好再 restore，避免 hypertable 建不起来
  sudo -n -u postgres psql -q -d "$TARGET_DB" -c 'create extension if not exists timescaledb;' 2>/dev/null \
    && log "已预建 timescaledb 扩展" || log "（timescaledb 扩展已存在或无需预建）"
fi
# TimescaleDB 官方建议：restore 前后各调一次（需要超级用户）
sudo -n -u postgres psql -q -d "$TARGET_DB" -c 'select timescaledb_pre_restore();' >/dev/null 2>&1 \
  && log "已执行 timescaledb_pre_restore()" || log "（timescaledb 不可用，跳过 pre_restore）"

log "开始 pg_restore（hypertable 的 chunk 索引会重建，稍慢）"
# 必须以超级用户执行：TimescaleDB 的目录表（metadata/dimension/chunk_column_stats…）只有超级用户能写
chmod 711 "$WORK"; chmod 644 "$WORK/db.dump"
set +e
sudo -n -u postgres pg_restore --no-owner --no-privileges -d "$TARGET_DB" "$WORK/db.dump" 2> "$WORK/restore.err"
RC=$?
set -e
chmod 600 "$WORK/db.dump"
# 只容忍 TimescaleDB 内部对象（chunk/连续聚合）的报错，其它错误一律致命
REAL="$(grep -i 'error' "$WORK/restore.err" | grep -viE '_timescaledb_internal|continuous_agg|timescaledb' || true)"
if [[ -n "$REAL" ]]; then
  echo "$REAL" | sed 's/^/    /' | tail -15
  die "pg_restore 出现非 TimescaleDB 内部对象的错误"
fi
log "pg_restore 结束（退出码 $RC，仅 TimescaleDB 内部对象告警 $(grep -ci error "$WORK/restore.err" || echo 0) 条）"

sudo -n -u postgres psql -q -d "$TARGET_DB" -c 'select timescaledb_post_restore();' >/dev/null 2>&1 \
  && log "已执行 timescaledb_post_restore()" || log "（跳过 post_restore）"

# 3.5) 授权给应用账号：pg_restore 以超级用户执行 + --no-owner，对象会属 postgres，
#      不授权的话应用连不上（TimescaleDB 的 chunk 在内部 schema 里，一并授权）
APP_DB="$(python3 -c 'import sys,urllib.parse as u;print(u.urlparse(sys.argv[1]).path.lstrip("/"))' "$PGURL")"
sudo -n -u postgres psql -q -d "$TARGET_DB" <<SQL || log "（授权有告警，继续校验）"
do \$\$ begin
  execute format('grant all on database %I to %I', '$TARGET_DB', '$APP_USER');
  execute format('alter database %I owner to %I', '$TARGET_DB', '$APP_USER');
end \$\$;
grant all on schema public to "$APP_USER";
grant all on all tables in schema public to "$APP_USER";
grant all on all sequences in schema public to "$APP_USER";
grant usage on schema _timescaledb_internal, _timescaledb_catalog, _timescaledb_config,
      _timescaledb_cache, timescaledb_information, timescaledb_experimental to "$APP_USER";
grant all on all tables in schema _timescaledb_internal to "$APP_USER";
grant all on all sequences in schema _timescaledb_internal to "$APP_USER";
SQL
log "已把 $TARGET_DB 的权限授予 $APP_USER"

# 4) 校验：静态表精确比对；明细表与归档快照比对（生产仍在入库）
SRC_URL="$PGURL"
cmp_table() {
  local t="$1"
  local a b
  a="$(psql "$SRC_URL" -tAc "select count(*) from $t" 2>/dev/null || echo '-')"
  b="$(psql "$TARGET_URL" -tAc "select count(*) from $t" 2>/dev/null || echo '-')"
  if [[ "$a" == "$b" && "$a" != "-" ]]; then log "  OK  $t: $a"; else log "  差异 $t: 生产=$a 恢复=$b"; fi
}
log "行数校验："
for tb in target_alias node; do cmp_table "$tb"; done
if grep -q '模式: 完整' "$WORK/MANIFEST.txt" 2>/dev/null; then
  # pinglog 与"归档时的快照值"比（生产还在持续入库，拿现在的值比必然不等）
  SNAP="$(sed -n 's/.*pinglog=\([0-9]*\).*/\1/p' "$WORK/MANIFEST.txt" | head -1)"
  GOT="$(psql "$TARGET_URL" -tAc 'select count(*) from pinglog' 2>/dev/null || echo '?')"
  NOW="$(psql "$SRC_URL" -tAc 'select count(*) from pinglog' 2>/dev/null || echo '?')"
  if [[ -n "$SNAP" && "$GOT" == "$SNAP" ]]; then
    log "  OK  pinglog: 恢复=$GOT = 归档快照=$SNAP（生产此刻已是 $NOW，符合预期）"
  else
    log "  差异 pinglog: 恢复=$GOT 归档快照=${SNAP:-?} 生产此刻=$NOW"
  fi
else
  log "  （仅配置备份，不含 pinglog 明细）"
fi

if [[ $DROP_SCRATCH -eq 1 && $PROD -eq 0 ]]; then
  if psql "$ADMIN_URL" -q -c "drop database \"$TARGET_DB\"" 2>/dev/null || sudo -n -u postgres dropdb --if-exists "$TARGET_DB"; then
    log "已删除临时库 $TARGET_DB"
  else
    log "警告: 临时库 $TARGET_DB 删除失败，请手动 dropdb"
  fi
else
  [[ $PROD -eq 0 ]] && log "临时库保留：$TARGET_DB（校验完可执行 dropdb 或加 --drop-scratch）"
fi
log "=== 恢复流程结束 ==="
