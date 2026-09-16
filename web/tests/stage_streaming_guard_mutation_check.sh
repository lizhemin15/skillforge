#!/usr/bin/env bash
# 双向自证：对「漏接流式」守卫做变异注入 —— 注入必须变红（且是预期那条），还原必须回绿。
#
# 三处变异覆盖三种失效形态：
#   M1 结构面·形态二：把 g.streamingChat() 换回裸 g.llm（线上 353.3 秒零帧的原始写法）
#   M2 结构面·形态一：把裸调 g.llm.Chat( 塞回训练链路
#   M3 行为面：streamingChat 偷偷返回裸客户端（结构面看着合规，实际材料丢了）
#   M4 守卫自身退化：匹配串改回 g.llm.Chat( → 下限断言必须 Fatalf（防空跑绿）
set -uo pipefail
# 仓库根从脚本自身位置推出来：本地是 /root/skillforge，CI 里是 $GITHUB_WORKSPACE。
# 以前这里写死 /root/skillforge，于是这把尺子只在本机跑得动、CI 里一接就废——
# 这也是它此前一直没人接线（preflight_parity 守卫报「ci.yml 里没有调用它」）的成因。
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# 工具链：本机 /usr/bin/go 是发行版自带的 go1.18，读不了 go.mod 里的 `go 1.25.0`，
# 报错长得像仓库坏了（`invalid go version '1.25.0': must match format 1.23`），实际
# 只是 PATH 里挑到了老 go。走 preflight 时它已经替我们修好 PATH，但**直接 `bash 本脚本`**
# （后台/干净 env 就是这样）会假红。这里自愈一次，省得下次又有人去查 go.mod。
# CI 里 /usr/local/go/bin/go 通常不存在（go 由 setup-go 提供），所以这条是 no-op。
if [[ -x /usr/local/go/bin/go && "$(command -v go)" != "/usr/local/go/bin/go" ]]; then
  PATH="/usr/local/go/bin:$PATH"; export PATH
fi
cd "$ROOT/internal/skillgen" || exit 2

# 前置条件：工作区必须干净。
# 为什么必须挡：restore 用 git checkout 还原，它会把**未提交的改动一起抹掉**——
# 实测踩过一次：改好 gofmt 还没提交就跑本脚本，还原把格式修复覆盖回旧版，
# 接着被 commit 进去，CI 的 gofmt 门禁红。不干净就直接不跑，别让尺子吃掉人的劳动。
#
# ⚠️ 顺序陷阱（2026-09-17 又踩一次，比上面那次更狠）：trap 必须挂在**这道检查之后**。
# 原来 trap 挂在前面，于是「拒绝运行」这条路径也会触发 EXIT trap → restore 照样
# git checkout → 它刚刚声明要保护的那份未提交改动，被它自己吃掉（实测连 wipe 掉
# generator.go/manual.go 里 7 处 withoutThinking，一次跑飞白干一轮）。
# 现在没过这道门时 trap 还没挂，exit 2 直接走人，工作区原样。
if [[ -n "$(git -C "$ROOT" status --porcelain internal/skillgen)" ]]; then
  echo "拒绝运行：internal/skillgen 有未提交改动，restore 会把它抹掉。先提交或 stash。"
  git -C "$ROOT" status --short internal/skillgen
  exit 2
fi

restore() {
  git -C "$ROOT" checkout -- internal/skillgen/manual.go internal/skillgen/generator.go internal/skillgen/stage_streaming_test.go 2>/dev/null
}
trap restore EXIT

run() { go test . -run "$1" -count=1 2>&1; }

RC=0
expect_red_of() { # $1=用例 $2=预期的 FAIL 特征串 $3=变异说明
  out=$(run "$1")
  if [[ "$out" == *"$2"* ]]; then
    echo "OK   注入→红：$3（命中：$2）"
  else
    echo "BAD  注入未变红或红错了地方：$3（期望含「$2」）"
    printf '%s\n' "$out" | tail -20
    RC=1
  fi
}

# ---- M1：自由函数调用点退回裸客户端（原发地）----
restore
sed -i 's/ExtractStructure(ctx, g\.streamingChat(), src)/ExtractStructure(ctx, g.llm, src)/' manual.go
grep -q 'ExtractStructure(ctx, g.llm, src)' manual.go || { echo "BAD M1 注入点没打上"; RC=1; }
expect_red_of TestNoSilentModelCallOutsideChatWithMaterial 'manual.go:' "M1 裸客户端递给自由函数"

