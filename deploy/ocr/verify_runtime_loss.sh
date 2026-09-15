#!/usr/bin/env bash
#
# 运行时目录丢失（TMPDIR 解包目录被清）防线 —— 对应用户反馈 ③「生成的 skill 和我给的素材
# 完全没有关系」的**真凶**。
#
# 事故链（2026-09-16 线上坐实）：
#   ocrd 是 PyInstaller onefile，启动时把 rapidocr 的模型/配置解包到 $TMPDIR/_MEIxxxx/。
#   默认 TMPDIR=/tmp，而 /usr/lib/tmpfiles.d/tmp.conf 是 `D /tmp 1777 root root -`（无保留期），
#   systemd-tmpfiles-clean.timer 每天 04:05 清一次 → 运行中的解包目录被删。
#   已加载进内存的模型照常推理，所以小件一切正常；等到引擎回收重建（RECYCLE_EVERY=20 页）
#   要重读 config.yaml 时才炸：
#     {"ok": false, "error": "[Errno 2] No such file or directory: '/tmp/_MEI…/config.yaml'"}
#   引擎一旦重建失败就永久坏掉 —— 此后每个需要 OCR 的请求 0 秒即错，直到进程重启。
#   用户可见：扫描件/混合 PDF 解析整份失败 → 素材为空 → 训练出与素材无关的技能。
#
# 两道防线（本脚本逐条测）：
#   A 部署层：systemd 单元给 ocrd 专用 TMPDIR（__PREFIX__/run/ocr-tmp），tmpfiles 不碰它。
#   B 代码层：ocrd 自己发现解包目录没了就 ①/health 如实报坏 ②请求明确报人话 ③非零退出让
#     systemd 重启自愈。
#
# 为什么要**冻结二进制**才测：非冻结运行（python3 deploy/ocr/ocrd.py）没有解包目录，
# 这类事故根本不存在，测出来的绿是假绿。二进制不在 → 退出 2（不是 PASS）。
#
# 用法：
#   bash deploy/ocr/verify_runtime_loss.sh                      # 默认 /opt/skillforge/bin/ocrd
#   OCRD_BIN=dist-bin/ocrd-amd64 bash deploy/ocr/verify_runtime_loss.sh
#   bash deploy/ocr/verify_runtime_loss.sh --break              # 自证：关掉守卫，必须变红
#
# 自证两种注入体（都用真的）：
#   1) --break                      → 新二进制 + OCRD_RUNTIME_GUARD=off（等价「没有守卫」）
#   2) OCRD_BIN=<线上旧二进制>       → 旧版本压根没有守卫，红得更真
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
BUILD="$ROOT/testdata/mixed/build_mixed_pdf.py"

BIN="${OCRD_BIN:-/opt/skillforge/bin/ocrd}"
PORT="${OCRD_PORT:-18093}"
BREAK=0
while [ $# -gt 0 ]; do
  case "$1" in
    --break) BREAK=1; shift ;;
    --port)  PORT="$2"; shift 2 ;;
    --bin)   BIN="$2"; shift 2 ;;
    *) echo "unknown arg: $1"; exit 2 ;;
  esac
done

[ -x "$BIN" ] || { echo "SKIP/FAIL: 冻结二进制不存在或不可执行：$BIN"; echo "（本事故只存在于 PyInstaller 冻结件；请用 OCRD_BIN=dist-bin/ocrd-amd64 指定）"; exit 2; }
[ -f "$BUILD" ] || { echo "FAIL: 缺 $BUILD，无法生成扫描件测试料"; exit 1; }

WORK="$(mktemp -d /tmp/verify_rtloss.XXXXXX)"
RT="$WORK/ocr-tmp"                       # 专用临时目录：模拟修 A 之后 systemd 给的 TMPDIR
mkdir -p "$RT"; chmod 700 "$RT"
RTMEI=""                                 # 第一次启动解包出来的 _MEI 目录（故障注入点）
LOG="$WORK/ocrd.log"
SUP=""
FAILED=0
RED_KINDS=""                             # 记录红在哪一类断言上，防止「红在起不来」被当自证通过
fail() { echo "FAIL: $*"; FAILED=1; }
ok()   { echo "  ok: $*"; }

