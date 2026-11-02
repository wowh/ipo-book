'use strict';

// 打新账本前端。所有数据来自本地 Go 服务，金额以「分」为整数传输，
// 费率以百万分之一为单位传输，展示与输入时再换算，避免浮点误差。

const state = {
  snapshot: null,
  // stats 是按 statFilter 取回的收益统计，和快照分开取：筛选取变只重取统计。
  stats: null,
  statFilter: { year: 0, month: 0, brokerId: 0 },
  scope: 'all',
  search: '',
  confirmDelete: false,
  confirmBrokerDelete: false,
  busy: false,
  lastBrokerId: null,
  // 记一笔时选中的新股：issuePick 是申购代码；issueFallback 兜住已不在列表里的老票。
  issuePick: null,
  issueFallback: null,
  // issueActive 是候选列表里键盘高亮的那一项，-1 表示没高亮。
  issueActive: -1,
  // issueComposing 标记中文输入法正在组字，此时输入框里还是拼音，不当作筛选关键词。
  issueComposing: false,
  // revision 是本地服务的数据版本号，用来发现「别的入口改过账本」。
  revision: null,
  // ai 是本地接口的接入信息，打开面板时读取。
  ai: null,
  showToken: false,
  confirmToken: false,
};

/* ---------- 与本地服务通信 ---------- */

async function call(method, ...args) {
  if (!window.mygo || typeof window.mygo.call !== 'function') {
    throw new Error('应用服务未就绪，请重新打开程序');
  }
  return window.mygo.call(method, ...args);
}

function errorText(err) {
  if (!err) return '操作失败';
  if (typeof err === 'string') return err;
  return err.message || String(err);
}

/* ---------- 金额与费率 ---------- */

// fixedToInt 把十进制文本按指定小数位数转成整数，全程走字符串，不经过浮点。
function fixedToInt(input, digits) {
  const raw = String(input ?? '').trim().replace(/,/g, '');
  if (raw === '') return 0;
  const match = /^(\d+)(?:\.(\d*))?$/.exec(raw);
  if (!match) return NaN;
  const frac = ((match[2] || '') + '0'.repeat(digits)).slice(0, digits);
  const whole = Number(match[1]);
  if (!Number.isSafeInteger(whole)) return NaN;
  return whole * Math.pow(10, digits) + Number(frac || '0');
}

function fixedText(value, digits) {
  const v = Number(value) || 0;
  const negative = v < 0;
  const abs = Math.abs(v);
  const scale = Math.pow(10, digits);
  const whole = Math.floor(abs / scale);
  if (digits === 0) return (negative ? '-' : '') + String(whole);
  let frac = String(abs % scale).padStart(digits, '0');
  if (frac !== '') frac = frac.replace(/0+$/, '');
  return (negative ? '-' : '') + String(whole) + (frac ? '.' + frac : '');
}

// 元 ↔ 分
const yuanToFen = (input) => fixedToInt(input, 2);

// fenToYuan 固定两位小数：金额不该出现 1.2 这种尾巴不齐的写法。
function fenToYuan(fen) {
  const value = Number(fen) || 0;
  const negative = value < 0;
  const abs = Math.abs(value);
  return (negative ? '-' : '') + Math.floor(abs / 100) + '.' + String(abs % 100).padStart(2, '0');
}
// 万分之 ↔ ppm（万分之 2.5 = 250 ppm）
const wanToPpm = (input) => fixedToInt(input, 2);
const ppmToWan = (ppm) => fixedText(ppm, 2);
// 百分数 ↔ ppm（3.85% = 38500 ppm）
const pctToPpm = (input) => fixedToInt(input, 4);
const ppmToPct = (ppm) => fixedText(ppm, 4);

// fenToYuanGrouped 在元的基础上加千分位和 ¥，用于列表里的申购金额。
//
// 申购金额动辄几十上百万，不加千分位读不出量级；股数那一列本来就有千分位，
// 两种数字排在一起也才齐整。
function fenToYuanGrouped(fen) {
  const value = Number(fen) || 0;
  const negative = value < 0;
  const abs = Math.abs(value);
  const yuan = Math.floor(abs / 100).toLocaleString('zh-CN');
  return (negative ? '-' : '') + '¥' + yuan + '.' + String(abs % 100).padStart(2, '0');
}

function fenToSignedYuan(fen) {
  const value = Number(fen) || 0;
  if (value === 0) return '0.00';
  return (value > 0 ? '+' : '') + fenToYuan(value);
}

function bpsText(bps) {
  const value = Number(bps) || 0;
  return (value / 100).toFixed(2) + '%';
}

function sharesText(shares) {
  const value = Number(shares) || 0;
  if (value === 0) return '—';
  return value.toLocaleString('zh-CN');
}

// shortDate 把 2026-10-20 显示成 26-10-20。
//
// 打新看的是「哪一天」，年份两位数足够区分，还能省下三个字符的列宽；
// 完整日期放在单元格的 title 里，鼠标停上去就能看全。
function shortDate(day) {
  if (!day) return '';
  return day.length >= 10 ? day.slice(2) : day;
}

function escapeHtml(text) {
  return String(text ?? '').replace(/[&<>"']/g, (ch) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  })[ch]);
}

function today() {
  const now = new Date();
  const pad = (n) => String(n).padStart(2, '0');
  return `${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())}`;
}

/* ---------- 提示 ---------- */

let toastTimer = null;

function toast(message, kind = 'info', duration = 3200) {
  const el = document.getElementById('toast');
  el.textContent = message;
  el.className = 'toast ' + kind;
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.hidden = true; }, duration);
}

/* ---------- 数据加载 ---------- */

async function loadSnapshot() {
  try {
    state.snapshot = await call('Service.Snapshot');
    // 统计单独取：它带着筛选条件，快照里那份汇总只对应「全部」。
    await loadStats();
    render();
  } catch (err) {
    toast('读取账本失败：' + errorText(err), 'error', 6000);
    return;
  }
  // 顺带记下版本号，之后的轮询以它为基准，自己的操作不会触发「外部更新」提示。
  try {
    state.revision = await call('Service.Revision');
  } catch (err) {
    state.revision = null;
  }
}

async function refreshIssues() {
  if (state.busy) return;
  state.busy = true;
  const btn = document.getElementById('btn-refresh');
  btn.disabled = true;
  btn.textContent = '同步中…';
  try {
    const result = await call('Service.Refresh', state.scope);
    const parts = [`已同步 ${result.count} 只新股`];
    if (result.added > 0) parts.push(`新增 ${result.added} 只`);
    parts.push('来源：' + result.meta.sourceLabel);
    toast(parts.join('，'), result.meta.fallback ? 'warn' : 'ok', result.meta.fallback ? 6000 : 2600);
    await loadSnapshot();
  } catch (err) {
    toast(errorText(err), 'error', 6000);
  } finally {
    state.busy = false;
    btn.disabled = false;
    btn.textContent = '同步新股';
  }
}

/* ---------- 渲染 ---------- */

function render() {
  const snap = state.snapshot;
  if (!snap) return;
  renderSourceLine(snap.meta);
  renderIssues(snap);
  renderEntries(snap);
  renderBrokers(snap);
  renderStats();
  refreshOpenIssueDrawer(snap);
  document.getElementById('entry-count').textContent = String(snap.summary ? snap.summary.entryCount : 0);
  document.getElementById('broker-count').textContent = String((snap.brokers || []).length);
}

