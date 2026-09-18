#!/usr/bin/env python3
"""抓一段 /api/chat 的原始响应字节，看它到底是什么协议（SSE？CRLF？NDJSON？）。

为什么要这个：我第一版时延脚本按 "\n\n" 切帧，结果 44.9s 的流里读到 0 帧 ——
说明切帧规则和服务实际吐的格式不一致。时延账本要可信，先得知道原始格式长什么样。
只打前 900 字节（含控制字符可视化），不外发。
"""
import json
import os
import sys
import urllib.request

BASE = os.environ.get("BASE", "http://127.0.0.1:8092")
U = os.environ.get("SKILLFORGE_ADMIN_USER", "")
P = os.environ.get("SKILLFORGE_ADMIN_PASS", "")

body = json.dumps({"username": U, "password": P}).encode()
req = urllib.request.Request(BASE + "/api/login", method="POST", data=body)
req.add_header("Content-Type", "application/json")
tok = json.loads(urllib.request.urlopen(req, timeout=30).read())["token"]

req = urllib.request.Request(BASE + "/api/chat", method="POST", data=json.dumps({
    "session_id": "raw-%d" % int(__import__("time").time()),
    "message": "把这段文字原样整理成 Word：数据要素产业协同推进会在京召开。",
    "mode": "auto", "skill": "",
}).encode())
req.add_header("Content-Type", "application/json")
req.add_header("Authorization", "Bearer " + tok)
with urllib.request.urlopen(req, timeout=60) as r:
    print("HTTP", r.status, "| content-type:", r.headers.get("content-type"))
    data = r.read(900)
sys.stdout.buffer.write(b"---- RAW FIRST 900 BYTES ----\n")
sys.stdout.flush()
os.write(1, data.replace(b"\r", b"<CR>").replace(b"\n", b"<LF>\n"))
