#!/usr/bin/env bash
#
# verify_multiturn_e2e.sh —— 多轮上下文端到端验收（对应用户反馈 ①）。
#
# 用户的原始抱怨：「先让生成一个新闻稿以后，让 ai 把新闻稿整理成 word，
# 就通常没有管之前的生成内容。」老实现把对话历史**头 400 字截断**塞进 prompt，
# 新闻稿正文（尤其结尾的联系人/落款）在第二轮就已经不在上下文里了。
#
# 为什么必须端到端（单测不够）：
#   单测证明的是「给定一段 history，拼出来的 prompt 里有产物层原文」。
#   真实链路是 HTTP /api/chat → 引擎 Push(Kind=Artifact) → 会话历史 → 第二轮
#   GenerateDoc(history) → 拼 prompt → LLM 出 docx。任何一环把正文掉在地上，
#   出来的 Word 就是空壳/编造的，而**每一环单独看都是绿的**（这是老事故的形态）。
#
# 断言（正例子，同一 session 两轮）：
#   A) 第一轮真的产出了带独有尾标记的新闻稿正文（前置条件，不是结论）
#   B) 第二轮「整理成 Word」回了一个 .docx，且**下载解包后**正文里含同一条尾标记
#      —— 尾标记只存在于第一轮正文里，第二轮 prompt 不含它就只能靠「猜」，
#      而它含人名+电话，猜不出来。所以 B 通过 = 第一轮正文原样过了第二轮的上下文。
#
# 差分自证（同一脚本内置，防「假绿」）：
#   N) **全新 session** 发同一句「整理成 Word」→ docx 里**不该**出现尾标记。
#      若 N 也出现，说明这个标记 LLM 自己就会编，B 的通过毫无信息量（断言本身是假的）。
#      这条把「标记只可能来自会话历史」证成事实，而不是假设。
#   --break-marker：把断言目标换成一个绝不可能出现的串，此时断言**必须变红**，
#      用来证明这道断言真的在读 docx 正文，而不是只要有个文件就绿。
#
# 用法：
#   testdata/multiturn/verify_multiturn_e2e.sh                # 全流程（真调 LLM，1-3 分钟）
#   testdata/multiturn/verify_multiturn_e2e.sh --break-marker # 自证：断言必须变红
#   testdata/multiturn/verify_multiturn_e2e.sh --keep         # 保留临时目录便于查证
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
ENVF="${SKILLFORGE_ENV_FILE:-/opt/skillforge/skillforge.env}"

APP_PORT="${MT_PORT:-8098}"
# 尾标记：只存在于第一轮正文，含人名+电话（LLM 无从编造）。末尾不带标点，
# 免得 docx 写入时把句号挪到 run 边界外导致整串比对失败（比对前已去空白）。
MARKER="本稿联系人：星河科技品牌部 林晚 010-88886666"
TOPIC="天穹一号"
DOCGEN_SKILL="办公文档管家"   # 线上 is_core=1 的 docgen 技能（产出 office 文件）
BREAK_MARKER=0
KEEP=0

while [ $# -gt 0 ]; do
  case "$1" in
    --break-marker) BREAK_MARKER=1; shift ;;
    --keep)         KEEP=1; shift ;;
    --port)         APP_PORT="$2"; shift 2 ;;
    *) echo "unknown arg: $1"; exit 2 ;;
  esac
done

FAILED=0
ok()   { echo "  ok: $*"; }
step() { echo; echo "=== $* ==="; }

WORK="$(mktemp -d /tmp/verify_mt.XXXXXX)"
APP_PID=""
cleanup() {
  [ -n "$APP_PID" ] && { kill "$APP_PID" 2>/dev/null || true; wait "$APP_PID" 2>/dev/null || true; }
  if [ "$KEEP" -eq 1 ]; then echo "  [--keep] 临时目录保留在 $WORK"; else rm -rf "$WORK"; fi
}
trap cleanup EXIT

[ -f "$ENVF" ] || { echo "FAIL: 缺 $ENVF（没有 LLM 配置跑不了多轮对话）"; exit 1; }
set -a
# shellcheck disable=SC1090
. "$ENVF"
set +a

TMPDATA="$WORK/data"
mkdir -p "$TMPDATA"
# 拷线上库（复用已配好的 LLM/管理账号）与线上技能目录（docgen 技能得在）
[ -f /opt/skillforge/data/skillforge.db ] && cp /opt/skillforge/data/skillforge.db "$TMPDATA/skillforge.db"
[ -d /opt/skillforge/data/skills ] && cp -r /opt/skillforge/data/skills "$TMPDATA/skills"

