#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""「中间材料是整段在长，不是一行在抖」的线上尺子。

盯的是用户的原始投诉：「现在速度过于慢了，中间可以流式输出思考的一些中间材料，
现在一直卡着计时，用户体验不佳」。

修复前的线上形态（本尺子必须能把它打红）：后端只下发 material —— **160 字单行尾巴**，
前端每帧原地替换。屏幕上就是「一行字在原地抖 + 计时在涨」，dump 里 25 帧全是同一个
长度的尾巴，且每帧开头都被从词中间劈开（「兰察布风电场的…」）。

修复后的形态：material_log —— 1200 字滚动窗口、首字对齐到句读、**只往尾部加**。

判据（前提不成立就不评后面的任何一条，避免空跑绿）：

  L1 前提：真走到「执笔/起草」跳，且该段出现非空 material_log（字段都没有 = 修复没上线）
  L2 段内只增：同一跳的连续帧里，后一帧必须**含住**前一帧的尾巴（见 material_kept）
     ——证明是同一段文字在长，而不是每帧换一段新文字
  L3 段内至少 3 个不同取值（真在长，不是每帧同一个值）
  L4 段内最长 ≥ 400 字（**这条是修复前那版的死穴**：material 上限 160 字，
     C 端无论怎么看都是那一条尾巴）

退出码：0=绿；1=红（断言没通过）；2=尺子坏（没拿到帧 / 请求失败 / 没走到要守的那一跳 /
本轮没走到带思考链的路径）。2 和 1 必须分开：把「尺子没接上」算成红，会让人去改断言；
算成绿，会让人以为功能好了。

环境变量: SF_HOST / SF_PORT / SF_PROMPT / SF_LIMIT / SF_LOG_MAX / SF_DUMP /
          SF_DUMP_JSON / SF_ATTEMPTS（默认 2，第一次若没走到思考路径就重发一次）

