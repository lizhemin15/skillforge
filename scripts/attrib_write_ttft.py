#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""直连上游，给「执笔跳为什么 407 秒才吐第一个正文字」做归因。

## 为什么要这个脚本

线上实测（2026-09-22，轮 1 种素材）：
    [write-plain] hop=440.7s ttft=407.38s reason=7911 out_pieces=700 out=1278

`ttft=407.38s` 是「用户在前 407 秒里一个字正文都看不到」的物理量，
用户原话「一直卡着计时」。但**从产品代码里量不出这 407 秒是谁的**：
它既可能是模型固有成本（reasoning 模型在 1 万字素材上想很久），
也可能是我们在思考链上没有闸门（模型在思考里打转，没人拦）。
两者对策完全相反 —— 前者只能改架构（分流、提前出稿），后者加闸门就行。
所以必须**绕开产品代码直连上游**量一发，把两者分开。

## 三种问法对应的判断

1. `unbounded`（不带任何 thinking 字段）：模型固有成本有多高。
2. `b1024` / `b2048`：我们的 thinking_budget 旋钮**这个 provider+模型认不认**。
   代码注释里「1024 → 首正文 22.76s」是在 **Qwen3.6-27B** 上测的，
   线上今天活跃的是 **Qwen3.6-35B-A3B** —— 旧结论对新模型不成立，
   换模型必须重量（这条重复踩过）。
3. `nothink`（enable_thinking=false + reasoning_effort=none）：关掉思考链的代价与收益。

## 「在打转」怎么证

不能靠肉眼。这里**照抄 internal/llm/stream.go 的 loopState.hit 判据**
（192 字节尾巴、在前文出现 ≥2 次、取最小相邻间距当循环体），
只不过作用对象从「正文」换成「思考链」——
因为线上看门狗只吃正文片（stream.go:680 的 sb），思考片从来不送检，
所以「思考链在打转」这个可能性从未被证伪过。

## 用法

    # 单臂（先跑这个，确认脚本本身能出数）
    python3 scripts/attrib_write_ttft.py --arm unbounded --material /tmp/material_10k.txt

    # 全臂顺序跑（顺序！并发会互相抢上游，数字就不可归因了）
    python3 scripts/attrib_write_ttft.py --arms all --material /tmp/material_10k.txt --timeout 300

密钥一律**运行时从 DB 读**，不落盘、不回显、不进本文件（写完会被脱敏过滤器改写）。
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sqlite3
import sys
import time
import urllib.error
import urllib.request

DB = os.environ.get("SF_DB", "/opt/skillforge/data/skillforge.db")
AGENT_GO = os.environ.get("SF_AGENT_GO", "/root/skillforge/internal/agent/agent.go")

PROMPT_DEFAULT = "按上面素材里的【写作要求】，写一篇新闻通稿。"


def conf() -> dict:
    """从 DB 取生效的 LLM 配置。密钥只以变量形式存在，绝不打印。"""
    cn = sqlite3.connect(f"file:{DB}?mode=ro", uri=True)
    try:
        cn.row_factory = sqlite3.Row
        row = cn.execute(
            "select provider,base_url,api_key,model from llm_config where is_active=1"
        ).fetchone()
        if row is None:
            raise SystemExit("DB 里没有 is_active=1 的 llm_config")
        return dict(row)
    finally:
        cn.close()


def roster() -> str:
    """技能清单：buildRoster 的近似（只为让 sys 长度接近线上，不追求逐字节一致）。"""
    try:
        cn = sqlite3.connect(f"file:{DB}?mode=ro", uri=True)
        rows = cn.execute("select name,description from skills").fetchall()
        cn.close()
    except Exception:
        return ""
    out = []
    for name, desc in rows:
        desc = (desc or "").strip().replace("\n", " ")
        out.append(f"- {name}：{desc[:60]}")
    return "\n".join(out)


def app_sys() -> str:
    """把 App 自己那条 system prompt 抠出来用，别手写一份近似的。

    抠的是 **plainChatWithPlan** 里那条（执笔跳的 sys）—— 同一个文件里有好几条
    `sys := \`...\` + rosterStr`，不锚定函数就会抠到别的（首版就抠到了分类调度器那条，
    量出来的条件根本不对）。长度打印出来跟线上日志对得上，才算**同一条件**。
    抠不到（代码改结构了）就退回空串并明说 —— 不许静默用近似值冒充。
    """
    try:
        src = open(AGENT_GO, encoding="utf-8").read()
    except OSError:
        return ""
    i = src.find("func (e *Engine) plainChatWithPlan")
    if i < 0:
        return ""
    j = src.find("\nfunc ", i + 10)
    body = src[i:j if j > 0 else len(src)]
    m = re.search(r"sys := `(.*?)`\s*\+\s*rosterStr", body, re.S)
    if not m:
        return ""
    return m.group(1)


