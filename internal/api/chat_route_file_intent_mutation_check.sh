#!/usr/bin/env bash
# 「要文件却被路由到写作技能」纠偏闸门的断言自证脚本。
#
# 为什么必须有这个脚本：chat_route_file_intent_test.go 全绿只能证明「现在没坏」，
# 证明不了「坏了会被抓住」。这条链路上有三类断言最容易写成摆设：
#   - 只断言「有没有那句说明」——说明打了、文件照样没出来，用户还是拿不到 Word；
#   - 断言锚在具体措辞上——换成别的说法就假红（本文件初版锚了字面词「文档生成」，
#     真答案写的是「已改用「办公文档管家」生成」，尺子把好人当坏人拦下）；
#   - 闸门看起来装了、实际恒不生效（初版 HasDocJSONContract 只认 "DOCJSON" 这个
#     字面量，而真实提示词里从没出现过它 —— 闸门在生产里永远不开）。
# 做法：往**出货文件**（chat.go / agent.go）注入真实故障，要求对应断言变红；还原后回绿。
#
# 关于 A2 断言（发文请求真发给了带契约的技能）：它和 A1 是同生守卫 —— 请求没发给
# 带契约的技能，parseDocJSON 必然失败、也就必然没有 file 帧，A1 先 Fatalf，A2 没机会
# 打印。为了让每条断言都有专属注入而硬造一个「文件出来了但请求发给了别的技能」的假故障，
# 只会得到一条永远抓不住东西的假自证。这里如实标注：A2 随 A1 同红，不单列注入。
#
# 用法：bash internal/api/chat_route_file_intent_mutation_check.sh
set -uo pipefail
cd "$(dirname "$0")/../.."
export PATH=/usr/local/go/bin:$PATH

CHAT=internal/api/chat.go
AGENT=internal/agent/agent.go
BAK_DIR="$(mktemp -d)"
for f in "$CHAT" "$AGENT"; do
  cp "$f" "$BAK_DIR/$(basename "$f")"
done
sums() { md5sum "$CHAT" "$AGENT" | awk '{print $1}'; }
BEFORE="$(sums)"

# restore 只还原，**不删备份**：每条注入后都要复原一次，备份删了第二次还原就是空操作，
# 注入状态会一路带到下一条（自证结果全乱）。也因此**不用 git checkout** ——
# 出货文件上可能压着未提交的人工改动，git 还原会把人的劳动一起抹掉。
restore() {
  for f in "$CHAT" "$AGENT"; do
    cp "$BAK_DIR/$(basename "$f")" "$f"
  done
}
cleanup() { restore; rm -rf "$BAK_DIR"; }
trap cleanup EXIT

fails=0
run_test() {
  go test ./internal/api/ -run 'TestRouteFileIntent|TestRouteWriteIntent' -count=1 2>&1
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

# 注入 1 = 修复前的原样：闸门恒不开，静默降级成写正文（线上 docgen 腿红就是这个形态）。
inject_case '闸门恒不开（intent=docgen 命中写作技能时静默降级成写正文）' \
  "$CHAT" 'if dsc := h.eng.PickDocGenSkill(req.Message); dsc != nil {' \
  'if dsc := (*agent.SkillContent)(nil); dsc != nil {' \
  '要 Word 却只拿到文字'

# 注入 2 = 今天真实踩到的坑：契约锚点退回「认某个词」，对真技能恒 false。
inject_case '契约锚点退回认字面词（真实的文档技能没有这个词，闸门永远不开）' \
  "$AGENT" 'hasSpec := strings.Contains(p, "docjson") ||
		(strings.Contains(p, `"format"`) && strings.Contains(p, `"filename"`))' \
  'hasSpec := strings.Contains(p, "docjson")' \
  '要 Word 却只拿到文字'

# 注入 3 = 方向开错：不看来意图，凡是命中非 docgen 技能就纠偏。
inject_case '不看意图就纠偏（用户要写文章，却被塞一份文件下载）' \
  "$CHAT" 'if strings.EqualFold(strings.TrimSpace(eval.Intent), "docgen") && sc.SkillType != model.SkillTypeDocGen {' \
  'if sc.SkillType != model.SkillTypeDocGen {' \
  'FAIL: TestRouteWriteIntentNotCoercedToDocGen'

# 注入 4 = 换了技能但不讲为什么换（用户要的是「为什么」，不是「skill_type=docgen」）。
inject_case '换技能不讲原因（用户看不懂自己锁的技能为什么被绕过）' \
  "$CHAT" '型技能，产出正文而不是文件；" +' '型技能，" +' \
  '换技能没讲清为什么换'

echo
if ! out="$(run_test)"; then
  echo "✗ 还原后测试还是红的 —— 注入没被干净还原，出货文件可能已被改坏"
  echo "$out" | tail -20
  fails=$((fails + 1))
elif [ "$(sums)" != "$BEFORE" ]; then
  # 文件内容必须**逐字节**回到注入前（含未提交的人工改动），否则这个脚本本身在改坏仓库。
  echo "✗ 还原后文件内容与注入前不一致 —— 出货文件被这个脚本改动了"
  md5sum "$CHAT" "$AGENT" | sed 's/^/      /'
  fails=$((fails + 1))
else
  echo "还原：全绿且逐字节一致 ✓"
fi

echo
[ "$fails" -eq 0 ] && echo "全部注入都被抓住，断言可信" || echo "$fails 条注入没被抓住 —— 断言需要收紧"
exit $((fails > 0))
