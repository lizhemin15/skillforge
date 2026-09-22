#!/usr/bin/env python3
"""真浏览器终验：聊天流式过程中的**中间材料**是否真在滚（线上/本地同一套断言）。

为什么要真浏览器：
  node 测试（chat_trace.test.mjs）是**读源码 + 手搓 DOM 字符串**的断言。它能检查
  「renderTrace 里写了 s.material 分支」，却检查不出运行时**有没有真的收到材料、
  材料是不是在滚动、有没有挂错步骤**。用户原话：「现在速度过于慢了，中间可以流式
  输出思考的一些中间材料，现在一直卡着计时，用户体验不佳」——只有跳秒不算进度。
  所以必须发一条真消息，在真流式过程中采样真 DOM。

判据（每条打 ok/FAIL，任何 FAIL 都算红、退出码 1；不是崩溃红）：
    M1 材料出现在**首段正文之前**（用户等的这段时间屏幕上有真内容，而不是跳秒）
    M2 材料**在滚**：≥3 个不同的尾部快照（一次性贴一块不算滚）
    M3 材料被截尾（≤220 字），不会撑破面板
    M4 材料只挂**进行中**那一步：`.ctk-step.done .ctk-mat` 全程为 0
    M5 前提：整轮真收到 ≥200 字正文（否则「没材料」可能只是这轮太短，M1 是空跑）
    M6 真流式过程中页面无 JS 异常（历史坑：RAF 丢接收者）
    M7 最长静默 ≤8s：**屏幕连续多久没有任何东西在动**（材料/正文/步骤任一在动即算不静默）。
       M1~M4 只看「整轮曾经有没有材料」，放得过「材料两秒滚完、之后四十秒全静止」这种形态
       —— 而那正是用户投诉「一直卡着计时」的那一段，所以必须单列这条闸。

负向自证（INJECT_NOMAT=1）：
  用 page.route 在**网络层**把 chat.js 里材料渲染分支改掉（`(s.material` → `(false && s.material`），
  浏览器拿到的就是一份「不渲染材料」的真代码 → M1 必须精确转红、退出码 1。
  在测试里手改 DOM 造红是假的；这里改的是浏览器实际执行的脚本。

用法：
  python3 web/tests/chat_material_e2e.py                          # 打 127.0.0.1:8092
  BASE=http://127.0.0.1:9999 python3 web/tests/chat_material_e2e.py
  INJECT_NOMAT=1 python3 web/tests/chat_material_e2e.py           # 负向自证，必须红

REQUIRE_FILE=1：docgen（生成 .docx）那条路径专用。
  该路径的交付物是**文件卡片**，正文不进聊天气泡，所以不能用「气泡 ≥200 字」当前提。
  这条更不能改用「材料 ≥200 字」当前提 —— 那等于拿 M1 自己给自己当前提（循环论证）。
  独立前提只有文件卡片本身：出现「已生成文档」的 atx-link。

SKIP 规则：playwright 不可用 / 页面打不开 → 打 SKIP 并 exit 0。SKIP != PASS。

为什么没有第二把「几何尺子」（2026-09-23 实测后故意不加）：
  曾另写过一个 chat_material_visible_e2e.py，专门量材料区的渲染几何：可见多行高度、
  内部滚动贴底。线上真值把这个念头否掉了 —— 写作链路「等待期」实测只有 **4.8s**
  （模型 5 秒内就开写正文，20s 时正文已 2080 字），材料峰值 310 字、材料区
  scrollHeight == clientHeight == 74px，**从不溢出**。于是「溢出时贴底」永远空跑
  （gap=0 恒真），「增长 ≥200 字」在 4.8s 窗口里根本达不到 —— 那不是产品红，是尺子
  门槛错。空跑的断言不是防线、是噪音（用户已明确反感「计时器空转」式的假动作）。
  「不可见」这条主路径本文件已覆盖：材料块 display:none 时 innerText 为空 → mats 为空
  → M1 转红（不是靠 textContent 骗自己）。真痛的「卡着计时」现场不在写作链路，
  在训练链路（一轮 860.5s / 20~27 分钟）—— 尺子该往那边搬。
"""
# LIVE-LEGS: writing | docgen REQUIRE_FILE=1 PROMPT_KEY=docgen
# ↑ 线上验收 leg 声明。scripts/acceptance-live.sh 只认这一行来枚举要跑几条 leg
#   （不许在 runner 里写死文件名 —— 那是「漏加 = 这个 leg 不存在」的老洞）；
#   web/tests/live_e2e_roster.test.mjs 守着它跟文件真身不许脱钩。
import os
import re
import sys
import time

