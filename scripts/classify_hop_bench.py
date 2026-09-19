#!/usr/bin/env python3
# 第一跳耗时拆解 v4：**契约瘦身不能丢掉路由正确性**。
#
# v3 已确证：线上生效那家（siliconflow/Qwen3.6-27B）的耗时 ≈ 输出字数 ÷ ~50 tok/s。
#   A 现契约              短提示 10.29s / out 811 字
#   B 只加「写短点」        短提示  7.73s / out 710 字   ← 光喊口号没用
#   C 干脆不输出 steps      短提示  1.93s / out 125 字   ← 立竿见影，但步骤板没了
#
# 所以 v4 测「中间路线」D：**保留 4 阶段 steps，但只让模型给 phase+≤10 字 detail**，
# label/status 由服务端补（那是固定文案，模型写它纯属浪费）。同时在 6 条代表性消息上
# 逐条比对 A/D 的 intent/action/skill —— 快而判错等于更糟。
import json
import os
import re
import sqlite3
import time
import urllib.request

DB = os.environ.get("SKILLFORGE_DB", "/opt/skillforge/data/skillforge.db")


def real_sys():
    src = open("/root/skillforge/internal/agent/agent.go", encoding="utf-8").read()
    m = re.search(r"sys := `(.*?)`\s*\+\s*rosterStr", src, re.S)
    if not m:
        raise SystemExit("没抠到 sys 提示词")
    return m.group(1)


def real_roster():
    con = sqlite3.connect(DB)
    rows = con.execute(
        "select slug,name,skill_type,description from skills where enabled=1").fetchall()
    out = ["- slug=%s | 名称=%s | 类型=%s | 描述=%s" % r for r in rows]
    return "\n".join(out) if out else "（当前没有可用的写作技能）"


def active_cfg():
    con = sqlite3.connect(DB)
    r = con.execute("select id,provider,base_url,api_key,model from llm_config "
                    "where is_active=1 limit 1").fetchone()
    return dict(id=r[0], provider=r[1], base=r[2].rstrip("/"), key=r[3], model=r[4])


LEAN = """

【覆盖上面第 3 条的 steps 契约（硬要求，优先于前文）】
steps 仍然输出 4 项，但每项**只给两个字段**：
  {"phase":"analyze|match|params|generate","detail":"≤10字"}
- 不要写 label、不要写 status（系统会补）。
- reason ≤12 字；禁止复述用户原文、禁止解释判断过程。
- params/needs 没有内容时给 {} / []，不要写任何占位话术。
- 全篇 JSON 总计不超过 220 字。"""

MSGS = [
    ("写文章", "写一篇关于数据要素产业协同推进会的新闻稿，正文不少于 1200 字，按给定的写作要求写。"),
    ("写正文不要文件", "写一份关于开展数据治理专项行动的通知。素材：2026年起推进…。正文不少于600字，直接输出正文。"),
    ("模板填充", "用采购验收单模板帮我填一份，随便编点数据。"),
    ("空白模板", "发我一个采购合同的空白模板文件。"),
    ("闲聊问答", "你们这个系统是干什么用的？"),
    ("实时数据", "帮我查一下数据工具箱里有哪些库表，并统计每个库的表数量。"),
]


def call(cfg, sys, user):
    body = {"model": cfg["model"],
            "messages": [{"role": "system", "content": sys}, {"role": "user", "content": user}],
            "stream": True, "enable_thinking": False, "reasoning_effort": "none",
            "thinking_budget": 512, "response_format": {"type": "json_object"}}
    req = urllib.request.Request(
        cfg["base"] + "/chat/completions", data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + cfg["key"]})
    t0 = time.time()
    out = []
    with urllib.request.urlopen(req, timeout=180) as r:
        for raw in r:
            line = raw.decode("utf-8", "replace").strip()
            if not line.startswith("data:"):
                continue
            d = line[5:].strip()
            if d == "[DONE]":
                break
            try:
                j = json.loads(d)
            except Exception:
                continue
            for ch in (j.get("choices") or []):
                pc = (ch.get("delta") or {}).get("content") or ""
                if pc:
                    out.append(pc)
    return "".join(out), time.time() - t0


def brief(txt):
    try:
        j = json.loads(re.sub(r"^```[a-z]*|```$", "", txt.strip(), flags=re.M).strip())
    except Exception:
        return "PARSE_FAIL " + txt[:80]
    st = j.get("steps") or []
    return "intent=%s action=%s skill=%s needs=%d steps=%d" % (
        j.get("intent"), j.get("action"), (j.get("skill_slug") or "-"),
        len(j.get("needs") or []), len(st))


cfg = active_cfg()
sysA = real_sys() + "\n\n技能清单：\n" + real_roster()
sysD = sysA + LEAN
print("线上生效: config#%d %s / %s | 现契约 %d 字 | 瘦身契约 %d 字" % (
    cfg["id"], cfg["provider"], cfg["model"], len(sysA), len(sysD)))
print("=" * 104)
totA = totD = 0.0
for name, q in MSGS:
    a_txt, a_s = call(cfg, sysA, q)
    d_txt, d_s = call(cfg, sysD, q)
    totA += a_s
    totD += d_s
    print("%-14s A %5.2fs %-58s" % (name, a_s, brief(a_txt)[:58]))
    print("%-14s D %5.2fs %-58s  out %d字→%d字" % (
        "", d_s, brief(d_txt)[:58], len(a_txt), len(d_txt)))
print("=" * 104)
print("6 条消息合计: A %.1fs  →  D %.1fs   （平均 %.2fs → %.2fs）" % (
    totA, totD, totA / len(MSGS), totD / len(MSGS)))
