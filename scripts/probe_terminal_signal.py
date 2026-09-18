#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""裸流「终止信号 + 思考预算」探针（按 DB 生效配置直连 provider）。

两个问题一次问清：

1. **终止信号**（R2 的安全前提）：provider 正常收尾到底是发 `data: [DONE]`，
   还是靠最后一片带 `finish_reason`，还是**什么都不给、直接关连接**？
   如果它两样都不给，那「没看到终止信号就判截断」这把尺子会把正常流全判死
   —— 必须先量，再决定判据。
2. **思考预算**（提速的候选）：`thinking_budget` 这个参数（Qwen3 系在
   SiliconFlow 上认）到底管不管用。管用的话，起草跳就能从「想 40 秒」
   变成「想 8 秒」，质量还在；不管用就直接排除这条路，别写进代码。

凭据只从 DB 读、只回显长度，不落盘不回显。
"""
import json
import sqlite3
import sys
import time
import urllib.request

DB = "/opt/skillforge/data/skillforge.db"
SHORT = "用一句话说明数据中台是什么。"


def active_cfg():
    c = sqlite3.connect(DB)
    c.row_factory = sqlite3.Row
    row = c.execute(
        "select provider, base_url, api_key, model from llm_config where is_active=1 limit 1"
    ).fetchone()
    if not row:
        print("!! llm_config 里没有 is_active=1 的行")
        sys.exit(2)
    return dict(row)


def raw_probe(label, base, cred, model, extra, timeout=120):
    """发一次裸流，只数「终止信号」与时刻表，不回显任何正文/凭据。"""
    url = base.rstrip("/") + "/chat/completions"
    body = {"model": model, "stream": True, "stream_options": {"include_usage": True},
            "messages": [{"role": "user", "content": SHORT}]}
    body.update(extra)
    req = urllib.request.Request(
        url,
        data=json.dumps(body).encode("utf-8"),
        headers={"Content-Type": "application/json",
                 "Authorization": "Bearer " + cred,
                 "Accept": "text/event-stream"},
    )
    t0 = time.time()
    saw_done = False
    finish_reasons = []
    frames = r_pieces = c_pieces = 0
    first_c = None
    eof_clean = False
    err = ""
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            for raw in r:
                line = raw.decode("utf-8", "replace").strip()
                if not line.startswith("data:"):
                    continue
                payload = line[5:].strip()
                if payload == "[DONE]":
                    saw_done = True
                    break
                try:
                    d = json.loads(payload)
                except Exception:
                    continue
                frames += 1
                for ch in d.get("choices") or []:
                    fr = ch.get("finish_reason")
                    if fr:
                        finish_reasons.append(fr)
                    delta = ch.get("delta") or {}
                    if delta.get("reasoning_content"):
                        r_pieces += 1
                    if delta.get("content"):
                        c_pieces += 1
                        if first_c is None:
                            first_c = time.time() - t0
        eof_clean = True  # 循环自然结束 = provider 关连接收尾
    except Exception as e:  # noqa: BLE001
        err = "%s %s" % (type(e).__name__, str(e)[:120])

    print("--- %s" % label)
    print("    附加参数 : %s" % (json.dumps(extra, ensure_ascii=False) if extra else "（无）"))
    print("    时刻     : 首片正文 %s｜总 %.1fs%s"
          % (("%.1fs" % first_c) if first_c is not None else "—", time.time() - t0,
             ("｜报错 " + err) if err else ""))
    print("    片数     : 思考链 %d｜正文 %d｜帧 %d" % (r_pieces, c_pieces, frames))
    print("    终止信号 : [DONE]=%s｜finish_reason=%s｜连接自然关闭=%s"
          % (saw_done, finish_reasons or "无", eof_clean))
    print()
    return {"done": saw_done, "fin": bool(finish_reasons), "eof": eof_clean,
            "r": r_pieces, "c": c_pieces, "first_c": first_c}


def main():
    cfg = active_cfg()
    base, cred, model = cfg["base_url"], cfg["api_key"], cfg["model"]
    print("生效配置 : provider=%s  model=%s" % (cfg["provider"], model))
    print("base_url : %s" % base)
    print("凭据     : [REDACTED len=%d]" % len(cred))
    print()

    # 1) 关思考链的小请求：最快，专门看终止信号长什么样。
    a = raw_probe("A 终止信号（enable_thinking=false）", base, cred, model,
                  {"enable_thinking": False}, timeout=60)
    # 2) 思考预算是否被认账：带思考，分别给 / 不给 budget。
    b = raw_probe("B 带思考·不限预算", base, cred, model, {}, timeout=180)
    c = raw_probe("C 带思考·thinking_budget=512", base, cred, model,
                  {"thinking_budget": 512}, timeout=180)

    print("===== 结论 =====")
    if a["done"] or a["fin"]:
        print("终止信号：provider 给（[DONE]=%s / finish_reason=%s）"
              "⇒ R2 可以「两者都缺才判截断」，误伤面最小。" % (a["done"], a["fin"]))
    else:
        print("终止信号：provider **只靠关连接收尾**（无 [DONE]、无 finish_reason）"
              "⇒ R2 不能硬判截断，得改用「EOF 前提下校验正文可解析性」这类软判据。")

    if c["r"] and b["r"] and c["r"] < b["r"] * 0.7:
        print("思考预算：管用（思考链片数 %d → %d，降 %.0f%%）"
              "⇒ 起草跳可以限预算提速。" % (b["r"], c["r"], 100 * (1 - c["r"] / b["r"])))
    elif c["r"] and b["r"]:
        print("思考预算：**没管用**（片数 %d vs %d，几乎没变）⇒ 别写进代码。"
              % (b["r"], c["r"]))
    else:
        print("思考预算：数据不足（C 思考链 %d 片 / B %d 片），别急着下结论。"
              % (c["r"], b["r"]))


if __name__ == "__main__":
    main()
