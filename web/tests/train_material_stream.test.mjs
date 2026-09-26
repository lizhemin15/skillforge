#!/usr/bin/env node
// 训练页「中间材料」实况块 —— 行为测试（零依赖，CI 里直接 node 跑）。
//
// 守的是一类**静默劣化**：材料是有了，但写成了「一片一个 DOM 节点」。
// 二十分钟训练、每秒几十个片段，DOM 里会攒到几万个节点，页面越来越卡，
// 最后用户看到的仍然是「卡着」。静态看代码（`case 'delta'` 存在、脚本体面）
// 全都正常，review 抓不到；只有真把帧喂进去数节点才抓得到。
//
// 做法与 web/tests/chat_composer.test.mjs 同源：从**出货文件**里抠出真正在跑的
// 那段 case 代码（花括号配平），配一个极简 DOM 替身把它跑起来 —— 不在测试里
// 抄一份实现。抄的那份永远不会红。

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const ROOT = join(here, '..', '..');
const read = (p) => readFileSync(join(ROOT, p), 'utf8');

let failures = 0;
function check(name, cond, extra = '') {
  if (cond) console.log(`  ok   ${name}`);
  else { failures++; console.log(`  FAIL ${name}${extra ? ' — ' + extra : ''}`); }
}

// 从 switch 里抠一个 case 的整块（含花括号），做花括号配平。
function extractCase(js, name) {
  const at = js.indexOf(`case '${name}'`);
  if (at < 0) return null;
  const open = js.indexOf('{', at);
  if (open < 0) return null;
  let depth = 0;
  for (let i = open; i < js.length; i++) {
    if (js[i] === '{') depth++;
    else if (js[i] === '}') {
      depth--;
      if (depth === 0) return js.slice(at, i + 1);
    }
  }
  return null;
}

// 从出货文件里抠一个顶层函数声明（含花括号），做花括号配平 —— 与 extractCase 同一套手法。
// 为什么必须有：case 'delta' 分支末尾调了 pinLive()（把材料拉进视野），而 pinLive 是
// 训一场训练时由 makeLivePinner 造出来的闭包。抠出来的代码块单独跑时它不在作用域里，
// 于是整条腿以「pinLive is not defined」崩掉 —— 崩掉的红不算红（2026-09-18 实测）。
// 正确做法不是给个空桩糊过去，而是把**出货文件里的真函数**抠出来现造成真贴底器喂进去。
function extractFn(js, name) {
  const at = js.indexOf(`function ${name}(`);
  if (at < 0) return null;
  const open = js.indexOf('{', at);
  if (open < 0) return null;
  let depth = 0;
  for (let i = open; i < js.length; i++) {
    if (js[i] === '{') depth++;
    else if (js[i] === '}') {
      depth--;
      if (depth === 0) return js.slice(at, i + 1);
    }
  }
  return null;
}

// 极简 DOM：只需要支持「按 id 查得到」「建节点计数」「appendChild/remove」。
// 建节点计数是关键判据 —— 「一片一节点」必然把它推到帧数量级。
function makeDom() {
  const byId = new Map();
  let created = 0;
  const mk = (tag) => {
    const el = {
      tagName: tag, id: '', className: '', dataset: {},
      style: { cssText: '' }, textContent: '',
      children: [], scrollTop: 0, scrollHeight: 0,
      appendChild(c) { this.children.push(c); if (c.id) byId.set(c.id, c); return c; },
      remove() { if (this.id) byId.delete(this.id); },
    };
    return el;
  };
  const log = mk('div');
  log.id = logId;
  byId.set(logId, log);
  const document = { createElement: (t) => { created++; return mk(t); } };
  const $ = (id) => byId.get(id) || null;
  return { document, $, log, created: () => created, byId };
}

const adminJS = read('web/js/admin.js');
const adminHTML = read('web/admin.html');
const block = extractCase(adminJS, 'delta');
const pinnerSrc = extractFn(adminJS, 'makeLivePinner');
const adminGo = read('internal/api/admin.go');

