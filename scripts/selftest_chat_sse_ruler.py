#!/usr/bin/env python3
"""尺子自证：chat-sse-timeline.py 到底能不能判「只在跳秒、没有中间材料」？

为什么必须有这个脚本：用户投诉的那一轮（「一直卡着计时」）是随机撞上的 —— 等它
再发生一次来"复验"，等于永远验不了。而线上真跑一轮要 20~27 分钟、还要真人提供
1 万字素材。所以用桩流把**确定的**形态喂给尺子，看它判得对不对：

  1. healthy   trace 首帧即到 + 边想边放材料 + 材料早于首正文 + 步骤 1 只占 ~2s
                                       → 必须全绿(RC=0)，一列 FAIL 都不许有
  2. ticking   心跳每秒在跳、**一个字材料都没有**，13s 后才吐正文（事故原形）
               → A3/A4/A5 必须红(RC=1)，但 A1/A2 仍绿 ——
                 证明红是红在「没材料」，不是红在「心跳管道断了」
  3. precise   正文一直在流、心跳也一直在跳，**唯独没有材料**
               → 只有 A3 一条红。这是「精确转红」：A3 之外的判据不许跟着红，
                 否则一有波动就一片红，等于没判据
  4. nothing   一帧 trace 都没来（只有 delta）
               → 必须 PREMISE_MISS(RC=2)。一把「什么都判绿」或「什么都判红」的
                 尺子都等于没有尺子，这一例就是那个「本轮根本没跑起来」的场景
  5. notsse    HTTP 200 但 Content-Type 是 JSON（内嵌页/网关把流换成 JSON 都长这样）
               → 必须 PREMISE_MISS(RC=2)：拿到的不是 SSE，A1~A6 全在量空气，
                 报成「材料坏了」是**假红**，会把人引去修错的地方
  6. http500   HTTP 500 + JSON（服务端自己炸了）
               → 同样必须 PREMISE_MISS(RC=2)，但走的不是 ctype 守卫、而是 urlopen
                 抛异常那条分支。两条分支都要挡住，否则尺子只在某一种坏法下才认得出来

桩流的帧格式照抄 internal/api/chat.go：`event: X\\ndata: {json}\\n\\n`；
trace 的 data 是步骤数组（status=="active" 那一步挂 material），delta 带 t。

用法: python3 scripts/selftest_chat_sse_ruler.py
退出码: 0 = 五例都符合预期；1 = 尺子本身有问题（判错档 / 退出码不符 / 判据串档）。
"""
import json
import os
import re
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
RULER = os.path.join(HERE, 'chat-sse-timeline.py')
PORT = int(os.environ.get('RULER_PORT', '8138'))
FAILS = []
# 尺子里的静默预算（SILENT_BUDGET_MS=12000）——2 号例必须真的跨过它，
# 否则「没材料」这档会被静默判据放过，自证就成了空跑。
SILENCE = 13.0


def sse(ev, obj):
    return ('event: %s\ndata: %s\n\n' % (ev, json.dumps(obj, ensure_ascii=False))).encode()


def step(phase, label, status, detail='', material=None):
    s = {'phase': phase, 'label': label, 'status': status, 'detail': detail}
    if material is not None:
        s['material'] = material
    return s


