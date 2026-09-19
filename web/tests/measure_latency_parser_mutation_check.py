#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""measure_chat_latency.py 解析层的确定性尺子（合成料，秒级，不用真跑、不要活服务）。

为什么需要它：2026-09-19 我拿 `scripts/measure_chat_latency.py` 量「慢在哪一步」，
它报出「首中间材料 0.0s，最大静默 66.3s，各步首次出现时刻只有 ① 意图分析」。
我照着这份账去追「服务端没把材料发给前端」，追了一轮 —— 全是假的。

真因在解析层：trace 帧是个**数组**，脚本写的是 `ev = ev[0]`。
数组第 0 位永远是**最早那一步**（① 意图分析，已经 done、没有 material），
而「计时器此刻挂在哪一步」「中间材料」都挂在 **active** 那步上。于是尺子
既看不见步骤推进、也看不见材料帧 → 把「屏幕一直在滚」报成 66.3s 静默。
原始 SSE 帧实测：首材料 0.0s、材料滚到 69.0s、最大静默 **3.6s**。

教训：**尺子自己也会撒谎，而且撒谎时指向的是别人**。所以这里给解析层配一把
合成料的尺子。关键是每一条断言都配了**对照物**（老实现必须在那条料上失败）——
否则「断言恒真」和「料喂不进解析层」长得一模一样，分不出来。