// 材料块住哪个容器：**从出货源码的训练通道配置里读**，不在测试里写死。
// 写死会白丢一层覆盖 —— 接线被改到别的元素上时，「块进了 tr-log 吗」还会照样绿。
// 取法：先定位训练通道特有的 `logId: 'tr-log'`，再就**近**读同一行的 materialId。
// （不能拿大括号配平去框整个配置：配置里的 onDone 是个带函数体的箭头函数，
//  花括号配平会被它带偏；就近窗口没有这个风险。）
const logIdAt = adminJS.indexOf(`logId: 'tr-log'`);
const cfgWin = logIdAt >= 0 ? adminJS.slice(logIdAt, logIdAt + 400) : '';
const logId = (cfgWin.match(/logId:\s*'([^']+)'/) || [, ''])[1];
const matId = (cfgWin.match(/materialId:\s*'([^']+)'/) || [, ''])[1];

check('从训练通道配置里读到了 logId / materialId', !!logId && !!matId,
  `实得 logId「${logId}」materialId「${matId}」—— 配置形状变了或接线被删`);
// 容器是静态写在 HTML 里的（材料块是 JS 建的，所以只查容器 id）。
check('日志容器 id 在 admin.html 里真实存在（不是悬空 id）', !!logId && adminHTML.includes(`id="${logId}"`),
  `admin.html 里没有 id="${logId}" —— 材料会挂到一个不存在的容器上`);
// 材料块 id 若等于容器 id：`$(o.materialId)` 首次就命中容器本身，于是往容器上写
// textContent —— 整个日志被这一段覆盖掉，而且不再有子节点可供数（真红会变成假绿）。
check('材料块 id ≠ 容器 id（否则第一次写入就把日志容器覆盖了）', !!matId && matId !== logId,
  `materialId 与 logId 都是「${matId}」`);

// ---- ① 抠得出真代码 ----
check('抠出 admin.js 里真正在跑的 case \'delta\' 分支', !!block, '抠不到说明前端没接 delta 帧，或写法变了导致配平失败');
check('抠出真·贴底器 makeLivePinner（delta 帧里调的就是它）', !!pinnerSrc,
  '抠不到 → 下面只能用桩糊，等于这条腿不再覆盖「材料进视野」');

// 用真函数现造贴底器：几何只要 has-effect 的数值即可（贴底逻辑与具体数值无关，
// 它只做「scrollTop = scrollHeight」和「离底 NEAR 内才跟随」）。
function mkPin() {
  const modal = {
    scrollTop: 0, scrollHeight: 5000, clientHeight: 800,
    addEventListener() { this._hasScrollListener = true; },
  };
  const log = { scrollTop: 0, scrollHeight: 3000 };
  const makeLivePinner = new Function(`return (${pinnerSrc});`)();
  let calls = 0;
  const inner = makeLivePinner(modal, log);
  return { modal, log, pinLive: () => { calls++; inner(); }, calls: () => calls };
}

// ---- ② 喂 500 帧，数节点 ----
if (block) {
  const dom = makeDom();
  const pin = pinnerSrc ? mkPin() : { pinLive: () => {}, calls: () => 0 };
  const run = new Function('ev', '$', 'document', 'pinLive', 'o',
    `let lastEvAt = 0; switch (ev.type) { ${block} } return lastEvAt;`);

  const FRAMES = 500;
  let lastAt = 0;
  let threw = null;
  for (let i = 0; i < FRAMES; i++) {
    try {
      // 第 5 个参数 = 出货源码里的通道配置（材料块 id / 日志容器都从配置里读，见上方）。
      lastAt = run(
        { type: 'delta', data: JSON.stringify({ kind: i % 2 ? 'think' : 'text', text: '片段' + i }) },
        dom.$, dom.document, pin.pinLive, { materialId: matId, logId },
      );
    } catch (e) { threw = e; break; }
  }
  check(`${FRAMES} 帧材料喂进去不抛异常`, !threw, threw ? String(threw && threw.message) : '');
  check('每帧材料都调了贴底器（材料流的同时把实况区拉进视野）',
    pin.calls() === FRAMES, `只调了 ${pin.calls()} / ${FRAMES} 次`);
  check('实况块只建 1 个 DOM 节点（不是一片一个节点）',
    dom.created() === 1, `一共建了 ${dom.created()} 个节点 —— 一片一节点会随帧数线性增长，二十分钟下来把页面拖死`);
  check('实况块在日志容器里（不是飘在页面别处）', dom.log.children.length === 1 && dom.log.children[0].id === matId);

  const mp = dom.byId.get('tr-material');
  check('正文字符有上限，不无限增长', !!mp && mp.textContent.length <= 700, mp ? `当前 ${mp.textContent.length} 字` : '块丢了');
  check('留住的是最近的片段（尾部），不是开头的', !!mp && mp.textContent.includes('片段' + (FRAMES - 1)), mp ? mp.textContent.slice(0, 40) : '');
  check('换类别时插了分隔符（思考/正文不连成一句）', !!mp && mp.textContent.includes('\n'));
  check('收到材料就算「有进度」（刷新 lastEvAt，心跳不会误报卡死）', lastAt > 0);
}

