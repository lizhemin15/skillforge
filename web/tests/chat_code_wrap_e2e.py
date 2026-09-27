#!/usr/bin/env python3
"""真浏览器终验：答案里的「超长单行代码块」不许把对话列横向拉长，且必须自动换行。

背景（2026-09-27 用户原话）：
  「skillhub 项目里面的对话模块，在生成的代码块里面单行很长的时候，直接整个
    对话框被横向拉的很长，我期望不要这样，应当可以自动换行」

线上实测（1366×768 真 Chromium，8092，真模型真流式）：
  答案里出现一条 520 字符、**没有一个空格**的 SQL 代码行时：
    .ch-col   = 860（固定，没变）
    .ch-body  = 4124  ← 正文槽被撑爆
    .ch-bubble= 3217（max-width:78% 算的是被撑爆的槽，所以也飞了）
    .ch-scroll 内容宽 4313 / 视口 1102 → 对话区出现横向滚动条，正文整块跑出屏幕
  改后同一输入：.ch-body=768、.ch-bubble=599、代码块内部 0 横向滚动、pre 高度 204（换行了）

根因（两条，缺一条都还会坏）：
  ① js/chat.js 的正文槽用的是**内联** `style.flex = '1'`。flex 项的默认
     `min-width: auto` 意味着「不许缩到内容最小宽度以下」，而一条没有空格的超长
     代码行的最小内容宽度就是那一整行 → 槽被撑到 4000px 级。修法：`.ch-body`
     （flex:1 **+ min-width:0**），槽可以收缩，气泡回到 78% 上限。
  ② `.ch-bubble pre` 原来只有 `overflow-x: auto`：代码块内部横向滚动 —— 用户看到的是
     「整块代码被裁掉一半、要左右拖」。修法：`white-space: pre-wrap`（保留缩进/换行、
     行尾可折）+ `overflow-wrap: anywhere`（连没有空格的长 token 也能从任意字符断开）。

判据（任何 FAIL 都算红、退出码 1；SKIP 不算通过，由 runner 决定放行）：
  W1 页面有 #chat-col / vendor marked（否则量出来的都是空跑）
  W2 负向自证：**旧结构**（内联 flex:1 + 旧 pre 样式）必须复现「横向拉长」
     —— 这条是尺子有没有牙齿的证明。旧结构都不拉长，说明 W3~W6 全是假绿。
  W3 当前实现的结构不拉长（.ch-col.scrollWidth ≤ clientWidth+1）
  W4 代码块内部 0 横向滚动（pre.scrollWidth ≤ clientWidth+1）且**真的折行了**（高 > 40px）
  W5 气泡回到 78% 上限（≤ .ch-msg 宽的 78% + 2）
  W6 pre 计算样式 white-space: pre-wrap（换行真开着，不是死配置）
  W7 整页无横向滚动
  W8 静态源码闸门：chat.js 的 assistant 正文槽必须是 `.ch-body`，且不许再出现内联
     `style.flex = '1'`（浏览器腿量的是注入的 DOM，管不到 JS 真身 —— 少了这条，
     谁把 JS 改回内联 flex 谁就能让本文件继续全绿）

用法：
  python3 web/tests/chat_code_wrap_e2e.py                       # 默认打 127.0.0.1:8092
  BASE=http://127.0.0.1:9999 python3 web/tests/chat_code_wrap_e2e.py
  INJECT_OLD=1  ...   # 负向自证：正向腿改用旧结构 → W3/W4/W5/W6 必须转红

本地静态靶面（不需要后端 / 不需要模型）：
  python3 scripts/serve_web_static.py 8123
  BASE=http://127.0.0.1:8123 python3 web/tests/chat_code_wrap_e2e.py

SKIP 规则：playwright 不可用 / 页面打不开 / 没有 vendor marked → 打 SKIP 并 exit 0。
SKIP != PASS。
"""
# LIVE-LEGS: default
import os
import re
import sys

BASE = os.environ.get('BASE', 'http://127.0.0.1:8092')
INJECT_OLD = os.environ.get('INJECT_OLD') == '1'

WEB = os.path.join(os.path.dirname(os.path.abspath(__file__)), '..')

# 520 个 a、没有一个空格 —— 复刻线上那条「一整行没有一个空格」的长 SQL。
LONG_TOKEN = 'a' * 520
MD = ('下面是一段示例代码：\n\n```sql\nSELECT id, title, ' + LONG_TOKEN + ';\n```\n\n结束。')

