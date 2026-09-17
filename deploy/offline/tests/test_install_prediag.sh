#!/usr/bin/env bash
#
# test_install_prediag.sh —— 装前体检 prediag_gate 的行为验收（正向）
#
# 为什么测这个函数而不是「跑一遍 install.sh」：install.sh 会写 /etc/systemd、装文件、
# 起服务 —— 在 CI 里跑不动，于是最该被盯住的那段判断反而没人测。这里用仓库既有做法：
# **从出货文件里把真函数抠出来跑**（awk 按标记抽），不在测试里另写一份实现 ——
# 复制品迟早和本体分叉，那时测试绿、出货错。
#
# 覆盖的行为面（每条都对应一个客户真会撞上的场景）：
#   S1 体检干净        → 继续装（rc=0），且**体检原文必须打给客户**（不然等于没体检）
#   S2 体检有问题      → 停在动文件之前（rc≠0），且给出两条可选路径
#   S3 有问题 + --no-ocr        → 继续（本次不装解析服务，不受影响）
#   S4 有问题 + --allow-degraded → 继续，但必须说清「这不是修复」
#   S5 老包（不支持 -diag）      → 继续（把老包判成「装不了」是尺子的缺陷，不是客户的问题）
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
INSTALL_SH="$HERE/../install.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fail=0
OUT=""
RC=0

say_fail() { echo "FAIL[$1]: $2"; fail=1; }
say_ok()   { echo "ok  [$1] $2"; }

# ---------- 0. 事前断言：抽不到真函数就必须红（否则后面所有绿的都无意义）----------
awk '/^# >>> prediag_gate/{f=1} f{print} /^# <<< prediag_gate/{f=0}' "$INSTALL_SH" > "$WORK/gate.sh"
if [ ! -s "$WORK/gate.sh" ]; then
	echo "FAIL[事前]: 没能从 install.sh 里抠出 prediag_gate（标记 # >>> prediag_gate 被删了？）"
	exit 1
fi
if ! grep -q 'prediag_gate()' "$WORK/gate.sh"; then
	echo "FAIL[事前]: 抽出来的片段里没有 prediag_gate 函数定义"
	exit 1
fi

# ---------- 造三种假的 bin/skillforge ----------
cat > "$WORK/sf-ok" <<'EOF'
#!/bin/sh
echo "SkillForge 基座体检（离线：不联网、不调用模型）"
echo "glibc   : 2.17"
echo "── 解析服务 ──"
echo "  解析服务与包内基线一致，可用"
exit 0
EOF
cat > "$WORK/sf-bad" <<'EOF'
#!/bin/sh
echo "SkillForge 基座体检（离线：不联网、不调用模型）"
echo "glibc   : 2.17（这份包要求 ≥2.28）"
echo "✗ 解析服务在这台机器上起不来：包内基线要求 glibc 2.28，本机 2.17"
echo "  修法：换一份与本机匹配的离线包，或本次加 --no-ocr"
exit 1
EOF
cat > "$WORK/sf-old" <<'EOF'
#!/bin/sh
echo "flag provided but not defined: -diag"
exit 2
EOF
chmod +x "$WORK"/sf-ok "$WORK"/sf-bad "$WORK"/sf-old

# 事前断言：假件必须真的按预期退出 —— 假件本身错了，后面的红绿都是假的
"$WORK/sf-ok"  >/dev/null || { echo "FAIL[事前]: 假件 sf-ok 没有返回 0"; exit 1; }
"$WORK/sf-bad" >/dev/null && { echo "FAIL[事前]: 假件 sf-bad 竟然返回 0"; exit 1; }
[ "$("$WORK/sf-old" >/dev/null 2>&1; echo $?)" = "2" ] || { echo "FAIL[事前]: 假件 sf-old 的退出码不是 2"; exit 1; }

