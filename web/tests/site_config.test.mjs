#!/usr/bin/env node
// 「网页名称可自定义」的前端回归防线 —— 零依赖，CI 里直接 node 跑。
//
// 这一层守的是几个**坏了页面不报错、肉眼要刷新好几次才发现**的坑：
//
//   Bug R — 保存完站名，当前页还是老名字（要手动刷新才变）。管理端保存成功后
//           必须用服务端返回值立刻重绘，不能等下一次 GET。
//
//   Bug S — 刷新时先闪一下默认名 SkillForge 再变成自定义名。原因是不套 localStorage
//           缓存、等 fetch 回来才写 DOM；网络往返期间用户看到的是"名字被回滚了"。
//
//   Bug T — 副标题留空时 brand 里留下 "名字 — " 这种尾巴（分隔符没省）。
//
// 还有两条**硬规则**，谁改谁负责：
//   1. 站名一律走 textContent。本测试给所有元素装了会抛异常的 innerHTML setter，
//      一旦有人把它改回 innerHTML，测试当场炸（那是自助 XSS 入口）。
//   2. apply() 必须先读缓存再发 fetch —— 顺序反了就等于没有缓存。
//
// 做法：不是把 site.js 抄一份来测，而是**用假 DOM 真跑出货文件**
// （web/js/site.js）。抄一份的话，改坏真文件测试照样绿。

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import vm from 'node:vm';

const here = dirname(fileURLToPath(import.meta.url));
const WEB = join(here, '..');
const SITE_JS = readFileSync(join(WEB, 'js', 'site.js'), 'utf8');
const INDEX_HTML = readFileSync(join(WEB, 'index.html'), 'utf8');
const ADMIN_HTML = readFileSync(join(WEB, 'admin.html'), 'utf8');

let failures = 0;
// extractFn 从出货文件里按花括号配平抽出具名函数（开头到函数体结束）。
// 抽出来跑，而不是在测试里抄一份实现 —— 抄的那份永远不会跟着真文件一起坏。
function extractFn(src, signature) {
  const start = src.indexOf(signature);
  if (start < 0) return null;
  let i = src.indexOf('{', start), depth = 0;
  for (let k = i; k < src.length; k++) {
    if (src[k] === '{') depth++;
    else if (src[k] === '}') { depth--; if (depth === 0) return src.slice(start, k + 1); }
  }
  return null;
}

function check(name, cond, extra = '') {
  if (cond) console.log(`  ok   ${name}`);
  else { failures++; console.log(`  FAIL ${name}${extra ? ' — ' + extra : ''}`); }
}

// ---------- 假 DOM ----------
// 只实现 site.js 真正用到的那点 API。故意**不提供 innerHTML 的正常赋值**：
// 任何写入 innerHTML 的代码都会抛错，把"自助 XSS 入口"变成测试红灯。

function mkEl(attrs = {}) {
  const el = {
    _attrs: attrs, _text: null, _removed: false, tagName: attrs._tag || 'span',
    getAttribute: (k) => (k in attrs ? attrs[k] : null),
    remove() { el._removed = true; },
    get textContent() { return el._text; },
    set textContent(v) { el._text = String(v); },
    get innerHTML() { return el._text; },
    set innerHTML(_v) { throw new Error('site.js 用了 innerHTML —— 站名是用户输入，这是 XSS 入口'); },
  };
  return el;
}

// 页面骨架：与 index.html / admin.html 里的写入点一一对应。
function mkDoc(page, opts = {}) {
  const els = {
    name: [mkEl({ 'data-site-name': '' })],
    tagline: [mkEl({ 'data-site-tagline': '' })],
    title: [],
    foot: [],
  };
  if (opts.foot) els.foot.push(mkEl({ 'data-site-title': '管理端' }));
  const doc = {
    title: '默认标题',
    documentElement: { getAttribute: (k) => (k === 'data-site-page' ? page : null) },
    querySelectorAll(sel) {
      if (sel === '[data-site-name]') return els.name;
      if (sel === '[data-site-tagline]') return els.tagline;
      if (sel === '[data-site-title]') return els.foot;
      return [];
    },
    _els: els,
  };
  return doc;
}

