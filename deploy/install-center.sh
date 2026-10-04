#!/usr/bin/env bash
# PingAtlas 中心端 · 一键安装 / 升级
#
#   curl -fsSL https://raw.githubusercontent.com/Y5jttt/pingatlas/main/deploy/install-center.sh | sudo bash
#
# 可选参数（也可用同名环境变量）：
#   --version v0.3.1     指定版本（默认 latest release）
#   --dir /opt/pingatlas 安装目录
#   --port 18991         监听端口（只监听 127.0.0.1）
#   --db-name pingatlas  数据库名
#   --db-user pingatlas  数据库用户
#   --db-pass xxx        数据库口令（默认随机生成）
#   --admin-pwd xxx      管理面板密码（默认随机生成）
#   --base-url <url>     从镜像下载（形如 https://<镜像>/<owner>/<repo>），用于 github.com 下载受限的网络
#   --binary <文件>      用本地已有的二进制安装（完全离线，跳过校验；请自行确认来源可信）
#   --no-systemd         只装二进制与配置，不创建 systemd 单元
#   --dry-run            只打印将要做什么，不实际改动
#
# 重复执行 = 升级：会替换二进制，但**不会覆盖已有的 center.json**。
set -Eeuo pipefail

SLUG="Y5jttt/pingatlas"
VERSION="latest"
DIR="/opt/pingatlas"
PORT="18991"
DB_NAME="pingatlas"
DB_USER="pingatlas"
DB_PASS=""
ADMIN_PWD=""
USE_SYSTEMD=1
DRY=0
BASE_URL_OVERRIDE=""
LOCAL_BIN=""
PGHOST="127.0.0.1"
PGPORT="5432"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) VERSION="${2:?}"; shift 2 ;;
    --dir)     DIR="${2:?}";     shift 2 ;;
    --port)    PORT="${2:?}";    shift 2 ;;
    --db-name) DB_NAME="${2:?}"; shift 2 ;;
    --db-user) DB_USER="${2:?}"; shift 2 ;;
    --db-pass) DB_PASS="${2:?}"; shift 2 ;;
    --admin-pwd) ADMIN_PWD="${2:?}"; shift 2 ;;
    --base-url) BASE_URL_OVERRIDE="${2:?}"; shift 2 ;;
    --binary)   LOCAL_BIN="${2:?}";       shift 2 ;;
    --no-systemd) USE_SYSTEMD=0; shift ;;
    --dry-run) DRY=1; shift ;;
    -h|--help) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "未知参数: $1（用 --help 看用法）" >&2; exit 2 ;;
  esac
done

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[!]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[✗]\033[0m %s\n' "$*" >&2; exit 1; }
run()  { if [[ $DRY -eq 1 ]]; then echo "    [dry-run] $*"; else "$@"; fi; }
rand() { openssl rand -hex 12 2>/dev/null || head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'; }

[[ $DRY -eq 1 || $EUID -eq 0 ]] || die "请用 root 运行（sudo bash $0 …）"

# ---------- 环境识别 ----------
case "$(uname -s)" in Linux) ;; *) die "目前只支持 Linux（macOS/Windows 请用源码安装）" ;; esac
case "$(uname -m)" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) die "不支持的架构: $(uname -m)" ;;
esac
command -v curl >/dev/null || die "缺少 curl，请先安装（apt install -y curl）"
command -v sha256sum >/dev/null || die "缺少 sha256sum（coreutils）"

log "目标环境: Linux/$ARCH，安装目录 $DIR，端口 $PORT"

# ---------- 解析版本 ----------
if [[ "$VERSION" == "latest" ]]; then
  VERSION="$(curl -fsSL --connect-timeout 10 --max-time 30 "https://api.github.com/repos/$SLUG/releases/latest" \
             | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)"
  [[ -n "$VERSION" ]] || die "取最新版本失败（网络或 GitHub API 限流），可显式指定 --version v0.3.1"
fi
ASSET="pingatlas-center-linux-$ARCH"
BASE="https://github.com/$SLUG/releases/download/$VERSION"
log "将安装版本: $VERSION（$ASSET）"

# ---------- 下载并校验 ----------
[[ -n "$BASE_URL_OVERRIDE" ]] && BASE="${BASE_URL_OVERRIDE%/}/$VERSION"
CURL=(curl -fL --connect-timeout 10 --max-time 300 --retry 2 --retry-delay 2)

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
if [[ -n "$LOCAL_BIN" ]]; then
  [[ -f "$LOCAL_BIN" ]] || die "指定的本地文件不存在: $LOCAL_BIN"
  run install -m 755 "$LOCAL_BIN" "$TMP/$ASSET"
  warn "使用本地文件安装，已跳过 SHA-256 校验（请自行确认文件来源可信）"
else
  log "下载 $ASSET（来源 $BASE）"
  if ! "${CURL[@]}" -o "$TMP/$ASSET" "$BASE/$ASSET"; then
    die "下载失败。常见原因：所在网络访问 github.com 的 release 下载受限。
     可选做法：
       1) 换镜像：  … | sudo bash -s -- --base-url https://<你的镜像>/pingatlas
       2) 本地安装：在能联网的机器上下载 $ASSET，scp 到本机后执行
                    sudo bash $0 --binary /路径/$ASSET
       3) 只是网络抖动：重跑一次即可"
  fi
  if "${CURL[@]}" -o "$TMP/checksums.txt" "$BASE/checksums.txt"; then
    ( cd "$TMP" && grep " $ASSET\$" checksums.txt | sha256sum -c - ) || die "SHA-256 校验失败，已中止（文件可能被篡改或版本不匹配）"
    log "SHA-256 校验通过"
  else
    die "取不到 checksums.txt，无法校验完整性。请用 --base-url 指定能同时提供该文件的镜像，或用 --binary 本地安装"
  fi
