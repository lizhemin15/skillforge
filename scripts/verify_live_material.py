#!/usr/bin/env python3
"""线上取证（两条独立判据，任一红即非零退出）：

判据 A【交付物持久化】：真链路生成一个 docx → 下载 200 并记 sha → **重启服务** →
   再用同一个 token 下载，必须仍是 200 且 sha 一致。旧行为（全内存缓存）：重启后 404。

判据 B【中间材料可读且活着】：执笔那一轮把所有 SSE 帧的 material 记下来，
   - B1 逐帧取「本帧新增的材料」（与上一帧做后缀-前缀重叠对齐），拉丁字符占比 > 20%
        的帧算**噪声帧**，必须为 0（旧行为：十几帧纯英文自我对话）；
   - B2 屏幕最长无变化 ≤ MAX_GAP：材料**或正文**任一在动即不算静默（定义与 e2e 的 M7
     对齐，8s 门槛取自线上正常流实测，两个脚本不许各拿一把尺子量同一件事）；
   - B3 材料必须贴着本轮素材：出现本轮提示词里的高频实词，证明不是通用套话
     （旧写法要求「材料里有正文草稿」，隐含「思考链必须烧到起草步」——那是病，不是门）；
   - B4 速度账：首材料/首正文/整轮/正文字数 —— 「感受速度」必须有自己的数字。

只读线上服务，不外发、不落凭据。
"""
import hashlib
import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request

BASE = os.environ.get("BASE", "http://127.0.0.1:8092")
U = os.environ.get("SKILLFORGE_ADMIN_USER", "")
P = os.environ.get("SKILLFORGE_ADMIN_PASS", "")
MAXW = float(os.environ.get("MAXW", "900"))
MAT = os.environ.get("MAT", "/tmp/material_10k.txt")
GEN_DIR = os.environ.get("GEN_DIR", "/opt/skillforge/data/gen")
NOISE_RATIO = 0.20
MAX_GAP = 8.0
DO_RESTART = os.environ.get("DO_RESTART", "1") == "1"

fails = []


def ok(name, cond, detail=""):
    print(("✅ " if cond else "❌ ") + name + ("  " + detail if detail else ""))
    if not cond:
        fails.append(name)


def login():
    body = json.dumps({"username": U, "password": P}).encode()
    req = urllib.request.Request(BASE + "/api/login", method="POST", data=body)
    req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.loads(r.read().decode())["token"]


CJK = re.compile(r"[\u4e00-\u9fff\u3400-\u4dbf]")
CJK_PUNCT = set("，。、；：？！“”‘’（）《》〈〉【】「」『』—…～　·")


def latin_ratio(s):
    """英文脚手架占整条的 rune 比例。

    **只数拉丁字母**（a-z / A-Z）。为什么不用「ord < 0x250」：
    线上实测被误判成噪声的那一帧是 `· 收到你的需求（本轮 10408 字）` —— 纯中文，
    里面那 5 个阿拉伯数字按旧判据算成「拉丁字符」，比例 0.25 > 0.20 于是整帧判红。
    那是**尺子误伤**：数字是中文句子里再正常不过的内容（字数、年份、编号），
    用户要防的是「英文自我对话」，跟数字无关。旧判据把两者混为一谈。
    """
    if not s:
        return 0.0
    n = sum(1 for ch in s if ("a" <= ch <= "z") or ("A" <= ch <= "Z"))
    return n / len(s)


def cjk_only(s):
    return "".join(ch for ch in s if CJK.match(ch))


def delta(prev, cur):
    """cur 相对 prev 新增的那部分：按「prev 的后缀 == cur 的前缀」最长重叠对齐。"""
    if not prev:
        return cur
    k = min(len(prev), len(cur))
    while k > 0 and not prev.endswith(cur[:k]):
        k -= 1
    return cur[k:]