--selftest：不开模型，用合成帧序列自证这把尺子的区分力（见 self_test）。
"""
import http.client
import json
import os
import sys
import time

HOST = os.environ.get("SF_HOST", "127.0.0.1")
PORT = int(os.environ.get("SF_PORT", "8092"))
# 默认 prompt 必须真走到「执笔」那一跳，否则量的是别的路径（空跑绿）。素材给全
# （时间/主体/内容/成果），模型才不会先追问要素直接进写作。
PROMPT = os.environ.get("SF_PROMPT",
                        "2026年9月20日，公司在京召开数据要素产业协同推进会。会议由副总经理李某某主持，"
                        "来自产业链上下游的32家单位代表参加。会议围绕数据要素市场化配置、产业协同机制"
                        "建设进行研讨，并发布了3项合作成果。请把以上内容整理成一篇 800 字左右的新闻稿，"
                        "标题、导语、正文、结尾都要有，直接写，不要再问我要素。")
LIMIT = float(os.environ.get("SF_LIMIT", "150"))    # 秒；到点按已收帧判
LOG_MAX = int(os.environ.get("SF_LOG_MAX", "400"))  # L4 门槛（离 1200 上限留足余量）
JUMP = ("执笔", "起草", "生成正文")

# ---------------------------------------------------------------- 纯函数（可自证）
# 旁白的三种形态，来自后端源码（material_filter.go / classify_narration.go /
# chat_note.go）。识别旁白是**判据的一部分**：材料的滚动窗口把旁白当分隔符用，
# 于是「材料正文」和「旁白」是两种东西，判「只增」只能在正文上判。
NARRATION_HEAD = ("· ", "模型思考中…", "已装配：", "收到你的需求")


def is_narration(line):
    s = line.strip()
    return any(s.startswith(h) for h in NARRATION_HEAD)


def strip_align_marker(v):
    """摘掉窗口首部的「…」。

    它不是内容，是 appendMaterialWindow 在窗口顶到 1200 后按句读**前移**首字时加的可读性
    标记（下一次 append 又会被 TrimLeft 掉）。判据里留着它，就会出现「前一帧结尾是正文、
    后一帧开头是 …」→ 逐字前缀比对直接归零 —— 2026-09-22 的假红就是这么来的。
    """
    return v.lstrip("…")


def strip_trailing_narration(v):
    """摘掉**尾部**的旁白行（可能连着好几行，也可能整帧就只有旁白）。

    后端每次 append 都是 dropTrailingNarration(mat)+text：旧旁白被摘掉、新旁白接上，
    所以「前一帧的尾巴是旁白」而「后一帧里没有这句旁白」是**合法**的，不是换了一段文字。
    整帧只有旁白时结果为空串 —— 那是「本帧没有材料正文」，不是「有一行 21 字的正文」，
    自证 P8 就是被这个区分卡住的（漏了它，无思考链的一轮会被判成红而不是尺子坏）。
    """
    parts = v.split("\n")
    while parts and is_narration(parts[-1]):
        parts.pop()
    return "\n".join(parts).strip()


def material_body(v):
    """一帧 material_log 的「材料正文」—— 尺子的所有判据都在它上面判。"""
    return strip_trailing_narration(strip_align_marker(v or ""))


def material_kept(prev, cur, slack=300):
    """前一帧的正文尾巴有多少字被后一帧**含住**（含在哪里不管）。

    为什么不是「后一帧的前缀 == 前一帧的后缀」：窗口顶到上限后前部会被丢掉、首字按句读
    前移，前面还可能有旁白行被替换 —— 三件事都会让**前缀**变样，但正文尾巴一直在。
    只要「前帧尾巴 X 字」能在后帧里找到，就是同一段文字在长；找不到就是真换代（缓冲重置）。

    返回 (含有多少字, 需要多少字)。
    """
    p, c = material_body(prev), material_body(cur)
    if not p or not c:
        return None
    need = max(0, min(len(p), len(c)) - slack)
    if need == 0:
        return (0, 0)                      # 太短，判不出，不算红也不给绿
    return (1 if p[-need:] in c else 0, need)


def judge(frames, verbose=True):
    """frames = [(t, label, detail, material, material_log)]，返回 (rc, 输出行)。"""
    out = []

    def say(s):
        out.append(s)

    if not frames:
        return 2, ["尺子坏：一帧 trace 都没收到（不是产品慢，是尺子没接上）"]

    say("总帧 %d，跨度 %.1fs，收到材料日志的帧 %d" %
        (len(frames), frames[-1][0], sum(1 for f in frames if f[4].strip())))

    # 分「跳」成段：同一 label 连续出现算一段（换跳时后端的日志会被清空，跨段不评）。
    segs, cur = [], None
    for f in frames:
        if cur is None or f[1] != cur[0]:
            cur = (f[1], [])
            segs.append(cur)
        cur[1].append(f)
    write_segs = [s for s in segs if any(k in s[0] for k in JUMP)]
    if not write_segs:
        say("尺子坏：整轮没出现「%s」跳，本次要守的那段没量到" % "/".join(JUMP))
        say("      实际经过：%s" % " -> ".join(dict.fromkeys(f[1] for f in frames)))
        return 2, out

    # L1 前提：执笔段里有没有 material_log 这个字段的非空值。
    logs = [(lbl, f[4]) for lbl, fs in write_segs for f in fs if f[4].strip()]
    if not logs:
        # 这一步红，等于「修复没上线 / 材料日志被掐了」——正是修复前那版的形态。
        say("L1 ✗ 执笔/起草段里一个非空 material_log 都没有")
        mx = max((len(f[3]) for lbl, fs in write_segs for f in fs), default=0)
        say("   该段最长 material 单行尾巴 = %d 字（修复前上限 160：这正是「一行字在抖」）" % mx)
        best = max((f for lbl, fs in write_segs for f in fs), key=lambda f: len(f[3]), default=None)
        if best:
            say("   最长那条尾巴原文：%r" % best[3][:120])
        return 1, out
    say("L1 ✓ 执笔/起草段带 material_log 的帧 %d（最长 %d 字）"
        % (len(logs), max(len(v) for _, v in logs)))

    # L1b 前提：日志里得有**材料正文**，不能全是旁白。
    # 「一个非空 material_log 都没有」和「日志全是旁白」是两件事：后者多半是本轮没走到
    # 带思考链的路径（写稿跳默认关思考，见 llm 请求的 reason=0），此时屏幕上是正文在
    # 流式，用户并不缺可读材料 —— 不是产品病，是本次没量到，算尺子坏（2）。
    bodies = [(lbl, material_body(f[4])) for lbl, fs in write_segs for f in fs]
    bodies = [(lbl, b) for lbl, b in bodies if b]
    if not bodies:
        say("尺子坏：执笔段的 material_log 全是旁白（没有材料正文）")
        say("      多半是本轮路由到「无思考链」的写稿路径（后端日志形如 reason=0），"
            "屏幕上正文自己在流式，用户不缺可读材料。重跑；若每次都这样才需要查产品。")
        say("      实际经过：%s" % " -> ".join(dict.fromkeys(f[1] for f in frames)))
        return 2, out

    fails = []
    for lbl, fs in write_segs:
        raw = [f[4] for f in fs if material_body(f[4])]
        # 门槛按**帧数**（有材料正文的帧），不是按「不同取值数」：每帧同一个值=没在长，
        # 那种段的取值数只有 1，按取值数当门槛会把「没在长」整段跳过 → 判据失灵
        # （自证 P7 就是被这个卡住的：期望红，实得绿）。
        if len(raw) < 2:
            continue
        vals = list(dict.fromkeys(material_body(v) for v in raw))
        worst = None
        for a, b in zip(raw, raw[1:]):
            if a == b:
                continue
            kept = material_kept(a, b)
            if kept is None:
                continue
            got, need = kept
            if need and not got:
                worst = (a, b, need)
                break
        if worst:
            fails.append("L2 段「%s」不是只往尾部加：前一帧正文的尾巴 %d 字在后一帧里找不到"
                         "（后一帧换了一段新文字）" % (lbl, worst[2]))
            # 光有「找不着」判不出病因：是缓冲被重置（内容换代），还是窗口首部按句读前移
            # 到旁白之后（同一段文字，只是前部换了个切点）。两种都会让前缀全变，必须把
            # 两端原文摆出来看 —— 2026-09-22 就是只留了 60 字前缀，卡在这儿只能猜。
            print("   前帧正文尾 80：%r" % material_body(worst[0])[-80:])
            print("   后帧正文头 80：%r" % material_body(worst[1])[:80])
            print("   前帧正文头 80：%r" % material_body(worst[0])[:80])
            continue
        if len(vals) < 3:
            fails.append("L3 段「%s」只有 %d 个不同取值（每帧同一个值 = 没在长）" % (lbl, len(vals)))
        if max(len(v) for v in vals) < LOG_MAX:
            fails.append("L4 段「%s」最长材料正文只有 %d 字 < %d（修复前那版上限就是 160）"
                         % (lbl, max(len(v) for v in vals), LOG_MAX))

    for f in fails:
        say("✗ " + f)
    if fails:
        say("\n尺子红：%d 条。用户看到的是「一行字在抖 + 计时在涨」。" % len(fails))
        return 1, out
    say("L2/L3/L4 ✓ 各段：%s" % "，".join(
        "%s=%d字/%d帧" % (lbl, max((len(material_body(f[4])) for f in fs), default=0), len(fs))
        for lbl, fs in write_segs))
    return 0, out


# ---------------------------------------------------------------- 自证（不开模型）
def _frames(pairs):
    return [(i * 0.4, "⑤ 按要点执笔", "", "", v) for i, v in enumerate(pairs)]


def _corpus(n, zhu="数据要素市场化配置与产业协同机制建设"):
    """造 n 句**互不相同**的材料文本。

    为什么不能用 mat * k（同一句反复）：反复文本自带周期性，任意错位都能凑出长公共子串，
    于是「逐字前缀」那种错模型在它上面**也是绿的** —— 2026-09-22 收口时就是这样漏掉的：
    P2 用 mat*40 造「带 … 前移」，把判据退回修复前那版，自证竟然还是全绿（只有 P3 变红）。
    夹具必须和线上真材料同构：一句话里带序号，句句不重。
    """
    z = [zhu, "产业链上下游协同攻关", "绿色算力与储能配套", "公共数据授权运营试点"]
    return "".join("%d号事项围绕%s展开研讨，形成%d条可落地举措。" % (i, z[i % len(z)], i + 1)
                   for i in range(n))


def self_test():
    """用合成帧序列证明这把尺子**能变红也能变绿**（每条判据都得有反例）。

    不能只靠线上跑：一次 40~50s 且受模型路由影响，判据本身对没对没人守着。
    每条反例都要**指名道姓**打在某一条判据上（L1/L2/L3/L4），否则「红了」也判不出病因。
    """
    mat = "公司召开数据要素产业协同推进会发布3项合作成果，32家产业链上下游单位代表与会。"
    cases = []

    # P1 修复后形态：纯材料尾部增长（无旁白、无前移），必须绿。
    # 帧长要**越过 L4 门槛**（LOG_MAX=400）：360 字的夹具在真判据下该红（没长够），
    # 留短了这条自证自己就会红 —— 夹具得跟判据同一个门槛。
    cases.append(("P1 纯材料尾部增长", 0, _frames([_corpus(20), _corpus(30), _corpus(45)])))

    # P2 窗口顶到上限后首部按句读前移（**每帧带前导「…」**），必须绿。
    # 这是线上 A/B 那次假红的真形态：前一帧结尾是正文、后一帧开头是「…」，
    # 逐字比前缀的旧模型在这里**必然归零**（正文里没有第二个「…」能对上）。
    #
    # 三帧，不是两帧：只有两帧时取值数=2，L2 一放过就会被 L3（<3 个取值）接手报红 ——
    # 红是红了，红的却不是 L2，于是「L2 的判绿方向」没人验。帧数必须让 L3/L4 都没话说。
    # 前移量取 250 字（**小于 slack 300**）：真实产品每帧只前移一小片，越过 slack 的
    # 大跳本来就该判红，拿它当「合法前移」的夹具会把判据的手脚绑起来。
    A = _corpus(40)
    B = _corpus(12, "绿电交易")
    C = _corpus(6, "数据信托")
    cases.append(("P2 满窗口首部前移（带 … 标记）", 0,
                  _frames(["…" + A, "…" + A[250:] + B, "…" + A[250:] + B + C])))

    cases.append(("P3 尾部旁白被新旁白替换", 0,
                  _frames([_corpus(20), _corpus(30) + "\n模型思考中…已产出 239 字（已 2s）",
                           _corpus(45) + "\n模型思考中…已产出 571 字（已 5s）"])))
    cases.append(("P4 材料正文长得够长（L4）", 0, _frames([_corpus(10), _corpus(20), _corpus(40)])))

    # P5 修复前那版形态：只有 160 字单行尾巴、没有 material_log → L1 必须红。
    old = [(i * 0.4, "④ 按要点执笔", "", "兰察布风电场的" + mat * 2, "") for i in range(5)]
    cases.append(("P5 修复前（无 material_log，只有 160 字尾巴）", 1, old))

    # P6 真·缓冲重置：三帧三件**互不相干**的长文本 → **L2 单独**必须红。
    # 这个夹具的形状是刻意的，两个坑都踩过：
    #   ① 反例必须**真换文字**：写成 mat*20 → mat*40 那是继续长（同一段在长），该绿不该红；
    #   ② 只有两帧时，L2 一旦放过就轮到 L3（2 个取值 < 3）报红 —— 红是红了，但红的不是 L2，
    #      于是「L2 的判红方向」其实没人验：把 material_kept 改成永远返回「含住了」，
    #      自证照样全绿。所以这里给**三个**取值、且每帧都过 LOG_MAX，
    #      让 L3/L4 都没话说，红只能从 L2 出。
    cases.append(("P6 缓冲被重置（换成不相干文字，L2 单独判红）", 1,
                  _frames([_corpus(38, "煤炭产能置换"), _corpus(40, "海上风电并网"),
                           _corpus(42, "数据跨境流动")])))


    # P7 每帧同一个值（没在长）→ L3 必须红。
    cases.append(("P7 每帧同一个值", 1, _frames([mat * 20, mat * 20, mat * 20, mat * 20])))

    # P8b 材料在长、但**没长够**（最长 360 字 < 400）→ L4 单独必须红。
    # 这条是给「L4 的判红方向」上的钉子：没有它，把门槛从 400 调到 160（= 修复前那版
    # 上限），自证照样 14/14 全绿 —— L4 到底还会不会响，没人验。
    # 帧长刻意夹在 160~400 之间：低于 160 的话连糯化后的门槛也能打红，就分不出对错了。
    cases.append(("P8b 材料没长够（360 字 < L4 门槛 400）", 1,
                  _frames([_corpus(3), _corpus(6), _corpus(9)])))

    # P8 本轮没走到思考链（日志全是旁白）→ 尺子坏(2)，不是红。
    cases.append(("P8 日志全是旁白（本轮无思考链）", 2,
                  _frames(["模型思考中…已产出 86 字（已 2s）", "模型思考中…已产出 120 字（已 3s）"])))

    # P9 没走到执笔跳 → 尺子坏(2)。
    cases.append(("P9 没走到执笔跳", 2,
                  [(0.4, "① 解析需求", "", "", ""), (0.8, "② 构思要点", "", "", "材料")]))

    # ---- body() 的契约断言（不是帧序列，是函数契约）----------------------------
    # 为什么单列：摘「…」和摘尾部旁白这两件事**在 L2 上是不承重的**。
    #   · 摘「…」：比对改成「子串任意位置命中」以后，前导「…」永远不会落进被比的那段尾巴
    #     （need = min(p,c) - 300，尾巴从 index 300 往后取），所以它不影响任何 L2 判决；
    #   · 摘尾部旁白：这个**承重**（P3/P8 就是它撑着的）。
    # 不承重的实现细节如果没人钉，就会被下一个人「看着没用」删掉，或者被人误以为
    # 「戴了它就安全」而绕开真判据。所以在这里把契约直接写成等式：
    # 变异实验 M4（body 不再摘「…」）在帧序列上是全绿的，只有这几条会红。
    mat_line = "0号事项围绕数据要素市场化配置与产业协同机制建设展开研讨，形成1条可落地举措。"
    contract = [
        ("C1 body 摘掉前导 …", material_body("…" + mat_line), mat_line),
        ("C2 body 摘掉尾部旁白行", material_body(mat_line + "\n模型思考中…已产出 12 字（已 1s）"), mat_line),
        ("C3 body 摘掉连着的多行旁白",
         material_body(mat_line + "\n模型思考中…已产出 12 字（已 1s）\n· 正在校对"), mat_line),
        ("C4 整帧只有旁白时 body 为空", material_body("模型思考中…已产出 12 字（已 1s）"), ""),
        ("C5 空帧不炸", material_body(""), ""),
    ]
    bad = 0
    for name, got_v, want_v in contract:
        if got_v != want_v:
            bad += 1
            print("✗ %s（期望 %r，实得 %r）" % (name, want_v[:60], got_v[:60]))
        else:
            print("✓ %s" % name)

    for name, want, fr in cases:
        got, lines = judge(fr, verbose=False)
        mark = "✓" if got == want else "✗"
        if got != want:
            bad += 1
        print("%s %s（期望 %d，实得 %d）" % (mark, name, want, got))
        if got != want:
            for ln in lines:
                print("      " + ln)
    total = len(cases) + len(contract)
    if bad:
        print("\n尺子自证红：%d/%d 条判据的方向不对 —— 这把尺子现在判不了对错。" % (bad, total))
        return 1
    print("\n尺子自证绿：%d/%d（绿/红/尺子坏三档方向都对，body 契约也在钉子上）" % (total, total))
    return 0


# ---------------------------------------------------------------- 线上取帧
def fetch(attempt):
    body = json.dumps({"session_id": "gate-matlog-%d-%d" % (int(time.time()), attempt),
                       "message": PROMPT}, ensure_ascii=False).encode()
    try:
        conn = http.client.HTTPConnection(HOST, PORT, timeout=LIMIT + 30)
        conn.request("POST", "/api/chat", body=body,
                     headers={"Content-Type": "application/json"})
        resp = conn.getresponse()
    except Exception as e:  # noqa: BLE001
        return None, "尺子坏：请求发不出去 %s" % e

    frames = []   # (t, label, detail, material, material_log)
    tname = ""
    t0 = time.time()
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
            # 一帧是**整组步骤**（①和④会一起出现）—— 只取「进行中的那一步」，
            # 否则同一帧被记两次、标签来回跳，段被切碎，L2 会在段边界上假红。
            act = [x for x in d if x.get("status") == "active"]
            pick = act[-1] if act else next(
                (x for x in d if x.get("material_log") or x.get("material")), None)
            if pick is not None:
                frames.append((t, pick.get("label", ""), pick.get("detail", ""),
                               pick.get("material", "") or "",
                               pick.get("material_log", "") or ""))
    conn.close()
    return frames, None


def dump(frames):
    if os.environ.get("SF_DUMP"):
        with open(os.environ["SF_DUMP"], "w") as fh:
            for f in frames:
                fh.write("%.1f\t%s\tmat=%d\tlog=%d\t%s\n"
                         % (f[0], f[1], len(f[3]), len(f[4]),
                            f[4][:60].replace("\n", "⏎")))
    # SF_DUMP_JSON：整段日志原文（不截断）。60 字前缀在 L2 变红时**不足以判**「是缓冲被重置」
    # 还是「窗口前部按句读前移了」——两者都会让前缀全变。2026-09-22 就是栽在这上面：
    # 只有前缀，肉眼分不清，只能靠猜。留一份全量才查得动。
    if os.environ.get("SF_DUMP_JSON"):
        with open(os.environ["SF_DUMP_JSON"], "w") as fh:
            for f in frames:
                fh.write(json.dumps({"t": round(f[0], 1), "label": f[1], "detail": f[2],
                                     "material": f[3], "material_log": f[4]},
                                    ensure_ascii=False) + "\n")


def main():
    if "--selftest" in sys.argv:
        return self_test()

    attempts = max(1, int(os.environ.get("SF_ATTEMPTS", "2")))
    last = None
    for i in range(attempts):
        frames, err = fetch(i)
        if err:
            print(err)
            return 2
        dump(frames)
        rc, lines = judge(frames)
        for ln in lines:
            print(ln)
        if rc != 2 or i == attempts - 1 or "思考链" not in "\n".join(lines):
            return rc
        # rc=2 且原因是「本轮没走到带思考链的路径」→ 值得重发一次：模型路由有方差，
        # 而这条路径恰恰是材料窗口存在的唯一理由，量不到就没法下结论。
        print("—— 第 %d 次没走到带思考链的路径，重发一次（模型路由有方差）\n" % (i + 1))
        last = rc
    return last if last is not None else 2


if __name__ == "__main__":
    sys.exit(main())
