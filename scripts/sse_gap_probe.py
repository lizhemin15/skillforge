#!/usr/bin/env python3
"""线上 SSE 空档探针 —— 量「用户屏幕上到底静默了多久」。

为什么需要它：界面上「一直卡着计时」是一种**主观**抱怨，说不清是哪一段静默。
这个脚本拿原始帧流当证据：每一帧记下发时间，把帧与帧之间的空档算出来，再
按「空档里有没有材料」分类。它不解析业务语义，只回答三个问题：

  1. 一轮对话里，最长的一段完全静默是多久？发生在哪个步骤？
  2. 那段静默里，后端一个字节都没发，还是发了但没有材料（只有心跳跳秒）？
  3. 材料（trace 步骤的 detail / 思考片段）到底出没出来、什么时候出来？

用法：
    python3 scripts/sse_gap_probe.py --skill 公司新闻通稿 --message "写一篇..."

输出分两段：逐帧时间线（可 --quiet 关掉）+ 空档分析（>--gap 秒的空档）。
"""
import argparse
import json
import time
import urllib.request

p = argparse.ArgumentParser()
p.add_argument("--base", default="http://127.0.0.1:8092")
p.add_argument("--skill", default="")
p.add_argument("--mode", default="manual")
p.add_argument("--message", required=True)
p.add_argument("--session", default="")
p.add_argument("--gap", type=float, default=2.0, help="只报大于这个秒数的空档")
p.add_argument("--timeout", type=float, default=900.0)
p.add_argument("--quiet", action="store_true")
p.add_argument("--out", default="", help="原始帧落盘（jsonl），供后续复核")
a = p.parse_args()

body = {
    "message": a.message,
    "session_id": a.session or ("gap-probe-" + str(int(time.time()))),
    "mode": a.mode,
    "skill": a.skill,
}
req = urllib.request.Request(
    a.base.rstrip("/") + "/api/chat",
    data=json.dumps(body).encode(),
    headers={"Content-Type": "application/json"},
)

frames = []
t0 = time.time()
resp = urllib.request.urlopen(req, timeout=a.timeout)
ev = None
for raw in resp:
    line = raw.decode("utf-8", "replace").rstrip("\n")
    now = time.time() - t0
    if line.startswith("event: "):
        ev = line[7:].strip()
        continue
    if not line.startswith("data: "):
        # 空行 / 注释行：SSE 的分帧符，不记时间（记了会把每帧算成两次）
        continue
    data = line[6:]
    frames.append({"t": now, "ev": ev or "", "data": data})

total = time.time() - t0
print("本轮总耗时 %.1fs，共 %d 帧" % (total, len(frames)))

if a.out:
    with open(a.out, "w", encoding="utf-8") as f:
        for fr in frames:
            f.write(json.dumps(fr, ensure_ascii=False) + "\n")
    print("原始帧已落盘：" + a.out)


def head(s, n=110):
    s = s.replace("\n", "\\n")
    return s[:n] + ("…" if len(s) > n else "")


if not a.quiet:
    print("\n=== 逐帧时间线 ===")
    for fr in frames:
        print("%7.2fs  %-6s %s" % (fr["t"], fr["ev"], head(fr["data"])))

# 空档分析：材料帧 = trace 帧里带 material 字段的，或 delta 帧（正文在下发）。
# 心跳帧（trace 但只有「已用 Ns」）**不算材料** —— 它正是「跳秒但没内容」的元凶。
print("\n=== 空档分析（>%.1fs） ===" % a.gap)
prev = None
gaps = []
for fr in frames:
    if prev is not None:
        d = fr["t"] - prev["t"]
        if d > a.gap:
            # 这一档里有没有材料？看空档**结束的那一帧**
            has_mat = ("material" in fr["data"]) or fr["ev"] == "delta"
            gaps.append((d, prev, fr, has_mat))
    prev = fr

if not gaps:
    print("没有 >%.1fs 的空档（说明心跳把界面撑住了；但仍要问：这期间有材料吗？）" % a.gap)
else:
    for d, before, after, has_mat in gaps:
        print(
            "空档 %5.1fs  %s → %s   空档结束时%s材料"
            % (
                d,
                before["t"],
                after["t"],
                "有" if has_mat else "**没有**",
            )
        )
        print("    前：%-6s %s" % (before["ev"], head(before["data"], 160)))
        print("    后：%-6s %s" % (after["ev"], head(after["data"], 160)))

# 材料统计：整轮里材料帧的出现时刻，用户「看见它在动」的时间轴
mats = [fr for fr in frames if "material" in fr["data"]]
print(
    "\n=== 材料帧统计 ===\nmaterial 帧 %d 个，首帧 %s，末帧 %s"
    % (
        len(mats),
        ("%.2fs" % mats[0]["t"]) if mats else "（整轮一个都没有）",
        ("%.2fs" % mats[-1]["t"]) if mats else "-",
    )
)
# 首次有内容（材料或正文）出现的时刻 —— 这是「第一眼看见东西」的客观指标
first = next((fr for fr in frames if "material" in fr["data"] or fr["ev"] == "delta"), None)
print("第一次看见内容：%s" % (("%.2fs" % first["t"]) if first else "（整轮没有）"))
