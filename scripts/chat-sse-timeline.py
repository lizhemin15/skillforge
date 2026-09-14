#!/usr/bin/env python3
"""量测 POST /api/chat 的 SSE 到达时间线。

判据（两轮要修的问题）：
  - 首帧 trace 必须 < 1s 到达（修前是几十秒空白）；
  - 阻塞阶段每 ~3s 有 trace 心跳；
  - 首段正文 delta 之前就必须有 trace 帧。
  - **中间材料**：除了跳秒的计时，trace 帧里必须出现模型正在产出的内容
    （step.material）。用户原话：「现在速度过于慢了，中间可以流式输出思考的
    一些中间材料，现在一直卡着计时，用户体验不佳」——只有计时跳动不算进度。

材料挂在 `trace` 帧里 `status == "active"` 那一步的 `material` 字段上
（internal/agent.TraceStep.Material，json:"material,omitempty"）；只挂进行中那一步，
尾部 160 字、400ms 节流。**材料不会进顶部状态条**，所以必须单独判定。

最强的一条判据是 A4「最大静默」：把「正文 delta」和「材料帧」都算作「屏幕上有新东西」，
量出两次之间最长的一段空白。修前这段就是那 33s / 40s 的纯跳秒。

用法: python3 chat-sse-timeline.py <base_url> "<问题>" [--label 名字]
退出码: 0 = 全 PASS；1 = 有 FAIL（负向自证靠它，别改成恒 0）。
"""
import json
import sys
import time
import urllib.request

SILENT_BUDGET_MS = 5000  # 静默预算：超过这么久屏幕上没有一点新内容就是「卡着计时」


def run(base, question, label=""):
    url = base.rstrip("/") + "/api/chat"
    body = json.dumps({"session_id": "tl-%d" % int(time.time()), "message": question}).encode()
    req = urllib.request.Request(url, data=body, headers={"Content-Type": "application/json", "Accept": "text/event-stream"})
    t0 = time.time()
    marks = []          # (ms, ev, note)
    trace_frames = 0
    first_delta_ms = None
    mat_frames = 0
    first_mat_ms = None
    last_mat = ""
    mat_chars = 0
    content_ms = []     # 每一次「屏幕上有新内容」的时刻（正文 delta 或材料帧）
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
                    note = data[:60]
                    try:
                        st = json.loads(data)
                        steps_seen = max(steps_seen, len(st))
                        act = [s for s in st if s.get("status") == "active"]
                        if act:
                            note = act[0].get("label", "") + " | " + act[0].get("detail", "")
                            mat = act[0].get("material", "") or ""
                            if mat:
                                mat_frames += 1
                                mat_chars += len(mat)
                                if first_mat_ms is None:
                                    first_mat_ms = ms
                                last_mat = mat
                                content_ms.append(ms)
                                note = "②材料 %d 字 | %s" % (len(mat), mat[-36:].replace("\n", " "))
                        else:
                            note = "all done (%d steps)" % len(st)
                    except Exception:
                        pass
                    marks.append((ms, ev, note))
                elif ev == "delta":
                    if first_delta_ms is None:
                        first_delta_ms = ms
                    try:
                        t = json.loads(data).get("t", "")
                    except Exception:
                        t = data
                    if t:
                        content_ms.append(ms)
                    if len(marks) < 200:
                        marks.append((ms, ev, t[:40].replace("\n", " ")))
                else:
                    marks.append((ms, ev, data[:70].replace("\n", " ")))
    total = int((time.time() - t0) * 1000)

    # 最大静默：从 t0 起算，相邻两次「有新内容」之间的最长空白（到收尾也算一段）。
    gap_max, gap_at = 0, 0
    prev = 0
    for m in content_ms:
        if m - prev > gap_max:
            gap_max, gap_at = m - prev, prev
        prev = m
    if total - prev > gap_max:
        gap_max, gap_at = total - prev, prev

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
    print("首帧 trace 到达 : %s ms" % first_trace)
    print("trace 帧总数    : %d" % trace_frames)
    print("材料帧数/字数   : %d 帧 / %d 字" % (mat_frames, mat_chars))
    print("首个材料帧      : %s ms" % first_mat_ms)
    print("材料尾部        : %s" % last_mat[-60:].replace("\n", " "))
    print("首段正文 delta  : %s ms" % first_delta_ms)
    print("最大静默        : %d ms (起于 %d ms)" % (gap_max, gap_at))
    print("整轮耗时        : %d ms" % total)

    c1 = first_trace is not None and first_trace < 1000
    c2 = first_trace is not None and first_delta_ms is not None and first_trace <= first_delta_ms
    c3 = first_mat_ms is not None and (first_delta_ms is None or first_mat_ms < first_delta_ms)
    c4 = gap_max < SILENT_BUDGET_MS
    print("结论:")
    print("  A1 首帧<1000ms                 -> %s" % ("PASS" if c1 else "FAIL"))
    print("  A2 首正文前有 trace            -> %s" % ("PASS" if c2 else "FAIL"))
    print("  A3 首正文前有中间材料（非跳秒）-> %s" % ("PASS" if c3 else "FAIL"))
    print("  A4 最大静默<%dms             -> %s" % (SILENT_BUDGET_MS, "PASS" if c4 else "FAIL"))
    ok = c1 and c2 and c3 and c4
    print("  => %s" % ("ALL PASS" if ok else "HAS FAIL"))
    return ok, dict(first_trace=first_trace, trace_frames=trace_frames, mat_frames=mat_frames,
                    first_mat_ms=first_mat_ms, first_delta_ms=first_delta_ms,
                    gap_max=gap_max, total=total)


if __name__ == "__main__":
    base = sys.argv[1]
    q = sys.argv[2]
    label = sys.argv[4] if len(sys.argv) > 4 and sys.argv[3] == "--label" else ""
    ok, _ = run(base, q, label)
    sys.exit(0 if ok else 1)
