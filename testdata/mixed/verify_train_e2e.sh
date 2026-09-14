#!/usr/bin/env bash
#
# verify_train_e2e.sh —— 端到端验收：**真的训练一次**，用「前几页扫描 + 后面可选中」的
# 混合型 PDF，回答用户那句「生成的 skill 似乎和我给的内容完全没有关系」到底修没修好。
#
# 为什么必须端到端（单测不够）：
#   单测只能证明「判据函数在给定 stats 下会拦」；真实链路是
#   HTTP 上传 → ocrd 逐页判文本层 → 落 source/*.txt → 素材门禁 → LLM 抽特征 → 出技能。
#   任何一环把内容掉在地上，产出的技能就与素材无关，而**每一环单独看都是绿的**。
#
# 四条断言（B/C 的实现见同目录 assert_skill_material.sh）：
#   A) 解析层：ocrd 对混合 PDF 返回的文本里含扫描页关键句（薄文本层也得走 OCR）
#   B) 素材层：训练后 source/*.txt 里含扫描页 + 文字层页两侧关键句
#   C) 产物层：技能产物（非 source/）里出现独有主题词「蒲公英月报」
#   D) 负例：解析出来等于没读到正文（纯图无字 PDF）时必须**硬失败**，
#      且不许留下半成品技能目录（静默降级生成无关技能正是老事故的根因）
#
# 用法：
#   testdata/mixed/verify_train_e2e.sh                 # 全流程（真调 LLM，几分钟）
#   testdata/mixed/verify_train_e2e.sh --ocr-only      # 只跑 A（不烧 LLM 额度）
#   testdata/mixed/verify_train_e2e.sh --neg-only      # 只跑 D
#   testdata/mixed/verify_train_e2e.sh --break-ocr     # 自证 A：注入旧行为，A 必须变红
#   testdata/mixed/verify_train_e2e.sh --break-artifact <技能目录>
#                                                      # 自证 B/C：抠掉扫描页正文，必须变红
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
ENVF="${SKILLFORGE_ENV_FILE:-/opt/skillforge/skillforge.env}"

OCR_PORT=8097
APP_PORT=8099
TOPIC="蒲公英月报"
RUN_FULL=1
BREAK_OCR=0
BREAK_ARTIFACT=""

while [ $# -gt 0 ]; do
  case "$1" in
    --ocr-only)     RUN_FULL=0; RUN_NEG=0; shift ;;
    --neg-only)     RUN_FULL=0; RUN_NEG=1; OCR_ONLY=0; shift ;;
    --break-ocr)    BREAK_OCR=1; RUN_FULL=0; RUN_NEG=0; shift ;;
    --break-artifact) BREAK_ARTIFACT="${2:-}"; RUN_FULL=0; shift 2 ;;
    *) echo "unknown arg: $1"; exit 2 ;;
  esac
done
RUN_NEG="${RUN_NEG:-1}"
OCR_ONLY="${OCR_ONLY:-0}"

FAILED=0
ok()   { echo "  ok: $*"; }
step() { echo; echo "=== $* ==="; }

WORK="$(mktemp -d /tmp/verify_e2e.XXXXXX)"
OCR_PID=""; APP_PID=""
cleanup() {
  [ -n "$APP_PID" ] && { kill "$APP_PID" 2>/dev/null || true; wait "$APP_PID" 2>/dev/null || true; }
  [ -n "$OCR_PID" ] && { kill "$OCR_PID" 2>/dev/null || true; wait "$OCR_PID" 2>/dev/null || true; }
}
trap cleanup EXIT

# ---------- 自证 B/C：抠掉扫描页正文，断言必须变红 ----------
if [ -n "$BREAK_ARTIFACT" ]; then
  step "自证 B/C：把产物里的扫描页正文抠掉，assert_skill_material.sh 必须变红"
  [ -d "$BREAK_ARTIFACT" ] || { echo "FAIL: 目录不存在 $BREAK_ARTIFACT"; exit 2; }
  cp -r "$BREAK_ARTIFACT" "$WORK/broken"
  python3 - "$WORK/broken" <<'PY'