BASE = os.environ.get('BASE', 'http://127.0.0.1:8092')
MAXW = int(os.environ.get('MAXW', '240'))  # 单轮最长等多久（秒）
# M7 的门槛：屏幕最长允许几秒不变。默认 8s —— 依据是线上正常流的最长帧间隔实测
# ~1.2s（关思考链首片 0.6s），8s 已是六倍余量，而用户投诉的那段是 39.8s。
# 0 = 关掉这条断言（只在明确要跑「不设阈值的观察」时用）。
GAP_MAX_MS = int(os.environ.get('GAP_MAX_MS', '8000'))
# 负向自证要打的那**一个**条件：`(s.material ? '<span class="ctk-mat">…`。
# 只认这个形态（后面紧跟材料 span），不去碰 `esc(s.material)` 这类无关出现。
NOMAT_RE = re.compile(r'''\(s\.material(?=\s*\?\s*'<span class="ctk-mat")''')
INJ = {'hits': 0, 'landed': False}  # 注入落地状态，供 N1 断言自证前提
# DUMP_TIMELINE=1 打「哪一段没有任何东西在动」的时间线。
#
# 为什么要有这个开关：用户的投诉原话是「现在速度过于慢了，中间可以流式输出思考的一些
# 中间材料，现在一直卡着计时」。要回答「慢在哪」，唯一可信的来源是**真浏览器里的帧时序**
# ——服务端日志只能证明它发出去了，证明不了用户屏幕上有没有东西在动。
# 这条时间线就是把人眼看到的「卡住」量成秒：每个采样点记 [时刻] 进行中的步骤 / 材料字数 /
# 正文字数；只在「步骤变了 / 材料长了 / 正文长了」时打点，所以两行之间的时间差就是
# **屏幕上什么都没变的那段静默**，长间隔一眼看得出来。
DUMP_TIMELINE = os.environ.get('DUMP_TIMELINE', '') == '1'


def _material_log_cap():
    """从 Go 源码读 materialLogCap —— 判据必须跟着实现走，不写死。

    为什么：M3 原本写死 `<= 220`，那是更早一版材料面板的上限；后端换成一整块可滚动的
    材料日志（窗口取尾部 materialLogCap 字）之后，实测峰值就到 1086~1198 了，而判据没跟着
    改 —— 结果是**尺子过期**：产品明明是对的，writing 腿却恒红，还顺手把真故障的信号埋了。
    教训：凡是「实现里有个常量、尺子里抄了个数字」的判据，一律改成读源码，脱钩一次就够。
    下面找不到常量时**不静默兜底**：打一行 STALE 让它显眼（rc 由断言决定，不由这行决定）。
    """
    root = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
    src = os.path.join(root, 'internal', 'api', 'chat_trace.go')
    try:
        with open(src, encoding='utf-8') as f:
            m = re.search(r'materialLogCap\s*=\s*(\d+)', f.read())
        if m:
            return int(m.group(1))
    except OSError:
        pass
    print('[STALE] 读不到 internal/api/chat_trace.go 的 materialLogCap，退回避风港 1200；'
          '这不是通过，是判据来源丢了，请修尺子')
    return 1200


# 材料面板上限（DOM 里统计的字数）。留 64 字余量给后端为「首字对齐句读」加的前缀省略号
# 与尾部旁白行 —— 那些不计入窗口本身，但会出现在 DOM 文本里。
MAT_CAP = _material_log_cap() + 64


