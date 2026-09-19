#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""毫秒级帧到达时刻探针：回答「3s 心跳在阻塞阶段到底有没有到客户端」。
用 readline() 逐行读原始套接字（不经缓冲整块），确保测的是到达时刻而不是解包时刻。"""
import http.client, json, sys, time

msg = sys.argv[1] if len(sys.argv) > 1 else "写一篇关于数据要素产业协同推进会的新闻稿，400 字左右。"
conn = http.client.HTTPConnection("127.0.0.1", 8092, timeout=300)
body = json.dumps({"session_id": "beat-probe-%d" % time.time(), "message": msg})
conn.request("POST", "/api/chat", body, {"Content-Type": "application/json"})
r = conn.getresponse()
t0 = time.time()
ev = None
prev = None
gaps = []
while True:
    line = r.readline()
    if not line:
        break
    s = line.decode("utf-8", "replace").rstrip("\n")
    if s.startswith("event: "):
        ev = s[7:].strip()
    elif s.startswith("data: "):
        now = time.time() - t0
        if prev is not None:
            gaps.append((prev, now, ev))
        prev = now
        d = s[6:]
        label = ""
        try:
            j = json.loads(d)
            if isinstance(j, list):
                act = [x for x in j if x.get("status") == "active"]
                label = (act[-1].get("label", "") + "｜" + act[-1].get("detail", "")[:34]
                         + "｜材料:" + act[-1].get("material", "")[-18:]) if act else "(无 active)"
            else:
                label = (j.get("detail") or j.get("t") or j.get("url") or "")[:60]
        except Exception:
            label = d[:60]
        print("[%6.2fs] %-6s %s" % (now, ev, label), flush=True)
print("\n最大静默:")
for a, b, e in sorted(gaps, key=lambda x: x[1] - x[0], reverse=True)[:4]:
    print("  %5.2fs  %.2fs → %.2fs  (%s)" % (b - a, a, b, e))
print("总时长 %.2fs  帧数 %d" % (prev or 0, len(gaps) + 1))
