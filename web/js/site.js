/* 站点名称/副标题的运行时注入。
 *
 * 为什么要有这个文件：站名是管理端可改的**运行时数据**，而 HTML 里的
 * `SkillForge` 是构建时硬编码的。不注入的话改完站名刷新页面还是老名字。
 *
 * 两条硬规则（踩过的坑，别改）：
 *  1. 一律用 textContent 写入，**绝不用 innerHTML** —— 站名是用户输入，
 *     走 innerHTML 就是一个自助 XSS 入口。
 *  2. 先套 localStorage 缓存再拉接口。否则每次刷新都会先闪一下默认名
 *     再变成自定义名（网络往返期间的白字），改过名的人看着像回滚了。
 *
 * 页面侧契约（在 HTML 里声明，本文件不认识具体页面）：
 *   <html data-site-page="index|admin">  决定 <title> 怎么拼
 *   [data-site-name]    站名写入点
 *   [data-site-tagline] 副标题写入点。注意：管理端页头那个"管理端"是**页面标识**，
 *                       故意不挂这个属性 —— 挂了就会被站点副标题盖掉，两个页头
 *                       会长得一模一样，用户分不清自己在哪一页。
 *   [data-site-title]   页脚等处"站名 · 后缀"的写入点（可选）
 */
(() => {
  const CACHE_KEY = 'sf_site_v1';
  // 页面 → <title> 模板。副标题为空时自动省掉分隔符，不留"Name — "这种尾巴。
  const TITLE = {
    index: (n, t) => (t ? `${n} — ${t}` : n),
    admin: (n) => `${n} · 管理端`,
  };

  const page = document.documentElement.getAttribute('data-site-page') || 'index';

  function apply(cfg) {
    if (!cfg || !cfg.name) return;
    for (const el of document.querySelectorAll('[data-site-name]')) el.textContent = cfg.name;
    for (const el of document.querySelectorAll('[data-site-tagline]')) {
      // 副标题留空 = 移除该元素而不是留个空壳（否则 brand 里多一段间距）
      if (cfg.tagline) el.textContent = cfg.tagline;
      else el.remove();
    }
    for (const el of document.querySelectorAll('[data-site-title]')) {
      const suffix = el.getAttribute('data-site-title');
      el.textContent = suffix ? `${cfg.name} · ${suffix}` : cfg.name;
    }
    document.title = (TITLE[page] || TITLE.index)(cfg.name, cfg.tagline || '');
  }

  // 暴露给管理端：保存成功后立刻用新值刷新当前页（不用等下一次 fetch）
  window.sfSiteApply = apply;

  // 1) 缓存先上，避免闪烁
  try {
    const cached = JSON.parse(localStorage.getItem(CACHE_KEY) || 'null');
    if (cached && cached.name) apply(cached);
  } catch (_) { /* 缓存坏了不影响主流程 */ }

  // 2) 再拉真值。失败就保持默认/缓存 —— 站名拉不到不该让页面报错
  fetch('/api/site', { headers: { 'Accept': 'application/json' } })
    .then((r) => (r.ok ? r.json() : null))
    .then((cfg) => {
      if (!cfg || !cfg.name) return;
      apply(cfg);
      try { localStorage.setItem(CACHE_KEY, JSON.stringify(cfg)); } catch (_) {}
    })
    .catch(() => {});
})();