fi

# ---------- 数据库 ----------
log "检查 PostgreSQL 与 TimescaleDB"
if [[ $DRY -eq 0 ]]; then
  command -v psql >/dev/null || die "没找到 psql。请先安装 PostgreSQL 16 + TimescaleDB 后重试"
  sudo -u postgres psql -tAc "select 1" >/dev/null 2>&1 || die "无法以 postgres 用户连接数据库（需要本机 PostgreSQL）"
  sudo -u postgres psql -tAc "select 1 from pg_available_extensions where name='timescaledb'" | grep -q 1 \
    || die "PostgreSQL 里没有 timescaledb 扩展。请先安装 TimescaleDB（并在 postgresql.conf 的 shared_preload_libraries 里加载），再重跑本脚本"
fi

[[ -n "$DB_PASS" ]] || DB_PASS="$(rand)"
[[ -n "$ADMIN_PWD" ]] || ADMIN_PWD="$(rand)"

if [[ $DRY -eq 0 ]]; then
  if ! sudo -u postgres psql -tAc "select 1 from pg_roles where rolname='$DB_USER'" | grep -q 1; then
    run sudo -u postgres psql -q -c "create role \"$DB_USER\" login password '$DB_PASS'"
    log "已创建数据库用户 $DB_USER"
  else
    run sudo -u postgres psql -q -c "alter role \"$DB_USER\" with password '$DB_PASS'"
    log "数据库用户 $DB_USER 已存在，已更新口令"
  fi
  if ! sudo -u postgres psql -tAc "select 1 from pg_database where datname='$DB_NAME'" | grep -q 1; then
    run sudo -u postgres createdb -O "$DB_USER" "$DB_NAME"
    log "已创建数据库 $DB_NAME"
  fi
  run sudo -u postgres psql -q -d "$DB_NAME" -c 'create extension if not exists timescaledb'
  log "TimescaleDB 扩展已就绪（表结构由中心首次启动时自动创建）"
fi

# ---------- 安装二进制 ----------
run install -d -m 755 "$DIR"
run install -m 755 "$TMP/$ASSET" "$DIR/pingatlas-center"
log "二进制已安装到 $DIR/pingatlas-center"

# ---------- 配置（已存在则不动） ----------
CONF="$DIR/center.json"
if [[ -f "$CONF" ]]; then
  log "已存在 $CONF，保持不变（升级不会覆盖你的配置）"
else
  if [[ $DRY -eq 1 ]]; then
    echo "    [dry-run] 生成 $CONF（数据库口令与管理密码随机）"
  else
    umask 077
    cat > "$CONF" <<EOF
{
  "DB": "postgres://$DB_USER:$DB_PASS@$PGHOST:$PGPORT/$DB_NAME",
  "AdminPwd": "$ADMIN_PWD",
  "Token": ""
}
EOF
    chmod 600 "$CONF"
    log "已生成 $CONF（权限 600）"
  fi
fi

# ---------- systemd ----------
if [[ $USE_SYSTEMD -eq 1 ]]; then
  UNIT=/etc/systemd/system/pingatlas-center.service
  if [[ $DRY -eq 1 ]]; then
    echo "    [dry-run] 写 $UNIT 并 systemctl enable --now pingatlas-center"
  else
    cat > "$UNIT" <<EOF
[Unit]
Description=PingAtlas center (aggregator + TimescaleDB)
After=network-online.target postgresql.service
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$DIR
ExecStart=$DIR/pingatlas-center --conf center.json
Restart=always
RestartSec=3
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable --now pingatlas-center >/dev/null 2>&1 || warn "systemctl 启动失败，请看 journalctl -u pingatlas-center"
    sleep 2
    systemctl is-active --quiet pingatlas-center \
      && log "中心已启动（systemd: pingatlas-center）" \
      || warn "服务未处于 active，请查 journalctl -u pingatlas-center -n 50"
  fi
fi

# ---------- 收尾提示 ----------
cat <<EOF

────────────────────────────────────────────────────────
 PingAtlas 中心安装完成 ($VERSION)

  面板地址 : http://127.0.0.1:$PORT/admin/
  管理密码 : $( [[ -f "$CONF" && $DRY -eq 0 ]] && sed -n 's/.*"AdminPwd": *"\([^"]*\)".*/\1/p' "$CONF" || echo '（沿用原有 center.json）' )

  ⚠️ 中心只监听 127.0.0.1（安全默认），从外部访问请二选一：
     · 反向代理（推荐）：nginx/Caddy 把 https://你的域名 反代到 127.0.0.1:$PORT
     · 临时隧道：ssh -L $PORT:127.0.0.1:$PORT 用户名@这台机器

  加节点：登录面板 → 添加节点 → 复制一次性安装码 → 在那台机器上按提示执行

  常用命令：
     systemctl status pingatlas-center
     journalctl -u pingatlas-center -n 50
     升级：重跑本脚本即可（不会覆盖 center.json）
────────────────────────────────────────────────────────
EOF