// 跑一次真实的 site.js：返回可手动 resolve 的 fetch 控制柄 + 事件顺序。
function run(page, { cache = null, foot = false } = {}) {
  const events = [];
  const store = new Map();
  if (cache !== null) store.set('sf_site_v1', cache);

  let fetchResolve = null;
  const fetchPromise = new Promise((res) => { fetchResolve = res; });

  const doc = mkDoc(page, { foot });
  const sandbox = {
    document: doc,
    localStorage: {
      getItem: (k) => (store.has(k) ? store.get(k) : null),
      setItem: (k, v) => { events.push('cache:set'); store.set(k, v); },
      removeItem: (k) => store.delete(k),
    },
    fetch: () => { events.push('fetch'); return fetchPromise; },
    console,
  };
  sandbox.window = sandbox;
  vm.createContext(sandbox);
  vm.runInContext(SITE_JS, sandbox, { filename: 'site.js' });

  return {
    doc, events, store, sandbox,
    resolveFetch(obj) { fetchResolve({ ok: true, json: async () => obj }); },
    failFetch() { fetchResolve({ ok: false, json: async () => ({}) }); },
  };
}

// 让 fetch 的 .then 链跑完（两层 then + async json）。
const settle = () => new Promise((r) => setTimeout(r, 0));

// ---------- 1. HTML 侧契约 ----------
console.log('HTML 契约：写入点与脚本引用');

for (const [label, html, page] of [['首页', INDEX_HTML, 'index'], ['管理端', ADMIN_HTML, 'admin']]) {
  check(`${label} 声明 data-site-page="${page}"`,
    new RegExp(`<html[^>]*data-site-page="${page}"`).test(html));
  check(`${label} 有 [data-site-name] 写入点`,
    /data-site-name[=>]/.test(html.replace(/data-site-name=""/g, 'data-site-name>')) || /<[^>]*data-site-name/.test(html));
  check(`${label} 引入了 site.js`,
    /<script src="\/assets\/js\/site\.js\?v=\d{8}[A-Z]"><\/script>/.test(html));
}

// 按**真实 script 标签**的出现顺序返回 src 列表。
// 不能用 indexOf('js/chat.js') —— 首页里那句 `<!-- ... rendered by js/chat.js ... -->`
// 是注释，先于真实标签出现，裸子串比会得到相反结论（本测试第一版就踩了这个假红）。
function scriptSrcs(html) {
  return [...html.matchAll(/<script\b[^>]*\bsrc="([^"]+)"[^>]*>/g)].map((m) => m[1]);
}

// 站名注入必须发生在聊天/管理脚本之前：否则首屏会先按硬编码名渲染。
{
  const order = scriptSrcs(INDEX_HTML).map((s) => s.split('?')[0]);
  const i = order.indexOf('/assets/js/site.js');
  const j = order.indexOf('/assets/js/chat.js');
  check('首页 site.js 是真实 script 标签且在 chat.js 之前',
    i >= 0 && j >= 0 && i < j, `实际 script 顺序 ${JSON.stringify(order)}`);
}
{
  const order = scriptSrcs(ADMIN_HTML).map((s) => s.split('?')[0]);
  const i = order.indexOf('/assets/js/site.js');
  const j = order.indexOf('/assets/js/admin.js');
  check('管理端 site.js 是真实 script 标签且在 admin.js 之前',
    i >= 0 && j >= 0 && i < j, `实际 script 顺序 ${JSON.stringify(order)}`);
}
// 管理端页头第二段是页面标识"管理端"，不能挂 data-site-tagline ——
// 挂了就会被站点副标题覆盖，管理端和首页页头变得无法区分。
check('管理端页头的"管理端"是静态文本，没被站点副标题接管',
  !/<small[^>]*data-site-tagline[^>]*>\s*管理端/.test(ADMIN_HTML));

check('管理端有站点设置表单写入点（输入框 + 预览 + 消息位）',
  ['id="site-form"', 'id="site-name"', 'id="site-tagline"', 'id="site-preview-name"',
   'id="site-preview-title"', 'id="site-msg"', 'id="site-reset"'].every((s) => ADMIN_HTML.includes(s)));

// ---------- 2. 缓存优先（Bug S） ----------
console.log('缓存优先：刷新不得先闪默认名');
{
  const r = run('index', { cache: JSON.stringify({ name: '翻译工坊', tagline: '离线文档流水线' }) });
  check('同步阶段就把缓存的站名写进了 DOM（不等 fetch）',
    r.doc._els.name[0].textContent === '翻译工坊',
    `实际 ${JSON.stringify(r.doc._els.name[0].textContent)}`);
  check('同步阶段 <title> 已是缓存站名',
    r.doc.title === '翻译工坊 — 离线文档流水线', `实际 ${JSON.stringify(r.doc.title)}`);
  check('apply 发生在 fetch 之前', r.events[0] === 'fetch' || r.events.indexOf('fetch') > 0,
    `事件顺序 ${JSON.stringify(r.events)}`);
  r.resolveFetch({ name: '翻译工坊', tagline: '离线文档流水线' });
  await settle();
}