// ---- ③ 后端确实把材料发出去了，并且攒批 ----
check('后端发 delta 帧', /send\("delta"/.test(adminGo));
check('后端用攒批器（不是一片一帧）', /NewMaterialRelay\(/.test(adminGo));
check('训练 ctx 上挂了材料接收器', /skillgen\.WithDelta\(/.test(adminGo));
check('阶段边界先 Flush 再发 step（材料尾巴不串到下个阶段）',
  /relay\.Flush\(\)\s*\n\s*send\("step"/.test(adminGo));
check('训练结束再 Flush 一次（最后一阶段尾巴不丢）',
  /\}\)\s*\n\s*relay\.Flush\(\)/.test(adminGo));

// ---- ④ 尺子自己也要被守：节点数必须「边跑边采」 ----
// 训练终帧（done）一到，前端按设计把 #tr-material 收掉（admin.js「留着会让人以为还在跑」），
// 所以**窗口结束后再数节点必然得 0**。2026-09-17 线上踩过这个弯路：整条腿「--- 8/9 ok ---」，
// 红的那条其实就是尺子量错了时刻，跟被测系统无关。这条断言就守这个时刻。
const leg = read('web/tests/admin_train_progress_e2e.py');
// 用 Python 式缩进分块真解析出 while 循环体（**不能拿注释当边界**：紧贴循环末尾、
// 缩进掉回 8 空格的语句语义上在循环外，用注释切会让它落进「循环内」——
// 2026-09-17 注入 2 就是这么当的哑炮）。
const legLines = leg.split('\n');
const indOf = (l) => (l.match(/^ */) || [''])[0].length;
const wAt = legLines.findIndex((l) => l.includes('while time.time() - t0 < SAMPLE_SECONDS'));
let loopEnd = legLines.length;
if (wAt >= 0) {
  const wInd = indOf(legLines[wAt]);
  for (let i = wAt + 1; i < legLines.length; i++) {
    const l = legLines[i];
    if (l.trim() === '' || l.trim().startsWith('#')) continue;  // 空行/注释不结束块
    if (indOf(l) <= wInd) { loopEnd = i; break; }
  }
}
const loopBody = wAt >= 0 ? legLines.slice(wAt, loopEnd).join('\n') : '';
check('尺子边跑边采实况块节点数（循环体内有 mat_node_snaps.append）',
  /mat_node_snaps\.append\(/.test(loopBody));
const SEL = "eval_on_selector_all('#tr-log .material'";
const allSel = leg.split(SEL).length - 1;
const inLoopSel = loopBody.split(SEL).length - 1;
check('实况块节点只在循环内采（循环外一次都不数，终帧收块时数必得 0 = 假红）',
  inLoopSel >= 1 && allSel === inLoopSel, `文件内 ${allSel} 处，循环内 ${inLoopSel} 处`);
check('尺子用后端终态信号收工（不把「训练已跑完」算成屏幕静默）',
  /stop_reason, ended_at = 'terminal'/.test(leg));
check('时间线落盘带收工原因+时刻（分析脚本才不用瞎猜窗口右端）',
  /'stop_reason': stop_reason/.test(leg) && /'ended_at': ended_at/.test(leg));
const an = read('scripts/analyze_train_timeline.py');
check('分析脚本用真终态当窗口右端（否则尾部空窗会算成静默）', /right = ended_at if/.test(an));

if (failures) { console.log(`\n${failures} 项失败`); process.exit(1); }
console.log('\n全部通过');