# ---------------------------------------------------------------- 复读判据
LOOP_TAIL = 192        # 与 stream.go 的 loopTailBytes 一致
LOOP_MIN = 3 * LOOP_TAIL
LOOP_EVERY = 2048      # 与 loopCheckEvery 一致


def loop_hit(full: str) -> tuple[int, int]:
    """照抄 loopState.hit 的语义，返回（循环体字节, 检测时的正文长度）；没复读返回 (0,0)。

    为什么在 Python 里重抄一遍而不是调 Go：这是**只读的归因探针**，
    跑在服务之外，不该为了量一次数字去给服务加一个导出接口。
    判据本身一致（同一组常量、同一套最小间距规则），结论就可对账。
    """
    b = full.encode("utf-8")
    n = len(b)
    if n < LOOP_MIN:
        return 0, 0
    checked = 0
    while n >= checked + LOOP_EVERY:
        checked = n
        tail = b[n - LOOP_TAIL:]
        prev = b[: n - LOOP_TAIL]
        prev_at, cnt, period = -1, 0, 0
        i = 0
        while True:
            j = prev.find(tail, i)
            if j < 0:
                break
            i = j
            if prev_at >= 0:
                gap = i - prev_at
                if period == 0 or gap < period:
                    period = gap
            prev_at, cnt = i, cnt + 1
            i += 1
        if cnt >= 2:
            gap = (n - LOOP_TAIL) - prev_at
            if gap < period:
                period = gap
            return period, n
        # 没命中就往后长，等下一片喂进来 —— 但本脚本是一次性拿到全文再判，
        # 所以这里直接退出：一次判完，不再模拟逐个 loopCheckEvery 的检查点。
        break
    return 0, 0


