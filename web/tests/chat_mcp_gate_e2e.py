#!/usr/bin/env python3
# LIVE-LEGS: mcp_gate TIMEOUT_S=1500
# ↑ 这里**故意只声明正跑那一条**。负向 leg（INJECT_MCP_OFF=1）挂不上名册：
#   scripts/acceptance-live.sh:143 要求 `--- a/b ok ---` 里 a==b 才算 PASS，而负向 leg 的
#   **意图就是有一条红**，挂进去必然常年 FAIL，然后被人 ALLOW_SKIP=1 糊掉。
#   它的正确位置是「本地闸门」——scripts/preflight.sh 里同 MCP 自证那一段（能起活服务、
#   有 playwright 的地方）。**退出码语义（注意别再抄错）**：负向 leg 在「注入后 B1 精确转红、
#   且红名单全在预期级联内」时**打 `✓ 负向自证成立` 并 rc=0**；只有「B1 没红 / 红了无关断言 /
#   出现游离红」才 rc=1。所以 preflight 的 selfcheck 认 rc=0 是对的 —— 它同时还要带上
#   REQUIRE_LIVE=1，否则下面的环境性 SKIP 会被 selfcheck 读成 ✓（见 REQUIRE_LIVE 处注释）。
#   不挂 CI 是因为 CI 没有活服务与模型。防它腐烂的机制是名册元守卫
#   （web/tests/live_e2e_roster.test.mjs 要求本文件有合法 LIVE-LEGS 声明）+ preflight 那行真调它。
#   改本文件时别把这一行补回去。
"""MCP 用户级门控的线上真浏览器终验：「默认不调度、勾了才调度、勾了必须真能用」。

为什么必须是真浏览器 + 真模型，而不是单测：
  这条门控最典型的失败形态就是**单测全绿**那种。后端 9 个门控单测量的是
  `FilterMCP(reg, allow)` 的入参出参；前端 `chat_mcp.test.mjs` 量的是 `mcpPayloadIds`
  这个纯函数。两边都绿，中间那条**真的把勾选集送上网络**的链路
  （chat.js → POST /api/chat 的 `mcp` 字段 → 后端闸门 → 工具表裁剪）谁都没量。
  用户报的正是这一层：「勾选以后…刚刚测了下似乎没能正常使用」。
  所以这里读的是**浏览器真实发出的请求体**（不是我们自己算出来的变量），
  SSE 是从**真实响应流**里分流出来的（clone 后另起 reader，不干扰真流式）。

三条命根子：
  A. 没勾 → 请求体 `mcp` 必须是 `[]`，整轮 SSE 里 0 个 `mcp_` 工具。
     方向必须是「宁可少给」：多给 = 内网业务系统被无意识地挂上去，是安全问题。
  B. 勾了 → 请求体必须真带那台的 id，且 SSE 里真出现 `mcp_<alias>_<真工具名>`。
  C. 勾了之后模型**真调出可核对的数据**（正文命中直连 list_apis 的真实接口名/路径 ≥2），
     不能只看「工具名出现过」就宣布「能调度」。

真值从哪来（**不经 skillforge 自己的 Go 客户端**，否则是拿自己的输出证自己）：
  · 直连 MCP 端点 `tools/list`      → 真工具名集合（用来核对 B2 的名字是真的）
  · 直连 MCP 端点 `tools/call list_apis` → 真接口名/路径集合（用来核对 B3 的数据是真的）
  · 兜底：读 DataToolbox 的 SQLite `apis` 表
凭据（url/api_key）只从 /opt/skillforge/data/skillforge.db 只读取、只在内存里转手，
**一律不打印**（只打印主机名与长度）——这个脚本的输出会被贴进验收记录。

负向自证（INJECT_MCP_OFF=1）：网络层把 chat.js 里 `chatPayload(text, currentMCPIds())`
改成 `chatPayload(text, [])`，精确复刻用户报的那个 bug（界面勾了、网线没带）。
此时 **B1 必须精确转红**，且 A1/A2 必须还绿；若 B1 没红 → 说明这把尺子量不到真故障，
脚本自己判红（否则就是尺子假绿，比不挂尺子更坏）。

用法：
    BASE=http://127.0.0.1:8092 python3 web/tests/chat_mcp_gate_e2e.py        # 正跑（期望全绿）
    BASE=http://127.0.0.1:8092 INJECT_MCP_OFF=1 python3 web/tests/chat_mcp_gate_e2e.py
                                                                           # 负向自证（期望 rc=0
                                                                           #  + `✓ 负向自证成立`）
    # 本地闸门（preflight）必须再加 REQUIRE_LIVE=1：环境不足时 SKIP→FAIL，
    # 免得「服务没起 / playwright 没装」被 selfcheck 的 rc=0 读成 ✓。
"""
import json
import os
import re
import sqlite3
import sys
import time
import urllib.request