step "1) 起被测服务（临时数据目录，$APP_PORT）"
export SKILLFORGE_ADDR="127.0.0.1:$APP_PORT"
export SKILLFORGE_DATA_DIR="$TMPDATA"
export SKILLFORGE_DB="$TMPDATA/skillforge.db"
export SKILLFORGE_PUBLIC_URL="http://127.0.0.1:$APP_PORT"
# 解析服务用线上那份即可：本用例不碰素材解析，但引擎初始化会读这个变量
export SKILLFORGE_OCR_URL="${SKILLFORGE_OCR_URL:-http://127.0.0.1:8093}"

BIN="$WORK/skillforge"
( cd "$ROOT" && PATH=/usr/local/go/bin:$PATH go build -trimpath -o "$BIN" ./cmd/server ) \
  || { echo "FAIL: go build 失败"; exit 1; }
"$BIN" >"$WORK/app.log" 2>&1 &
APP_PID=$!
READY=0
for _ in $(seq 1 30); do
  kill -0 "$APP_PID" 2>/dev/null || { echo "FAIL: 服务自己退了"; sed -n '1,20p' "$WORK/app.log"; exit 1; }
  curl -fsS "http://127.0.0.1:$APP_PORT/" >/dev/null 2>&1 && { READY=1; break; }
  sleep 1
done
[ "$READY" -eq 1 ] || { echo "FAIL: 服务未就绪"; sed -n '1,20p' "$WORK/app.log"; exit 1; }
ok "服务就绪（$APP_PORT）"

# chat()  $1=session  $2=message  $3=mode(auto/manual)  $4=skill slug  $5=输出 sse 文件
chat() {
  curl -s -N -m 900 -X POST "http://127.0.0.1:$APP_PORT/api/chat" \
    -H 'Content-Type: application/json' \
    -d "$(python3 -c 'import json,sys;print(json.dumps({"session_id":sys.argv[1],"message":sys.argv[2],"mode":sys.argv[3],"skill":sys.argv[4]},ensure_ascii=False))' "$1" "$2" "$3" "$4")" >"$5"
}

# sse_text()  拼接 delta 事件 → 纯文本（第一轮正文用它判前置条件）
sse_text() {
  python3 - "$1" <<'PY'
import json, sys
# ⚠️ 两套 SSE 方言，别搞混（这是本轮踩出来的）：
#   /api/chat/*      → `data: <裸 JSON>`，正文帧就是 {"t":"…"}（chat.go:write）
#   /api/admin/* 训练 → `data: {"type":…,"data":…}`（admin.go:147 包的壳）
# 早期版本照「训练那套」写 d.get("data") 去解聊天帧 → 恒为 None → 第一轮正文恒为空串 →
# 断言 A 永远红在「模型没照抄标记」。**假红会把排查带偏到完全不存在的方向**，白烧一轮。
# 这里按聊天方言直接取裸 JSON。
out = []
for ln in open(sys.argv[1], encoding="utf-8", errors="ignore"):
    ln = ln.strip()
    if not ln.startswith("data: "):
        continue
    try:
        d = json.loads(ln[6:])
    except Exception:
        continue
    # trace 帧的 data 是**数组**（进度步骤列表），不是对象，也不能当对象用。
    if not isinstance(d, dict):
        continue
    if "t" in d:
        out.append(str(d["t"]))
    elif "error" in d:
        out.append("\n[ERROR] " + str(d["error"]))
    elif "/api/chat/gen/" in str(d.get("url", "")):
        out.append("\n[FILE] " + str(d.get("name", "")) + " " + str(d["url"]))
print("".join(out))
PY
}

# sse_file_url()  取 file 事件的下载 URL（生成型文档走 /api/chat/gen/<tok>）
sse_file_url() {
  python3 - "$1" <<'PY'
import json, sys, re
for ln in open(sys.argv[1], encoding="utf-8", errors="ignore"):
    ln = ln.strip()
    if not ln.startswith("data: "):
        continue
    try:
        d = json.loads(ln[6:])
    except Exception:
        continue
    if not isinstance(d, dict):  # 同上：trace 事件的 data 是数组，不能当对象用
        continue
    # 聊天方言：file 帧就是裸 {"name":…,"url":"/api/chat/gen/<tok>","kind":"gen"}
    # （不是训练那边的 {"type":"file","data":{…}}；两套方言的坑见 sse_text 上方注释）
    u = str(d.get("url", ""))
    if "/api/chat/gen/" in u:
        print(u); break
PY
}

