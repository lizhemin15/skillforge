#!/usr/bin/env bash
# 自检「时区 / 时间」栏的离线终验 —— 在**没有系统时区库**的机器上做双向对照：
#   · 出货二进制（内嵌 IANA tzdata）→ 这一栏必须 OK（时间不会偏 8 小时）
#   · 剥离内嵌 tzdata 的对照版      → 同一环境下必须**精确转红**并给出 tzdata 修法
#
# 为什么必须双向：单向只能证明「某一版在某个环境下绿」。
# 只有「同环境、旧红新绿」才能同时证明 (a) 注入的是真故障，(b) 尺子抓得住它。
#
# 敌对环境的造法：unshare -m + 把 /usr/share/zoneinfo 等目录 bind 成空目录，
# 再把 ZONEINFO 指向空目录 —— 这就是最小化安装/CentOS 精简版/scratch 容器的真实形态。
# 不碰生产目录，退出命名空间即恢复。
#
# 退出码：0 = 双向对照符合预期；1 = 不符；0 且打印 SKIP = 环境做不到（SKIP ≠ PASS）。
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="${REPO:-$HERE/../../..}"

if [ "$(id -u)" != "0" ] || ! command -v unshare >/dev/null 2>&1 || ! unshare -m true >/dev/null 2>&1; then
	echo "SKIP: 需要 root + unshare 挂载命名空间才能造「没有时区库」的机器"
	exit 0
fi
# go 工具链：PATH 里第一个不一定够新（本机 /usr/bin/go 就是 1.18，编不动 go.mod 写 1.25 的仓库）。
# 所以按「能用」挑，而不是按「先找到」挑 —— 否则这个测试会在有的环境里静默变成编译失败，
# 让人以为是时区功能坏了。挑不到就 SKIP 并说明（SKIP ≠ PASS）。
GO_BIN=""
for _c in "${GO:-}" "$(command -v go 2>/dev/null)" /usr/local/go/bin/go /usr/local/go1.25/bin/go; do
	[ -n "$_c" ] && [ -x "$_c" ] || continue
	case "$("$_c" version 2>/dev/null | awk '{print $3}')" in
		go1.2[5-9]*|go1.[3-9][0-9]*) GO_BIN="$_c"; break ;;
	esac
done
if [ -z "$GO_BIN" ]; then
	echo "SKIP: 找不到 go >= 1.25 的工具链（本仓库 go.mod 要求 1.25），用 GO=/path/to/go 指定后重跑"
	exit 0
fi
echo "工具链：$GO_BIN $("$GO_BIN" version)"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
EMPTY="$WORK/empty"
mkdir -p "$EMPTY"

# ── 出货版 ────────────────────────────────────────────────────────────
( cd "$REPO" && "$GO_BIN" build -o "$WORK/shipping" ./cmd/server ) || { echo "FAIL: 出货二进制编译失败"; exit 1; }

# ── 对照版：只删掉内嵌 tzdata 那一行，其它一字不动 ─────────────────────
mkdir -p "$WORK/src"
tar -C "$REPO" --exclude=.git --exclude=node_modules -cf - . | tar -C "$WORK/src" -xf -
python3 - "$WORK/src/cmd/server/main.go" <<'PY' || exit 1
import sys, re
p = sys.argv[1]
s = open(p, encoding='utf-8').read()
before = s.count('_ "time/tzdata"')
if before != 1:
    print(f"FAIL[事前]: main.go 里 time/tzdata 的注入点应恰好 1 处，实际 {before} 处 —— "
          f"注入点没了，这个对照会变成空跑")
    sys.exit(1)
# 连同注释一起删掉，留下合法的 import 块（必须是「合法但行为退化」，不能是崩溃红）
s = re.sub(r'\n\t// 内嵌 IANA 时区库.*?\n\t_ "time/tzdata"\n', '\n', s, flags=re.S)
assert '_ "time/tzdata"' not in s, "删除后仍存在注入点"
open(p, 'w', encoding='utf-8').write(s)
print("对照版：已剥离内嵌 tzdata")
PY
( cd "$WORK/src" && "$GO_BIN" build -o "$WORK/notzdata" ./cmd/server ) || { echo "FAIL: 对照版编译失败（必须是合法编译，不许拿崩溃当红）"; exit 1; }

