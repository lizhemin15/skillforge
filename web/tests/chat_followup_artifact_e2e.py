# -*- coding: utf-8 -*-
"""线上验收：多轮指代——「先写新闻稿，再整理成 Word」必须搬运上一轮产物原文。

用户诉求原文（2026-09-17）：
  「先让生成一个新闻稿以后，让 ai 把新闻稿整理成 word，就通常没有管之前的生成内容。」

根因（已在 agent/compact.go 的产物层 + agent.go GenerateDoc 的注入里修）：
  产物正文是用户下一轮「整理成…/导出成…」的指代对象。老实现里模型看不到它，
  只能凭空另造一份文档 —— 用户看到的就是「它不管我上一轮写的东西」。

本腿的判据刻意**不依赖模型措辞**：第 1 轮让模型在正文里原样带一个本轮随机锚串
（形如「锚串3f8a1c2d」）。这个锚串只可能来自上一轮产物——第 2 轮的提示词里根本没有它，
模型也不可能猜到。于是：
  · 交付文件里出现锚串 ⇒ 上一轮产物真的进了上下文（诉求 1 被满足）
  · 交付文件里没有锚串 ⇒ 另起炉灶（诉求 1 复现）

负向对照（手动跑，不进 LIVE-LEGS）：
  SKIP_TURN1=1 python3 web/tests/chat_followup_artifact_e2e.py
  跳过第 1 轮 → 锚串从未存在 → F2 必须转红、RC=1。
  这一步证明 F2 不是在空跑绿（没有它，「模型自己造出锚串」无法被排除）。

  SKIP_TURN1=1 的 RC=1 是**预期**结果，不是回归。

# LIVE-LEGS: news2word
# ↑ 线上验收 leg 声明。scripts/acceptance-live.sh 只认这一行来枚举要跑几条 leg；
#   web/tests/live_e2e_roster.test.mjs 守着它跟文件真身不许脱钩。
"""
import base64
import os
import re
import sys
import time
import uuid
import zipfile

BASE = os.environ.get('BASE', 'http://127.0.0.1:8092')
MAXW = int(os.environ.get('MAXW', '300'))       # 单轮最长等多久（秒）
SKIP_TURN1 = os.environ.get('SKIP_TURN1') == '1'

# 本轮唯一锚串：32 位随机，只出现在第 1 轮的提示词里。
ANCHOR = '锚串' + uuid.uuid4().hex[:8]

PROMPT1 = (
    '写一篇关于星禾科技发布数据中台 3.0 的新闻稿，正文不少于 400 字，直接输出正文，不要任何解释。'
    f'必须在正文第一段原样包含这个编号：{ANCHOR}（一字不许改，不许翻译，不许加空格）。'
)
PROMPT2 = '把上面这篇新闻稿原样整理成 Word 文档，正文保持原样，不要重写、不要压缩。'

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


READ_JS = """() => {
  const bubbles = Array.from(document.querySelectorAll('.ch-msg.assistant .ch-bubble'));
  const link = Array.from(document.querySelectorAll('a.atx-link'))
    .find(a => ((a.innerText || '').includes('已生成文档')));
  return {
    bubbleCount: bubbles.length,
    lastBubble: bubbles.length ? (bubbles[bubbles.length - 1].innerText || '') : '',
    // 第 1 轮的产物一定在第一个 assistant 气泡里（这一轮之后才会有文件卡片）
    firstBubble: bubbles.length ? (bubbles[0].innerText || '') : '',
    genfile: !!link,
    genHref: link ? (link.getAttribute('href') || '') : '',
    active: !!document.querySelector('.ctk-step.active'),
    sendIdle: !document.querySelector('#chat-send').disabled,
    errs: (window.__errs || []),
  };
}"""

ERR_HOOK = """() => {
  window.__errs = [];
  window.addEventListener('error', e => window.__errs.push(String(e.message).slice(0, 120)));
  return true;
}"""

# 页面内取文件字节（带 cookie），base64 回传。
FETCH_JS = """async (href) => {
  const r = await fetch(href, {credentials: 'include'});
  const buf = new Uint8Array(await r.arrayBuffer());
  let bin = '';
  for (let i = 0; i < buf.length; i++) bin += String.fromCharCode(buf[i]);
  return {b64: btoa(bin), status: r.status, ctype: r.headers.get('content-type') || ''};
}"""


