#!/usr/bin/env bash
# 静态资源尺子（verify-live-assets.sh）的**自证**：尺子本身也会说谎。
#
# 为什么要有这个：这条尺子的第一版写死了 /js/chat.js（真实路径是 /assets/js/chat.js），
# 于是每次都抓到 19 字节的 404 页面 → 永远报「二进制换了但前端资源没换」。
# 一把恒红的尺子和一把恒绿的尺子一样没用 —— 而且更坏：它会让人去修一个不存在的故障。
#
# 这里用一个**本地桩站**把四种情形都跑一遍（不碰线上、不需要网络）：
#   ① 真串    → rc=0 且打 ASSETS_OK         （该绿的时候绿）
#   ② 假串    → rc=1 且**精确打那条 FAIL 行**（该红的时候红，且红在正确的位置）
#   ③ 错端口  → rc=2 且打 PREMISE_MISS       （前提不成立 ≠ 断言失败，两者必须分得开）
#   ④ 还原    → rc=0                         （红过一次还能回绿，排除「一次红了就再也绿不了」）
#   ⑤ 非常规路径（无 /assets 前缀、非 chat.js 文件名）→ rc=0
#      —— 这条专治「写死路径」：路径必须从服务端返回的 index.html 里现取。
#
# 退出码：0 全绿 / 1 有判据不成立。
set -uo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# 可被外部覆盖：变异自证那一条会指到「故意写死路径」的坏副本上
RULER="${RULER:-$SELF_DIR/verify-live-assets.sh}"
[ -f "$RULER" ] || { echo "PREMISE_MISS: 找不到尺子 $RULER"; exit 1; }

MARK="SF_RULER_真串_MARKER"
DOC="$(mktemp -d /tmp/ruler-doc.XXXXXX)"
SRV_PID=""
cleanup() {
  [ -n "$SRV_PID" ] && kill "$SRV_PID" >/dev/null 2>&1
  rm -rf "$DOC"
}
trap cleanup EXIT

# 桩站内容：真·特征串只写进 js；css 里放个不同串，用来验证「命中至少一个资源」
write_site() {
  local jspath="$1" marker="$2"
  # 先把上一轮的桩站清干净：留着旧的 /assets/js/chat.js 会让「写死路径」的坏副本
  # 蒙对（变异自证那一条就红不起来）—— 桩站必须每轮只存在**这一轮**的那些资源。
  find "$DOC" -mindepth 1 -delete 2>/dev/null
  mkdir -p "$DOC$(dirname "$jspath")"
  printf '<!doctype html><html><head><link rel="stylesheet" href="/assets/css/style.css?v=SELFTEST"></head><body><script src="%s?v=SELFTEST"></script></body></html>' "$jspath" > "$DOC/index.html"
  mkdir -p "$DOC/assets/css"
  # 桩文件必须 >1000 字节：尺子带「资源取不到或不完整（sz<1000）」的护栏（真资源几百 KB），
  # 桩太小会被这条护栏拦下 —— 那是**桩不合格**，不是尺子错。
  { printf '/* pad */\n'; for i in $(seq 1 60); do printf '.pad-%s{padding:%spx}\n' "$i" "$i"; done; printf '.ctk-mat{color:#333}\n'; } > "$DOC/assets/css/style.css"
  { printf '// pad\n'; printf 'var s = "%s";\n' "$marker"; for i in $(seq 1 60); do printf '// padding line %s padding padding padding\n' "$i"; done; } > "$DOC$jspath"
}

# 找一个空闲端口（不写死：本机可能被别的服务占着）
free_port() {
  python3 - <<'PY'
import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()
PY
}

PORT="$(free_port)"
[ -n "$PORT" ] || { echo "PREMISE_MISS: 拿不到空闲端口"; exit 1; }
BASE="http://127.0.0.1:$PORT"

write_site "/assets/js/chat.js" "$MARK"
( cd "$DOC" && exec python3 -m http.server "$PORT" --bind 127.0.0.1 ) >/tmp/ruler-selftest-srv.log 2>&1 &
SRV_PID=$!
for i in $(seq 1 40); do
  curl -s -o /dev/null -m 2 "$BASE/" && break
  sleep 0.25
done

FAIL=0
note() { echo "  $*"; }
bad()  { echo "✗ $*"; FAIL=1; }

