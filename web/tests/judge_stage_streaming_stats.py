#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""judge_stage_streaming_capture.py 的判定半边：把 raw SSE + 逐帧时间戳合起来算账。

为什么要拆成两个文件：采集要跑二十来分钟，判定逻辑却必须能反复重跑、随时改阈值。
采集脚本只负责「一个字节都不丢地落盘 + 每帧到达的相对秒」，判定全在这里。

输入：
  RAW_LOG      原始 SSE 字节（judge_stage_streaming_capture.py 落的盘）
  FRAMES_LOG   逐行「+  123.4s [kind/stage] …」——只取时间戳列，正文以 raw 为准
                （日志行截断到 300 字符，用它判内容会把长帧误判成没材料）

真实外层 schema（实测，不是猜的）：
  {"data": "<正文>", "type": "status|step|delta"}
  · status → data 是「开始训练技能：X」
  · step   → data 是阶段进度整句，**阶段边界**（粘性阶段靠它）
  · delta  → data 是内层 JSON 字符串 {"kind":"think|text|note","text":"…"}
第一版按 {"kind":…,"text":…} 取字段，实测 297 帧全是 key=None，尺子空跑（假绿）。

判定：
  A1 训练真开流：总帧数 ≥ 20 且首帧 ≤ 60s（不然用户开局就干等）
  A2 长阶段内部必须有材料：跨度 ≥ 120s 的阶段，其内材料帧必须 ≥ 3
  A3 全局最长静默 ≤ MAX_SILENCE（默认 120s）——「卡着计时」的直接反面指标
  A4 材料总量要够看：材料帧累计字符 ≥ 400
  A5 点名阶段（默认 8.5/9）有材料滚出 —— 8.5/9「裁判独立试用评分」是漏接流式的
     原发地（trialDraft 试用写稿 + judgeDraft 逐维评分都在这一段）。**它不是必跑阶段**：
     generator.go:312 的开关是 `mp != nil && len(mp.Structure.Categories) > 0`，
     也就是「素材被识别成写作手册」才会跑。素材是普通文档时它按设计不出现。
     可用 EXPECT_STAGES 覆盖。

     所以 A5 有三态，缺一不可：
       · 出现且有材料 → PASS
       · 出现却零材料 → FAIL（这才是真正的漏接流式）
       · 没出现       → 只有当日志里同时出现「5/9 未按手册处理」（=本轮确实走了通用
                        路径）才算 N/A；否则 FAIL（没在手边的解释）。
     没有第三条前置条件，「把 8.5/9 整段删掉」或「手册识别永远失败」都会让尺子
     一路绿 —— 那是空跑绿，不是通过。
  A6 抽取层活证据：解析帧必须写明「文本层直取 N / OCR M」且 M < N+M
     （可选中页不许喂 OCR —— 用户投诉②的线上出证）

用法：
  python3 web/tests/judge_stage_streaming_stats.py            # 打印报告
  python3 web/tests/judge_stage_streaming_stats.py --assert   # 断言模式，不达标 RC=1
