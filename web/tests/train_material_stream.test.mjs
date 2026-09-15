#!/usr/bin/env node
// 训练页「中间材料」实况块 —— 行为测试（零依赖，CI 里直接 node 跑）。
//
// 守的是一类**静默劣化**：材料是有了，但写成了「一片一个 DOM 节点」。
// 二十分钟训练、每秒几十个片段，DOM 里会攒到几万个节点，页面越来越卡，
// 最后用户看到的仍然是「卡着」。静态看代码（`case 'delta'` 存在、脚本体面）
// 全都正常，review 抓不到；只有真把帧喂进去数节点才抓得到。
//
// 做法与 web/tests/chat_composer.test.mjs 同源：从**出货文件**里抠出真正在跑的
// 那段 case 代码（花括号配平），配一个极简 DOM 替身把它跑起来 —— 不在测试里
// 抄一份实现。抄的那份永远不会红。

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const ROOT = join(here, '..', '..');
const read = (p) => readFileSync(join(ROOT, p), 'utf8');

let failures = 0;
function check(name, cond, extra = '') {
  if (cond) console.log(`  ok   ${name}`);
  else { failures++; console.log(`  FAIL ${name}${extra ? ' — ' + extra : ''}`); }
}

// 从 switch 里抠一个 case 的整块（含花括号），做花括号配平。
function extractCase(js, name) {
  const at = js.indexOf(`case '${name}'`);
  if (at < 0) return null;
  const open = js.indexOf('{', at);
  if (open < 0) return null;
  let depth = 0;
  for (let i = open; i < js.length; i++) {
    if (js[i] === '{') depth++;
    else if (js[i] === '}') {
      depth--;
      if (depth === 0) return js.slice(at, i + 1);
    }
  }
  return null;
}

// 极简 DOM：只需要支持「按 id 查得到」「建节点计数」「appendChild/remove」。
// 建节点计数是关键判据 —— 「一片一节点」必然把它推到帧数量级。
function makeDom() {
  const byId = new Map();
  let created = 0;
  const mk = (tag) => {
    const el = {
      tagName: tag, id: '', className: '', dataset: {},
      style: { cssText: '' }, textContent: '',
      children: [], scrollTop: 0, scrollHeight: 0,
      appendChild(c) { this.children.push(c); if (c.id) byId.set(c.id, c); return c; },
      remove() { if (this.id) byId.delete(this.id); },
    };
    return el;
  };
  const log = mk('div');
  log.id = 'tr-log';
  byId.set('tr-log', log);
  const document = { createElement: (t) => { created++; return mk(t); } };
  const $ = (id) => byId.get(id) || null;
  return { document, $, log, created: () => created, byId };
}

const adminJS = read('web/js/admin.js');
const block = extractCase(adminJS, 'delta');
const adminGo = read('internal/api/admin.go');

// ---- ① 抠得出真代码 ----
check('抠出 admin.js 里真正在跑的 case \'delta\' 分支', !!block, '抠不到说明前端没接 delta 帧，或写法变了导致配平失败');

// ---- ② 喂 500 帧，数节点 ----
if (block) {
  const dom = makeDom();
  const run = new Function('ev', '$', 'document', `let lastEvAt = 0; switch (ev.type) { ${block} } return lastEvAt;`);

  const FRAMES = 500;
  let lastAt = 0;
  let threw = null;
  for (let i = 0; i < FRAMES; i++) {
    try {
      lastAt = run(
        { type: 'delta', data: JSON.stringify({ kind: i % 2 ? 'think' : 'text', text: '片段' + i }) },
        dom.$, dom.document,
      );
    } catch (e) { threw = e; break; }
  }
  check(`${FRAMES} 帧材料喂进去不抛异常`, !threw, threw ? String(threw && threw.message) : '');
  check('实况块只建 1 个 DOM 节点（不是一片一个节点）',
    dom.created() === 1, `一共建了 ${dom.created()} 个节点 —— 一片一节点会随帧数线性增长，二十分钟下来把页面拖死`);
  check('实况块在日志容器里（不是飘在页面别处）', dom.log.children.length === 1 && dom.log.children[0].id === 'tr-material');

  const mp = dom.byId.get('tr-material');
  check('正文字符有上限，不无限增长', !!mp && mp.textContent.length <= 700, mp ? `当前 ${mp.textContent.length} 字` : '块丢了');
  check('留住的是最近的片段（尾部），不是开头的', !!mp && mp.textContent.includes('片段' + (FRAMES - 1)), mp ? mp.textContent.slice(0, 40) : '');
  check('换类别时插了分隔符（思考/正文不连成一句）', !!mp && mp.textContent.includes('\n'));
  check('收到材料就算「有进度」（刷新 lastEvAt，心跳不会误报卡死）', lastAt > 0);
}

// ---- ③ 后端确实把材料发出去了，并且攒批 ----
check('后端发 delta 帧', /send\("delta"/.test(adminGo));
check('后端用攒批器（不是一片一帧）', /NewMaterialRelay\(/.test(adminGo));
check('训练 ctx 上挂了材料接收器', /skillgen\.WithDelta\(/.test(adminGo));
check('阶段边界先 Flush 再发 step（材料尾巴不串到下个阶段）',
  /relay\.Flush\(\)\s*\n\s*send\("step"/.test(adminGo));
check('训练结束再 Flush 一次（最后一阶段尾巴不丢）',
  /\}\)\s*\n\s*relay\.Flush\(\)/.test(adminGo));

if (failures) { console.log(`\n${failures} 项失败`); process.exit(1); }
console.log('\n全部通过');
