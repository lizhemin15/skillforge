#!/usr/bin/env node
// 技能知识库左树的回归防线：左树「编辑」按钮必须是活的。
//
// 真实 bug（用户视角，本轮报障）：「左侧分类各不一样，用户也不能编辑」——
// 树上明明有「编辑」按钮，点下去什么都不发生。用户以为文件不可编辑/界面坏了。
//
// 机制：行上有 onclick=window.openEditorFile(...)，而按钮自己的 onclick 只有
// `event.stopPropagation()` —— 冒泡被掐死后按钮又不做任何事，点击被整个吃掉，
// 行上的入口永远不触发。**光看后端权限是对的**（editable 一路传到前端了），
// 所以这个 bug 后端测试抓不住，只能在这里守。
//
// 做法沿用仓库既有惯例（见 readonly_file_editor.test.mjs、deploy/offline/*.sh）：
// 从出货文件 admin.js 里抽真函数来跑，而不是在测试里重写一份实现。
//
// 断言分两层：
//   A. 行为层：带 editable:true 的文件，「编辑」按钮必须能打开编辑器。
//   B. 反例层：只读文件不许出现「编辑」按钮；分组顺序与后端 orderOf 一致。

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

function extractFunction(source, name) {
  const sig = `function ${name}(`;
  const start = source.indexOf(sig);
  if (start < 0) {
    throw new Error(`在 admin.js 里找不到 function ${name}( —— 函数被删/改名了？` +
      `测试必须测出货文件里的真函数，请同步更新本测试。`);
  }
  const braceStart = source.indexOf('{', start);
  let depth = 0;
  for (let i = braceStart; i < source.length; i++) {
    const c = source[i];
    if (c === '{') depth++;
    else if (c === '}') { depth--; if (depth === 0) return source.slice(start, i + 1); }
  }
  throw new Error(`function ${name} 的花括号不配平，无法抽取`);
}

// 抽 `const x = ...;` 形式的单行常量（esc 是箭头函数，不是 function 声明）
function extractConstLine(source, name) {
  const start = source.indexOf(`const ${name} = `);
  if (start < 0) throw new Error(`在 admin.js 里找不到 const ${name} =`);
  const head = source.slice(start, source.indexOf('\n', start));
  if (head.trimEnd().endsWith(';')) return head;
  return source.slice(start, source.indexOf(';', start) + 1);
}

function extractObjectConst(source, name) {
  const start = source.indexOf(`const ${name} = {`);
  if (start < 0) throw new Error(`在 admin.js 里找不到 const ${name}`);
  const braceStart = source.indexOf('{', start);
  let depth = 0;
  for (let i = braceStart; i < source.length; i++) {
    const c = source[i];
    if (c === '{') depth++;
    else if (c === '}') { depth--; if (depth === 0) return source.slice(start, i + 1); }
  }
  throw new Error(`const ${name} 的花括号不配平`);
}

const fns = [extractConstLine(src, 'esc'), ...['escapeJs', 'fileViewMode', 'renderSkillFiles']
  .map((n) => extractFunction(src, n))].join('\n');

const sandbox = { console, document: { getElementById: () => null }, window: {} };
sandbox.window = sandbox;
sandbox.$ = () => null;
vm.createContext(sandbox);
vm.runInContext(
  `${extractObjectConst(src, 'KIND_LABEL')}\n` +
  `${extractObjectConst(src, 'KIND_ICON')}\n` +
  `${extractObjectConst(src, 'KIND_EMPTY_HINT')}\n` +
  `var curSkillSlug = '采购合同';\n${fns}\n` +
  `globalThis.__api = { renderSkillFiles, KIND_LABEL };`,
  sandbox,
);
const api = sandbox.__api;

const render = (files) => {
  const body = { innerHTML: '' };
  api.renderSkillFiles(body, files);
  return body.innerHTML;
};

// ---------- A. 「编辑」按钮必须是活的 ----------
console.log('A. 左树「编辑」按钮的可点击性');

