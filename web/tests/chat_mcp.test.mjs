#!/usr/bin/env node
// 「MCP 数据源勾选」前端回归防线 —— 零依赖，CI 里直接 node 跑。
//
// 守的一类坑：**门控失灵，而且失灵的方向是"多给"**。
// 用户原话：「管理员确认开启 mcp 后，用户端在聊天界面使用的时候，上方应当多一个
// mcp 指定的按钮，可以勾选使用管理员已经开启的 mcp，否则默认应当是不调度 mcp 的。
// 勾选以后，你要确认确实能调度 mcp 进行使用。」
//
// 为什么这条比一般的 UI 回归更要紧：漏给用户看得见（他会骂），多给看不见
// （模型的工具表里多挂了一台内网业务系统的 MCP，谁都不会发现）。
// 所以下面每一条断言都朝「关闭」倒：
//
//   Bug M1 — 不勾也带上数据源：请求体里 mcp 不是空数组（或压根没这个字段，
//            后端只能靠猜）。默认态必须是"一个 MCP 都不挂"。
//   Bug M2 — 勾选集直接照发：管理员停用/删掉那台 MCP 之后，用户浏览器里
//            还留着旧 id。照发 → 后端闸门被打开（"用户选中了 MCP"），
//            但工具表一台都挂不上 → 模型空转到轮数上限 → 编一个答案给用户。
//            必须发之前跟服务端当前清单求交集。
//   Bug M3 — 没得选还常驻按钮：管理员一台都没开，界面上多一个按钮，
//            点开是空面板 —— 用户会以为"这里坏了"。
//   Bug M4 — 弹出层关不掉：CSS 给 .ch-mcp-layer 写了 display:flex，
//            会盖掉浏览器默认的 [hidden]{display:none}，缺了守卫那条，
//            层设了 hidden 还在屏幕上（技能层踩过同一个坑）。
//
// 做法：**从出货文件里抠出真正跑的函数来跑**（extractFn 花括号配平），
// 不在测试里抄一份实现 —— 抄一份的话，改坏了真文件测试照样绿。

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const WEB = join(here, '..');
const CHAT_JS = readFileSync(join(WEB, 'js', 'chat.js'), 'utf8');
const INDEX_HTML = readFileSync(join(WEB, 'index.html'), 'utf8');
const STYLE_CSS = readFileSync(join(WEB, 'css', 'style.css'), 'utf8');

let checks = 0, failures = 0;
function check(name, cond, extra = '') {
  checks++;
  if (cond) { console.log(`  ok   ${name}`); return; }
  failures++;
  console.log(`  FAIL ${name}${extra ? ' — ' + extra : ''}`);
}

// ---------- 从出货文件里抽函数 ----------
// 花括号配平抽取具名函数（含函数体），然后把它体里引用的闭包变量当形参注进去。
function extractFn(src, signature) {
  const start = src.indexOf(signature);
  if (start < 0) return null;
  let depth = 0, seen = false;
  for (let k = src.indexOf('{', start); k < src.length; k++) {
    if (src[k] === '{') { depth++; seen = true; }
    else if (src[k] === '}') { depth--; if (seen && depth === 0) return src.slice(start, k + 1); }
  }
  return null;
}

// bind 返回的是**业务函数本身**（new Function 造的是一个返回它的工厂，要再调一次）。
function bind(src, signature, deps = [], values = []) {
  const fn = extractFn(src, signature);
  if (!fn) throw new Error('抽不到函数：' + signature);
  return new Function(...deps, 'return ' + fn)(...(values || []));
}

