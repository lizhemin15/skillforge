#!/usr/bin/env bash
# 「模型热切换」的断言自证脚本（2026-09-26）。
#
# 为什么必须有：internal/agent/llm_hotswap_test.go 与 internal/api/llm_hotswap_wiring_test.go
# 全绿只证明「今天换了模型真能生效」，证明不了「哪天有人把指纹换血摘了 / 把外部注入的守卫
# 删了 / 把管理端的推送摘了，会被抓住」。而这条链路的故障形态用户也是**看得见**的：
# 线上实测把模型从 A 切到 B、界面显示「在用：B」，每一轮问答仍在打 A 的地址，
# A 已欠费 → 用户持续收到 402，只能得出「这系统不支持热切换」。
#
# 三条注入分别打在三种坏法上：
#   1. 管理端接线被摘（点「切换」只改库，运行期不动）—— 用户按下按钮那一下。
#   2. 引擎的指纹换血被摘（退回「nil 才建」，库里改了永不生效）—— 直连改库/启动时读一次。
#   3. 外部注入守卫被删（调用方指定的客户端被库配置悄悄顶掉）—— 测试替身与一次性实例。
#
# 用法：bash internal/api/llm_hotswap_mutation_check.sh
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
for c in "$GO_BIN_ENV" "$(command -v go || true)" /usr/local/go1.25/bin/go /usr/local/go/bin/go /usr/bin/go /opt/go/bin/go; do
  if go_ok "$c"; then GO_BIN="$c"; break; fi
done
if [ -z "$GO_BIN" ]; then
  echo "环境红：挑不到 go >= $(sed -n 's/^go \(.*\)/\1/p' go.mod | head -1)（不是断言红，先修环境：export GO_BIN=... 或把新版 go 放进 PATH）" >&2
  exit 2
fi

ADMIN=internal/api/admin.go
AGENT=internal/agent/agent.go
BAK_DIR="$(mktemp -d)"
cp "$ADMIN" "$BAK_DIR/admin.go"
cp "$AGENT" "$BAK_DIR/agent.go"

# restore 只还原、**不删备份**（备份删了第二次还原就是空操作，注入态会一路带到下一条），
# 也**不用 git checkout** —— 出货文件上可能压着未提交的人工改动，git 还原会把它一起抹掉。
restore() {
  cp "$BAK_DIR/admin.go" "$ADMIN"
  cp "$BAK_DIR/agent.go" "$AGENT"
}
cleanup() { restore; rm -rf "$BAK_DIR"; }
trap cleanup EXIT

fails=0
CASE_RE='TestHotSwap|TestInjectedClient|TestReloadLLM|TestEnsureLLM|TestSetLLMKeeps|TestSwitchLLM|TestEditActiveLLM|TestDeleteActiveLLM|TestListLLMReports'
run_test() {
  "$GO_BIN" test ./internal/agent/ ./internal/api/ -run "$CASE_RE" -count=1 2>&1
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

# 注入 1 = 线上那一下的原样：管理端「切换」只改库、不推运行期。
# 这是唯一一条打在 **接线** 上的注入：其余两条打判据，只证明判据有牙；
# 这条证明「判据真的被调用」，缺了它，判据写得再对也可能根本没接上。
# 锚点必须唯一：`a.applyActiveLLM()` 在 upsert/切换/删除三处都有，
# 只按这一行定位会命中 2~3 次（脚本会以「锚点命中 N 次」拒绝注入）。
# 所以带上紧邻的、只此一份的注释行。
inject_case '管理端「切换」不再推给运行期（只改库 → 用户看到换了、实际没换）' \
  "$ADMIN" \
  '	// 「切换」按钮的语义就是立刻生效。这里不刷，用户切完发现还是旧模型在答，
	// 只会得到一个「热切换是坏的」结论。
	a.applyActiveLLM()' \
  '	// 「切换」按钮的语义就是立刻生效。这里不刷，用户切完发现还是旧模型在答，
	// 只会得到一个「热切换是坏的」结论。
	// （注入：接线被摘，只改库）' \
  '点启用后运行期应立刻是'

# 注入 2 = 引擎的指纹换血被摘：退回旧实现的「nil 才建」。
# 覆盖的是「没人点按钮」的那条路：直接改库、或启动时读一次之后库又变了。
inject_case '引擎指纹换血被摘（退回 nil 才建 → 库里改了永不生效）' \
  "$AGENT" \
  '	if e.llm != nil && fp == e.llmFP {' \
  '	if e.llm != nil {' \
  '就地改配置后引擎仍握着旧模型'

# 注入 3 = 外部注入守卫被删：拿库里的配置顶掉调用方指定好的客户端。
# 这条抓的是「修好热切换的同时把替身机制弄坏」——症状是测试开始互相干扰、
# 一次性实例被换成另一个网关，而生产上看不出任何异常。
inject_case '外部注入的客户端被库配置顶掉（替身/指定实例被悄悄换掉）' \
  "$AGENT" \
  '	if e.llm != nil && e.llmFP == "" {' \
  '	if false && e.llm != nil && e.llmFP == "" {' \
  '注入的客户端被换掉了'

echo
if [ "$fails" -eq 0 ]; then
  echo "自证通过：3/3 条注入都被预期断言抓住，且还原后回绿。"
else
  echo "自证失败：$fails 条注入没被抓住（这些断言现在没有判别力）。"
fi

# 还原必须逐字节回到原样：否则脚本本身会把注入态留在出货文件里（比不跑更糟）。
before="$(md5sum "$BAK_DIR/admin.go" "$BAK_DIR/agent.go" | awk '{print $1}' | tr '\n' ' ')"
after="$(md5sum "$ADMIN" "$AGENT" | awk '{print $1}' | tr '\n' ' ')"
if [ "$before" != "$after" ]; then
  echo "✗ 还原后 md5 不一致：$before vs $after —— 出货文件被留在改动状态！"
  exit 1
fi
echo "还原校验：逐字节一致 ✓"
[ "$fails" -eq 0 ] || exit 1
