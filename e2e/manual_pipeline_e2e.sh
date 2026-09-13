#!/bin/bash
# 手册模式流水线 E2E：真素材 → 上传 → 断言产物（分类数 / 范文数 / 保真）
#
# 【为什么有这个脚本】技能工厂曾栽过一次「静默降级」：手册 12 个分类抽全了，
# 但 7 个分类的范文一篇没切出来，技能照样训练成功并注册上线。所以这条流水线
# 必须有能独立复跑的端到端验证。
#
# 【本脚本的核心铁律：自证「我连的是我自己起的服务」】
# 上一版脚本（/tmp/run_e2e_green.sh）栽在一个极隐蔽的地方：
#   8099 端口被一个残留的验证实例占着 → 脚本起的服务 `bind: address already
#   in use` 直接失败 → 但脚本没检查，继续发上传请求 → 请求打到了那个残留实例
#   上 → 结果写进残留实例的数据目录，而脚本去读自己建的干净目录 → 读到两个内置
#   技能、0 分类 0 范文 → 报「假红」。
# 反方向同样致命：若残留实例里躺着旧的成功产物，同一条路径就是「假绿」。
# 所以：端口随机（不跟任何人抢）+ 就绪必须用日志行断言 data 路径 == 本次目录
# （服务自己打印的那行 `listening on <addr> (data: <dir>)` 是现成的归属证据）。
#
# 【退出码语义】0=通过  1=失败  2=跳过（缺素材，显式标注，绝不假装通过）
set -uo pipefail

REPO=/root/skillforge
BIN=${BIN:-/opt/skillforge/skillforge}
PDF=$REPO/testdata/manual/manual-vector.pdf
DATADIR=${DATADIR:-/tmp/sf-e2e-$(date +%H%M%S)}
LOG=$DATADIR/sse.log
UPLOADER=$REPO/e2e/upload_manual.py
SKILLNAME=${SKILLNAME:-手册测试e2e}

# --- 期望值（来自真实手册 fixture：第一~第十三章写作类 + 第十四章自查清单）---
WANT_CATEGORIES=12
WANT_EXAMPLES=24          # 12 类 × 2 篇
WANT_FIDELITY_RATIO=1.0   # 24/24 必须是原文连续子串

echo "########## 手册流水线 E2E ##########"
echo "被测二进制：$BIN"
[ -x "$BIN" ] || { echo "✗ 二进制不存在或不可执行：$BIN"; exit 1; }
stat -c '  编译时间 %y  %s bytes' "$BIN"

# 真实素材 54MB，不入版本库；换机器/清素材时会缺 —— 跳过但显式说明
if [ ! -f "$PDF" ]; then
  echo "SKIP: 缺真实素材 $PDF"
  echo "  真实手册 PDF 体积过大不进版本库；按断言铁律，跳过必须显式标注，"
  echo "  绝不允许在没有素材时输出「通过」。"
  exit 2
fi
if [ ! -f "$UPLOADER" ]; then
  echo "✗ 缺上传脚本：$UPLOADER"; exit 1
fi

# --- 随机空闲端口：彻底避免和残留实例抢端口 ---
PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
ADDR="127.0.0.1:${PORT}"
echo "本次端口：$ADDR（随机，避免残留实例占位）"
echo "本次数据目录：$DATADIR"
echo

rm -rf "$DATADIR"; mkdir -p "$DATADIR"

# 环境：先继承线上 env（拿模型密钥），再覆盖成本次的目录/端口
set -a; [ -f /opt/skillforge/skillforge.env ] && source /opt/skillforge/skillforge.env; set +a
export SKILLFORGE_DATA_DIR="$DATADIR"
export SKILLFORGE_DB="$DATADIR/skillforge.db"
export SKILLFORGE_ADDR="$ADDR"
# 上传脚本的地址必须由这里注入：它自己写死端口会打到残留实例上（本次事故根因）。
# 归属断言只覆盖了「服务是我起的」，若不注入地址，「请求发到哪」就仍然是断的。
export SKILLFORGE_BASE_URL="http://$ADDR"

"$BIN" > "$DATADIR/server.log" 2>&1 &
SRV=$!
echo "本次实例 pid=$SRV"

cleanup() { kill "$SRV" 2>/dev/null; wait "$SRV" 2>/dev/null; }
trap cleanup EXIT

