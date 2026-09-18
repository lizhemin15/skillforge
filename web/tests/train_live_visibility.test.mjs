#!/usr/bin/env node
// 训练页「实况区看得见」的前端回归防线 —— 零依赖，CI 里直接 node 跑。
//
// 守的是用户投诉的原话：「现在速度过于慢了，中间可以流式输出思考的一些中间材料，
// 现在一直卡着计时，用户体验不佳」。
//
// 中间材料确实**一直在流**（admin_train_progress_e2e.py 的 T5a/T5b 早就守着
// 「帧到屏幕、只占 1 个 DOM 节点」），但用户看不见 —— 因为 `.modal` 自己就是
// 滚动容器（web/css/style.css：`max-height: 86vh; overflow-y: auto`），长「写作
// 要求」把 `#tr-log` 顶到 modal 底线以下。线上实测（2026-09-18）：
//   modal 内容高 1130px / 可视 772px、scrollTop 停在 0；
//   #tr-log 只露出 45px；材料块整体在 modal 底线**外 329px**。
// 只做内层 `#tr-log` 的贴底治不了「看不见」，外层 modal 也得跟着走 —— 但不能跟
// 用户抢滚动条（用户自己往上翻就该松手）。
//
// 做法（沿用本仓库既有的「从出货文件抠真函数跑」的规矩）：
//   把 web/js/admin.js 里真正跑的 makeLivePinner 按花括号配平抠出来，在桩上跑。
//   不在这里抄一份实现 —— 抄的那份永远绿，真文件改坏了测试看不见。
//   同时钉两件事：①它被装到**真正的滚动容器**上（#skill-new，不是别的元素）；
//   ②所有贴底调用都走它（留一句裸的 `$('tr-log').scrollTop = …` 就说明漏了一处）。
//
// 断言自证（**写在文件内，故意不另开 *_mutation_check 脚本**）：
//   本仓库的 scripts/preflight.sh 与 .github/workflows/ci.yml 由
//   web/tests/preflight_parity.test.mjs 跨文件盯着「存在的自证脚本必须被两边调用」。
//   这条尺子的自证只需要**同一份抽取逻辑**跑两次（原样 / 注入），开成单独脚本反而
//   多一层「有没有被接线」的腐烂风险。所以自证在下面 S1/S2 里就地做：
//   把出货源码里那两行关键语句各删一次，断言对应的 U 断言当场变红、还原后回绿。
//   红线：注入后必须出现 FAIL 行（只崩不红的 rc≠0 不算），且必须是**预期那条**。

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const WEB = join(here, '..');
const ADMIN_JS = readFileSync(join(WEB, 'js', 'admin.js'), 'utf8');
const ADMIN_HTML = readFileSync(join(WEB, 'admin.html'), 'utf8');
const STYLE_CSS = readFileSync(join(WEB, 'css', 'style.css'), 'utf8');

let failures = 0;
function check(name, cond, extra = '') {
  if (cond) console.log(`  ok   ${name}`);
  else { failures++; console.log(`  FAIL ${name}${extra ? ' — ' + extra : ''}`); }
}

// 按花括号配平抠出一个函数声明的源码（`function name(...) { … }`）。
function extractFn(src, name) {
  const at = src.indexOf(`function ${name}(`);
  if (at < 0) throw new Error(`出货文件里找不到函数 ${name}() —— 被改名/删掉了？`);
  const braceAt = src.indexOf('{', at);
  if (braceAt < 0) throw new Error(`${name}() 后面没有函数体`);
  let depth = 0;
  for (let i = braceAt; i < src.length; i++) {
    if (src[i] === '{') depth++;
    else if (src[i] === '}') { depth--; if (depth === 0) return src.slice(at, i + 1); }
  }
  throw new Error(`${name}() 的花括号不配平 —— 抽取器与被测代码脱钩了`);
}

