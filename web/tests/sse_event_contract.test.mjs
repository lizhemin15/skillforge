#!/usr/bin/env node
// SSE 事件名契约测试 —— 零依赖，CI 里直接 node 跑。
//
// 守的是一类**静默故障**：后端发的事件名，前端 switch 里没有对应的 case，
// 于是那些帧被无声丢弃；页面上不报错、控制台不报错，只有用户看到「什么都没有」。
//
//   实例一（训练页）：internal/api/admin.go 发 `status` / `step`，
//     而 web/js/admin.js 只 `case 'stage'` —— 「开始训练」和九个阶段的进度帧
//     全部被吞掉。二十分钟里屏幕上只有一个空日志框 + 不动的「训练中…」。
//     用户投诉原话：「一直卡着计时，用户体验不佳」。这类 bug 靠肉眼 review
//     基本抓不到（两个文件各看各的都很正常），必须机器盯。
//
// 做法：静态解析。事件名两边都从**出货文件本身**抽出来，不在测试里抄清单——
// 抄清单的话，改了真文件测试照样绿，等于没守。
//   - 后端：`send("xxx"` 字面量调用 + `const evXxx = "xxx"` 常量 + `Type: "xxx"` 结构体字面量
//   - 前端：`case 'xxx'` / `case "xxx"`

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const ROOT = join(here, '..', '..');

let failures = 0;
function check(name, cond, extra = '') {
  if (cond) console.log(`  ok   ${name}`);
  else { failures++; console.log(`  FAIL ${name}${extra ? ' — ' + extra : ''}`); }
}

const read = (p) => readFileSync(join(ROOT, p), 'utf8');

// ---- 后端侧：抽事件名 ----
function backendEvents(goSrc) {
  const names = new Set();
  // send("status", ...) / write("delta", ...) / emit("x", ...)
  for (const m of goSrc.matchAll(/\b(?:send|write|emit|writeEvent)\("([a-z_]+)"/g)) names.add(m[1]);
  // const evDelta = "delta"
  for (const m of goSrc.matchAll(/const\s+ev[A-Za-z]+\s*=\s*"([a-z_]+)"/g)) names.add(m[1]);
  // model.StreamEvent{Type: "delta", ...}
  for (const m of goSrc.matchAll(/Type:\s*"([a-z_]+)"/g)) names.add(m[1]);
  return names;
}

// ---- 前端侧：抽已经处理的 case 名 ----
function frontendHandled(jsSrc) {
  const names = new Set();
  for (const m of jsSrc.matchAll(/case\s+['"]([a-z_]+)['"]\s*:/g)) names.add(m[1]);
  return names;
}

// 事件名 -> 处理它的前端文件。新增一个带类型帧的 SSE 端点时，必须在这里登记，
// 否则下面的「端点清单」断言会红 —— 强制有人为「谁在前端接这个流」做一次决定，
// 而不是发完帧就不管了（admin 训练页就是这么丢的）。
const PAIRS = [
  {
    label: '训练进度流 POST /api/admin/train',
    backend: 'internal/api/admin.go',
    frontend: ['web/js/admin.js'],
  },
  {
    label: '聊天流 POST /api/chat',
    backend: 'internal/api/chat.go',
    frontend: ['web/js/chat.js'],
  },
  {
    label: '技能试用流 POST /api/generate',
    backend: 'internal/api/skills.go',
    frontend: ['web/js/app.js'],
  },
];

console.log('SSE 事件名契约');
for (const pair of PAIRS) {
  const evs = backendEvents(read(pair.backend));
  const handled = new Set();
  for (const f of pair.frontend) for (const n of frontendHandled(read(f))) handled.add(n);
  const missing = [...evs].filter((e) => !handled.has(e)).sort();
  console.log(`  ${pair.label}: 后端 [${[...evs].sort().join(', ')}] / 前端认 [${[...handled].sort().join(', ')}]`);
  check(`${pair.label} —— 后端发出的每个事件名前端都有 case`,
    evs.size > 0 && missing.length === 0,
    missing.length ? `没人在前端接这些帧：${missing.join(', ')}（帧会被静默丢弃，界面只剩转圈）` : '后端一个事件名都没抽到，检查解析');
}

// 训练页那条 bug 的具体形态：只认 'stage' 而丢掉 'status'/'step'。把这条单独钉死，
// 以后谁把 case 改回 'stage' 一个、或者后端把名字改回去，这里立刻红。
{
  const adminJS = read('web/js/admin.js');
  const adminGo = read('internal/api/admin.go');
  check('训练页认 status', /case\s+['"]status['"]\s*:/.test(adminJS));
  check('训练页认 step', /case\s+['"]step['"]\s*:/.test(adminJS));
  check('后端确实还在发 status/step 这两个名字',
    /send\("status"/.test(adminGo) && /send\("step"/.test(adminGo));
}

// 训练要跑二十分钟、后端只在阶段边界发帧，中间可能长时间静默。屏幕必须靠**心跳**
// 持续显示「还在跑」——「卡着计时不动」就是用户投诉的那句话。没了心跳，长跑任务
// 会退化成「一个不动的框」。
{
  const adminJS = read('web/js/admin.js');
  check('训练页有 1 秒心跳（setInterval 刷「已用时长」）', /setInterval\([\s\S]{0,400}?1000\s*\)/.test(adminJS));
  check('心跳里显示已用时长与距上次进度', /距上次进度/.test(adminJS));
  check('心跳在 finally 里被清掉（不留定时器泄漏）', /finally\s*\{[\s\S]{0,120}?clearInterval\(hb\)/.test(adminJS));
  check('每次收到进度帧都刷新「上次进度」时间戳', /lastEvAt\s*=\s*Date\.now\(\)/.test(adminJS));
}

if (failures) { console.log(`\n${failures} 项失败`); process.exit(1); }
console.log('\n全部通过');
