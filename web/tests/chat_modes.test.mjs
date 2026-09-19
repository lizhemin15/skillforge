#!/usr/bin/env node
// 「聊天两种方式」的前端回归防线 —— 零依赖，CI 里直接 node 跑。
//
// 守的是一类**坏了界面不报错、用户也说不清哪里不对**的坑：
//
//   Bug U — 手动选了技能，发出去却退化成自动调度（mode/skill 漏发或在自动模式下
//           残留了上一次选的技能）。表现是"我明明锁定了采购合同，它却给我写了篇通稿"，
//           聊天里没有任何报错，最难查。
//   Bug V — 技能候选不分组：核心技能（通用能力）混在业务技能里，用户每次都要
//           从头找"办公文档管家"。
//   Bug W — 手动模式没选技能就直接发出去，后端替你猜一个 —— 那就等于没有"指定技能"。
//   Bug X — 手动没选技能点发送，界面必须**当场**告诉用户"差一步"。
//           这一条升级过一次：原来是弹技能面板（面板自己还有 Bug X：点发送的冒泡
//           立刻把它关掉 → "点了毫无反应"）；现在没有面板了，改为推荐行抖动高亮。
//           坑的性质没变：反馈必须落在用户该点的地方，不能只写控制台。
//
// 做法：**从出货文件里抠出真正跑的那个函数来跑**（web/js/chat.js 里的
// chatPayload / sendBlocked / chipPlan / runChip / syncThumb），而不是在测试里
// 抄一份实现。抄一份的话，改坏了真文件测试照样绿。

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const WEB = join(here, '..');
const CHAT_JS = readFileSync(join(WEB, 'js', 'chat.js'), 'utf8');
const INDEX_HTML = readFileSync(join(WEB, 'index.html'), 'utf8');
const STYLE_CSS = readFileSync(join(WEB, 'css', 'style.css'), 'utf8');

let failures = 0;
function check(name, cond, extra = '') {
  if (cond) console.log(`  ok   ${name}`);
  else { failures++; console.log(`  FAIL ${name}${extra ? ' — ' + extra : ''}`); }
}

// extractFn 按花括号配平从出货文件里抽出具名函数（含函数体）。
function extractFn(src, signature) {
  const start = src.indexOf(signature);
  if (start < 0) return null;
  let depth = 0, seen = false;
  for (let k = src.indexOf('{', start); k < src.length; k++) {
    if (src[k] === '{') { depth++; seen = true; }
    else if (src[k] === '}') { depth--; if (seen && depth === 0) return src.slice(start, k + 1); }
  }
  return null;
}

// 把出货函数取出来，并把函数体里引用的闭包变量当形参注进去。
// 注意外面还要再包一层：new Function(...deps, 'return ' + fn) 得到的是一个
// **返回该函数的工厂**，多调一次 () 就等于把业务函数当无参函数跑掉了。
function bind(src, signature, deps = [], values = []) {
  const fn = extractFn(src, signature);
  if (!fn) throw new Error('抽不到函数：' + signature);
  return new Function(...deps, 'return ' + fn)(...(values || []));
}

// extractClickListener：抽出**真的会关那一层**的文档级 click 监听器（按函数体里的
// marker 认人，如 closeSkLayer() / closeMCPLayer()），并带出它注册在哪个阶段。
//
// 为什么不"取第一个 / 全文搜"：同一个仓库里这类监听器会长出好几个（技能层、MCP 层…），
// 先加的那个会把断言喂饱，后加的那个怎么坏都不响 —— 2026-09-19 真的发生了：
// MCP 层一加，技能层的「捕获阶段」「isConnected 兜底」两条断言当场变假绿
// （chat_ui_mutation_check.py 注入 14/15 报出来的）。
function extractClickListener(src, marker) {
  const out = [];
  let from = 0;
  for (;;) {
    const i = src.indexOf("document.addEventListener('click'", from);
    if (i < 0) break;
    const open = src.indexOf('{', i);
    if (open < 0) break;
    let depth = 0, end = -1;
    for (let k = open; k < src.length; k++) {
      if (src[k] === '{') depth++;
      else if (src[k] === '}') { depth--; if (depth === 0) { end = k; break; } }
    }
    if (end < 0) break;
    const body = src.slice(i, end + 1);
    out.push({ body, capture: /^\s*,\s*true\s*\)/.test(src.slice(end + 1, end + 14)) });
    from = end + 1;
  }
  return out.filter((x) => x.body.includes(marker));
}

