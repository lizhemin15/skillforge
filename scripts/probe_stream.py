#!/usr/bin/env python3
"""逐帧打印 /api/chat 的到达时间（**不去重**），用来分清三种「卡」：

  ① 心跳还在、只是没有新材料  → 屏幕上是个跳秒的计时（用户说的「一直卡着计时」）；
  ② 连心跳都停了              → 服务端整条 SSE 卡死；
  ③ 有 delta 在流但很慢        → 模型本身慢，只是没被看见。

为什么不能复用 measure_chat_latency.py：那个脚本为了出「变化时间线」把心跳帧
按 (label, detail, material) 去重了，正好把「心跳有没有断」这个最关键的证据
一起删掉。取证要什么就留什么，别图省事。

用法：PROMPT='...' SID=probe-1 MAXW=240 python3 scripts/probe_stream.py
"""
import json
import os
import sys
import time
import urllib.request

BASE = os.environ.get("BASE", "http://127.0.0.1:8092")
MAXW = float(os.environ.get("MAXW", "240"))
PROMPT = os.environ["PROMPT"]
SID = os.environ.get("SID", "probe-%d" % int(time.time()))
LABEL = os.environ.get("LABEL", "probe")

req = urllib.request.Request(BASE + "/api/login", method="POST", data=json.dumps({
    "username": os.environ.get("SKILLFORGE_ADMIN_USER", ""),
    "password": os.environ.get("SKILLFORGE_ADMIN_PASS", ""),
}).encode())
req.add_header("Content-Type", "application/json")
tok = json.loads(urllib.request.urlopen(req, timeout=30).read())["token"]

body = json.dumps({"session_id": SID, "message": PROMPT, "mode": "auto", "skill": ""}).encode()
req = urllib.request.Request(BASE + "/api/chat", method="POST", data=body)
req.add_header("Content-Type", "application/json")
req.add_header("Authorization", "Bearer " + tok)

t0 = time.time()
print(f"===== {LABEL}｜sid={SID}｜prompt={PROMPT[:60]}…", flush=True)
n_ev = n_delta = n_mat = n_hb = 0
heartbeat_gap = 0.0
last_arrive = t0
try:
    with urllib.request.urlopen(req, timeout=MAXW) as r:
        buf = b""
        while True:
            chunk = r.read1(4096) if hasattr(r, "read1") else r.read(4096)
            if not chunk:
                break
            now = time.time()
            gap = now - last_arrive
            last_arrive = now
            buf += chunk
            while b"\n\n" in buf:
                raw, buf = buf.split(b"\n\n", 1)
                ev_name, payload = "", ""
                for ln in raw.decode("utf-8", "ignore").split("\n"):
                    ln = ln.strip()
                    if ln.startswith("event:"):
                        ev_name = ln[6:].strip()
                    elif ln.startswith("data:"):
                        payload += ln[5:].strip()
                if not payload or payload == "[DONE]":
                    continue
                n_ev += 1
                try:
                    ev = json.loads(payload)
                except Exception:
                    print(f"[{now-t0:6.1f}s] (非 JSON 载荷，{len(payload)} 字节)", flush=True)
                    continue
                if isinstance(ev, list):
                    st = ev[0] if ev else {}
                    mat = st.get("material") or ""
                    if mat:
                        n_mat += 1
                        print(f"[{now-t0:6.1f}s] 🧱素材(+{len(mat)}字，间隔{gap:5.1f}s)：{mat[-70:]}", flush=True)
                    else:
                        n_hb += 1
                        heartbeat_gap = max(heartbeat_gap, gap)
                        print(f"[{now-t0:6.1f}s] 💓心跳(间隔{gap:5.1f}s)：{st.get('detail','')[:50]}", flush=True)
                    continue
                k = ev.get("type") or ev.get("kind") or ev_name
                txt = ev.get("t") or ev.get("text") or ""
                if k == "delta" and txt:
                    n_delta += 1
                    print(f"[{now-t0:6.1f}s] ✍️正文片(+{len(txt)}字)：{txt[:50]!r}", flush=True)
                else:
                    print(f"[{now-t0:6.1f}s] 📦{k}：{json.dumps(ev, ensure_ascii=False)[:130]}", flush=True)
except Exception as e:
    print(f"!! 异常（{time.time()-t0:.1f}s）：{type(e).__name__} {str(e)[:200]}", flush=True)
print(f"===== 小结 {LABEL}：总 {time.time()-t0:.1f}s｜帧 {n_ev}（心跳 {n_hb} / 素材 {n_mat} / 正文片 {n_delta}）"
      f"｜最长帧间隔 {heartbeat_gap:.1f}s", flush=True)
