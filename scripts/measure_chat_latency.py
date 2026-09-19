#!/usr/bin/env python3
"""量一量「把新闻稿整理成 Word」这一轮到底慢在哪。

为什么要有这个脚本（而不是靠感觉）：
  用户投诉「速度过于慢了 …… 一直卡着计时」。计时器在转不等于知道时间花在哪：
  可能是模型慢、可能是文档生成慢、可能是意图路由先纠偏再跑一遍。
  只有把每一帧到达时刻记下来，才能说清是「模型在慢慢吐」还是「卡死在某一步」。
  本脚本只读线上服务（不外发、不落凭据），输出纯时延账本。
"""
import json
import os
import re
import time
import urllib.request

BASE = os.environ.get("BASE", "http://127.0.0.1:8092")
U = os.environ.get("SKILLFORGE_ADMIN_USER", "")
P = os.environ.get("SKILLFORGE_ADMIN_PASS", "")
MAXW = float(os.environ.get("MAXW", "600"))

NEWS = (
    "2026年9月18日，公司在京召开数据要素产业协同推进会。会议由副总经理李某某主持，"
    "来自产业链上下游的32家单位代表参加。会上发布了《产业数据协同白皮书》，"
    "白皮书提出到2028年建成覆盖全域的产业数据互联网，实现跨行业数据可信流通。"
    "会议还签署了6项合作协议，涉及数据交易、隐私计算与跨境数据流动等领域。"
    "公司总经理王某某在总结讲话中指出，要坚持开放协同，共同构建开放、敏捷、可信的产业数据互联网。"
)


def login():
    body = json.dumps({"username": U, "password": P}).encode()
    req = urllib.request.Request(BASE + "/api/login", method="POST", data=body)
    req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.loads(r.read().decode())["token"]


def stream_round(tok, session_id, message, label):
    body = json.dumps({"session_id": session_id, "message": message, "mode": "auto", "skill": ""}).encode()
    req = urllib.request.Request(BASE + "/api/chat", method="POST", data=body)
    req.add_header("Content-Type", "application/json")
    req.add_header("Authorization", "Bearer " + tok)
    t0 = time.time()
    first_ev = None
    first_text = None
    first_mat = None
    seen = {}
    phases = {}
    events = 0
    kinds = {}
    text_len = 0
    doc = None
    print(f"\n===== {label} =====")
    print(f"[prompt] {message[:60]}…")
    try:
        with urllib.request.urlopen(req, timeout=MAXW) as r:
            buf = b""
            while True:
                chunk = r.read1(4096) if hasattr(r, "read1") else r.read(4096)
                if not chunk:
                    break
                buf += chunk
                while b"\n\n" in buf:
                    raw, buf = buf.split(b"\n\n", 1)
                    # 服务端每帧是「event: xxx\ndata: {...}」两行 —— 不能整块拿去 startswith('data:')，
                    # 那样会把每一帧都当噪声丢掉（第一版就是这么读到 0 帧的）。
                    ev_name = ""
                    payload = ""
                    for ln in raw.decode("utf-8", "ignore").split("\n"):
                        ln = ln.strip()
                        if ln.startswith("event:"):
                            ev_name = ln[6:].strip()
                        elif ln.startswith("data:"):
                            payload += ln[5:].strip()
                    if not payload or payload == "[DONE]":
                        continue
                    events += 1
                    now = time.time()
                    if first_ev is None:
                        first_ev = now
                    try:
                        ev = json.loads(payload)
                    except Exception:
                        continue
                    if isinstance(ev, list):      # trace 帧是数组
                        ev = ev[0] if ev else {}
                        k = ev_name or "trace"
                    else:
                        k = ev.get("type") or ev.get("kind") or ev_name or "?"
                    kinds[k] = kinds.get(k, 0) + 1
                    txt = ev.get("delta") or ev.get("text") or ev.get("content") or ev.get("t") or ""
                    if isinstance(txt, str) and txt:
                        if first_text is None:
                            first_text = now
                        text_len += len(txt)
                        # 正文增量帧 = 用户在屏幕上真的看到字在变，必须计入可见变化。
                        seen.setdefault("marks", []).append((now - t0, "正文 " + txt[:24]))
                    mat = ev.get("material")
                    if mat and first_mat is None:
                        first_mat = now
                        print(f"[{now-t0:6.1f}s] \U0001f9f1 首片中间材料：{str(mat)[:100]}")
                    if k == "trace":
                        # 全量步骤时间线：每次「可见内容变化」打一行（心跳帧只有秒数变化，
                        # 不算变化）。这张表就是「慢在哪一步」的账，别再靠猜。
                        lab = str(ev.get("label") or "")
                        det = re.sub(r"（已用 \d+s）", "", str(ev.get("detail") or ""))
                        m2 = str(mat or "")
                        # 判「可见内容有没有变」必须看**尾部原文**：中间材料是截尾滚动
                        # 窗口（长度恒定、头部被挤掉、只有尾部在动）。用头部 m2[:60] 当 key，
                        # 滚动中的材料会被判成「没变化」→ 正在滚被记成静默，尺子自己造出
                        # 一个不存在的卡顿（2026-09-18 已经为这个形状付过一次代价：
                        # 39.8s 假静默）。
                        key = (lab, det, m2[-80:])
                        if key != seen.get("last"):
                            seen["last"] = key
                            seen.setdefault("marks", []).append((now - t0, f"{lab}｜{str(mat or '')[-30:]}"))
                            phases.setdefault(lab, now - t0)
                            print(f"[{now-t0:6.1f}s] ⏱ {lab}｜{det[:70]}"
                                  + (f"｜材料：{m2[:70]}" if m2 else ""))
                        if lab:
                            seen["cur"] = (lab, now)
                    blob = json.dumps(ev, ensure_ascii=False)
                    if re.search(r"\.(docx|xlsx|pptx|pdf)", blob, re.I):
                        doc = now
                        print(f"[{now-t0:6.1f}s] \U0001f4c4 交付物帧：{blob[:160]}")
                    elif doc is None and events <= 4:
                        print(f"[{now-t0:6.1f}s] 帧 {k}: {blob[:120]}")
    except Exception as e:
        print(f"!! 请求异常（{time.time()-t0:.1f}s）：{type(e).__name__} {str(e)[:160]}")
    total = time.time() - t0
    print(f"--- {label} 用时 {total:.1f}s｜帧 {events}｜首帧 {('%.1f' % (first_ev-t0)) if first_ev else '-'}s"
          f"｜首中间材料 {('%.1f' % (first_mat-t0)) if first_mat else '-'}s"
          f"｜首正文 {('%.1f' % (first_text-t0)) if first_text else '-'}s｜正文 {text_len} 字"
          f"｜交付物 {('%.1f' % (doc-t0)) if doc else '未出现'}s")
    print(f"    帧类型分布：{kinds}")
    # 真实静默账：可见变化 = trace 内容变化（尾部原文）+ 正文增量帧。
    marks = seen.get("marks") or []
    if len(marks) >= 2:
        gaps = sorted(((marks[i][0] - marks[i-1][0], marks[i-1], marks[i])
                       for i in range(1, len(marks))), key=lambda x: -x[0])
        g, a, b = gaps[0]
        print(f"    最大静默 {g:.1f}s（{a[0]:.1f}s «{a[1][:26]}» → {b[0]:.1f}s «{b[1][:26]}»）｜可见变化 {len(marks)} 次")
        for g2, a2, b2 in gaps[1:3]:
            print(f"    次大静默 {g2:.1f}s（{a2[0]:.1f}s «{a2[1][:26]}» → {b2[0]:.1f}s）")
        if os.environ.get("DUMP"):
            with open(f"/tmp/marks_{int(t0)}.jsonl", "w") as f:
                for t, s2 in marks:
                    f.write(json.dumps([round(t, 2), s2], ensure_ascii=False) + "\n")
    else:
        print(f"    ⚠ 可见变化只有 {len(marks)} 次，静默账算不出来 —— 先怀疑尺子")
    if phases:
        print("    各步首次出现时刻：" + "｜".join(f"{k}@{v:.1f}s" for k, v in phases.items()))
    return total, doc, text_len