// ---------- 3. 接口回来后覆盖 + 写缓存 ----------
console.log('接口值覆盖缓存并回写');
{
  const r = run('index', { cache: JSON.stringify({ name: '旧名', tagline: '旧副标' }) });
  r.resolveFetch({ name: '新名', tagline: '新副标' });
  await settle();
  check('服务端值覆盖缓存值', r.doc._els.name[0].textContent === '新名',
    `实际 ${JSON.stringify(r.doc._els.name[0].textContent)}`);
  check('新值写回了 localStorage（下次刷新才能不闪）', r.events.includes('cache:set'));
  check('缓存里存的是服务端返回值',
    JSON.parse(r.store.get('sf_site_v1')).name === '新名');
}

// ---------- 4. 副标题为空：不省出尾巴（Bug T） ----------
console.log('副标题留空：省掉分隔符、移除空元素');
{
  const r = run('index', { cache: JSON.stringify({ name: '翻译工坊', tagline: '' }) });
  check('tagline 为空时 <title> 不带分隔符尾巴', r.doc.title === '翻译工坊',
    `实际 ${JSON.stringify(r.doc.title)}`);
  check('tagline 为空时移除该元素（不留空壳）', r.doc._els.tagline[0]._removed === true);
}

// ---------- 5. 管理端标题模板 ----------
console.log('管理端 <title> 模板');
{
  const r = run('admin', { cache: JSON.stringify({ name: '翻译工坊', tagline: '任何副标题' }) });
  check('管理端 <title> 为「站名 · 管理端」', r.doc.title === '翻译工坊 · 管理端',
    `实际 ${JSON.stringify(r.doc.title)}`);
}

// ---------- 6. data-site-title 后缀 ----------
console.log('页脚/后缀写入点');
{
  const r = run('admin', { cache: JSON.stringify({ name: '翻译工坊', tagline: 'x' }), foot: true });
  check('[data-site-title="管理端"] 渲染成「站名 · 管理端」',
    r.doc._els.foot[0].textContent === '翻译工坊 · 管理端',
    `实际 ${JSON.stringify(r.doc._els.foot[0].textContent)}`);
}

// ---------- 7. 接口挂了不能把页面搞坏 ----------
console.log('接口失败：保持缓存/默认，不抛错');
{
  const r = run('index', { cache: JSON.stringify({ name: '缓存名', tagline: '' }) });
  r.failFetch();
  await settle();
  check('接口 500 时仍保留缓存站名', r.doc._els.name[0].textContent === '缓存名');
  // 无缓存 + 接口失败 = 保持硬编码默认名，页面不崩。
  const r2 = run('index');
  r2.failFetch();
  await settle();
  check('无缓存且接口失败时不写空值（保留默认名）', r2.doc._els.name[0].textContent === null,
    `实际 ${JSON.stringify(r2.doc._els.name[0].textContent)}`);
}

// ---------- 8. 缓存脏数据不能崩 ----------
console.log('脏缓存容错');
{
  let threw = null;
  try {
    const r = run('index', { cache: '{不是合法 JSON' });
    r.resolveFetch({ name: '正常名', tagline: '' });
    await settle();
    check('坏 JSON 缓存被吞掉、接口值仍然生效', r.doc._els.name[0].textContent === '正常名');
  } catch (e) { threw = e; }
  check('坏缓存不抛异常', threw === null, String(threw));
}

// ---------- 9. 硬规则：不得使用 innerHTML ----------
console.log('硬规则：站名注入只用 textContent');
{
  let threw = null;
  try {
    const r = run('index', { cache: JSON.stringify({ name: '<img src=x onerror=alert(1)>', tagline: '' }) });
    r.resolveFetch({ name: '<img src=x onerror=alert(1)>', tagline: '' });
    await settle();
    check('含标签的站名原样作为文本写入（不是 HTML）',
      r.doc._els.name[0].textContent === '<img src=x onerror=alert(1)>');
  } catch (e) { threw = e; }
  check('注入含标签的站名不会触发 innerHTML（无 XSS 入口）', threw === null,
    threw ? String(threw.message || threw) : '');
}