cleanup() {
  if [ -n "$SUP" ]; then
    pkill -P "$SUP" 2>/dev/null || true
    kill "$SUP" 2>/dev/null || true
  fi
  pkill -f "$BIN --port $PORT" 2>/dev/null || true
  sleep 0.3
  rm -rf "$WORK"
}
trap cleanup EXIT

# 监督循环：等价 systemd 的 Restart=on-failure（进程非零退出就拉起来）。
# 没有这一段，「非零退出 → systemd 重启 → 恢复」这条自愈链就测不出来。
# 注意 start_supervised 内部**必须前台等进程结束**才能拿到真实退出码：
# 写成 `... &` 再 echo $? 拿到的是「后台启动成功」的 0，那样 S5 永远绿（假绿）。
start_supervised() {
  if [ "$BREAK" -eq 1 ]; then
    OCRD_RUNTIME_GUARD=off TMPDIR="$RT" "$BIN" --port "$PORT" >>"$LOG" 2>&1
  else
    TMPDIR="$RT" "$BIN" --port "$PORT" >>"$LOG" 2>&1
  fi
  local rc=$?
  echo "EXIT rc=$rc" >>"$LOG"
  return $rc
}
: >"$LOG"
( while :; do start_supervised || true; sleep 0.3; done ) &
SUP=$!

wait_health() { # $1=超时秒数 $2=阶段名
  local t="$1" tag="$2" i
  for i in $(seq 1 "$t"); do
    curl -fsS -m 5 "http://127.0.0.1:$PORT/health" >"$WORK/health.json" 2>/dev/null && return 0
    sleep 1
  done
  fail "$tag: ${t}s 内 /health 未就绪"
  sed -n '1,20p' "$LOG"
  return 1
}

jget() { python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get(sys.argv[2]))' "$1" "$2" 2>/dev/null; }

echo "=== 0) 测试料：3 页纯扫描件（必须走 OCR 才有正文）==="
python3 "$BUILD" "$WORK/scan3.pdf" --scan 3 --text 0 --tag RTLOSS >/dev/null || { echo "FAIL: 造扫描件失败"; exit 1; }
ok "scan3.pdf 就绪（$(stat -c%s "$WORK/scan3.pdf")B）"

