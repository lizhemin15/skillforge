#!/usr/bin/env node
// 管理端「只读文件」契约的前端回归防线（零依赖，CI 里直接 node 跑）。
//
// 真实 bug（用户视角）：技能知识库里点开 style_profile.md，界面给出可编辑框
// 和「保存」按钮 —— 因为 editSkillFile() 完全无视列表/详情接口返回的
// editable:false。用户认真改完点保存，后端 store.WriteFile 硬拒 400
// 「该文件只读，不可编辑」，改动全丢，界面看着像坏了。
//
// 这个 bug 后端测试抓不住（后端本来就对），只能在前端守。做法沿用仓库既有惯例
// （见 deploy/offline/test-fontref.sh）：**从出货文件里把真函数抽出来测**，
// 而不是在测试里另写一份实现 —— 否则测试和实现各说各话，漂移了还绿。
//
// 两层断言：
//   A. 行为层：抽出 fileViewMode / readOnlyNotice 的源码，eval 后逐个用例验。
//   B. 接线层：确认 editSkillFile 真的**用**了它，且只读分支里没有任何保存入口。
//      （函数写对了但没人调用，是最容易出现的假绿。）

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

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

// 抽出 `function <name>(...) { ... }` 的完整源码（花括号配平，能扛嵌套）。
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
    else if (c === '}') {
      depth--;
      if (depth === 0) return source.slice(start, i + 1);
    }
  }
  throw new Error(`function ${name} 的花括号不配平，无法抽取`);
}

// 抽出 `if (<cond>) { ... }` 这个块的源码（同样配平），用来检查只读分支内部。
function extractIfBlock(source, cond) {
  const start = source.indexOf(cond);
  if (start < 0) throw new Error(`在 admin.js 里找不到分支 ${cond} —— 只读接线被删了？`);
  const braceStart = source.indexOf('{', start);
  let depth = 0;
  for (let i = braceStart; i < source.length; i++) {
    const c = source[i];
    if (c === '{') depth++;
    else if (c === '}') {
      depth--;
      if (depth === 0) return source.slice(braceStart + 1, i);
    }
  }
  throw new Error(`分支 ${cond} 的花括号不配平`);
}

// ---------- A. 行为层 ----------
const fnSrc = [extractFunction(src, 'fileViewMode'), extractFunction(src, 'readOnlyNotice')].join('\n');
const sandbox = {};
new Function('exports', `${fnSrc}\n` +
  'exports.fileViewMode = fileViewMode; exports.readOnlyNotice = readOnlyNotice;')(sandbox);
const { fileViewMode, readOnlyNotice } = sandbox;

// 真实 payload 形态（来自 internal/store/skill_files.go 的 fileKind() + ListFiles）。
const styleAnchor = { path: 'style_profile.md', kind: 'style', name: 'style_profile.md', size: 68, editable: false, binary: false };
const systemPrompt = { path: 'system_prompt.md', kind: 'prompt', editable: true, binary: false };
const excelSource = { path: 'source/报价单.xlsx', kind: 'source', editable: false, binary: true };
const exampleMd = { path: 'examples/example01.md', kind: 'example', editable: true, binary: false };

console.log('A. fileViewMode 行为');
check('style_profile.md → readonly', fileViewMode(styleAnchor) === 'readonly', `实际 ${fileViewMode(styleAnchor)}`);
check('system_prompt.md → edit', fileViewMode(systemPrompt) === 'edit', `实际 ${fileViewMode(systemPrompt)}`);
check('examples/*.md → edit', fileViewMode(exampleMd) === 'edit', `实际 ${fileViewMode(exampleMd)}`);
check('二进制素材优先走 preview（不进只读视图）', fileViewMode(excelSource) === 'preview', `实际 ${fileViewMode(excelSource)}`);
check('payload 缺失时不锁死（→ edit）', fileViewMode(undefined) === 'edit', `实际 ${fileViewMode(undefined)}`);

console.log('B. readOnlyNotice 说清原因');
check('风格锚点给出「风格锚点」解释', readOnlyNotice(styleAnchor).includes('风格锚点'), readOnlyNotice(styleAnchor));
check('其它只读文件给出通用解释', readOnlyNotice({ kind: 'other', editable: false }).includes('只读'));
check('无 payload 也不炸', typeof readOnlyNotice(undefined) === 'string');

// ---------- C. 接线层 ----------
console.log('C. editSkillFile 真的接线了');
check('用 fileViewMode 决策（而非继续看 j.binary）', src.includes('const mode = fileViewMode(j);'));
const previewBlock = src.includes('if (mode === \'preview\') {');
check('二进制分支改由 mode 判定', previewBlock);

const roBlock = extractIfBlock(src, "if (mode === 'readonly') {");
check('只读分支渲染了只读标记', roBlock.includes('只读'));
check('只读分支给了原因文案', roBlock.includes('readOnlyNotice(j)'));
check('只读分支挂的是只读编辑器', roBlock.includes('{ readOnly: true }'));
check('只读分支内部没有任何保存入口', !/saveSkillFile/.test(roBlock), '只读分支里出现了 saveSkillFile');
// 注意别用裸「保存」二字：分支里的注释（「Ctrl-S 不接任何保存回调」）也含这两个字，
// 会把注释误判成按钮 —— 这里锚定真实的按钮标记。
check('只读分支不渲染「保存」按钮', !/>保存</.test(roBlock), '只读分支里出现了「保存」按钮');

console.log('D. 树上也能看出只读');
check('文件树用 fileViewMode 打只读标', src.includes("fileViewMode(f) === 'readonly'"));
check('保存函数有只读兜底', /if \(cmReadOnly\)/.test(src), 'saveSkillFile 缺少只读兜底');

// ---------- E. 自证：抽取器真的能抓住破坏 ----------
console.log('E. 抽取器自证（防假绿）');
check('抽出的 fileViewMode 是真源码', fnSrc.includes('f.editable === false'));
check('抽出的只读分支非空', roBlock.length > 100, `长度 ${roBlock.length}`);
let threw = false;
try { extractFunction(src, 'noSuchFunction__selfcheck'); } catch (e) { threw = true; }
check('抽不到函数时抛错而非静默通过', threw);

console.log(failures === 0
  ? '\n全绿：只读文件不会渲染出保存入口。'
  : `\n${failures} 项失败。`);
process.exit(failures === 0 ? 0 : 1);