function renderSourceLine(meta) {
  const el = document.getElementById('source-line');
  if (!meta) {
    el.textContent = '本地账本';
    return;
  }
  const bits = [];
  if (meta.issuesUpdatedAt) {
    const d = new Date(meta.issuesUpdatedAt);
    const stamp = isNaN(d.getTime())
      ? meta.issuesUpdatedAt
      : `${d.getMonth() + 1}月${d.getDate()}日 ${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
    bits.push(`新股数据更新于 ${stamp}`);
  } else {
    bits.push('尚未同步新股数据');
  }
  if (meta.sourceLabel) bits.push(`来源 ${meta.sourceLabel}`);
  el.textContent = bits.join(' · ');
  if (meta.fallback && meta.warning) {
    const warn = document.createElement('span');
    warn.className = 'warn';
    warn.textContent = ' · ' + meta.warning;
    el.appendChild(warn);
  }
}

function issueStage(issue) {
  const listDate = issue.listingDate || '';
  if (!listDate) return { key: 'issuing', label: '发行中', cls: 'tag tag-on' };
  if (listDate >= today()) return { key: 'issuing', label: '待上市', cls: 'tag tag-on' };
  return { key: 'done', label: '已上市', cls: 'tag tag-done' };
}

function renderIssues(snap) {
  const body = document.getElementById('issue-body');
  const empty = document.getElementById('issue-empty');
  const issues = snap.issues || [];
  const recorded = new Set((snap.entries || []).map((e) => e.fxCode));
  const keyword = state.search.trim().toLowerCase();

  const rows = issues.filter((issue) => {
    if (state.scope !== 'all' && issueStage(issue).key !== state.scope) return false;
    if (!keyword) return true;
    return (issue.name || '').toLowerCase().includes(keyword)
      || (issue.fxCode || '').includes(keyword)
      || (issue.stockCode || '').includes(keyword);
  });

  body.innerHTML = rows.map((issue) => {
    const stage = issueStage(issue);
    const price = issue.priceFen > 0 ? fenToYuan(issue.priceFen) : '<span class="muted">待定</span>';
    const amount = issue.issueShares > 0
      ? (issue.issueShares / 10000).toLocaleString('zh-CN', { maximumFractionDigits: 2 }) + ' 万股'
      : '<span class="muted">—</span>';
    const done = recorded.has(issue.fxCode);
    const action = done
      ? '<button class="row-btn quiet" data-view-issue="' + escapeHtml(issue.fxCode) + '">查看</button>'
      : '<button class="row-btn" data-add-issue="' + escapeHtml(issue.fxCode) + '">记一笔</button>';
    return `<tr>
      <td class="code">${escapeHtml(issue.fxCode)}</td>
      <td class="name">${escapeHtml(issue.name)}</td>
      <td class="num">${price}</td>
      <td class="num muted">${amount}</td>
      <td class="muted" title="${escapeHtml(issue.purchaseDate)}">${escapeHtml(shortDate(issue.purchaseDate)) || '—'}</td>
      <td class="muted" title="${escapeHtml(issue.resultDate)}">${escapeHtml(shortDate(issue.resultDate)) || '—'}</td>
      <td class="muted" title="${escapeHtml(issue.listingDate)}">${escapeHtml(shortDate(issue.listingDate)) || '—'}</td>
      <td class="col-action"><span class="row-actions"><span class="${stage.cls}">${stage.label}</span>${action}</span></td>
    </tr>`;
  }).join('');

  empty.hidden = rows.length > 0;
  if (issues.length > 0 && rows.length === 0) {
    empty.hidden = false;
    empty.innerHTML = '<p>没有符合条件的新股</p><p class="hint">换个筛选条件或搜索词试试</p>';
  } else if (rows.length > 0) {
    empty.hidden = true;
  }
}

function renderEntries(snap) {
  const body = document.getElementById('entry-body');
  const empty = document.getElementById('entry-empty');
  const entries = snap.entries || [];

  body.innerHTML = entries.map((view) => {
    const realized = Number(view.realizedFen) || 0;
    const realizedCls = realized > 0 ? 'profit' : realized < 0 ? 'loss' : 'muted';
    const realizedText = view.soldShares > 0 ? fenToSignedYuan(realized) : '<span class="muted">—</span>';
    const tagCls = view.allottedShares > 0 ? 'tag tag-win' : 'tag';
    const saleCount = Number(view.saleCount) || 0;
    // 卖出明细放在状态标签的悬浮提示里，列表不占更多列。
    const saleTitle = saleCount > 0
      ? (view.sales || []).map((s) => `${s.date || ''} ${sharesText(s.shares)}股 @${fenToYuan(s.priceFen)}`).join('；')
      : '';
    const holding = view.holdingShares > 0
      ? `<span class="muted"> 持 ${sharesText(view.holdingShares)}</span>` : '';
    // 申购金额 = 发行价 × 申购股数，和股数挤在同一格：上排金额、下排股数。
    // 单独开一列会把表格撑宽，而这两项本来就是一回事——占多少钱、打了多少股。
    // 金额才是记账真正关心的一栏（占用多少资金、资金成本按它算），所以放在主行；
    // 股数是核对用的明细，放次行。
    const applyShares = Number(view.applyShares) || 0;
    const applyFunds = Number(view.applyFundsFen) || 0;
    const applyTitle = applyShares > 0 && Number(view.issuePriceFen) > 0
      ? `申购 ${sharesText(applyShares)} 股 × ${fenToYuan(view.issuePriceFen)} 元 = ${fenToYuanGrouped(applyFunds)}`
      : '';
    // 金额未知时（老记录没填发行价）退回只显示股数，不要把 0 当成金额写出来。
    const applyMain = applyFunds > 0 ? fenToYuanGrouped(applyFunds) : sharesText(applyShares);
    const applySub = applyFunds > 0 && applyShares > 0 ? sharesText(applyShares) + ' 股' : '';
    const applyCell = applyShares > 0 || applyFunds > 0
      ? `<span class="cell-main">${applyMain}</span>` +
        (applySub ? `<span class="cell-sub">${applySub}</span>` : '')
      : '<span class="muted">—</span>';
    const capital = Number(view.capitalCostFen) || 0;
    const capitalText = capital > 0
      ? fenToYuan(capital) + (view.capitalCostEstimated ? '<span class="est" title="按账户年化利率与冻款天数估算">估</span>' : '')
      : '<span class="muted">—</span>';
    return `<tr>
      <td class="col-name">
        <span class="name">${escapeHtml(view.name)}</span>
        <span class="code"> ${escapeHtml(view.fxCode)}</span>
      </td>
      <td class="muted col-account">${escapeHtml(view.account)}</td>
      <td class="num col-apply" title="${escapeHtml(applyTitle)}">${applyCell}</td>
      <td class="num col-lot">${sharesText(view.allottedShares)}${holding}</td>
      <td class="num">${Number(view.allottedShares) > 0 ? fenToYuan(view.costFen) : '<span class="muted">—</span>'}</td>
      <td class="num muted">${capitalText}</td>
      <td class="num ${realizedCls}">${realizedText}</td>
      <td><span class="${tagCls}" title="${escapeHtml(saleTitle)}">${escapeHtml(view.status)}${saleCount > 1 ? ` · ${saleCount} 笔` : ''}</span></td>
      <td class="col-action"><button class="row-btn quiet" data-edit-entry="${view.id}">编辑</button></td>
    </tr>`;
  }).join('');

  empty.hidden = entries.length > 0;
}

function renderBrokers(snap) {
  const body = document.getElementById('broker-body');
  const empty = document.getElementById('broker-empty');
  const brokers = snap.brokers || [];
  const entries = snap.entries || [];

  // 每个账户的占用资金与资金成本按记录累加。
  const stats = new Map();
  for (const e of entries) {
    const acc = stats.get(e.brokerId) || { count: 0, funds: 0, capital: 0 };
    acc.count += 1;
    acc.funds += Number(e.applyFundsFen) || 0;
    acc.capital += Number(e.capitalCostFen) || 0;
    stats.set(e.brokerId, acc);
  }

  body.innerHTML = brokers.map((b) => {
    const s = stats.get(b.id) || { count: 0, funds: 0, capital: 0 };
    const label = b.account && b.account !== b.name ? `${b.name} · ${b.account}` : b.name;
    return `<tr>
      <td class="name">${escapeHtml(label)}</td>
      <td class="num muted">${b.commissionPpm > 0 ? '万分之 ' + ppmToWan(b.commissionPpm) : '—'}</td>
      <td class="num muted">${b.minCommissionFen > 0 ? fenToYuan(b.minCommissionFen) : '—'}</td>
      <td class="num">${b.annualRatePpm > 0 ? ppmToPct(b.annualRatePpm) + '%' : '—'}</td>
      <td class="num muted">${s.count || '—'}</td>
      <td class="num muted">${s.funds > 0 ? fenToYuanGrouped(s.funds) : '—'}</td>
      <td class="num">${s.capital > 0 ? fenToYuan(s.capital) : '—'}</td>
      <td class="col-action"><button class="row-btn quiet" data-edit-broker="${b.id}">编辑</button></td>
    </tr>`;
  }).join('');

  if (brokers.length > 0) {
    empty.hidden = true;
  } else {
    empty.hidden = false;
    empty.innerHTML = '<p>还没有券商账户</p><p class="hint">先建一个账户，填上佣金率和资金年化利率，记账时会自动估算成本</p>';
  }
}

/* ---------- 收益统计 ---------- */

// loadStats 按当前筛选取统计。请求带序号，连续切换筛选时只认最后一次的结果，
// 否则慢的那个请求回来会把界面刷回上一个筛选条件。
let statsSeq = 0;

async function loadStats() {
  const seq = ++statsSeq;
  try {
    const stats = await call('Service.Stats', state.statFilter);
    if (seq !== statsSeq) return;
    state.stats = stats;
    renderStatFilters();
    renderStats();
  } catch (err) {
    if (seq !== statsSeq) return;
    toast('统计失败：' + errorText(err), 'error', 6000);
  }
}

// renderStatFilters 铺开三个下拉框。年份与账户列的是账本里的全集，
// 选完一年还能切回别的年份。
function renderStatFilters() {
  const stats = state.stats;
  if (!stats) return;
  const filter = state.statFilter;

  const year = document.getElementById('stat-year');
  const yearOptions = ['<option value="0">全部年份</option>'];
  for (const value of stats.years || []) {
    yearOptions.push(`<option value="${value}">${value} 年</option>`);
  }
  year.innerHTML = yearOptions.join('');
  year.value = String(filter.year || 0);
  if (year.value === '') {
    // 选中的年份已经没有记录了（比如那条记录被删掉），退回「全部年份」。
    filter.year = 0;
    year.value = '0';
  }

  const month = document.getElementById('stat-month');
  if (month.options.length === 0) {
    const monthOptions = ['<option value="0">全部月份</option>'];
    for (let i = 1; i <= 12; i += 1) monthOptions.push(`<option value="${i}">${i} 月</option>`);
    month.innerHTML = monthOptions.join('');
  }
  month.value = String(filter.month || 0);

  const brokers = state.snapshot ? state.snapshot.brokers || [] : [];
  const broker = document.getElementById('stat-broker');
  const brokerOptions = ['<option value="0">全部账户</option>'];
  for (const item of brokers) {
    brokerOptions.push(`<option value="${item.id}">${escapeHtml(brokerLabel(item))}</option>`);
  }
  broker.innerHTML = brokerOptions.join('');
  broker.value = String(filter.brokerId || 0);
  if (broker.value === '') {
    filter.brokerId = 0;
    broker.value = '0';
  }

  document.getElementById('stat-reset').hidden = !(filter.year || filter.month || filter.brokerId);
}

// statScopeText 描述当前筛选范围，例如「2026 年 10 月 · 华泰证券 · 12 条记录」。
function statScopeText(summary) {
  const filter = state.statFilter;
  const bits = [];
  if (filter.year) bits.push(`${filter.year} 年`);
  if (filter.month) bits.push(`${filter.month} 月`);
  if (filter.brokerId) {
    const all = (state.snapshot && state.snapshot.brokers) || [];
    const found = all.find((item) => item.id === filter.brokerId);
    if (found) bits.push(brokerLabel(found));
  }
  const scope = bits.length > 0 ? bits.join(' · ') : '全部记录';
  return `${scope} · ${Number(summary.entryCount) || 0} 条记录`;
}

function renderStats() {
  const stats = state.stats;
  if (!stats) return;
  const summary = stats.summary || {};
  document.getElementById('stat-scope').textContent = statScopeText(summary);

  const realized = Number(summary.realizedFen) || 0;
  const realizedEl = document.getElementById('stat-realized');
  realizedEl.textContent = signedYuan(realized);
  realizedEl.className = 'stat-value ' + (realized > 0 ? 'profit' : realized < 0 ? 'loss' : '');

  const saleCount = Number(summary.saleCount) || 0;
  document.getElementById('stat-return').textContent = saleCount > 0
    ? `已实现收益率 ${bpsText(summary.returnBps)} · 卖出 ${saleCount} 笔 ${fenToYuanGrouped(summary.sellAmountFen || 0)}`
    : '卖出后显示收益率';

  const capital = Number(summary.capitalCostFen) || 0;
  document.getElementById('stat-capital').textContent = fenToYuan(capital);
  const estimated = Number(summary.estimatedCapitalFen) || 0;
  document.getElementById('stat-capital-foot').textContent = capital > 0
    ? (estimated > 0 ? `其中 ${fenToYuan(estimated)} 为按利率估算` : '全部为实际发生额')
    : '在账户里填写资金年化利率后可见';

  const annual = Number(summary.annualizedBps) || 0;
  const annualEl = document.getElementById('stat-annual');
  annualEl.textContent = summary.capitalDays > 0 ? bpsText(annual) : '—';
  annualEl.className = 'stat-value ' + (annual > 0 ? 'profit' : annual < 0 ? 'loss' : '');
  document.getElementById('stat-annual-foot').textContent = summary.capitalDays > 0
    ? '按实际占用资金天数折算'
    : '记录申购资金与冻款天数后可见';

  document.getElementById('stat-allot').textContent =
    `${Number(summary.allottedIssues) || 0} / ${Number(summary.appliedIssues) || 0} 只`;

  const winRate = summary.appliedIssues > 0 ? `中签率 ${bpsText(summary.winRateBps)}` : '记录申购后显示中签率';
  document.getElementById('stat-winrate').textContent =
    winRate + (summary.allottedShares > 0 ? `，累计 ${sharesText(summary.allottedShares)} 股` : '');

  document.getElementById('stat-holding').textContent = fenToYuan(summary.holdingCostFen || 0);
  document.getElementById('stat-holding-shares').textContent =
    summary.holdingShares > 0 ? `未卖出 ${sharesText(summary.holdingShares)} 股` : '当前无持仓';

  const totalFee = (Number(summary.sellFeeFen) || 0) + (Number(summary.otherFeeFen) || 0);
  document.getElementById('stat-fee').textContent = fenToYuan(totalFee);
  document.getElementById('stat-fee-foot').textContent =
    `卖出费用 ${fenToYuan(summary.sellFeeFen || 0)} · 其他 ${fenToYuan(summary.otherFeeFen || 0)}`;

  renderMonthList(stats.byMonth || []);
  renderDetail(stats.details || [], summary);
  renderYearTable(stats.byYear || [], summary);
  renderAccountTable(summary.byAccount || [], summary);
}

// renderMonthList 把每月净收益画成条形，一眼看出收益集中在哪几个月。
//
// 这份列表是「按月翻账本」的入口，只跟账户筛选走：年份与月份都不参与。
// 点了 2026 年 10 月，上面的年份筛选会跟着变成 2026（不然「这个月」是哪一个
// 就不确定了），但别的年份的月份必须还留在列表里——否则想再看去年那个月，
// 只能先点「重置」，等于是把按月浏览的路堵死了。
function renderMonthList(months) {
  const list = document.getElementById('month-list');
  const empty = document.getElementById('month-empty');
  empty.hidden = months.length > 0;
  // 没有数据时连空表格一起收起来，只留一句提示。
  list.hidden = months.length === 0;
  // 列表只露出四行多的位置，标题上标总数，剩下的往下滚。
  const counted = months.filter((item) => !item.undated).length;
  document.getElementById('month-count').textContent = counted > 0 ? ` ${counted} 个月` : '';
  if (months.length === 0) {
    list.innerHTML = '';
    return;
  }
  const max = Math.max(1, ...months.map((item) => Math.abs(Number(item.realizedFen) || 0)));
  const filter = state.statFilter;
  // 高亮要对到具体的某个月：只比月份的话，筛 10 月会把每个年份的 10 月都点亮。
  const activeKey = filter.year && filter.month
    ? `${filter.year}-${String(filter.month).padStart(2, '0')}`
    : '';

  // 每个月归到自己的年份下面，年度小标题插在一年的头一次出现处。
  const perYear = new Map();
  for (const item of months) {
    if (!item.key) continue;
    const year = item.key.slice(0, 4);
    perYear.set(year, (perYear.get(year) || 0) + 1);
  }

  let currentYear = null;
  const rows = [];
  for (const item of months) {
    const net = Number(item.realizedFen) || 0;
    const cls = toneClass(net);
    const width = Math.round((Math.abs(net) / max) * 100);
    const parts = [`${item.entryCount} 条`];
    parts.push(item.allottedIssues > 0 ? `中签 ${item.allottedIssues} 只` : '未中签');
    parts.push(item.saleCount > 0
      ? `卖出 ${item.saleCount} 笔 ${fenToYuanGrouped(item.sellAmountFen || 0)}`
      : '未卖出');

    // 没填申购日的分组没有年份也没有月份：不插年度标题（它自己就写着），也不能点。
    const year = item.key ? item.key.slice(0, 4) : '';
    if (year && year !== currentYear) {
      currentYear = year;
      const yearActive = Number(year) === Number(filter.year) ? ' active' : '';
      const monthsInYear = perYear.get(year) || 0;
      rows.push(`<div class="month-year${yearActive}">` +
        `<span>${escapeHtml(year)} 年</span>` +
        `<em>${monthsInYear} 个月</em></div>`);
    }

    const active = item.key && item.key === activeKey ? ' active' : '';
    // 年份已经由上面的小标题交代过，行里只写月份，免得一行里年份重复两遍。
    const rowLabel = item.key ? `${Number(item.key.slice(5, 7))} 月` : item.label;
    const tag = item.key
      ? `<button class="month-row${active}" data-month="${escapeHtml(item.key)}" title="只看 ${escapeHtml(item.label)}">`
      : '<div class="month-row static">';
    const close = item.key ? '</button>' : '</div>';
    rows.push(`${tag}
      <span class="month-label">${escapeHtml(rowLabel)}<small>${escapeHtml(parts.join(' · '))}</small></span>
      <span class="bar-track"><span class="bar-fill ${cls}" style="width:${width}%"></span></span>
      <span class="month-value ${cls}">${signedYuan(net)}</span>
    ${close}`);
  }
  list.innerHTML = rows.join('');

  // 选中的那个月可能在列表底部：滚到它那儿，别让人以为筛选没生效。
  const activeRow = list.querySelector('.month-row.active');
  if (activeRow) {
    const offset = activeRow.getBoundingClientRect().top - list.getBoundingClientRect().top;
    list.scrollTop = Math.max(0, list.scrollTop + offset - 30);
  } else {
    list.scrollTop = 0;
  }
}

// renderDetail 铺开当前筛选下的逐条明细：这个月到底赚在哪几只票上。
//
// 这一块不做任何再计算，每行的数字和上面的卡片、月份条形出自同一份后端结果，
// 逐行相加正好等于「合计」那一行。点某个月只是换了筛选条件，明细跟着走。
function renderDetail(details, summary) {
  const body = document.getElementById('detail-body');
  const wrap = document.getElementById('detail-wrap');
  const empty = document.getElementById('detail-empty');
  const hint = document.getElementById('detail-hint');
  const rows = details || [];

  document.getElementById('detail-count').textContent = rows.length > 0 ? ` ${rows.length} 条` : '';
  empty.hidden = rows.length > 0;
  wrap.hidden = rows.length === 0;
  hint.textContent = rows.length === 0
    ? '这个范围里没有记录'
    : '点上面的某个月只看那个月；点某一行可改这条记录';

  if (rows.length === 0) {
    body.innerHTML = '';
    return;
  }

  const bodyRows = rows.map((row) => {
    const sold = Number(row.saleCount) || 0;
    const realized = Number(row.realizedFen) || 0;
    const allotted = Number(row.allottedShares) || 0;
    const funds = Number(row.applyFundsFen) || 0;
    const sell = sold > 0
      ? fenToYuanGrouped(row.sellAmountFen || 0) + (sold > 1 ? ` <span class="muted">${sold} 笔</span>` : '')
      : '<span class="muted">未卖出</span>';
    const net = sold > 0 ? fenToSignedYuan(realized) : '<span class="muted">—</span>';
    const rate = sold > 0 ? bpsText(row.returnBps) : '<span class="muted">—</span>';
    return `<tr class="detail-row" data-entry-id="${escapeHtml(row.entryId)}" title="点开修改这条记录">
      <td class="col-name">
        <span class="name">${escapeHtml(row.name || '未命名')}</span>
        <span class="code"> ${escapeHtml(row.fxCode || '')}</span>
      </td>
      <td class="muted col-account">${escapeHtml(row.account || '—')}</td>
      <td class="col-date">${escapeHtml(row.purchaseDate || '未填')}</td>
      <td class="num col-lot">${allotted > 0 ? sharesText(allotted) : '<span class="muted">未中签</span>'}</td>
      <td class="num col-apply">${funds > 0 ? fenToYuanGrouped(funds) : '<span class="muted">—</span>'}</td>
      <td class="num">${sell}</td>
      <td class="num ${sold > 0 ? toneClass(realized) : ''}">${net}</td>
      <td class="num ${sold > 0 ? toneClass(realized) : ''}">${rate}</td>
      <td class="col-detail-action"><button class="row-btn quiet" data-edit-entry="${escapeHtml(row.entryId)}">编辑</button></td>
    </tr>`;
  });

  // 合计行取 summary，不在这里把明细加一遍：同一个数只有一个来源。
  if (rows.length > 1) {
    const realized = Number(summary.realizedFen) || 0;
    bodyRows.push(`<tr class="total-row">
      <td class="name">合计</td>
      <td class="muted">${Number(summary.entryCount) || 0} 条</td>
      <td></td>
      <td class="num">${sharesText(summary.allottedShares)}</td>
      <td class="num">${fenToYuanGrouped(summary.applyFundsFen || 0)}</td>
      <td class="num">${fenToYuanGrouped(summary.sellAmountFen || 0)}</td>
      <td class="num ${toneClass(realized)}">${signedYuan(realized)}</td>
      <td class="num ${toneClass(realized)}">${Number(summary.saleCount) > 0 ? bpsText(summary.returnBps) : '—'}</td>
      <td class="col-detail-action"></td>
    </tr>`);
  }
  body.innerHTML = bodyRows.join('');
}

// renderYearTable 按申购年份汇总；筛到单一年份时不再重复一行「合计」。
function renderYearTable(buckets, summary) {
  const body = document.getElementById('year-body');
  document.getElementById('year-empty').hidden = buckets.length > 0;
  document.getElementById('year-table').hidden = buckets.length === 0;
  const rows = buckets.map((item) => `<tr>
      <td class="name">${escapeHtml(item.label)}</td>
      <td class="num muted">${item.entryCount}</td>
      <td class="num">${item.allottedIssues}</td>
      <td class="num muted">${item.applyFundsFen > 0 ? fenToYuanGrouped(item.applyFundsFen) : '—'}</td>
      <td class="num muted">${item.sellAmountFen > 0 ? fenToYuanGrouped(item.sellAmountFen) : '—'}</td>
      <td class="num ${toneClass(item.realizedFen)}">${signedYuan(item.realizedFen)}</td>
      <td class="num ${toneClass(item.realizedFen)}">${item.saleCount > 0 ? bpsText(item.returnBps) : '—'}</td>
    </tr>`);
  if (buckets.length > 1) {
    rows.push(`<tr class="total-row">
      <td class="name">合计</td>
      <td class="num muted">${summary.entryCount || 0}</td>
      <td class="num">${summary.allottedIssues || 0}</td>
      <td class="num muted">${fenToYuanGrouped(summary.applyFundsFen || 0)}</td>
      <td class="num muted">${fenToYuanGrouped(summary.sellAmountFen || 0)}</td>
      <td class="num ${toneClass(summary.realizedFen)}">${signedYuan(summary.realizedFen)}</td>
      <td class="num ${toneClass(summary.realizedFen)}">${bpsText(summary.returnBps)}</td>
    </tr>`);
  }
  body.innerHTML = rows.join('');
}

function renderAccountTable(accounts, summary) {
  const body = document.getElementById('account-body');
  document.getElementById('account-empty').hidden = accounts.length > 0;
  document.getElementById('account-table').hidden = accounts.length === 0;
  const rows = accounts.map((acc) => `<tr>
      <td class="name">${escapeHtml(acc.account)}</td>
      <td class="num muted">${acc.entryCount}</td>
      <td class="num">${acc.allottedIssues}</td>
      <td class="num muted">${acc.applyFundsFen > 0 ? fenToYuanGrouped(acc.applyFundsFen) : '—'}</td>
      <td class="num muted">${acc.capitalCostFen > 0 ? fenToYuan(acc.capitalCostFen) : '—'}</td>
      <td class="num ${toneClass(acc.realizedFen)}">${signedYuan(acc.realizedFen)}</td>
      <td class="num ${toneClass(acc.realizedFen)}">${acc.saleCount > 0 ? bpsText(acc.returnBps) : '—'}</td>
    </tr>`);
  if (accounts.length > 1) {
    rows.push(`<tr class="total-row">
      <td class="name">合计</td>
      <td class="num muted">${summary.entryCount || 0}</td>
      <td class="num">${summary.allottedIssues || 0}</td>
      <td class="num muted">${fenToYuanGrouped(summary.applyFundsFen || 0)}</td>
      <td class="num muted">${fenToYuan(summary.capitalCostFen || 0)}</td>
      <td class="num ${toneClass(summary.realizedFen)}">${signedYuan(summary.realizedFen)}</td>
      <td class="num ${toneClass(summary.realizedFen)}">${bpsText(summary.returnBps)}</td>
    </tr>`);
  }
  body.innerHTML = rows.join('');
}

// toneClass 按涨红跌绿的约定给金额上色。
function toneClass(fen) {
  const value = Number(fen) || 0;
  return value > 0 ? 'profit' : value < 0 ? 'loss' : 'muted';
}

function signedYuan(fen) {
  const value = Number(fen) || 0;
  if (value === 0) return '0.00';
  // 统计页的金额动辄几万几十万，统一带千分位；和其他页的净收益一样不加 ¥。
  const abs = Math.abs(value);
  const yuan = Math.floor(abs / 100).toLocaleString('zh-CN');
  return (value > 0 ? '+' : '-') + yuan + '.' + String(abs % 100).padStart(2, '0');
}

/* ---------- 打新记录表单 ---------- */

const drawer = document.getElementById('drawer');
const mask = document.getElementById('drawer-mask');
const form = document.getElementById('entry-form');

/* ---------- 卖出明细 ---------- */

// saleRowHTML 生成一张卖出的输入卡片。
//
// 卡片里既有「成交价」也有「成交金额」：交割单给的是金额，而均价往往除不尽，
// 两者都留着，填了金额就以金额为准，成交价只用于展示。
function saleRowHTML(sale = {}) {
  const id = sale.id ? String(sale.id) : '';
  const price = sale.priceFen > 0 ? fenToYuan(sale.priceFen) : '';
  const amount = sale.amountFen > 0 ? fenToYuan(sale.amountFen) : '';
  const fee = sale.feeFen > 0 ? fenToYuan(sale.feeFen) : '';
  return `<div class="sale-row" data-sale-id="${escapeHtml(id)}">
    <div class="sale-row-head">
      <span class="sale-index">第 <span class="sale-no"></span> 笔</span>
      <button type="button" class="sale-del">删除</button>
    </div>
    <div class="field-row">
      <label class="field">
        <span>成交日</span>
        <input class="sale-date" type="date" value="${escapeHtml(sale.date || '')}">
      </label>
      <label class="field">
        <span>股数</span>
        <input class="sale-shares" type="text" inputmode="numeric" placeholder="500" value="${sale.shares > 0 ? sale.shares : ''}">
      </label>
    </div>
    <div class="field-row">
      <label class="field">
        <span>成交价（元）</span>
        <input class="sale-price" type="text" inputmode="decimal" placeholder="27.38" value="${price}">
      </label>
      <label class="field">
        <span>成交金额（元）</span>
        <input class="sale-amount" type="text" inputmode="decimal" placeholder="选填" value="${amount}">
      </label>
    </div>
    <div class="field-row">
      <label class="field">
        <span>费用（元）</span>
        <input class="sale-fee" type="text" inputmode="decimal" placeholder="留空按佣金率估算" value="${fee}">
      </label>
      <label class="field">
        <span>备注</span>
        <input class="sale-note" type="text" placeholder="选填" value="${escapeHtml(sale.note || '')}">
      </label>
    </div>
  </div>`;
}

// renderSales 重画整个明细列表，用于打开抽屉时铺开已有的卖出。
function renderSales(sales) {
  const list = document.getElementById('sale-list');
  list.innerHTML = (sales || []).map((sale) => saleRowHTML(sale)).join('');
  renumberSales();
  updateSaleTotal();
}

// addSaleRow 在末尾追加一张空白卡片。
function addSaleRow() {
  const list = document.getElementById('sale-list');
  list.insertAdjacentHTML('beforeend', saleRowHTML({}));
  renumberSales();
  updateSaleTotal();
  const rows = list.querySelectorAll('.sale-row');
  const last = rows[rows.length - 1];
  if (last) last.querySelector('.sale-shares').focus();
}

// renumberSales 按当前顺序重排「第 N 笔」，删掉中间一笔后序号不留空。
function renumberSales() {
  document.querySelectorAll('#sale-list .sale-row').forEach((row, index) => {
    const no = row.querySelector('.sale-no');
    if (no) no.textContent = String(index + 1);
  });
}

// collectSales 把明细卡片读成数组，顺便挡住明显填错的。
//
// 整行都空着的卡片直接跳过：用户点了「加一笔」却没填，不该因此保存不了。
function collectSales() {
  const rows = Array.from(document.querySelectorAll('#sale-list .sale-row'));
  const sales = [];
  for (let i = 0; i < rows.length; i += 1) {
    const row = rows[i];
    const sharesRaw = row.querySelector('.sale-shares').value.trim();
    const priceRaw = row.querySelector('.sale-price').value.trim();
    const amountRaw = row.querySelector('.sale-amount').value.trim();
    const feeRaw = row.querySelector('.sale-fee').value.trim();
    const date = row.querySelector('.sale-date').value || '';
    if (!sharesRaw && !priceRaw && !amountRaw && !date) continue;

    const shares = Number(sharesRaw);
    if (!Number.isInteger(shares) || shares <= 0) {
      return { error: `第 ${i + 1} 笔卖出要填大于 0 的股数` };
    }
    const price = priceRaw === '' ? 0 : yuanToFen(priceRaw);
    const amount = amountRaw === '' ? 0 : yuanToFen(amountRaw);
    const fee = feeRaw === '' ? 0 : yuanToFen(feeRaw);
    if ([price, amount, fee].some(Number.isNaN)) {
      return { error: `第 ${i + 1} 笔卖出的价格或金额填写不正确` };
    }
    if (price === 0 && amount === 0) {
      return { error: `第 ${i + 1} 笔卖出要填成交价或成交金额` };
    }
    sales.push({
      id: Number(row.dataset.saleId) || 0,
      date,
      shares,
      priceFen: price,
      amountFen: amount,
      feeFen: fee,
      note: row.querySelector('.sale-note').value.trim(),
    });
  }
  return { sales };
}

// saleRowShares 读一张卡片的股数，空或非法按 0 算。
function saleRowShares(row) {
  const value = Number(row.querySelector('.sale-shares').value.trim());
  return Number.isInteger(value) && value > 0 ? value : 0;
}

// updateSaleTotal 在明细下面显示合计，填多了立刻标出来。
function updateSaleTotal() {
  const el = document.getElementById('sale-total');
  const rows = Array.from(document.querySelectorAll('#sale-list .sale-row'));
  let shares = 0;
  let amount = 0;
  let fee = 0;
  let filled = 0;
  for (const row of rows) {
    const count = saleRowShares(row);
    if (count === 0) continue;
    filled += 1;
    shares += count;
    const amountRaw = row.querySelector('.sale-amount').value.trim();
    const priceRaw = row.querySelector('.sale-price').value.trim();
    const amountFen = amountRaw !== ''
      ? Number(yuanToFen(amountRaw)) || 0
      : (Number(yuanToFen(priceRaw)) || 0) * count;
    amount += amountFen;
    const feeRaw = row.querySelector('.sale-fee').value.trim();
    if (feeRaw !== '') fee += Number(yuanToFen(feeRaw)) || 0;
  }
  if (filled === 0) {
    el.innerHTML = '<span class="muted">还没有卖出记录，仍持有</span>';
    return;
  }
  const allotted = Number(document.getElementById('f-allotted').value.trim()) || 0;
  const parts = [`共 <strong>${filled}</strong> 笔`, `<strong>${sharesText(shares)}</strong> 股`,
    `成交 <strong>${fenToYuan(amount)}</strong>`];
  if (fee > 0) parts.push(`费用 <strong>${fenToYuan(fee)}</strong>`);
  let text = parts.join(' · ');
  if (allotted > 0 && shares > allotted) {
    text += ` <span class="warn">超过中签 ${sharesText(allotted)} 股</span>`;
  }
  el.innerHTML = text;
}

function findIssue(fxCode) {
  const issues = (state.snapshot && state.snapshot.issues) || [];
  return issues.find((issue) => issue.fxCode === fxCode) || null;
}

function findEntry(id) {
  const entries = (state.snapshot && state.snapshot.entries) || [];
  return entries.find((entry) => String(entry.id) === String(id)) || null;
}

function brokers() {
  return (state.snapshot && state.snapshot.brokers) || [];
}

function findBroker(id) {
  return brokers().find((b) => String(b.id) === String(id)) || null;
}

function brokerLabel(b) {
  if (!b) return '';
  return b.account && b.account !== b.name ? `${b.name} · ${b.account}` : b.name;
}

function refreshBrokerOptions(selectedId) {
  const select = document.getElementById('f-broker');
  const list = brokers();
  if (list.length === 0) {
    select.innerHTML = '<option value="">（先建一个券商账户）</option>';
    return;
  }
  select.innerHTML = list.map((b) => {
    const value = String(b.id);
    const label = brokerLabel(b);
    return `<option value="${b.id}"${String(selectedId) === value ? ' selected' : ''}>${escapeHtml(label)}</option>`;
  }).join('');
}

/* ---------- 新股详情 ---------- */

const issueDrawer = document.getElementById('issue-drawer');
const issueMask = document.getElementById('issue-mask');

// openIssueDrawer 打开一只新股的详情：先看发行信息，再看自己在这只票上的账。
//
// 「新股列表」里的「查看」走到这里，而不是直接进记账表单——想核对发行价、申购日
// 或者看看中签和卖出情况的人，不该被扔进一个正在编辑的表单里。
function openIssueDrawer(fxCode) {
  const issue = findIssue(fxCode);
  if (!issue) return;
  const entries = ((state.snapshot && state.snapshot.entries) || [])
    .filter((item) => item.fxCode === fxCode);
  issueDrawer.dataset.fxCode = fxCode;
  renderIssueDetail(issue, entries);
  issueDrawer.hidden = false;
  issueMask.hidden = false;
}

// refreshOpenIssueDrawer 让开着的详情跟着数据刷新。
//
// 详情是独立的快照，agent 从接口改账（或自己改完回来）时，不重绘会一直显示旧值。
function refreshOpenIssueDrawer(snap) {
  const fxCode = issueDrawer.dataset.fxCode;
  if (!fxCode || issueDrawer.hidden) return;
  const issue = (snap.issues || []).find((item) => item.fxCode === fxCode);
  if (!issue) return;
  const entries = (snap.entries || []).filter((item) => item.fxCode === fxCode);
  renderIssueDetail(issue, entries);
}

function closeIssueDrawer() {
  issueDrawer.hidden = true;
  issueMask.hidden = true;
}

// kvRows 把「名称 / 值」两列铺成对齐的列表。值默认按文本转义，
// 需要放标签时传 { html } 显式声明，别让调用方随手塞未转义的串。
function kvRows(rows) {
  return '<dl class="kv">' + rows.map(([key, value]) => {
    const cell = value && typeof value === 'object' && value.html !== undefined
      ? value.html
      : escapeHtml(value);
    return `<dt>${escapeHtml(key)}</dt><dd>${cell}</dd>`;
  }).join('') + '</dl>';
}

function renderIssueDetail(issue, entries) {
  const stage = issueStage(issue);
  document.getElementById('issue-title').textContent = issue.name || '新股';
  const sub = [];
  if (issue.fxCode) sub.push('申购代码 ' + issue.fxCode);
  if (issue.stockCode) sub.push('证券代码 ' + issue.stockCode);
  document.getElementById('issue-sub').textContent = sub.join(' · ');

  const price = issue.priceFen > 0 ? fenToYuan(issue.priceFen) + ' 元' : '待定';
  const size = issue.issueShares > 0
    ? (issue.issueShares / 10000).toLocaleString('zh-CN', { maximumFractionDigits: 2 }) + ' 万股'
    : '—';
  const pe = issue.peHundredths > 0 ? (issue.peHundredths / 100).toFixed(2) + ' 倍' : '—';

  const info = kvRows([
    ['发行价', price],
    ['发行规模', size],
    ['发行市盈率', pe],
    ['申购日', issue.purchaseDate || '未公布'],
    ['中签公布', issue.resultDate || '未公布'],
    ['上市日', issue.listingDate || '未公布'],
    ['状态', { html: `<span class="${stage.cls}">${escapeHtml(stage.label)}</span>` }],
  ]);

  const mine = entries.length === 0
    ? '<p class="issue-note">这只票还没记账。点右下角「记一笔」把它加进账本。</p>'
    : entries.map(issueEntryCard).join('');
  const count = entries.length > 1 ? `<span class="issue-count">${entries.length} 条记录</span>` : '';

  document.getElementById('issue-detail').innerHTML =
    info + `<h3 class="issue-section">我的打新${count}</h3>` + mine;

  // 底部主按钮跟着记录变：只有一条记录时直接去改它；没记过、或者同一只票在多个
  // 账户都记了（各自卡片上有「编辑记录」），主按钮就是给这只票再记一笔。
  const primary = document.getElementById('btn-issue-entry');
  const single = entries.length === 1 ? entries[0] : null;
  primary.textContent = single ? '编辑记录' : '记一笔';
  primary.dataset.entryId = single ? String(single.id) : '';
  primary.dataset.fxCode = issue.fxCode;
}

// issueEntryCard 是「我的打新」里的一条记录摘要。
function issueEntryCard(view) {
  const rows = [];
  // 与列表「金额 / 股数」同序：先看占多少钱，再看打了多少股。
  rows.push(['申购', `${fenToYuanGrouped(view.applyFundsFen || 0)} · ${sharesText(view.applyShares)} 股`]);
  rows.push(['中签', view.allottedShares > 0 ? sharesText(view.allottedShares) + ' 股' : '未中签']);
  if (view.allottedShares > 0) rows.push(['缴款成本', fenToYuan(view.costFen)]);
  if (view.capitalCostFen > 0) {
    rows.push(['资金成本', fenToYuan(view.capitalCostFen) + (view.capitalCostEstimated ? '（估）' : '')]);
  }
  if (view.soldShares > 0) rows.push(['卖出', `${sharesText(view.soldShares)} 股 · ${fenToYuan(view.sellAmountFen)}`]);
  if (view.holdingShares > 0) rows.push(['仍持有', sharesText(view.holdingShares) + ' 股']);

  const sales = (view.sales || []).map((s) => `<div class="mine-sale">
      <span>${escapeHtml(s.date || '—')}</span>
      <span>${sharesText(s.shares)} 股</span>
      <span class="num">@${fenToYuan(s.priceFen)}</span>
      <span class="num">${fenToYuan(s.amountFen)}</span>
    </div>`).join('');

  const realized = Number(view.realizedFen) || 0;
  const realizedCls = realized > 0 ? 'profit' : realized < 0 ? 'loss' : '';
  const realizedRow = view.soldShares > 0
    ? `<div class="mine-total">净收益 <strong class="${realizedCls}">${fenToSignedYuan(realized)}</strong>` +
      (view.returnBps ? `<span class="muted"> · ${bpsText(view.returnBps)}</span>` : '') + '</div>'
    : '';

  return `<div class="mine-card">
    <div class="mine-head">
      <span class="mine-account">${escapeHtml(view.account || '')}</span>
      <span class="tag">${escapeHtml(view.status)}</span>
    </div>
    ${kvRows(rows)}
    ${sales ? `<div class="mine-sales">${sales}</div>` : ''}
    ${realizedRow}
    <button type="button" class="mini-btn" data-edit-entry-id="${view.id}">编辑记录</button>
  </div>`;
}

/* ---------- 记一笔：新股从列表里选（可输入过滤） ---------- */

// 候选按发行顺序排：申购日倒序，空日期垫底，同日按代码倒序——与新股列表同一个口径。
function issueCandidates() {
  const issues = (state.snapshot && state.snapshot.issues) || [];
  const rows = issues.slice().sort((a, b) => {
    const da = a.purchaseDate || '';
    const db = b.purchaseDate || '';
    if (da !== db) {
      if (!da) return 1;
      if (!db) return -1;
      return da < db ? 1 : -1;
    }
    const ca = a.fxCode || '';
    const cb = b.fxCode || '';
    if (ca === cb) return 0;
    return ca < cb ? 1 : -1;
  });
  // 老记录对应的新股可能已经不在当前列表里（列表只覆盖最近的发行），补一条，编辑时别丢了归属。
  const fallback = state.issueFallback;
  if (fallback && fallback.fxCode && !rows.some((item) => item.fxCode === fallback.fxCode)) {
    rows.unshift(fallback);
  }
  return rows;
}

function pickedIssue() {
  if (!state.issuePick) return null;
  return issueCandidates().find((item) => item.fxCode === state.issuePick) || null;
}

// 快照里的新股用 priceFen，记账记录用 issuePriceFen：两个来源都可能出现在候选里。
function issuePriceFen(issue) {
  return (issue && (issue.priceFen || issue.issuePriceFen)) || 0;
}

// 中文也走同一条 includes：输入「天元」能命中「天元重工」，不需要拼音或分词。
function issueMatches(issue, keyword) {
  if (!keyword) return true;
  return (issue.name || '').toLowerCase().includes(keyword)
    || (issue.fxCode || '').includes(keyword)
    || (issue.stockCode || '').includes(keyword);
}

// 输入框里显示「名称 · 代码」；纯文本，改一个字就当作重新筛选，不再绑定上一只票。
function issueInputText(issue) {
  if (!issue) return '';
  return [issue.name || '未命名', issue.fxCode].filter(Boolean).join(' · ');
}

function renderIssueList(keyword) {
  const list = document.getElementById('f-issue-list');
  const kw = String(keyword || '').trim().toLowerCase();
  const rows = issueCandidates().filter((issue) => issueMatches(issue, kw));

  if (rows.length === 0) {
    state.issueActive = -1;
    // 中文还没上屏时输入框里是拼音字母，拿它筛中文名必然筛空；这时别急着报「没有匹配」。
    list.innerHTML = state.issueComposing
      ? '<p class="combo-empty">继续输入…</p>'
      : candidateEmptyText();
  } else {
    // 已选中的那只一并高亮，展开后从它开始上下移动，换票不用重新找。
    const activeIndex = state.issuePick
      ? rows.findIndex((issue) => issue.fxCode === state.issuePick)
      : -1;
    state.issueActive = activeIndex;
    list.innerHTML = rows.map((issue, index) => {
      const meta = [issue.fxCode, issue.purchaseDate].filter(Boolean).join(' · ');
      const active = index === activeIndex ? ' active' : '';
      return `<button type="button" class="combo-item${active}" data-fx="${escapeHtml(issue.fxCode)}">` +
        `<span class="ci-name">${escapeHtml(issue.name || '未命名')}</span>` +
        `<span class="ci-meta">${escapeHtml(meta)}</span></button>`;
    }).join('');
  }
  list.hidden = false;
  list.scrollTop = 0;
  if (state.issueActive >= 0) {
    const activeEl = list.querySelector('.combo-item.active');
    if (activeEl) activeEl.scrollIntoView({ block: 'nearest' });
  }
}

function candidateEmptyText() {
  const total = ((state.snapshot && state.snapshot.issues) || []).length;
  if (total === 0) return '<p class="combo-empty">还没有新股数据，先点右上角「同步新股」</p>';
  return '<p class="combo-empty">没有匹配的新股，换个关键词试试</p>';
}

function closeIssueList() {
  document.getElementById('f-issue-list').hidden = true;
  state.issueActive = -1;
}

// 选中一只新股：填回输入框，带出发行价与申购日，收列表，焦点交给下一个要填的券商。
function pickIssue(fxCode) {
  const issue = issueCandidates().find((item) => item.fxCode === fxCode);
  if (!issue) return false;
  state.issuePick = issue.fxCode;
  document.getElementById('f-issue').value = issueInputText(issue);
  closeIssueList();
  fillIssueFields(true);
  updateIssueHint();
  updateDrawerSub();
  document.getElementById('f-broker').focus();
  return true;
}

// 选中新股后带出基本信息。overwrite 用于用户主动换票：换了一只票，价格和申购日就该跟着换。
function fillIssueFields(overwrite) {
  const issue = pickedIssue();
  if (!issue) return;
  const priceFen = issuePriceFen(issue);
  const date = issue.purchaseDate || '';
  const priceEl = document.getElementById('f-price');
  const purchaseEl = document.getElementById('f-purchase');
  const freezeEl = document.getElementById('f-freeze');
  if (priceFen > 0 && (overwrite || !priceEl.value.trim())) priceEl.value = fenToYuan(priceFen);
  if (date && (overwrite || !purchaseEl.value)) {
    purchaseEl.value = date;
    if (!freezeEl.value.trim()) freezeEl.value = date;
  }
}

// 提示行兼当「已带出什么」的收据：选中后就地告诉用户价格和申购日从哪来的。
function updateIssueHint() {
  const hint = document.getElementById('f-issue-hint');
  const issue = pickedIssue();
  if (!issue) {
    hint.textContent = ((state.snapshot && state.snapshot.issues) || []).length === 0
      ? '还没有新股数据，先点右上角「同步新股」把发行信息拉下来。'
      : '点开可浏览全部，或输入名称、代码筛选（中文也行）；选中后自动带出价格与申购日。';
    return;
  }
  const bits = [];
  const priceFen = issuePriceFen(issue);
  if (priceFen > 0) bits.push('发行价 ' + fenToYuan(priceFen) + ' 元');
  if (issue.purchaseDate) bits.push('申购日 ' + issue.purchaseDate);
  hint.textContent = bits.length ? '已选 · ' + bits.join(' · ') : '已选；这只票还没有发行价和申购日，请手动补。';
}

function updateDrawerSub() {
  const issue = pickedIssue();
  const bits = [];
  if (issue && issue.fxCode) bits.push('申购代码 ' + issue.fxCode);
  if (issue && issue.purchaseDate) bits.push('申购日 ' + issue.purchaseDate);
  if (issue && issue.listingDate) bits.push('上市 ' + issue.listingDate);
  document.getElementById('drawer-sub').textContent = bits.join(' · ');
}

// 候选列表的键盘操作：上下移动、回车选中、Esc 只收列表（别把抽屉一起关了）。
function handleIssueKeydown(event) {
  const list = document.getElementById('f-issue-list');
  const input = event.target;
  const items = list.hidden ? [] : Array.from(list.querySelectorAll('[data-fx]'));

  if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
    event.preventDefault();
    if (list.hidden) {
      renderIssueList(input.value);
      return;
    }
    if (items.length === 0) return;
    const step = event.key === 'ArrowDown' ? 1 : -1;
    let next = state.issueActive + step;
    if (next < 0) next = items.length - 1;
    if (next >= items.length) next = 0;
    state.issueActive = next;
    items.forEach((el, i) => el.classList.toggle('active', i === next));
    items[next].scrollIntoView({ block: 'nearest' });
    return;
  }

  if (event.key === 'Enter') {
    // 表单里回车的默认行为是提交；选票的时候得先拦住，否则一按就把半截记录存了。
    if (list.hidden) {
      event.preventDefault();
      renderIssueList(input.value);
      return;
    }
    event.preventDefault();
    const target = items[state.issueActive >= 0 ? state.issueActive : 0];
    if (target) pickIssue(target.dataset.fx);
    return;
  }

  if (event.key === 'Escape' && !list.hidden) {
    event.preventDefault();
    event.stopPropagation();
    closeIssueList();
  }
}

function openDrawer(options = {}) {
  const { issue = null, entry = null } = options;
  state.confirmDelete = false;

  document.getElementById('f-id').value = entry ? entry.id : '';

  // 新股只能从列表里选，名称与代码都跟着选中项走，不再手打。
  const fxCode = entry ? entry.fxCode : (issue ? issue.fxCode : '');
  state.issueFallback = entry && !findIssue(fxCode) ? entry : null;
  state.issuePick = fxCode || null;
  closeIssueList();
  updateIssueHint();

  const picked = pickedIssue();
  document.getElementById('f-issue').value = issueInputText(picked);

  document.getElementById('f-price').value = entry && entry.issuePriceFen > 0 ? fenToYuan(entry.issuePriceFen) : '';
  document.getElementById('f-apply').value = entry && entry.applyShares > 0 ? String(entry.applyShares) : '';
  document.getElementById('f-allotted').value = entry && entry.allottedShares > 0 ? String(entry.allottedShares) : '';

  document.getElementById('f-purchase').value = entry ? entry.purchaseDate || '' : '';
  document.getElementById('f-freeze').value = entry ? entry.freezeDate || '' : '';
  document.getElementById('f-refund').value = entry ? entry.refundDate || '' : '';
  document.getElementById('f-capital').value = entry && entry.capitalCostFen > 0 ? fenToYuan(entry.capitalCostFen) : '';

  document.getElementById('f-otherfee').value = entry && entry.otherFeeFen > 0 ? fenToYuan(entry.otherFeeFen) : '';
  document.getElementById('f-note').value = entry ? entry.note || '' : '';
  // 卖出按明细铺开：有几笔就有几张卡片，改哪一笔改哪一张。
  renderSales(entry ? entry.sales : []);

  // 新记一笔按选中新股带出价格与申购日；编辑态只在空处补，不覆盖账上已有的数字。
  fillIssueFields(!entry);
  updateDrawerSub();
  updateIssueHint();

  document.getElementById('drawer-title').textContent = entry ? '修改记录' : '记一笔';
  document.getElementById('btn-delete').hidden = !entry;
  document.getElementById('btn-delete').textContent = '删除';
  document.getElementById('form-note').textContent =
    '成交金额填了就以它为准，成交价只用于展示；费用留空时按账户佣金率逐笔估算。';

  const preferred = entry ? entry.brokerId : (state.lastBrokerId || (brokers()[0] || {}).id);
  refreshBrokerOptions(preferred);
  drawer.hidden = false;
  mask.hidden = false;
  // 手记一笔时直接把候选铺开——这一步就是选票；编辑已有记录时焦点给券商，别一打开就盖住表单。
  if (state.issuePick) {
    document.getElementById('f-broker').focus();
  } else {
    document.getElementById('f-issue').focus();
    renderIssueList('');
  }
}

function closeDrawer() {
  closeIssueList();
  drawer.hidden = true;
  mask.hidden = true;
  state.confirmDelete = false;
}

async function submitForm(event) {
  event.preventDefault();

  const id = Number(document.getElementById('f-id').value) || 0;
  const picked = pickedIssue();
  if (!picked) {
    toast('请先从列表里选一只新股', 'warn');
    document.getElementById('f-issue').focus();
    return;
  }
  const fxCode = picked.fxCode || '';
  const name = (picked.name || '').trim();
  const stockCode = (picked.stockCode || '').trim();
  const brokerId = Number(document.getElementById('f-broker').value) || 0;
  if (!brokerId) {
    toast('请先在「券商账户」里建一个账户', 'warn', 4200);
    closeDrawer();
    openBrokerDrawer(null);
    return;
  }

  const price = yuanToFen(document.getElementById('f-price').value);
  const applyRaw = document.getElementById('f-apply').value.trim();
  const allottedRaw = document.getElementById('f-allotted').value.trim();
  const otherFee = yuanToFen(document.getElementById('f-otherfee').value);
  const capital = yuanToFen(document.getElementById('f-capital').value);

  if ([price, otherFee, capital].some(Number.isNaN)) {
    toast('价格或金额填写不正确，请用数字，例如 4.48', 'warn');
    return;
  }

  const applyShares = applyRaw === '' ? 0 : Number(applyRaw);
  const allottedShares = allottedRaw === '' ? 0 : Number(allottedRaw);
  if (![applyShares, allottedShares].every((n) => Number.isInteger(n) && n >= 0)) {
    toast('数量请填写整数股数', 'warn');
    return;
  }

  // 卖出按明细收集：一张卡片是一笔成交，整行空着的跳过。
  const collected = collectSales();
  if (collected.error) {
    toast(collected.error, 'warn');
    return;
  }
  const sales = collected.sales;
  const soldShares = sales.reduce((sum, sale) => sum + sale.shares, 0);
  if (soldShares > allottedShares) {
    toast(`卖出合计 ${soldShares} 股，超过中签的 ${allottedShares} 股`, 'warn');
    return;
  }

  const entry = {
    id,
    brokerId,
    fxCode,
    stockCode,
    name,
    issuePriceFen: price,
    purchaseDate: document.getElementById('f-purchase').value || '',
    freezeDate: document.getElementById('f-freeze').value || '',
    refundDate: document.getElementById('f-refund').value || '',
    capitalCostFen: capital,
    applyShares,
    allottedShares,
    allotPriceFen: price,
    otherFeeFen: otherFee,
    sales,
    note: document.getElementById('f-note').value.trim(),
  };

  const saveBtn = document.getElementById('btn-save');
  saveBtn.disabled = true;
  saveBtn.textContent = '保存中…';
  try {
    await call('Service.SaveEntry', entry);
    state.lastBrokerId = brokerId;
    closeDrawer();
    toast(id ? '记录已更新' : '已记入账本', 'ok', 2000);
    await loadSnapshot();
  } catch (err) {
    toast(errorText(err), 'error', 5000);
  } finally {
    saveBtn.disabled = false;
    saveBtn.textContent = '保存';
  }
}

async function deleteEntry() {
  const id = Number(document.getElementById('f-id').value) || 0;
  if (!id) return;
  const btn = document.getElementById('btn-delete');
  if (!state.confirmDelete) {
    state.confirmDelete = true;
    btn.textContent = '确认删除';
    document.getElementById('form-note').textContent = '再点一次「确认删除」，这条记录将被移除。';
    return;
  }
  try {
    await call('Service.DeleteEntry', id);
    closeDrawer();
    toast('记录已删除', 'ok', 2000);
    await loadSnapshot();
  } catch (err) {
    toast(errorText(err), 'error', 5000);
  }
}

/* ---------- 券商账户表单 ---------- */

const brokerDrawer = document.getElementById('broker-drawer');
const brokerMask = document.getElementById('broker-mask');
const brokerForm = document.getElementById('broker-form');

function openBrokerDrawer(broker) {
  state.confirmBrokerDelete = false;
  document.getElementById('b-id').value = broker ? broker.id : '';
  document.getElementById('b-name').value = broker ? broker.name : '';
  document.getElementById('b-account').value = broker ? broker.account : '';
  document.getElementById('b-commission').value = broker && broker.commissionPpm > 0 ? ppmToWan(broker.commissionPpm) : '';
  document.getElementById('b-min').value = broker && broker.minCommissionFen > 0 ? fenToYuan(broker.minCommissionFen) : '';
  document.getElementById('b-rate').value = broker && broker.annualRatePpm > 0 ? ppmToPct(broker.annualRatePpm) : '';
  document.getElementById('b-note').value = broker ? broker.note || '' : '';

  document.getElementById('broker-title').textContent = broker ? '修改账户' : '新增账户';
  document.getElementById('btn-broker-delete').hidden = !broker;
  document.getElementById('btn-broker-delete').textContent = '删除';
  document.getElementById('broker-note').textContent =
    '佣金率按成交金额计算，低于最低佣金时按最低佣金收取；资金年化利率用于估算冻结期间的占用成本。';

  brokerDrawer.hidden = false;
  brokerMask.hidden = false;
  document.getElementById('b-name').focus();
}

function closeBrokerDrawer() {
  brokerDrawer.hidden = true;
  brokerMask.hidden = true;
  state.confirmBrokerDelete = false;
}

async function submitBroker(event) {
  event.preventDefault();
  const id = Number(document.getElementById('b-id').value) || 0;
  const name = document.getElementById('b-name').value.trim();
  if (!name) {
    toast('请填写券商名称', 'warn');
    return;
  }
  const commission = wanToPpm(document.getElementById('b-commission').value);
  const minCommission = yuanToFen(document.getElementById('b-min').value);
  const rate = pctToPpm(document.getElementById('b-rate').value);
  if ([commission, minCommission, rate].some(Number.isNaN)) {
    toast('佣金率、最低佣金或年化利率填写不正确', 'warn');
    return;
  }

  const broker = {
    id,
    name,
    account: document.getElementById('b-account').value.trim(),
    commissionPpm: commission,
    minCommissionFen: minCommission,
    annualRatePpm: rate,
    note: document.getElementById('b-note').value.trim(),
  };

  const saveBtn = document.getElementById('btn-broker-save');
  saveBtn.disabled = true;
  saveBtn.textContent = '保存中…';
  try {
    const saved = await call('Service.SaveBroker', broker);
    state.lastBrokerId = saved && saved.id ? saved.id : id;
    closeBrokerDrawer();
    toast(id ? '账户已更新' : '账户已新增', 'ok', 2200);
    await loadSnapshot();
  } catch (err) {
    toast(errorText(err), 'error', 5000);
  } finally {
    saveBtn.disabled = false;
    saveBtn.textContent = '保存';
  }
}

async function deleteBroker() {
  const id = Number(document.getElementById('b-id').value) || 0;
  if (!id) return;
  const btn = document.getElementById('btn-broker-delete');
  if (!state.confirmBrokerDelete) {
    state.confirmBrokerDelete = true;
    btn.textContent = '确认删除';
    document.getElementById('broker-note').textContent = '再点一次「确认删除」。有打新记录的账户不能删除。';
    return;
  }
  try {
    await call('Service.DeleteBroker', id);
    closeBrokerDrawer();
    toast('账户已删除', 'ok', 2000);
    await loadSnapshot();
  } catch (err) {
    toast(errorText(err), 'error', 5000);
  }
}

/* ---------- 备份与恢复 ---------- */

async function backup() {
  const btn = document.getElementById('btn-backup');
  btn.disabled = true;
  try {
    const result = await call('Desktop.SaveBackup');
    if (result && result.canceled) return;
    if (result && result.path) toast('备份已保存到 ' + result.path, 'ok', 4200);
  } catch (err) {
    toast(errorText(err), 'error', 5000);
  } finally {
    btn.disabled = false;
  }
}

async function restore() {
  const btn = document.getElementById('btn-restore');
  if (!state.confirmDelete) {
    state.confirmDelete = true;
    btn.textContent = '确认恢复';
    toast('恢复会用备份内容替换当前账本，再点一次继续', 'warn', 5000);
    setTimeout(() => {
      state.confirmDelete = false;
      btn.textContent = '恢复';
    }, 6000);
    return;
  }
  state.confirmDelete = false;
  btn.textContent = '恢复';
  btn.disabled = true;
  try {
    const result = await call('Desktop.LoadBackup');
    if (result && result.canceled) return;
    toast(`已导入 ${result.imported} 条记录`, 'ok', 3000);
    await loadSnapshot();
  } catch (err) {
    toast(errorText(err), 'error', 6000);
  } finally {
    btn.disabled = false;
  }
}

/* ---------- AI 接入 ---------- */

const aiDrawer = document.getElementById('ai-drawer');
const aiMask = document.getElementById('ai-mask');

async function openAIDrawer() {
  aiDrawer.hidden = false;
  aiMask.hidden = false;
  await loadAIInfo();
}

function closeAIDrawer() {
  aiDrawer.hidden = true;
  aiMask.hidden = true;
  state.showToken = false;
}

async function loadAIInfo() {
  const body = document.getElementById('ai-body');
  try {
    state.ai = await call('Desktop.APIInfo');
    renderAI();
  } catch (err) {
    body.innerHTML = '';
    const note = document.createElement('p');
    note.className = 'ai-note';
    note.textContent = errorText(err);
    body.appendChild(note);
  }
}

function renderAI() {
  const info = state.ai || {};
  const token = info.token || '';
  const masked = state.showToken ? token : '•'.repeat(Math.min(token.length || 8, 28));
  const base = info.baseUrl || '（未启动）';
  const skillDir = info.skillDir || '';
  const discovery = info.discoveryPath || '';
  const pointer = info.pointerPath || '';

  document.getElementById('ai-body').innerHTML = `
    <div class="ai-block">
      <p class="ai-label">接口状态</p>
      <p class="ai-status">
        <span class="ai-dot${info.running ? '' : ' off'}"></span>
        ${info.running ? '运行中，只监听本机' : '未启动，重启应用后恢复'}
      </p>
    </div>

    <div class="ai-block">
      <p class="ai-label">接口地址</p>
      <div class="ai-value"><span>${escapeHtml(base)}</span>
        <span class="ai-actions"><button type="button" class="mini-btn" data-copy="${escapeHtml(base)}">复制</button></span>
      </div>
    </div>

    <div class="ai-block">
      <p class="ai-label">访问令牌</p>
      <div class="ai-value"><span>${escapeHtml(masked)}</span>
        <span class="ai-actions">
          <button type="button" class="mini-btn" data-toggle-token>${state.showToken ? '隐藏' : '显示'}</button>
          <button type="button" class="mini-btn" data-copy="${escapeHtml(token)}">复制</button>
        </span>
      </div>
      <p class="ai-note">令牌等于账本的写权限，只在本机保存；怀疑泄露就点右下「重置令牌」。</p>
    </div>

    <div class="ai-block">
      <p class="ai-label">发现文件</p>
      <div class="ai-value"><span>${escapeHtml(discovery)}</span>
        <span class="ai-actions"><button type="button" class="mini-btn" data-copy="${escapeHtml(discovery)}">复制</button></span>
      </div>
      <p class="ai-note">agent 读这个文件就能拿到地址与令牌。应用退出时会自动删掉它。</p>
    </div>

    <div class="ai-block">
      <p class="ai-label">固定指针</p>
      <div class="ai-value"><span>${escapeHtml(pointer)}</span>
        <span class="ai-actions"><button type="button" class="mini-btn" data-copy="${escapeHtml(pointer)}">复制</button></span>
      </div>
      <p class="ai-note">端口可能被占用换成随机端口，数据目录也可能被改；这个位置不变，
        agent 从它开始找最省事。</p>
    </div>

    <div class="ai-block">
      <p class="ai-label">skill 包</p>
      <div class="ai-value"><span>${escapeHtml(skillDir)}</span>
        <span class="ai-actions"><button type="button" class="mini-btn" data-copy="${escapeHtml(skillDir)}">复制</button></span>
      </div>
      <p class="ai-note">点「导出 skill 包」选个目录（例如 agent 的技能目录），把生成的目录交给它，
        它会按 SKILL.md 里的说明读写账本。</p>
    </div>

    <p class="ai-label" style="margin-top:14px">导出后可以直接这样用</p>
    <pre class="ai-code">cd ${escapeHtml(skillDir)}
./scripts/ipobook.sh health
./scripts/ipobook.sh get "/api/issues?scope=issuing"</pre>
  `;
}

// copyText 优先用剪贴板接口；应用内页面不是安全上下文时退回 execCommand。
async function copyText(text) {
  if (!text) return false;
  try {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch (err) {
    // 继续走下面的兜底方案
  }
  const area = document.createElement('textarea');
  area.value = text;
  area.style.cssText = 'position:fixed;left:-9999px;top:0;opacity:0';
  document.body.appendChild(area);
  area.select();
  let ok = false;
  try {
    ok = document.execCommand('copy');
  } catch (err) {
    ok = false;
  }
  area.remove();
  return ok;
}

async function exportSkill() {
  const btn = document.getElementById('btn-ai-skill');
  btn.disabled = true;
  btn.textContent = '正在导出…';
  try {
    const result = await call('Desktop.ExportSkill');
    if (result && result.canceled) return;
    toast('skill 已导出到 ' + result.dir, 'ok', 5200);
    await loadAIInfo();
  } catch (err) {
    toast(errorText(err), 'error', 6000);
  } finally {
    btn.disabled = false;
    btn.textContent = '导出 skill 包';
  }
}

async function rotateToken() {
  const btn = document.getElementById('btn-ai-token');
  if (!state.confirmToken) {
    state.confirmToken = true;
    btn.textContent = '确认重置';
    toast('重置后旧令牌立即失效，再点一次确认', 'warn', 4000);
    setTimeout(() => {
      state.confirmToken = false;
      btn.textContent = '重置令牌';
    }, 5000);
    return;
  }
  state.confirmToken = false;
  btn.textContent = '重置令牌';
  try {
    state.ai = await call('Desktop.RotateToken');
    state.showToken = true;
    renderAI();
    toast('令牌已重置，其他地方要用新令牌', 'ok', 3600);
  } catch (err) {
    toast(errorText(err), 'error', 6000);
  }
}

/* ---------- 外部改动轮询 ---------- */

// 本地接口改了账本时，界面自己不会知道；定时比对版本号再刷新。
function startRevisionWatch() {
  setInterval(checkRevision, 2500);
}

async function checkRevision() {
  if (state.busy) return;
  try {
    const revision = await call('Service.Revision');
    if (state.revision === null) {
      state.revision = revision;
      return;
    }
    if (revision === state.revision) return;
    state.revision = revision;
    await loadSnapshot();
    toast('账本被外部更新，已重新载入', 'info', 2600);
  } catch (err) {
    // 读不到就下次再试，不打扰用户。
  }
}

/* ---------- 事件绑定 ---------- */

// activateTab 切换标签页，同时同步地址里的 hash，便于记住上次位置。
function activateTab(name, updateHash = true) {
  const valid = ['issues', 'entries', 'brokers', 'stats'];
  if (!valid.includes(name)) name = 'issues';
  document.querySelectorAll('.tab').forEach((t) => {
    t.classList.toggle('active', t.dataset.tab === name);
  });
  valid.forEach((view) => {
    document.getElementById('view-' + view).hidden = view !== name;
  });
  if (updateHash && location.hash !== '#' + name) {
    history.replaceState(null, '', '#' + name);
  }
}

function currentTab() {
  const hash = (location.hash || '').replace('#', '');
  // 允许 #stats/2026-10 这种带筛选条件的深链，取斜杠前的那一段判断标签页。
  const name = hash.split('/')[0];
  return ['issues', 'entries', 'brokers', 'stats'].includes(name) ? name : 'issues';
}

function bindEvents() {
  document.getElementById('btn-refresh').addEventListener('click', refreshIssues);
  document.getElementById('btn-backup').addEventListener('click', backup);
  document.getElementById('btn-restore').addEventListener('click', restore);
  document.getElementById('btn-ai').addEventListener('click', openAIDrawer);
  document.getElementById('ai-close').addEventListener('click', closeAIDrawer);
  document.getElementById('ai-mask').addEventListener('click', closeAIDrawer);
  document.getElementById('btn-add-sale').addEventListener('click', addSaleRow);
  const saleList = document.getElementById('sale-list');
  saleList.addEventListener('input', updateSaleTotal);
  saleList.addEventListener('change', updateSaleTotal);
  saleList.addEventListener('click', (event) => {
    const btn = event.target.closest('.sale-del');
    if (!btn) return;
    const row = btn.closest('.sale-row');
    if (!row) return;
    row.remove();
    renumberSales();
    updateSaleTotal();
    toast('已移除这一笔，保存后生效', 'info', 2200);
  });
  document.getElementById('f-allotted').addEventListener('input', updateSaleTotal);

  // 新股输入框：点一下就是下拉，列出全部候选；打字边输边筛。
  const issueInput = document.getElementById('f-issue');
  issueInput.addEventListener('focus', () => renderIssueList(''));
  issueInput.addEventListener('input', () => {
    // 改了文字就作废之前的选中，免得「文字是新票、代码还是旧票」。
    state.issuePick = null;
    renderIssueList(issueInput.value);
    updateIssueHint();
    updateDrawerSub();
  });
  // 中文输入法：拼音上屏前输入框里是字母，这时候别拿它去筛中文名。
  issueInput.addEventListener('compositionstart', () => { state.issueComposing = true; });
  issueInput.addEventListener('compositionend', () => {
    state.issueComposing = false;
    renderIssueList(issueInput.value);
  });
  issueInput.addEventListener('keydown', handleIssueKeydown);
  issueInput.addEventListener('blur', () => {
    closeIssueList();
    // 输完没从列表里选，就把文字还原成空，避免看起来像已经选好了。
    if (!state.issuePick) issueInput.value = '';
    updateIssueHint();
  });
  // 用 mousedown 抢在 blur 之前选中，否则列表先被收起，点击就落空了。
  document.getElementById('f-issue-list').addEventListener('mousedown', (event) => {
    const item = event.target.closest('[data-fx]');
    if (!item) return;
    event.preventDefault();
    pickIssue(item.dataset.fx);
  });
  document.addEventListener('mousedown', (event) => {
    if (!event.target.closest('#issue-combo')) closeIssueList();
  });

  document.getElementById('btn-ai-skill').addEventListener('click', exportSkill);
  document.getElementById('btn-ai-token').addEventListener('click', rotateToken);

  document.querySelectorAll('.tab').forEach((tab) => {
    tab.addEventListener('click', () => activateTab(tab.dataset.tab));
  });

  document.getElementById('issue-filter').addEventListener('click', (event) => {
    const chip = event.target.closest('.chip');
    if (!chip) return;
    document.querySelectorAll('#issue-filter .chip').forEach((c) => c.classList.remove('active'));
    chip.classList.add('active');
    state.scope = chip.dataset.scope;
    renderIssues(state.snapshot);
  });

  document.getElementById('issue-search').addEventListener('input', (event) => {
    state.search = event.target.value;
    renderIssues(state.snapshot);
  });

  document.getElementById('issue-body').addEventListener('click', (event) => {
    const addBtn = event.target.closest('[data-add-issue]');
    if (addBtn) {
      const issue = findIssue(addBtn.dataset.addIssue);
      if (issue) openDrawer({ issue });
      return;
    }
    const viewBtn = event.target.closest('[data-view-issue]');
    if (viewBtn) {
      openIssueDrawer(viewBtn.dataset.viewIssue);
    }
  });

  document.getElementById('entry-body').addEventListener('click', (event) => {
    const btn = event.target.closest('[data-edit-entry]');
    if (!btn) return;
    const entry = findEntry(btn.dataset.editEntry);
    if (entry) openDrawer({ entry });
  });

  document.getElementById('broker-body').addEventListener('click', (event) => {
    const btn = event.target.closest('[data-edit-broker]');
    if (!btn) return;
    const broker = findBroker(btn.dataset.editBroker);
    if (broker) openBrokerDrawer(broker);
  });

  // 收益统计的筛选：改一项只重取统计，不重读整个账本。
  document.getElementById('stat-year').addEventListener('change', (event) => {
    state.statFilter.year = Number(event.target.value) || 0;
    loadStats();
  });
  document.getElementById('stat-month').addEventListener('change', (event) => {
    state.statFilter.month = Number(event.target.value) || 0;
    loadStats();
  });
  document.getElementById('stat-broker').addEventListener('change', (event) => {
    state.statFilter.brokerId = Number(event.target.value) || 0;
    loadStats();
  });
  document.getElementById('stat-reset').addEventListener('click', () => {
    state.statFilter = { year: 0, month: 0, brokerId: 0 };
    loadStats();
  });
  // 点月度条形按那个月筛选，「这个月赚了多少、赚在哪几只票上」一步到位：
  // 卡片换成这个月的数，下面的明细列出这个月的记录。
  document.getElementById('month-list').addEventListener('click', async (event) => {
    const row = event.target.closest('[data-month]');
    if (!row) return;
    const [year, month] = row.dataset.month.split('-');
    state.statFilter.year = Number(year) || 0;
    state.statFilter.month = Number(month) || 0;
    await loadStats();
    // 明细在月度列表下面，窗口不高时要保证它真的进了视野。
    const panel = document.getElementById('detail-panel');
    if (panel) panel.scrollIntoView({ block: 'nearest' });
  });

  // 明细每一行都是入口：点行或点「编辑」都进同一条记录的表单。
  document.getElementById('detail-body').addEventListener('click', (event) => {
    const row = event.target.closest('[data-entry-id]');
    if (!row) return;
    const entry = findEntry(row.dataset.entryId);
    if (entry) openDrawer({ entry });
  });

  document.getElementById('btn-manual').addEventListener('click', () => openDrawer({}));
  document.getElementById('btn-add-broker').addEventListener('click', () => openBrokerDrawer(null));

  // 新股详情：两个出口——改已有记录，或给这只票补一笔。
  document.getElementById('issue-close').addEventListener('click', closeIssueDrawer);
  document.getElementById('btn-issue-close').addEventListener('click', closeIssueDrawer);
  issueMask.addEventListener('click', closeIssueDrawer);

  document.getElementById('issue-detail').addEventListener('click', (event) => {
    const btn = event.target.closest('[data-edit-entry-id]');
    if (!btn) return;
    const entry = findEntry(btn.dataset.editEntryId);
    if (!entry) return;
    closeIssueDrawer();
    openDrawer({ entry });
  });

  document.getElementById('btn-issue-entry').addEventListener('click', (event) => {
    const btn = event.currentTarget;
    const entry = btn.dataset.entryId ? findEntry(btn.dataset.entryId) : null;
    const fxCode = btn.dataset.fxCode || '';
    closeIssueDrawer();
    if (entry) {
      openDrawer({ entry });
      return;
    }
    const issue = findIssue(fxCode);
    if (issue) openDrawer({ issue });
  });
  document.getElementById('drawer-close').addEventListener('click', closeDrawer);
  document.getElementById('btn-cancel').addEventListener('click', closeDrawer);
  document.getElementById('btn-delete').addEventListener('click', deleteEntry);
  document.getElementById('drawer-mask').addEventListener('click', closeDrawer);
  document.getElementById('broker-close').addEventListener('click', closeBrokerDrawer);
  document.getElementById('btn-broker-cancel').addEventListener('click', closeBrokerDrawer);
  document.getElementById('broker-mask').addEventListener('click', closeBrokerDrawer);
  document.getElementById('btn-broker-delete').addEventListener('click', deleteBroker);
  form.addEventListener('submit', submitForm);
  brokerForm.addEventListener('submit', submitBroker);

  document.getElementById('ai-body').addEventListener('click', async (event) => {
    const copyBtn = event.target.closest('[data-copy]');
    if (copyBtn) {
      const ok = await copyText(copyBtn.dataset.copy);
      toast(ok ? '已复制' : '复制失败，请手动选中文本', ok ? 'ok' : 'warn', 2200);
      return;
    }
    if (event.target.closest('[data-toggle-token]')) {
      state.showToken = !state.showToken;
      renderAI();
    }
  });

  document.addEventListener('keydown', (event) => {
    if (event.key !== 'Escape') return;
    if (!aiDrawer.hidden) closeAIDrawer();
    else if (!brokerDrawer.hidden) closeBrokerDrawer();
    else if (!issueDrawer.hidden) closeIssueDrawer();
    else if (!drawer.hidden) closeDrawer();
  });
}

document.addEventListener('DOMContentLoaded', async () => {
  bindEvents();
  // #stats/2026-10 是带筛选条件的深链（截图与排查用）：先定好条件再读数据，
  // 否则会先按「全部」算一遍，再被第二个请求覆盖。
  const statsLink = location.hash.match(/^#stats(?:\/(\d{4})(?:-(\d{1,2}))?)?$/);
  if (statsLink) {
    state.statFilter.year = Number(statsLink[1]) || 0;
    state.statFilter.month = Number(statsLink[2]) || 0;
  }
  activateTab(currentTab(), false);
  await loadSnapshot();
  startRevisionWatch();
  // #ai / #entry / #issue 是抽屉的深链，方便直接打开和截图核对。
  // #entry/<id> 直接打开某条记录的详情，改已有记录时少点一步；
  // 末尾加 /sales 时顺手滚到卖出明细，明细较长时不用手动往下找。
  // #issue/<申购代码> 打开某只新股的详情。
  const entryLink = location.hash.match(/^#entry\/(\d+)(\/sales)?$/);
  const issueLink = location.hash.match(/^#issue\/([0-9A-Za-z]+)$/);
  if (location.hash === '#ai') {
    await openAIDrawer();
  } else if (issueLink) {
    openIssueDrawer(issueLink[1]);
  } else if (entryLink) {
    const id = Number(entryLink[1]);
    const entries = (state.snapshot && state.snapshot.entries) || [];
    const entry = entries.find((item) => item.id === id);
    openDrawer(entry ? { entry } : {});
    if (entryLink[2]) {
      const list = document.getElementById('sale-list');
      if (list) list.scrollIntoView({ block: 'center' });
    }
  } else if (location.hash === '#entry') {
    openDrawer({});
  }
  const snap = state.snapshot;
  if (snap && snap.meta && !snap.meta.issuesUpdatedAt) {
    refreshIssues();
  }
});