// ---------- 尺子自检：抽不到函数必须红，不能静默空跑 ----------
// 本文件所有断言都建立在这几个函数能从出货文件里抠出来之上。签名被改名/被删，
// extractFn 返回 null，如果只让 bind 抛异常，node 会带着堆栈退出 ——
// rc!=0 但没有 FAIL 行，那是崩溃红，证明不了任何断言有效（自证脚本里判它不合格）。
// 所以先在抽签阶段把它变成一条 FAIL 行。
const SIGS = [
  'function readMCPPick(storage)',
  'function writeMCPPick(storage, ids)',
  'function mcpPayloadIds(picked, servers)',
  'function mcpBtnText(n)',
  'function toggleMCP(id)',
  'function renderMCP()',
  'function chatPayload(text, mcpIds)',
];
const missing = SIGS.filter((s) => !extractFn(CHAT_JS, s));
missing.forEach((s) => check(`出货文件里能抠到 ${s}`, false, '抽不到 = 下面相关断言全在空跑'));
if (missing.length) {
  console.log(`\n${checks} 项断言`);
  console.log(`${failures} 项失败（函数抠不出来，后面的断言不评）`);
  process.exit(1);
}

// ---------- 极简假 DOM ----------
// 只实现 renderMCP / closeMCPLayer 真正用到的那几个面：
// hidden / className / textContent / children / innerHTML='' / appendChild /
// classList.toggle / setAttribute / contains。
// 手搓而不是引 jsdom：这个项目的前端测试一贯零依赖，CI 上不装 node_modules。
function makeEl(tag) {
  const n = {
    tagName: (tag || 'div').toUpperCase(),
    className: '', textContent: '', hidden: false,
    children: [], attrs: {}, _cls: new Set(),
    get classList() {
      const self = this;
      return {
        toggle(c, on) { if (on) self._cls.add(c); else self._cls.delete(c); },
        add(c) { self._cls.add(c); },
        remove(c) { self._cls.delete(c); },
        contains(c) { return self._cls.has(c); },
      };
    },
    set innerHTML(v) { if (v === '') this.children = []; },
    get innerHTML() { return ''; },
    appendChild(c) { this.children.push(c); return c; },
    setAttribute(k, v) { this.attrs[k] = String(v); },
    getAttribute(k) { return this.attrs[k]; },
    contains(t) { return t === this || this.children.indexOf(t) >= 0; },
    addEventListener() {},
  };
  return n;
}
// el(tag, cls, text) —— 与出货文件同签名
function fakeEl(tag, cls, text) {
  const n = makeEl(tag);
  if (cls) n.className = cls;
  if (text !== undefined && text !== null) n.textContent = text;
  return n;
}

// 假 localStorage：只实现 getItem/setItem
function fakeStorage(init) {
  const m = Object.assign({}, init || {});
  return {
    getItem: (k) => (k in m ? m[k] : null),
    setItem: (k, v) => { m[k] = String(v); },
    removeItem: (k) => { delete m[k]; },
    _raw: m,
  };
}

// ---------- 取出真函数 ----------
const readMCPPick = bind(CHAT_JS, 'function readMCPPick(storage)', ['MCP_KEY'], ['skillforge.mcp']);
const mcpPayloadIds = bind(CHAT_JS, 'function mcpPayloadIds(picked, servers)');
const mcpBtnText = bind(CHAT_JS, 'function mcpBtnText(n)');

console.log('MCP 数据源勾选 · 前端门控（改坏了必须红）');

