#!/usr/bin/env bash
#
# install.sh 第 6 段「装后服务探测」的正向 + 负向自证。
#
# 为什么要这么测：
#   「端口在听就算装好了」正是 2026-09-15 线上事故的判定方式 —— ocrd 进程活着、端口听着，
#   但抽字全废，安装过程一路绿。所以这段判定逻辑必须有两组证据：
#     正向：健康服务 → 绿、退出码 0；
#     负向：僵尸服务（200 但有 ok:false）/ 端口不通 / 主服务 500 → 红、退出码非 0，
#           且必须打印出**预期的那条** FAIL 文案（不是崩溃造成的「红」）。
#
# 怎么保证测的是真代码：
#   本脚本不重写探测逻辑，而是从 deploy/offline/install.sh 里把第 6 段**原样抠出来**跑
#   （只替掉环境：PREFIX/HERE/端口号 + 一个假的 journalctl），并把脚本末尾的「退出码合取」
#   段一并抠出接在后面 —— 否则断言退出码时测的是第 6 段的退出码，而不是真脚本的契约。
#   抠取位置若因改动而失配，脚本会直接报 FAIL，不会「静静地测了个空」。
#
# 另外两处专门盯过的假红（都有对应场景）：
#   · 正文结尾无换行（真 ocrd 形态）被读循环丢掉 → 场景 1 用无换行正文，场景 1b 用有换行正文；
#   · 体检脚本路径：场景 1 验证它真被装到 $PREFIX/bin，场景 6 验证包里没带时提示必须改口。
#
# 负向自证（注入真故障 → 必须由绿转红）：
#   INJECT=1  把探测条件退化回「只认 HTTP 200」—— 这就是历史故障的判定方式；
#   INJECT=2  把读循环退回「只写 while 条件」的旧写法 —— 丢掉无换行结尾的最后一行正文。
#   两者都必须让**预期的那条**场景转红、且不该红的场景保持绿。判据见文件末尾 inject_verdict()：
#   ① 必须出现 FAIL；② 红的必须属于预期场景；③ 不该红的必须仍绿（否则「什么都判红」也能骗过 ①）。
#
# 用法：
#   bash deploy/offline/tests/install_probe_test.sh          # 正向+负向场景
#   INJECT=1 bash deploy/offline/tests/install_probe_test.sh # 注入回归 → 预期按场景转红
#
# 退出码：
#   普通模式：0=全绿；1=有 FAIL 行。
#   注入模式：0=自证成功（确实按预期转红了）；1=自证失败（没红 / 红错了 / 顺带把别人也判红）。
#   —— 注入模式下「乱红一通」算失败，不算成功，否则这把尺子可以靠恒真骗过 CI。

set -uo pipefail

HERE_T="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE_T/../../.." && pwd)"
SRC="$REPO/deploy/offline/install.sh"
FAKE="$HERE_T/fake_http.py"
INJECT="${INJECT:-0}"

