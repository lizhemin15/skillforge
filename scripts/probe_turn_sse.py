#!/usr/bin/env python3
"""对同一个会话重发某一轮，把 SSE **原始帧**逐条落盘。

为什么需要它：verify_live_material.py 的 round_stream 只数帧类型
（`kinds[k] = kinds.get(k,0)+1`），**把 error 帧的正文扔了** —— 于是线上
第2轮（整理成 Word）失败时，尺子只会喊「❌ A0 没有产出链接」，说不出为什么。
只会喊「红了」的尺子等于半个尺子。

用法：
  SID=liveverify-1789850050 MSG='把上面这篇新闻稿原样整理成 Word 文档（.docx），正文一字不改。' \
    python3 scripts/probe_turn_sse.py
输出：/tmp/probe_turn.sse（原始帧）+ stdout 上的 error/关键帧摘要
"""
import json
import os
import re
import sys
import time
import urllib.request

BASE = os.environ.get("BASE", "http://127.0.0.1:8092")
U = os.environ.get("SKILLFORGE_ADMIN_USER", "")
P = os.environ.get("SKILLFORGE_ADMIN_PASS", "")
SID = os.environ["SID"]
MSG = os.environ["MSG"]
MAXW = float(os.environ.get("MAXW", "900"))
OUT = os.environ.get("OUT", "/tmp/probe_turn.sse")


def login():
    body = json.dumps({"username": U, "password": P}).encode()
    req = urllib.request.Request(BASE + "/api/login", method="POST", data=body)
    req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.loads(r.read().decode())["token"]


def main():
    tok = login()
    body = json.dumps({"session_id": SID, "message": MSG, "mode": "auto",
                       "skill": ""}).encode()
    req = urllib.request.Request(BASE + "/api/chat", method="POST", data=body)
    req.add_header("Content-Type", "application/json")
    req.add_header("Authorization", "Bearer " + tok)
    t0 = time.time()
    kinds, errors, files, deltas = {}, [], [], []
    with open(OUT, "w", encoding="utf-8") as fp, urllib.request.urlopen(req, timeout=MAXW) as r:
        buf = b""
        while True:
            chunk = r.read1(4096) if hasattr(r, "read1") else r.read(4096)
            if not chunk:
                break
            buf += chunk
            while b"\n\n" in buf:
                raw, buf = buf.split(b"\n\n", 1)
                txt = raw.decode("utf-8", "ignore")
                now = time.time() - t0
                fp.write(f"--- t={now:.2f}s ---\n{txt}\n\n")
                fp.flush()
                ev, payload = "", ""
                for ln in txt.split("\n"):
                    ln = ln.strip()
                    if ln.startswith("event:"):
                        ev = ln[6:].strip()
                    elif ln.startswith("data:"):
                        payload += ln[5:].strip()
                if not payload or payload == "[DONE]":
                    continue
                try:
                    obj = json.loads(payload)
                except Exception:
                    continue
                for st in (obj if isinstance(obj, list) else [obj]):
                    if not isinstance(st, dict):
                        continue
                    k = st.get("kind") or st.get("type") or ev or "?"
                    kinds[k] = kinds.get(k, 0) + 1
                    if k == "error" or st.get("error"):
                        errors.append((now, json.dumps(st, ensure_ascii=False)))
                    if st.get("url"):
                        files.append((now, st.get("url"), st.get("name")))
                    if st.get("t"):
                        deltas.append(st["t"])
    print(f"[{time.time()-t0:.1f}s] 结束；帧类型 {kinds}")
    print(f"正文合计 {len(''.join(deltas))} 字")
    for t, u, n in files:
        print(f"文件帧 @{t:.1f}s url={u} name={n}")
    if errors:
        print("=== error 帧原文 ===")
        for t, e in errors:
            print(f"@{t:.1f}s {e}")
    else:
        print("本轮没有 error 帧")
    print(f"原始帧已落盘：{OUT}")
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