BASE = os.environ.get('BASE', 'https://skillforge.open-claw.click')
MAXW = int(os.environ.get('MAXW', '420'))
TIMEOUT_S = int(os.environ.get('TIMEOUT_S', '1500'))   # 名册里的 leg 超时（两条 leg 共用）
SF_DB = os.environ.get('SF_DB', '/opt/skillforge/data/skillforge.db')
DTD_DB = os.environ.get('DTD_DB', '/opt/datatoolbox/data/data-store.db')
DUMP_TIMELINE = os.environ.get('DUMP_TIMELINE') == '1'
# 网络层注入：让「勾了也没带上」这个故障成真（见文件头负向自证）
INJECT_MCP_OFF = os.environ.get('INJECT_MCP_OFF') == '1'
# REQUIRE_LIVE=1：把「真·环境性跳过」升级成 FAIL（rc=1）。
# 动机（2026-09-19 实测踩到）：本脚本在 playwright 缺失 / 页面打不开 / 库里没启用 MCP 这些
# **环境不足**的情况下按设计打 `SKIP` 并 rc=0 —— 名册 runner 会把 `^SKIP` 判成「未验证=FAIL」，
# 那个语义是对的；但 **preflight 的 selfcheck 只认 rc**，于是同一份 SKIP 在本地闸门里会被
# 印成 ✓。后果：本机服务没起、或 playwright 没装时，这把尺子「看着挂着、其实一次都没验」，
# 正是我们最怕的假绿。所以本地闸门调用时必须带 REQUIRE_LIVE=1：跑不成=红，要么去把环境弄好，
# 要么承认这条腿没验。名册 / CI 不设这个变量，语义不变。
REQUIRE_LIVE = os.environ.get('REQUIRE_LIVE') == '1'

# 提示词：必须**逼出工具调用**，否则这条 leg 是空跑（没工具调用时 B2 恒红/恒绿，两种都不算证据）。
# 这条预设放文件里而不是 runner 的命令行里：中文长提示词塞进 leg 的 env 会被空格切碎，
# 而且提示词本身是最该被审查的东西，不该藏进 scripts/（名册元守卫也这么要求）。
# 要求「名称与工具返回原文一致」是为了让 B3 的核对有靶子：模型自己改写过的名字是核对不上的。
PROMPT_PRESETS = {
    'mcp_list': (
        '数据工具箱里现在配置了哪些数据接口？请必须调用工具去查真实清单，'
        '然后逐条列出接口名称、请求方法和路径。'
        '名称和路径必须与工具返回的原文一致，不要改写，不要编造。'
    ),
}
PROMPT_KEY = os.environ.get('PROMPT_KEY', 'mcp_list')
if PROMPT_KEY not in PROMPT_PRESETS:
    print(f'未知 PROMPT_KEY={PROMPT_KEY}，可选：{sorted(PROMPT_PRESETS)}')
    sys.exit(2)
PROMPT = os.environ.get('PROMPT', PROMPT_PRESETS[PROMPT_KEY])

INJ = {'hits': 0, 'landed': False}
fails = []
checks = 0


def env_skip(msg):
    """真·环境性跳过（playwright 缺失 / 页面打不开 / 库里没启用 MCP / 真值源取不到）。

    默认：打 `SKIP` 行 + rc=0 —— 名册 runner（scripts/acceptance-live.sh:128）据此判「未验证」，
    SKIP≠PASS。REQUIRE_LIVE=1：改成打 FAIL 行 + rc=1（同 SystemExit 退出，免得调用方
    忘了传返回值、把红当绿继续往下印小结）。"""
    if REQUIRE_LIVE:
        print(f'FAIL 环境不足以真验（REQUIRE_LIVE=1，否则这个 SKIP 会被 preflight 的 rc 判成 ✓）：{msg}')
        sys.exit(1)
    print(f'SKIP {msg}')
    return 0


def check(name, ok, extra=''):
    global checks
    checks += 1
    if ok:
        print(f'ok   {name}')
    else:
        print(f'FAIL {name} {extra}')
        fails.append(name)
    return bool(ok)