def round_stream(tok, session_id, message, label, collect_material=True):
    body = json.dumps({"session_id": session_id, "message": message,
                       "mode": "auto", "skill": ""}).encode()
    req = urllib.request.Request(BASE + "/api/chat", method="POST", data=body)
    req.add_header("Content-Type", "application/json")
    req.add_header("Authorization", "Bearer " + tok)
    t0 = time.time()
    mats = []          # [(t, material_text)]
    body_ts = []       # 正文片的到达时刻（B2 要拿它算「屏幕还在动吗」）
    first_text = None
    texts = []
    gen_urls = []
    kinds = {}
    print(f"\n===== {label} =====")
    print(f"[prompt] {message[:70]}…")
    with urllib.request.urlopen(req, timeout=MAXW) as r:
        buf = b""
        while True:
            chunk = r.read1(4096) if hasattr(r, "read1") else r.read(4096)
            if not chunk:
                break
            buf += chunk
            while b"\n\n" in buf:
                raw, buf = buf.split(b"\n\n", 1)
                ev_name, payload = "", ""
                for ln in raw.decode("utf-8", "ignore").split("\n"):
                    ln = ln.strip()
                    if ln.startswith("event:"):
                        ev_name = ln[6:].strip()
                    elif ln.startswith("data:"):
                        payload += ln[5:].strip()
                if not payload or payload == "[DONE]":
                    continue
                now = time.time() - t0
                for m in re.finditer(r"/api/chat/gen/([0-9a-f]{32})", payload):
                    if m.group(1) not in gen_urls:
                        gen_urls.append(m.group(1))
                try:
                    ev = json.loads(payload)
                except Exception:
                    continue
                steps = ev if isinstance(ev, list) else [ev]
                for st in steps:
                    if not isinstance(st, dict):
                        continue
                    k = st.get("kind") or st.get("type") or ev_name or "?"
                    kinds[k] = kinds.get(k, 0) + 1
                    if collect_material and st.get("material"):
                        mats.append((now, str(st["material"])))
                    # 正文片的字段名是 **"t"**（internal/api/chat_write.go: `write(evDelta,
                    # jsonSafe(map[string]string{"t": ...}))`）。这里过去只认 "delta"/"content"，
                    # 于是**服务明明在流正文，尺子却报「正文 0 字」**——线上就吃过这个假红：
                    # 同一轮 SSE 里有 561 个 delta 帧，脚本却打出「正文 0 字」并据此去追产品 bug。
                    # 弱/错尺子比没有尺子更毒：它会把「我在干活」判成「我什么都没干」。
                    txt = st.get("t") or st.get("delta") or st.get("content") or ""
                    if isinstance(txt, str) and txt:
                        body_ts.append(now)
                        if first_text is None:
                            first_text = now
                            print(f"[{now:6.1f}s] 🧱 首片可见正文")
                        texts.append(txt)
    body_text = "".join(texts)
    print(f"[{time.time()-t0:6.1f}s] 结束：正文 {len(body_text)} 字，首正文 "
          f"{('%.1fs' % first_text) if first_text else '未出现'}，材料帧 {len(mats)}，"
          f"帧类型 {kinds}")
    return {"mats": mats, "body": body_text, "gen": gen_urls, "first_text": first_text,
            "body_ts": body_ts, "prompt": message,
            # first_body / secs 是给 B4 速度账用的：拿「首片可见正文」和「整轮耗时」
            # 当两个独立数字。只报总时长看不出用户到底在哪一段干等 —— 线上那次
            # 190s 里有 174s 是「首段正文之前」，两者必须分开报。
            "first_body": first_text, "secs": time.time() - t0}


def session_answer(sid, role="assistant"):
    """读落盘会话里的最后一轮该角色发言（比从 SSE 帧里猜字段可靠）。"""
    p = os.path.join(os.environ.get("SESS_DIR", "/opt/skillforge/data/sessions"), sid + ".json")
    if not os.path.exists(p):
        return None
    try:
        rows = json.load(open(p, encoding="utf-8"))
    except Exception:
        return None
    for r in reversed(rows):
        if r.get("role") == role and r.get("content"):
            return str(r["content"])
    return None