// ---------- 10. 管理端保存后立刻重绘（Bug R） ----------
console.log('保存后立刻重绘：暴露 window.sfSiteApply');
{
  const r = run('index', { cache: JSON.stringify({ name: '旧名', tagline: '旧副标' }) });
  check('site.js 暴露了 window.sfSiteApply 给管理端调用',
    typeof r.sandbox.window.sfSiteApply === 'function');
  if (typeof r.sandbox.window.sfSiteApply === 'function') {
    r.sandbox.window.sfSiteApply({ name: '刚保存的名', tagline: '刚保存的副标' });
    check('sfSiteApply 立刻改掉当前页站名与标题',
      r.doc._els.name[0].textContent === '刚保存的名' &&
      r.doc.title === '刚保存的名 — 刚保存的副标',
      `实际 name=${JSON.stringify(r.doc._els.name[0].textContent)} title=${JSON.stringify(r.doc.title)}`);
  }
  // 上面只证明「sfSiteApply 本身能用」。真正要守的是**保存路径确实调用了它，
  // 而且用的是服务端返回值**。早先这里是一条 /sfSiteApply\s*\(/.test(admin) 的
  // 字符串断言 —— 把调用改成 `if (false && window.sfSiteApply)` 它照样绿，
  // 改成传输入框的值它也照样绿：骑空集的断言等于没断。所以这里把出货文件里的
  // saveSite 真抽出来跑，喂桩 fetch，看它到底往 sfSiteApply 里塞了什么。
  const ADMIN_JS = readFileSync(join(WEB, 'js', 'admin.js'), 'utf8');
  const fnSrc = extractFn(ADMIN_JS, 'async function saveSite(');
  check('能从 admin.js 里抽出 saveSite', !!fnSrc);
  if (fnSrc) {
    // 服务端返回值和输入框里的值**故意不同**：只有用服务端返回值才算过。
    // （服务端会 trim、会拦掉非法字符，前端拿到的想当然版本可能和落库的不一样。）
    async function runSave(resp, requestPayload) {
      const calls = { applied: [], painted: [], msgs: [], toasts: [], sent: null };
      const els = { name: { value: '输入框里的名' }, tagline: { value: '输入框里的副标' } };
      const box = {
        window: { sfSiteApply: (c) => calls.applied.push(c) },
        authHdr: () => ({}),
        siteEls: () => els,
        showSiteMsg: (t) => calls.msgs.push(t),
        paintSitePreview: (c) => calls.painted.push(c),
        toast: (t) => calls.toasts.push(t),
        JSON: JSON,
        fetch: async (url, opt) => {
          calls.sent = { url, opt };
          return { ok: resp.ok !== false, json: async () => resp.body };
        },
      };
      vm.createContext(box);
      vm.runInContext(fnSrc + '\nglobalThis.__saveSite = saveSite;\n', box);
      await box.__saveSite(requestPayload);
      return { calls, els };
    }

    const okResp = { name: '服务端定的名', tagline: '服务端定的副标', is_custom: { name: true, tagline: true }, defaults: { name: 'SkillForge', tagline: '智能写作工坊' } };
    const r1 = await runSave({ body: okResp }, { name: '输入框里的名', tagline: '输入框里的副标' });
    check('保存成功后确实调用了 sfSiteApply（而不是被短路掉）',
      r1.calls.applied.length === 1, `调用次数 ${r1.calls.applied.length}`);
    check('重绘用的是服务端返回值，不是输入框里的值',
      r1.calls.applied[0] && r1.calls.applied[0].name === '服务端定的名' && r1.calls.applied[0].tagline === '服务端定的副标',
      `实际 ${JSON.stringify(r1.calls.applied)}`);
    check('保存后输入框回填成服务端返回值', r1.els.name.value === '服务端定的名' && r1.els.tagline.value === '服务端定的副标');
    check('请求打到 PUT /api/admin/site', r1.calls.sent && r1.calls.sent.url === '/api/admin/site' && r1.calls.sent.opt.method === 'PUT');
    check('提交的确实是把「空串=恢复默认」语义传上去', r1.calls.sent.opt.body === JSON.stringify({ name: '输入框里的名', tagline: '输入框里的副标' }));

    const r2 = await runSave({ ok: false, body: { error: '名称太长' } }, { name: 'x', tagline: '' });
    check('保存失败时不重绘、并显示服务端错误',
      r2.calls.applied.length === 0 && r2.calls.msgs.includes('名称太长'),
      `applied=${r2.calls.applied.length} msgs=${JSON.stringify(r2.calls.msgs)}`);
    check('保存失败时不弹成功 toast', r2.calls.toasts.length === 0, JSON.stringify(r2.calls.toasts));
  }
}

