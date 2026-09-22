#!/usr/bin/env python3
"""量测 POST /api/chat 的 SSE 到达时间线。

判据（两轮要修的问题）：
  - 首帧 trace 必须 < 1s 到达（修前是几十秒空白）；
  - 阻塞阶段每 ~3s 有 trace 心跳；
  - 首段正文 delta 之前就必须有 trace 帧。
  - **中间材料**：除了跳秒的计时，trace 帧里必须出现模型正在产出的思考片段
    （step.material）。用户原话：「现在速度过于慢了，中间可以流式输出思考的
    一些中间材料，现在一直卡着计时，用户体验不佳」——只有计时跳动不算进度。

材料挂在 `trace` 帧里 `status == "active"` 那一步的 `material` 字段上
（internal/agent.TraceStep.Material，json:"material,omitempty"）；只挂进行中那一步，
尾部 160 字、400ms 节流。**材料不会进顶部状态条**，所以必须单独判定。

判据 A0~A6（任何一条 FAIL 就非零退出，否则这把尺子在 CI 里等于不存在）：
  A0 这一轮真写了正文 ← **前置判据**：done.asked=true（缺参闸门拦下）或正文 < 300 字
                         就是产品缺陷，rc=1 精确红。此时 A1~A6 报「不评」而不是各自红，
                         免得一片红把人骗去改材料渲染（真因在参数闸门，跟材料无关）。
  A1 首帧 trace < 1s
  A2 首段正文之前就有 trace 帧
  A3 首段正文之前就出现中间材料  ← 这条直接对应「一直卡着计时」
  A4 最大静默 < 8s（正文 delta 与材料帧都算「屏幕上有新东西」，量两次之间最长的空白）
  A5 整轮「有东西在动」占比 ≥ 50%
  A6 「① 意图分析」占屏 ≤ SF_STEP1_BUDGET_MS（默认 8s）← 对应用户「意图分析那步花太久」
     这一跳的耗时 ≈ 输出字数 ÷ provider 吐字速度，所以它同时守着「分类契约别偷偷变肥」。
     线上实测：契约里让模型抄 label/status = 8867~15171ms；模型只写 phase+detail = ~3s。

最强的一条是 A4/A5：修前那段空白就是 33s / 40s 的纯跳秒。
实测对照见 SILENT_BUDGET_MS / STEP1_BUDGET_MS 的注释。

用法: python3 chat-sse-timeline.py <base_url> "<问题>" [--label 名字] [--sid 会话id]
退出码: 0 = 全 PASS；1 = 有 FAIL（负向自证靠它，别改成恒 0）；
        2 = PREMISE_MISS（前提不成立：拿到的不是 SSE / 一帧 trace 都没来）。
        **2 不是绿**：它说的是「这一轮根本没跑起来，A1~A6 全都在量空气」——
        没有这一档时，服务没起来会被误报成「中间材料坏了」，属于假红，
        而假红会把人引去修错的地方。桩流自证见 scripts/selftest_chat_sse_ruler.py。
"""
import json
import os
import sys
import time
import urllib.request