// ============================================================
// 1. DOM 存在 + 默认不现
// ============================================================
const wantIds = ['mcp-box', 'mcp-btn', 'mcp-label', 'mcp-layer', 'mcp-list', 'mcp-foot'];
wantIds.forEach((id) => {
  check(`index.html 有 #${id}`, new RegExp(`id="${id}"`).test(INDEX_HTML));
});
// 默认态必须是隐藏的：HTML 里带了 hidden 才算"默认不调度"这件事在界面上成立
const boxTag = (INDEX_HTML.match(/<div class="ch-mcp" id="mcp-box"[^>]*>/) || [''])[0];
check('index.html #mcp-box 默认带 hidden（没勾/没得选时按钮不出现）', /\bhidden\b/.test(boxTag), boxTag);
// 层也必须默认 hidden，否则一进页面就挂着一个面板
const layerTag = (INDEX_HTML.match(/<div class="ch-mcp-layer" id="mcp-layer"[^>]*>/) || [''])[0];
check('index.html #mcp-layer 默认带 hidden', /\bhidden\b/.test(layerTag), layerTag);
// 按钮挂在 .ch-mrow（输入框正上方那一行）里，不在推荐行
// —— 推荐行每次 renderChips() 清空 innerHTML，放那儿按钮会一闪一闪消失
const mrow = (INDEX_HTML.match(/<div class="ch-mrow">[\s\S]*?<\/div>\s*<textarea/) || [''])[0];
check('MCP 按钮在 .ch-mrow 内（不在会被 renderChips 清空的推荐行）', /id="mcp-box"/.test(mrow));

// ============================================================
// 2. CSS：弹出层那条 [hidden] 守卫不能省
// ============================================================
check('style.css 给 .ch-mcp-layer 写了 display:flex（所以守卫是必需的）',
  /\.ch-mcp-layer\s*\{[^}]*display:\s*flex/.test(STYLE_CSS));
