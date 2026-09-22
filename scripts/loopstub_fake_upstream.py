#!/usr/bin/env python3
"""假上游（OpenAI 兼容）：专门用来**确定性**触发复读看门狗。

为什么需要它：线上 35B 模型不肯配合复读（换了两版极端提示词，一轮只吐 74 字节、
reset 帧 0 个，尺子只能报 PREMISE_MISS）。等模型自己进复读循环等于等运气，
「看门狗能不能收手」这件事就没法验收。假上游把「上游真复读」这个前提**造出来**，
让真服务端（stream.go 的检测器 + chat.go 的 reset 帧 + 前端清屏契约）跑完整链路。

行为：
  · 第一次写作请求（没有 frequency_penalty）→ 吐「循环测试」重复 3000 遍。
    这是真复读：尾巴 192 字节在长文里反复出现，看门狗必须响。
  · 带 frequency_penalty 的请求（看门狗的重试）→ 吐一小段干净正文，
    且短语只出现一次 —— 用来验「清屏之后接的是干净答案，不是半截垃圾」。
  · 请求里出现「可选的分类」→ 回一个合法分类 JSON，分类名**从请求自带的分类表里抠**
    （RouteCategory 分支）。实测提醒：这分支在本桩上**通常走不到** —— 会话先过
    「意图分类」，那一步的提示词不含「可选的分类」，于是拿到的是复读；技能没匹配上时
    更到不了 RouteCategory。所以别以为本桩在替模型做完整分类，它只是个
    「确定性复读源」，这也正是它存在的意义（模型不肯复读时唯一能把前提造出来的东西）。
"""
import http.server
import json
import re
import sys
import threading
import time

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 8198

CAT_RE = re.compile(r"^\|\s*([^|\n]+?)\s*\|", re.M)


def classify_json(body):
    """从请求里的分类表抠第一个真实分类名，保证 matchCategory 能命中。"""
    txt = "\n".join(m.get("content", "") for m in body.get("messages", []))
    for name in CAT_RE.findall(txt):
        n = name.strip()
        if n in ("分类", "---", "类别") or set(n) <= set("-: "):
            continue
        return {"category": n, "confidence": "high", "reason": "假上游：直接把表里第一类还给你"}
    return {"category": "", "confidence": "low", "reason": "假上游：没找到分类表"}


def sse(self, obj):
    self.wfile.write(("data: " + json.dumps(obj, ensure_ascii=False) + "\n\n").encode("utf-8"))
    self.wfile.flush()


def chunk(text):
    return {"id": "stub", "object": "chat.completion.chunk", "model": "stub",
            "choices": [{"index": 0, "delta": {"content": text}}]}


class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        try:
            body = json.loads(self.rfile.read(n) or b"{}")
        except Exception:
            body = {}
        txt = "\n".join(m.get("content", "") for m in body.get("messages", []))
        penalty = float(body.get("frequency_penalty") or 0.0)
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()

        if "可选的分类" in txt:
            sys.stderr.write("[stub] classify → JSON\n")
            sse(self, chunk(json.dumps(classify_json(body), ensure_ascii=False)))
        elif penalty > 0:
            sys.stderr.write("[stub] write retry penalty=%.2f → 干净正文\n" % penalty)
            sse(self, chunk("这是重试之后的干净答案："))
            time.sleep(0.3)
            sse(self, chunk("上游曾经陷进复读，看门狗提前收手并带上 frequency_penalty 重问了一次，"
                            "所以现在这一段里「循环测试」只出现一遍。"))
        else:
            sys.stderr.write("[stub] write first pass → 复读 3000 遍\n")
            for _ in range(3000):  # 12 字节 × 3000 = 36KB，远超 12288 的客户端上限
                sse(self, chunk("循环测试"))
                time.sleep(0.002)
        try:
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
        except OSError:
            pass  # 看门狗提前断流：连接会先断，这里不该炸
        self.close_connection = True


srv = http.server.ThreadingHTTPServer(("127.0.0.1", PORT), H)
print("loopstub listening on %d" % PORT, flush=True)
threading.Thread(target=srv.serve_forever, daemon=True).start()
while True:
    time.sleep(3600)