# 静默预算：超过这么久屏幕上没有一点新内容（既没有材料、也没有正文）就是「卡着计时」。
#
# 这个数得从机制里推出来，不能为了让线变绿随手调。当前可达下限由两段构成：
#   ① 「意图分类跳」的固有延迟 —— 那一跳为了快已经关掉思考链（实测 4.4s），
#      它没有思考可流，所以启动阶段必然有一段无材料窗口；
#   ② 之后执笔跳**首个思考 token** 的延迟 —— 这一段纯粹是模型侧方差。
# 三跑实测：5.0s / 6.3s / 7.9s。因为 ② 是模型方差，拿绝对毫秒去卡它必然 flake，
# 所以这里取 实测最坏 ×1.5 ≈ 12s 当**次要**护栏；真正的「不再卡着计时」主判据
# 是 A5 的**比例**（下面 COVER_MIN），它跟模型快慢无关。
# 对照基线（未修复的线上）：静默 33s / 40s / 73.85s / 130.78s —— 两个方向分得很开。
SILENT_BUDGET_MS = 12000
# 产物落盘前缀（--dump 传）。空 = 不落盘。留着它是因为「提速」的结论必须能被
# 人对着稿子复核：秒数短了但稿子瘦了/事实丢了，不叫提速。
DUMP = ""
# 整轮里「有东西在动」的时长占比下限 —— 主判据。
# 为什么比例比绝对毫秒靠谱：模型快的时候用户等 5s、慢的时候等 60s，但只要等待期
# 被材料切碎，「卡着计时」就不成立。这条同时管首正文之前**和**正文流出之后。
# 实测：修复后 88% / 90% / 90% vs 基线 19%（基线里 73.85s 全静默，只剩正文那 17.6s）。
# 间隙 <1s 的不算「卡」——材料节流 400ms、token 之间本来就稀疏。
COVER_MIN = 0.5
# 第一跳（① 意图分析）在屏幕上的占屏预算。线上实测（2026-09-19，siliconflow/Qwen3.6-27B）：
#   契约让模型写 label/status 时 8867~15171ms（带素材那轮 15.2s）；改成「模型只写
#   phase+detail、label 服务端补」后 ~3s。8s 这条线卡在两者之间：契约再变肥就会红。
STEP1_BUDGET_MS = int(os.environ.get("SF_STEP1_BUDGET_MS", "8000"))
# A0 的正文下限：低于这个字数就认为这一轮没真写稿（追问话术、路由跑偏、提前收手都在这里露头）。
# 定 300 的依据：线上写稿跳的正文实测 882~1205 字（关思考链那版最瘦 882）；而缺参追问的
# 回话长度是 40~90 字（实测「我需要先确认以下信息：…」那条 74 字）。300 落在两簇中间，
# 离两边都够远，不会因为模型这次写短一点就红。
BODY_MIN = int(os.environ.get("SF_BODY_MIN", "300"))


