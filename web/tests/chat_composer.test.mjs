#!/usr/bin/env node
// 聊天输入区（composer）布局 + 贴底滚动的前端回归防线 —— 零依赖，CI 里直接 node 跑。
//
// 守两条「肉眼一眼看得见、代码一个错都不报」的坑：
//
//   Bug Y — 输入框被「自动调度 / 指定技能」胶囊挤到右边。
//           胶囊 `#ch-switch` 与 textarea 同处一个 display:flex 的 .ch-input-box，
//           而自己是 flex:none（宽 166px）→ textarea 要到框内 x≈518 才开始，
//           宽度只剩 630/860（DOM 实测：textarea.x=518、w=630，框宽 860）。
//           用户原话：「输入框被 自动调度、指定技能 挤到右边，应当占据所有的位置」。
//           还有一个连带的坑：样式表顶部那条**给 admin 表单用的**元素选择器规则
//           `textarea { min-height: 96px }` 会漏进聊天输入框（`.ch-input` 不写
//           min-height 就吃全局的），空输入框高 96px、正文只有 23px，一大片死区。
//
//   Bug Z — 对话不自动往下滚。两个成因叠在一起：
//           ① `.ch-scroll` 上是 `scroll-behavior: smooth`，而流式输出每几十毫秒写一次
//              `scrollTop = scrollHeight` —— 平滑动画每次赋值都被打断重启，滚动永远
//              追不上正文（实测落后 994px，约三分之二的答案在视口外）；
//           ② 只赋一次值不够：markdown 重排 / 字体图片落地会在赋值**之后**继续改
//              scrollHeight，只赋一次就停在中途（观感是「差一行没到底」）。
//
// 做法：CSS 用项目既有的合并式解析器取**有效声明**（同名规则后者覆盖、逗号组整项
// 等值匹配、var() 解析成真值）；结构用 DOM-free 的标签深度扫描读输入框的**直系
// 子元素**；JS 用标记切片（composer:stick-begin/end）把出货文件里真正跑的
// keepBottom 抽出来在桩上跑 —— 而不是在测试里抄一份实现。抄一份的话，真文件改坏了
// 测试照样绿。
//
// 断言自证：web/tests/chat_composer_mutation_check.sh
// （6 条注入破坏 + 1 条「标记被删」防静默跳过，要求「全红 → 还原 → 全绿」）

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const WEB = join(here, '..');
const CSS_PATH = join(WEB, 'css', 'style.css');
const INDEX_PATH = join(WEB, 'index.html');
const CHAT_JS_PATH = join(WEB, 'js', 'chat.js');

const CSS = readFileSync(CSS_PATH, 'utf8');
const INDEX = readFileSync(INDEX_PATH, 'utf8');
const CHAT_JS = readFileSync(CHAT_JS_PATH, 'utf8');

let failures = 0;
function check(name, cond, extra = '') {
  if (cond) console.log(`  ok   ${name}`);
  else { failures++; console.log(`  FAIL ${name}${extra ? ' — ' + extra : ''}`); }
}

// ---------- CSS 小解析器（与 glassmorphism.test.mjs 同一套语义）----------

// 从 `{` 起取配平的花括号体。
function balancedBody(css, braceStart) {
  let depth = 0;
  for (let i = braceStart; i < css.length; i++) {
    if (css[i] === '{') depth++;
    else if (css[i] === '}') { depth--; if (depth === 0) return css.slice(braceStart + 1, i); }
  }
  throw new Error('花括号不配平，无法解析 CSS');
}

// 按分号切声明，但不切进括号里（data:URI 里可能有分号）。
function splitDecls(body) {
  const out = [];
  let buf = '', depth = 0;
  for (const c of body) {
    if (c === '(') depth++;
    else if (c === ')') depth--;
    if (c === ';' && depth === 0) { out.push(buf); buf = ''; } else buf += c;
  }
  if (buf.trim()) out.push(buf);
  return out;
}

