/* SkillForge — public workspace */
(() => {
  const $ = (id) => document.getElementById(id);

  const grid = $('skill-grid');
  const loading = $('loading');
  const countEl = $('skill-count');
  const workspace = $('workspace');
  const hero = $('hero');

  // render skill cards
  function card(sk) {
    const params = (sk.input_params || []).length;
    const iconMap = { '写作': '✍️', '报告': '📄', '宣传': '📣', '新闻': '📰', '其他': '✦' };
    const icon = iconMap[sk.category || '其他'] || '✦';
    const disabled = sk.enabled === false ? ' disabled' : '';
    return `<a class="skill-card${disabled}" href="#${sk.slug}">
      <div class="icon">${icon}</div>
      ${sk.is_core ? '<span class="badge-core">CORE</span>' : ''}
      <h3>${esc(sk.name)}</h3>
      <p>${esc(sk.description || '')}</p>
      <div class="foot">
        ${params ? `<span class="params">${params} 个输入项</span>` : '<span></span>'}
        <span class="dir-arrow">→</span>
      </div>
    </a>`;
  }

  function esc(s) {
    return String(s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  }

  async function loadSkills() {
    try {
      const r = await fetch('/api/skills');
      const data = await r.json();
      const list = data.skills || [];
      loading.style.display = 'none';
      countEl.textContent = `${list.length} 个技能`;
      grid.innerHTML = list.map(card).join('') || `<div class="empty"><div class="glyph">✦</div><h3>还没有可用技能</h3></div>`;
      // wire hash
      if (location.hash) openSkill(location.hash.slice(1));
    } catch (e) {
      loading.innerHTML = '<div class="empty"><h3>加载失败，请重试</h3></div>';
    }
  }

  async function openSkill(slug) {
    try {
      const r = await fetch('/api/skills/' + slug);
      if (!r.ok) throw new Error('not found');
      const sk = await r.json();
      renderWorkspace(sk);
    } catch (e) { /* ignore */ }
  }

  function renderWorkspace(sk) {
    hero.style.display = 'none';
    location.hash = sk.slug;
    $('w-title').textContent = sk.name;
    $('w-desc').textContent = sk.description || '';
    const chips = [];
    if (sk.category) chips.push(`<span class="chip">${esc(sk.category)}</span>`);
    if (sk.version) chips.push(`<span class="chip">v${sk.version}</span>`);
    if (sk.is_core) chips.push(`<span class="chip core">核心</span>`);
    $('w-chips').innerHTML = chips.join('');
    $('w-head').textContent = sk.name + ' · 开始写作';

    // build form
    const fields = $('w-fields');
    const params = sk.input_params || [];
    fields.innerHTML = params.length ? params.map(p => `
      <div class="field">
        <label>${esc(p.label || p.name)}${p.required ? '' : ' <span class="muted">(可选)</span>'}</label>
        <textarea id="p-${esc(p.name)}" placeholder="${esc(p.placeholder || '')}" ${p.required ? 'required' : ''} rows="${(p.type === 'textarea' || !p.type) ? 3 : 1}"></textarea>
        ${p.hint ? `<div class="hint">${esc(p.hint)}</div>` : ''}
      </div>`).join('') : `<div class="dim" style="margin-bottom:14px">这个技能没有额外输入项，点击生成即可。</div>`;

    workspace.style.display = 'block';
    workspace.scrollIntoView({ behavior: 'smooth' });
    window.sk = sk;
  }

  // generate via SSE
  $('gen-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const sk = window.sk;
    if (!sk) return;
    const params = sk.input_params || [];
    const input = {};
    for (const p of params) {
      const el = $('p-' + p.name);
      if (el && el.value.trim()) input[p.name] = el.value.trim();
    }
    const btn = $('btn-gen');
    const spin = $('btn-gen-spin');
    const txt = $('btn-gen-txt');
    btn.disabled = true; spin.style.display = 'inline-block'; txt.textContent = '生成中…';
    $('btn-clear').style.display = 'inline-flex';
    $('out-placeholder').style.display = 'none';
    const out = $('out');
    out.style.display = 'block';
    out.className = 'out';
    out.innerHTML = '';

    try {
      const resp = await fetch('/api/generate', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ slug: sk.slug, input })
      });
      if (!resp.ok) {
        const j = await resp.json().catch(() => ({}));
        out.innerHTML = `<span class="stream-err">${esc(j.error || '请求失败')}</span>`;
        return;
      }
      const reader = resp.body.getReader();
      const dec = new TextDecoder();
      let bufStr = '';
      let doneSent = false;
      while (true) {
        const { value, done } = await reader.read();
        if (done) break;
        bufStr += dec.decode(value, { stream: true });
        const blocks = bufStr.split('\n\n');
        bufStr = blocks.pop();
        for (const b of blocks) {
          const line = b.trim();
          if (!line.startsWith('data:')) continue;
          try {
            const ev = JSON.parse(line.slice(5).trim());
            handleEvent(ev, out);
            if (ev.type === 'done' || ev.type === 'error') doneSent = true;
          } catch (_) {}
        }
      }
      // show cursor if no terminal event
      if (!doneSent) {
        const cur = out.querySelector('.cursor');
        if (cur) cur.remove();
        out.insertAdjacentHTML('beforeend', `<span class="stream-done">✔ 完成</span>`);
      }
    } catch (err) {
      out.innerHTML = `<span class="stream-err">网络错误：${esc(err.message)}</span>`;
    } finally {
      btn.disabled = false; spin.style.display = 'none'; txt.textContent = '开始生成';
    }
  });

  function handleEvent(ev, out) {
    switch (ev.type) {
      case 'meta':
        break;
      case 'delta':
        // remove cursor then append
        const cur = out.querySelector('.cursor');
        if (cur) cur.remove();
        out.insertAdjacentText('beforeend', ev.data || '');
        out.insertAdjacentHTML('beforeend', `<span class="cursor"></span>`);
        out.scrollTop = out.scrollHeight;
        break;
      case 'done':
        out.querySelector('.cursor')?.remove();
        out.insertAdjacentHTML('beforeend', `<span class="stream-done">✔ 生成完成</span>`);
        break;
      case 'error':
        out.querySelector('.cursor')?.remove();
        out.insertAdjacentHTML('beforeend', `<span class="stream-err">✕ ${esc(ev.data)}</span>`);
        break;
    }
  }

  $('btn-clear').addEventListener('click', () => {
    const out = $('out');
    out.style.display = 'none';
    out.innerHTML = '';
    $('out-placeholder').style.display = 'block';
  });

  window.addEventListener('hashchange', () => {
    if (location.hash) openSkill(location.hash.slice(1));
  });

  loadSkills();
})();