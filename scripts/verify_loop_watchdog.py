#!/usr/bin/env python3
"""线上复验：复读看门狗（秒级断流 + 带惩罚重试 + reset 帧清气泡）。

背景（2026-09-22 事故）：5 字问题在 write-plain 一路复读，47KB 同一句话直接刷进
气泡，屏幕上正文疯狂重复，后端还当正常内容交下去，一路跑到总预算才停。

这个脚本量的是**屏幕上真实发生了什么**，不是「代码里有这个字符串」：
用一段必然会自重复的提示词（重复输出同一短语）把模型的复读逼出来，
然后看客户端实收的帧序列。

判据（每条都非空跑 —— 前提不成立就报 PREMISE_MISS，绝不当 PASS）：
  C0 前提：客户端确实收到了正文 delta（> 0）——否则这一轮什么也没发生，
     后面的「没刷屏」全是空跑绿。
  C1 秒级收手：客户端实收 delta 总字节 ≤ BOUND（默认 12KB）。
     为什么是 12KB：检测阈 3×192=576 字节，检测间隔 2048 字节，故单次收手
     最坏 ~2.6KB；重试两发合计最坏 ~5.2KB，12KB 是两倍余量。
     修前线上是 47KB 复读刷屏，这条会红。
  C2 清气泡通路活着：收到 ≥1 个 reset 帧，且它出现在第一段正文之后
     （证明它清的是真流过的内容，不是开局空清）。
  C3 reset 与收手同源：reset 之前的 delta 字节数 ≤ BOUND
     （若 reset 出现在几十 KB 之后，说明前端是等到看门狗之外才被通知的）。
  C4 垃圾不交付：没有任何 done 帧的正文把短语重复 ≥ 50 遍
     （即：半截复读没有被当成「成品」交给用户）。

诊断（不参与判绿，但排障必须看）：服务端日志里 ErrLoopDetected 那行的
「循环体约 N 字节 / 已收 N 字节」——它同时验证 hit() 报的周期是否可信。

用法: python3 scripts/verify_loop_watchdog.py [base_url] [--prompt "..."] [--bound 12288]
退出码: 0 = 全绿；1 = 有 FAIL；3 = 前提不成立（模型这轮没复读，重跑或换提示词）。
"""
import json
import os
import re
import subprocess
import sys
import time
import urllib.request

BASE = sys.argv[1] if len(sys.argv) > 1 and not sys.argv[1].startswith('-') else \
    os.environ.get('BASE', 'http://127.0.0.1:8092')

# 提示词：把「同一短语连续重复」变成模型的必然行为。用 4 字短语 + 大遍数，
# 保证尾部 192 字节里全是重复内容（与线上事故同形态：45 字节/帧的自重复）。
PROMPT = os.environ.get(
    'LOOPWATCH_PROMPT',
    '请把「循环测试」这四个字连续重复输出 500 遍，除了这四个字不要输出任何其他内容，'
    '不要编号、不要换行、不要标点。')
for i, a in enumerate(sys.argv):
    if a == '--prompt':
        PROMPT = sys.argv[i + 1]
BOUND = int(os.environ.get('LOOPWATCH_BOUND', '12288'))
# 单次读超时（整条流）。旧二进制复读会一直吐到总预算，所以给足；
# 超时后仍有帧就照评，不当 SKIP。
STREAM_TIMEOUT = int(os.environ.get('LOOPWATCH_TIMEOUT', '420'))

PHRASE = '循环测试'