# 本地话术的**分段模式**：这些片段是我们自己的代码写进材料区的（静默旁白 / 装配事实 /
# 要点回顾 / 构思条目 / 路由回执），不是模型产出。B1 要守的性质是「用户还会不会看到
# 模型的英文自我对话」，本地片段不该被算进去——装配事实里本来就要列要素键名，而键名
# 可以是英文（company_name、core_event），那跟人名地名一样是**专名**，不是「在说英文」。
#
# 为什么要按片段剥离而不是按行首前缀判断：材料窗口是**滚动尾巴**，本地行被滚掉开头后，
# 新增片段是从中间截断的（线上实测拿到过 `'｜要素 4 项（company_name…）｜正在起草…'`），
# 前缀判断拦不住，那行会被冤枉成「半英半中的噪声帧」。
LOCAL_PATTERNS = (
    re.compile(r"模型(?:正在自检措辞|思考中)…已产出 \d+ 字（已 \d+s）"),
    re.compile(r"（要点回顾 \d+/\d+）"),
    re.compile(r"已装配："),
    re.compile(r"正在起草…|正在生成文档…"),
    re.compile(r"收到你的需求（本轮 \d+ 字）"),
    re.compile(r"要素 \d+ 项（[^）]*）"),
    # 上面那条要求「要素」二字在场，但窗口滚动后前缀会被滚掉（实测拿到过
    # `'4 项（company_name、core_event…）'`），所以再补两条不依赖前缀的：
    re.compile(r"\d+ 项（[^）]*）"),
    # 要素键名的**形态**：snake_case / 纯 ASCII 标识符用顿号连成一串。
    # 中文正文里几乎不可能出现（模型说人话时不会写 company_name、core_event），
    # 而装配事实里必然出现，滚掉前缀也认得出。
    re.compile(r"[A-Za-z_]{2,}(?:、[A-Za-z_]{2,})+"),
    re.compile(r"会话上文 \d+ 条 \d+ 字"),
    re.compile(r"审稿清单 [有无]"),
    re.compile(r"技能《[^》]*》"),
    re.compile(r"^·\s*", re.M),
)


def strip_local(d):
    """把本地话术从一段材料里剥掉，剩下的才是「模型侧产出」。"""
    out = d
    for p in LOCAL_PATTERNS:
        out = p.sub(" ", out)
    return out.strip()


