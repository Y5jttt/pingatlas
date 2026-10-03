# 部署手册（内网运维版）

> 这份文档是项目原先的《部署说明》，保留完整内容（含中心/节点分步部署、鉴权细节、运维自查）。
> 对外总览见仓库根目录的 [README.md](../README.md)。

---

# pingatlas 部署说明

**双二进制**：角色在编译期用 build tag 决定，运行时**不能**切换
（`--role` 参数只为兼容旧命令行保留，会被忽略）。

```bash
# 中心端
GOOS=linux GOARCH=amd64 go build -tags center -o pingatlas-center .
# 节点端（两种架构）
GOOS=linux GOARCH=amd64 go build -tags node  -o pingatlas-node .
GOOS=linux GOARCH=arm64 go build -tags node  -o pingatlas-node-arm64 .
# 测试
go test -tags center -count=1 ./...   # 中心端
go test -tags node   -count=1 ./...   # 节点端
```

`handleDownload` 会从 **center 可执行文件同目录**读取 `pingatlas-node` /
`pingatlas-node-arm64` 发给安装脚本 —— 部署 center 时把这两个文件一起放过去，
否则 `/agent/download?arch=arm64` 会 404。

## Center（中心机，建议干净的 2C2G50G）

1. 装 PostgreSQL（可选 TimescaleDB 扩展，装了自动按天 chunk + 7 天列存压缩，明细永久保留）
2. `center.json`（**权限设为 600**，里面是管理密码等敏感配置；
   代码只在改密码/重新生成 token 时收紧权限，手写文件不会被动收紧）：
   ```json
   {
     "Listen": ":18991",
     "AdminPwd": "换成长度≥6的管理密码（只管管理面板，与节点鉴权完全分离）",
     "Token": "",
     "DB": "postgres://pingatlas:密码@127.0.0.1:5432/pingatlas",
     "TrustedProxies": ["172.16.0.0/12"]
   }
   ```
   - **`Token` 建议显式留空**：留空 = 关闭"全局万能钥匙"，只接受各节点自己的
     Ed25519 签名（或节点自己的独立 token）。不写这个键则会自动生成一个全局 token
     并写回配置 —— 那是给单机快速上手用的兼容模式，多节点部署不要用。
   - `AdminPwd` 为空时**所有** `/api/admin/*` 都返回 401（安全默认）；
     老配置只填了 `Token` 的，启动时会把 `Token` 迁移成 `AdminPwd` 并打印提示。
   - `DB` 留空 = 内存模式（仅测试用，重启清零）。
   - `TrustedProxies`：**只有**来自这些地址的反代请求才会采信 `X-Forwarded-For`
     （回环地址默认可信）。跨机部署 nginx 时必须在这里登记代理 IP，否则按代理 IP 计数限流。
     直连对端不可信时 XFF 被完全忽略 —— 这是防爆破锁定不可绕过的前提。
3. 启动：`./pingatlas-center --conf center.json`
4. 浏览器开 `http://IP:18991/`：节点状态、明细、目标管理（加/改目标即时生效）。
   也可 API：`POST /api/targets  {"alias":"web-prod","ip":"1.2.3.4"}`
   **alias 是历史归属名：同 alias 改 IP 即完成切换，历史不断档；节点 60s 内热加载，无需重启。**
   请求体里**没给**的 `province/city/telecom` 会沿用库里的旧值（不会被清空）；
   但新目标必须带上这三个维度，否则不会出现在全国地图与省级曲线上。
   面板的"编辑"入口只能换 IP，改维度要用这个 API 重新提交一次。
5. 删除节点（重装 / token 泄露 / 节点名用尽）：
   `POST /api/admin/node/del  {"name":"节点名"}`（需 `X-Admin-Pwd`）。
   删除只清注册记录并断开长连接，历史明细保留。若中心还留着全局 `Token`
   （即"万能钥匙"没关），用它的旧节点删掉记录后仍能连上——要彻底作废请把 `Token`
   显式留空并用面板生成的一次性安装码重新装。
   **被删节点会持续重连并握手失败刷日志**，记得在机器上同时停掉并卸载：
   `systemctl disable --now pingatlas-node`（OpenRC：`rc-service pingatlas-node stop && rc-update del pingatlas-node`）。
