#!/usr/bin/env python3
"""按 端口=模式 起若干假 HTTP 监听，供 install.sh 探测逻辑做正/负向自证。

只写最笨的实现：固定路由、固定响应体。目的是让被验证的是 install.sh 的判定逻辑，
而不是这个假服务。
"""
import http.server
import socketserver
import sys
import threading

# 每个模式的响应：状态码 + JSON 体
BODIES = {
    # 健康解析服务：真 ocrd 的 /health 形态
    # 注意：真 ocrd 是 `wfile.write(json.dumps(...))`，**正文结尾没有换行**（deploy/ocr/ocrd.py）。
    # 这里刻意保持「无换行」，因为 install.sh 的读循环正是在这种形态上出过假红。
    "ocr-ok": (200, b'{"ok": true, "runtime_ok": true, "version": "ocrd-v5-runtime-guard"}'),
    # 同样的正文 + 结尾换行：证明「有/无换行」两种形态都能被正确解析（双向自证）
    "ocr-ok-nl": (200, b'{"ok": true, "runtime_ok": true, "version": "ocrd-v5-runtime-guard"}\n'),
    # 2026-09-15 线上事故的形态：进程活着、端口听着、HTTP 200，但引擎已坏
    "ocr-zombie": (200, b'{"ok": false, "runtime_ok": false, '
                        b'"version": "ocrd-v5-runtime-guard", "error": "engine rebuild failed"}'),
    # 端口被别的进程占了：能连、也回 200，但根本不是 ocrd
    "ocr-impostor": (200, b'{"hello": "i am not ocrd"}'),
    # 主服务健康：GET /api/site 真实形态（公开接口）
    "site-ok": (200, b'{"name": "SkillForge"}'),
    # 主服务「端口在听但功能没好」：二进制与配置不匹配时就是这个样子
    "site-500": (500, b'{"error": "boom"}'),
}

MODE_OF = {}
for arg in sys.argv[1:]:
    port_s, mode = arg.split("=", 1)
    if mode not in BODIES:
        sys.exit("unknown mode: %s" % mode)
    MODE_OF[int(port_s)] = mode


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):  # noqa: N802
        code, body = BODIES[MODE_OF[self.server.server_address[1]]]
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


socketserver.TCPServer.allow_reuse_address = True
servers = []
for port in MODE_OF:
    srv = socketserver.TCPServer(("127.0.0.1", port), Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    servers.append(srv)

print("ready %s" % " ".join("%d=%s" % (p, MODE_OF[p]) for p in sorted(MODE_OF)), flush=True)
threading.Event().wait()
