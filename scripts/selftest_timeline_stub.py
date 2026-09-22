#!/usr/bin/env python3
"""桩流自证：用**假服务端**喂 3 种形状的 SSE，验证 chat-sse-timeline.py 这把尺子
能红、能绿、且不会假绿。

为什么要桩流：线上跑一轮要 20~27 分钟，尺子自己有没有毛病根本测不动；
更糟的是「尺子恒绿」和「产品真好」在线上看来一模一样。桩流让三种形状在几秒内复现，
把「尺子能不能红」和「产品好不好」彻底分开。

三个桩（形状取自线上真实故障，不是编的）：
  S1 事故形状：needs 帧 + 74 字追问 + done.asked=true   → 期望 rc=1、A0 FAIL、
                真因写明「被缺参闸门拦下」、A1~A6 全部「不评」（不许跟着红）
  S2 瘦稿形状：有 trace 有材料，但正文只有 168 字、done.asked=false → 期望 rc=1、
                A0 FAIL 写「正文只有 168 字」
  S3 健康形状：trace + 材料 + 1200 字正文流式 → 期望 rc=0、ALL PASS

判据（缺一即自证失败）：
  C1 三个桩的 rc 全部命中期望（**S1/S2 必须是 1，不是 2**：PREMISE_MISS 会把
     「产品缺陷」说成「这次没量到」，那正是这把尺子上一版的毛病）
  C2 S1 的真因必须点名缺参闸门，且 A1~A6 一行 FAIL 都不许有（只有「不评」）
  C3 S2 的真因必须报出实际字数
  C4 S3 必须 ALL PASS（尺子能绿，而不是坏在恒红）
"""
import http.server
import json
import os
import pathlib
import re
import subprocess
import sys
import threading
import time

REPO = pathlib.Path(__file__).resolve().parent.parent
RULER = REPO / "scripts" / "chat-sse-timeline.py"

CLS_ACTIVE = [{"phase": "classify", "label": "① 意图分析", "status": "active", "detail": "判断意图"}]


def healthy_script():
    """S3：健康形状 —— 首帧快、材料先于正文、正文持续吐。"""
    steps = [(0.05, "trace", CLS_ACTIVE)]
    # 材料先于正文出现（A3 的前提），且间隔 <1s（避免把 A4/A5 打成「静默」）
    for i, d in enumerate((1.20, 1.60, 2.00)):
        mats = "".join(["素材要点%d：%s。" % (j + 1, "内容" * 6) for j in range(i + 1)])
        steps.append((d, "trace", [
            {"phase": "classify", "label": "① 意图分析", "status": "done", "detail": "已判明"},
            {"phase": "write", "label": "② 执笔", "status": "active", "detail": "按素材起草", "material": mats},
        ]))
    for i in range(20):  # 20 段 × ~60 字 ≈ 1200 字正文，每 150ms 一段
        steps.append((2.10 + i * 0.15, "delta", {"t":
            "这是按你给的素材写出来的第%d段正文，把来龙去脉讲清楚，并且交代清楚时间、地点、"
            "参与方与结果，让读者不需要额外背景也能看懂。" % (i + 1)}))
    steps.append((4.05, "done", {"skill": "stub-healthy"}))
    return steps


def accident_script():
    """S1：线上事故形状（2026-09-22）—— 整轮 4.5s，只有一句缺参追问。"""
    return [
        (0.05, "trace", CLS_ACTIVE),
        (0.20, "trace", [{"phase": "classify", "label": "① 意图分析", "status": "active", "detail": "判断意图"},
                         {"phase": "write", "label": "② 起草", "status": "pending", "detail": "等待补充"}],
         ),
        (4.30, "needs", [{"name": "headline_highlight", "label": "标题亮点（可选，未提供则生成）",
                          "type": "text", "required": False}]),
        (4.40, "delta", {"t": "我需要先确认以下信息：标题亮点（可选，未提供则生成）。"}),
        (4.51, "done", {"skill": "stub-accident", "asked": "true"}),
    ]