class Stub(BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, *a):
        pass

    def do_POST(self):
        n = int(self.headers.get('Content-Length') or 0)
        body = json.loads(self.rfile.read(n) or b'{}')
        mode = 'nothing'
        for tok in str(body.get('message', '')).split():
            if tok.startswith('MODE='):
                mode = tok.split('=', 1)[1]

        if mode in ('notsse', 'http500'):
            # 两种坏法各有各的分支：200+JSON 走 ctype 守卫，500 走 urlopen 抛异常。
            # 少测一种，尺子就会在那种坏法下把「量空气」报成「材料坏了」。
            is_200 = (mode == 'notsse')
            b = json.dumps({'error': 'bad content-type' if is_200 else 'boom'}).encode()
            self.send_response(200 if is_200 else 500)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(b)))
            self.send_header('Connection', 'close')
            self.end_headers()
            self.wfile.write(b)
            self.close_connection = True
            return

        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.send_header('Cache-Control', 'no-cache')
        # 必须显式关连接：尺子是读到 EOF 才收工的，HTTP/1.1 keep-alive 会让它一直等。
        self.send_header('Connection', 'close')
        self.end_headers()
        self.close_connection = True

        def w(b):
            self.wfile.write(b)
            self.wfile.flush()

        s1 = step('intent', '① 意图分析', 'active', '判断本轮要做什么')
        s1d = step('intent', '① 意图分析', 'done', '写作需求：新闻通稿')
        s2 = step('write', '② 执笔', 'active', '按素材写稿')

        if mode == 'healthy':
            w(sse('trace', [s1]))
            for i in range(5):                      # 边想边放材料（首正文之前）
                time.sleep(0.35)
                w(sse('trace', [step('intent', '① 意图分析', 'active', '分析素材',
                                     material='思考片段%d：先确认体裁与读者，用素材里的%s' % (i, '事实' * 8))]))
            w(sse('trace', [s1d, step('write', '② 执笔', 'active', '落笔', material='拟稿：导语→主体→结尾')]))
            for i in range(10):                     # 正文
                time.sleep(0.25)
                w(sse('delta', {'t': '正文第%d段，正常流式输出。' % i}))
            w(sse('trace', [s1d, step('write', '② 执笔', 'done', '完成')]))
            w(sse('done', {'skill': '公司新闻通稿'}))

        elif mode == 'ticking':
            # 事故原形：计时器一直在跳（trace 心跳每 0.4s 一帧、active 步骤在换），
            # 但 material 一个字都没有；直到 13s 后才开始吐正文。
            t0 = time.time()
            i = 0
            while time.time() - t0 < SILENCE:
                i += 1
                cur = s1 if (time.time() - t0) < 3.0 else s2   # 3s 换步，让 A6 绿
                w(sse('trace', [cur]))
                time.sleep(0.4)
            w(sse('trace', [s1d, step('write', '② 执笔', 'active', '落笔')]))
            for k in range(6):
                time.sleep(0.25)
                w(sse('delta', {'t': '迟到的正文第%d段。' % k}))
            w(sse('done', {'skill': ''}))
            print('      (桩流跳了 %d 次心跳、全程无材料)' % i, file=sys.stderr)

        elif mode == 'precise':
            # 正文一直在流、心跳一直在跳，唯独没有材料 → 只该 A3 红。
            w(sse('trace', [s1]))
            for k in range(14):
                time.sleep(0.3)
                w(sse('delta', {'t': '正文第%d段。' % k}))
                if k == 7:
                    w(sse('trace', [s1d, step('write', '② 执笔', 'active', '落笔')]))
            w(sse('done', {'skill': ''}))

        else:  # nothing
            w(sse('delta', {'t': '只有正文，一帧 trace 都没有。'}))
            w(sse('done', {'skill': ''}))


def verdicts(out):
    """把 A1~A6 的判定抽成 {编号: PASS/FAIL}，不靠空格对齐做匹配（对齐一改就假红）。"""
    n = re.sub(r'\s+', ' ', out)
    # 判据名里带 '>' 的（如 A5 写着「占比>=50%」）不能靠 [^>] 卡字符 —— 那样会把
    # 整条 A5 漏掉，字典里少一项就会被读成「判据不符」。锚在 '-> ' 上才对。
    return {m.group(1): m.group(2) for m in re.finditer(r'A(\d) .*?-> (PASS|FAIL)', n)}


ALL_PASS = {str(i): 'PASS' for i in range(1, 7)}


