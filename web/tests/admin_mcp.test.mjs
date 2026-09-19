#!/usr/bin/env node
// 「MCP 连接」面板显示的前端回归防线 —— 零依赖，CI 里直接 node 跑。
//
// 用户原话：「mcp 连接处请优化显示，目前堆在一起不是很好看，可以做的更美观一些」。
//
// 改前的真实现场（1440 宽、卡片行宽 611px，playwright 实测）：
//   25 个工具名当 `<code>` 用空格串成一段 → 那一块 **256px 高**，占整行 344px 的 **74%**；
//   字号 11px、无边框无底色；靠 `word-break:break-all` 硬折行，名字被从词中间切断
//   （`mcp_datato` / `olbox_call_platform_api`）—— 同一行里看不出哪截属于哪个工具。
//
// 本文件守的是「改完之后不许退化」，尤其是三种**看着漂亮但已经坏了**的形态：
//   D1 为了整齐把名字折断/截断 → 少一个工具、或名字拼错，界面上完全看不出来（多给/少给都静默）；
//   D2 折叠了但折叠是假的（`display:flex` 盖掉 `[hidden]` 的默认 display:none）→ 内容还在屏幕上；
//   D3 为了少显示几行，把 25 个砍成「前 N 个 + 其余」→ 少给工具，而用户以为那就是全部。
//
// 做法沿用仓库惯例（见 skill_category_admin_ui.test.mjs）：从出货文件 admin.js 里抽
// **真函数**塞进 node 沙箱跑，不在测试里重写一份实现 —— 抄一份的话，真文件改坏了测试照样绿。

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import vm from 'node:vm';