# skip() 于 2026-09-19 移除：它的唯一调用者是 A2 的前提判定，而那条路在健康系统上
# 高频触发 SKIP，会被名册 runner 直接判 FAIL（scripts/acceptance-live.sh:127 把 `^SKIP`
# 当未验证），最终只会被 ALLOW_SKIP=1 糊过去 → 尺子腐烂。详见 A2 处的改判说明。
# 真·环境性跳过（playwright 缺失 / 页面打不开）仍打 `SKIP` 行并 rc=0 —— 那是 runner
# 想要的语义（跑不了 = 这条腿失败），与「前提式 SKIP」是两件事。


def report():
    # ★ 小结行必须原样打 `--- N/M ok ---`：runner 拿它抠断言数（scripts/acceptance-live.sh:123），
    #   格式一动这条腿「绿了也会被判红」。SKIP 之类的说明**另起一行**，不许并进来
    #   （web/tests/live_e2e_roster.test.mjs 有专门的元守卫盯住这件事，2026-09-19 实测撞到过）。
    print(f'--- {checks - len(fails)}/{checks} ok ---')
    if fails:
        print('FAILED: ' + '; '.join(fails))
        return 1
    return 0


# ===== 真值源：直连 MCP 端点，不经本仓库的 Go 客户端 =====

def sanitize_ident(s):
    """移植 internal/tools/mcp_tool.go 的 sanitizeIdent（非 ASCII 一律变下划线，首尾下划线裁掉）。"""
    out = ''.join(ch if (ch.isascii() and (ch.isalnum() or ch in '_-')) else '_' for ch in s)
    out = out.strip('_')
    return out or 'srv'


def mcp_alias(srv_id, srv_name):
    """移植 mcpAlias：ASCII 化的 id → ASCII 化的 name（中文名会退化成空，那时才用哈希）。"""
    a = sanitize_ident(srv_id)
    if a != 'srv':
        return a
    a = sanitize_ident(srv_name)
    if a != 'srv':
        return a
    import hashlib
    return 'srv' + hashlib.sha1(f'{srv_id}|{srv_name}'.encode()).hexdigest()[:6]


def rpc(url, key, method, params, rid=1, timeout=30):
    body = json.dumps({'jsonrpc': '2.0', 'id': rid, 'method': method, 'params': params}).encode()
    req = urllib.request.Request(url, data=body, method='POST')
    req.add_header('Content-Type', 'application/json')
    req.add_header('Accept', 'application/json, text/event-stream')
    if key:
        # 头值只在内存里拼，不打印（脱敏过滤器会把打印出来的引号头吃掉，反而不发不发）
        req.add_header('Authorization', 'Bearer ' + key)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        raw = r.read().decode('utf-8', 'replace')
    # 兼容 SSE 包装的 JSON-RPC 响应
    if raw.lstrip().startswith('data:') or 'event:' in raw[:200]:
        for ln in raw.splitlines():
            ln = ln.strip()
            if ln.startswith('data:'):
                try:
                    return json.loads(ln[5:].strip())
                except Exception:
                    continue
    return json.loads(raw)


def db_servers():
    """只读取出启用的 MCP 服务器配置。返回 [{id,name,url,key}]；凭据不落日志。"""
    try:
        con = sqlite3.connect(f'file:{SF_DB}?mode=ro', uri=True)
        rows = con.execute(
            'SELECT id, name, url, api_key FROM mcp_servers WHERE enabled=1').fetchall()
        con.close()
    except Exception as e:
        print(f'⚠ 读不到 {SF_DB} 里的 mcp_servers：{str(e)[:90]}')
        return []
    return [{'id': r[0], 'name': r[1], 'url': r[2], 'key': r[3]} for r in rows]


