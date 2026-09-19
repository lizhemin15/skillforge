#!/usr/bin/env bash
# Eval「模型手抖容忍层」的断言自证脚本。
#
# 为什么必须有：eval_flex_test.go 全绿只证明「现在没坏」，证明不了「退回严格解会被抓住」。
# 这条链路的值钱之处在于**杀伤半径**：Eval 是每轮对话的第一跳，整包解析失败 =
# 分类结果全丢 + 重试（多一次模型调用，用户对着计时器多等）。线上实测那次模型
# 判断完全正确，只有 params 被双重编码成字符串，就被整包丢掉了。
#
# 做法：往**出货文件** eval_flex.go 注入「退回 Go 严格解」这类真故障，要求对应断言变红；
# 还原后回绿。注入形态就是线上那一次真故障（2026-09-19 通知类任务）。
#
# 用法：bash internal/agent/eval_flex_mutation_check.sh
set -uo pipefail
cd "$(dirname "$0")/../.."
export PATH=/usr/local/go/bin:$PATH

FLEX=internal/agent/eval_flex.go
BAK_DIR="$(mktemp -d)"
cp "$FLEX" "$BAK_DIR/eval_flex.go"
BEFORE="$(md5sum "$FLEX" | awk '{print $1}')"

# restore 只还原、**不删备份**：每条注入后都要复原一次，备份删了第二次还原就是空操作，
# 注入状态会一路带到下一条（自证结果全乱）。也因此**不用 git checkout** ——
# 出货文件上可能压着未提交的人工改动，git 还原会把人的劳动一起抹掉。
restore() { cp "$BAK_DIR/eval_flex.go" "$FLEX"; }
cleanup() { restore; rm -rf "$BAK_DIR"; }
trap cleanup EXIT

fails=0
run_test() {
  go test ./internal/agent/ -run 'TestEval' -count=1 2>&1
}

# ---------- 基线：不注入时必须全绿 ----------
if ! out="$(run_test)"; then
  echo "基线就是红的，先修好再来做注入自证："
  echo "$out" | tail -20
  exit 1
fi
echo "基线：全绿 ✓"

# 注入：$1=故障说明 $2=锚点 $3=替换文本 $4=应变红的断言（测试名或断言文案）
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

# 注入 1 = 线上那一次真故障的原样：params 退回 Go 严格解，双重编码的字符串当场炸整包。
inject_case 'params 退回严格解（双重编码字符串 → 整包分类丢失 + 白跑一次重试）' \
  'e.Params = flexObj(r.Params)' \
  '{
		var m map[string]interface{}
		if err := json.Unmarshal(r.Params, &m); err != nil {
			return err
		}
		e.Params = m
	}' \
  '--- FAIL: TestEvalLiveDoubleEncodedParamsIsTolerated'

# 注入 2 = 宽容层做成「什么都不接」：不报错但 params 空，下游照样拿不到文件类型与标题。
inject_case '双重编码不解码（不报错但参数全空 → 与没修一个观感）' \
  '		if err := json.Unmarshal([]byte(inner), &m); err == nil {
			return m
		}
		cur = []byte(inner)' \
  '		cur = []byte(inner)' \
  'params 一个都没解出来'

# 注入 3 = 步骤板 phase/status 兜底被删：前端认不出阶段 → 用户又只看到一个跳秒的计时。
inject_case '步骤板状态不兜底（前端认不出阶段 → 整块不渲染）' \
  '		default:
			s.Status = "done"
		}' \
  '		}' \
  '第 1 段状态期望兜底成 done'

echo
if ! out="$(run_test)"; then
  echo "✗ 还原后测试还是红的 —— 注入没被干净还原，出货文件可能已被改坏"
  echo "$out" | tail -20
  fails=$((fails + 1))
elif [ "$(md5sum "$FLEX" | awk '{print $1}')" != "$BEFORE" ]; then
  # 文件内容必须**逐字节**回到注入前（含未提交的人工改动），否则这个脚本本身在改坏仓库。
  echo "✗ 还原后文件内容与注入前不一致 —— 出货文件被这个脚本改动了"
  fails=$((fails + 1))
else
  echo "还原：全绿且逐字节一致 ✓"
fi

echo
[ "$fails" -eq 0 ] && echo "全部注入都被抓住，断言可信" || echo "$fails 条注入没被抓住 —— 断言需要收紧"
exit $((fails > 0))