def main():
    tok = login()
    print("登录 OK（token 已隐去）")
    sid = "lat-%d" % int(time.time())
    if os.environ.get("LEG") == "1":
        # 复刻验收腿 chat_followup_artifact_e2e 的两轮（同一 session）：
        # 第 1 轮走技能写稿，第 2 轮「把上面那篇整理成 Word」。
        # 上一条腿在这条路径上 420s 超时（rc=124），要分清是「模型慢」还是「某一步卡住」。
        p1 = ("写一篇关于星禾科技发布数据中台 3.0 的新闻稿，正文不少于 400 字，直接输出正文，不要任何解释。"
              "必须在正文第一段原样包含这个编号：锚串deadbeef（一字不许改，不许翻译，不许加空格）。")
        p2 = "把上面这篇新闻稿原样整理成 Word 文档，正文保持原样，不要重写、不要压缩。"
        t1, _, n1 = stream_round(tok, sid, p1, "第1轮（LEG 复刻）：技能写稿")
        t2, doc, n2 = stream_round(tok, sid, p2, "第2轮（LEG 复刻，同 session）：整理成 Word")
        print(f"\n===== 账本 =====\n第1轮 {t1:.1f}s；第2轮 {t2:.1f}s；交付物 {'有' if doc else '无'}")
        return
    t1, _, n1 = stream_round(tok, sid, "写一篇关于数据要素产业协同推进会的新闻稿，400 字左右。", "第1轮：生成新闻稿")
    t2, doc, n2 = stream_round(tok, sid, NEWS + "\n\n把上面这篇新闻稿原样整理成 Word 文档，正文保持原样，不要重写、不要压缩。",
                               "第2轮：整理成 Word（独立素材版）")
    print(f"\n===== 账本 =====\n第1轮 {t1:.1f}s / {n1} 字；第2轮 {t2:.1f}s；交付物 {'有' if doc else '无'}")


if __name__ == "__main__":
    main()