6. 面板「设置」页里除**修改密码**以外的项（端口/名称/归档/超时/邮件告警）后端不实现，
   保存会显示成功但不会生效 —— 这些由 `center.json` 与 systemd 管理。
7. 接口约定：`/api/province_history.json` 的 `ips[].history[i]` 可能是 **`null`**
   （该时间桶内没有任何有效样本，全是超时）—— 前端应画**断点**而不是 0；
   同一位置的 `loss[i]` 在超时桶里是 100（不是 0）。超时不再参与延迟平均。
8. **首页口径（多节点）**：`/api/mapping.json`、`/api/mapping_detail.json`、`/api/province_history.json`
   都接受 `?node=<节点名>`：
   - 不传 = **全网最优**：每个目标取最新一分钟，多节点并列时取更优值
     （优先非超时，再取更小延迟）；响应带 `scope=best` 与 `nodes=[...]`。
     **节点越多数字越低**是该口径的固有属性，看趋势时要注意。
   - `?node=node-bj-01` = **单节点视角**：只统计该节点，响应带 `scope=node`、`node=<名字>`。
   - 未知节点 → **400**（避免拼错后静默显示成空地图）。
   首页右上角下拉框即切换口径，选择会写进 URL，可直接分享 `https://<域名>/?node=<节点名>`。
9. **首页矩阵接口**：`/api/matrix.json`（目标 × 节点矩阵 + 运营商/地区汇总 + 节点卡片 + 省级聚合）
   供首页三段式布局使用。`?window=5m|15m|1h`（实时：窗口内每个目标-节点取最新一行）、
   `?window=24h|7d`（历史：窗口内有效样本均值）；`?node=` 同上；另有 `province`/`telecom`/
   `q`/`only=diff|timeout`/`sort`/`order`/`limit`/`offset` 过滤（只作用于 `rows`，
   ②③ 汇总面板始终基于全量目标）。
   - 值约定与地图一致：`delay=-1` 无数据、`0` 超时；`spread=-1` 表示节点间不可比较
     （有节点超时/无数据），不是"两台一样快"。
   - **地图（`mapping.json`/`mapping_detail.json`）始终是实时 5 分钟快照**，不跟随 `?window=`；
     切到 24h 均值时只有结果表与汇总面板变化（首页有文字提示）。
10. **节点显示名**优先取数据库里存的 `label`（面板"新增节点"按地区+运营商自动拼好，
   例如 四川/成都 + 腾讯云 → **成都腾讯**），其次才是中心配置 `NodeGeo`（兼容老部署）：
   `"NodeGeo":{"node-bj-01":{"Label":"北京一号"},"node-xm-01":{"Label":"厦门一号"}}`；
   `"SelfNode"` 标记与中心同机的那台，两者都没有时退回节点 id。
   **地图上不标注探测节点位置**，所以不需要经纬度（旧配置里的 `Lng/Lat` 会被忽略）。

## 节点身份与鉴权（Ed25519 签名）

两套凭据**彻底分离**，改一个不影响另一个：

| | 管什么 | 谁生成 | 存在哪 |
|---|---|---|---|
| `AdminPwd` | 管理面板 `/api/admin/*` | 你 | `center.json`（明文，文件 600） |
| 节点身份（Ed25519） | 节点上报通道 `/agent/ws`、`/agent/report` 等 | **节点自己** | 私钥：节点 `/etc/pingatlas/node.key`（600，永不外发）<br>公钥：中心 `node.pubkey`（可公开） |

**握手过程**（私钥与 token 都不上线）：

```
中心 → 节点:  challenge { nonce }                      ← 每次连接都不同，防重放
节点 → 中心:  auth { name, alg:ed25519, cred:签名 }     ← sign(私钥, "name|nonce")
中心:        按【名字】取该名字登记的公钥 → 验签 → 建立长连接
```

