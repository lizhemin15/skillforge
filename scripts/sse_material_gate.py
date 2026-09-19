#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""「只有计时器在转」的回归尺子。

盯的是用户的原始投诉：某个阻塞跳里屏幕上只有步骤标签和不断 +3 的「已用 Ns」，
材料区一片空白 —— 视觉上就是卡住。判据只有两条，都能被真故障打红：

  G1 最长静默窗口 ≤ GAP_MAX（帧与帧之间的到达间隔）
  G2 覆盖那段窗口的那一步，材料必须非空（本地旁白/思考链二者有其一即可）

为什么 G2 是必要的：G1 单看会「空跑绿」——3s 心跳一直在跳，间隔永远 3s，
但材料可以是空的，用户看到的仍然只是计时器。2026-09-19 线上复现的正是这种：
「④ 按要点执笔」滚了 117 秒、材料 (empty)。

退出码：0=绿；1=红（真故障）；2=尺子坏（没拿到任何 trace 帧 / 请求失败）。
"""
import http.client
import json
import os
import re
import sys
import time

HOST = os.environ.get("SF_HOST", "127.0.0.1")
PORT = int(os.environ.get("SF_PORT", "8092"))
# 默认 prompt 必须能真走到「执笔/起草」那一跳：素材给全（时间/主体/内容/成果），
# 否则模型会先追问要素，那条路上根本没有长时间阻塞跳 —— 尺子就会空跑绿。
PROMPT = os.environ.get("SF_PROMPT",
                        "2026年9月18日，公司在京召开数据要素产业协同推进会。会议由副总经理李某某主持，"
                        "来自产业链上下游的32家单位代表参加。会议围绕数据要素市场化配置、产业协同机制"
                        "建设进行研讨，并发布了3项合作成果。请把以上内容整理成一篇 600 字左右的新闻稿，"
                        "标题、导语、正文、结尾都要有，直接写，不要再问我要素。")
LIMIT = float(os.environ.get("SF_LIMIT", "150"))   # 秒；到点就按已收到的帧判
GAP_MAX = float(os.environ.get("SF_GAP_MAX", "6"))

BODY = json.dumps({"session_id": "gate-material-%d" % int(time.time()), "message": PROMPT},
                  ensure_ascii=False).encode()

conn = http.client.HTTPConnection(HOST, PORT, timeout=LIMIT + 30)
t0 = time.time()
try:
    conn.request("POST", "/api/chat", body=BODY, headers={"Content-Type": "application/json"})
    resp = conn.getresponse()
except Exception as e:  # noqa: BLE001
    print("尺子坏：请求发不出去", e)
    sys.exit(2)

frames = []   # (t, label, detail, material)
ev, tname = None, ""
while time.time() - t0 < LIMIT:
    line = resp.readline()
    if not line:
        break
    s = line.decode("utf-8", "replace").strip()
    if s.startswith("event:"):
        tname = s[6:].strip()
        continue
    if not s.startswith("data:"):
        continue
    t = time.time() - t0
    try:
        d = json.loads(s[5:].strip())
    except Exception:  # noqa: BLE001
        continue
    if tname == "trace" and isinstance(d, list):
        # 一帧是**整组步骤**，①和④会一起出现；必须只取「进行中的那一步」，
        # 否则同一帧被记两次、标签来回跳，段就被切成 1 帧宽（量不出静默）。
        act = [s for s in d if s.get("status") == "active"]
        pick = act[-1] if act else next((s for s in d if s.get("material")), None)
        if pick is not None:
            frames.append((t, pick.get("label", ""), pick.get("detail", ""), pick.get("material", "")))
conn.close()

if os.environ.get("SF_DUMP"):
    with open(os.environ["SF_DUMP"], "w") as fh:
        for f in frames:
            fh.write("%.1f\t%s\t%s\n" % (f[0], f[1], (f[3] or "")[:40]))
if not frames:
    print("尺子坏：一帧 trace 都没收到（不是产品慢，是尺子没接上）")
    sys.exit(2)

gap, gap_at = 0.0, 0
for i in range(1, len(frames)):
    g = frames[i][0] - frames[i - 1][0]
    if g > gap:
        gap, gap_at = g, i
tail = frames[gap_at]
mats = [f for f in frames if f[3].strip()]
print("总帧 %d，跨度 %.1fs" % (len(frames), frames[-1][0]))
print("最大静默 %.1fs @%.1fs ->「%s｜%s」" % (gap, tail[0], tail[1], tail[2][:40]))
print("带材料的帧 %d/%d；最长窗口那帧材料：%r" % (len(mats), len(frames), tail[3][:60]))

# G0：必须真走到「执笔/起草」那一跳，否则这把尺子量的是别的路径（空跑绿）。
JUMP = ("执笔", "起草", "生成正文")
hit = [f for f in frames if any(k in f[1] for k in JUMP)]
if not hit:
    print("尺子坏：整轮没出现「%s」跳，测不到本次要守的那段静默" % "/".join(JUMP))
    print("      实际经过的步骤：" + " -> ".join(dict.fromkeys(f[1] for f in frames)))
    sys.exit(2)

# G2 按「跳」逐段看：执笔/起草那一段里，除首帧外每一帧都必须有材料。
# 只看最长窗口那一帧会漏掉「大部分帧空白、偶尔来一句」的情形 —— 用户看到的
# 仍然是长时间空屏。首帧豁免是因为 clock.Set 那一刻旁白还没发出第一行。
bad, seg_empty, seg_n = [], 0, 0
cur, spans = None, []
for f in frames:
    if cur is None or f[1] != cur[0]:
        cur = (f[1], [])
        spans.append(cur)
    cur[1].append(f)
for label, fs in spans:
    if not any(k in label for k in JUMP):
        continue
    for f in fs[1:]:
        seg_n += 1
        if not f[3].strip():
            seg_empty += 1
print("执笔/起草段：内部帧 %d，其中材料空白 %d" % (seg_n, seg_empty))
if seg_n == 0:
    print("尺子坏：执笔/起草段只有首帧，量不出静默")
    sys.exit(2)
if seg_empty:
    bad.append("G2 执笔段 %d/%d 内部帧材料空白（屏幕上只剩「已用 Ns」）" % (seg_empty, seg_n))
if gap > GAP_MAX:
    bad.append("G1 静默 %.1fs > %.1fs" % (gap, GAP_MAX))
if not tail[3].strip():
    bad.append("G2b 最长窗口材料为空")
if bad:
    for b in bad:
        print("FAIL", b)
    sys.exit(1)
print("PASS G1/G2")
sys.exit(0)
