#!/usr/bin/env node
// 「聊天两种方式」的前端回归防线 —— 零依赖，CI 里直接 node 跑。
//
// 守的是一类**坏了界面不报错、用户也说不清哪里不对**的坑：
//
//   Bug U — 手动选了技能，发出去却退化成自动调度（mode/skill 漏发或在自动模式下
//           残留了上一次选的技能）。表现是"我明明锁定了采购合同，它却给我写了篇通稿"，
//           聊天里没有任何报错，最难查。
//   Bug V — 技能面板不分组：核心技能（通用能力）混在业务技能里，用户每次都要
//           从头找"办公文档管家"。
//   Bug W — 手动模式没选技能就直接发出去，后端替你猜一个 —— 那就等于没有"指定技能"。
//
// 做法：**从出货文件里抠出真正跑的那个函数来跑**（web/js/chat.js 里的
// chatPayload / sendBlocked / skPanelHTML / skItemHtml），而不是在测试里抄一份实现。
// 抄一份的话，改坏了真文件测试照样绿。

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

// ---------- 真实的依赖（从出货文件里抠，避免测试自带一份"理想实现"）----------
const esc = bind(CHAT_JS, 'function esc(');
const TYPE_META = { write: { tag: '写作' }, docgen: { tag: '文档' }, template: { tag: '模板' }, query: { tag: '查询' } };
const cardDesc = (sk, tag) => sk.description || tag;

const skItemHtml = bind(CHAT_JS, 'function skItemHtml(', ['TYPE_META', 'esc', 'cardDesc'], [TYPE_META, esc, cardDesc]);
const skPanelHTML = bind(CHAT_JS, 'function skPanelHTML(', ['TYPE_META', 'esc', 'cardDesc', 'skItemHtml'], [TYPE_META, esc, cardDesc, skItemHtml]);
// chatPayload 依赖三个闭包状态（sessionId/chatMode/pickedSkill），这里做成柯里化调用：
//   chatPayload('s1','manual',skill)('要发的话')
const chatPayload = (sid, mode, picked) =>
  bind(CHAT_JS, 'function chatPayload(', ['sessionId', 'chatMode', 'pickedSkill'], [sid, mode, picked]);
const sendBlocked = bind(CHAT_JS, 'function sendBlocked(');
const panelClosesOnClick = bind(CHAT_JS, 'function panelClosesOnClick(');

// 从出货文件里抠出配置表（FOOTNOTE / PLACEHOLDER / HERO_SUB / HERO_EG），
// 而不是在测试里另抄一份文案 —— 抄一份的话，改了真文件测试照样绿。
function extractObj(src, name) {
  const m = src.match(new RegExp('const\\s+' + name + '\\s*=\\s*\\{[\\s\\S]*?\\n\\s*\\};'));
  if (!m) throw new Error('抽不到配置表：' + name);
  // 注意：不能写成 'return ' + 定义 —— return 后面接不了 const/var 声明。
  // 要让声明留在函数体里，再单独 return 出来。
  return new Function(m[0] + '\nreturn ' + name + ';')();
}
const FOOTNOTE = extractObj(CHAT_JS, 'FOOTNOTE');
const PLACEHOLDER = extractObj(CHAT_JS, 'PLACEHOLDER');
const HERO_SUB = extractObj(CHAT_JS, 'HERO_SUB');
const HERO_EG = extractObj(CHAT_JS, 'HERO_EG');

const SKILLS = [
  { slug: '办公文档管家', name: '办公文档管家', description: '生成或填写 Word/Excel/PDF/PPT', skill_type: 'docgen', is_core: 1 },
  { slug: '技能工厂', name: '技能工厂', description: '按需求产出一份可落地的技能定义', skill_type: 'write', is_core: 1 },
  { slug: '采购合同', name: '采购合同', description: '按模板生成采购合同', skill_type: 'template', is_core: 0 },
  { slug: '公司新闻通稿', name: '公司新闻通稿', description: '写公司新闻通稿', skill_type: 'write', is_core: 0 },
];

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
}

console.log('聊天方式 · 面板与提示跟随模式（Bug X）');
{
  // Bug X —— 手动没选技能点发送：submit() 会 openSkPanel 提示补选，
  // 但这次点击继续冒泡到 document 的"点外面就收起"监听上，面板刚开就被关掉。
  // 界面上表现为「点了发送毫无反应」，不报错、控制台干净，只能靠真浏览器复现。
  const opts = (o) => Object.assign(
    { panelHidden: false, inPanel: false, inPickBtn: false, inSend: false, inInput: false }, o);
  check('点发送键不收起面板', panelClosesOnClick(opts({ inSend: true })) === false);
  check('点输入框不收起面板', panelClosesOnClick(opts({ inInput: true })) === false);
  check('点面板内不收起面板', panelClosesOnClick(opts({ inPanel: true })) === false);
  check('点技能钮不收起面板', panelClosesOnClick(opts({ inPickBtn: true })) === false);
  check('点真正的空白处才收起', panelClosesOnClick(opts()) === true);
  check('面板本就没开时不动', panelClosesOnClick(opts({ panelHidden: true })) === false);

  // 三处提示文案都得跟模式走：只改其中一处，用户会看到自相矛盾的界面
  //（tab 写着"指定技能"，欢迎语却说"我帮你挑技能"）。
  for (const t of [FOOTNOTE, PLACEHOLDER, HERO_SUB, HERO_EG]) {
    check('文案两档都有', !!t.auto && !!t.manual);
    check('文案两档不同', t.auto !== t.manual);
  }
  // 自动档说"我来调度"，手动档不能再说"我帮你挑" —— 技能是用户自己锁的
  check('自动档欢迎语提调度', /调度|挑/.test(HERO_SUB.auto));
  check('手动档欢迎语不提"我来挑"', !/调度|我来挑/.test(HERO_SUB.manual));
  check('手动档欢迎语要求先指定技能', /指定|先选|锁定/.test(HERO_SUB.manual));
  check('手动档占位符要求写材料', /材料/.test(PLACEHOLDER.manual));

  // 欢迎语元素必须真的存在于出货 HTML 里，否则 setMode 里改了也没人看
  check('index.html 有 #ch-w-sub', INDEX_HTML.includes('id="ch-w-sub"'));
  check('index.html 有 #ch-w-eg', INDEX_HTML.includes('id="ch-w-eg"'));
  // 光有文案表和元素还不够 —— setMode 必须真的把它们接上。
  // 漏接的表现是：切到手动档，脚注变了、占位符变了，欢迎语还写着"我来挑技能"。
  check('setMode 接了 heroSub', /heroSub\.textContent\s*=\s*HERO_SUB\[chatMode\]/.test(CHAT_JS));
  check('setMode 接了 heroEg', /heroEg\.textContent\s*=\s*HERO_EG\[chatMode\]/.test(CHAT_JS));
  check('取到了欢迎语元素', /heroSub\s*=\s*\$\('#ch-w-sub'\)/.test(CHAT_JS));
}

