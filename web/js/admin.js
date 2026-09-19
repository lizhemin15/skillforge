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
  let cmReadOnly = false;   // 当前编辑器是不是只读视图（style_profile.md 这类不可变锚点）

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
    loadMCP();
    loadManageSkills();
    loadSite();
    loadAccount();
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
    $('llm-name').value = c.provider || ''; // 后端只有 provider（显示名），没有 name 字段
    $('llm-base').value = c.base_url || '';
    $('llm-model').value = c.model || '';
    $('llm-key').value = '••••••••'; // masked placeholder
    $('llm-key').placeholder = '留空保持不变';
    $('llm-key').required = false;
    $('llm-cancel').style.display = 'inline-flex';
    $('llm-model-list').innerHTML = ''; // 换了服务，上一个的模型清单留着会误导
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
  // ---------- LLM: 自动获取模型清单 ----------
  // 手打模型名是最大的低级错误来源（差一个字符就 400），所以支持一键从 provider 拉取。
  // 用 datalist 而不是 <select>：既能下拉挑，也能手填 provider 没列出来的名字。
  $('llm-fetch-models').addEventListener('click', async () => {
    const msg = $('llm-msg');
    const btn = $('llm-fetch-models');
    const base = $('llm-base').value.trim();
    if (!base) { msg.className = 'msg err'; msg.textContent = '请先填写 API Base URL'; return; }
    btn.disabled = true; btn.textContent = '获取中…';
    msg.className = 'msg'; msg.textContent = '正在从该服务拉取模型清单…';
    try {
      const r = await fetch('/api/admin/llms/models', {
        method: 'POST', headers: authHdr(),
        body: JSON.stringify({
          id: parseInt($('llm-id').value || '0') || 0,
          base_url: base,
          api_key: $('llm-key').value, // 编辑态这里是掩码，后端会自动回退用库里已存的 key
        }),
      });
      const j = await r.json();
      if (!r.ok) { msg.className = 'msg err'; msg.textContent = j.error || '获取失败'; return; }
      const list = j.models || [];
      $('llm-model-list').innerHTML = list.map(m => `<option value="${esc(m)}"></option>`).join('');
      if (list.length === 1 && !$('llm-model').value) $('llm-model').value = list[0];
      msg.className = 'msg ok';
      msg.textContent = `拉到 ${list.length} 个模型（来源 ${j.endpoint}）。点模型输入框可下拉选择，也可继续手填`;
    } catch (err) {
      msg.className = 'msg err'; msg.textContent = '网络错误';
    } finally {
      btn.disabled = false; btn.textContent = '获取模型';
    }
  });

  $('llm-cancel').addEventListener('click', () => {
    $('llm-form').reset(); $('llm-id').value = 0; $('llm-cancel').style.display = 'none';
    $('llm-key').required = true; $('llm-key').placeholder = 'sk-…';
  });

  $('llm-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const isNew = $('llm-id').value === '0';
    const payload = {
      id: isNew ? 0 : parseInt($('llm-id').value),
      // 字段名必须和 model.LLMConfig 的 json tag 对齐：后端 readBody 用
      // DisallowUnknownFields 解码，多一个/错一个名字就是 400「请求体无效」，
      // 报错地点离原因很远。（这里曾经发的是 name，后端只有 provider，导致保存必失败。）
      provider: $('llm-name').value.trim(),
      base_url: $('llm-base').value.trim().replace(/\/+$/, ''),
      model: $('llm-model').value,
      api_key: $('llm-key').value,
      // 新增即启用；编辑时不表态（false），由后端沿用该记录原有的启用状态，
      // 免得「改个模型名」顺手把正在用的服务停掉。
      is_active: isNew
    };
    // if masked placeholder on update, skip key
    if (!isNew && ($('llm-key').value === '••••••••' || $('llm-key').value === '')) delete payload.api_key;
    const msg = $('llm-msg');
    msg.textContent = '保存中…';
    try {
      const r = await fetch('/api/admin/llms', { method: 'POST', headers: authHdr(), body: JSON.stringify(payload) });
      const j = await r.json();
      if (!r.ok) { msg.className = 'msg err'; msg.textContent = j.error || '保存失败'; return; }
      // 别再说「已保存并自动切换」——编辑态后端是沿用原状态，根本没切换，骗人。
      msg.className = 'msg ok'; msg.textContent = isNew ? '已保存并启用' : '已保存';
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

  // ---------- train: 实况区「贴底」 ----------
  // 为什么需要它（线上实测，2026-09-18）：
  //   材料在流、计时器在跳，但用户看不见材料 —— 因为 `.modal` 自己就是滚动容器
  //   （max-height:86vh; overflow-y:auto），长「写作要求」把 #tr-log 顶到了 modal 底线以下。
  //   实测：modal 内容高 1130px / 可视 772px，scrollTop 停在 0，#tr-log 只露出 45px，
  //   材料块在底线外 329px。用户原话「中间可以流式输出思考的一些中间材料，现在一直卡着计时」
  //   说的就是这个 —— 只贴内层日志（下面那句 scrollTop）治不了「看不见」。
  // 两条规矩：
  //   ① 内层日志永远贴底（材料像终端一样滚）；
  //   ② 外层 modal 也贴底，但**用户自己往上翻就松手**，不跟用户抢滚动条。
  function makeLivePinner(modal, logEl) {
    const NEAR = 40; // px：离底多少算「还在底部」
    let on = true;
    if (modal && modal.addEventListener) {
      modal.addEventListener('scroll', () => {
        on = modal.scrollHeight - modal.scrollTop - modal.clientHeight <= NEAR;
      }, { passive: true });
    }
    return function pinLive() {
      if (!modal || !logEl) return;
      logEl.scrollTop = logEl.scrollHeight; // ①
      if (on) modal.scrollTop = modal.scrollHeight; // ②
    };
  }

  // ---------- train: submit + SSE progress ----------
  $('train-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const go = $('tr-go'), spin = $('tr-spin'), txt = $('tr-txt');
    go.disabled = true; spin.style.display = 'inline-block'; txt.textContent = '训练中…';
    $('tr-log').style.display = 'block';
    $('tr-log').innerHTML = '';
    $('tr-result').style.display = 'none';
    // 每场训练一份贴底器：把实况区拉进视野（上一场的手动翻页状态不该带到这一场）。
    const pinLive = makeLivePinner($('skill-new'), $('tr-log'));
    pinLive(); // 点「开始训练」就先把实况区拉进视野，而不是等第一帧材料

    const fd = new FormData();
    fd.append('name', $('tr-name').value);
    fd.append('category', $('tr-cat').value || '');
    fd.append('description', $('tr-desc').value || '');
    fd.append('requirement', $('tr-req').value || '');
    for (const f of selectedFiles) fd.append('files', f);

    // 心跳：后端只在阶段边界发帧，阶段内部可能几十秒静默（一跑二十分钟）。
    // 屏幕必须持续有「还在跑」的活气，否则用户看到的只是一个不动的框 —— 投诉原话
    // 「一直卡着计时，用户体验不佳」就是这个。每秒刷新已用时长 + 距上次进度多久。
    const tickFmt = (s) => (s >= 60 ? Math.floor(s / 60) + 'm' + String(s % 60).padStart(2, '0') + 's' : s + 's');
    let lastEvAt = Date.now();
    const t0 = Date.now();
    const hb = setInterval(() => {
      const now = Date.now();
      txt.textContent = '训练中… 已 ' + tickFmt(Math.round((now - t0) / 1000)) +
        '（距上次进度 ' + tickFmt(Math.round((now - lastEvAt) / 1000)) + '）';
    }, 1000);

    const logLine = (stage, s, cls) => {
      lastEvAt = Date.now();
      const div = document.createElement('div');
      div.className = 'ln ' + (cls || '');
      div.innerHTML = `<span class="t">${esc(stage)}</span><span class="s">${esc(s)}</span>`;
      $('tr-log').appendChild(div);
      // 实况材料块始终贴在最下面：新阶段日志插在它前面，材料块跟着往下走。
      const live = $('tr-material');
      if (live) $('tr-log').appendChild(live);
      pinLive();
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
            // 终帧到了，实况块收掉：留着会让人以为还在跑。
            if (ev.type === 'done' || ev.type === 'error') {
              const live = $('tr-material');
              if (live) live.remove();
            }
            switch (ev.type) {
              // ⚠️ 契约：后端 internal/api/admin.go 发的是 status / step / error / done。
              // 这里以前只认 'stage'，于是「开始训练」和整场训练的阶段进度帧被静默丢弃 ——
              // 界面上只剩一个空日志框 + 不动的「训练中…」，二十分钟看不出任何进展。
              // 事件名对不上是静默故障（没有任何报错），所以下面配了契约测试守着。
              // 中间材料：模型流式吐出的思考链/正文片段（后端已攒批：~400ms 或 240 字节一帧）。
              // 必须**就地更新**同一个实况块，不能一片一个 DOM 节点 —— 二十分钟下来那是
              // 几万个节点，页面直接被拖死。这里只保留尾部若干字符，像终端在跑。
              case 'delta': {
                let d = ev.data;
                if (typeof d !== 'object') {
                  try { d = JSON.parse(d); } catch (_) { d = { kind: 'note', text: String(ev.data) }; }
                }
                const label = d.kind === 'think' ? '思考' : (d.kind === 'note' ? '提示' : '正文');
                let mp = $('tr-material');
                if (!mp) {
                  mp = document.createElement('div');
                  mp.id = 'tr-material';
                  mp.className = 'ln material';
                  mp.style.cssText = 'color:#6b7280;white-space:pre-wrap;word-break:break-all;border-left:2px solid #d1d5db;padding-left:8px;margin:6px 0;';
                  $('tr-log').appendChild(mp);
                }
                // 换类别时补一个分隔，否则「思考」和「正文」会连成一句读。
                // 注意：分隔符必须拼进下面那次赋值里。先 += 再整体覆盖「看着等价」，
                // 实际会把刚加的分隔符冲掉（本条行为测试抓到的就是这个）。
                const sep = (mp.dataset.kind && mp.dataset.kind !== d.kind) ? '\n' : '';
                mp.dataset.kind = d.kind;
                mp.textContent = sep + '… ' + label + '：' + String(d.text || '').replace(/\s+/g, ' ');
                // 尾部截断：DOM 里只留最近这一段，历史材料没有回看需求（阶段边界会打点）。
                if (mp.textContent.length > 700) mp.textContent = '… ' + mp.textContent.slice(-697);
                lastEvAt = Date.now();
                pinLive();
                break;
              }
              case 'status': logLine('•', ev.data, ''); break;
              case 'step':
              case 'stage': // 旧名，向后兼容
                logLine('→', ev.data, ''); break;
              case 'done':
                const r = ev.data && typeof ev.data === 'object' ? ev.data : JSON.parse(ev.data);
                // 降级交付必须显性说出来：技能落盘了，但裁判没验收通过（没跑完或没过线）。
                // 以前这种情况只在 fidelity.md 里留一行，前端照样显示「已就绪」——
                // 于是「生成的技能和我给的素材没关系」这件事在界面上完全看不出来。
                if (r.degraded) {
                  logLine('⚠️', '技能已生成，但裁判未验收通过：' + (r.degrade_reason || '原因未知'), 'err');
                } else {
                  logLine('✔', '技能「' + r.name + '」训练完成', 'ok');
                }
                const rd = $('tr-result');
                rd.style.display = 'block';
                rd.innerHTML = `<h4${r.degraded ? ' style="color:#c0392b"' : ''}>${r.degraded ? '⚠️ 新技能已就绪（未验收通过）' : '✔ 新技能已就绪'}</h4>
                  <div class="row">名称：<b>${esc(r.name)}</b> (§ ${esc(r.slug)})</div>
                  <div class="row">版本：<b>v${r.version}</b></div>
                  <div class="row">参数：<b>${(r.input_params || []).length}</b> 个</div>
                  <div class="params">${(r.input_params || []).map(p => `<span class="param-tag">${esc(p.label || p.name)}</span>`).join('')}</div>
                  ${r.degraded ? `<div class="row" style="margin-top:10px;color:#c0392b">裁判未验收通过：${esc(r.degrade_reason || '')}</div>
                  <div class="row" style="color:#888">技能已落盘可用，但请人工复核 fidelity.md 与范文后再投入生产。</div>` : ''}
                  <div class="row" style="margin-top:10px">前台列表已可用，也可在「技能管理」里编辑。</div>`;
                loadManageSkills();
                break;
              case 'error':
                // 素材门禁失败等硬错误必须「响亮」：以前只往日志里打一行红字，
                // 用户滚一下就当警告忽略了，然后拿着上一次的成功结果以为训练过了。
                logLine('✕', ev.data, 'err');
                {
                  const rd = $('tr-result');
                  rd.style.display = 'block';
                  rd.innerHTML = `<h4 style="color:#c0392b">✕ 训练已中止</h4>
                    <div class="row">${esc(ev.data)}</div>
                    <div class="row" style="margin-top:10px;color:#888">素材没读到内容时，本次不会生成技能——避免交付一份与素材无关的成品。</div>`;
                }
                break;
            }
          } catch (_) {}
        }
      }
    } catch (err) {
      logLine('!', '网络错误：' + err.message, 'err');
    } finally {
      clearInterval(hb);
      go.disabled = false; spin.style.display = 'none'; txt.textContent = '开始训练';
      selectedFiles = []; renderTags();
    }
  });

  // ---------- skill management ----------
  // ⚠️ 管理列表打的是 **/api/admin/skills（全量）**，不是公开的 /api/skills。
  // 公开列表按设计过滤掉停用技能，用它渲染管理列表的话：停用一次技能，这一行
  // 就从界面上消失 —— 下面渲染的「启用」按钮和「已停用」药丸永远没人能点到，
  // 想恢复只能手改 sqlite（Bug N：用户报「业务技能停用了就消失了」）。
  async function loadManageSkills() {
    try {
      const r = await fetch('/api/admin/skills', { headers: authHdr() });
      const j = await r.json();
      const list = (j.skills || []);
      const box = $('skill-manage-list');
      if (!list.length) { box.innerHTML = '<div class="empty"><h3>暂无技能</h3></div>'; return; }
      const core = list.filter((s) => s.is_core);
      const biz = list.filter((s) => !s.is_core);
      // 核心 = 通用能力（办公文档管家、技能工厂），换谁用都得有；业务技能只对
      // 某个场景成立。两者混在一列里，用户根本看不出哪些是「平台自带的本事」。
      const rowHtml = (sk, isCoreGroup) => `
        <div class="prov-row${sk.is_core ? ' is-core' : ''}">
          <div class="pk">${isCoreGroup ? '★' : '✦'}</div>
          <div class="inf">
            <div class="nm">${esc(sk.name)} ${sk.is_core ? '<span class="pill-active">核心</span>' : ''}${sk.enabled ? '' : '<span class="pill-off">已停用</span>'}</div>
            <div class="dt">${esc(sk.slug)} · v${sk.version} · ${(sk.input_params || []).length} 参数</div>
          </div>
          <div class="row-actions">
            <button class="btn ghost" style="padding:4px 8px;font-size:12px" onclick="window.openSkillDetail('${esc(sk.slug)}','${esc(sk.name.replace(/'/g, "\\'"))}')">编辑</button>
            <button class="btn ghost" style="padding:4px 8px;font-size:12px" onclick="window.toggleSkill('${esc(sk.slug)}',${!sk.enabled})">${sk.enabled ? '停用' : '启用'}</button>
            <button class="btn ghost" style="padding:4px 8px;font-size:12px" title="${sk.is_core ? '取消后变成业务技能，可以被删除' : '标记为通用能力，排在聊天技能列表最前面且不可删除'}" onclick="window.setSkillCore('${esc(sk.slug)}',${!sk.is_core})">${sk.is_core ? '取消核心' : '设为核心'}</button>
            ${sk.is_core ? '' : `<button class="icon-btn danger" title="删除" onclick="window.delSkill('${esc(sk.slug)}')">✕</button>`}
          </div>
        </div>`;
      const groupBlock = (title, sub, items, isCoreGroup) => `
        <div class="mg-group">
          <div class="mg-head">
            <span class="mg-dot${isCoreGroup ? ' core' : ''}"></span>
            <div class="mg-htxt"><div class="mg-title">${title}</div><div class="mg-sub">${sub}</div></div>
            <span class="mg-count">${items.length}</span>
          </div>
          <div class="mg-rows">${
            items.length
              ? items.map((sk) => rowHtml(sk, isCoreGroup)).join('')
              : `<div class="mg-empty">${isCoreGroup ? '还没有核心技能。在业务技能右侧点「设为核心」，把通用能力提上来。' : '暂无业务技能，点右上角「+ 新建技能」加一个。'}</div>`
          }</div>
        </div>`;
      box.innerHTML =
        groupBlock('核心技能 · 通用能力', '与具体业务无关、换谁用都得有的本事，例如按需求造技能、生成办公文档。核心技能不可删除。', core, true) +
        groupBlock('业务技能 · 具体场景', '只在某类业务里成立的本事，例如某公司的通稿、某种办事流程。它们不占核心位。', biz, false);
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

  // 核心/业务是**语义**上的分类，不是权限位：核心技能排在聊天技能列表最前面，
  // 并且受删除保护（api 层同样挡，不是只靠前端藏按钮）。
  window.setSkillCore = async (slug, isCore) => {
    if (!isCore && !confirm('取消核心后，该技能会变成业务技能，并且可以被删除。继续？')) return;
    const r = await fetch('/api/admin/skills/core', { method: 'POST', headers: authHdr(), body: JSON.stringify({ slug, is_core: isCore }) });
    const j = await r.json().catch(() => ({}));
    if (r.ok) { toast(isCore ? '已设为核心技能' : '已取消核心', 'ok'); loadManageSkills(); loadSkillsPublic && loadSkillsPublic(); }
    else toast(j.error || '操作失败', 'err');
  };

  // ---------- knowledge-base skill detail ----------
  // slug 在 onclick 属性里是 encodeURIComponent 过的（不然中文/引号塞不进属性），
  // 而拼 API URL 时还要再编码一次交给 HTTP —— 不加区分地拼，就会**编码两次**：
  // 服务端解出来是字面量 "%E9%87%87%E8%B4%AD%E5%90%88%E5%90%8C"，一律 404。
  // 本轮实测：技能文件管理里的「删除」「上传素材」「新增范文」以及新加的模板上传/下载
  // 全都栽在这上面（界面上有按钮，点了报「技能不存在 / no such file」）。
  // 统一走这两个助手：decSlug 拿原始名，urlSlug 得到「恰好一次」编码的 URL 片段。
  function decSlug(slug) {
    try { return decodeURIComponent(slug); } catch (e) { return slug; } // 名字里真有 % 时不炸
  }
  function urlSlug(slug) { return encodeURIComponent(decSlug(slug)); }

  // templatefile = 技能目录里真被 fill_template 填充的 .docx/.xlsx 模板；
  // 和 template（写作模板 template.md，教模型「怎么写」）是两回事，必须分开列，
  // 否则用户会去改那份 md，以为改了模板样式。
  //
  // 本轮新增三个 kind（字符串是与后端的契约，不许改）：
  //   category —「分类要求」：categories/_index.md（分类路由表）+ categories/01-经营业绩.md…
  //              人工可编辑，是这次的核心新功能（后端 fileKind 返回 editable=true）。
  //   reviewer —「审稿清单」：reviewer.md，审稿人逐条核对的判据，可编辑。
  //   fidelity —「质量报告」：fidelity.md，机器产出的裁判试跑报告，**只读**（后端 editable=false）。
  // 图标刻意与既有 8 个不撞车：📚 分类手册 / 🔍 逐条核对 / 📊 试跑报告。
  const KIND_LABEL = { prompt: '核心提示词', template: '写作模板', templatefile: '模板文件（可填）', requirement: '训练需求', category: '分类要求', style: '风格画像', example: '参考范文', source: '原始素材', reviewer: '审稿清单', fidelity: '质量报告', other: '其他' };
  const KIND_ICON = { prompt: '🧠', template: '📋', templatefile: '📃', requirement: '📐', category: '📚', style: '🎯', example: '📄', source: '📁', reviewer: '🔍', fidelity: '📊', other: '📎' };
  // 空分组也要显示的 kind（用户来这里就是为了找模板/加范文，隐藏空组等于把入口藏了）
  // category 进这张表是**有意的**：它是本轮核心新功能，用户是专门来找「分类要求」的，
  // 而分类由训练流程抽取后才有文件 —— 如果空着就藏起来，用户看到的就是「这功能是不是没上线」，
  // 正是这次反馈的「左侧分类各不一样、看不懂什么逻辑」的根因。空组 + 一句说明比静默消失清楚。
  // 文案把两件事说清楚：(1) 分类是抽出来的、每个技能天生不一样（不是数据错乱）；
  // (2) 想要手册里没有的分类，点本组标题右侧那颗按钮自己加。
  // reviewer / fidelity 不放：它们是流程/机器产物，用户在这两个分组里没有可做的动作，
  // 空着显示只会占屏（内容生成后分组自然出现）。
  const KIND_EMPTY_HINT = {
    templatefile: '暂无模板文件，点上方「上传模板」加一个（.docx/.xlsx，同名重传即替换）',
    example: '暂无，下方添加',
    category: '暂无分类。分类由训练从手册抽出，各技能不一样；要加手册里没有的，点上面的「+ 新增分类」（categories/_index.md 是分类路由表）',
  };
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
      // 注：后端仍然下发 manual_mode（= 技能目录里有没有 categories/），但前端**不再拿它
      // 当门禁**——分类的新增/改名/删除已对非手册技能放开（后端会把 categories/ 目录、
      // _index.md 路由表一起建出来）。字段留着是因为「这个技能本来走哪条运行时路线」
      // 仍是回执文案与后端提示要用的信息，不是死字段。
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
    // 分组显示顺序。**必须与后端 store.orderOf() 逐项一致**，否则同一个技能目录
    // 在接口里是一种顺序、在树上又是另一种，用户会以为「两处内容不一样」。
    // 后端 orderOf：prompt=1, template=2, templatefile=3, requirement=4, category=5,
    //               style=6, example=7, source=8, reviewer=9, fidelity=10。
    // 语义：核心提示词 → 模板类 → 需求/分类（「要什么」与「各类别怎么写」是一段叙事，挨着放）
    //       → 风格 → 范文/素材 → 审稿清单 → 质量报告垫底（机器产物，放最后看）。
    // 这个数组同时是「哪些 kind 会出现在树上」的**白名单**：不在这里的 kind 一律不渲染
    // （other 故意不在其中）。所以后端新加 kind 时，这里不补上 = 用户在界面上根本看不到那个文件。
    const groups = ['prompt', 'template', 'templatefile', 'requirement', 'category', 'style', 'example', 'source', 'reviewer', 'fidelity'];
    // LEFT: file tree; RIGHT: editor
    let tree = '', right = '';
    // top action row (new example / upload source) pinned above tree
    tree += `<div class="kb-tree-actions">
        <button class="btn ghost" onclick="window.addExampleView()">+ 新增范文</button>
        <button class="btn ghost" onclick="window.uploadTemplateView()">+ 上传模板</button>
        <button class="btn ghost" onclick="window.uploadSourceView()">+ 上传素材</button>
        <button class="btn ghost" onclick="window.editMetaView()">元数据</button>
        <button class="btn ghost" onclick="window.showReviewPanel()">✨ AI 优化</button>
        <button class="btn ghost" onclick="window.showVersionsPanel()">↩ 版本</button>
      </div>`;
    for (const k of groups) {
      const items = files.filter(f => f.kind === k);
      if (!items.length && !KIND_EMPTY_HINT[k]) continue;
      // 「分类要求」组的标题右侧挂「+ 新增分类」。**不再按手册模式门禁**（2026-09 放开）：
      // 早先非手册技能没有 categories/ 目录、后端 create 只会回 400「不是手册模式」，
      // 所以把这颗按钮藏起来；现在后端允许凭空建分类（连 categories/ 目录一起建），
      // 门禁消失。留着门禁的后果是：非手册技能永远看不到这个入口，而它恰恰是
      // 「手册里没有我们常写的那类稿子」时**唯一**的自助路径 —— 用户只能去 SSH 改磁盘。
      // 分类**不是固定枚举**：它是训练期从手册里抽出来的章节骨架，所以两个技能的
      // 分类清单本来就不一样（一个手册 12 类、另一个 5 类）。这正是「左侧分类各不一样」的
      // 正常原因，不是数据错乱；KIND_EMPTY_HINT[category] 的空组文案负责把这件事讲出来。
      const titleOps = (k === 'category')
        ? ` <button class="link-btn" title="新增一个分类（手册里没有的也可以加；非手册技能会把分类体系一起建起来）" onclick="window.newCategoryView()">+ 新增分类</button>`
        : '';
      tree += `<div class="kb-group">
        <div class="kb-group-title">${KIND_ICON[k]} ${KIND_LABEL[k]} <span class="dim" style="font-weight:400">(${items.length})</span>${titleOps}</div>
        <div class="kb-list">`;
      if (!items.length) {
        tree += `<div class="kb-empty">${KIND_EMPTY_HINT[k] || '暂无'}</div>`;
      } else {
        for (const f of items) {
          const canEdit = f.editable;
          // 可删白名单 = 后端 store.DeleteFile 真正允许删的三类（example / source /
          // templatefile，见 internal/store/skill_files.go）。**别把它放宽成「canEdit 就可删」**：
          // 后端对 prompt/template/requirement/category/reviewer 一律回「核心文件不可删除」，
          // 前端多给一个删除按钮 = 承诺一个点下去只会报错的操作。
          // fidelity（质量报告）与 style（风格锚点）是只读的**质量证据**，既不可编辑也不可删，
          // 所以两个入口都不能有 —— 这里靠身不在白名单里达成，不要顺手加进去。
          const canDel = f.kind === 'example' || f.kind === 'source' || f.kind === 'templatefile';
          // 分类行的「改名 / 删除」入口。判据是后端下发的 category_name **有没有值**，
          // 而不是「路径是不是以 categories/ 开头」：前缀猜法等于把 store 的命名规则
          // 抄第二份，两份规则一漂移，界面就会摆出「点下去必 400」的按钮。
          // categories/_index.md（路由表）与 _template.md 不是分类，后端不给 category_name，
          // 于是天然没有这两个入口 —— 它们本来也不许删。
          const isCategory = k === 'category' && !!f.category_name;
          const catName = f.category_name || '';
          // 只读文本在树上就要看得出来，别等点进去才发现改不了
          const roTag = fileViewMode(f) === 'readonly' ? ` <span class="chip">只读</span>` : '';
          // 分类的**展示名**取自分类文件里的首个标题（H1），和文件名可能不一致
          // （03-新闻通稿.md 的 H1 就是「领导讲话稿」）。不显式标出来，用户看到
          // 「文件名和界面上的名字对不上」只会以为是 bug；而运行时路由找的正是这个 H1 名。
          // 没有 category_name 的（_index.md / _template.md）不打标签，一眼能区分
          // 「这是分类」和「这是分类制度的说明文件」。
          const catTag = isCategory ? ` <span class="chip" title="分类展示名（取自文件首个标题，运行时按它路由）">${esc(catName)}</span>` : '';
          tree += `<div class="kb-tree-row" data-path="${esc(f.path)}" onclick="window.openEditorFile('${encodeURIComponent(curSkillSlug)}','${escapeJs(f.path)}')" title="${esc(f.path)}">
            <span class="kb-tree-name">${esc(f.name)} <span class="dim">${f.size}B</span>${catTag}${roTag}</span>
            <span class="kb-tree-ops">
              ${isCategory ? `<button class="link-btn" title="改名（级联改写路由表与审稿清单；范文原文不动）" onclick="event.stopPropagation();window.renameCategoryView('${escapeJs(f.path)}','${escapeJs(catName)}')">改名</button>` +
                `<button class="icon-btn danger" title="删除该分类" onclick="event.stopPropagation();window.delCategory('${encodeURIComponent(curSkillSlug)}','${escapeJs(f.path)}','${escapeJs(catName)}')">✕</button>` : ''}
              ${canEdit ? `<button class="link-btn" title="编辑" onclick="event.stopPropagation();window.openEditorFile('${encodeURIComponent(curSkillSlug)}','${escapeJs(f.path)}')">编辑</button>` : ''}
              <button class="link-btn" title="下载" onclick="event.stopPropagation();window.downloadSkillFile('${encodeURIComponent(curSkillSlug)}','${escapeJs(f.path)}')">⬇</button>
              ${canDel ? `<button class="icon-btn danger" title="删除" onclick="event.stopPropagation();window.delSkillFile('${encodeURIComponent(curSkillSlug)}','${escapeJs(f.path)}','${escapeJs(f.kind)}')">✕</button>` : ''}
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
    function mountCm(holder, opts) {
      opts = opts || {};
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
        readOnly: !!opts.readOnly,
        foldGutter: true,
        gutters: ['CodeMirror-foldgutter', 'CodeMirror-linenumbers'],
        extraKeys: {
          'Ctrl-S': function () { if (!opts.readOnly && cmSaveCb) cmSaveCb(); },
          'Cmd-S': function () { if (!opts.readOnly && cmSaveCb) cmSaveCb(); },
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
      cmReadOnly = !!opts.readOnly;
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
      cmSaveCb = null; cmEditorSlug = null; cmReadOnly = false;
    }

    function cmContent() {
      if (cmEditor) return cmEditor.getValue();
      const el = $('kb-content'); return el ? el.value : '';
    }

    // 单个文件的展示模式：'preview'（二进制，走预览）| 'readonly'（只读文本）| 'edit'。
    //
    // 为什么必须区分：后端 store.WriteFile 对「不可变锚点」是硬拒的 ——
    // style_profile.md 在 fileKind() 里被标成 editable=false，PUT 直接 400
    // 「该文件只读，不可编辑」（见 internal/store/skill_files.go）。
    // 前端若仍然渲染一个可编辑框 + 「保存」按钮，就是在承诺一个它做不到的操作：
    // 用户认真改完点保存，等来的只有一条报错，且改动全丢。
    // 之前的实现就是这么干的 —— 列表接口明明带着 editable:false，编辑器完全无视它。
    function fileViewMode(f) {
      if (!f) return 'edit';
      if (f.binary) return 'preview';
      if (f.editable === false) return 'readonly';
      return 'edit';
    }
    window.fileViewMode = fileViewMode;

    // 只读原因：不能只甩一个灰标签，得说清为什么不让改。
    function readOnlyNotice(f) {
      if (f && f.kind === 'style') {
        return '风格锚点由训练流程固化：改了会让后续生成的文风漂移，故不可编辑';
      }
      if (f && f.kind === 'fidelity') {
        // fidelity.md 是 review/optimize 流程自己写的裁判试跑报告。这里给出具体原因，
        // 而不是落到下面那句泛泛的「只读内容」——用户看到「质量报告」只想改，得先知道
        // 改了也会在下次评测被覆盖，才不会来回试。
        return '质量报告由评测流程自动产出：手动改动会在下次试跑时被覆盖，故不可编辑';
      }
      return '该文件为只读内容，不可编辑';
    }
    window.readOnlyNotice = readOnlyNotice;

    // open a file from the tree -> highlight + edit
    window.openEditorFile = async (slug, path) => {
      slug = decSlug(slug);
      // highlight active row
      const rows = document.querySelectorAll('#kb-tree .kb-tree-row');
      rows.forEach(r => r.classList.toggle('active', r.dataset.path === decodeURIComponent(path)));
      await window.editSkillFile(encodeURIComponent(slug), path);
    };

    // edit a file (CodeMirror)
    window.editSkillFile = async (slug, path) => {
      slug = decSlug(slug);
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
        const mode = fileViewMode(j);
        // Binary (PDF/image/Office): read via /raw as a blob, render read-only preview.
        if (mode === 'preview') {
          ed.innerHTML =
            '<div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:8px">' +
              '<b>预览 ' + esc(j.path) + '</b>' +
              '<div style="display:flex;gap:8px;align-items:center">' +
                '<button class="btn ghost" onclick="window.downloadSkillFile(\'' + encodeURIComponent(slug) + '\',\'' + path + '\')">下载</button>' +
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
              // 逐页统计 + 警告条：混合型 PDF 只读出薄水印层时，旧版这里显示
              // 「已提取文本 · 1129 字符」，看着一切正常，用户完全无从察觉。
              const st = pj.stats || {};
              const pageInfo = (typeof st.pages === 'number' && st.pages > 0)
                ? (' · 共 ' + st.pages + ' 页（文本层 ' + (st.text_pages || 0) + ' / OCR ' + (st.ocr_pages || 0) + ' / 空白 ' + (st.empty_pages || 0) + '）')
                : '';
              const warnBar = pj.warning
                ? '<div style="padding:10px 12px;font-size:12px;color:#e0a030;background:rgba(224,160,48,.08);border-bottom:1px solid var(--border)">⚠️ ' + esc(pj.warning) + '　如内容明显偏少，请检查是否为扫描件或尝试重新导出 PDF。</div>'
                : '';
              holder.innerHTML =
                warnBar +
                '<div style="padding:8px 12px;font-size:12px;color:#888;border-bottom:1px solid var(--border);display:flex;justify-content:space-between;align-items:center">' +
                  '<span>已提取文本 · ' + esc(t.length) + ' 字符（文档内容自动解析）' + esc(pageInfo) + '</span>' +
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

        // 只读文本（style_profile.md 等不可变锚点）：渲染成查看器，不给保存入口。
        if (mode === 'readonly') {
          ed.innerHTML =
            '<div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:8px">' +
              '<b>' + esc(j.path) + '</b>' +
              '<div style="display:flex;gap:8px;align-items:center">' +
                '<span class="chip">只读</span>' +
                '<span class="dim" style="font-size:11.5px">' + esc(readOnlyNotice(j)) + '</span>' +
                '<span id="cm-stats" class="dim" style="font-size:11.5px"></span>' +
                '<button class="btn ghost" onclick="toggleCmFullscreen()">全屏</button>' +
                '<button class="btn ghost" onclick="window.downloadSkillFile(\'' + encodeURIComponent(slug) + '\',\'' + path + '\')">下载</button>' +
                '<button class="btn ghost" onclick="closeDetailEditor()">关闭</button>' +
              '</div>' +
            '</div>' +
            '<div id="cm-holder"></div>';
          cmSaveCb = null; // Ctrl-S 不接任何保存回调
          cmEditorSlug = slug;
          const roEditor = mountCm(ed.querySelector('#cm-holder'), { readOnly: true });
          roEditor.setValue(j.content || '');
          roEditor.on('change', updateCmStats);
          updateCmStats();
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
      slug = decSlug(slug);
      // 双保险：只读视图本来就不渲染保存入口，这里再拦一道，
      // 免得将来某处误接线又把请求发到后端（后端会 400，用户白等一条报错）。
      if (cmReadOnly) { toast('该文件为只读内容，不可编辑', 'err'); return; }
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
    const r = await fetch(`/api/admin/skills/${urlSlug(slug)}/example`, {
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
    const r = await fetch(`/api/admin/skills/${urlSlug(slug)}/file`, {
      method: 'POST', headers: { 'Authorization': 'Bearer ' + token() }, body: fd
    });
    const j = await r.json();
    if (r.ok) { toast('素材已上传', 'ok'); closeDetailEditor(); loadSkillFiles(curSkillSlug); }
    else toast(j.error || '上传失败', 'err');
  };

  // 上传可填模板（.docx/.xlsx）——落到技能目录顶层，也就是 fill_template 真正读的位置。
  // 换模板 = 同名重传：后端同名覆盖，前端这里不用另设「替换」入口。
  window.uploadTemplateView = () => {
    const ed = $('kb-editor');
    ed.style.display = 'block'; ed.classList.remove('hidden');
    ed.classList.remove('ide-mode'); ed.classList.add('form-mode');
    ed.innerHTML = `
      <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:8px">
        <b>上传模板文件</b>
        <div><button class="btn ghost" onclick="closeDetailEditor()">取消</button>
        <button class="btn primary" onclick="window.doUploadTemplate('${encodeURIComponent(curSkillSlug)}')">上传</button></div>
      </div>
      <div class="drop" id="tpl-drop" style="margin-bottom:8px">点击选择模板文件（.docx / .xlsx）</div>
      <p class="dim" style="margin:0">同名重传即替换原模板；模板不参与版本回滚，替换前可先下载留存。</p>
      <input type="file" id="tpl-input" accept=".docx,.xlsx" style="display:none">`;
    const drop = $('tpl-drop'), input = $('tpl-input');
    drop.addEventListener('click', () => input.click());
    input.addEventListener('change', () => { if (input.files.length) drop.textContent = '已选择：' + input.files[0].name; });
  };
  window.doUploadTemplate = async (slug) => {
    const input = $('tpl-input');
    if (!input.files.length) { toast('请选择模板文件', 'err'); return; }
    const fd = new FormData();
    fd.append('file', input.files[0]);
    fd.append('target', 'template'); // 后端据此落到技能目录顶层（而非 source/），并跳过文字抽取
    const r = await fetch(`/api/admin/skills/${urlSlug(slug)}/file`, {
      method: 'POST', headers: { 'Authorization': 'Bearer ' + token() }, body: fd
    });
    const j = await r.json();
    if (r.ok) { toast('模板已上传', 'ok'); closeDetailEditor(); loadSkillFiles(curSkillSlug); }
    else toast(j.error || '上传失败', 'err');
  };

  // 下载技能文件。
  //
  // 不能用 `<a href="…&download=1">`：那个接口走 Bearer 头鉴权，`<a>` 带不上头，
  // 浏览器点下去只会拿到 401 —— 界面上有「⬇」但永远下不来（本轮真浏览器的实测结果，
  // 之前一直被「验证代理替请求补了 Authorization」掩盖着，看不出问题）。
  // 所以改成前端自己 fetch（带 authHdr()）→ 转 blob → 触发保存。
  window.downloadSkillFile = async (slug, path) => {
    const url = `/api/admin/skills/${urlSlug(slug)}/file?path=${encodeURIComponent(path)}&download=1`;
    let r;
    try {
      r = await fetch(url, { headers: authHdr() });
    } catch (e) { toast('网络错误', 'err'); return; }
    if (!r.ok) {
      let msg = '下载失败（' + r.status + '）';
      try { msg = (await r.json()).error || msg; } catch (e) { /* 非 JSON 响应 */ }
      toast(msg, 'err');
      return;
    }
    const blob = await r.blob();
    // 文件名优先取服务端 Content-Disposition（RFC 5987 的 filename* 带中文名），
    // 取不到再退回路径末段 —— 中文名走路径会被 URL 编码，直接当文件名会是一串 %E9…
    const cd = r.headers.get('Content-Disposition') || '';
    const star = /filename\*=UTF-8''([^;]+)/i.exec(cd);
    const plain = /filename="?([^";]+)"?/i.exec(cd);
    let name = path.split('/').pop();
    if (star) { try { name = decodeURIComponent(star[1]); } catch (e) { /* 保持原样 */ } }
    else if (plain) { name = plain[1]; }
    const a = document.createElement('a');
    const objUrl = URL.createObjectURL(blob);
    a.href = objUrl;
    a.download = name;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(objUrl), 10000);
  };

  // delete example / source / template file
  window.delSkillFile = async (slug, path, kind) => {
    const what = kind === 'templatefile' ? '这个模板文件？删掉后该技能就没有可填模板了'
      : kind === 'source' ? '这个原始素材？'
      : '这篇示例范文？';
    if (!confirm('删除' + what)) return;
    const r = await fetch(`/api/admin/skills/${urlSlug(slug)}/file?path=${encodeURIComponent(path)}`, {
      method: 'DELETE', headers: authHdr()
    });
    const j = await r.json();
    if (r.ok) { toast('已删除', 'ok'); loadSkillFiles(curSkillSlug); }
    else toast(j.error || '删除失败', 'err');
  };

  // ---------- 分类结构：新增 / 改名 / 删除（只对手册模式技能） ----------
  //
  // 为什么要有这三块界面：分类是训练期从写作手册抽出来的骨架，手册里没有的类别
  // （本单位常写、但手册没写的那种稿子）以前只能去手改磁盘文件。手改很危险 ——
  // 一个分类名同时散落在分类文件、范文目录、prompt 路由表、reviewer 审稿清单、
  // meta 清单里，漏改一处就是「界面上看着改了、运行时还在按旧名字找类」的静默失效。
  // 所以增删改一律走后端，由后端做整段锚定的级联改写；前端只负责把「改了哪些文件」
  // 摊开给用户看。

  // 变更回执：后端每个写接口都回 { ok, change }，change 里有 files_touched /
  // files_deleted / example_count / warnings。**必须展示**：用户改个名字，
  // 结果 6 个文件被改写，界面上如果只说一句「已保存」，出了问题只能靠猜。
  // warnings 单独用红字：它表示「本该在却没在」（比如路由表里压根没有这个分类的行），
  // 那是需要人知道的数据不一致，不能混在正常回执里被滑过去。
  window.showCategoryChange = (title, ch) => {
    const p = $('kb-panel');
    if (!p || !ch) return;
    const list = (arr) => (arr && arr.length)
      ? arr.map(x => `<div class="dim" style="padding:2px 0">· ${esc(x)}</div>`).join('')
      : '<div class="dim">（无）</div>';
    const warns = (ch.warnings && ch.warnings.length)
      ? `<div class="msg err" style="margin-top:10px">${ch.warnings.map(esc).join('<br>')}</div>` : '';
    const head = esc(ch.name || '') + (ch.old_name ? '（原 ' + esc(ch.old_name) + '）' : '');
    p.classList.remove('hidden'); p.style.display = 'block';
    p.innerHTML = `<div class="kb-panel-head">
        <div>
          <b>${esc(title)}</b>
          <div class="card-sub">${head} · 涉及范文 ${ch.example_count || 0} 篇</div>
          <div style="margin-top:10px"><b style="font-size:13px">已改写</b>${list(ch.files_touched)}</div>
          ${(ch.files_deleted && ch.files_deleted.length) ? `<div style="margin-top:10px"><b style="font-size:13px">已删除</b>${list(ch.files_deleted)}</div>` : ''}
          <div class="card-sub" style="margin-top:10px">范文原文（examples/、source/）不在改写范围内，内容一个字都没动。</div>
          ${warns}
        </div>
        <button class="btn ghost" onclick="closeDetailEditor()">关闭</button>
      </div>`;
  };

  // 变更成功后的统一收尾：先重载左树（分类清单变了），再把回执浮出来。
  // 顺序不能反 —— loadSkillFiles 会重建右栏 DOM，先弹回执会被下一次渲染抹掉。
  async function afterCategoryChange(title, ch) {
    await loadSkillFiles(curSkillSlug);
    window.showCategoryChange(title, ch);
  }

  // 三个动作的公共错误出口。
  // 400 = 用户自己能改的（名字非法、重名、要 force…），直接把后端那句中文原样显示；
  // 5xx = 服务端的事，别让用户以为是自己的输入错了。
  function catErrMsg(r, j) {
    return (j && j.error) || (r.status >= 500 ? '服务端错误（' + r.status + '）' : '操作失败');
  }

  window.newCategoryView = () => {
    destroyCm();
    const ed = $('kb-editor');
    ed.style.display = 'block'; ed.classList.remove('hidden');
    ed.classList.remove('ide-mode'); ed.classList.add('form-mode');
    ed.innerHTML = `
      <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:8px">
        <b>新增分类</b>
        <div><button class="btn ghost" onclick="closeDetailEditor()">取消</button>
        <button class="btn primary" onclick="window.doNewCategory('${encodeURIComponent(curSkillSlug)}')">创建</button></div>
      </div>
      <div class="field"><label>分类名</label><input type="text" id="nc-name" placeholder="例如：会议纪要"></div>
      <div class="field"><label>触发词（选填）</label><input type="text" id="nc-trigger" placeholder="用户说这些话就该走这个类，逗号分隔"></div>
      <div class="field"><label>写作要求（选填）</label><textarea id="nc-req" rows="6" style="width:100%" placeholder="该类稿子的写法要求；留空则稍后手动补 categories/*.md"></textarea></div>
      <p class="dim" style="margin:8px 0 0">只建骨架、不塞范文：建完到左树该分类下用「上传范文」往里加稿子。新分类一开始没有范文，运行时会明确告诉模型「本类暂无范文」，不会拿别的类凑数。</p>`;
  };
  window.doNewCategory = async (slug) => {
    const name = ($('nc-name').value || '').trim();
    if (!name) { toast('分类名不能为空', 'err'); return; }
    const r = await fetch(`/api/admin/skills/${urlSlug(slug)}/categories`, {
      method: 'POST', headers: authHdr(),
      body: JSON.stringify({ name, trigger: ($('nc-trigger').value || '').trim(), requirement: ($('nc-req').value || '').trim() })
    });
    let j = {}; try { j = await r.json(); } catch (e) { /* 非 JSON */ }
    if (r.ok) { toast('分类已创建', 'ok'); afterCategoryChange('分类已创建', j.change); }
    else toast(catErrMsg(r, j), 'err');
  };

  // 改名。file 是分类文件路径（categories/03-x.md），newName 是展示名。
  // 传同名也算合法修改：H1 可能本来就和文件名不一致，用户点「改名」只想把两者对齐。
  window.renameCategoryView = (file, name) => {
    destroyCm();
    const ed = $('kb-editor');
    ed.style.display = 'block'; ed.classList.remove('hidden');
    ed.classList.remove('ide-mode'); ed.classList.add('form-mode');
    ed.innerHTML = `
      <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:8px">
        <b>分类改名</b>
        <div><button class="btn ghost" onclick="closeDetailEditor()">取消</button>
        <button class="btn primary" onclick="window.doRenameCategory('${encodeURIComponent(curSkillSlug)}','${escapeJs(file)}')">确认改名</button></div>
      </div>
      <div class="field"><label>当前分类名</label><input type="text" value="${esc(name)}" readonly></div>
      <div class="field"><label>新分类名</label><input type="text" id="rc-name" value="${esc(name)}"></div>
      <p class="dim" style="margin:8px 0 0">改名会级联改写：分类文件、范文目录、prompt 路由表、reviewer 审稿清单、meta 清单。范文原文（examples/、source/）一个字都不动。改完左侧分类清单会立刻刷新，运行时就按新名字路由。</p>`;
  };
  window.doRenameCategory = async (slug, file) => {
    const newName = ($('rc-name').value || '').trim();
    if (!newName) { toast('新分类名不能为空', 'err'); return; }
    const r = await fetch(`/api/admin/skills/${urlSlug(slug)}/categories/rename`, {
      method: 'POST', headers: authHdr(), body: JSON.stringify({ file, new_name: newName })
    });
    let j = {}; try { j = await r.json(); } catch (e) { /* 非 JSON */ }
    if (r.ok) { toast('已改名', 'ok'); afterCategoryChange('分类已改名', j.change); }
    else toast(catErrMsg(r, j), 'err');
  };

  // 删除分类。两段确认：第一段是常规「你确定吗」，第二段只在**这一类下面还有范文**时出现。
  // 有范文时后端不删、只回 need_force + example_count，这里就拿真实篇数再问一次 ——
  // 那十几篇是手册原文，删掉界面上恢复不了，所以确认框里必须写出「几篇」这个具体数字，
  // 而不是一句含糊的「该分类非空」。空分类不需要第二段：删个空壳还要点两次是折腾人。
  window.delCategory = async (slug, file, name) => {
    if (!confirm('删除分类「' + name + '」？\n会同时删掉它的分类文件与范文目录，界面上无法恢复。')) return;
    const url = (force) => `/api/admin/skills/${urlSlug(slug)}/categories?file=${encodeURIComponent(file)}`
      + (force ? '&force=1' : '');
    let r = await fetch(url(false), { method: 'DELETE', headers: authHdr() });
    let j = {}; try { j = await r.json(); } catch (e) { /* 非 JSON */ }
    // 靠 need_force 这个机器可读标记走第二段，**不去匹配错误文案**：
    // 文案是给人看的、随时会改，一旦匹配不上，有范文的分类就永远删不掉 ——
    // 用户只会看到一句「删除失败」，没有下一步可走。
    if (!r.ok && j && j.need_force) {
      const n = j.example_count || 0;
      if (!confirm('「' + name + '」下面还有 ' + n + ' 篇范文，删除后无法从界面恢复。\n确定连同这 ' + n + ' 篇一起删除？')) return;
    } else if (!r.ok) {
      toast(catErrMsg(r, j), 'err');
      return;
    }
    if (r.ok) { toast('分类已删除', 'ok'); afterCategoryChange('分类已删除', j.change); return; }
    r = await fetch(url(true), { method: 'DELETE', headers: authHdr() });
    try { j = await r.json(); } catch (e) { j = {}; }
    if (r.ok) { toast('分类已删除', 'ok'); afterCategoryChange('分类已删除', j.change); }
    else toast(catErrMsg(r, j), 'err');
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
    // prefill with current skill name/desc from the **admin** list.
    // 别改成公开的 /api/skills：它过滤掉停用技能，于是编辑一个已停用技能的
    // 「名称/分类/描述」时表单是空的，`if (!sk) return;` 静默跳过 —— 用户看到
    // 空白输入框，一保存就把名称/描述清空了（同一个 Bug N 的第二个症状）。
    fetch('/api/admin/skills', { headers: authHdr() }).then(r => r.json()).then(j => {
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

  // ---------- 站点设置 ----------
  // 契约：写接口只认 {name, tagline}；空串 = 恢复默认。前端一律 textContent，绝不 innerHTML。
  const siteEls = () => ({
    name: $('site-name'), tagline: $('site-tagline'),
    pName: $('site-preview-name'), pTag: $('site-preview-tagline'),
    pTitle: $('site-preview-title'), pFoot: $('site-preview-footer')
  });

  // 默认值由后端 GET /api/admin/site 的 defaults 下发，前端**不硬编码**。
  // 硬编码一份就是同一事实写两遍：后端改了 store.DefaultSiteName 而前端没跟，
  // 这里显示的名字就开始骗人。
  let siteDefaults = { name: '', tagline: '' };

  function paintSitePreview(cfg) {
    const e = siteEls();
    if (!e.pName) return;
    const nm = cfg.name || siteDefaults.name || '';
    e.pName.textContent = nm;
    e.pTag.textContent = cfg.tagline || '';
    e.pTag.style.display = cfg.tagline ? '' : 'none';
    const page = document.documentElement.dataset.sitePage || 'admin';
    e.pTitle.textContent = document.title;
    e.pFoot.textContent = page === 'admin' ? nm + ' · 管理端' : nm;
  }

  // 纯函数（不碰 DOM、不发请求），入参就是 GET /api/admin/site 的响应体。
  // 单独抽出来是为了让前端防线能直接从出货文件里抽这个函数来跑。
  //
  // 踩过的坑：is_custom 是 {name:bool, tagline:bool} **对象**，第一版这里直接
  // `j.is_custom ? 自定义 : 默认`，对象恒为真 —— 明明两项都是默认值，界面却一直
  // 显示「当前为自定义名称」。Go 侧测试守不到这条，它只看接口字段对不对。
  function siteStatusText(j) {
    const c = (j && j.is_custom) || {};
    const d = (j && j.defaults) || {};
    const hint = (d.name || d.tagline)
      ? `（默认：${d.name || ''}${d.tagline ? ' / ' + d.tagline : ''}）`
      : '';
    if (!c.name && !c.tagline) return '当前为默认名称';
    if (c.name && c.tagline) return `名称和副标题都自定义了${hint}`;
    return `${c.name ? '名称' : '副标题'}已自定义${hint}`;
  }

  function showSiteMsg(text, cls) {
    const m = $('site-msg');
    m.className = 'msg ' + (cls || '');
    m.textContent = text;
  }

  async function loadSite() {
    if (!$('site-name')) return;
    try {
      const r = await fetch('/api/admin/site', { headers: authHdr() });
      const j = await r.json();
      if (!r.ok) { showSiteMsg(j.error || '读取失败', 'err'); return; }
      siteDefaults = j.defaults || siteDefaults;
      siteEls().name.value = j.name || '';
      siteEls().tagline.value = j.tagline || '';
      paintSitePreview(j);
      showSiteMsg(siteStatusText(j));
    } catch (err) { showSiteMsg('网络错误', 'err'); }
  }

  ['site-name', 'site-tagline'].forEach(id => {
    const el = $(id);
    if (el) el.addEventListener('input', () => paintSitePreview({
      name: $('site-name').value.trim(), tagline: $('site-tagline').value.trim()
    }));
  });

  async function saveSite(payload, okText) {
    showSiteMsg('保存中…');
    try {
      const r = await fetch('/api/admin/site', { method: 'PUT', headers: authHdr(), body: JSON.stringify(payload) });
      const j = await r.json();
      if (!r.ok) { showSiteMsg(j.error || '保存失败', 'err'); return; }
      siteEls().name.value = j.name || '';
      siteEls().tagline.value = j.tagline || '';
      // 立刻用服务端返回值刷新当前页（含 <title>），不等下一次 GET
      if (window.sfSiteApply) window.sfSiteApply({ name: j.name, tagline: j.tagline });
      paintSitePreview(j);
      showSiteMsg(okText || '已保存');
      toast(okText || '已保存', 'ok');
    } catch (err) { showSiteMsg('网络错误', 'err'); }
  }

  const siteForm = $('site-form');
  if (siteForm) {
    siteForm.addEventListener('submit', (e) => {
      e.preventDefault();
      saveSite({ name: $('site-name').value.trim(), tagline: $('site-tagline').value.trim() });
    });
  }
  const siteResetBtn = $('site-reset');
  if (siteResetBtn) {
    siteResetBtn.addEventListener('click', () => {
      $('site-name').value = ''; $('site-tagline').value = '';
      saveSite({ name: '', tagline: '' }, '已恢复默认');
    });
  }

  // ---------- account ----------
  // 管理员账号页：改用户名 / 改密码。
  // 关键点：PUT 成功时**服务端会换发新令牌**（改名后旧令牌的 sub 指向不存在的
  // 账号，会被 Middleware 401）。所以这里必须把新令牌写回 localStorage，
  // 否则用户一改完名字，下一次点任何按钮就被弹回登录页 —— 看着像"改坏了"。
  function showAccMsg(text, cls) {
    const m = $('account-msg');
    if (!m) return;
    m.className = 'msg ' + (cls || '');
    m.textContent = text;
  }

  async function loadAccount() {
    if (!$('account-user')) return;
    try {
      const r = await fetch('/api/admin/account', { headers: authHdr() });
      const j = await r.json();
      if (!r.ok) { showAccMsg(j.error || '读取失败', 'err'); return; }
      $('account-user').value = j.username || '';
      showAccMsg('');
    } catch (err) { showAccMsg('网络错误', 'err'); }
  }

  const accountForm = $('account-form');
  if (accountForm) {
    accountForm.addEventListener('submit', async (e) => {
      e.preventDefault();
      const cur = $('account-current').value;
      const npw = $('account-new').value;
      const cfm = $('account-confirm').value;
      if (!cur) { showAccMsg('请先填写当前密码', 'err'); return; }
      if (npw && npw.length < 8) { showAccMsg('新密码至少 8 位', 'err'); return; }
      if (npw && cfm !== npw) { showAccMsg('两次输入的新密码不一致', 'err'); return; }
      showAccMsg('保存中…');
      try {
        const r = await fetch('/api/admin/account', {
          method: 'PUT', headers: authHdr(),
          body: JSON.stringify({
            current_password: cur,
            new_username: $('account-user').value.trim(),
            new_password: npw,
            confirm_password: cfm
          })
        });
        const j = await r.json();
        if (!r.ok) { showAccMsg(j.error || '保存失败', 'err'); return; }
        if (j.token) localStorage.setItem(TOKEN_KEY, j.token);
        $('account-user').value = j.username || '';
        $('account-current').value = '';
        $('account-new').value = '';
        $('account-confirm').value = '';
        const what = [j.username_changed ? '用户名' : '', j.password_changed ? '密码' : ''].filter(Boolean).join('和');
        showAccMsg('已保存（' + what + '已更新）', '');
        toast('账号已更新', 'ok');
      } catch (err) { showAccMsg('网络错误', 'err'); }
    });
  }

  // ---------- MCP 服务（管理员统一配置 + 开关）----------
  // 三条要点：
  //  1. 编辑态密钥框留空 = 沿用原值（后端 isMaskedKey 认空串，库里取原值回填）。
  //  2. 开关走 toggle 接口即时落库并重连，不用再点保存。
  //  3. 测试连接不落库——先探通路再保存，避免存一堆连不上的配置。
  function showMCPMsg(text, cls) {
    const m = $('mcp-msg');
    if (!m) return;
    m.className = 'msg ' + (cls || '');
    m.textContent = text;
  }
  function showMCPListMsg(text, cls) {
    const m = $('mcp-list-msg');
    if (!m) return;
    m.className = 'msg ' + (cls || '');
    m.textContent = text;
  }

  function mcpStateText(s) {
    if (!s.enabled) return '未启用';
    const st = s.status || {};
    if (st.ok) return '已连接 ' + (st.server || '') + (st.version ? ' v' + st.version : '') + ' · ' + st.tool_count + ' 个工具';
    if (st.error) return '连接失败：' + st.error;
    return '尚未连接（点右侧「测试」）';
  }

  async function loadMCP() {
    if (!$('mcp-list')) return;
    try {
      const r = await fetch('/api/admin/mcp', { headers: authHdr() });
      if (r.status === 401) { logout(); return; }
      const j = await r.json();
      const list = j.servers || [];
      const box = $('mcp-list');
      if (!list.length) {
        box.innerHTML = '<div class="empty" style="padding:30px 0"><h3>还没有接入 MCP 服务</h3>' +
          '<p class="dim">接入后，智能助手就能直接查数据库、调内部接口，而不是瞎猜</p></div>';
        return;
      }
      box.innerHTML = list.map(s => {
        const st = s.status || {};
        const tools = (st.tools || []).map(n => '<code style="font-size:11px">' + esc(n) + '</code>').join(' ');
        return `
        <div class="prov-row">
          <div class="inf" style="min-width:0">
            <div class="nm">${esc(s.name)} ${s.enabled ? '<span class="pill-active">已启用</span>' : ''}</div>
            <div class="dt">${esc(s.id)} · ${esc(s.url)} · 超时 ${s.timeout_sec}s · 密钥 ${s.has_key ? esc(s.key_mask) : '未设置'}</div>
            <div class="dt" style="${st.ok ? '' : 'color:#b45309'}">${esc(mcpStateText(s))}</div>
            ${tools ? '<div class="dt" style="margin-top:4px;line-height:1.7">' + tools + '</div>' : ''}
          </div>
          <div class="row-actions" style="display:flex;align-items:center;gap:8px">
            <label style="display:flex;align-items:center;gap:4px;font-size:12px;white-space:nowrap;margin:0">
              <input type="checkbox" style="width:auto;margin:0" ${s.enabled ? 'checked' : ''}
                onchange="window.mcpToggle('${esc(s.id)}', this.checked)">启用
            </label>
            <button class="btn ghost" style="padding:4px 8px;font-size:12px" onclick="window.mcpEdit('${esc(s.id)}')">编辑</button>
            <button class="btn ghost" style="padding:4px 8px;font-size:12px" onclick="window.mcpProbe('${esc(s.id)}')">测试</button>
            <button class="icon-btn danger" title="删除" onclick="window.mcpDel('${esc(s.id)}')">✕</button>
          </div>
        </div>`;
      }).join('');
    } catch (err) { showMCPListMsg('加载失败', 'err'); }
  }

  window.mcpToggle = async (id, enabled) => {
    showMCPListMsg('正在' + (enabled ? '启用' : '停用') + '并重连…');
    try {
      const r = await fetch('/api/admin/mcp/toggle', {
        method: 'POST', headers: authHdr(), body: JSON.stringify({ id, enabled }),
      });
      const j = await r.json();
      if (!r.ok) { showMCPListMsg(j.error || '操作失败', 'err'); loadMCP(); return; }
      showMCPListMsg(enabled ? '已启用，工具已挂上' : '已停用，工具已摘除', 'ok');
      // 重连是后台异步做的，稍等一下再拉状态，否则看到的还是上一轮结果。
      setTimeout(loadMCP, 900);
    } catch (err) { showMCPListMsg('网络错误', 'err'); }
  };

  window.mcpEdit = async (id) => {
    const r = await fetch('/api/admin/mcp', { headers: authHdr() });
    const j = await r.json();
    const s = (j.servers || []).find(x => x.id === id);
    if (!s) return;
    $('mcp-id').value = s.id;
    $('mcp-id').readOnly = true; // 标识是工具名前缀，改了等于换一套工具名，不允许中途改
    $('mcp-name').value = s.name || '';
    $('mcp-url').value = s.url || '';
    $('mcp-key').value = ''; // 留空 = 沿用原密钥
    $('mcp-key').placeholder = s.has_key ? '留空 = 沿用原密钥（' + s.key_mask + '）' : '没有鉴权就留空';
    $('mcp-timeout').value = s.timeout_sec || 30;
    $('mcp-enabled').checked = !!s.enabled;
    $('mcp-cancel').style.display = 'inline-flex';
    $('mcp-test-out').innerHTML = '';
    window.scrollTo({ top: 0, behavior: 'smooth' });
  };

  function resetMCPForm() {
    $('mcp-form').reset();
    $('mcp-id').readOnly = false;
    $('mcp-timeout').value = '30';
    $('mcp-key').placeholder = '没有鉴权就留空';
    $('mcp-cancel').style.display = 'none';
    $('mcp-test-out').innerHTML = '';
  }
  window.mcpCancel = resetMCPForm;
  $('mcp-cancel') && $('mcp-cancel').addEventListener('click', resetMCPForm);

  // 表单构造集中在这一处：保存与测试连接共用，避免两边字段不一致
  // （少了字段 → 后端 DisallowUnknownFields 回 400「请求体无效」，报错还不提字段名）。
  function mcpFormPayload() {
    const mcpPayload = {
      id: $('mcp-id').value.trim(),
      name: $('mcp-name').value.trim(),
      url: $('mcp-url').value.trim(),
      api_key: $('mcp-key').value.trim(),
      enabled: $('mcp-enabled').checked,
      timeout_sec: parseInt($('mcp-timeout').value || '30', 10) || 30,
    };
    return mcpPayload;
  }

  window.mcpProbe = async (id) => {
    // 从列表点「测试」：把该行配置填进表单再探，复用同一条链路
    if (id) await window.mcpEdit(id);
    showMCPMsg('正在连接…');
    $('mcp-test-out').innerHTML = '';
    try {
      const r = await fetch('/api/admin/mcp/test', {
        method: 'POST', headers: authHdr(), body: JSON.stringify(mcpFormPayload()),
      });
      const j = await r.json();
      if (!r.ok) { showMCPMsg(j.error || '探测失败', 'err'); return; }
      if (!j.ok) {
        showMCPMsg('连接失败', 'err');
        $('mcp-test-out').innerHTML = '<div class="msg err" style="display:block">' + esc(j.error || '未知错误') + '</div>';
        return;
      }
      showMCPMsg('连接成功', 'ok');
      const names = (j.remote_tools || []).map(n => '<code style="font-size:11px">' + esc(n) + '</code>').join(' ');
      $('mcp-test-out').innerHTML =
        '<div class="msg ok" style="display:block">服务器：' + esc(j.server || '') +
        (j.version ? ' v' + esc(j.version) : '') + ' · 工具 ' + (j.tool_count || 0) + ' 个</div>' +
        '<div class="dt" style="margin-top:6px;line-height:1.8">' + names + '</div>';
    } catch (err) { showMCPMsg('网络错误', 'err'); }
  };

  $('mcp-test') && $('mcp-test').addEventListener('click', () => window.mcpProbe(null));

  $('mcp-reload') && $('mcp-reload').addEventListener('click', async () => {
    showMCPListMsg('正在重新连接…');
    try {
      const r = await fetch('/api/admin/mcp/refresh', { method: 'POST', headers: authHdr() });
      const j = await r.json();
      if (!r.ok) { showMCPListMsg(j.error || '重连失败', 'err'); return; }
      showMCPListMsg('已重连', 'ok');
      loadMCP();
    } catch (err) { showMCPListMsg('网络错误', 'err'); }
  });

  window.mcpDel = async (id) => {
    if (!confirm('删除这个 MCP 服务？其工具会立刻从对话中摘除。')) return;
    const r = await fetch('/api/admin/mcp/' + encodeURIComponent(id), { method: 'DELETE', headers: authHdr() });
    const j = await r.json();
    if (r.ok) { toast('已删除', 'ok'); loadMCP(); } else toast(j.error || '删除失败', 'err');
  };

  const mcpForm = $('mcp-form');
  if (mcpForm) {
    mcpForm.addEventListener('submit', async (e) => {
      e.preventDefault();
      showMCPMsg('保存并重连中…');
      try {
        const r = await fetch('/api/admin/mcp', {
          method: 'POST', headers: authHdr(), body: JSON.stringify(mcpFormPayload()),
        });
        const j = await r.json();
        if (!r.ok) { showMCPMsg(j.error || '保存失败', 'err'); return; }
        showMCPMsg('已保存，正在后台重连…', 'ok');
        resetMCPForm();
        loadMCP();
      } catch (err) { showMCPMsg('网络错误', 'err'); }
    });
  }

  function logout() {
    localStorage.removeItem(TOKEN_KEY);
    $('shell').style.display = 'none';
    $('login-view').style.display = 'block';
  }

  // auto enter if token exists
  if (token()) enter();
  else { $('login-view').style.display = 'block'; }
})();