def truth_for(srv):
    """直连那台 MCP：真工具名集合（已带 mcp_<alias>_ 前缀）+ 真接口名/路径集合。"""
    alias = mcp_alias(srv['id'], srv['name'])
    prefix = 'mcp_' + alias + '_'
    try:
        host = srv['url'].split('//')[-1].split('/')[0]
        print(f"[真值] 直连 {host}（key len={len(srv['key'] or '')}）")
    except Exception:
        pass
    remote_tools = []
    try:
        res = rpc(srv['url'], srv['key'], 'tools/list', {}, 3)
        remote_tools = [t.get('name', '') for t in (res.get('result', {}).get('tools') or [])]
    except Exception as e:
        print(f'⚠ tools/list 直连失败：{str(e)[:90]}')
    tool_names = {prefix + sanitize_ident(t) for t in remote_tools if t}
    apis = set()
    try:
        res = rpc(srv['url'], srv['key'], 'tools/call',
                  {'name': 'list_apis', 'arguments': {}}, 5)
        txt = ''.join(c.get('text', '') for c in
                      (res.get('result', {}).get('content') or []))
        data = json.loads(txt)
        for a in (data.get('apis') or []):
            if a.get('name'):
                apis.add(str(a['name']).strip())
            if a.get('path'):
                apis.add(str(a['path']).strip())
    except Exception as e:
        print(f'⚠ list_apis 直连失败（改用本地 DB 兜底）：{str(e)[:90]}')
    if not apis:
        try:
            con = sqlite3.connect(f'file:{DTD_DB}?mode=ro', uri=True)
            for nm, pth in con.execute('SELECT name, path FROM apis'):
                if nm:
                    apis.add(str(nm).strip())
                if pth:
                    apis.add(str(pth).strip())
            con.close()
        except Exception as e:
            print(f'⚠ 本地 DB 兜底也失败：{str(e)[:90]}')
    print(f'[真值] 远端工具 {len(remote_tools)} 个（本地名前缀 {prefix}）/ 真接口标识 {len(apis)} 条')
    return prefix, tool_names, apis


# ===== 浏览器侧：抓真实请求体 + 从真响应流里分流 SSE =====

# 包一层 fetch：只做「记录」，不动流本身（resp.clone() 另起 reader），
# 所以浏览器拿到的仍是真流式 —— 这个脚本量的就是用户那条路径。
INIT_JS = """() => {
  window.__mcpnet = {reqs: [], errs: []};
  window.addEventListener('error', e => window.__mcpnet.errs.push(String(e.message).slice(0, 120)));
  const of = window.fetch;
  window.fetch = function(input, init) {
    const url = (typeof input === 'string') ? input : ((input && input.url) || '');
    const pr = of.apply(this, arguments);
    // 只认聊天那一跳（/api/chat 或 /api/chat?x=1），别把 /api/chat/history 也算进来
    if (/\\/api\\/chat(\\?|$)/.test(url)) {
      const rec = {url: url, body: (init && init.body) ? String(init.body) : '', sse: '', chunks: 0};
      const idx = window.__mcpnet.reqs.push(rec) - 1;
      pr.then(function(resp) {
        try {
          const rd = resp.clone().body.getReader();
          const dec = new TextDecoder();
          const pump = function() {
            return rd.read().then(function(r) {
              if (r.done) return;
              window.__mcpnet.reqs[idx].sse += dec.decode(r.value, {stream: true});
              window.__mcpnet.reqs[idx].chunks++;
              return pump();
            });
          };
          return pump();
        } catch (e) { window.__mcpnet.errs.push(String(e).slice(0, 120)); }
      }).catch(function(e) { window.__mcpnet.errs.push(String(e).slice(0, 120)); });
    }
    return pr;
  };
  return true;
}"""

READ_JS = """() => {
  const bubbles = Array.from(document.querySelectorAll('.ch-msg.assistant .ch-bubble'));
  const box = document.querySelector('#mcp-box');
  const rows = Array.from(document.querySelectorAll('#mcp-list .ch-skrow'));
  const row = rows.length ? rows[0] : null;
  return {
    bubbleCount: bubbles.length,
    lastBubble: bubbles.length ? (bubbles[bubbles.length - 1].innerText || '') : '',
    active: !!document.querySelector('.ctk-step.active'),
    sendIdle: !document.querySelector('#chat-send').disabled,
    mcpVisible: !!(box && !box.hidden),
    mcpLabel: (document.querySelector('#mcp-label') || {}).innerText || '',
    mcpFoot: (document.querySelector('#mcp-foot') || {}).innerText || '',
    mcpRows: rows.length,
    mcpRowOn: row ? row.getAttribute('aria-pressed') : null,
    mcpLayerOpen: !!document.querySelector('#mcp-layer') && !document.querySelector('#mcp-layer').hidden,
    pick: (localStorage.getItem('skillforge.mcp') || ''),
    errs: (window.__mcpnet || {}).errs || [],
  };
}"""


def wait_turn(pg, maxw=None):
    """等到这一轮真的结束（发送按钮回可用 + 无进行中步骤）或超时。"""
    deadline = time.time() + (maxw or MAXW)
    last, stable, last_len = {}, 0, -1
    while time.time() < deadline:
        time.sleep(1.0)
        last = pg.evaluate(READ_JS)
        if last['sendIdle'] and not last['active'] and last['bubbleCount'] > 0 \
                and len(last['lastBubble']) == last_len:
            stable += 1
            if stable >= 2:
                return last, True
        else:
            stable = 0
        last_len = len(last['lastBubble'])
    return last, False


