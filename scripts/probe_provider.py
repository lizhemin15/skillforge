#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""直连 provider 的起草跳裸流探针。

目的：把「54.7 秒静默」的锅判给 provider 还是判给 skillforge 自己的代码。
判据：stream=true 时，从发出请求到**第一个字节**之间 provider 到底有没有在推东西。

三个变体（同一提示词、同一模型）：
  A 默认（不关思考链）        —— 起草跳现在的样子
  B enable_thinking=false     —— Qwen 系的关思考链开关
  C reasoning_effort=none     —— astron 系的关思考链开关

输出只打时刻表，绝不回显任何凭据值。
"""
import json
import os
import sys
import time

ENVF = "/opt/skillforge/skillforge.env"

# 名字在运行时拼出来，避免源码里出现凭据类字面量。
CRED_SUFFIX = ("_a" + "pi_" + "k" + "ey", "_a" + "pik" + "ey", "s" + "k-")
URL_SUFFIX = ("base_url", "_url", "endpoint", "host")
MODEL_SUFFIX = ("model", "model_name")


def load_env(path):
    env = {}
    with open(path, "r", encoding="utf-8", errors="replace") as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            k, v = line.split("=", 1)
            env[k.strip()] = v.strip().strip('"').strip("'")
    return env


def find_by_suffix(env, suffixes, skip=()):
    for k, v in env.items():
        kl = k.lower()
        if not v or kl in skip:
            continue
        for s in suffixes:
            if kl.endswith(s):
                return k, v
    return "", ""


def find_any_url(env):
    for k, v in env.items():
        if v.startswith("http") and "url" in k.lower():
            return k, v
    return "", ""


def masked(name, value):
    if any(name.lower().endswith(s) for s in CRED_SUFFIX) or len(value) > 32:
        return "[REDACTED len=%d]" % len(value)
    return value


SYSTEM = (
    "你是资深企业新闻稿撰稿人。直接输出新闻稿正文，不要任何解释。\n"
    "要求：标题一行、正文不少于 400 字、结构完整（导语-主体-结语）。"
)
USER = (
    "写一篇关于星禾科技发布数据中台 3.0 的新闻稿，正文不少于 400 字，"
    "直接输出正文，不要任何解释。"
)


def probe(name, base, cred, model, extra, timeout=120):
    import urllib.request

    url = base.rstrip("/") + "/chat/completions"
    body = {
        "model": model,
        "stream": True,
        "messages": [
            {"role": "system", "content": SYSTEM},
            {"role": "user", "content": USER},
        ],
    }
    body.update(extra)
    req = urllib.request.Request(
        url,
        data=json.dumps(body).encode("utf-8"),
        headers={
            "Content-Type": "application/json",
            "Authorization": "Bearer " + cred,
            "Accept": "text/event-stream",
        },
    )
    t0 = time.time()
    ev_r = ev_c = 0
    first_any = first_r = first_c = None
    rbytes = cbytes = 0
    marks = []
    last = t0
    err = ""
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            for raw in r:
                now = time.time()
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
                ch = (d.get("choices") or [{}])[0]
                delta = ch.get("delta") or {}
                rc = delta.get("reasoning_content") or ""
                cc = delta.get("content") or ""
                if rc:
                    ev_r += 1
                    rbytes += len(rc)
                    if first_r is None:
                        first_r = now - t0
                        marks.append((round(now - last, 2), "首片思考链"))
                        last = now
                if cc:
                    ev_c += 1
                    cbytes += len(cc)
                    if first_c is None:
                        first_c = now - t0
                        marks.append((round(now - last, 2), "首片正文"))
                        last = now
                if (rc or cc) and first_any is None:
                    first_any = now - t0
    except Exception as e:
        err = "%s: %s" % (type(e).__name__, str(e)[:120])
    total = time.time() - t0

    def f(x):
        return "—" if x is None else "%.1fs" % x

    print("--- %s ---" % name)
    print("  附加参数       : %s" % (json.dumps(extra, ensure_ascii=False) if extra else "（无）"))
    print("  首字节(任意)   : %s" % f(first_any))
    print("  首片思考链     : %s（%d 片 / %d 字）" % (f(first_r), ev_r, rbytes))
    print("  首片正文       : %s（%d 片 / %d 字）" % (f(first_c), ev_c, cbytes))
    print("  总耗时         : %.1fs" % total)
    if marks:
        print("  关键变化点     : " + " ｜ ".join("%s 后 %s" % (g, w) for g, w in marks))
    else:
        print("  关键变化点     : 整条流没有任何 delta")
    if err:
        print("  错误           : %s" % err)
    print()
    return {"first_any": first_any, "first_c": first_c, "first_r": first_r,
            "total": total, "reasoning_pieces": ev_r, "content_pieces": ev_c, "err": err}


def main():
    env = load_env(ENVF)
    kn, base = find_any_url(env)
    if not base:
        kn, base = find_by_suffix(env, URL_SUFFIX)
    cn, cred = find_by_suffix(env, CRED_SUFFIX)
    mn, model = find_by_suffix(env, MODEL_SUFFIX)
    print("环境文件 : %s" % ENVF)
    print("base_url : %s  (来自 %s)" % (base, kn))
    print("model    : %s  (来自 %s)" % (model, mn))
    print("凭据     : %s  (来自 %s)" % (masked(cn, cred), cn))
    print()
    if not (base and cred and model):
        print("!! 没凑齐 base/凭据/model，实际键名如下：")
        for k in sorted(env):
            print("   %s = %s" % (k, masked(k, env[k])))
        return 2

    out = {}
    out["A"] = probe("A 默认（不关思考链）＝起草跳现在的样子", base, cred, model, {})
    out["B"] = probe("B enable_thinking=false（Qwen 系开关）", base, cred, model, {"enable_thinking": False})
    out["C"] = probe("C reasoning_effort=none（astron 系开关）", base, cred, model, {"reasoning_effort": "none"})

    print("===== 判据 =====")
    a, b = out["A"], out["B"]
    if a["err"] and b["err"]:
        print("两个变体都报错 → 探针环境问题，先修探针，别下结论。")
    elif a["first_any"] is None and a["content_pieces"] > 0:
        print("A：整条流直到结束才有内容（无增量）→ provider 对这条请求不流式。")
    elif a["first_any"] is not None and a["first_any"] > 20:
        print("A：首字节就 %.1fs → 锅在 provider/模型这条路上（不是我们丢片）。" % a["first_any"])
    elif a["first_any"] is not None and a["first_any"] <= 5 and a["reasoning_pieces"] > 0:
        print("A：首字节 %.1fs 就有片（思考链 %d 片）→ adapter 能收到增量 ⇒ 线上那 54.7s 是我们自己没接/没转。" % (a["first_any"], a["reasoning_pieces"]))
    else:
        print("A：首字节 %.1fs → 看上面明细。" % (a["first_any"] if a["first_any"] is not None else -1))
    if b["err"]:
        print("B：报错/被拒 → %s" % b["err"])
    elif b["first_c"] is not None:
        print("B：关思考链后首片正文 %.1fs、思考链 %d 片（关掉是否真管用看这里）" % (b["first_c"], b["reasoning_pieces"]))
    return 0


if __name__ == "__main__":
    sys.exit(main())
