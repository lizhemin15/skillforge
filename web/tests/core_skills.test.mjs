#!/usr/bin/env node
// 「核心技能 = 通用能力」的前端回归防线 —— 零依赖，CI 里直接 node 跑。
//
// 守的是一类**改错了界面照常显示、事后才被用户发现**的坑：
//
//   Bug X — 技能管理里核心技能和业务技能混成一坨：用户每次都要从头找
//           「办公文档管家」，而"核心"这个分类也就白设了。
//   Bug Y — 核心技能被删掉（删完还能继续用，但聊天里再也匹配不到通用能力）。
//           删除按钮必须由 is_core 把关。
//   Bug Z — 切换核心标记没打鉴权头 / 打错接口，点了没反应也不报错。
//
// 这一层测不了 DOM 渲染（管理端要登录态 + CodeMirror），所以测的是**行为契约**：
// 出货文件里到底调了哪个接口、发了什么字段、什么条件下渲染删除按钮。
// 真正的界面由浏览器 E2E 兜（见 memory/2026-09-12.md 的验收记录）。

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const WEB = join(here, '..');
const ADMIN_JS = readFileSync(join(WEB, 'js', 'admin.js'), 'utf8');
const ADMIN_HTML = readFileSync(join(WEB, 'admin.html'), 'utf8');
const STYLE_CSS = readFileSync(join(WEB, 'css', 'style.css'), 'utf8');

let failures = 0;
function check(name, cond, extra = '') {
  if (cond) console.log(`  ok   ${name}`);
  else { failures++; console.log(`  FAIL ${name}${extra ? ' — ' + extra : ''}`); }
}

// 取 setSkillCore 的函数体做文本级契约检查（含接口、字段、鉴权头）。
const start = ADMIN_JS.indexOf('window.setSkillCore =');
const end = ADMIN_JS.indexOf('// ---------- knowledge-base skill detail ----------', start);
const setCoreBody = start >= 0 && end > start ? ADMIN_JS.slice(start, end) : '';

console.log('技能管理 · 核心/业务分组（Bug X）');
{
  check('列表按 is_core 分成两组', /const core = .*is_core/.test(ADMIN_JS) && /const biz = .*!sk\.is_core|const biz = .*!.*is_core/.test(ADMIN_JS));
  check('有「核心技能」分组标题', ADMIN_JS.includes('核心技能'));
  check('有「业务技能」分组标题', ADMIN_JS.includes('业务技能'));
  check('核心组渲染在业务组之前',
    ADMIN_JS.indexOf("groupBlock('核心技能") >= 0 &&
    ADMIN_JS.indexOf("groupBlock('核心技能") < ADMIN_JS.indexOf("groupBlock('业务技能"), '顺序反了');
  check('分组说明写清"通用能力"口径', ADMIN_JS.includes('通用能力') && ADMIN_JS.includes('不可删除'));
  check('空核心组有引导文案', ADMIN_JS.includes('设为核心'));
  // 页面上的说明也要跟代码口径一致，否则用户看到的规则和系统行为是两套
  check('admin.html 说明核心=通用能力', ADMIN_HTML.includes('核心技能') && ADMIN_HTML.includes('通用能力'));
  check('admin.html 说明核心不可删除', ADMIN_HTML.includes('不可删除'));
  for (const sel of ['.mg-group', '.mg-dot.core', '.mg-empty']) {
    check(`style.css 有 ${sel}`, STYLE_CSS.includes(sel));
  }
}

console.log('技能管理 · 核心技能的删除保护（Bug Y）');
{
  // 删除按钮必须被 is_core 条件包住：核心技能没有 ✕。
  check('核心技能不渲染删除按钮', /\$\{sk\.is_core \? '' : `<button class="icon-btn danger"[^`]*delSkill/.test(ADMIN_JS),
    '删除按钮没有用 is_core 把关');
  // 反向确认：删除函数本身还在（别为了"保护"把功能整没了）
  check('非核心仍可删除', /window\.delSkill = /.test(ADMIN_JS));
}

console.log('技能管理 · 切换核心标记的请求契约（Bug Z）');
{
  check('抽到了 setSkillCore', setCoreBody.length > 0);
  check('打到 /api/admin/skills/core', setCoreBody.includes("'/api/admin/skills/core'"));
  check('用 POST', /method:\s*'POST'/.test(setCoreBody));
  check('带鉴权头', setCoreBody.includes('authHdr()'));
  check('发 slug 与 is_core', /JSON\.stringify\(\{\s*slug,\s*is_core:\s*isCore\s*\}\)/.test(setCoreBody));
  check('成功后刷新两个列表', setCoreBody.includes('loadManageSkills()') && setCoreBody.includes('loadSkillsPublic'));
  check('失败有提示', /toast\(j\.error/.test(setCoreBody));
  // 取消核心会让技能变成可删除 → 必须先确认，避免误点
  check('取消核心有二次确认', /!isCore && !confirm\(/.test(setCoreBody));
  // 行内按钮文案：核心→"取消核心"，非核心→"设为核心"
  check('按钮文案随状态切换', /\$\{sk\.is_core \? '取消核心' : '设为核心'\}/.test(ADMIN_JS));
}

if (failures) { console.log(`\n${failures} 项失败`); process.exit(1); }
console.log('\n全部通过');