const html = render([
  { path: 'system_prompt.md', name: 'system_prompt.md', kind: 'prompt', size: 100, editable: true },
  { path: 'template.md', name: 'template.md', kind: 'template', size: 110, editable: true },
  { path: 'requirement.md', name: 'requirement.md', kind: 'requirement', size: 120, editable: true },
  { path: 'categories/01-经营业绩.md', name: '01-经营业绩.md', kind: 'category', size: 200, editable: true },
  { path: 'examples/经营业绩/01.md', name: '01.md', kind: 'example', size: 300, editable: true },
  { path: 'source/参考件.pdf', name: '参考件.pdf', kind: 'source', size: 400, editable: false, binary: true },
  { path: 'style_profile.md', name: 'style_profile.md', kind: 'style', size: 500, editable: false },
]);

const editAttrs = [...html.matchAll(/<button class="link-btn"[^>]*>编辑<\/button>/g)]
  .map((m) => (m[0].match(/onclick="([^"]*)"/) || [])[1] || '');

check('带 editable:true 的文件渲染出了「编辑」按钮', editAttrs.length === 5,
  `期望 5 个（prompt + template + requirement + category + example），实际 ${editAttrs.length} 个`);

check('★「编辑」按钮真的能打开编辑器（不是只 stopPropagation 的死按钮）',
  editAttrs.length > 0 && editAttrs.every((a) => /openEditorFile/.test(a)),
  `按钮 onclick 实际是 ${JSON.stringify(editAttrs)} —— 只掐冒泡、自己不做事，` +
  '点击被吃掉，行上的 openEditorFile 永远不触发。这是用户报「不能编辑」的直接原因');

check('「编辑」按钮打开的是**自己那一行**的文件（不是行索引串了）',
  editAttrs.some((a) => a.includes(encodeURIComponent('采购合同')) && a.includes('categories/01-')) &&
  editAttrs.every((a) => a.includes(encodeURIComponent('采购合同'))));

check('分类要求也能编辑（categories/*.md 是人工维护的写作口径，不该只读）',
  editAttrs.some((a) => a.includes("categories/01-")),
  '分类要求若不给出编辑入口，用户就只能 SSH 改要求');

// ---------- B. 反例层 ----------
console.log('B. 反例：只读文件与分组契约');

check('只读文件（style_profile.md）不给「编辑」按钮',
  !/style_profile\.md[\s\S]{0,300}?编辑/.test(html),
  '只读文件给编辑按钮＝承诺一个点下去只会报错的操作（见 readonly_file_editor.test.mjs）');

check('只读文件在树上带「只读」标记（别等点进去才发现改不了）',
  /style_profile\.md[\s\S]{0,300}?只读/.test(html));

check('范文按类别子目录（examples/<类别>/NN.md）也能完整渲染出来',
  html.includes('01.md') && html.includes('examples/经营业绩/01.md'),
  '早先只认一级目录，子目录范文在管理端一个都看不见');

// 分组顺序：必须与后端 store.orderOf 一致，否则接口和界面两处顺序打架
const order = ['核心提示词', '写作模板', '训练需求', '分类要求', '风格画像', '参考范文', '原始素材'];
const pos = order.map((label) => html.indexOf(label));
check('分组顺序：核心提示词 → 模板 → 需求/分类 → 风格 → 范文/素材',
  pos.every((p, i) => p >= 0 && (i === 0 || p > pos[i - 1])),
  `实际位置 ${JSON.stringify(order.map((l, i) => `${l}@${pos[i]}`))}`);

check('分类要求为空时也显示分组 + 说明（用户会以为功能没上线）',
  render([{ path: 'system_prompt.md', name: 'system_prompt.md', kind: 'prompt', size: 1, editable: true }])
    .includes('分类要求'));

console.log(failures ? `\n${failures} 条断言红了` : '\n全绿');
process.exit(failures ? 1 : 0);
