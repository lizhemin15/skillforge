#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""线上真训练取证：长阶段里到底有没有「流式中间材料」滚出来。

背景（用户原话）：「现在速度过于慢了，中间可以流式输出思考的一些中间材料，现在一直卡着计时，
用户体验不佳」——训练一跑二十来分钟，如果阶段边界只发一条进度、阶段内部是分钟级静默模型调用，
界面上就只剩一个跳秒的计时器。

本脚本不依赖浏览器，直接对训练接口发一次真训练（带**前几页扫描 + 后面可选中**的混合 PDF，
即用户投诉过的那种料），把整条 SSE 连同**每帧到达的相对秒**落盘，然后回答两个问题：

  Q1 素材层：可选中页是否零 OCR 直解析、扫描页是否只 OCR 那几页（材料帧里有无解析统计）
  Q2 用户体验层：**最长的静默间隔**是多久？8.5/9 裁判段内部有没有材料滚出来？

用法（凭据只从环境变量来，不落盘、不打印）：
  set -a; . /opt/skillforge/skillforge.env; set +a
  ADMIN_USER="$SKILLFORGE_ADMIN_USER" ADMIN_PASS="$SKILLFORGE_ADMIN_PASS" \
  BASE=http://127.0.0.1:8092 TRAIN_NAME='线上长阶段流式取证' \
  MATERIAL=/tmp/mixed_scan_vector.pdf \
  python3 web/tests/judge_stage_streaming_capture.py

产物：
  /tmp/judge_live_frames.log   逐帧「+123.4s [类型/阶段] 文本…」（人工可读）
  /tmp/judge_live_raw.sse      原始字节（供 stats_of_sse.py 复核，避免弱断言）
  stdout                       末段汇总：帧类型直方图、各阶段材料帧数、最长静默间隔

