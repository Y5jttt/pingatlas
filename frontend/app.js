const { createApp, ref, computed, onMounted, onUnmounted, nextTick } = Vue;

const TEL_NAMES = { ctcc: '电信', cucc: '联通', cmcc: '移动' };
// 丢包标记：红色小闪电。
// 地图标记：symbolSize [6,9]（按截图量到约 5×6px，颜色 #e61610）；
// 它的图例小图标是 iconfont 字形（.text-danger #ff5252）。
// 踩过的三个坑：
//  1) path:// 会被 ECharts 拉伸填满正方形 → 非正方形闪电会被压扁，所以用正方形 viewBox；
//  2) 白色描边在十几像素上会吃掉腰部 → 干脆不描边；
//  3) 太小(10px)会糊成一个红点认不出、太大(19px)又会显得笨重 → 取 18（实际 10.5×16.5px）。
// 形状长宽比 0.73（真实闪电约 0.5~0.8），实际显示 12 × 16.5 px。
const LOSS_BOLT_SVG =
  '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="24" height="24">' +
  '<polygon points="14,1 4,13 13,13 10,23 20,11 10,11" fill="#e61610"/></svg>';
const LOSS_BOLT = 'image://data:image/svg+xml;charset=utf-8,' + encodeURIComponent(LOSS_BOLT_SVG);
// 运营商文字色（与 style.css 里 .tel-badge.* 的底色完全一致，只用于文字，不画色块）
const TEL_COLORS = { ctcc: '#9ccc65', cucc: '#ffba57', cmcc: '#00acc1' };

const PROV_COORDS = {
  '北京': [116.4, 39.9], '天津': [117.2, 39.1], '河北': [114.5, 38.0],
  '山西': [112.5, 37.9], '内蒙古': [111.8, 40.8], '辽宁': [123.4, 41.8],
  '吉林': [125.3, 43.9], '黑龙江': [126.6, 45.8], '上海': [121.5, 31.2],
  '江苏': [119.8, 33.0], '浙江': [120.2, 30.3], '安徽': [117.3, 31.8],
  '福建': [119.3, 26.1], '江西': [115.9, 27.6], '山东': [117.0, 36.7],
  '河南': [113.7, 33.9], '湖北': [112.4, 30.6], '湖南': [112.0, 27.1],
  '广东': [113.3, 23.1], '广西': [108.4, 22.8], '海南': [110.0, 19.2],
  '重庆': [106.5, 29.6], '四川': [103.0, 30.6], '贵州': [106.7, 26.6],
  '云南': [102.7, 25.0], '西藏': [89.1, 31.5], '陕西': [108.9, 34.3],
  '甘肃': [103.8, 36.0], '青海': [96.0, 36.0], '宁夏': [106.3, 37.5],
  '新疆': [87.6, 43.8]
};

// 配色：白底极简风，白底文字用加深变体保证可读性
const C = {
  accent: '#4680ff', accentDark: '#1162e8',
  text: '#373a3c', dim: '#6c757d', muted: '#868e96', head: '#495057',
  border: '#e3eaef', border2: '#ced4da',
  green: '#9ccc65', yellow: '#ffba57', red: '#ff5252',
};

// 延迟分档（地图色块 = 按图例逐像素取色得到的实测值）：
//   超时 #e6180a / >250 #f89834 / 201-250 #f7ec44 / 101-200 #bff55f / 51-100 #45dd3a / ≤50 #23ab1d
// text 为表格/文字用色（白底需加深），fill 为地图色块用色（与图例一致）
const BANDS = [
  { max: 50, label: '≤50ms', text: '#2e7d32', fill: '#23ab1d' },
  { max: 100, label: '51-100ms', text: '#558b2f', fill: '#45dd3a' },
  { max: 200, label: '101-200ms', text: '#9e9d24', fill: '#bff55f' },
  { max: 250, label: '201-250ms', text: '#ef6c00', fill: '#f7ec44' },
  { max: Infinity, label: '>250ms', text: '#e65100', fill: '#f89834' }
];
const TIMEOUT_TEXT = '#d32f2f', TIMEOUT_FILL = '#e6180a';
const NODATA_TEXT = '#9e9e9e', NODATA_FILL = '#e8ecef'; // 无数据用页面中性灰
const BANDS_DESC = BANDS.slice().reverse(); // 图例顺序：超时在上、≤50 在下

// delay < 0 => 该目标在本窗口没有任何数据（无数据），0 => 真实超时
function isNoData(delay) {
  return delay === undefined || delay === null || delay < 0;
}

function isTimeout(delay) {
  return !isNoData(delay) && (delay === 0 || delay >= 2000);
}

