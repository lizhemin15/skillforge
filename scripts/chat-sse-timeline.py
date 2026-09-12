#!/usr/bin/env python3
"""量测 POST /api/chat 的 SSE 到达时间线。

判据（本次要修的问题）：
  - 首帧 trace 必须 < 1s 到达（修前是几十秒空白）；
  - 阻塞阶段每 ~3s 有 trace 心跳；
  - 首段正文 delta 之前就必须有 trace 帧。

用法: python3 sse_timeline.py <base_url> "<问题>" [--label 名字]
"""
import json
import sys
import time
import urllib.request

def run(base, question, label=""):
    url = base.rstrip("/") + "/api/chat"
    body = json.dumps({"session_id": "tl-%d" % int(time.time()), "message": question}).encode()
    req = urllib.request.Request(url, data=body, headers={"Content-Type": "application/json", "Accept": "text/event-stream"})
    t0 = time.time()
    marks = []          # (ms, ev, note)
    trace_frames = 0
    first_delta_ms = None
    steps_seen = 0
    buf = b""
    with urllib.request.urlopen(req, timeout=180) as resp:
        while True:
            chunk = resp.read(1)
            if not chunk:
                break
            buf += chunk
            while b"\n\n" in buf:
                raw, buf = buf.split(b"\n\n", 1)
                text = raw.decode("utf-8", "replace")
                ev, data = None, ""
                for line in text.splitlines():
                    if line.startswith("event: "):
                        ev = line[7:].strip()
                    elif line.startswith("data: "):
                        data += line[6:]
                ms = int((time.time() - t0) * 1000)
                if ev == "trace":
                    trace_frames += 1
                    try:
                        st = json.loads(data)
                        act = [s for s in st if s.get("status") == "active"]
                        note = (act[0].get("label", "") + " | " + act[0].get("detail", "")) if act else ("all done (%d steps)" % len(st))
                        if not act:
                            note = "all done (%d steps)" % len(st)
                    except Exception:
                        note = data[:60]
                    marks.append((ms, ev, note))
                elif ev == "delta":
                    if first_delta_ms is None:
                        first_delta_ms = ms
                    try:
                        t = json.loads(data).get("t", "")
                    except Exception:
                        t = data
                    if len(marks) < 200:
                        marks.append((ms, ev, t[:40].replace("\n", " ")))
                else:
                    marks.append((ms, ev, data[:70].replace("\n", " ")))
    total = int((time.time() - t0) * 1000)
    print("=" * 74)
    print("LABEL:", label, "| target:", url)
    print("question:", question)
    print("-" * 74)
    for ms, ev, note in marks[:40]:
        print("%7d ms  %-6s %s" % (ms, ev, note))
    if len(marks) > 40:
        print("... 共 %d 条事件" % len(marks))
    print("-" * 74)
    first_trace = next((m for m, e, _ in marks if e == "trace"), None)
    print("首帧 trace 到达: %s ms" % first_trace)
    print("trace 帧总数  : %d" % trace_frames)
    print("首段正文 delta: %s ms" % first_delta_ms)
    print("整轮耗时      : %d ms" % total)
    print("结论: 首帧<1000ms -> %s ; 首正文前有 trace -> %s" % (
        "PASS" if (first_trace is not None and first_trace < 1000) else "FAIL",
        "PASS" if (first_trace is not None and first_delta_ms is not None and first_trace <= first_delta_ms) else "FAIL",
    ))
    return first_trace, trace_frames, first_delta_ms, total

if __name__ == "__main__":
    base = sys.argv[1]
    q = sys.argv[2]
    label = sys.argv[4] if len(sys.argv) > 4 and sys.argv[3] == "--label" else ""
    run(base, q, label)
