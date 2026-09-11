#!/usr/bin/env bash
# 回归防线：字体目录「引用判定」的路径边界（Bug O）
#
# 背景：卸载实例时要知道「旧版共享字体目录 /usr/local/share/fonts/skillforge 还有没有
# 别人在用」。旧实现用 `grep -F "$目录"` 做子串匹配，于是
# PDF_FONT_FILE=/usr/local/share/fonts/skillforge-skillforge/gbsn00lp.ttf
# 里的前缀 /usr/local/share/fonts/skillforge 被命中 → 误判「仍被引用」→
# 旧共享目录永远清不掉（卸载有残留）。
#
# 本脚本不复制逻辑：它从真正出厂的 uninstall.sh 里**抽出** font_ref_regex() 来测，
# 保证测的就是发出去的那份代码（避免测试与实现各写一份、双双漂移）。
#
# 用法：bash deploy/offline/test-fontref.sh   （无需 root、无副作用）
set -uo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
TARGET="$HERE/uninstall.sh"
[ -f "$TARGET" ] || { echo "找不到 $TARGET"; exit 2; }

# 抽出函数体（从 "^font_ref_regex() {" 到下一个顶格 "}"）
FN=$(awk '/^font_ref_regex\(\) \{/{f=1} f{print} f&&/^\}/{exit}' "$TARGET")
[ -n "$FN" ] || { echo "✗ 无法从 uninstall.sh 抽出 font_ref_regex()（函数被改名/删除？）"; exit 2; }
eval "$FN"

PASS=0
FAIL=0
# $1=目录 $2=被搜文本 $3=期望(yes/no) $4=用例名
t() {
	local hits got
	hits=$(printf '%s\n' "$2" | grep -E -- "$(font_ref_regex "$1")" || true)
	[ -n "$hits" ] && got=yes || got=no
	if [ "$got" = "$3" ]; then
		echo "  ✓ $4"
		PASS=$((PASS + 1))
	else
		echo "  ✗ $4（期望 $3，实得 $got）"
		FAIL=$((FAIL + 1))
	fi
}

L=/usr/local/share/fonts/skillforge
A=/usr/local/share/fonts/skillforge-sfk4-a

echo "### 旧版共享目录 $L"
t "$L" "SKILLFORGE_PDF_FONT_FILE=$L"                 yes "行尾精确值=本目录 → 算引用"
t "$L" "SKILLFORGE_PDF_FONT_FILE=$L/gbsn00lp.ttf"    yes "目录内文件 → 算引用"
t "$L" "Environment=\"PDF_FONT_FILE=$L\""            yes "带引号精确值 → 算引用"
t "$L" "ExecStart=/x --font-dir $L"                  yes "空白分隔 → 算引用"
t "$L" "PDF_FONT_FILE=$L-skillforge/gbsn00lp.ttf"    no  "★Bug O：前缀同名的另一实例目录 → 不算引用"
t "$L" "PDF_FONT_FILE=$L-sfk4-a/gbsn00lp.ttf"        no  "★Bug O：-sfk4-a 前缀 → 不算引用"
t "$L" "# 历史说明里提到 $L-x"                        no  "注释里的同前缀串 → 不算引用"

echo "### 实例目录 $A"
t "$A" "PDF_FONT_FILE=$A/gbsn00lp.ttf"               yes "实例自己的字体文件 → 算引用"
t "$A" "PDF_FONT_FILE=$A"                            yes "精确值 → 算引用"
t "$A" "PDF_FONT_FILE=${A}2/gbsn00lp.ttf"            no  "更像长的名字(-a2) → 不算引用"
t "$A" "PDF_FONT_FILE=/usr/local/share/fonts/skillforge-sfk4-b/gbsn00lp.ttf" no "别的实例 → 不算引用"

echo "PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ]
