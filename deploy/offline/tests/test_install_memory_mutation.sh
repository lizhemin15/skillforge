#!/usr/bin/env bash
#
# test_install_memory_mutation.sh —— 「装前内存检查」的负向自证
#
# 每项都往 install.sh 里注入一个**真实故障**（仍是合法 shell、装得下去，只是行为退化），
# 要求 test_install_memory.sh 精确地在那一条断言上转红、退出码非零，然后还原、复跑回绿。
#
# 为什么不做成「删掉整段」：删掉会让脚本语法崩掉，红的就不是断言而是解析错误 ——
# 崩溃红不算红（那种红无法证明断言真的在盯着行为）。所以每项注入都保持可执行。
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
INSTALL_SH="$HERE/../install.sh"
CHECK="$HERE/test_install_memory.sh"
WORK="$(mktemp -d)"
BAK="$WORK/install.sh.bak"

cp "$INSTALL_SH" "$BAK"
cleanup() { cp "$BAK" "$INSTALL_SH"; rm -rf "$WORK"; }
trap cleanup EXIT

fail=0
OUT=""

# inject <名字> <含 old 的文件> <含 new 的文件>
# 替换不中即报事前失败并退出 —— 注入点失效时脚本会一路绿，看着像自证通过，
# 其实什么都没注入（这类假绿比红危险得多）。
inject() {
	local name="$1" oldf="$2" newf="$3"
	if ! python3 - "$INSTALL_SH" "$oldf" "$newf" <<'PY'
import sys
path, oldf, newf = sys.argv[1], sys.argv[2], sys.argv[3]
s = open(path, encoding='utf-8').read()
old = open(oldf, encoding='utf-8').read()
new = open(newf, encoding='utf-8').read()
if old not in s:
    sys.exit(3)
open(path, 'w', encoding='utf-8').write(s.replace(old, new, 1))
PY
	then
		echo "FAIL[事前]: 注入点不存在 —— 这项突变没生效，后面的绿是假的"
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
		echo "FAIL[$name]: 精确转红了但退出码是 0 —— CI 里等于没挂这把尺子"
		fail=1
		return
	fi
	echo "$name 精确转红：FAIL[$want] ✓（rc=$rc）"
}

assert_green() { # <名字>
	local rc=0
	OUT="$(bash "$CHECK" 2>&1)" || rc=$?
	if [ "$rc" -ne 0 ]; then
		echo "FAIL[$1]: 还原了却不绿 —— 注入残留或验收脚本本身坏了"
		grep -E '^FAIL' <<<"$OUT" | sed 's/^/    /'
		fail=1
		return
	fi
	echo "$1 复跑：绿 ✓"
}

echo "── 基准（未注入）──"
assert_green "基准"

# ── P-a：只看 /proc/meminfo，忽略 cgroup 限额 ──────────────────────────
echo
echo "── P-a：容器里物理内存看着 16GB、实际只给 800MB，实现只看物理内存就会误判充裕 ──"
cat > "$WORK/a.old" <<'EOF'
	if [ -n "$cg_bytes" ]; then
EOF
cat > "$WORK/a.new" <<'EOF'
	if false; then
EOF
if inject "P-a" "$WORK/a.old" "$WORK/a.new"; then
	assert_red "P-a" "M4a"
fi
cp "$BAK" "$INSTALL_SH"

# ── P-b：把「警告」升级成「拒绝安装」──────────────────────────────────
echo
echo "── P-b：内存小就拒绝安装（内存紧的机器也能跑纯文字功能，硬拦会把客户挡在门外）──"
cat > "$WORK/b.old" <<'EOF'
	c_warn "内存偏小：本机上限约 ${mb} MB —— 文档解析服务大概率会被系统 OOM 干掉。"
EOF
cat > "$WORK/b.new" <<'EOF'
	die "内存偏小：本机上限约 ${mb} MB —— 文档解析服务大概率会被系统 OOM 干掉。"
EOF
if inject "P-b" "$WORK/b.old" "$WORK/b.new"; then
	assert_red "P-b" "M6"
fi
cp "$BAK" "$INSTALL_SH"

# ── P-c：探测不到也当「内存偏小」（把「未知」报成「有问题」）──────────
echo
echo "── P-c：读不到 /proc/meminfo 也报「内存偏小」（把「未知」当成「失败」）──"
cat > "$WORK/c.old" <<'EOF'
	if [ "$mb" -eq 0 ]; then
EOF
cat > "$WORK/c.new" <<'EOF'
	if false; then
EOF
if inject "P-c" "$WORK/c.old" "$WORK/c.new"; then
	assert_red "P-c" "M5b"
fi
cp "$BAK" "$INSTALL_SH"

# ── P-d：偏小档只报问题、不给离线能做的修法 ───────────────────────────
echo
echo "── P-d：偏小档只说「内存不够」，不说怎么就地加 swap（红灯不带路）──"
cat > "$WORK/d.old" <<'EOF'
	c_warn "  · 加 swap（离线可用，最省事）："
	c_warn "      fallocate -l 4G /swapfile && chmod 600 /swapfile && mkswap /swapfile && swapon /swapfile"
EOF
cat > "$WORK/d.new" <<'EOF'
	c_warn "  · 加内存（容量不够就是不够）"
EOF
if inject "P-d" "$WORK/d.old" "$WORK/d.new"; then
	assert_red "P-d" "M3c"
fi
cp "$BAK" "$INSTALL_SH"

# ── P-e：反面防呆 —— 出货文件里根本没有这项检查时，不许静默「全绿」──
echo
echo "── P-e：出货文件里没有 check_memory（整段被删）时，验收脚本必须报出来 ──"
cat > "$WORK/e.old" <<'EOF'
check_memory
EOF
cat > "$WORK/e.new" <<'EOF'
# check_memory 被删掉了
EOF
if inject "P-e" "$WORK/e.old" "$WORK/e.new"; then
	OUT="$(bash "$CHECK" 2>&1)"; rc=$?
	if grep -q '装前内存检查验收：通过' <<<"$OUT" && [ "$rc" -eq 0 ]; then
		echo "FAIL[P-e]: 检查整段没了，验收脚本还报通过 —— 这脚本从来没在盯这件事"
		fail=1
	else
		echo "P-e 转红：检查缺失被报出 ✓（rc=$rc）"
	fi
fi
cp "$BAK" "$INSTALL_SH"

echo
echo "── 收尾：还原后必须回绿 ──"
assert_green "收尾"
if [ "$(bash -n "$INSTALL_SH" && echo OK)" != "OK" ]; then
	echo "FAIL[收尾]: install.sh 语法被注入改坏了（还原不干净）"
	fail=1
fi

[ "$fail" -eq 0 ] || { echo "内存检查突变自证：失败"; exit 1; }
echo "内存检查突变自证：通过"
