#!/usr/bin/env bash
# 线上（已部署实例）多轮上下文实测 —— 用户原话：
#   「先让生成一个新闻稿以后，让 ai 把新闻稿整理成 word，就通常没有管之前的生成内容」
#
# 与 testdata/multiturn/verify_multiturn_e2e.sh 的区别：那个脚本自己 go build 一个
# 临时实例（测代码），这个脚本打**已经跑在线上端口上的进程**（测部署产物）。
# 两者都要跑：本地绿只说明代码对，线上绿才说明「用户点进去就是绿的」。
#
# 用法：bash testdata/multiturn/verify_live_multiturn.sh [BASE_URL] [SKILL_SLUG]
set -u
BASE="${1:-http://127.0.0.1:8092}"
SKILL="${2:-r503-ocr超时验收}"     # 线上「企业公关稿件写作助手」（write 型）
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fail=0
say() { printf '%s\n' "$*"; }
ok()  { printf '  ok   %s\n' "$*"; }
bad() { printf '  FAIL %s\n' "$*"; fail=$((fail+1)); }

chat() { # $1 session  $2 message  $3 out.sse
  curl -s -N -m 900 -X POST "$BASE/api/chat" -H 'Content-Type: application/json' \
    -d "$(python3 -c 'import json,sys;print(json.dumps({"session_id":sys.argv[1],"message":sys.argv[2],"mode":"auto","skill":sys.argv[3]},ensure_ascii=False))' "$1" "$2" "$3")" >"$3"
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

sse_file_url() {
  python3 - "$1" <<'PY'
import json, sys
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
    u = str(d.get("url", ""))
    if "/api/chat/gen/" in u:
        print(u); break
PY
}

lcs_len() { # 最长公共子串长度（.docx 或纯文本）
  python3 - "$1" "$2" <<'PY'
import re, sys, zipfile
def dtext(p):
    if p.endswith(".docx"):
        xml = zipfile.ZipFile(p).read("word/document.xml").decode("utf8")
        return re.sub(r"\s+", "", "".join(re.findall(r"<w:t[^>]*>(.*?)</w:t>", xml, re.S)))
    return re.sub(r"\s+", "", open(p, encoding="utf8").read())
a, b = dtext(sys.argv[1]), dtext(sys.argv[2])
best = 0
for i in range(len(a)):
    if len(a) - i <= best:
        break
    j = best + 1
    while i + j <= len(a) and a[i:i + j] in b:
        j += 1
    j -= 1
    if j > best:
        best = j
print(best)
PY
}

say "线上多轮上下文实测 · $BASE · skill=$SKILL"

R1='帮我写一篇新闻稿，主题是星河科技发布新一代空间计算芯片「星核 X1」，发布时间 2026 年 9 月 14 日，发布单位星河科技，地点北京，核心信息：单芯片算力 200 TOPS、功耗降低 40%、已与三家车企达成量产合作。要求写成标准企业新闻稿（标题＋导语＋正文＋结尾联系方式），篇幅 400 字以上。直接输出正文，不要反问我任何问题。'
SID="live-mt-$(date +%s)"
step1="$WORK/r1.sse"
step2="$WORK/r2.sse"

chat "$SID" "$R1" "$step1"
sse_text "$step1" >"$WORK/r1.txt"
n1=$(python3 -c 'import re,sys;print(len(re.sub(r"\s+","",open(sys.argv[1],encoding="utf8").read())))' "$WORK/r1.txt")
if [ "$n1" -ge 200 ]; then ok "A) 第一轮有正文（去空白 $n1 字）"; else bad "A) 第一轮正文只有 $n1 字，前置条件不成立（后面断言无意义）"; fi
grep -o '星核' "$WORK/r1.txt" | head -1 | grep -q . && ok "A2) 第一轮正文确实来自本轮素材（含「星核」）" || bad "A2) 第一轮正文不含素材关键词「星核」"

say ""
say "第二轮：把上面那篇整理成 Word"
chat "$SID" '把上面那篇新闻稿整理成 Word 文档发我，正文内容保持原样，不要重写。' "$step2"
URL="$(sse_file_url "$step2")"
if [ -n "$URL" ]; then
  curl -s -m 120 "$BASE$URL" -o "$WORK/out.docx"
  ok "B0) 第二轮给出文件下载链接 $URL"
  sz=$(stat -c%s "$WORK/out.docx")
  head -c 4 "$WORK/out.docx" | grep -q 'PK' && ok "B0b) 下载物是真 docx（${sz}B）" || bad "B0b) 下载的不是 zip/docx（${sz}B，前4字节非 PK）"
else
  bad "B0) 第二轮没给出文件链接（SSE 尾部：$(tail -c 300 "$step2" | tr -d '\n' | head -c 300)）"
fi

if [ -s "$WORK/out.docx" ]; then
  L="$(lcs_len "$WORK/r1.txt" "$WORK/out.docx")"
  if [ "$L" -ge 60 ]; then ok "B) docx 与第一轮正文逐字相同 $L 字（要求 ≥60）→ 上一轮产物真的进了这一轮"; else bad "B) docx 与第一轮正文最长公共串仅 $L 字（要求 ≥60）→ 多轮上下文没生效"; fi
  # 差分：全新 session 发同一句话，不该带出第一轮内容
  chat "live-neg-$(date +%s)" '把上面那篇新闻稿整理成 Word 文档发我，正文内容保持原样，不要重写。' "$WORK/r3.sse"
  U3="$(sse_file_url "$WORK/r3.sse")"
  if [ -n "$U3" ]; then
    curl -s -m 120 "$BASE$U3" -o "$WORK/neg.docx"
    L3="$(lcs_len "$WORK/r1.txt" "$WORK/neg.docx")"
    if [ "$L3" -lt 30 ]; then ok "N) 新 session 的产物与第一轮正文最长公共串仅 $L3 字（<30）→ 断言 B 不是恒真"; else bad "N) 新 session 也带出了 $L3 字第一轮内容 → 断言 B 不成立（可能是素材/缓存串味）"; fi
  else
    say "  note N) 新 session 未产出文件（未触发 docgen），跳过差分断言"
  fi
fi

say ""
if [ "$fail" -eq 0 ]; then say "线上多轮上下文验收通过 ✅"; else say "$fail 项失败 ❌"; fi
exit $((fail > 0 ? 1 : 0))
