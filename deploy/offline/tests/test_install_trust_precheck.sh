#!/usr/bin/env bash
# install.sh「TLS 信任预检」段的验收 —— 从**出货文件**里抠出真代码来跑。
#
# 为什么从 install.sh 里抠：这段的失效形态是「客户机器上一条黄字都不出来」，
# 而黄字是客户唯一能看到的线索。在测试里重抄一遍实现，就测不到出货版本里
# 真正的判断（也测不到它后来被谁改坏）。
#
# 三个场景，每个都断言「该出现的字样出现、不该出现的别乱出现」：
#   A. 配了 CA 且文件在位            → 绿字「已在位」，不吓人
#   B. 配了 CA 但路径写错/文件没拷来 → 黄字点名缺哪个路径（这是最常见的手误）
#   C. 既没系统根证书、也没配 CA     → 黄字同时给出「自签」与「机器太素」两条修法
#
# 退出码：0 = 三场景全对；非 0 = 有场景不符（CI 里就是红）。
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
INSTALL_SH="${INSTALL_SH:-$HERE/../install.sh}"
[ -f "$INSTALL_SH" ] || { echo "SKIP: 找不到 $INSTALL_SH"; exit 0; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# ── 1) 出货文件先过硬语法关 ─────────────────────────────────────────────
if ! bash -n "$INSTALL_SH"; then
	echo "FAIL[S0]: $INSTALL_SH 语法不过关"
	exit 1
fi

# ── 2) 抠出信任预检段 ──────────────────────────────────────────────────
START_MARK='# ---- 3.5 TLS 信任预检'
END_MARK='# ---------- 5. 安装 systemd 服务'
awk -v s="$START_MARK" -v e="$END_MARK" '
	index($0, s) == 1 { on = 1 }
	on && index($0, e) == 1 { on = 0 }
	on { print }
' "$INSTALL_SH" > "$TMP/block.sh"

if [ ! -s "$TMP/block.sh" ]; then
	echo "FAIL[S1]: 抠不出信任预检段（标记被改掉了？出货代码段落名：$START_MARK）"
	exit 1
fi
if ! grep -q 'SKILLFORGE_CA_BUNDLE' "$TMP/block.sh"; then
	echo "FAIL[S1b]: 抠出来的段里没有 SKILLFORGE_CA_BUNDLE —— 抠错地方了"
	exit 1
fi

# 把真代码里的系统信任库路径改写到临时目录，好让我们控制「有/没有」
sed -e "s|/etc/ssl/certs|$TMP/store|g" -e "s|/etc/pki/tls/certs|$TMP/pki|g" \
	"$TMP/block.sh" > "$TMP/block_patched.sh"
if grep -q '/etc/ssl/certs' "$TMP/block_patched.sh"; then
	echo "FAIL[S1c]: 路径改写不完整，测试会读到本机真实信任库（= 断言不可控）"
	exit 1
fi

run_case() { # $1=场景名 $2=env 内容 $3=store 存在与否(yes/no)
	local name="$1" envbody="$2" havestore="$3"
	rm -rf "$TMP/store" "$TMP/pki"
	if [ "$havestore" = "yes" ]; then
		# 注意：sed 把 /etc/ssl/certs → $TMP/store，所以真代码查的是
		# $TMP/store/ca-certificates.crt（不带 /certs 一层）。路径造错会让
		# 「有系统根证书」这个前提不成立，后续断言就全是空跑。
		# 必须有内容：真代码用 -s 判「系统根证书库是否存在」，空文件会被当成没有
		# （这正是「信任库为空」该有的判定），造前提时别忘了给字节。
		mkdir -p "$TMP/store" && echo "-----BEGIN CERTIFICATE-----" > "$TMP/store/ca-certificates.crt"
	fi
	printf '%s\n' "$envbody" > "$TMP/sf.env"
	# shellcheck disable=SC2034
	{
		echo 'set -uo pipefail'
		echo 'c_warn() { printf "WARN|%s\n" "$*"; }'
		echo 'c_ok()   { printf "OK|%s\n" "$*"; }'
		echo 'c_info() { printf "INFO|%s\n" "$*"; }'
		echo "ENV_FILE=$TMP/sf.env"
		echo "SERVICE_NAME=skillforge"
		echo "BIN=/opt/skillforge/skillforge"
		cat "$TMP/block_patched.sh"
	} > "$TMP/run.sh"
	bash "$TMP/run.sh" > "$TMP/out_$name.txt" 2>&1
	echo "$?"
}

fail=0
expect() { # $1=场景 $2=文件 $3=要有的串 $4=不该有的串(可空)
	local name="$1" f="$2" want="$3" notwant="${4:-}"
	if ! grep -qF -- "$want" "$f"; then
		echo "FAIL[$name]: 输出里没有「$want」"
		sed 's/^/    | /' "$f"
		fail=1
	fi
	if [ -n "$notwant" ] && grep -qF -- "$notwant" "$f"; then
		echo "FAIL[$name]: 不该出现「$notwant」却出现了"
		sed 's/^/    | /' "$f"
		fail=1
	fi
}

# ── 场景 A：配了 CA 且文件在位 ─────────────────────────────────────────
: > "$TMP/ca.pem"
rc=$(run_case A "SKILLFORGE_CA_BUNDLE=$TMP/ca.pem" yes)
[ "$rc" = "0" ] || { echo "FAIL[A]: 退出码 $rc（这段不该让安装失败）"; fail=1; }
expect A "$TMP/out_A.txt" "自定义 CA 已在位"
if grep -qF 'WARN|' "$TMP/out_A.txt"; then
	echo "FAIL[A]: 配置正确却出了警告 —— 客户会被无谓地吓到"
	sed 's/^/    | /' "$TMP/out_A.txt"
	fail=1
fi

# ── 场景 B：配了 CA，但路径不存在（手误/忘拷）─────────────────────────
rc=$(run_case B "SKILLFORGE_CA_BUNDLE=$TMP/nope.crt:$TMP/ca.pem" yes)
[ "$rc" = "0" ] || { echo "FAIL[B]: 退出码 $rc"; fail=1; }
expect B "$TMP/out_B.txt" "$TMP/nope.crt"
expect B "$TMP/out_B.txt" "证书校验失败"
expect B "$TMP/out_B.txt" "systemctl restart"
if grep -qF "自定义 CA 已在位" "$TMP/out_B.txt"; then
	echo "FAIL[B]: 明明有一个路径缺失，却报「已在位」—— 客户会以为配好了"
	fail=1
fi

# ── 场景 C：没有系统根证书、也没配 CA（两个子情形都要说清）───────────
rc=$(run_case C "" no)
[ "$rc" = "0" ] || { echo "FAIL[C]: 退出码 $rc"; fail=1; }
expect C "$TMP/out_C.txt" "ca-certificates"
expect C "$TMP/out_C.txt" "当前全部用 http 内网地址"
expect C "$TMP/out_C.txt" "SKILLFORGE_CA_BUNDLE"
expect C "$TMP/out_C.txt" "TLS 信任库"

# ── 场景 D：机器没有系统根证书，但客户配了自定义 CA ──────────────────
# 这条最容易被写成假红：配了 CA 不等于「所有 https 都没问题」，
# 公网地址仍旧会失败，得照实说，但也不能说成本机全废。
rc=$(run_case D "SKILLFORGE_CA_BUNDLE=$TMP/ca.pem" no)
[ "$rc" = "0" ] || { echo "FAIL[D]: 退出码 $rc"; fail=1; }
expect D "$TMP/out_D.txt" "已在位"
expect D "$TMP/out_D.txt" "其它 https 站点"

if [ "$fail" -ne 0 ]; then
	echo "安装信任预检验收：失败"
	exit 1
fi
echo "安装信任预检验收：4/4 场景通过"
