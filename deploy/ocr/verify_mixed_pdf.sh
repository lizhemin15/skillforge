#!/usr/bin/env bash
# 混合型 PDF 解析回归防线（对应用户反馈 ②：可选中部分不该送 OCR；扫描页不该被静默丢弃）。
#
# 背景事故：ocrd 旧逻辑逐页判 `if txt:` —— 只要文本层非空就直取。真实扫描件几乎都被
# 扫描软件写入了一层「页码 + 品牌水印」的薄文本层（≤20 字），于是真扫描页一页都不送
# OCR，整本材料只剩水印。要命的是 chars 非 0，全链路绿灯，训练出来的技能与素材无关。
#
# 三份测试料与判据：
#   textonly   : 全页有文字层      → ocr_pages 必须为 0（可选中 PDF 零 OCR，秒回）
#   mixed_plain: 2 扫描页+3 文字页 → 扫描页关键词必须抽到（走 OCR），文字页也必须在
#   mixed_wm   : 同上但扫描页叠薄文本层 → **本次事故的回归断言**，扫描页关键词必须抽到
#
# 自证：`--break` 用 OCRD_MIN_PAGE_CHARS=1 起服务（等价旧的 `if txt:`），同样三条断言
# 必须变红。红在崩溃/起不来上不算红（那是环境问题，不是「防线能抓 bug」）。
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
BUILD="$ROOT/testdata/mixed/build_mixed_pdf.py"

PORT=""
BREAK=0
while [ $# -gt 0 ]; do
  case "$1" in
    --port) PORT="$2"; shift 2 ;;
    --break) BREAK=1; shift ;;
    *) echo "unknown arg: $1"; exit 2 ;;
  esac
done

FAILED=0
fail() { echo "FAIL: $*"; FAILED=1; }
ok()   { echo "  ok: $*"; }

WORK="$(mktemp -d /tmp/verify_mixed.XXXXXX)"
PID=""

cleanup() { [ -n "$PID" ] && { kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; }; }
trap cleanup EXIT

echo "=== 0) 生成测试料 ==="
if [ ! -f "$BUILD" ]; then
  echo "FAIL: 缺 $BUILD，无法生成混合 PDF 测试料"; exit 1
fi
python3 "$BUILD" "$WORK/textonly.pdf"  --scan 0 --text 3 --tag TEXTLINE >/dev/null || { echo "FAIL: 造 textonly 失败"; exit 1; }
python3 "$BUILD" "$WORK/mixed.pdf"    --scan 2 --text 3 --tag ALPHA    >/dev/null || { echo "FAIL: 造 mixed 失败"; exit 1; }
python3 "$BUILD" "$WORK/mixed_wm.pdf" --scan 2 --text 3 --tag BETA --watermark >/dev/null || { echo "FAIL: 造 mixed_wm 失败"; exit 1; }
ok "三份测试料就绪（$WORK）"

if [ -z "$PORT" ]; then
  PORT=8098
  if [ "$BREAK" -eq 1 ]; then
    echo "=== 1) 启动 ocrd（port=$PORT, 自证注入 OCRD_MIN_PAGE_CHARS=1 等价旧逻辑）==="
    OCRD_MIN_PAGE_CHARS=1 python3 "$ROOT/deploy/ocr/ocrd.py" --port "$PORT" >"$WORK/ocrd.log" 2>&1 &
  else
    echo "=== 1) 启动 ocrd（port=$PORT, OCRD_MIN_PAGE_CHARS=${OCRD_MIN_PAGE_CHARS:-默认40}）==="
    python3 "$ROOT/deploy/ocr/ocrd.py" --port "$PORT" >"$WORK/ocrd.log" 2>&1 &
  fi
  PID=$!
  READY=0
  for _ in $(seq 1 40); do
    if ! kill -0 "$PID" 2>/dev/null; then
      fail "ocrd 自己退出了（看日志，这是环境问题不是防线问题）"
      sed -n '1,20p' "$WORK/ocrd.log"
      exit 1
    fi
    curl -fsS "http://127.0.0.1:$PORT/health" >/dev/null 2>&1 && { READY=1; break; }
    sleep 2
  done
  [ "$READY" -eq 1 ] || { fail "80 秒内 /health 未就绪"; sed -n '1,20p' "$WORK/ocrd.log"; exit 1; }
  ok "health 就绪"