WORK="$(mktemp -d /tmp/sf-probe-test.XXXXXX)"
FAKE_PID=""
cleanup() {
	[ -n "$FAKE_PID" ] && kill "$FAKE_PID" 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

FAILS=0
FAIL_LINES=""
pass() { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; FAILS=$((FAILS + 1)); FAIL_LINES="$FAIL_LINES$*"$'\n'; }
info() { printf '  ---- %s\n' "$*"; }

# ---------- 1. 从 install.sh 抠出第 6 段真代码 ----------
# 起点：最后一次 `if [ "$DO_START" -eq 1 ]; then`（第 6 段，服务启动/探活）
# 终点：紧随「未启动服务，跳过端口探活」之后那个顶格 fi
BLOCK="$WORK/block.sh"
awk '
	/^if \[ "\$DO_START" -eq 1 \]; then$/ { start = NR }
	{ lines[NR] = $0 }
	END {
		if (!start) { exit 3 }
		for (i = start; i <= NR; i++) {
			print lines[i]
			if (lines[i] ~ /未启动服务，跳过端口探活/) { seen = 1; continue }
			if (seen && lines[i] == "fi") { found = 1; exit 0 }
		}
		exit 4
	}
' "$SRC" > "$BLOCK"
awk_rc=$?
if [ "$awk_rc" -ne 0 ]; then
	fail "从 install.sh 抠第 6 段失败（rc=$awk_rc）：install.sh 的结构可能变了，本测试需要同步改"
	exit 1
fi
# 抠出来的必须真的是那段带功能探测的代码，否则后面全是空跑绿
grep -q 'probe "\$OCR_PORT" /health' "$BLOCK" || {
	fail "抠出的代码里没有 probe 解析服务 /health 的调用 —— 抠取位置错了"
	exit 1
}
grep -q 'probe "\$PORT" /api/site' "$BLOCK" || {
	fail "抠出的代码里没有 probe 主服务 /api/site 的调用 —— 抠取位置错了"
	exit 1
}

# 结尾的「退出码合取」也得抠出来：第 6 段只负责把 OCR_RC 置成 1 并继续往下走，真正的
# 退出码取决于脚本末尾那几行。不抠它，测出来的退出码就永远是第 6 段的退出码（即 0），
# 断言「退出码应为 1」会一直红——那是测试的缺口，不是产品的缺口。
TAIL="$WORK/tail.sh"
awk '/^# 退出码取两者合取/{f=1} f{print}' "$SRC" > "$TAIL"
if ! grep -q 'exit "\$OCR_RC"' "$TAIL"; then
	fail "没抠到 install.sh 末尾的退出码合取段（找的是「# 退出码取两者合取」之后的内容）——安装脚本结构变了"
	exit 1
fi
info "已从 install.sh 抠出第 6 段（$(wc -l < "$BLOCK") 行）+ 退出码合取段（$(grep -c . "$TAIL") 行）"

# c_* 打印函数也照抄源码，别在测试里重写
grep -E '^c_(info|ok|warn|fail)\(\)' "$SRC" > "$WORK/colors.sh"
[ -s "$WORK/colors.sh" ] || { fail "没抠到 c_* 打印函数"; exit 1; }

# ---------- 2. 负向自证：把判定退化成历史写法 ----------
if [ "$INJECT" = "1" ]; then
	# 注入点必须真的存在，否则这次「自证」什么都没证明
	before="$(grep -c 'if http_ok "\$oh" && json_true "\$oh" ok; then' "$BLOCK")"
	if [ "$before" -ne 1 ]; then
		fail "注入点不存在（期望 1 处，实到 $before）—— 注入内容与代码失配，本次自证无效"
		exit 1
	fi
	sed -i 's|if http_ok "\$oh" \&\& json_true "\$oh" ok; then|if true; then|' "$BLOCK"
	after="$(grep -c 'if true; then' "$BLOCK")"
	[ "$after" -ge 1 ] || { fail "注入没生效"; exit 1; }
	info "已注入回归：解析服务判定退化为「只认 HTTP 200」（历史故障的判定方式）"
elif [ "$INJECT" = "2" ]; then
	# 把读循环退回「只写 while 条件」的旧写法：read 在「EOF 且无分隔符」时返回非零，
	# 于是正文结尾没有换行的 /health（= 真 ocrd 的形态）最后一行被整条丢掉。
	# 期望：场景 1（无换行）转红，场景 1b（有换行）仍然绿 —— 两侧一起才说明这把尺子
	# 卡的是「有没有换行」这个真差异，而不是「什么都判红」。
	before="$(grep -c 'while IFS= read -r -t 2 -u 3 line || \[ -n "\$line" \]; do' "$BLOCK")"
	if [ "$before" -ne 1 ]; then
		fail "注入点不存在（期望 1 处，实到 $before）—— 读循环的写法变了，本次自证无效"
		exit 1
	fi
	sed -i 's#while IFS= read -r -t 2 -u 3 line || \[ -n "$line" \]; do#while IFS= read -r -t 2 -u 3 line; do#' "$BLOCK"
	grep -q 'while IFS= read -r -t 2 -u 3 line; do' "$BLOCK" || { fail "注入没生效"; exit 1; }
	info "已注入回归：读循环退化为旧写法（丢掉「EOF 且无分隔符」的最后一行正文）"
else
	:
fi

# ---------- 3. 组装可执行脚本（环境假、代码真） ----------
compose() { # $1=主服务端口 $2=OCR 端口 $3=HERE(含/不含体检脚本)
	local port="$1" ocrport="$2" here="$3"
	# 用**引号 heredoc + 占位符替换**，不用无引号 heredoc：
	# 无引号 heredoc 会在「组装期」展开 $VAR，于是想留给运行时用的 ${OCR_RC:-unset} / $RESULT
	# 必须写成反斜杠转义的形式 —— 那种写法太容易被编辑器/补丁工具吃掉一层转义，踩过一次：
	# 转义丢了之后 ${OCR_RC} 在组装期就被展开成常量，OCR_RC 断言从此恒等于 unset（假绿）。
	cat > "$WORK/run.sh" <<'PREAMBLE'
set -euo pipefail
PREFIX="__WORK__/prefix"
HERE="__HERE__"
PORT="__PORT__"
OCR_PORT="__OCRPORT__"
SERVICE_NAME="sfprobe"
DO_START=1
# 真脚本里 DO_OCR 由 --no-ocr 或「包里没有 ocrd」决定，这里恒为 1（要测的就是它的分支）
DO_OCR=1
# 真脚本在进入第 6 段前先初始化 DOCTOR=""，这里保持一致
DOCTOR=""
SELFTEST_RC=0
# 真脚本在进入第 6 段之前就把 OCR_RC 置 0（install.sh 第 739 行），这里必须一致：
# 健康路径下第 6 段根本不会给 OCR_RC 赋值，不预置的话探针只会读到 unset。
OCR_RC=0
RESULT="__WORK__/result.txt"
trap 'printf "PROBE_OCR_RC=%s\n" "${OCR_RC:-unset}" > "$RESULT"' EXIT
PATH="__WORK__/bin:$PATH"   # 把假 journalctl 放前面，避免真 journalctl 拖慢/刷屏
PREAMBLE
	sed -i "s|__WORK__|$WORK|g; s|__HERE__|$here|g; s|__PORT__|$port|g; s|__OCRPORT__|$ocrport|g" "$WORK/run.sh"
	# 组装完先自检：占位符必须全部替换掉，且运行时才该展开的东西必须还留着 $
	if grep -q '__[A-Z]*__' "$WORK/run.sh"; then
		fail "组装 run.sh 时占位符没替换干净"
		exit 1
	fi
	grep -q 'OCR_RC:-unset' "$WORK/run.sh" || { fail "run.sh 里的 OCR_RC 探针丢失（组装期被展开了）"; exit 1; }
	cat "$WORK/colors.sh" >> "$WORK/run.sh"
	cat "$BLOCK" >> "$WORK/run.sh"
	# 真脚本的退出码语义（合取）也照抄，别在测试里重写
	cat "$TAIL" >> "$WORK/run.sh"
}
mkdir -p "$WORK/bin" "$WORK/prefix/bin"
# 假 journalctl：dump_log 只关心它有没有输出、命令提示对不对
printf '#!/bin/sh\necho "FAKE-JOURNAL: $*"\n' > "$WORK/bin/journalctl"
chmod +x "$WORK/bin/journalctl"

# 真体检脚本放进来一份，测「装到位 + 提示给全路径」这条正向路径
HERE_WITH_DOCTOR="$WORK/bundle"
HERE_NO_DOCTOR="$WORK/bundle-nodoctor"
mkdir -p "$HERE_WITH_DOCTOR" "$HERE_NO_DOCTOR"
# 源文件在仓库的 scripts/ 下；包里的布局是 install.sh 与 sf-ocr-doctor.sh 并列
# （见 scripts/build-offline-bundle.sh），所以这里也并列放。
cp "$REPO/scripts/sf-ocr-doctor.sh" "$HERE_WITH_DOCTOR/sf-ocr-doctor.sh" || {
	fail "拿不到 scripts/sf-ocr-doctor.sh，场景 1/6 的「有体检脚本」前提不成立"
	exit 1
}

free_port() {
	python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

# ---------- 4. 场景 ----------
run_case() { # $1=标签 $2=OCR模式|none $3=主服务模式|none $4=HERE $5=期望退出码 $6=必须出现的文案(正则) $7=必须不出现的文案(可空) $8=期望的 OCR_RC(可空，默认同 $5)
	local label="$1" ocrmode="$2" sitemode="$3" here="$4" want_rc="$5" want_txt="$6" not_txt="${7:-}" want_ocrrc="${8:-$5}"

	local ocrport siteport spec=""
	ocrport="$(free_port)"
	siteport="$(free_port)"
	[ "$ocrmode" != "none" ] && spec="$ocrport=$ocrmode"
	if [ "$sitemode" != "none" ]; then spec="${spec:+$spec }$siteport=$sitemode"; fi

	FAKE_PID=""
	if [ -n "$spec" ]; then
		# shellcheck disable=SC2086
		python3 "$FAKE" $spec > "$WORK/fake.log" 2>&1 &
		FAKE_PID=$!
		local i=0
		while [ "$i" -lt 40 ]; do
			grep -q '^ready ' "$WORK/fake.log" && break
			i=$((i + 1)); sleep 0.1
		done
	fi

	compose "$siteport" "$ocrport" "$here"
	rm -f "$WORK/result.txt"
	local out rc
	out="$(bash "$WORK/run.sh" 2>&1)"
	rc=$?
	local ocrrc
	ocrrc="$(sed -n 's/^PROBE_OCR_RC=//p' "$WORK/result.txt" 2>/dev/null || true)"

	[ -n "$FAKE_PID" ] && { kill "$FAKE_PID" 2>/dev/null; wait "$FAKE_PID" 2>/dev/null; FAKE_PID=""; }

	printf '%s\n' "$out" | sed 's/^/      | /'

	# 退出码与 OCR_RC 必须同时命中：只对退出码的话，「崩了」也能被当成「按预期红了」
	if [ "$rc" -eq "$want_rc" ]; then
		pass "$label：退出码 $rc（期望 $want_rc）"
	else
		fail "$label：退出码 $rc，期望 $want_rc"
	fi
	if [ "$ocrrc" = "$want_ocrrc" ]; then
		pass "$label：探测变量 OCR_RC=$ocrrc（期望 $want_ocrrc）"
	else
		fail "$label：探测变量 OCR_RC=$ocrrc，期望 $want_ocrrc"
	fi
	# 关键：必须是预期的那条 FAIL 文案。没有它就说明红得不明不白（崩溃红 / 走了别的分支）
	if printf '%s' "$out" | grep -qE "$want_txt"; then
		pass "$label：出现预期文案 /$want_txt/"
	else
		fail "$label：没出现预期文案 /$want_txt/ —— 这条红不是预期的红"
	fi
	if [ -n "$not_txt" ] && printf '%s' "$out" | grep -qE "$not_txt"; then
		fail "$label：出现了不该有的文案 /$not_txt/"
	else
		[ -n "$not_txt" ] && pass "$label：未出现 /$not_txt/"
	fi
}

echo "== 场景 1：解析服务与主服务都健康 → 应全绿、退出码 0 =="
# 用 ocr-ok：正文结尾**没有换行**，这是真 ocrd 的形态（deploy/ocr/ocrd.py: wfile.write(json.dumps(...))）。
# 曾经这里出过假红 —— 读循环在「EOF 且无分隔符」时丢掉最后一行正文，健康的 /health 被判成没回 ok。
run_case "健康(正文无结尾换行)" "ocr-ok" "site-ok" "$HERE_WITH_DOCTOR" 0 "文档解析服务功能探测通过" ""
if [ -x "$WORK/prefix/bin/sf-ocr-doctor.sh" ]; then
	pass "体检脚本已装到 \$PREFIX/bin（提示指向的路径真实存在）"
else
	fail "体检脚本没被装到 \$PREFIX/bin —— 提示会指向不存在的文件"
fi

echo "== 场景 1b：同上但正文结尾带换行 → 也必须绿（两种形态都得认） =="
run_case "健康(正文有结尾换行)" "ocr-ok-nl" "site-ok" "$HERE_WITH_DOCTOR" 0 "文档解析服务功能探测通过" ""

echo "== 场景 2：解析服务僵尸（200 但 ok:false）→ 应红、退出码 1 =="
# 这条就是历史事故形态：旧判定「端口在听」会放它过去
run_case "僵尸引擎" "ocr-zombie" "site-ok" "$HERE_WITH_DOCTOR" 1 "端口在听，但功能探测没通过" ""

echo "== 场景 3：解析服务端口完全不通 → 应红、退出码 1 =="
# wait_port 会真的等满 30 秒（60 次 × 0.5s），这是脚本原本的行为，不偷懒
run_case "端口不通" "none" "site-ok" "$HERE_WITH_DOCTOR" 1 "没监听" ""

echo "== 场景 4：端口被非 ocrd 进程占着 → 应红、退出码 1 =="
run_case "冒名顶替" "ocr-impostor" "site-ok" "$HERE_WITH_DOCTOR" 1 "端口在听，但功能探测没通过" ""

echo "== 场景 5：主服务端口在听但功能没好（500）→ 应红、退出码 1 =="
# 主服务探测失败是**直接 exit 1**，脚本走不到解析服务那一段 —— 所以这里 OCR_RC 应保持预置的 0，
# 期望值显式给 0：断言成 1 就成了「测试自己造出来的红」。
run_case "主服务 500" "ocr-ok" "site-500" "$HERE_WITH_DOCTOR" 1 "主服务端口在听，但功能探测没通过" "" 0

echo "== 场景 6：包里没带体检脚本 → 提示必须改口，不能指向不存在的路径 =="
# 场景 1 已经把体检脚本装进同一个 $PREFIX/bin 了，而真脚本对「上次装过」有兜底分支
# （elif [ -x "$PREFIX/bin/sf-ocr-doctor.sh" ]）。要测「新机器 + 包里也没带」这个情形，
# 必须先把上次装的清掉，否则场景前提不成立，测的是另一条分支。
rm -f "$WORK/prefix/bin/sf-ocr-doctor.sh"
run_case "无体检脚本" "ocr-zombie" "site-ok" "$HERE_NO_DOCTOR" 1 "没带 sf-ocr-doctor.sh" "sudo $HERE_NO_DOCTOR/sf-ocr-doctor.sh"

echo
# 负向自证的判据（缺一条都不算自证成功）：
#   ① 必须出现 FAIL 行（否则这把尺子对退化无感）；
#   ② 转红的必须包含**预期的那个场景**（不是别的场景碰巧红了）；
#   ③ 不该红的场景必须仍然绿（否则「什么都判红」也能骗过 ①）。
inject_verdict() { # $1=必须转红的场景标签 $2=必须仍绿的场景标签(可空)
	local must_red="$1" must_green="${2:-}"
	if [ "$FAILS" -eq 0 ]; then
		printf '\033[31m\033[1m负向自证失败\033[0m：注入回归后仍然全绿 —— 这些断言抓不到退化，等于没测\n'
		exit 1
	fi
	if ! printf '%s' "$FAIL_LINES" | grep -qF "$must_red"; then
		printf '\033[31m\033[1m负向自证失败\033[0m：红了 %d 条，但没有一条属于「%s」—— 红得不明不白，不是预期的红\n' "$FAILS" "$must_red"
		exit 1
	fi
	if [ -n "$must_green" ] && printf '%s' "$FAIL_LINES" | grep -qF "$must_green"; then
		printf '\033[31m\033[1m负向自证失败\033[0m：「%s」也被判红了 —— 说明是「什么都判红」，量的不是那个真差异\n' "$must_green"
		exit 1
	fi
	printf '\033[32m\033[1m负向自证通过\033[0m：注入回归后出现 %d 条 FAIL，且红的正是「%s」' "$FAILS" "$must_red"
	[ -n "$must_green" ] && printf '；「%s」保持绿' "$must_green"
	printf '\n'
	exit 0
}

if [ "$INJECT" = "1" ]; then
	# 判定退化为「只认 HTTP 200」→ 僵尸引擎 / 冒名顶替 / 无体检脚本 必须转红
	inject_verdict "僵尸引擎" "健康(正文无结尾换行)"
elif [ "$INJECT" = "2" ]; then
	# 读循环退化 → 只有「正文无结尾换行」的健康场景该红，「有结尾换行」的必须仍绿
	inject_verdict "健康(正文无结尾换行)" "健康(正文有结尾换行)"
fi

if [ "$FAILS" -eq 0 ]; then
	printf '\033[32m\033[1m全部场景通过\033[0m\n'
	exit 0
fi
printf '\033[31m\033[1m%d 条 FAIL\033[0m\n' "$FAILS"
exit 1
