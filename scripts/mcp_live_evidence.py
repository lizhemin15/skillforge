#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""抓「对话真调 MCP」的原始证据：tool_call / tool_result 轨迹 + 正文片段。
终验脚本 mcp_live_verify.py 只判真假（工具名、真 SQL 痕迹），这里留人可读的原始料。"""
import json, re, sys, time, urllib.request

BASE = "http://127.0.0.1:8092"
sess = "mcp-evidence-%d" % int(time.time())
msg = ("用已接入的 DataToolbox MCP 工具查：数据库里有哪些表、各有多少行。"
       "必须真调 MCP 工具，不要用 http_request，不要编。")
body = json.dumps({"session_id": sess, "message": msg}).encode()
req = urllib.request.Request(BASE + "/api/chat", data=body,
                             headers={"Content-Type": "application/json"}, method="POST")
traces, deltas, names = [], [], []
t0 = time.time()
with urllib.request.urlopen(req, timeout=1200) as r:
    ev = None
    for raw in r:
        line = raw.decode("utf-8", "replace").rstrip("\n")
        if line.startswith("event: "):
            ev = line[7:].strip()
            if ev not in names:
                names.append(ev)
        elif line.startswith("data: "):
            d = line[6:]
            if ev == "trace":
                try: traces.append(json.loads(d))
                except Exception: pass
            elif ev == "delta":
                try: deltas.append(json.loads(d).get("text", ""))
                except Exception: deltas.append(d[:80])

ans = "".join(deltas)
alltrace = json.dumps(traces, ensure_ascii=False)
print("耗时 %.1fs  事件类型: %s" % (time.time() - t0, ",".join(names)))
print("trace 条数: %d  正文 %d 字" % (len(traces), len(ans)))
print("\n===== 工具轨迹 =====")
for t in traces:
    s = json.dumps(t, ensure_ascii=False)
    if "mcp" in s.lower() or "tool" in s.lower():
        print("-- " + s[:600])
print("\n===== 正文片段 =====")
print(ans[:900])
print("\n===== 判据 =====")
names_hit = sorted(set(re.findall(r"mcp_datatoolbox_\w+", alltrace + ans)))
print("真实 MCP 工具名:", names_hit or "（无）")
print("正文有表名:", sorted(set(re.findall(r"\bt_\w+", ans)))[:6] or "（无）")
print("裸调 http_request:", "http_request" in alltrace + ans)
sys.exit(0 if names_hit else 1)
