#!/usr/bin/env bash
# 大模型输出 JSON 的「手抖容忍层」断言自证脚本。
#
# 为什么必须有：param_flex_test.go 全绿只能证明「现在没坏」，证明不了「退回严格解
# 会被抓住」。这条链路上最容易写成摆设的断言有两类：
#   - 只断言「能解出来」—— 解出来但字段是空的（比如 options 整条丢了），下拉渲染成空，
#     用户看到的还是错的东西，测试却绿；
#   - 只断言「没报错」—— 把宽容做成「什么都吞」，连非法 JSON 都放行，
#     等于把模型的错藏起来，排障时两眼一抹黑。
# 做法：往**出货文件** param_flex.go 注入「退回 Go 严格解」的真实故障，要求对应断言变红；
# 还原后回绿。注入的就是线上那一次真故障的形态（2026-09-17：整轮训练 18 秒中断）。
#
# 用法：bash internal/model/param_flex_mutation_check.sh
set -uo pipefail
cd "$(dirname "$0")/../.."
export PATH=/usr/local/go/bin:$PATH

FLEX=internal/model/param_flex.go
BAK_DIR="$(mktemp -d)"
cp "$FLEX" "$BAK_DIR/param_flex.go"
BEFORE="$(md5sum "$FLEX" | awk '{print $1}')"

# restore 只还原、**不删备份**：每条注入后都要复原一次，备份删了第二次还原就是空操作，
# 注入状态会一路带到下一条（自证结果全乱）。也因此**不用 git checkout** ——
# 出货文件上可能压着未提交的人工改动，git 还原会把人的劳动一起抹掉。
restore() { cp "$BAK_DIR/param_flex.go" "$FLEX"; }
cleanup() { restore; rm -rf "$BAK_DIR"; }
trap cleanup EXIT

fails=0
run_test() {
  go test ./internal/model/ -run 'TestParam' -count=1 2>&1
}

# ---------- 基线：不注入时必须全绿 ----------
if ! out="$(run_test)"; then
  echo "基线就是红的，先修好再来做注入自证："
  echo "$out" | tail -20
  exit 1
fi
echo "基线：全绿 ✓"

# 注入：$1=故障说明 $2=锚点 $3=替换文本 $4=应变红的断言
inject_case() {
  local desc="$1" old="$2" new="$3" expect="$4"
  python3 - "$FLEX" "$old" "$new" <<'PY'
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

# 注入 1 = 线上那一次真故障的原样：options 退回 Go 严格解，对象数组当场炸整轮训练。
inject_case 'options 退回严格解（对象数组 → 整轮训练第 2 步中断）' \
  'p.Options = flexStrList(r.Options)' \
  'if err := json.Unmarshal(r.Options, &p.Options); err != nil {
		return err
	}' \
  '--- FAIL: TestParamOptionsObjectArrayIsTolerated'

# 注入 2 = 把「是」这类中文布尔吞掉：required 恒 false，必填项在页面上变成选填。
inject_case '中文布尔被吞（"是" → required 恒 false，必填项静默变选填）' \
  'p.Required = flexBool(r.Required)' \
  'p.Required = strings.Contains(string(r.Required), "true")' \
  '--- FAIL: TestParamScalarCoercion'

# 注入 3 = 救不回来时把选项整条丢掉（下拉少一项，用户不知道有这个选项）。
# 注：这条原来是「非法 JSON 静默放行」，实测注入后**测试没红** —— 那个错是 Go 解码器
# 在顶层先炸的，不依赖本包代码，拿它当自证目标只会得到假自证。已换成承重的那条。
inject_case '救不回来就丢数据（选项整条消失，下拉少一项）' \
  '		if out, err := json.Marshal(t); err == nil {
			return string(out)
		}
		return ""' \
  '		return ""' \
  '--- FAIL: TestParamObjectWithoutLabelKeepsData'

echo
if ! out="$(run_test)"; then
  echo "✗ 还原后测试还是红的 —— 注入没被干净还原，出货文件可能已被改坏"
  echo "$out" | tail -20
  fails=$((fails + 1))
elif [ "$(md5sum "$FLEX" | awk '{print $1}')" != "$BEFORE" ]; then
  # 文件内容必须**逐字节**回到注入前（含未提交的人工改动），否则这个脚本本身在改坏仓库。
  echo "✗ 还原后文件内容与注入前不一致 —— 出货文件被这个脚本改动了"
  md5sum "$FLEX" | sed 's/^/      /'
  fails=$((fails + 1))
else
  echo "还原：全绿且逐字节一致 ✓"
fi

echo
[ "$fails" -eq 0 ] && echo "全部注入都被抓住，断言可信" || echo "$fails 条注入没被抓住 —— 断言需要收紧"
exit $((fails > 0))