check('style.css 有 .ch-mcp-layer[hidden] { display: none; } 守卫',
  /\.ch-mcp-layer\[hidden\]\s*\{\s*display:\s*none/.test(STYLE_CSS));
check('style.css 有 .ch-mcp[hidden] { display: none; }（按钮同理）',
  /\.ch-mcp\[hidden\]\s*\{\s*display:\s*none/.test(STYLE_CSS));

// ============================================================
// 3. 请求体：不勾 = 空数组（Bug M1）
// ============================================================
// 把 chatPayload 抠出来跑。它引用 sessionId / chatMode / pickedSkill 三个闭包变量。
function payloadOf(mcpIds) {
  const fn = bind(CHAT_JS, 'function chatPayload(text, mcpIds)', ['sessionId', 'chatMode', 'pickedSkill'], ['sess1', 'auto', null]);
  return JSON.parse(fn('写个新闻稿', mcpIds));
}
const pNone = payloadOf([]);
check('不勾 → 请求体 mcp 是空数组', Array.isArray(pNone.mcp) && pNone.mcp.length === 0, JSON.stringify(pNone.mcp));
const pOne = payloadOf(['crm']);
check('勾一台 → 请求体 mcp 只有那一台', JSON.stringify(pOne.mcp) === '["crm"]', JSON.stringify(pOne.mcp));
// 忘了传（undefined）必须落到"不调度"那一侧，不能是 undefined/null/省略字段：
// 后端拿到的字段类型一变，就得靠猜 —— 猜错的方向就是"多给"。
const pForget = payloadOf(undefined);
check('调用方忘传 → 兜底为空数组（不是 undefined / 不是省略字段）',
  Array.isArray(pForget.mcp) && pForget.mcp.length === 0, JSON.stringify(pForget.mcp));
check('请求体里 mcp 字段始终存在（后端不需要区分"没勾"和"老前端"）', 'mcp' in pForget);

// ============================================================
// 4. 发送前跟服务端清单求交集（Bug M2）
// ============================================================
const servers = [{ id: 'crm', name: 'CRM' }, { id: 'erp', name: 'ERP' }];
check('勾选集 ∩ 清单：在架的保留', JSON.stringify(mcpPayloadIds(['crm'], servers)) === '["crm"]');
check('勾选集 ∩ 清单：已下架的剔掉',
  JSON.stringify(mcpPayloadIds(['crm', 'gone'], servers)) === '["crm"]');
check('清单全下架 → 退回完全不调度（宁可少给）',
  JSON.stringify(mcpPayloadIds(['gone'], servers)) === '[]');
check('空清单 → 空数组', JSON.stringify(mcpPayloadIds(['crm'], [])) === '[]');
check('清单里有垃圾项（null / 无 id）不炸',
  JSON.stringify(mcpPayloadIds(['crm'], [null, {}, { id: 'crm' }, undefined])) === '["crm"]');
check('多选保持顺序', JSON.stringify(mcpPayloadIds(['erp', 'crm'], servers)) === '["erp","crm"]');

// ============================================================
// 5. localStorage：脏数据必须读成"不勾"（失败落关闭侧）
// ============================================================
const badCases = [
  ['没有这个 key', {}],
  ['不是 JSON', { 'skillforge.mcp': '[' }],
  ['JSON 但不是数组', { 'skillforge.mcp': '{"a":1}' }],
  ['数组里混了非字符串', { 'skillforge.mcp': '[1,null,{},"crm"]' }],
];
badCases.forEach(([label, init]) => {
  const got = readMCPPick(fakeStorage(init));
  const ok = Array.isArray(got) && got.every((x) => typeof x === 'string' && x);
  const expectCRM = label === '数组里混了非字符串';
  check(`localStorage ${label} → ${expectCRM ? '只留合法 id' : '读成空'}`,
    ok && (expectCRM ? JSON.stringify(got) === '["crm"]' : got.length === 0), JSON.stringify(got));
});
check('localStorage 正常值原样读出',
  JSON.stringify(readMCPPick(fakeStorage({ 'skillforge.mcp': '["crm","erp"]' }))) === '["crm","erp"]');

// ============================================================
// 6. 勾选/取消真的写回 storage
// ============================================================
{
  const st = fakeStorage({});
  const t = bind(CHAT_JS, 'function toggleMCP(id)',
    ['readMCPPick', 'writeMCPPick', 'renderMCP', 'window'],
    [readMCPPick, (s, ids) => s.setItem('skillforge.mcp', JSON.stringify(ids)), () => {}, { localStorage: st }]);
  t('crm');
  check('勾一台 → 写进 localStorage', st.getItem('skillforge.mcp') === '["crm"]', st.getItem('skillforge.mcp'));
  t('erp');
  check('再勾一台 → 两台都在', st.getItem('skillforge.mcp') === '["crm","erp"]', st.getItem('skillforge.mcp'));
  t('crm');
  check('再点一次 → 取消勾选', st.getItem('skillforge.mcp') === '["erp"]', st.getItem('skillforge.mcp'));
}

// ============================================================
// 7. 按钮文案：未勾时不带数字
// ============================================================
check('未勾 → 文案不带数字（"MCP 数据源"）', mcpBtnText(0) === 'MCP 数据源', mcpBtnText(0));
check('勾 2 台 → 文案带台数', mcpBtnText(2) === 'MCP 数据源 · 2', mcpBtnText(2));

// ============================================================
// 8. renderMCP：没得选就不露按钮（Bug M3）+ 勾选态落到 DOM 上
// ============================================================
function runRenderMCP(serversIn, pickedIn, initialHidden = true) {
  const dom = {
    mcpBox: makeEl('div'), mcpBtn: makeEl('button'),
    mcpLabel: makeEl('span'), mcpList: makeEl('div'), mcpFoot: makeEl('div'),
  };
  dom.mcpBox.hidden = initialHidden;               // true = HTML 默认态
  const spy = { closed: 0 };
  const storage = fakeStorage(pickedIn ? { 'skillforge.mcp': JSON.stringify(pickedIn) } : {});
  const currentMCPIds = () => mcpPayloadIds(readMCPPick(storage), serversIn);
  const fn = bind(CHAT_JS, 'function renderMCP()',
    ['mcpBox', 'mcpBtn', 'mcpLabel', 'mcpList', 'mcpFoot', 'mcpServers', 'el',
      'currentMCPIds', 'mcpBtnText', 'closeMCPLayer'],
    [dom.mcpBox, dom.mcpBtn, dom.mcpLabel, dom.mcpList, dom.mcpFoot, serversIn,
      fakeEl, currentMCPIds, mcpBtnText, () => { spy.closed++; }]);
  fn();
  return Object.assign(dom, { spy });
}

{
  const d = runRenderMCP([], []);
  check('清单空（管理员没开任何 MCP）→ 按钮不出现', d.mcpBox.hidden === true);
}
{
  // ★ 关键一条：**从"按钮正显示着"起跑**。
  // 场景就是用户会遇到的：他开着页面，管理员在后台把这台 MCP 停用了（或唯一一台断了线）。
  // 下一次 loadMCPServers 回来后 renderMCP 必须把按钮收回去。
  // 注意不能只测"从 hidden 起跑还保持 hidden" —— 那种测法骑在测试自己写死的初始值上，
  // 把守卫整条删掉它照样绿（本文件的自证脚本第 5 条就是这么把我抓出来的）。
  const d = runRenderMCP([], [], false);
  check('清单空了但按钮正显示着 → 必须收回去（管理员停用后不残留）', d.mcpBox.hidden === true);
  check('清单空了 → 弹出层也一并关掉', d.spy.closed > 0, String(d.spy.closed));
}
{
  const d = runRenderMCP(servers, []);
  check('有可选项、但没勾 → 按钮出现', d.mcpBox.hidden === false);
  check('有可选项、但没勾 → 按钮不是"已生效"态', d.mcpBtn.classList.contains('is-on') === false);
  check('有可选项、但没勾 → 面板列出 2 台', d.mcpList.children.length === 2, String(d.mcpList.children.length));
  check('有可选项、但没勾 → 脚注说清"这轮完全不调用 MCP"', /完全不调用/.test(d.mcpFoot.textContent), d.mcpFoot.textContent);
}
{
  const d = runRenderMCP(servers, ['crm']);
  check('勾了 1 台 → 按钮切成"已生效"态', d.mcpBtn.classList.contains('is-on') === true);
  check('勾了 1 台 → 按钮文案带台数', d.mcpLabel.textContent === 'MCP 数据源 · 1', d.mcpLabel.textContent);
  check('勾了 1 台 → 那一行标成选中', d.mcpList.children.filter((r) => /is-on/.test(r.className)).length === 1);
  check('勾了 1 台 → aria-pressed 如实反映', d.mcpList.children.filter((r) => r.attrs['aria-pressed'] === 'true').length === 1);
  check('勾了 1 台 → 脚注变成"这轮会带上它们的工具"', /会带上/.test(d.mcpFoot.textContent), d.mcpFoot.textContent);
}
{
  // storage 里留着已下架的 id：界面必须按"没勾"渲染（否则用户以为这轮带了数据源）
  const d = runRenderMCP(servers, ['gone']);
  check('勾选集里的 id 已下架 → 界面按未勾渲染（不假装已生效）', d.mcpBtn.classList.contains('is-on') === false);
}

// ============================================================
// 9. 接线：initModes 必须真的调起来（游离代码 = 按钮永远是死的）
// ============================================================
check('initModes 里调了 wireMCP()', /wireMCP\(\);/.test(CHAT_JS));
check('initModes 里调了 loadMCPServers()', /loadMCPServers\(\);/.test(CHAT_JS));
check('stream() 送出请求体时带上了勾选集', /chatPayload\(text,\s*currentMCPIds\(\)\)/.test(CHAT_JS));
check('清单接口是 /api/mcp/servers', /fetch\('\/api\/mcp\/servers'\)/.test(CHAT_JS));
// 点层外关掉必须走捕获阶段（理由同技能层：层内点了会重渲染，冒泡时白名单失效）
check('点层外关掉走捕获阶段（第三参 true）',
  /document\.addEventListener\('click',[\s\S]{0,600}?\},\s*true\);/.test(CHAT_JS));

console.log(`\n${checks} 项断言`);
if (failures) { console.log(`${failures} 项失败`); process.exit(1); }
console.log('全部通过');
