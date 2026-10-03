# 第三方组件与出处

本项目自身代码以 **MIT** 许可发布（见 `LICENSE`）。下面列出随仓库一起分发的第三方组件，
它们各自遵循自己的许可证；**各组件的许可证与版权归其作者所有**，本项目不做任何再许可。

## 前端（随仓库内置，无构建步骤）

| 组件 | 版本 | 许可证 | 位置 | 说明 |
| --- | --- | --- | --- | --- |
| Vue | 3.5.12 | MIT | `frontend/vue.global.prod.js` | 文件头保留原始许可声明（`(c) 2018-present Yuxi (Evan) You`） |
| ECharts | 4.x（文件头未标注精确版本，请以文件头与上游为准） | Apache-2.0 | `frontend/echarts.min.js` | 文件头保留 Apache 基金会许可声明 |
| china.js（ECharts 中国地图扩展） | 未标注 | **未标注** | `frontend/china.js` | 见下方"地图数据"一节 |

## 后台管理面板

`frontend/admin-dist/` 是第三方后台脚手架（gin-vue-admin 一系）的**编译产物**，
随本项目一起分发，未做再许可。**产物内没有附带许可证文件**，上游仓库（flipped-aurora/gin-vue-admin）声明为 Apache-2.0 —— 这一点请以上游仓库声明为准，本项目只是如实转述、未做独立核实。取证记录：frontend\admin-dist\assets\Alerts-BWVoA72y.js: gin-vue、frontend\admin-dist\assets\Badge-BqSkm4DL.js: gin-vue、frontend\admin-dist\assets\Button-DF2YG0-S.js: gin-vue、frontend\admin-dist\assets\Dashboard-C3RHF86O.js: gin-vue、frontend\admin-dist\assets\Empty-DZD4M--t.js: gin-vue、frontend\admin-dist\assets\Loading-seCZqqOv.js: gin-vue

如果你是上游项目的维护者并认为此处标注不当，请提 issue，我们会立即更正或移除。

## 地图数据（重要）

`frontend/china.js` 携带的中国地图数据来自第三方（ECharts 早期地图扩展），
该文件**没有附带许可证或数据来源声明**。在中国境内**公开展示**中国地图需要符合
《地图管理条例》并使用带有**审图号**的标准地图。

因此：

- 本项目把它作为**技术演示**使用；**对外公开部署前**请自行确认合规性，
  或替换为有审图号的标准地图数据（例如自然资源部标准地图服务提供的版本）。
- 若你不确定合规性，最稳妥的做法是**不要公开部署带该地图的页面**（或整站改为不展示地图）。

## Go 依赖

以下模块通过 `go.mod` 引入，代码未复制进本仓库，各自遵循其自身许可证（多为 MIT / BSD / Apache-2.0），
许可证全文见各模块仓库：

- `github.com/jackc/pgx/v5` v5.7.1
- `golang.org/x/net` v0.30.0
- `modernc.org/sqlite` v1.33.1
- `github.com/dustin/go-humanize` v1.0.1
- `github.com/google/uuid` v1.6.0
- `github.com/gorilla/websocket` v1.5.3
- `github.com/hashicorp/golang-lru/v2` v2.0.7
- `github.com/jackc/pgpassfile` v1.0.0
- `github.com/jackc/pgservicefile` v0.0.0-20240606120523-5a60cdf6a761
- `github.com/jackc/puddle/v2` v2.2.2
- `github.com/mattn/go-isatty` v0.0.20
- `github.com/ncruces/go-strftime` v0.1.9
- `github.com/remyoudompheng/bigfft` v0.0.0-20230129092748-24d4a6f8daec
- `golang.org/x/crypto` v0.28.0
- `golang.org/x/sync` v0.8.0
- `golang.org/x/sys` v0.26.0
- `golang.org/x/text` v0.19.0
- `modernc.org/gc/v3` v3.0.0-20240107210532-573471604cb6
- `modernc.org/libc` v1.55.3
- `modernc.org/mathutil` v1.6.0
- `modernc.org/memory` v1.8.0
- `modernc.org/strutil` v1.2.0
- `modernc.org/token` v1.1.0

## 构建产物

`pingatlas_center_linux` / `pingatlas_node_linux` / `pingatlas_node_arm64` / `pingatlas.exe`
等是编译产物，**不随仓库分发**（见 `.gitignore`）。它们包含上述 Go 依赖的编译结果，
分发二进制时请一并遵守各依赖的许可证要求。

## 上游项目与出处声明

本项目的接口形态与页面结构参考了开源项目 **smartping / smartping-admin**（由第三方作者开发），
**该名称与相关版权归其作者所有**。本仓库不包含其源代码，仅在接口兼容与界面形态上做了对齐；
若你计划对外分发，请自行核对该上游项目的许可证与署名要求（本文件只是如实说明出处，不构成法律意见）。

后台管理面板（`frontend/admin-dist/`）是 gin-vue-admin 一系的编译产物，详见上文"后台管理面板"一节。
