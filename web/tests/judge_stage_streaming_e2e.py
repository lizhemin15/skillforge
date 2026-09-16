#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""训练页「长阶段流式」线上 leg —— 把采集半边和判定半边串起来，真跑一次训练。

# LIVE-LEGS: train_stream TIMEOUT_S=2100 TRAIN_WAIT_MIN=25

为什么要有这个文件（而不是让 runner 直接跑那两个脚本）：
  `judge_stage_streaming_capture.py`（一字节不丢地落盘 + 每帧到达的相对秒）和
  `judge_stage_streaming_stats.py --assert`（判 A1~A6：真开流 / 长阶段有材料 /
  最长静默 / 材料总量 / 点名阶段 / 可选中页零 OCR）此前**谁也没调用** ——
  runner 只枚举 `*_e2e.py`，而那两个是 `*_capture.py` / `*_stats.py`，
  正好落在枚举之外。于是「8.5/9 阶段 353.3 秒零帧」那次事故里，仓库里明明躺着
  能抓住它的尺子，报告却是全绿。这就是「0 引用的尺子」的典型下场。

本文件只做三件事：造测试料 → 跑采集 → 跑判定，并把判定结果数成 runner 认的
`--- N/M ok ---`。**判定逻辑一行都不在这里重写** —— 抄的那份永远不会红。

前提（缺哪个就自己造，造不出来就红，不许拿 SKIP 混过去）：
  · 真模型在线：本 leg 会真建一次技能，约 12~25 分钟
  · ADMIN_USER / ADMIN_PASS：管理员凭据（只走环境变量，不落盘、不打印）
  · 测试料：优先用 MATERIAL 指定的 pdf；否则用仓库脚本**现造**混合料
    （前 2 页扫描 + 后 6 页可选中）。必须现造，不能捡 /tmp 里上轮遗留的那份 ——
    否则「文本层直取 6 / OCR 2」这条证据就变成「上次跑的时候碰巧留下的」，
    A6（可选中页零 OCR）会退化成空跑绿。