import pathlib, sys
n = 0
for f in pathlib.Path(sys.argv[1]).rglob("*.txt"):
    lines = f.read_text(encoding="utf-8", errors="ignore").splitlines(True)
    keep = [l for l in lines if "落款联系人固定写" not in l]
    if len(keep) != len(lines):
        f.write_text("".join(keep), encoding="utf-8"); n += 1
print(f"  剥离扫描页正文的文件数：{n}")
PY
  if bash "$HERE/assert_skill_material.sh" "$WORK/broken" "$TOPIC"; then
    echo "FAIL: 注入「扫描页内容丢失」后断言竟然通过 → 这道防线抓不到用户报的 bug"
    exit 1
  fi
  echo "  ok: 注入后确实变红（自证通过）"
  step "还原：原始产物上断言必须变绿"
  if bash "$HERE/assert_skill_material.sh" "$BREAK_ARTIFACT" "$TOPIC"; then
    echo "  ok: 还原后变绿（双向自证完成）"
    exit 0
  fi
  echo "FAIL: 原始产物本身就不通过断言"
  exit 1
fi

step "0) 造测试料"
python3 "$HERE/build_topic_pdf.py" "$WORK/topic.pdf" --topic "$TOPIC" \
  --scan 2 --text 2 --watermark >/dev/null || { echo "FAIL: 造混合 PDF 失败"; exit 1; }
python3 "$HERE/build_topic_pdf.py" "$WORK/blank.pdf" --topic "$TOPIC" \
  --scan 0 --text 0 --blank 3 >/dev/null || { echo "FAIL: 造纯图无字 PDF 失败"; exit 1; }
ok "topic.pdf（2 扫描页带薄文本层 + 2 文字层页）、blank.pdf（3 页纯图无字）"

step "1) 起 ocrd（源码版，$OCR_PORT）"
if [ "$BREAK_OCR" -eq 1 ]; then
  echo "  [自证注入] OCRD_MIN_PAGE_CHARS=1 —— 等价旧的「文本层非空即直取，扫描页全丢」"
  OCRD_MIN_PAGE_CHARS=1 python3 "$ROOT/deploy/ocr/ocrd.py" --port "$OCR_PORT" \
    >"$WORK/ocrd.log" 2>&1 &
else
  python3 "$ROOT/deploy/ocr/ocrd.py" --port "$OCR_PORT" >"$WORK/ocrd.log" 2>&1 &
fi
OCR_PID=$!
READY=0
for _ in $(seq 1 45); do
  kill -0 "$OCR_PID" 2>/dev/null || { echo "FAIL: ocrd 自己退了"; sed -n '1,15p' "$WORK/ocrd.log"; exit 1; }
  curl -fsS "http://127.0.0.1:$OCR_PORT/health" >/dev/null 2>&1 && { READY=1; break; }
  sleep 2
done
[ "$READY" -eq 1 ] || { echo "FAIL: 90 秒内 ocrd /health 未就绪"; sed -n '1,15p' "$WORK/ocrd.log"; exit 1; }
sleep 6   # RapidOCR 模型惰性加载：进程起来不等于模型就绪
ok "health 就绪"

step "A) 解析层断言：扫描页正文必须抽出来（薄文本层也走 OCR）"
curl -s -m 900 -F "file=@$WORK/topic.pdf" "http://127.0.0.1:$OCR_PORT/extract" -o "$WORK/extract.json"
python3 - "$WORK/extract.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
text = d.get("text") or ""
st = d.get("stats") or {}
print(f"    stats={st} chars={d.get('chars')}")
bad = []
if not d.get("ok"):
    bad.append(f"ok!=true err={d.get('error')!r}")
if "落款联系人固定写" not in text:
    bad.append("扫描页正文没抽出来（薄文本层被当正文直取了？）")
if "写作检查清单" not in text:
    bad.append("文字层页正文丢了")
if st.get("ocr_pages", 0) < 2:
    bad.append(f"ocr_pages={st.get('ocr_pages')} < 2（扫描页没走 OCR）")