要点与运维含义：

- **中心只存公钥**：数据库被拖走、中心磁盘被翻，都拿不到任何能冒充节点的东西。
- **身份绑定在登记那一刻**：新装节点用**一次性安装码**（30 分钟、用掉即废、绑定节点名）
  登记公钥；老节点（已有独立 token）用该 token 走一次认证后自助登记。
  **名字只取鉴权结果，绝不取请求体**，所以 A 的凭据无法给 B 登记公钥。
- **节点侧选路顺序**：连接时**有私钥就先试签名**，失败且还有 token 才退回 HMAC。
  因此清空 token 之后节点依然能上线（含重启），而尚未登记公钥的新节点也能用 token 完成首次登记。
- **公钥指纹**：`SHA-256(公钥)` 前 16 位。节点启动日志里打一次，面板"节点状态"里显示
  （`pubkey_fp` 字段），两边比对即可确认"这台机器确实是登记的那把钥匙"——
  也是发现"有人抢先用安装码登记了别的公钥"的手段。
- **密钥丢失**：节点发现"配置里记录过指纹、但 `node.key` 不在了"会**拒绝启动并报错**，
  绝不静默换一个身份。恢复方式：从备份还原该文件；或确认要换身份时在 `config.json`
  里加 `"KeyRegen": true`（一次性），再到中心重新登记。
- **换钥匙 / 吊销**：`POST /api/admin/node/key/reset {"name":"节点名"}` 清空公钥
  （旧签名立即失效）。之后怎么恢复，取决于该节点还有没有 token：
  **库里有 token** → 节点下次启动会用 token 重新登记公钥（自助恢复）；
  **库里 token 已清空**（推荐终态，见下条）→ 没有可用凭据了，需要人工介入：
  删掉该节点记录后重新生成安装码重装（`node/del` + `node/add`），或把新 token 写进节点配置。
  删掉节点记录等于彻底吊销。
- **兼容通道与"关掉万能钥匙"**：未登记公钥的节点仍可用
  `X-PingAtlas-Auth: name:ts:hmac(token,"name|ts")`（老客户端）；带请求体摘要的签名格式是
  `name:ts:ed25519b:签名`，比老格式多保护了请求体。
  节点侧**有私钥就优先用签名**，签名失败且手上还有 token 时才退回 HMAC —— 所以
  `UPDATE node SET token=''` 不会让已登记公钥的节点失联，重启也能凭私钥自己上线
  （已实测：清空 token 后重启节点仍然 `Ed25519 签名 鉴权通过`）。
  ⚠️ **清空顺序**：务必先把所有节点升到带"签名优先"逻辑的版本（0.3.1+）再清 token，
  否则老节点会一直尝试 HMAC 而连不上。
- **清空 token 的完整流程**（本仓库作者的实际做法）：
  1. 节点全部确认 `auth=ed25519`（面板"节点状态"或 `node/status` 接口）；
  2. `UPDATE node SET token=''`；
  3. 反证：用旧 token 伪造一次 `/agent/report` → 必须 **403**；
  4. 重启一台节点 → 应显示 `Ed25519 签名 鉴权通过`（证明不再依赖 token）。
  新装节点不受影响：`node/add` 仍会给新节点生成它自己的 token，用于首次登记公钥 ✓
- **边界**：签名只证明"这台机器持有该名字登记的私钥"，**不证明**它物理上在某个地区
  （那是地理/硬件证明的事）；中心被完全控制时攻击者仍能改数据。
- **不要把 `node.key` 打进镜像/快照**：克隆机器会让两台共享同一身份，等于回到"共享秘密"。
  节点检测到"密钥与配置记录的指纹不一致"时会报错拒绝启动。
- **兼容通道**：见下一条"兼容通道与关掉万能钥匙"。
- **边界**：签名只证明"这台机器持有该名字登记的私钥"，**不证明**它物理上在某个地区
  （那是地理/硬件证明的事）；中心被完全控制时攻击者仍能改数据。

