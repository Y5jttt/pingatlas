# PingAtlas

**多节点全国延迟监控**：自建 ICMP 探测点 → WebSocket 实时汇聚 → TimescaleDB 存储 → 地图 / 矩阵看板。

<p>
<img alt="Go" src="https://img.shields.io/badge/Go-1.23-00ADD8?logo=go">
<img alt="PostgreSQL" src="https://img.shields.io/badge/PostgreSQL-16%20%2B%20TimescaleDB-4169E1?logo=postgresql">
<img alt="Vue" src="https://img.shields.io/badge/Vue-3-4FC08D?logo=vuedotjs">
<img alt="License" src="https://img.shields.io/badge/License-MIT-green">
</p>

[功能](#功能) · [架构](#架构) · [快速上手](#快速上手) · [配置](#配置) · [部署](#部署) · [备份与恢复](#备份与恢复) · [常见问题](#常见问题) · [许可与出处](#许可与出处)

---

## 这是什么

一台机器探测全国目标，只能说明"从那一台看过去"的网络质量。PingAtlas 让你**部署任意多台探测节点**，
对同一批目标并行发 ICMP，把结果按 **(目标 × 节点)** 矩阵聚合起来，做出三件在单点监控里做不到的事：

1. **横向对比**：同一个目标，北京、厦门、十堰谁的延迟更好、谁在丢包，一眼看出；
2. **单点劣化不会被平均掩盖**：曲线**不做任何合并/平均**，每个探测点一条独立的线；
3. **视角自由切换**：可以看"全网最优"，也可以切到某一台节点，**整页数据（含地图、汇总、明细）都按那台重算**。

适合自建监控、IDC/线路商做多线路质量对比、以及对"某个省某个运营商到我的服务到底快不快"这类问题有疑问的人。

## 界面

**总览**：左侧按省份着色的延迟地图，右侧「区域 / 运营商」汇总（全部节点 / 电信 / 联通 / 移动 / 各大区），底部是目标与探测点的实时统计。

![总览](docs/images/01-overview.png)

**悬停 / 固定省份明细**：鼠标停在某省，右侧卡片展开该省逐条明细；点击即可**固定**住，方便慢慢看、滚动、复制。

![省份明细](docs/images/02-province-pinned.png)

**单目标延迟曲线**：一个目标一张图，每个探测点各一条线（另配丢包副轴），**不做任何合并或平均** —— 单点劣化不会被掩盖；可切换时间窗口与运营商。

![延迟曲线](docs/images/03-target-latency.png)


## 功能

**看板**
- 地图：按省份着色的延迟分布、超时/丢包标记；**鼠标悬停省份 → 右侧「区域/运营商」卡片展开该省逐条明细**（点击可**固定**住，方便慢慢看/滚动/复制）
- 汇总表：全部节点 / 电信 / 联通 / 移动 / 各大区，含**最快 · 最慢 · 平均**与占比条
- 统计条：平均延迟、丢包、最快、最慢（**最快/最慢会明确标出是哪台节点、哪个地点**）
- 结果表：**按目标**（一行一个 IP，各节点一列）与**按监测点**（一行一个探测点）两种视图；支持排序、只看差异、只看异常、每页行数可选（默认 50）、页码跳转、CSV 导出
- 每目标历史曲线：一个目标一张图，各探测点各一条线，带丢包副轴；时间窗口 1h / 6h / 12h / 24h / 3d / 7d

**探测与身份**
- 节点数量不限，中心只保存每台节点的**公钥**
- 每台节点一对 **Ed25519** 密钥做签名鉴权，**没有可共享的全局密钥**（`Token` 留空即不启用）
- 一键安装：面板生成**一次性安装码** → 在目标机器上执行安装脚本即可接入
- 在线换版：中心可向节点推送自升级
- 节点离线时用本地 SQLite 缓冲，恢复后补传

**运维**
- 中心日志自带自我观测（数据体积、行数、压缩状态）
- 备份 / 恢复脚本 + 保留策略 + systemd 定时器（[deploy/](deploy/)）
- 前端是**纯静态文件**（Vue 3 全局构建 + ECharts），改完不需要任何前端构建步骤，重新编译中心即可（`//go:embed frontend`）

## 架构

```mermaid
flowchart LR
  subgraph NODES["探测节点 × N（只需要出网）"]
    A["pingatlas-node<br/>ICMP 探测 · 本地 SQLite 缓冲"]
  end
  subgraph CENTER["中心 pingatlas-center（只监听 127.0.0.1:18991）"]
    B1["Ed25519 签名鉴权"]
    B2["目标下发 · 数据汇聚"]
    B3["矩阵 / 地图 / 管理 API"]
    B4["内嵌前端 go:embed"]
  end
  DB[("PostgreSQL 16 + TimescaleDB<br/>pinglog(hypertable) · target_alias · node · meta")]
  NG["nginx / Caddy（TLS 反向代理）"]
  U["浏览器看板"]

  A -- "WSS /agent/ws（挑战-应答握手）" --> B1
  A -. "HTTP 回退 /agent/report" .-> B2
  B1 --> B3
  B2 --> DB
  B3 --> DB
  B4 --> NG
  NG --> U
```

### 一次接入的完整过程

```mermaid
sequenceDiagram
  participant U as 管理员（面板）
  participant C as 中心
  participant N as 新节点

  U->>C: 「添加节点」→ 生成一次性安装码（30 分钟有效、用一次即废）
  U->>N: 在目标机器执行安装脚本（带安装码）
  N->>C: POST /agent/redeem（安装码 + 本机公钥）
  C-->>N: 兑换成功并登记公钥 → 之后只靠签名鉴权，无共享密钥
  N->>C: GET /agent/ws
  C-->>N: challenge(nonce)
  N->>C: auth{name, alg=ed25519, cred=sign(name|nonce)}
  C-->>N: 鉴权通过 + 下发目标列表
  loop 每分钟一轮
    N->>C: 上报 (目标 × 节点) 探测结果
  end
```

- **中心只监听 `127.0.0.1:18991`**（安全默认），对外由 nginx/Caddy 反向代理并加 TLS
- 节点只需要**出网**，不需要被访问；鉴权走 `X-PingAtlas-Auth` 请求头（不再把口令放进 URL）
- 表由中心启动时自动建/迁移；`pinglog` 是 TimescaleDB hypertable，便于日后加压缩与保留策略
- 数据规模参考：几百个目标 × 数台节点时，每天约百万行量级

## 快速上手

**前置**：Go 1.23+、PostgreSQL 16 + TimescaleDB（中心）；Linux（节点，需要 `CAP_NET_RAW`）

```bash
# 1) 建库（表结构由中心自动创建/迁移）
sudo -u postgres psql -c "create database pingatlas;"
sudo -u postgres psql -d pingatlas -c "create extension if not exists timescaledb;"

# 2) 写配置 center.json（放在运行目录即可）
cat > center.json <<'EOF'
{
  "DB": "postgres://pingatlas:你的数据库口令@127.0.0.1:5432/pingatlas",
  "AdminPwd": "自定义管理面板密码",
  "Token": ""
}
EOF

# 3) 编译并运行中心
go build -tags center -trimpath -ldflags '-s -w' -o pingatlas-center .
./pingatlas-center --conf center.json
#   日志出现「已连接数据库」「监听 127.0.0.1:18991」即成功

# 4) 打开看板
#    首页      http://127.0.0.1:18991/
#    管理面板  http://127.0.0.1:18991/admin/     （用上面 AdminPwd 登录）

# 5) 编译并安装一台探测节点
go build -tags node -trimpath -ldflags '-s -w' -o pingatlas-node .
#    然后在管理面板「添加节点」→ 拿到一次性安装码 → 在目标机器上按提示执行安装脚本（也可用 /agent/install.sh）
```

跑测试：

```bash
go test -tags center -count=1 ./...
go test -tags node   -count=1 ./...
```

## 配置

### 中心 `center.json`

| 字段 | 说明 |
| --- | --- |
| `DB` | PostgreSQL 连接串（含 TimescaleDB 的库），例如 `postgres://pingatlas:口令@127.0.0.1:5432/pingatlas` |
| `AdminPwd` | 管理面板与 `/api/admin/*` 的密码（请求头 `X-Admin-Pwd`，**不进 URL、不进 nginx access log**） |
| `Token` | 全局共享令牌。**留空字符串 = 明确禁用**（推荐：全部用节点 Ed25519 身份）；键不存在则自动生成一个 |

> 其它字段见 [conf_center.go](conf_center.go)。**`center.json` 含数据库口令，权限请设 `600`，且不要提交进仓库**（`.gitignore` 已忽略）。

### 节点

节点的配置由安装流程生成，并在运行中**热加载**中心下发的目标列表。要点：

- **私钥**：`/etc/pingatlas/node.key`（`600`）——**这是节点的身份**，换机/重装时保留它就能保持同一身份；丢了可用面板的「重置密钥」让节点重新登记
- **本地缓冲**：SQLite（路径见节点 `config.json`），中心不可达时暂存数据
- **目标**：由中心下发，节点不需要手写目标列表

## 部署

- 面向真实服务器的分步部署（systemd 单元、权限、反代、日志自查）：[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)
- 典型的 systemd 单元长这样（节点，注意 raw socket 能力与内存上限）：

```ini
[Service]
ExecStart=/opt/pingatlas/pingatlas-node --conf /etc/pingatlas/config.json
Restart=always
AmbientCapabilities=CAP_NET_RAW CAP_NET_ADMIN
CapabilityBoundingSet=CAP_NET_RAW CAP_NET_ADMIN
NoNewPrivileges=true
MemoryMax=300M
```

## 备份与恢复

```bash
sudo deploy/backup.sh                # 完整备份：pg_dump -Fc + center.json + 本机节点私钥 + MANIFEST
sudo deploy/backup.sh --config-only  # 只备份配置类表（不含 pinglog 明细，体积小很多）

sudo deploy/restore.sh backups/pingatlas-backup-YYYYmmdd-HHMMSS.tar.gz --drop-scratch
#   默认恢复到【临时库】并逐表比对行数（绝不动生产）；加 --prod --yes 才会覆盖生产
```

- 归档含数据库口令与节点私钥 → 目录 `700`、归档 `600`，并生成 `.sha256` 校验
- 保留策略：普通备份 14 天；周日备份额外保留 8 周（可用 `PINGATLAS_KEEP_DAYS` / `PINGATLAS_KEEP_WEEKS` 调整）
- systemd 定时器与 cron 两种写法、以及"如何单独备份其它节点的私钥"，见 [deploy/README.md](deploy/README.md)

## 常见问题

**为什么中心只监听 127.0.0.1？**
安全默认。对外请用 nginx/Caddy 反代并加 TLS，不要把中心直接暴露到公网。

**节点为什么要 root / CAP_NET_RAW？**
ICMP 需要原始套接字。systemd 单元里用 `AmbientCapabilities=CAP_NET_RAW` 授予即可，不必给完整 root 权限。

**节点密钥丢了会怎样？**
监控不会中断（节点用原私钥继续连）。但重新安装/换机时没有它就换不回同一身份：保留 `node.key` 即可；确实丢了就在面板里「重置密钥」。

**怎么让某台节点不在前台显示？**
面板里取消该节点的「前台展示」。探测、上报、入库都继续，只是不出现在首页的节点列表/表格列/图表/下拉里。

**数据涨得太快怎么办？**
`pinglog` 是 TimescaleDB hypertable，可以自行加压缩与保留策略；备份时用 `--config-only` 只备配置。

**地图数据的合规问题？**
中国地图数据由第三方文件提供（`frontend/china.js`），其许可与「审图号」要求见 [THIRD-PARTY.md](THIRD-PARTY.md)。**对外公开部署前请自行确认合规性，或替换为符合规定的标准地图数据。**

## 目录结构

```
main_center.go / main_node.go   两个入口（build tag: center / node）
center.go                       中心：路由、内嵌前端、节点管理调度
ws_hub.go / ws_node.go          中心侧与节点侧的 WebSocket 长连接与握手
auth_center.go / node_key.go    Ed25519 鉴权与节点密钥
reporter.go / engine.go         节点：探测引擎与上报
matrix.go                       矩阵 / 地图 / 汇总 API
admin_api.go                    管理面板 API（节点增删改、密钥重置、在线升级等）
center_db.go / node_store.go    存储层（PostgreSQL + TimescaleDB / 节点本地缓冲）
conf_center.go / conf_node.go   两端配置
frontend/                       内嵌前端（纯静态：Vue 3 + ECharts + 地图）
deploy/                         备份 / 恢复脚本与 systemd 单元
docs/                           部署手册等文档
```

## 计划中

- **告警通知**：阈值 + 持续时间 + 多通道（企业微信/钉钉/邮件/Webhook）+ 静默去重与恢复通知
- **任意时间段查询**与**同目标多节点曲线叠加对比**
- **可用率 / SLA 报表**（日/周/月，可导出）
- 探测能力扩展：TCP / HTTP(S) 探针、抖动（jitter）、连续丢包段

## 许可与出处

- 本项目以 **MIT** 许可发布，见 [LICENSE](LICENSE)
- 随仓库分发的第三方组件（Vue / ECharts / 地图数据 / 后台面板产物）与 Go 依赖、以及**中国地图数据的合规提示**，见 [THIRD-PARTY.md](THIRD-PARTY.md)
- 接口形态与页面结构参考了开源项目 **smartping / smartping-admin**，出处声明见 THIRD-PARTY.md
