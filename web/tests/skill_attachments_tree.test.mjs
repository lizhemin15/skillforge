#!/usr/bin/env node
// 管理端「下发附件」（kind=attach）的前端回归防线（零依赖，CI 里直接 node 跑）。
//
// 场景（用户原话）：「给 skillforge 一键生成查询 skill，并能够给用户发送拆分出来的
// word 模板，通常是源文件中的附件」—— 训练时从手册/通知的 docx/PDF 里拆出内嵌附件，
// 落到技能目录的 attachments/ 下，运行时随回答下发给用户。
//
// 后端那一半由 internal/store/skill_files_attach_test.go 守。这个文件守前端：
// **后端列了、前端不画**同样是用户可见的「附件没了」——管理员没法核对到底拆出了
// 什么、也没法自己下载一份看看，用户报「说好有模板却打不开」时只能 SSH 翻目录。
//
// 断言分两层：
//   A. 渲染层：抽出货的 renderSkillFiles 真函数，喂假文件列表，验产出的 HTML。
//      - attach 文件要画出来、带下载入口、**不给删除按钮**（删文件不会同步清 DB 里的
//        附件声明，声明还在文件没了 = 用户拿到点开就 404 的卡）；
//      - 分组顺序：attach 紧跟 templatefile，且在 requirement 之前（两个视图必须一致，
//        否则管理员按界面顺序理解「先看什么」会看歪）；
//      - 没附件的技能（绝大多数手册）也要显示空态说明 —— 否则新能力永远不被发现；
//      - 反例：source/ 里的 docx 不能混进附件组（「所有二进制都算附件」就是退化）。
//   B. 白名单层：groups 数组必须含 attach。它同时是「哪些 kind 会渲染」的白名单，
//      漏了它就是后端列了前端不画 —— 而且不会有任何报错，只是静静地少一组。

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import vm from 'node:vm';
import { check, failCount, extractFunction, extractObjectConst } from './_admin_harness.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'js', 'admin.js'), 'utf8');

// ---------- B. 白名单层（先在源码上判，最省事也最直接）----------
console.log('B. groups 白名单');
const groupsLine = /const groups = \[([^\]]*)\]/.exec(src);
check('admin.js 里能找到 groups 白名单字面量', !!groupsLine);
const groups = groupsLine ? groupsLine[1].split(',').map((s) => s.trim().replace(/^'|'$/g, '')) : [];
check('groups 含 attach（不含 = 后端列了前端不画，静默少一组）', groups.includes('attach'),
  `实际：${groups.join(',')}`);
const gi = (k) => groups.indexOf(k);
check('attach 排在 templatefile 之后、requirement 之前（与后端 orderOf 一致）',
  gi('templatefile') >= 0 && gi('templatefile') < gi('attach') && gi('attach') < gi('requirement'),
  `templatefile@${gi('templatefile')} attach@${gi('attach')} requirement@${gi('requirement')}`);

// ---------- A. 渲染层 ----------
console.log('A. renderSkillFiles 渲染');
const KIND_LABEL = extractObjectConst(src, 'KIND_LABEL');
const KIND_ICON = extractObjectConst(src, 'KIND_ICON');
const KIND_EMPTY_HINT = extractObjectConst(src, 'KIND_EMPTY_HINT');
const fns = ['esc', 'escapeJs', 'fileViewMode', 'decSlug', 'urlSlug', 'renderSkillFiles']
  .map((n) => extractFunction(src, n))
  .join('\n');

const sandbox = {
  console,
  encodeURIComponent,
  decodeURIComponent,
  setTimeout: (fn) => 0,
  clearTimeout: () => {},
  token: () => 'TESTTOKEN',
  authHdr: () => ({ Authorization: 'Bearer TESTTOKEN' }),
  toast: () => {},
  skill: null,
  window: {},
};
sandbox.window = sandbox;
sandbox.$ = () => null;

