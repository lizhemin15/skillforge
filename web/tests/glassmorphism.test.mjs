#!/usr/bin/env node
// 毛玻璃层（复刻 www.deepseek.com）的前端回归防线 —— 零依赖，CI 里直接 node 跑。
//
// 这一层守的是几个"肉眼很难发现、坏了也不报错"的坑，每个都对应一条真实踩到的 bug：
//
//   Bug P — 聊天页整页滚动，输入框被挤出视口。
//           .layout 只有 min-height:100vh 没有 height，flex 子项按内容撑高，
//           docScrollH 857 > 视口 577，composerVisibleWithoutScroll=false。
//           矮屏上必须滚动才能打字；输入框还脱离 .ds-bg 色域，玻璃背后没色可透。
//
//   Bug Q — backdrop-filter 嵌套，内层玻璃看不见页面背景。
//           规范：带 backdrop-filter 的元素会成为子元素的 backdrop root。
//           .ch-input-box 嵌在 .ch-input-bar 内 → 它 blur 的是输入栏自己的半透白，
//           blur 等于白做。实测颗粒度：裸背景 4.54 → 修好后玻璃内 0.13（抹平 97%）。
//
//   还有一个"没有它整套白做"的前置：页面必须有 .ds-bg 背景渐变层。
//   纯色底上涂再多 backdrop-filter 也只会糊出纯色 —— 毛玻璃的成因是背后有色可透。
//
// 解析器实现要点（都是踩过的坑）：
//   · 选择器匹配按逗号组切分后**整项等值**比较 —— 亮边这类声明常写在
//     `.ch-card, .atx-link, ... { }` 组里；只认"名字后紧跟花括号"会漏掉。
//     同时等值比较天然挡掉前缀误命中（`.sb` 不会误吞 `.sb-toggle`）。
//   · 声明必须**合并所有同名规则、后者覆盖**，不能只看最后一条 ——
//     .ds-bg 的 position/z-index 写在前一条，后一条只补 background。
//   · CSS 变量要解析（值常写 var(--ds-glass-bg) 而非字面 rgba）。

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const WEB = join(here, '..');
const CSS_PATH = join(WEB, 'css', 'style.css');
const INDEX_PATH = join(WEB, 'index.html');
const ADMIN_PATH = join(WEB, 'admin.html');

let failures = 0;
function check(name, cond, extra = '') {
  if (cond) console.log(`  ok   ${name}`);
  else { failures++; console.log(`  FAIL ${name}${extra ? ' — ' + extra : ''}`); }
}

// ---------- CSS 小解析器 ----------

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

