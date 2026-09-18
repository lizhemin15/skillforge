#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""量「起草跳限思考预算」这笔提速到底伤不伤质量。

背景（已实测）：provider 认 `thinking_budget`。一句话题目上 512 就把思考链
从 1050 片压到 512 片、首片正文从 37.8s 提到 21.1s。但**起草跳跑的是长文**，
一句话题目的结论不能直接搬到长文上——所以拿真实提示词量三个档位：
不限 / 1024 / 2048，看两件事：

  1. 省多少秒（首片正文时刻 + 总耗时）；
  2. 稿子是否还完整（正文字数、是否以句末标点收尾、段落数）。

判据：只有「稿子仍然完整（≥400 字且以句末标点收尾）」的档位才允许写进代码。
凭据只从 DB 读、只回显长度。
"""
import json
import sqlite3
import sys
import time
import urllib.request

DB = "/opt/skillforge/data/skillforge.db"
PROMPT = ("写一篇关于星禾科技发布数据中台 3.0 的新闻稿，正文不少于 400 字，"
          "直接输出正文，不要任何解释。")
ENDERS = "。！？…”\"'"  # 句末标点（含右引号，模型常以引号收尾）


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


def run(label, base, cred, model, extra, timeout=240):
    url = base.rstrip("/") + "/chat/completions"
    body = {"model": model, "stream": True,
            "messages": [{"role": "user", "content": PROMPT}]}
    body.update(extra)
    req = urllib.request.Request(
        url, data=json.dumps(body).encode("utf-8"),
        headers={"Content-Type": "application/json",
                 "Authorization": "Bearer " + cred,
                 "Accept": "text/event-stream"},
    )
    t0 = time.time()
    r_pieces = 0
    first_c = None
    text = []
    err = ""
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            for raw in r:
                line = raw.decode("utf-8", "replace").strip()
                if not line.startswith("data:"):
                    continue
                payload = line[5:].strip()
                if payload == "[DONE]":
                    break
                try:
                    d = json.loads(payload)
                except Exception:
                    continue
                for ch in d.get("choices") or []:
                    delta = ch.get("delta") or {}
                    if delta.get("reasoning_content"):
                        r_pieces += 1
                    if delta.get("content"):
                        if first_c is None:
                            first_c = time.time() - t0
                        text.append(delta["content"])
    except Exception as e:  # noqa: BLE001
        err = "%s %s" % (type(e).__name__, str(e)[:120])

    body_text = "".join(text).strip()
    paras = [p for p in body_text.split("\n") if p.strip()]
    tail_ok = bool(body_text) and body_text[-1] in ENDERS
    print("--- %s" % label)
    print("    参数   : %s" % (json.dumps(extra, ensure_ascii=False) if extra else "（不限）"))
    print("    耗时   : 首片正文 %s｜总 %.1fs%s"
          % (("%.1fs" % first_c) if first_c is not None else "—", time.time() - t0,
             ("｜报错 " + err) if err else ""))
    print("    思考链 : %d 片" % r_pieces)
    print("    稿子   : %d 字｜%d 段｜以句末标点收尾=%s"
          % (len(body_text), len(paras), tail_ok))
    print("    末尾40 : …%s" % body_text[-40:].replace("\n", "⏎"))
    print()
    return {"r": r_pieces, "chars": len(body_text), "first_c": first_c,
            "tail_ok": tail_ok, "total": time.time() - t0, "err": err}


def main():
    cfg = active_cfg()
    base, cred, model = cfg["base_url"], cfg["api_key"], cfg["model"]
    print("生效配置 : provider=%s  model=%s" % (cfg["provider"], model))
    print("凭据     : [REDACTED len=%d]" % len(cred))
    print()
    a = run("A 不限预算（现状）", base, cred, model, {})
    b = run("B thinking_budget=1024", base, cred, model, {"thinking_budget": 1024})
    c = run("C thinking_budget=2048", base, cred, model, {"thinking_budget": 2048})

    print("===== 结论 =====")
    base_t = a["first_c"] or 0
    for label, r in (("1024", b), ("2048", c)):
        if r["err"]:
            print("budget=%s 报错（%s）⇒ 不能写进代码。" % (label, r["err"]))
            continue
        ok = r["chars"] >= 400 and r["tail_ok"]
        save = base_t - (r["first_c"] or 0)
        print("budget=%s：稿子 %d 字（完整=%s）｜首片正文 %s（省 %.1fs）"
              % (label, r["chars"], ok,
                 ("%.1fs" % r["first_c"]) if r["first_c"] else "—", save))
    print("（基准 A：%d 字，完整=%s，首片正文 %.1fs）"
          % (a["chars"], a["chars"] >= 400 and a["tail_ok"], base_t))


if __name__ == "__main__":
    main()
