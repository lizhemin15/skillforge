#!/usr/bin/env node
// 分类结构管理（新增 / 改名 / 删除）的前端回归防线。
//
// 背景（用户报障）：「skill 编辑界面左侧的分类各不一样，用户也不能编辑，是什么逻辑」——
// 两件事混在一起：
//   ① 分类**天生每个技能不一样**（它是训练期从手册抽出来的章节骨架，一个手册 12 类、
//      另一个 5 类），这本身是对的，错在界面上从不说明、也没有任何入口能改；
//   ② 分类只能靠 SSH 改磁盘文件，界面上一个字都动不了。
//
// 所以本轮把「新增分类」挂到分类分组标题、「改名/删除」挂到分类行，全部走后端级联接口。
// 本测试守的就是这两个入口的**存在性、归属和语义**：入口摆错地方（比如非手册技能也摆）
// 或摆成死按钮，就是又一次「用户看不懂、也点不动」。
//
// 做法沿用仓库既有惯例（见 skill_file_tree_edit_button.test.mjs）：
// 从出货文件 admin.js 里抽**真函数**来跑，不在测试里重写一份实现。

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import vm from 'node:vm';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'js', 'admin.js'), 'utf8');

let failures = 0;
function check(name, cond, extra = '') {
  if (cond) console.log(`  ok   ${name}`);
  else { failures++; console.log(`  FAIL ${name}${extra ? ' — ' + extra : ''}`); }
}

// 抽取 `function name(...) {...}`（花括号配平）。
// 注意 `async function name()` —— indexOf('function name(') 会命中 async 后面那一段，
// 抽出来的片段丢了 async，塞进沙箱跑就是「await is only valid in async functions」。
// 所以命中后要回头把 'async ' 一起带上。
function extractFunction(source, name) {
  const sig = `function ${name}(`;
  let start = source.indexOf(sig);
  if (start < 0) throw new Error(`admin.js 里找不到 function ${name}() —— 被删/改名了？`);
  if (source.slice(start - 6, start) === 'async ') start -= 6;
  return sliceBalanced(source, start, source.indexOf('{', source.indexOf('function ' + name)));
}

// 抽取 `window.name = (args) => {...}` / `window.name = async (args) => {...}`
function extractAssigned(source, name) {
  const sig = `window.${name} = `;
  const start = source.indexOf(sig);
  if (start < 0) throw new Error(`admin.js 里找不到 window.${name} = —— 被删/改名了？`);
  const arrow = source.indexOf('=>', start);
  if (arrow < 0) throw new Error(`window.${name} 不是箭头函数，本测试的抽取器需要更新`);
  const braceStart = source.indexOf('{', arrow);
  return sliceBalanced(source, start, braceStart);
}

function sliceBalanced(source, start, braceStart) {
  let depth = 0;
  for (let i = braceStart; i < source.length; i++) {
    const c = source[i];
    if (c === '{') depth++;
    else if (c === '}') { depth--; if (depth === 0) return source.slice(start, i + 1); }
  }
  throw new Error('花括号不配平，无法抽取');
}

function extractConstLine(source, name) {
  const start = source.indexOf(`const ${name} = `);
  if (start < 0) throw new Error(`admin.js 里找不到 const ${name} =`);
  const head = source.slice(start, source.indexOf('\n', start));
  if (head.trimEnd().endsWith(';')) return head;
  return source.slice(start, source.indexOf(';', start) + 1);
}

function extractObjectConst(source, name) {
  const start = source.indexOf(`const ${name} = {`);
  if (start < 0) throw new Error(`admin.js 里找不到 const ${name}`);
  return sliceBalanced(source, start, source.indexOf('{', start));
}

// ---------- 沙箱：把出货文件里的真函数放进一个假 DOM ----------
const FNS = [
  ...['escapeJs', 'fileViewMode', 'renderSkillFiles'].map((n) => extractFunction(src, n)),
  ...['newCategoryView', 'doNewCategory', 'renameCategoryView', 'doRenameCategory',
    'delCategory', 'showCategoryChange'].map((n) => extractAssigned(src, n)),
];
const HELPERS = ['catErrMsg', 'afterCategoryChange'].map((n) => extractFunction(src, n));

