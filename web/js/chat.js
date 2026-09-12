/* SkillForge — conversation UI */
(() => {
  const $ = (s) => document.querySelector(s);
  const scroll = $('#chat-scroll'), col = $('#chat-col');
  const input = $('#chat-input'), send = $('#chat-send');
  const welcome = $('#welcome'), suggs = $('#suggestions');
  const pill = $('#active-skill-pill');
  const modeAuto = $('#mode-auto'), modeManual = $('#mode-manual');
  const skBtn = $('#skill-pick-btn'), skLabel = $('#skill-pick-label');
  const skPanel = $('#skill-panel'), skSearch = $('#skill-search'), skList = $('#skill-list');
  const footnote = $('#ch-footnote');
  const heroSub = $('#ch-w-sub'), heroEg = $('#ch-w-eg');

  // —— 两种对话方式 ——
  // auto   : 让引擎自己理解意图，从技能库里挑（默认，适合"我也不知道该用哪个"）
  // manual : 用户点名技能，后端整跳过一次意图分类 —— 不只是更准，也快得多
  const MODE_KEY = 'skillforge.chatmode';
  const SKILL_KEY = 'skillforge.chatskill';
  let chatMode = 'auto';
  let pickedSkill = null;   // { slug, name } —— manual 模式下锁定发送的技能
  let allSkills = [];       // /api/skills 缓存（面板与欢迎卡片共用）
  let skFetched = false;

  const FOOTNOTE = {
    auto: '引擎会自己理解你的需求，从技能库里挑最合适的技能',
    manual: '已锁定技能，全程只按这一个技能的规矩来 —— 跳过意图识别，出结果更快',
  };
  const PLACEHOLDER = {
    auto: '说说你想写什么…（Enter 发送，Shift+Enter 换行）',
    manual: '把材料和要求直接写在这里…（Enter 发送，Shift+Enter 换行）',
  };
  // 欢迎语也得跟着模式变。自动模式下说"我帮你挑最合适的技能"是对的；
  // 手动模式下还这么说就是骗人 —— 技能是你自己锁的，界面得说清楚。
  const HERO_SUB = {
    auto: '直接说你想要的文章，我来调度最合适的技能为你起草。',
    manual: '先在下方指定一个技能，再说要写什么 —— 全程只按这个技能来，跳过意图识别。',
  };
  const HERO_EG = {
    auto: '例如：「写一段写给客户的产品介绍，200 字左右」',
    manual: '例如：选「采购合同」→「甲方 XX 公司，采购 30 台服务器，含税」',
  };

  // sessionId tracks the ACTIVE local session; restored on boot so a refresh
  // keeps the same id (and thus the server-side in-memory history).
  let sessionId = null;
  let busy = false;

  const AVATARS = { u: '', a: '' };  // 用户走人形剪影 SVG;AI 走 sparkle SVG
    // 用户头像:极简人形剪影(减号肩 + 圆头),石墨风雪,方角 vs AI 圆形形成形色区分。
    const USER_AVATAR_SVG =
      '<svg viewBox="0 0 16 16" fill="none" xmlns="http://www.w3.org/2000/svg">' +
        '<circle cx="8" cy="5.2" r="2.6" fill="currentColor"/>' +
        '<path d="M2.6 13.4c.6-2.7 2.8-4.2 5.4-4.2s4.8 1.5 5.4 4.2" stroke="currentColor" stroke-width="1.4" stroke-linecap="round"/>' +
      '</svg>';
    // AI 头像:极简四芒火花(sparkle)。比孤零零的中圆点「·」更易识别是 AI 助手,黑白灰一致。
    const AI_AVATAR_SVG =
      '<svg viewBox="0 0 16 16" fill="none" xmlns="http://www.w3.org/2000/svg">' +
        '<path d="M8 1.5c.45 2.6 2.4 4.55 5 5-2.6.45-4.55 2.4-5 5-.45-2.6-2.4-4.55-5-5 2.6-.45 4.55-2.4 5-5z" fill="currentColor"/>' +
        '<path d="M12.5 10.2c.2 1.1.9 1.7 2 1.9-1.1.2-1.8 1-2 2.1-.2-1.1-.9-1.9-2.1-2.1 1.1-.2 1.8-.8 2-1.9z" fill="currentColor" opacity=".55"/>' +
      '</svg>';
    function avatarNode(who) {
      const n = document.createElement('div');
      n.className = 'ch-avatar';
      if (who === 'u') n.innerHTML = USER_AVATAR_SVG;
      else n.innerHTML = AI_AVATAR_SVG;
      return n;
    }

  /* ---------- markdown (marked) config ---------- */
  // Render assistant answers as markdown. Only allow safe URL protocols and
  // no raw HTML from the model (defense against javascript: urls / injections).
  function sanitizeUrl(u) {
    if (!u) return '';
    try {
      const p = new URL(u, location.origin).protocol;
      return ['http:', 'https:', 'mailto:'].includes(p) ? u : '';
    } catch { return ''; }
  }
  (window.marked || {}).setOptions && marked.setOptions({
    gfm: true, breaks: true,
    renderer: (() => {
      if (typeof marked.Renderer !== 'function') return undefined;
      const r = new marked.Renderer();
      const origLink = r.link.bind(r), origImg = r.image.bind(r);
      // marked v4 renderer uses plain-arg signatures: link(href,title,text)
      r.link = (href, title, text) => {
        href = sanitizeUrl(href);
        if (!href) return String(text || '');
        return origLink(href, title, text);
      };
      r.image = (href, title, text) => {
        href = sanitizeUrl(href);
        return href ? `<img src="${esc(href)}" alt="${esc(text || '')}" title="${esc(title || '')}">` : '';
      };
      return r;
    })(),
  });

  /* ---------- local session store (browser persistence) ---------- */
  // Conversations live in localStorage so a refresh / re-open keeps history
  // and the active session id stays stable (otherwise the server-side in-memory
  // history for a random id is lost forever).
  const STORE_KEY = 'skillforge.sessions.v1';
  let store = loadStore();
  // files emitted by the in-flight assistant turn (SSE `file` events). Collected
  // during streaming, persisted onto the assistant message at turn end so a
  // refresh keeps the download cards.
  let pendingFiles = [];

  function loadStore() {
    try {
      const raw = localStorage.getItem(STORE_KEY);
      if (!raw) return { activeId: null, sessions: [] };
      const d = JSON.parse(raw);
      if (!d || !Array.isArray(d.sessions)) return { activeId: null, sessions: [] };
      return { activeId: d.activeId || null, sessions: d.sessions };
    } catch { return { activeId: null, sessions: [] }; }
  }
  function saveStore() {
    try { localStorage.setItem(STORE_KEY, JSON.stringify(store)); } catch {}
  }
  function findSession(id) { return store.sessions.find((s) => s.id === id) || null; }
  function activeSession() { return findSession(store.activeId); }
  function touchSession(id) {
    const s = findSession(id);
    if (s) { s.updated = Date.now(); saveStore(); }
  }

  function newSession() {
    const id = 's_' + Date.now().toString(36) + Math.random().toString(36).slice(2, 8);
    const s = { id, title: '新对话', created: Date.now(), updated: Date.now(), messages: [] };
    store.sessions.unshift(s);
    store.activeId = id;
    sessionId = id;   // wire the current request id to this local session
    saveStore();
    return s;
  }

  // Keep at most 50 sessions; evict oldest beyond that.
  function prune(max = 50) {
    if (store.sessions.length > max) {
      const keep = store.sessions.slice(0, max);
      if (!keep.some((s) => s.id === store.activeId)) store.activeId = keep[0].id;
      store.sessions = keep;
      saveStore();
    }
  }

  function setActive(id) {
    store.activeId = id;
    sessionId = id;
    saveStore();
    router.render();
    const s = findSession(id);
    if (s && s.messages.length) renderMessages(id);
    else clearChat(); // empty session → show welcome
  }

  function deleteSession(id) {
    store.sessions = store.sessions.filter((s) => s.id !== id);
    if (store.activeId === id) {
      store.activeId = store.sessions[0] ? store.sessions[0].id : null;
      saveStore();
      clearChat();
      if (store.activeId) renderMessages(store.activeId);
    } else saveStore();
    router.render();
  }

  function renameSession(id, title) {
    const s = findSession(id);
    if (s && title) { s.title = title; saveStore(); router.render(); }
  }

  function clearAll() {
    store = { activeId: null, sessions: [] };
    saveStore();
    clearChat();
    router.render();
  }

  // Minutes ago → "刚刚 / N 分钟前 / N 小时前 / 昨天 / M月D日"
  function relTime(ts) {
    if (!ts) return '';
    const diff = Date.now() - ts;
    const m = Math.floor(diff / 60000);
    if (m < 1) return '刚刚';
    if (m < 60) return m + ' 分钟前';
    const h = Math.floor(m / 60);
    if (h < 24) return h + ' 小时前';
    const d = Math.floor(h / 24);
    const dt = new Date(ts);
    if (d === 1) return '昨天';
    if (dt.getFullYear() === new Date().getFullYear()) return (dt.getMonth() + 1) + '月' + dt.getDate() + '日';
    return (dt.getFullYear()) + '年' + (dt.getMonth() + 1) + '月' + dt.getDate() + '日';
  }

  // Title fallback: derive from the first user message.
  function titleFrom(text) {
    const t = String(text || '').trim().replace(/\s+/g, ' ');
    if (!t) return '新对话';
    return t.length > 18 ? t.slice(0, 18) + '…' : t;
  }

  // ---- sidebar render + actions ----
  const router = (function () {
    const list = $('#sb-list');
    const count = $('#sb-count'), clearBtn = $('#sb-clear');
    const newBtn = $('#sb-new'), toggle = $('#sb-toggle'), sidebar = $('#sidebar');

    function render() {
      const sessions = store.sessions;
      list.innerHTML = '';
      count.textContent = sessions.length ? sessions.length + ' 个对话' : '';
      clearBtn.classList.toggle('hidden', sessions.length === 0);
      if (sessions.length === 0) {
        // dynamic empty state — lives INSIDE the list so a refresh / render
        // always reflects it (a static child would be wiped by innerHTML='').
        const emptyEl = el('div', 'sb-empty', '还没有对话');
        list.appendChild(emptyEl);
        return;
      }
      sessions.forEach((s) => {
        const item = el('div', 'sb-item' + (s.id === store.activeId ? ' active' : ''));
        item.dataset.id = s.id;
        item.setAttribute('role', 'button');
        item.tabIndex = 0;
        const title = el('span', 'sb-item-title', s.title);
        const meta = el('span', 'sb-item-meta', relTime(s.updated));
        const del = el('button', 'sb-item-del', '');
        del.title = '删除对话';
        del.innerHTML = '<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M18 6L6 18M6 6l12 12"/></svg>';
        del.onclick = (e) => { e.stopPropagation(); if (confirm('删除这个对话？')) deleteSession(s.id); };
        item.appendChild(title); item.appendChild(meta); item.appendChild(del);
        // click → switch; dblclick / Enter → rename
        item.addEventListener('click', () => { if (s.id !== store.activeId) setActive(s.id); closeSidebarMobile(); });
        item.addEventListener('dblclick', (e) => { e.stopPropagation(); promptRename(s.id, s.title); });
        list.appendChild(item);
      });
      sidebar.classList.toggle('hidden', false);
    }

    function promptRename(id, cur) {
      const name = prompt('重命名对话：', cur);
      if (name && name.trim()) renameSession(id, name.trim());
    }

    function open() { sidebar.classList.add('open'); }
    function close() { sidebar.classList.remove('open'); }
    function isOpen() { return sidebar.classList.contains('open'); }

    // desktop: clicks inside sidebar side keep it open
    return { render, open, close, isOpen, promptRename };
  })();

  function closeSidebarMobile() {
    if (window.innerWidth <= 900) router.close();
  }

  // ---- history rendering ----
  function clearChat() {
    // remove only conversation rows — the welcome block lives INSIDE #chat-col,
    // so wiping innerHTML would delete it and it can never come back.
    col.querySelectorAll('.ch-msg').forEach((n) => n.remove());
    welcome.classList.remove('hidden');
    setPill(null);
  }

  function renderMessages(sessionIdToRender) {
    const s = findSession(sessionIdToRender);
    if (!s) return;
    clearChat();
    welcome.classList.add('hidden'); // history present → hide welcome
    s.messages.forEach((m) => {
      if (m.role === 'user') {
        addUser(m.text || '');
      } else {
        // render without a "正在使用技能" pill — that's a live-stream transient.
        const bubble = addAssistant(null);
        const html = marked.parse(m.text || '', { breaks: true, gfm: true });
        bubble.innerHTML = html;
        // restore any file cards the assistant produced (survives refresh)
        if (Array.isArray(m.files)) {
          m.files.forEach((f) => appendFileLink(null, f));
        }
      }
    });
    keepBottom();
  }

  /* ---------- app init ---------- */
  function boot() {
    // resume: active local session exists → keep its id + history; else new one.
    if (store.activeId && findSession(store.activeId)) {
      sessionId = store.activeId;
    } else {
      const s = newSession();
      sessionId = s.id;
    }
    router.render();
    if (sessionId) {
      const s = findSession(sessionId);
      if (s && s.messages.length) { renderMessages(sessionId); }
    }
    // wire sidebar UI
    $('#sb-new').addEventListener('click', () => {
      const s = newSession();
      sessionId = s.id;
      clearChat();
      router.render();
      input.focus();
      closeSidebarMobile();
    });
    $('#sb-clear').addEventListener('click', () => {
      if (confirm('清空全部对话？此操作不可撤销。')) { clearAll(); newSession(); input.focus(); }
    });
    $('#sb-toggle').addEventListener('click', () => {
      const sb = $('#sidebar');
      sb.classList.toggle('open');
    });
    document.addEventListener('keydown', (e) => {
      if ((e.metaKey || e.ctrlKey) && (e.key === 'n' || e.key === 'N')) {
        e.preventDefault();
        const s = newSession();
        sessionId = s.id;
        clearChat();
        router.render();
        input.focus();
        closeSidebarMobile();
      }
    });

    renderSuggestions();
    wireModes();
    initModes();
    autoGrow();
    input.focus();
    input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); submit(); }
    });
    send.addEventListener('click', submit);
    window.addEventListener('resize', keepBottom);
    window.addEventListener('storage', (e) => {
      if (e.key === STORE_KEY) location.reload();
    });
  }

  // 默认兜底建议（API 拉取失败或技能为空时使用）
  const FALLBACK_SUGGESTIONS = [
    '写一段 200 字的产品介绍',
    '给客户写一封正式的邮件',
    '把这段话改得更有文采',
  ];

  // 技能类型 → 人话标签 + 克制图标（黑白灰，无 emoji）
  const TYPE_META = {
    write:    { tag: '写文章',   icon: '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M12 20h9"/><path d="M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4Z"/></svg>' },
    docgen:   { tag: '生成文档', icon: '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/><path d="M8 13h8M8 17h8"/></svg>' },
    query:    { tag: '办事流程', icon: '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><circle cx="11" cy="11" r="7"/><path d="m21 21-4.3-4.3"/></svg>' },
    template: { tag: '办事项',   icon: '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M21 19V5a2 2 0 0 0-2-2H5a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2Z"/><path d="M9 3v18M16 11v6"/></svg>' },
  };

  // 每个技能的"傻瓜示例"：点一下就能发（贴合其能力触发路由）
  function quickExample(sk) {
    if (!sk) return '';
    const s = String(sk.slug || sk.name || '');
    if (sk.skill_type === 'docgen') return '帮我生成一份员工考勤表 Excel';
    if (s.includes('新闻') || s.includes('通稿')) return '写一篇公司完成 B 轮融资的新闻通稿';
    if (s.includes('公积金')) return '办租房公积金提取，需要哪些材料、给张申请表模板';
    const tag = (TYPE_META[sk.skill_type] || {}).tag || '写内容';
    if (sk.name && sk.description) return `（用「${sk.name}」${tag}：${sk.description}）`;
    return sk.name ? `用「${sk.name}」帮我写一段内容` : '';
  }

  // 组装把表单字段变成自然语言请求（submitSkillForm 中处理）

  function renderSuggestions() {
    suggs.innerHTML = '';
    fetch('/api/skills')
      .then((r) => (r.ok ? r.json() : Promise.reject()))
      .then((data) => {
        const sk = (data && data.skills) || [];
        if (!sk.length) { renderFallback(); return; }
        renderSkillCards(sk);
      })
      .catch(renderFallback);
  }

  function renderFallback() {
    suggs.innerHTML = '';
    suggs.className = 'ch-sugs-row'; // 重置容器，避免残留卡片网格样式
    const items = FALLBACK_SUGGESTIONS.slice();
    const b = el('div', 'ch-sugs-row');
    items.forEach((t) => {
      const p = el('button', 'ch-sug', t);
      p.onclick = () => { setInput(t); input.focus(); };
      b.appendChild(p);
    });
    suggs.appendChild(b);
  }

  // —— 场景卡片网格（一卡 = 一件事，点开即填）——
  // 核心技能排最前：它们是"通用能力"，用户第一次来最该看见的就是这两张卡。
  function renderSkillCards(list) {
    suggs.className = 'ch-card-grid';
    suggs.innerHTML = '';
    const sorted = list.slice().sort((a, b) => (b.is_core ? 1 : 0) - (a.is_core ? 1 : 0));
    sorted.forEach((sk) => {
      const t = TYPE_META[sk.skill_type] || TYPE_META.write;
      const card = el('button', 'ch-card' + (sk.is_core ? ' is-core' : ''));
      card.type = 'button';
      card.dataset.slug = sk.slug;
      const hasForm = Array.isArray(sk.input_params) && sk.input_params.length > 0;
      card.innerHTML =
        '<span class="ch-card-ic">' + (sk.is_core ? CORE_STAR : t.icon) + '</span>' +
        '<span class="ch-card-txt">' +
          '<span class="ch-card-name">' + esc(sk.name || sk.slug || '') +
            (sk.is_core ? '<span class="ch-card-core">核心</span>' : '') + '</span>' +
          '<span class="ch-card-desc">' + esc(cardDesc(sk, t.tag)) + '</span>' +
        '</span>' +
        '<span class="ch-card-tag">' + esc(t.tag) + '</span>';
      card.addEventListener('click', () => {
        if (chatMode === 'manual') {
          // 手动模式：点卡片 = 指定这个技能，并顺手把示例填进去
          pickSkill(sk);
          const q = quickExample(sk);
          if (q) { setInput(q); input.focus(); }
          return;
        }
        if (hasForm && Array.isArray(sk.input_params)) openSkillForm(sk, card);
        else { const q = quickExample(sk); if (q) { setInput(q); input.focus(); } }
      });
      suggs.appendChild(card);
    });
  }

  const CORE_STAR = '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3.5 14.4 9l6 .5-4.6 4 1.4 5.9L12 16.4 6.8 19.4 8.2 13.5 3.6 9.5l6-.5z"/></svg>';

  function cardDesc(sk, tag) {
    const d = sk.description ? sk.description.trim() : '';
    if (d) return d;
    const base = { write: '帮你起草', docgen: '帮你生成文件', query: '帮你办', template: '帮你办' }[sk.skill_type] || '帮你';
    return sk.category ? (base + '「' + sk.category + '」相关') : (base + '一件事');
  }

  // —— 填空表单：技能参数 → 待填字段 → 组装成一条请求发送 ——
  function openSkillForm(sk, card) {
    // 已展开则收起
    const existing = card.parentElement.querySelector('.ch-card-form');
    if (existing && existing.dataset.slug === sk.slug) { existing.remove(); card.classList.remove('open'); return; }
    card.parentElement.querySelectorAll('.ch-card-form').forEach((f) => f.remove());
    card.parentElement.querySelectorAll('.ch-card.open').forEach((c) => c.classList.remove('open'));

    const form = el('div', 'ch-card-form');
    form.dataset.slug = sk.slug;
    const title = el('div', 'ch-card-form-title', sk.name + ' — 填几个关键信息，帮你生成');
    form.appendChild(title);
    const body = el('div', 'ch-card-form-body');
    const fields = [];
    sk.input_params.forEach((p) => {
      const fld = el('div', 'ch-field');
      const lbl = el('label', 'ch-field-label', (p.label || p.name) + (p.required ? ' *' : ''));
      fld.appendChild(lbl);
      let ctl = null;
      if (p.type === 'select') {
        ctl = document.createElement('select');
        ctl.className = 'ch-field-ctl';
        (p.options || []).forEach((o) => {
          const op = el('option', '', o); op.value = o; ctl.appendChild(op);
        });
      } else if (p.type === 'textarea') {
        ctl = document.createElement('textarea');
        ctl.className = 'ch-field-ctl';
        ctl.rows = 3;
      } else if (p.type === 'number') {
        ctl = document.createElement('input');
        ctl.type = 'number'; ctl.className = 'ch-field-ctl';
        if (typeof p.min === 'number') ctl.min = p.min;
        if (typeof p.max === 'number') ctl.max = p.max;
      } else {
        ctl = document.createElement('input');
        ctl.type = 'text'; ctl.className = 'ch-field-ctl';
      }
      if (p.placeholder) ctl.placeholder = p.placeholder;
      if (p.default) ctl.value = p.default;
      ctl.dataset.required = p.required ? '1' : '0';
      ctl.dataset.plabel = p.label || p.name;
      ctl.dataset.pname = p.name || '';
      fld.appendChild(ctl);
      if (p.help) { const h = el('div', 'ch-field-help', p.help); fld.appendChild(h); }
      body.appendChild(fld);
      fields.push(ctl);
    });
    form.appendChild(body);
    const actions = el('div', 'ch-card-form-actions');
    const go = el('button', 'ch-btn primary', '生成');
    go.type = 'button';
    go.addEventListener('click', () => submitSkillForm(sk, fields, form));
    const cancel = el('button', 'ch-btn', '取消');
    cancel.type = 'button';
    cancel.addEventListener('click', () => { form.remove(); card.classList.remove('open'); });
    actions.appendChild(go); actions.appendChild(cancel);
    form.appendChild(actions);
    card.appendChild(form);
    card.classList.add('open');
    const first = fields[0]; if (first && first.focus) first.focus();
  }

  function submitSkillForm(sk, fields, form) {
    const filled = [];
    let missing = false;
    fields.forEach((c) => {
      const v = (c.value || '').trim();
      if (!v && c.dataset.required === '1') { missing = true; c.classList.add('err'); return; }
      c.classList.remove('err');
      if (v) filled.push(v);
    });
    if (missing) return;
    form.remove();
    // 组装成一句话给后端（保持参数名映射，让意图/参数提取能命中）
    const label = (TYPE_META[sk.skill_type] || {}).tag || '内容';
    const q = filled.length
      ? '用「' + sk.name + '」' + label + '，' + filled.join('；')
      : quickExample(sk);
    setInput(q);
    submit();
  }

  function renderSug(t) {
    const b = document.createElement('button');
    b.className = 'ch-sug'; b.textContent = t;
    b.onclick = () => { setInput(t); input.focus(); };
    suggs.appendChild(b);
  }

  function setInput(t) { input.value = t; autoGrow(); }
  function autoGrow() {
    input.style.height = 'auto';
    input.style.height = Math.min(input.scrollHeight, 160) + 'px';
  }

  /* ---------- DOM helpers ---------- */
  function addUser(text) {
    const wrap = el('div', 'ch-msg user');
    addBubble(wrap, 'u', text);
    col.appendChild(wrap);
    keepBottom();
    return wrap;
  }

  function addAssistant(skillName) {
    const wrap = el('div', 'ch-msg assistant');
    const av = avatarNode('a');
    const right = el('div');
    right.style.flex = '1';
    if (skillName) {
      const p = el('div', 'ch-pill');
      p.innerHTML = '✦ <em>正在使用技能</em> · <span>' + esc(skillName) + '</span>';
      right.appendChild(p);
    }
    const bubble = el('div', 'ch-bubble');
    bubble.id = 'bubble-' + Date.now();
    right.appendChild(bubble);
    wrap.appendChild(av); wrap.appendChild(right);
    col.appendChild(wrap);
    keepBottom();
    return bubble;
  }

  function addBubble(wrap, who, text) {
    const av = avatarNode(who);
    const bub = el('div', 'ch-bubble', '');
    bub.appendChild(document.createTextNode(text));
    if (who === 'u') { wrap.appendChild(bub); wrap.appendChild(av); }
    else { wrap.appendChild(av); wrap.appendChild(bub); }
  }

  function el(tag, cls, text) {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = text;
    return n;
  }

  function esc(s) {
    return String(s).replace(/[&<>"']/g, (c) => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  }

  function keepBottom() { scroll.scrollTop = scroll.scrollHeight; }

  function setPill(name) {
    if (name) {
      pill.classList.remove('hidden');
      pill.innerHTML = '✦ 技能 · <span>' + esc(name) + '</span>';
    } else {
      pill.classList.add('hidden');
    }
  }

  /* ---------- 对话方式切换 + 技能选择面板 ---------- */
  function setMode(m, silent) {
    chatMode = m === 'manual' ? 'manual' : 'auto';
    try { localStorage.setItem(MODE_KEY, chatMode); } catch (e) {}
    modeAuto.classList.toggle('on', chatMode === 'auto');
    modeManual.classList.toggle('on', chatMode === 'manual');
    modeAuto.setAttribute('aria-selected', String(chatMode === 'auto'));
    modeManual.setAttribute('aria-selected', String(chatMode === 'manual'));
    skBtn.classList.toggle('hidden', chatMode !== 'manual');
    footnote.textContent = FOOTNOTE[chatMode];
    input.placeholder = PLACEHOLDER[chatMode];
    if (heroSub) heroSub.textContent = HERO_SUB[chatMode];
    if (heroEg) heroEg.textContent = HERO_EG[chatMode];
    // 切到手动却还没选技能 → 直接把面板打开，别让用户猜下一步干什么
    if (chatMode === 'manual' && !pickedSkill && !silent) openSkPanel(true);
    else if (chatMode === 'auto') closeSkPanel();
  }

  function initModes() {
    let m = 'auto', slug = '';
    try {
      m = localStorage.getItem(MODE_KEY) || 'auto';
      slug = localStorage.getItem(SKILL_KEY) || '';
    } catch (e) {}
    // 技能是后加载的：先记下 slug，等索引回来再对齐（技能可能已被删/停用）
    setMode(m, true);
    if (slug) pendingSlug = slug;
    loadSkillIndex().then(() => {
      if (pendingSlug && allSkills.some((s) => s.slug === pendingSlug)) {
        pickSkill(allSkills.find((s) => s.slug === pendingSlug), true);
      } else if (pendingSlug) {
        try { localStorage.removeItem(SKILL_KEY); } catch (e) {}
      }
    });
  }
  let pendingSlug = '';

  function loadSkillIndex() {
    if (skFetched) return Promise.resolve(allSkills);
    return fetch('/api/skills')
      .then((r) => (r.ok ? r.json() : Promise.reject()))
      .then((data) => {
        allSkills = ((data && data.skills) || []).slice()
          .sort((a, b) => (b.is_core ? 1 : 0) - (a.is_core ? 1 : 0));
        skFetched = true;
        renderSkList('');
        return allSkills;
      })
      .catch(() => allSkills);
  }

  // 技能面板的 HTML 由这个纯函数产出（skills/picked 全部走参数）。
  // 抽成纯函数不是为了好看：分组与「核心标」是这一轮的核心交付物，
  // 必须能被 web/tests 直接喂数据跑，而不是靠人肉点界面确认。
  function skPanelHTML(q, picked, skills) {
    const kw = (q || '').trim().toLowerCase();
    const all = Array.isArray(skills) ? skills : [];
    const hit = all.filter((sk) => {
      if (!kw) return true;
      return (String(sk.name || '') + ' ' + String(sk.slug || '') + ' ' + String(sk.description || ''))
        .toLowerCase().includes(kw);
    });
    // 核心技能（通用能力）永远独占一组且排在业务技能前面 —— 顺序不靠后端排序兜底。
    const core = hit.filter((s) => s.is_core), biz = hit.filter((s) => !s.is_core);
    const block = (title, items) => items.length
      ? '<div class="ch-skgroup"><div class="ch-skgroup-h">' + title + '<i>' + items.length + '</i></div>' +
        items.map((sk) => skItemHtml(sk, picked)).join('') + '</div>'
      : '';
    // 已锁定技能时给一条"退路"：一键回到自动调度，免得用户找不到取消入口
    const reset = picked
      ? '<button type="button" class="ch-skitem ch-skreset" data-reset="1">' +
        '<span class="ch-skitem-ic">↺</span>' +
        '<span class="ch-skitem-txt"><span class="ch-skitem-name">不指定技能</span>' +
        '<span class="ch-skitem-desc">改由引擎自己理解需求、按需挑技能</span></span>' +
        '<span class="ch-skitem-tag">自动调度</span></button>'
      : '';
    return reset + block('核心技能 · 通用能力', core) + block('业务技能', biz);
  }

  function renderSkList(q) {
    skList.innerHTML = skPanelHTML(q, pickedSkill, allSkills) || '<div class="ch-skempty">没有匹配的技能</div>';
    skList.querySelectorAll('.ch-skitem').forEach((n) => {
      n.addEventListener('click', () => {
        if (n.dataset.reset) { clearSkill(); setMode('auto'); closeSkPanel(); input.focus(); return; }
        const sk = allSkills.find((s) => s.slug === n.dataset.slug);
        if (sk) { pickSkill(sk); input.focus(); }
      });
    });
  }

  function skItemHtml(sk, picked) {
    const t = TYPE_META[sk.skill_type] || TYPE_META.write;
    const on = picked && picked.slug === sk.slug ? ' on' : '';
    return '<button type="button" class="ch-skitem' + on + '" data-slug="' + esc(sk.slug) + '">' +
      '<span class="ch-skitem-ic">' + (sk.is_core ? '★' : '✦') + '</span>' +
      '<span class="ch-skitem-txt">' +
        '<span class="ch-skitem-name">' + esc(sk.name || sk.slug) +
          (sk.is_core ? '<span class="ch-core">核心</span>' : '') + '</span>' +
        '<span class="ch-skitem-desc">' + esc(cardDesc(sk, t.tag)) + '</span>' +
      '</span>' +
      '<span class="ch-skitem-tag">' + esc(t.tag) + '</span>' +
    '</button>';
  }

  function pickSkill(sk, silent) {
    if (!sk) return;
    pickedSkill = { slug: sk.slug, name: sk.name || sk.slug };
    try { localStorage.setItem(SKILL_KEY, pickedSkill.slug); } catch (e) {}
    skLabel.textContent = pickedSkill.name;
    skBtn.classList.add('picked');
    if (!silent) {
      closeSkPanel();
      footnote.textContent = '已指定「' + pickedSkill.name + '」· 跳过意图识别，出结果更快';
      document.querySelectorAll('.ch-card').forEach((c) => c.classList.toggle('on', c.dataset.slug === pickedSkill.slug));
    }
    renderSkList(skSearch.value);
  }

  function clearSkill() {
    pickedSkill = null;
    try { localStorage.removeItem(SKILL_KEY); } catch (e) {}
    skLabel.textContent = '选择技能';
    skBtn.classList.remove('picked');
    document.querySelectorAll('.ch-card.on').forEach((c) => c.classList.remove('on'));
    renderSkList(skSearch.value);
  }

  function openSkPanel(nudge) {
    skPanel.classList.remove('hidden');
    skBtn.setAttribute('aria-expanded', 'true');
    loadSkillIndex();
    if (nudge) {
      skPanel.classList.add('nudge');
      setTimeout(() => skPanel.classList.remove('nudge'), 600);
      try { skSearch.focus(); } catch (e) {}
    }
  }
  function closeSkPanel() {
    skPanel.classList.add('hidden');
    skBtn.setAttribute('aria-expanded', 'false');
  }
  // 后端降级说明 → 渲染成回复开头的引用行。
  // 单独抽出来是为了能被回归测试直接调用：这句话只有一条，丢了用户就抓瞎。
  function metaNoteLine(obj) {
    if (!obj || !obj.note) return '';
    return '> ' + obj.note + '\n\n';
  }

  function toggleSkPanel() {
    if (skPanel.classList.contains('hidden')) openSkPanel(false); else closeSkPanel();
  }

  // 面板开着时，这次点击该不该把它收起来？
  // 返回 true = 收起。纯函数：入参是"点在哪"，不碰 DOM，方便回归测试。
  function panelClosesOnClick(o) {
    if (o.panelHidden) return false;                       // 本来就没开，无所谓
    if (o.inPanel || o.inPickBtn) return false;            // 点在面板/技能钮上：那不是"点外面"
    if (o.inSend || o.inInput) return false;               // 点在发送键/输入框上：见调用处注释
    return true;
  }

  function wireModes() {
    modeAuto.addEventListener('click', () => setMode('auto'));
    modeManual.addEventListener('click', () => setMode('manual'));
    skBtn.addEventListener('click', (e) => { e.stopPropagation(); toggleSkPanel(); });
    skSearch.addEventListener('input', () => renderSkList(skSearch.value));
    // 点面板外面 / 按 Esc 收起。
    // ⚠️ 这条判断必须放过"发送键"和输入框：手动模式没选技能时，submit() 正是靠
    // openSkPanel 来提示用户补选技能，而这次点击会继续冒泡到这里 —— 若把它当成
    // "点在外面"，面板刚开就被同一击关掉，用户看到的是「点了发送毫无反应」。
    // 抽成纯函数是为了能被 web/tests 跑真代码、并且突变注入能精确变红。
    document.addEventListener('click', (e) => {
      if (panelClosesOnClick({
        panelHidden: skPanel.classList.contains('hidden'),
        inPanel: skPanel.contains(e.target),
        inPickBtn: skBtn.contains(e.target),
        inSend: send.contains(e.target),
        inInput: input.contains(e.target),
      })) closeSkPanel();
    });
    document.addEventListener('keydown', (e) => { if (e.key === 'Escape') closeSkPanel(); });
  }

  /* ---------- core send ---------- */
  // 组装 /api/chat 的请求体。抽成独立纯函数是为了能被 web/tests 直接跑真代码：
  // 手动模式下 mode/skill 一旦漏发，后端会静默退回自动调度，
  // 用户"明明锁了技能"却拿到别的东西 —— 这种失败在界面上一点提示都没有。
  function chatPayload(text) {
    return JSON.stringify({
      session_id: sessionId,
      message: text,
      mode: chatMode,
      skill: (chatMode === 'manual' && pickedSkill) ? pickedSkill.slug : '',
    });
  }

  // 手动模式没选技能时不许发送：与其让后端猜，不如让用户先选一个。
  function sendBlocked(mode, picked) {
    return mode === 'manual' && !picked;
  }

  async function submit() {
    const text = input.value.trim();
    if (!text || busy) return;
    // 手动模式却没选技能 —— 不能偷偷退回自动（用户以为是"指定"的），
    // 直接打开面板并把搜索框聚焦，把这一步补上。
    if (sendBlocked(chatMode, pickedSkill)) { openSkPanel(true); return; }
    input.value = ''; input.focus(); autoGrow();
    if (welcome && !welcome.classList.contains('hidden')) welcome.classList.add('hidden');

    // persist user message + auto-title (first message, untitled session)
    const sess = activeSession();
    sess.messages.push({ role: 'user', text });
    if (sess.title === '新对话') sess.title = titleFrom(text);
    sess.updated = Date.now();
    saveStore(); router.render();

    addUser(text);
    busy = true;
    send.disabled = true;

    const bubble = addAssistant(null);
    bubble.textContent = '';
    const cursor = el('span', 'stream-cursor');
    bubble.appendChild(cursor);
    // trace panel lives above the answer bubble (inside the same right column)
    const trace = el('div', 'ch-trace hidden');
    const right = bubble.parentElement;
    right.insertBefore(trace, right.firstChild);
    setPill(null);
    keepBottom();

    let acc = '';
    try {
      await stream(bubble, text, trace);
    } catch (err) {
      bubble.classList.add('err');
      bubble.textContent = '连接失败：' + (err && err.message || err);
    } finally {
      cursor.remove();
      busy = false;
      send.disabled = false;
      // persist assistant answer (full raw markdown) after streaming finishes
      const full = bubble.dataset.md || '';
      const msg = { role: 'assistant', text: full, skill: trace?.dataset?.skill || null };
      if (pendingFiles.length) msg.files = pendingFiles.slice();
      pendingFiles = [];
      if (full) {
        const s = activeSession();
        s.messages.push(msg);
        s.updated = Date.now();
        saveStore(); router.render(); prune();
      }
    }
  }

  async function stream(bubble, text, trace) {
    const res = await fetch('/api/chat', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: chatPayload(text),
    });
    if (!res.ok || !res.body) {
      const t = await res.text().catch(() => '');
      throw new Error((t && t.slice(0, 120)) || ('HTTP ' + res.status));
    }

    const reader = res.body.getReader();
    const dec = new TextDecoder();
    let buf = '';

    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let idx;
      while ((idx = buf.indexOf('\n\n')) >= 0) {
        const block = buf.slice(0, idx); buf = buf.slice(idx + 2);
        handleEvent(block, bubble, trace);
      }
    }
  }

  function handleEvent(block, bubble, trace) {
    const lines = block.split('\n');
    let ev = '', data = '';
    for (const l of lines) {
      if (l.startsWith('event:')) ev = l.slice(6).trim();
      else if (l.startsWith('data:')) data = l.slice(5).trim();
    }
    if (!data) return;
    let obj; try { obj = JSON.parse(data); } catch (e) { return; }

    switch (ev) {
      case 'skill':
        if (obj.skill) setPill(obj.skill);
        if (obj.name && trace) trace.dataset.skill = obj.name;
        if (obj.type && trace) trace.dataset.stype = obj.type;
        break;
      case 'trace':
        if (trace && Array.isArray(obj)) renderTrace(trace, obj);
        break;
      case 'meta':
        if (obj.text) appendText(bubble, obj.text + '\n');
        // 降级说明要走在 trace 判断外面。挂在里面的话，只要轨迹面板没渲染出来
        // （换布局/元素缺失），"你锁的技能没了、已改用自动调度"这句话就被静默吞掉，
        // 用户只会看到结果不对，永远不知道该怪谁。
        if (metaNoteLine(obj)) appendText(bubble, metaNoteLine(obj));
        if (trace) {
          // 手动/自动要贯穿整轮：轨迹面板的角色徽章按它切换措辞。
          if (obj.mode) trace.dataset.mode = obj.mode;
          if (obj.skill) {
            trace.dataset.skill = obj.skill;
          } else if (obj.intent === 'query') {
            trace.dataset.skill = '通用办事查询';
          } else if (obj.intent === 'write') {
            trace.dataset.skill = '通用写作';
          }
        }
        break;
      case 'delta':
        appendText(bubble, obj.t || '');
        break;
      case 'file':
        // 文件永远以独立、可见的对话消息出现；绝不埋进可能折叠的 trace 面板。
        pendingFiles.push(obj);
        appendFileLink(bubble, obj);
        break;
      case 'needs':
        setPill(obj.skill || null);
        break;
      case 'done':
        flushMarkdown(bubble);
        bubble.querySelectorAll('.stream-cursor').forEach((c) => c.remove());
        break;
      case 'error':
        bubble.classList.add('err');
        appendText(bubble, '⚠ ' + (obj.error || '出错了'));
        break;
    }
  }

  function appendText(bubble, t) {
    // Markdown streaming renderer (simple + correct): accumulate the FULL raw
    // markdown in bubble.data-md, re-render EVERYTHING with marked on each
    // delta, keep the blinking cursor appended. Full re-parse per delta avoids
    // block-boundary bugs that would drop middle chunks of long answers.
    // marked is fast enough for message-length text; correctness wins.
    let full = bubble.dataset.md || '';
    full += t;
    bubble.dataset.md = full;

    const html = marked.parse(full, { breaks: true, gfm: true });
    bubble.innerHTML = html + '<span class="stream-cursor"></span>';
    keepBottom();
  }

  // Called on the 'done' event: final re-render without the blinking cursor.
  // Keeps data-md so submit() can persist the FULL raw markdown afterwards.
  function flushMarkdown(bubble) {
    const full = bubble.dataset.md || '';
    if (!full) return;
    const html = marked.parse(full, { breaks: true, gfm: true });
    bubble.innerHTML = html;
  }

  /* ---------- reasoning trace panel ---------- */
  // Renders the orchestrator's decision pipeline above an answer:
  // phase → dedicated sub-agent role shown as a badge on each stage, so the
  // multi-agent pipeline (understand → retrieve → extract → write) is visible.
  function appendFileLink(_, obj) {
    // 文件以独立、可见的 assistant 侧消息出现（有头像、不受气泡/折叠面板影响）
    const wrap = el('div', 'ch-msg assistant');
    const av = avatarNode('a');
    const right = el('div');
    right.style.flex = '1';
    const box = el('div', 'atx');
    box.innerHTML = downloadLinkHtml(obj);
    right.appendChild(box);
    wrap.appendChild(av); wrap.appendChild(right);
    col.appendChild(wrap);
    keepBottom();
  }

  function fileIconSVG(name) {
      // 极简单色纸片图标 + 等宽类型徽标。黑白灰统一，不用 Office 彩色，与 Linear 系设计一致。
      let tag = 'FILE', sub = '';
      if (/\.(docx?|doc)$/i.test(name||'')) tag = 'WORD';
      else if (/\.pdf$/i.test(name||''))    tag = 'PDF';
      else if (/\.pptx?$/i.test(name||''))  tag = 'PPT';
      else if (/\.xlsx?$/i.test(name||''))  tag = 'XLSX';
      else if (/\.(txt|md)$/i.test(name||'')) tag = 'TXT';
      if (tag.length > 3) sub = tag; // 短标签放图标内，长标签用 3 字母缩略
      const letter = (sub ? sub.slice(0,3) : tag);
      return '<svg class="atx-ic-svg" viewBox="0 0 32 32" fill="none" xmlns="http://www.w3.org/2000/svg">' +
               '<path d="M8 2h12l6 6v20a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2z" ' +
                 'stroke="currentColor" stroke-width="1.4" fill="var(--panel)"/>' +
               '<path d="M20 2v6h6" stroke="currentColor" stroke-width="1.4" fill="none"/>' +
               '<text x="16" y="21" text-anchor="middle" font-size="6.5" ' +
                 'font-family="' + 'ui-monospace,SFMono-Regular,Menlo,monospace' + '" ' +
                 'font-weight="700" fill="var(--text-dim)">' + letter + '</text>' +
             '</svg>';
    }

    function downloadLinkHtml(obj) {
      const name = esc(obj.name || '文件');
      const url = obj.url || '#';
      const isGen = obj.kind === 'gen';
      return '<a class="atx-link" href="' + esc(url) + '" download>' +
               '<span class="atx-ic">' + fileIconSVG(obj.name) + '</span>' +
               '<span class="atx-meta">' +
                 '<span class="atx-name">' + name + '</span>' +
                 '<span class="atx-hint">' + (isGen ? '已生成文档' : '可用模板') + '</span>' +
               '</span>' +
               '<span class="atx-dl">' +
                 '<svg viewBox="0 0 16 16" fill="none" xmlns="http://www.w3.org/2000/svg">' +
                   '<path d="M8 2v7m0 0L4.5 6.5M8 9l3.5-2.5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/>' +
                   '<path d="M3 11.5v1A1.5 1.5 0 0 0 4.5 14h7a1.5 1.5 0 0 0 1.5-1.5v-1" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/>' +
                 '</svg>' +
                 '<span>下载</span>' +
               '</span>' +
             '</a>';
    }

  const AGENTS = {
    analyze:  { n: '1', role: '意图分析', act: '理解你的请求' },
    match:    { n: '2', role: '技能检索', act: '匹配写作技能' },
    params:   { n: '3', role: '要素提炼', act: '抽取写作要素' },
    generate: { n: '4', role: '内容执笔', act: '起草生成内容' },
  };
  // Type-aware sub-agent roles: when the orchestrator dispatches a query/flow
  // skill (办事查询), the panel badges read as flow-steering, not writing.
  const AGENTS_QUERY = {
    analyze:  { n: '1', role: '意图分析', act: '理解你的问题' },
    match:    { n: '2', role: '流程检索', act: '匹配办事流程' },
    params:   { n: '3', role: '要素提炼', act: '提取办理信息' },
    generate: { n: '4', role: '流程执笔', act: '给出办事步骤' },
  };
  // 手动模式（用户点名下技能）——第一步不该再自称"意图分析"，
  // 面板要如实说明"这一跳被跳过了"，否则和右边写着"跳过意图分析"的文案自相矛盾。
  const AGENTS_MANUAL = {
    analyze:  { n: '1', role: '指定技能', act: '沿用手动选择' },
    match:    { n: '2', role: '载入能力', act: '注入提示词与模板' },
    params:   { n: '3', role: '参数校对', act: '核对要素是否齐备' },
    generate: { n: '4', role: '直接执行', act: '按该技能的约定处理' },
  };
  const MARK = {
    done: '<span class="ctk-dot ok">✓</span>',
    active: '<span class="ctk-dot live"></span>',
    waiting: '<span class="ctk-dot wait">…</span>',
    pending: '<span class="ctk-dot"></span>',
  };

  // "AI 调度台" — an open timeline workbench showing how the orchestrator
  // decomposes the user's request, picks a skill and walks it step by step.
  // First trace event builds the DOM (open by default); later events just
  // refresh the header/status and step states in place.
  function renderTrace(trace, steps) {
    trace.classList.remove('hidden');
    const skillName = trace.dataset.skill || '通用写作';
    const doneCount = steps.filter((s) => s.status === 'done').length;
    const active = steps.find((s) => s.status === 'active');
    const waiting = steps.find((s) => s.status === 'waiting');
    const live = active || waiting;
    const allDone = doneCount === steps.length && steps.length > 0;

    // ---- header ----
    let head = trace.querySelector('.ch-tr-head');
    if (!head) {
      head = el('div', 'ch-tr-head');
      head.innerHTML =
        '<span class="ch-tr-chev">▸</span>' +
        '<span class="ch-tr-head-title">AI 调度台</span>' +
        '<span class="ch-tr-pill">' + esc(skillName) + '</span>' +
        '<span class="ch-tr-status"></span>';
      head.addEventListener('click', () => {
        const body = trace.querySelector('.ch-tr-body');
        const isOpen = trace.classList.toggle('open');
        // 记下用户的手动选择：之后自动开合一律让位，别跟用户抢控制权
        trace.dataset.manual = isOpen ? 'open' : 'close';
        if (body) body.style.display = isOpen ? 'block' : 'none';
        head.querySelector('.ch-tr-chev').textContent = isOpen ? '▾' : '▸';
      });
      const body = el('div', 'ch-tr-body');
      body.style.display = 'none';
      trace.classList.remove('open');
      trace.appendChild(head); trace.appendChild(body);
    }
    // status badge: live pulse while running, ✓ count when done
    const statusEl = head.querySelector('.ch-tr-status');
    const pills = head.querySelector('.ch-tr-pill');
    pills.innerHTML = esc(skillName);
    pills.classList.toggle('miss', !trace.dataset.skill);
    if (allDone) {
      statusEl.className = 'ch-tr-status ok';
      statusEl.innerHTML = '✓ 完成';
    } else if (live) {
      statusEl.className = 'ch-tr-status live';
      statusEl.innerHTML = '<span class="ctk-pulse"></span>' + esc(live.detail || ''); 
    } else {
      statusEl.className = 'ch-tr-status';
      statusEl.textContent = doneCount + '/' + steps.length;
    }

    // 运行中自动展开时间线、结束后自动收起。用户反馈过「问完以后看不到中间步骤，
    // 一直转圈然后直接出答案」，所以过程默认必须可见；跑完收起保持界面清爽。
    // 手动点过表头的（dataset.manual）以用户为准，不再自动干预。
    if (trace.dataset.manual !== 'open' && trace.dataset.manual !== 'close') {
      const shell = trace.querySelector('.ch-tr-body');
      const wantOpen = !!live;
      trace.classList.toggle('open', wantOpen);
      if (shell) shell.style.display = wantOpen ? 'block' : 'none';
      const chev = head.querySelector('.ch-tr-chev');
      if (chev) chev.textContent = wantOpen ? '▾' : '▸';
    }

    // ---- step timeline ----
    let body = trace.querySelector('.ch-tr-body');
    body.innerHTML = '';
    steps.forEach((s, i) => {
      const st = s.status || 'pending';
      const row = el('div', 'ctk-step ' + st + (i === steps.length - 1 ? ' last' : ''));
      const agents = trace.dataset.mode === 'manual'
        ? AGENTS_MANUAL
        : ((trace.dataset.stype === 'query' || trace.dataset.stype === 'template') ? AGENTS_QUERY : AGENTS);
      const agent = agents[s.phase] || { role: '', act: '' };
      const mark = MARK[st] || MARK.pending;
      row.innerHTML =
        '<span class="ctk-rail">' +
          '<span class="ctk-node">' + mark + '</span>' +
          (i < steps.length - 1 ? '<span class="ctk-line"></span>' : '') +
        '</span>' +
        '<span class="ctk-cell">' +
          '<span class="ctk-label-row">' +
            '<span class="ctk-num">' + (agent.n || s.phase) + '</span>' +
            '<span class="ctk-label">' + esc(s.label || s.detail || '') + '</span>' +
            '<span class="ctk-agent' + (s.phase === 'generate' ? ' brand' : '') + '">' + esc(agent.role) + '</span>' +
          '</span>' +
          '<span class="ctk-detail">' + esc(s.detail || '') + '</span>' +
        '</span>';
      body.appendChild(row);
    });
    keepBottom();
  }

  boot();
})();