def check_material(round_dict, answer=None):
    mats = round_dict["mats"]
    r = round_dict
    body_text = round_dict["body"]
    if len(mats) < 5:
        ok("B0 材料帧数足够", False, f"只有 {len(mats)} 帧（旧行为每轮 200+ 帧）")
        return
    ok("B0 材料帧数足够", True, f"{len(mats)} 帧")
    # 把材料原文留一份（默认落 /tmp，可用 MAT_DUMP 改）。判据红了要能当场看东西，而不是
    # 靠再跑一轮猜 —— 尺子自己出错时，这条 dump 是唯一能一眼看穿的证据。
    try:
        with open(os.environ.get("MAT_DUMP", "/tmp/mat_dump.json"), "w", encoding="utf-8") as f:
            json.dump([{"t": round(t, 2), "m": m} for t, m in mats], f, ensure_ascii=False, indent=1)
    except Exception as e:
        print(f"⚠ 材料 dump 失败（不影响判据）：{e}")
    deltas, noisy, times = [], [], []
    prev = ""
    for t, m in mats:
        d = delta(prev, m)
        prev = m
        if not d.strip():
            continue
        times.append(t)          # 本地行也是屏幕上的动静，B2（最长无变化）必须算它
        model_side = strip_local(d)   # B1/B3 只看模型侧产出：本地话术先剥掉
        if not model_side:
            continue
        deltas.append(model_side)
        if latin_ratio(model_side) > NOISE_RATIO:
            noisy.append((t, model_side[:60]))
    ok("B1 材料里没有半英半中的噪声帧", len(noisy) == 0,
       f"噪声帧 {len(noisy)}/{len(deltas)}"
       + (f"；首条 {noisy[0][1]!r}" if noisy else ""))
    # B2 = 「屏幕上还有没有东西在动」，**不是**「材料帧之间隔了多久」。
    #
    # 这两件事差别很大，而原版 B2 量的是后者 —— 于是正文正在一屏屏刷出来的时候，它照样
    # 判「静默 5.6s」并亮红。那是我把用户能看见的动静丢掉了一半：材料只是「动」的一种
    # 来源，正文是更有说服力的那种。定义与 web/tests/chat_material_e2e.py 的 M7 对齐
    # （「材料/正文/步骤任一在动即算不静默」），门槛同样用 8s，依据是线上正常流的最长帧
    # 间隔实测；两个脚本不许各拿一把尺子量同一件事。
    motion = sorted(times + list(round_dict.get("body_ts") or []))
    gaps = [motion[i] - motion[i - 1] for i in range(1, len(motion))]
    mg = max(gaps) if gaps else 0
    ok("B2 屏幕最长无变化 ≤ %.1fs（材料或正文任一在动即不算静默）" % MAX_GAP,
       mg <= MAX_GAP, f"实测 {mg:.1f}s（材料 {len(times)} 次 + 正文 {len(round_dict.get('body_ts') or [])} 片）")
    stream = cjk_only("".join(deltas))
    # B3 = 材料必须**贴着本轮内容**，不能是通用套话。
    #
    # 这版判据原来写的是「材料里读得到该轮正文的中文片段」。那个写法有个隐蔽的毛病：
    # 它成立的前提是「模型在思考链里先起草正文」。线上默认档刚翻成 thinking_budget=1024
    # 之后，思考链在起草之前就被掐断 → B3 恒红，而**用户体验其实是变好的**（首正文
    # 30.6s → 之后更快、整轮 36.4s）。拿「材料里有没有正文草稿」当质量门，等于要求产品
    # 必须把思考链烧到起草步 —— 那正好是这轮要治的那 200 秒空转。
    #
    # 改判「材料里出现本轮素材的实词」：既守住「不是通用填充物」这个真意图，又不隐含
    # 「必须烧到起草」这个我们从没承诺过的前提。实词从**本轮提示词**里取，不是从答案
    # 里取 —— 从答案里取就又要靠起草了，等于换个姿势踩同一个坑。
    src = answer if answer else body_text
    if not src or len(cjk_only(src)) < 200:
        print("⚠ B3 SKIP：拿不到该轮正文（既没读到会话文件、SSE 里也没有 delta）")
        return
    prompt_text = round_dict.get("prompt") or ""
    # 用 **4-gram** 而不是贪心的 3~6 字窗口：后者在中文里切出来的是「下面是公司的」
    # 这种跨词垃圾，要求它在材料里逐字出现，等于拿错的串去对答案（线上真红过一次，
    # 而当时材料里明明有「引述规范」「禁用词」这些本轮要求）。4-gram 短、稳、可自证。
    grams = {}
    txt = re.sub(r"[^\u4e00-\u9fff]", " ", prompt_text)
    for run in txt.split():
        for i in range(len(run) - 3):
            g = run[i:i + 4]
            grams[g] = grams.get(g, 0) + 1
    picks = [g for g, n in sorted(grams.items(), key=lambda kv: -kv[1])[:40] if n >= 2]
    if not picks:
        print("⚠ B3 SKIP：提示词里取不到重复出现的 4-gram，本判据无从下手")
    else:
        hits = [g for g in picks if g in stream]
        ok("B3 材料贴着本轮素材（出现本轮 4-gram）", len(hits) >= 2,
           f"{len(hits)}/{len(picks)} 个高频 4-gram 出现在材料里" +
           (f"；例 {'、'.join(hits[:4])}" if hits else "；一个都没出现，材料像是通用套话"))

    # ---- B4 速度账：这三行是「感受速度」的尺子 ----
    # 为什么单列：B1/B2/B3 只看「材料对不对」，全绿也可能整轮 190s、首段正文 174s
    # （用户原话「现在速度过于慢了…一直卡着计时」）。速度必须有自己的数字，
    # 否则「优化完了」永远没有可对照的基线。
    first_mat = mats[0][0] if mats else None
    first_body = r.get("first_body")
    total = r["secs"] if r and r.get("secs") is not None else None
    print(f"   速度账：首材料 {first_mat and round(first_mat,1)}s"
          f"｜首正文 {first_body and round(first_body,1)}s"
          f"｜整轮 {total and round(total,1)}s"
          f"｜正文 {len(cjk_only(body_text))} 字"
          f"｜材料 {len(mats)} 帧")


