#!/usr/bin/env node
// 管理端「模板文件」契约的前端回归防线（零依赖，CI 里直接 node 跑）。
//
// 真实 bug（用户视角）：「skill 需要返回 word 或者 excel，模板的源文件不应当在技能里吗？
// 我看目前的 skill 里面都没有」—— 模板 .docx/.xlsx 就躺在技能目录顶层，Agent 的
// fill_template 也真在读它，但管理端文件树只列白名单 md，模板一个都看不见，
// 换模板只能 SSH 进服务器。
//
// 后端已把顶层 .docx/.xlsx 列进清单（kind=templatefile，见 internal/store 的
// skill_files_template_test.go）。这个文件守前端那一半，因为「后端列了、前端不画」
// 完全是同一种用户可见结果：界面上还是看不到模板。
//
// 断言分两层（沿用仓库惯例，见 deploy/offline/test-fontref.sh）：
//   A. 渲染层：抽出货的 renderSkillFiles 真函数，喂假文件列表，验产出的 HTML。
//   B. 接线层：抽出货的 doUploadTemplate 真执行，验它真的把 target=template 发给了后端 ——
//      少了这个字段，上传会落到 source/，「上传成功」但模板分组依然为空。
//
// 注意别写成恒真空断言（`/docx/.test(html)` 这种）：下面每条断言都配了**反例**——
// 例如「source/ 里的 .docx 必须归在素材组、不能出现在模板组」，如果实现退化成
// 「所有 .docx 都算模板」，反例会红。

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import vm from 'node:vm';

const here = dirname(fileURLToPath(import.meta.url));
const ADMIN_JS = join(here, '..', 'js', 'admin.js');
const src = readFileSync(ADMIN_JS, 'utf8');

let failures = 0;
function check(name, cond, extra = '') {
  if (cond) {
    console.log(`  ok   ${name}`);
  } else {
    failures++;
    console.log(`  FAIL ${name}${extra ? ' — ' + extra : ''}`);
  }
}

// 抽出真函数的源码。admin.js 里有三种写法，都得认（只认一种就会「找不到函数」而误判成删了）：
//   function foo(...) {...}                      —— 函数声明
//   const foo = (...) => ...;                    —— 箭头（表达式体）
//   window.foo = async (...) => {...}            —— 挂到 window 上的箭头（函数体）
function extractFunction(source, name) {
  const patterns = [`function ${name}(`, `const ${name} = `, `window.${name} = `];
  const hit = patterns.map((p) => source.indexOf(p)).filter((i) => i >= 0).sort((a, b) => a - b)[0];
  if (hit === undefined) throw new Error(`在 admin.js 里找不到 ${name} —— 被删/改名了？`);
  const declStart = hit;
  const arrowEnd = source.indexOf('=>', declStart);
  const braceStart = source.indexOf('{', declStart);
  // 表达式体箭头 = `=>` 之后（跳过空白）不是 `{`。
  // 别用「=> 出现在 { 之前」当判据：`async (x) => {` 也满足它，
  // 结果只抽到函数头那半行（本测试自己踩过第二个坑）。
  let afterArrow = arrowEnd < 0 ? '' : source.slice(arrowEnd + 2).replace(/^\s+/, '')[0];
  const isExpressionArrow = arrowEnd >= 0 && afterArrow !== '{' && arrowEnd < (braceStart < 0 ? Infinity : braceStart);
  if (isExpressionArrow) {
    // 取到本行行尾。不能用「第一个分号」——`&amp;` / `&#39;` 这类 HTML 实体内就带分号，
    // 会把函数从中间切断（本测试踩过的第一个坑）。
    const lineEnd = source.indexOf('\n', arrowEnd);
    const line = source.slice(declStart, lineEnd < 0 ? source.length : lineEnd).trimEnd();
    return line.endsWith(';') ? line : line + ';';
  }
  let depth = 0;
  for (let i = braceStart; i < source.length; i++) {
    if (source[i] === '{') depth++;
    else if (source[i] === '}') {
      depth--;
      if (depth === 0) return source.slice(declStart, i + 1);
    }
  }
  throw new Error(`${name} 花括号不配平`);
}

// 抽出 `const <name> = { ... }`（对象字面量，配平花括号）。
function extractObjectConst(source, name) {
  const start = source.indexOf(`const ${name} = {`);
  if (start < 0) throw new Error(`在 admin.js 里找不到 const ${name} = { —— 被删/改名了？`);
  let depth = 0;
  for (let i = source.indexOf('{', start); i < source.length; i++) {
    if (source[i] === '{') depth++;
    else if (source[i] === '}') {
      depth--;
      if (depth === 0) return source.slice(start, i + 1) + ';';
    }
  }
  throw new Error(`const ${name} 花括号不配平`);
}

