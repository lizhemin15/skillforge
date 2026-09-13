#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""假模型：替身 LLM，让运行时 E2E 能确定性驱动手册写作流水线。

【为什么需要它】手册写作的三段式（判类 → 按类执笔 → 审稿改稿）每一步都靠模型输出
接口。用真模型跑 E2E 只能看"像不像"，不能断言"注入到 prompt 里的到底是哪一类的要求"，
而且每次结果都不一样、跑一次要几十秒。换成一个只认标记的替身，就能把断言钉在
确定性事实上面：

  - 故意写成"格式可能不合法"的样子？不，替身就按契约回 JSON —— JSON 模式的解析
    能力由单元测试覆盖，这里测的是**接线**（帧序、注入命中、轮次、反问）。
  - 每一笔请求都落到 FAKE_LLM_LOG，供 E2E 事后核对 prompt 里到底出现了什么。

分发规则（按 system 里的特征串）：
  含「分类路由器」   → 分类判定 JSON
  含「你是审稿人」   → 审稿 JSON（草稿里还有 FLAGGED-SENTENCE 就 revise，否则 pass）
  含「你是稿件修改者」→ 改稿全文（把被点名的那句换掉）
  其余              → 起草全文

分类判定不猜：从 prompt 里那张「| 分类 | 触发场景 |」表里取出分类名，再看用户需求里
点名了哪一个。需求里写 UNCLEAR 就返回空类别（模拟"判不出来"，用于验证反问通道）。
"""

import json
import os
import re
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOG = os.environ.get("FAKE_LLM_LOG", "/tmp/fake-llm.jsonl")

# 起草正文里的"待审问题句"：审稿人会点它，改稿会把它换掉。
FLAGGED = "FLAGGED-SENTENCE：本次会议取得圆满成功，与会人员都很高兴。"
FIXED = "FIXED-SENTENCE：会议决定由办公室在 9 月 20 日前完成整改，责任人李四。"
DRAFT_MARK = "DRAFT-BODY"


def log(kind, system, user, stream):
    with open(LOG, "a", encoding="utf-8") as f:
        f.write(json.dumps({"kind": kind, "stream": stream,
                            "system": system, "user": user},
                           ensure_ascii=False) + "\n")


def table_categories(user):
    """从路由 prompt 的分类表里取出分类名（保序）。"""
    names = []
    for line in user.splitlines():
        m = re.match(r"^\|\s*(.+?)\s*\|\s*(.*?)\s*\|\s*$", line)
        if not m or m.group(1) in ("分类", "---") or set(m.group(1)) == {"-"}:
            continue
        names.append(m.group(1))
    return names


def route(user):
    """判类：需求里点名了某个分类就用它；写 UNCLEAR 表示判不出来。"""
    names = table_categories(user)
    # 只看「用户这次的需求」那一段：前面表格里出现的分类名不能算命中。
    req = user.split("## 用户这次的需求")[-1]
    if "UNCLEAR" in req:
        return {"category": "", "confidence": "low", "reason": "需求信息不足，看不出类别"}
    for n in names:
        if n and n in req:
            return {"category": n, "confidence": "high", "reason": "需求里点名了「%s」" % n}
    return {"category": "", "confidence": "low", "reason": "没有可对应的分类"}


def review(user):
    """审稿：草稿里还有待审问题句就报 revise，否则 pass。

    用草稿内容本身当状态，不靠调用次数——重跑、乱序都不影响判定。
    """
    if FLAGGED in user:
        return {"verdict": "revise", "issues": [{
            "severity": "blocking",
            "rule": "REQ-NEWS-9：不得出现主观评价",
            "quote": FLAGGED,
            "fix": "改成写清决议事项、责任人与时限",
        }]}
    return {"verdict": "pass", "issues": []}


def dispatch(system, user, stream):
    if "分类路由器" in system:
        log("route", system, user, stream)
        return json.dumps(route(user), ensure_ascii=False)
    if "你是审稿人" in system:
        log("review", system, user, stream)
        return json.dumps(review(user), ensure_ascii=False)
    if "你是稿件修改者" in system:
        log("revise", system, user, stream)
        # 改稿 prompt 里同时含草稿全文与审稿意见，把草稿整体重放一遍即可
        return DRAFT_MARK + "\n" + FIXED + "\n正文其余部分保持原样。\n"
    log("draft", system, user, stream)
    return DRAFT_MARK + "\n" + FLAGGED + "\n正文其余部分保持原样。\n"


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(n)
        try:
            body = json.loads(raw)
        except Exception:
            self.send_error(400)
            return
        msgs = body.get("messages") or []
        system = "\n".join(m.get("content", "") for m in msgs if m.get("role") == "system")
        user = "\n".join(m.get("content", "") for m in msgs if m.get("role") == "user")
        stream = bool(body.get("stream"))
        text = dispatch(system, user, stream)

        if stream:
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Connection", "keep-alive")
            self.end_headers()
            for i in range(0, len(text), 24):
                chunk = {"id": "fake", "object": "chat.completion.chunk",
                         "created": 0, "model": body.get("model", "fake"),
                         "choices": [{"index": 0, "delta": {"content": text[i:i + 24]},
                                      "finish_reason": None}]}
                self.wfile.write(("data: " + json.dumps(chunk, ensure_ascii=False) + "\n\n").encode())
                self.wfile.flush()
            end = {"id": "fake", "object": "chat.completion.chunk", "created": 0,
                   "model": body.get("model", "fake"),
                   "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]}
            self.wfile.write(("data: " + json.dumps(end, ensure_ascii=False) + "\n\n").encode())
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
            return

        payload = {"id": "fake", "object": "chat.completion", "created": 0,
                   "model": body.get("model", "fake"),
                   "choices": [{"index": 0, "finish_reason": "stop",
                                "message": {"role": "assistant", "content": text}}],
                   "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}
        data = json.dumps(payload, ensure_ascii=False).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


def main():
    port = int(os.environ.get("FAKE_LLM_PORT", "0"))
    open(LOG, "w").close()
    srv = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    # 端口由脚本指定：E2E 要把它写进 SKILLFORGE_LLM_BASE_URL。
    print("FAKE_LLM_READY %d" % srv.server_address[1], flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    sys.exit(main())