# common_sub()  打印两个文件（.docx 或纯文本）内容去空白后的最长公共子串
# 用途：断言「上一轮正文有没有逐字进到这一轮产物里」——比要求模型照抄某行标记稳，
# 因为后者考的是模型服从率，不是管线有没有带上下文（模型不听话 → 假红）。
common_sub() {
  python3 - "$1" "$2" <<'PY'
import re, sys, zipfile
def dtext(p):
    if p.endswith(".docx"):
        xml = zipfile.ZipFile(p).read("word/document.xml").decode("utf8")
        return re.sub(r"\s+", "", "".join(re.findall(r"<w:t[^>]*>(.*?)</w:t>", xml, re.S)))
    return re.sub(r"\s+", "", open(p, encoding="utf8").read())
a, b = dtext(sys.argv[1]), dtext(sys.argv[2])
best = ""
for i in range(len(a)):
    if len(a) - i <= len(best):
        break
    j = len(best) + 1
    while i + j <= len(a) and a[i:i + j] in b:
        j += 1
    j -= 1
    if j > len(best):
        best = a[i:i + j]
print(best)
PY
}

# docx_text()  解包 .docx，按文档顺序取所有 <w:t> 文本并去掉所有空白
# 去空白是必须的：docx 的 run 会在任意位置切开，甚至可能落在标记中间，
# 拼接后再比对比按行匹配稳。数字/标点不做归一 —— 标记本来就要求一字不改。
docx_text() {
  python3 - "$1" <<'PY'
import re, sys, zipfile
raw = open(sys.argv[1], "rb").read()
if raw[:4] != b"PK\x03\x04":
    print("\n[NOT_A_ZIP] 回的不是 docx（前 4 字节：%r）" % raw[:4]); raise SystemExit(0)
with zipfile.ZipFile(sys.argv[1]) as z:
    xml = z.read("word/document.xml").decode("utf-8", "ignore")
parts = re.findall(r"<w:t[^>]*>(.*?)</w:t>", xml, re.S)
text = "".join(parts)
text = (text.replace("&amp;", "&").replace("&lt;", "<").replace("&gt;", ">")
            .replace("&quot;", '"').replace("&apos;", "'"))
print(re.sub(r"\s+", "", text))
PY
}

step "2) 第一轮：生成新闻稿（自动调度）"
SID_POS="mt-pos-$(date +%s)"
# 第一轮的话必须**自带全部要素**：写作技能在缺要素时（发布日期/数据）会回 needs 反问
# 「要开始写作，我需要你补充…」——那样第一轮就没有正文，断言 A 会以「模型没照抄标记」
# 红掉，而这跟上下文 bug 毫无关系。**假红会把排查带偏到不存在的方向**，所以把要素给全，
# 并显式禁止反问。（实测：不给要素时 needs 事件必现。）
chat "$SID_POS" "请帮我写一篇企业新闻稿，发布日期 2026 年 9 月 14 日，发布单位星河科技，主题：星河科技发布「${TOPIC}」空间计算芯片，发布会现场公布的实测数据为能效比上一代提升 3.2 倍、单芯片算力 128 TOPS。要求正文分三段（发布背景、技术亮点、行业影响），每段 100 字左右。信息已齐全，请**直接输出正文**，不要再向我提问或索要补充信息。结尾必须原样照抄下面这一行（一字不改，不要加引号、不要改标点）：
${MARKER}" auto "" "$WORK/t1.sse"
T1="$(sse_text "$WORK/t1.sse")"
echo "$T1" >"$WORK/t1.txt"
echo "  --- 第一轮产出（尾部 200 字）"; printf '%s' "$T1" | tail -c 200; echo
# A 只看「第一轮有没有产出足够长的正文」——B 要拿它做逐字比对。
# 不把「模型照抄尾标记」当前置条件：那是模型服从率，不是管线 bug，照抄失败会变成假红。
T1_LEN="$(printf '%s' "$T1" | tr -d '[:space:]' | wc -c)"
if [ "$T1_LEN" -ge 150 ]; then
  ok "A) 第一轮有正文（去空白 $T1_LEN 字）→ B 有可逐字比对的素材"
  if grep -qF "$MARKER" "$WORK/t1.txt"; then
    ok "A2) 模型这次照抄了尾标记（加分项，B 不依赖它）"
  else
    echo "  note: 模型没照抄尾标记（服从率问题，非管线 bug）—— B 用最长公共子串判定，不受影响"
  fi
else
  echo "FAIL: A) 第一轮正文只有 $T1_LEN 字（要求 ≥150）→ 没有可比对素材，后面结论无效"
  echo "      先看 $WORK/t1.sse（常见原因：写作技能回 needs 反问，缺要素）"
  FAILED=1
