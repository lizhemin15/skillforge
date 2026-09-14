#!/usr/bin/env bash
# 线上（已部署实例）「用户素材层 + 多轮记忆」实测 —— 用户原话：
#   「我手动添加了一个一万字的写作要求和示例来创建 skill，然后直接提出一个写作需求
#     的时候，有时候似乎像是没看到我的信息一样的，还在问我要信息，要的时候也不是
#     根据我目前提供的信息的基础上来进一步补充，而是直接通用的补充。还有就是我多轮
#     对话的时候，似乎就忘了我之前问了什么，以及你自己回答了什么。」
#
# 断言设计（全部是**可判定的字面量**，不看模型心情）：
#   A) 第一轮：贴一份 2500+ 字要求（中段埋一句「必须出现的句子」），要求直接出正文。
#      判据① 第一轮输出里出现中段那句 → 证明模型看到的是**整段**素材，不是 600 字头尾。
#      判据② 第一轮输出 ≥200 字（真的写了，而不是「请提供主题/篇幅」的通用追问）。
#   B) 第二轮（同一个 session，**不再重贴素材**）：「我刚才让你必须写进去的那句话是什么？」
#      → 答案里必须出现那句 → 证明它记得用户之前给过什么（多轮记忆，不是每轮从零问）。
#   N) 差分负例：全新 session，只发第一轮那句指令（不给素材）→ 输出里**不得**出现那句。
#      否则断言 A① 就是恒真的假绿（模型可能自己编出来）。
#
# 用法：bash testdata/multiturn/verify_live_material_context.sh [BASE_URL] [--break-marker]
#   --break-marker：把判据串换成一个不可能出现的串，用来证明这脚本真的在读 SSE 正文
#                   （正常应该整片变红；红不出来说明判据是空的）。
set -u
BASE="http://127.0.0.1:8092"
BREAK=0
for a in "$@"; do
  case "$a" in
    --break-marker) BREAK=1 ;;
    http*) BASE="$a" ;;
  esac
done

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fail=0
say() { printf '%s\n' "$*"; }
ok()  { printf '  ok   %s\n' "$*"; }
bad() { printf '  FAIL %s\n' "$*"; fail=$((fail+1)); }

MARK='本次合作由星河智联提供算力底座'          # 埋在中段的判据句
if [ "$BREAK" -eq 1 ]; then
  MARK='本句绝无可能出现QZX9137'               # 自证用：换了它，A①/B 必须变红
fi

# ---- 造素材：2500+ 字写作要求，判据句埋在中段（第 9 条里） ----
python3 - "$WORK/req.txt" <<'PY'
import sys
rules = [
    "一、文体必须是单位内部通知，不要写成新闻稿、不要写成营销软文。",
    "二、标题分两行：第一行单位名称，第二行事项名称，居中。",
    "三、开头必须交代背景，不得少于两句话。",
    "四、正文分三段：第一段讲目的，第二段讲安排，第三段讲要求。",
    "五、全篇使用第三人称，不得出现「我们」「你们」这类口语指代。",
    "六、涉及数字一律用阿拉伯数字，涉及金额一律加「人民币」三个字。",
    "七、落款写单位全称加日期，日期用「XXXX年X月X日」格式。",
    "八、不得出现感叹号，不得使用「热烈」「隆重」这类宣传词。",
    "九、正文第二段末尾必须原样写进这样一句话：「本次合作由星河智联提供算力底座」。",
    "十、全篇不得出现英文缩写，技术名词一律写中文全称。",
    "十一、段落之间不空行，段首缩进两格。",
    "十二、篇幅控制在四百字到六百字之间。",
]
filler = (
    "以下是对上述要求的补充说明，供写作者理解风格边界："
    "单位公文讲求准确、克制、可执行，任何修辞都要服务于把事说清楚这个唯一目的；"
    "凡是不能落到具体动作、具体时间、具体责任人的表述，都应当删掉。"
)
out = ["《单位公文写作要求与示例》", "", "【硬性要求】"]
for r in rules:
    out.append(r)
    out.append(filler * 3)
out += ["", "【示例段落】",
        "各部门：为落实上级关于数据治理工作的部署，现就有关事项通知如下。"
        "本次工作以摸清家底、统一口径为目标，由信息中心牵头，各部门配合，"
        "于本季度内完成数据资产目录的编制与复核。",
        "请各部门指定一名联络人，于三日内将名单报信息中心备案。",
        "", "【中段判据标记】REQ_HEAD 之后的中段内容必须能原样注入。"]
open(sys.argv[1], "w", encoding="utf8").write("\n".join(out))
PY

n_mat=$(python3 -c 'import re,sys;print(len(re.sub(r"\s+","",open(sys.argv[1],encoding="utf8").read())))' "$WORK/req.txt")
say "线上「素材层 + 多轮记忆」实测 · $BASE · 素材 $n_mat 字（旧行为只留头尾 600 字）$([ "$BREAK" -eq 1 ] && echo ' · 自证模式: 判据串已破坏')"
say ""

