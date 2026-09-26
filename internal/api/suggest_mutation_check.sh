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
# 推荐行的「延迟预算」那半边守在内层：思考链关不掉时，接口照样 200 空数组、
# 前端照样静默降级 —— 所以 llm.FastJSON 也被这份自证覆盖。
FASTJSON=internal/llm/fastjson.go
BAK_DIR="$(mktemp -d)"
for f in "$SUGGEST" "$AGENT" "$ROUTER" "$FASTJSON"; do
  cp "$f" "$BAK_DIR/$(basename "$f")"
done
# 注意：restore 只负责还原，**不能**在这里删备份 —— 每条注入后都要还原一次，
# 备份删了第二次还原就成了空操作，注入状态会一路带到下一条（自证结果全乱）。
restore() {
  for f in "$SUGGEST" "$AGENT" "$ROUTER" "$FASTJSON"; do
    cp "$BAK_DIR/$(basename "$f")" "$f"
  done
}
trap 'restore; rm -rf "$BAK_DIR"' EXIT

fails=0
# 两个包一起跑：api 守「handler/路由/解析」，llm 守「这次调用真的够快」。
# ⚠️ 必须自己聚合 rc：直接顺序写两条 `go test` 的话，函数返回的是**最后一条**的
# 退出码 —— api 包真红了、llm 包绿着，整个 run_test 就是 0，注入自证会把
# 「抓住了」判成「没抓住」。第一版就这么写的：14 条注入全被报成假绿，
# 看上去是断言不行，其实是自证工具自己在骗人。
run_test() {
  local o1 o2 rc=0
  o1="$(go test ./internal/api/ -run 'Suggest' -count=1 2>&1)" || rc=1
  o2="$(go test ./internal/llm/ -run 'FastJSON|NormalizeBaseURL' -count=1 2>&1)" || rc=1
  printf '%s\n%s\n' "$o1" "$o2"
  return $rc
}

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
  "$FASTJSON" '"response_format": map[string]string{"type": "json_object"},' \
  '"response_format": map[string]string{"type": "text"},' \
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

# 「取不到模型就静默降级」这道短路有两个条件，但**只有 nil 那一半测得出来**：
#   · 拿掉 `h.eng == nil ||` → h.eng.HasLLM() 在 nil 接收者上取锁 → 空指针 panic
#     → 公网端点从 200 空数组变成 500/连接重置。这条注入打的就是它。
#   · 拿掉 `|| !h.eng.HasLLM()` → 走进 ensureLLM，那边返回 error，接口**仍然**
#     200 空数组 —— 与短路路径观测等价（2026-09-26 实测：原来那条 `if false`
#     注入就是这么变成假绿的）。它只是纵深防御，别为了「让它也能被抓」去写
#     一条自己造差异的假断言。
inject_case 'nil 引擎不再短路（路由没接线时端点从 200 空数组变成 panic）' \
  "$SUGGEST" 'if h.eng == nil || !h.eng.HasLLM() {' 'if !h.eng.HasLLM() {' \
  'FAIL: TestSuggestRoute_NoLLMIsEmpty200'

inject_case '思考链开关被删（线上必然超时，功能静默消失）' \
  "$FASTJSON" 'body["enable_thinking"] = false' 'body["_unused_thinking"] = false' \
  'FAIL: TestFastJSON_SendsKnobsThatKeepItFast'

inject_case 'max_tokens 上限被删（思考链可能吃满 completion，回空 content）' \
  "$FASTJSON" '"max_tokens":      maxTokens,' '"max_tokens":      0,' \
  'FAIL: TestFastJSON_SendsKnobsThatKeepItFast'

inject_case '严格网关的 400 重试被删（换 provider 等于功能消失）' \
  "$FASTJSON" 'case status == http.StatusBadRequest && knob == knobBoth:' 'case status == http.StatusTeapot && knob == knobBoth:' \
  'FAIL: TestFastJSON_RetriesWithoutKnobOn400'

inject_case '空 content 的报错不再指向思考链（排查方向会全错）' \
  "$FASTJSON" '多半是思考链吃掉了 max_tokens' '未返回内容' \
  'FAIL: TestFastJSON_EmptyContentExplainsReasoningBudget'

inject_case '禁编造的约束被删（模型开始编用户没说过的产品名/数字）' \
  "$SUGGEST" '"6. **send 里不许出现对话中没出现过的具体事实**：产品名、单位名、人名、数字、金额、日期、型号都不许编。" +' \
  '"" +' \
  'FAIL: TestSuggestPrompt_CarriesContext'

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
