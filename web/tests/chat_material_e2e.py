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
"""
# LIVE-LEGS: writing | docgen REQUIRE_FILE=1 PROMPT_KEY=docgen
# ↑ 线上验收 leg 声明。scripts/acceptance-live.sh 只认这一行来枚举要跑几条 leg
#   （不许在 runner 里写死文件名 —— 那是「漏加 = 这个 leg 不存在」的老洞）；
#   web/tests/live_e2e_roster.test.mjs 守着它跟文件真身不许脱钩。
import os
import sys
import time

BASE = os.environ.get('BASE', 'http://127.0.0.1:8092')
MAXW = int(os.environ.get('MAXW', '240'))  # 单轮最长等多久（秒）

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
  window.__mt = {samples: [], t0: Date.now(), errs: []};
  window.addEventListener('error', e => window.__mt.errs.push(String(e.message).slice(0,120)));
  clearInterval(window.__iv);
  window.__iv = setInterval(() => {
    const act = document.querySelector('.ctk-step.active');
    const m = act ? act.querySelector('.ctk-mat') : null;
    const b = document.querySelector('.ch-msg.assistant .ch-bubble');
    const txt = m ? (m.innerText || '') : '';
    window.__mt.samples.push([
      Date.now() - window.__mt.t0,
      txt.length,
      txt.slice(-40),
      b ? (b.innerText || '').replace(/\\s+/g, '').length : 0,
      !!act,
      document.querySelectorAll('.ctk-step.done .ctk-mat').length,
    ]);
  }, 200);
  return true;
}"""

READ_JS = """() => {
  const s = window.__mt || {samples: [], errs: []};
  const b = document.querySelector('.ch-msg.assistant .ch-bubble');
  return {samples: s.samples, errs: s.errs,
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
                        n = body.count('(s.material')
                        body = body.replace('(s.material', '(false && s.material')
                        print(f'[inject] chat.js 里材料渲染分支命中 {n} 处，已改成恒不渲染')
                        route.fulfill(status=r.status, body=body,
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
        check('M5 前提：整轮真收到 ≥200 字正文（否则 M1 是空跑）', ok_body,
              f"bubble={last['bubble']}")

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
        check('M3 材料被截尾 ≤220 字（不撑破面板）', max(s[1] for s in mats) <= 220,
              f"峰值 {max(s[1] for s in mats)} 字")
        check('M4 材料只挂进行中那一步（`.ctk-step.done .ctk-mat` 全程为 0）',
              max(s[5] for s in samples) == 0, f"done 步里出现过 {max(s[5] for s in samples)} 个材料块")
    else:
        # 没材料：M1/M2/M3/M4 全红。这是 INJECT_NOMAT=1 时要看到的结果。
        check('M1 材料出现在首段正文之前（等的时候屏幕上有真内容，不是跳秒）', False,
              '整轮没有材料帧')
        check('M2 材料在滚（≥3 个不同尾部快照，不是一次性贴一块）', False, '整轮没有材料帧')
        check('M3 材料被截尾 ≤220 字（不撑破面板）', False, '整轮没有材料帧')
        check('M4 材料只挂进行中那一步（`.ctk-step.done .ctk-mat` 全程为 0）',
              max((s[5] for s in samples), default=0) == 0, '')

    check('M6 真流式过程中页面无 JS 异常', not last['errs'], str(last['errs'][:2]))
    return report()


if __name__ == '__main__':
    sys.exit(main())
