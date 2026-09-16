#!/usr/bin/env bash
# install.sh「时区」处理的验收 —— 从**出货文件**里抠真代码跑，不重抄实现。
#
# 为什么值得一把尺子：时区坏了不报错，只让时间偏 8 小时；而安装脚本一旦
# 把「本机就是 UTC」也当成「已配置」，客户看到的是一行 TZ=UTC，
# 以为已经设过了 —— 那他永远不会去改这几小时。
#
# 场景：
#   Z1. 环境里已有 TZ（非 UTC 等价物）→ 原样采用（装机环境/Docker 传进来的值不能被无视）
#   Z2. TZ 是 UTC 等价物 → 不许当作「已配置」（要留注释好让客户知道能改）
#   Z3. 完全没设 TZ → 从系统文件探测；探测不出就允许为空，但不许输出垃圾
#   Z4. 生成的 env 里必须有「取消注释即用」的 TZ 提示行（否则客户不知道怎么改）
#
# 退出码：0 = 全对；非 0 = 有场景不符。
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
INSTALL_SH="${INSTALL_SH:-$HERE/../install.sh}"
[ -f "$INSTALL_SH" ] || { echo "SKIP: 找不到 $INSTALL_SH"; exit 0; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
bash -n "$INSTALL_SH" || { echo "FAIL[Z0]: $INSTALL_SH 语法不过关"; exit 1; }

# ── 抠 detect_timezone ────────────────────────────────────────────────
awk '/^detect_timezone\(\) \{$/ { on = 1 } on { print } on && /^\}$/ { exit }' \
	"$INSTALL_SH" > "$TMP/tz.sh"
if ! grep -q 'zoneinfo' "$TMP/tz.sh"; then
	echo "FAIL[Z1a]: 抠不出 detect_timezone —— 函数改名/改位置了，这个测试会测到空气"
	exit 1
fi

# ── 抠「写进 env 的那段」 ─────────────────────────────────────────────
awk "/printf '# ---- 时间 ----/ { on = 1 } on { print } on && /^\tfi\$/ { exit }" \
	"$INSTALL_SH" > "$TMP/envblock.sh"
if ! grep -q 'TZ=' "$TMP/envblock.sh"; then
	echo "FAIL[Z1b]: 抠不出 env 时间段（# ---- 时间 ---- … fi）"
	exit 1
fi

# ── 抠「装完给客户打印时区」那段 ──────────────────────────────────────
# 客户看完安装输出就该知道「我的时间对不对，不对该改哪一行」。
# 如果只写进 env 而不打印，客户在装完之后完全无从得知要不要动这个值。
awk '/^if \[ -n "\$TZ_DETECTED" \]; then$/ { on = 1 } on { print } on && /^fi$/ { exit }' \
	"$INSTALL_SH" > "$TMP/report.sh"
if ! grep -q 'c_ok' "$TMP/report.sh" || ! grep -q 'c_info' "$TMP/report.sh"; then
	echo "FAIL[Z1c]: 抠不出「装完打印时区」那段 —— 这段没了客户就不知道时间对不对"
	exit 1
fi

run_tz() { # $1 = TZ 的值（"" = 完全清掉）
	{ cat "$TMP/tz.sh"; echo 'printf "RESULT=%s\n" "$(detect_timezone)"'; } > "$TMP/run.sh"
	if [ -z "$1" ]; then
		env -u TZ bash "$TMP/run.sh"
	else
		TZ="$1" bash "$TMP/run.sh"
	fi
}

fail=0

# ── Z1：环境已有 TZ → 原样采用 ────────────────────────────────────────
out="$(run_tz "Asia/Shanghai")"
echo "Z1 输出：$out"
if ! grep -qF 'RESULT=Asia/Shanghai' <<<"$out"; then
	echo "FAIL[Z1]: 环境里显式给的 TZ 没被采用（容器/装机环境传进来的时区被无视）"
	fail=1
fi

# ── Z2：UTC 等价物 → 不算「已配置」 ───────────────────────────────────
for utcname in UTC Etc/UTC GMT Etc/GMT; do
	out="$(run_tz "$utcname")"
	echo "Z2($utcname) 输出：$out"
	if ! grep -qF 'RESULT=' <<<"$out" || [ "$(sed -n 's/^RESULT=//p' <<<"$out")" != "" ]; then
		echo "FAIL[Z2]: TZ=$utcname 不是有效时区配置，不该被当成已设（会让客户以为时间已经对了）"
		fail=1
	fi
done

# ── Z3：没设 TZ → 允许探测/为空，但不许垃圾 ──────────────────────────
out="$(run_tz "")"
got="$(sed -n 's/^RESULT=//p' <<<"$out")"
echo "Z3 输出：RESULT=[$got]"
if [ -n "$got" ] && ! grep -Eq '^[A-Za-z_]+(/[A-Za-z0-9_+.-]+)*$' <<<"$got"; then
	echo "FAIL[Z3]: 探测出来的时区名不是合法 IANA 名字（写进 env 会让服务解析不了时区）"
	fail=1
fi

# ── Z3b（有条件）：用假 /etc/timezone 验证「真读系统文件」──────────────
# 需要 root + 能 unshare 挂载命名空间；做不到就 SKIP（SKIP ≠ PASS，会明说）。
if [ "$(id -u)" = "0" ] && command -v unshare >/dev/null 2>&1 && \
	unshare -m true >/dev/null 2>&1; then
	: > "$TMP/tzfile"
	printf 'Asia/Kolkata\n' > "$TMP/tzfile"
	out="$(env -u TZ TMP="$TMP" FAKE_TZFILE="$TMP/tzfile" \
		unshare -m bash -c "mount --bind '$TMP/tzfile' /etc/timezone && . '$TMP/tz.sh' && { declare -F detect_timezone >/dev/null || { echo NOSOURCE; exit 3; }; } && printf 'RESULT=%s\n' \"\$(detect_timezone)\"" 2>/dev/null)"
	echo "Z3b 输出：$out"
	if grep -qF 'NOSOURCE' <<<"$out"; then
		echo "FAIL[Z3b]: 抠出来的函数段 source 不进 shell —— 测试脚本自身失效，不许当通过"
		fail=1
	elif [ -z "$out" ]; then
		echo "SKIP[Z3b]: 环境不支持 unshare 挂载命名空间，跳过「真读 /etc/timezone」这一条"
	elif ! grep -qF 'RESULT=Asia/Kolkata' <<<"$out"; then
		echo "FAIL[Z3b]: 有 /etc/timezone 却没读它（探测逻辑坏了，客户机器上的时区不会被采纳）"
		fail=1
	fi