环境变量：RAW_LOG / FRAMES_LOG / MAX_SILENCE / EXPECT_STAGES
"""
from __future__ import annotations

import json
import os
import re
import sys

RAW_LOG = os.environ.get("RAW_LOG", "/tmp/judge_live_raw.sse")
FRAMES_LOG = os.environ.get("FRAMES_LOG", "/tmp/judge_live_frames.log")
MAX_SILENCE = float(os.environ.get("MAX_SILENCE", "120"))
# 8.5/9 那段（裁判独立试用评分：试用写稿 + 逐维打分）是「漏接流式」事故的原发地，
# commit 16ce74d 之前这里是个跳秒的计时器，所以这一段必须被点名验到。
# （但只有手册模式才跑它 —— 见下面 COND_STAGES 的三态判定。）
EXPECT_STAGES = [s for s in os.environ.get(
    "EXPECT_STAGES", "8.5/9").split("|") if s]
# 「本轮没跑这个阶段」的**唯一**可接受理由：日志里出现了这个标记，说明流程真的走了
# 通用路径（8.5/9 的开关是 mp!=nil && 手册分类>0，见 generator.go:312）。
# 没这条前置，把 8.5/9 整段删掉也能绿 —— N/A 必须是**被证明过的**不适用。
COND_STAGES = {"8.5/9": "5/9 未按手册处理",
               "裁判": "5/9 未按手册处理"}
# 跨度 ≥ 该值却没滚出任何材料的阶段 → WARN（列出来给人看，但不判 FAIL：
# 有些阶段天然是机械活，跑得久不代表坏了）
WARN_SILENT_SPAN = float(os.environ.get("WARN_SILENT_SPAN", "60"))

MATERIAL_KINDS = ("think", "text", "note")
PARSE_RE = re.compile(r"文本层直取\s*(\d+)\s*/\s*OCR\s*(\d+)")


def parse_raw(path: str) -> list[dict]:
    """把 raw SSE 解成外层事件列表（不截断、按到达顺序）。"""
    with open(path, "rb") as f:
        blob = f.read().decode("utf-8", "replace")
    evs = []
    for block in blob.split("\n\n"):
        block = block.strip()
        if not block.startswith("data:"):
            continue
        try:
            evs.append(json.loads(block[5:].strip()))
        except Exception:  # noqa: BLE001
            evs.append({"type": "?", "data": block[:200]})
    return evs


def decode(ev: dict, cur_stage: str) -> tuple[str, str, str]:
    """外层事件 → (kind, stage, text)。delta 必须再解一层内层 JSON。"""
    typ = str(ev.get("type") or ev.get("kind") or "?")
    body = str(ev.get("data")) if ev.get("data") is not None else str(ev.get("text") or "")
    kind, stage = typ, cur_stage
    if typ == "delta":
        try:
            inner = json.loads(body)
        except Exception:  # noqa: BLE001
            inner = None
        if isinstance(inner, dict):
            kind = "delta:" + str(inner.get("kind") or inner.get("type") or "raw")
            stage = str(inner.get("stage") or inner.get("step")
                        or inner.get("name") or "") or cur_stage
            body = str(inner.get("text") or inner.get("delta") or inner.get("message") or "")
        else:
            kind = "delta:raw"
    return kind, stage, body


def is_material(kind: str) -> bool:
    return any(m in kind for m in MATERIAL_KINDS)


def main() -> int:
    for p in (RAW_LOG, FRAMES_LOG):
        if not os.path.isfile(p):
            print("SKIP：缺文件 %s —— 采集脚本还没跑完或没跑过" % p)
            return 0
    evs = parse_raw(RAW_LOG)
    stamps = []
    for line in open(FRAMES_LOG, encoding="utf-8"):
        m = re.match(r"^\+\s*([0-9.]+)s", line)
        if m:
            stamps.append(float(m.group(1)))
    if not evs:
        print("FAIL：raw SSE 里一帧都没有（训练根本没开流）")
        return 1 if "--assert" in sys.argv else 0
    skew = len(evs) - len(stamps)
    if skew:
        print("注意：时间戳行数(%d) 与 raw 帧数(%d) 差 %+d —— 时间列按缺失处理"
              % (len(stamps), len(evs), skew))
    el = stamps + [None] * max(0, skew)

    # 逐帧解码（粘性阶段）+ 统计
    stage_stats: dict[str, dict] = {}
    order: list[str] = []
    gaps: list[tuple[float, str, str]] = []
    prev_t = 0.0
    last_by_stage: dict[str, float] = {}
    stage_inner_gap: dict[str, float] = {}
    hist: dict[str, int] = {}
    kindseq: list[str] = []
    parse_ev: tuple[int, int] | None = None
    total = 0.0
    cur_stage = ""
    for ev, t in zip(evs, el):
        kind, stage, body = decode(ev, cur_stage)
        if str(ev.get("type") or "") == "step":
            cur_stage = body
        hist[kind] = hist.get(kind, 0) + 1
        kindseq.append(kind)
        if parse_ev is None:
            m = PARSE_RE.search(body)
            if m:
                parse_ev = (int(m.group(1)), int(m.group(2)))
        if t is None:
            continue
        total = t
        s = stage or "(无阶段)"
        d = stage_stats.setdefault(s, {"n": 0, "mat": 0, "chars": 0, "first": t, "last": t})
        if s not in order:
            order.append(s)
        d["n"] += 1
        d["last"] = t
        if is_material(kind):
            d["mat"] += 1
            d["chars"] += len(body)
        if s in last_by_stage:
            stage_inner_gap[s] = max(stage_inner_gap.get(s, 0.0), t - last_by_stage[s])
        last_by_stage[s] = t
        gaps.append((t - prev_t, kind, s))
        prev_t = t
    gaps.append((total - prev_t, "(结束)", ""))

    print("===== 线上真训练 SSE 账本 =====")
    print("总帧数 %d，采集跨度 %.1f 分钟（%.1fs）" % (len(evs), total / 60.0, total))
    print("\n帧类型直方图：")
    for k, v in sorted(hist.items(), key=lambda kv: -kv[1]):
        print("  %-18s %d" % (k, v))

    print("\n各阶段：帧数 / 材料帧 / 材料字符 / 跨度 / 阶段内最长静默")
    for s in order:
        d = stage_stats[s]
        print("  %-34s %5d %6d %8d %8.1fs %8.1fs"
              % (s[:34], d["n"], d["mat"], d["chars"], d["last"] - d["first"],
                 stage_inner_gap.get(s, 0.0)))

    gaps.sort(key=lambda g: -g[0])
    print("\n全局最长静默 TOP5（「卡着计时」的直接反面指标）：")
    for g, k, s in gaps[:5]:
        print("  %7.1fs（前帧类型=%s 阶段=%s）" % (g, k, s or "-"))

    mat_frames = [k for k in kindseq if is_material(k)]
    mat_chars = 0
    cur_stage = ""
    for ev in evs:
        kind, _st, body = decode(ev, cur_stage)
        if str(ev.get("type") or "") == "step":
            cur_stage = body
        if is_material(kind):
            mat_chars += len(body)

    max_silence = gaps[0][0] if gaps else 0.0
    long_stages = [s for s in order
                   if stage_stats[s]["last"] - stage_stats[s]["first"] >= 120]
    silent_long = [s for s in long_stages if stage_stats[s]["mat"] < 3]
    named = {p: 0 for p in EXPECT_STAGES}
    present = {p: False for p in EXPECT_STAGES}
    for s in order:
        for p in EXPECT_STAGES:
            if re.search(p, s):
                present[p] = True
                if stage_stats[s]["mat"] > 0:
                    named[p] += stage_stats[s]["mat"]

    print("\n抽取层活证据（投诉②：可选中页不许走 OCR）：")
    if parse_ev:
        print("  解析帧：文本层直取 %d 页 / OCR %d 页" % parse_ev)
    else:
        print("  ⚠ 没抓到解析帧（A6 会 FAIL）")

    # WARN（不判 FAIL）：跨度大却一帧材料都没有的阶段。机械活跑久了不算坏，
    # 但值得一眼看见——如果 8.5/9 掉进这里，A5 已经会 FAIL 了。
    silent_warn = [s for s in order
                   if stage_stats[s]["last"] - stage_stats[s]["first"] >= WARN_SILENT_SPAN
                   and stage_stats[s]["mat"] == 0]
    if silent_warn:
        print("\n⚠ WARN 跨度≥%.0fs 但零材料帧的阶段（机械活？还是又漏接流式？自己看）：" % WARN_SILENT_SPAN)
        for s in silent_warn:
            d = stage_stats[s]
            print("    %-34s 跨度 %.1fs" % (s[:34], d["last"] - d["first"]))

    print("\n===== 结论 =====")
    first_t = el[0] if el and el[0] is not None else -1.0
    checks = [
        ("A1 训练真开流（帧数≥20 且首帧≤60s）",
         len(evs) >= 20 and 0 <= first_t <= 60.0,
         "帧数=%d 首帧=%.1fs" % (len(evs), first_t)),
        ("A2 长阶段（跨度≥120s）内部都有材料",
         not silent_long, "静默长阶段=%s" % (silent_long or "无")),
        ("A3 全局最长静默≤%.0fs" % MAX_SILENCE,
         max_silence <= MAX_SILENCE, "实测最长静默=%.1fs" % max_silence),
        ("A4 材料累计≥400 字",
         mat_chars >= 400, "材料帧=%d 累计=%d 字" % (len(mat_frames), mat_chars)),
    ]
    # A5 是三态：True / False / None（None = 被证明过的 N/A，不计入 bad）。
    all_stage_text = "\n".join(order)
    for p in EXPECT_STAGES:
        if named[p] > 0:
            checks.append(("A5 点名阶段「%s」有材料滚出" % p, True, "材料帧=%d" % named[p]))
        elif present[p]:
            # 阶段出现了却一帧材料都没有：漏接流式的产品故障（事故当场就是这个形状）。
            checks.append(("A5 点名阶段「%s」有材料滚出" % p, False,
                           "阶段已出现但零材料帧 —— 漏接流式，用户只会看到计时器在跳"))
        elif p in COND_STAGES and COND_STAGES[p] in all_stage_text:
            # 没跑，且日志自证了「为什么没跑」：条件阶段在本轮不适用。
            checks.append(("A5 点名阶段「%s」有材料滚出" % p, None,
                           "N/A：本轮走到「" + COND_STAGES[p] + "」→ 该阶段按设计不执行"
                           "（不是通过，是未适用）"))
        else:
            # 既没跑、也没解释 —— 阶段被删了 / 流程在它之前就崩了 / 正则过期了。
            checks.append(("A5 点名阶段「%s」有材料滚出" % p, False,
                           "日志里既没有该阶段、也没有「%s」这条不适用理由："
                           "要么阶段被删/流程提前崩了，要么 EXPECT_STAGES 正则已过期"
                           % COND_STAGES.get(p, "（无适用条件登记）")))
    checks.append(("A6 可选中页零 OCR（解析帧写明文本层直取 N>0 且 OCR 只吃扫描页）",
                   bool(parse_ev) and parse_ev[0] > 0 and parse_ev[1] < parse_ev[0] + parse_ev[1],
                   ("文本层直取=%d OCR=%d" % parse_ev) if parse_ev else "未抓到解析帧"))
    bad = 0
    for name, ok, detail in checks:
        # 三态：ok is None = 被证明过的 N/A（阶段本轮按设计不跑）。
        # 关键：N/A **绝不许印成 PASS**（否则等于把「没验」说成「验过了」），
        # 也**不许计入 bad**（那会把「本轮本来就不该跑」当故障 —— 假红）。
        if ok is None:
            print("  %s %s（%s）" % ("N/A ", name, detail))
        elif ok:
            print("  PASS %s（%s）" % (name, detail))
        else:
            print("  FAIL %s（%s）" % (name, detail))
            bad += 1
    print("\n逐帧明细：%s\n原始 SSE：%s" % (FRAMES_LOG, RAW_LOG))
    if "--assert" in sys.argv and bad:
        print("\nRC=1：%d 项不达标" % bad)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