if st.get("text_pages", 0) < 2:
    bad.append(f"text_pages={st.get('text_pages')} < 2（可选中页没直取）")
for b in bad:
    print(f"FAIL: A) {b}")
sys.exit(3 if bad else 0)
PY
A_RC=$?
if [ "$A_RC" -eq 0 ]; then ok "A) 扫描页与文字层页都被正确读到"; else FAILED=1; fi

if [ "$BREAK_OCR" -eq 1 ]; then
  echo
  if [ "$FAILED" -ne 0 ]; then
    echo "=== 自证通过：注入旧行为后 A) 确实变红 ==="
    exit 0
  fi
  echo "FAIL: 注入旧行为后 A) 仍然全绿 → 这条断言抓不到「扫描页被静默丢弃」"
  exit 1
fi

if [ "$OCR_ONLY" -eq 1 ] && [ "$RUN_FULL" -eq 0 ]; then
  echo; [ "$FAILED" -eq 0 ] && { echo "=== A) 通过 ==="; exit 0; } || { echo "=== A) 未通过 ==="; exit 1; }
fi
[ "$RUN_FULL" -eq 0 ] && [ "$RUN_NEG" -eq 0 ] && { echo; echo "=== A) 通过 ==="; exit 0; }

step "2) 起被测服务（临时数据目录，拷线上库以复用已配好的 LLM）"
TMPDATA="$WORK/data"
mkdir -p "$TMPDATA"
[ -f /opt/skillforge/data/skillforge.db ] && cp /opt/skillforge/data/skillforge.db "$TMPDATA/skillforge.db"
[ -f "$ENVF" ] || { echo "FAIL: 缺 $ENVF（没有 LLM 配置跑不了训练）"; exit 1; }
set -a
# shellcheck disable=SC1090
. "$ENVF"
set +a
export SKILLFORGE_ADDR="127.0.0.1:$APP_PORT"
export SKILLFORGE_DATA_DIR="$TMPDATA"
export SKILLFORGE_DB="$TMPDATA/skillforge.db"
export SKILLFORGE_OCR_URL="http://127.0.0.1:$OCR_PORT"
export SKILLFORGE_PUBLIC_URL="http://127.0.0.1:$APP_PORT"
export SKILLFORGE_OCR_TIMEOUT="${SKILLFORGE_OCR_TIMEOUT:-900s}"

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

curl -s -o "$WORK/login.json" -X POST "http://127.0.0.1:$APP_PORT/api/login" \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"${SKILLFORGE_ADMIN_USER:-admin}\",\"password\":\"${SKILLFORGE_ADMIN_PASS:-}\"}"
AUTH_TOK="$(python3 -c "import json;print((json.load(open('$WORK/login.json')) or {}).get('token',''))" 2>/dev/null || true)"
[ -n "$AUTH_TOK" ] || { echo "FAIL: 登录失败"; cat "$WORK/login.json"; exit 1; }
ok "已登录"

train() {  # $1=技能名 $2=PDF $3=输出 sse 文件
  curl -s -N -m 2400 -X POST "http://127.0.0.1:$APP_PORT/api/admin/train" \
    -H "Authorization: Bea""rer $AUTH_TOK" \
    -F "name=$1" -F "category=测试" -F "description=端到端验收" \
    -F "requirement=按素材风格写作" -F "files=@$2" >"$3"
}
# slug 必须**从服务端回报里取**，不能在本地猜。
# 踩过一次：本地 slugify 用 `s/[^a-z0-9]\+/-/g` 把中文全干掉，算出 "e2e123348"，
# 而服务端保留中文只做小写 → 真实 slug 是 "蒲公英e2e123348"。于是技能明明训练成功、
# 产物也正确，断言却红在「技能目录不存在」——**假红**。假红和假绿一样坏：
# 它让人去查一个不存在的 bug，或者更糟，让人开始不信这道门禁。
slug_from_sse() {  # $1=sse 文件；取 done 事件里的真实 slug
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
    if d.get("type") == "done":
        m = re.search(r'"slug"\s*:\s*"([^"]+)"', str(d.get("data", "")))
        if m:
            print(m.group(1)); break
PY
}