chat() { # $1 session  $2 素材文件(- 表示不带)  $3 消息  $4 out.sse
  python3 - "$1" "$2" "$3" "$4" <<'PY'
import json, sys
sid, mat, msg, out = sys.argv[1:5]
text = "" if mat == "-" else open(mat, encoding="utf8").read()
full = (text + "\n\n" + msg) if text else msg
json.dump({"session_id": sid, "message": full, "mode": "auto", "skill": ""},
          open(out + ".req", "w", encoding="utf8"), ensure_ascii=False)
PY
  curl -s -N -m 900 -X POST "$BASE/api/chat" -H 'Content-Type: application/json' \
    --data-binary @"$4.req" >"$4"
}

sse_text() { # 聊天方言：data: <裸 JSON>，正文帧 {"t":"…"}（trace 帧是数组，跳过）
  python3 - "$1" <<'PY'
import json, sys
out = []
for ln in open(sys.argv[1], encoding="utf-8", errors="ignore"):
    ln = ln.strip()
    if not ln.startswith("data: "):
        continue
    try:
        d = json.loads(ln[6:])
    except Exception:
        continue
    if not isinstance(d, dict):
        continue
    if "t" in d:
        out.append(str(d["t"]))
    elif "error" in d:
        out.append("\n[ERROR] " + str(d["error"]))
print("".join(out))
PY
}

ASKA='按我上面给你的那份写作要求，写一份关于开展数据治理专项工作的通知，直接输出正文，不要反问我任何问题。'

SID="live-mat-$(date +%s)"
say "第一轮: 只贴素材（3840 字写作要求），让它先收着"
chat "$SID" "$WORK/req.txt" '这是我以后写公文要长期遵循的写作要求，先收下，只回「已收到」三个字。' "$WORK/r0.sse" >/dev/null
sse_text "$WORK/r0.sse" | head -c 120 | tr -d '\n' | sed 's/^/  r0: /'
say ""
say "第二轮（素材在**历史**里，本轮只发指令）: 「$ASKA」"
chat "$SID" - "$ASKA" "$WORK/r1.sse"
sse_text "$WORK/r1.sse" >"$WORK/r1.txt"
n1=$(python3 -c 'import re,sys;print(len(re.sub(r"\s+","",open(sys.argv[1],encoding="utf8").read())))' "$WORK/r1.txt")

if [ "$n1" -ge 200 ]; then
  ok "A②) 第二轮出的是正文（去空白 $n1 字）—— 没有退回通用追问"
else
  bad "A②) 第二轮只有 $n1 字（「请提供主题/篇幅/落款」式通用追问）: $(tr -d '\n' <"$WORK/r1.txt" | head -c 200)"
fi
if grep -q "$MARK" "$WORK/r1.txt"; then
  ok "A①) 中段判据句进了正文 —— 历史里的素材被整段注入（$n_mat 字），不是 600 字头尾"
else
  bad "A①) 正文里没有中段判据句「$MARK」→ 历史素材的中段没被注入（旧行为：3840 字素材 → 头尾几百字）"
fi

say ""
say "第二轮（同一 session，不重贴素材）: 「我刚才让你必须写进去的那句话是什么？」"
chat "$SID" - '你刚才按我的要求写的那份通知里，我要求必须原样写进去的那一句话是什么？只回那句话本身，不要解释。' "$WORK/r2.sse"
sse_text "$WORK/r2.sse" >"$WORK/r2.txt"
if grep -q "$MARK" "$WORK/r2.txt"; then
  ok "B) 它记得用户之前给过什么（答案里出现那句）—— 多轮记忆生效"
else
  bad "B) 答案里没有那句: $(tr -d '\n' <"$WORK/r2.txt" | head -c 200) → 多轮就忘了我之前说了什么"
fi

say ""
say "差分负例（全新 session，不给素材，只发同一句指令）"
chat "live-mat-neg-$(date +%s)" - "$ASKA" "$WORK/r3.sse"
sse_text "$WORK/r3.sse" >"$WORK/r3.txt"
if grep -q "$MARK" "$WORK/r3.txt"; then
  bad "N) 没给素材也写出了那句 → 断言 A① 是恒真的假绿（模型自己编的，不是读了我的素材）"
else
  n3=$(python3 -c 'import re,sys;print(len(re.sub(r"\s+","",open(sys.argv[1],encoding="utf8").read())))' "$WORK/r3.txt")
  ok "N) 没素材时写不出那句（本轮 $n3 字）→ 断言 A① 不是恒真"
fi

say ""
if [ "$BREAK" -eq 1 ]; then
  if [ "$fail" -gt 0 ]; then
    say "自证模式：判据串已换成不可能出现的串，$fail 项按预期变红 ✅（说明这脚本真在读正文）"
    exit 0
  fi
  say "自证模式失败：换掉判据串后竟然还是全绿 —— 这套断言是空的 ❌"
  exit 1
fi
if [ "$fail" -eq 0 ]; then say "线上素材层 + 多轮记忆验收通过 ✅"; else say "$fail 项失败 ❌"; fi
exit $((fail > 0 ? 1 : 0))
