#!/usr/bin/env bash
#
# test_install_prediag_mutation.sh —— 装前体检的**负向自证**
#
# 每项往 install.sh 的 prediag_gate 里注入一个**真实故障**（仍是合法 shell、装得下去，
# 只是行为退化），要求 test_install_prediag.sh 精确地在那一条断言上转红、退出码非零，
# 然后还原、复跑回绿。不这么做的后果：函数里某条分支写坏了，测试仍然全绿，
# 客户在真机上才知道 —— 而这条分支的全部意义就是「装之前把问题说清」。
#
# 为什么不做成「删掉整段」：删掉会让脚本语法崩、抽不出函数，红的就不是断言而是解析
# 错误 —— 崩溃红不算红（那种红无法证明断言真的在盯着行为）。每项注入都保持可执行。
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
INSTALL_SH="$HERE/../install.sh"
CHECK="$HERE/test_install_prediag.sh"
WORK="$(mktemp -d)"
BAK="$WORK/install.sh.bak"
cp "$INSTALL_SH" "$BAK"
cleanup() { cp "$BAK" "$INSTALL_SH"; rm -rf "$WORK"; }
trap cleanup EXIT

fail=0
OUT=""

# inject <名字> <old> <new>：替换不中即报事前失败 —— 注入点失效时脚本会一路绿，
# 看着像自证通过，其实什么都没注入（这类假绿比红危险得多）。
inject() {
	local name="$1" old="$2" new="$3"
	cp "$BAK" "$INSTALL_SH"
	if ! OLD="$old" NEW="$new" python3 - "$INSTALL_SH" <<'PY'
import os, sys
path = sys.argv[1]
s = open(path, encoding='utf-8').read()
old, new = os.environ['OLD'], os.environ['NEW']
if old not in s:
    sys.exit(3)
open(path, 'w', encoding='utf-8').write(s.replace(old, new, 1))
PY
	then
		echo "FAIL[事前]: 注入点不存在（$name）—— 这项突变没生效，后面的绿是假的"
		fail=1
		return 1
	fi
	# 注入后语法必须仍然合法：语法崩掉的红不算红
	if ! bash -n "$INSTALL_SH"; then
		echo "FAIL[事前]: 注入 $name 后 install.sh 语法不合法 —— 这不算真故障注入"
		fail=1
		return 1
	fi
	return 0
}

assert_red() { # <名字> <期望的 FAIL 标签>
	local name="$1" want="$2" rc=0
	OUT="$(bash "$CHECK" 2>&1)" || rc=$?
	if [ -z "$OUT" ]; then
		echo "FAIL[$name]: 验收脚本没有任何输出（崩了？）"
		fail=1
		return
	fi
	if ! grep -q "FAIL\[$want\]" <<<"$OUT"; then
		echo "FAIL[$name]: 期望 $want 精确转红，实际没有 —— 注入的是真故障吗？"
		grep -E '^FAIL' <<<"$OUT" | sed 's/^/    /'
		fail=1
		return
	fi
	if [ "$rc" -eq 0 ]; then
		echo "FAIL[$name]: $want 红了，但验收脚本退出码仍是 0 —— 挂到 CI 里等于没挂"
		fail=1
		return
	fi
	echo "ok  [$name] 注入后精确红在 $want，rc=$rc"
}

# 基线：未注入时必须全绿（否则后面的「红」无法归因给注入）
if ! bash "$CHECK" >/dev/null 2>&1; then
	echo "FAIL[事前]: 未注入时验收脚本就不是绿的 —— 先修本体，再谈自证"
	bash "$CHECK" 2>&1 | grep -E '^FAIL' | sed 's/^/    /'
	exit 1
fi
echo "ok  [基线] 未注入时全绿"

# ---- M1：拦住之后直接把客户放走（abort 分支不再退出）----
if inject "M1" '	printf '"'"'  %s\n'"'"' "要根治：换一份与本机匹配的离线包（体检输出里写着本机 glibc 与这份包的要求）。"
	exit 1
}' '	printf '"'"'  %s\n'"'"' "要根治：换一份与本机匹配的离线包（体检输出里写着本机 glibc 与这份包的要求）。"
	return 0
}'; then
	assert_red "M1 体检有问题却放行" "S2"
fi

# ---- M2：把体检的退出码吞掉（rc 恒 0 → 问题再也判不出来）----
if inject "M2" 'out="$("$bin" -diag 2>&1)" || rc=$?' 'out="$("$bin" -diag 2>&1 || true)"'; then
	assert_red "M2 吞掉体检退出码" "S2"
fi

# ---- M3：老包保护失效（不支持 -diag 的老包被判成故障）----
if inject "M3" '*"flag provided but not defined"*|*"无法识别的参数"*|*"unknown flag"*)' '*"__绝不匹配__"*)'; then
	assert_red "M3 老包保护失效" "S5"
fi

# ---- M4：体检原文不再转发给客户（结论只留在函数内部）----
if inject "M4" '	printf '"'"'%s\n'"'"' "$out"
	case "$out" in' '	:
	case "$out" in'; then
	assert_red "M4 体检原文不转发" "S1b"
fi

# 还原后必须回绿（证明前面的红确实是注入引起的）
cp "$BAK" "$INSTALL_SH"
if bash "$CHECK" >/dev/null 2>&1; then
	echo "ok  [还原] 复跑回绿"
else
	echo "FAIL[还原]: 还原后仍未转绿"
	bash "$CHECK" 2>&1 | grep -E '^FAIL' | sed 's/^/    /'
	fail=1
fi

echo
if [ "$fail" -eq 0 ]; then
	echo "装前体检负向自证：全部通过"
	exit 0
fi
echo "装前体检负向自证：有失败项"
exit 1