// ---- 组装沙箱：只喂出货文件里的真源码，测试里不另写实现 ----
const KIND_LABEL = extractObjectConst(src, 'KIND_LABEL');
const KIND_ICON = extractObjectConst(src, 'KIND_ICON');
const KIND_EMPTY_HINT = extractObjectConst(src, 'KIND_EMPTY_HINT');
const fns = ['esc', 'escapeJs', 'fileViewMode', 'decSlug', 'urlSlug', 'renderSkillFiles', 'doUploadTemplate', 'downloadSkillFile']
  .map((n) => extractFunction(src, n))
  .join('\n');

const uploadCalls = [];
const fetchCalls = [];
const toasts = [];
const savedFiles = [];
const sandbox = {
  console,
  encodeURIComponent,
  decodeURIComponent,
  // setTimeout 也得给：沙箱里没它，被测函数走到「延迟回收 blob URL」就 ReferenceError ——
  // 报错点看着像产品 bug，实为沙箱缺全局（vm 不会继承 Node 的全局对象）
  setTimeout: (fn) => 0,
  clearTimeout: () => {},
  FileReader: class {},
  // 记录型桩：断言「桩收到了什么」，而不是只断言「没报错」
  FormData: class {
    constructor() { this.entries = []; }
    append(k, v) { this.entries.push([k, v]); }
  },
  fetch: async (url, opts) => {
    fetchCalls.push({ url, opts, fd: opts && opts.body });
    if (sandbox.__nextStatus && sandbox.__nextStatus !== 200) {
      return {
        ok: false, status: sandbox.__nextStatus,
        json: async () => ({ error: '未登录' }),
      };
    }
    return {
      ok: true, status: 200,
      headers: { get: (h) => (h.toLowerCase() === 'content-disposition'
        ? "attachment; filename=\"______.docx\"; filename*=UTF-8''" + encodeURIComponent('采购合同模板.docx')
        : null) },
      blob: async () => ({ size: 39810, type: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document' }),
      json: async () => ({ ok: true, path: '采购合同模板.docx' }),
    };
  },
  toast: (m, k) => toasts.push([m, k]),
  closeDetailEditor: () => { sandbox.__closed = true; },
  loadSkillFiles: () => { sandbox.__reloaded = true; },
  token: () => 'TESTTOKEN',
  authHdr: () => ({ 'Authorization': 'Bearer TESTTOKEN' }),
  URL: { createObjectURL: (b) => 'blob:fake', revokeObjectURL: () => {} },
  document: {
    getElementById: () => null,
    createElement: () => {
      const el = { download: '', href: '', click() { savedFiles.push({ name: el.download, href: el.href }); } , remove() {} };
      return el;
    },
    body: { appendChild: () => {} },
  },
  window: {},
};
sandbox.window = sandbox;
sandbox.$ = (id) => sandbox.__els[id];
sandbox.__els = { 'tpl-input': { files: [{ name: '采购合同模板.docx', size: 123 }] } };

vm.createContext(sandbox);
vm.runInContext(
  `${KIND_LABEL}\n${KIND_ICON}\n${KIND_EMPTY_HINT}\nvar curSkillSlug = '采购合同';\n${fns}\n` +
    `globalThis.__api = { renderSkillFiles, doUploadTemplate, downloadSkillFile, KIND_LABEL, KIND_EMPTY_HINT };`,
  sandbox,
);
const api = sandbox.__api;

// ---------- A. 渲染层 ----------
console.log('A. renderSkillFiles 渲染');

const withTpl = [
  { path: 'system_prompt.md', name: 'system_prompt.md', kind: 'prompt', size: 100, editable: true },
  { path: 'template.md', name: 'template.md', kind: 'template', size: 200, editable: true },
  { path: '采购合同模板.docx', name: '采购合同模板.docx', kind: 'templatefile', size: 20480, editable: false, binary: true },
  { path: 'source/参考件.docx', name: '参考件.docx', kind: 'source', size: 999, editable: false, binary: true },
];
const body = { innerHTML: '' };
api.renderSkillFiles(body, withTpl);
const html = body.innerHTML;

check('模板分组出现在文件树里', html.includes('模板文件'));
check('模板文件名被渲染出来', html.includes('采购合同模板.docx'),
  '用户看到的现象就是「模板一个都不显示」，这条是核心');
check('模板行带下载入口，且走带鉴权的 fetch 而不是裸 <a href>',
  html.includes(`window.downloadSkillFile('${encodeURIComponent('采购合同')}','采购合同模板.docx')`) &&
  !/href="[^"]*download=1/.test(html),
  '裸 <a href> 带不上 Bearer 头 → 真浏览器点下载只会 401（本轮实测踩到）');
check('模板行带删除入口',
  html.includes(`window.delSkillFile('${encodeURIComponent('采购合同')}','采购合同模板.docx','templatefile')`),
  '模板是用户资产，界面上要能删回「无模板」状态');
check('模板行不给「编辑」按钮（二进制改不了，给了就是骗用户）',
  !/采购合同模板\.docx[\s\S]{0,300}?编辑/.test(html));

// 分组顺序：模板文件必须紧跟在「写作模板」后面（用户找模板时两处要挨着）
const iTemplate = html.indexOf('写作模板');
const iTemplateFile = html.indexOf('模板文件');
const iSource = html.indexOf('原始素材');
check('「模板文件」分组排在「写作模板」之后、其它组之前',
  iTemplate >= 0 && iTemplateFile > iTemplate && iTemplateFile < iSource,
  `写作模板@${iTemplate} 模板文件@${iTemplateFile} 原始素材@${iSource}`);

// 反例：source/ 里的 .docx 不能冒充模板（两套视图打架的退化形态）
const tplGroupHtml = html.slice(iTemplateFile, iSource > iTemplateFile ? iSource : undefined);
check('source/ 下的 .docx 不出现在模板分组里',
  !tplGroupHtml.includes('参考件.docx'),
  '把「所有 .docx 都算模板」写成就退化了，这条会红');

// 空分组也要显示（否则入口藏起来 = 用户再次找不到模板）
const body2 = { innerHTML: '' };
api.renderSkillFiles(body2, [
  { path: 'system_prompt.md', name: 'system_prompt.md', kind: 'prompt', size: 100, editable: true },
]);
check('技能没有模板时，「模板文件」分组仍然显示空态提示',
  body2.innerHTML.includes('模板文件') && body2.innerHTML.includes('上传模板'),
  '空态里要带上传入口，否则用户不知道去哪加模板');
check('kind 标签表里有 templatefile（免得出现 undefined 分组标题）',
  typeof api.KIND_LABEL.templatefile === 'string' && api.KIND_LABEL.templatefile.length > 0);

// ---------- B. 接线层 ----------
console.log('B. doUploadTemplate 真的把模板传给后端');

await api.doUploadTemplate(encodeURIComponent('采购合同')); // UI 的 onclick 就是这么传的（已编码）
const call = fetchCalls[0] || {};
const entries = call.fd ? call.fd.entries : [];
const getFd = (k) => (entries.find((e) => e[0] === k) || [])[1];

check('上传打到技能文件接口（POST）',
  (call.url || '').includes('/api/admin/skills/' + encodeURIComponent('采购合同') + '/file') &&
  call.opts && call.opts.method === 'POST',
  JSON.stringify(call.url));
check('带了鉴权头（否则 401，模板永远传不上去）',
  !!(call.opts && call.opts.headers && String(call.opts.headers.Authorization || '').startsWith('Bearer ')));
check('带上了文件本体', getFd('file') && getFd('file').name === '采购合同模板.docx');
check('带上了 target=template（少了它会落到 source/，模板分组照样空）',
  getFd('target') === 'template',
  '这是本次修复的关键接线');
check('slug 只编码一次（二次编码 → 服务端拿到字面量 %E9…，一律 404）',
  !/%25/i.test(call.url || '') &&
  (call.url || '').includes('/api/admin/skills/' + encodeURIComponent('采购合同') + '/file'),
  call.url);
check('上传成功后提示 + 刷新文件树',
  sandbox.__reloaded === true && toasts.some(([m, k]) => k === 'ok'));

// ---------- C. 下载 ----------
console.log('C. downloadSkillFile 真能下下来');

await api.downloadSkillFile(encodeURIComponent('采购合同'), '采购合同模板.docx'); // 同 UI 形态
const dlCall = fetchCalls[fetchCalls.length - 1] || {};
const dlAuth = (dlCall.opts && dlCall.opts.headers && dlCall.opts.headers.Authorization) || '';
check('下载请求带上了 Authorization（少了它 = 401，界面有 ⬇ 但永远下不来）',
  dlAuth.startsWith('Bearer '), JSON.stringify(dlAuth));
check('下载打到 download=1 的文件接口，且 slug 只编码一次（否则 404）',
  /download=1/.test(dlCall.url || '') && !/%25/i.test(dlCall.url || '') &&
  (dlCall.url || '').includes('/api/admin/skills/' + encodeURIComponent('采购合同') + '/file'),
  dlCall.url);
// 二次编码是最容易复发的形态（%25 = 编码过的 '%'），单独立一条，红了能一眼看出原因
check('下载 URL 里没有二次编码痕迹（%25）', !/%25/i.test(dlCall.url || ''), dlCall.url);
const saved = savedFiles[savedFiles.length - 1] || {};
check('真的触发了保存，文件名取的是服务端下发的 RFC 5987 中文名',
  saved.name === '采购合同模板.docx', JSON.stringify(saved.name));

// 401 时不能静默：要给出错误提示（用户看到「点了没反应」是最糟的）
sandbox.__nextStatus = 401;
const before = toasts.length;
await api.downloadSkillFile(encodeURIComponent('采购合同'), '采购合同模板.docx');
check('401 时有错误提示而不是静默失败',
  toasts.length > before && toasts[toasts.length - 1][1] === 'err',
  JSON.stringify(toasts.slice(before)));
sandbox.__nextStatus = 200;

if (failures) {
  console.log(`\n${failures} 项失败`);
  process.exit(1);
}
console.log('\n全部通过');
