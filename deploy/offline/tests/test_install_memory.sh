#!/usr/bin/env bash
#
# test_install_memory.sh —— 「装前内存检查」这一栏的验收
#
# 守的是三件事：
#   ① 内存紧的机器上，安装输出必须把「这台机器内存不够」和「怎么办」说清楚 ——
#      因为客户看到的现象是「扫描件时好时坏」，没人会联想到内存；
#   ② cgroup 限额必须参与判定 —— 容器里 /proc/meminfo 显示的是宿主机内存，
#      只看它会把 512MB 的容器判成「充裕」，恰好漏掉最容易 OOM 的那一类；
#   ③ 只警告不拦 —— 内存紧的机器仍然能用（纯文字写作不碰 OCR），
#      硬拦会把本来可用的客户挡在门外；探测不到时也不能当成失败。
#
# 断言方式：把 install.sh 里的 mem_limit_mb / check_memory 真函数抠出来跑，
# 用假 /proc/meminfo + 假 cgroup 文件造前提（不依赖本机真实内存，也不碰真 /sys）。
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
INSTALL_SH="${INSTALL_SH:-$HERE/../install.sh}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

bash -n "$INSTALL_SH" || { echo "FAIL[M0]: $INSTALL_SH 语法不过关"; exit 1; }

# ── 抠真函数：mem_limit_mb / check_memory ─────────────────────────────
for fn in mem_limit_mb check_memory; do
	awk -v fn="$fn" '$0 == fn "() {" { on = 1 } on { print } on && /^}$/ { exit }' \
		"$INSTALL_SH" > "$TMP/$fn.sh"
	if ! grep -q . "$TMP/$fn.sh"; then
		echo "FAIL[M0]: 抠不出 $fn() —— 函数改名/改位置了，这测试会测到空气"
		exit 1
	fi
done
if ! grep -q 'MEM_COMFY_MB' "$INSTALL_SH"; then
	echo "FAIL[M0]: install.sh 里找不到内存阈值常量 —— 阈值没了，下面的断言全是空跑"
	exit 1
fi
# 光有函数不够：这里自己会调 check_memory，所以「安装流程里根本没调用它」这种情况
# 下面的断言照样全绿 —— 而客户装的时候确实一次都没跑过。必须盯住顶层调用。
if ! grep -qE '^check_memory$' "$INSTALL_SH"; then
	echo "FAIL[M0]: install.sh 里没有顶层调用 check_memory —— 函数在，装的时候却不跑"
	exit 1
fi

run_check() { # $1 = MemTotal 行（或空 = 文件不可读）；$2 = cgroup v2 内容（或空 = 无该文件）；$3 = cgroup v1 内容
	local mi="$TMP/meminfo"
	if [ -n "$1" ]; then
		printf '%s\n' "$1" > "$mi"
	else
		rm -f "$mi" # 路径不存在 = 探测不到
	fi
	local cg2="$TMP/cg2" cg1="$TMP/cg1"
	[ -n "$2" ] && printf '%s\n' "$2" > "$cg2" || rm -f "$cg2"
	[ -n "$3" ] && printf '%s\n' "$3" > "$cg1" || rm -f "$cg1"

	{
		# install.sh 里的输出函数与阈值常量的真身在这里补上（它们不属于被测逻辑）
		awk '/^MEM_(MIN|COMFY)_MB=/ { print }' "$INSTALL_SH"
		echo 'fmt_c() { printf "%s| %s\\n" "$1" "$2"; }'
		echo 'c_ok()   { fmt_c OK  "$*"; }'
		echo 'c_warn() { fmt_c WARN "$*"; }'
		echo 'c_info() { fmt_c INFO "$*"; }'
		echo 'die()    { fmt_c DIE "$*"; exit 9; }'
		cat "$TMP/mem_limit_mb.sh" "$TMP/check_memory.sh"
		echo 'check_memory'
		echo 'echo "RC=$?"'
	} > "$TMP/run.sh"
	SKILLFORGE_MEMINFO="$mi" \
		SKILLFORGE_CGROUP_MEMORY_MAX="$cg2" \
		SKILLFORGE_CGROUP_MEMORY_LIMIT="$cg1" \
		SERVICE_NAME=skillforge \
		bash "$TMP/run.sh" 2>&1
}

fail=0
expect() { # $1 = 条件描述；$2 = 命中则为 0 的 grep 表达式；$3 = 输出
	if ! grep -qE "$2" <<<"$3"; then
		echo "FAIL[$1]"
		echo "$3" | sed 's/^/    /'
		fail=1
	fi
}
reject() { # 反向断言：不该出现的东西出现了
	if grep -qE "$2" <<<"$3"; then
		echo "FAIL[$1]"
		echo "$3" | sed 's/^/    /'
		fail=1
	fi
}

