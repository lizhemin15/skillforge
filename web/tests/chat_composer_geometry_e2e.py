#!/usr/bin/env python3
"""真浏览器终验：聊天页输入区几何 + 流式自动贴底（线上/本地同一套断言）。

为什么要真浏览器（node/jsdom 不够）：
  2026-09-14 线上实测取证的两个缺陷，静态断言全绿而界面是坏的 ——
  1) 胶囊行和 textarea 同一行 → textarea 被挤到右边：宽只剩 630/860、左偏 176px。
     只有**真实布局引擎**给出的 getBoundingClientRect 才算数。
  2) `.ch-scroll` 带 `scroll-behavior: smooth`，而流式每帧都 `scrollTop = scrollHeight`：
     平滑动画每次赋值重启 → 实测流式过程中**落后 994px**（三分之二正文在视口外）。
     这个只有**真流式**（真模型、真分帧）才能测出来，所以本脚本发一条真消息。

判据（每条打 ok/FAIL，任何 FAIL 都算红、退出码 1；不是崩溃红）：
  几何（输入框该占满的位置只剩「发送」按钮那点地儿）：
    G1 胶囊行在 textarea 上方（两行，不同 y）
    G2 胶囊行与 textarea 无水平/垂直重叠
    G3 textarea 空值高度 ≤ 60px（96px 死区回归防线）
    G4 textarea + 发送按钮 ≈ 输入框内宽（空隙只允许内边距+gap ≤ 34px，
       即证明没有别的东西在旁边偷宽度）
    G4b textarea 宽 ≥ 输入框宽 - 90px（历史 bug 时 630/860，这条是「占满」锚点）
    G5 textarea 左缘只比输入框左缘内缩 ≤14px（历史 bug 时内缩 176px）
  流式贴底：
    S1 `.ch-scroll` 计算样式 scroll-behavior == auto（smooth 是根因防线）
    S3 前提：`.ch-msg.assistant .ch-bubble` 真收到 ≥200 字流式正文（否则 S2 是空跑）
    S2 流式全程落后 ≤ 6px（旧行为实测落后 994px）；流式结束后 gap ≤ 2px

用法：
  python3 web/tests/chat_composer_geometry_e2e.py                 # 打 127.0.0.1:8092
  BASE=http://127.0.0.1:9999 python3 web/tests/chat_composer_geometry_e2e.py
  PROMPT='...' python3 web/tests/chat_composer_geometry_e2e.py     # 换提示词

SKIP 规则：playwright 不可用 / 页面打不开 → 打 SKIP 并 exit 0。SKIP != PASS。
"""
import hashlib
import os
import sys
import time

BASE = os.environ.get('BASE', 'http://127.0.0.1:8092')
# 必须逼出「长到能撑破滚动容器」的正文：短答案在 664px 高的容器里根本不溢出，
# 贴底断言会 gap=0 恒绿（空跑绿）。所以默认提示词就要 3000 字以上。
PROMPT = os.environ.get('PROMPT', (
    '写一份「星河科技空间计算芯片 X1 发布会」的完整策划方案，不少于 3000 字，'
    '分成 12 个部分：背景、目标、时间地点、议程、场地布置、嘉宾邀约、媒体传播、'
    '物料清单、预算概要、风险预案、执行排期、效果评估。每部分都要写满，'
    '直接输出正文，不要反问我任何问题。'))
# INJECT_SMOOTH=1：活页面注入 scroll-behavior: smooth（复刻历史根因），S2 必须转红。
# 这是 S2 的负向自证 —— 不注入时绿、注入后必须红，否则断言没有牙齿。
INJECT_SMOOTH = os.environ.get('INJECT_SMOOTH') == '1'

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


GEOM_JS = """() => {
  const sc = document.querySelector('.ch-scroll');
  const mrow = document.querySelector('.ch-mrow');
  const ta = document.querySelector('.ch-input') || document.querySelector('textarea');
  const send = document.querySelector('#chat-send');
  const box = ta && ta.parentElement;
  const R = e => { const r = e.getBoundingClientRect();
    return {x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width), h: Math.round(r.height)}; };
  if (!sc || !ta || !box) return {missing: true, hasScroll: !!sc, hasTa: !!ta};
  return {scroll: R(sc), mrow: mrow ? R(mrow) : null, ta: R(ta),
          send: send ? R(send) : null, box: R(box),
          taMinH: getComputedStyle(ta).minHeight,
          scrollBehavior: getComputedStyle(sc).scrollBehavior};
}"""

SAMPLER_JS = """() => {
  const sc = document.querySelector('.ch-scroll');
  window.__sc = {samples: [], t0: Date.now()};
  clearInterval(window.__iv);
  window.__iv = setInterval(() => {
    window.__sc.samples.push([Date.now() - window.__sc.t0,
                              Math.round(sc.scrollHeight - sc.scrollTop - sc.clientHeight)]);
  }, 200);
  return true;
}"""

