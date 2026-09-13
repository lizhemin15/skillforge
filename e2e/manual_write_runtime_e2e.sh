#!/bin/bash
# 手册写作【运行时】E2E：建技能 → 判类路由 → 素材注入命中 → 审稿改稿 → 判不出就反问
#
# 【为什么另起一个脚本】
#   manual_pipeline_e2e.sh 只能证明「素材抽出来了」（分类数/范文数/保真）。
#   「抽出来的素材有没有真的喂进模型」「有没有严格按手册那一类的要求写」「审稿是真在
#   跑还是装样子」「判不出类别时会不会硬着头皮瞎写」——全在运行时这条路上，跑素材那条
#   线一个都覆盖不到。历史上出过「分类目录建好了、运行时却在凭分类名瞎猜」的事故，
#   所以这条线必须独立自证。
#
# 【假模型】e2e/fake_llm.py 顶替真模型：把每次请求的 system/user 原样写进 llm.jsonl，
#   并确定性地产出可判定的标记串（DRAFT-BODY / FLAGGED-SENTENCE / FIXED-SENTENCE）。
#   断言全部落在「请求里注入了什么」和「产物里出现了什么」上，与实现写法解耦：
#   重构内部函数不会假红，断了真正要保的行为才会红。
#
# 【归属铁律】就绪断言必须同时命中「本次端口」+「本次数据目录」——服务自己打印的
#   那行 `listening on <addr> (data: <dir>)` 是现成的归属证据。上一版脚本栽在
#   8099 被残留实例占着、请求打到了别人实例上，读到旧产物报假红/假绿。
#
# 退出码：0=通过  1=失败  2=跳过（缺二进制/缺假模型，显式标注，绝不假装通过）
set -uo pipefail

REPO=/root/skillforge
BIN=${BIN:-/opt/skillforge/skillforge}
FAKE=$REPO/e2e/fake_llm.py
DATADIR=${DATADIR:-/tmp/sf-write-e2e-$(date +%H%M%S)}
SLUG=${SLUG:-e2e-manual-write}

echo "########## 手册写作运行时 E2E ##########"
echo "被测二进制：$BIN"
[ -x "$BIN" ] || { echo "✗ 二进制不存在或不可执行：$BIN"; exit 1; }
stat -c '  编译时间 %y  %s bytes' "$BIN"
[ -f "$FAKE" ] || { echo "✗ 缺假模型：$FAKE"; exit 1; }

free_port() {
  python3 - <<'PY'
import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()
PY
}

APP_PORT=$(free_port)
LLM_PORT=$(free_port)
ADDR="127.0.0.1:${APP_PORT}"
export FAKE_LLM_PORT="$LLM_PORT"
export FAKE_LLM_LOG="$DATADIR/llm.jsonl"

rm -rf "$DATADIR"; mkdir -p "$DATADIR"
echo "本次端口：$ADDR（随机） / 假模型：127.0.0.1:$LLM_PORT"
echo "本次数据目录：$DATADIR"
echo

# --- 假模型先起：它不开，被测服务的第一条模型请求会失败并掩盖真实断言 ---
python3 "$FAKE" > "$DATADIR/llm.log" 2>&1 &
LLM_PID=$!
for i in $(seq 1 30); do
  grep -q 'FAKE_LLM_READY' "$DATADIR/llm.log" 2>/dev/null && break
  kill -0 "$LLM_PID" 2>/dev/null || { echo "✗ 假模型退出"; tail -20 "$DATADIR/llm.log"; exit 1; }
  sleep 0.5
done
grep -q 'FAKE_LLM_READY' "$DATADIR/llm.log" || { echo "✗ 假模型未就绪"; tail -20 "$DATADIR/llm.log"; exit 1; }
echo "假模型就绪 ✓（日志 $DATADIR/llm.jsonl）"

