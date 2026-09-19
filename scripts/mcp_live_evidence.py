#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""抓「对话真调 MCP」的原始证据：tool_call / tool_result 轨迹 + 正文片段。
终验脚本 mcp_live_verify.py 只判真假（工具名、真 SQL 痕迹），这里留人可读的原始料。"""
import json, re, sys, time, urllib.request

BASE = "http://127.0.0.1:8092"
sess = "mcp-evidence-%d" % int(time.time())
# 故意只要 1~2 次工具调用：要的是「轨迹证据」，不是压力测试。
# 之前让它「逐个表查行数」会触发十几次工具调用，跑十几分钟（用户明确嫌慢）。
msg = ("用已接入的 DataToolbox MCP 工具列出数据库里的表名，一次调用拿到即可。"
       "必须真调 MCP 工具，不要用 http_request，不要编。")
body = json.dumps({"session_id": sess, "message": msg}).encode()
req = urllib.request.Request(BASE + "/api/chat", data=body,
                             headers={"Content-Type": "application/json"}, method="POST")
traces, deltas, names = [], [], []
t0 = time.time()
t_first_trace = t_first_delta = t_last = None
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
            now = time.time() - t0
            t_last = now
            if ev == "trace":
                if t_first_trace is None:
                    t_first_trace = now
                try: traces.append(json.loads(d))
                except Exception: pass
            elif ev == "delta":
                if t_first_delta is None:
                    t_first_delta = now
                # ⚠️ 正文帧的键是 "t"（见 internal/api/chat.go 的 delta 帧），
                # 早先这里写成 "text" → 正文永远 0 字、判据「正文有表名」永远假阴性。
                # 尺子坏比没尺子更糟：0 字会被当成「模型没答」。
                try:
                    ev_obj = json.loads(d)
                    deltas.append(ev_obj.get("t") or ev_obj.get("text")
                                  or ev_obj.get("delta") or ev_obj.get("content") or "")
                except Exception:
                    deltas.append(d[:80])

ans = "".join(deltas)
alltrace = json.dumps(traces, ensure_ascii=False)
# 「卡计时器」是用户明确投诉过的体验问题：所以首材料时刻要打出来，别只打总耗时。
print("总耗时 %.1fs  事件类型: %s" % (t_last or 0, ",".join(names)))
print("首块中间材料 %.2fs   首段正文 %.2fs   （空转越短越好）"
      % (t_first_trace if t_first_trace is not None else -1,
         t_first_delta if t_first_delta is not None else -1))
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
print("正文有表名:", sorted(set(re.findall(r"\b(?!mcp_)[A-Za-z]\w*_[A-Za-z0-9_]+\b", ans)))[:6] or "（无）")
print("裸调 http_request:", "http_request" in alltrace + ans)
# 尺子自证：有 delta 帧却一个字都没解出来 = 解析键写错（历史 bug），
# 这种情况直接非零退出（rc=2），别把「尺子坏」伪装成「模型没答」。
if "delta" in names and not ans:
    print("!! 尺子坏：收到 delta 帧但正文解析为 0 字，检查帧字段名")
    sys.exit(2)
sys.exit(0 if names_hit else 1)