// 枚举所有「选择器 → 规则体」，顺序即文档顺序；@media 里的规则也抽（扁平化）。
// 必须先剥注释再解析：注释里可以出现任意字符（含 `.x {` 这种字样），
// 不剥的话注释会被当成选择器的一部分、规则被整个漏掉。
function allRules(css) {
  const src = css.replace(/\/\*[\s\S]*?\*\//g, '');
  const rules = [];
  const re = /([^{}]+)\{/g;
  let m;
  while ((m = re.exec(src)) !== null) {
    const selText = m[1].trim();
    if (!selText || selText.startsWith('@')) continue;
    rules.push({ selectors: selText.split(',').map((s) => s.trim()), body: balancedBody(src, m.index + m[1].length) });
  }
  return rules;
}

// :root 里的自定义属性，用于把 var(--x) 换成真值。
function cssVars(css) {
  const vars = {};
  for (const r of allRules(css)) {
    if (!r.selectors.includes(':root')) continue;
    for (const part of splitDecls(r.body)) {
      const i = part.indexOf(':');
      if (i < 0) continue;
      const prop = part.slice(0, i).trim();
      if (prop.startsWith('--')) vars[prop] = part.slice(i + 1).replace(/\/\*[\s\S]*?\*\//g, '').trim();
    }
  }
  return vars;
}

function resolveVars(value, vars, depth = 0) {
  if (depth > 5 || !value) return value;
  const next = value.replace(/var\(\s*(--[\w-]+)\s*(?:,[^)]*)?\)/g, (whole, name) =>
    vars[name] !== undefined ? vars[name] : whole);
  return next === value ? next : resolveVars(next, vars, depth + 1);
}

// 选择器在整份样式表里的**有效声明**：所有含该选择器的规则按序合并，后者覆盖前者。
// 逗号组内按整项等值匹配（`.ch-card` 命中 `.ch-card, .atx-link` 组，但 `.sb` 不会命中 `.sb-toggle`）。
function mergedDecls(css, selector, vars) {
  const decls = {};
  for (const r of allRules(css)) {
    if (!r.selectors.includes(selector)) continue;
    for (const part of splitDecls(r.body)) {
      const i = part.indexOf(':');
      if (i < 0) continue;
      const prop = part.slice(0, i).trim().toLowerCase();
      if (!prop || prop.startsWith('--')) continue;
      decls[prop] = resolveVars(part.slice(i + 1).replace(/!important/gi, '').replace(/\/\*[\s\S]*?\*\//g, '').trim(), vars);
    }
  }
  return decls;
}

// flex 简写 → 实际 flex-basis（按 CSS 语义还原），用来判断「这个盒子是不是整行」。
// 只认 flex-basis 是没法判断的：整行常写成简写 `flex: 0 0 100%`。
function flexBasis(decls) {
  const f = (decls['flex'] || '').replace(/\s+/g, ' ').trim();
  if (f) {
    if (f === 'none' || f === 'auto' || f === 'initial') return 'auto';
    const parts = f.split(' ');
    if (parts.length >= 3) return parts[2];
    if (parts.length === 1) return /^\d/.test(parts[0]) ? '0%' : parts[0];
    return '0%'; // `flex: <grow> <shrink>` → basis 缺省 0%
  }
  return decls['flex-basis'] || 'auto';
}

// ---------- HTML 标签深度扫描（DOM-free，够用且不引第三方依赖）----------

const TAG = `<(\\/?)([a-zA-Z][\\w-]*)((?:[^>"']|"[^"]*"|'[^']*')*)>`;

const stripComments = (html) => html.replace(/<!--[\s\S]*?-->/g, '');

// 某个 needle（如 `class="ch-input-box"`）所在元素的标签名与开标签区间。
function openTagOf(html, needle) {
  const i = html.indexOf(needle);
  if (i < 0) return null;
  const lt = html.lastIndexOf('<', i);
  const gt = html.indexOf('>', i);
  if (lt < 0 || gt < 0) return null;
  const tag = (html.slice(lt + 1).match(/^[a-zA-Z][\w-]*/) || [''])[0].toLowerCase();
  if (!tag) return null;
  return { tag, openStart: lt, openEnd: gt };
}

// needle 所在元素的内部（与它配对的闭合标签之前的全部内容）。
function innerOf(html, needle) {
  const o = openTagOf(html, needle);
  if (!o) return null;
  const re = new RegExp(`<(\\/?)${o.tag}\\b((?:[^>"']|"[^"]*"|'[^']*')*)>`, 'g');
  re.lastIndex = o.openStart;
  let depth = 0, m;
  while ((m = re.exec(html)) !== null) {
    if (m[1] === '/') depth--;
    else if (!/\/\s*$/.test(m[2])) depth++;
    if (depth === 0 && m.index > o.openStart) return html.slice(o.openEnd + 1, m.index);
  }
  return null;
}

// 一段 HTML 里深度为 0 的元素（直系子元素），含其属性原文。
function topChildren(inner) {
  const out = [];
  const re = new RegExp(TAG, 'g');
  let depth = 0, m;
  while ((m = re.exec(inner)) !== null) {
    if (m[1] === '/') { depth--; continue; }
    if (depth === 0) out.push({ tag: m[2].toLowerCase(), attrs: m[3] });
    if (!/\/\s*$/.test(m[3])) depth++;
  }
  return out;
}

const html = stripComments(INDEX);
const hasAttr = (attrs, needle) => attrs.includes(needle);
const desc = (c) => {
  const cls = (c.attrs.match(/class="([^"]*)"/) || [, ''])[1];
  const id = (c.attrs.match(/id="([^"]*)"/) || [, ''])[1];
  return `${c.tag}${id ? '#' + id : ''}${cls ? '.' + cls.split(/\s+/).join('.') : ''}`;
};

// ---------- A. 布局：输入框必须占满整行 ----------
console.log('A. 布局：胶囊独占一行，输入框占满剩余宽度');

const vars = cssVars(CSS);
const mrow = mergedDecls(CSS, '.ch-mrow', vars);
check('.ch-mrow 存在且有声明', Object.keys(mrow).length > 0, '一条都没解析到 → style.css 里那条定稿块丢了？');
check('.ch-mrow 是整行（flex-basis = 100%）', flexBasis(mrow) === '100%', `实际 basis=${flexBasis(mrow)}（flex=${mrow['flex']}）`);
check('.ch-mrow 不参与伸缩（grow = 0）', /^0\b/.test((mrow['flex'] || '').trim().replace(/\s+/g, ' ')), `flex=${mrow['flex']}`);
check('.ch-mrow .ch-switch 存在（行内对齐被显式接管，免得胶囊被 align-self 又拉走）',
  Object.keys(mergedDecls(CSS, '.ch-mrow .ch-switch', vars)).length > 0);

const chInput = mergedDecls(CSS, '.ch-input', vars);
check('.ch-input 撑满剩余宽度（flex: 1）', (chInput['flex'] || '').trim() === '1', `flex=${chInput['flex']}`);
check('.ch-input 允许收缩（min-width: 0）', (chInput['min-width'] || '').trim() === '0', `min-width=${chInput['min-width']}`);

// min-height：全局 `textarea { min-height: 96px }` 是元素选择器，会漏进 .ch-input。
// 这里比的是**有效值**（类选择器覆盖元素选择器），不是「那条全局规则还在不在」。
const globalTa = mergedDecls(CSS, 'textarea', vars);
const effMinH = chInput['min-height'] !== undefined ? chInput['min-height'] : globalTa['min-height'];
check('.ch-input 生效的 min-height 是 0（只有自己定死，才不吃全局 textarea 的 96px）',
  effMinH !== undefined && parseFloat(effMinH) === 0,
  `有效值=${effMinH}（.ch-input=${chInput['min-height']}，全局 textarea=${globalTa['min-height']}）`);

// 结构：直系子元素顺序必须是「胶囊行 → textarea → 发送键」
const boxInner = innerOf(html, 'class="ch-input-box"');
check('index.html 里能找到 .ch-input-box 的内部', typeof boxInner === 'string' && boxInner.length > 0);
const kids = boxInner ? topChildren(boxInner) : [];
console.log(`  （.ch-input-box 的直系子元素：${kids.map(desc).join(' → ') || '(空)'}）`);
check('.ch-input-box 的直系子元素是「胶囊行 → textarea#chat-input → 发送键」',
  kids.length === 3 &&
    kids[0].tag === 'div' && hasAttr(kids[0].attrs, 'class="ch-mrow"') &&
    kids[1].tag === 'textarea' && hasAttr(kids[1].attrs, 'id="chat-input"') &&
    kids[2].tag === 'button',
  kids.map(desc).join(' → '));

const mrowInner = innerOf(html, 'class="ch-mrow"');
check('胶囊 #ch-switch 在 .ch-mrow 里（不是 .ch-input-box 的直系子元素，不占 input 的行）',
  !!mrowInner && mrowInner.includes('id="ch-switch"') && !kids.some((c) => hasAttr(c.attrs, 'id="ch-switch"')),
  `mrow 内是否含 ch-switch=${!!mrowInner && mrowInner.includes('id="ch-switch"')}`);
check('textarea 没有被一起塞进 .ch-mrow（塞进去胶囊还是跟它同行）',
  !!mrowInner && !mrowInner.includes('id="chat-input"'));

// ---------- B. 贴底滚动：容器不许 smooth，赋值后必须补帧 ----------
console.log('B. 贴底滚动：容器不许 smooth，赋值后必须补帧到底');

const scrollBox = mergedDecls(CSS, '.ch-scroll', vars);
check('.ch-scroll 的 scroll-behavior 不是 smooth（流式每次赋值都会重启动画、滚动永远追不上）',
  (scrollBox['scroll-behavior'] || 'auto') !== 'smooth', `实际 ${scrollBox['scroll-behavior']}`);
check('.ch-scroll 仍是可滚动容器（overflow-y 没被误删）',
  /^(auto|scroll)$/.test(scrollBox['overflow-y'] || ''), `overflow-y=${scrollBox['overflow-y']}`);

// 按标记切片，从出货文件里抽出真正跑的 keepBottom（不是测试里抄的副本）。
const BEGIN = '// --- composer:stick-begin';
const END = '// --- composer:stick-end';
const stickSrc = (() => {
  const a = CHAT_JS.indexOf(BEGIN), b = CHAT_JS.indexOf(END);
  return a < 0 || b <= a ? null : CHAT_JS.slice(a, b);
})();

function makeHarness(winOverride) {
  const queue = [];
  const win = winOverride !== undefined ? winOverride
    : { requestAnimationFrame: (cb) => { queue.push(cb); return queue.length; } };
  const scroll = { scrollTop: 0, scrollHeight: 1000 };
  const fn = new Function('scroll', 'window', `"use strict";\n${stickSrc}\nreturn keepBottom;`)(scroll, win);
  return { queue, win, scroll, fn };
}
// 把排队的帧全跑掉（补帧里还会再排帧，跑空为止）。
function runFrames(h, max = 10) {
  let n = 0;
  while (h.queue.length && n++ < max) h.queue.shift()();
  return n;
}

if (!stickSrc) {
  check('能从 chat.js 切到 keepBottom 的实现（composer:stick-begin/end 标记还在）', false,
    '标记被删 → 下面 B 段会全部静默跳过，那才是真的失去防线');
} else {
  const h1 = makeHarness();
  h1.fn();
  check('keepBottom 调一次就把视口顶到底（同步赋值，不等下一帧）',
    h1.scroll.scrollTop === 1000 && h1.scroll.scrollTop === h1.scroll.scrollHeight, `scrollTop=${h1.scroll.scrollTop}`);

  // 关键一条：模拟「赋值之后 markdown 重排 / 图片落地又长高了」，补帧必须把它吃掉。
  h1.scroll.scrollHeight = 1400;
  runFrames(h1);
  check('赋值之后内容又长高（重排/图片落地），补帧把视口重新顶到底',
    h1.scroll.scrollTop === 1400, `scrollTop=${h1.scroll.scrollTop} / scrollHeight=1400`);

  const h2 = makeHarness();
  h2.fn(); h2.fn(); h2.fn();
  check('流式期间连续调用只排一次补帧（不是每帧排队，不跟渲染抢）',
    h2.queue.length === 1, `排了 ${h2.queue.length} 帧`);

  const h3 = makeHarness();
  h3.fn();
  h3.queue.shift()();
  check('补帧不是一次性的：第一帧结束后再排一帧（把二次增长也吃掉）',
    h3.queue.length === 1, `第一帧后剩余 ${h3.queue.length} 帧`);

  const h4 = makeHarness({});
  let threw = false;
  try { h4.fn(); } catch (e) { threw = true; }
  check('环境没有 requestAnimationFrame 时也不抛，同步赋值仍然生效',
    !threw && h4.scroll.scrollTop === 1000, `threw=${threw} scrollTop=${h4.scroll.scrollTop}`);
}

// ---------- C. 断言自证：别骑在空集 / 空切片上 ----------
console.log('C. 自证（解析器与切片本身有效，断言不是恒真）');

check('切片非空且含 scrollTop 赋值', !!stickSrc && stickSrc.includes('scrollTop'),
  `切片长度=${stickSrc ? stickSrc.length : 0}`);
check('CSS 解析器能取到 .ch-input-box 的多条声明',
  Object.keys(mergedDecls(CSS, '.ch-input-box', vars)).length >= 3,
  `只取到 ${Object.keys(mergedDecls(CSS, '.ch-input-box', vars)).length} 条`);
check('CSS 变量能被解析成真值（--text 之类不留在 var(...) 里）',
  Object.values(vars).some((v) => /^#|^rgb/.test(v)), `vars 条数=${Object.keys(vars).length}`);
// flexBasis 本身要准，否则「整行」那条断言可能恒真或恒假。
check('flexBasis 语义自证（0 0 100% → 100%；0 0 auto → auto；1 → 0%；none → auto）',
  flexBasis({ flex: '0 0 100%' }) === '100%' && flexBasis({ flex: '0 0 auto' }) === 'auto' &&
  flexBasis({ flex: '1' }) === '0%' && flexBasis({ flex: 'none' }) === 'auto',
  [flexBasis({ flex: '0 0 100%' }), flexBasis({ flex: '0 0 auto' }), flexBasis({ flex: '1' }), flexBasis({ flex: 'none' })].join('/'));
check('整项等值匹配挡得住前缀误命中（.ch-input 不吃 .ch-input-box）',
  JSON.stringify(mergedDecls('.ch-input { padding: 1px; } .ch-input-box { position: relative; }', '.ch-input', {})) === '{"padding":"1px"}');
// 标签扫描器自证：注释要被剥掉、自闭合标签不占深度、嵌套子元素不能算成直系子元素。
const probeHtml = stripComments('<div class="x"><!-- <div class="fake"> --><div class="a"><path/></div><textarea id="t"></textarea></div>');
const probeKids = topChildren(innerOf(probeHtml, 'class="x"'));
check('标签扫描自证（剥注释 / 自闭合不占深度 / 孙辈不算子元素）',
  probeKids.length === 2 && probeKids[0].tag === 'div' && probeKids[1].tag === 'textarea',
  probeKids.map(desc).join(' → '));

if (failures) { console.log(`\n${failures} 项失败`); process.exit(1); }
console.log('\n全部通过');