# ---------- 跑真函数：在子壳里把 install.sh 的输出小工具补齐，然后 source 抽出来的片段 ----------
run_gate() { # <fake> <DO_OCR> <ALLOW_DEGRADED>
	local fake="$1" do_ocr="$2" allow="$3"
	RC=0
	OUT="$(DO_OCR="$do_ocr" ALLOW_DEGRADED="$allow" bash -c '
		set -uo pipefail
		c_info() { printf "  %s\n" "$*"; }
		c_ok()   { printf "  ok %s\n" "$*"; }
		c_warn() { printf "  ! %s\n" "$*"; }
		c_fail() { printf "  X %s\n" "$*"; }
		step()   { printf "[%s] %s\n" "$1" "$2"; }
		die()    { c_fail "$*"; exit 1; }
		source "$1"
		prediag_gate "$2"
	' _ "$WORK/gate.sh" "$fake" 2>&1)" || RC=$?
}

echo "== S1 体检干净：继续装，且体检原文必须打给客户 =="
run_gate "$WORK/sf-ok" 1 0
if [ "$RC" -ne 0 ]; then say_fail "S1" "体检干净却拦住了安装（rc=$RC）"; else say_ok "S1" "rc=0，继续安装"; fi
if grep -q 'glibc   : 2.17' <<<"$OUT"; then
	say_ok "S1b" "体检原文转发给了客户"
else
	say_fail "S1b" "体检原文没打出来（客户看不到结论，等于没体检）"
fi

echo "== S2 体检有问题：停在动文件之前，并给出两条路径 =="
run_gate "$WORK/sf-bad" 1 0
if [ "$RC" -ne 1 ]; then say_fail "S2" "体检有问题却没有拦住安装（rc=$RC，期望 1）"; else say_ok "S2" "rc=1，停在装前"; fi
for want in '两条可选路径' '--no-ocr' '--allow-degraded'; do
	if grep -q -- "$want" <<<"$OUT"; then
		say_ok "S2b" "给出「$want」"
	else
		say_fail "S2b" "拦住之后没说「$want」—— 客户只知道自己被拦了，不知道下一步干嘛"
	fi
done

echo "== S3 有问题 + --no-ocr：本次不装解析服务，不受影响 → 继续 =="
run_gate "$WORK/sf-bad" 0 0
if [ "$RC" -ne 0 ]; then say_fail "S3" "本次 --no-ocr 却仍被拦住（rc=$RC）"; else say_ok "S3" "rc=0，继续安装"; fi
if grep -q '不受影响' <<<"$OUT"; then say_ok "S3b" "说明了本次不受影响"; else say_fail "S3b" "没说清「本次 --no-ocr 不受影响」"; fi

echo "== S4 有问题 + --allow-degraded：必须说清「这不是修复」=="
run_gate "$WORK/sf-bad" 1 1
if [ "$RC" -ne 0 ]; then say_fail "S4" "--allow-degraded 却被拦住（rc=$RC）"; else say_ok "S4" "rc=0，按显式放行继续"; fi
if grep -q '这不是修复' <<<"$OUT"; then
	say_ok "S4b" "点明「这不是修复」"
else
	say_fail "S4b" "放行了却没说「这不是修复」—— 客户会以为问题被解决了"
fi

echo "== S5 老包（不支持 -diag）：不许判成故障 =="
run_gate "$WORK/sf-old" 1 0
if [ "$RC" -ne 0 ]; then say_fail "S5" "老包被当成了故障（rc=$RC）：把尺子的缺陷算到客户头上"; else say_ok "S5" "rc=0，老包照装"; fi
if grep -q '老包' <<<"$OUT"; then say_ok "S5b" "如实说明是老包、跳过体检"; else say_fail "S5b" "没说明为什么跳过体检"; fi

echo
if [ "$fail" -eq 0 ]; then
	echo "装前体检 prediag_gate：全部通过"
	exit 0
fi
echo "装前体检 prediag_gate：有失败项"
exit 1