checks = 0
fails = []


def check(name, ok, detail=''):
    global checks
    checks += 1
    print(f'  {"ok  " if ok else "FAIL"} {name}' + ((f' — {detail}') if detail and not ok else ''))
    if not ok:
        fails.append(name)
    return bool(ok)


def report():
    print(f'--- {checks - len(fails)}/{checks} ok ---')
    if fails:
        print('FAILED: ' + '; '.join(fails))
        return 1
    return 0


# 注入一条 assistant 消息，结构照线上 chat.js：
#   .ch-msg.assistant > .ch-avatar + <slot> > .ch-bubble > (marked 渲染的 pre)
# slot 用 .ch-body（当前实现）或内联 flex:1（旧实现）。pre 的样式由旧/新 CSS 决定，
# 「旧 pre 样式」用一段 !important 覆盖复刻，避免动到仓库文件。
BUILD_JS = """({ slotOld, preOld, md }) => {
  document.querySelectorAll('.ch-msg').forEach((n) => n.remove());
  let st = document.getElementById('__oldpre');
  if (preOld) {
    if (!st) { st = document.createElement('style'); st.id = '__oldpre'; document.head.appendChild(st); }
    st.textContent = '.ch-bubble pre{white-space:pre !important;overflow-wrap:normal !important;'
                   + 'word-break:normal !important;overflow-x:auto !important}';
  } else if (st) { st.remove(); }
  const col = document.getElementById('chat-col');
  const msg = document.createElement('div');
  msg.className = 'ch-msg assistant';
  msg.innerHTML = '<div class="ch-avatar">A</div>'
    + (slotOld ? '<div style="flex:1"></div>' : '<div class="ch-body"></div>');
  const slot = msg.lastElementChild;
  const bubble = document.createElement('div');
  bubble.className = 'ch-bubble';
  bubble.innerHTML = window.marked.parse(md, { gfm: true, breaks: true });
  slot.appendChild(bubble);
  col.appendChild(msg);
}"""

MEASURE_JS = """() => {
  const q = (s) => document.querySelector(s);
  const R = (el) => el.getBoundingClientRect();
  const pre = q('.ch-msg.assistant .ch-bubble pre');
  if (!pre) return { missing: true };
  const bubble = q('.ch-msg.assistant .ch-bubble');
  const slot = q('.ch-msg.assistant > .ch-body') || q('.ch-msg.assistant > div:last-child');
  const col = q('#chat-col'), scroll = q('#chat-scroll');
  return {
    colW: Math.round(R(col).width), colScrollW: col.scrollWidth,
    scrollClientW: scroll.clientWidth, scrollScrollW: scroll.scrollWidth,
    slotW: Math.round(R(slot).width), slotMinWidth: getComputedStyle(slot).minWidth,
    bubbleW: Math.round(R(bubble).width),
    preW: Math.round(R(pre).width), preH: Math.round(R(pre).height),
    preScrollW: pre.scrollWidth, preClientW: pre.clientWidth,
    preWhiteSpace: getComputedStyle(pre).whiteSpace,
    docScrollW: document.documentElement.scrollWidth,
    docClientW: document.documentElement.clientWidth,
  };
}"""


def is_stretched(m):
    """「横向拉长」判据：对话列自身出现横向溢出，或气泡突破 78% 上限。
    用 .ch-col（860 定宽）而不是整页 scrollWidth —— `.layout.ch-lock` 是 overflow:hidden，
    整页看起来永远不滚，被拉长的是列的内部。"""
    if m.get('missing'):
        return False
    bubble_cap = 812 * 0.78 + 2   # .ch-msg 内容宽 812 的 78%
    return m['colScrollW'] > m['colW'] + 1 or m['bubbleW'] > bubble_cap


