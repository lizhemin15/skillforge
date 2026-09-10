/* SkillForge — admin console */
(() => {
  const $ = (id) => document.getElementById(id);
  const esc = (s) => String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

  const TOKEN_KEY = 'sf_token';
  const token = () => localStorage.getItem(TOKEN_KEY);
  const authHdr = () => ({ 'Content-Type': 'application/json', 'Authorization': 'Bearer ' + token() });

  let toastTimer;
  // module-level CodeMirror editor instance (nullable), plus which behavior the current editor is bound to
  let cmEditor = null;
  let cmSaveCb = null;      // function(slug,path,...) bound to current editor
  let cmEditorSlug = null;

  // ---------- toast ----------
  function toast(msg, cls) {
    const t = $('toast');
    t.textContent = msg;
    t.className = 'toast show ' + (cls || '');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => t.className = 'toast ' + (cls || ''), 2600);
  }

  // ---------- login ----------
  $('login-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const msg = $('login-msg');
    msg.className = 'msg'; msg.textContent = '登录中…';
    try {
      const r = await fetch('/api/login', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ username: $('lg-user').value, password: $('lg-pass').value })
      });
      const j = await r.json();
      if (!r.ok) { msg.className = 'msg err'; msg.textContent = j.error || '登录失败'; return; }
      localStorage.setItem(TOKEN_KEY, j.token);
      enter();
    } catch (err) { msg.className = 'msg err'; msg.textContent = '网络错误'; }
  });

  async function enter() {
    $('login-view').style.display = 'none';
    $('shell').style.display = 'block';
    $('shell').classList.remove('hidden');
    loadProviders();
    loadManageSkills();
  }

  // ---------- tabs ----------
  // `.hidden { display:none !important }` 会覆盖 inline style，因此切换 tab 必须
  // 同步增删 `hidden` 类，仅设 style.display 会导致目标 pane 仍被 !important 隐藏。
  document.querySelectorAll('.admin-tabs button').forEach(btn => {
    btn.addEventListener('click', () => {
      document.querySelectorAll('.admin-tabs button').forEach(b => b.classList.remove('active'));
      btn.classList.add('active');
      document.querySelectorAll('.tab-pane').forEach(p => {
        p.classList.add('hidden');
        p.style.display = 'none';
      });
      const target = $('tab-' + btn.dataset.tab);
      target.classList.remove('hidden');
      target.style.display = 'block';
    });
  });

  // ---------- LLM providers ----------
  async function loadProviders() {
    try {
      const r = await fetch('/api/admin/llms', { headers: authHdr() });
      if (r.status === 401) { logout(); return; }
      const j = await r.json();
      const list = j.configs || [];
      const box = $('prov-list');
      if (!list.length) {
        box.innerHTML = `<div class="empty" style="padding:30px 0"><h3>还没有配置 LLM 服务</h3><p class="dim">先添加一个，前台才能生成文章</p></div>`;
        return;
      }
      const icons = { deepseek: '🔷', openai: '◯', qwen: '🌂', anthropic: '✳', other: '☁' };
      box.innerHTML = list.map(c => `
        <div class="prov-row">
          <div class="pk">${icons[c.provider && c.provider.toLowerCase()] || icons.other}</div>
          <div class="inf">
            <div class="nm">${esc(c.name || c.provider)} ${c.is_active ? '<span class="pill-active">在用</span>' : ''}</div>
            <div class="dt">${esc(c.model)} · base ${esc(c.base_url)}</div>
          </div>
          <div class="row-actions">
            <button class="btn ghost" style="padding:4px 8px;font-size:12px" onclick="window.editProv(${c.id})">编辑</button>
            ${c.is_active ? '' : `<button class="btn ghost" style="padding:4px 8px;font-size:12px" onclick="window.useProv(${c.id})">切换</button>`}
            <button class="icon-btn danger" title="删除" onclick="window.delProv(${c.id})">✕</button>
          </div>
        </div>`).join('');
    } catch (e) { $('prov-msg').textContent = '加载失败'; }
  }

  window.editProv = async (id) => {
    const r = await fetch('/api/admin/llms', { headers: authHdr() });
    const j = await r.json();
    const c = (j.configs || []).find(x => x.id === id);
    if (!c) return;
    $('llm-id').value = c.id;
    $('llm-name').value = c.name || '';
    $('llm-base').value = c.base_url || '';
    $('llm-model').value = c.model || '';
    $('llm-key').value = '••••••••'; // masked placeholder
    $('llm-key').placeholder = '留空保持不变';
    $('llm-key').required = false;
    $('llm-cancel').style.display = 'inline-flex';
    window.scrollTo({ top: 0, behavior: 'smooth' });
  };
  window.useProv = async (id) => {
    const r = await fetch('/api/admin/llms/active', { method: 'POST', headers: authHdr(), body: JSON.stringify({ id }) });
    const j = await r.json();
    if (r.ok) { toast('已切换服务', 'ok'); loadProviders(); } else toast(j.error || '切换失败', 'err');
  };
  window.delProv = async (id) => {
    if (!confirm('删除这个服务配置？')) return;
    const r = await fetch('/api/admin/llms/' + id, { method: 'DELETE', headers: authHdr() });
    const j = await r.json();
    if (r.ok) { toast('已删除', 'ok'); loadProviders(); } else toast(j.error || '删除失败', 'err');
  };
  $('llm-cancel').addEventListener('click', () => {
    $('llm-form').reset(); $('llm-id').value = 0; $('llm-cancel').style.display = 'none';
    $('llm-key').required = true; $('llm-key').placeholder = 'sk-…';
  });

  $('llm-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const isNew = $('llm-id').value === '0';
    const payload = {
      id: isNew ? 0 : parseInt($('llm-id').value),
      name: $('llm-name').value,
      base_url: $('llm-base').value.trim().replace(/\/+$/, ''),
      model: $('llm-model').value,
      api_key: $('llm-key').value,
      is_active: false
    };
    // if masked placeholder on update, skip key
    if (!isNew && ($('llm-key').value === '••••••••' || $('llm-key').value === '')) delete payload.api_key;
    const msg = $('llm-msg');
    msg.textContent = '保存中…';
    try {
      const r = await fetch('/api/admin/llms', { method: 'POST', headers: authHdr(), body: JSON.stringify(payload) });
      const j = await r.json();
      if (!r.ok) { msg.className = 'msg err'; msg.textContent = j.error || '保存失败'; return; }
      msg.className = 'msg ok'; msg.textContent = '已保存并自动切换';
      $('llm-form').reset(); $('llm-id').value = 0; $('llm-cancel').style.display = 'none';
      $('llm-key').required = true; $('llm-key').placeholder = 'sk-…';
      loadProviders();
    } catch (err) { msg.className = 'msg err'; msg.textContent = '网络错误'; }
  });

  // ---------- train: file upload ----------
  let selectedFiles = [];
  const drop = $('tr-drop'), fileInput = $('tr-file');
  drop.addEventListener('click', () => fileInput.click());
  drop.addEventListener('dragover', e => { e.preventDefault(); drop.classList.add('over'); });
  drop.addEventListener('dragleave', () => drop.classList.remove('over'));
  drop.addEventListener('drop', e => { e.preventDefault(); drop.classList.remove('over'); addFiles(e.dataTransfer.files); });
  fileInput.addEventListener('change', () => addFiles(fileInput.files));
  function addFiles(files) {
    for (const f of files) selectedFiles.push(f);
    renderTags();
  }
  window.removeFile = (i) => { selectedFiles.splice(i, 1); renderTags(); };
  function renderTags() {
    $('tr-tags').innerHTML = selectedFiles.map((f, i) =>
      `<span class="file-tag">${esc(f.name)} <button onclick="removeFile(${i})">✕</button></span>`).join('');
  }

  // ---------- train: submit + SSE progress ----------
  $('train-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const go = $('tr-go'), spin = $('tr-spin'), txt = $('tr-txt');
    go.disabled = true; spin.style.display = 'inline-block'; txt.textContent = '训练中…';
    $('tr-log').style.display = 'block';
    $('tr-log').innerHTML = '';
    $('tr-result').style.display = 'none';

    const fd = new FormData();
    fd.append('name', $('tr-name').value);
    fd.append('category', $('tr-cat').value || '');
    fd.append('description', $('tr-desc').value || '');
    fd.append('requirement', $('tr-req').value || '');
    for (const f of selectedFiles) fd.append('files', f);

    const logLine = (stage, s, cls) => {
      const div = document.createElement('div');
      div.className = 'ln ' + (cls || '');
      div.innerHTML = `<span class="t">${esc(stage)}</span><span class="s">${esc(s)}</span>`;
      $('tr-log').appendChild(div);
      $('tr-log').scrollTop = $('tr-log').scrollHeight;
    };

    try {
      const resp = await fetch('/api/admin/train', { method: 'POST', headers: { 'Authorization': 'Bearer ' + token() }, body: fd });
      if (!resp.ok) {
        const j = await resp.json().catch(() => ({}));
        logLine('!', j.error || '请求失败', 'err');
        return;
      }
      const reader = resp.body.getReader();
      const dec = new TextDecoder();
      let bufStr = '';
      while (true) {
        const { value, done } = await reader.read();
        if (done) break;
        bufStr += dec.decode(value, { stream: true });
        const blocks = bufStr.split('\n\n');
        bufStr = blocks.pop();
        for (const b of blocks) {
          const line0 = b.trim();
          if (!line0.startsWith('data:')) continue;
          try {
            const ev = JSON.parse(line0.slice(5).trim());
            switch (ev.type) {
              case 'stage': logLine('→', ev.data, ''); break;
              case 'done':
                const r = ev.data && typeof ev.data === 'object' ? ev.data : JSON.parse(ev.data);
                logLine('✔', '技能「' + r.name + '」训练完成', 'ok');
                const rd = $('tr-result');
                rd.style.display = 'block';
                rd.innerHTML = `<h4>✔ 新技能已就绪</h4>
                  <div class="row">名称：<b>${esc(r.name)}</b> (§ ${esc(r.slug)})</div>
                  <div class="row">版本：<b>v${r.version}</b></div>
                  <div class="row">参数：<b>${(r.input_params || []).length}</b> 个</div>
                  <div class="params">${(r.input_params || []).map(p => `<span class="param-tag">${esc(p.label || p.name)}</span>`).join('')}</div>
                  <div class="row" style="margin-top:10px">前台列表已可用，也可在「技能管理」里编辑。</div>`;
                loadManageSkills();
                break;
              case 'error': logLine('✕', ev.data, 'err'); break;
            }
          } catch (_) {}
        }
      }
    } catch (err) {
      logLine('!', '网络错误：' + err.message, 'err');
    } finally {
      go.disabled = false; spin.style.display = 'none'; txt.textContent = '开始训练';
      selectedFiles = []; renderTags();
    }
  });

  // ---------- skill management ----------
  async function loadManageSkills() {
    try {
      const r = await fetch('/api/skills');
      const j = await r.json();
      const list = (j.skills || []);
      const box = $('skill-manage-list');
      if (!list.length) { box.innerHTML = '<div class="empty"><h3>暂无技能</h3></div>'; return; }
      box.innerHTML = list.map(sk => `
        <div class="prov-row">
          <div class="pk">✦</div>
          <div class="inf">
            <div class="nm">${esc(sk.name)} ${sk.is_core ? '<span class="pill-active">CORE</span>' : ''}</div>
            <div class="dt">${esc(sk.slug)} · v${sk.version} · ${(sk.input_params || []).length} 参数</div>
          </div>
          <div class="row-actions">
            <button class="btn ghost" style="padding:4px 8px;font-size:12px" onclick="window.openSkillDetail('${esc(sk.slug)}','${esc(sk.name.replace(/'/g,"\\'"))}')">编辑</button>
            <button class="btn ghost" style="padding:4px 8px;font-size:12px" onclick="window.toggleSkill('${esc(sk.slug)}',${!sk.enabled})">${sk.enabled ? '停用' : '启用'}</button>
            ${sk.is_core ? '' : `<button class="icon-btn danger" title="删除" onclick="window.delSkill('${esc(sk.slug)}')">✕</button>`}
          </div>
        </div>`).join('');
    } catch (e) {}
  }
  window.toggleSkill = async (slug, enabled) => {
    const r = await fetch('/api/admin/skills/toggle', { method: 'POST', headers: authHdr(), body: JSON.stringify({ slug, enabled }) });
    const j = await r.json();
    if (r.ok) { toast(enabled ? '已启用' : '已停用', 'ok'); loadManageSkills(); loadSkillsPublic && loadSkillsPublic(); }
    else toast(j.error || '操作失败', 'err');
  };
  window.delSkill = async (slug) => {
    if (!confirm('删除该技能及其所有内容？')) return;
    const r = await fetch('/api/admin/skills/' + slug, { method: 'DELETE', headers: authHdr() });
    const j = await r.json();
    if (r.ok) { toast('已删除', 'ok'); loadManageSkills(); } else toast(j.error || '删除失败', 'err');
  };

  // ---------- knowledge-base skill detail ----------
  const KIND_LABEL = { prompt: '核心提示词', template: '写作模板', requirement: '训练需求', style: '风格画像', example: '参考范文', source: '原始素材', other: '其他' };
  const KIND_ICON = { prompt: '🧠', template: '📋', requirement: '📐', style: '🎯', example: '📄', source: '📁', other: '📎' };
  let curSkillSlug = null;
  let curEditPath = null; // last file open in the IDE editor (persists across reloads)

  window.openSkillDetail = async (slug, name) => {
    curSkillSlug = slug;
    curEditPath = null; // reset last-edited when opening a fresh skill
    $('skill-detail-mask').style.display = 'flex';
    $('skill-detail-mask').classList.remove('hidden');
    $('sd-title').textContent = name || slug;
    $('sd-sub').textContent = slug;
    $('sd-body').innerHTML = '<p class="dim" style="padding:20px">加载中…</p>';
    await loadSkillFiles(slug);
  };
  window.closeSkillDetail = () => {
    $('skill-detail-mask').style.display = 'none';
    $('skill-detail-mask').classList.add('hidden');
  };

  async function loadSkillFiles(slug) {
    const body = $('sd-body');
    try {
      const r = await fetch(`/api/admin/skills/${encodeURIComponent(slug)}/files`, { headers: authHdr() });
      if (r.status === 401) { logout(); return; }
      const j = await r.json();
      if (!r.ok) { body.innerHTML = `<p class="msg err">${esc(j.error || '加载失败')}</p>`; return; }
      const files = j.files || [];
      renderSkillFiles(body, files);
      // auto-open: prefer last edited file (still present), else system_prompt.md, else first editable
      const keep = curEditPath ? files.find(f => f.path === curEditPath && f.editable) : null;
      const primary = keep
        || files.find(f => f.path === 'system_prompt.md' && f.editable)
        || files.find(f => f.editable);
      if (primary) window.openEditorFile(encodeURIComponent(slug), primary.path);
    } catch (e) { body.innerHTML = '<p class="msg err">网络错误</p>'; }
  }

  function renderSkillFiles(body, files) {
    const groups = ['prompt', 'template', 'requirement', 'style', 'example', 'source'];
    // LEFT: file tree; RIGHT: editor
    let tree = '', right = '';
    // top action row (new example / upload source) pinned above tree
    tree += `<div class="kb-tree-actions">
        <button class="btn ghost" onclick="window.addExampleView()">+ 新增范文</button>
        <button class="btn ghost" onclick="window.uploadSourceView()">+ 上传素材</button>
        <button class="btn ghost" onclick="window.editMetaView()">元数据</button>
        <button class="btn ghost" onclick="window.showReviewPanel()">✨ AI 优化</button>
        <button class="btn ghost" onclick="window.showVersionsPanel()">↩ 版本</button>
      </div>`;
    for (const k of groups) {
      const items = files.filter(f => f.kind === k);
      if (!items.length && k !== 'example') continue;
      tree += `<div class="kb-group">
        <div class="kb-group-title">${KIND_ICON[k]} ${KIND_LABEL[k]} <span class="dim" style="font-weight:400">(${items.length})</span></div>
        <div class="kb-list">`;
      if (!items.length) {
        tree += `<div class="kb-empty">暂无，下方添加</div>`;
      } else {
        for (const f of items) {
          const canEdit = f.editable;
          const canDel = f.kind === 'example' || f.kind === 'source';
          tree += `<div class="kb-tree-row" data-path="${esc(f.path)}" onclick="window.openEditorFile('${encodeURIComponent(curSkillSlug)}','${escapeJs(f.path)}')" title="${esc(f.path)}">
            <span class="kb-tree-name">${esc(f.name)} <span class="dim">${f.size}B</span></span>
            <span class="kb-tree-ops">
              ${canEdit ? `<button class="link-btn" onclick="event.stopPropagation()">编辑</button>` : ''}
              <a class="link-btn" title="下载" href="/api/admin/skills/${encodeURIComponent(curSkillSlug)}/file?path=${encodeURIComponent(f.path)}&download=1" onclick="event.stopPropagation()">⬇</a>
              ${canDel ? `<button class="icon-btn danger" title="删除" onclick="event.stopPropagation();window.delSkillFile('${encodeURIComponent(curSkillSlug)}','${escapeJs(f.path)}')">✕</button>` : ''}
            </span>
          </div>`;
        }
      }
      tree += `</div></div>`;
    }
    // right panel is the CM editor container + overlay panel (AI-review / versions)
    right = `<div class="kb-editor hidden" id="kb-editor" style="display:none"></div>
      <div id="kb-panel" class="hidden" style="display:none"></div>`;
    body.innerHTML = `<div class="kb-split">
        <div class="kb-tree-pane" id="kb-tree">${tree}</div>
        <div class="kb-editor-pane">${right}</div>
      </div>`;
  }

  // helper to get slug for a path (dummy; actual slug is global curSkillSlug)
  function slugFromPath() { return curSkillSlug; }
  function escapeJs(s) { return s.replace(/\\/g, '\\\\').replace(/'/g, "\\'").replace(/\n/g, ' '); }

  // ---- CodeMirror-backed file editor ----
    // module: mount a CM editor into a given holder div
    function mountCm(holder) {
      const ta = document.createElement('textarea');
      ta.id = 'kb-content';
      if (holder.dataset.placeholder) ta.placeholder = holder.dataset.placeholder;
      holder.appendChild(ta);
      const editor = CodeMirror.fromTextArea(ta, {
        lineNumbers: true,
        mode: { name: 'markdown', highlightFormatting: true },
        lineWrapping: false,
        styleActiveLine: true,
        matchBrackets: true,
        autoCloseBrackets: true,
        foldGutter: true,
        gutters: ['CodeMirror-foldgutter', 'CodeMirror-linenumbers'],
        extraKeys: {
          'Ctrl-S': function () { if (cmSaveCb) cmSaveCb(); },
          'Cmd-S': function () { if (cmSaveCb) cmSaveCb(); },
          Tab: function (cm) {
            if (cm.somethingSelected()) cm.indentSelection('add');
            else cm.replaceSelection('\t');
          },
          'Shift-Tab': function (cm) { cm.indentSelection('subtract'); },
          'Ctrl-/': function (cm) { cm.toggleComment({ lineComment: '//', blockComment: ['/*', '*/'] }); }
        }
      });
      editor.setSize('100%', 360);
      cmEditor = editor;
      return editor;
    }

    function updateCmStats() {
      if (!cmEditor) return;
      const v = cmEditor.getValue();
      const chars = v.length;
      const words = v.replace(/\s+/g, ' ').trim() ? v.replace(/\s+/g, ' ').trim().split(' ').length : 0;
      const st = $('cm-stats'); if (st) st.textContent = chars + ' 字符 · ' + words + ' 词';
    }

    function destroyCm() {
      if (cmEditor) { cmEditor.toTextArea(); cmEditor = null; }
      cmSaveCb = null; cmEditorSlug = null;
    }

    function cmContent() {
      if (cmEditor) return cmEditor.getValue();
      const el = $('kb-content'); return el ? el.value : '';
    }

    // open a file from the tree -> highlight + edit
    window.openEditorFile = async (slug, path) => {
      slug = decodeURIComponent(slug);
      // highlight active row
      const rows = document.querySelectorAll('#kb-tree .kb-tree-row');
      rows.forEach(r => r.classList.toggle('active', r.dataset.path === decodeURIComponent(path)));
      await window.editSkillFile(encodeURIComponent(slug), path);
    };

    // edit a file (CodeMirror)
    window.editSkillFile = async (slug, path) => {
      slug = decodeURIComponent(slug);
      curEditPath = decodeURIComponent(path); // remember for reload persistence
      destroyCm();
      const ed = $('kb-editor');
      ed.style.display = 'flex'; // clear inline none; ide-mode supplies flex column
      ed.classList.remove('hidden');
      ed.classList.add('ide-mode');
      ed.classList.remove('form-mode');
      ed.innerHTML = '<p class="dim">加载…</p>';
      try {
        const r = await fetch('/api/admin/skills/' + encodeURIComponent(slug) + '/file?path=' + encodeURIComponent(path), { headers: authHdr() });
        const j = await r.json();
        if (!r.ok) { ed.innerHTML = '<p class="msg err">' + esc(j.error || '加载失败') + '</p>'; return; }
        // Binary (PDF/image/Office): read via /raw as a blob, render read-only preview.
        if (j.binary) {
          ed.innerHTML =
            '<div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:8px">' +
              '<b>预览 ' + esc(j.path) + '</b>' +
              '<div style="display:flex;gap:8px;align-items:center">' +
                '<a class="btn ghost" href="/api/admin/skills/' + encodeURIComponent(slug) + '/file?path=' + encodeURIComponent(path) + '&download=1">下载</a>' +
                '<button class="btn ghost" onclick="closeDetailEditor()">关闭</button>' +
              '</div>' +
            '</div>' +
            '<div id="kb-preview" style="flex:1;min-height:320px;border:1px solid var(--border);border-radius:8px;overflow:auto;padding:8px;background:#111"></div>';
          const holder = ed.querySelector('#kb-preview');
          try {
            // Office documents: extract text via the backend (sync) for inline text preview.
            if (isOffice(j.mime)) {
              holder.innerHTML = '<p class="dim" style="padding:24px">正在提取文档内容…</p>';
              const pr = await fetch('/api/admin/skills/' + encodeURIComponent(slug) + '/file?path=' + encodeURIComponent(path) + '&preview=1', { headers: authHdr() });
              const pj = await pr.json();
              if (!pj || !pj.previewable) { holder.innerHTML = '<p class="dim" style="padding:24px">该格式不支持内容提取，请使用下载按钮查看原文件。</p>'; return; }
              if (pj.error) { holder.innerHTML = '<p class="msg err">' + esc(pj.error) + '</p>'; return; }
              const t = pj.text || '';
              holder.innerHTML =
                '<div style="padding:8px 12px;font-size:12px;color:#888;border-bottom:1px solid var(--border);display:flex;justify-content:space-between;align-items:center">' +
                  '<span>已提取文本 · ' + esc(t.length) + ' 字符（文档内容自动解析）</span>' +
                  '<span style="opacity:.7">原始排版请用「下载」查看</span>' +
                '</div>' +
                '<pre style="white-space:pre-wrap;word-break:break-word;overflow-wrap:anywhere;padding:14px 16px;font-size:13px;line-height:1.7;margin:0">' + esc(t || '(无文本内容)') + '</pre>';
              return;
            }
            const rb = await fetch('/api/admin/skills/' + encodeURIComponent(slug) + '/raw?path=' + encodeURIComponent(path), { headers: { 'Authorization': 'Bearer ' + token() } });
            if (!rb.ok) { holder.innerHTML = '<p class="msg err">预览加载失败 (' + rb.status + ')</p>'; return; }
            const blob = await rb.blob();
            const url = URL.createObjectURL(blob);
            if (isPdf(j.mime)) {
              holder.innerHTML = '<iframe src="' + url + '" style="width:100%;height:100%;min-height:600px;border:0;border-radius:4px" title="PDF 预览"></iframe>';
            } else if (isImage(j.mime)) {
              holder.innerHTML = '<div style="display:flex;align-items:center;justify-content:center;height:100%"><img src="' + url + '" style="max-width:100%;max-height:70vh;border-radius:4px" alt="预览"></div>';
            } else {
              holder.innerHTML = '<p class="dim" style="padding:24px">该格式不支持内嵌预览，请使用下载按钮查看原始文件。</p>';
            }
          } catch (e) { holder.innerHTML = '<p class="msg err">网络错误</p>'; }
          return;
        }
        
        ed.innerHTML =
          '<div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:8px">' +
            '<b>编辑 ' + esc(j.path) + '</b>' +
            '<div style="display:flex;gap:8px;align-items:center">' +
              '<span id="cm-stats" class="dim" style="font-size:11.5px"></span>' +
              '<button class="btn ghost" onclick="toggleCmFullscreen()">全屏</button>' +
              '<button class="btn ghost" onclick="closeDetailEditor()">取消</button>' +
              '<button class="btn primary" onclick="window.saveSkillFile(\'' + encodeURIComponent(slug) + '\',\'' + escapeJs(j.path) + '\')">保存</button>' +
            '</div>' +
          '</div>' +
          '<div id="cm-holder"></div>';
        cmSaveCb = () => window.saveSkillFile(slug, path);
        cmEditorSlug = slug;
        const holder = ed.querySelector('#cm-holder');
        const editor = mountCm(holder);
        editor.setValue(j.content || '');
        editor.focus(); editor.setCursor(0, 0);
        editor.on('change', updateCmStats);
        updateCmStats();
      } catch (e) { ed.innerHTML = '<p class="msg err">网络错误</p>'; }
    };
    function escapeTextarea(s) { return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;'); }
    function isPdf(m) { return /application\/pdf/i.test(m || ''); }
    function isImage(m) { return /^image\//i.test(m || ''); }
    function isOffice(m) { return /wordprocessingml|spreadsheetml|presentationml/i.test(m || ''); }


    window.saveSkillFile = async (slug, path) => {
      slug = decodeURIComponent(slug);
      let content;
      if (cmEditor && cmEditorSlug === slug) content = cmEditor.getValue();
      else content = $('kb-content') ? $('kb-content').value : '';
      const r = await fetch('/api/admin/skills/' + encodeURIComponent(slug) + '/file', {
        method: 'PUT',
        headers: authHdr(),
        body: JSON.stringify({ path: decodeURIComponent(path), content: content })
      });
      const j = await r.json();
      if (r.ok) { toast('已保存', 'ok'); destroyCm(); closeDetailEditor(); loadSkillFiles(curSkillSlug); }
      else toast(j.error || '保存失败', 'err');
    };

    // fullscreen toggle
    window.toggleCmFullscreen = () => {
      const holder = document.getElementById('cm-holder');
      if (!holder) return;
      const isFs = holder.classList.toggle('cm-fs');
      document.body.classList.toggle('cm-fs-body', isFs);
      if (cmEditor) { cmEditor.setSize('100%', isFs ? 'calc(100vh - 140px)' : 360); cmEditor.refresh(); }
    };
    window.closeDetailEditor = function closeDetailEditor() {
      const ed = $('kb-editor'); if (ed) { ed.style.display = 'none'; ed.classList.add('hidden'); ed.innerHTML = ''; }
      const pn = $('kb-panel'); if (pn) { pn.style.display = 'none'; pn.classList.add('hidden'); }
      destroyCm();
    }

  // add example
  window.addExampleView = () => {
    destroyCm();
    const ed = $('kb-editor');
    ed.style.display = 'block'; ed.classList.remove('hidden');
    ed.classList.remove('ide-mode'); ed.classList.add('form-mode');
    ed.innerHTML =
      '<div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:8px">' +
        '<b>新增参考范文</b>' +
        '<div style="display:flex;gap:8px;align-items:center">' +
          '<button class="btn ghost" onclick="toggleCmFullscreen()">全屏</button>' +
          '<button class="btn ghost" onclick="closeDetailEditor()">取消</button>' +
          '<button class="btn primary" onclick="window.doAddExample(\'' + encodeURIComponent(curSkillSlug) + '\')">保存</button>' +
        '</div>' +
      '</div>' +
      '<div id="cm-holder" data-placeholder="粘贴一篇范文章节…"></div>';
    mountCm(ed.querySelector('#cm-holder'));
  };
  window.doAddExample = async (slug) => {
    const content = cmContent();
    if (!content.trim()) { toast('内容不能为空', 'err'); return; }
    const r = await fetch(`/api/admin/skills/${encodeURIComponent(slug)}/example`, {
      method: 'POST', headers: authHdr(), body: JSON.stringify({ content })
    });
    const j = await r.json();
    if (r.ok) { toast('范已添加', 'ok'); closeDetailEditor(); loadSkillFiles(curSkillSlug); }
    else toast(j.error || '添加失败', 'err');
  };

  // upload raw source material
  window.uploadSourceView = () => {
    const ed = $('kb-editor');
    ed.style.display = 'block'; ed.classList.remove('hidden');
    ed.classList.remove('ide-mode'); ed.classList.add('form-mode');
    ed.innerHTML = `
      <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:8px">
        <b>上传原始素材</b>
        <div><button class="btn ghost" onclick="closeDetailEditor()">取消</button>
        <button class="btn primary" onclick="window.doUploadSource('${encodeURIComponent(curSkillSlug)}')">上传</button></div>
      </div>
      <div class="drop" id="src-drop" style="margin-bottom:8px">点击选择或拖拽素材文件（用于后续作为参考）</div>
      <input type="file" id="src-input" style="display:none">`;
    const drop = $('src-drop'), input = $('src-input');
    drop.addEventListener('click', () => input.click());
    input.addEventListener('change', () => { if (input.files.length) drop.textContent = '已选择：' + input.files[0].name; });
  };
  window.doUploadSource = async (slug) => {
    const input = $('src-input');
    if (!input.files.length) { toast('请选择文件', 'err'); return; }
    const fd = new FormData();
    fd.append('file', input.files[0]);
    const r = await fetch(`/api/admin/skills/${encodeURIComponent(slug)}/file`, {
      method: 'POST', headers: { 'Authorization': 'Bearer ' + token() }, body: fd
    });
    const j = await r.json();
    if (r.ok) { toast('素材已上传', 'ok'); closeDetailEditor(); loadSkillFiles(curSkillSlug); }
    else toast(j.error || '上传失败', 'err');
  };

  // delete example file
  window.delSkillFile = async (slug, path) => {
    if (!confirm('删除这篇示例范文？')) return;
    const r = await fetch(`/api/admin/skills/${encodeURIComponent(slug)}/file?path=${encodeURIComponent(path)}`, {
      method: 'DELETE', headers: authHdr()
    });
    const j = await r.json();
    if (r.ok) { toast('已删除', 'ok'); loadSkillFiles(curSkillSlug); }
    else toast(j.error || '删除失败', 'err');
  };

  // edit metadata
  window.editMetaView = () => {
    const ed = $('kb-editor');
    ed.style.display = 'block'; ed.classList.remove('hidden');
    ed.classList.remove('ide-mode'); ed.classList.add('form-mode');
    ed.innerHTML = `<div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:8px">
        <b>编辑元数据</b>
        <div><button class="btn ghost" onclick="closeDetailEditor()">取消</button>
        <button class="btn primary" onclick="window.saveMeta('${encodeURIComponent(curSkillSlug)}')">保存</button></div>
      </div>
      <div class="field"><label>名称</label><input type="text" id="kb-meta-name" required></div>
      <div class="field"><label>分类</label><input type="text" id="kb-meta-cat"></div>
      <div class="field"><label>描述</label><input type="text" id="kb-meta-desc"></div>`;
    // prefill with current skill name/desc from public list
    fetch('/api/skills').then(r => r.json()).then(j => {
      const sk = (j.skills || []).find(x => x.slug === curSkillSlug);
      if (!sk) return;
      $('kb-meta-name').value = sk.name || '';
      $('kb-meta-cat').value = sk.category || '';
      $('kb-meta-desc').value = sk.description || '';
    }).catch(() => {});
  };
  window.saveMeta = async (slug) => {
    const payload = {
      name: $('kb-meta-name').value,
      category: $('kb-meta-cat').value,
      description: $('kb-meta-desc').value
    };
    const r = await fetch(`/api/admin/skills/${encodeURIComponent(slug)}`, {
      method: 'PATCH', headers: authHdr(), body: JSON.stringify(payload)
    });
    const j = await r.json();
    if (r.ok) { toast('元数据已更新', 'ok'); closeDetailEditor(); loadManageSkills(); }
    else toast(j.error || '保存失败', 'err');
  };

  // ---------- new skill (AI train or manual) ----------
  window.nsTab = (mode) => {
    document.querySelectorAll('.ns-seg-btn').forEach(b => b.classList.toggle('active', b.dataset.ns === mode));
    $('ns-view-train').style.display = mode === 'train' ? 'block' : 'none';
    $('ns-view-manual').style.display = mode === 'manual' ? 'block' : 'none';
  };
  window.newSkillView = () => {
    $('skill-new-mask').style.display = 'flex';
    $('skill-new-mask').classList.remove('hidden');
    nsTab('train');
    // clear train form
    try { $('train-form').reset(); } catch (_) {}
    selectedFiles = []; try { renderTags(); } catch (_) {}
    $('tr-log').style.display = 'none'; $('tr-log').innerHTML = '';
    $('tr-result').style.display = 'none';
    const go = $('tr-go'); go.disabled = false;
    $('tr-spin').style.display = 'none'; $('tr-txt').textContent = '开始训练';
    // clear manual form
    $('ns-slug').value = ''; $('ns-name').value = ''; $('ns-cat').value = ''; $('ns-desc').value = ''; $('ns-prompt').value = '';
    $('ns-msg').textContent = ''; $('ns-msg').className = 'msg';
  };
  window.closeNewSkill = () => {
    $('skill-new-mask').style.display = 'none';
    $('skill-new-mask').classList.add('hidden');
  };
  window.doNewSkill = async () => {
    const slug = $('ns-slug').value.trim().toLowerCase().replace(/[^a-z0-9-]+/g, '-');
    const payload = {
      slug, name: $('ns-name').value.trim(),
      category: $('ns-cat').value.trim(),
      description: $('ns-desc').value.trim(),
      system_prompt: $('ns-prompt').value,
      input_params: []
    };
    if (!slug || !payload.name) { $('ns-msg').className = 'msg err'; $('ns-msg').textContent = '需填 slug 与名称'; return; }
    const r = await fetch('/api/admin/skills', { method: 'POST', headers: authHdr(), body: JSON.stringify(payload) });
    const j = await r.json();
    if (r.ok) {
      toast('技能已创建', 'ok'); closeNewSkill(); loadManageSkills(); loadSkillsPublic && loadSkillsPublic();
    } else { $('ns-msg').className = 'msg err'; $('ns-msg').textContent = j.error || '创建失败'; }
  };

  // ====== AI optimize (review) + version rollback ======
  window.showReviewPanel = () => {
    const p = $('kb-panel');
    p.classList.remove('hidden'); p.style.display = 'block';
    p.innerHTML = `
      <div class="kb-panel-head">
        <div><b>✨ AI 优化现有技能</b>
          <div class="card-sub">基于不可变「风格画像」做增量改写——只改与此指令相关的部分，绝不整篇重写。优化前会自动备份到新版本，可随时回滚。</div>
        </div>
        <div><button class="btn ghost" onclick="this.closest('#kb-panel').style.display='none'">收起</button></div>
      </div>
      <div class="field" style="margin-top:12px">
        <label>优化指令</label>
        <textarea id="rv-instruction" rows="3" placeholder="例：语气再正式一点、多补充数据论证、把第3节拆成小标题、减少感叹号、面向更年轻的读者重写开头……"></textarea>
      </div>
      <div class="field" style="margin-top:8px">
        <label>变更备注（可选，写进版本记录方便回滚识别）</label>
        <input type="text" id="rv-note" placeholder="如：调整语气为更正式">
      </div>
      <button class="btn primary" onclick="window.doReview()">
        <span id="rv-spin" class="spin" style="display:none"></span><span id="rv-txt">开始优化</span>
      </button>
      <div class="log" id="rv-log" style="margin-top:12px;display:none"></div>
      <div id="rv-result" style="display:none"></div>`;
  };

  window.doReview = async () => {
    const ins = $('rv-instruction').value.trim();
    if (!ins) { toast('请填写优化指令', 'err'); return; }
    const note = ($('rv-note').value || '').trim();
    $('rv-spin').style.display = 'inline-block'; $('rv-txt').textContent = '优化中…';
    const log = $('rv-log'); log.style.display = 'block'; log.className = 'log';
    log.textContent = '读取风格画像与当前提示词，增量改写中…';
    try {
      const r = await fetch(`/api/admin/skills/${encodeURIComponent(curSkillSlug)}/review`, {
        method: 'POST', headers: authHdr(), body: JSON.stringify({ instruction: ins, note })
      });
      const j = await r.json();
      log.className = 'log ' + (r.ok ? 'ok' : 'err');
      if (r.ok) {
        log.textContent = `优化完成 → 新版本 v${j.version}（旧 v${j.old_version}，已备份可回滚）`;
        const res = $('rv-result'); res.style.display = 'block';
        res.innerHTML = `<b style="font-size:13px">优化后的核心提示词预览（未手动保存即已落盘）</b>
          <pre style="white-space:pre-wrap;word-break:break-word;overflow-wrap:anywhere;background:var(--bg-soft);border:1px solid var(--line-soft);border-radius:var(--radius-sm);padding:12px;max-height:320px;overflow:auto;font-size:12px;margin-top:8px">${escapeTextarea(j.prompt || '')}</pre>`;
      } else {
        log.textContent = j.error || '优化失败';
      }
    } catch (e) { log.className = 'log err'; log.textContent = '网络错误'; }
    $('rv-spin').style.display = 'none'; $('rv-txt').textContent = '开始优化';
  };

  window.showVersionsPanel = () => {
    const p = $('kb-panel');
    p.classList.remove('hidden'); p.style.display = 'block';
    p.innerHTML = `<div class="kb-panel-head"><b>↩ 版本回滚</b>
      <div class="card-sub">每次 AI 优化自动备份上一版提示词，可一键回滚到任意历史版本。</div>
      <div class="dim" style="margin-top:8px">加载中…</div></div>`;
    loadVersions();
  };

  async function loadVersions() {
    const p = $('kb-panel');
    let rows = '';
    try {
      const r = await fetch(`/api/admin/skills/${encodeURIComponent(curSkillSlug)}/versions`, { headers: authHdr() });
      if (r.status === 401) { logout(); return; }
      const j = await r.json();
      const vs = j.versions || [];
      if (!vs.length) { rows = '<div class="kb-empty">暂无历史版本（首次 AI 优化后才会生成）</div>'; }
      else rows = `<div style="margin-top:12px">` + vs.map(v => `
        <div class="kb-row" style="justify-content:space-between">
          <span><b>v${esc(v.version)}</b>
            ${esc(v.note) ? `<span class="dim"> · ${esc(v.note)}</span>` : ''}
            <span class="dim"> · ${esc(v.time || '')}</span></span>
          <button class="btn ghost kb-btn" onclick="window.doRollback(${v.version})">回滚</button>
        </div>`).join('') + `</div>`;
    } catch (e) { rows = '<div class="kb-empty">网络错误</div>'; }
    p.innerHTML = `<div class="kb-panel-head"><b>↩ 版本回滚</b>
      <div class="card-sub">每次 AI 优化自动备份上一版提示词，可一键回滚到任意历史版本。</div>
      ${rows}
      <div class="msg" id="rb-msg"></div></div>`;
  }

  window.doRollback = async (version) => {
    if (!confirm(`确定回滚到 v${version}？会覆盖当前提示词与模板，但该操作本身可再次回滚。`)) return;
    const r = await fetch(`/api/admin/skills/${encodeURIComponent(curSkillSlug)}/rollback`, {
      method: 'POST', headers: authHdr(), body: JSON.stringify({ version })
    });
    const j = await r.json();
    const msg = $('rb-msg');
    if (r.ok) { msg.className = 'msg ok'; msg.textContent = `已回滚 → v${j.version}`; loadVersions(); }
    else { msg.className = 'msg err'; msg.textContent = j.error || '回滚失败'; }
  };

  function logout() {
    localStorage.removeItem(TOKEN_KEY);
    $('shell').style.display = 'none';
    $('login-view').style.display = 'block';
  }

  // auto enter if token exists
  if (token()) enter();
  else { $('login-view').style.display = 'block'; }
})();