# --- 被测服务：绝不 source 线上 env，本次要的是假模型而不是真密钥 ---
export SKILLFORGE_ADDR="$ADDR"
export SKILLFORGE_DATA_DIR="$DATADIR"
export SKILLFORGE_DB="$DATADIR/skillforge.db"
export SKILLFORGE_ADMIN_USER="e2e-admin"
export SKILLFORGE_ADMIN_PASS="e2e-pass-123"
# 真密钥一律不写进脚本：这些只是本地 E2E 占位串，且字面量会撞上脱敏过滤器被改写。
PLACEHOLDER="e2e-local-dev-only"
export SKILLFORGE_JWT_SECRET="$PLACEHOLDER"
export SKILLFORGE_LLM_PROVIDER="openai"
export SKILLFORGE_LLM_BASE_URL="http://127.0.0.1:${LLM_PORT}"
export SKILLFORGE_LLM_API_KEY="$PLACEHOLDER"
export SKILLFORGE_LLM_MODEL="fake-model"
export SKILLFORGE_BASE_URL="http://$ADDR"

"$BIN" > "$DATADIR/server.log" 2>&1 &
SRV=$!
echo "本次实例 pid=$SRV"

cleanup() { kill "$SRV" "$LLM_PID" 2>/dev/null; wait "$SRV" 2>/dev/null; }
trap cleanup EXIT

ready=0
for i in $(seq 1 40); do
  if grep -qF "listening on ${ADDR} (data: ${DATADIR})" "$DATADIR/server.log" 2>/dev/null; then
    echo "就绪 ✓ 已确认本实例 data=${DATADIR}"; ready=1; break
  fi
  if ! kill -0 "$SRV" 2>/dev/null; then
    echo "✗ 服务进程已退出（pid=$SRV）——不接受「打到别人实例上」的结果"
    tail -20 "$DATADIR/server.log"; exit 1
  fi
  sleep 1
done
[ "$ready" = 1 ] || { echo "✗ 40s 内未确认服务归属"; tail -20 "$DATADIR/server.log"; exit 1; }
sleep 0.5

echo
echo "=== ① 建技能（管理端建 + 投放手册素材：3 类 + 3 篇范文 + 审稿清单）==="
python3 - "http://$ADDR" "$SLUG" "$DATADIR" <<'PY'
import json, os, sys, urllib.request

base, slug, datadir = sys.argv[1], sys.argv[2], sys.argv[3]