console.log('技能面板 · 分组（Bug V）');
{
  const html = skPanelHTML('', null, SKILLS);
  const iCore = html.indexOf('核心技能');
  const iBiz = html.indexOf('业务技能');
  check('有核心技能分组', iCore >= 0);
  check('有业务技能分组', iBiz >= 0);
  check('核心分组排在业务分组前面', iCore >= 0 && iBiz > iCore, `core@${iCore} biz@${iBiz}`);
  // 两个核心技能都必须在核心组里（而不是靠名字里有没有"通用"两字）
  const head = html.slice(0, iBiz);
  check('办公文档管家在核心组', head.includes('办公文档管家'));
  check('技能工厂在核心组', head.includes('技能工厂'));
  const tail = html.slice(iBiz);
  check('采购合同在业务组', tail.includes('采购合同'));
  check('公司新闻通稿在业务组', tail.includes('公司新闻通稿'));
  check('核心技能带核心角标', (html.match(/class="ch-core">核心</g) || []).length === 2,
    '核心角标数=' + (html.match(/class="ch-core">核心</g) || []).length);
  check('核心组计数为 2', /核心技能 · 通用能力<i>2<\/i>/.test(html));
  check('业务组计数为 2', /业务技能<i>2<\/i>/.test(html));

  // 未选技能时不出现"退路"行；选了技能必须出现（否则用户找不到取消入口）
  check('未选技能无退路行', !html.includes('data-reset'));
  const picked = skPanelHTML('', { slug: '采购合同', name: '采购合同' }, SKILLS);
  check('已选技能出现退路行', picked.includes('data-reset="1"'));
  check('退路行排在核心组之前', picked.indexOf('data-reset') < picked.indexOf('核心技能'));
  check('已选项标记 on', /class="ch-skitem on" data-slug="采购合同"/.test(picked));

  // 搜索必须同时命中名称与描述
  check('搜索命中名称', skPanelHTML('采购', null, SKILLS).includes('采购合同'));
  check('搜索命中描述', skPanelHTML('通稿', null, SKILLS).includes('公司新闻通稿'));
  check('搜索不误命中', !skPanelHTML('采购', null, SKILLS).includes('办公文档管家'));
  check('搜索无结果返回空串', skPanelHTML('不存在的技能', null, SKILLS) === '');

  // 空列表不能炸（后端还没 seed 完 / 全部停用时）
  check('空列表不炸', skPanelHTML('', null, []) === '');
  check('skills 为 undefined 不炸', skPanelHTML('', null, undefined) === '');

  // 排序无关：即使后端把业务技能排在前面，前端也得分对组
  const reversed = skPanelHTML('', null, [SKILLS[2], SKILLS[1], SKILLS[3], SKILLS[0]]);
  check('乱序输入也分对组', reversed.indexOf('技能工厂') < reversed.indexOf('采购合同'));
}

console.log('聊天界面 · 结构与样式');
{
  for (const th of ['mode-auto', 'mode-manual', 'skill-pick-btn', 'skill-panel', 'skill-search', 'ch-footnote']) {
    check(`index.html 有 #${th}`, INDEX_HTML.includes('id="' + th + '"'));
  }
  check('两种方式都有文案', INDEX_HTML.includes('自动调度') && INDEX_HTML.includes('指定技能'));
  check('默认档=自动调度', /id="mode-auto"[^>]*class="ch-mode on"/.test(INDEX_HTML)
    || /class="ch-mode on"[^>]*id="mode-auto"/.test(INDEX_HTML));
  check('未选技能时默认不显示技能钮（自动档）', /id="skill-pick-btn"[^>]*hidden/.test(INDEX_HTML)
    || /hidden[^>]*id="skill-pick-btn"/.test(INDEX_HTML));
  // 样式必须真的存在：丢了 CSS 面板会变成一片裸文字堆在页面里
  for (const sel of ['.ch-modes', '.ch-mode.on', '.ch-skpanel', '.ch-skitem.on', '.ch-core', '.ch-card.is-core']) {
    check(`style.css 有 ${sel}`, STYLE_CSS.includes(sel));
  }
  check('本页资源带缓存版本号', /\?v=20\d{6}[A-Z]/.test(INDEX_HTML));
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
  check('不再直接用 obj.note 拼引用行（免得又挪回 trace 里面）',
    !CHAT_JS.includes('if (obj.note) appendText(bubble'));
}

if (failures) { console.log(`\n${failures} 项失败`); process.exit(1); }
console.log('\n全部通过');