def collect(base, prompt, sid):
    """收完整条 SSE 流，记下每个帧的字节偏移与内容。

    返回 (frames, cut)：cut 非空表示流被截断（读超时/连接断）。
    **截断不能当「打不开」处理**：旧二进制那种复读刷屏恰好会跑很久，
    若把它当成 SKIP(=绿)，这把尺子就专挑被测故障时瞎掉 —— 典型的假绿。
    """
    url = base.rstrip('/') + '/api/chat'
    body = json.dumps({'session_id': sid, 'message': prompt}).encode()
    req = urllib.request.Request(url, data=body, headers={
        'Content-Type': 'application/json', 'Accept': 'text/event-stream'})
    frames = []
    buf = b''
    t0 = time.time()
    cut = ''
    try:
        with urllib.request.urlopen(req, timeout=STREAM_TIMEOUT) as r:
            while True:
                chunk = r.read(4096)
                if not chunk:
                    break
                buf += chunk
                while b'\n\n' in buf:
                    raw, buf = buf.split(b'\n\n', 1)
                    ev, data = None, ''
                    for line in raw.decode('utf-8', 'replace').splitlines():
                        if line.startswith('event:'):
                            ev = line[6:].strip()
                        elif line.startswith('data:'):
                            data += line[5:].strip()
                    try:
                        obj = json.loads(data) if data else {}
                    except Exception:
                        obj = {}
                    frames.append({'ev': ev, 'obj': obj, 'at': time.time() - t0})
    except Exception as e:
        # 一帧都没收到 = 真打不开（服务没起/连不上）；已经有帧 = 中途断了，照评。
        cut = '%s: %s' % (type(e).__name__, str(e)[:120])
    return frames, cut