# ── M1：内存充裕（8GB，无 cgroup 限额）→ 绿灯，不许吓唬人 ──────────────
out="$(run_check 'MemTotal:        8192000 kB' 'max' '')"
echo "M1 输出：$(tr '\n' '|' <<<"$out")"
expect "M1a" '^OK\| 内存：上限约 7\.8 GB' "$out"
reject "M1b" 'WARN' "$out"
expect "M1c" 'RC=0' "$out"

# ── M2：偏紧（1.5GB）→ 黄字提醒，但不能出现 swap 命令级别的催促 ────────
out="$(run_check 'MemTotal:        1536000 kB' 'max' '')"
echo "M2 输出：$(tr '\n' '|' <<<"$out")"
expect "M2a" 'WARN\| 内存偏紧：本机上限约 1500 MB' "$out"
expect "M2b" '批量导入' "$out"
reject "M2c" 'fallocate' "$out" # 1.5GB 还没到「必须加 swap」的地步，说多了就是噪音
expect "M2d" 'RC=0' "$out"

# ── M3：偏小（900MB）→ 必须说清现象 + 给出离线可做的修法 ───────────────
out="$(run_check 'MemTotal:         900000 kB' 'max' '')"
echo "M3 输出：$(tr '\n' '|' <<<"$out")"
expect "M3a" 'WARN\| 内存偏小：本机上限约 878 MB' "$out"
# 客户看不懂「内存不足」，得把现象说成他实际看到的东西
expect "M3b" '时好时坏' "$out"
# 修法必须离线可执行：fallocate/mkswap/swapon 都不需要联网
expect "M3c" 'fallocate -l 4G /swapfile.*mkswap.*swapon' "$out"
# 加 swap 不写 fstab = 重启就没了，客户会以为「时好时坏」又复发
expect "M3d" '/etc/fstab' "$out"
# 红/黄灯都要带路：怎么确认自己确实被 OOM 了
expect "M3e" 'killed process' "$out"
# 修法里不许出现联网命令（内网机器装不了包，给了等于没给）
reject "M3f" 'apt-get install|dnf install|yum install' "$out"
expect "M3g" 'RC=0' "$out"

# ── M4：容器里物理 16GB 但 cgroup 只给 800MB → 必须按 800MB 判 ─────────
out="$(run_check 'MemTotal:       16384000 kB' '838860800' '')"
echo "M4 输出：$(tr '\n' '|' <<<"$out")"
expect "M4a" 'WARN\| 内存偏小：本机上限约 800 MB' "$out"
# 这条是关键：只看 /proc/meminfo 的实现会在这里报「上限约 16 GB」的绿灯
reject "M4b1x" '内存：上限约 16 GB' "$out"

# ── M4b：cgroup v1 的「无限制」是天文数字，不是限额 ────────────────────
out="$(run_check 'MemTotal:        8192000 kB' '' '9223372036854771712')"
echo "M4b 输出：$(tr '\n' '|' <<<"$out")"
expect "M4b1" '^OK\| 内存：上限约 7\.8 GB' "$out"

# ── M5：探测不到 → 跳过，不算失败（「未知」不等于「有问题」）──────────
out="$(run_check '' '' '')"
echo "M5 输出：$(tr '\n' '|' <<<"$out")"
expect "M5a" '^INFO\| 内存：探测不到上限' "$out"
reject "M5b" 'WARN|DIE' "$out"
expect "M5c" 'RC=0' "$out"

# ── M6：任何档位都不得阻断安装（OOM 风险是「提示」，不是「拒绝安装」）──
for spec in 'MemTotal:        8192000 kB|max|' 'MemTotal:        1536000 kB|max|' 'MemTotal:         900000 kB|max|' 'MemTotal:         200000 kB|max|'; do
	IFS='|' read -r mi cg2 cg1 <<<"$spec"
	out="$(run_check "$mi" "$cg2" "$cg1")"
	if ! grep -q 'RC=0' <<<"$out"; then
		echo "FAIL[M6]: 内存 ${mi#MemTotal:} 时安装被拦住了 —— 内存紧的机器也能跑纯文字功能，不许拦"
		echo "$out" | sed 's/^/    /'
		fail=1
	fi
	reject "M6" 'DIE' "$out"
done

[ "$fail" -eq 0 ] || { echo "装前内存检查验收：失败"; exit 1; }
echo "装前内存检查验收：通过"
