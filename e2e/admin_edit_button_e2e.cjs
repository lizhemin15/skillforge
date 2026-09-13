// 端到端实测：技能知识库左树里点「编辑」按钮，编辑器必须真的打开。
//
// 为什么必须点**按钮**而不是点行：行的 onclick 是 openEditorFile，按钮的 onclick
// 是 stopPropagation + 自己的动作。点按钮时冒泡被掐死，**只有按钮自己的 handler
// 能生效** —— 所以「点按钮后编辑器打开了」就等价于「按钮接对了线」。
// 反过来，按钮是死按钮时点它什么也不会发生（行接不到事件）。
//
// 凭据从 skillforge.env 读，不打印、不进日志。
const { chromium } = require('/root/.hermes/hermes-agent-old/node_modules/playwright');
const fs = require('node:fs');

const BASE = 'http://127.0.0.1:8092';
const envSrc = fs.readFileSync('/opt/skillforge/skillforge.env', 'utf8');
const env = {};
for (const line of envSrc.split('\n')) {
  const m = line.match(/^([A-Za-z_]+)=(.*)$/);
  if (m) env[m[1]] = m[2].replace(/^["']|["']$/g, '');
}
const USER = env.SKILLFORGE_ADMIN_USER, PASS = env.SKILLFORGE_ADMIN_PASS;
if (!USER || !PASS) { console.error('env 里没有管理员凭据'); process.exit(2); }

let failures = 0;
const check = (name, cond, extra = '') => {
  if (cond) console.log(`  ok   ${name}`);
  else { failures++; console.log(`  FAIL ${name}${extra ? ' — ' + extra : ''}`); }
};

(async () => {
  const browser = await chromium.launch();
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(String(e)));

  await page.goto(`${BASE}/admin`, { waitUntil: 'domcontentloaded' });
  await page.fill('#lg-user', USER);
  await page.fill('#lg-pass', PASS);
  await page.click('#login-form button[type=submit]');
  await page.waitForSelector('#shell:not(.hidden)', { timeout: 15000 });
  console.log('登录成功');

  // 拿技能列表（页面内 fetch，自动带鉴权头）
  const skills = await page.evaluate(async () => {
    const r = await fetch('/api/skills', { headers: { Authorization: 'Bearer ' + (localStorage.getItem('sf_token') || '') } });
    return r.ok ? r.json() : { error: r.status };
  });
  const list = Array.isArray(skills) ? skills : (skills.skills || skills.data || []);
  check('拿得到技能列表', list.length > 0, JSON.stringify(skills).slice(0, 200));
  if (!list.length) { await browser.close(); process.exit(1); }

  // 优先挑带 categories/ 的技能（本轮新功能），否则用第一个
  const withCat = list.find((s) => (s.kinds || s.fileKinds || []).includes?.('category'));
  const skill = withCat || list[0];
  const slug = skill.slug || skill.name;
  console.log(`测试技能：${slug}`);

  await page.evaluate((s) => window.openSkillDetail(encodeURIComponent(s), ''), slug);
  await page.waitForSelector('#kb-tree .kb-tree-row', { timeout: 15000 });

  // 左树里所有带「编辑」按钮的行
  const rows = await page.evaluate(() => {
    return [...document.querySelectorAll('#kb-tree .kb-tree-row')].map((row) => {
      const btn = [...row.querySelectorAll('button')].find((b) => b.textContent.trim() === '编辑');
      return { path: row.dataset.path, hasEditBtn: !!btn };
    });
  });
  const editable = rows.filter((r) => r.hasEditBtn);
  console.log(`左树共 ${rows.length} 行，其中带「编辑」按钮的 ${editable.length} 行`);
  check('左树里有带「编辑」按钮的文件', editable.length > 0,
    `行: ${JSON.stringify(rows.map((r) => r.path))}`);

  const target = editable[0];
  if (!target) { await browser.close(); process.exit(1); }
  console.log(`点「编辑」按钮 → ${target.path}`);

  // 关键动作：点**按钮**（不是行）
  await page.evaluate((p) => {
    const row = [...document.querySelectorAll('#kb-tree .kb-tree-row')].find((r) => r.dataset.path === p);
    [...row.querySelectorAll('button')].find((b) => b.textContent.trim() === '编辑').click();
  }, target.path);

  // 断言：编辑器真的打开了，而且打开的是这一行的文件
  let opened = false, editorPath = '';
  try {
    await page.waitForFunction(
      () => { const ed = document.getElementById('kb-editor'); return ed && ed.classList.contains('ide-mode'); },
      { timeout: 8000 },
    );
    opened = true;
    editorPath = await page.evaluate(() => {
      const ed = document.getElementById('kb-editor');
      const t = ed.textContent || '';
      return t.slice(0, 120);
    });
  } catch { opened = false; }

  check('★ 点「编辑」按钮后编辑器打开了（按钮是活的）', opened,
    '按钮点击被吃掉 → 编辑器不出现。这就是用户报的「不能编辑」');
  check('选中的行被高亮为 active（说明走的确实是这一行）',
    opened && await page.evaluate((p) => {
      const r = [...document.querySelectorAll('#kb-tree .kb-tree-row')].find((x) => x.dataset.path === p);
      return !!r && r.classList.contains('active');
    }, target.path));

  // 证据留档
  if (opened) {
    const name = await page.evaluate(() => (document.body.innerText.match(/已打开|编辑/) || [''])[0]);
    console.log(`  编辑器首屏文本：${JSON.stringify(editorPath.replace(/\s+/g, ' ').slice(0, 100))}`);
  }
  await page.screenshot({ path: '/tmp/sf-edit-btn-e2e.png', fullPage: false });
  console.log('截图：/tmp/sf-edit-btn-e2e.png');

  check('控制台没有 JS 异常', errors.length === 0, errors.slice(0, 2).join(' | '));

  await browser.close();
  console.log(failures ? `\n${failures} 条断言红了` : '\n全绿 ✓');
  process.exit(failures ? 1 : 0);
})().catch((e) => { console.error('脚本异常：', e.message); process.exit(3); });