def print_timeline(samples, limit_gap_ms=1500):
    """把采样打成时间线；只打变化点，间隔 >limit_gap_ms 时标注静默时长。"""
    if not samples:
        print('时间线：（没有采样）')
        return
    print('---- 时间线（只打变化点；两行之间 = 屏幕上什么都没变）----')
    prev = None
    prev_t = 0
    for s in samples:
        t, mlen, tail, blen, active, _done, label = (list(s) + ['', ''])[:7]
        # 去重键必须带**材料内容**（tail），不能只看字数：材料是截尾的滚动窗口
        # （M3 要求 ≤220 字），长度早就顶到上限了，新内容进来时长度一点不变——
        # 按长度判「有没有变化」会把一段真在滚的材料误报成 40 秒静默（实测踩过）。
        key = (label, mlen, tail, blen, active)
        if key == prev:
            continue
        gap = t - prev_t
        mark = ''
        if prev is not None and gap > limit_gap_ms:
            mark = f'   ← 前面 {gap / 1000:.1f}s 无任何变化'
        print(f'[{t / 1000:6.1f}s] 步骤={label or "-":<10} 材料={mlen:>5}字 正文={blen:>5}字 '
              f'进行中={int(bool(active))}{mark}')
        prev, prev_t = key, t
    t, mlen, _tail, blen, active, _done, label = (list(samples[-1]) + ['', ''])[:7]
    print(f'[{(samples[-1][0]) / 1000:6.1f}s] （末次采样）材料={mlen}字 正文={blen}字')


def silent_gaps(samples):
    """按「内容有没有变」切出静默段：返回 [(起点ms, 终点ms, 时长ms, 步骤标签)]。

    什么算「有东西在动」：进行中步骤变了、材料内容变了（长度或尾部原文任一）、
    正文长度变了。**材料尾部原文必须算**——理由见 print_timeline 的注释。
    """
    out = []
    if not samples:
        return out
    prev_key, prev_t, prev_label, prev_active = None, None, '', False
    for s in samples:
        t, mlen, tail, blen, active, _done, label = (list(s) + ['', ''])[:7]
        key = (label, mlen, tail, blen, active)
        if key != prev_key:
            # 只统计「进行中」时的静默：整轮答完之后的静止不是用户在等，
            # 把它算进来会让采样尾巴拖多长就报多长（假红）。
            if prev_active and prev_t is not None and t - prev_t > 0:
                out.append((prev_t, t, t - prev_t, prev_label))
            prev_key, prev_t, prev_label, prev_active = key, t, label, bool(active)
    # 收尾：末尾这段「一直冻结到采样结束」的静止必须报出来。
    # 漏掉它是**最坏形态**：流真的挂住时画面冻死后再无任何变化，循环里永远等不到
    # 「下一次变化」来结算这段静默 —— M7 会假绿（离线自证 case ② 实测抓到过）。
    if prev_active and prev_t is not None and samples[-1][0] - prev_t > 0:
        out.append((prev_t, samples[-1][0], samples[-1][0] - prev_t, prev_label))
    return out

# 提示词预设：**必须放在文件里**，不能让 runner 用命令行传。
# 原因：runner 是 bash，leg 声明按空格切；把中文长提示词塞进 leg 的 env 赋值里，
# 既会被空格切碎，也会让「提示词」这条最该被审查的东西藏进 scripts/ 里没人看。
# 每条 leg 只带「无空格的键=值」。
PROMPT_PRESETS = {
    # 写作路径：材料来自「执笔跳」——材料正是在这段静默里流出来的。
    # 太短的问答会走关思考链的路由跳，材料本来就不该出现，断言会变成空跑。
    #
    # 2026-09-17 修：这条跟 docgen 是同一天同一个坑，但当时只补了 docgen 那一半 ——
    # 默认跑出来的材料末帧是「用户提及标题和字数，但缺少[必填]参数『背景与核心素材』，
    # 需追问以便生成具体内容」，正文只 61 字 → M5 前提 FAIL、5/6、MAT_RC=1。
    # **模型是按契约追问，不是功能坏了**；但这条 leg 的默认跑因此永远拿不到正文，
    # M1~M4 变成空跑（前提不成立还判绿是最危险的形态，M5 正是为拦这个而存在）。
    # 尺子的前提必须自己给全，不能指望模型替我们把素材编出来。
    # （实测：补上「背景与核心素材」后同一路径 6/6 全绿、正文 1276 字。）
    'writing': (
        '写一份关于开展数据治理专项工作的通知。'
        '背景与核心素材：本单位自 2026 年起推进数据治理，已建成数据中台、'
        '完成 12 类主数据梳理，现需规范数据质量与安全责任。'
        '正文不少于 600 字，直接输出正文'
    ),
    # docgen 路径：交付物是 .docx 文件卡片（正文不进气泡），且这一跳关掉思考链，
    # 材料只能来自「流式 JSON 里的正文」。走这条路要用 REQUIRE_FILE=1 换前提。
    #
    # 2026-09-17 修：原来这条预设只给了一个标题，**没给必填参数**。于是模型很合理地
    # 回一句「缺少必填参数『背景与核心素材』，需追问」——它是按契约做事，可这条 leg
    # 却因此永远拿不到文件卡片，M1~M6 全成空跑（前提不成立还判绿最危险）。
    # 尺子的前提必须自己给全：这类路径要么真跑通、要么明说前提没造出来，
    # 不能把「模型在按契约追问」当成「功能坏了」，也不能当成「测过了」。
    # （实测：补上「背景与核心素材」后同一路径 6/6 全绿、正文 1276 字。）
    'docgen': (
        '帮我生成一份《关于开展数据治理专项行动的通知》的 Word 文档。'
        '背景与核心素材：本单位自 2026 年起推进数据治理，已建成数据中台、'
        '完成 12 类主数据梳理，现需规范数据质量与安全责任。直接生成文件'
    ),
}
PROMPT_KEY = os.environ.get('PROMPT_KEY', 'writing')
if PROMPT_KEY not in PROMPT_PRESETS:
    print(f'未知 PROMPT_KEY={PROMPT_KEY}，可选：{sorted(PROMPT_PRESETS)}')
    sys.exit(2)