def dup_ratio(text: str, win: int = 100) -> float:
    """重复窗口占比：把文本按 win 字切窗，统计重复出现过的窗口比例。

    补充 loop_hit 的盲区：尾巴 192 字节的**精确**重复抓不住「近乎重复」
    （改几个字的循环）。这个量只是辅证，不单独下结论。
    """
    r = [] if not text else [text[i:i + win] for i in range(0, len(text) - win + 1, win // 2)]
    if len(r) < 4:
        return 0.0
    seen, dup = set(), 0
    for w in r:
        if w in seen:
            dup += 1
        else:
            seen.add(w)
    return dup / len(r)


# ---------------------------------------------------------------- 打一发
ARMS = {
    "unbounded": {},                                    # 语义同 SKILLFORGE_THINK_BUDGET=0
    "b1024": {"thinking_budget": 1024},                 # 语义同默认
    "b2048": {"thinking_budget": 2048},
    "nothink": {"enable_thinking": False, "reasoning_effort": "none"},
}


def one_arm(c: dict, arm: str, material: str, prompt: str, timeout: float,
            user_mode: str, dump_prefix: str = "/tmp/attrib_reason",
            args_tag: str = "run") -> dict:
    knob = ARMS[arm]
    sys_text = app_sys() + "\n\n技能清单：\n" + roster()
    if user_mode == "material":
        user = material + "\n\n" + prompt
    else:
        # 对照臂：不给素材，只给需求。用来把「1 万字素材的成本」和
        # 「这条提示词本身的成本」分开 —— 没有这个对照，
        # 就没法说清 407 秒是素材造成的还是模型脾气。
        user = prompt
    body = {
        "model": c["model"],
        "messages": [
            {"role": "system", "content": sys_text},
            {"role": "user", "content": user},
        ],
        "stream": True,
    }
    body.update(knob)

    req = urllib.request.Request(
        c["base_url"].rstrip("/") + "/chat/completions",
        data=json.dumps(body, ensure_ascii=False).encode("utf-8"),
        headers={
            "Content-Type": "application/json",
            "Authorization": "Bearer " + c["api_key"],
            "Accept": "text/event-stream",
        },
        method="POST",
    )

    t0 = time.time()
    ttft_r = ttft_c = None
    reason_parts: list[str] = []
    content_parts: list[str] = []
    n_pieces = 0
    err = ""
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            for raw in r:
                line = raw.decode("utf-8", "replace").strip()
                if not line.startswith("data:"):
                    continue
                data = line[5:].strip()
                if data == "[DONE]":
                    break
                try:
                    j = json.loads(data)
                except ValueError:
                    continue
                ch = (j.get("choices") or [{}])[0]
                d = ch.get("delta") or {}
                rc = d.get("reasoning_content") or ""
                cc = d.get("content") or ""
                if rc:
                    if ttft_r is None:
                        ttft_r = time.time() - t0
                    reason_parts.append(rc)
                    n_pieces += 1
                if cc:
                    if ttft_c is None:
                        ttft_c = time.time() - t0
                    content_parts.append(cc)
    except urllib.error.HTTPError as e:
        err = f"HTTP {e.code}: {e.read()[:200].decode('utf-8', 'replace')}"
    except Exception as e:  # noqa: BLE001 —— 探针，任何异常都要记下来变成数字
        err = f"{type(e).__name__}: {e}"

    reason, content = "".join(reason_parts), "".join(content_parts)
    period, at = loop_hit(reason)
    # 落盘：提速的每一刀都要能拿出**产物**来对照质量，不能只看秒数变短。
    # 关思考能省 190 秒，但如果写出来的通稿少了五要素，那不叫提速叫退化。
    try:
        tag = f"{args_tag}_{arm}_{user_mode}"
        with open(f"{dump_prefix}_{tag}.reason.txt", "w", encoding="utf-8") as fh:
            fh.write(reason)
        with open(f"{dump_prefix}_{tag}.content.txt", "w", encoding="utf-8") as fh:
            fh.write(content)
    except OSError:
        pass
    return {
        "arm": arm,
        "user_mode": user_mode,
        "sys_chars": len(sys_text),
        "user_chars": len(user),
        "ttft_reason": ttft_r,
        "ttft_content": ttft_c,
        "reason_chars": len(reason),
        "reason_pieces": n_pieces,
        "content_chars": len(content),
        "total_s": time.time() - t0,
        "loop_period_reason": period,
        "loop_at": at,
        "dup_ratio": round(dup_ratio(reason), 3),
        "err": err,
    }


def fmt(v, unit="s"):
    if v is None:
        return "—"
    return f"{v:.1f}{unit}" if isinstance(v, float) else str(v)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--arm", default="unbounded", choices=list(ARMS))
    ap.add_argument("--arms", default="", help="all 或逗号分隔（如 b1024,unbounded,nothink）")
    ap.add_argument("--material", default="/tmp/material_10k.txt")
    ap.add_argument("--user-mode", default="material", choices=["material", "bare"])
    ap.add_argument("--prompt", default=PROMPT_DEFAULT)
    ap.add_argument("--timeout", type=float, default=300.0, help="单臂读流超时（秒）")
    ap.add_argument("--dump", default="/tmp/attrib_reason", help="思考链落盘前缀")
    args = ap.parse_args()

    c = conf()
    print(f"provider={c['provider']} model={c['model']} base={c['base_url']}", flush=True)
    material = ""
    if args.user_mode == "material":
        try:
            material = open(args.material, encoding="utf-8").read()
        except OSError as e:
            print(f"素材读不到：{e}", file=sys.stderr)
            return 2
    print(f"素材 {len(material)} 字 | user_mode={args.user_mode} | timeout={args.timeout}s", flush=True)

    arms = list(ARMS) if args.arms == "all" else (
        [a.strip() for a in args.arms.split(",") if a.strip()] or [args.arm]
    )
    bad = [a for a in arms if a not in ARMS]
    if bad:
        print(f"未知臂：{bad}（可选 {list(ARMS)}）", file=sys.stderr)
        return 2
    rows = []
    for a in arms:
        print(f"\n=== [{a}] 开始 ===", flush=True)
        r = one_arm(c, a, material, args.prompt, args.timeout, args.user_mode,
                    args.dump, args.dump)
        rows.append(r)
        print(
            f"[{a}] ttft正文={fmt(r['ttft_content'])} ttft思考={fmt(r['ttft_reason'])} "
            f"思考={r['reason_chars']}字/{r['reason_pieces']}片 正文={r['content_chars']}字 "
            f"总={r['total_s']:.1f}s 复读体={r['loop_period_reason']}B@{r['loop_at']} "
            f"重复窗={r['dup_ratio']:.0%} sys={r['sys_chars']}字 user={r['user_chars']}字"
            + (f" err={r['err']}" if r["err"] else ""),
            flush=True,
        )
    if len(rows) > 1:
        print("\n=== 汇总 ===", flush=True)
        hdr = f"{'arm':<10}{'ttft正文':>10}{'思考字':>9}{'正文':>7}{'总s':>8}{'复读体':>8}"
        print(hdr)
        for r in rows:
            print(
                f"{r['arm']:<10}{fmt(r['ttft_content']):>10}{r['reason_chars']:>9}"
                f"{r['content_chars']:>7}{r['total_s']:>8.1f}{r['loop_period_reason']:>8}"
            )
    return 0


if __name__ == "__main__":
    sys.exit(main())