const here = dirname(fileURLToPath(import.meta.url));
const WEB = join(here, '..');
const src = readFileSync(join(WEB, 'js', 'admin.js'), 'utf8');
// 先剥注释再解析：CSS 注释里会出现 `break-all` 这种**讨论词**（本文件自己就写了
// 「曾经用过 break-all」），不剥的话全文正则会被注释喂饱 —— glassmorphism.test.mjs
// 已经为这个形状付过一次代价。
const CSS = readFileSync(join(WEB, 'css', 'style.css'), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');

let failures = 0;
function check(name, cond, extra = '') {
  if (cond) { console.log(`  ok   ${name}`); return; }
  failures++;
  console.log(`  FAIL ${name}${extra ? ' — ' + extra : ''}`);
}

// ---- 抽取器（花括号配平）----
function sliceBalanced(source, start, braceStart) {
  let depth = 0;
  for (let i = braceStart; i < source.length; i++) {
    const c = source[i];
    if (c === '{') depth++;
    else if (c === '}') { depth--; if (depth === 0) return source.slice(start, i + 1); }
  }
  throw new Error('花括号不配平，无法抽取');
}

function extractFunction(source, name) {
  const sig = `function ${name}(`;
  let start = source.indexOf(sig);
  if (start < 0) throw new Error(`admin.js 里找不到 function ${name}() —— 被删/改名了？`);
  if (source.slice(start - 6, start) === 'async ') start -= 6;
  return sliceBalanced(source, start, source.indexOf('{', source.indexOf(sig)));
}

function extractAssigned(source, name) {
  const sig = `window.${name} = `;
  const start = source.indexOf(sig);
  if (start < 0) throw new Error(`admin.js 里找不到 window.${name} = —— 被删/改名了？`);
  const arrow = source.indexOf('=>', start);
  if (arrow < 0) throw new Error(`window.${name} 不是箭头函数，抽取器需要更新`);
  return sliceBalanced(source, start, source.indexOf('{', arrow));
}

function extractConst(source, name) {
  const start = source.indexOf(`const ${name} = `);
  if (start < 0) throw new Error(`admin.js 里找不到 const ${name} =`);
  // 取到**行尾**而不是第一个 `;`：`esc` 的实现里 `'&': '&amp;'` 自带分号，
  // 按第一个分号切会切出一个语法不完整的片段（沙箱直接 SyntaxError）。
  const eol = source.indexOf('\n', start);
  return source.slice(start, eol < 0 ? source.length : eol);
}

// ---- 沙箱：只放渲染真正需要的东西 ----
function buildSandbox(storeExtra = {}) {
  const store = { ...storeExtra };
  const sandbox = {
    console,
    localStorage: {
      getItem: (k) => (k in store ? store[k] : null),
      setItem: (k, v) => { store[k] = String(v); },
      removeItem: (k) => { delete store[k]; },
    },
    escProxy: null,
  };
  sandbox.window = sandbox;
  vm.createContext(sandbox);
  const prog =
    extractConst(src, 'esc') + '\n' +
    extractConst(src, 'MCP_TOOLS_KEY') + '\n' +
    extractFunction(src, 'mcpToolPrefix') + '\n' +
    extractFunction(src, 'mcpToolsOpen') + '\n' +
    extractFunction(src, 'mcpToolsHTML') + '\n' +
    extractFunction(src, 'mcpStatePill') + '\n' +
    extractFunction(src, 'mcpRowHTML') + '\n' +
    extractAssigned(src, 'mcpToggleTools') + '\n' +
    'globalThis.__api = { mcpToolPrefix, mcpToolsHTML, mcpStatePill, mcpRowHTML };';
  new vm.Script(prog, { filename: 'admin.js(cell)' }).runInContext(sandbox);
  return { api: sandbox.__api, sandbox, store };
}

const REAL_TOOLS = [
  'mcp_datatoolbox_ask_user', 'mcp_datatoolbox_call_api', 'mcp_datatoolbox_call_platform_api',
  'mcp_datatoolbox_create_api', 'mcp_datatoolbox_create_app', 'mcp_datatoolbox_create_app_from_template',
  'mcp_datatoolbox_create_dashboard', 'mcp_datatoolbox_delete_app', 'mcp_datatoolbox_describe_table',
  'mcp_datatoolbox_execute_api', 'mcp_datatoolbox_execute_sql', 'mcp_datatoolbox_get_api_detail',
  'mcp_datatoolbox_get_db_schema', 'mcp_datatoolbox_get_db_sql_hints', 'mcp_datatoolbox_get_platform_detail',
  'mcp_datatoolbox_get_tables', 'mcp_datatoolbox_list_apis', 'mcp_datatoolbox_list_apps',
  'mcp_datatoolbox_list_databases', 'mcp_datatoolbox_list_platform_apis', 'mcp_datatoolbox_list_platforms',
  'mcp_datatoolbox_profile_table', 'mcp_datatoolbox_search_tables', 'mcp_datatoolbox_update_app',
  'mcp_datatoolbox_execute_flow',
];

const SERVER = {
  id: 'datatoolbox', name: '数据工具箱', url: 'http://127.0.0.1:8080/mcp',
  timeout_sec: 60, has_key: true, key_mask: 'dok_…e631', enabled: true,
  status: { ok: true, server: 'data-ontology', version: '1.0.0', tool_count: 25, tools: REAL_TOOLS },
};

const { api } = buildSandbox();

console.log('— 单个条目的结构 —');
const row = api.mcpRowHTML(SERVER);
check('渲染出 .mcp-tools 工具区', row.includes('class="mcp-tools"'));
check('头部写清工具数量', /工具\s*<b>25<\/b>\s*个/.test(row), row.slice(0, 0) || '');
check('头部带上服务端标识与版本',
  row.includes('data-ontology') && row.includes('1.0.0'));
check('服务名/标识/URL/超时/密钥掩码都在', ['数据工具箱', 'datatoolbox', 'http://127.0.0.1:8080/mcp', '超时 60s', 'dok_…e631']
  .every((s) => row.includes(s)));
check('状态做成徽标而不是一整行彩色文字', row.includes('pill-conn ok') && !row.includes('color:#b45309'));

console.log('— D3 工具名不许为了好看被砍 —');
const chips = [...row.matchAll(/<span class="tchip" title="([^"]*)">([^<]*)<\/span>/g)]
  .map((m) => ({ full: m[1], short: m[2] }));
check('chip 数量守恒（25 个工具一个不少）', chips.length === 25, `实际 ${chips.length}`);
check('每个 chip 的 title 都是完整工具名，顺序与后端一致',
  chips.map((c) => c.full).join(',') === REAL_TOOLS.join(','));
check('显示的短名 = 全名去掉公共前缀',
  chips.every((c) => c.full.startsWith('mcp_datatoolbox_') && c.short === c.full.slice('mcp_datatoolbox_'.length)));
check('两个前缀不同的工具不会被混在一起（execute_flow 也在）',
  chips.some((c) => c.short === 'execute_flow'));

console.log('— 公共前缀：只显示一次，且必须切在 _ 上 —');
check('前缀只出现一次（不在每个 chip 里重复）',
  chips.every((c) => !c.short.includes('mcp_datatoolbox_')) && row.includes('mcp-tools-pref'));
check('25 个名字算出的前缀是 mcp_datatoolbox_', api.mcpToolPrefix(REAL_TOOLS) === 'mcp_datatoolbox_');
check('公共前缀没落在下划线时回退成空串（宁可显示全名，也不许切出半个词）',
  api.mcpToolPrefix(['alpha_one', 'alphx_two']) === '');
check('前缀要么为空、要么以 _ 结尾（绝不切在词中间）',
  (() => {
    for (const set of [REAL_TOOLS, ['a_b', 'a_c'], ['x', 'y'], ['p_q_r', 'p_q_s']]) {
      const p = api.mcpToolPrefix(set);
      if (p !== '' && !p.endsWith('_')) return false;
    }
    return true;
  })());
check('拆不出前缀时 chip 显示全名',
  (() => {
    const h = api.mcpToolsHTML({ tools: ['alpha_one', 'beta_two'] });
    return h.includes('>alpha_one<') && h.includes('>beta_two<') && !h.includes('mcp-tools-pref');
  })());
check('只有一个工具时不拆前缀', api.mcpToolPrefix(['only_one']) === '');

console.log('— D2 折叠不许是假的 —');
check('默认收着（body 带 hidden）', /class="mcp-tools-body"\s+hidden/.test(row));
check('CSS 补了 [hidden] 守卫（否则 display:flex 会盖掉默认 display:none）',
  /\.mcp-tools-body\[hidden\]\s*\{\s*display:\s*none/.test(CSS));
const { sandbox: sbOpen } = buildSandbox({ 'skillforge.mcpToolsOpen': '1' });
check('记住展开状态（localStorage=1 时默认展开）',
  !/class="mcp-tools-body"\s+hidden/.test(
    vm.runInContext('mcpRowHTML', sbOpen)(SERVER)));

console.log('— D1 名字不许被折断/截断 —');
check('.mcp-tools-body 不许用 break-all',
  /\.mcp-tools-body\s*\{[^}]*word-break:\s*keep-all/.test(CSS) && !/\.mcp-tools-body\s*\{[^}]*break-all/.test(CSS));
check('.tchip 整词不折行（white-space:nowrap）', /\.tchip\s*\{[^}]*white-space:\s*nowrap/.test(CSS));
check('工具名不再走老那条 dt-wrap + break-all 的路',
  !row.includes('dt-wrap') && !/word-break:\s*break-all/.test(CSS));

console.log('— 转义与三态 —');
const evil = api.mcpRowHTML({ ...SERVER, name: '<img src=x onerror=alert(1)>',
  status: { ok: true, server: 'x', tools: ['mcp_x_<b>boom</b>'] } });
check('服务名里的 HTML 被转义', !evil.includes('<img src=x') && evil.includes('&lt;img'));
check('工具名里的 HTML 被转义', !evil.includes('<b>boom</b>') && evil.includes('&lt;b&gt;boom'));
check('三态徽标：已连接/连接失败/未连接',
  api.mcpStatePill({ ok: true }).includes('pill-conn ok') &&
  api.mcpStatePill({ error: 'dial tcp timeout' }).includes('pill-conn err') &&
  api.mcpStatePill({}).includes('pill-conn idle'));
check('连接失败时错误原文要显示出来（不能只给个红标）',
  api.mcpRowHTML({ ...SERVER, status: { error: 'dial tcp 127.0.0.1:8080: connect' } })
    .includes('dial tcp 127.0.0.1:8080: connect'));
check('没挂上工具时不渲染空工具区', api.mcpToolsHTML({ ok: true, tools: [] }) === '');

console.log('— 状态只留一点点颜色（黑白灰极简） —');
check('.pill-conn 用伪元素小圆点表状态', /\.pill-conn::before\s*\{[^}]*border-radius:\s*50%/.test(CSS));
check('三态靠点的颜色区分（ok/err 各有着色，idle 走默认灰）',
  /\.pill-conn\.ok::before\s*\{[^}]*var\(--ok\)/.test(CSS) &&
  /\.pill-conn\.err::before\s*\{[^}]*#b45309/.test(CSS));

console.log('');
if (failures) { console.log(`${failures} 条不合格`); process.exit(1); }
console.log('全部通过');