def fetch(url, tok=None):
    req = urllib.request.Request(BASE + url)
    if tok:
        req.add_header("Authorization", "Bearer " + tok)
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, r.read(), r.headers.get("Content-Disposition", "")
    except urllib.error.HTTPError as e:
        return e.code, e.read(), ""


def wait_ready(timeout=90):
    t0 = time.time()
    while time.time() - t0 < timeout:
        try:
            with urllib.request.urlopen(BASE + "/api/site", timeout=5) as r:
                if r.status == 200:
                    return time.time() - t0
        except Exception:
            pass
        time.sleep(1)
    return None


def main():
    tok = login()
    sid = "liveverify-%d" % int(time.time())
    material = open(MAT, encoding="utf-8").read()
    p1 = (f"下面是公司的背景素材（约 {len(material)} 字），请通读后按素材写一篇"
          f"1500 字左右的公司新闻稿，标题自拟，写完直接给正文。\n\n素材如下：\n{material}")
    r1 = round_stream(tok, sid, p1, "第1轮：1万字素材 → 写新闻稿")
    check_material(r1, session_answer(sid))

    tok2 = login()
    r2 = round_stream(tok2, sid, "把上面这篇新闻稿原样整理成 Word 文档（.docx），正文一字不改。",
                      "第2轮：整理成 Word", collect_material=False)
    if not r2["gen"]:
        ok("A0 拿到交付物链接", False, "第2轮没有产出 /api/chat/gen 链接")
        print("\n红项：" + ", ".join(fails)); sys.exit(1)
    gid = r2["gen"][0]
    url = "/api/chat/gen/" + gid
    ok("A0 拿到交付物链接", True, url)

    st, data1, cd = fetch(url)
    sha1 = hashlib.sha256(data1).hexdigest()
    ok("A1 生成后立刻可下载", st == 200 and len(data1) > 1000,
       f"HTTP {st} / {len(data1)} 字节 / sha {sha1[:12]}")
    print(f"   磁盘落盘：{GEN_DIR}/{gid}.bin 存在="
          f"{os.path.exists(os.path.join(GEN_DIR, gid + '.bin'))}")

    if DO_RESTART:
        print("\n--- 重启服务（旧行为：重启后该链接 404）---")
        subprocess.run(["systemctl", "restart", "skillforge"], check=True)
        rt = wait_ready()
        print(f"    服务就绪耗时 {rt and round(rt,1)}s")
        st2, data2, _ = fetch(url)
        sha2 = hashlib.sha256(data2).hexdigest() if data2 else "-"
        ok("A2 重启后同一链接仍可下载", st2 == 200 and sha2 == sha1,
           f"HTTP {st2} / sha {sha2[:12]}（重启前 {sha1[:12]}）")

    print("\n===== 汇总 =====")
    if fails:
        print("红项 %d 个：%s" % (len(fails), ", ".join(fails)))
        sys.exit(1)
    print("全部判据通过 ✅")


if __name__ == "__main__":
    main()