// ---------- 真实的依赖（从出货文件里抠，避免测试自带一份"理想实现"）----------
const CHIP_HINT = (() => {
  const m = CHAT_JS.match(/const\s+CHIP_HINT\s*=\s*\{[\s\S]*?\n\s*\};/);
  if (!m) throw new Error('抽不到配置表：CHIP_HINT');
  return new Function(m[0] + '\nreturn CHIP_HINT;')();
})();
// chipPlan 不是孤立函数：它调的 askFor / askedFor / quickExample 全在同一个闭包里。
// 所以不能只抽 chipPlan 一个 —— 那会 ReferenceError。这里把整条链都从出货文件抽出来
// 再互相注入，等于**在测试里重建同一个闭包**，跑的还是真代码，一行没抄。
const TYPE_META = (() => {
  const m = CHAT_JS.match(/const\s+TYPE_META\s*=\s*\{[\s\S]*?\n\s*\};/);
  if (!m) throw new Error('抽不到配置表：TYPE_META');
  return new Function(m[0] + '\nreturn TYPE_META;')();
})();
const quickExample = bind(CHAT_JS, 'function quickExample(', ['TYPE_META'], [TYPE_META]);
const askFor = bind(CHAT_JS, 'function askFor(', ['quickExample'], [quickExample]);
const shortAskLabel = bind(CHAT_JS, 'function shortAskLabel(');
// allSkills 是运行态闭包变量 —— 这里当成参数注入，等价于"索引已加载"的状态。
const buildChipPlan = (allSkills = []) => {
  const askedFor = bind(CHAT_JS, 'function askedFor(',
    ['askFor', 'allSkills', 'shortAskLabel'], [askFor, allSkills, shortAskLabel]);
  return bind(CHAT_JS, 'function chipPlan(',
    ['CHIP_HINT', 'askedFor', 'askFor', 'quickExample'],
    [CHIP_HINT, askedFor, askFor, quickExample]);
};
// runChip 依赖 pickSkill/clearSkill/setMode/setInput/submit 这些动作，这里全部换成
// 探针 —— 测的是"点了 chip 有没有接到该接的动作"，不是那几个动作自身的行为。
function makeRunChip(log, opts = {}) {
  // input 先造出来：setInput 要真的写进它的 value，否则 toEnd 探针读到的是空串，
  // 测试就分不清"填了没填"（假绿）。
  const input = { value: opts.inputValue || '', focus: () => log.push('focus') };
  const deps = {
    pickSkill: (sk) => log.push('pick:' + sk.slug),
    clearSkill: () => log.push('unpick'),
    setMode: (m) => log.push('mode:' + m),
    setInput: (t) => { input.value = t; log.push('input:' + t); },
    submit: () => log.push('submit'),
    // 勾选层的开关：runChip 只负责"点了要开/关层"，层自身的行为另有测试
    toggleSkLayer: () => log.push('layer:toggle'),
    // 光标归位：真实现会调 input.setSelectionRange（沙箱的裸对象没有），
    // 这里换成探针，测的是"填完有没有把光标送走"。
    toEnd: (node) => log.push('toEnd:' + String(node && node.value)),
    input,
    allSkills: opts.skills || [],
  };
  const names = Object.keys(deps);
  const fn = bind(CHAT_JS, 'function runChip(', names, names.map((n) => deps[n]));
  return { runChip: fn, log };
}

// chatPayload / sendBlocked 是纯函数，直接抽真代码。
const chatPayload = (sid, mode, picked) =>
  bind(CHAT_JS, 'function chatPayload(', ['sessionId', 'chatMode', 'pickedSkill'], [sid, mode, picked]);
const sendBlocked = bind(CHAT_JS, 'function sendBlocked(');
// 滑块几何：必须用实测宽度，不能把 50% 写死（两档文案宽度不同、字体回退会错位）。
const syncThumb = bind(CHAT_JS, 'function syncThumb(');

const SKILLS = [
  { slug: '办公文档管家', name: '办公文档管家', description: '生成或填写 Word/Excel/PDF/PPT', skill_type: 'docgen', is_core: 1 },
  { slug: '技能工厂', name: '技能工厂', description: '按需求产出一份可落地的技能定义', skill_type: 'write', is_core: 1 },
  { slug: '采购合同', name: '采购合同', description: '按模板生成采购合同', skill_type: 'template', is_core: 0 },
  { slug: '公司新闻通稿', name: '公司新闻通稿', description: '写公司新闻通稿', skill_type: 'write', is_core: 0 },
];

// 默认沙箱：技能索引已加载（allSkills = SKILLS）
const chipPlan = buildChipPlan(SKILLS);

console.log('聊天方式 · 请求体（Bug U）');
{
  // 手动 + 已选技能：mode 与 skill 必须同时到位
  const b = JSON.parse(chatPayload('s1', 'manual', { slug: '采购合同', name: '采购合同' })('甲方改成北京华创'));
  check('手动模式带上 mode=manual', b.mode === 'manual', JSON.stringify(b));
  check('手动模式带上 skill', b.skill === '采购合同', JSON.stringify(b));
  check('手动模式保留原文', b.message === '甲方改成北京华创');
  check('手动模式带 session_id', b.session_id === 's1');

  // 自动模式：哪怕还残留着上次选的技能，也必须不带 skill，
  // 否则"切回自动"只是视觉效果，后端仍按旧技能走。
  const a = JSON.parse(chatPayload('s1', 'auto', { slug: '采购合同', name: '采购合同' })('帮我写篇通稿'));
  check('自动模式 mode=auto', a.mode === 'auto', JSON.stringify(a));
  check('自动模式不残留 skill', a.skill === '', JSON.stringify(a));

  // 手动但没选技能：不许把空 slug 当"指定技能"发出去
  const n = JSON.parse(chatPayload('s1', 'manual', null)('随便'));
  check('手动未选技能时 skill 为空', n.skill === '');
}

