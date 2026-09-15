#!/usr/bin/env node
// 「停用」必须可逆 —— 前端 + 接线契约防线。
//
// 用户报的原文：「业务技能停用了就消失了」。
//
// Bug N 的结构值得记住：**管理端列表打的是公开接口**。
// `internal/api/skills.go` 的 List 会把停用技能过滤掉（公开口径：对话页
// 技能勾选层、首页卡片都不该出现停用技能，Generate 对它们直接 403）——
// 这是对的。错的是管理端「技能管理」也拿这个列表渲染，而它自己明明渲染了
// `<span class="pill-off">已停用</span>` 和「启用」按钮：**渲染代码永远收不到
// 数据**。于是点一次「停用」，那一行就从界面上蒸发，连恢复入口一起带走，
// 想启用只能手改 sqlite。
//
// 同一个根因的第二个症状：管理端编辑技能元信息（名称/分类/描述）也用公开列表
// 预填表单，编辑一个**已停用**技能时 `if (!sk) return;` 静默跳过 → 输入框全空，
// 用户一保存就把名称/描述清空了。
//
// 所以这里守三件事：
//   ① 管理端读接口 = /api/admin/skills（全量），别再退回公开列表；
//   ② 公开列表**仍然**过滤停用技能（防「把过滤删掉」这种反向修法）；
//   ③ 后端真注册了这条路由，且 ListAll 里不许出现过滤用的 continue。
// 这一层测不了渲染（管理端要登录态 + CodeMirror），测的是**行为契约**：
// 出货文件里到底调了哪个接口、哪条路由指向哪个 handler。

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const WEB = join(here, '..');
const REPO = join(WEB, '..');
const ADMIN_JS = readFileSync(join(WEB, 'js', 'admin.js'), 'utf8');
const ROUTER_GO = readFileSync(join(REPO, 'internal', 'api', 'router.go'), 'utf8');
const SKILLS_GO = readFileSync(join(REPO, 'internal', 'api', 'skills.go'), 'utf8');

let failures = 0;
function check(name, cond, extra = '') {
  if (cond) console.log(`  ok   ${name}`);
  else { failures++; console.log(`  FAIL ${name}${extra ? ' — ' + extra : ''}`); }
}

// 抠函数体：从 startAnchor 到 endAnchor 之前。锚点找不到 → 空串，调用方必红。
function sliceFrom(src, startAnchor, endAnchor) {
  const a = src.indexOf(startAnchor);
  if (a < 0) return '';
  const b = src.indexOf(endAnchor, a + startAnchor.length);
  return b > a ? src.slice(a, b) : src.slice(a);
}

const manageBody = sliceFrom(ADMIN_JS, 'async function loadManageSkills()', 'window.toggleSkill =');
const listBody = sliceFrom(SKILLS_GO, 'func (h *Skills) List(', 'func (h *Skills) ListAll(');
const listAllBody = sliceFrom(SKILLS_GO, 'func (h *Skills) ListAll(', 'func (h *Skills) Get(');
// 元信息预填：锚点是那句 find(curSkillSlug)，它前面**最近的一次** fetch 就是
// 预填用的接口。用 lastIndexOf 精确回溯，不靠「往回数 400 字符」这种会在
// 注释变长时静默失准的做法。
const prefillAnchor = ADMIN_JS.indexOf("const sk = (j.skills || []).find(x => x.slug === curSkillSlug)");
const prefillFetchAt = prefillAnchor >= 0 ? ADMIN_JS.lastIndexOf('fetch(', prefillAnchor) : -1;
const prefillBody = prefillAnchor >= 0 && prefillFetchAt >= 0 ? ADMIN_JS.slice(prefillFetchAt, prefillAnchor) : '';

console.log('技能管理 · 停用技能还在列表里（Bug N：停用变成不可逆）');
{
  check('抽到了 loadManageSkills', manageBody.length > 0);
  check('管理列表打 /api/admin/skills（全量）',
    manageBody.includes("'/api/admin/skills'"), '管理端又用回公开列表了');
  check('管理列表不再打公开 /api/skills',
    !/fetch\('\/api\/skills'/.test(manageBody), '公开列表过滤掉停用技能，拿它渲染管理列表 = 停用即消失');
  check('管理列表带鉴权头', manageBody.includes('authHdr()'));
  // 修好了也得让人看得见：药丸 + 恢复入口必须还在
  check('仍渲染「已停用」药丸', ADMIN_JS.includes('pill-off') && ADMIN_JS.includes('已停用'));
  check('仍渲染「启用」按钮', /\$\{sk\.enabled \? '停用' : '启用'\}/.test(ADMIN_JS));
}

console.log('技能管理 · 编辑元信息时的预填（同一个根因的第二个症状）');
{
  check('抽到了元信息预填逻辑', prefillBody.length > 0);
  check('预填也打 /api/admin/skills',
    prefillBody.includes("'/api/admin/skills'"), '编辑已停用技能时表单会是空的，一保存就清空名称');
  check('预填不再打公开 /api/skills',
    !/fetch\('\/api\/skills'/.test(prefillBody), '停用技能读不到 → 输入框空 → 保存即清空元信息');
}

console.log('后端 · 两个口径不许再被合成一个');
{
  check('抽到了 Skills.List', listBody.length > 0);
  check('抽到了 Skills.ListAll', listAllBody.length > 0);
  // ① 公开口径必须仍然过滤停用技能 —— 否则「停用」会漏进对话页，用户点了拿 403
  check('公开 List 仍然跳过停用技能',
    /if\s*!s\.Enabled[^{]*\{\s*continue\s*\}/.test(listBody),
    '公开列表把过滤删了：停用技能会出现在对话页勾选层（点了必 403）');
  // ② 管理口径不许过滤：过滤必然要 continue 跳过，所以「没有 continue」就是判据
  check('ListAll 里没有过滤用的 continue',
    !/\bcontinue\b/.test(listAllBody), 'ListAll 又在过滤停用技能，管理端会重新变成只读一半');
  check('ListAll 回传 disabled_count', listAllBody.includes('disabled_count'));
}

console.log('路由 · 管理端全量列表真的有挂上');
{
  check('注册了 GET /api/admin/skills',
    /"GET \/api\/admin\/skills"\s*,\s*h\.Auth\.Middleware\(h\.Skills\.ListAll\)/.test(ROUTER_GO),
    '路由没挂上 → 前端 404 → 列表空 → 比修之前更糟');
  check('挂在鉴权中间件后面', /h\.Auth\.Middleware\(h\.Skills\.ListAll\)/.test(ROUTER_GO));
  // 反向确认：公开路由还在（别为了加管理端把公开接口顶掉）
  check('公开 GET /api/skills 仍在', /"GET \/api\/skills"\s*,\s*h\.Skills\.List\b/.test(ROUTER_GO));
}

if (failures) { console.log(`\n${failures} 项失败`); process.exit(1); }
console.log('\n全部通过');