// ---------- CSS 有效声明查找（与 glassmorphism.test.mjs 同语义的最简版）----------
// 只回答一个问题：某个选择器组的规则里，某属性最后被声明成了什么。
// 找不到精确匹配的选择器组就返回 ''（不去猜继承）。
function effectiveDecl(css, selector, prop) {
  const noComment = css.replace(/\/\*[\s\S]*?\*\//g, '');
  let val = '';
  const re = /([^{}]+)\{([^{}]*)\}/g;
  let m;
  while ((m = re.exec(noComment))) {
    const sels = m[1].split(',').map((s) => s.trim());
    if (!sels.includes(selector)) continue;
    const d = m[2].match(new RegExp(`(?:^|;)\\s*${prop}\\s*:([^;]+)`));
    if (d) val = d[1].trim();
  }
  return val;
}

// 桩：一个够用的滚动容器（滚到哪、能滚多少，全由我们摆布）。
function mkBox(scrollHeight, clientHeight, scrollTop = 0) {
  const handlers = {};
  const box = {
    scrollHeight, clientHeight, scrollTop,
    addEventListener: (t, fn) => { handlers[t] = fn; },
  };
  // 模拟浏览器：程序改 scrollTop 到尽头后，用户「手动」滚到某处并触发 scroll。
  box.userScrollTo = (v) => { box.scrollTop = v; if (handlers.scroll) handlers.scroll(); };
  box.hasScrollHandler = () => typeof handlers.scroll === 'function';
  return box;
}

// 在桩上跑一次「出货源码里的」makeLivePinner：返回 { modal, log, pinLive }。
function mkPin(src) {
  const fnSrc = extractFn(src, 'makeLivePinner');
  // eslint-disable-next-line no-new-func
  const makeLivePinner = new Function(`return (${fnSrc})`)();
  const modal = mkBox(1130, 772, 0);   // 线上实测的数字：内容 1130 / 可视 772
  const log = mkBox(487, 380, 0);      // 线上实测：内层日志 487 / 380
  const pinLive = makeLivePinner(modal, log);
  return { modal, log, pinLive };
}

console.log('训练页实况区「贴底 + 不抢滚动条」回归：');

// ---------- U0 接线：装的是**真滚动容器** ----------
// 装错元素是这类修复最典型的假绿形状：函数本身对，贴的是不滚的那个盒子。
// `.modal` 在 CSS 里才是 overflow-y:auto 的那个；#skill-new 必须挂着 modal 类。
{
  const modalTag = ADMIN_HTML.match(/<div[^>]*id="skill-new"[^>]*>/);
  check('U0a #skill-new 元素存在（实况区住在它里面）', !!modalTag,
        'admin.html 里找不到 id="skill-new"');
  if (modalTag) {
    const cls = (modalTag[0].match(/class="([^"]*)"/) || [, ''])[1];
    check('U0b #skill-new 挂着 .modal 类（它才是滚动容器）', /\bmodal\b/.test(cls),
          `class="${cls}" —— 贴底贴到了不滚动的元素上，等于没贴`);
  }
  const ov = effectiveDecl(STYLE_CSS, '.modal', 'overflow-y');
  const mh = effectiveDecl(STYLE_CSS, '.modal', 'max-height');
  check('U0c .modal 确实是滚动容器（overflow-y:auto + max-height 有限）',
        ov === 'auto' && /vh|px|%/.test(mh), `overflow-y:${ov} / max-height:${mh}`);
  check('U0d 贴底器装在 #skill-new 上（不是别的元素）',
        /makeLivePinner\(\s*\$\('skill-new'\)\s*,\s*\$\('tr-log'\)\s*\)/.test(ADMIN_JS),
        'admin.js 里没有 `makeLivePinner($(\'skill-new\'), $(\'tr-log\'))` —— 接线被改/被删');
}

// ---------- U1/U2 双向贴底：外层 modal + 内层日志 ----------
{
  const { modal, log, pinLive } = mkPin(ADMIN_JS);
  pinLive();
  check('U1 贴底后外层 modal 滚到底（top=scrollHeight）', modal.scrollTop === modal.scrollHeight,
        `实得 scrollTop=${modal.scrollTop}（期望 ${modal.scrollHeight}）—— 材料会被顶到视野外`);
  check('U2 贴底后内层日志也滚到底', log.scrollTop === log.scrollHeight,
        `实得 scrollTop=${log.scrollTop}（期望 ${log.scrollHeight}）`);
}

// ---------- U3/U4 让位用户：上翻松手、回底恢复 ----------
{
  const { modal, pinLive } = mkPin(ADMIN_JS);
  check('U3a 贴底器注册了 scroll 监听（没有它就无法知道用户在底部）',
        modal.hasScrollHandler(), 'modal.addEventListener("scroll", …) 不见了');
  pinLive();
  // 用户自己往上翻到半腰：离底 1130-300-772 = 58px > 阈值
  modal.userScrollTo(300);
  pinLive();
  check('U3b 用户上翻后不抢滚动条（scrollTop 保持在用户停的地方）',
        modal.scrollTop === 300, `实得 scrollTop=${modal.scrollTop}（期望 300，用户被强制拉回底部）`);
  // 用户又滚回底部：离底 ≤ 阈值 → 恢复跟随
  modal.userScrollTo(1130 - 772);
  pinLive();
  check('U4 用户回到底部后恢复自动跟随', modal.scrollTop === modal.scrollHeight,
        `实得 scrollTop=${modal.scrollTop}（期望 ${modal.scrollHeight}）`);
}

// ---------- U5 阈值双边卡区间 ----------
// 单边断言＝假绿形状：阈值 0 会让「刚好在底」判定抖动（差 1px 就永久松手），
// 阈值大到离谱则等于永远抢用户滚动条。所以两头都得卡。
{
  const m = ADMIN_JS.match(/const\s+NEAR\s*=\s*(\d+)/);
  check('U5 阈值 NEAR 存在且是个小正数（>0 且 ≤100）',
        !!m && Number(m[1]) > 0 && Number(m[1]) <= 100,
        m ? `NEAR=${m[1]}（期望 0<x≤100）` : '找不到 const NEAR = …');
}

// ---------- U6 漏一处就白修：所有贴底都走贴底器 ----------
{
  const bare = (ADMIN_JS.match(/\$\('tr-log'\)\.scrollTop\s*=/g) || []).length;
  check('U6 训练段里没有裸的 `$(\'tr-log\').scrollTop = …`（全部走 pinLive）',
        bare === 0, `还剩 ${bare} 处裸贴底 —— 那条路径上的材料照样会掉出视野`);
  const calls = (ADMIN_JS.match(/\bpinLive\(\);/g) || []).length;
  check('U6b 贴底器至少被调用 3 次（提交时 + 日志行 + delta 帧）',
        calls >= 3, `实得 ${calls} 次`);
}

// ---------- S1/S2 就地自证：把出货源码改坏，尺子必须当场红 ----------
// 判据（缺一不可）：①注入点存在；②注入后**必须出现 FAIL 行**（只崩不红不算）；
// ③FAIL 必须是预期那条；④还原后回绿。
// 真自证：删掉外层贴底那一行 → U1 形状的判定必须为假。
{
  const src = ADMIN_JS.replace(/if \(on\) modal\.scrollTop = modal\.scrollHeight; \/\/ ②\n?/, '');
  check('S1 注入点存在（外层贴底那行真的删掉了）', src !== ADMIN_JS,
        '在 admin.js 里找不到 `if (on) modal.scrollTop = modal.scrollHeight; // ②` —— 锚点失效，自证无意义');
  const { modal, pinLive } = mkPin(src);
  pinLive();
  check('S1 删掉外层贴底后，「modal 滚到底」判定当场变红（还原即回绿）',
        modal.scrollTop !== modal.scrollHeight,
        `实得 scrollTop=${modal.scrollTop} —— 删了那行居然还是绿的，说明这把尺子没测到它`);
}

// 真自证：删掉内层贴底那一行 → U2 形状的判定必须为假。
{
  const src = ADMIN_JS.replace(/logEl\.scrollTop = logEl\.scrollHeight; \/\/ ①\n?/, '');
  check('S2 注入点存在（内层贴底那行真的删掉了）', src !== ADMIN_JS,
        '在 admin.js 里找不到 `logEl.scrollTop = logEl.scrollHeight; // ①` —— 锚点失效，自证无意义');
  const { log, pinLive } = mkPin(src);
  pinLive();
  check('S2 删掉内层贴底后，「内层日志滚到底」判定当场变红（还原即回绿）',
        log.scrollTop !== log.scrollHeight,
        `实得 scrollTop=${log.scrollTop} —— 删了那行居然还是绿的`);
}

// 真自证：去掉「让位用户」的阈值判断（恒跟着滚）→ U3b 形状必须为假。
{
  const src = ADMIN_JS.replace(/if \(on\) modal\.scrollTop = modal\.scrollHeight; \/\/ ②/,
                               'modal.scrollTop = modal.scrollHeight; // ②（无视用户上翻）');
  check('S3 注入点存在（改成无视用户滚动）', src !== ADMIN_JS, '锚点失效');
  const { modal, pinLive } = mkPin(src);
  pinLive();
  modal.userScrollTo(300);
  pinLive();
  check('S3 去掉让位判断后，「用户上翻不被抢」判定当场变红（还原即回绿）',
        modal.scrollTop !== 300,
        `实得 scrollTop=${modal.scrollTop} —— 用户被强行拉回底部却报绿`);
}

console.log('');
if (failures) {
  console.log(`FAILED: 训练页实况区贴底（${failures} 条）`);
  process.exit(1);
}
console.log('PASS: 训练页实况区贴底（外层 modal + 内层日志 + 让位用户）');
