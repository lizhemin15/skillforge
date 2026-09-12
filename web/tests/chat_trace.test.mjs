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
  }
  // innerHTML 只会被喂 class="…" 片段，所以解析出子节点即可；
  // 关键是要像真 DOM 一样**清空旧子节点**，否则时间线会跨帧累积成幽灵行。
  set innerHTML(v) {
    this._innerHTML = v;
    this.children = [];
    const re = /class="([^"]+)"/g;
    let m;
    while ((m = re.exec(v)) !== null) this.children.push(new El(m[1].split(' ')[0]));
  }
  get innerHTML() { return this._innerHTML; }
  appendChild(c) { this.children.push(c); return c; }
  insertBefore(c) { this.children.unshift(c); return c; }
  addEventListener(ev, fn) { this._listeners[ev] = fn; }
  click() { if (this._listeners.click) this._listeners.click(); }
  // 查询返回稳定的桩元素：同一个选择器每次都拿到同一个对象，便于断言
  // 必须像真 DOM：找不到就返回 null。早期版本「总是造一个桩返回」，
  // 结果 if (!head) 分支永远不执行 —— 测试看着绿，实际一行真逻辑都没跑到。
  querySelector(sel) {
    const cls = sel.replace(/^\./, '');
    for (const c of this.children) {
      if (typeof c.className === 'string' && c.className.split(' ').includes(cls)) return c;
    }
    return null;
  }
}

const esc = (s) => String(s ?? '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
const AGENTS = { analyze: { n: 1, role: '分析' }, match: { n: 2, role: '匹配' }, params: { n: 3, role: '提炼' }, generate: { n: 4, role: '执笔' } };
const AGENTS_QUERY = AGENTS;
const MARK = { pending: '·', active: '●', waiting: '‖', done: '✓' };

const srcFn = extractFunction(src, 'renderTrace');
check('renderTrace 从出货文件里抽到了', srcFn.length > 500, `len=${srcFn.length}`);
const renderTrace = new Function('esc', 'AGENTS', 'AGENTS_QUERY', 'MARK', 'el', 'keepBottom',
  `return ${srcFn}`)(esc, AGENTS, AGENTS_QUERY, MARK, (tag, cls) => new El(cls), () => {});

function mkTrace(stype) {
  const t = new El('ch-msg-trace');
  t.dataset.stype = stype || '';
  return t;
}
const bodyOf = (t) => t.querySelector('.ch-tr-body');
const step = (status, detail) => ({ phase: 'generate', label: '④ 执笔', detail, status });

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

console.log('B. 接线层');
check('时间线插在答案上方（insertBefore 到首位）', /insertBefore\(trace,/.test(src),
  '没找到 insertBefore(trace, …)');
check('异步事件流里有 trace 分支', /case 'trace'/.test(src), "缺 case 'trace'");
{
  const html = readFileSync(INDEX_HTML, 'utf8');
  const m = html.match(/chat\.js\?v=(\d{8}[A-Za-z])/);
  check('index.html 给 chat.js 带了缓存版本号', !!m, '未找到 chat.js?v=…');
  check('缓存版本号已 bump 到本次改动（>= 20260912A）', m && m[1] >= '20260912A', m ? m[1] : '—');
}

console.log(failures === 0 ? '\n全部通过' : `\n${failures} 项失败`);
process.exit(failures === 0 ? 0 : 1);