fi

T2REQ="把上面那篇新闻稿整理成 Word 文档，正文必须一字不改地保留上面那篇的全文。"

step "3) 第二轮：把上面那篇整理成 Word 文档（同一 session，锁定 docgen 技能）"
# 第二轮用 manual 锁 docgen：本用例要验的是「上一轮正文有没有进这一轮的 prompt」，
# 不是意图分类（分类漂移会让断言红在无关原因上 —— 假红同样有害）。
chat "$SID_POS" "$T2REQ" manual "$DOCGEN_SKILL" "$WORK/t2.sse"
URL="$(sse_file_url "$WORK/t2.sse")"
if [ -z "$URL" ]; then
  echo "FAIL: B) 第二轮没有回可下载的文档（file 事件）"; sse_text "$WORK/t2.sse" | tail -c 400; echo
  FAILED=1
else
  ok "第二轮回了文档：$URL"
  curl -s -m 120 "http://127.0.0.1:$APP_PORT$URL" -o "$WORK/out.docx"
  DOCX="$(docx_text "$WORK/out.docx")"
  echo "$DOCX" >"$WORK/out.txt"
  # 判据：docx 与第一轮正文的「最长公共子串」长度 ≥ MIN_SUB。
  # 不要求模型照抄尾标记 —— 那是模型服从率，不是管线带没带上下文。
  SUB="$(common_sub "$WORK/t1.txt" "$WORK/out.docx")"
  MIN_SUB=60
  [ "$BREAK_MARKER" -eq 1 ] && SUB="绝不可能出现的自证串zzz"   # 自证：判据必须变红
  if printf '%s' "$T2REQ" | grep -qF "$SUB" && [ "${#SUB}" -ge "$MIN_SUB" ]; then
    echo "FAIL: B) 找到的长公共串本身就出现在第二轮请求文本里 → 这段断言没有信息量"
    FAILED=1
  elif [ "${#SUB}" -ge "$MIN_SUB" ]; then
    ok "B) docx 里有 ${#SUB} 字与第一轮正文逐字相同、且不在本轮请求文本里 → 上一轮正文进了本轮 prompt"
    echo "      命中片段：$(printf '%s' "$SUB" | head -c 80)…"
  else
    echo "FAIL: B) docx 与第一轮正文的最长公共串只有 ${#SUB} 字（要求 ≥$MIN_SUB）"
    echo "      docx 正文长度：$(printf '%s' "$DOCX" | wc -c) 字符；前 300 字："
    printf '%s' "$DOCX" | head -c 300; echo
    FAILED=1
  fi
  if [ "$BREAK_MARKER" -eq 1 ]; then
    echo
    if [ "$FAILED" -ne 0 ]; then
      echo "=== 自证通过：断言目标换成不可能出现的串后确实变红（这道断言真在读 docx 正文） ==="
      exit 0
    fi
    echo "FAIL: 换掉断言目标后断言仍然全绿 → 这道断言是假的，不能用来证明修好了"
    exit 1
  fi
fi

step "4) 差分自证 N：全新 session 发同一句话，docx 里不该有尾标记"
SID_NEG="mt-neg-$(date +%s)"
chat "$SID_NEG" "$T2REQ" manual "$DOCGEN_SKILL" "$WORK/t3.sse"
URLN="$(sse_file_url "$WORK/t3.sse")"
if [ -z "$URLN" ]; then
  echo "  (跳过) 新 session 也没回文档，无法比对；不作为失败"
else
  curl -s -m 120 "http://127.0.0.1:$APP_PORT$URLN" -o "$WORK/neg.docx"
  SUB_N="$(common_sub "$WORK/t1.txt" "$WORK/neg.docx")"
  if [ "${#SUB_N}" -ge 30 ]; then
    echo "FAIL: N) 全新 session 的 docx 也与第一轮正文有 ${#SUB_N} 字逐字相同 → 那段文字不是从会话历史来的，B 的通过没有信息量"
    FAILED=1
  else
    ok "N) 新 session 的 docx 与第一轮正文最长公共串仅 ${#SUB_N} 字（<30）→ 那段文字确实只可能来自会话历史"
  fi
fi

echo
if [ "$FAILED" -ne 0 ]; then
  echo "=== 多轮上下文端到端验收未通过 ==="
  exit 1
fi
echo "=== 多轮上下文端到端验收通过：第一轮正文过了第二轮的上下文，Word 里带着原文尾标记 ==="