vm.createContext(sandbox);
vm.runInContext(
  `${KIND_LABEL}\n${KIND_ICON}\n${KIND_EMPTY_HINT}\nvar curSkillSlug = '培训通知';\n${fns}\n` +
    `globalThis.__api = { renderSkillFiles, KIND_LABEL, KIND_ICON, KIND_EMPTY_HINT };`,
  sandbox,
);
const api = sandbox.__api;

check('KIND_LABEL 里有 attach（否则分组标题渲染成 undefined）',
  typeof api.KIND_LABEL.attach === 'string' && api.KIND_LABEL.attach.length > 0);
check('KIND_ICON 里有 attach（否则分组标题前面是个空洞）',
  typeof api.KIND_ICON.attach === 'string' && api.KIND_ICON.attach.length > 0);

const withAttach = [
  { path: 'system_prompt.md', name: 'system_prompt.md', kind: 'prompt', size: 100, editable: true },
  { path: '采购合同模板.docx', name: '采购合同模板.docx', kind: 'templatefile', size: 20480, editable: false, binary: true },
  { path: 'attachments/附件2-报名表.docx', name: '附件2-报名表.docx', kind: 'attach', size: 15360, editable: false, binary: true },
  { path: 'requirement.md', name: 'requirement.md', kind: 'requirement', size: 300, editable: true },
  { path: 'source/原稿.docx', name: '原稿.docx', kind: 'source', size: 999, editable: false, binary: true },
];
const body = { innerHTML: '' };
api.renderSkillFiles(body, withAttach);
const html = body.innerHTML;

check('附件分组出现在文件树里', html.includes(api.KIND_LABEL.attach));
check('附件名被渲染出来', html.includes('附件2-报名表.docx'),
  '用户看到的现象就是「拆出来的附件一个都不显示」，这条是核心');
check('附件行带下载入口，且走带鉴权的 fetch 而不是裸 <a href>',
  html.includes('downloadSkillFile(') && html.includes('附件2-报名表.docx') &&
    !/href="[^"]*download=1/.test(html),
  '裸 <a href> 带不上 Bearer 头 → 真浏览器点下载只会 401');
check('附件行**不给**删除按钮（删了文件但 DB 声明还在 → 用户拿到 404 卡）',
  !/附件2-报名表\.docx[\s\S]{0,400}?delSkillFile/.test(html));
check('附件行不给「编辑」按钮（二进制改不了，给了就是骗用户）',
  !/附件2-报名表\.docx[\s\S]{0,300}?编辑/.test(html));

// 分组顺序：attach 紧跟 templatefile，且在 requirement 之前
const iTplFile = html.indexOf('采购合同模板.docx');
const iAttach = html.indexOf('附件2-报名表.docx');
const iReq = html.indexOf('requirement.md');
check('附件分组排在模板文件之后、训练需求之前',
  iTplFile >= 0 && iAttach > iTplFile && iReq > iAttach,
  `templatefile@${iTplFile} attach@${iAttach} requirement@${iReq}`);

// 反例：source/ 下的 docx 不能出现在附件组里
const attachGroupHtml = html.slice(iAttach, iReq > iAttach ? iReq : undefined);
check('source/ 下的 .docx 不出现在附件分组里',
  !attachGroupHtml.includes('原稿.docx'),
  '把「所有二进制都算下发附件」写成就退化了，这条会红');

// 空态：没有附件的技能也要显示说明（否则新能力永远不被发现）
const body2 = { innerHTML: '' };
api.renderSkillFiles(body2, [
  { path: 'system_prompt.md', name: 'system_prompt.md', kind: 'prompt', size: 100, editable: true },
]);
check('技能没有附件时，「下发附件」分组仍显示空态说明',
  body2.innerHTML.includes(api.KIND_LABEL.attach) &&
    body2.innerHTML.includes(api.KIND_EMPTY_HINT.attach.slice(0, 8)),
  '空态说明要讲清「附件从源文档自动拆出来」，否则管理员不知道这组什么时候会有东西');

console.log(failCount() === 0 ? '\n全绿' : `\n${failCount()} 条失败`);
process.exit(failCount() === 0 ? 0 : 1);
