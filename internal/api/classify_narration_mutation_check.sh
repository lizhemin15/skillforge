#!/usr/bin/env bash
# 「分类跳静默破冰」的断言自证脚本。
#
# 为什么必须有：classify_narration_test.go 全绿只证明「现在挂着旁白」，证明不了
# 「哪天有人把挂载删了 / 换成编造的文案，会被抓住」。而这条链路的故障形态用户是
# **看得见**的：线上实测分类跳静默 54.0s，屏幕上只有不断 +1 的「已用 Ns」，
# 用户原话「现在速度过于慢了，中间可以流式输出思考的一些中间材料，现在一直卡着计时」。
#
# 做法：往**出货文件**注入真故障，要求**预期那条**断言变红，还原后回绿。
# 三条注入分别打在三种坏法上：接线被摘、冷启动编上下文、旁白行被当正文混排。
#
# 用法：bash internal/api/classify_narration_mutation_check.sh
set -uo pipefail
cd "$(dirname "$0")/../.."
export PATH=/usr/local/go/bin:$PATH

CHAT=internal/api/chat.go
NARR=internal/api/classify_narration.go
BAK_DIR="$(mktemp -d)"
cp "$CHAT" "$BAK_DIR/chat.go"
cp "$NARR" "$BAK_DIR/classify_narration.go"

# restore 只还原、**不删备份**（同 eval_flex 那条脚本：备份删了第二次还原就是空操作，
# 注入状态会一路带到下一条）。也**不用 git checkout** —— 出货文件上可能压着未提交的
# 人工改动，git 还原会把人的劳动一起抹掉。
restore() {
  cp "$BAK_DIR/chat.go" "$CHAT"
  cp "$BAK_DIR/classify_narration.go" "$NARR"
}
cleanup() { restore; rm -rf "$BAK_DIR"; }
trap cleanup EXIT

fails=0
run_test() {
  go test ./internal/api/ -run 'TestClassifyNarrationTellsLocalTruths|TestClassifyHopNarratesWhenModelIsSilent' -count=1 2>&1
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

# 注入 1 = 线上那 54s 静默的原样：把分类跳的旁白调用摘掉（接线断掉，模型又不吐字）。
inject_case '分类跳的旁白被摘掉（模型不吐字 → 屏幕只剩跳秒的计时）' \
  "$CHAT" \
  'stopNarrate := clock.Narrate(classifyNarration(req.Message, history))' \
  'stopNarrate := func() {}' \
  '分类跳静默'

# 注入 2 = 为了让屏幕好看去编上下文：冷启动（无素材、无上文）时照样说有素材。
# 注意这里故意用**不带「段」字的**说法（「已带上你给的素材」）——原实现的窄判据
# 只查「段素材」，正好放过这一种同样在骗人的写法，是被这条注入抓出来后改宽的。
inject_case '冷启动旁白编造上下文（没有素材却说带着素材）' \
  "$NARR" \
  'if mats == 0 && turns == 0 {
		out = append(out, "这是本轮第一句话，没有上文要承接")
	}' \
  'if mats == 0 && turns == 0 {
		out = append(out, "已带上你给的素材一起判定")
	}' \
  '无素材无历史时不该出现'

# 注入 3 = 旁白行自带「· 」：Narrate 会给每行再补一个，界面上变成双点「· · 收到…」。
inject_case '旁白行自带「· 」（与 Narrate 补的点叠成双点）' \
  "$NARR" \
  'out := []string{"收到你的需求（本轮 "' \
  'out := []string{"· 收到你的需求（本轮 "' \
  '叠成双点'

echo
if [ "$fails" -eq 0 ]; then
  echo "自证通过：3/3 条注入都被预期断言抓住，且还原后回绿。"
else
  echo "自证失败：$fails 条注入没被抓住（这些断言现在没有判别力）。"
fi

# 还原必须逐字节回到原样：否则脚本本身会把注入态留在出货文件里（比不跑更糟）。
before="$(md5sum "$BAK_DIR/chat.go" "$BAK_DIR/classify_narration.go" | awk '{print $1}' | tr '\n' ' ')"
after="$(md5sum "$CHAT" "$NARR" | awk '{print $1}' | tr '\n' ' ')"
if [ "$before" != "$after" ]; then
  echo "✗ 还原后 md5 不一致：$before vs $after —— 出货文件被留在改动状态！"
  exit 1
fi
echo "还原校验：逐字节一致 ✓"
[ "$fails" -eq 0 ] || exit 1