PROMPT = os.environ.get('PROMPT', PROMPT_PRESETS[PROMPT_KEY])
# INJECT_NOMAT=1：网络层把材料渲染分支改掉，复刻「后端有材料、前端不显示」这个故障。
INJECT_NOMAT = os.environ.get('INJECT_NOMAT') == '1'
# docgen 路径：前提改成「出现已生成文档卡片」（见文件头说明，不能用材料当前提）
REQUIRE_FILE = os.environ.get('REQUIRE_FILE') == '1'

fails = []
checks = 0


def check(name, ok, extra=''):
    global checks
    checks += 1
    if ok:
        print(f'ok   {name}')
    else:
        print(f'FAIL {name} {extra}')
        fails.append(name)
    return bool(ok)


def report():
    print(f'--- {checks - len(fails)}/{checks} ok ---')
    if fails:
        print('FAILED: ' + '; '.join(fails))
        return 1
    return 0


# 在页面里起一个采样器：每 200ms 记录「材料尾部 / 材料长度 / 正文长度 / 有无 active 步」。
# 采样必须在页面内部跑（Python 侧轮询会漏帧，200ms 的滚动窗口只有真 setInterval 抓得住）。
SAMPLER_JS = """() => {
  window.__mt = {samples: [], t0: Date.now(), errs: [], maxLiving: 0, badNums: {}, numSeen: 0};
  window.addEventListener('error', e => window.__mt.errs.push(String(e.message).slice(0,120)));
  clearInterval(window.__iv);
  window.__iv = setInterval(() => {
    const act = document.querySelector('.ctk-step.active');
    const m = act ? act.querySelector('.ctk-mat') : null;
    const b = document.querySelector('.ch-msg.assistant .ch-bubble');
    const txt = m ? (m.innerText || '') : '';
    // 同时「进行中」的格子数：正常恒 ≤1。两个一起转 = 板上出现了同名重复步骤
    // （Carry 接管了 t≈0 那格之后又追加了一格同名，被接管那格永远转不完）。
    const living = document.querySelectorAll('.ctk-step.active').length;
    if (living > window.__mt.maxLiving) window.__mt.maxLiving = living;
    const nseen = document.querySelectorAll('.ctk-num').length;
    if (nseen > window.__mt.numSeen) window.__mt.numSeen = nseen;
    // 编号栏只许是数字：前端拿不到 phase 的映射时会把 phase **原样**印上去
    // （后端写 phase="plan" 的那版线上就显示英文单词 plan、角色徽标空白）。
    document.querySelectorAll('.ctk-num').forEach(function (e) {
      const v = (e.innerText || '').trim();
      if (v && !/^[0-9]+$/.test(v)) window.__mt.badNums[v] = (window.__mt.badNums[v] || 0) + 1;
    });
    window.__mt.samples.push([
      Date.now() - window.__mt.t0,
      txt.length,
      txt.slice(-40),
      b ? (b.innerText || '').replace(/\\s+/g, '').length : 0,
      !!act,
      document.querySelectorAll('.ctk-step.done .ctk-mat').length,
      act ? ((act.querySelector('.ctk-label') || {}).innerText || '').trim() : '',
    ]);
  }, 200);
  return true;
}"""