def run_case(name, mode, want_rc, want_sub=(), forbid_sub=(), want_verdicts=None):
    env = dict(os.environ, BASE='http://127.0.0.1:%d' % PORT)
    p = subprocess.run([sys.executable, RULER, 'http://127.0.0.1:%d' % PORT, 'MODE=%s' % mode,
                        '--label', name],
                       capture_output=True, text=True, timeout=180, env=env)
    out = p.stdout + p.stderr
    ok = True
    if p.returncode != want_rc:
        ok = False
        FAILS.append('%s: 退出码 %d，期望 %d' % (name, p.returncode, want_rc))
    for s in want_sub:
        if s not in out:
            ok = False
            FAILS.append('%s: 输出里没有 %r' % (name, s))
    for s in forbid_sub:
        if s in out:
            ok = False
            FAILS.append('%s: 输出里出现了不该有的 %r' % (name, s))
    if want_verdicts is not None:
        got = verdicts(out)
        if got != want_verdicts:
            ok = False
            FAILS.append('%s: 判据结果 %s，期望 %s' % (name, got, want_verdicts))
    print('%s %s（RC=%d，期望 %d）判据=%s' % ('ok  ' if ok else 'FAIL', name, p.returncode, want_rc, verdicts(out)))
    for line in out.splitlines():
        if re.match(r'\s*(A\d |  =>|PREMISE_MISS)', line):
            print('      |', line.strip()[:150])
    return ok


def main():
    srv = ThreadingHTTPServer(('127.0.0.1', PORT), Stub)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    time.sleep(0.3)
    print('=== 尺子自证（桩流，格式照抄 internal/api/chat.go）===')
    print('桩流端口: %d ，静默档 %.0fs\n' % (PORT, SILENCE))
    try:
        # 1) 健康轮：不许有任何 FAIL（含 A3、A6）
        run_case('1. healthy（有材料 + 材料早于正文 + 步骤1 只占 ~2s）', 'healthy', 0,
                 want_sub=['ALL PASS'], forbid_sub=['FAIL'], want_verdicts=ALL_PASS)
        # 2) 事故原形：红必须红在「没材料」上，心跳管道本身（A1/A2）仍是绿的 ——
        #    若这例把 A1/A2 也判红，说明尺子根本分不清「没材料」和「心跳断了」。
        run_case('2. ticking（只跳秒 13s，无材料）', 'ticking', 1,
                 want_verdicts={'1': 'PASS', '2': 'PASS', '3': 'FAIL', '4': 'FAIL', '5': 'FAIL', '6': 'PASS'})
        # 3) 精确转红：只有 A3 一条红
        run_case('3. precise（正文在流、心跳在跳、唯独没材料）', 'precise', 1,
                 want_verdicts={'1': 'PASS', '2': 'PASS', '3': 'FAIL', '4': 'PASS', '5': 'PASS', '6': 'PASS'})
        # 4) 本轮什么都没发生：必须自认量不到，不许报绿
        run_case('4. nothing（零 trace 帧）', 'nothing', 2, want_sub=['PREMISE_MISS'])
        # 5) 拿到的不是 SSE（200 + JSON）：走 ctype 守卫
        run_case('5. notsse（HTTP 200 + JSON 体）', 'notsse', 2,
                 want_sub=['PREMISE_MISS', '拿到的不是 SSE'])
        # 6) 服务端 500：走 urlopen 抛异常那条分支
        run_case('6. http500（HTTP 500 + JSON）', 'http500', 2,
                 want_sub=['PREMISE_MISS', 'HTTP Error 500'])
    finally:
        srv.shutdown()

    print()
    if FAILS:
        for f in FAILS:
            print('FAIL', f)
        print('%d 项不符预期' % len(FAILS))
        return 1
    print('=== 六例全符预期：尺子能绿、能红、能精确转红、且对「没跑起来/不是 SSE」拒绝报绿 ===')
    return 0


if __name__ == '__main__':
    sys.exit(main())