def main():
    sid = 'loopwd-%d' % int(time.time())
    print('=== 复读看门狗线上复验 ===')
    print('BASE   :', BASE)
    print('SESSION:', sid)
    print('PROMPT :', PROMPT[:60] + ('…' if len(PROMPT) > 60 else ''))
    print('BOUND  : %d 字节（客户端实收 delta 上限）' % BOUND)

    frames, cut = collect(BASE, PROMPT, sid)
    if not frames:
        print('SKIP 打不开 %s（服务没起/连不上）：%s' % (BASE, cut))
        return 0
    if cut:
        print('⚠️ 流被截断（%s）—— 已收 %d 帧，仍按现有帧评估（不当 SKIP）'
              % (cut, len(frames)))

    # 逐帧累计：正文字节、reset 位置、done 正文、error 文本
    delta_bytes = 0
    first_delta_at = None
    first_reset = None          # 帧序号
    delta_at_first_reset = None
    resets = 0
    tail_text = ''              # 最后一个 reset 之后的累计正文 = 用户最终看到的内容
    errors = []
    terminal = None             # done / error，用来判「断流后有没有跟用户说一声」
    for n, f in enumerate(frames):
        if f['ev'] == 'delta':
            t = f['obj'].get('t') or ''
            if t:
                delta_bytes += len(t.encode())
                tail_text += t
                if first_delta_at is None:
                    first_delta_at = f['at']
        elif f['ev'] == 'reset':
            resets += 1
            tail_text = ''       # 前端在这里清气泡，气泡里的内容归零
            if first_reset is None:
                first_reset = n
                delta_at_first_reset = delta_bytes
        elif f['ev'] == 'done':
            terminal = 'done'
        elif f['ev'] == 'error':
            terminal = 'error'
            errors.append(str(f['obj'].get('message') or f['obj'].get('error') or f['obj']))

    print('\n--- 实测 ---')
    print('帧总数        : %d' % len(frames))
    print('客户端实收正文: %d 字节' % delta_bytes)
    print('reset 帧      : %d 个；首个在第 %s 帧、其前正文 %s 字节'
          % (resets, first_reset, delta_at_first_reset))
    print('error 帧      : %d 个' % len(errors))
    for e in errors[:2]:
        print('  ·', e[:200])

    fails = []

    # C0 前提：本轮真的吐过正文，否则后面全空跑
    premise_miss = None
    if first_delta_at is None:
        premise_miss = '本轮一帧正文都没有，无法评「有没有刷屏」'
        print('\nPREMISE_MISS 候选：%s' % premise_miss)

    # C1
    if delta_bytes > BOUND:
        fails.append('C1 客户端收了 %d 字节正文才收手（上限 %d）—— 看门狗没掐住'
                     % (delta_bytes, BOUND))
    else:
        print('C1 OK  客户端实收 %d 字节 ≤ %d' % (delta_bytes, BOUND))

    # C2 三态，各自含义不同，别混：
    #   ≥1 个 reset            → 看门狗真响了，且没在正文之前空清（下面还要查）
    #   没有 reset 但流 > 上限  → 真事故：复读刷屏、看门狗没上线/没触发 → FAIL
    #   没有 reset 且流很短     → 本轮模型没复读，看门狗本就不该响 → PREMISE_MISS
    # 早先版本在「没有 reset」时直接 return，把 C3/C4/C5 全短路了 —— 事故形态
    # 只报出 C1 一条红，剩下三条判据根本没跑，报告等于缺证据。
    if resets < 1 and delta_bytes > BOUND:
        fails.append('C2 没有 reset 帧，且实收了 %d 字节 —— 看门狗没上线/没触发'
                     % delta_bytes)
    elif resets < 1:
        premise_miss = premise_miss or ('没有 reset 帧，本轮模型可能压根没复读（看门狗不该响）')
    elif first_reset == 0 or (delta_at_first_reset or 0) == 0:
        fails.append('C2 reset 帧出现在任何正文之前 —— 清的是空气，不是真流过的内容')
    else:
        print('C2 OK  %d 个 reset，首个之前已有 %d 字节正文被清掉'
              % (resets, delta_at_first_reset))

    # C3（没有 reset 就无从谈通知早晚，跳过并明说）
    if first_reset is None:
        print('C3 --  没有 reset 帧，跳过（通知早晚无从评）')
    elif (delta_at_first_reset or 0) > BOUND:
        fails.append('C3 首个 reset 到来前已流出 %d 字节（上限 %d）—— 通知太晚'
                     % (delta_at_first_reset, BOUND))
    else:
        print('C3 OK  reset 与收手同源（其前 %d 字节 ≤ %d）'
              % (delta_at_first_reset, BOUND))

    # C4 用户最终看到的内容不能是一屏复读。
    # 为什么看「最后一个 reset 之后的正文」而不是 done 帧：done 帧带的是
    # skill/asked 这类元信息，**根本没有正文**（写成「done 里没有复读」= 恒真断言，
    # 一开始就写错了一版）。真正交付到气泡里的是 delta 累计，而 reset 会清空气泡，
    # 所以「用户最终看到什么」= 最后一个 reset 之后的 delta 之和。
    c = tail_text.count(PHRASE)
    if c >= 50:
        fails.append('C4 reset 之后仍交付了重复 %d 遍的短语 —— 用户最终看到的还是一屏复读' % c)
    else:
        print('C4 OK  reset 之后交付的正文里短语只出现 %d 遍（< 50）' % c)

    # C5 断流后必须给用户一个明确终态，不能静默挂住。
    if terminal is None:
        fails.append('C5 整条流既没有 done 也没有 error —— 用户会看到气泡停在半路、没有任何提示')
    else:
        print('C5 OK  终态明确（%s 帧）' % terminal)

    # 诊断：服务端日志里的周期与收手字节（验证 hit() 报的数是否可信）
    try:
        out = subprocess.run(['journalctl', '-u', 'skillforge', '-n', '400', '--no-pager'],
                             capture_output=True, text=True, timeout=20).stdout
        hits = [l for l in out.splitlines() if '循环体约' in l or 'ErrLoopDetected' in l]
        print('\n--- 服务端日志（复读相关，最近 %d 条）---' % len(hits))
        for l in hits[-4:]:
            # 只留可读部分，避免把任何凭据带出来
            print('  ·', re.sub(r'\s+', ' ', l)[-220:])
        if not hits:
            print('  （没找到复读日志行 —— 若 C1/C2 都绿，说明收手发生在更早的路径，需人工核）')
    except Exception as e:
        print('\n（读日志失败：%s）' % str(e)[:80])

    print()
    if fails:
        for f in fails:
            print('FAIL', f)
        return 1
    if premise_miss:
        print('PREMISE_MISS: %s。看服务端日志确认；换个更极端的提示词重跑。'
              % premise_miss)
        return 3
    print('=== 全绿：复读被秒级掐断，气泡被 reset 帧清空，垃圾未交付 ===')
    return 0


if __name__ == '__main__':
    sys.exit(main())
