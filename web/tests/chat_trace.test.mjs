#!/usr/bin/env node
// 「AI 调度台」过程可见性的前端回归防线（零依赖，CI 里直接 node 跑）。
//
// 真实 bug（用户视角）：「skillforge 问问题以后中间步骤都没有显示，一直加载状态，
// 最后显示一个最终答案」。根因在后端（trace 只在分类器返回后才发、且 chat 意图
// 完全不发），但**展开/收起的行为在前端**：调度台过去是「默认折叠、点开才看」，
// 就算后端把心跳步骤发全了，用户屏幕上仍然只有一个静态状态条。
//
// 所以这里守的是前端这一半：运行中必须自动展开时间线，跑完自动收起，手动开合
// 过则以后者为准。做法沿用仓库既有惯例（见 readonly_file_editor.test.mjs）：
// **从出货文件里把真函数抽出来、配一套最小 DOM 桩跑真逻辑**，而不是在测试里
// 另写一份实现 —— 否则测试和实现各说各话，漂移了还绿。
//
// 两层断言：
//   A. 行为层：抽出 renderTrace，喂各种 status 组合，验展开态。
//   B. 接线层：时间线必须真的插在答案上方（insertBefore），且 index.html 的
//      缓存版本号写对（改了 chat.js 不 bump ?v=，用户端就是旧文件=白改）。

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const CHAT_JS = join(here, '..', 'js', 'chat.js');
const INDEX_HTML = join(here, '..', 'index.html');
const src = readFileSync(CHAT_JS, 'utf8');

let failures = 0;
function check(name, cond, extra = '') {
  if (cond) {
    console.log(`  ok   ${name}`);
  } else {
    failures++;
    console.log(`  FAIL ${name}${extra ? ' — ' + extra : ''}`);
  }
}

// 抽出 `function <name>(...) { ... }` 的完整源码（花括号配平，能扛嵌套）。
function extractFunction(source, name) {
  const sig = `function ${name}(`;
  const start = source.indexOf(sig);
  if (start < 0) return '';
  let depth = 0;
  for (let i = source.indexOf('{', start); i < source.length; i++) {
    if (source[i] === '{') depth++;
    else if (source[i] === '}') {
      depth--;
      if (depth === 0) return source.slice(start, i + 1);
    }
  }
  return '';
}