echo "=== 1) 起服务（TMPDIR=$RT，冻结二进制=$BIN，break=$BREAK）==="
wait_health 120 "启动" || { echo "FAIL: 服务起不来，这是环境问题不是防线问题"; exit 1; }
D1="$(jget "$WORK/health.json" runtime_dir)"
V="$(jget "$WORK/health.json" version)"
ok "health 就绪 version=$V runtime_dir=$D1"
case "$D1" in
  "$RT"/*) ok "S1 解包目录落在专用 TMPDIR 下 → 修 A（TMPDIR 生效）成立" ;;
  /tmp/*|"/tmp") fail "S1 解包目录落在 /tmp → 会被 systemd-tmpfiles 清掉（TMPDIR 没生效）" ;;
  ""|"None") fail "S1 拿不到 runtime_dir → 不是冻结二进制，本脚本的绿是假绿" ;;
  *) fail "S1 解包目录位置意外：$D1（期望在 $RT 下）" ;;
esac
[ "$(jget "$WORK/health.json" runtime_ok)" = "True" ] \
  && ok "S1 /health 报 runtime_ok=true（健康态）" \
  || fail "S1 健康态下 runtime_ok 不为 true"
RTMEI="$(dirname "$D1")"   # 只取到 $RT 这一层，下面用 glob 找 _MEI*

echo "=== 2) 前提断言：故障前这份扫描件能真抽出正文（否则后面「恢复」无从对比）==="
curl -s -m 600 -F "file=@$WORK/scan3.pdf" "http://127.0.0.1:$PORT/extract" -o "$WORK/before.json"
python3 - "$WORK/before.json" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
if not d.get("ok"): print(f"FAIL: 故障前抽取就失败：ok=false err={d.get('error')!r}"); sys.exit(1)
st=d.get("stats") or {}
if st.get("ocr_pages",0) < 3: print(f"FAIL: 故障前 ocr_pages={st.get('ocr_pages')} < 3（这份料没走 OCR，前提不成立）"); sys.exit(1)
if "RTLOSS扫描段关键句1" not in (d.get("text") or ""): print("FAIL: 故障前关键词没抽到（前提不成立）"); sys.exit(1)
print(f"  ok: S2 故障前抽取正常 stats={st} chars={d.get('chars')}")
PY
[ $? -eq 0 ] || { fail "S2 前提不成立"; RED_KINDS="$RED_KINDS 前提"; }

echo "=== 3) 注入故障：删掉解包目录（正是 systemd-tmpfiles 干的事）==="
GOT="$(ls -d "$RT"/_MEI* 2>/dev/null | head -1)"
[ -n "$GOT" ] || { fail "S3 前置：$RT 下找不到 _MEI* 解包目录"; exit 1; }
rm -rf "$GOT"
ok "已删除 $GOT（模拟 tmpfiles 清理）"

curl -fsS -m 5 "http://127.0.0.1:$PORT/health" >"$WORK/health2.json" 2>/dev/null
ROK="$(jget "$WORK/health2.json" runtime_ok)"; HOK="$(jget "$WORK/health2.json" ok)"
if [ "$ROK" = "False" ] && [ "$HOK" = "False" ]; then
  ok "S3 /health 如实报坏 runtime_ok=false ok=false（监控能发现）"
else
  fail "S3 解包目录已丢，/health 仍报 runtime_ok=$ROK ok=$HOK → 服务在「自己坏了但看起来正常」状态，监控瞎"
  RED_KINDS="$RED_KINDS 健康可见性"
fi

echo "=== 4) 坏掉之后的请求：必须说人话 + 秒回 + 声明自愈（不许回 Errno 2）==="
T0=$(date +%s.%N)
curl -s -m 120 -F "file=@$WORK/scan3.pdf" "http://127.0.0.1:$PORT/extract" -o "$WORK/after.json"
CRC=$?
T1=$(date +%s.%N)
MS=$(python3 -c "print(int(($T1-$T0)*1000))")
if [ "$CRC" -ne 0 ] && [ ! -s "$WORK/after.json" ]; then
  # curl 非零且没有响应体：不能判成「防线坏了」，也不能判成过 —— 如实说明（避免狼来了）
  fail "S4 请求没有拿到响应体（curl rc=$CRC）→ 本项证据缺失，请人工看日志：$LOG"
  RED_KINDS="$RED_KINDS 无响应体"
else
  python3 - "$WORK/after.json" "$MS" <<'PY'
import json,sys
p,ms=sys.argv[1],int(sys.argv[2])
try: d=json.load(open(p))
except Exception as e: print(f"FAIL: S4 响应不是 JSON（{e}）"); sys.exit(1)
bad=[]
if d.get("ok") is not False: bad.append(f"ok={d.get('ok')!r}（坏掉的服务仍报成功）")
err=str(d.get("error") or "")
if "Errno 2" in err or "No such file" in err:
    bad.append(f"error 还是原始 errno，没说人话：{err!r}")
if "运行时目录" not in err: bad.append(f"error 没点明「运行时目录丢失」：{err!r}")
if d.get("runtime_ok") is not False: bad.append(f"runtime_ok={d.get('runtime_ok')!r}（没暴露坏状态）")
if d.get("self_healing") is not True: bad.append(f"self_healing={d.get('self_healing')!r}（没说会自愈）")
if ms > 5000: bad.append(f"耗时 {ms}ms > 5s（坏掉还让调用方白等上传）")
print(f"    响应：ok={d.get('ok')} runtime_ok={d.get('runtime_ok')} self_healing={d.get('self_healing')} 耗时={ms}ms error={err[:90]!r}")
for b in bad: print(f"FAIL: S4 {b}")
sys.exit(1 if bad else 0)
PY
  [ $? -eq 0 ] || { FAILED=1; RED_KINDS="$RED_KINDS 坏态报错"; }
fi
if [ "$FAILED" -eq 0 ]; then ok "S4 明确报错 + 秒回 + 声明自愈"; fi

echo "=== 5) 自愈：非零退出 → 监督循环拉起 → 同一份扫描件又能抽（无需人工）==="
EXITED=0
for _ in $(seq 1 20); do
  grep -q 'EXIT rc=' "$LOG" && { EXITED=1; break; }
  sleep 1
done
if [ "$EXITED" -eq 1 ]; then
  RC="$(grep -o 'EXIT rc=[0-9]*' "$LOG" | head -1 | cut -d= -f2)"
  if [ "$RC" != "0" ]; then
    ok "S5 进程以 rc=$RC 非零退出（systemd Restart=on-failure 会拉起；若是 0 则永不重启=永久坏）"
  else
    fail "S5 进程 rc=0 退出 → Restart=on-failure 不动作，服务永久坏到人工介入"
    RED_KINDS="$RED_KINDS 退出码"
  fi
else
  fail "S5 删掉解包目录后进程 20s 内没有退出（守卫没触发自愈）"
  RED_KINDS="$RED_KINDS 无自愈退出"
fi

D2=""
if wait_health 180 "重启后"; then
  D2="$(jget "$WORK/health.json" runtime_dir)"
  ok "S5 重启后 health 就绪 runtime_dir=$D2"
  if [ -n "$D1" ] && [ "$D2" != "$D1" ]; then
    ok "S5 解包目录换成新的 → 确实重启过（不是同一个进程假装恢复）"
  else
    fail "S5 重启后 runtime_dir 与故障前相同（$D1）→ 可疑：并未真正重启"
    RED_KINDS="$RED_KINDS 未重启"
  fi
  curl -s -m 600 -F "file=@$WORK/scan3.pdf" "http://127.0.0.1:$PORT/extract" -o "$WORK/recovered.json"
  python3 - "$WORK/recovered.json" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
bad=[]
if not d.get("ok"): bad.append(f"ok=false err={d.get('error')!r}")
st=d.get("stats") or {}
if st.get("ocr_pages",0) < 3: bad.append(f"ocr_pages={st.get('ocr_pages')} < 3")
if "RTLOSS扫描段关键句1" not in (d.get("text") or ""): bad.append("恢复后关键词仍没抽到")
for b in bad: print(f"FAIL: S5 {b}")
if not bad: print(f"  ok: S5 故障后同一份扫描件恢复正常抽取 stats={st} chars={d.get('chars')}")
sys.exit(1 if bad else 0)
PY
  [ $? -eq 0 ] || { FAILED=1; RED_KINDS="$RED_KINDS 恢复后抽取"; }
else
  fail "S5 重启后 /health 未就绪（看日志：$LOG）"
  RED_KINDS="$RED_KINDS 重启后不可用"
fi

if [ "$BREAK" -eq 1 ]; then
  echo "=== 自证模式：期望断言变红 ==="
  if [ "$FAILED" -eq 0 ]; then
    echo "FAIL: --break（OCRD_RUNTIME_GUARD=off）下断言竟然全绿 → 这道防线根本没在防，是假绿"
    exit 1
  fi
  case "$RED_KINDS" in
    *健康可见性*) ok "自证通过：去掉守卫后，最先红在「/health 不敢报坏」→ 正是本防线要抓的点（红因：$RED_KINDS）" ;;
    *前提*)      echo "FAIL: 只在前提断言上红（故障前抽取就不成立）→ 这次红不算自证，是环境问题"; exit 1 ;;
    *)           echo "FAIL: 红了但红因不是「健康可见性」：$RED_KINDS → 不能证明这条防线能抓本 bug"; exit 1 ;;
  esac
  exit 0
fi

if [ "$FAILED" -ne 0 ]; then
  echo "=== 防线未通过（红因：$RED_KINDS）==="
  exit 1
fi
echo "=== 防线通过：解包目录被清时 /health 如实报坏 → 请求说人话秒回 → 非零退出 → 重启后同一份扫描件恢复正常 ==="