def wait_turn(pg, need_file=False, maxw=None):
    """等到这一轮真的结束（发送按钮回可用）或超时。返回最后一帧读数。"""
    deadline = time.time() + (maxw or MAXW)
    last = {}
    stable = 0
    last_len = -1
    while time.time() < deadline:
        time.sleep(1.0)
        last = pg.evaluate(READ_JS)
        done = last['sendIdle'] and not last['active'] and last['bubbleCount'] > 0
        if need_file:
            done = done and last['genfile']
        if done and len(last['lastBubble']) == last_len:
            stable += 1
            if stable >= 2:
                return last, True
        else:
            stable = 0
        last_len = len(last['lastBubble'])
    return last, False


def office_text(raw: bytes) -> str:
    """从 docx/xlsx/pptx 里把所有可见文本抽出来（纯标准库，不看格式细节）。"""
    out = []
    try:
        with zipfile.ZipFile(__import__('io').BytesIO(raw)) as z:
            for n in z.namelist():
                if not (n.endswith('.xml') or n.endswith('.rels')):
                    continue
                if 'word/document' in n or 'word/footnotes' in n or 'sharedStrings' in n \
                        or 'sheet' in n or 'slide' in n:
                    xml = z.read(n).decode('utf-8', 'ignore')
                    out.append(re.sub(r'<[^>]+>', '', xml))
    except Exception as e:
        print(f'⚠ 交付物不是 OOXML（{str(e)[:80]}），退回按纯文本读')
        out.append(raw.decode('utf-8', 'ignore'))
    return '\n'.join(out)


def main():
    try:
        from playwright.sync_api import sync_playwright
    except Exception as e:  # pragma: no cover
        print(f'SKIP playwright 不可用：{e}')
        return 0

    print(f'[锚串] {ANCHOR}' + ('（SKIP_TURN1=1：负向对照，F2 必须转红）' if SKIP_TURN1 else ''))
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
            pg.evaluate(ERR_HOOK)
            return run_checks(pg)
        finally:
            br.close()


def run_checks(pg):
    turn1_text = ''
    if not SKIP_TURN1:
        pg.fill('textarea', PROMPT1)
        pg.click('#chat-send')
        first, ended1 = wait_turn(pg)
        turn1_text = first.get('firstBubble') or ''
        print(f"[第1轮] 结束={ended1} 产物 {len(turn1_text)} 字 / 尾部「{turn1_text[-40:] if turn1_text else ''}」")
    else:
        print('[第1轮] 已按 SKIP_TURN1=1 跳过')

    # P 系列是前提：前提不成立时 F 系列全成空跑，必须先断。
    if not SKIP_TURN1:
        check('P1 前提：第 1 轮真收到 ≥300 字正文（否则 F 系列是空跑）',
              len(turn1_text) >= 300, f'实得 {len(turn1_text)} 字')
        check('P2 前提：第 1 轮产物里真带上了本轮锚串（锚串没进产物的话 F2 无从判断）',
              ANCHOR in turn1_text, f'锚串 {ANCHOR} 不在第 1 轮产物里')

    pg.fill('textarea', PROMPT2)
    pg.click('#chat-send')
    last, ended2 = wait_turn(pg, need_file=True)
    print(f"[第2轮] 结束={ended2} 文件卡片={last.get('genfile')} href={last.get('genHref')} "
          f"气泡 {len(last.get('lastBubble') or '')} 字")

    ok_file = check('F1 第 2 轮交付了「已生成文档」文件卡片',
                    bool(last.get('genfile')), f"genfile={last.get('genfile')}")

    doc_text = ''
    if ok_file:
        href = last['genHref']
        if not href.startswith('http'):
            href = BASE.rstrip('/') + href
        got = pg.evaluate(FETCH_JS, href)
        raw = base64.b64decode(got['b64'])
        print(f"[交付物] HTTP {got['status']} / {got['ctype']} / {len(raw)} 字节")
        doc_text = office_text(raw)
        print(f"[交付物正文] {len(doc_text)} 字 / 含锚串={ANCHOR in doc_text}")

    check('F2 交付文件里含第 1 轮的锚串（= 真的搬运了上一轮产物，不是另起炉灶）',
          ANCHOR in doc_text, f'交付物正文 {len(doc_text)} 字，锚串 {ANCHOR} 不在其中')
    check('F3 交付正文长度 ≥ 第 1 轮产物的 50%（= 搬运全文，不是只抄一句话）',
          len(doc_text) >= len(turn1_text) * 0.5,
          f'交付 {len(doc_text)} 字 vs 第 1 轮 {len(turn1_text)} 字')
    check('F4 两轮真流式过程中页面无 JS 异常', not last.get('errs'), str((last.get('errs') or [])[:2]))
    return report()


if __name__ == '__main__':
    sys.exit(main())
