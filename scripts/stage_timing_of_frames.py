#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""从训练帧日志里重建「阶段耗时表 + 最长静默空档」，给提速做 before/after 对照。

输入是 web/tests/judge_stage_streaming_capture.py 落下的 FRAMES_LOG，格式：

    +  860.5s [step/6/9 生成骨架模板…] 7/9 挑选范文 / 生成示例…
    +  713.4s [delta/] 这一步的中间材料文本…

为什么单独抽成一个文件：**before 与 after 必须用同一份解析逻辑**。
如果 before 的账是当时手算的、after 的账是另写一段脚本算的，
那两份数字不可比 —— 差别分不清是「真的变快了」还是「尺子换了」。

它回答两件事：
  Q1 每一阶段各自吃掉多少秒（提速该省在哪一段、实际省了多少）
  Q2 每一阶段内部「屏幕最长多久没有任何新东西」（用户体感的卡顿）

用法：
    stage_timing_of_frames.py <frames.log> [--json out.json] [--label before]

退出码：能解析出 ≥1 个阶段即 0；读不到文件/一行都解析不出即 1（不当成静默绿）。
"""
from __future__ import annotations

import json
import re
import sys

# 帧行：+  860.5s [step/6/9 生成骨架模板…] 7/9 挑选范文…
#         +    1.3s [delta:think/1/9 分析参考文件…] Here
#         +  713.4s [delta:text/4/9 撰写系统提示词…] 正文串
# 注意类型里带冒号（delta:think / delta:text）—— 早先按 kind=="delta" 数材料帧，
# 数出来恒为 0，阶段表看着正常但「材料帧」那一列是假零（空跑绿的反面：假零）。
BRACKET_RE = re.compile(r"^\+\s*([\d.]+)s\s+\[([^\]]*)\]\s?(.*)$")
FRAME_RE = re.compile(r"^\+\s*([\d.]+)s\s+\[([^\]/]*)/([^\]]*)\]\s?(.*)$")
# 阶段号：'7/9 挑选范文…' 或 '5.5/9 校验交付物卫生…'（子阶段用小数）
STAGE_RE = re.compile(r"^\s*(\d+(?:\.\d+)?)/9\s+(.*)$")


def parse(path: str):
    frames = []  # (ts, kind, stage_ctx, text)
    with open(path, encoding="utf-8", errors="replace") as fh:
        for ln in fh:
            m = BRACKET_RE.match(ln.rstrip("\n"))
            if not m:
                continue
            ts = float(m.group(1))
            head = m.group(2).strip()      # 'step/6/9 …' 或 'delta:think/1/9 …'
            text = m.group(3)
            kind, _, ctx = head.partition("/")
            frames.append((ts, kind.strip(), ctx.strip(), text))
    return frames


def stage_table(frames):
    """按「next=阶段声明」的 step 帧切段，返回 [(stage, start, end, deltas, gap)]。"""
    marks = []  # (stage_label, ts)
    for ts, kind, _ctx, text in frames:
        if kind != "step":
            continue
        m = STAGE_RE.match(text)
        if m:
            marks.append((f"{m.group(1)}/9 {m.group(2).strip()}", ts))
    if not marks:
        return []
    end_all = frames[-1][0]
    out = []
    for i, (label, start) in enumerate(marks):
        end = marks[i + 1][1] if i + 1 < len(marks) else end_all
        inner = [f for f in frames if start <= f[0] <= end and f[1].startswith("delta")]
        # 阶段内最长静默：相邻帧（含本段边界）最大间隔
        ts_list = [start] + sorted({f[0] for f in frames if start <= f[0] <= end}) + [end]
        gap = max(b - a for a, b in zip(ts_list, ts_list[1:])) if len(ts_list) > 1 else 0.0
        out.append({
            "stage": label,
            "start": round(start, 1),
            "end": round(end, 1),
            "dur": round(end - start, 1),
            "think_frames": sum(1 for f in inner if f[1] == "delta:think"),
            "text_frames": sum(1 for f in inner if f[1] == "delta:text"),
            "delta_frames": len(inner),
            "max_silence": round(gap, 1),
        })
    return out


def main(argv):
    if len(argv) < 2 or argv[1].startswith("-"):
        usage = __doc__ or ""
        tail = usage.split("用法：")[1].split("\n")[0].strip() if "用法：" in usage else \
            "stage_timing_of_frames.py <frames.log> [--json out.json] [--label before]"
        print(tail)
        return 2
    path = argv[1]
    label = "run"
    json_out = None
    rest = argv[2:]
    for i, a in enumerate(rest):
        if a == "--label" and i + 1 < len(rest):
            label = rest[i + 1]
        if a == "--json" and i + 1 < len(rest):
            json_out = rest[i + 1]
    try:
        frames = parse(path)
    except OSError as e:
        print(f"FAIL 读不到帧日志 {path}：{e}")
        return 1
    if not frames:
        print(f"FAIL {path} 里一行帧都解析不出 —— 格式变了或日志是空的（不当成静默绿）")
        return 1

    total = round(frames[-1][0], 1)
    deltas = sum(1 for f in frames if f[1].startswith("delta"))
    table = stage_table(frames)
    if not table:
        print(f"FAIL {path} 里没有可识别的阶段声明帧（'N/9 …'）")
        return 1

    print(f"=== {label}：{path} ===")
    print(f"总时长 {total}s ／ 中间材料帧 {deltas} ／ 帧 {len(frames)}")
    print(f"{'阶段':34} {'起':>7} {'止':>7} {'耗时':>7} {'思考帧':>6} {'正文帧':>6} {'段内最长静默':>12}")
    for r in table:
        print(f"{r['stage'][:34]:34} {r['start']:7.1f} {r['end']:7.1f} "
              f"{r['dur']:7.1f} {r['think_frames']:6d} {r['text_frames']:6d} "
              f"{r['max_silence']:12.1f}")
    worst = max(table, key=lambda r: r["max_silence"])
    print(f"体感最差阶段：{worst['stage']} —— 静默 {worst['max_silence']}s")
    if json_out:
        json.dump({"label": label, "path": path, "total": total,
                   "delta_frames": deltas, "stages": table},
                  open(json_out, "w", encoding="utf-8"), ensure_ascii=False, indent=1)
        print(f"已落盘 {json_out}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
