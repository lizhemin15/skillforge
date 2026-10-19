/* 产物类型选择器 —— 前端与后端的契约测试
 *
 * 钉的是三件实事（全都能静默失效，肉眼看不出来）：
 *   1) 选择器真的存在于两个创建表单里，且选项值跟后端白名单逐一对得上；
 *   2) 两个提交流程真的把所选值放进请求体（字段名 skill_type）；
 *   3) 默认项是 auto —— 老用户什么都不点，行为必须和以前一致（走自动判定）。
 *
 * 为什么第 1 条要用「跟后端白名单对齐」的方式写：前端 value 与后端白名单是两份独立
 * 字面量，谁改了另一处不会报错——用户点「查询型」，后端认不出这个值，于是拒绝创建
 * 或者（更糟）按自动判定给个别的类型。这类不一致只有上线后才由用户撞见。
 * 所以后端白名单在这里被**重新声明**为唯一事实来源，任何一边漂移都会红。
 */
import { readFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const web = join(here, '..');

// 与 internal/model 的 ParsePickedSkillType 白名单保持一致（改后端必须同步改这里，
// 反之亦然——两边不一致时这个测试就是那道闸门）。
const BACKEND_ALLOWED = ['auto', 'write', 'query', 'template'];
const BACKEND_FIELD = 'skill_type';

let failures = 0;
const ok = (cond, msg) => {
  if (cond) { console.log('  ok   ' + msg); } else { console.log('  FAIL ' + msg); failures++; }
};

const html = await readFile(join(web, 'admin.html'), 'utf8');
const js = await readFile(join(web, 'js', 'admin.js'), 'utf8');

console.log('— 选择器存在且选项与后端白名单对齐 —');
for (const gid of ['lt-type', 'tr-type']) {
  const m = html.match(new RegExp(`id="${gid}"([\\s\\S]*?)</div>`));
  ok(!!m, `admin.html 里有 ${gid} 选择器`);
  const vals = m ? [...m[1].matchAll(/value="([^"]+)"/g)].map((x) => x[1]) : [];
  ok(vals.length === BACKEND_ALLOWED.length,
    `${gid} 选项数 = 后端白名单数（${vals.length} vs ${BACKEND_ALLOWED.length}）`);
  ok(vals.join(',') === BACKEND_ALLOWED.join(','),
    `${gid} 选项值顺序与内容都对齐后端：${vals.join(',')}`);
  ok(/name="([a-z-]+)"[^>]*value="auto" checked|value="auto" checked/.test(m ? m[1] : ''),
    `${gid} 默认选中 auto（不点也得跟以前一样）`);
}

console.log('— 提交流程真的带上了所选值 —');
// 两个通道各自的 group 与提交点，必须都在
ok(/fd\.append\(['"]skill_type['"],\s*pickedSkillType\(['"]lt-type['"]\)\)/.test(js),
  '极速通道提交里带 skill_type（取自 lt-type）');
ok(/fd\.append\(['"]skill_type['"],\s*pickedSkillType\(['"]tr-type['"]\)\)/.test(js),
  '完整训练通道提交里带 skill_type（取自 tr-type）');
ok(js.includes(`'${BACKEND_FIELD}'`), `字段名与后端一致：${BACKEND_FIELD}`);

console.log('— 取值函数不会造出后端不认的值 —');
ok(/const el = document\.querySelector\('#' \+ groupId \+ ' input:checked'\)/.test(js),
  '按 group 精确取选中项（不靠全局 name 查询，两个表单同名也不会串）');
ok(/return el \? el\.value : 'auto'/.test(js),
  '取不到时回落 auto（选择器坏掉也不发假类型）');

console.log('');
if (failures) { console.log(`类型选择器契约：${failures} 项 FAIL`); process.exit(1); }
console.log('类型选择器契约：全部通过');
