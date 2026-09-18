#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""按 DB 里生效的 provider 配置直连裸流探针（复用 probe_provider.probe）。

为什么要读 DB：env 里的 SKILLFORGE_LLM_* 是 is_active=0 的旧配置，
真正在打的是 llm_config 表里 is_active=1 的那行。

判据（只看 A）：
  首片思考链很早（<3s）→ provider 在流里推 reasoning_content
                          ⇒ 线上那 54.7s 静默是我们自己没接/没转发，代码问题
  首片思考链为 —、首片正文 ~60s → provider 把思考链憋到结尾才给
                          ⇒ 静默在 provider 侧，代码改不了这一跳的等待
输出不回显任何凭据值。
"""
import sqlite3
import sys

sys.path.insert(0, "/root/skillforge/scripts")
from probe_provider import probe  # noqa: E402

DB = "/opt/skillforge/data/skillforge.db"


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


def main():
    cfg = active_cfg()
    base = cfg["base_url"]
    cred = cfg["api_key"]
    model = cfg["model"]
    print("生效配置 : provider=%s  model=%s" % (cfg["provider"], model))
    print("base_url : %s" % base)
    print("凭据     : [REDACTED len=%d]" % len(cred))
    print()

    a = probe("A 默认（带思考）＝起草跳现在的样子", base, cred, model, {}, timeout=90)
    b = probe("B enable_thinking=false", base, cred, model, {"enable_thinking": False}, timeout=90)
    d = probe("D chat_template_kwargs.enable_thinking=false", base, cred, model,
              {"chat_template_kwargs": {"enable_thinking": False}}, timeout=90)

    print("===== 判据 =====")
    if a["err"]:
        print("A 报错：%s → 先修探针，别下结论" % a["err"])
    elif a["first_r"] is not None and a["first_r"] < 10:
        print("A：%.1fs 就有思考链片（共 %d 片），首片正文 %.1fs"
              % (a["first_r"], a["reasoning_pieces"],
                 a["first_c"] if a["first_c"] is not None else -1))
        print("⇒ provider **在推** reasoning_content。线上 54.7s 静默＝我们这一跳没接/没转发。")
    elif a["content_pieces"] > 0 and (a["first_c"] or 0) > 30:
        print("A：思考链 0 片，首片正文 %.1fs，总 %.1fs" % (a["first_c"], a["total"]))
        print("⇒ provider 把思考链全憋在内部，静默在 provider 侧（这条路上没有可转发的料）。")
    else:
        print("A：首字节 %s，思考链 %d 片，正文 %d 片，总 %.1fs → 看明细"
              % (a["first_any"], a["reasoning_pieces"], a["content_pieces"], a["total"]))
    for nm, v in (("B", b), ("D", d)):
        if v["err"]:
            print("%s：报错 %s" % (nm, v["err"]))
        else:
            print("%s：首片正文 %s，思考链 %d 片，总 %.1fs"
                  % (nm, v["first_c"], v["reasoning_pieces"], v["total"]))
    return 0


if __name__ == "__main__":
    sys.exit(main())