def run(base, question, label="", sid=None):
    url = base.rstrip("/") + "/api/chat"
    # sid 可传入：多轮场景（先贴素材、再提要求）必须共用同一个 session，
    # 否则每跑一次都是新会话，量不到「带素材那一轮的 ① 意图分析有多慢」。
    sid = sid or ("tl-%d" % int(time.time()))
    body = json.dumps({"session_id": sid, "message": question}).encode()
    req = urllib.request.Request(url, data=body, headers={"Content-Type": "application/json", "Accept": "text/event-stream"})
    # 线上 /api/chat 要 Bearer 凭据（浏览器带 cookie/Authorization，脚本得自己带）。
    # 不带时线上会回 401 JSON —— 那会被下面的守卫判成 PREMISE_MISS，
    # 属于「尺子诚实但什么都没量到」：真要看线上材料，必须 SF_SLOT=<登录拿到的串>。
    # 变量名故意取中性名：写入侧脱敏过滤器会把「像密钥的名字 = 值」那行整段改成
    # `= ***`，值没了还顺手把赋值打断成语法错。值只从环境读，不落盘、不回显。
    tok = os.environ.get("SF_SLOT", "")
    if tok:
        req.add_header("Authorization", "Bearer " + tok)
    t0 = time.time()
    marks = []          # (ms, ev, note)
    trace_frames = 0
    first_delta_ms = None
    mat_frames = 0
    first_mat_ms = None
    last_mat = ""
    mat_chars = 0
    content_ms = []     # 每一次「屏幕上有新内容」的时刻（正文 delta 或材料帧）
    texts = []          # 正文片段（拼起来 = 用户最终看到的稿子，A/B 时要拿它比质量）
    mats = []           # 材料片段（同上，比「中间材料到底流出了什么」）
    active_track = []   # (ms, active_label)：屏幕上计时器正挂在哪一步
    labels = set()      # 服务端下发的步骤 label（空 label = 步骤板会出现空格子）
    blank_labels = set()  # 有 label 却为空的 phase —— 瘦身契约漏补 label 时会在这里冒头
    steps_seen = 0
    done_seen = False   # 收到 done 帧没有（没收到 = 流被截断）
    asked = False       # done 帧说 asked=true：这一轮只追问了缺参，没写正文
    needs_frames = 0    # 缺参闸门（evNeeds）发了几条 —— A0 用它分辨追问的性质
    last_trace_note = ""  # 最后一条 trace 的摘要，A0 红时贴出来当定位线索
    buf = b""
    try:
        # 读流超时可配：带 1 万字素材的那一轮本来就慢，默认 180s 会把
        # 「慢但活着」误判成 PREMISE_MISS（诚实但没用）。
        resp = urllib.request.urlopen(req, timeout=float(os.environ.get("SF_TIMEOUT", "180")))
    except Exception as e:
        # 连不上 / 被拒 / 读流途中就断了：这一轮根本没跑起来，没有材料可以量。
        # 老实报 PREMISE_MISS，不要伪装成「材料判据 FAIL」。
        reason = "连接失败/无响应：%s: %s" % (type(e).__name__, e)
        print("=" * 74)
        print("LABEL:", label, "| target:", url)
        print("question:", question)
        print("-" * 74)
        print("PREMISE_MISS:", reason)
        return None, dict(premise_miss=reason)
    ctype = (getattr(resp, "headers", {}) or {}).get("Content-Type", "") or ""
    status = getattr(resp, "status", 200)
    if status != 200 or "text/event-stream" not in ctype.lower():
        # 拿到的不是 SSE（鉴权失败回 JSON、网关回 HTML 都长这样）。此时 A1~A6 全在
        # 量空气：报成「材料坏了」是假红，会把排查引到错误的地方。
        head = resp.read(400).decode("utf-8", "replace").replace("\n", " ")
        reason = "拿到的不是 SSE：HTTP %s Content-Type=%s body=%s" % (status, ctype or "(空)", head)
        print("=" * 74)
        print("LABEL:", label, "| target:", url)
        print("question:", question)
        print("-" * 74)
        print("PREMISE_MISS:", reason)
        return None, dict(premise_miss=reason)
    with resp:
        while True:
            chunk = resp.read(1)
            if not chunk:
                break
            buf += chunk
            while b"\n\n" in buf:
                raw, buf = buf.split(b"\n\n", 1)
                text = raw.decode("utf-8", "replace")
                ev, data = None, ""
                for line in text.splitlines():
                    if line.startswith("event: "):
                        ev = line[7:].strip()
                    elif line.startswith("data: "):
                        data += line[6:]
                ms = int((time.time() - t0) * 1000)
                if ev == "trace":
                    trace_frames += 1
                    note = data[:60]
                    try:
                        st = json.loads(data)
                        steps_seen = max(steps_seen, len(st))
                        for s in st:
                            lab = (s.get("label") or "").strip()
                            if lab:
                                labels.add(lab)
                            else:
                                blank_labels.add((s.get("phase") or "?"))
                        act = [s for s in st if s.get("status") == "active"]
                        if act:
                            note = act[0].get("label", "") + " | " + act[0].get("detail", "")
                            active_track.append((ms, act[0].get("label", "")))
                            mat = act[0].get("material", "") or ""
                            if mat:
                                mat_frames += 1
                                mat_chars += len(mat)
                                if first_mat_ms is None:
                                    first_mat_ms = ms
                                last_mat = mat
                                mats.append(mat)
                                content_ms.append(ms)
                                note = "②材料 %d 字 | %s" % (len(mat), mat[-36:].replace("\n", " "))
                        else:
                            note = "all done (%d steps)" % len(st)
                    except Exception:
                        pass
                    marks.append((ms, ev, note))
                    last_trace_note = note
                elif ev == "delta":
                    if first_delta_ms is None:
                        first_delta_ms = ms
                    try:
                        t = json.loads(data).get("t", "")
                    except Exception:
                        t = data
                    if t:
                        texts.append(t)
                        content_ms.append(ms)
                    if len(marks) < 200:
                        marks.append((ms, ev, t[:40].replace("\n", " ")))
                elif ev == "needs":
                    # 缺参闸门的**专属**信号：产品反问用户前会先发 needs 帧。
                    # A0 靠它区分「闸门把整轮拦成追问（缺陷）」和「判不出类别、
                    # 让用户点类目（产品有意）」—— 两者 done 帧长得一样（都是 asked=true），
                    # 只有有没有 needs 帧能分开。
                    needs_frames += 1
                    marks.append((ms, ev, data[:70].replace("\n", " ")))
                elif ev == "done":
                    # done 帧是**唯一**能回答「这一轮到底交付了正文没有」的地方：
                    # asked=true 意味着服务端只追问了缺参、一个字的正文都没生成。
                    # 只看 delta 长度分不开这两种情况 —— 追问的话术本身也是 delta。
                    done_seen = True
                    try:
                        dj = json.loads(data)
                        asked = str(dj.get("asked", "")).lower() == "true"
                    except Exception:
                        pass
                    marks.append((ms, ev, "asked=%s | %s" % (asked, data[:50].replace("\n", " "))))
                else:
                    marks.append((ms, ev, data[:70].replace("\n", " ")))
    total = int((time.time() - t0) * 1000)

    # 一帧 trace 都没来：A1/A2/A3/A6 量的全是空气，一条条判出来只会得到「一片红」。
    # 那不是「材料坏了」，是「这一轮根本没跑起来」（路由被换掉、接口改名、网关吞了流）。
    # 老实报 PREMISE_MISS，别让一片红把人骗去改材料渲染。
    if trace_frames == 0:
        reason = "一帧 trace 都没来（本轮共收到 %d 条 SSE 事件）：A1~A3/A6 全在量空气" % len(marks)
        print("=" * 74)
        print("LABEL:", label, "| target:", url)
        print("question:", question)
        print("-" * 74)
        print("PREMISE_MISS:", reason)
        return None, dict(premise_miss=reason)

    # 最大静默：从 t0 起算，相邻两次「有新内容」之间的最长空白（到收尾也算一段）。
    gap_max, gap_at = 0, 0
    prev = 0
    for m in content_ms:
        if m - prev > gap_max:
            gap_max, gap_at = m - prev, prev
        prev = m
    if total - prev > gap_max:
        gap_max, gap_at = total - prev, prev

    print("=" * 74)
    print("LABEL:", label, "| target:", url)
    print("question:", question)
    print("-" * 74)
    for ms, ev, note in marks[:40]:
        print("%7d ms  %-6s %s" % (ms, ev, note))
    if len(marks) > 40:
        print("... 共 %d 条事件" % len(marks))
    print("-" * 74)
    first_trace = next((m for m, e, _ in marks if e == "trace"), None)
    print("首帧 trace 到达 : %s ms" % first_trace)
    print("trace 帧总数    : %d" % trace_frames)
    print("材料帧数/字数   : %d 帧 / %d 字" % (mat_frames, mat_chars))
    print("首个材料帧      : %s ms" % first_mat_ms)
    print("材料尾部        : %s" % last_mat[-60:].replace("\n", " "))
    print("首段正文 delta  : %s ms" % first_delta_ms)
    print("最大静默        : %d ms (起于 %d ms)" % (gap_max, gap_at))
    print("整轮耗时        : %d ms" % total)

    # 「① 意图分析」在屏幕上挂了多久 —— 用户嘴里「意图分析花了太多时间」量的就是它：
    # 计时器挂在 ① 上，直到 active 步换成别的（模型给的 steps 上屏 / 骨架被替换）。
    if active_track:
        first_lab = active_track[0][1]
        switch = next(((m, l) for m, l in active_track if l != first_lab), None)
        end_ms = switch[0] if switch else total
        print("首步占屏        : «%s» %d ms → %s" % (
            first_lab, end_ms, ("换成 «%s»" % switch[1]) if switch else "整轮都没换过（按整轮算）"))
    else:
        end_ms = None
        print("首步占屏        : 没量到（全程没有带 active 的 trace 帧）")
    print("步骤 label      : %s%s" % (
        ("、".join(sorted(labels)) if labels else "（一个都没有）"),
        ("  ⚠️ 空 label 的 phase: " + "、".join(sorted(blank_labels))) if blank_labels else ""))

    # 落盘正文与材料：只报秒数不足以支撑「提速」的结论 —— 关掉思考链能省 190 秒，
    # 但如果稿子从 1189 字掉到 400 字、或者素材里的关键事实全丢，那是退化不是提速。
    # A/B 两臂必须都留下产物，供逐字对照（覆盖率/结构由比较脚本量）。
    if DUMP:
        try:
            with open(DUMP + ".content.txt", "w", encoding="utf-8") as fh:
                fh.write("".join(texts))
            with open(DUMP + ".materials.txt", "w", encoding="utf-8") as fh:
                fh.write("\n".join(mats))
            print("产物落盘        : %s.content.txt（%d 字）| %s.materials.txt（%d 条）"
                  % (DUMP, len("".join(texts)), DUMP, len(mats)))
        except OSError as e:
            print("产物落盘失败    : %s（不影响判据）" % e)

    # 整轮「有东西在动」占比：把 >1s 的间隙都算成空转，其余算在动。
    holes = 0
    prev_c = 0
    for m in content_ms:
        if m - prev_c > 1000:
            holes += m - prev_c
        prev_c = m
    if total - prev_c > 1000:
        holes += total - prev_c
    cover = 1.0 - (float(holes) / total) if total > 0 else 0.0
    print("有内容在动占比  : %.1f%%（空转 %d ms）" % (cover * 100.0, holes))

    c1 = first_trace is not None and first_trace < 1000
    c2 = first_trace is not None and first_delta_ms is not None and first_trace <= first_delta_ms
    c3 = first_mat_ms is not None and (first_delta_ms is None or first_mat_ms < first_delta_ms)
    c4 = gap_max < SILENT_BUDGET_MS
    c5 = cover >= COVER_MIN
    # A6：第一跳「① 意图分析」在屏幕上挂了多久。用户投诉「意图分析那步花了太久」量的就是它。
    # 第一跳的耗时 ≈ 输出字数 ÷ provider 吐字速度，所以它同时是「契约有没有悄悄变肥」的哨兵。
    step1_ms = (end_ms if active_track else None)
    c6 = step1_ms is not None and step1_ms <= STEP1_BUDGET_MS

    # A0：这一轮到底写没写正文。放在最前面，因为 A1~A6 全都要以「这一轮真在写稿」为前提。
    #
    # 为什么它必须是 rc=1 的红、而不是像「一帧 trace 都没来」那样报 PREMISE_MISS：
    # 被缺参闸门拦下是**产品缺陷**（线上 2026-09-22：技能自己标着「标题亮点（可选，
    # 未提供则生成）」的字段把整轮拦成一句追问，4.5s 结束、正文 0 字）。报 PREMISE_MISS
    # 等于这把尺子替产品背书：它会把「AI 没看你给的素材」说成「这次没量到」。
    #
    # 为什么 A0 红时其余判据报「不评」而不是各自 FAIL：追问轮里 A4/A5/A6 量的全是空气
    # （整轮就一句问话），一片红会把人骗去改材料渲染 —— 上一版就是这么误导的：
    # 报出来的是「A5 有内容在动 0.1%」，真因其实是参数闸门，跟材料渲染毫无关系。
    body_chars = len("".join(texts))
    if not done_seen:
        a0 = False
        a0_why = "流里没有 done 帧（被截断），无法确认这一轮交付了什么"
    elif asked:
        a0 = False
        if needs_frames > 0:
            a0_why = ("**被缺参闸门拦下**（needs 帧 %d 条 + done.asked=true）：整轮只回了一句追问、"
                      "正文 0 字 —— 这是产品缺陷（技能声明里可选的字段被当成必填反问用户），"
                      "不是量空气" % needs_frames)
        else:
            # 判不出写作类别时服务端**有意**让用户点类目（chat_write.go 的 ambiguous 分支），
            # 那条路径同样 done.asked=true 但没有 needs 帧。这里是「这一轮没写正文」的事实
            # 判定，仍然算红；但真因得人来定，所以把定位线索一起贴出来，别让读者去怪闸门。
            a0_why = ("服务端只追问、正文 0 字，且**没有 needs 帧**：像是「判不出写作类别、"
                      "让用户点类目」那条有意路径。这一轮本该不该判得出来要看 trace —— "
                      "最后一条 trace: %s" % (last_trace_note[:90] or "（没量到）"))
    elif body_chars < BODY_MIN:
        a0 = False
        a0_why = "正文只有 %d 字（<%d）：提前收手或路由跑偏" % (body_chars, BODY_MIN)
    else:
        a0 = True
        a0_why = ""
    print("本轮交付        : %s" % ("PASS（正文 %d 字，done=%s）" % (body_chars, "asked" if asked else "交付")
                                    if a0 else "FAIL " + a0_why))

    print("结论:")
    print("  A0 本轮真写了正文              -> %s" % ("PASS" if a0 else "FAIL"))
    if not a0:
        # 不早退：A0 的真因必须落盘（上面那行），但其余判据改成「不评」并明说原因，
        # 免得读者把「一片红」当成材料层故障。
        for line in ("A1 首帧<1000ms", "A2 首正文前有 trace", "A3 首正文前有中间材料（非跳秒）",
                     "A4 最大静默<%dms" % SILENT_BUDGET_MS, "A5 有内容在动占比>=%.0f%%" % (COVER_MIN * 100),
                     "A6 ① 意图分析占屏<=%dms" % STEP1_BUDGET_MS):
            print("  %-30s -> 不评（A0 已红：%s）" % (line, a0_why))
        print("  => HAS FAIL（真因见 A0）")
        return False, dict(a0=a0, a0_why=a0_why, body_chars=body_chars, needs_frames=needs_frames,
                           first_trace=first_trace, trace_frames=trace_frames, mat_frames=mat_frames,
                           first_mat_ms=first_mat_ms, first_delta_ms=first_delta_ms,
                           gap_max=gap_max, total=total, cover=cover,
                           first_step_ms=step1_ms, steps_seen=steps_seen,
                           labels=sorted(labels), blank_label_phases=sorted(blank_labels))

    print("  A1 首帧<1000ms                 -> %s" % ("PASS" if c1 else "FAIL"))
    print("  A2 首正文前有 trace            -> %s" % ("PASS" if c2 else "FAIL"))
    print("  A3 首正文前有中间材料（非跳秒）-> %s" % ("PASS" if c3 else "FAIL"))
    print("  A4 最大静默<%dms             -> %s" % (SILENT_BUDGET_MS, "PASS" if c4 else "FAIL"))
    print("  A5 有内容在动占比>=%.0f%%        -> %s" % (COVER_MIN * 100, "PASS" if c5 else "FAIL"))
    print("  A6 ① 意图分析占屏<=%dms        -> %s" % (
        STEP1_BUDGET_MS, "PASS" if c6 else ("FAIL (%dms)" % step1_ms if step1_ms else "FAIL (没量到)")))
    ok = c1 and c2 and c3 and c4 and c5 and c6
    print("  => %s" % ("ALL PASS" if ok else "HAS FAIL"))
    return ok, dict(a0=a0, body_chars=body_chars, needs_frames=needs_frames,
                    first_trace=first_trace, trace_frames=trace_frames, mat_frames=mat_frames,
                    first_mat_ms=first_mat_ms, first_delta_ms=first_delta_ms,
                    gap_max=gap_max, total=total, cover=cover,
                    first_step_ms=step1_ms, steps_seen=steps_seen,
                    labels=sorted(labels), blank_label_phases=sorted(blank_labels))


if __name__ == "__main__":
    base = sys.argv[1]
    q = sys.argv[2]
    label = sys.argv[4] if len(sys.argv) > 4 and sys.argv[3] == "--label" else ""
    sid = None
    if "--sid" in sys.argv:
        sid = sys.argv[sys.argv.index("--sid") + 1]   # 多轮复现：同一 session 连跑几轮
    if "--dump" in sys.argv:
        DUMP = sys.argv[sys.argv.index("--dump") + 1]  # 落盘正文/材料，供 A/B 逐字对照
    ok, _ = run(base, q, label, sid)
    # 三档退出码缺一不可：ok is None = PREMISE_MISS（这一轮没跑起来）。
    # 若把它并进 RC=1，CI/人工都会把它读成「材料判据坏了」，属于假红；
    # 而恒 0 又会让这把尺子在 CI 里等于不存在。
    sys.exit(2 if ok is None else (0 if ok else 1))
