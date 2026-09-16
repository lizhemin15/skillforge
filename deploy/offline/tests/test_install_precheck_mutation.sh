#!/usr/bin/env bash
#
# test_install_precheck_mutation.sh —— 「安装期三项预检」的负向自证
#
# 为什么要有这一份：装前内存检查（test_install_memory_mutation.sh）有注入自证，
# 但另外三把安装期尺子长期只有**正向场景**：
#   · test_install_trust_precheck.sh  （TLS 信任库预检）
#   · test_install_public_url.sh      （对外地址默认值）
#   · test_install_timezone.sh        （时区探测与写入）
# 「正向 4/4 通过」只证明这些字样在**当前实现**下会出现；不证明它们在实现退化后会**消失**。
# 实现一旦退回历史写法（坏路径被静默忽略 / 客户显式地址被覆盖 / 时区探测了却不写），
# 这三把尺子照样全绿 —— 那就是「挂着的尺子在量空气」。
#
# 每项都往 install.sh 注入一个**真实故障**：仍是合法 shell、装得下去，只是行为退化，
# 要求对应尺子**精确地**在那一条断言上转红、退出码非零；随后还原、复跑回绿。
# 崩溃红不算红（解析错误无法证明断言在盯着行为），所以注入一律保持可执行。
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
INSTALL_SH="$HERE/../install.sh"
WORK="$(mktemp -d)"
BAK="$WORK/install.sh.bak"

cp "$INSTALL_SH" "$BAK"
cleanup() { cp "$BAK" "$INSTALL_SH"; rm -rf "$WORK"; }
trap cleanup EXIT

fail=0
OUT=""

# inject <名字> <含 old 的文件> <含 new 的文件>
# 替换不中即报事前失败 —— 注入点失效时脚本会一路绿，看着像自证通过，
# 其实什么都没注入（这类假绿比红危险得多）。
inject() {
	local name="$1" oldf="$2" newf="$3"
	if ! python3 - "$INSTALL_SH" "$oldf" "$newf" <<'PY'
import sys
path, oldf, newf = sys.argv[1], sys.argv[2], sys.argv[3]
s = open(path, encoding='utf-8').read()
old = open(oldf, encoding='utf-8').read()
new = open(newf, encoding='utf-8').read()
if s.count(old) != 1:
    sys.exit(3)
open(path, 'w', encoding='utf-8').write(s.replace(old, new, 1))
PY
	then
		echo "FAIL[事前]: 注入点不存在或不唯一 —— 这项突变没生效，后面的绿是假的"
		fail=1
		return 1
	fi
	return 0
}

assert_red() { # <名字> <尺子脚本> <期望的 FAIL 标签>
	local name="$1" check="$2" want="$3" rc=0
	OUT="$(bash "$check" 2>&1)" || rc=$?
	if [ -z "$OUT" ]; then
		echo "FAIL[$name]: 尺子没有任何输出（崩了？）—— 崩溃红不算红"
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

assert_green() { # <名字> <尺子脚本>
	local name="$1" check="$2" rc=0
	OUT="$(bash "$check" 2>&1)" || rc=$?
	if [ "$rc" -ne 0 ]; then
		echo "FAIL[$name]: 还原了却不绿 —— 注入残留或尺子本身坏了"
		grep -E '^FAIL' <<<"$OUT" | sed 's/^/    /'
		fail=1
		return
	fi
	echo "$name 复跑：绿 ✓"
}

TRUST="$HERE/test_install_trust_precheck.sh"
URL="$HERE/test_install_public_url.sh"
TZCHK="$HERE/test_install_timezone.sh"

echo "── 基准（未注入，三把尺子各自跑一遍）──"
assert_green "信任预检" "$TRUST"
assert_green "对外地址" "$URL"
assert_green "时区" "$TZCHK"

# ── P-tls1：坏路径被静默忽略（配了 CA 但文件没拷过来）─────────────────
echo
echo "── P-tls1：SKILLFORGE_CA_BUNDLE 里的路径不存在时不再报警，直接报「已在位」──"
cat > "$WORK/t1.old" <<'EOF'
		if [ ! -f "$trust_one" ]; then
			trust_missing="$trust_missing $trust_one"
		fi
EOF
cat > "$WORK/t1.new" <<'EOF'
		if false; then
			trust_missing="$trust_missing $trust_one"
		fi
EOF
if inject "P-tls1" "$WORK/t1.old" "$WORK/t1.new"; then
	assert_red "P-tls1" "$TRUST" "B"
fi
cp "$BAK" "$INSTALL_SH"

# ── P-tls2：没有系统根证书时不再提示（内网离线机的重灾区）──────────────
echo
echo "── P-tls2：本机没有系统根证书（ca-certificates）时保持沉默 ──"
cat > "$WORK/t2.old" <<'EOF'
		trust_warn=1
		c_warn "本机既没有系统根证书（ca-certificates），也没配 SKILLFORGE_CA_BUNDLE。"
EOF
cat > "$WORK/t2.new" <<'EOF'
		:
		:
EOF
if inject "P-tls2" "$WORK/t2.old" "$WORK/t2.new"; then
	assert_red "P-tls2" "$TRUST" "C"
fi
cp "$BAK" "$INSTALL_SH"

# ── P-url：客户显式给的 --public-url 被自动探测覆盖（历史 bug 的写法）────
echo
echo "── P-url：客户显式给的 --public-url 被内网 IP 探测覆盖（反代/域名场景直接坏）──"
cat > "$WORK/u.old" <<'EOF'
if [ -z "$PUBLIC_URL" ]; then
EOF
cat > "$WORK/u.new" <<'EOF'
if true; then
EOF
if inject "P-url" "$WORK/u.old" "$WORK/u.new"; then
	assert_red "P-url" "$URL" "P3"
fi
cp "$BAK" "$INSTALL_SH"

# ── P-tz：探测到了时区却不写进 env（等于白探测，时间还是错 8 小时）────
echo
echo "── P-tz：探测到时区却不写进 env（客户以为时间对了，其实还在 UTC）──"
# 注：锚必须落在**整行**上。曾经写成「if 那行 + 下一行 printf 的前半句」，
# 而 heredoc 收尾必然补一个换行 —— 半行锚永远匹配不上，
# 症状就是这里报「注入点不存在」。半行锚要配 printf '%s' 才成立，不值得。
cat > "$WORK/z.old" <<'EOF'
	if [ -n "$TZ_DETECTED" ]; then
EOF
cat > "$WORK/z.new" <<'EOF'
	if false; then
EOF
if inject "P-tz" "$WORK/z.old" "$WORK/z.new"; then
	assert_red "P-tz" "$TZCHK" "Z4c"
fi
cp "$BAK" "$INSTALL_SH"

echo
echo "── 收尾：还原后必须回绿，且与备份逐字节一致 ──"
assert_green "收尾·信任预检" "$TRUST"
assert_green "收尾·对外地址" "$URL"
assert_green "收尾·时区" "$TZCHK"
if ! cmp -s "$BAK" "$INSTALL_SH"; then
	echo "FAIL[收尾]: install.sh 与备份不一致（还原不干净）"
	fail=1
fi

[ "$fail" -eq 0 ] || { echo "安装期预检突变自证：失败"; exit 1; }
echo "安装期预检突变自证：通过"
