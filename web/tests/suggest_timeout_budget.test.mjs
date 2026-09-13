// 守「推荐行到底给模型多少时间」这一对数字。
//
// 为什么单独立一条：这两个数写在两个语言的文件里（router.go 的 ctx / chat.js 的 abort），
// 它们不匹配时的表现是**完全静默**的 —— 服务端还在答、客户端已经断了，或者兜底重试
// 刚发出去就被 ctx 掐死；用户看到的是「动态推荐怎么老是那几句」，页面上没有任何错误。
// 这次就是这样踩的：后端 5s 卡掉了 ×4 预算的那次重试，等于兜底白加。
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const REPO = new URL('../../', import.meta.url);
const read = (p) => readFileSync(new URL(p, REPO), 'utf8');

test('建议行的超时预算：后端要够 3 次尝试，前端要比后端长', () => {
  const go = read('internal/api/router.go');
  const js = read('web/js/chat.js');

  // 后端：newSuggestHandler(h.Eng, 9*time.Second)
  const mGo = go.match(/newSuggestHandler\([^,]+,\s*(\d+)\s*\*\s*time\.Second\)/);
  assert.ok(mGo, 'router.go 里找不到 newSuggestHandler 的超时参数 —— 路由被改写了，这条守卫失效');
  const backendSec = Number(mGo[1]);

  // 前端：setTimeout(() => { if (ctl) ctl.abort(); }, 10500)
  // 绑到 abort 那一句上，避免顺手抓到别的 setTimeout。
  const mJs = js.match(/ctl\.abort\(\);\s*\}\s*,\s*(\d+)\s*\)/);
  assert.ok(mJs, 'chat.js 里找不到推荐行的 abort 超时 —— 这条守卫失效');
  const frontendMs = Number(mJs[1]);

  // ① 后端得容得下 FastJSON 内部的 3 次尝试（换开关 / 放大预算）。
  //    实测单次 ~1s，最坏链路 ~6s；低于 8s 就是在赌 provider 永远只走第一趟。
  assert.ok(
    backendSec >= 8,
    `后端 ctx 只有 ${backendSec}s：FastJSON 最多打 3 次请求（400 摘字段 / 空 content ×4 预算），` +
      `卡在 5s 时那次放大预算的重试会被掐断 —— 兜底白加，而且这层失败完全无声。`,
  );

  // ② 前端必须比后端长。前端先掐的话，服务端答出来也没人接，
  //    表现和网络错误一模一样，没人会往「谁先超时」上想。
  assert.ok(
    frontendMs > backendSec * 1000,
    `前端 abort ${frontendMs}ms 没比后端 ${backendSec}s 长：客户端先断，服务端的结果白算。`,
  );

  // ③ 别顺手把前端调成「用户等半分钟」那种数：推荐行是锦上添花，
  //    超过 15s 还不如早点退回规则版。
  assert.ok(
    frontendMs <= 15000,
    `前端 abort ${frontendMs}ms 太长了：推荐行等这么久，不如直接退回规则版胶囊。`,
  );
});