# --- 归属断言：日志里必须同时出现「本次端口」和「本次数据目录」---
ready=0
for i in $(seq 1 40); do
  if grep -qF "listening on ${ADDR} (data: ${DATADIR})" "$DATADIR/server.log" 2>/dev/null; then
    echo "就绪 ✓ 已确认本实例 data=${DATADIR}"
    ready=1; break
  fi
  if ! kill -0 "$SRV" 2>/dev/null; then
    echo "✗ 服务进程已退出（pid=$SRV）——不接受「打到别人实例上」的结果"
    echo "--- server.log ---"; tail -20 "$DATADIR/server.log"
    exit 1
  fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "✗ 40s 内未确认服务归属（端口 $ADDR / 目录 $DATADIR）"
  echo "--- server.log ---"; tail -20 "$DATADIR/server.log"
  exit 1
fi

echo
echo "=== 开始上传 $(date +%H:%M:%S) ==="
python3 "$UPLOADER" "$SKILLNAME" "$PDF" "$LOG"
up=$?
echo "=== 上传结束 $(date +%H:%M:%S)（退出码 $up）==="

FAIL=0
report() { if [ "$1" = 1 ]; then echo "  ✓ $2"; else echo "  ✗ $2"; FAIL=1; fi; }

echo
echo "########## 结果断言 ##########"

# 0) 上传本身没报错
if [ "$up" = 0 ] && ! grep -q '"error"' "$LOG" 2>/dev/null; then
  report 1 "上传流程无 error 帧"
else
  report 0 "上传流程出现错误（退出码 $up）"
  grep -o '"error"[^}]*' "$LOG" 2>/dev/null | head -5
fi

# 1) 服务端日志无错误
if grep -qiE "panic|bind: address already in use" "$DATADIR/server.log" 2>/dev/null; then
  report 0 "服务端日志有 panic / 端口冲突"
else
  report 1 "服务端日志无 panic / 端口冲突"
fi

SKILLDIR="$DATADIR/skills/$SKILLNAME"

# 2) 分类数
ncat=$(ls "$SKILLDIR"/categories/*.md 2>/dev/null | grep -v '_index' | wc -l)
if [ "$ncat" = "$WANT_CATEGORIES" ]; then
  report 1 "分类数 $ncat == $WANT_CATEGORIES"
else
  report 0 "分类数 $ncat != $WANT_CATEGORIES（手册结构没抽全）"
fi

# 3) 范文数
nex=$(find "$SKILLDIR/examples" -name '*.md' 2>/dev/null | wc -l)
if [ "$nex" = "$WANT_EXAMPLES" ]; then
  report 1 "范文数 $nex == $WANT_EXAMPLES"
else
  report 0 "范文数 $nex != $WANT_EXAMPLES（RED 基线的静默降级就是这个症状）"
fi

# 4) 保真：每篇范文必须是 OCR 原文的连续子串 + 覆盖每个分类
python3 - "$SKILLDIR" "$WANT_CATEGORIES" <<'PY'
import glob, os, sys
from collections import Counter

skilldir, want_cat = sys.argv[1], int(sys.argv[2])
txts = glob.glob(f"{skilldir}/source/*.txt")
if not txts:
    print("  ✗ 找不到 source/*.txt，无法核对保真"); sys.exit(1)
src = None
for t in txts:                        # 挑文本（source/ 下同时有 .pdf）
    try:
        src = open(t, encoding="utf-8").read(); break
    except UnicodeDecodeError:
        continue
if not src:
    print("  ✗ source/ 下没有可读文本"); sys.exit(1)

paths = sorted(glob.glob(f"{skilldir}/examples/*/*.md"))
bad = [os.path.relpath(p, skilldir) for p in paths
       if open(p, encoding="utf-8").read().strip() not in src]
per = Counter(os.path.basename(os.path.dirname(p)) for p in paths)

ok_fid   = 1 if (paths and not bad) else 0
ok_cover = 1 if len(per) == want_cat else 0
print(f"  {'✓' if ok_fid else '✗'} 保真：{len(paths)-len(bad)}/{len(paths)} 篇是原文连续子串")
if bad: print("      未命中：", bad[:5])
print(f"  {'✓' if ok_cover else '✗'} 覆盖：{len(per)} 类（期望 {want_cat}）")
sys.exit(0 if (ok_fid and ok_cover) else 1)
PY
[ $? -eq 0 ] || FAIL=1

# 5) 质量报告（fidelity.md）必须落盘，否则降级不可核对
if [ -f "$SKILLDIR/fidelity.md" ]; then
  report 1 "质量报告 fidelity.md 已落盘（降级可核对）"
else
  report 0 "缺 fidelity.md —— 切分失败会退化成不可核对的静默降级"
fi

echo
if [ "$FAIL" = 0 ]; then
  echo "########## E2E 通过 ✓ ##########"
  echo "产物：$SKILLDIR"
  exit 0
else
  echo "########## E2E 失败 ✗ ##########"
  echo "产物：$SKILLDIR"
  echo "服务日志：$DATADIR/server.log"
  exit 1
fi
