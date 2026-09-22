#!/usr/bin/env python3
"""尺子自证：verify_loop_watchdog.py 到底能不能判？

为什么必须有这个脚本：线上实测「模型这轮只吐 3578 字节就自己停了」，看门狗
（旧二进制）压根没响 —— 拿这种跑法去"复验"，只能得到 PREMISE_MISS，永远
证明不了尺子能抓住真事故。而真事故（47KB 复读刷屏）是**随机撞上**的，
不能靠等它再发生一次。

所以用桩流把三种确定的形态喂给尺子，看它判得对不对：
  1. accident  复读刷到 47KB、没有 reset、正常 done  → 必须 FAIL(RC=1)，且 C1/C4 精确报红
  2. fixed     ~2KB 复读 → reset 清气泡 → 干净短答案 → error 帧（循环体约 N 字节）
                                                    → 必须全绿(RC=0)
  3. nothing   干净短答案、无 reset、无异常            → 必须 PREMISE_MISS(RC=3)，不许当绿

第 3 例是关键：一把「什么都判绿」的尺子和没有尺子一样，第 3 例就是那个
「本轮没发生任何事」的场景 —— 它必须自己承认量不到（RC=3），不许报绿。
桩流的帧格式照抄 internal/api/chat.go 的 write()：`event: X\\ndata: {json}\\n\\n`，
delta 带 t、reset 是空对象、error 带 error 字段。

用法: python3 scripts/test_loop_watchdog_ruler.py
退出码: 0 = 三例都符合预期；1 = 尺子本身有问题。
"""
import json
import os
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
RULER = os.path.join(HERE, 'verify_loop_watchdog.py')
PORT = int(os.environ.get('RULER_PORT', '8137'))
PHRASE = '循环测试'
FAILS = []


def sse(ev, obj):
    return ('event: %s\ndata: %s\n\n' % (ev, json.dumps(obj, ensure_ascii=False))).encode()


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

        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.send_header('Cache-Control', 'no-cache')
        self.end_headers()

        def w(b):
            self.wfile.write(b)
            self.wfile.flush()

        if mode == 'accident':
            # 复读刷屏：一路重复到 47KB（线上事故的形态），然后正常 done ——
            # 旧二进制就是这样把 47KB 垃圾当正常内容交下去的。
            blob = PHRASE * 3000          # 36000 字节
            w(sse('delta', {'t': blob}))
            w(sse('delta', {'t': PHRASE * 950}))
            w(sse('done', {'skill': ''}))
        elif mode == 'fixed':
            # 修后形态：先流一小段（会被清掉），看门狗触发 → reset 清气泡 →
            # 重试给出干净答案；两次都复读时以 error 帧收尾（明确告知用户）。
            w(sse('delta', {'t': PHRASE * 200}))
            w(sse('reset', {}))
            w(sse('delta', {'t': '重试后的干净答案：这段里没有重复短语。'}))
            w(sse('error', {'error': 'write-plain: 模型复读（循环体约 12 字节，'
                                     '已收 2412 字节时收手），按不完整丢弃'}))
        else:
            w(sse('delta', {'t': '这是一段正常短答案，没有复读。'}))
            w(sse('done', {'skill': ''}))


def run_case(name, mode, want_rc, want_substrings, forbid_substrings=()):
    env = dict(os.environ, BASE='http://127.0.0.1:%d' % PORT)
    p = subprocess.run([sys.executable, RULER, '--prompt', 'MODE=%s' % mode],
                       capture_output=True, text=True, timeout=120, env=env)
    out = p.stdout + p.stderr
    ok = True
    if p.returncode != want_rc:
        ok = False
        FAILS.append('%s: 退出码 %d，期望 %d' % (name, p.returncode, want_rc))
    for s in want_substrings:
        if s not in out:
            ok = False
            FAILS.append('%s: 输出里没有 %r（这条判据没生效）' % (name, s))
    for s in forbid_substrings:
        if s in out:
            ok = False
            FAILS.append('%s: 输出里出现了不该有的 %r' % (name, s))
    print('%s %s（RC=%d，期望 %d）' % ('ok  ' if ok else 'FAIL', name, p.returncode, want_rc))
    for line in out.splitlines():
        if line.startswith(('FAIL', 'PREMISE_MISS', 'C1', 'C4', 'C5', '=== ')):
            print('      |', line[:150])
    return ok


def main():
    srv = ThreadingHTTPServer(('127.0.0.1', PORT), Stub)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    time.sleep(0.3)
    print('=== 尺子自证（桩流，格式照抄 internal/api/chat.go）===')
    print('桩流端口: %d\n' % PORT)
    try:
        # 1) 真事故形态：必须红，且红在「收不住」和「最终还是一屏复读」上
        # 事故形态必须**多条判据同时红**（C1 收不住 / C2 看门狗没上线 / C4 最终交付还是复读）：
        # 只报一条红的报告等于缺证据，早退版本就是这么漏掉的。
        run_case('1. accident（47KB 复读、无 reset）', 'accident', 1,
                 ['FAIL C1', 'FAIL C2', 'FAIL C4'])
        # 2) 修后形态：必须绿（含 reset 清气泡、终态明确）
        run_case('2. fixed（reset 清气泡 + 干净重试）', 'fixed', 0,
                 ['C1 OK', 'C2 OK', 'C4 OK', 'C5 OK'],
                 forbid_substrings=['FAIL '])
        # 3) 什么都没发生：必须自认量不到，不许当绿
        run_case('3. nothing（干净短答案）', 'nothing', 3, ['PREMISE_MISS'])
    finally:
        srv.shutdown()

    print()
    if FAILS:
        for f in FAILS:
            print('FAIL', f)
        print('%d 项不符预期' % len(FAILS))
        return 1
    print('=== 三例全符预期：尺子能红、能绿、且拒绝假绿 ===')
    return 0


if __name__ == '__main__':
    sys.exit(main())