READ_JS = """() => {
  const s = window.__mt || {samples: [], errs: []};
  const b = document.querySelector('.ch-msg.assistant .ch-bubble');
  return {samples: s.samples, errs: s.errs, maxLiving: s.maxLiving || 0, badNums: s.badNums || {},
          numSeen: s.numSeen || 0,
          bubble: b ? (b.innerText || '').replace(/\\s+/g, '').length : 0,
          genfile: Array.from(document.querySelectorAll('a.atx-link .atx-hint'))
                     .some(e => (e.innerText || '').includes('已生成文档')),
          active: !!document.querySelector('.ctk-step.active'),
          sendIdle: !document.querySelector('#chat-send').disabled,
          steps: document.querySelectorAll('.ctk-step').length};
}"""


def main():
    try:
        from playwright.sync_api import sync_playwright
    except Exception as e:  # pragma: no cover
        print(f'SKIP playwright 不可用：{e}')
        return 0

    with sync_playwright() as p:
        br = p.chromium.launch()
        try:
            pg = br.new_page(viewport={'width': 1280, 'height': 900})

            # ---- 负向自证：在网络层改掉浏览器实际执行的 chat.js ----
            if INJECT_NOMAT:
                def _rewrite(route):
                    try:
                        r = route.fetch()
                        body = r.text()
                        # 只打「材料渲染」那一个条件：`(s.material ? '<span class="ctk-mat">…`
                        # ⚠ 老的 body.replace('(s.material', …) 会连 `esc(s.material)` 一起改（两处命中），
                        #   那是把注入撒到无关代码上；更要命的是命中 0 处时它**一声不响**就放行，
                        #   于是「注入死了」会被当成「负向自证通过」。所以这里：精确匹配 + 命中数不对就标红。
                        new_body, hits = NOMAT_RE.subn('(false', body)
                        INJ['hits'] = hits
                        INJ['landed'] = hits == 1 and 'ctk-mat' in body
                        print(f'[inject] 材料渲染分支命中 {hits} 处（要求恰好 1 处），已改成恒不渲染')
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
                print(f'SKIP 打不开 {BASE}（服务没起？）：{str(e)[:120]}')
                return 0
            pg.wait_for_selector('.ch-scroll', timeout=10000)
            pg.wait_for_selector('textarea', timeout=10000)
            return run_checks(pg)
        finally:
            br.close()