// 枚举所有"选择器 → 规则体"，顺序即文档顺序。@media/@supports 里的规则也照抽（扁平化）。
// 注意：必须先剥掉注释再解析 —— CSS 注释里可以出现任意字符（含 `.ds-bg {` 这种字样），
// 不剥的话注释会被当成选择器的一部分（`/* 背景层 */\n.ds-bg` ≠ `.ds-bg`），规则被整个漏掉。
function allRules(css) {
  const src = css.replace(/\/\*[\s\S]*?\*\//g, '');
  const rules = [];
  // ([^{}]+) 不会跨花括号，所以内层规则也能被单独抽到。
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

// 静态资源 → 版本号映射。
// 只认 `<link href="/assets/css/style.css?v=…">` / `<script src="…/chat.js?v=…">` 这类**真引用**，
// 不能扫全文 —— 内联 JS 里的字符串（如 fetch('js/chat.js')）会被误当成资源引用，计数虚高。
function assetRefsIn(html) {
  const out = [];
  for (const m of html.matchAll(/(?:src|href)\s*=\s*["']([^"']+\.(?:css|js))(\?v=([\w.-]+))?["']/g)) {
    out.push({ file: m[1], version: m[3] });
  }
  return out;
}
function assetVersions(html) {
  const out = {};
  for (const { file, version } of assetRefsIn(html)) if (version) out[file] = version;
  return out;
}

// ---------- 审计：把三份出货文件当输入，返回失败项列表 ----------
export function audit(rawCss, htmlIndex, htmlAdmin) {
  const problems = [];
  const need = (name, cond, extra = '') => { if (!cond) problems.push(`${name}${extra ? ' — ' + extra : ''}`); };
  const vars = cssVars(rawCss);
  const css = rawCss;
  // 1) 背景渐变层必须存在（毛玻璃的成因，没有它整套是纯色块）
  need('index.html 有 .ds-bg 背景层', /class="ds-bg"/.test(htmlIndex));
  need('admin.html 有 .ds-bg 背景层', /class="ds-bg"/.test(htmlAdmin));
  const bg = mergedDecls(css, '.ds-bg', vars);
  need('.ds-bg 固定定位', bg['position'] === 'fixed', `实际 ${bg['position']}`);
  need('.ds-bg 铺满视口', /^(0|0px)( 0(px)?)*$/.test(bg['inset'] || ''), `inset=${bg['inset']}`);
  need('.ds-bg 在内容之下 (z-index:-1)', bg['z-index'] === '-1', `实际 ${bg['z-index']}`);
  need('.ds-bg 是渐变而非纯色', /gradient\(/.test(bg['background'] || bg['background-image'] || ''));

  // 2) 颗粒层：磨砂感唯一不可伪造的证据（高频细节被 blur 抹平）
  const grain = mergedDecls(css, '.ds-bg::after', vars);
  need('.ds-bg::after 有噪点纹理', /feTurbulence/.test(grain['background-image'] || ''));
  need('.ds-bg::after 有可见不透明度', parseFloat(grain['opacity']) >= 0.3, `opacity=${grain['opacity']}`);

  // 3) 签名件：输入框必须是玻璃
  const box = mergedDecls(css, '.ch-input-box', vars);
  need('.ch-input-box 有 backdrop-filter blur', /blur\(/.test(box['backdrop-filter'] || ''), `实际 ${box['backdrop-filter']}`);
  need('.ch-input-box 有 -webkit- 前缀（Safari）', /blur\(/.test(box['-webkit-backdrop-filter'] || ''));
  need('.ch-input-box 是半透白而非实心', /rgba\(255,\s*255,\s*255,\s*(0?\.\d+)\)/.test(box['background'] || ''),
    `background=${box['background']}`);
  need('.ch-input-box 有亮白棱边', /rgba\(255,\s*255,\s*255/.test(box['border-color'] || box['border'] || ''));

  // 4) Bug Q 核心：外层容器不得用 backdrop-filter，否则内层玻璃被 backdrop root 挡住
  for (const sel of ['.ch-input-bar', '.sb']) {
    const d = mergedDecls(css, sel, vars);
    const bf = d['backdrop-filter'] || 'none';
    need(`${sel} 不得用 backdrop-filter（会挡住内层玻璃）`, bf === 'none', `实际 ${bf}`);
  }

  // 5) 卡片是玻璃 + 有亮边
  const card = mergedDecls(css, '.ch-card', vars);
  need('.ch-card 有 backdrop-filter blur', /blur\(/.test(card['backdrop-filter'] || ''));
  need('.ch-card 有亮边', /rgba\(255,\s*255,\s*255/.test(card['border-color'] || card['border'] || ''),
    `border=${card['border-color'] || card['border']}`);

  // 6) Bug P 核心：聊天页锁视口（输入框钉在视口底），admin 不锁（仍需整页滚动）
  const lock = mergedDecls(css, '.layout.ch-lock', vars);
  need('.layout.ch-lock 锁住视口高度', /100d?vh/.test(lock['height'] || ''), `实际 ${lock['height']}`);
  need('.layout.ch-lock 禁止整页滚动', lock['overflow'] === 'hidden', `实际 ${lock['overflow']}`);
  need('index.html 的 .layout 挂了 ch-lock', /class="layout ch-lock"/.test(htmlIndex));
  need('admin.html 不得挂 ch-lock（管理端要能整页滚）', !/ch-lock/.test(htmlAdmin));

  // 7) 缓存铁律：每个静态资源都要带 ?v=；**同一文件名在两页的版本号必须一致**
  //    （改了 index 忘了 admin = 有人拿到旧 CSS，这种不一致才是真 bug；
  //      不同文件之间版本号不同是正常的，不该拿来比。）
  const vi = assetVersions(htmlIndex), va = assetVersions(htmlAdmin);
  for (const [label, html, table] of [['index.html', htmlIndex, vi], ['admin.html', htmlAdmin, va]]) {
    const refs = assetRefsIn(html);
    const noVer = refs.filter((r) => !r.version).map((r) => r.file);
    need(`${label} 的静态资源都带 ?v=`, noVer.length === 0, `漏版本：${noVer.join(', ')}`);
  }
  const shared = Object.keys(vi).filter((f) => f in va);
  need('两页至少共享一个资源（否则后面的一致性检查是空集假绿）', shared.length > 0, `实际 ${shared.length} 个`);
  const mismatched = shared.filter((f) => vi[f] !== va[f]);
  need('同一资源在两页的版本号一致', mismatched.length === 0,
    mismatched.map((f) => `${f}: index=${vi[f]} admin=${va[f]}`).join('; '));

  return problems;
}

// ---------- 跑真实出货文件 ----------
const css = readFileSync(CSS_PATH, 'utf8');
const htmlIndex = readFileSync(INDEX_PATH, 'utf8');
const htmlAdmin = readFileSync(ADMIN_PATH, 'utf8');

console.log('A. 毛玻璃层契约（背景层 / 玻璃面 / backdrop root / 视口锁定）');
const problems = audit(css, htmlIndex, htmlAdmin);
if (problems.length === 0) console.log('  ok   全部契约成立');
else { failures += problems.length; problems.forEach((p) => console.log(`  FAIL ${p}`)); }

// ---------- B. 自证：注入破坏必须变红（防"骑空集上假绿"）----------
// 每条防线都当场打一针反向破坏，确认审计真的会红。断言一旦退化成恒真，
// 这里立刻暴露 —— 光测"改动后是绿的"证明不了断言有效。
console.log('B. 自证：注入破坏必须变红');
// 变异一律**追加到样式表末尾** —— 层叠里最后一条赢，所以这样改必然真的生效。
// （曾经写成"替换第一条匹配的规则"，结果被后面的规则覆盖 → 变异是空操作，
//   看起来像"断言太弱"，其实是针没扎进去。变异无效比断言无效更隐蔽。）
const mutations = [
  ['去掉背景层', (c, i, a) => [c, i.replace(/class="ds-bg"/, 'class="ds-bg-removed"'), a]],
  ['.ds-bg 改成纯色', (c, i, a) => [c + '\n.ds-bg { background: #ffffff; background-image: none; }\n', i, a]],
  ['去掉颗粒层', (c, i, a) => [c + '\n.ds-bg::after { background-image: none; }\n', i, a]],
  ['颗粒层调到看不见', (c, i, a) => [c + '\n.ds-bg::after { opacity: .02; }\n', i, a]],
  ['输入框去掉 blur', (c, i, a) => [c + '\n.ch-input-box { backdrop-filter: none; -webkit-backdrop-filter: none; }\n', i, a]],
  ['输入框改实心', (c, i, a) => [c + '\n.ch-input-box { background: rgb(255,255,255); }\n', i, a]],
  ['输入框去掉亮边', (c, i, a) => [c + '\n.ch-input-box { border-color: rgba(0,0,0,.2); }\n', i, a]],
  ['卡片去掉 blur', (c, i, a) => [c + '\n.ch-card { backdrop-filter: none; -webkit-backdrop-filter: none; }\n', i, a]],
  // 复现 Bug Q：给外层容器重新加上 backdrop-filter
  ['输入栏重新加上 blur（Bug Q 复现）', (c, i, a) => [c + '\n.ch-input-bar { backdrop-filter: blur(12px); }\n', i, a]],
  ['侧边栏重新加上 blur（Bug Q 复现）', (c, i, a) => [c + '\n.sb { backdrop-filter: blur(12px); }\n', i, a]],
  // 复现 Bug P：锁定被去掉 / 被误加到 admin
  ['去掉视口锁定', (c, i, a) => [c + '\n.layout.ch-lock { height: auto; min-height: 100vh; overflow: visible; }\n', i, a]],
  ['admin 误挂 ch-lock（Bug P 回归）', (c, i, a) => [c, i, a.replace(/class="layout"/, 'class="layout ch-lock"')]],
  ['index 漏挂 ch-lock', (c, i, a) => [c, i.replace(/class="layout ch-lock"/, 'class="layout"'), a]],
  // 缓存：admin 的 style.css 版本落后于 index（改了 index 忘 admin）
  ['admin 版本落后', (c, i, a) => [c, i, a.replace(/(style\.css\?v=)([\w.-]+)/, (m, p, v) => p + v + '-stale')]],
];

for (const [label, mutate] of mutations) {
  const [mc, mi, ma] = mutate(css, htmlIndex, htmlAdmin);
  // 先确认这针真的扎进去了（变异是空操作就不能拿来判断言强弱）
  if (mc === css && mi === htmlIndex && ma === htmlAdmin) {
    failures++; console.log(`  FAIL 变异「${label}」是空操作 —— 没改动任何文件，测不出断言强弱`);
    continue;
  }
  let got = null;
  try { got = audit(mc, mi, ma); } catch (e) { got = ['(解析抛错) ' + e.message]; }
  check(`破坏「${label}」→ 审计变红`, got.length > 0);
}

// C. 解析器自证：防止解析器本身退化成"什么都匹配不到"（那样 A 段会静默假绿）
console.log('C. 解析器自证（保证断言不是骑在空集上）');
const vars = cssVars(css);
const probe = mergedDecls(css, '.ch-input-box', vars);
check('解析器能取到 .ch-input-box 的多条声明', Object.keys(probe).length >= 5, `只取到 ${Object.keys(probe).length} 条`);
check('逗号组选择器能被解析（.ch-card 取到 border-color）', !!probe && !!mergedDecls(css, '.ch-card', vars)['border-color']);
check('CSS 变量能被解析（--ds-glass-bg 换成真值）', /rgba\(255/.test(vars['--ds-glass-bg'] || ''), `实际 ${vars['--ds-glass-bg']}`);
// 拿一小段合成 CSS 验"整项等值匹配"本身：`.sb` 不能吃掉 `.sb-toggle` 的声明。
// （不能写成 `|| true` 那种兜底 —— 那就是恒真断言，等于没测。）
const eqProbe = mergedDecls('.sb { padding: 1px; } .sb-toggle { position: absolute; }', '.sb', {});
check('整项等值匹配挡得住前缀误命中（.sb 不吃 .sb-toggle）',
  eqProbe['padding'] === '1px' && !('position' in eqProbe), JSON.stringify(eqProbe));

console.log(failures === 0
  ? '\n全绿：毛玻璃层的成因（背景层）、质感（颗粒）、机制（不嵌套 backdrop root）与布局（视口锁定）都在。'
  : `\n${failures} 项失败。`);
process.exit(failures === 0 ? 0 : 1);