def wire(pg, skip):
    """取本轮新增的 /api/chat 记录（skip = 之前已消费的条数）。"""
    recs = pg.evaluate("""() => (window.__mcpnet.reqs || []).map(r => ({url: r.url, body: r.body, sse: r.sse, chunks: r.chunks}))""")
    return recs[skip:], len(recs)


def tool_calls(sse_text):
    """从真 SSE 里解析出工具调用名。

    锚定解析而不是裸子串 grep：工具名只可能出现在 trace 步骤的 label
    （后端 internal/api/agent_loop.go 造的 `工具 N · <localName>`）里，
    所以按这个格式整段匹配 —— `grep mcp_` 那种写法连一句解释性正文都会命中，
    0 命中/恒命中都会误判（这类假断言在本仓库踩过）。
    """
    names, labels, events = set(), [], 0
    for ln in sse_text.splitlines():
        ln = ln.strip()
        if not ln.startswith('data:'):
            continue
        try:
            ev = json.loads(ln[5:].strip())
        except Exception:
            continue
        events += 1
        stack = [ev]
        while stack:
            cur = stack.pop()
            if isinstance(cur, dict):
                sts = cur.get('steps')
                if isinstance(sts, list):
                    stack.extend(sts)
                elif 'label' in cur:
                    lbl = str(cur.get('label') or '')
                    labels.append(lbl)
                    m = re.match(r'^工具\s*\d+\s*·\s*([A-Za-z0-9_.:-]+)$', lbl.strip())
                    if m:
                        names.add(m.group(1))
            elif isinstance(cur, list):
                stack.extend(cur)
    return names, labels, events


def body_mcp(body):
    """从真实请求体里取 mcp 字段。取不到返回 None（= 字段压根没发，与「发了个空数组」不同）。"""
    try:
        d = json.loads(body)
    except Exception:
        return None
    return d.get('mcp')


