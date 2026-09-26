#!/usr/bin/env bash
# 「零材料强制门」的断言自证脚本（2026-09-26）。
#
# 为什么必须有：internal/api/chat_needs_gate_wiring_test.go 里那几条用例全绿，只证明
# 「今天拦得住」，证明不了「哪天有人把停问摘了 / 把出口删了 / 把门槛调松了，会被抓住」。
# 而这条链路的故障形态用户是**看得见**的：线上实测「帮我写一份会议纪要」这样一句话指令 +
# 零材料，分类器一条 needs 都不报 → 闸门形同不存在 → 写作跳直通，产出 645 字带假日期、
# 假参会人的会议纪要。用户读到的是「它替我编了」。
#
# 做法：往**出货文件**注入真故障，要求**预期那条**断言变红，还原后回绿。
# 三条注入分别打在三种坏法上：接线被摘、出口被删（问成死循环）、门槛被调松（打回原投诉）。
#
# 用法：bash internal/api/zero_material_gate_mutation_check.sh
set -uo pipefail
cd "$(dirname "$0")/../.."

# go 走候选解析，不写死本机工具链：写死 /usr/local/go/bin/go 在开发机对（1.25），
# 在 CI 上（go 由 setup-go 提供）会变成一堆莫名其妙的编译错 —— 环境红冒充断言红。
# 但候选**必须按 go.mod 的版本要求筛**：本机 /usr/bin/go 是发行版自带的 1.18，
# 拿它跑 `go test` 只会报 "invalid go version '1.25.0'"（同样是与判据无关的环境红）。
NEED_GO="$(sed -n 's/^go \([0-9]*\)\.\([0-9]*\).*/\1\2/p' go.mod | head -1)"
go_ok() {
  local bin="$1" v
  [ -n "$bin" ] || return 1
  if [ ! -x "$bin" ] && ! command -v "$bin" >/dev/null 2>&1; then return 1; fi
  v="$("$bin" env GOVERSION 2>/dev/null | sed -n 's/^go\([0-9]*\)\.\([0-9]*\).*/\1\2/p')"
  [ -n "$v" ] && [ "$v" -ge "${NEED_GO:-0}" ]
}
GO_BIN_ENV="${GO_BIN:-}"
GO_BIN=""
for c in "$GO_BIN_ENV" "$(command -v go || true)" /usr/local/go/bin/go /usr/bin/go /opt/go/bin/go; do
  if go_ok "$c"; then GO_BIN="$c"; break; fi
done
if [ -z "$GO_BIN" ]; then
  echo "环境红：挑不到 go >= $(sed -n 's/^go \(.*\)/\1/p' go.mod | head -1)（不是断言红，先修环境：export GO_BIN=... 或把新版 go 放进 PATH）" >&2
  exit 2
fi

CHAT=internal/api/chat.go
GATE=internal/agent/needs_gate.go
BAK_DIR="$(mktemp -d)"
cp "$CHAT" "$BAK_DIR/chat.go"
cp "$GATE" "$BAK_DIR/needs_gate.go"

# restore 只还原、**不删备份**（备份删了第二次还原就是空操作，注入态会一路带到下一条），
# 也**不用 git checkout** —— 出货文件上可能压着未提交的人工改动，git 还原会把它一起抹掉。
restore() {
  cp "$BAK_DIR/chat.go" "$CHAT"
  cp "$BAK_DIR/needs_gate.go" "$GATE"
}
cleanup() { restore; rm -rf "$BAK_DIR"; }
trap cleanup EXIT

fails=0
run_test() {
  "$GO_BIN" test ./internal/api/ -run 'TestNeedsGate' -count=1 2>&1
}

# ---------- 基线：不注入时必须全绿 ----------
if ! out="$(run_test)"; then
  echo "基线就是红的，先修好再来做注入自证："
  echo "$out" | tail -20
  exit 1
fi
echo "基线：全绿 ✓"