def source_gate():
    """静态源码闸门：浏览器腿量的是注入的 DOM，管不到 JS 真身。"""
    chat = open(os.path.join(WEB, 'js', 'chat.js'), encoding='utf-8').read()
    css = open(os.path.join(WEB, 'css', 'style.css'), encoding='utf-8').read()
    check('W8a chat.js 的 assistant 正文槽用 .ch-body（不是内联 flex）',
          "el('div', 'ch-body')" in chat and "style.flex = '1'" not in chat,
          "没找到 el('div', 'ch-body')，或仍存在内联 style.flex = '1'")
    check('W8b style.css 的 .ch-body 声明了 min-width: 0',
          re.search(r'\.ch-body\s*\{[^}]*min-width:\s*0', css) is not None,
          '.ch-body 缺少 min-width:0（槽又会拒绝收缩）')
    check('W8c style.css 的 .ch-bubble pre 声明了 pre-wrap',
          re.search(r'\.ch-bubble pre\s*\{[^}]*white-space:\s*pre-wrap', css) is not None,
          '.ch-bubble pre 没有 white-space:pre-wrap（长行不会折）')
    check('W8d .ch-bubble pre 许可在任意字符处断开（overflow-wrap: anywhere）',
          re.search(r'\.ch-bubble pre\s*\{[^}]*overflow-wrap:\s*anywhere', css) is not None,
          '没有 overflow-wrap:anywhere → 无空格长 token 仍会顶出去')


def main():
    try:
        from playwright.sync_api import sync_playwright
    except Exception as e:  # pragma: no cover
        print(f'SKIP playwright 不可用：{e}')
        return 0

    with sync_playwright() as p:
        br = p.chromium.launch()
        try:
            pg = br.new_page(viewport={'width': 1366, 'height': 768})
            try:
                pg.goto(BASE, wait_until='domcontentloaded', timeout=15000)
            except Exception as e:
                print(f'SKIP 打不开 {BASE}（服务没起？）：{str(e)[:120]}')
                return 0
            try:
                pg.wait_for_selector('#chat-col', timeout=10000)
            except Exception:
                print('SKIP 页面没有 #chat-col（不是聊天页？）')
                return 0
            if not pg.evaluate('() => !!(window.marked && window.marked.parse)'):
                print('SKIP vendor marked 没加载（静态靶面挂错？）—— 量不出真渲染结果')
                return 0

            # W2 负向自证：旧结构（内联 flex:1 + 旧 pre 样式）必须复现拉长
            pg.evaluate(BUILD_JS, {'slotOld': True, 'preOld': True, 'md': MD})
            pg.wait_for_timeout(120)
            before = pg.evaluate(MEASURE_JS)

            # W3~ W7 正向：当前实现（INJECT_OLD=1 时故意换回旧结构，用于自证尺子有牙）
            old = INJECT_OLD
            pg.evaluate(BUILD_JS, {'slotOld': old, 'preOld': old, 'md': MD})
            pg.wait_for_timeout(120)
            after = pg.evaluate(MEASURE_JS)

            if after.get('missing'):
                check('注入的代码块真的渲染出来了', False, '找不到 .ch-msg.assistant .ch-bubble pre')
                return report()

            print('负向（旧结构）: ' + str(before))
            print('正向（当前实现）: ' + str(after))
            if INJECT_OLD:
                print('（INJECT_OLD=1 自证模式：正向腿也用了旧结构，W3~W6 应当转红）')

            check('W1 注入的 assistant 消息与代码块渲染成功',
                  not before.get('missing') and not after.get('missing'), '')
            check('W2 负向自证：旧结构必须复现横向拉长（尺子有牙）',
                  is_stretched(before), f"col.scrollW={before['colScrollW']} vs col={before['colW']}")
            check('W3 当前实现：对话列不横向溢出',
                  after['colScrollW'] <= after['colW'] + 1,
                  f"col.scrollW={after['colScrollW']} col={after['colW']}")
            check('W4 代码块内部 0 横向滚动，且真的折行了',
                  after['preScrollW'] <= after['preClientW'] + 1 and after['preH'] > 40,
                  f"pre.scrollW={after['preScrollW']}/client={after['preClientW']} h={after['preH']}")
            check('W5 气泡回到 78% 宽度上限内',
                  after['bubbleW'] <= 812 * 0.78 + 2, f"bubbleW={after['bubbleW']}")
            check('W6 pre 计算样式 white-space = pre-wrap',
                  after['preWhiteSpace'] == 'pre-wrap', f"实际={after['preWhiteSpace']}")
            check('W7 整页无横向滚动',
                  after['docScrollW'] <= after['docClientW'] + 1,
                  f"doc={after['docScrollW']}/{after['docClientW']}")
            source_gate()
            return report()
        finally:
            br.close()


if __name__ == '__main__':
    sys.exit(main())