def run_checks(pg):
    if INJECT_NOMAT:
        # 注入前提自证 —— 先断「这把刀真砍到了」，再看血。
        # ① 请求的路线要真被改写（命中数恰好 1）；② 改写后的脚本要**语法合法**：
        #    改坏成语法错只会让整页 JS 死掉，那不是「材料不渲染」而是「页面崩了」，
        #    两种形态混在一起，红得再漂亮也证明不了 M1/M7 抓的是材料缺失（实测踩过：
        #    一次注入让整轮 150s 连正文都没出现，M5 前提跟着红，证据就脏了）。
        # ③ 还要看浏览器**实际拿到**的那份（不是我们自己手里的变量）。
        info = pg.evaluate("""() => fetch('/assets/js/chat.js').then(r => r.text()).then(t => {
            let parses = true;
            try { new Function(t); } catch (e) { parses = false; }
            return {injected: /\\(false\\s*\\?\\s*'<span class="ctk-mat"/.test(t),
                    parses: parses, len: t.length};
        })""")
        check('N1 注入前提：浏览器真收到改写后的 chat.js（命中 1 处 + 语法合法）',
              INJ['landed'] and info['injected'] and info['parses'],
              f"hits={INJ['hits']} injected={info['injected']} parses={info['parses']} len={info['len']}")

    pg.evaluate(SAMPLER_JS)
    pg.fill('textarea', PROMPT)
    pg.click('#chat-send')

    # 轮询到**整轮真的结束**为止，或超时。
    #
    # 为什么不能只看「没有 active 步 + 正文连续几次采样不涨」（2026-09-17 线上实测）：
    # writing 腿就是这么假红的 —— 两个阶段之间的空档里 active=0、正文还停在 94 字
    # （正文刚开流），循环当场收工、采样停掉，接着「M5 前提：整轮真收到 ≥200 字正文」
    # 不成立 → 判红。而材料末帧尾部已经是「特此通知。XX公司数据治理办公室2026年X月」，
    # 说明正文马上就写完了：**是尺子提前收工，不是产品没产出**。
    # 真终态信号只有一个：发送按钮重新可用（chat.js 在 turn 结束时 send.disabled=false）。
    deadline = time.time() + MAXW
    stable = 0
    last_bubble = 0
    last = {}
    turn_ended = False
    while time.time() < deadline:
        time.sleep(1.0)
        last = pg.evaluate(READ_JS)
        if last.get('sendIdle') and last['bubble'] > 0 and last['bubble'] == last_bubble \
                and not last['active']:
            stable += 1
            if stable >= 2:
                turn_ended = True
                break
        else:
            stable = 0
        last_bubble = last['bubble']
    if not turn_ended:
        print(f'⚠ 采样窗口用尽（MAXW={MAXW}s）而整轮尚未结束 —— 下面的 M5 前提可能因此不成立，'
              f'那不是产品红，是窗口给短了')
    pg.evaluate('() => clearInterval(window.__iv)')
    last = pg.evaluate(READ_JS)

    samples = last['samples']
    print(f"采样 {len(samples)} 帧 / steps={last['steps']} / 正文 {last['bubble']} 字 / errs={last['errs'][:2]}")
    if DUMP_TIMELINE:
        print_timeline(samples)
    mats = [s for s in samples if s[1] > 0]
    if mats:
        print('材料首帧: ' + str(mats[0]))
        print('材料末帧: ' + str(mats[-1]))
        tails = []
        for s in mats:
            if not tails or tails[-1] != s[2]:
                tails.append(s[2])
        print(f'材料窗口 {len(mats)} 帧 / 不同尾部快照 {len(tails)} 个 / 峰值 {max(s[1] for s in mats)} 字')
    else:
        print('材料窗口: 整轮没有任何材料帧' + ('（INJECT_NOMAT=1，这是预期）' if INJECT_NOMAT else ''))

    # M5 是前提，先断它：没有真交付物的话「没材料」不能算通过，但也不能算材料失败。
    if REQUIRE_FILE:
        ok_body = bool(last.get('genfile'))
        check('M5 前提：出现「已生成文档」卡片（该路径交付物是文件，否则 M1 是空跑）', ok_body,
              f"genfile={last.get('genfile')}")
    else:
        ok_body = last['bubble'] >= 200
        # 前提不成立时**点名根因**：该腿的提示词末尾写着「直接输出正文」，若屏幕上却只有
        # 文件卡片 + 一条「已为您生成《…docx》」的短回执，那不是「材料尺子坏了」，而是
        # 交付形态被翻了 —— 2026-09-23 实锤：同一份输入 8 跑里 4 跑命中 docgen 型技能，
        # intent=write 也被直落发文分支（正文 39 字）。修法见 internal/agent/agent.go 的
        # TextOnlyDropsDocGenSkill + api/chat.go 的 2a-pre0 闸门；Go 侧守卫在
        # internal/agent/textonly_route_test.go。这里把它翻译成一句能搜索的故障名，
        # 免得下一个人对着一行「bubble=39」猜半天。
        hint = ''
        if not ok_body and last.get('genfile'):
            hint = ('  ← 交付形态被翻转：提示词明说「直接输出正文」，却走了文档生成分支'
                    '（见 agent.TextOnlyDropsDocGenSkill / chat.go 2a-pre0）')
        check('M5 前提：整轮真收到 ≥200 字正文（否则 M1 是空跑）', ok_body,
              f"bubble={last['bubble']}{hint}")

    if mats:
        first = mats[0]
        check('M1 材料出现在首段正文之前（等的时候屏幕上有真内容，不是跳秒）',
              first[3] == 0, f"材料首帧时正文已 {first[3]} 字")
        tails = []
        for s in mats:
            if not tails or tails[-1] != s[2]:
                tails.append(s[2])
        check('M2 材料在滚（≥3 个不同尾部快照，不是一次性贴一块）', len(tails) >= 3,
              f'不同快照 {len(tails)} 个')
        check('M3 材料被截尾 ≤%d 字（不撑破面板）' % MAT_CAP, max(s[1] for s in mats) <= MAT_CAP,
              f"峰值 {max(s[1] for s in mats)} 字 / 判据上限 {MAT_CAP}"
              f"（取自 internal/api/chat_trace.go 的 materialLogCap）")
        check('M4 材料只挂进行中那一步（`.ctk-step.done .ctk-mat` 全程为 0）',
              max(s[5] for s in samples) == 0, f"done 步里出现过 {max(s[5] for s in samples)} 个材料块")
    else:
        # 没材料：M1/M2/M3/M4 全红。这是 INJECT_NOMAT=1 时要看到的结果。
        check('M1 材料出现在首段正文之前（等的时候屏幕上有真内容，不是跳秒）', False,
              '整轮没有材料帧')
        check('M2 材料在滚（≥3 个不同尾部快照，不是一次性贴一块）', False, '整轮没有材料帧')
        check('M3 材料被截尾 ≤%d 字（不撑破面板）' % MAT_CAP, False, '整轮没有材料帧')
        check('M4 材料只挂进行中那一步（`.ctk-step.done .ctk-mat` 全程为 0）',
              max((s[5] for s in samples), default=0) == 0, '')

    check('M6 真流式过程中页面无 JS 异常', not last['errs'], str(last['errs'][:2]))

    # M8/M9 是**步骤板本身**的两条渲染契约（2026-09-19 加）。为什么必须挂到线上真页面：
    # 步骤板是这一轮里用户唯一能看到的进展来源，而后端那两条缺陷在**单测里都是绿的**
    # 才上的线（进程内真路由测出来才发现）：板上出现两个 ①、其中一个永远转不完
    # （「一直卡着计时」的另一半就是这个），以及 phase 写了前端认不得的值 ——
    # chat.js:1371 的 `(agent.n || s.phase)` 会把 phase 原样印在编号栏上，
    # 那格里显示一个英文单词，角色徽标空白。所以这两条只在**真浏览器渲染出的 DOM** 上量。
    # 先断前提，再断不变量：整轮一个 active 格都没出现过 / 一个编号栏都没渲染出来时，
    # 下面的「≤1」「没有英文字面量」是**空跑绿**（尺子没量到东西，不许读成通过）。
    check('M8a 前提：整轮真出现过「进行中」的格子（否则 M8 是空跑）',
          last.get('maxLiving', 0) >= 1, f"峰值 {last.get('maxLiving')} 格 active")
    check('M9a 前提：步骤板真渲染出编号栏（否则 M9 是空跑）',
          last.get('numSeen', 0) >= 1, f"编号栏元素峰值 {last.get('numSeen')} 个")
    check('M8 步骤板同时「进行中」的格子数 ≤1（两个一起转 = 板上出现了同名重复步骤）',
          last.get('maxLiving', 0) <= 1, f"峰值 {last.get('maxLiving')} 格同时 active")
    check('M9 步骤板编号栏全是数字（出现英文字面量 = 后端 phase 前端认不得）',
          not last.get('badNums'), f"非数字编号栏：{last.get('badNums')}")

    # M7 是「用户到底卡了多久」的硬闸。M1~M4 只要整轮**曾经**有过材料就算绿，
    # 所以它们放得过这种形态：材料在两秒内滚完、之后四十秒屏幕一个字都不动
    # （真实线上时间线：材料 12.0s 定格后，直到 51.8s 才出第一个正文字 —— 用户原话
    # 「一直卡着计时」说的就是这一段）。最长静默必须按**内容**算（材料尾部原文），
    # 按字数算会把截尾滚动窗口误判成静默，见 print_timeline 注释。
    gaps = silent_gaps(samples)
    biggest = max(gaps, key=lambda g: g[2], default=(0, 0, 0, ''))
    if DUMP_TIMELINE:
        top = sorted(gaps, key=lambda g: -g[2])[:3]
        print('最长静默段（内容级：材料尾部原文/正文长度/步骤标签任一变化即打断）：')
        for g in top:
            print(f'  {g[2] / 1000:6.1f}s  {g[0] / 1000:6.1f}s→{g[1] / 1000:6.1f}s  步骤={g[3] or "-"}')
    check(f'M7 最长静默 ≤{GAP_MAX_MS / 1000:.1f}s（材料/正文/步骤任一在动即算不静默）',
          biggest[2] <= GAP_MAX_MS,
          f'最长 {biggest[2] / 1000:.1f}s（{biggest[0] / 1000:.1f}s→{biggest[1] / 1000:.1f}s '
          f'步骤={biggest[3] or "-"}）；静默 >{GAP_MAX_MS / 1000:.1f}s 的段数 '
          f'{sum(1 for g in gaps if g[2] > GAP_MAX_MS)}')
    return report()


if __name__ == '__main__':
    sys.exit(main())