def run_checks(pg, prefix, tool_names, truth_apis):
    # ---- A 轮：不勾 ----
    # 清掉上一轮可能残留的勾选，并刷新页面让 UI 从干净状态渲染（A 轮的前提）。
    # reload 会让执行上下文销毁，playwright 这时可能抛异常 —— 那是预期内的，吞掉即可；
    # 真正的前提由下面 wait_for_selector + mcpVisible 来断。
    try:
        pg.evaluate("() => { localStorage.removeItem('skillforge.mcp'); location.reload(); }")
    except Exception:
        pass
    try:
        pg.wait_for_selector('.ch-scroll', timeout=15000)
        pg.wait_for_selector('textarea', timeout=15000)
    except Exception as e:
        return env_skip(f'刷新后页面没起来（{BASE}）：{str(e)[:100]}')
    pg.evaluate(INIT_JS)
    time.sleep(1.0)
    st = pg.evaluate(READ_JS)
    if not st['mcpVisible']:
        # 前提式 SKIP 已废弃（铁律，2026-09-19）：库里明明有 enabled=1 的服务器（main 里刚断过），
        # 界面却没渲染出这个按钮 —— 要么是公开接口没把它透出来、要么是前端渲染挂了，两种都该有人去看。
        # 打成 SKIP 只会被 ALLOW_SKIP=1 糊掉，尺子就烂了。所以这里判红，并把两种可能都写在证据里。
        check('A0 前提：聊天界面真渲染出 MCP 数据源按钮（库里 enabled=1 才该有）', False,
              f"mcpVisible=False；按钮文案「{st['mcpLabel']}」/ 可选项 {st['mcpRows']} 台 —— "
              f'要么公开状态接口没透出、要么前端没渲染，去看一眼再决定是环境还是 bug')
        return report()
    print(f"[A 轮] 不勾：按钮文案「{st['mcpLabel']}」/ 可选项 {st['mcpRows']} 台 / pick={st['pick']!r}")

    consumed = 0
    pg.fill('textarea', PROMPT)
    pg.click('#chat-send')
    a_end, a_done = wait_turn(pg)
    a_recs, consumed = wire(pg, consumed)
    a_txt = a_end['lastBubble']
    print(f"[A 轮] 结束={a_done} 正文 {len(a_txt)} 字 / SSE 记录 {len(a_recs)} 条"
          f" / chunks={[r['chunks'] for r in a_recs]}")

    # 前提先断：没有真请求体、没有真回答时，下面的断言全是空跑。
    ok_a_wire = check('A0 前提：真抓到 /api/chat 请求（否则 A1 是空跑）',
                      len(a_recs) == 1, f'{len(a_recs)} 条')
    if ok_a_wire:
        check('A1 未勾选：请求体 mcp 字段是空数组（默认不调度 MCP）',
              body_mcp(a_recs[0]['body']) == [],
              f"实发 mcp={body_mcp(a_recs[0]['body'])!r} / body 前 120 字 {a_recs[0]['body'][:120]}")
    else:
        check('A1 未勾选：请求体 mcp 字段是空数组（默认不调度 MCP）', False, '没抓到请求')
    a_names, a_labels, a_events = tool_calls(a_recs[0]['sse'] if a_recs else '')
    # ★ 按标记认人：判的是「有没有 mcp_ 前缀的工具」，不是「工具全集是否为空」。
    #   内置工具（http_request / run_python）与门控无关，拿全集当判据必假红
    #   （2026-09-19 实测踩到：未勾那轮调了 http_request → A2 假红）。
    a_mcp = sorted(n for n in a_names if n.startswith('mcp_'))
    a_builtin = sorted(n for n in a_names if not n.startswith('mcp_'))
    print(f"[A 轮] SSE 事件 {a_events} 条 / 工具步骤 {[l for l in a_labels if l.startswith('工具')][:3]}"
          f" / 内置工具 {a_builtin}")
    # ★ 2026-09-19 改判：这一条**不再走前提式 SKIP**，改成无条件的不变式 + 空样本自曝。
    #   原因不是「放宽」，是被名册的两条硬约束逼出来的正确形状：
    #     a. scripts/acceptance-live.sh:127 用 `grep -qE '^SKIP'` 判「未验证」→ 打 SKIP 行
    #        这条腿在名册里直接算 FAIL（SKIP≠PASS，这是对的）；
    #     b. 而同一条腿在**健康系统**上跑，模型有相当概率压根不调工具 → 前提式 SKIP 会
    #        在健康系统上高频触发 → 这条腿常年飘红 → 很快就被 ALLOW_SKIP=1 糊过去 → 尺子死掉。
    #   空样本下这条不变式**依然是真判断，不是空跑绿**：它判的是「整轮里有没有 mcp_ 调用」，
    #   而门控泄漏的必要条件是「MCP 工具真的被交给模型并被调用」；一整轮一个工具都没调，
    #   等于没有任何东西被交出去，泄漏通道根本不成立。
    #   确定性证据不靠这条：未勾选那轮的请求体必须是 mcp=[]（A1，纯网络层，与模型行为无关），
    #   加上后端 FilterMCP 的 9 条单测。A2/A4 是**旁证**，样本为空时由下面这行 INFO 自曝。
    if not a_names:
        print('INFO A 轮模型没调任何工具：A2/A4 本次样本为空（不变式仍成立，'
              '确定性证据由 A1 请求体 mcp=[] + 后端 FilterMCP 单测承担）')
    check('A2 未勾选：整轮 SSE 里 0 个 mcp_ 工具调用（多给 = 内网系统被无声挂上）',
          not a_mcp, f'出现了 {a_mcp}（内置 {a_builtin} 不算 MCP）')
    check('A3 前提：未勾那轮真跑完（正文 ≥60 字 + 真有 trace 事件），否则 A2/A4 是空跑',
          a_done and len(a_txt) >= 60 and a_events > 0,
          f"done={a_done} 正文 {len(a_txt)} 字 事件 {a_events} 条")
    a_hits = sorted(x for x in truth_apis if x and x in a_txt)
    check('A4 未勾选：正文没有泄漏内网真接口名/路径（命中 <2 条）', len(a_hits) < 2,
          f'命中 {len(a_hits)} 条：{a_hits[:4]}')

    # ---- B 轮：勾上 ----
    pg.click('#mcp-btn')
    time.sleep(0.6)
    st = pg.evaluate(READ_JS)
    check('B0a 勾选交互：点按钮弹出 MCP 层', st['mcpLayerOpen'], f"layerOpen={st['mcpLayerOpen']}")
    pg.click('#mcp-list .ch-skrow')
    time.sleep(0.6)
    st = pg.evaluate(READ_JS)
    pick = []
    try:
        pick = json.loads(st['pick'] or '[]')
    except Exception:
        pass
    check('B0b 勾选交互：按钮文案带台数 + 行 aria-pressed=true + 底部变成「已选」+ 写进了取回集',
          ('· 1' in st['mcpLabel']) and st['mcpRowOn'] == 'true'
          and '已选' in st['mcpFoot'] and len(pick) == 1,
          f"label={st['mcpLabel']!r} aria={st['mcpRowOn']!r} foot={st['mcpFoot']!r} pick={pick}")

    pg.fill('textarea', PROMPT)
    pg.click('#chat-send')
    b_end, b_done = wait_turn(pg)
    b_recs, consumed = wire(pg, consumed)
    b_txt = b_end['lastBubble']
    print(f"[B 轮] 结束={b_done} 正文 {len(b_txt)} 字 / SSE 记录 {len(b_recs)} 条"
          f" / chunks={[r['chunks'] for r in b_recs]}")

    ok_b_wire = check('B0c 前提：真抓到第 2 轮 /api/chat 请求（否则 B1 是空跑）',
                      len(b_recs) == 1, f'{len(b_recs)} 条')
    if ok_b_wire:
        bmcp = body_mcp(b_recs[0]['body'])
        check('B1 已勾选：请求体 mcp 真带上了那台的 id（勾了不生效 = 用户报的那个 bug）',
              isinstance(bmcp, list) and len(bmcp) == 1 and bmcp[0] == pick[0] if pick else False,
              f"实发 mcp={bmcp!r} / 界面取回集={pick}")
    else:
        check('B1 已勾选：请求体 mcp 真带上了那台的 id（勾了不生效 = 用户报的那个 bug）',
              False, '没抓到请求')
    b_names, b_labels, b_events = tool_calls(b_recs[0]['sse'] if b_recs else '')
    # 同样按标记认人：只看 mcp_ 工具与「直连 tools/list 真名集」的包含关系，
    # 内置工具（run_python / http_request）不参与判定（它们与门控无关）。
    b_mcp = {n for n in b_names if n.startswith('mcp_')}
    b_builtin = sorted(n for n in b_names if not n.startswith('mcp_'))
    print(f"[B 轮] SSE 事件 {b_events} 条 / 工具步骤 "
          f"{[l for l in b_labels if l.startswith('工具')][:4]} / 内置工具 {b_builtin}")
    check('B2 已勾选：SSE 里真出现 mcp_ 工具调用，且工具名 ∈ 直连 tools/list 的真名集',
          bool(b_mcp) and b_mcp <= tool_names,
          f'调用 {sorted(b_mcp)} / 真名集 {len(tool_names)} 个'
          + (f'；不在真名集里的：{sorted(b_mcp - tool_names)}' if b_mcp - tool_names else ''))
    check('B3 前提：已勾那轮真跑完（正文 ≥60 字），否则 B3 是空跑',
          b_done and len(b_txt) >= 60, f'done={b_done} 正文 {len(b_txt)} 字')
    b_hits = sorted(x for x in truth_apis if x and x in b_txt)
    check('B4 已勾选：正文命中直连 list_apis 的真接口名/路径 ≥2 条（真调到数据，不只是名字出现过）',
          len(b_hits) >= 2, f'命中 {len(b_hits)} 条：{b_hits[:4]}')

    errs = list(a_end.get('errs') or []) + list(b_end.get('errs') or [])
    check('B5 全程无 JS 异常', not errs, str(errs[:2]))
    return None