def req(method, path, body=None, token=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(base + path, data=data, method=method)
    r.add_header("Content-Type", "application/json")
    if token:
        r.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(r, timeout=20) as resp:
        return json.loads(resp.read().decode() or "{}")

tok = req("POST", "/api/login", {"username": "e2e-admin", "password": "e2e-pass-123"})["token"]
print("  管理端登录 ✓")

# 技能提示词里带一个独特标记，供「审稿是否共享起草上下文」的负向断言用
sysprompt = (
    "SYS-SKILL-MARK\n"
    "你是单位的公文写作助手，按手册分类要求写作。\n"
)
req("POST", "/api/admin/skills", {
    "slug": slug, "name": "E2E 公文写作", "description": "运行时 E2E 用",
    "category": "general", "system_prompt": sysprompt,
}, tok)
print(f"  技能已创建 ✓ slug={slug}")

sk = os.path.join(datadir, "skills", slug)
def put(rel, text):
    p = os.path.join(sk, rel)
    os.makedirs(os.path.dirname(p), exist_ok=True)
    open(p, "w", encoding="utf-8").write(text)

# --- 3 个分类：每类一个独特要求标记 + 一篇带独特标记的真实范文 ---
# 每类的「要求标记」和「范文标记」都不会出现在别类里 —— 这样才能断言
# 「按 A 类写作时只注入了 A 类素材」，串味（把别类要求/范文喂进去）必红。
put("categories/01-会议纪要.md", """# 会议纪要

## 触发场景
- 内部会议结束后需要形成记录与决议事项

## 写作要求
REQ-MEETING-7：必须写明决议事项、责任人与完成时限；不得描写会议气氛。

## 参考范文
- examples/会议纪要/01.md
""")
put("categories/02-新闻通稿.md", """# 新闻通稿

## 触发场景
- 对外发布活动、新产品或重大事项时需要一篇可直接发布的消息稿

## 写作要求
REQ-NEWS-9：首段必须交代时间、地点、主体、事件；通篇不得使用第一人称。

## 参考范文
- examples/新闻通稿/01.md
""")
put("categories/03-情况通报.md", """# 情况通报

## 触发场景
- 发生隐患、事故或典型问题后需要向内部通报情况的书面材料

## 写作要求
REQ-REPORT-3：必须写明发生经过、原因分析与整改措施；不得出现推测性结论。

## 参考范文
- examples/情况通报/01.md
""")
# 范文：会议纪要那类走「参考范文」清单引用（refs 路径），另外两类只靠目录扫描
# （回退路径）—— 两条读法都要覆盖。
put("examples/会议纪要/01.md", "EX-MEETING-1 范文正文：会上决定了三件事。\n")
put("examples/新闻通稿/01.md", "EX-NEWS-1 范文正文：某公司今日在发布厅举行发布会。\n")
put("examples/情况通报/01.md", "EX-REPORT-1 范文正文：排查发现两处隐患。\n")
put("reviewer.md", """# 审稿清单

CHK-REVIEW-1：不得出现主观评价；每条决议必须有责任人与完成时限。
""")
print("  手册素材已投放 ✓ 3 类 / 3 篇范文 / 1 份审稿清单")
PY
[ $? -eq 0 ] || { echo "✗ 建技能/投放素材失败"; exit 1; }

chat() {  # chat <session> <message> <输出文件>
  curl -sS -N -X POST "http://$ADDR/api/chat" \
    -H 'Content-Type: application/json' \
    -d "$(python3 -c 'import json,sys;print(json.dumps({"session_id":sys.argv[1],"message":sys.argv[2],"mode":"manual","skill":sys.argv[3]}))' "$1" "$2" "$SLUG")" \
    > "$3" 2>"$3.err"
}

echo
echo "=== ② 场景 A：新闻通稿（正常三段式）==="
A_OFF=$(wc -l < "$FAKE_LLM_LOG" 2>/dev/null || echo 0)
chat "e2e-sA" "写一篇新闻通稿：某公司今日发布新一代产品。" "$DATADIR/sA.sse"
echo "  SSE 帧数：$(grep -c '^event:' "$DATADIR/sA.sse")"

echo "=== ③ 场景 B：判不出类别（UNCLEAR，必须反问而不是硬写）==="
B_OFF=$(wc -l < "$FAKE_LLM_LOG" 2>/dev/null || echo 0)
chat "e2e-sB" "整理一下这段：UNCLEAR" "$DATADIR/sB.sse"
echo "  SSE 帧数：$(grep -c '^event:' "$DATADIR/sB.sse")"

echo "=== ④ 场景 C：同一会话连写两份（情况通报 → 会议纪要）==="
C_OFF=$(wc -l < "$FAKE_LLM_LOG" 2>/dev/null || echo 0)
chat "e2e-sC" "写一份情况通报：关于安全隐患排查，归档标记 HIST-OLD-1。" "$DATADIR/sC1.sse"
chat "e2e-sC" "再写一份会议纪要：项目周会已开完。" "$DATADIR/sC2.sse"
echo "  SSE 帧数：C1=$(grep -c '^event:' "$DATADIR/sC1.sse") C2=$(grep -c '^event:' "$DATADIR/sC2.sse")"

FAIL=0
report() { if [ "$1" = 1 ]; then echo "  ✓ $2"; else echo "  ✗ $2"; FAIL=1; fi; }

echo
echo "########## 结果断言 ##########"

# 0) 服务端没炸
if grep -qiE 'panic' "$DATADIR/server.log"; then
  report 0 "服务端日志有 panic"; grep -i -m3 panic "$DATADIR/server.log"
else
  report 1 "服务端日志无 panic"
fi
kill -0 "$SRV" 2>/dev/null || report 0 "服务进程已退出（跑到一半挂了）"

# 1) 逐场景断言（SSE 帧 + 假模型日志双向对齐）
python3 - "$DATADIR" "$A_OFF" "$B_OFF" "$C_OFF" <<'PY'
import json, os, re, sys

datadir = sys.argv[1]
offs = {"A": int(sys.argv[2]), "B": int(sys.argv[3]), "C": int(sys.argv[4])}
FAIL = []

def ok(cond, msg):
    print(("  ✓ " if cond else "  ✗ ") + msg)
    if not cond:
        FAIL.append(msg)

def frames(path):
    """SSE 文本 → [(事件名, dict)]"""
    out = []
    ev = None
    for line in open(path, encoding="utf-8"):
        line = line.rstrip("\n")
        if line.startswith("event: "):
            ev = line[7:]
        elif line.startswith("data: "):
            try:
                out.append((ev, json.loads(line[6:])))
            except json.JSONDecodeError:
                out.append((ev, {}))
    return out

def deltas(fs):
    return "".join(d.get("t", "") for e, d in fs if e in ("delta", "message", "token"))

def traces(fs):
    txt = []
    for e, d in fs:
        if e in ("trace", "meta", "step"):
            txt.append(json.dumps(d, ensure_ascii=False))
    return "\n".join(txt)

logs = [json.loads(l) for l in open(os.path.join(datadir, "llm.jsonl"), encoding="utf-8")]
def window(a, b):
    return logs[a:b]

# ---------- A：正常三段式 ----------
fa = frames(os.path.join(datadir, "sA.sse"))
names = [e for e, _ in fa]
txt_a = deltas(fa)
tr_a = traces(fa)

ok("error" not in names, "A 无 error 帧")
ok(names and names[-1] == "done", f"A 末帧是 done（实得 {names[-1] if names else '无'}）")
ok("REQ-NEWS-9" not in txt_a and "EX-NEWS-1" not in txt_a,
   "A 交付稿里没有把「要求/范文」当正文吐出来（那是给模型的，不是给用户的）")
ok("新闻通稿" in tr_a and "命中" in tr_a,
   "A trace 里显性化了命中的分类「新闻通稿」（不是把用户原话抄进 trace —— 那也算「出现」）")
ok("FIXED-SENTENCE" in txt_a and "FLAGGED-SENTENCE" not in txt_a,
   "A 交付的是**改后**版本（审稿点名的句子已被替换）")
ok(txt_a.count("DRAFT-BODY") == 1,
   f"A 交付稿只出现一次（起草不流式下发，实得 {txt_a.count('DRAFT-BODY')} 次）")
ok("已审稿" in txt_a or "审稿" in tr_a, "A 审稿这一步对用户可见")

wa = window(offs["A"], offs["B"])
kinds = [x.get("kind") for x in wa]
ok(kinds.count("route") == 1, f"A 判类调用 1 次（实得 {kinds.count('route')}）")
ok(kinds.count("draft") == 1, f"A 执笔调用 1 次（实得 {kinds.count('draft')}）")
ok(kinds.count("review") == 2, f"A 审稿 2 轮（上限内跑满，实得 {kinds.count('review')}）")
ok(kinds.count("revise") == 1, f"A 改稿 1 次（实得 {kinds.count('revise')}）")

d = [x for x in wa if x.get("kind") == "draft"][0]
blob = d["system"] + "\n" + d["user"]
ok("REQ-NEWS-9" in blob, "A 执笔 prompt 注入了本类写作要求 REQ-NEWS-9")
ok("EX-NEWS-1" in blob, "A 执笔 prompt 注入了本类真实范文 EX-NEWS-1")
for bad in ("REQ-MEETING-7", "EX-MEETING-1", "REQ-REPORT-3", "EX-REPORT-1"):
    ok(bad not in blob, f"A 未串味：不含别类素材 {bad}")

rv = [x for x in wa if x.get("kind") == "review"]
ok(all("CHK-REVIEW-1" in (x["system"] + x["user"]) for x in rv),
   "A 审稿 prompt 注入了手册审稿清单 CHK-REVIEW-1")
ok(all("REQ-NEWS-9" in (x["system"] + x["user"]) for x in rv),
   "A 审稿 prompt 带上「本类要求」当尺子")
ok(all("SYS-SKILL-MARK" not in x["system"] for x in rv),
   "A 审稿用独立 system（不共享起草上下文——否则审稿会替自己的稿子辩护）")
ok(all("DRAFT-BODY" not in x["system"] for x in rv), "A 待审稿件不进 system")
ok(rv[0]["user"].count("FLAGGED-SENTENCE") >= 1 and "FLAGGED-SENTENCE" not in rv[1]["user"],
   "A 第 2 轮审的是**改后**稿（第 1 轮审的原稿、第 2 轮不再是原稿）")

rz = [x for x in wa if x.get("kind") == "revise"]
ok(rz and "FLAGGED-SENTENCE" in rz[0]["user"], "A 改稿 prompt 里点名了要改的那句（审稿意见真传下去了）")

# ---------- B：判不出类别 → 反问 ----------
fb = frames(os.path.join(datadir, "sB.sse"))
names_b = [e for e, _ in fb]
txt_b = deltas(fb)
done_b = [d for e, d in fb if e == "done"]
mid_b = "\n".join(json.dumps(d, ensure_ascii=False) for e, d in fb)
ok("error" not in names_b, "B 无 error 帧（判不出不是错误，是要用户拍板）")
ok(names_b and names_b[-1] == "done", "B 末帧是 done")
ok(done_b and str(done_b[-1].get("asked", "")).lower() == "true",
   f"B done 帧带 asked=true（实得 {done_b[-1] if done_b else '无'}）")
ok(txt_b.strip() != "", "B 反问文案已下发（不是静默停在思考中）")
for c in ("会议纪要", "新闻通稿", "情况通报"):
    ok(c in txt_b or c in mid_b, f"B 反问清单里列出了候选类别「{c}」")

wb = window(offs["B"], offs["C"])
kb = [x.get("kind") for x in wb]
ok(kb.count("route") == 1, f"B 判类调用 1 次（实得 {kb.count('route')}）")
ok(kb.count("draft") == 0, f"B **没有**硬着头皮起草（draft 调用 {kb.count('draft')} 次，应为 0）")
ok(kb.count("review") == 0, f"B 没有审稿（review 调用 {kb.count('review')} 次，应为 0）")
ok("DRAFT-BODY" not in txt_b, "B 没有吐出任何稿件")

# ---------- C：同会话连写两份，靠会话历史判类 ----------
fc1 = frames(os.path.join(datadir, "sC1.sse"))
fc2 = frames(os.path.join(datadir, "sC2.sse"))
txt_c2 = deltas(fc2)
ok("done" in [e for e, _ in fc1] and "done" in [e for e, _ in fc2], "C 两轮都跑到 done")
ok("会议纪要" in traces(fc2) and "命中" in traces(fc2),
   "C 第二轮 trace 显性化命中「会议纪要」（不该沿用上一轮的分类结论）")

wc = window(offs["C"], len(logs))
kc = [x.get("kind") for x in wc]
ok(kc.count("route") == 2, f"C 两轮各判类 1 次（实得 {kc.count('route')}）")
d2 = [x for x in wc if x.get("kind") == "draft"][-1]
blob2 = d2["system"] + "\n" + d2["user"]
ok("REQ-MEETING-7" in blob2 and "EX-MEETING-1" in blob2,
   "C 第二轮注入了会议纪要那类的「要求 + 范文」")
ok("REQ-REPORT-3" not in blob2 and "EX-REPORT-1" not in blob2,
   "C 第二轮没把第一轮那类的素材留在 prompt 里（串味必红）")
rvc = [x for x in wc if x.get("kind") == "review" and "REQ-MEETING-7" in (x["system"] + x["user"])]
ok(rvc and all("HIST-OLD-1" not in x["user"] for x in rvc),
   "C 第二轮的审稿 prompt 不带会话历史原文（审稿只看这一篇稿子）")
ok(txt_c2.count("DRAFT-BODY") == 1, "C 第二轮交付稿只出现一次")

print()
sys.exit(1 if FAIL else 0)
PY
[ $? -eq 0 ] || FAIL=1

echo
if [ "$FAIL" = 0 ]; then
  echo "########## E2E 通过 ✓ ##########"
  echo "产物：$DATADIR（server.log / llm.jsonl / sA.sse / sB.sse / sC*.sse）"
  exit 0
else
  echo "########## E2E 失败 ✗ ##########"
  echo "产物：$DATADIR"
  echo "假模型日志：$FAKE_LLM_LOG（看 kind=route/draft/review/revise 各注入了什么）"
  exit 1
fi