else
	echo "SKIP[Z3b]: 需要 root + unshare 才能造假 /etc/timezone"
fi

# ── Z4：env 里得有「取消注释即用」的提示 ─────────────────────────────
{
	echo 'TZ_DETECTED=""'
	cat "$TMP/envblock.sh"
} > "$TMP/env_utc.sh"
out_utc="$(bash "$TMP/env_utc.sh")"
echo "Z4(UTC) 输出：$(tr '\n' '|' <<<"$out_utc")"
if ! grep -q 'TZ=Asia/Shanghai' <<<"$out_utc"; then
	echo "FAIL[Z4]: 探测不出时区时没有给出可抄的 TZ 提示，客户不知道该怎么改"
	fail=1
fi
if grep -qE '^TZ=' <<<"$out_utc"; then
	echo "FAIL[Z4b]: 探测不出时区却写了 TZ=（等于谎报本机时区）"
	fail=1
fi

{
	echo 'TZ_DETECTED="Asia/Shanghai"'
	cat "$TMP/envblock.sh"
} > "$TMP/env_cn.sh"
out_cn="$(bash "$TMP/env_cn.sh")"
echo "Z4(CN) 输出：$(tr '\n' '|' <<<"$out_cn")"
if ! grep -qE '^[[:space:]]*TZ=Asia/Shanghai' <<<"$out_cn"; then
	echo "FAIL[Z4c]: 探测到时区却没写进 env（等于白探测，时间还是错 8 小时）"
	fail=1
fi
# 服务读的是 KEY=VALUE，行首多出空白会让 systemd 认不出来
if grep -qE '^[[:space:]]+TZ=' <<<"$out_cn"; then
	echo "FAIL[Z4d]: TZ 行有前导空白，systemd 的 EnvironmentFile 不吃"
	fail=1
fi

# ── Z5：装完的提示必须能让客户自己判断/动手 ──────────────────────────
run_report() { # $1 = TZ_DETECTED 的值
	{
		echo "ENV_FILE=/opt/skillforge/skillforge.env"
		printf 'TZ_DETECTED=%s\n' "$1"
		# 出货环境里这两个函数由 install.sh 定义，这里只留「能看出打印了什么」的最小替身
		echo 'c_ok() { printf "OK| %s\n" "$*"; }'
		echo 'c_info() { printf "i| %s\n" "$*"; }'
		cat "$TMP/report.sh"
	} > "$TMP/report_run.sh"
	bash "$TMP/report_run.sh"
}

rep_cn="$(run_report Asia/Shanghai)"
echo "Z5(有时区) 输出：$(tr '\n' '|' <<<"$rep_cn")"
if ! grep -q 'Asia/Shanghai' <<<"$rep_cn"; then
	echo "FAIL[Z5a]: 装完没告诉客户当前生效的时区名，他没法确认时间对不对"
	fail=1
fi
if grep -q '早 8 小时' <<<"$rep_cn"; then
	echo "FAIL[Z5b]: 时区已经探测对了，还在吓唬客户「会早 8 小时」——提示贬值成噪音"
	fail=1
fi

rep_utc="$(run_report '')"
echo "Z5(无时区) 输出：$(tr '\n' '|' <<<"$rep_utc")"
for want in '早 8 小时' 'TZ=Asia/Shanghai' 'skillforge.env' '重启'; do
	if ! grep -qF "$want" <<<"$rep_utc"; then
		echo "FAIL[Z5c]: 时区没配好时，装完的提示里缺「$want」——客户知道时间不对，但不知道改哪儿"
		fail=1
	fi
done

[ "$fail" -eq 0 ] || { echo "时区处理验收：失败"; exit 1; }
echo "时区处理验收：通过"