def thin_script():
    """S2：瘦稿形状 —— 流程全对，但正文只有 168 字就收手（关思考链版本最怕这个）。"""
    body = "这是一段被提前收手的正文，用来验证尺子量得出「字不够」。" * 6  # 168 字
    return [
        (0.05, "trace", CLS_ACTIVE),
        (0.60, "trace", [{"phase": "classify", "label": "① 意图分析", "status": "done", "detail": "已判明"},
                         {"phase": "write", "label": "② 执笔", "status": "active", "detail": "按素材起草",
                          "material": "素材要点：一片被截断的稿子。"}],),
        (0.70, "delta", {"t": body}),
        (1.20, "done", {"skill": "stub-thin"}),
    ]


def start_stub(script):
    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *a):  # 静音：桩的访问日志会淹没判据输出
            pass

        def do_POST(self):
            n = int(self.headers.get("Content-Length") or 0)
            self.rfile.read(n)
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Connection", "close")
            self.end_headers()
            t0 = time.time()
            for delay, ev, payload in script:
                wait = t0 + delay - time.time()
                if wait > 0:
                    time.sleep(wait)
                data = payload if isinstance(payload, str) else json.dumps(payload, ensure_ascii=False)
                try:
                    self.wfile.write(("event: %s\ndata: %s\n\n" % (ev, data)).encode("utf-8"))
                    self.wfile.flush()
                except OSError:
                    return  # 尺子提前断开（RC=1 后也一样）：桩不该因此炸
            self.close_connection = True

    srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv, srv.server_address[1]


def probe(script):
    srv, port = start_stub(script)
    try:
        p = subprocess.run([sys.executable, str(RULER), "http://127.0.0.1:%d" % port, "写一篇新闻稿"],
                           cwd=REPO, capture_output=True, text=True, timeout=120,
                           env=dict(os.environ, SF_TIMEOUT="30"))
    finally:
        srv.shutdown()
    return p.returncode, p.stdout + p.stderr


fails = []


def check(cond, msg):
    print(("  OK   " if cond else "  FAIL ") + msg)
    if not cond:
        fails.append(msg)


print("S1 事故形状（needs 帧 + 追问 + asked=true）")
rc, out = probe(accident_script())
print("      rc=%d" % rc)
for ln in out.splitlines():
    if ln.startswith("本轮交付") or ln.strip().startswith("A0") or "不评" in ln or "=>" in ln:
        print("      |", ln.strip()[:120])
check(rc == 1, "S1 rc=1（不是 2 —— PREMISE_MISS 会把产品缺陷说成没量到）")
check("A0 本轮真写了正文              -> FAIL" in out, "S1 A0 FAIL")
check("被缺参闸门拦下" in out, "S1 真因点名缺参闸门")
check(re.search(r"needs 帧 1 条", out) is not None, "S1 报出 needs 帧条数")
check(out.count("不评（A0 已红") == 6, "S1 A1~A6 六条全部「不评」（不跟着红）")
check("-> FAIL" in out and out.count("-> FAIL") == 1, "S1 全篇只有 A0 一行 FAIL")

print("S2 瘦稿形状（正文 168 字）")
rc, out = probe(thin_script())
print("      rc=%d" % rc)
for ln in out.splitlines():
    if ln.startswith("本轮交付") or ln.strip().startswith("A0"):
        print("      |", ln.strip()[:120])
check(rc == 1, "S2 rc=1")
check("正文只有 168 字" in out, "S2 真因报出实际字数（168）")

print("S3 健康形状（trace + 材料 + 1200 字正文）")
rc, out = probe(healthy_script())
print("      rc=%d" % rc)
for ln in out.splitlines():
    if ln.strip().startswith(("A0", "A1", "A5", "=>", "本轮交付")):
        print("      |", ln.strip()[:120])
check(rc == 0, "S3 rc=0（尺子能绿）")
check("ALL PASS" in out, "S3 ALL PASS")
check("A0 本轮真写了正文              -> PASS" in out, "S3 A0 PASS（量的是真正文，不是追问话术）")

if fails:
    print("STUB_SELFTEST_RC=1 桩流自证失败 %d 条：" % len(fails))
    for f in fails:
        print("  -", f)
    sys.exit(1)
print("STUB_SELFTEST_RC=0 三个桩全部命中期望（能红 / 能绿 / 不假绿）")
