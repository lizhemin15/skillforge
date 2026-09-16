#!/usr/bin/env bash
# install.sh「对外访问地址默认值」的验收 —— 从**出货文件**里抠出真函数来跑。
#
# 为什么这件事值得一把尺子：默认值填错的失效形态很隐蔽 —— 页面能打开、
# 上传也能用，只有「下载/分享链接」指向别的主机（localhost 会让每个同事
# 的浏览器去连他自己的电脑）。客户会报「下载失败」，而现场里看不出是地址问题。
#
# 场景（每个都断言不该出现的字样也别出现）：
#   P1. 机器有内网 IP        → 默认值必须是 http://<内网IP>:<port>，不能是 localhost
#   P2. 探测不到（无 ip/hostname 可用）→ 才允许退回 localhost
#   P3. 客户用 --public-url 显式给过 → 绝不能被探测值覆盖（他的域名/反代场景）
#
# 退出码：0 = 全对；非 0 = 有场景不符。
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
INSTALL_SH="${INSTALL_SH:-$HERE/../install.sh}"
[ -f "$INSTALL_SH" ] || { echo "SKIP: 找不到 $INSTALL_SH"; exit 0; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

bash -n "$INSTALL_SH" || { echo "FAIL[P0]: $INSTALL_SH 语法不过关"; exit 1; }

# ── 抠函数段（地址探测）───────────────────────────────────────────────
awk '/^# ---------- 地址探测 ----------$/ { on = 1 }
     on && /^# ---------- 参数 ----------$/ { on = 0 }
     on { print }' "$INSTALL_SH" > "$TMP/detect.sh"
if ! grep -q 'default_public_url()' "$TMP/detect.sh" || ! grep -q 'detect_lan_ip()' "$TMP/detect.sh"; then
	echo "FAIL[P1a]: 抠不出 detect_lan_ip/default_public_url —— 段落标记被改了，测试会测到空气"
	exit 1
fi

# ── 抠「默认值怎么定」那段（PUBLIC_URL 为空时才猜）─────────────────────
awk '/^PUBLIC_URL_AUTO=0$/ { on = 1 } on { print } on && /^fi$/ { exit }' \
	"$INSTALL_SH" > "$TMP/defaultblock.sh"
if ! grep -q 'default_public_url' "$TMP/defaultblock.sh"; then
	echo "FAIL[P1b]: 抠不出默认值判定段（PUBLIC_URL_AUTO=0 … fi）"
	exit 1
fi

# ── 造假环境：受控的 ip / hostname，绝不看真机网卡 ────────────────────
mkdir -p "$TMP/bin_lan" "$TMP/bin_none"
cat > "$TMP/bin_lan/ip" <<'EOF'
#!/bin/sh
case "$*" in
	"route get 1") echo "1.0.0.0 via 10.0.0.1 dev eth0 src 10.11.12.13 uid 0"; exit 0 ;;
	*) exit 1 ;;
esac
EOF
cat > "$TMP/bin_lan/hostname" <<'EOF'
#!/bin/sh
echo "10.11.12.13 build-node"
EOF
# 探测失败：ip 直接失败、hostname 只给回环
cat > "$TMP/bin_none/ip" <<'EOF'
#!/bin/sh
exit 1
EOF
cat > "$TMP/bin_none/hostname" <<'EOF'
#!/bin/sh
echo "127.0.0.1"
EOF
chmod +x "$TMP/bin_lan/ip" "$TMP/bin_lan/hostname" "$TMP/bin_none/ip" "$TMP/bin_none/hostname"

run_default() { # $1=bin 目录 $2=预设的 PUBLIC_URL（可空）
	# 变量必须在真代码**之前**赋值：默认值判定段是会改 PUBLIC_URL 的，
	# 赋值写在后面等于把结果又擦成空（第一版就踩了这个，表现为恒空跑红）。
	{
		echo "PORT=8092"
		printf 'PUBLIC_URL=%s\n' "$2"
		printf 'PUBLIC_URL_SET=%s\n' "${4:-0}"
		echo 'PUBLIC_URL_AUTO=0'
		cat "$TMP/detect.sh" "$TMP/defaultblock.sh"
		echo 'printf "RESULT=%s\n" "$PUBLIC_URL"'
	} > "$TMP/run_$3.sh"
	PATH="$1:/usr/bin:/bin" bash "$TMP/run_$3.sh"
}

fail=0

# ── P1：有内网 IP → 默认给内网 IP ─────────────────────────────────────
out="$(run_default "$TMP/bin_lan" "" p1)"
echo "P1 输出：$out"
if ! grep -qF 'RESULT=http://10.11.12.13:8092' <<<"$out"; then
	echo "FAIL[P1]: 有内网 IP 却没用它当默认对外地址（同事会打不开/下载失败）"
	fail=1
fi
if grep -qF 'localhost' <<<"$out"; then
	echo "FAIL[P1b]: 探测到了内网 IP 却仍然回落到 localhost"
	fail=1
fi

# ── P2：探测不到 → 才退回 localhost ──────────────────────────────────
out="$(run_default "$TMP/bin_none" "" p2)"
echo "P2 输出：$out"
if ! grep -qF 'RESULT=http://localhost:8092' <<<"$out"; then
	echo "FAIL[P2]: 探测不到内网 IP 时应退回 localhost（并靠提示语告知客户），实际不匹配"
	fail=1
fi

# ── P3：显式 --public-url → 不许被覆盖 ───────────────────────────────
out="$(run_default "$TMP/bin_lan" "https://sf.corp.example.com" p3 1)"
echo "P3 输出：$out"
if ! grep -qF 'RESULT=https://sf.corp.example.com' <<<"$out"; then
	echo "FAIL[P3]: 客户显式给的 --public-url 被自动探测覆盖了（反代/域名场景直接坏）"
	fail=1
fi

# ── P4：装完必须把「替你猜的地址」念出来，否则客户永远不知道它哪来的 ──
if ! grep -q '对外访问地址' "$INSTALL_SH"; then
	echo "FAIL[P4]: install.sh 里没有任何「对外访问地址」的提示语 —— 客户不知道这个值从哪来、怎么改"
	fail=1
fi
if [ "$(grep -c 'SKILLFORGE_PUBLIC_URL' "$INSTALL_SH")" -lt 1 ]; then
	echo "FAIL[P4b]: 提示语里没给出改哪个变量（客户知道地址不对也不知道去哪改）"
	fail=1
fi

[ "$fail" -eq 0 ] || { echo "对外地址默认值验收：失败"; exit 1; }
echo "对外地址默认值验收：4/4 场景通过"
