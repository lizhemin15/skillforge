#!/usr/bin/env bash
# 变异自证：TextOnlyDropsDocGenSkill 这把尺子（Go 侧）四个方向都必须能变红。
# 四重判据：①注入点存在（assert，缺了就指名「尺子坏」）②注入后出现 FAIL 行
#           ③红的是**预期那一格** ④还原后回绿。
# 退出码：0 = 四记变异全命中且无残留；1 = 有方向不响/红错格/有残留（preflight 靠这个 rc 判红）。
#
# 坑（写这脚本时自己踩的，记在这儿免得下次再来）：
#   · 调用点不要给 run() 的输出再加 `grep '^(ok|FAIL|--- FAIL)'` —— 子测试行是
#     `    --- FAIL: …`（有缩进），加锚点过滤会把真红行全吞掉，表现成「注入后一片安静」。
#   · 还原用 `cp` 备份，**绝不用 `git checkout`**：本轮手误 `git checkout agent.go`
#     把尚未提交的新代码整段冲掉（备份法只回滚注入的那一处）。
#   · 末尾别用 `grep -c … || echo ok` 收尾：grep 没命中时 rc=1 会被 `||` 吃掉，
#     于是「有残留」也变绿。判据必须自己 if 判、自己 accumulate BAD。
#   · 跑的时候源码被临时改写，不许并行提交、不许并行跑 go test。
set -u
cd /root/skillforge
F=internal/agent/agent.go
cp "$F" /tmp/agent.go.orig

run() {
  go test ./internal/agent/ -run 'TextOnly' -count=1 2>&1 |
    grep -E '^(ok|FAIL|    --- FAIL: TestTextOnlyDropsDocGenSkill/)' | sed 's/^/  /'
}
restore() { cp /tmp/agent.go.orig "$F"; }

BAD=0
need() { # need <方向> <实测红格数> <期望红格数> <要出现的那格名字>
  local n="$2"
  if [ "$n" -lt "$3" ]; then
    echo "  ✗ $1：注入后红格只有 $n 个（期望 ≥$3）→ 这把尺子在这个方向上不响"
    BAD=1
  elif ! run | grep -q "$4"; then
    echo "  ✗ $1：响了，但红的不是预期那一格（没看到「$4」）→ 判据③不过"
    BAD=1
  else
    echo "  ✓ $1：红在预期格（$4）"
  fi
}
reds() { run | grep -c '^      --- FAIL: TestTextOnlyDropsDocGenSkill/' || true; }

inj() { # inj <文件> <原文> <替换> <标签>
  python3 - "$1" "$2" "$3" "$4" <<'PY'
import sys
p, old, new, tag = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
s = open(p, encoding='utf-8').read()
if old not in s:
    print(f"  ✗ {tag}：注入点不存在（源码/尺子已变，先修尺子再谈结论）→ 判据①不过")
    raise SystemExit(3)
open(p, 'w', encoding='utf-8').write(s.replace(old, new, 1))
print(f"  注入落地：{tag}")
PY
}

GATE_OLD='	if manual || skillType != model.SkillTypeDocGen {'

echo "===== 基线（必须全绿：无 FAIL 行） ====="
run
if run | grep -q 'FAIL'; then
  echo "  ✗ 基线就红了 —— 先修产品或尺子，别急着跑变异"
  BAD=1
else
  echo "  ✓ 基线无 FAIL"
fi

echo
echo "===== M-A：闸门永不弃技能（= 线上那 4/8 故障原样） → 正向用例必须红 ====="
inj "$F" "${GATE_OLD}
		return false
	}" '	if true {
		return false
	}' "闸门恒不触发" || BAD=1
run
need "M-A" "$(reds)" 2 "线上原文+命中docgen型技能"
restore

echo
echo "===== M-B：去掉 manual 例外（用户点名的技能也被抢走） → manual 那格必须红 ====="
inj "$F" "$GATE_OLD" "	if skillType != model.SkillTypeDocGen {" "manual 例外被拆" || BAD=1
run
need "M-B" "$(reds)" 1 "同句但用户手动点名了该技能"
restore

echo
echo "===== M-C：不看技能类型（连写作型技能也弃） → 反向故障两格必须红 ====="
inj "$F" "${GATE_OLD}
		return false
	}" '	if false {
		return false
	}' "技能类型判据被拆" || BAD=1
run
need "M-C" "$(reds)" 2 "同句命中的是写作型技能"
restore

echo
echo "===== M-D：拿题材词当判据（历史上真被这样写过） → 压舱石那格必须红 ====="
inj "$F" "	return ExplicitTextOnly(msg)" '	return ExplicitTextOnly(msg) || strings.Contains(msg, "通知")' "题材词混进判据" || BAD=1
run
need "M-D" "$(reds)" 2 "要文件的用户"
restore

echo
echo "===== 还原后（必须回到全绿） ====="
run
if run | grep -q 'FAIL'; then
  echo "  ✗ 还原后仍红 —— 备份/还原路径有 bug，工作区可能已脏"
  BAD=1
else
  echo "  ✓ 还原后回绿"
fi
if grep -q 'strings.Contains(msg, "通知")' "$F"; then
  echo "  ✗ 有注入残留（题材词那条还在源码里）"
  BAD=1
else
  echo "  ✓ 无注入残留"
fi

echo
if [ "$BAD" = 0 ]; then
  echo "全部通过：四记变异全部红在预期格，基线/还原均绿，无残留。"
else
  echo "有方向没守成 —— 见上面 ✗ 行。"
fi
exit "$BAD"