# ---- M2：训练链路里直接阻塞调用（往真实阶段函数里塞一行）----
restore
python3 - <<'PY'
import re
p = "manual.go"
s = open(p, encoding="utf-8").read()
# 在 buildManual 函数体开头插入一次裸阻塞调用（形态一）
i = s.index("func (g *Generator) buildManual(")
j = s.index("\n", i) + 1
s = s[:j] + '\t_, _ = g.llm.Chat(ctx, "sys", "user")\n' + s[j:]
open(p, "w", encoding="utf-8").write(s)
PY
grep -q 'g.llm.Chat(ctx, "sys", "user")' manual.go || { echo "BAD M2 注入点没打上"; RC=1; }
expect_red_of TestNoSilentModelCallOutsideChatWithMaterial 'manual.go:' "M2 链路里直接 g.llm.Chat("

# ---- M2b：分类器被削弱（两种漏接形态只认一种）----
restore
sed -i 's|func(l string) bool { return strings.Contains(l, "g.llm == nil") }, // 未配置模型的守卫|func(l string) bool { return false }, // 未配置模型的守卫|' stage_streaming_test.go
out=$(run TestGllmUseClassifierPinsBothLeakShapes)
if [[ "$out" == *"分类器判错"* ]]; then
  echo "OK   注入→红：M2b 分类器削弱被钉住（分类器判错）"
else
  echo "BAD  M2b 分类器削弱后仍全绿 —— 守卫的守卫空跑"
  printf '%s\n' "$out" | tail -20
  RC=1
fi

# ---- M3：包装器偷换回裸客户端（结构面合规、行为面漏材料的绕道）----
restore
sed -i 's|func (g \*Generator) streamingChat() chatClient { return chatFunc(g.chatWithMaterial) }|func (g *Generator) streamingChat() chatClient { return g.llm }|' generator.go
grep -q 'return g.llm }' generator.go || { echo "BAD M3 注入点没打上"; RC=1; }
out=$(run TestBuildManualStreamsMaterial)
if [[ "$out" == *"必须走流式"* ]]; then
  echo "OK   注入→红：M3 streamingChat 偷换裸客户端（行为面抓住：必须走流式）"
else
  echo "BAD  M3 未变红：行为面没抓住偷换"
  printf '%s\n' "$out" | tail -20
  RC=1
fi

# ---- M4：守卫自身退化（把启发式改回只看 g.llm.Chat(）----
restore
sed -i 's/if !strings.Contains(line, "g.llm") {/if !strings.Contains(line, "g.llm.Chat(") {/' stage_streaming_test.go
out=$(run TestNoSilentModelCallOutsideChatWithMaterial)
if [[ "$out" == *"守卫自身失效"* ]]; then
  echo "OK   注入→红：M4 启发式退化被下限断言抓住（防空跑绿）"
else
  echo "BAD  M4 守卫退化后仍然全绿 —— 这把尺子会空跑"
  printf '%s\n' "$out" | tail -20
  RC=1
fi

# ---- 还原 → 必须回绿 ----
restore
if go test . -run 'TestNoSilentModelCallOutsideChatWithMaterial|TestBuildManualStreamsMaterial|TestGllmUseClassifierPinsBothLeakShapes' -count=1 >/tmp/guard_restored.log 2>&1; then
  echo "OK   还原→绿"
else
  echo "BAD  还原后没回绿"
  tail -20 /tmp/guard_restored.log
  RC=1
fi

# ---- 收尾：本脚本用 sed 改过 Go 源码，格式化必须仍然是干净的 ----
# 为什么放在这个脚本里：本地只跑 go test 时看不出 gofmt 漂移，CI 的 vet/gofmt 门禁
# 会红（实测 b18dfc4 就这么被拦下来一次）。尺子长在会动源码的地方才拦得住。
if [[ -z "$(gofmt -l .)" ]]; then
  echo "OK   gofmt 干净（本地不再漏过 CI 的格式化门禁）"
else
  echo "BAD  以下文件未格式化，CI 会红："
  gofmt -l .
  RC=1
fi

echo "----"
if [[ $RC -eq 0 ]]; then echo "双向自证通过：5 处注入全红、还原全绿、gofmt 干净"; else echo "双向自证失败（RC=1）"; fi
exit $RC