# 注入：$1=故障说明 $2=被改的文件 $3=锚点 $4=替换文本 $5=应变红的断言（文案片段）
inject_case() {
  local desc="$1" file="$2" old="$3" new="$4" expect="$5"
  python3 - "$file" "$old" "$new" <<'PY'
import sys
path, old, new = sys.argv[1], sys.argv[2], sys.argv[3]
s = open(path, encoding='utf-8').read()
n = s.count(old)
if n != 1:
    sys.exit(f"注入失败：锚点在 {path} 里命中 {n} 次（应为 1 次）—— 出货文件改了，"
             f"请同步更新本脚本的锚点，别让自证脚本变成永远绿的摆设")
open(path, 'w', encoding='utf-8').write(s.replace(old, new))
PY
  if [ $? -ne 0 ]; then echo "✗ [$desc] 注入失败"; fails=$((fails + 1)); return; fi

  local out rc
  out="$(run_test)"; rc=$?
  if [ $rc -eq 0 ]; then
    echo "✗ [$desc] 注入后测试仍然全绿 —— 断言是假的（抓不住这个故障）"
    fails=$((fails + 1))
  elif grep -q 'build failed\|\[build failed\]\|cannot use\|undefined:' <<<"$out"; then
    # 「红在编译上不算红」：编译不过说明注入本身是坏的，不能算断言有效。
    echo "✗ [$desc] 注入把代码改到编译不过 —— 这次红不算数"
    echo "$out" | grep -m3 '\.go:' | sed 's/^/      /'
    fails=$((fails + 1))
  elif ! grep -qF -- "$expect" <<<"$out"; then
    echo "✗ [$desc] 测试红了，但红的不是预期那条（期望含「$expect」）"
    echo "$out" | grep '^--- FAIL\|^    --- FAIL' | sed 's/^/      /'
    fails=$((fails + 1))
  else
    echo "✓ [$desc] → 「$expect」变红"
  fi
  restore
}

echo
echo "注入自证（每条都必须变红）"

# 注入 1 = 线上那 645 字假纪要的原样：接线被摘（停问那一跳永远不触发）。
# 这是唯一一条打在 **chat.go 接线** 上的注入：其余两条打判据，只证明判据有牙；
# 这条证明「判据真的被调用」，缺了它，判据写得再对也可能根本没接上。
inject_case '零材料强制门的接线被摘（停问永远不触发 → 照旧编事实）' \
  "$CHAT" \
  '); len(zm) > 0 {' \
  '); false && len(zm) > 0 {' \
  '却没拦'

# 注入 2 = 出口被删：停问文案让用户回「就按你的」，而那一轮消息只有 4 个字，
# 材料依然真空、必填项依然没落实 —— 不认授权词就是**问成死循环**，用户永远走不到写作。
# 这一格是产品级的坏法（可用性死锁），不是文案问题，必须单独钉。
inject_case '「就按你的」授权词不被认（停问出口闭不上 → 问成死循环）' \
  "$GATE" \
  'if GrantedFreeRein(msg) {' \
  'if false {' \
  '死循环'

# 注入 3 = 门槛被调松：把真空线从 80 字提到 800 字，灰区（有素材但不到 800 字）也被
# 本地停问吃掉，不再走疑点回执那条锚定原文的路 —— 就是把 2026-09-22 那次投诉
# 「像没看到我给的信息，还在问我要信息」原样放回来。
inject_case '真空线被调松（灰区素材也被拦 → 打回「像没看到我给的信息」）' \
  "$GATE" \
  'const noMaterialChars = 80' \
  'const noMaterialChars = 800' \
  '疑点跳一次都没调'

echo
if [ "$fails" -eq 0 ]; then
  echo "自证通过：3/3 条注入都被预期断言抓住，且还原后回绿。"
else
  echo "自证失败：$fails 条注入没被抓住（这些断言现在没有判别力）。"
fi

# 还原必须逐字节回到原样：否则脚本本身会把注入态留在出货文件里（比不跑更糟）。
before="$(md5sum "$BAK_DIR/chat.go" "$BAK_DIR/needs_gate.go" | awk '{print $1}' | tr '\n' ' ')"
after="$(md5sum "$CHAT" "$GATE" | awk '{print $1}' | tr '\n' ' ')"
if [ "$before" != "$after" ]; then
  echo "✗ 还原后 md5 不一致：$before vs $after —— 出货文件被留在改动状态！"
  exit 1
fi
echo "还原校验：逐字节一致 ✓"
[ "$fails" -eq 0 ] || exit 1