本脚本只**采集**，不做判定；判定用的阈值必须从这次真实采到的分布里定，不许拍脑袋。
"""
from __future__ import annotations

import json
import os
import sys
import time
import urllib.error
import urllib.request
from typing import NoReturn

BASE = os.environ.get("BASE", "http://127.0.0.1:8092").rstrip("/")
USER = os.environ.get("ADMIN_USER", "")
PASS = os.environ.get("ADMIN_PASS", "")
TRAIN_NAME = os.environ.get("TRAIN_NAME", "线上长阶段流式取证")
MATERIAL = os.environ.get("MATERIAL", "/tmp/mixed_scan_vector.pdf")
REQUIREMENT = os.environ.get("REQUIREMENT", (
    "把这份设备操作手册训练成一个技能：技能要能根据用户给出的故障现象，"
    "从手册里定位到对应章节，输出该章节的处置步骤，并给出注意事项。"
    "请务必把手册里的章节结构与关键步骤整理进技能。"
))
FRAMES_LOG = os.environ.get("FRAMES_LOG", "/tmp/judge_live_frames.log")
RAW_LOG = os.environ.get("RAW_LOG", "/tmp/judge_live_raw.sse")


def die(msg: str, code: int = 2) -> NoReturn:
    print(msg, file=sys.stderr)
    sys.exit(code)


def login() -> str:
    if not USER or not PASS:
        die("SKIP：需要 ADMIN_USER / ADMIN_PASS 环境变量（从 /opt/skillforge/skillforge.env source）")
    body = json.dumps({"username": USER, "password": PASS}).encode()
    req = urllib.request.Request(
        BASE + "/api/login", data=body,
        headers={"Content-Type": "application/json"}, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            tok = json.loads(r.read().decode()).get("token", "")
    except Exception as e:  # noqa: BLE001
        die("SKIP：登录失败（%s）—— 先确认线上服务在跑、凭据对" % type(e).__name__)
    if not tok:
        die("SKIP：登录返回里没有 token")
    print("ok   已登录（凭据走环境变量，未落盘、未打印）")
    return tok


def build_multipart(name: str, requirement: str) -> tuple[bytes, str]:
    if not os.path.isfile(MATERIAL):
        die("SKIP：素材不存在 %s" % MATERIAL)
    boundary = "----sfboundary%d" % int(time.time() * 1000)
    with open(MATERIAL, "rb") as f:
        blob = f.read()
    parts = []
    for field, value in (("name", name), ("category", ""), ("description", ""),
                         ("requirement", requirement)):
        parts.append(("--%s\r\nContent-Disposition: form-data; name=\"%s\"\r\n\r\n%s\r\n"
                      % (boundary, field, value)).encode())
    parts.append(("--%s\r\nContent-Disposition: form-data; name=\"files\"; filename=\"%s\"\r\n"
                  "Content-Type: application/pdf\r\n\r\n"
                  % (boundary, os.path.basename(MATERIAL))).encode())
    parts.append(blob)
    parts.append(("\r\n--%s--\r\n" % boundary).encode())
    return b"".join(parts), "multipart/form-data; boundary=%s" % boundary


def main() -> int:
    tok = login()
    payload, ctype = build_multipart(TRAIN_NAME, REQUIREMENT)
    print("ok   素材 %s（%.1f MB）→ POST %s/api/admin/train"
          % (os.path.basename(MATERIAL), len(payload) / 1048576.0, BASE))

    req = urllib.request.Request(
        BASE + "/api/admin/train", data=payload, method="POST",
        headers={"Content-Type": ctype, "Authorization": "Bearer " + tok})
    # 服务端同一时刻只允许一个训练任务（HTTP 409「已有训练任务在运行」）。
    # 取证脚本不能因为「别人正在跑」就红着退出——那不是被测系统坏了，
    # 而是本脚本没等锁。等锁上限 TRAIN_WAIT_MIN 分钟，超时才 FAIL。
    wait_min = float(os.environ.get("TRAIN_WAIT_MIN", "30"))
    deadline = time.time() + wait_min * 60
    while True:
        t0 = time.time()
        try:
            resp = urllib.request.urlopen(req, timeout=3600)
            break
        except urllib.error.HTTPError as e:
            body = e.read().decode("utf-8", "replace")[:400]
            if e.code == 409 and time.time() < deadline:
                left = int(deadline - time.time())
                print("… 服务端已有训练在跑，%ds 后重试（剩余等待预算 %ds）" % (45, left))
                time.sleep(45)
                continue
            die("FAIL：训练请求被拒 HTTP %d：%s" % (e.code, body), 1)
    frames = []          # (elapsed, kind, stage, text)
    raw_fh = open(RAW_LOG, "wb")
    with resp, open(FRAMES_LOG, "w", encoding="utf-8") as fh:
        buf = b""
        last_kind_stage = ("", "")
        cur_stage = ""        # 粘性阶段：step 帧宣告阶段边界，其后 delta 都归它
        while True:
            chunk = resp.read(1)
            if not chunk:
                break
            raw_fh.write(chunk)
            raw_fh.flush()
            buf += chunk
            while b"\n\n" in buf:
                block, buf = buf.split(b"\n\n", 1)
                text = block.decode("utf-8", "replace")
                if not text.startswith("data:"):
                    continue
                data = text[5:].strip()
                try:
                    ev = json.loads(data)
                except Exception:  # noqa: BLE001
                    ev = {"raw": data[:200]}
                el = time.time() - t0
                # 真实外层 schema（internal/api/admin.go 的 SSE 写出口，实测）：
                #   {"data": "<正文>", "type": "status|step|delta"}
                #   type=status → data 是「开始训练技能：X」这类整句
                #   type=step   → data 是阶段进度整句（阶段边界，用来做粘性阶段）
                #   type=delta  → data 是**内层 JSON 字符串**，真正的材料事件在里面
                #                 （{"kind":"think|text|note","text":"…"}），必须再解一层，
                #                 否则取到的全是空串，会把「有材料」误判成「没材料」。
                #   注：字段名是 data/type，不是 kind/text —— 第一版按 kind/text 取，
                #       实测 297 帧全部 key=None，尺子等于空跑（假绿）。
                typ = str(ev.get("type") or ev.get("kind") or "")
                body_txt = (str(ev.get("data")) if ev.get("data") is not None
                            else str(ev.get("text") or ""))
                kind = typ
                stage = cur_stage
                if typ == "delta":
                    try:
                        inner = json.loads(body_txt)
                    except Exception:  # noqa: BLE001
                        inner = None
                    if isinstance(inner, dict):
                        kind = "delta:" + str(inner.get("kind") or inner.get("type") or "raw")
                        stage = str(inner.get("stage") or inner.get("step")
                                    or inner.get("name") or "") or cur_stage
                        body_txt = str(inner.get("text") or inner.get("delta")
                                       or inner.get("message") or "")
                    else:
                        kind = "delta:raw"   # 裸文本增量，不是 JSON 信封
                if typ == "step":
                    cur_stage = body_txt
                if not body_txt:
                    body_txt = json.dumps(ev, ensure_ascii=False)[:200]
                frames.append((el, kind, stage, body_txt))
                fh.write("+%7.1fs [%s/%s] %s\n" % (el, kind, stage, body_txt[:300]))
                fh.flush()
                if kind or stage:
                    last_kind_stage = (kind, stage)

    raw_fh.close()
    total = time.time() - t0
    print("\n===== 采集完成 用时 %.1f 分钟，共 %d 帧 =====" % (total / 60.0, len(frames)))

    hist = {}
    for _, kind, stage, _ in frames:
        hist[kind or "(无类型)"] = hist.get(kind or "(无类型)", 0) + 1
    print("\n帧类型直方图：")
    for k, v in sorted(hist.items(), key=lambda kv: -kv[1]):
        print("  %-10s %d" % (k, v))

    print("\n各阶段：帧数 / 材料类帧数 / 该阶段最长静默")
    order, stage_stats = [], {}
    for el, kind, stage, _ in frames:
        s = stage or "(无阶段)"
        if s not in stage_stats:
            stage_stats[s] = {"n": 0, "mat": 0, "first": el, "last": el}
            order.append(s)
        d = stage_stats[s]
        d["n"] += 1
        d["last"] = el
        if kind in ("think", "text", "note") or kind.startswith("delta:think") or kind.startswith("delta:text") or kind.startswith("delta:note"):
            d["mat"] += 1
    prev = 0.0
    gaps = []
    for el, kind, stage, _ in frames:
        gaps.append((el - prev, prev, kind, stage))
        prev = el
    gaps.append((total - prev, prev, "(结束)", ""))

    # 每阶段内部最长静默（只看该阶段相邻帧）
    per_stage_gap = {}
    last_t = {}
    for el, kind, stage, _ in frames:
        s = stage or "(无阶段)"
        if s in last_t:
            g = el - last_t[s]
            if g > per_stage_gap.get(s, 0.0):
                per_stage_gap[s] = g
        last_t[s] = el
    for s in order:
        d = stage_stats[s]
        print("  %-22s 帧%-4d 材料%-4d 跨度%6.1fs 阶段内最长静默 %6.1fs"
              % (s, d["n"], d["mat"], d["last"] - d["first"], per_stage_gap.get(s, 0.0)))

    gaps.sort(key=lambda g: -g[0])
    print("\n全局最长静默 TOP5（这是「卡着计时」的直接反面指标）：")
    for g, after, kind, stage in gaps[:5]:
        print("  %7.1fs 静默（前一个帧类型=%s 阶段=%s）" % (g, kind or "-", stage or "-"))

    print("\n材料帧总字符数：%d" % sum(len(t) for _, k, _, t in frames if "think" in k or "text" in k or "note" in k))
    print("逐帧明细：%s\n原始 SSE：%s" % (FRAMES_LOG, RAW_LOG))
    return 0


if __name__ == "__main__":
    sys.exit(main())