def main():
    try:
        from playwright.sync_api import sync_playwright
    except Exception as e:  # pragma: no cover
        # 真·环境性跳过（本机没装 playwright）。REQUIRE_LIVE=1 时转红：本地闸门里
        # 「跑不起来」必须等于红，不能等于 ✓。
        return env_skip(f'playwright 不可用：{e}')

    servers = db_servers()
    if not servers:
        # 前提式 SKIP 已废弃（铁律，2026-09-19）：没有启用中的 MCP = 这条腿没验。
        # 打成 SKIP 会被 runner/selfcheck 一路糊成绿，所以显式判红并指名道姓。
        check('A0 前提：库里至少有一台 enabled=1 的 MCP 服务器（管理员真开过门控）', False,
              f'{SF_DB} 里没有启用中的 MCP 服务器 —— 门控无从验起')
        return report()
    prefix, tool_names, truth_apis = truth_for(servers[0])
    print(f'[配置] leg 超时 {TIMEOUT_S}s / 单轮窗口 {MAXW}s / 注入={INJECT_MCP_OFF}')
    # 前提：正跑 leg 的 B2/B4 都要拿「直连真值」当靶子。真值源连不上（真名集空、真接口识别为空）
    # 时还往下跑，会把「靶子没了」判成「门控坏了」—— 那是假红。负向 leg 只判 B1（不依赖真值），照跑。
    if not INJECT_MCP_OFF and (not tool_names or not truth_apis):
        check('B2/B4 前提：直连 MCP 端点真取到靶子（真工具名 + 真接口标识）', False,
              f'工具 {len(tool_names)} 个 / 真接口标识 {len(truth_apis)} 条'
              f'（直连 {servers[0]["url"]} 失败？没有靶子就判不了 B2/B4）')
        return report()

    with sync_playwright() as p:
        br = p.chromium.launch()
        try:
            pg = br.new_page(viewport={'width': 1280, 'height': 900})
            if INJECT_MCP_OFF:
                def _rewrite(route):
                    try:
                        r = route.fetch()
                        body = r.text()
                        # 精确到「这一处调用」：命中数必须恰好 1。
                        # 命中 0 处时若一声不响放行，「注入死了」就会被读成「负向自证通过」。
                        new_body, hits = MCP_RE.subn('chatPayload(text, [])', body)
                        INJ['hits'] = hits
                        INJ['landed'] = hits == 1 and 'currentMCPIds' in body
                        print(f"[inject] chatPayload(text, currentMCPIds()) 命中 {hits} 处（要求恰好 1 处）")
                        route.fulfill(status=r.status, body=new_body,
                                      headers={k: v for k, v in r.headers.items()
                                               if k.lower() not in ('content-length', 'content-encoding')})
                    except Exception as e:
                        print(f'[inject] 改写失败：{str(e)[:100]}')
                        route.continue_()
                pg.route('**/chat.js*', _rewrite)
            try:
                pg.goto(BASE, wait_until='domcontentloaded', timeout=15000)
            except Exception as e:
                # 真·环境性跳过（服务没起 / 地址不通）。REQUIRE_LIVE=1 时转红。
                return env_skip(f'打不开 {BASE}（服务没起？）：{str(e)[:120]}')
            pg.wait_for_selector('.ch-scroll', timeout=10000)
            if INJECT_MCP_OFF:
                info = pg.evaluate("""() => fetch('/assets/js/chat.js').then(r => r.text()).then(t => {
                    let parses = true;
                    try { new Function(t); } catch (e) { parses = false; }
                    return {injected: /chatPayload\\(text, \\[\\]\\)/.test(t), parses: parses, len: t.length};
                })""")
                check('N1 注入前提：浏览器真收到改写后的 chat.js（命中 1 处 + 语法合法）',
                      INJ['landed'] and info['injected'] and info['parses'],
                      f"hits={INJ['hits']} injected={info['injected']} parses={info['parses']}")
            rc = run_checks(pg, prefix, tool_names, truth_apis)
            if rc is not None:
                return rc
            rc = report()
            if INJECT_MCP_OFF:
                # 负向自证：不是「红了就算过」，必须**红在该红的那条**上。
                # 反例（把 B1 写成恒真、或注入没落到位）会让这条判红 —— 尺子必须能抓到假绿。
                got_red = [f for f in fails if f.startswith('B1 ')]
                side_red = [f for f in fails if f.startswith('A1 ') or f.startswith('A2 ')]
                # 收严：注入「勾了也不带上」只应级联到 B1/B2/B4（都依赖 MCP 真被调度）；
                # 红名单里出现别的断言 = 注入打到了不相干的地方，证据是脏的，不能算成立。
                allowed = ('B1 ', 'B2 ', 'B4 ')
                stray = [f for f in fails if not f.startswith(allowed)]
                if not got_red:
                    print('✗ 负向自证失败：注入「勾了也不带上」之后 B1 竟然还是绿的 —— '
                          '这把尺子量不到真故障（假绿）')
                    return 1
                if side_red:
                    print(f'✗ 负向自证失败：注入把无关断言也弄红了 {side_red}，证据是脏的')
                    return 1
                if stray:
                    print(f'✗ 负向自证失败：红名单里出现预期之外的断言 {stray}（只允许 {list(allowed)} 级联），'
                          f'注入打到不相干的地方了')
                    return 1
                print(f'✓ 负向自证成立：注入后 B1 精确转红（红名单 {fails}，全在预期级联内），'
                      f'A1/A2 仍绿')
                return 0
            return rc
        finally:
            br.close()


MCP_RE = re.compile(r'chatPayload\(\s*text\s*,\s*currentMCPIds\(\)\s*\)')

if __name__ == '__main__':
    sys.exit(main())