function esc(s) {
  return String(s === undefined || s === null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

// 表格/文字用色
function delayColor(delay) {
  if (isNoData(delay)) return NODATA_TEXT;
  if (isTimeout(delay)) return TIMEOUT_TEXT;
  for (const b of BANDS) { if (delay <= b.max) return b.text; }
  return TIMEOUT_TEXT;
}

// 地图色块用色
function delayFill(delay) {
  if (isNoData(delay)) return NODATA_FILL;
  if (isTimeout(delay)) return TIMEOUT_FILL;
  for (const b of BANDS) { if (delay <= b.max) return b.fill; }
  return TIMEOUT_FILL;
}

function formatDelay(delay) {
  if (isNoData(delay)) return '无数据';
  if (isTimeout(delay)) return '超时';
  return delay.toFixed(0) + 'ms';
}

// 表格数字用色：数字默认纯深灰，只有异常值才上色（好坏靠地图与"质量"列表达）
function cellColor(delay) {
  if (isNoData(delay)) return NODATA_TEXT;
  if (isTimeout(delay)) return TIMEOUT_TEXT;
  if (delay > 200) return '#e07b1a';
  return '#373a3c';
}

// 浅色 tooltip 外壳（替代原先写死的深色 #1a2733）
function tipBox(minWidth) {
  return '<div class="sp-tip-box" style="font-size:12px;line-height:1.8;background:#fff;color:' + C.text +
    ';padding:8px 10px;border:1px solid ' + C.border + ';border-radius:4px;box-shadow:0 2px 8px rgba(0,0,0,.12);' +
    (minWidth ? 'min-width:' + minWidth + 'px;' : '') + '">';
}

// sparkline：把 24 个小时均值画成 SVG path（-1 = 无数据，断开）
function sparkPath(arr) {
  if (!arr || arr.length === 0) return '';
  const W = 100, H = 22, PAD = 2;
  let max = 0;
  for (const v of arr) { if (v > max) max = v; }
  if (max <= 0) max = 1;
  const step = arr.length > 1 ? (W / (arr.length - 1)) : W;
  let d = '', pen = false;
  for (let i = 0; i < arr.length; i++) {
    const v = arr[i];
    if (v < 0) { pen = false; continue; }
    const x = (i * step).toFixed(1);
    const y = (H - PAD - (v / max) * (H - PAD * 2)).toFixed(1);
    d += (pen ? ' L' : ' M') + x + ',' + y;
    pen = true;
  }
  return d.trim();
}

const App = {
  template: `
<div class="app-root">
  <header class="app-header">
   <div class="wrap header-inner">
    <a class="brand" href="/">
      <span class="brand-mark">SP</span>
      <span class="brand-words">
        <b>PingAtlas</b>
        <em>全国延迟监控</em>
      </span>
    </a>
    <span class="spacer"></span>
    <div class="header-right">
      <span class="hdr-stat" v-if="healthTotal > 0">
        <span class="hd ok"><i></i>正常 <b>{{ healthOk }}</b></span>
        <span class="hd warn"><i></i>超时 <b>{{ healthWarn }}</b></span>
        <span class="hd bad"><i></i>无数据 <b>{{ healthBad }}</b></span>
      </span>
      <span class="hdr-sep"></span>
      <span class="hdr-time">最后检测 <b>{{ lastCheckTime }}</b></span>
      <a class="hdr-link" href="/admin">管理</a>
    </div>
   </div>
  </header>

  <!-- 深色导航条：视图/口径/窗口/搜索 -->
  <nav class="app-nav">
   <div class="wrap nav-inner">
    <div class="tabs">
      <button :class="{active: viewMode==='all'}" @click="setViewMode('all')">全部</button>
      <button :class="{active: viewMode==='ctcc'}" @click="setViewMode('ctcc')">电信</button>
      <button :class="{active: viewMode==='cucc'}" @click="setViewMode('cucc')">联通</button>
      <button :class="{active: viewMode==='cmcc'}" @click="setViewMode('cmcc')">移动</button>
      <button :class="{active: viewMode==='best'}" @click="setViewMode('best')">最优</button>
    </div>
    <span class="nav-sep"></span>
    <div class="tabs">
      <button :class="{active: winMode==='5m'}" @click="setWindow('5m')">实时 5 分钟</button>
      <button :class="{active: winMode==='15m'}" @click="setWindow('15m')">15 分钟</button>
      <button :class="{active: winMode==='1h'}" @click="setWindow('1h')">1 小时</button>
      <button :class="{active: winMode==='24h'}" @click="setWindow('24h')">24 小时均值</button>
    </div>
    <span class="nav-sep"></span>
    <select v-model="nodeScope" @change="setNodeScope(nodeScope)" class="sel dark">
      <option value="">全网最优</option>
      <option v-for="n in nodeOptions" :key="n.id" :value="n.id">{{ n.label }}</option>
    </select>
    <input v-model="searchQ" @input="onSearchInput" class="search-input dark" placeholder="IP / 省 / 市 / 运营商">
    <button class="btn-mini dark" @click="refreshData">⟳ 刷新</button>
    <span class="nav-hint">{{ lastUpdate }} · {{ scopeLabel }}</span>
   </div>
  </nav>

  <div class="app-main wrap">
    <!-- 左栏：地图卡片 + 探测节点卡片（两张独立卡片，地图自动填满卡片高度） -->
    <div class="app-col">
    <section class="panel map-panel">
      <div class="panel-title">
        <span>全国目标延迟分布</span>
        <span class="hint" v-if="winMode !== '5m'">地图为实时快照</span>
        <span class="spacer"></span>
        <label class="checkbox-label"><input type="checkbox" v-model="showLossMark" @change="renderMap"> 丢包标记</label>
        <button class="btn-mini" @click="resetMapView">复位</button>
      </div>
      <div class="map-container">
        <div v-if="loading" class="loading-mask">加载中</div>
        <div ref="mapEl" class="map-box"></div>
        <div class="map-legend">
          <div class="legend-item"><span class="legend-color" style="background:#e6180a"></span>超时</div>
          <div class="legend-item" v-for="b in bandsDesc" :key="b.label">
            <span class="legend-color" :style="{background: b.fill}"></span>{{ b.label }}
          </div>
          <div class="legend-item"><span class="legend-color" style="background:#e8ecef"></span>无数据</div>
        </div>
      </div>

      <!-- 探测节点（与地图上的圆点对应；点一下切换单节点视角，详细统计在悬停提示里） -->

        <div class="node-strip">
        <div class="node-card" v-for="n in cards" :key="n.id"
             :class="{active: nodeScope===n.id}" @click="toggleNode(n.id)" :title="nodeTitle(n)">
          <span class="status-dot" :class="n.online ? 'ok' : 'bad'"></span>
          <b>{{ n.label }}</b>
          <span class="tag" v-if="n.self">本机</span>
        </div>
        <div v-if="cards.length === 0" class="empty">暂无节点</div>
      </div>
    </section>
    </div>

    <!-- 右：区域/运营商汇总（区域/运营商 | 最快 | 最慢 | 平均 表） -->
    <div class="app-col">
    <section class="panel">
      <div class="panel-title">
        <span>区域 / 运营商</span>
        <span class="hint">{{ nodeScope ? '当前视角：' + scopeLabel : '最快 / 最慢为跨节点取值' }}</span>
        <span class="spacer"></span>
        <span class="hint">点击地区行可过滤下方结果</span>
      </div>
      <!-- 悬停地图上的省份时，在这里显示该省全部目标（按运营商分列），移出即恢复下方全国汇总 -->
        <div v-if="activeProv" class="hover-detail">
          <div class="hover-head">
            <b>{{ activeProv }}</b>
            <span class="tag" v-if="pinnedProv">已固定</span>
            <span class="hover-sub">
              {{ hoverStat.count }} 个目标 · 平均 {{ fmt0(hoverStat.avg) }}ms · 丢包 {{ hoverStat.loss }}%
              · 最快 {{ hoverStat.best ? locText(hoverStat.best.row) + ' ' + formatDelay(hoverStat.best.delay) : '—' }}
            </span>
            <span class="spacer"></span>
            <button class="btn-mini" @click="filterByProvince(activeProv)">只看该省</button>
            <button class="btn-mini" v-if="pinnedProv" @click="closeProvincePanel">取消固定</button>
          </div>
          <div class="hover-list" @mouseenter="cancelHoverClear">
            <div class="hover-row" v-for="r in hoverList" :key="r.alias">
              <span class="hr-tel" :style="{color: r.telColor}">[{{ r.telName }}]</span>
              <span class="hr-loc" :title="r.loc + ' ' + r.ip">{{ r.loc }}</span>
              <span class="hr-ip">{{ r.ip }}</span>
              <span class="hr-v" :style="{color: delayColor(r.delay)}">{{ formatDelay(r.delay) }}</span>
              <span class="hr-loss" v-if="r.loss > 0">丢{{ r.loss }}%</span>
            </div>
          </div>
        </div>
        <table class="sum-table sum-fixed">
        <thead><tr><th>区域 / 运营商</th><th>最快</th><th>最慢</th><th>平均</th></tr></thead>
        <tbody>
          <tr>
            <td>全部节点</td>
            <td class="fast"><div class="v">{{ extValue(allStat.fastest) }}</div><div class="s">{{ extNode(allStat.fastest) }}</div></td>
            <td><div class="v">{{ extValue(allStat.slowest) }}</div><div class="s">{{ extNode(allStat.slowest) }}</div></td>
            <td class="avgcell"><div class="v">{{ fmt0(allStat.avg) }}</div><div class="bar"><span :style="{width: avgBarW(allStat.avg), background: delayColor(allStat.avg)}"></span></div></td>
          </tr>
          <tr v-for="g in byTelecomView" :key="g.key" class="clickable" @click="filterByTelecom(g.key)">
            <td>{{ g.label }}</td>
            <td class="fast"><div class="v">{{ extValue(g.fastest) }}</div><div class="s">{{ extNode(g.fastest) }}</div></td>
            <td><div class="v">{{ extValue(g.slowest) }}</div><div class="s">{{ extNode(g.slowest) }}</div></td>
            <td class="avgcell"><div class="v">{{ fmt0(g.avg) }}</div><div class="bar"><span :style="{width: avgBarW(g.avg), background: delayColor(g.avg)}"></span></div></td>
          </tr>
          <tr v-for="g in byRegionView" :key="g.key" class="clickable" @click="filterByRegion(g.key)">
            <td>{{ g.key }}地区</td>
            <td class="fast"><div class="v">{{ extValue(g.fastest) }}</div><div class="s">{{ extNode(g.fastest) }}</div></td>
            <td><div class="v">{{ extValue(g.slowest) }}</div><div class="s">{{ extNode(g.slowest) }}</div></td>
            <td class="avgcell"><div class="v">{{ fmt0(g.avg) }}</div><div class="bar"><span :style="{width: avgBarW(g.avg), background: delayColor(g.avg)}"></span></div></td>
          </tr>
        </tbody>
      </table>
    </section>
    </div>
  </div>

  <!-- 全宽统计条（域名解析统计那一条） -->
  <div class="wrap">

  <div class="stat-strip">
      <span>目标 <b>{{ totalRows }}</b></span>
      <span>探测节点 <b>{{ cards.length }}</b></span>
      <span>平均延迟 <b>{{ fmt0(allStat.avg) }}</b></span>
      <span>丢包 <b>{{ allStat.loss }}%</b></span>
      <span>最快 <b class="fast">{{ extText(allStat.fastest) }}</b></span>
      <span>最慢 <b>{{ extText(allStat.slowest) }}</b></span>
      <span class="spacer"></span>
      <span>数据时刻 <b>{{ matrixAsOf }}</b></span>
    </div>
  </div>

  <!-- 下：测试结果（外面套 .wrap，避免 .panel 的 padding 覆盖 .wrap 的左右留白导致与上方卡片错位） -->
  <div class="wrap">
  <section class="panel result-panel">
    <div class="panel-title">
      <span>测试结果</span>
      <span class="hint" v-if="regionFilter">地区筛选：{{ regionFilter }} <a href="javascript:;" @click="clearFilters">清除</a></span>
      <span class="spacer"></span>
    </div>
    <div class="filter-bar">
      <div class="tabs">
        <button :class="{active: resultView==='matrix'}" @click="setResultView('matrix')">按目标（一行一个 IP）</button>
        <button :class="{active: resultView==='flat'}" @click="setResultView('flat')">按监测点</button>
      </div>
      <span class="nav-sep"></span>
      <div class="tabs">
        <button :class="{active: telFilter===''}" @click="telFilter=''">全部</button>
        <button :class="{active: telFilter==='ctcc'}" @click="telFilter='ctcc'">中国电信</button>
        <button :class="{active: telFilter==='cucc'}" @click="telFilter='cucc'">中国联通</button>
        <button :class="{active: telFilter==='cmcc'}" @click="telFilter='cmcc'">中国移动</button>
        <button :class="{active: onlyDiff}" @click="onlyDiff=!onlyDiff">只看差异</button>
        <button :class="{active: onlyTimeout}" @click="onlyTimeout=!onlyTimeout">只看异常</button>
      </div>
      <span class="spacer"></span>
      <div class="cdd" :class="{open: dd==='sort'}">
        <button type="button" class="cdd-trigger" @click.stop="toggleDd('sort')">
          <span class="cdd-value">{{ sortLabel }}</span>
          <svg class="cdd-arrow" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="6 9 12 15 18 9"/></svg>
        </button>
        <ul class="cdd-list" v-if="dd==='sort'">
          <li v-for="s in sorts" :key="s.k + ':' + s.o" :class="{selected: (s.k + ':' + s.o) === (sortKey + ':' + sortOrder)}" @click="pickSort(s)">排序：{{ s.label }}</li>
        </ul>
      </div>
      <div class="cdd" :class="{open: dd==='prov'}">
        <button type="button" class="cdd-trigger" @click.stop="toggleDd('prov')">
          <span class="cdd-value">{{ provLabel }}</span>
          <svg class="cdd-arrow" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="6 9 12 15 18 9"/></svg>
        </button>
        <ul class="cdd-list" v-if="dd==='prov'">
          <li class="cdd-all" :class="{selected: provFilter===''}" @click="pickProv('')">全部省份</li>
          <li v-for="p in provinceOptions" :key="p" :class="{selected: provFilter===p}" @click="pickProv(p)">{{ p }}</li>
        </ul>
      </div>
      <button class="btn-mini" @click="exportCSV">导出 CSV</button>
    </div>

    <!-- 视图一：按监测点（一行一个监测点，左列=地区+运营商，右侧为延迟数据） -->
    <div class="table-wrap" v-if="resultView==='flat'">
      <table class="result-table">
        <thead>
          <tr>
            <th style="width:16%">检测点</th>
            <th style="width:16%">响应IP</th>
            <th style="width:30%">IP位置</th>
            <th>延迟</th>
            <th>丢包</th>
            <th>发包/收包</th>
            <th>时刻</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(c, i) in pagedFlat" :key="c.alias + '|' + c.node + '|' + i"
              :class="{active: selectedAlias===c.alias}" @click="selectRow(c.row)">
            <td class="node-cell"><b>{{ c.label }}</b><span class="tag" v-if="c.self">本机</span></td>
            <td class="mono">{{ c.ip }}</td>
            <td class="loc"><span class="tel-badge" :class="c.row.telecom">{{ telName(c.row.telecom) }}</span>{{ locText(c.row) }}</td>
            <td class="mono" :style="{color: cellColor(c.delay)}">{{ formatDelay(c.delay) }}</td>
            <td class="mono" :style="{color: c.loss > 0 ? '#d32f2f' : '#373a3c'}">{{ c.loss >= 0 ? c.loss + '%' : '—' }}</td>
            <td class="mono dim">{{ c.send }} / {{ c.recv }}</td>
            <td class="mono dim">{{ c.logtime }}</td>
          </tr>
          <tr v-if="pagedFlat.length === 0"><td colspan="7" class="empty">没有符合条件的目标</td></tr>
        </tbody>
      </table>
    </div>

    <!-- 视图二：一行一个目的 IP —— 左侧 IP位置 / 探测IP，右侧依次是各监测点的延迟（丢包跟在延迟后） -->
    <div class="table-wrap" v-else>
      <table class="result-table">
        <thead>
          <tr>
            <th style="width:26%" @click="setSort('province')">IP位置{{ sortMark('province') }}</th>
            <th style="width:16%">探测IP</th>
            <th v-for="n in cards" :key="n.id">{{ n.label }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="r in pagedRows" :key="r.alias" :class="{active: selectedAlias===r.alias}"
              @click="selectRow(r)">
            <td class="loc"><span class="tel-badge" :class="r.telecom">{{ telName(r.telecom) }}</span>{{ locText(r) }}</td>
            <td class="mono">{{ r.ip }}</td>
            <td v-for="n in cards" :key="n.id" class="mono cell">
              <span :style="{color: cellColor(cellDelay(r, n.id))}">{{ formatDelay(cellDelay(r, n.id)) }}</span>
              <span class="loss-tag" v-if="cellLoss(r, n.id) > 0">{{ cellLoss(r, n.id) }}%</span>
            </td>
          </tr>
          <tr v-if="displayRows.length === 0"><td :colspan="cards.length + 2" class="empty">没有符合条件的目标</td></tr>
        </tbody>
      </table>
    </div>
    <div class="result-foot">
      <span class="hint">当前显示 {{ displayRows.length }}/{{ totalRows }}</span>
      <span class="spacer"></span>
        <div class="cdd cdd--up">
          <button type="button" class="cdd-trigger" @click.stop="toggleDd('page')">
            <span class="cdd-value">{{ pageSizeLabel }}</span>
            <svg class="cdd-arrow" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="6 9 12 15 18 9"/></svg>
          </button>
          <ul class="cdd-list" v-if="dd==='page'">
            <li v-for="o in pageSizeOpts" :key="o.v" :class="{selected: pageSize===o.v}" @click="pickPageSize(o.v)">{{ o.t }}</li>
          </ul>
        </div>
      <div class="pager" v-if="showPager">
        <button class="btn-mini" @click="prevPage" :disabled="page === 1">上一页</button>
        <span class="hint">第 {{ page }} / {{ pageCount }} 页</span>
        <button class="btn-mini" @click="nextPage" :disabled="page >= pageCount">下一页</button>
        <span class="hint">跳到</span>
        <input class="page-jump" type="number" min="1" :max="pageCount" v-model="jumpPage"
               @keyup.enter="doJump" placeholder="页码" aria-label="跳转到第几页">
        <button class="btn-mini" @click="doJump">跳转</button>
      </div>
    </div>

    <div class="province-panel" v-show="provinceVisible">
      <div class="panel-header">
        <h3>
          <span v-if="focusIp">目的 IP {{ focusIp }} 延迟曲线</span>
          <span v-else>{{ currentProvince }} 延迟曲线（全部 IP）</span>
          <a v-if="focusIp" href="javascript:;" class="hint" style="margin-left:10px;" @click="clearFocusIp">查看全省</a>
          <span v-if="provError" style="color:var(--red);font-size:12px;font-weight:400;margin-left:8px;">{{ provError }}</span>
        </h3>
        <div class="panel-controls">
          <label class="checkbox-label"><input type="checkbox" v-model="showDelay" @change="renderCharts"> 延迟</label>
          <label class="checkbox-label"><input type="checkbox" v-model="showLoss" @change="renderCharts"> 丢包</label>
          <div class="cdd" :class="{open: dd==='tel'}">
            <button type="button" class="cdd-trigger" @click.stop="toggleDd('tel')">
              <span class="cdd-value">{{ telLabel }}</span>
              <svg class="cdd-arrow" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="6 9 12 15 18 9"/></svg>
            </button>
            <ul class="cdd-list" v-if="dd==='tel'">
              <li v-for="o in telOpts" :key="o.v" :class="{selected: provTelFilter===o.v}" @click="pickTel(o.v)">{{ o.t }}</li>
            </ul>
          </div>
          <div class="cdd" :class="{open: dd==='time'}">
            <button type="button" class="cdd-trigger" @click.stop="toggleDd('time')">
              <span class="cdd-value">{{ timeLabel }}</span>
              <svg class="cdd-arrow" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="6 9 12 15 18 9"/></svg>
            </button>
            <ul class="cdd-list" v-if="dd==='time'">
              <li v-for="o in timeOpts" :key="o.v" :class="{selected: String(timeRange)===o.v}" @click="pickTime(o.v)">{{ o.t }}</li>
            </ul>
          </div>
          <button class="btn-close" @click="closeProvinceChart">✕ 关闭</button>
        </div>
      </div>
      <div class="province-chart-container" ref="provChartEl">
        <!-- 每张图 = 一个目的 IP，图里每个监测点一条线（不做任何合并） -->
        <div v-if="provIpList.length === 0" style="padding:40px;text-align:center;color:var(--text-dim);">暂无数据</div>
        <div v-for="(ip, idx) in provIpList" :key="idx" class="ip-chart-card">
          <div class="ip-chart-title">
            {{ telName(ip.telecom) }} {{ ip.city }} {{ ip.ip }}
            <span class="ip-badge" :class="ipBadgeClass(ip)">{{ ipBadgeText(ip) }}</span>
            <span class="hint">{{ ip.series.length }} 个监测点分线</span>
          </div>
          <div :ref="el => setIpChartRef(el, idx)" class="ip-chart-box"></div>
        </div>
      </div>
    </div>
  </section>
  </div>
</div>
`,

  setup() {
    const mapEl = ref(null);
    const provChartEl = ref(null);
    const loading = ref(true);
    const viewMode = ref('all');
    const showLossMark = ref(true);
    const lastUpdate = ref('');
    const lastCheckTime = ref('');
    const nodeName = ref('');

    // ---- 口径与窗口（D + 矩阵）----
    const NODE_KEY = 'sp_node_scope';
    const WIN_KEY = 'sp_win_mode';
    const nodeScope = ref('');
    const winMode = ref('5m');
    const nodeOptions = ref([]);
    const cards = ref([]);
    const scopeLabel = computed(() => {
      const c = cards.value.find(x => x.id === nodeScope.value);
      return nodeScope.value
        ? '口径：单节点 ' + (c ? c.label : nodeScope.value)
        : '口径：全网最优（各节点取更优值）';
    });
    try {
      const q = new URLSearchParams(location.search);
      nodeScope.value = (q.get('node') || localStorage.getItem(NODE_KEY) || '').trim();
      winMode.value = (q.get('window') || localStorage.getItem(WIN_KEY) || '5m').trim();
    } catch (e) { nodeScope.value = ''; }
    if (!['5m', '15m', '1h', '24h'].includes(winMode.value)) winMode.value = '5m';

    function syncURL() {
      try {
        localStorage.setItem(NODE_KEY, nodeScope.value);
        localStorage.setItem(WIN_KEY, winMode.value);
        const u = new URL(location.href);
        if (nodeScope.value) u.searchParams.set('node', nodeScope.value);
        else u.searchParams.delete('node');
        if (winMode.value !== '5m') u.searchParams.set('window', winMode.value);
        else u.searchParams.delete('window');
        history.replaceState(null, '', u);
      } catch (e) { /* 忽略：存不上也不影响请求 */ }
    }

    function setNodeScope(v) {
      nodeScope.value = (v || '').trim();
      syncURL();
      selectedAlias.value = '';
      fetchData();
      if (provinceVisible.value) loadProvinceHistory();
    }

    function toggleNode(id) {
      setNodeScope(nodeScope.value === id ? '' : id);
    }

    function setWindow(v) {
      winMode.value = v;
      syncURL();
      fetchData();
    }

    function queryString() {
      const p = [];
      if (nodeScope.value) p.push('node=' + encodeURIComponent(nodeScope.value));
      if (winMode.value) p.push('window=' + encodeURIComponent(winMode.value));
      return p.length ? ('?' + p.join('&')) : '';
    }

    // 矩阵（下方结果表/曲线）**不带 node**：点监测点只影响地图与顶部统计条，
    // 否则接口只回一个节点的单元格，而表头仍按全部节点渲染 → 另一列会显示"无数据"。
    function windowQuery() {
      return winMode.value ? ('?window=' + encodeURIComponent(winMode.value)) : '';
    }

    // ---- 地图数据 ----
    const mapDetailData = ref({});
    const mapDelayData = ref({});

    // ---- 矩阵/结果表数据 ----
    const rows = ref([]);
    const totalRows = ref(0);
    const byTelecom = ref([]);
    const byRegion = ref([]);
    const matrixAsOf = ref('');
    const searchQ = ref('');
    const telFilter = ref('');
    const provFilter = ref('');
    const regionFilter = ref('');
    const onlyDiff = ref(false);
    const onlyTimeout = ref(false);
    const sortKey = ref('telecom');   // 默认：电信 → 联通 → 移动
    const sortOrder = ref('asc');
    const selectedAlias = ref('');
    const page = ref(1);
    // 每页显示多少行：用户可选，0 = 全部。默认 50，选择记在 localStorage。
    const PAGE_SIZE_OPTS = [
      { v: 20, t: '20 行' }, { v: 50, t: '50 行' }, { v: 100, t: '100 行' },
      { v: 200, t: '200 行' }, { v: 500, t: '500 行' }, { v: 0, t: '全部' }
    ];
    function loadPageSize() {
      try {
        const v = parseInt((localStorage.getItem('pingatlas_page_size') || localStorage.getItem('sp2_page_size')) || '', 10);
        if (!isNaN(v) && PAGE_SIZE_OPTS.some((o) => o.v === v)) return v;
      } catch (e) { /* 隐私模式下 localStorage 可能不可用 */ }
      return 50;
    }
    const pageSize = ref(loadPageSize());
    function pickPageSize(v) {
      pageSize.value = Number(v) || 0;
      page.value = 1;
      try { localStorage.setItem('pingatlas_page_size', String(pageSize.value)); } catch (e) {}
      closeDd();
      scrollResultTop();
    }
    // 是否显示分页条：选了具体行数且行数超过一页时才有意义（"全部"时不显示）
    const showPager = computed(() =>
      pageSize.value > 0 && displayRows.value.length > pageSize.value);
    const pageSizeLabel = computed(() =>
      pageSize.value === 0 ? '显示全部' : '每页 ' + pageSize.value + ' 行');
    // 下方结果区视图：matrix = 一行一个目的 IP（默认，左 IP位置 / 探测IP，右侧各监测点延迟）；
    // flat = 一行一个监测点
    const resultView = ref('matrix');
    const SORTS = [
      { k: 'telecom', o: 'asc', label: '运营商（电信→联通→移动）' },
      { k: 'spread', o: 'desc', label: '节点间差异 ↓' },
      { k: 'delay', o: 'asc', label: '延迟升序' },
      { k: 'delay', o: 'desc', label: '延迟降序' },
      { k: 'loss', o: 'desc', label: '丢包降序' },
      { k: 'province', o: 'asc', label: '按省份' }
    ];
    const TEL_RANK = { ctcc: 0, cucc: 1, cmcc: 2 };

    // ---- 自定义下拉（原生 select 的弹出列表无法美化，改用组件；样式对齐后台趋势分析）----
    const dd = ref('');                  // 当前展开的下拉框 key
    const TEL_OPTS = [{ v: 'all', t: '全部运营商' }, { v: 'ctcc', t: '电信' }, { v: 'cucc', t: '联通' }, { v: 'cmcc', t: '移动' }];
    const TIME_OPTS = [{ v: '1', t: '最近1小时' }, { v: '6', t: '最近6小时' }, { v: '12', t: '最近12小时' }, { v: '24', t: '最近24小时' }, { v: '72', t: '最近3天' }, { v: '168', t: '最近7天' }];
    function toggleDd(k) { dd.value = (dd.value === k) ? '' : k; }
    function closeDd() { dd.value = ''; }
    function pickIp(v) { ipFilter.value = v; page.value = 1; closeDd(); }
    function pickSort(s) { applySort(s.k + ':' + s.o); closeDd(); }
    function pickProv(v) { provFilter.value = v; regionFilter.value = ''; page.value = 1; closeDd(); }
    function pickTel(v) { provTelFilter.value = v; renderProvinceChart(); closeDd(); }
    function pickTime(v) { timeRange.value = v; loadProvinceHistory(); closeDd(); }
    const sortLabel = computed(() => {
      const cur = SORTS.find(s => (s.k + ':' + s.o) === (sortKey.value + ':' + sortOrder.value));
      return '排序：' + (cur ? cur.label : '运营商（电信→联通→移动）');
    });
    const ipLabel = computed(() => {
      if (!ipFilter.value) return '全部目的 IP';
      const o = ipOptions.value.find(x => x.ip === ipFilter.value);
      return ipFilter.value + (o && o.desc ? ' · ' + o.desc : '');
    });
    const provLabel = computed(() => provFilter.value || '全部省份');
    const telLabel = computed(() => {
      const o = TEL_OPTS.find(x => x.v === provTelFilter.value);
      return o ? o.t : TEL_OPTS[0].t;
    });
    const timeLabel = computed(() => {
      const o = TIME_OPTS.find(x => x.v === String(timeRange.value));
      return o ? o.t : '最近24小时';
    });

    function setResultView(v) { resultView.value = v; page.value = 1; }
    function applySort(v) {
      const s = SORTS.find(x => x.k + ':' + x.o === v) || SORTS[0];
      sortKey.value = s.k;
      sortOrder.value = s.o;
      page.value = 1;
    }

    const REGION_PROVINCES = {
      '华东': ['上海', '江苏', '浙江', '安徽', '福建', '江西', '山东'],
      '华南': ['广东', '广西', '海南'],
      '华中': ['河南', '湖北', '湖南'],
      '华北': ['北京', '天津', '河北', '山西', '内蒙古'],
      '西南': ['重庆', '四川', '贵州', '云南', '西藏'],
      '西北': ['陕西', '甘肃', '青海', '宁夏', '新疆'],
      '东北': ['辽宁', '吉林', '黑龙江'],
      '港澳台': ['香港', '澳门', '台湾']
    };

    const provinceOptions = computed(() => {
      const s = new Set();
      for (const r of rows.value) if (r.province) s.add(r.province);
      return Array.from(s).sort();
    });

    // 目的 IP 选择器（只看单个目标的用法）
    const ipFilter = ref('');
    const ipOptions = computed(() => {
      const m = new Map();
      for (const r of rows.value) {
        m.set(r.ip, locText(r) + ' ' + (TEL_NAMES[r.telecom] || ''));
      }
      return Array.from(m.entries())
        .map(([ip, desc]) => ({ ip, desc: desc.trim() }))
        .sort((a, b) => a.ip.localeCompare(b.ip));
    });

    const displayRows = computed(() => {
      const q = searchQ.value.trim().toLowerCase();
      const regSet = regionFilter.value ? new Set(REGION_PROVINCES[regionFilter.value] || []) : null;
      const out = [];
      for (const r of rows.value) {
        if (telFilter.value && r.telecom !== telFilter.value) continue;
        if (provFilter.value && r.province !== provFilter.value) continue;
        if (ipFilter.value && r.ip !== ipFilter.value) continue;
        if (regSet && !regSet.has(r.province)) continue;
        if (q) {
          const hay = (r.alias + ' ' + r.ip + ' ' + r.province + r.city + ' ' + (TEL_NAMES[r.telecom] || '')).toLowerCase();
          if (hay.indexOf(q) < 0) continue;
        }
        if (onlyDiff.value && !(r.spread > 0)) continue;
        if (onlyTimeout.value) {
          let bad = r.missing > 0;
          if (!bad) {
            for (const k in r.cells) {
              const d = r.cells[k].delay;
              if (d <= 0 || d >= 2000) { bad = true; break; }
            }
          }
          if (!bad) continue;
        }
        out.push(r);
      }
      const k = sortKey.value, desc = sortOrder.value === 'desc';
      const val = (r) => {
        if (k === 'loss') {
          let m = -1;
          for (const n in r.cells) { if (r.cells[n].loss > m) m = r.cells[n].loss; }
          return m;
        }
        if (k === 'province') return 0;
        return r.spread;
      };
      out.sort((a, b) => {
        // 默认：电信 → 联通 → 移动；同运营商内按省+市稳定排列
        if (k === 'telecom') {
          const ra = a.telecom in TEL_RANK ? TEL_RANK[a.telecom] : 9;
          const rb = b.telecom in TEL_RANK ? TEL_RANK[b.telecom] : 9;
          if (ra !== rb) return desc ? rb - ra : ra - rb;
          const c = (a.province + a.city).localeCompare(b.province + b.city);
          return desc ? -c : c;
        }
        if (k === 'province') {
          const c = (a.province + a.city).localeCompare(b.province + b.city);
          return desc ? -c : c;
        }
        if (k === 'delay') {
          const da = a.best < 0 ? 1e9 : a.best, db = b.best < 0 ? 1e9 : b.best;
          return desc ? db - da : da - db;
        }
        const d = val(b) - val(a);
        return desc ? d : -d;
      });
      return out;
    });

    // 分页：flat 视图按"监测点行"分页，matrix 视图按"目标行"分页
    // 当前视图下要分页的行（按目标 / 按监测点），页码与切片都按它算
    const rowsForPage = computed(() => (resultView.value === 'flat' ? flatRows.value : displayRows.value));
    // pageSize = 0 表示"全部"，此时只有一页
    const effPageSize = computed(() => {
      if (pageSize.value > 0) return pageSize.value;
      return Math.max(1, rowsForPage.value.length);
    });
    const pageCount = computed(() => {
      const n = rowsForPage.value.length;
      return Math.max(1, Math.ceil(n / effPageSize.value));
    });

    const pagedRows = computed(() => {
      const p = Math.min(page.value, pageCount.value);
      const size = effPageSize.value;
      return displayRows.value.slice((p - 1) * size, p * size);
    });

    // 目的 IP 选择器已在上方声明（ipFilter/ipOptions）

    // 表格/CSV/气泡统一用的「位置」：不要斜线；**省名只出现一次**。
    //   省=市（直辖市）          北京 + 北京        -> 北京
    //   市名已带省名（含 AS 后缀）北京 + 北京AS4809 -> 北京AS4809（不再拼成"北京 北京AS4809"）
    //   市名已带省名（编号后缀）  北京 + 北京2       -> 北京2
    //   普通省市                 福建 + 福州        -> 福建 福州
    function locText(r) {
      const p = (r.province || '').trim(), c = (r.city || '').trim();
      if (!p && !c) return '-';
      if (!p) return c;
      if (!c) return p;
      if (c === p || c.indexOf(p) === 0) return c;
      return p + ' ' + c;
    }
    function whereOf(r) {
      return locText(r);
    }
    // 节点卡片只显示名字，统计放到悬停提示
    function nodeTitle(n) {
      return n.label + (n.self ? '（本机）' : '') + '\n均值 ' + fmt0(n.avg) + ' · 丢包 ' + n.loss + '%' +
        ' · 覆盖目标 ' + n.targets + '\n' + (n.online ? '在线' : '离线') + ' ' + (n.last || '') +
        '\n点击切换为该节点视角';
    }
    const flatRows = computed(() => {
      const out = [];
      for (const r of displayRows.value) {
        const cells = [];
        for (const n of cards.value) {
          const c = r.cells[n.id];
          if (!c) continue;
          cells.push({
            node: n.id, label: n.label, self: n.self, delay: c.delay, loss: c.loss,
            send: c.send, recv: c.recv, logtime: c.logtime
          });
        }
        // 同一个目的 IP 内按延迟升序：最快的监测点排最前
        cells.sort((a, b) => (a.delay <= 0 ? 1e9 : a.delay) - (b.delay <= 0 ? 1e9 : b.delay));
        for (const c of cells) {
          out.push({
            alias: r.alias, ip: r.ip, where: whereOf(r), row: r,
            node: c.node, label: c.label, self: c.self,
            delay: c.delay, loss: c.loss, send: c.send, recv: c.recv, logtime: c.logtime
          });
        }
      }
      return out;
    });
    const pagedFlat = computed(() => {
      const p = Math.min(page.value, pageCount.value);
      const size = effPageSize.value;
      return flatRows.value.slice((p - 1) * size, p * size);
    });

    // 全量统计：对应"全部节点"汇总行与顶部统计条
    const allStat = computed(() => {
      let sum = 0, n = 0, lossSum = 0, lossN = 0;
      let fast = null, slow = null;
      for (const r of rows.value) {
        const where = locText(r); // 与表格同一套规则：省名只出现一次
        // 统一走 rowBest：选了节点就是那台的数值，最快/最慢/平均/丢包都跟着节点视角
        const b = rowBest(r);
        if (b.loss >= 0) { lossSum += b.loss; lossN++; }
        if (b.delay > 0 && b.delay < 2000) {
          sum += b.delay; n++;
          const node = b.node || '';
          if (!fast || b.delay < fast.value) fast = { node, value: b.delay, where, telecom: r.telecom };
          if (!slow || b.delay > slow.value) slow = { node, value: b.delay, where, telecom: r.telecom };
        }
      }
      const ext = (e) => e ? { node: e.node, label: nodeLabelOf(e.node), value: Math.round(e.value * 100) / 100, where: e.where, telecom: e.telecom } : null;
      return {
        avg: n ? sum / n : -1,
        loss: lossN ? Math.round(lossSum / lossN * 100) / 100 : 0,
        fastest: ext(fast),
        slowest: ext(slow)
      };
    });

    // ---- 按当前口径（选了节点就是那台）重算"运营商 / 大区"汇总 ----
    // 接口返回的 byTelecom/byRegion 是全网算好的；选了节点就得在客户端按该节点重算。
    // 省 → 大区（反查上面的 REGION_PROVINCES）
    const PROV_REGION = {};
    for (const reg in REGION_PROVINCES) for (const p of REGION_PROVINCES[reg]) PROV_REGION[p] = reg;
    function regionOfRow(r) { return PROV_REGION[r.province] || ''; }
    // 聚合：同一组内取最快/最慢（都按 rowBest 口径），平均只统计有效延迟
    function groupOf(list, keyOf, labelOf, ext) {
      const acc = new Map();
      for (const r of list) {
        const key = keyOf(r);
        if (!key) continue;
        let s = acc.get(key);
        if (!s) { s = { key, label: labelOf(r, key), sum: 0, n: 0, fast: null, slow: null }; acc.set(key, s); }
        const b = rowBest(r);
        if (!(b.delay > 0 && b.delay < 2000)) continue;
        s.sum += b.delay; s.n++;
        const hit = { node: b.node || '', value: b.delay, where: locText(r), telecom: r.telecom };
        if (!s.fast || b.delay < s.fast.value) s.fast = hit;
        if (!s.slow || b.delay > s.slow.value) s.slow = hit;
      }
      return Array.from(acc.values()).map((s) => ({
        key: s.key, label: s.label, avg: s.n ? s.sum / s.n : -1,
        fastest: ext(s.fast), slowest: ext(s.slow)
      }));
    }
    const extHit = (e) => e ? { node: e.node, label: nodeLabelOf(e.node), value: Math.round(e.value * 100) / 100, where: e.where, telecom: e.telecom } : null;
    // 选了节点才重算；没选就用接口返回的全网值（省一次计算，也保证与接口完全一致）
    const byTelecomView = computed(() => (nodeScope.value
      ? groupOf(rows.value, (r) => r.telecom, (r, k) => TEL_NAMES[k] || k, extHit)
      : byTelecom.value));
    const byRegionView = computed(() => (nodeScope.value
      ? groupOf(rows.value, regionOfRow, (r, k) => k, extHit)
      : byRegion.value));

    // 数据变化后分页归位，避免停留在空页
    function clampPage() {
      if (page.value > pageCount.value) page.value = pageCount.value;
      if (page.value < 1) page.value = 1;
    }

    // 翻页/换每页行数后把结果表滚回顶部：分页控件已挪到表格下方，否则点完还停在页面底部
    function scrollResultTop() {
      const el = document.querySelector('.result-panel');
      if (el && el.scrollIntoView) el.scrollIntoView({ behavior: 'smooth', block: 'start' });
    }
    // 跳转到指定页：输入非法就什么都不做；超出范围夹到 1..pageCount
    const jumpPage = ref('');
    function doJump() {
      const n = parseInt(jumpPage.value, 10);
      if (isNaN(n)) { jumpPage.value = ''; return; }
      page.value = Math.min(Math.max(1, n), pageCount.value);
      jumpPage.value = '';
      scrollResultTop();
    }
    function nextPage() {
      if (page.value < pageCount.value) { page.value++; scrollResultTop(); }
    }
    function prevPage() {
      if (page.value > 1) { page.value--; scrollResultTop(); }
    }

    function setSort(k) {
      if (sortKey.value === k) {
        sortOrder.value = sortOrder.value === 'desc' ? 'asc' : 'desc';
      } else {
        sortKey.value = k;
        sortOrder.value = k === 'province' ? 'asc' : 'desc';
      }
      page.value = 1;
    }
    function sortMark(k) { return sortKey.value === k ? (sortOrder.value === 'desc' ? ' ↓' : ' ↑') : ''; }

    function onSearchInput() { page.value = 1; }
    function clearFilters() {
      regionFilter.value = '';
      provFilter.value = '';
      telFilter.value = '';
      searchQ.value = '';
      onlyDiff.value = false;
      onlyTimeout.value = false;
      page.value = 1;
    }
    function filterByRegion(reg) {
      regionFilter.value = regionFilter.value === reg ? '' : reg;
      provFilter.value = '';
      page.value = 1;
    }

    // 点地图上的省份 → 用省份过滤下方测试结果（再点一次取消）
    // ---- 悬停省份 → 右侧「区域/运营商」卡片显示该省明细 ----
    // 地图上不再弹省份提示（省得盖住地图）；鼠标移出地图即恢复全国汇总。
    const hoverProv = ref('');   // 悬停预览（鼠标在地图上停留的那个省）
    const pinnedProv = ref('');  // 点击固定住的省（鼠标可以离开地图，面板不消失）
    // 面板显示哪个省：**悬停优先**，其次才是固定住的
    // （固定 A 之后还想扫一眼 B：悬停 B 显示 B，鼠标移开自动回到 A）
    const activeProv = computed(() => hoverProv.value || pinnedProv.value);
    var hoverClearTimer = null;
    function cancelHoverClear() {
      if (hoverClearTimer) { clearTimeout(hoverClearTimer); hoverClearTimer = null; }
    }
    // 鼠标移出地图后不立刻清空：给 400ms 让用户把鼠标移到右侧面板（要滚动/复制时用）
    function scheduleHoverClear() {
      cancelHoverClear();
      hoverClearTimer = setTimeout(function () {
        hoverClearTimer = null;
        hoverProv.value = '';
      }, 400);
    }
    // 点击地图上的省份：固定/取消固定（原来的"过滤下方结果"改到面板里的「只看该省」按钮）
    function togglePinProvince(name) {
      if (!name) return;
      cancelHoverClear();
      pinnedProv.value = pinnedProv.value === name ? '' : name;
      hoverProv.value = name;
    }
    function closeProvincePanel() {
      const p = pinnedProv.value;
      pinnedProv.value = '';
      hoverProv.value = '';
      // 顺带把"正好是这个省"的筛选也清掉，回到全国视图（否则表格还停在那个省，容易困惑）
      if (p && provFilter.value === p) provFilter.value = '';
    }
    // 运营商的展示顺序：电信 → 联通 → 移动 → 其它（不能用字母序：cmcc<ctcc<cucc 会把移动排最前）
    const TEL_ORDER = { ctcc: 0, cucc: 1, cmcc: 2 };
    // 行级取值：矩阵接口给的是 best / best_node / cells（没有 delay/loss 字段，
    // 那两个只在 ?node= 单节点视角下才有）。这里按"全网最优"取：延迟用 best，
    // 丢包用 best_node 那台的丢包（没有就取各节点最大值）。
    // 曾经这里直接读 r.delay → 全是 undefined → 界面上整列显示"无数据"。
    function rowBest(r) {
      const cells = r.cells || {};
      // 选了单个节点 → 用那台的数值；没选 → 用接口给的"全网最优"
      const scope = nodeScope.value;
      if (scope) {
        const c = cells[scope];
        return { delay: c ? c.delay : -1, loss: c && c.loss >= 0 ? c.loss : -1, node: scope };
      }
      let delay = -1;
      if (typeof r.best === 'number') {
        delay = r.best;
      } else {
        for (const k in cells) {
          const d = cells[k] && cells[k].delay;
          if (d > 0 && d < 2000 && (delay < 0 || d < delay)) delay = d;
        }
      }
      let loss = -1;
      const bn = r.best_node;
      if (bn && cells[bn] && cells[bn].loss >= 0) {
        loss = cells[bn].loss;
      } else {
        for (const k in cells) {
          const l = cells[k] && cells[k].loss;
          if (l > loss) loss = l;
        }
      }
      return { delay, loss, node: r.best_node || '' };
    }

    const hoverRows = computed(() => {
      if (!activeProv.value) return [];
      const rank = (t) => (TEL_ORDER[t] === undefined ? 9 : TEL_ORDER[t]);
      return rows.value
        .filter((r) => r.province === activeProv.value)
        .slice()
        .sort((a, b) => {
          const ra = rank(a.telecom), rb = rank(b.telecom);
          if (ra !== rb) return ra - rb;
          // 有数据的排前面（按延迟升序），超时/无数据排最后
          const dv = (r) => { const d = rowBest(r).delay; return d > 0 ? d : 9e9; };
          return dv(a) - dv(b);
        });
    });
    // 逐行列表（与原先的地图提示框同样格式）：[运营商] 位置 IP: 延迟 丢包%
    // 顺序：先按运营商（电信→联通→移动→其它），同一运营商内按延迟从小到大。
    const hoverList = computed(() => hoverRows.value.map((r) => {
      const b = rowBest(r);
      return {
        alias: r.alias,
        loc: locText(r),
        ip: r.ip,
        delay: b.delay,
        loss: b.loss,
        telName: TEL_NAMES[r.telecom] || r.telecom || '',
        telColor: TEL_COLORS[r.telecom] || C.dim
      };
    }));

    const hoverStat = computed(() => {
      let sum = 0, n = 0, lossSum = 0, lossN = 0, best = null;
      for (const r of hoverRows.value) {
        const b = rowBest(r);
        if (b.delay > 0 && b.delay < 2000) {
          sum += b.delay; n++;
          if (!best || b.delay < best.delay) best = { row: r, delay: b.delay };
        }
        if (b.loss >= 0) { lossSum += b.loss; lossN++; }
      }
      return { count: hoverRows.value.length, avg: n ? sum / n : -1,
               loss: lossN ? Math.round(lossSum / lossN) : 0, best };
    });

    function filterByProvince(prov) {
      provFilter.value = (provFilter.value === prov) ? '' : prov;
      regionFilter.value = '';
      selectedAlias.value = '';
      page.value = 1;
    }

    // 点汇总表里的运营商行 → 按运营商过滤
    function filterByTelecom(tel) {
      telFilter.value = (telFilter.value === tel) ? '' : tel;
      page.value = 1;
    }

    function cellDelay(r, nodeId) {
      const c = r.cells[nodeId];
      return c ? c.delay : -1;
    }
    function cellLoss(r, nodeId) {
      const c = r.cells[nodeId];
      return c ? c.loss : -1;
    }
    function rowLogtime(r) {
      for (const k in r.cells) return r.cells[k].logtime || '';
      return '';
    }
    function fmt0(v) {
      if (v === undefined || v === null || v < 0) return '—';
      return v.toFixed(0) + 'ms';
    }
    function extText(e) {
        if (!e || !e.value || e.value <= 0) return '—';
        // 单节点视角下不必再重复节点名
        const head = nodeScope.value ? '' : nodeLabelOf(e.node);
        return (head ? head + ' ' : '') + e.value.toFixed(0) + 'ms' + (e.where ? ' · ' + e.where : '');
      }
    // 汇总表两行排版：主值 / 次行（节点 · 地点）
    function extValue(e) {
      if (!e || !e.value || e.value <= 0) return '—';
      return e.value.toFixed(0) + 'ms';
    }
    // 次行 = 监测点 · 地点 运营商（地点与运营商之间用空格，不加点；AS 后缀保留原样）
    function extNode(e) {
        if (!e || !e.value || e.value <= 0) return '';
        // 单节点视角下不必再重复节点名（整页都是这台）
        let s = nodeScope.value ? '' : nodeLabelOf(e.node);
        if (e.where) s += (s ? ' · ' : '') + e.where;
        if (e.telecom) s += (s ? ' ' : '') + (TEL_NAMES[e.telecom] || e.telecom);
        return s;
      }
    // 平均列的比例条：300ms 打满，用于横向对比各省/各运营商
    function shortTime(v) {
      // x 轴时间标签：03:05:10 → 03:05（窗口 ≥2 天时带上"10-03 03:05"）
      const s = String(v === undefined || v === null ? '' : v);
      const m = s.match(/(?:(\d{4})-)?(?:(\d{2})-(\d{2})[ T])?(\d{2}:\d{2})(?::\d{2})?/);
      if (!m) return s;
      const hm = m[4];
      const md = m[2] ? (m[2] + '-' + m[3]) : '';
      const hours = Number(timeRange.value) || 24;
      return (md && hours >= 48) ? (md + ' ' + hm) : hm;
    }
    function avgBarW(v) {
      if (v === undefined || v === null || v < 0) return '0%';
      return Math.min(100, (v / 300) * 100) + '%';
    }
    // 24h 历史不足时别画一条空线（新部署的节点只有几小时数据）
    function hasSpark(arr) {
      if (!arr) return false;
      let n = 0;
      for (const v of arr) { if (v >= 0) n++; if (n >= 3) return true; }
      return false;
    }
    function nodeLabelOf(id) {
      const c = cards.value.find(x => x.id === id);
      return c ? c.label : (id || '');
    }
    function sparkPathOf(arr) { return sparkPath(arr); }

    // ---- 地图/省级曲线状态 ----
    const provinceVisible = ref(false);
    const currentProvince = ref('');
    const provTelFilter = ref('all');
    const timeRange = ref('24');
    const provIpList = ref([]);
    const provError = ref('');
    const focusIp = ref(''); // 非空 = 只画这一个目的 IP（全省时为全部 IP）
    const showDelay = ref(true);   // 曲线显示项：默认延迟与丢包都显示
    const showLoss = ref(true);

    let mainChart = null;
    let ipCharts = [];
    let ipChartEls = [];
    let refreshTimer = null;
    let lastSuccessTime = '';

    function telName(tel) { return TEL_NAMES[tel] || tel || ''; }
    function setIpChartRef(el, idx) { if (el) ipChartEls[idx] = el; }

    function ipBadgeClass(ip) {
      const lastDelay = ip.history && ip.history.length ? ip.history[ip.history.length - 1] : ip.delay;
      const lastLoss = ip.loss && ip.loss.length ? ip.loss[ip.loss.length - 1] : 0;
      if (isNoData(lastDelay)) return 'warn';
      if (isTimeout(lastDelay) || lastLoss >= 100) return 'bad';
      if (lastLoss > 0) return 'warn';
      return 'ok';
    }
    function ipBadgeText(ip) {
      const lastDelay = ip.history && ip.history.length ? ip.history[ip.history.length - 1] : ip.delay;
      const lastLoss = ip.loss && ip.loss.length ? ip.loss[ip.loss.length - 1] : 0;
      if (isNoData(lastDelay)) return '无数据';
      if (isTimeout(lastDelay)) return '超时';
      if (lastLoss > 0) return '丢包' + lastLoss + '%';
      return formatDelay(lastDelay);
    }

    // Health
    const healthOk = ref(0);
    const healthWarn = ref(0);
    const healthBad = ref(0);
    const healthTotal = computed(() => healthOk.value + healthWarn.value + healthBad.value);

    function calcHealth() {
      let ok = 0, warn = 0, bad = 0;
      for (const prov in mapDetailData.value) {
        const detail = mapDetailData.value[prov];
        for (const tel of ['ctcc', 'cucc', 'cmcc']) {
          const cities = detail[tel];
          if (!cities) continue;
          for (const city in cities) {
            for (const ip of cities[city]) {
              if (isNoData(ip.delay)) continue;
              if (isTimeout(ip.delay) || ip.loss >= 100) bad++;
              else if (ip.loss > 0) warn++;
              else ok++;
            }
          }
        }
      }
      healthOk.value = ok;
      healthWarn.value = warn;
      healthBad.value = bad;
    }

    function getBestDelay(province) {
      const detail = mapDetailData.value[province];
      if (!detail) return null;
      let best = null, bestTel = null, bestCity = '', bestIp = '';
      for (const tel of ['ctcc', 'cucc', 'cmcc']) {
        const cities = detail[tel];
        if (!cities) continue;
        for (const city in cities) {
          for (const ip of cities[city]) {
            if (ip.delay > 0 && ip.delay < 2000) {
              if (best === null || ip.delay < best) {
                best = ip.delay; bestTel = tel; bestCity = city; bestIp = ip.ip || '';
              }
            }
          }
        }
      }
      return best !== null ? { delay: best, tel: bestTel, city: bestCity, ip: bestIp } : null;
    }

    function buildTooltip(province) {
      const detail = mapDetailData.value[province];
      if (!detail) return tipBox(200) + '<span style="color:' + C.muted + ';">暂无数据</span></div>';
      let html = tipBox(230);
      html += '<b style="color:' + C.head + ';">' + esc(province) + '</b><br/>';
      let rowsHtml = '';
      let rowCount = 0;
      for (const tel of ['ctcc', 'cucc', 'cmcc']) {
        const cities = detail[tel];
        if (!cities) continue;
        for (const city in cities) {
          for (const ip of cities[city]) {
            const dStr = formatDelay(ip.delay);
            const dColor = delayColor(ip.delay);
            const lossStr = ip.loss > 0 ? ' <span style="color:' + C.red + ';">丢包' + esc(ip.loss) + '%</span>' : '';
            // 运营商连同方括号一起用同色（只改文字颜色，不加底色块）
            const telStr = '<span style="color:' + (TEL_COLORS[tel] || C.dim) + ';font-weight:600;">[' + esc(TEL_NAMES[tel]) + ']</span>';
            rowsHtml += '<div style="color:' + C.dim + ';">' + telStr + ' ' + esc(city) + ' <span style="font-family:monospace;color:' + C.text + ';">' + esc(ip.ip) + '</span>: <span style="color:' + dColor + ';font-weight:600;">' + dStr + '</span>' + lossStr + '</div>';

            rowCount++;
          }
        }
      }
      // 行数多（如广东 20+ 个 IP）时排两列，避免一列太高超出地图容器

      html += '<div class="sp-tip-list' + (rowCount > 8 ? ' sp-tip-list--two' : '') + '">' + rowsHtml + '</div>';

      const best = getBestDelay(province);
      if (best) {
        // 最优行也带上"是哪个地区/哪个 IP"，不只给运营商和延迟
        const bTel = '<span style="color:' + (TEL_COLORS[best.tel] || C.accent) + ';font-weight:600;">[' + esc(TEL_NAMES[best.tel]) + ']</span>';
        const bCity = best.city ? ' ' + esc(best.city) : '';
        const bIp = best.ip ? ' <span style="font-family:monospace;">' + esc(best.ip) + '</span>' : '';
        html += '<div style="margin-top:5px;padding-top:5px;border-top:1px solid ' + C.border + ';color:' + C.accent + ';font-weight:600;">最优: ' + bTel + bCity + bIp + ' ' + formatDelay(best.delay) + '</div>';
      }
      html += '<div style="color:' + C.muted + ';font-size:10px;margin-top:4px;">点击查看历史曲线</div></div>';
      return html;
    }

    function targetTooltip(r) {
      let html = tipBox(210);
      html += '<b style="color:' + C.head + ';">' + esc(locText(r)) + ' ' + (TEL_NAMES[r.telecom] || '') + '</b><br/>';
      html += '<span style="font-family:monospace;color:' + C.text + ';">' + esc(r.ip) + '</span><br/>';
      for (const k in r.cells) {
        const c = r.cells[k];
        html += '<div style="color:' + C.dim + ';">' + esc(nodeLabelOf(k)) + ': <span style="color:' + delayColor(c.delay) + ';font-weight:600;">' + formatDelay(c.delay) + '</span>' + (c.loss > 0 ? ' <span style="color:' + C.red + ';">丢' + c.loss + '%</span>' : '') + '</div>';
      }
      if (r.spread >= 0) {
        html += '<div style="margin-top:4px;color:' + C.accent + ';">节点间差异 ' + r.spread + 'ms</div>';
      }
      html += '</div>';
      return html;
    }

    // 地图上的探测节点标记已移除（用户要求：不需要在地图上标节点位置，
    // 这样新增节点也不必再配经纬度；节点只以地图下方的胶囊/统计条体现）
    function selectedMarkPoint(series) {
      const r = rows.value.find(x => x.alias === selectedAlias.value);
      if (!r || !PROV_COORDS[r.province]) return;
      series.markPoint.data.push({
        name: '目标:' + r.ip,
        targetAlias: r.alias,
        coord: PROV_COORDS[r.province],
        value: 2,
        symbol: 'pin',
        symbolSize: 42,
        itemStyle: { color: C.yellow },
        label: { show: true, formatter: '选中', color: '#fff', fontSize: 10, fontWeight: 'bold' }
      });
    }

    function renderMap() {
      if (!mainChart) return;
      const bestByProv = {};
      for (const tel of ['ctcc', 'cucc', 'cmcc']) {
        const data = mapDelayData.value[tel] || [];
        for (const item of data) {
          if (item.value > 0 && item.value < 2000) {
            if (bestByProv[item.name] === undefined || item.value < bestByProv[item.name]) {
              bestByProv[item.name] = item.value;
            }
          }
        }
      }

      let data;
      let seriesName;
      if (viewMode.value === 'all' || viewMode.value === 'best') {
        data = Object.entries(bestByProv).map(([name, val]) => ({
          name, value: val, itemStyle: { normal: { areaColor: delayFill(val) } }
        }));
        seriesName = '最优延迟';
      } else {
        const tel = viewMode.value;
        data = (mapDelayData.value[tel] || []).map(item => ({
          name: item.name, value: item.value, itemStyle: { normal: { areaColor: delayFill(item.value) } }
        }));
        seriesName = TEL_NAMES[tel];
      }

      const series = {
        name: seriesName, type: 'map', map: 'china', roam: true,
        zoom: 1.2,                       // 初始放大一点，让地图占满卡片
        scaleLimit: { min: 1, max: 6 },
        label: { normal: { show: false } },
        itemStyle: {
          normal: { borderColor: '#fff', borderWidth: 1, areaColor: '#e8ecef' },
          emphasis: { areaColor: '#c6d8ff', label: { show: true, color: C.text } }
        },
        data,
        markPoint: { data: [], symbolSize: 30, zlevel: 10 }
      };

      // 丢包标记（保留原有"!"图钉）
      if (showLossMark.value) {
        for (const prov in mapDetailData.value) {
          const detail = mapDetailData.value[prov];
          let hasLoss = false;
          for (const tel of ['ctcc', 'cucc', 'cmcc']) {
            const cities = detail[tel];
            if (!cities) continue;
            for (const city in cities) {
              for (const ip of cities[city]) {
                if (ip.loss > 0) { hasLoss = true; break; }
              }
              if (hasLoss) break;
            }
            if (hasLoss) break;
          }
          if (hasLoss && PROV_COORDS[prov]) {
            series.markPoint.data.push({
              name: prov, coord: PROV_COORDS[prov], value: 0,
              // 红色小闪电（细长比例 + 18px，兼顾"看得出是闪电"与"不笨重"）
              symbol: LOSS_BOLT, symbolSize: 18,
              // markPoint 默认会显示数值标签（value=0 → 闪电上顶着一个"0"），必须显式关掉
              label: { show: false },
              emphasis: { label: { show: false } }
            });
          }
        }
      }
      selectedMarkPoint(series);

      // setOption 的第二个参数 notMerge=true 会整块替换配置，把地图的缩放/拖动位置一起重置
      // （10 秒刷新一次 → 地图会不停跳回原位）。这里先记住当前视野，渲染完再恢复。
      let keep = null;
      if (!pendingMapReset) {
        const cur = mainChart.getOption();
        const s0 = cur && cur.series && cur.series[0];
        if (s0 && (s0.zoom != null || s0.center != null)) keep = { zoom: s0.zoom, center: s0.center };
      }
      pendingMapReset = false;

      mainChart.setOption({
        backgroundColor: 'transparent',
        title: {
          text: nodeName.value || 'PingAtlas',
          subtext: lastCheckTime.value,
          left: 'center', top: 0,
          textStyle: { color: C.head, fontSize: 14, fontWeight: 600 },
          subtextStyle: { color: C.muted }
        },
        tooltip: {
          trigger: 'item', backgroundColor: 'transparent', borderWidth: 0, padding: 0,
          // enterable：允许鼠标移进提示框，长列表才能在框内滚动看全

          enterable: true,

          position(point, params, dom, rect, size) {
            // 用图表自身的宽高来夹取气泡位置（原来用 window.innerWidth，
            // 卡片 overflow:hidden 后靠右/靠下的气泡会被裁掉）
            const viewW = size.viewSize[0], viewH = size.viewSize[1];
            const contentW = size.contentSize[0], contentH = size.contentSize[1];
            let x = point[0] + 10;
            if (x + contentW > viewW) x = Math.max(4, point[0] - contentW - 10);
            let y = point[1] - contentH / 2;
            if (y + contentH > viewH) y = Math.max(10, viewH - contentH - 10);
            if (y < 0) y = 10;
            return [x, y];
          },
          formatter: (params) => {
            const d = params.data || {};
            if (d.targetAlias) {
              const r = rows.value.find(x => x.alias === d.targetAlias);
              return r ? targetTooltip(r) : '';
            }
            return ''; // 省份不再在地图上弹提示：明细改为显示在右侧「区域/运营商」卡片里
          }
        },
        series
      }, true);
      // 恢复刷新前的缩放/位置（点"复位"时不恢复，交回默认视野）
      if (keep) {
        mainChart.setOption({ series: [{ zoom: keep.zoom, center: keep.center }] });
      }
    }

    // 点"复位" → 下一次渲染不要恢复旧视野，回到默认的 1.2 倍居中
    let pendingMapReset = false;
    function resetMapView() { pendingMapReset = true; renderMap(); }

    async function fetchData() {
      try {
        const q = queryString();
        const [mapRes, detailRes, matrixRes] = await Promise.all([
          fetch('/api/mapping.json' + q),
          fetch('/api/mapping_detail.json' + q),
          fetch('/api/matrix.json' + windowQuery())
        ]);
        if (matrixRes.status === 400 && nodeScope.value) {
          // 指定的节点已被删除/名字拼错：回到"全网最优"，避免留下空白页
          nodeScope.value = '';
          syncURL();
          return fetchData();
        }
        if (!mapRes.ok || !detailRes.ok || !matrixRes.ok) {
          throw new Error('HTTP ' + mapRes.status + ' / ' + detailRes.status + ' / ' + matrixRes.status);
        }
        const [mapJson, detailJson, matrixJson] = await Promise.all([
          mapRes.json(), detailRes.json(), matrixRes.json()
        ]);
        nodeName.value = mapJson.text || '';
        lastCheckTime.value = mapJson.subtext || '';
        mapDelayData.value = mapJson.avgdelay || {};
        mapDetailData.value = detailJson || {};

        rows.value = matrixJson.rows || [];
        totalRows.value = matrixJson.total || rows.value.length;
        byTelecom.value = matrixJson.byTelecom || [];
        byRegion.value = matrixJson.byRegion || [];
        matrixAsOf.value = matrixJson.as_of || '';  // 注意 API 字段是 snake_case 的 as_of
        cards.value = matrixJson.nodes || [];
        nodeOptions.value = cards.value.map(c => ({ id: c.id, label: c.label }));
        clampPage();
        loading.value = false;
        lastSuccessTime = new Date().toLocaleTimeString();
        lastUpdate.value = '更新于 ' + lastSuccessTime;
        calcHealth();
        renderMap();
      } catch (e) {
        console.error('fetch error', e);
        loading.value = false;
        lastUpdate.value = lastSuccessTime
          ? '数据获取失败（最后成功 ' + lastSuccessTime + '）'
          : '数据获取失败';
      }
    }

    function refreshData() { fetchData(); }
    function setViewMode(mode) { viewMode.value = mode; renderMap(); }

    // ---- 选中结果行：只显示该 IP 的曲线 ----
    function selectRow(r) {
      selectedAlias.value = selectedAlias.value === r.alias ? '' : r.alias;
      renderMap();
      if (selectedAlias.value) showIpChart(r);
      else closeProvinceChart();
    }

    // ---- 导出 CSV（跟随当前视图）----
    function exportCSV() {
      let lines;
      if (resultView.value === 'flat') {
        lines = [['检测点', '响应IP', 'IP位置', '运营商', '延迟(ms)', '丢包(%)', '发包', '收包', '时刻'].join(',')];
        for (const c of flatRows.value) {
          lines.push([c.label, c.ip, locText(c.row), TEL_NAMES[c.row.telecom] || c.row.telecom, c.delay, c.loss, c.send, c.recv, c.logtime]
            .map(v => '"' + String(v === undefined || v === null ? '' : v).replace(/"/g, '""') + '"').join(','));
        }
      } else {
        const head = ['IP位置', '运营商', '探测IP'];
        for (const c of cards.value) head.push(c.label + '(ms)');
        lines = [head.join(',')];
        for (const r of displayRows.value) {
          const cells = cards.value.map(c => {
            const d = cellDelay(r, c.id);
            const l = cellLoss(r, c.id);
            if (d < 0) return '';
            const base = isTimeout(d) ? '超时' : String(d);
            return l > 0 ? (base + ' 丢' + l + '%') : base;
          });
          const row = [locText(r), TEL_NAMES[r.telecom] || r.telecom, r.ip].concat(cells);
          lines.push(row.map(v => '"' + String(v === undefined || v === null ? '' : v).replace(/"/g, '""') + '"').join(','));
        }
      }
      const blob = new Blob(['\ufeff' + lines.join('\r\n')], { type: 'text/csv;charset=utf-8' });
      const a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = 'pingatlas-' + resultView.value + '-' + new Date().toISOString().slice(0, 16).replace(/[:T]/g, '') + '.csv';
      document.body.appendChild(a);
      a.click();
      document.body.removeChild(a);
      URL.revokeObjectURL(a.href);
    }

    // ---- 历史曲线：每个目的 IP 一张图，图里【每个监测点一条线】（不做任何合并）----
    // focusIp 非空 = 只看这一个 IP；为空 = 看该省全部 IP（每张图同样是分监测点分线）
    const NODE_COLORS = ['#4680ff', '#9ccc65', '#ffba57', '#6f42c1', '#00acc1', '#ff5252', '#fd7e14', '#20c997'];
    let provRaw = null;   // 最近一次"按监测点分别取回"的原始数据（切换运营商筛选时直接重画，不再请求）

    function showProvinceChart(province) {
      focusIp.value = '';
      currentProvince.value = province;
      provError.value = '';
      provinceVisible.value = true;
      nextTick(() => loadProvinceHistory());
    }

    // 点结果表某一行：只看这个 IP
    function showIpChart(row) {
      focusIp.value = row.ip || '';
      currentProvince.value = row.province || currentProvince.value;
      provError.value = '';
      provinceVisible.value = true;
      nextTick(() => loadProvinceHistory());
    }

    // 从"单 IP"切回"全省"
    function clearFocusIp() {
      focusIp.value = '';
      renderProvinceChart();
    }

    function closeProvinceChart() {
      provinceVisible.value = false;
      disposeIpCharts();
      provIpList.value = [];
    }

    function disposeIpCharts() {
      ipCharts.forEach(c => { if (c) c.dispose(); });
      ipCharts = [];
      ipChartEls = [];
    }

    // 每个监测点各取一次（?node=），随后按 IP 聚合：一个 IP 一张图、图里 N 条线
    async function loadProvinceHistory() {
      if (!currentProvince.value) return;
      const prov = currentProvince.value;
      const hours = timeRange.value;
      // 曲线始终按全部监测点取数：点节点不影响下方对比结果
      const nodes = cards.value;
      const colorOf = {};
      nodes.forEach((n, i) => { colorOf[n.id] = NODE_COLORS[i % NODE_COLORS.length]; });
      const raw = [];
      let failed = 0;
      for (const n of nodes) {
        const url = '/api/province_history.json?province=' + encodeURIComponent(prov) +
          '&hours=' + hours + '&node=' + encodeURIComponent(n.id);
        try {
          const res = await fetch(url);
          if (!res.ok) { failed++; continue; }
          raw.push({ node: n, ips: ((await res.json()).ips) || [] });
        } catch (e) { failed++; }
      }
      if (raw.length === 0) { provError.value = '数据获取失败'; provIpList.value = []; return; }
      provRaw = { colorOf, raw };
      provError.value = failed ? (failed + ' 个监测点取数失败') : '';
      renderCharts();
    }

    function renderProvinceChart() { renderCharts(); }

    function renderCharts() {
      disposeIpCharts();
      if (!provRaw) { provIpList.value = []; return; }
      const { colorOf, raw } = provRaw;
      const byIp = new Map();
      for (const { node, ips } of raw) {
        for (const it of ips) {
          if (focusIp.value && it.ip !== focusIp.value) continue;
          if (provTelFilter.value !== 'all' && it.telecom !== provTelFilter.value) continue;
          let e = byIp.get(it.ip);
          if (!e) {
            e = { ip: it.ip, city: it.city, telecom: it.telecom, delay: -1, timeout: false,
                  history: [], loss: [], times: [], series: [] };
            byIp.set(it.ip, e);
          }
          e.series.push({
            name: node.label, color: colorOf[node.id],
            times: it.times || [], hist: it.history || [], loss: it.loss || []
          });
          // 徽章：取该 IP 各监测点最新的值里最好的一个；
          // 若该节点在这段时间只有超时记录（history 全是 null 但 times 非空）→ 标"超时"而不是"无数据"
          const hist = it.history || [];
          if ((it.times || []).length > 0 && hist.length > 0 && hist.every(v => v === null || v === undefined)) {
            e.timeout = true;
          }
          for (let k = hist.length - 1; k >= 0; k--) {
            const v = hist[k];
            if (v === null || v === undefined) continue;
            if (v > 0 && v < 2000) { if (e.delay < 0 || v < e.delay) e.delay = v; }
            else if (v === 0 || v >= 2000) { e.timeout = true; }
            break;
          }
        }
      }
      const out = [];
      for (const e of byIp.values()) {
        const tset = new Set();
        for (const s of e.series) for (const t of s.times) tset.add(t);
        e.times = Array.from(tset).sort();
        // 各监测点时标不完全一致 → 统一时间轴，缺失处 null（断线，不跨点连线）
        e.series.forEach(s => {
          const idx = new Map(s.times.map((t, i) => [t, i]));
          s.data = e.times.map(t => {
            if (!idx.has(t)) return null;
            const v = s.hist[idx.get(t)];
            return (v === undefined || v === null || v >= 2000) ? null : v;
          });
          s.lossData = e.times.map(t => {
            if (!idx.has(t)) return null;
            const v = s.loss[idx.get(t)];
            return (v === undefined || v === null || v < 0) ? null : v;
          });
        });
        if (e.delay < 0 && e.timeout) e.delay = 0;   // 徽章显示"超时"
        out.push(e);
      }
      out.sort((a, b) => (a.delay <= 0 ? 1e9 : a.delay) - (b.delay <= 0 ? 1e9 : b.delay));
      provIpList.value = out;
      if (out.length === 0) return;

      nextTick(() => {
        out.forEach((e, i) => {
          const el = ipChartEls[i];
          if (!el) return;
          const chart = echarts.init(el);
          ipCharts.push(chart);
          const pts = e.times.map(t => t.substring(5, 16));
          // 每个监测点：延迟（实线，左轴 ms）+ 丢包（同色虚线，右轴 %），两项都可关掉
          const series = [];
          for (const s of e.series) {
            if (showDelay.value) {
              series.push({
                name: s.name, type: 'line', yAxisIndex: 0, data: s.data, symbol: 'none', connectNulls: false,
                lineStyle: { color: s.color, width: 1.6 }, itemStyle: { color: s.color }
              });
            }
            if (showLoss.value) {
              series.push({
                name: s.name + ' 丢包', type: 'line', yAxisIndex: 1, data: s.lossData, symbol: 'none', connectNulls: false,
                lineStyle: { color: s.color, width: 1, type: 'dashed', opacity: 0.8 }, itemStyle: { color: s.color }
              });
            }
          }
          const yAxis = [{
            type: 'value', name: 'ms', min: 0, nameTextStyle: { color: C.muted },
            axisLabel: { color: C.muted, fontSize: 10 }, splitLine: { lineStyle: { color: '#f1f3f5' } }
          }];
          if (showLoss.value) {
            yAxis.push({
              type: 'value', name: '%', min: 0, max: 100, nameTextStyle: { color: C.muted },
              axisLabel: { color: C.muted, fontSize: 10 }, splitLine: { show: false }
            });
          }
          chart.setOption({
            backgroundColor: 'transparent',
            tooltip: {
              trigger: 'axis',
              backgroundColor: '#fff', borderColor: C.border, textStyle: { color: C.text, fontSize: 11 },
              extraCssText: 'box-shadow:0 2px 8px rgba(0,0,0,.12);',
              formatter: function(params) {
                if (!params || !params.length) return '';
                // 同一个监测点的延迟 + 丢包合并到同一行（而不是每条曲线一行）
                const groups = new Map();
                for (const p of params) {
                  const isLoss = /丢包/.test(p.seriesName);
                  const node = p.seriesName.replace(/\s*丢包$/, '');
                  let g = groups.get(node);
                  if (!g) { g = { node: node, marker: p.marker, delay: null, loss: null }; groups.set(node, g); }
                  const miss = (p.value === null || p.value === undefined || (typeof p.value === 'number' && isNaN(p.value)));
                  const val = miss ? null : p.value;
                  if (isLoss) g.loss = val; else g.delay = val;
                }
                let result = '<b>' + esc(e.times[params[0].dataIndex] || '') + '</b><br/>';
                for (const g of groups.values()) {
                  const parts = [];
                  if (showDelay.value) parts.push(g.delay === null ? '延迟 无数据' : g.delay + 'ms');
                  if (showLoss.value) parts.push(g.loss === null ? '丢包 无数据' : '丢包 ' + g.loss + '%');
                  // 用 &nbsp; 而不是普通空格：tooltip 是 HTML，连续空格会被折叠成一个
                  result += g.marker + g.node + '&nbsp;&nbsp;' + parts.join('&nbsp;&nbsp;') + '<br/>';
                }
                return result;
              }
            },
            legend: { data: series.map(s => s.name), textStyle: { color: C.dim, fontSize: 11 }, top: 0, right: 0 },
            dataZoom: [{ type: 'inside', start: 0, end: 100 }],
            grid: { left: 45, right: showLoss.value ? 45 : 20, top: 30, bottom: 35 },
            xAxis: {
              type: 'category', data: pts, boundaryGap: false,
              axisLabel: { rotate: 0, interval: 'auto', showMinLabel: true, showMaxLabel: true, color: C.muted, fontSize: 10, formatter: shortTime },
              axisLine: { lineStyle: { color: C.border } }
            },
            yAxis,
            graphic: series.length === 0 ? [{
              type: 'text', left: 'center', top: 'middle',
              style: { text: '请在右上角勾选「延迟」或「丢包」', fill: C.muted, fontSize: 12 }
            }] : [],
            series
          });
        });
      });
    }

    let resizeObserver = null;

    function handleResize() {
      nextTick(() => {
        if (mainChart) mainChart.resize();
        ipCharts.forEach(c => { if (c) c.resize(); });
      });
    }

    onMounted(() => {
      // 自定义下拉：点空白处/按 Esc 收起
      document.addEventListener('click', () => { dd.value = ''; });
      window.addEventListener('keydown', (e) => { if (e.key === 'Escape') dd.value = ''; });
      nextTick(() => {
        mainChart = echarts.init(mapEl.value);
        mainChart.on('click', (params) => {
          const d = params.data || {};
          if (d.targetAlias) { return; }                          // 点"选中"图钉：不做事
          if (params.name && mapDetailData.value[params.name]) {
            togglePinProvince(params.name);                       // ① 固定/取消固定右侧明细面板
            filterByProvince(params.name);                        // ② 同时按该省过滤下方结果表（原有逻辑）
          }
        });
        // 悬停省份 → 右侧预览；移出地图延迟 400ms 再清空（够把鼠标移到面板上滚动/复制）
        mainChart.on('mouseover', (params) => {
          const d = params.data || {};
          if (d.targetAlias) return;
          if (params.name && mapDetailData.value[params.name]) {
            cancelHoverClear();
            hoverProv.value = params.name;
          }
        });
        mainChart.on('globalout', () => { scheduleHoverClear(); });
        // 悬停省份 → 把该省明细送到右侧「区域/运营商」卡片；移出地图 → 恢复全国汇总
        mainChart.on('mouseover', (params) => {
          const d = params.data || {};
          if (d.targetAlias) return;                              // 悬停"选中目标"图钉时不切换
          if (params.name && mapDetailData.value[params.name]) hoverProv.value = params.name;
        });
        mainChart.on('globalout', () => { hoverProv.value = ''; });
        if (window.ResizeObserver && mapEl.value) {
          resizeObserver = new ResizeObserver(() => handleResize());
          resizeObserver.observe(mapEl.value);
        }
        fetchData();
        refreshTimer = setInterval(fetchData, 60000);
        window.addEventListener('resize', handleResize);
      });
    });

    onUnmounted(() => {
      if (refreshTimer) clearInterval(refreshTimer);
      if (resizeObserver) resizeObserver.disconnect();
      window.removeEventListener('resize', handleResize);
      if (mainChart) mainChart.dispose();
      disposeIpCharts();
    });

    return {
      mapEl, provChartEl, loading, viewMode, showLossMark, lastUpdate, lastCheckTime, nodeName,
      bands: BANDS, bandsDesc: BANDS_DESC, cellColor, allStat,
      nodeScope, winMode, nodeOptions, cards, scopeLabel, setNodeScope, toggleNode, setWindow,
      mapDetailData, provinceVisible, currentProvince, provTelFilter, timeRange, provIpList, provError,
      healthOk, healthWarn, healthBad, healthTotal,
      rows, totalRows, byTelecom, byRegion, byTelecomView, byRegionView, matrixAsOf, selectedAlias,
      searchQ, telFilter, provFilter, regionFilter, onlyDiff, onlyTimeout,
      provinceOptions, displayRows, pagedRows, page, pageCount, pageSize, rowsForPage,
      pageSizeOpts: PAGE_SIZE_OPTS, pageSizeLabel, pickPageSize, showPager,
      resultView, setResultView, flatRows, pagedFlat, ipFilter, ipOptions, whereOf, locText, nodeTitle, sorts: SORTS, applySort, sortKey, sortOrder,
      setSort, sortMark, onSearchInput, clearFilters, filterByRegion, prevPage, nextPage, jumpPage, doJump,
      cellDelay, cellLoss, rowLogtime, fmt0, extText, extValue, extNode, avgBarW, hasSpark, sparkPathOf,
      refreshData, setViewMode, showProvinceChart, closeProvinceChart, loadProvinceHistory, renderCharts,
      focusIp, showDelay, showLoss, showIpChart, clearFocusIp, filterByProvince, filterByTelecom,
      dd, toggleDd, closeDd, pickIp, pickSort, pickProv, pickTel, pickTime,
      sortLabel, ipLabel, provLabel, telLabel, timeLabel, telOpts: TEL_OPTS, timeOpts: TIME_OPTS,
      renderProvinceChart, renderMap, resetMapView, delayColor, formatDelay, telName, setIpChartRef,
      ipBadgeClass, ipBadgeText, selectRow, exportCSV,
      hoverProv, pinnedProv, activeProv, hoverRows, hoverList, hoverStat,
      togglePinProvince, closeProvincePanel, cancelHoverClear
    };
  }
};

createApp(App).mount('#app');