### 运维自查（建议每天看一眼日志）

- 启动时会打印 `centerdb: 压缩已启用（chunk N 个，已压缩 M 个）` 或
  `!!! 压缩未生效`；**后者意味着明细按原始体积增长（约 10 倍），磁盘会很快写满**。
- center 每 6 小时打印一次 `存储自查 体积=… 行数=… chunk=… 已压缩=…`；
  其中 `体积` 取 `hypertable_size()`，即 chunk 的**真实占用**
  （只看父表 `pg_total_relation_size` 会得到几十 kB 的假数字）；
  若出现 `!!! 最早 chunk 已有 X 但没有任何 chunk 被压缩`，说明 7 天压缩策略没生效。
- 手动核实（在 PG 上直接跑）：
  ```sql
  SELECT extversion FROM pg_extension WHERE extname='timescaledb';
  SELECT * FROM timescaledb_information.compression_settings WHERE hypertable_name='pinglog';
  SELECT is_compressed, count(*) FROM timescaledb_information.chunks
    WHERE hypertable_name='pinglog' GROUP BY 1;
  -- 节点重传超过 7 天的老数据时，这条必须不报错（压缩 chunk 的可写性因版本而异）：
  INSERT INTO pinglog (logtime,target,node_id) VALUES (now()-interval '30 days','probe','probe')
    ON CONFLICT DO NOTHING;
  ```
- 目标表重复 IP 会让 `current_ip` 唯一索引建不上，启动日志会告警并给出排查 SQL。

## Node（每台探测机）

1. 二进制放好，沿用旧 `config.json`（Chinamap 格式不变），只需加一段：
   ```json
   "Center": {"Endpoint": "http://中心IP:18991", "Token": "与center一致"}
   ```
   推荐直接用面板生成的一键安装命令（一次性安装码换独立 token，token 不进 URL/日志）。
2. 启动：`./pingatlas-node --conf config.json`（root，需 RAW socket）
3. 行为：每分钟一轮（每目标 20 包、50ms 间隔、3s 收尾）→ SQLite 缓冲（WAL，24h 兜底）
   → 30s 批量上报 → 失败不动水位自动重推；中心不可达时按最近一次下发的目标独立续测，数据不丢。
4. 注意：center 模式下目标列表以中心 `target_alias` 为准（完全替换 Chinamap）。
   **正式切换前先把现有全量目标录入中心**，避免节点目标列表被清空。
   中心把目标清空后，节点重启会读到持久化的空列表并保持"不探测"（不会退回旧的 Chinamap）。

## 端口/资源

- center 监听 18991（TCP）；node 不监听任何端口
- 明细行 166B/节点/分钟级目标；5 节点×360 目标压缩后 ~22GB/年（**压缩失效时约 10 倍**）
- 需要管理权限的接口：`/api/admin/*`、`/api/nodes`、`/api/history`
  （鉴权头 `X-Admin-Pwd`；也兼容 `?pwd=`，但会在反代日志里留下明文密码，不建议使用）

---

## 开源与许可

- 本项目以 **MIT** 许可发布，见 [`LICENSE`](LICENSE)。
- 随仓库分发的第三方组件（Vue / ECharts / china.js 地图 / 后台面板产物）与 Go 依赖的许可说明、
  以及**中国地图数据的合规提示**，见 [`THIRD-PARTY.md`](THIRD-PARTY.md)。
- 前端是**纯静态文件**（`frontend/`），没有构建步骤：改完 `app.js` / `style.css` / `index.html`
  后重新编译中心（前端通过 `//go:embed frontend` 打进二进制）即可生效。
- 两个可执行文件由 build tag 区分：
  `go build -tags center`（中心 + 内嵌前端） / `go build -tags node`（节点探针）。
- **不要把真实配置提交进仓库**：`center.json`（含数据库口令、管理密码）与 `node.key`
  （节点私钥）已在 `.gitignore` 中，请保持如此。