跑法：python3 web/tests/measure_latency_parser_check.py
"""
import importlib.util
import os
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
SCRIPT = os.path.join(ROOT, "scripts", "measure_chat_latency.py")

bad = []
n = 0


def check(cond, msg):
    global n
    n += 1
    print(("  PASS " if cond else "  FAIL ") + msg)
    if not cond:
        bad.append(msg)
    return cond


def load_mod():
    """把出货脚本当模块加载进来 —— 测的是真跑的那份解析逻辑，不是这里抄的第二份。"""
    if not os.path.exists(SCRIPT):
        print("!! 找不到 %s —— 尺子自己定位不到被测对象，算 FAIL" % SCRIPT)
        sys.exit(1)
    spec = importlib.util.spec_from_file_location("measure_chat_latency", SCRIPT)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def pick_old(steps):
    """**老实现**（出过事的那版）：直接取数组第 0 位。只当对照物用，不许被修好。"""
    s = steps[0] if steps else {}
    if isinstance(s, list):
        s = s[0] if s else {}
    return s, [(x.get("label") or "") if isinstance(x, dict) else "" for x in steps]


def main():
    m = load_mod()
    for fn in ("pick_active_step", "trace_visible_key", "top_silences"):
        if not hasattr(m, fn):
            check(False, "出货脚本必须有解析函数 %s()" % fn)
    if bad:
        print("\n=== 结论 ===\n尺子缺零件，后面不用跑了")
        return 1

    print("\n【F1】材料挂在 active 那步（历史假静默的形状）")
    f1 = [
        {"label": "① 意图分析", "status": "done", "detail": "识别意图"},
        {"label": "④ 按要点执笔", "status": "active", "detail": "按要点写正文", "material": "要点一：标题含年会全称"},
    ]
    s, labs = m.pick_active_step(f1)
    check(s.get("label") == "④ 按要点执笔", "取到的是 active 那一步（拿到 %r）" % s.get("label"))
    check(bool(s.get("material")), "active 那步的材料真的取到了（拿到了才有可见变化）")
    check(labs == ["① 意图分析", "④ 按要点执笔"], "全部步骤 label 都收进时间线（%r）" % (labs,))
    so, _ = pick_old(f1)
    check(so.get("label") == "① 意图分析" and not so.get("material"),
          "对照物：老实现 ev[0] 在这条料上必然看不到材料与真步骤（复现该 bug）")

    print("\n【F2】切步瞬间：材料还留在刚做完那一步上")
    f2 = [
        {"label": "① 意图分析", "status": "done", "material": "已装配：未命中技能手册，走通用写作"},
        {"label": "③ 构思要点", "status": "active", "detail": "先把这一篇怎么写想清楚"},
    ]
    s2, _ = m.pick_active_step(f2)
    check(s2.get("label") == "③ 构思要点", "步骤认的是 active 那格（%r）" % s2.get("label"))
    check("未命中技能手册" in str(s2.get("material") or ""),
          "材料从上一格兜底取回，切步那一刻不算静默")

    print("\n【F3】异常形状不许崩")
    s3, l3 = m.pick_active_step([])
    check(s3 == {} and l3 == [], "空数组 → 空 step，不抛异常")
    s3b, l3b = m.pick_active_step([None, "垃圾", {"label": "① 意图分析", "status": "done"}])
    check(s3b.get("label") == "① 意图分析" and l3b == ["① 意图分析"],
          "非 dict 元素被滤掉、label 表不被污染（labels=%r）" % (l3b,))

    print("\n【F4】心跳帧不算「可见变化」")
    k1, *_ = m.trace_visible_key({"label": "① 意图分析", "detail": "正在理解你的问题…（已用 3s）", "material": "abc"})
    k2, *_ = m.trace_visible_key({"label": "① 意图分析", "detail": "正在理解你的问题…（已用 9s）", "material": "abc"})
    check(k1 == k2, "只有「已用 Ns」在跳 → key 不变（否则静默账被心跳填满、等于没量）")

    print("\n【F5】材料尾部在滚必须算「可见变化」")
    base = "甲" * 160
    k3, *_ = m.trace_visible_key({"label": "④ 按要点执笔", "status": "active", "material": base + "第一段"})
    k4, *_ = m.trace_visible_key({"label": "④ 按要点执笔", "status": "active", "material": base + "第二段"})
    check(k3 != k4, "截尾滚动窗口里尾部变了 → key 必须变（2026-09-18 的 39.8s 假静默就是这个）")

    print("\n【F6】端到端静默账：合成一份「材料一直在滚、正文迟到」的真账")
    # 真实现场：0.0s 起材料每 0.4s 滚一次（滚到 69.0s，共 ~170 帧），正文 66.3s 才来。
    frames = []
    for i in range(173):
        t = round(i * 0.4, 2)
        frames.append((t, "trace", {
            "label": "① 意图分析" if t < 9.4 else "④ 按要点执笔",
            "status": "active",
            "detail": "按要点写正文…（已用 %ds）" % int(t),
            "material": ("构思要点 " * 40 + "第 %d 段" % i)[-200:],
        }))
    for j in range(30):                       # 66.3s 起，正文 delta 帧
        frames.append((round(66.3 + j * 0.2, 2), "delta", {"t": "字"}))
    frames.sort(key=lambda x: x[0])

    marks = []
    seen_last = None
    for t, k, ev in frames:
        if k == "delta":
            marks.append((t, "正文 " + ev["t"]))
            continue
        st, _ = m.pick_active_step([ev] if isinstance(ev, list) else
                                   [{"label": "① 意图分析", "status": "done"}, ev])
        key, lab, _det, _mat = m.trace_visible_key(st)
        if key != seen_last:
            seen_last = key
            marks.append((t, lab))
    gaps = m.top_silences(marks, 1)
    g = gaps[0][0] if gaps else None
    check(g is not None and g < 5.0, "最大静默 %.1fs（期望 <5s；真实现场实测 3.6s）" % (g if g is not None else -1))

    print("\n【F7】对照物：同一份合成料，老实现必须报出假静默")
    old_marks, seen_last = [], None
    for t, k, ev in frames:
        if k == "delta":
            old_marks.append((t, "正文 " + ev["t"]))
            continue
        st, _ = pick_old([{"label": "① 意图分析", "status": "done"}, ev])
        key, lab, _det, _mat = m.trace_visible_key(st)
        if key != seen_last:
            seen_last = key
            old_marks.append((t, lab))
    old_gaps = m.top_silences(old_marks, 1)
    old_g = old_gaps[0][0] if old_gaps else None
    check(old_g is not None and old_g > 60.0,
          "老实现报出 %.1fs 静默（复现 66.3s 假账）→ 说明 F6 的断言有判别力，不是恒绿"
          % (old_g if old_g is not None else -1))

    print("\n=== 结论 ===")
    if bad:
        print("%d/%d 不合格：" % (len(bad), n))
        for b in bad:
            print("  ✗ " + b)
        return 1
    print("全部 %d 条合格：解析层认 active 步、材料不丢、静默账不会被心跳或滚动骗。" % n)
    return 0


if __name__ == "__main__":
    sys.exit(main())