// ---- 最小 DOM 桩：只要 renderTrace 用得到的那几个口子 ----
class ClassList {
  constructor() { this.set = new Set(); }
  add(c) { this.set.add(c); }
  remove(c) { this.set.delete(c); }
  contains(c) { return this.set.has(c); }
  toggle(c, force) {
    // 真 DOM 语义：给了 force 就按 force 来，别翻转 ——
    // 桩要是实现错了，测出来的是桩的 bug 不是产品的 bug。
    const want = force === undefined ? !this.set.has(c) : !!force;
    if (want) this.set.add(c); else this.set.delete(c);
    return want;
  }
}
class El {
  constructor(cls = '') {
    this.className = cls;
    this.classList = new ClassList();
    if (cls) cls.split(' ').forEach((c) => this.classList.add(c));
    this.dataset = {};
    this.style = {};
    this.textContent = '';
    this._innerHTML = '';
    this.children = [];
    this._q = new Map();
    this._listeners = {};
    // 容器度量：材料日志的贴底行为要靠 scrollTop/scrollHeight/clientHeight 才能
    // 验。桩给一组**真溢出**的数（400 > 100），否则「scrollTop === scrollHeight」
    // 在 0 === 0 上恒真 —— 那是空跑绿，什么都没测到。
    this.scrollTop = 0;
    this.scrollHeight = 400;
    this.clientHeight = 100;
  }
  // innerHTML 只会被喂带 class="…" 的片段。**必须按真 DOM 建出嵌套层级**：
  // 早期版本把每个 class="…" 都拍成扁平兄弟节点，于是 '.ctk-mat-log .ctk-mat-body'
  // 这类后代选择器永远查不到元素 —— 贴底断言拿到的永远是 null（测试看着在跑，
  // 实际一行真逻辑没验到）。桩的实现错了，测出来的是桩的 bug 而不是产品的 bug。
  set innerHTML(v) {
    this._innerHTML = v;
    this.children = [];
    this.textContent = '';
    // 极简 HTML 解析：只认标签边界与 class 属性，其余（文本）挂到当前节点上。
    // 未闭合/自闭合标签不压栈，避免栈错位把后续节点挂到错的父上。
    const VOID = new Set(['img', 'br', 'hr', 'input', 'meta', 'link']);
    const re = /<(\/?)([a-zA-Z][a-zA-Z0-9]*)([^>]*?)(\/?)>/g;
    const stack = [this];
    let last = 0;
    let m;
    const text = (s) => { if (s) stack[stack.length - 1].textContent += s; };
    while ((m = re.exec(v)) !== null) {
      text(v.slice(last, m.index));
      last = m.index + m[0].length;
      const [, close, tag, attrs, selfClose] = m;
      if (close) {
        if (stack.length > 1) stack.pop();
        continue;
      }
      const cm = /class="([^"]*)"/.exec(attrs);
      const kid = new El(cm ? cm[1] : '');
      kid.tagName = tag.toUpperCase();
      const parent = stack[stack.length - 1];
      kid.parent = parent;
      parent.children.push(kid);
      if (!VOID.has(tag.toLowerCase()) && !selfClose) stack.push(kid);
    }
    text(v.slice(last));
  }
  get innerHTML() { return this._innerHTML; }
  appendChild(c) { c.parent = this; this.children.push(c); return c; }
  insertBefore(c) { c.parent = this; this.children.unshift(c); return c; }
  addEventListener(ev, fn) { this._listeners[ev] = fn; }
  click() { if (this._listeners.click) this._listeners.click(); }
  // 查询返回稳定的桩元素：同一个选择器每次都拿到同一个对象，便于断言
  // 必须像真 DOM：找不到就返回 null。早期版本「总是造一个桩返回」，
  // 结果 if (!head) 分支永远不执行 —— 测试看着绿，实际一行真逻辑都没跑到。
  querySelector(sel) {
    // 支持后代选择器（'.ctk-mat-log .ctk-mat-body'）：材料日志的查询就是这个形式，
    // 不支持的话桩只返回 null，贴底断言就变成永远不执行。
    // 语义按真 DOM：最后一段是「要返回的元素」，前面的段必须出现在它的祖先链上。
    // 只查直接子节点是不够的 —— 日志元素在 body > row > span 三层之下。
    const parts = String(sel).trim().split(/\s+/).map((p) => p.replace(/^\./, ''));
    const last = parts[parts.length - 1];
    const wantAncestors = parts.slice(0, -1);
    const hasAncestors = (node) => {
      const want = [...wantAncestors];
      let cur = node.parent;
      while (cur && want.length) {
        const cls = typeof cur.className === 'string' ? cur.className.split(' ') : [];
        if (cls.includes(want[want.length - 1])) want.pop();
        cur = cur.parent;
      }
      return want.length === 0;
    };
    const walk = (node) => {
      for (const c of node.children) {
        if (typeof c.className !== 'string') continue;
        if (c.className.split(' ').includes(last) && hasAncestors(c)) return c;
        const deeper = walk(c);
        if (deeper) return deeper;
      }
      return null;
    };
    return walk(this);
  }
}

