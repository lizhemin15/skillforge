/* SkillForge — conversation UI */
(() => {
  const $ = (s) => document.querySelector(s);
  const scroll = $('#chat-scroll'), col = $('#chat-col');
  const input = $('#chat-input'), send = $('#chat-send');
  const welcome = $('#welcome');
  // 全页唯一的"多入口"：输入框上方那一条推荐行。内容由 renderChips() 按对话状态现算，
  // 不再有技能卡片网格 / 技能下拉面板 / 搜索框 / 脚注那四套并存的入口。
  const chipsBox = $('#chips');
  const chipsWrap = $('#chips-wrap');
  const inputBar = $('.ch-input-bar');
  // 技能勾选层（「指定技能」档的下拉面板）
  const skLayer = $('#sk-layer'), skQ = $('#sk-q'), skList = $('#sk-list'), skFoot = $('#sk-foot');
  const switchBox = $('#ch-switch');
  const modeAuto = $('#mode-auto'), modeManual = $('#mode-manual');
  const thumb = $('#ch-switch-thumb');

  // —— 两种对话方式 ——
  // auto   : 让引擎自己理解意图，从技能库里挑（默认，适合"我也不知道该用哪个"）
  // manual : 用户点名技能，后端整跳过一次意图分类 —— 不只是更准，也快得多
  const MODE_KEY = 'skillforge.chatmode';
  const SKILL_KEY = 'skillforge.chatskill';
  let chatMode = 'auto';
  let pickedSkill = null;   // { slug, name } —— manual 模式下锁定发送的技能
  let allSkills = [];       // /api/skills 缓存（推荐行与技能候选共用）
  let skFetched = false;
  // 推荐行的运行态：轮次、上一轮说了什么、上一轮模型回了什么、上一轮用了哪个技能、
  // 是否在等用户补信息、产出物。这些全部喂给 chipPlan() 现算"现在能干哪几件事"。
  const flow = { turns: 0, lastUser: '', lastReply: '', lastSkill: '', askedBack: false, hasFile: false };
  let chipToken = 0;        // 丢弃过期的一次 LLM 精修响应（连发几轮时防止旧结果覆盖新界面）

  const PLACEHOLDER = {
    auto: '说说你想写什么…（Enter 发送，Shift+Enter 换行）',
    manual: '把材料和要求直接写在这里…（Enter 发送，Shift+Enter 换行）',
  };
  // 推荐行的抬头也得跟档走：自动档说"我推荐"，手动档不能说"我推荐" ——
  // 手动档是用户在点名技能，此时抬头的职责是提醒他"先选一个"。
  const CHIP_HINT = {
    auto: '试试',
    manual: '指定技能',
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

    renderChips();
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

  /* ---------- 推荐行：全页唯一的入口 ----------
     上一版把入口铺满整屏：12 张技能卡（点开还有填空表单）+ 模式按钮 + 技能下拉 +
     搜索框 + 脚注。用户第一眼不知道该动哪个。这一版只留一条推荐行，而且它的内容
     不是静态菜单 —— 由 chipPlan() 按对话状态现算"现在能干哪几件事"：

       没说过话     → 两条通用问话 + 两个通用技能（最该先看见的）
       模型在追问   → "就按你的思路写"
       已有产出     → "再精简一半""换个更正式的语气"
       上一轮用了X  → "继续用「X」"（点了直接切到指定技能档）
       指定技能档   → 这一行变成技能候选，选了之后变成该技能的示范问句

     chipPlan 是纯函数：状态进、清单出，不碰 DOM。推荐得对不对能直接被回归测试
     喂状态验证，不用靠人肉点界面看。 */
  function chipPlan(st) {
    const s = st || {};
    const skills = Array.isArray(s.skills) ? s.skills : [];
    const core = skills.filter((k) => k.is_core);
    const biz = skills.filter((k) => !k.is_core);
    const mode = s.mode === 'manual' ? 'manual' : 'auto';
    const picked = s.picked || null;
    const turns = Number(s.turns) || 0;
    const items = [];
    const cap = 4;

    // —— 指定技能档 ——
    // 这一档没选技能就发不出去（见 sendBlocked），所以推荐行必须给出入口。
    // 入口形态是一颗「打开勾选层」的胶囊，而不是技能候选本身：
    //   技能库有几十上百个，推荐行只有 4 颗的位置，铺在这里 = 后面的技能永远点不到。
    //   （上一版就是这么坏掉的：只列前 4 个，用户报"都没法选指定技能"。）
    if (mode === 'manual') {
      if (picked) {
        const nm = picked.name || picked.slug;
        items.push({
          kind: 'skillpick',
          label: '技能：' + nm,
          title: '点开技能列表，可换一个或取消指定',
        });
        // 锁定了技能，此时最有用的是"用它写点什么"：给示范问句而不是技能列表
        askedFor(picked).forEach((a) => items.push(a));
        return { hint: '已指定', items: items.slice(0, cap) };
      }
      items.push({ kind: 'skillpick', label: '选择技能 ▾', title: '展开技能列表，勾一个来指定' });
      return { hint: CHIP_HINT.manual, items: items };
    }

    // —— 自动调度档 · 空态 ——
    if (turns === 0) {
      // 前两条是纯文本问话：不依赖技能库，后端还没 seed 完也能点。
      items.push({ kind: 'ask', label: '写一段产品介绍', send: '写一段给客户看的产品介绍，200 字左右' });
      items.push({ kind: 'ask', label: '做一份会议纪要', send: '帮我做一份会议纪要模板，导出成 Word 文件' });
      core.slice(0, cap - items.length).forEach((k) => items.push(askFor(k)));
      return { hint: CHIP_HINT.auto, items: items };
    }

    // —— 自动调度档 · 对话中 ——
    // 优先级：先把"模型正等你回话"这条顶上去 —— 那是当下唯一能推进的事。
    if (s.askedBack) {
      items.push({ kind: 'ask', label: '就按你的思路写', send: '按你的思路先出一版，缺的信息我后面补' });
    }
    if (s.hasFile) {
      items.push({ kind: 'ask', label: '再精简一半', send: '把上面这份内容压缩到一半长度，保留关键信息' });
      items.push({ kind: 'ask', label: '换个更正式的语气', send: '语气改得更正式一些，适合直接发给客户' });
    }
    if (s.lastSkill) {
      items.push({ kind: 'again', slug: s.lastSkill, label: '继续用这个技能', title: '切到指定技能档并锁定「' + s.lastSkill + '」' });
    }
    // 不管聊到哪一步，"再短一点 / 换个开头"永远是合理的下一步
    if (items.length < cap) items.push({ kind: 'ask', label: '再短一点', send: '把上面的内容再压缩一些，保留结论' });
    if (items.length < cap) items.push({ kind: 'ask', label: '换个开头', send: '换一个更有吸引力的开头重写，其余保持不变' });
    return { hint: CHIP_HINT.auto, items: items.slice(0, cap) };
  }

  // 一个技能 → 一颗可点的推荐问话。label 必须短（一行塞得下 4 颗），
  // 真正发出去的句子放 send，悬停可见 —— 用户点之前就知道会发生什么。
  function askFor(sk) {
    const q = quickExample(sk) || ('用「' + (sk.name || sk.slug) + '」帮我写一段内容');
    return { kind: 'ask', label: sk.name || sk.slug, send: q, title: q };
  }
  function askedFor(picked) {
    if (!picked) return [];
    const sk = (allSkills || []).find((k) => k.slug === picked.slug) || picked;
    const a = askFor(sk);
    // ⚠️ 锁定技能后，这一行里已经躺着一颗「✓ 技能名」了（取消指定）。
    // 再挂一颗同名 chip，用户根本分不清哪颗是取消、哪颗是发送 ——
    // 实测就是「✓ 办公文档管家」+「办公文档管家」并排，纯噪声。
    // 所以这里的标签必须换成**点下去会发出去的那句话**。
    a.label = shortAskLabel(a.send, sk.name || sk.slug);
    return [a];
  }

  // chip 是一行胶囊，标签太长就把输入框顶下去。全句照发（title 里有全文）。
  function shortAskLabel(q, name) {
    let t = String(q || '').replace(/\s+/g, ' ').trim();
    // quickExample 的兜底形态是「（用「技能名」标签：描述）」：
    // 整串糊在胶囊上又长又绕，取冒号后面的描述才是"点了会发生什么"。
    if (t.startsWith('（')) {
      const seg = t.replace(/^（/, '').replace(/）$/, '').split('：');
      t = seg.length > 1 ? seg.slice(1).join('：') : t;
    }
    const p = '用「' + name + '」';
    if (name && t.startsWith(p)) t = t.slice(p.length);
    t = t.trim();
    return t.length > 20 ? t.slice(0, 19) + '…' : t;
  }


  function chipNode(c) {
    const b = el('button', 'ch-sug'
      + (c.kind === 'pick' ? ' is-pick' : '')
      + (c.kind === 'pick' && c.core ? ' is-core' : '')
      + (c.kind === 'unpick' ? ' is-on' : '')
      // 「选择技能 ▾」：虚线表示"还能选"，展开中点亮，跟已指定状态视觉上分开
      + (c.kind === 'skillpick' ? ' is-pick' : '')
      + (c.kind === 'skillpick' && skOpen ? ' is-on' : '')
      + (c.kind === 'again' ? ' is-pick' : ''));
    b.type = 'button';
    b.textContent = c.label;
    if (c.title) b.title = c.title;
    b.dataset.kind = c.kind;
    if (c.slug) b.dataset.slug = c.slug;
    if (c.send) b.dataset.send = c.send;
    b.addEventListener('click', () => runChip(c));
    return b;
  }

  function runChip(c) {
    if (!c) return;
    if (c.kind === 'pick') {
      const sk = (allSkills || []).find((k) => k.slug === c.slug);
      if (sk) pickSkill(sk);
      input.focus();
      return;
    }
    if (c.kind === 'unpick') { clearSkill(); input.focus(); return; }
    // 技能勾选层：这一颗不是"发一句话"，是"开一个面板"，绝不能落到下面的 ask 分支 ——
    // 落到那里就会把「选择技能」四个字填进输入框然后发出去，变成一句对模型毫无意义的请求。
    if (c.kind === 'skillpick') { toggleSkLayer(); return; }
    if (c.kind === 'again') {
      const sk = (allSkills || []).find((k) => k.slug === c.slug)
        || { slug: c.slug, name: c.slug };
      pickSkill(sk);
      setMode('manual');
      input.focus();
      return;
    }
    // ask：**只把这句话填进输入框，不直接发**。
    // 上一版是点了就发 —— 看着省事，实际是逼用户"要么接受这个句子，要么等它写完再让它重写"，
    // 想改个字数/语气都得白烧一次模型调用。现在光标落在末尾，接着打字就是改。
    // 没有文案的 chip 一律当无效：input.value = undefined 会把字面量 "undefined"
    // 写进输入框，submit() 再把它当用户说的话发出去 —— 一个空 chip 能发出一条假消息。
    const q = c.send || c.label;
    if (!q) return;
    setInput(q);
    toEnd(input);
    input.focus();
  }

  // 光标送到末尾：填完就是接着改，不是让人再按一下 End。
  // 测试沙箱里的 input 是裸对象（没有 setSelectionRange），所以这里必须容错。
  function toEnd(node) {
    try {
      if (node && typeof node.setSelectionRange === 'function') {
        const n = String(node.value || '').length;
        node.setSelectionRange(n, n);
      }
    } catch (e) {}
  }

  // 把当前状态渲染成推荐行。hint 用一个不起眼的小字标签，不抢视觉。
  function renderChips(extra) {
    if (!chipsBox) return;
    const plan = chipPlan(Object.assign({
      skills: allSkills,
      mode: chatMode,
      picked: pickedSkill,
      turns: flow.turns,
      askedBack: flow.askedBack,
      hasFile: flow.hasFile,
      lastSkill: flow.lastSkill,
    }, extra || {}));
    chipsBox.innerHTML = '';
    if (plan.hint && plan.items.length) {
      chipsBox.appendChild(el('span', 'ch-chips-h', plan.hint));
    }
    plan.items.forEach((c) => chipsBox.appendChild(chipNode(c)));
    return plan;
  }

  // —— LLM 精修 ——
  // 规则版是"保底且瞬时"的：先把上面那几条渲染出去（用户零等待），
  // 再异步问后端"就这段对话，接下来最可能想干什么"，回来若有更像样的建议就换上去。
  // 失败/超时一律保留规则版 —— 推荐行宁愿平庸，也不能空着或闪。
  function refineChips() {
    if (chatMode !== 'auto' || pickedSkill) return;
    if (!flow.turns) return;
    const token = ++chipToken;
    let payload;
    try {
      payload = JSON.stringify({
        session_id: sessionId,
        last_user: flow.lastUser,
        last_reply: flow.lastReply.slice(0, 600),
        used_skill: flow.lastSkill || '',
        skills: (allSkills || []).slice(0, 12).map((k) => ({ slug: k.slug, name: k.name })),
      });
    } catch (e) { return; }
    const ctl = (typeof AbortController === 'function') ? new AbortController() : null;
    // 10.5s：必须比服务端那个 9s ctx 长一点点（见 internal/api/router.go 的注释）。
    // 客户端先掐断的话，服务端就算答出来了也没人接，表现和网络错误一样 ——
    // 而这层失败是完全静默的（退回规则版胶囊），没人会想到是「谁先超时」的问题。
    // 服务端最多 3 次尝试（换开关 / 放大预算重试），别按「一次请求」估这个数。
    const timer = setTimeout(() => { if (ctl) ctl.abort(); }, 10500);
    fetch('/api/chat/suggest', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: payload,
      signal: ctl ? ctl.signal : undefined,
    })
      .then((r) => (r.ok ? r.json() : Promise.reject()))
      .then((d) => {
        clearTimeout(timer);
        if (token !== chipToken) return;              // 已经又聊了一轮，这批建议过期了
        if (chatMode !== 'auto' || pickedSkill) return;
        const list = ((d && d.chips) || [])
          .filter((x) => x && x.label)
          .map((x) => ({ kind: 'ask', label: String(x.label).slice(0, 20), send: String(x.send || x.label) }))
          .slice(0, 4);
        if (!list.length) return;                     // 模型没给出东西：保留规则版
        if (!chipsBox) return;
        chipsBox.innerHTML = '';
        chipsBox.appendChild(el('span', 'ch-chips-h', '接下来'));
        list.forEach((c) => chipsBox.appendChild(chipNode(c)));
        chipsBox.classList.add('refined');
      })
      .catch(() => { clearTimeout(timer); });
  }

  // 模型是否在问用户要东西？—— 推荐行据此优先给"补上它"的选项，
  // 而不是继续塞新任务：用户正被反问时，最该点的就是回答那个问题。
  function asksBack(reply) {
    const t = String(reply || '').trim();
    if (!t) return false;
    if (/[?？]\s*$/.test(t)) return true;
    return /(请|麻烦|需要你|希望你)(补充|提供|告诉|明确|确认|发一下|给一下|上传)/.test(t);
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

  // --- composer:stick-begin（回归测试按这两个标记切片抽取，改动请勿删标记）---
  let stickRaf = 0;   // 合并补帧请求：流式期间每帧都会调 keepBottom，只排一次补帧
  /* 贴底两条铁律（缺一条用户就看到「不自动往下滚」）：
     ① 容器**不能**是 scroll-behavior: smooth —— 平滑滚动每次赋值都会重启一段动画，
        而流式输出每几十毫秒就赋值一次，滚动永远追不上正文（实测落后 994px，
        约三分之二答案在视口外）。见 style.css 里 `.ch-scroll` 的注释。
     ② 赋值之后必须**再补一帧**：markdown 重排 / 字体图片落地会在本次赋值之后
        继续改变 scrollHeight，只赋一次就会停在中途（观感是「差一行没到底」）。 */
  function keepBottom() {
    scroll.scrollTop = scroll.scrollHeight;
    // 必须 `window.requestAnimationFrame(...)` 这样带接收者调用：拆成
    // `const raf = window.requestAnimationFrame; raf(cb)` 在 Chrome 上会
    // Illegal invocation（与 document.querySelector 同类）。
    if (typeof window.requestAnimationFrame !== 'function' || stickRaf) return;
    stickRaf = window.requestAnimationFrame(function () {
      stickRaf = window.requestAnimationFrame(function () {
        stickRaf = 0;
        scroll.scrollTop = scroll.scrollHeight;
      });
    });
  }
  // --- composer:stick-end ---

  // 引擎这一轮挑中的技能：不再有常驻胶囊显示它（消息气泡里本来就有「正在使用技能」），
  // 这里只记进状态，供推荐行下一轮算"继续用这个技能"。
  function setPill(name) { flow.lastSkill = name || ''; }

  /* ---------- 对话方式切换 ---------- */
  // setMode 只做三件事：改状态、把视觉同步过去、把推荐行重算。
  // 技能选择不再有独立面板 —— 推荐行本身就是候选列表（见 chipPlan 的 manual 分支）：
  // 少一个浮层、少一套"点外面收起"的判断，也少一处能不一致的状态。
  // 选中态的类名**只能有一个来源**，且必须与 CSS 里的选择器逐字一致：
  // style.css 写的是 `.ch-switch-opt.is-on`。
  // ⚠️ 这里曾经 toggle 的是 'on' —— 滑块照滑、aria 照改，但文字选中态一动不动，
  // 而 HTML 预置的 is-on 谁也摘不掉，看着就永远停在「自动调度」上：
  // 界面不报错、控制台干净，只有人眼能发现。测试见 chat_modes.test.mjs 的
  // 「选中类名必须与 CSS 对得上」一节（改一侧不改另一侧就变红）。
  const ON_CLASS = 'is-on';
  function paintMode(mode, auto, manual) {
    auto.classList.toggle(ON_CLASS, mode === 'auto');
    manual.classList.toggle(ON_CLASS, mode === 'manual');
    auto.setAttribute('aria-selected', String(mode === 'auto'));
    manual.setAttribute('aria-selected', String(mode === 'manual'));
  }

  function setMode(m, silent) {
    chatMode = m === 'manual' ? 'manual' : 'auto';
    try { localStorage.setItem(MODE_KEY, chatMode); } catch (e) {}
    if (switchBox) switchBox.dataset.mode = chatMode;   // 滑块位置由 CSS 读这个属性
    paintMode(chatMode, modeAuto, modeManual);
    syncThumb();
    input.placeholder = PLACEHOLDER[chatMode];
    // 切回自动档时层必须收掉：那一档不需要指定技能，层挂在屏幕上只会误导
    if (chatMode === 'auto') closeSkLayer();
    renderChips();
    if (chatMode === 'manual' && !pickedSkill && !silent) nudgeChips();
  }

  // 滑块几何：thumb 的宽度与位移都按两个按钮的实测位置算。
  // 两档文字宽度不同，写死 50% 会在字体回退/窄屏下错位。
  function syncThumb() {
    if (!thumb || !modeAuto || !modeAuto.offsetWidth) return;
    const from = chatMode === 'auto' ? modeAuto : modeManual;
    thumb.style.width = from.offsetWidth + 'px';
    thumb.style.transform = 'translateX(' + (from.offsetLeft - modeAuto.offsetLeft) + 'px)';
  }

  // 手动档还没选技能 → 推荐行抖一下 + 高亮。
  // 这是"点了发送毫无反应"的唯一解药：必须让用户立刻知道还差一步，就差在这一行里。
  // 先 renderChips()：此刻这一行应该已经是"技能候选清单"（chipPlan 的 manual 分支），
  // 保证抖的那一行就是用户该点的那一行。
  function nudgeChips() {
    if (!chipsBox) { input.focus(); return; }
    renderChips();
    chipsBox.classList.remove('need');
    void chipsBox.offsetWidth;        // 强制重排，连按两次也能重放动画
    chipsBox.classList.add('need');
    setTimeout(() => chipsBox.classList.remove('need'), 1600);
    input.focus();
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
      renderChips();
      syncThumb();   // 索引/字体就位后再校一次滑块
      if (skOpen) renderSkLayer();
    });
    wireSkLayer();
  }
  let pendingSlug = '';

  function loadSkillIndex() {
    if (skFetched) return Promise.resolve(allSkills);
    return fetch('/api/skills')
      .then((r) => (r.ok ? r.json() : Promise.reject()))
      .then((data) => {
        // 核心技能（通用能力）排最前：推荐行只显示前 4 个，顺序就是"先看见谁"。
        // 不靠后端排序兜底 —— 后端多塞一个技能就可能把通用能力挤出屏幕。
        allSkills = ((data && data.skills) || []).slice()
          .sort((a, b) => (b.is_core ? 1 : 0) - (a.is_core ? 1 : 0));
        skFetched = true;
        renderChips();
        // 层开着时技能库才到货（用户手快）：列表得补上，否则面板一直是"加载中"
        if (skOpen) renderSkLayer();
        return allSkills;
      })
      .catch(() => allSkills);
  }

  function pickSkill(sk, silent) {
    if (!sk) return;
    pickedSkill = { slug: sk.slug, name: sk.name || sk.slug };
    try { localStorage.setItem(SKILL_KEY, pickedSkill.slug); } catch (e) {}
    renderChips();
  }

  function clearSkill() {
    pickedSkill = null;
    try { localStorage.removeItem(SKILL_KEY); } catch (e) {}
    renderChips();
  }

  /* ---------- 技能勾选层（「指定技能」档的下拉面板） ----------
     三层分工，全部可测：
       · orderSkills / skillMatches / filterSkills / togglePick 是**纯函数** ——
         状态进、清单出，不碰 DOM。回归测试直接喂一份技能清单就能断言
         "技能库里的 13 个是不是都在面板里、搜索是不是真过滤了、再勾一次会不会取消"，
         不需要人去点界面（人肉点界面 = 这条防线等于没有）。
       · renderSkLayer 只把上面的结果铺成 DOM。
       · open/close/toggle 管可见性。 */

  // 核心技能（通用能力）排最前：用户找"办公文档管家"的频率远高于找某个业务技能。
  // 不依赖后端排序 —— 后端哪天多塞一个技能就可能把通用能力挤出屏幕。
  function orderSkills(list) {
    return (list || []).slice().sort((a, b) => (b && b.is_core ? 1 : 0) - (a && a.is_core ? 1 : 0));
  }

  // 搜什么：名字、slug、用途说明。用户在输入框里想到的往往是"合同"这种词，
  // 而它多半躺在 description 里 —— 只搜名字会搜不到（然后用户以为技能没了）。
  function skillMatches(sk, q) {
    const needle = String(q || '').trim().toLowerCase();
    if (!needle) return true;
    if (!sk) return false;
    const hay = [sk.name, sk.slug, sk.description]
      .map((v) => String(v == null ? '' : v)).join(' ').toLowerCase();
    return hay.indexOf(needle) >= 0;
  }

  function filterSkills(list, q) {
    return orderSkills(list).filter((sk) => skillMatches(sk, q));
  }

  // 勾选语义 = 单选 + 可取消：点已勾的那颗 → 取消（返回空串），点别的 → 换过去。
  // 为什么是单选而不是多选：后端 /api/chat 只吃一个 skill 字段（锁定单个技能、跳过意图分类）。
  // 面板做成多选就等于给出一个后端根本不兑现的承诺 —— 勾三个只会有一个生效，用户还查不出原因。
  function togglePick(currentSlug, slug) {
    if (!slug) return currentSlug || '';
    return currentSlug === slug ? '' : slug;
  }

  let skOpen = false;                  // 勾选层当前是否开着
  function renderSkLayer() {
    if (!skList) return;
    const q = skQ ? skQ.value : '';
    const list = filterSkills(allSkills, q);
    skList.innerHTML = '';
    if (!list.length) {
      const empty = el('div', 'ch-sklayer-empty');
      // 两种"空"要分开说：技能库真为空 / 只是搜索没命中。
      // 混成一句话的话，用户在搜索框里打错一个字就会以为技能被删了。
      empty.textContent = (allSkills && allSkills.length)
        ? '没有匹配「' + String(q).trim() + '」的技能' : '技能库还在加载，稍等一下';
      skList.appendChild(empty);
    } else {
      list.forEach((sk) => {
        const on = !!pickedSkill && pickedSkill.slug === sk.slug;
        const row = el('button', 'ch-skrow' + (on ? ' is-on' : ''));
        row.type = 'button';
        row.setAttribute('role', 'option');
        row.setAttribute('aria-selected', on ? 'true' : 'false');
        if (sk.slug) row.dataset.slug = sk.slug;
        const box = el('span', 'ch-skbox');
        box.setAttribute('aria-hidden', 'true');
        const main = el('span', 'ch-skmain');
        const nm = el('span', 'ch-skname');
        nm.textContent = sk.name || sk.slug || '';
        main.appendChild(nm);
        if (sk.is_core) {
          const tag = el('span', 'ch-sktag');
          tag.textContent = '通用';
          main.appendChild(tag);
        }
        if (sk.description) {
          const d = el('span', 'ch-skdesc');
          d.textContent = sk.description;
          main.appendChild(d);
        }
        row.appendChild(box);
        row.appendChild(main);
        row.addEventListener('click', () => chooseSkill(sk));
        skList.appendChild(row);
      });
    }
    if (skFoot) {
      // 页脚只干一件事：告诉用户"勾了之后会怎样"。不写这句话，面板看起来像个多选过滤器。
      skFoot.textContent = pickedSkill
        ? '已指定「' + (pickedSkill.name || pickedSkill.slug) + '」，再点一次可取消'
        : '勾一个技能，之后的提问都用它（再点一次可取消）';
    }
  }

  // 勾选：换/取消 → 关层 → 焦点回到输入框（下一步一定是打字）。
  function chooseSkill(sk) {
    if (!sk) return;
    const next = togglePick(pickedSkill ? pickedSkill.slug : '', sk.slug);
    if (next) pickSkill(sk); else clearSkill();
    closeSkLayer();
    input.focus();
  }

  function openSkLayer() {
    if (!skLayer) return;
    // 每次打开都清空搜索词：上次搜的"合同"留着，会让用户以为技能库里只有合同。
    if (skQ && skQ.value) skQ.value = '';
    skOpen = true;
    skLayer.hidden = false;
    renderSkLayer();
    if (skQ) skQ.focus();
    renderChips();                     // 触发器要显示成"展开中"
  }

  function closeSkLayer() {
    if (!skLayer || !skOpen) return;
    skOpen = false;
    skLayer.hidden = true;
    renderChips();
  }

  function toggleSkLayer() { if (skOpen) closeSkLayer(); else openSkLayer(); }

  function wireSkLayer() {
    if (!skLayer) return;
    if (skQ) {
      skQ.addEventListener('input', renderSkLayer);
      skQ.addEventListener('keydown', (e) => {
        if (e.key === 'Enter') {
          // 搜到就直接回车 —— 不逼用户"搜完再把鼠标挪到列表上点一下"
          e.preventDefault();
          const first = filterSkills(allSkills, skQ.value)[0];
          if (first) chooseSkill(first);
        } else if (e.key === 'Escape') {
          e.preventDefault();
          closeSkLayer();
          input.focus();
        }
      });
    }
    // 点层外关掉。**必须排除推荐行和输入栏**：手动档没选技能点发送时，submit() 会主动
    // 打开这一层，而文档级 click 监听在冒泡末端才跑 —— 不排除的话它会把刚打开的层
    // 当场关掉，用户看到的就是"点了发送毫无反应"（这个坑踩过一次，别再踩）。
    //
    // ★ 必须是**捕获阶段**（第三参 true），不能是冒泡阶段。
    //   原因：点「选择技能 ▾」时 openSkLayer() 会 renderChips() 重建整行，被点的那颗
    //   button 当场变成游离节点；冒泡到 document 时白名单的归属判断全失效，
    //   白名单失效 → 层刚 hidden=false 又被自己关回 true。表现是"点一下闪一下就没了"，
    //   静态文本断言看不出来（源码里白名单写着呢），只有真 DOM 跑一次才暴露。
    //   捕获阶段在事件往下走时就判归属，此时 DOM 还没被重渲染，contains() 是真的。
    document.addEventListener('click', (e) => {
      if (!skOpen) return;
      const t = e.target;
      // 游离节点（重渲染产生的旧按钮）没有祖先链，contains() 必然 false ——
      // 用 isConnected 兜底：不连在文档上的东西不可能是"点层外"。
      if (t && t.isConnected === false) return;
      if (skLayer.contains(t)) return;
      if (chipsWrap && chipsWrap.contains(t)) return;
      if (inputBar && inputBar.contains(t)) return;
      closeSkLayer();
    }, true);
    // 焦点在输入框里时按 Esc 也要能关（不然只能去点外面）
    document.addEventListener('keydown', (e) => {
      if (skOpen && e.key === 'Escape' && !(skQ && document.activeElement === skQ)) {
        closeSkLayer();
      }
    });
  }

  // 后端降级说明 → 渲染成回复开头的引用行。
  // 单独抽出来是为了能被回归测试直接调用：这句话只有一条，丢了用户就抓瞎。
  function metaNoteLine(obj) {
    if (!obj || !obj.note) return '';
    return '> ' + obj.note + '\n\n';
  }

  function wireModes() {
    modeAuto.addEventListener('click', () => setMode('auto'));
    modeManual.addEventListener('click', () => setMode('manual'));
    // role=tablist 的可达性要求：左右方向键也能切档
    if (switchBox) switchBox.addEventListener('keydown', (e) => {
      if (e.key === 'ArrowLeft') { setMode('auto'); modeAuto.focus(); }
      if (e.key === 'ArrowRight') { setMode('manual'); modeManual.focus(); }
    });
    // 字体没就绪时 offsetWidth 可能为 0 → 滑块停在起点（视觉上等于没切档）。
    // 字体加载完和窗口尺寸变化时各校正一次。
    if (document.fonts && document.fonts.ready && document.fonts.ready.then) {
      document.fonts.ready.then(syncThumb);
    }
    window.addEventListener('resize', syncThumb);
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
    if (sendBlocked(chatMode, pickedSkill)) {
      nudgeChips();
      // 光抖一下还不够：这一档的唯一出路在勾选层里，直接把层打开、搜索框聚焦，
      // 用户下一步就只剩"勾一个"。抖是"看这里"，开层是"这里就能解决"。
      openSkLayer();
      return;
    }
    input.value = ''; input.focus(); autoGrow();
    closeSkLayer();                    // 发出去了就把层收掉，别让它压在对话上面
    if (welcome && !welcome.classList.contains('hidden')) welcome.classList.add('hidden');

    // persist user message + auto-title (first message, untitled session)
    const sess = activeSession();
    sess.messages.push({ role: 'user', text });
    if (sess.title === '新对话') sess.title = titleFrom(text);
    sess.updated = Date.now();
    saveStore(); router.render();

    addUser(text);
    // 推荐行的输入 = 这一轮开始之前的状态：先归档再重置。不重置的话 askedBack /
    // hasFile 会带着上一轮的结论进来（该反问时不反问）。
    flow.turns += 1;
    flow.lastUser = text;
    flow.askedBack = false;
    flow.hasFile = false;
    flow.lastReply = '';
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
      const hadFiles = pendingFiles.length > 0;
      const msg = { role: 'assistant', text: full, skill: trace?.dataset?.skill || null };
      if (hadFiles) msg.files = pendingFiles.slice();
      pendingFiles = [];
      if (full) {
        const s = activeSession();
        s.messages.push(msg);
        s.updated = Date.now();
        saveStore(); router.render(); prune();
      }
      // 这一轮的结果决定下一轮推荐什么：答完就重算，再异步请模型精修。
      flow.lastReply = full;
      flow.hasFile = hadFiles;
      flow.askedBack = asksBack(full);
      renderChips();
      refineChips();
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
          // 中间材料：模型正在想的片段。有它用户才看得到「在动的是什么」，
          // 而不是只有一个跳秒的计时器。后端已截成尾部 160 字并节流下发。
          (s.material
            ? '<span class="ctk-mat"><span class="ctk-mat-tag">思考中</span>' + esc(s.material) + '</span>'
            : '') +
        '</span>';
      body.appendChild(row);
    });
    keepBottom();
  }

  boot();
})();