function boot({ manualMode = true, members = [] } = {}) {
  const els = {};        // id -> 假元素
  const calls = [];      // fetch 调用记录
  const confirms = [];   // confirm 调用记录 + 返回值队列
  const toasts = [];
  const answers = [...members];   // confirm 的返回值队列
  const json = () => JSON.stringify(answers.length ? answers.shift() : {});

  const el = (id) => (els[id] = els[id] || { value: '', innerHTML: '', style: {}, dataset: {},
    classList: { add() {}, remove() {} }, textContent: '' });
  const responses = [];
  let respIdx = 0;

  const sandbox = {
    console,
    window: {},
    document: { getElementById: (id) => el(id) },
    $: (id) => el(id),
    encodeURIComponent,
    JSON,
    confirm: (m) => { confirms.push(m); return confirmQueue.length ? confirmQueue.shift() : true; },
    alert: () => {},
    fetch: async (url, opt) => {
      calls.push({ url, opt: opt || {} });
      const spec = responses[respIdx++] || { ok: true, status: 200, body: {} };
      return { ok: spec.ok, status: spec.status || (spec.ok ? 200 : 400),
        json: async () => spec.body };
    },
    toast: (t, k) => toasts.push({ t, k }),
    authHdr: () => ({ 'Content-Type': 'application/json' }),
    urlSlug: (s) => String(s).replace(/\//g, '%2F'),
    loadSkillFiles: async () => {},   // 重载左树（本测试只关心回执渲染）
    closeDetailEditor: () => {},
    destroyCm: () => {},
    setTimeout,
  };
  const confirmQueue = [];
  sandbox.window = sandbox;
  vm.createContext(sandbox);
  const prog =
    `${extractConstLine(src, 'esc')}\n` +
    `${extractObjectConst(src, 'KIND_LABEL')}\n` +
    `${extractObjectConst(src, 'KIND_ICON')}\n` +
    `${extractObjectConst(src, 'KIND_EMPTY_HINT')}\n` +
    `var curSkillSlug = '采购合同';\nvar curEditPath = null;\n` +
    `${FNS.join('\n')}\n${HELPERS.join('\n')}\n` +
    `globalThis.__api = { renderSkillFiles, newCategoryView, doNewCategory, renameCategoryView,` +
    ` doRenameCategory, delCategory, showCategoryChange };`;
  const script = new vm.Script(prog, { filename: 'admin.js(cell)' });
  script.runInContext(sandbox);
  return { api: sandbox.__api, els, el, calls, confirms, toasts, responses,
    pushConfirm(v) { confirmQueue.push(v); }, get confirmQueue() { return confirmQueue; } };
}

const CAT_ROWS = [
  { path: 'categories/01-新闻通稿.md', name: '01-新闻通稿.md', kind: 'category', size: 200, editable: true, category_name: '新闻通稿' },
  { path: 'categories/02-公司新闻通稿.md', name: '02-公司新闻通稿.md', kind: 'category', size: 210, editable: true, category_name: '公司新闻通稿' },
  { path: 'categories/03-领导讲话.md', name: '03-领导讲话.md', kind: 'category', size: 220, editable: true, category_name: '领导讲话稿' },
];
const INDEX_ROW = { path: 'categories/_index.md', name: '_index.md', kind: 'category', size: 90, editable: true };

// box = boot() 的返回值（不是 box.api —— 传错就是 box.api.api === undefined）
function render(box, files, manualMode) {
  const body = { innerHTML: '' };
  box.api.renderSkillFiles(body, files, manualMode);
  return body.innerHTML;
}

// ---------- A. 「+ 新增分类」入口 ----------
console.log('A. 「+ 新增分类」入口的门禁');

const manualHtml = render(boot(), CAT_ROWS, true);
check('手册模式技能：分类分组标题上有「+ 新增分类」',
  /新增分类[\s\S]*?onclick="window\.newCategoryView\(\)"/.test(manualHtml) ||
  /onclick="window\.newCategoryView\(\)"[^>]*>\+ 新增分类/.test(manualHtml),
  '没有这个入口，手册里没写的类别就永远加不进来（只能 SSH 改磁盘）');

const nonManualHtml = render(boot(), [{ path: 'system_prompt.md', name: 'system_prompt.md', kind: 'prompt', size: 1, editable: true }], false);
check('非手册技能：不出现「+ 新增分类」',
  !nonManualHtml.includes('newCategoryView'),
  '非手册技能没有 categories/ 目录，后端 create 只会回 400「不是手册模式」——' +
  '摆出这个按钮＝承诺一个必然失败的操作');

// 最需要这个按钮的时刻：categories/ 目录还在（后端 manual_mode=true），但树上一行分类
// 都没有了 —— 连路由表 _index.md 都没了。这在上传来的技能里是真会发生的（用户手工删过、
// 或某次删除把 _index.md 一并带走），目录还在，所以后端仍然算手册模式。
// 若前端靠「有没有分类行」猜手册模式，这里就会把入口藏掉，用户再也加不回分类。
// 注意 fixture 里**不能**留 _index.md：留了的话 kind==='category' 的行数就不是 0，
// 「靠行数猜」和「看 manual_mode」在数据上恰好同解，这条断言就抓不住那个故障了。
const emptiedHtml = render(boot(), [{ path: 'system_prompt.md', name: 'system_prompt.md', kind: 'prompt', size: 1, editable: true }], true);
check('★ 分类被删空后「+ 新增分类」仍在（判据是后端的 manual_mode，不是前端的行数）',
  emptiedHtml.includes('newCategoryView'),
  '靠数分类行来推断手册模式 → 删空后入口消失，用户再也加不回分类');

// ---------- B. 分类行的「改名 / 删除」归属性 ----------
console.log('B. 分类行的「改名 / 删除」');

const treeHtml = render(boot(), [...CAT_ROWS, INDEX_ROW], true);
const renameBtns = [...treeHtml.matchAll(/onclick="event\.stopPropagation\(\);window\.renameCategoryView\(([^)]*)\)"/g)].map((m) => m[1]);
const delBtns = [...treeHtml.matchAll(/onclick="event\.stopPropagation\(\);window\.delCategory\(([^)]*)\)"/g)].map((m) => m[1]);

check('三个真分类各有一个「改名」入口', renameBtns.length === 3, `实际 ${renameBtns.length}`);
check('三个真分类各有一个「删除」入口', delBtns.length === 3, `实际 ${delBtns.length}`);
check('★ categories/_index.md（路由表）没有改名/删除入口',
  !/renameCategoryView\([^)]*_index\.md/.test(treeHtml) && !/delCategory\([^)]*_index\.md/.test(treeHtml),
  '_index.md 是分类路由表、不是分类；后端也不许删。给了入口＝点下去只会报错');
check('★ 改名/删除按钮都带**自己那一行**的分类名（不是行索引串了）',
  renameBtns.some((a) => a.includes('领导讲话稿')) &&
  renameBtns.some((a) => a.includes("categories/03-领导讲话.md")) &&
  delBtns.some((a) => a.includes('新闻通稿')) &&
  delBtns.every((a) => a.includes(encodeURIComponent('采购合同'))),
  `改名参数 ${JSON.stringify(renameBtns)} / 删除参数 ${JSON.stringify(delBtns)}`);
check('★ 删除按钮真的调 delCategory（不是只掐冒泡的死按钮）',
  delBtns.length === 3 && delBtns.every((a) => a.trim().length > 0),
  '只 stopPropagation 不做事 = 点击被吃掉（见 skill_file_tree_edit_button.test.mjs 的真实事故）');
check('分类在树上标出展示名（H1 与文件名可能不一致）',
  treeHtml.includes('领导讲话稿'),
  '03-领导讲话.md 的 H1 是「领导讲话稿」，运行时按 H1 路由；界面上不标出来，' +
  '用户会以为「文件名和名字对不上」是 bug');

// ---------- C. 新增分类：请求契约 ----------
console.log('C. 新增分类的请求契约');

let box = boot();
box.api.newCategoryView();
check('新增分类表单给出分类名 / 触发词 / 写作要求三个输入',
  ['nc-name', 'nc-trigger', 'nc-req'].every((id) => box.el('kb-editor').innerHTML.includes(id)),
  '抽取阶段只有名字能填的话，新分类的写作要求就只能事后手改 md');
box.el('nc-name').value = ' 会议纪要 ';
box.el('nc-trigger').value = '纪要,会议记录';
box.el('nc-req').value = '按议题分节';
await box.api.doNewCategory(encodeURIComponent('采购合同'));   // eslint-disable-line
const c = box.calls[0];
check('新增分类 → POST /api/admin/skills/<slug>/categories',
  c && c.url === '/api/admin/skills/' + encodeURIComponent('采购合同') + '/categories' && c.opt.method === 'POST',
  `实际 ${c && c.url} ${c && c.opt.method}`);
check('新增分类请求体是 {name, trigger, requirement}（名字已 trim）',
  c && JSON.parse(c.opt.body).name === '会议纪要' && JSON.parse(c.opt.body).trigger === '纪要,会议记录',
  `实际 ${c && c.opt.body}`);

// ---------- D. 改名：请求契约 ----------
console.log('D. 改名的请求契约');

box = boot();
box.api.renameCategoryView('categories/03-领导讲话.md', '领导讲话稿');
check('改名表单预填当前分类名，且带新分类名输入框',
  box.el('kb-editor').innerHTML.includes('rc-name') && box.el('kb-editor').innerHTML.includes('领导讲话稿'));
box.el('rc-name').value = '主要领导讲话';
await box.api.doRenameCategory(encodeURIComponent('采购合同'), 'categories/03-领导讲话.md');   // eslint-disable-line
const rc = box.calls[0];
check('改名 → POST …/categories/rename，体为 {file, new_name}',
  rc && /\/categories\/rename$/.test(rc.url) &&
  JSON.parse(rc.opt.body).file === 'categories/03-领导讲话.md' &&
  JSON.parse(rc.opt.body).new_name === '主要领导讲话',
  `实际 ${rc && rc.url} ${rc && rc.opt.body}`);

// ---------- E. 删除：两段确认 ----------
console.log('E. 删除的两段确认');

// E1. 空分类：一次确认就够（后端 200）
box = boot();
box.responses.push({ ok: true, status: 200, body: { ok: true, change: { action: 'delete', name: '会议纪要', example_count: 0 } } });
await box.api.delCategory(encodeURIComponent('采购合同'), 'categories/09-会议纪要.md', '会议纪要');   // eslint-disable-line
check('空分类：只问一次，直接 DELETE（不带 force）',
  box.confirms.length === 1 && box.calls.length === 1 && !/force=1/.test(box.calls[0].url),
  `confirm ${box.confirms.length} 次 / 请求 ${box.calls.map((x) => x.url)}`);

// E2. 有范文：后端回 need_force → 必须再问一次，且把**真实篇数**写进确认文案
box = boot();
box.responses.push({ ok: false, status: 400, body: { error: '该分类下还有范文', need_force: true, example_count: 7 } });
box.responses.push({ ok: true, status: 200, body: { ok: true, change: { action: 'delete', name: '新闻通稿', example_count: 7, files_deleted: ['categories/01-新闻通稿.md'] } } });
box.pushConfirm(true);
await box.api.delCategory(encodeURIComponent('采购合同'), 'categories/01-新闻通稿.md', '新闻通稿');   // eslint-disable-line
check('★ 有范文时问第二次，且第二次的请求带 force=1',
  box.confirms.length === 2 && box.calls.length === 2 &&
  !/force=1/.test(box.calls[0].url) && /force=1/.test(box.calls[1].url),
  `confirm ${box.confirms.length} 次 / 请求 ${box.calls.map((x) => x.url)}`);
check('★ 第二次确认文案里写着范文篇数（7 篇），不是含糊的「该分类非空」',
  /\b7\b/.test(box.confirms[1] || ''),
  `实际文案 ${JSON.stringify(box.confirms[1])} —— 删的是手册原文，用户得知道要删几篇`);
check('二段确认靠 need_force 这个机器可读标记，不匹配错误文案',
  !/该分类下还有范文/.test(String(box.api.delCategory)), true);

// E3. 用户在第一段就取消 → 一个请求都不许发
box = boot();
box.pushConfirm(false);
await box.api.delCategory(encodeURIComponent('采购合同'), 'categories/01-新闻通稿.md', '新闻通稿');   // eslint-disable-line
check('用户取消 → 不发任何删除请求',
  box.calls.length === 0 && box.toasts.length === 0,
  `实际请求 ${box.calls.length} 个 —— 取消还能删成功是最严重的一类 bug`);

// E4. 用户在第二段（有范文那一步）取消 → 不许带 force 重试
box = boot();
box.responses.push({ ok: false, status: 400, body: { error: '该分类下还有范文', need_force: true, example_count: 3 } });
box.pushConfirm(true); box.pushConfirm(false);
await box.api.delCategory(encodeURIComponent('采购合同'), 'categories/01-新闻通稿.md', '新闻通稿');   // eslint-disable-line
check('★ 第二段确认取消 → 不带 force 重试（范文一篇都不能被顺手删掉）',
  box.calls.length === 1 && !/force=1/.test(box.calls[0].url),
  `实际请求 ${box.calls.map((x) => x.url)}`);

// ---------- F. 回执：改了哪些文件必须摊开 ----------
console.log('F. 变更回执');

box = boot();
box.api.showCategoryChange('分类已改名', {
  action: 'rename', name: '主要领导讲话', old_name: '领导讲话稿', example_count: 4,
  files_touched: ['system_prompt.md', 'categories/03-主要领导讲话.md', 'reviewer.md'],
  warnings: ['路由表里没有该分类的行，请检查'],
});
const receipt = box.el('kb-panel').innerHTML;
check('回执列出被改写的文件（不是只说一句「已保存」）',
  ['system_prompt.md', 'reviewer.md', 'categories/03-主要领导讲话.md'].every((f) => receipt.includes(f)),
  '改了 5 个文件却只提示「已保存」，出问题用户只能猜');
check('回执显示新旧名字与范文篇数', receipt.includes('领导讲话稿') && receipt.includes('主要领导讲话') && receipt.includes('4'));
check('★ warnings（本该在却没在）用红字单列，不混在正常回执里',
  /msg err[\s\S]{0,200}?路由表里没有该分类的行/.test(receipt),
  '数据不一致静默滑过，用户永远发现不了路由表缺行');

console.log(failures ? `\n${failures} 条断言红了` : '\n全绿');
process.exit(failures ? 1 : 0);