BUBBLE_JS = """() => {
  const b = document.querySelector('.ch-msg.assistant .ch-bubble');
  return b ? b.innerText.replace(/\\s+/g, '') : '';
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
    g = pg.evaluate(GEOM_JS)
    if g.get('missing'):
        check('页面有 .ch-scroll / textarea / 输入框容器', False, str(g))
        return report()
    print('几何: ' + str(g))
    ta, mrow, box, send = g['ta'], g['mrow'], g['box'], g['send']

    check('G1 胶囊行在 textarea 上方（两行，不再是同一行）',
          bool(mrow) and mrow['y'] + mrow['h'] <= ta['y'] + 1,
          f"mrow.bottom={mrow and mrow['y'] + mrow['h']} ta.top={ta['y']}")
    if mrow:
        ox = min(ta['x'] + ta['w'], mrow['x'] + mrow['w']) - max(ta['x'], mrow['x'])
        oy = min(ta['y'] + ta['h'], mrow['y'] + mrow['h']) - max(ta['y'], mrow['y'])
        check('G2 胶囊行与 textarea 无重叠（胶囊不挤占输入区）',
              not (ox > 0 and oy > 0), f'重叠 {ox}x{oy}px')
    check('G3 空输入框高度 ≤ 60px（96px 死区回归防线）', ta['h'] <= 60, f"h={ta['h']}")
    slack = box['w'] - ta['w'] - (send['w'] if send else 0)
    check('G4 textarea + 发送按钮 ≈ 输入框内宽（没别的东西在旁边偷宽度，空隙 ≤34px）',
          slack <= 34, f"box.w={box['w']} ta.w={ta['w']} send.w={send and send['w']} 空隙={slack}")
    check('G4b textarea 宽 ≥ 输入框宽 - 90px（历史 bug 时 630/860）',
          ta['w'] >= box['w'] - 90, f"ta.w={ta['w']} box.w={box['w']}")
    check('G5 textarea 左缘内缩 ≤14px（历史 bug 时内缩 176px）',
          0 <= ta['x'] - box['x'] <= 14, f"内缩={ta['x'] - box['x']}px")
    check('S1 .ch-scroll 计算样式 scroll-behavior == auto（smooth 是根因）',
          g['scrollBehavior'] == 'auto', f"实际={g['scrollBehavior']}")

    # —— 真流式（必须真撑破容器才算测到） ——
    if INJECT_SMOOTH:
        pg.add_style_tag(content='.ch-scroll { scroll-behavior: smooth !important; }')
        print('已注入 scroll-behavior: smooth（负向自证模式：S2 应当转红）')
    pg.fill('textarea', PROMPT)
    pg.press('textarea', 'Enter')
    print('已发送，等正文开始流式（最多 180s，正文 120 字起才挂采样器）…')
    t0 = time.time()
    while time.time() - t0 < 180:
        pg.wait_for_timeout(1000)
        if len(pg.evaluate(BUBBLE_JS)) >= 120:
            break
    pg.evaluate(SAMPLER_JS)
    print(f'正文开始流式（等了 {int(time.time() - t0)}s）→ 采样器已挂上')
    prev, same, last = None, 0, 0
    t1 = time.time()
    while time.time() - t1 < 480:
        pg.wait_for_timeout(1000)
        txt = pg.evaluate(BUBBLE_JS)
        h = hashlib.md5(txt.encode()).hexdigest()
        if h == prev and len(txt) >= 800:
            same += 1
            if same >= 8:          # 连续 8s 内容一字不变 = 生成结束
                break
        else:
            same = 0
        prev, last = h, len(txt)
        if int(time.time() - t1) % 20 == 0:
            print(f'  … 流式 {int(time.time() - t1)}s, 正文 {last} 字')

    out = pg.evaluate("""() => {
      const sc = document.querySelector('.ch-scroll');
      const b = document.querySelector('.ch-msg.assistant .ch-bubble');
      clearInterval(window.__iv);
      const s = window.__sc.samples;
      const worst = s.slice().sort((a, b) => b[1] - a[1]).slice(0, 5);
      return {streamedChars: b ? b.innerText.replace(/\\s+/g, '').length : 0,
              sampleCount: s.length,
              maxGap: Math.max(0, ...s.map(x => x[1])),
              worst,
              scrollH: sc.scrollHeight, clientH: sc.clientHeight,
              finalGap: Math.round(sc.scrollHeight - sc.scrollTop - sc.clientHeight)};
    }""")
    overflow = out['scrollH'] - out['clientH']
    print(f"流式采样 {out['sampleCount']} 次；maxGap={out['maxGap']}；"
          f"容器 scrollHeight={out['scrollH']} clientHeight={out['clientH']} 溢出={overflow}px；"
          f"最差 5 帧(ms,gap)={out['worst']}")
    # 前提校验必须在 S2 之前：前提不成立时 S2 的绿是空跑绿，不能出现。
    if not check('S3a 前提：真收到 ≥800 字流式正文', out['streamedChars'] >= 800,
                 f"chars={out['streamedChars']}"):
        print('（前提不成立 → 不评 S2，避免空跑绿）')
        return report()
    if not check('S3b 前提：真撑破滚动容器（溢出 ≥100px，否则贴底断言恒真）',
                 overflow >= 100, f'溢出仅 {overflow}px'):
        print('（没撑破容器 → 不评 S2，避免空跑绿）')
        return report()
    check('S2 流式全程落后 ≤ 6px（旧行为实测落后 994px）',
          out['maxGap'] <= 6, f"maxGap={out['maxGap']}")
    check('S2b 流式结束后贴底（gap ≤ 2px）', out['finalGap'] <= 2, f"finalGap={out['finalGap']}")
    return report()


if __name__ == '__main__':
    sys.exit(main())
