#!/usr/bin/env bash
# 推荐行（/api/chat/suggest）的「断言自证」脚本。
#
# 为什么必须有这个脚本：internal/api/suggest_test.go 全绿只能证明「现在没坏」，
# 证明不了「坏了会被抓住」。这份测试里最容易写成假断言的地方有两类：
#   - 拿实现自己的常量当期望（例如「应封顶 suggestMaxChips 颗」——常量改大，
#     测试跟着改大，永远绿）；
#   - 只断言「返回 200」，不验内容（含 JSON 解析失败、prompt 丢上下文都照样绿）。
# 做法：往**出货文件**里逐个注入真实故障，要求对应的那条断言变红；还原后必须重新变绿。
# 哪条注入还是绿的，就说明那条断言是假的，脚本直接退出 1。
#
# 用法：bash internal/api/suggest_mutation_check.sh
# 注意：`-run Suggest` 会同时跑到它们依赖的 fakeLLM；注入改的是实现文件，不动测试。
set -uo pipefail
cd "$(dirname "$0")/../.."
export PATH=/usr/local/go/bin:$PATH

SUGGEST=internal/api/suggest.go
AGENT=internal/agent/agent.go
ROUTER=internal/api/router.go
BAK_DIR="$(mktemp -d)"
for f in "$SUGGEST" "$AGENT" "$ROUTER"; do
  cp "$f" "$BAK_DIR/$(basename "$f")"
done
# 注意：restore 只负责还原，**不能**在这里删备份 —— 每条注入后都要还原一次，
# 备份删了第二次还原就成了空操作，注入状态会一路带到下一条（自证结果全乱）。
restore() {
  for f in "$SUGGEST" "$AGENT" "$ROUTER"; do
    cp "$BAK_DIR/$(basename "$f")" "$f"
  done
}
trap 'restore; rm -rf "$BAK_DIR"' EXIT

fails=0
run_test() { go test ./internal/api/ -run 'Suggest' -count=1 2>&1; }

# ---------- 基线：不注入时必须全绿 ----------
if ! out="$(run_test)"; then
  echo "基线就是红的，先修好再来做注入自证："
  echo "$out" | tail -20
  exit 1
fi
echo "基线：全绿 ✓"

# 注入：$1=故障说明 $2=目标文件 $3=锚点 $4=替换文本 $5=应变红的断言
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
  elif ! grep -qF "$expect" <<<"$out"; then
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

inject_case '推荐行上限偷偷放宽（第 5 颗换行，「一行」的极简就破了）' \
  "$SUGGEST" "suggestMaxChips = 4" "suggestMaxChips = 5" \
  'FAIL: TestSuggestParse_DedupeAndCap'

inject_case '去重被删（模型数不清时会给出两颗一样的胶囊）' \
  "$SUGGEST" 'if label == "" || seen[label] {' 'if label == "" {' \
  'FAIL: TestSuggestParse_DedupeAndCap'

inject_case '占位符过滤被删（上屏一颗写着 undefined 的胶囊）' \
  "$SUGGEST" 'case "", "undefined", "null", "nil", "n/a", "none", "无":' 'case "":' \
  'FAIL: TestSuggestParse_DropsPlaceholders'

inject_case 'label 截断被删（长句子把推荐行挤成两行）' \
  "$SUGGEST" 'return clampRunes(t, suggestLabelRunes)' 'return t' \
  'FAIL: TestSuggestParse_LabelTruncatesByRune'

inject_case 'prompt 丢掉「用户刚说」（模型只能凭空猜下一步）' \
  "$SUGGEST" 'b.WriteString("- 用户刚说：" + clampRunes(strings.TrimSpace(req.LastUser), 200) + "\n")' \
  'b.WriteString("")' \
  'FAIL: TestSuggestPrompt_CarriesContext'

inject_case 'prompt 丢掉技能清单（建议脱离这个站真实能力）' \
  "$SUGGEST" 'if len(req.Skills) > 0 {' 'if false {' \
  'FAIL: TestSuggestPrompt_CarriesContext'

inject_case 'prompt 丢掉「这是第一轮」标注（第一轮被当成改稿场景）' \
  "$SUGGEST" 'b.WriteString("- 助手还没回复（这是对话的第一轮）\n")' 'b.WriteString("")' \
  'FAIL: TestSuggestPrompt_FirstTurn'

inject_case '长回复不再截断（一次推荐行调用被整篇交付说明撑爆）' \
  "$SUGGEST" 'b.WriteString("- 助手刚答：" + clampRunes(strings.TrimSpace(req.LastReply), suggestReplyRunes) + "\n")' \
  'b.WriteString("- 助手刚答：" + strings.TrimSpace(req.LastReply) + "\n")' \
  'FAIL: TestSuggestPrompt_ClipsLongReply'

inject_case 'JSON 模式关掉（模型开始套 ```json 围栏，解析全靠捡漏）' \
  "$AGENT" 'return cli.Chat(ctx, sys, user, true)' 'return cli.Chat(ctx, sys, user, false)' \
  'FAIL: TestSuggestRoute_RegisteredAndServesChips'

inject_case '路由没注册（前端静默降级成规则版，谁也不会发现）' \
  "$ROUTER" 'mux.Handle("POST /api/chat/suggest", suggest)' 'suggest = suggest' \
  'FAIL: TestSuggestRoute_RegisteredAndServesChips'

inject_case '超时被去掉（模型卡住时整个请求跟着挂着）' \
  "$SUGGEST" 'ctx, cancel := context.WithTimeout(ctx, h.timeout)' 'ctx, cancel := context.WithCancel(ctx)' \
  'FAIL: TestSuggestRoute_TimesOutAsEmpty'

inject_case '空输入不再短路（首页刚打开就白白烧一次模型）' \
  "$SUGGEST" 'if strings.TrimSpace(req.LastUser) == "" {' 'if false {' \
  'FAIL: TestSuggestRoute_EmptyInputShortCircuits'

inject_case '没配模型时不再短路（走了 nil 引擎的 ensureLLM）' \
  "$SUGGEST" 'if h.eng == nil || !h.eng.HasLLM() {' 'if false {' \
  'FAIL: TestSuggestRoute_NoLLMIsEmpty200'

echo
if ! out="$(run_test)"; then
  echo "✗ 还原后测试还是红的 —— 注入没被干净还原，出货文件可能已被改坏"
  echo "$out" | tail -20
  fails=$((fails + 1))
else
  echo "还原：全绿 ✓"
fi

echo
[ "$fails" -eq 0 ] && echo "全部注入都被抓住，断言可信" || echo "$fails 条注入没被抓住 —— 断言需要收紧"
exit $((fails > 0))