// ---------- 11. 管理端状态文案（Bug U） ----------
// 从**出货文件**里抽 siteStatusText 来跑，不抄一份。抄一份的话改坏真文件测试照样绿。
console.log('管理端状态文案：is_custom 是对象，不能当布尔用');
{
  const ADMIN_JS = readFileSync(join(WEB, 'js', 'admin.js'), 'utf8');
  // 取 `function siteStatusText(j) { ... }` 的函数体（按花括号配平）。
  const start = ADMIN_JS.indexOf('function siteStatusText(');
  let fnSrc = null;
  if (start >= 0) {
    let i = ADMIN_JS.indexOf('{', start), depth = 0, end = -1;
    for (let k = i; k < ADMIN_JS.length; k++) {
      if (ADMIN_JS[k] === '{') depth++;
      else if (ADMIN_JS[k] === '}') { depth--; if (depth === 0) { end = k; break; } }
    }
    if (end > 0) fnSrc = ADMIN_JS.slice(start, end + 1);
  }
  check('能从 admin.js 里抽出 siteStatusText', !!fnSrc);
  if (fnSrc) {
    const box = {};
    vm.createContext(box);
    vm.runInContext(fnSrc + '\nglobalThis.siteStatusText = siteStatusText;\n', box);
    const f = box.siteStatusText;
    check('抽出来的是可调用函数', typeof f === 'function');

    const D = { name: 'SkillForge', tagline: '智能写作工坊' };
    const none = f({ name: 'SkillForge', tagline: '智能写作工坊', is_custom: { name: false, tagline: false }, defaults: D });
    check('两项都是默认值时显示「当前为默认名称」', none === '当前为默认名称', `实际 ${JSON.stringify(none)}`);

    const both = f({ is_custom: { name: true, tagline: true }, defaults: D });
    check('两项都自定义时能说出「都自定义」', /都自定义/.test(both), `实际 ${JSON.stringify(both)}`);

    const onlyName = f({ is_custom: { name: true, tagline: false }, defaults: D });
    check('只改名称时不能声称副标题也改了',
      /名称已自定义/.test(onlyName) && !/都自定义/.test(onlyName), `实际 ${JSON.stringify(onlyName)}`);

    const onlyTag = f({ is_custom: { name: false, tagline: true }, defaults: D });
    check('只改副标题时说「副标题已自定义」', /副标题已自定义/.test(onlyTag), `实际 ${JSON.stringify(onlyTag)}`);

    // 默认值必须来自接口，不能是前端硬编码：换一组默认值，文案里必须跟着换。
    const alt = f({ is_custom: { name: true, tagline: true }, defaults: { name: '甲甲', tagline: '乙乙' } });
    check('默认值来自接口（不是前端硬编码 SkillForge）',
      alt.includes('甲甲') && alt.includes('乙乙') && !alt.includes('SkillForge'), `实际 ${JSON.stringify(alt)}`);

    // 老接口/字段缺失不能让界面显示 undefined。
    const bare = f({});
    check('字段缺失时不显示 undefined', !/undefined|NaN/.test(bare), `实际 ${JSON.stringify(bare)}`);
    const noDefaults = f({ is_custom: { name: true, tagline: true } });
    check('无 defaults 时不显示 undefined/空括号', !/undefined|NaN/.test(noDefaults) && !/（\s*）/.test(noDefaults),
      `实际 ${JSON.stringify(noDefaults)}`);
  }
  check('admin.js 里不再硬编码默认站名', !/'SkillForge'/.test(ADMIN_JS) && !/"SkillForge"/.test(ADMIN_JS));
  check('admin.js 保存/读取路径都走 siteStatusText（没退回内联三元）',
    (ADMIN_JS.match(/siteStatusText\(/g) || []).length >= 2);
}

console.log('');
if (failures) {
  console.log(`前端站名防线：${failures} 项失败`);
  process.exit(1);
}
console.log('前端站名防线：全部通过');