# ── 敌对环境：把系统时区库全挂空，并让 ZONEINFO 也指向空目录 ───────────
HOSTILE='/tmp/sf-hostile-empty'
mkdir -p "$HOSTILE"
# 打包机/开发机上装了 Go，time.LoadLocation 会兜底到 $GOROOT/lib/time/zoneinfo.zip。
# 这一级客户机上没有，但它的存在会让「本机自检全绿」变成假绿 —— 所以敌对环境的
# 裸机形态必须连这一级一起摘掉：既把编译进二进制的默认 GOROOT 挂空，又把 GOROOT 指到不存在的路径。
GOROOT_DEFAULT="$("$GO_BIN" env GOROOT 2>/dev/null)"
run_hostile() { # $1 = 二进制；$2 = TZ 名；$3 = 1 表示「装作装了 Go 的打包机」（保留 GOROOT 兜底）
	unshare -m bash -c "
		for d in /usr/share/zoneinfo /usr/share/lib/zoneinfo /usr/lib/locale/TZ /etc/zoneinfo; do
			[ -d \"\$d\" ] && mount --bind '$HOSTILE' \"\$d\"
		done
		if [ '${3:-0}' != '1' ]; then
			[ -n '$GOROOT_DEFAULT' ] && [ -d '$GOROOT_DEFAULT/lib/time' ] &&
				mount --bind '$HOSTILE' '$GOROOT_DEFAULT/lib/time'
		fi
		if [ '${3:-0}' = '1' ]; then
			# 打包机形态：GOROOT 就是本机真实的 Go 安装目录（那里面有 zoneinfo.zip）
			env -u TZ -u GOROOT ZONEINFO='$HOSTILE' TZ='$2' '$1' -selftest
		else
			env -u TZ ZONEINFO='$HOSTILE' GOROOT='$HOSTILE/no-such-go' TZ='$2' '$1' -selftest
		fi
	" 2>&1
}

tz_line()    { grep -E '^\[[0-9]+/[0-9]+\] 时区 / 时间 ' <<<"$1" | head -1; }
tz_status()  { awk '{print $NF}' <<<"$(tz_line "$1")"; }
tz_detail()  { awk '/^\[[0-9]+\/[0-9]+\] 时区 \/ 时间 /{on=1;next} /^\[[0-9]+\/[0-9]+\] /{on=0} on' <<<"$1"; }

fail=0

# ── 第一向：出货版在「没有时区库」的机器上必须绿 ──────────────────────
out="$(run_hostile "$WORK/shipping" Asia/Shanghai)"
if ! grep -q '自检' <<<"$out"; then
	echo "FAIL[P]: 出货二进制在敌对环境下没跑起来（这不是时区问题，是本脚本的环境没搭好）"
	echo "$out" | head -5
	exit 1
fi
got="$(tz_status "$out")"
echo "出货版 · 无时区库机器 · 时区栏 = ${got:-<没找到这一栏>}"
if [ "$got" != "OK" ]; then
	echo "FAIL[P]: 内嵌时区库没起作用 —— 客户机器上时间会静默差 8 小时。详情："
	tz_detail "$out"
	fail=1
elif ! tz_detail "$out" | grep -q '内嵌'; then
	echo "FAIL[P2]: 通过了，但没说清「时区库来自二进制内嵌」—— 客户换包/换机时会再踩一次"
	tz_detail "$out"
	fail=1
fi

# ── 第一向补：装成「打包机」时不许谎报「内嵌已兜住」────────────────────
# 这一条防的是历史误判：打包机上有 Go，命中 GOROOT 里那份 zoneinfo.zip，
# 于是自检报「内嵌生效」，客户换到裸机才发现时间差 8 小时。
out_go="$(run_hostile "$WORK/shipping" Asia/Shanghai 1)"
got_go="$(tz_status "$out_go")"
det_go="$(tz_detail "$out_go")"
echo "出货版 · 假装装了 Go 的打包机 · 时区栏 = ${got_go:-<没找到这一栏>}"
if [ "$got_go" != "OK" ]; then
	echo "FAIL[G1]: 这种机器上时区本来就是能解析的，不该报红（红在别处 = 尺子错了）"
	echo "$det_go"
	fail=1
fi
if grep -q '\*\*二进制内嵌\*\*' <<<"$det_go"; then
	echo "FAIL[G2]: 命中 GOROOT 里那份 zoneinfo.zip，却报「二进制内嵌」—— 客户会以为裸机也安全"
	fail=1
fi
if ! grep -q '装了 Go' <<<"$det_go" || ! grep -q '客户机' <<<"$det_go"; then
	echo "FAIL[G3]: 没提醒「这一级只在装了 Go 的机器上有、客户机通常没有」，等于把假绿留给客户"
	echo "$det_go"
	fail=1
fi

# ── 第二向：剥离内嵌的对照版在同一环境下必须精确转红 ──────────────────
out2="$(run_hostile "$WORK/notzdata" Asia/Shanghai)"
got2="$(tz_status "$out2")"
echo "对照版 · 无时区库机器 · 时区栏 = ${got2:-<没找到这一栏>}"
if [ -z "$(tz_line "$out2")" ]; then
	echo "FAIL[N]: 对照版输出里没有「时区 / 时间」这一栏 —— 红在了别处（崩溃红不算红）"
	echo "$out2" | head -8
	exit 1
fi
if [ "$got2" != "失败" ]; then
	echo "FAIL[N]: 没有内嵌时区库 + 机器上没有 zoneinfo，这一栏竟然还是「$got2」——"
	echo "         说明这几行判定抓不住真实的静默失效，客户得自己去猜时间为什么不对。"
	fail=1
fi
det2="$(tz_detail "$out2")"
echo "对照版详情：$det2"
for want in tzdata /usr/share/zoneinfo; do
	if ! grep -q "$want" <<<"$det2"; then
		echo "FAIL[N2]: 转红了但修法里没有 $want —— 红灯不带路，等于让客户自己搜"
		fail=1
	fi
done
if ! grep -q '内嵌' <<<"$det2"; then
	echo "FAIL[N3]: 转红了但没告诉客户「换内嵌时区库的版本即可」这条离线修法"
	fail=1
fi

# ── 第三向：TZ 写错名（配置问题）与库缺失（环境问题）不能混为一谈 ──────
# 这一项不需要敌对环境：本机时区库好好的，把 TZ 写成解析不了的名字即可。
out3="$(env -u TZ TZ='Asia/nowhere' "$WORK/shipping" -selftest 2>&1)"
got3="$(tz_status "$out3")"
echo "出货版 · TZ=Asia/nowhere · 时区栏 = ${got3:-<没找到这一栏>}"
det3="$(tz_detail "$out3")"
if [ "$got3" != "失败" ]; then
	echo "FAIL[N4]: TZ 写错名时这一栏是「$got3」，客户会以为时区没问题"
	fail=1
fi
if grep -q 'tzdata 包\|apt-get install tzdata' <<<"$det3"; then
	echo "FAIL[N5]: 库是好的，却让客户去装 tzdata —— 把配置问题误诊成环境问题，客户会白折腾"
	fail=1
fi
if ! grep -q 'Asia/nowhere' <<<"$det3"; then
	echo "FAIL[N6]: 没点名客户自己写的那个值，他找不到改哪里"
	fail=1
fi

[ "$fail" -eq 0 ] || { echo "时区自检终验：失败"; exit 1; }
echo "时区自检终验：双向对照通过（出货版绿 / 剥离内嵌版精确转红）"