console.log('聊天方式 · 发送守卫（Bug W）');
{
  check('手动+未选技能 → 拦住', sendBlocked('manual', null) === true);
  check('手动+已选技能 → 放行', sendBlocked('manual', { slug: 'x' }) === false);
  check('自动+未选技能 → 放行', sendBlocked('auto', null) === false);

  // Bug X（升级版）：被拦住那一帧必须让用户看见"差一步"，而且要**给出出路**。
  // 面板重做之后，"出路"就是勾选层 —— 抖只是"看这里"，开层才是"这里能解决"。
  // 断言钉在整段形状上（nudge + openLayer + return），不写 includes('nudgeChips')
  // 那种会命中函数定义的恒真假绿。
  const blockedBranch = (CHAT_JS.match(/if \(sendBlocked\(chatMode, pickedSkill\)\) \{[\s\S]*?\n    \}/) || [''])[0];
  check('★ 拦住后抖动推荐行 + 打开勾选层 + 立即返回',
    /nudgeChips\(\)/.test(blockedBranch) && /openSkLayer\(\)/.test(blockedBranch) && /return;/.test(blockedBranch),
    blockedBranch);
  check('★ 拦住那一帧不发请求（分支里没有 fetch）', !/fetch\(/.test(blockedBranch), blockedBranch);
  // nudgeChips 必须真的把 .need 类挂上去（只弹个 toast 的话，用户视线在输入框上）
  const nudge = extractFn(CHAT_JS, 'function nudgeChips(');
  check('抽到了 nudgeChips', !!nudge);
  check('抖动前先重算推荐行（保证抖的就是该点的那一行）',
    /renderChips\(\)[\s\S]*?classList\.add\('need'\)/.test(nudge || ''));
  check('抖完会摘掉 .need（不留下永久高亮）', /remove\('need'\)/.test(nudge || ''));
  check('抖完聚焦输入框（用户下一步是打字）', /input\.focus\(\)/.test(nudge || ''));
}

console.log('指定技能 · 推荐行给的是"开面板"的入口（Bug V 的根治）');
{
  // 旧契约：手动档把技能铺成 pick 候选，最多 4 颗。用户实测报"都没法选指定技能" ——
  // 技能库十几个起，铺 4 颗 = 剩下的永远点不到。新契约：推荐行只给一颗入口，
  // 全量清单搬进勾选层（下面的"技能勾选层"一节测它）。
  const plan = chipPlan({ skills: SKILLS, mode: 'manual', picked: null, turns: 0 });
  const labels = plan.items.map((c) => c.label).join('|');
  check('没选技能时给一颗开面板的入口', plan.items.some((c) => c.kind === 'skillpick'), labels);
  check('★ 不再把技能铺成 pick 候选（铺了就有技能永远点不到）',
    !plan.items.some((c) => c.kind === 'pick'), labels);
  check('入口带 slug 提示（肉眼知道点了会展开）', /▾/.test(labels), labels);
  check('入口抬头提醒"先选技能"', plan.hint === CHIP_HINT.manual, plan.hint);
  check('入口不超过 4 颗（不折行顶掉输入框）', plan.items.length <= 4, 'len=' + plan.items.length);

  // 已指定技能 → 入口显示已选的那个，并把位置让给"用它写点什么"的示范问句
  const picked = chipPlan({
    skills: SKILLS, mode: 'manual', picked: { slug: '采购合同', name: '采购合同' }, turns: 1,
  });
  check('已指定时入口显示技能名', picked.items.some((c) => c.kind === 'skillpick' && c.label.includes('采购合同')));
  check('已指定时给出示范问句', picked.items.some((c) => c.kind === 'ask' && c.send));
  check('已指定时不再堆技能候选', !picked.items.some((c) => c.kind === 'pick'));

  // ★ 两颗同名 chip 是纯噪声：实测并排出现「✓ 办公文档管家」+「办公文档管家」，
  // 用户看不出哪颗是改指定、哪颗是发送问句 → 示范 chip 的标签必须是**要发出去的那句话**。
  const labelsOf = (r) => r.items.map((c) => c.label);
  // ⚠️ 不能直接比字符串：入口那颗的真值是「技能：采购合同」，跟「采购合同」不相等，
  // 但用户眼里是两颗同名的 —— 撞名的是去掉前缀/勾号之后的显示文本。
  const bare = (l) => String(l).replace(/^(技能：|[✓✔\s]+)/, '');
  check('两颗 chip 去掉前缀后不撞名（撞了就分不清改指定/发送）',
    new Set(labelsOf(picked).map(bare)).size === picked.items.length,
    labelsOf(picked).map(bare).join(' | '));
  const dAsk = picked.items.find((c) => c.kind === 'ask');
  check('示范 chip 标签不等于技能名（否则跟入口那颗撞在一起）',
    !!dAsk && dAsk.label !== '采购合同', dAsk && dAsk.label);
  check('标签不过长（一行胶囊不至于把输入框顶下去）',
    !!dAsk && dAsk.label.length <= 21, dAsk && dAsk.label);
  // docgen 技能走 quickExample 的干净问句分支 → 标签就该是这句的截断
  const docPicked = chipPlan({
    skills: SKILLS, mode: 'manual', picked: { slug: '办公文档管家', name: '办公文档管家' }, turns: 1,
  });
  const pAsk = docPicked.items.find((c) => c.kind === 'ask');
  check('干净问句→标签是 send 的截断（点下去发的仍是全文）',
    !!pAsk && pAsk.send.startsWith(pAsk.label.replace(/…$/, '')), pAsk && (pAsk.label + ' / ' + pAsk.send));

  // 非 docgen 技能走的是 quickExample 兜底串「（用「名」标签：描述）」——
  // 整串糊在胶囊上又长又绕，标签该取冒号后的描述。
  const bizPicked = chipPlan({
    skills: SKILLS, mode: 'manual', picked: { slug: '采购合同', name: '采购合同' }, turns: 1,
  });
  const bAsk = bizPicked.items.find((c) => c.kind === 'ask');
  check('兜底串的标签取描述段，不把整串括号糊上去',
    !!bAsk && !bAsk.label.includes('（') && bAsk.label.includes('按模板生成采购合同'), bAsk && bAsk.label);
  check('标签摘掉了「用「技能名」」前缀', !!bAsk && !bAsk.label.startsWith('用「'), bAsk && bAsk.label);
  // 截断只动标签，不许动 send —— 发出去的必须还是完整那句
  check('截断不伤 send（send 里没有省略号）', !!bAsk && !bAsk.send.includes('…'), bAsk && bAsk.send);

  // 空技能库不能炸（后端还没 seed 完 / 全停用时）。
  // 断言的是"不炸 + 仍给出入口"：技能库空的时候把手动档的入口也收掉，
  // 用户就彻底没有出路了（面板里空着，推荐行也没得点）。
  const empty = chipPlan({ skills: [], mode: 'manual', picked: null, turns: 0 });
  check('空技能库不炸且仍给入口', empty.items.length >= 1 && empty.items.some((c) => c.kind === 'skillpick'));
  const noSk = chipPlan({ mode: 'manual', picked: null });
  check('skills 为 undefined 不炸且仍给入口', noSk.items.some((c) => c.kind === 'skillpick'));
}

console.log('推荐行 · 随对话状态变化');
{
  const auto0 = chipPlan({ skills: SKILLS, mode: 'auto', picked: null, turns: 0 });
  check('空态给得出推荐动作', auto0.items.length > 0);
  check('空态不依赖技能库也有两条通用问话（seed 没跑完也能用）',
    auto0.items.filter((c) => c.kind === 'ask').length >= 2, JSON.stringify(auto0.items.map((c) => c.label)));
  check('空态抬头是"试试"', auto0.hint === CHIP_HINT.auto);

  const asking = chipPlan({ skills: SKILLS, mode: 'auto', picked: null, turns: 2, askedBack: true });
  check('模型在追问时，第一条就是"接着写"（当下唯一能推进的事）',
    asking.items[0].kind === 'ask' && /按你的思路/.test(asking.items[0].send), JSON.stringify(asking.items[0]));

  const withFile = chipPlan({ skills: SKILLS, mode: 'auto', picked: null, turns: 2, hasFile: true });
  check('已有产出后给出"再精简一半"', withFile.items.some((c) => /精简|压缩/.test(c.send)));

  const used = chipPlan({ skills: SKILLS, mode: 'auto', picked: null, turns: 2, lastSkill: '采购合同' });
  const again = used.items.find((c) => c.kind === 'again');
  check('上一轮用过技能 → 给出"继续用它"', !!again && again.slug === '采购合同');

  for (const st of [auto0, asking, withFile, used]) {
    check('每种状态都不超过 4 颗', st.items.length <= 4, 'len=' + st.items.length);
    check('每颗都有可点的 label', st.items.every((c) => !!c.label));
  }
}

console.log('推荐行 · 点击真的接到动作');
{
  // ask：点一下把话**填进输入框**，不直接发 —— 用户要先能改两个字（比如"200字"改"500字"）。
  // 旧契约是点了就发；那等于逼用户"要么接受这句，要么等它写完再让它重写"。
  {
    const { runChip, log } = makeRunChip([]);
    runChip({ kind: 'ask', label: '再短一点', send: '把上面的内容再压缩一些' });
    check('★ 点 ask → 只填入，不提交',
      log.join(',') === 'input:把上面的内容再压缩一些,toEnd:把上面的内容再压缩一些,focus', log.join(','));
    check('★ 点 ask 不会误发（日志里没有 submit）', !log.includes('submit'), log.join(','));
  }
  // 已有草稿也照样填：点胶囊是明确动作，用户点的就是"换成这句"。
  // （旧行为是"草稿优先、点了没反应" —— 这正是用户抱怨的那种"点了跟没点一样"。）
  {
    const { runChip, log } = makeRunChip([], { inputValue: '我自己写的' });
    runChip({ kind: 'ask', label: '再短一点', send: '压缩' });
    check('★ 有草稿时也照填（点了必须有反应）',
      log.join(',') === 'input:压缩,toEnd:压缩,focus', log.join(','));
  }
  // pick：只选不发送 —— 选完还要让用户自己说材料
  {
    const { runChip, log } = makeRunChip([], { skills: SKILLS });
    runChip({ kind: 'pick', slug: '采购合同' });
    check('点 pick → 指定技能且不提交', log.join(',') === 'pick:采购合同,focus', log.join(','));
  }
  {
    const { runChip, log } = makeRunChip([], { skills: SKILLS });
    runChip({ kind: 'unpick' });
    check('点 unpick → 取消指定且不提交', log.join(',') === 'unpick,focus', log.join(','));
  }
  // skillpick：入口那颗是"开面板"的开关。绝不能落到 ask 分支 ——
  // 落下去就会把「选择技能 ▾」四个字填进输入框然后当成用户的话发出去。
  {
    const { runChip, log } = makeRunChip([], { skills: SKILLS });
    runChip({ kind: 'skillpick', label: '选择技能 ▾' });
    check('★ 点入口 → 开/关勾选层，不填输入框、不提交',
      log.join(',') === 'layer:toggle', log.join(','));
  }
  // again：切回手动档并锁定那个技能（否则"继续用它"只改了界面文案）
  {
    const { runChip, log } = makeRunChip([], { skills: SKILLS });
    runChip({ kind: 'again', slug: '采购合同' });
    check('点"继续用这个技能" → 指定 + 切手动档', log.join(',') === 'pick:采购合同,mode:manual,focus', log.join(','));
  }
  // 技能已被删/停用（索引里找不到）也要能继续用 —— 后端有降级说明，前端不能卡住
  {
    const { runChip, log } = makeRunChip([], { skills: [] });
    runChip({ kind: 'again', slug: '已经删掉的技能' });
    check('技能已失效时仍能锁定（后端给降级说明）', /^pick:已经删掉的技能,mode:manual/.test(log.join(',')), log.join(','));
  }
  // 空 chip 不能炸
  {
    const { runChip, log } = makeRunChip([]);
    runChip(null); runChip({});
    check('空 chip 不炸（点了也不该有副作用）', log.length === 0, log.join(','));
  }
}

console.log('技能勾选层 · 纯函数（不点界面也能验证）');
{
  // 这一节是"指定技能"真正能用的地基：技能库有 N 个，面板必须不全漏、搜索必须真过滤。
  // 全是纯函数（状态进、清单出、不碰 DOM）—— 所以断言可以直接喂清单，不用人肉点界面。
  const orderSkills = bind(CHAT_JS, 'function orderSkills(');
  const skillMatches = bind(CHAT_JS, 'function skillMatches(');
  const filterSkills = bind(CHAT_JS, 'function filterSkills(', ['orderSkills', 'skillMatches'], [orderSkills, skillMatches]);
  const togglePick = bind(CHAT_JS, 'function togglePick(');

  // 核心技能（通用能力）必须排最前：用户找"办公文档管家"的频率远高于找某个业务技能
  const shuffled = [SKILLS[2], SKILLS[1], SKILLS[3], SKILLS[0]];
  const ordered = orderSkills(shuffled);
  check('核心技能排在业务技能前面（乱序输入也分对先后）',
    ordered[0].slug !== '采购合同' && ordered[1].slug !== '采购合同', ordered.map((s) => s.slug).join('|'));
  check('排序不丢技能（只是换顺序）', ordered.length === shuffled.length);
  check('orderSkills 不改原数组（纯函数）', shuffled[0].slug === '采购合同', shuffled.map((s) => s.slug).join('|'));

  // ★ 这次 bug 的核心：面板必须能列出**全部**技能。旧版推荐行只铺 4 颗，
  //   第 5 个开始的技能用户永远点不到。所以用 13 个的清单钉住"一个都不能少"。
  const many = Array.from({ length: 13 }, (_, i) => ({ slug: 'sk-' + i, name: '技能' + i, is_core: i === 12 ? 1 : 0 }));
  check('★ 面板列全（13 个技能一个不少，不是只有 4 个）', filterSkills(many, '').length === 13,
    'len=' + filterSkills(many, '').length);
  check('★ 第 13 个技能在面板里（旧版正好卡在第 4 个之后）',
    filterSkills(many, '').some((s) => s.slug === 'sk-12'));

  // 搜索：名字 / 说明 / slug 三个字段都要能搜到 —— 用户想的是"合同"，名字里不一定有
  check('搜名字命中', skillMatches(SKILLS[2], '采购'));
  check('搜说明命中（关键词常在 description 里）', skillMatches(SKILLS[0], 'Word'));
  check('搜 slug 命中', skillMatches(SKILLS[1], '工厂'));
  check('大小写不敏感', skillMatches(SKILLS[0], 'word'));
  check('首尾空格不影响', skillMatches(SKILLS[2], '  采购  '));
  check('过滤真的过滤（不是全返回）', filterSkills(SKILLS, '采购').length === 1,
    'len=' + filterSkills(SKILLS, '采购').length);
  check('没命中的关键词返回空（界面据此说"没找到"）', filterSkills(SKILLS, '不存在的词').length === 0);
  check('空查询返回全部（不是空列表）', filterSkills(SKILLS, '').length === SKILLS.length);
  check('undefined 技能库不炸', filterSkills(undefined, '').length === 0 && filterSkills(null, 'x').length === 0);
  check('技能对象缺字段不炸', filterSkills([{ slug: 'a' }], 'a').length === 1);

  // 勾选语义 = 单选 + 可取消。后端只吃一个 skill 字段，多选就是"给了不兑现的承诺"。
  check('没选过 → 勾上', togglePick('', '采购合同') === '采购合同');
  check('★ 再勾一次同一颗 → 取消（否则没法取消指定）', togglePick('采购合同', '采购合同') === '');
  check('★ 勾另一颗 → 换过去（单选的本质：不可能同时选中两个）', togglePick('采购合同', '技能工厂') === '技能工厂');
  check('空 slug 不改变现状（脏数据不清空已选）', togglePick('采购合同', '') === '采购合同');
}

console.log('技能勾选层 · 接线（层会不会被清掉 / 关掉）');
{
  // 结构断言。这些点踩过一次就会"点了没反应"，属于必须钉住的接线。
  check('index.html 里有层容器 #sk-layer', INDEX_HTML.includes('id="sk-layer"'));
  check('index.html 里有搜索框 #sk-q', INDEX_HTML.includes('id="sk-q"'));
  check('index.html 里有列表 #sk-list', INDEX_HTML.includes('id="sk-list"'));
  // ★ 层必须在 #chips 外面：renderChips() 每次清空 #chips 的 innerHTML，
  //   层放进去会被连带清掉 → 用户看到面板一闪就没了。
  const iLayer = INDEX_HTML.indexOf('id="sk-layer"');
  const iChips = INDEX_HTML.indexOf('id="chips"');
  check('★ 层在 #chips 之前且不在其中（不然被 renderChips 清掉）',
    iLayer > 0 && iChips > 0 && iLayer < iChips, `layer@${iLayer} chips@${iChips}`);
  check('层默认是 hidden（刷新页面不能自己弹出来）', /id="sk-layer"[^>]*hidden/.test(INDEX_HTML));

  // ★ CSS 必须显式处理 [hidden]：.ch-sklayer 用了 display:flex，
  //   会盖掉浏览器默认的 [hidden]{display:none} → JS 设了 hidden 也关不掉。
  check('★ style.css 有 .ch-sklayer[hidden]{display:none}（否则关不掉）',
    /\.ch-sklayer\[hidden\]\s*\{[^}]*display:\s*none/.test(STYLE_CSS));
  for (const sel of ['.ch-sklayer', '.ch-sklayer-q', '.ch-skrow', '.ch-skbox', '.ch-skname', '.ch-skdesc']) {
    check(`style.css 有 ${sel}`, STYLE_CSS.includes(sel));
  }
  check('层有最大高度（技能多了不把整页顶开）', /\.ch-sklayer\s*\{[\s\S]*?max-height/.test(STYLE_CSS));
  check('列表可滚动', /\.ch-sklayer-list\s*\{[^}]*overflow-y:\s*auto/.test(STYLE_CSS));
  check('层的选中态跟输入框胶囊一致（同一套会话状态，视觉不能各说各话）',
    /\.ch-skrow\.is-on/.test(STYLE_CSS));

  // chat.js 接线
  check('每次打开清空搜索词（不然用户以为技能库里只剩上次搜的）',
    /function openSkLayer[\s\S]{0,600}?skQ\.value = ''/.test(CHAT_JS) || /if \(skQ && skQ\.value\) skQ\.value = ''/.test(CHAT_JS));
  check('打开后聚焦搜索框（打开即可打字）', /function openSkLayer[\s\S]{0,700}?skQ\.focus\(\)/.test(CHAT_JS));
  // ★ 点外面关层时必须排除推荐行和输入栏：手动档点发送会主动开层，
  //   文档级监听在冒泡末端跑，不排除就会把刚开的层当场关掉 → "点了发送毫无反应"。
  // ★ 断言必须锚在**代码语句**上（`if (... ) return;`），不能只比对 `chipsWrap.contains(t)`
  //   这种片段 —— 注释里写一遍同样的字样，断言就被注释喂饱了，代码删掉照样绿。
  //   这个假绿真的发生过（chat_ui_mutation_check.py 注入 11 抓到），记着。
  check('★ 点外关闭排除了推荐行（chipsWrap）',
    /if \(chipsWrap && chipsWrap\.contains\(t\)\) return;/.test(CHAT_JS));
  check('★ 点外关闭排除了输入栏（inputBar）',
    /if \(inputBar && inputBar\.contains\(t\)\) return;/.test(CHAT_JS));
  check('点外关闭会真的关层', /document\.addEventListener\('click'[\s\S]{0,400}?closeSkLayer\(\)/.test(CHAT_JS));
  // ★ 点外关闭必须在**捕获阶段**，且要放行游离节点。
  //   线上实测翻车（2026-09-14）：点「选择技能 ▾」→ openSkLayer() 里 renderChips()
  //   重建整行，被点的那颗 button 当场变游离节点 → 冒泡到 document 时
  //   chipsWrap.contains(t) === false，白名单形同虚设 → 层刚 hidden=false 又被自己
  //   关回 true。用户看到"点一下闪一下就没了"，而静态断言（白名单写着呢）全绿。
  //   捕获阶段在事件往下走时判归属，那时 DOM 还没被重渲染。真 DOM 兜底见
  //   web/tests/chat_layer_e2e.py —— 这条改动必须两边都有证据。
  //
  // ★★ 2026-09-19 抓到这两条断言本身长成假绿（chat_ui_mutation_check.py 注入
  //   14/15 报「改坏了还是绿的」）：原来的写法一条是「取文件里**第一个**文档级
  //   click 监听器再找 }, true)」，一条是「全文搜 isConnected 那一行」。加了 MCP
  //   那一层之后，它俩同形的监听器排在前面 —— 先加的那层把断言喂饱，后加的那层
  //   怎么坏都不响。教训：**按标记认人是断言的义务**，"第几个 / 全文有没有"这类
  //   判据会被后来者静默顶替。现在改成按层抽出**那个真的会关层的监听器**再验形状。
  const skClick = extractClickListener(CHAT_JS, 'closeSkLayer()');
  const mcpClick = extractClickListener(CHAT_JS, 'closeMCPLayer()');
  check('★ 技能层点外关闭用捕获阶段（冒泡阶段会因重渲染丢白名单，层自己关掉自己）',
    skClick.length === 1 && skClick[0].capture, `命中 ${skClick.length} 个监听器`);
  check('★ 技能层点外关闭放行游离节点（isConnected 兜底）',
    skClick.length === 1 && /if \(t && t\.isConnected === false\) return;/.test(skClick[0].body));
  check('★ MCP 层点外关闭用捕获阶段（同一类故障：层内按钮点了会重渲染）',
    mcpClick.length === 1 && mcpClick[0].capture, `命中 ${mcpClick.length} 个监听器`);
  check('★ MCP 层点外关闭放行游离节点（isConnected 兜底）',
    mcpClick.length === 1 && /if \(t && t\.isConnected === false\) return;/.test(mcpClick[0].body));
  check('Esc 能关层', /Escape[\s\S]{0,200}?closeSkLayer\(\)/.test(CHAT_JS));
  check('搜索框回车直接勾第一个（搜到就回车是最快路径）',
    /skQ\.addEventListener\('keydown'[\s\S]{0,400}?filterSkills\(allSkills, skQ\.value\)\[0\]/.test(CHAT_JS));
  // ★ 手动档没选技能点发送 → 除了抖，还要把层打开（否则用户还是不知道去哪选）
  check('★ 发送被拦时打开勾选层', /sendBlocked\(chatMode, pickedSkill\)[\s\S]{0,300}?openSkLayer\(\)/.test(CHAT_JS));
  check('切回自动档时收掉层', /chatMode === 'auto'\)\s*closeSkLayer\(\)/.test(CHAT_JS));
  check('发出去之后收掉层', /input\.value = ''[\s\S]{0,120}?closeSkLayer\(\)/.test(CHAT_JS));
  check('技能库到货时补渲染层（用户手快先开了层）', /if \(skOpen\) renderSkLayer\(\)/.test(CHAT_JS));
  check('层有页脚说明"勾了会怎样"（不写就像个多选过滤器）', /skFoot\.textContent/.test(CHAT_JS));
  check('层里没找到技能时给独立文案（跟"没加载完"区分开）', /没有匹配/.test(CHAT_JS));
}

console.log('推荐行 · LLM 精修是"可失败的旁路"');
{
  // 规则版必须先渲染出来（用户零等待），精修只是替换。
  // 一批最容易踩的坑：接口不存在/超时/解析失败时把推荐行清空 —— 那还不如不做。
  check('打到 POST /api/chat/suggest', /fetch\('\/api\/chat\/suggest'/.test(CHAT_JS));
  // 只断言「有超时」+「是个像样的数」。**具体值不在这里钉死**：
  // 这个数必须和后端 ctx 一起看（前端 abort 要比后端长，否则服务端白算），
  // 钉两个文件里的两个常量正是它上次烂掉的原因 —— 一端改了另一端没人提醒。
  // 值的正确性归 web/tests/suggest_timeout_budget.test.mjs（跨文件比对那一对）。
  check('带超时（不让推荐行等一个慢模型）', /setTimeout\([\s\S]{0,80}?ctl\.abort\(\)[\s\S]{0,20}?\b(\d{4,5})\)/.test(CHAT_JS));
  check('超时值不是一个随便的小数（≥8s，要容得下后端 3 次尝试）',
    Number((CHAT_JS.match(/ctl\.abort\(\)[\s\S]{0,20}?(\d{4,5})\)/) || [])[1] || 0) >= 8000);
  const refine = extractFn(CHAT_JS, 'function refineChips(');
  check('抽到了 refineChips', !!refine);
  check('精修只在自动档、且没指定技能时跑', /chatMode !== 'auto' \|\| pickedSkill/.test(refine || ''));
  check('★ 非 2xx 走 reject（不把错误当结果渲染）', /r\.ok \? r\.json\(\) : Promise\.reject\(\)/.test(refine || ''));
  check('★ 模型没给建议时保留规则版（不清空）', /if \(!list\.length\) return;/.test(refine || ''));
  check('★ 失败兜底不抛到控制台外（catch 里不渲染）', /\.catch\(\(\) => \{ clearTimeout\(timer\); \}\)/.test(refine || ''));
  check('过期响应被丢弃（连发几轮时不覆盖新界面）', /token !== chipToken/.test(refine || ''));
  check('⚠️ 先渲染规则版再精修（顺序反了用户就要干等）',
    /renderChips\(\)[\s\S]{0,120}refineChips\(\)/.test(extractFn(CHAT_JS, 'function submit(') || '')
    || /renderChips\(\);?\s*\n\s*refineChips\(\);/.test(CHAT_JS));
}

console.log('聊天界面 · 单胶囊双档');
{
  for (const id of ['ch-switch', 'ch-switch-thumb', 'mode-auto', 'mode-manual', 'chips', 'chat-input', 'chat-send']) {
    check(`index.html 有 #${id}`, INDEX_HTML.includes('id="' + id + '"'));
  }
  check('两种方式都有文案', INDEX_HTML.includes('自动调度') && INDEX_HTML.includes('指定技能'));
  // ⚠️ 不能假设属性顺序：HTML 里 class 写在 id 前面，按 id 在前拼正则会恒假红。
  const tagOf = (id) => (INDEX_HTML.match(new RegExp('<button[^>]*id="' + id + '"[^>]*>')) || [''])[0];
  const autoTag = tagOf('mode-auto'), manualTag = tagOf('mode-manual');
  check('两档按钮都在', !!autoTag && !!manualTag);
  check('默认档=自动调度', /\bis-on\b/.test(autoTag));
  check('另一档默认不亮', manualTag && !/\bis-on\b/.test(manualTag));
  check('两档默认 aria-selected 与视觉一致',
    /aria-selected="true"/.test(autoTag) && /aria-selected="false"/.test(manualTag));

  // ★ 选中态漏网之鱼（补防）：上面几条只断言了**静态 HTML 的初始态**，
  // 而 JS 切换时 toggle 的是另一个类名（'on'），CSS 认的却是 'is-on' →
  // 滑块照滑、aria 照改，文字选中态一动不动，HTML 预置的 is-on 谁也摘不掉，
  // 界面就永远像停在「自动调度」上：不报错、控制台干净，只有人眼能发现。
  // 断言不看注释怎么写，类名两侧都从出货文件里读真值 —— 改一边不改另一边即红。
  {
    const mm = CHAT_JS.match(/const ON_CLASS\s*=\s*'([^']+)'/);
    const ON = mm ? mm[1] : '';
    check('chat.js 的选中类名是具名常量（不是散落字面量）', !!ON, String(mm));
    check('★ 该类名在 style.css 里有对应选择器',
      !!ON && STYLE_CSS.includes('.ch-switch-opt.' + ON), 'ON=' + ON);
    const clsOf = (tag) => ((tag.match(/class="([^"]*)"/) || [])[1] || '').split(/\s+/).filter(Boolean);
    const autoCls = clsOf(autoTag), manCls = clsOf(manualTag);
    check('静态初始态：自动档只带「基础类+选中类」两个 token',
      autoCls.length === 2 && autoCls.includes('ch-switch-opt') && autoCls.includes(ON), autoCls.join(' '));
    check('静态初始态：另一档不带选中类',
      manCls.length === 1 && manCls[0] === 'ch-switch-opt', manCls.join(' '));

    // 跑真函数而不是抄一份：两档各切一次，看类名跟不跟着走、旧档摘没摘干净
    const mkBtn = () => {
      const set = new Set();
      return { set, attrs: {},
        classList: { toggle: (c, on) => { if (on) set.add(c); else set.delete(c); } },
        setAttribute(k, v) { this.attrs[k] = v; } };
    };
    const run = (mode) => {
      const A = mkBtn(), M = mkBtn();
      bind(CHAT_JS, 'function paintMode(', ['ON_CLASS'], [ON])(mode, A, M);
      return { A, M };
    };
    const ra = run('auto');
    check('自动档：选中档带上 CSS 类', ra.A.set.has(ON));
    check('自动档：另一档不亮', !ra.M.set.has(ON));
    const rm = run('manual');
    check('指定技能档：选中档带上 CSS 类', rm.M.set.has(ON));
    check('★ 切档后旧档选中态被摘掉（否则两档同时亮）', !rm.A.set.has(ON));
    check('aria-selected 跟着视觉走（自动档）',
      ra.A.attrs['aria-selected'] === 'true' && ra.M.attrs['aria-selected'] === 'false');
    check('aria-selected 跟着视觉走（指定技能档）',
      rm.M.attrs['aria-selected'] === 'true' && rm.A.attrs['aria-selected'] === 'false');

    // 视觉同步只能有一处：setMode 必须调 paintMode，不能再悄悄 toggle 别的类名
    const sm = extractFn(CHAT_JS, 'function setMode(');
    check('setMode 走 paintMode', /paintMode\(chatMode, modeAuto, modeManual\)/.test(sm || ''));
    check('setMode 里不再直接 toggle 别的类名', !!sm && !/classList\.toggle\('on'/.test(sm));
  }

  // 滑块几何必须实测：写死 50% 在字体回退/窄屏下会错位（滑块压住文字/留半格）
  check('滑块宽度用实测 offsetWidth', /thumb\.style\.width = [^;]*offsetWidth/.test(CHAT_JS));
  check('滑块位移用实测 offsetLeft', /translateX\([^)]*offsetLeft/.test(CHAT_JS));
  check('宽度为 0 时不动（字体未就绪不写死错值）', /if \(!thumb \|\| !modeAuto \|\| !modeAuto\.offsetWidth\) return;/.test(CHAT_JS));
  // 直接跑：两档宽度不同 → 位移应等于两按钮左缘之差，宽度应等于当前档按钮宽度
  {
    const mk = (w, left) => ({ offsetWidth: w, offsetLeft: left, style: {} });
    const A = mk(84, 3), M = mk(90, 87);
    const thumbStub = { style: {} };
    const run = (mode) => new Function('chatMode', 'thumb', 'modeAuto', 'modeManual',
      'return ' + extractFn(CHAT_JS, 'function syncThumb('))(mode, thumbStub, A, M)();
    // ⚠️ 上面那两个括号不是手滑：new Function(...) 拿到的是「返回该函数的工厂」，
    // 少调一次就等于把 syncThumb 当无参函数跑掉，样式对象永远空 —— 看着像功能坏了。
    run('auto');
    check('自动档：滑块宽=该档宽、位移=0', thumbStub.style.width === '84px' && thumbStub.style.transform === 'translateX(0px)',
      JSON.stringify(thumbStub.style));
    run('manual');
    check('手动档：滑块宽=该档宽、位移=两档左缘之差',
      thumbStub.style.width === '90px' && thumbStub.style.transform === 'translateX(84px)',
      JSON.stringify(thumbStub.style));
  }
  // role=tablist 的可达性：方向键也要能切档
  check('键盘左右方向键能切档', /ArrowLeft[\s\S]{0,80}ArrowRight/.test(CHAT_JS));

  // 样式必须真的存在：丢了 CSS 会在输入框里变成两坨裸文字
  for (const sel of ['.ch-switch', '.ch-switch-thumb', '.ch-switch-opt', '.ch-switch-opt.is-on',
    '.ch-chips', '.ch-chips-h', '.ch-chips.need', '.ch-sug.is-pick']) {
    check(`style.css 有 ${sel}`, STYLE_CSS.includes(sel));
  }
  check('滑块有位移过渡（不是瞬移）', /\.ch-switch-thumb\s*\{[\s\S]*?transition:[^;]*transform/.test(STYLE_CSS));
  // 抖动动画真存在：JS 挂 .need，CSS 里如果没有 keyframes 就是白挂
  check('抖动 keyframes 存在', /@keyframes ch-need/.test(STYLE_CSS));
  check('抖动有次数上限（不无限抖）', /\.ch-chips\.need\s*\{[^}]*animation:[^;]*2/.test(STYLE_CSS));
  check('推荐行有最小高度占位（内容增减时输入框不上下跳）', /\.ch-chips\s*\{[\s\S]*?min-height/.test(STYLE_CSS));
  check('本页资源带缓存版本号', /\?v=20\d{6}[A-Z]/.test(INDEX_HTML));
}

console.log('聊天界面 · 极简（砍掉的入口不许偷偷回来）');
{
  // 用户的原话是"一堆可点击的，太复杂了"。这一节就是防止哪天又顺手加回来。
  for (const dead of ['skill-panel', 'skill-search', 'skill-pick-btn', 'ch-footnote', 'ch-w-sub', 'ch-w-eg', 'ch-modes']) {
    check(`index.html 不再有 ${dead}`, !INDEX_HTML.includes(dead));
  }
  for (const dead of ['skPanelHTML', 'skItemHtml', 'openSkPanel', 'panelClosesOnClick', 'renderSug', 'skillChoicesHTML', 'FOOTNOTE']) {
    check(`chat.js 不再有死代码 ${dead}`, !CHAT_JS.includes(dead));
  }
  // 空态只留一句话：欢迎块里不许再有可点元素
  const w = (INDEX_HTML.match(/<div class="ch-welcome"[\s\S]*?<\/div>/) || [''])[0];
  check('空态块内没有 button/a（点不到东西）', w && !/<button|<a\s/.test(w), w);
  check('空态保留一句欢迎语', /想写点什么/.test(w));
}

console.log('聊天方式 · 技能失效降级说明');
{
  const noteLine = bind(CHAT_JS, 'function metaNoteLine(');
  const degraded = { mode: 'auto', note: '指定的技能「公司新闻通稿」不可用，已改用自动调度' };
  check('有 note → 渲染成引用行', noteLine(degraded) === '> ' + degraded.note + '\n\n');
  check('没 note → 什么都不加', noteLine({ mode: 'auto' }) === '');
  check('空对象/undefined 不炸', noteLine(undefined) === '' && noteLine({}) === '');
  // 这句话必须落在"该轮回复"的渲染路径上，且不能被轨迹面板的存在与否连带。
  // 曾经它挂在 if (trace) 里面：轨迹面板一旦没渲染出来，降级说明就被静默吞掉，
  // 用户只看到"结果不对"，永远不知道是技能被删了。
  // ⚠️ 锚点要整段锚：早先写 indexOf('metaNoteLine(obj)') 会先命中函数定义
  // 「function metaNoteLine(obj) {」，定义在 case meta 之前 → 断言恒假红。
  // 定 call site 的整段形状才是"真的在这一段里被调用"。
  const iNote = CHAT_JS.indexOf('appendText(bubble, metaNoteLine(obj))');
  const iMeta = CHAT_JS.indexOf("case 'meta'");
  const iTraceGuard = CHAT_JS.indexOf('if (trace) {', iMeta);
  check('case meta 里调用了 metaNoteLine', iNote > 0);
  check('降级说明在 case meta 的渲染路径上', iMeta > 0 && iNote > iMeta);
  // 光比"在同一段里"不够：把它塞进下面那个 if (trace) 里也满足顺序关系，
  // 但那样就退回成"轨迹面板不在=这句话被吞"。所以必须证明它排在守卫之前。
  check('降级说明排在 if (trace) 守卫之前（面板缺失也不会被吞）',
    iTraceGuard > 0 && iNote < iTraceGuard);
  // 只比顺序还不够：往里再补一行重复调用也满足顺序，但那时"那一行"到底在守卫内
  // 还是守卫外就变得看运气。渲染这句话的地方只能有一处，且必须是守卫外那一处。
  // ⚠️ 数的是整段调用 appendText(bubble, metaNoteLine(obj))，不是 metaNoteLine(obj))
  // —— 后者在那一行里天然出现两次（if 判 + 调用），计数从 2 起，断言会恒红。
  check('降级说明只在一处渲染（没有第二处可以悄悄生效）',
    CHAT_JS.split('appendText(bubble, metaNoteLine(obj))').length - 1 === 1);
  check('不再直接用 obj.note 拼引用行（免得又挪回 trace 里面）',
    !CHAT_JS.includes('if (obj.note) appendText(bubble'));
}

if (failures) { console.log(`\n${failures} 项失败`); process.exit(1); }
console.log('\n全部通过');