"""
import os
import re
import subprocess
import sys
import tempfile
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
PY = sys.executable or "python3"
CAPTURE = ROOT / "web/tests/judge_stage_streaming_capture.py"
STATS = ROOT / "web/tests/judge_stage_streaming_stats.py"
BUILDER = ROOT / "testdata/mixed/build_mixed_pdf.py"

# 采集脚本自己的内部等待预算（分钟）。runner 的 TIMEOUT_S 是外层硬砍，
# 这里比它小一档，让脚本有时间自己收尾并打印「采集完成」，而不是被砍得没话说。
TRAIN_WAIT_MIN = os.environ.get("TRAIN_WAIT_MIN", "25")
# runner 逐腿超时（秒）。直接 `python3 本文件` 跑时没有这个变量，用下面的默认推导。
TIMEOUT_S = os.environ.get("TIMEOUT_S", "").strip()
MATERIAL = os.environ.get("MATERIAL", "").strip()

ok = 0
total = 0


def line(ok_, name, extra=""):
    global ok, total
    total += 1
    if ok_:
        ok += 1
        print("  ok   %s%s" % (name, (" — " + extra) if extra else ""))
    else:
        print("  FAIL %s%s" % (name, (" — " + extra) if extra else ""))
    return ok_


def main() -> int:
    print("训练页长阶段流式 leg：BASE=%s" % os.environ.get("BASE", "http://127.0.0.1:8092"))

    user = os.environ.get("ADMIN_USER", "")
    pw = os.environ.get("ADMIN_PASS", "")
    line(bool(user and pw), "管理员凭据在位（ADMIN_USER/ADMIN_PASS，不打印值）",
         "" if (user and pw) else "缺凭据 —— 这条 leg 没法建技能，不算通过")

    # 外层超时预算：默认 = 采集等待 + 3 分钟余量（含造料、判定）。
    try:
        wait_min = float(TRAIN_WAIT_MIN)
    except ValueError:
        print("TRAIN_WAIT_MIN=%r 不是数字" % TRAIN_WAIT_MIN)
        return 2
    budget = int(TIMEOUT_S) if TIMEOUT_S.isdigit() else int(wait_min * 60 + 180)
    print("  单腿预算：%ds（TIMEOUT_S=%s / TRAIN_WAIT_MIN=%s 分钟）"
          % (budget, TIMEOUT_S or "未设（直接跑本文件）", TRAIN_WAIT_MIN))

    tmp = Path(tempfile.mkdtemp(prefix="stream_leg_"))
    frames = str(tmp / "frames.log")
    raw = str(tmp / "raw.sse")

    material = MATERIAL
    if not material:
        material = str(tmp / "mixed_scan2_text6.pdf")
        r = subprocess.run([PY, str(BUILDER), material, "--scan", "2", "--text", "6",
                            "--tag", "SFLEG"], cwd=str(ROOT),
                           capture_output=True, text=True)
        print("  造料：%s" % (r.stdout.strip() or r.stderr.strip()[:300]))
    line(os.path.isfile(material), "测试料在位（混合型 PDF：扫描页 + 可选中页）", material)

    env = dict(os.environ)
    env.update({
        "MATERIAL": material,
        "TRAIN_NAME": "线上流式leg%s" % time.strftime("%m%d%H%M"),
        "TRAIN_WAIT_MIN": TRAIN_WAIT_MIN,
        "FRAMES_LOG": frames,
        "RAW_LOG": raw,
    })

    print("\n===== 采集半边：真建一次技能，逐帧记到达时刻（约 %s 分钟，别急）=====" % TRAIN_WAIT_MIN)
    t0 = time.time()
    cap = subprocess.run([PY, str(CAPTURE)], cwd=str(ROOT), env=env,
                         capture_output=True, text=True, timeout=budget)
    print(cap.stdout.rstrip())
    if cap.stderr.strip():
        print("[stderr] %s" % cap.stderr.strip()[-1500:])
    print("  采集用时 %.1fs" % (time.time() - t0))
    line(cap.returncode == 0, "采集半边跑完（rc=0）", "rc=%d" % cap.returncode)

    if not (os.path.isfile(frames) and os.path.isfile(raw)):
        line(False, "采集产物落盘（frames.log / raw.sse）", "缺文件，判定半边会空跑")
        print("\n--- %d/%d ok ---" % (ok, total))
        return 1

    print("\n===== 判定半边：A1~A6（判定逻辑全在 judge_stage_streaming_stats.py 里）=====")
    st = subprocess.run([PY, str(STATS), "--assert"], cwd=str(ROOT), env=env,
                        capture_output=True, text=True, timeout=180)
    out = st.stdout
    print(out.rstrip())
    if st.stderr.strip():
        print("[stderr] %s" % st.stderr.strip()[-1500:])

    # 判定半边自己吐的 PASS/FAIL 逐条搬进来 —— 不在本文件里重算，也不做「至少有一条 PASS」这种软判断。
    verdicts = re.findall(r"^\s*(PASS|FAIL)\s+(A\d[^\n（(]*)", out, re.M)
    line(bool(verdicts), "判定半边给出了逐条断言（不是空跑）",
         "抓到 %d 条 A* 断言" % len(verdicts) if verdicts else "一条断言都没抓到 —— 尺子空转，不算通过")
    for tag, name in verdicts:
        line(tag == "PASS", name.strip())

    print("\n采集产物：%s\n原始 SSE：%s" % (frames, raw))
    print("\n--- %d/%d ok ---" % (ok, total))
    return 0 if ok == total else 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except subprocess.TimeoutExpired as e:
        print("超时：%s（外层预算内没跑完 —— 别把超时当通过）" % (e.cmd[0] if e.cmd else "?"))
        print("\n--- %d/%d ok ---" % (ok, total))
        sys.exit(1)