run() { # run <期望rc> <说明> [特征串...]，可用 RUN_BASE 覆盖 BASE
  local want="$1" why="$2"; shift 2
  local out rc
  out="$(BASE="${RUN_BASE:-$BASE}" bash "$RULER" "$@" 2>&1)"; rc=$?
  echo "--- 情形：$why（期望 rc=$want，实得 rc=$rc）"
  printf '%s\n' "$out" | sed 's/^/    /'
  if [ "$rc" != "$want" ]; then
    bad "$why：退出码是 $rc，应为 $want"
  fi
  LAST_OUT="$out"
  LAST_RC="$rc"
}

# ① 真串 → 绿
run 0 "真特征串" "$MARK"
case "$LAST_OUT" in *ASSETS_OK*) note "✓ 打了 ASSETS_OK" ;; *) bad "真串情形没打 ASSETS_OK" ;; esac

# ② 假串 → 红，且必须是**预期那条** FAIL（不许是崩溃/别的错）
run 1 "假特征串" "SF_绝不存在的串_$RANDOM"
case "$LAST_OUT" in
  *"缺特征串"*) note "✓ 红在预期的断言上" ;;
  *) bad "假串情形的红不是预期那条 FAIL：$LAST_OUT" ;;
esac
case "$LAST_OUT" in *ASSETS_OK*) bad "假串情形竟然打了 ASSETS_OK（假绿）" ;; esac

# ③ 错端口 → 前提不成立 rc=2（和「断言失败 rc=1」必须分开）
DEAD_PORT="$(free_port)"
RUN_BASE="http://127.0.0.1:$DEAD_PORT" run 2 "端口 $DEAD_PORT 上没有服务" "$MARK"
RUN_BASE=""
case "$LAST_OUT" in *PREMISE_MISS*) note "✓ 报了 PREMISE_MISS" ;; *) bad "错端口情形没报 PREMISE_MISS" ;; esac

# ④ 还原 → 绿（红过之后还能回绿）
run 0 "还原回绿" "$MARK"
case "$LAST_OUT" in *ASSETS_OK*) note "✓ 回绿" ;; *) bad "还原后没能回绿" ;; esac

# ⑤ 非常规资源路径：没有 /assets 前缀、文件名也不叫 chat.js
#    写死路径的尺子会在这一条上原形毕露（它会去抓 /assets/js/chat.js 之外的错地址）。
write_site "/weird/nest/chat.js" "$MARK"
run 0 "非常规资源路径（/weird/nest/chat.js）" "$MARK"

# ⑥ 变异自证：把尺子改成「写死资源路径」的坏副本，⑤ 那条必须当场转红。
#    没有这一条，前面 5 个绿都可能是「尺子根本读不懂桩站」换来的。
BROKEN="$(mktemp /tmp/ruler-broken.XXXXXX.sh)"
python3 - "$RULER" "$BROKEN" <<'PY'
import re, sys
src = open(sys.argv[1], encoding="utf-8").read()
# 把「资源列表从服务端 index.html 里现取」换成写死 /assets/js/chat.js
# —— 这正是历史假红的成因（真实路径是 /assets/js/chat.js 时它蒙对了，
#    换成 /weird/nest/chat.js 就抓 404 页面并谎报「资源没换」）。
new, n = re.subn(r'mapfile -t ASSETS < <\(printf.*?\)\n', 'ASSETS=("/assets/js/chat.js")\n', src, flags=re.S)
assert n == 1, f"注入点没命中（命中 {n} 次）—— 尺子被改过就要同步改这里"
open(sys.argv[2], "w", encoding="utf-8").write(new)
PY
if [ ! -s "$BROKEN" ]; then
  bad "变异副本没造出来（注入点失配）"
else
  echo "--- 情形：变异副本（写死资源路径）在非常规路径上必须红"
  RUN_BASE="" RULER="$BROKEN" run 1 "变异副本 · 非常规资源路径" "$MARK"
  case "$LAST_OUT" in
    *"资源取不到或不完整"*) note "✓ 坏副本精确红在「资源取不到」（404 页面被识破）" ;;
    *) bad "坏副本没红在预期位置：$LAST_OUT" ;;
  esac
  RUN_BASE="" RULER="$SELF_DIR/verify-live-assets.sh"
fi
rm -f "$BROKEN"

echo
if [ "$FAIL" != "0" ]; then
  echo "RULER_SELFTEST_RED"
  exit 1
fi
echo "RULER_SELFTEST_OK 情形=6"