const esc = (s) => String(s ?? '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
const AGENTS = { analyze: { n: 1, role: '分析' }, match: { n: 2, role: '匹配' }, params: { n: 3, role: '提炼' }, generate: { n: 4, role: '执笔' } };
const AGENTS_QUERY = AGENTS;
const MARK = { pending: '·', active: '●', waiting: '‖', done: '✓' };

const srcFn = extractFunction(src, 'renderTrace');
check('renderTrace 从出货文件里抽到了', srcFn.length > 500, `len=${srcFn.length}`);
// 材料渲染也必须测**出货文件里的真函数**。抽不到就给空实现 —— 抽签失败会让
// A3/A4 的断言变红（真 FAIL 行），而不是抛异常把整个测试炸掉（崩溃红看不出红在哪）。
const matOfFn = extractFunction(src, 'matOf');
check('matOf 从出货文件里抽到了', matOfFn.length > 100, `len=${matOfFn.length}`);
const stickFn = extractFunction(src, 'stickMatLog');
check('stickMatLog 从出货文件里抽到了', stickFn.length > 100, `len=${stickFn.length}`);
const matOf = matOfFn ? new Function('esc', `return ${matOfFn}`)(esc) : () => '';
const stickMatLog = stickFn ? new Function(`return ${stickFn}`)() : () => {};
const renderTrace = new Function('esc', 'AGENTS', 'AGENTS_QUERY', 'MARK', 'el', 'keepBottom',
  'matOf', 'stickMatLog',
  `return ${srcFn}`)(esc, AGENTS, AGENTS_QUERY, MARK, (tag, cls) => new El(cls), () => {},
  matOf, stickMatLog);

function mkTrace(stype) {
  const t = new El('ch-msg-trace');
  t.dataset.stype = stype || '';
  return t;
}
const bodyOf = (t) => t.querySelector('.ch-tr-body');
const step = (status, detail) => ({ phase: 'generate', label: '④ 执笔', detail, status });
// 带材料的步：material 是单行尾巴（老字段），material_log 是滚动日志（新字段）。
const logStep = (log, mat) => ({
  phase: 'generate', label: '④ 内容执笔', status: 'active',
  detail: '正在撰写内容…（已用 40s）',
  material: mat || '', material_log: log || '',
});
const logBodyOf = (t) => bodyOf(t).querySelector('.ctk-mat-log .ctk-mat-body');

console.log('A. 行为层：展开态由运行状态驱动');
{
  const t = mkTrace();
  renderTrace(t, [step('active', '正在理解你的问题…（已用 3s）')]);
  check('运行中自动展开时间线', t.classList.contains('open') && bodyOf(t).style.display === 'block',
    `open=${t.classList.contains('open')} display=${bodyOf(t).style.display}`);
  const head0 = t.querySelector('.ch-tr-head');
  check('运行中状态条显示当前步骤文案', head0.querySelector('.ch-tr-status').innerHTML.includes('正在理解你的问题'),
    head0.querySelector('.ch-tr-status').innerHTML);
}
{
  const t = mkTrace();
  renderTrace(t, [step('active', 'x')]);
  renderTrace(t, [step('done', 'y'), step('active', '正在撰写内容…（已用 40s）')]);
  check('步骤组从 1 条变多条后仍保持展开', t.classList.contains('open'), 'updated frame collapsed');
  check('多步时间线渲染成行', bodyOf(t).children.length === 2, `rows=${bodyOf(t).children.length}`);
}
{
  const t = mkTrace();
  renderTrace(t, [step('active', 'x')]);
  renderTrace(t, [step('done', 'ok1'), { phase: 'analyze', label: '① 分析', detail: 'ok2', status: 'done' }]);
  check('跑完自动收起（保持界面清爽）', !t.classList.contains('open') && bodyOf(t).style.display === 'none',
    `open=${t.classList.contains('open')} display=${bodyOf(t).style.display}`);
  const head1 = t.querySelector('.ch-tr-head');
  check('跑完状态条显示完成', head1.querySelector('.ch-tr-status').innerHTML.includes('完成'),
    head1.querySelector('.ch-tr-status').innerHTML);
}
{
  const t = mkTrace();
  renderTrace(t, [step('waiting', '等待补充：金额')]);
  check('等待用户补充时保持展开（用户得看见在等什么）', t.classList.contains('open'),
    `open=${t.classList.contains('open')}`);
}
console.log('A2. 手动开合优先于自动');
{
  const t = mkTrace();
  renderTrace(t, [step('active', 'x')]);
  const head = t.querySelector('.ch-tr-head');
  head.click(); // 用户手动收起
  check('手动收起后立刻折起', !t.classList.contains('open'));
  renderTrace(t, [step('active', 'x（已用 6s）')]);
  check('手动收起后，后续心跳帧不得抢着展开', !t.classList.contains('open'),
    `open=${t.classList.contains('open')}`);
}
{
  const t = mkTrace();
  renderTrace(t, [step('active', 'x')]);
  renderTrace(t, [step('done', 'y')]);
  const head = t.querySelector('.ch-tr-head');
  head.click(); // 用户手动展开看历史的调度细节
  renderTrace(t, [step('done', 'z')]);
  check('手动展开后，收尾帧不得把它折回去', t.classList.contains('open'));
}

console.log('A3. 中间材料：计时之外必须有「正在动的内容」');
{
  // 用户抱怨：「速度过于慢了，中间可以流式输出思考的一些中间材料，现在一直卡着
  // 计时」。所以材料必须真渲染进行里 —— 只有跳秒的计时器不算进度。
  const t = mkTrace();
  renderTrace(t, [{
    phase: 'generate', label: '④ 内容执笔', status: 'active',
    detail: '正在撰写内容…（已用 40s）',
    material: '先看手册要求，这是一份通知，需要标题、正文、落款。',
  }]);
  const row = bodyOf(t).children[0];
  check('有材料时渲染 .ctk-mat', row.innerHTML.includes('ctk-mat'), row.innerHTML.slice(0, 160));
  check('材料文本真的进了行里', row.innerHTML.includes('这是一份通知'), row.innerHTML.slice(0, 160));
  check('材料带「思考中」标签（让用户知道这是什么）', row.innerHTML.includes('思考中'), row.innerHTML.slice(0, 160));
  check('detail（含计时）与材料并存', row.innerHTML.includes('已用 40s'), row.innerHTML.slice(0, 160));
  const head = t.querySelector('.ch-tr-head');
  check('材料不进顶部状态条（状态条只放一句话，否则会撑成一堵墙）',
    !head.querySelector('.ch-tr-status').innerHTML.includes('这是一份通知'),
    head.querySelector('.ch-tr-status').innerHTML);
}
{
  const t = mkTrace();
  renderTrace(t, [step('active', '正在撰写内容…（已用 3s）')]);
  check('无材料时不留空壳', !bodyOf(t).children[0].innerHTML.includes('ctk-mat'),
    bodyOf(t).children[0].innerHTML.slice(0, 120));
}
{
  // 材料是模型原样吐出来的文本，什么都可能有；不转义就是一个注入点。
  const t = mkTrace();
  renderTrace(t, [{
    phase: 'generate', label: '④ 内容执笔', status: 'active', detail: 'x',
    material: '<img src=x onerror=alert(1)>',
  }]);
  const row = bodyOf(t).children[0];
  check('材料被转义（不产生真标签）',
    row.innerHTML.includes('&lt;img') && !/<img/.test(row.innerHTML), row.innerHTML.slice(0, 160));
}

console.log('A4. 思考日志：整段在长（不是一行在地上抖），且默认贴底跟随');
{
  // 用户原话：「速度过于慢了，中间可以流式输出思考的一些中间材料，现在一直卡着计时，
  // 用户体验不佳」。日志窗口是这条诉求的正面解法：单行 160 字尾巴每帧原地替换，
  // 屏幕上是一行字在抖（实测被读成「程序坏了」）；日志是整段累积、只往尾部加。
  const t = mkTrace();
  // 夹具必须长到「只留尾巴就会丢头」—— 早先这里放的是 37 字，于是
  // 自证脚本把整段截成 slice(-40) 时断言照样绿：那条断言其实抓不住
  // 「整段在长」。真实日志上限 1200 字（后端 materialLogCap），
  // 所以夹具按真实形态铺 6 段、每段带序号，头尾各放唯一标记。
  const log = [
    '【1】先看手册要求：这是一份通知，需要标题、正文、落款三块。',
    '【2】确认时间节点：2026年9月20日前上报，责任单位是各旗县发改委。',
    '【3】核对必备要素：主送机关、成文日期、联系人电话都要齐。',
    '【4】起草正文：先写背景，再写具体要求，最后写报送方式与截止时间。',
    '【5】语气口径：用公文体，不用口语，不写口号式排比。',
    '【6】收口检查：落款单位与成文日期对齐，附件名与正文引用一致。',
  ].join('\n');
  renderTrace(t, [logStep(log)]);
  const row = bodyOf(t).children[0];
  check('有 material_log 时渲染滚动日志容器', row.innerHTML.includes('ctk-mat-log'), row.innerHTML.slice(0, 160));
  check('日志整段进 DOM（不是只剩尾巴一句）',
    row.innerHTML.includes('【1】先看手册要求') && row.innerHTML.includes('【6】收口检查'), row.innerHTML.slice(0, 220));
  const el = logBodyOf(t);
  check('贴底跟随：首帧就把日志滚到底（scrollTop === scrollHeight）',
    !!el && el.scrollTop === el.scrollHeight, el ? `top=${el.scrollTop} h=${el.scrollHeight}` : 'null');
}
{
  // 往上翻读半句时，下一帧（≤400ms 后）不许把人弹回底部 —— 那比不跟随更烦人。
  const t = mkTrace();
  renderTrace(t, [logStep('第一句。第二句。')]);
  const el1 = logBodyOf(t);
  el1.scrollTop = 120;               // 400(高) - 120 - 100(窗口) = 180 > 4px → 判定翻走
  el1._listeners.scroll();
  renderTrace(t, [logStep('第一句。第二句。第三句。')]);
  const el2 = logBodyOf(t);
  check('用户往上翻后，下一帧停在原处（不弹回底部）',
    !!el2 && el2.scrollTop === 120, el2 ? `top=${el2.scrollTop}` : 'null');
  el2.scrollTop = 300;               // 400 - 300 - 100 = 0 ≤ 4px 容差 → 判定回到底部
  el2._listeners.scroll();
  renderTrace(t, [logStep('第一句。第二句。第三句。第四句。')]);
  const el3 = logBodyOf(t);
  check('滑回底部后恢复跟随', !!el3 && el3.scrollTop === el3.scrollHeight,
    el3 ? `top=${el3.scrollTop} h=${el3.scrollHeight}` : 'null');
}
{
  // 老后端 / 回滚期没有 material_log 字段：必须照旧退回单行 material，别白屏。
  const t = mkTrace();
  renderTrace(t, [logStep('', '单行尾巴材料')]);
  const row = bodyOf(t).children[0];
  check('无 material_log 时退回单行 material（兼容老帧）',
    row.innerHTML.includes('单行尾巴材料') && !row.innerHTML.includes('ctk-mat-log'), row.innerHTML.slice(0, 160));
  check('无日志时不渲染日志容器（不留空壳）', logBodyOf(t) === null, 'leftover log node');
}
{
  const t = mkTrace();
  renderTrace(t, [logStep('<img src=x onerror=alert(1)>')]);
  const row = bodyOf(t).children[0];
  check('日志文本被转义（不产生真标签）',
    row.innerHTML.includes('&lt;img') && !/<img/.test(row.innerHTML), row.innerHTML.slice(0, 160));
}

console.log('B. 接线层');
check('时间线插在答案上方（insertBefore 到首位）', /insertBefore\(trace,/.test(src),
  '没找到 insertBefore(trace, …)');
check('异步事件流里有 trace 分支', /case 'trace'/.test(src), "缺 case 'trace'");
{
  const html = readFileSync(INDEX_HTML, 'utf8');
  const m = html.match(/chat\.js\?v=(\d{8}[A-Za-z])/);
  check('index.html 给 chat.js 带了缓存版本号', !!m, '未找到 chat.js?v=…');
  check('缓存版本号已 bump 到本次改动（>= 20260912A）', m && m[1] >= '20260912A', m ? m[1] : '—');
  // 改 CSS 不 bump style.css 的 ?v= 等于白改（用户端拿的是缓存里的旧样式），
  // 所以两者都要查 —— 只查 JS 的版本号挡不住「样式没生效」这类线上事故。
  const c = html.match(/style\.css\?v=(\d{8}[A-Za-z])/);
  check('index.html 给 style.css 带了缓存版本号且已 bump（>= 20260922A）',
    !!c && c[1] >= '20260922A', c ? c[1] : '—');
}

console.log(failures === 0 ? '\n全部通过' : `\n${failures} 项失败`);
process.exit(failures === 0 ? 0 : 1);