fi

# 单份断言：$1=文件 $2=标签 $3=要求 ocr_pages>=N $4=要求 ocr_pages==0(1/0) $5=必须命中的关键词
check() {
  local f="$1" tag="$2" minocr="$3" zeroocr="$4" kw="$5"
  local out="$WORK/$(basename "$f").json"
  local t0 t1
  t0=$(date +%s.%N)
  curl -s -m 900 -F "file=@$f" "http://127.0.0.1:$PORT/extract" -o "$out" || { fail "$tag: 请求失败"; return; }
  t1=$(date +%s.%N)
  echo "--- $tag"
  python3 - "$out" "$tag" "$minocr" "$zeroocr" "$kw" "$t0" "$t1" <<'PY'
import json, sys
path, tag, minocr, zeroocr, kw, t0, t1 = sys.argv[1:8]
minocr, zeroocr = int(minocr), int(zeroocr)
try:
    d = json.load(open(path))
except Exception as e:
    print(f"FAIL: {tag}: 响应不是 JSON ({e})"); sys.exit(3)
bad = []
if not d.get("ok"):
    bad.append(f"ok!=true err={d.get('error')!r}")
st = d.get("stats") or {}
pages = st.get("pages", 0); ocr = st.get("ocr_pages", 0); txt = st.get("text_pages", 0)
text = d.get("text") or ""
if pages == 0:
    bad.append("stats 缺 pages 字段（诊断信息没回传，上层无法判断素材可信度）")
if minocr and ocr < minocr:
    bad.append(f"ocr_pages={ocr} < {minocr}（扫描页没走 OCR，正文会丢）")
if zeroocr and ocr != 0:
    bad.append(f"ocr_pages={ocr} != 0（可选中 PDF 不该送 OCR）")
if kw and kw not in text:
    bad.append(f"关键词 {kw!r} 没抽到（该页正文丢了）")
if txt + ocr + st.get("empty_pages", 0) != pages:
    bad.append(f"页数对不上: text={txt} ocr={ocr} empty={st.get('empty_pages')} pages={pages}")
ms = int((float(t1) - float(t0)) * 1000)
print(f"    stats={st} chars={d.get('chars')} 耗时={ms}ms")
if bad:
    for b in bad:
        print(f"FAIL: {tag}: {b}")
    sys.exit(3)
print(f"  ok: {tag} 逐页判定正确")
PY
  [ $? -eq 0 ] || FAILED=1
}

echo "=== 2) 断言 ==="
check "$WORK/textonly.pdf"  "textonly(可选中,期望零OCR)"     0 1 ""
check "$WORK/mixed.pdf"     "mixed(2扫描页,期望OCR>=2)"      2 0 "ALPHA扫描段关键句1"
check "$WORK/mixed_wm.pdf"  "mixed_wm(薄文本层,回归断言)"    2 0 "BETA扫描段关键句1"

if [ "$BREAK" -eq 1 ]; then
  echo "=== 自证模式：期望上述断言变红 ==="
  if [ "$FAILED" -eq 0 ]; then
    echo "FAIL: --break 下断言竟然全绿 → 这道防线抓不到「薄文本层吞掉扫描页」这个 bug"
    exit 1
  fi
  echo "=== 自证通过：注入旧逻辑后防线确实变红 ==="
  exit 0
fi

if [ "$FAILED" -ne 0 ]; then
  echo "=== 防线未通过 ==="
  exit 1
fi
echo "=== 防线通过：文本层够厚直取、薄文本层/扫描页走 OCR、正文关键词全命中 ==="