if [ "$RUN_NEG" -eq 1 ]; then
  step "D) 负例：纯图无字素材必须硬失败，且不留半成品技能"
  NEGNAME="e2e负例$(date +%H%M%S)"
  # 负例不留半成品：用**训练前后目录快照求差**，而不是拿技能名去猜 slug。
  # 名字含中文/大写/特殊字符时手写 slug 规则必然对不上，快照差值不吃这套猜测——
  # 「没有任何新目录」本来就是我们要断言的事实本身。
  ls -1 "$TMPDATA/skills" 2>/dev/null | sort >"$WORK/skills.before" || true
  train "$NEGNAME" "$WORK/blank.pdf" "$WORK/neg.sse"
  if grep -q '"type":"error"' "$WORK/neg.sse"; then
    ok "训练按预期报错中止"
    grep -o '"data":"[^"]\{0,120\}' "$WORK/neg.sse" | tail -2
  else
    echo "FAIL: D) 素材读不出来却仍跑完训练（静默降级！）"; tail -3 "$WORK/neg.sse"; FAILED=1
  fi
  grep -q '"type":"done"' "$WORK/neg.sse" && { echo "FAIL: D) 居然回了 done（老事故复发）"; FAILED=1; }
  ls -1 "$TMPDATA/skills" 2>/dev/null | sort >"$WORK/skills.after" || true
  NEWDIRS="$(comm -13 "$WORK/skills.before" "$WORK/skills.after")"
  [ -n "$NEWDIRS" ] && { echo "FAIL: D) 失败路径留下半成品技能目录：$NEWDIRS"; FAILED=1; }
fi

if [ "$RUN_FULL" -eq 1 ]; then
  step "3) 正例：训练技能（混合型 PDF，前 2 页扫描 + 后 2 页可选中）"
  NAME="蒲公英E2E$(date +%H%M%S)"
  train "$NAME" "$WORK/topic.pdf" "$WORK/train.sse"
  echo "--- 训练进度（尾部）"
  python3 - "$WORK/train.sse" <<'PY'
import json, sys
for ln in open(sys.argv[1], encoding="utf-8", errors="ignore"):
    ln = ln.strip()
    if ln.startswith("data: "):
        try: d = json.loads(ln[6:])
        except Exception: continue
        print(f"    [{d.get('type')}] {str(d.get('data',''))[:150]}")
PY
  SKSLUG="$(slug_from_sse "$WORK/train.sse")"
  if [ -z "$SKSLUG" ]; then
    # 不退回 slugify：猜出来的路径会让后面所有断言红在一个假原因上（踩过）。
    echo "FAIL: done 事件里没拿到 slug，无法定位技能目录（别猜，先看 train.sse）"
    FAILED=1
  fi
  SK="$TMPDATA/skills/$SKSLUG"
  if [ -n "$SKSLUG" ] && [ ! -d "$SK" ]; then
    echo "FAIL: 服务端报了 slug=$SKSLUG，但目录不存在：$SK"
    ls -1 "$TMPDATA/skills" | head -10 | sed 's/^/      实际目录：/'
    FAILED=1
  fi

  step "B/C) 素材层 + 产物层断言（assert_skill_material.sh）"
  if bash "$HERE/assert_skill_material.sh" "$SK" "$TOPIC"; then
    ok "B/C 通过：扫描页正文进了素材，技能由这份素材长出来"
  else
    echo "FAIL: B/C 未通过（技能目录 $SK）"; FAILED=1
  fi
  echo "  [提示] 双向自证：bash $HERE/verify_train_e2e.sh --break-artifact $SK"
fi

echo
if [ "$FAILED" -ne 0 ]; then
  echo "=== 端到端验收未通过 ==="
  exit 1
fi
echo "=== 端到端验收通过：混合 PDF 的扫描页与文字层页都进了素材，技能由素材长出来 ==="
