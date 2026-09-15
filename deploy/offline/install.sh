#!/usr/bin/env bash
#
# SkillForge 一键离线安装脚本
# ============================
#
# 目标：把解压出来的目录整个拷到目标机器上，跑一条命令就能用。
#   - 零联网：全程不 curl / wget / apt / pip，需要的东西都在包里
#   - 幂等：重复执行 = 升级（保留已有密钥、账号和数据库）
#   - 可回滚：卸载用同目录下的 uninstall.sh，删得干净
#
# 用法：
#   sudo ./install.sh                       # 默认装到 /opt/skillforge，端口 8092
#   sudo ./install.sh --port 9000           # 换端口
#   sudo ./install.sh --ocr-port 9103       # 换文档解析服务（OCR）端口
#   sudo ./install.sh --no-ocr              # 不装文档解析服务（只装主服务）
#   sudo ./install.sh --prefix /srv/sf      # 换安装前缀
#   sudo ./install.sh --service sf-test     # 换服务名（同一台机器装第二份实例用）
#   sudo ./install.sh --force               # 顶掉同名的、别的前缀的既有实例（危险，见下）
#   sudo ./install.sh --skip-selftest       # 跳过装后自检（仅开发调试用）
#
# 端口冲突：安装前会**先扫描**主服务端口和 OCR 端口。任一被别的进程占用时，
# 交互式终端里会直接问你「换成哪个端口」（回车用建议值），非交互环境下会失败并
# 打印确切的命令（--port / --ocr-port），不会带着冲突硬装下去。
#
# 同一台机器装第二份实例：**必须换 --service 名字**（单元名 = 服务名）。
# 不换名字而前缀又不同，脚本会拒绝安装——因为覆盖单元不会立刻报错，但那个服务
# 下次重启就会静默切到本次的目录/数据/配置上，是最难排查的一类线上事故。
# 确实要顶掉旧实例（旧实例可以下线）时，显式加 --force。
#
# 前置要求只有一条：这台机器有 systemd（因此有 systemd-run）。
# 代码执行沙箱完全建立在 systemd 的降权机制上，它是本服务对外公开时唯一的防线，
# 缺了 systemd 服务装上也跑不了代码——所以脚本宁可拒绝安装，也不静默降级裸跑。

set -euo pipefail

PREFIX=/opt/skillforge
PORT=8092
# 文档解析服务（ocrd）默认 8093。它只监听 127.0.0.1，是主服务的内部依赖：
# 没有它 PDF/Word/Excel 的正文抽不出来，但对「纯文字写作」类技能没有影响。
OCR_PORT=8093
DO_OCR=1
DATA_DIR=""
ADMIN_USER=admin
ADMIN_PASS=""
# 字体目录**按实例隔离**（见下方 --font-dir 说明）：早期版本用固定的
# /usr/local/share/fonts/skillforge，结果卸载任一实例会把别的实例正在用的字体一起删掉
# （Bug K）——客户机本来就没有系统中文字体（所以才自带），删完表现为 PDF 中文/数字
# 静默变空白，静默故障里最难查的一类。
FONT_DIR=""
FONT_DIR_SET=0
SERVICE_NAME=skillforge
DO_START=1
DO_SELFTEST=1
PUBLIC_URL=""
# 顶掉「别人家的」服务单元需要显式 --force（见 0.5 冲突检查）。
FORCE=0
# --public-url 有没有显式给过：端口冲突时我们会改 PORT，默认的 PUBLIC_URL 必须跟着重算
PUBLIC_URL_SET=0

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SELF="${BASH_SOURCE[0]}"





# ---------- 输出小工具 ----------
c_info()  { printf '  %s\n' "$*"; }
c_ok()    { printf '  \033[32m✓\033[0m %s\n' "$*"; }
c_warn()  { printf '  \033[33m!\033[0m %s\n' "$*"; }
c_fail()  { printf '  \033[31m✗\033[0m %s\n' "$*"; }
step()    { printf '\n\033[1m[%s]\033[0m %s\n' "$1" "$2"; }
die()     { c_fail "$*"; exit 1; }

usage() {
	sed -n '2,22p' "$SELF" | sed 's/^# \{0,1\}//'
	exit 0
}

# ---------- 参数 ----------
while [ $# -gt 0 ]; do
	case "$1" in
		--prefix)      PREFIX="${2:?--prefix 需要一个目录}"; shift 2 ;;
		--port)        PORT="${2:?--port 需要端口号}"; shift 2 ;;
		--ocr-port)    OCR_PORT="${2:?--ocr-port 需要端口号}"; shift 2 ;;
		--no-ocr)      DO_OCR=0; shift ;;
		--data-dir)    DATA_DIR="${2:?--data-dir 需要一个目录}"; shift 2 ;;
		--admin-user)  ADMIN_USER="${2:?}"; shift 2 ;;
		--admin-pass)  ADMIN_PASS="${2:?}"; shift 2 ;;
		--font-dir)    FONT_DIR="${2:?}"; FONT_DIR_SET=1; shift 2 ;;
		--public-url)  PUBLIC_URL="${2:?}"; PUBLIC_URL_SET=1; shift 2 ;;
		# 服务名同时决定 systemd 单元名，所以同一台机器上装第二份实例（灰度/测试）
		# 必须换名字，否则会覆盖第一份的单元文件。参数名与 uninstall.sh 对齐（--service）。
		--service|--service-name) SERVICE_NAME="${2:?--service 需要一个服务名}"; shift 2 ;;
		--no-start)    DO_START=0; shift ;;
		--skip-selftest) DO_SELFTEST=0; shift ;;
		--force)       FORCE=1; shift ;;
		-h|--help)     usage ;;
		*)             die "无法识别的参数：$1（用 --help 看用法）" ;;
	esac
done

[ -n "$DATA_DIR" ] || DATA_DIR="$PREFIX/data"
[ -n "$PUBLIC_URL" ] || PUBLIC_URL="http://localhost:$PORT"
# 字体按实例隔离：/usr/local/share/fonts/skillforge-<服务名>。
# 这样卸载本实例只删自己的字体，不会把同机其它实例正在用的那份抽走（Bug K）。
[ "$FONT_DIR_SET" -eq 1 ] || FONT_DIR="/usr/local/share/fonts/skillforge-$SERVICE_NAME"

ENV_FILE="$PREFIX/skillforge.env"
BIN="$PREFIX/skillforge"
UNIT="/etc/systemd/system/$SERVICE_NAME.service"
# 文档解析服务：二进制与单元跟主服务同前缀/同服务名前缀命名，卸载才能一起清干净
OCR_BIN="$PREFIX/bin/ocrd"
OCR_UNIT="/etc/systemd/system/$SERVICE_NAME-ocr.service"
BUNDLED_FONT=""
# 包内自带 Python 拷到 $PREFIX 后的绝对路径（PY_BUNDLED=1 时才有值）。
BUNDLED_PY=""

printf '\033[1mSkillForge 离线安装\033[0m\n'
c_info "安装前缀 : $PREFIX"
c_info "监听端口 : $PORT"
c_info "数据目录 : $DATA_DIR"

# ---------- 0. 前置检查 ----------
step "0/7" "前置检查"

[ "$(id -u)" -eq 0 ] || die "需要用 root 运行：sudo $SELF"
c_ok "以 root 运行"

[ -f "$HERE/bin/skillforge" ] || die "包里找不到 bin/skillforge。请确认整个目录一起解压，别只拿出脚本。"
c_ok "找到服务二进制"

# 前缀/数据目录不能落在 /tmp、/var/tmp：
# 服务单元带 PrivateTmp=true（安全加固），单元内看到的 /tmp 是私有的空目录，
# 装在这里单元一启动就 203/EXEC 崩溃重启，日志里只有一句 "Failed to locate executable"，
# 客户很难看出是因为 PrivateTmp。与其事后排查，不如安装前拒掉。
for p in "$PREFIX" "$DATA_DIR"; do
	case "$p" in
		/tmp|/tmp/*|/var/tmp|/var/tmp/*)
			die "安装前缀/数据目录不能放在 $p —— 服务单元启用 PrivateTmp=true 后看不到宿主 /tmp，装在这里启动即 203/EXEC 崩溃。请换 /opt 或 /srv，例如：sudo $SELF --prefix /srv/skillforge" ;;
	esac
done

if [ ! -d /run/systemd/system ] && ! command -v systemctl >/dev/null 2>&1; then
	die "这台机器没有 systemd。代码执行沙箱依赖 systemd-run，缺了它本服务无法安全地对外提供代码执行能力。"
fi
command -v systemctl >/dev/null 2>&1 || die "找不到 systemctl（systemd 在但 PATH 里没有？请用 root 完整环境重跑）"
command -v systemd-run >/dev/null 2>&1 || die "找不到 systemd-run（systemd 太旧或安装不完整）。沙箱不可用，拒绝安装。"
c_ok "systemd / systemd-run 可用"

# python3：代码执行沙箱（自检探针 + 「执行代码」工具）的解释器。
#
# 不再只查写死的 /usr/bin/python3 —— 装没装 ≠ 在不在这一个路径上。源码编译/
# conda 装在 /usr/local/bin/python3、RHEL 8 系只有 /usr/libexec/platform-python
# 的机器，都会被旧逻辑误报成「目标机没有 python3」，用户照着提示去装一个已经
# 装好的东西，怎么装都「修不好」。
#
# ⚠️ PythonCandidates 必须与 internal/tools/exec.go 的 pythonCandidates 逐字一致：
# internal/tools/python_resolve_test.go 会解析这两份源码比对名单，脱钩即红。
PythonCandidates="/usr/bin/python3 /usr/local/bin/python3 /usr/libexec/platform-python"
PY_FOUND=""
# PY_FROM_PATH=1 表示命中的解释器不在候选名单里、只在 $PATH 上找到 —— 这种必须钉进
# 配置文件，见下面「生成配置」那步。
PY_FROM_PATH=0
# PY_BUNDLED=1 表示用的是**包内自带**的那份（拷到 $PREFIX/python 后钉进配置）。
PY_BUNDLED=0
# 包内自带的解释器源路径（在 $HERE 下，稍后拷进 $PREFIX）。
PY_BUNDLED_SRC=""

# ---------- 包里自带的 Python（优先于目标机上的任何解释器）----------
#
# 为什么要自带：客户机器常是内网最小安装 —— 没有 python3，而且**装不了**
# （没有源、没有 ISO 可挂）。旧版把「目标机得先有 python3」写进前置要求，等于把
# 「一键安装」拆成「你先想办法弄个 python 去」，用户报的现场就是这个。
# 现在离线包自带一份便携 CPython，安装时拷到 $PREFIX/python 并钉进配置，
# 目标机完全不需要预装解释器。
if [ -x "$HERE/python/bin/python3" ] \
	&& "$HERE/python/bin/python3" -c pass >/dev/null 2>&1; then
	PY_BUNDLED_SRC="$HERE/python/bin/python3"
elif [ -e "$HERE/python" ]; then
	# 目录在、解释器却跑不起来 = 包被截断或架构不符。必须说出来：否则用户拿到的是
	# 「安装成功但 AI 不能跑代码」，而原因（包坏了）从任何输出里都看不出来。
	# 注意 $HERE/python 不带 bin/python3 的老包不在此列 —— 那种走下面的候选/ PATH。
	c_warn "包内自带解释器存在但跑不起来：$HERE/python/bin/python3（包可能损坏或架构不符）"
fi
if [ -n "${SKILLFORGE_PYTHON:-}" ]; then
	# 显式指定就只认它：与 Go 侧同一条原则 —— 指了个用不了的路径时不去猜别的，
	# 否则运维以为在用自己那份、实际在用系统的，是最难查的坑。
	if [ -x "$SKILLFORGE_PYTHON" ] && "$SKILLFORGE_PYTHON" -c pass >/dev/null 2>&1; then
		PY_FOUND="$SKILLFORGE_PYTHON"
	else
		c_warn "SKILLFORGE_PYTHON=$SKILLFORGE_PYTHON 不可用：要存在、可执行、且能跑 \`-c pass\`"
	fi
else
	# 自带优先于目标机上的一切（含候选名单里那三个）：客户机器什么环境我们控制不了，
	# 包里这份是唯一能保证「一定存在、行为一致」的解释器。
	if [ -n "$PY_BUNDLED_SRC" ]; then
		PY_FOUND="$PY_BUNDLED_SRC"
		PY_BUNDLED=1
	fi
	if [ -z "$PY_FOUND" ]; then
		for _cand in $PythonCandidates; do
			if [ -x "$_cand" ] && "$_cand" -c pass >/dev/null 2>&1; then
				PY_FOUND="$_cand"
				break
			fi
		done
	fi
	# 候选全不命中时，再认一次 $PATH 里的 python3 —— 与 Go 侧 resolvePythonFrom 的
	# 最后一步等价（自己编译 / conda 装到 /opt/xxx/bin 这类不在候选名单里的位置）。
	#
	# 少了这一步，下面那句「找过：… 以及 $PATH」就是**假话**：用户去查 PATH 明明有
	# python3，还是被告知「没有 python3」，怎么修都修不好 —— 跟这轮修的误报同一个形状。
	if [ -z "$PY_FOUND" ]; then
		_via_path="$(command -v python3 2>/dev/null || true)"
		case "$_via_path" in
			/*) ;;
			# 相对路径没有意义：命中它的是安装时的 cwd，而服务由 systemd 起，
			# cwd 是 /。写进配置只会变成「看着对、行为随机」。
			*) _via_path="" ;;
		esac
		if [ -n "$_via_path" ] && "$_via_path" -c pass >/dev/null 2>&1; then
			PY_FOUND="$_via_path"
			PY_FROM_PATH=1
		fi
	fi
fi

# 为什么只警告不拦：写作主功能不依赖它，硬拦会把本来能用的客户挡在门外；但也不能
# 不吭声——装完自检里那条会红，客户看到的是「代码执行沙箱 失败」，得让他一眼看懂
# 是缺解释器，不是安全加固漏了。（真机上这就是 almalinux 最小安装的现场。）
if [ -z "$PY_FOUND" ]; then
	c_warn "这台机器没有找到可用的 python3 解释器（找过：包内自带、$PythonCandidates 以及 \$PATH）"
	c_warn "  影响：「代码执行沙箱」一项自检会失败，AI 无法跑代码/脚本校验；写作等主功能不受影响"
	c_warn "  注意：这不是沙箱降权/加固失败，就是缺个解释器"
	c_warn "  本包本应自带一份便携解释器（<包目录>/python/bin/python3）—— 上面这句出现，"
	c_warn "  通常是拿了旧版离线包（不带 python/），或包在解压时被截断。建议重新下载最新离线包。"
	c_warn "  不想换包就自己装一个：dnf install -y python3（RHEL/AlmaLinux 最小安装默认不带；Debian/Ubuntu 用 apt install python3）"
	c_warn "  离线机：挂发行版 ISO 或配本地源后按上面装，装完补跑一次自检：/opt/skillforge/skillforge -selftest"
	c_warn "  解释器装在别处（自己编译/conda）：装完在 $PREFIX.env 里加一行 SKILLFORGE_PYTHON=/usr/local/bin/python3"
	c_warn "  确实接受这一项不可用：加 --skip-selftest 跳过装后自检（自担风险，不推荐）"
else
	if [ "$PY_BUNDLED" = "1" ]; then
		c_ok "python3 可用（代码执行沙箱的解释器）：包内自带（目标机无需预装）"
	elif [ "$PY_FOUND" = "/usr/bin/python3" ]; then
		c_ok "python3 可用（代码执行沙箱的解释器）：$PY_FOUND"
	else
		# 走到这里说明旧逻辑会误报「没有 python3」——把命中的路径打出来，方便回访。
		c_ok "python3 可用（代码执行沙箱的解释器）：$PY_FOUND（非默认路径，已自动识别）"
	fi
fi

# ---------- 0.5 服务单元占用检查（Bug M）----------
# 服务名默认就是 skillforge。如果这台机器上已经装过一份（单元文件已存在）而它的安装
# 前缀跟本次不同，直接覆盖单元的后果极其隐蔽：
#   已在跑的那个进程不受影响（systemd 不会因为改文件就重启），**但下次重启/宕机恢复时
#   它会按新单元启动 → 静默切到另一份目录、另一份数据、另一份配置上**。
#   从外面看只是「服务重启了一下」，实际整个实例被换掉了。
# 所以在装之前就拦住，而不是等它哪天重启后表现诡异。
if [ -f "$UNIT" ]; then
	EXIST_PREFIX="$(sed -n 's|^[[:space:]]*WorkingDirectory=||p' "$UNIT" | head -n 1)"
	if [ -n "$EXIST_PREFIX" ] && [ "$EXIST_PREFIX" != "$PREFIX" ]; then
		if [ "$FORCE" -ne 1 ]; then
			c_fail "服务单元「$UNIT」已被占用，且指向另一个安装前缀："
			c_fail "  已存在：WorkingDirectory=$EXIST_PREFIX"
			c_fail "  本次要装：$PREFIX"
			c_fail "覆盖它不会立刻出问题（运行中的进程照旧），但该服务下次重启就会静默切到"
			c_fail "「$PREFIX」这份目录/数据/配置上——线上事故最常见的那种形态。"
			c_fail "想装第二份实例（同机并存）：加 --service <新名字>，例如 --service skillforge-test"
			c_fail "确认旧实例可以下线、真要顶掉它：加 --force"
			exit 1
		fi
		c_warn "按 --force 顶掉既有实例：$EXIST_PREFIX → $PREFIX（旧实例的数据目录不会自动迁移）"
	else
		c_info "检测到同前缀的既有安装，按覆盖升级处理"
	fi
fi

port_in_use() {
	# 判据：TCP LISTEN 里有没有这个端口。用 ss 优先，没有 ss 就退回 /proc/net/tcp
	# （离线最小系统常常没装 iproute2）。
	local p="${1:?port_in_use 需要端口号}"
	if command -v ss >/dev/null 2>&1; then
		local out
		out="$(ss -ltn 2>/dev/null || true)"
		case "$out" in
			*":$p "*) return 0 ;;
			*)       return 1 ;;
		esac
	fi
	# /proc/net/tcp 的本地地址是 16 进制大端，端口是低 4 位十六进制
	local hex
	hex="$(printf '%04X' "$p")"
	awk -v h="$hex" 'NR>1 && $4=="0A" { split($2,a,":"); if (toupper(a[2])==h) found=1 } END { exit found?0:1 }' /proc/net/tcp 2>/dev/null
}

port_owner() {
	# 谁占着这个端口 —— 只为了在冲突报告里写清楚"被谁占了"。
	# 拿不到就返回空（比如进程属于别的 namespace），不因此判失败。
	local p="${1:?}" out=""
	if command -v ss >/dev/null 2>&1; then
		out="$(ss -ltnp 2>/dev/null | awk -v p=":$p" '$4 ~ p"$" || index($4, p" ") {print; exit}' || true)"
	fi
	if [ -z "$out" ]; then
		local hex
		hex="$(printf '%04X' "$p")"
		out="$(awk -v h="$hex" 'NR>1 && $4=="0A" { split($2,a,":"); if (toupper(a[2])==h) print "inode="$10 }' /proc/net/tcp 2>/dev/null | head -n 1)"
	fi
	printf '%s' "$out"
}

valid_port() {
	case "${1:-}" in
		''|*[!0-9]*) return 1 ;;
	esac
	[ "$1" -ge 1 ] && [ "$1" -le 65535 ]
}

# 读一个端口：交互式终端下反复问直到拿到合法且空闲的端口；非交互直接判失败。
# 为什么必须"问到对为止"而不是问一次：
#   用户输入的第二个端口同样可能撞车（尤其是抄了旁边那台机器的端口），
#   只问一次就装下去，等于把这个冲突推到"服务起不来"才暴露。
ask_port() {
	local label="$1" def="$2" cur="$3" ans="" tries=0
	while [ "$tries" -lt 20 ]; do
		tries=$((tries + 1))
		printf '  %s端口 [%s]: ' "$label" "$def" >&2
		read -r ans || ans=""
		[ -n "$ans" ] || ans="$def"
		if ! valid_port "$ans"; then
			c_warn "「$ans」不是合法端口（1-65535），重来" >&2
			continue
		fi
		if [ "$ans" = "$cur" ]; then
			c_warn "「$ans」就是刚才冲突的那个端口，重来" >&2
			continue
		fi
		if port_in_use "$ans"; then
			c_warn "「$ans」也被占用了（$(port_owner "$ans")），换一个" >&2
			continue
		fi
		printf '%s' "$ans"
		return 0
	done
	return 1
}

# 注意：这里**不能**写成 `ss | awk | grep -q`。grep -q 命中即退出会让上游进程收到 SIGPIPE，
# 在 set -o pipefail 下整条管道被判失败 → 端口占用检测恒为「空闲」，冲突被静默放过。
#
# 主服务端口：如果是本服务自己占着（unit 已在跑），当"升级"处理，不算冲突。
MAIN_BUSY=0
if port_in_use "$PORT"; then
	if [ -f "$UNIT" ] && systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null; then
		c_warn "端口 $PORT 已被本服务（$SERVICE_NAME）占用——按「升级」处理"
	else
		MAIN_BUSY=1
	fi
fi
# OCR 端口：本实例的 ocr 单元在跑也算"自己的"，同样是升级。
OCR_BUSY=0
if [ "$DO_OCR" -eq 1 ] && port_in_use "$OCR_PORT"; then
	if [ -f "$OCR_UNIT" ] && systemctl is-active --quiet "$SERVICE_NAME-ocr" 2>/dev/null; then
		c_warn "端口 $OCR_PORT 已被本实例的文档解析服务占用——按「升级」处理"
	else
		OCR_BUSY=1
	fi
fi

# ---------- 0.4 端口预扫描（冲突要当场解决，不要等"服务起不来"）----------
# 为什么要"先扫描再装"：装到一半才发现端口被占，机器上已经留下了半个安装（目录、
# 用户、字体、可能还有单元文件），用户还得先搞清楚怎么清干净再来一遍。
# 扫描放在最前面，冲突就在这一步解决掉（或者在非交互环境里明确拒绝）。
suggest_port() {
	# 从 start 往上找第一个空闲端口；跳过 exclude（避免跟另一个服务撞成同一个）
	local p="$1" ex="$2" i=0
	while [ "$i" -lt 200 ]; do
		if [ "$p" != "$ex" ] && ! port_in_use "$p"; then
			printf '%s' "$p"; return 0
		fi
		p=$((p + 1)); i=$((i + 1))
	done
	return 1
}

TTY_MODE="none"
if [ -t 0 ]; then
	TTY_MODE="stdin"
elif [ -r /dev/tty ]; then
	TTY_MODE="tty"
fi

# 两个服务不能用一个端口：撞在一起时后起的那个必然起不来，而且报错很难懂（address in use）
if [ "$DO_OCR" -eq 1 ] && [ "$PORT" = "$OCR_PORT" ]; then
	die "主服务端口和 OCR 端口不能相同（都是 $PORT）。例：sudo $SELF --port 8092 --ocr-port 8093"
fi

if [ "$MAIN_BUSY" -eq 1 ] || [ "$OCR_BUSY" -eq 1 ]; then
	printf '\n\033[1m端口检查\033[0m\n'
	[ "$MAIN_BUSY" -eq 1 ] && { c_fail "主服务端口 $PORT 被别的进程占用：$(port_owner "$PORT")"; } || c_ok "主服务端口 $PORT 空闲"
	if [ "$DO_OCR" -eq 1 ]; then
		[ "$OCR_BUSY" -eq 1 ] && { c_fail "文档解析端口 $OCR_PORT 被别的进程占用：$(port_owner "$OCR_PORT")"; } || c_ok "文档解析端口 $OCR_PORT 空闲"
	fi

	if [ "$TTY_MODE" = "none" ]; then
		c_fail "非交互环境（stdin 不是终端），无法询问端口。请显式指定后重跑："
		c_fail "  sudo $SELF --port <主服务端口> --ocr-port <文档解析端口>"
		[ "$DO_OCR" -eq 0 ] && c_fail "  （已用 --no-ocr，只需 --port）"
		exit 1
	fi

	c_info "需要你指定没被占用的端口（直接回车 = 用建议值）："
	if [ "$MAIN_BUSY" -eq 1 ]; then
		SUG="$(suggest_port $((PORT + 1)) "$OCR_PORT" || printf '%s' "$((PORT + 100))")"
		NEW="$(ask_port "主服务" "$SUG" "$PORT")" || die "端口没有确定下来，安装中止（没有改动系统）"
		PORT="$NEW"
		MAIN_BUSY=0
	fi
	if [ "$OCR_BUSY" -eq 1 ]; then
		SUG="$(suggest_port $((OCR_PORT + 1)) "$PORT" || printf '%s' "$((OCR_PORT + 100))")"
		NEW="$(ask_port "文档解析" "$SUG" "$OCR_PORT")" || die "端口没有确定下来，安装中止（没有改动系统）"
		OCR_PORT="$NEW"
		OCR_BUSY=0
	fi
	c_ok "端口确定：主服务 $PORT ／ 文档解析 $OCR_PORT"
	# 端口变了，PUBLIC_URL 的默认值要跟着改（否则下载链接里还是旧端口）
	[ -n "$PUBLIC_URL_SET" ] || PUBLIC_URL="http://localhost:$PORT"
else
	c_ok "端口检查通过（主服务 $PORT$([ "$DO_OCR" -eq 1 ] && printf ' ／ 文档解析 %s' "$OCR_PORT") 都空闲）"
fi

# ---------- 1. 生成/复用密钥 ----------
step "1/7" "密钥与账号"

rand_hex() {
	if command -v openssl >/dev/null 2>&1; then
		openssl rand -hex "$1"
	else
		# 兜底：不依赖 openssl。取 /dev/urandom 转十六进制，去掉非十六进制字符。
		head -c "$(( $1 * 4 ))" /dev/urandom | od -An -tx1 | tr -d ' \n' | head -c "$(( $1 * 2 ))"
	fi
}

# 读已有 env 里的某个键（幂等安装的关键：不覆盖客户已经配置好的值）
env_get() {
	local key="$1"
	[ -f "$ENV_FILE" ] || return 0
	sed -n "s/^${key}=//p" "$ENV_FILE" | tail -n 1
}

if [ -f "$BIN" ] || [ -f "$ENV_FILE" ]; then
	c_warn "检测到已有安装 → 按「升级」处理：保留密钥/账号/数据库，只替换程序与系统服务"
else
	c_info "全新安装"
fi

OLD_SF_JWT="$(env_get SKILLFORGE_JWT_SECRET)"
OLD_ADMIN_PW="$(env_get SKILLFORGE_ADMIN_PASS)"
OLD_ADMIN_USER="$(env_get SKILLFORGE_ADMIN_USER)"
OLD_LLM_KEY="$(env_get SKILLFORGE_LLM_API_KEY)"
OLD_LLM_URL="$(env_get SKILLFORGE_LLM_BASE_URL)"
OLD_LLM_MODEL="$(env_get SKILLFORGE_LLM_MODEL)"
OLD_LLM_PROV="$(env_get SKILLFORGE_LLM_PROVIDER)"
OLD_FONT="$(env_get SKILLFORGE_PDF_FONT_FILE)"
OLD_PY="$(env_get SKILLFORGE_PYTHON)"

if [ -n "$OLD_SF_JWT" ]; then
	SF_JWT="$OLD_SF_JWT"
	c_ok "沿用已有签名密钥（换掉会让所有登录态失效）"
else
	SF_JWT="$(rand_hex 32)"
	c_ok "生成随机签名密钥（64 位十六进制）"
fi

if [ -n "$ADMIN_PASS" ]; then
	FINAL_PW="$ADMIN_PASS"
	c_ok "管理员密码：使用命令行指定的值"
elif [ -n "$OLD_ADMIN_PW" ]; then
	FINAL_PW="$OLD_ADMIN_PW"
	c_ok "管理员密码：沿用已有（$OLD_ADMIN_USER）"
else
	# 只生成足够强、且不含 shell 元字符的密码，避免 env 文件被任何解析方式误伤
	FINAL_PW="$(rand_hex 9)$(head -c 4 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 4)"
	c_ok "管理员密码：已生成随机强密码（安装结束会打印一次）"
fi
ADMIN_USER="${OLD_ADMIN_USER:-$ADMIN_USER}"

# ---------- 2. 安装字体（离线包自带，PDF 中文/数字全靠它）----------
step "2/7" "中文字体"

if ! ls "$HERE"/fonts/*.ttf >/dev/null 2>&1; then
	c_warn "包里没有 fonts/*.ttf。若目标机也没装中文字体，PDF 里的中文与数字会渲染成空白。"
else
	install -d -m 0755 "$FONT_DIR"
	for f in "$HERE"/fonts/*.ttf; do
		install -m 0644 "$f" "$FONT_DIR/$(basename "$f")"
	done
	c_ok "已安装 $(ls -1 "$HERE"/fonts/*.ttf | wc -l) 个字体到 $FONT_DIR"
	if command -v fc-cache >/dev/null 2>&1; then
		fc-cache -f "$FONT_DIR" >/dev/null 2>&1 || true
		c_info "已刷新 fontconfig 缓存（服务自身不依赖它，纯 Go 直接读字体文件）"
	fi
	# 明确指定字体文件，不靠运行时扫描去猜——自检会验证它真的覆盖中文+数字
	BUNDLED_FONT="$FONT_DIR/$(basename "$(ls -1 "$HERE"/fonts/*.ttf | head -n 1)")"
	c_ok "PDF 字体已显式指定：$BUNDLED_FONT"
fi

# ---------- 3. 落盘程序与数据目录 ----------
step "3/7" "安装程序文件"

install -d -m 0755 "$PREFIX"
install -d -m 0700 "$DATA_DIR"

if [ "$DO_START" -eq 1 ] && systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null; then
	systemctl stop "$SERVICE_NAME"
	c_info "已停止旧服务，准备替换二进制"
fi

# 原子替换：先 install 到临时名再 mv，避免「写到一半崩了留下半个二进制」
install -m 0755 "$HERE/bin/skillforge" "$BIN.new"
mv -f "$BIN.new" "$BIN"
c_ok "程序：$BIN"

# 文档解析服务（ocrd）：同一个前缀下的 bin/，与主服务同生命周期（装上/卸载一起走）。
# 它是「扫件/Word/Excel 正文抽取」的唯一实现——缺了它这类技能会报"解析服务不可用"，
# 纯文字写作类技能不受影响。所以缺文件只告警、不阻断安装。
if [ "$DO_OCR" -eq 1 ]; then
	install -d -m 0755 "$PREFIX/bin"
	if [ -f "$HERE/bin/ocrd" ]; then
		install -m 0755 "$HERE/bin/ocrd" "$OCR_BIN.new"
		mv -f "$OCR_BIN.new" "$OCR_BIN"
		c_ok "文档解析服务：$OCR_BIN（含 PP-OCRv4 模型，无需额外依赖）"
	else
		DO_OCR=0
		c_warn "包里没有 bin/ocrd → 跳过文档解析服务。"
		c_warn "影响：扫描件 PDF / Word / Excel 的正文抽不出来（纯文字写作类技能不受影响）。"
		c_warn "带上它需要重新下载完整离线包（含 ocrd 的那份）。"
	fi
fi

# ---------- 包内自带的 Python 运行时 ----------
#
# 为什么不直接用包目录里那份：客户装完常会把解压出来的目录（甚至 tar 包）清掉，
# 而服务是 systemd 长期跑的 —— 指向包目录的解释器会在某次清理后突然消失，表现为
# 「昨天还能跑代码，今天不能了」。所以拷进 $PREFIX，与二进制同生命周期。
if [ "$PY_BUNDLED" = "1" ]; then
	BUNDLED_PY="$PREFIX/python/bin/python3"
	# 先拷到 .new 再换入：中途失败不会留下半份运行时被下次安装当成好的用。
	# cp -a 保留可执行位与符号链接 —— 便携 Python 的 lib 下有大量 symlink，
	# 丢了会把整套标准库连根拔掉（表现为 import 全失败）。
	rm -rf "$PREFIX/python.new"
	cp -a "$HERE/python" "$PREFIX/python.new"
	# 沙箱是以降权用户跑的，必须让他读得到、执行得了：目录给 a+rx，普通文件给 a+r。
	# 少这一步，装完自检「代码执行沙箱」会红，报的还是权限错误 —— 用户根本看不出
	# 是自己这里少了个 chmod。
	chmod -R a+rX "$PREFIX/python.new"
	# 换入用「旧的先挪走」而不是先 rm：正在跑的服务（--no-start / 升级热替换）不至于
	# 在这一瞬间找不到解释器。挪走的旧件最后统一删。
	if [ -e "$PREFIX/python" ]; then
		rm -rf "$PREFIX/python.old"
		mv "$PREFIX/python" "$PREFIX/python.old"
	fi
	mv "$PREFIX/python.new" "$PREFIX/python"
	rm -rf "$PREFIX/python.old"
	# 拷完必须现场验一次：跨文件系统拷贝 / 权限位丢失 / 磁盘满，都会让这份运行时
	# 变成「文件在、跑不起来」。装完才发现在客户那里 = 一次无效交付，所以当场 die。
	"$BUNDLED_PY" -c pass >/dev/null 2>&1 \
		|| die "自带解释器拷到 $BUNDLED_PY 后跑不起来（拷贝不完整 / 权限不对 / 磁盘满？）——安装中止，不留下「装成功但跑不了代码」的实例"
	c_ok "自带 Python 运行时：$BUNDLED_PY（目标机无需预装解释器）"
fi

# 技能不随包带：程序启动时会自己 seed 内置技能（见 internal/store/seed.go），
# 包里再塞一份目录只会造成「有目录没数据库记录」的孤儿状态。

# ---------- 4. 写配置文件 ----------
step "4/7" "生成配置"

umask 077
: > "$ENV_FILE"
{
	printf '# SkillForge 运行配置 —— 由 install.sh 生成于 %s\n' "$(date '+%Y-%m-%d %H:%M:%S')"
	printf '#\n'
	printf '# 这个文件里有密钥，权限已设为 600，别提交进任何仓库、别贴到聊天里。\n'
	printf '# 改完要重启服务：systemctl restart %s\n\n' "$SERVICE_NAME"
	printf '# ---- 服务 ----\n'
	printf 'SKILLFORGE_ADDR=:%s\n' "$PORT"
	printf 'SKILLFORGE_DATA_DIR=%s\n' "$DATA_DIR"
	printf 'SKILLFORGE_DB=%s/skillforge.db\n' "$DATA_DIR"
	printf '# 对外可访问地址：生成下载链接用。改成客户实际访问的域名/IP 后再重启。\n'
	printf 'SKILLFORGE_PUBLIC_URL=%s\n\n' "$PUBLIC_URL"
	printf '# ---- 管理端账号 ----\n'
	printf 'SKILLFORGE_ADMIN_USER=%s\n' "$ADMIN_USER"
	printf '%s=%s\n' SKILLFORGE_ADMIN_PASS "$FINAL_PW"
	printf '%s=%s\n\n' SKILLFORGE_JWT_SECRET "$SF_JWT"
	printf '# ---- PDF 字体（离线包自带；显式指定可避免运行时猜字体）----\n'
	printf 'SKILLFORGE_PDF_FONT_FILE=%s\n\n' "$BUNDLED_FONT"
	# 解释器只在我们「从 $PATH 里认出来」时才钉住：
	#   - 服务由 systemd 起，它的 PATH 与你登录时的 shell **不同**。安装时认到
	#     /opt/xxx/bin/python3、服务起来却找不到 → 「安装说没问题、装完自检红」，
	#     用户两头都查不出所以然。钉住路径是唯一能保证两边看到同一个解释器的做法。
	#   - 命中候选名单里那三个位置的不钉：那是标准位置，钉死反而在系统升级换路径时僵住。
	#   - 上一版配置里已有的 SKILLFORGE_PYTHON 一律沿用（升级不改客户的手写配置）。
	if [ -n "$OLD_PY" ]; then
		printf '# ---- 解释器（沿用上一版配置）----\n'
		printf 'SKILLFORGE_PYTHON=%s\n\n' "$OLD_PY"
	elif [ "$PY_BUNDLED" = "1" ]; then
		printf '# ---- 解释器（离线包自带，目标机无需预装 python3）----\n'
		printf '# 必须钉住绝对路径：服务由 systemd 起，它的 PATH 跟你登录的 shell 不一样，\n'
		printf '# 只靠「找 $PATH」会出现「安装时说没问题、装完自检红」。\n'
		printf 'SKILLFORGE_PYTHON=%s\n\n' "$BUNDLED_PY"
	elif [ "$PY_FROM_PATH" = "1" ]; then
		printf '# ---- 解释器 ----\n'
		printf '# 这个解释器不在标准位置（/usr/bin、/usr/local/bin、platform-python），\n'
		printf '# 只在安装时的 $PATH 上找到。systemd 起的服务 PATH 和你登录的 shell 不同，\n'
		printf '# 不钉住就会出现「安装时认了、装完自检红」。换解释器时改这一行并重启服务。\n'
		printf 'SKILLFORGE_PYTHON=%s\n\n' "$PY_FOUND"
	fi
	if [ "$DO_OCR" -eq 1 ]; then
		printf '# ---- 文档解析服务（ocrd，本机回环，不对外）----\n'
		printf '# 只监听 127.0.0.1，主服务用它把 PDF/Word/Excel/扫描件抽成文本。\n'
		printf '# 端口冲突时安装脚本会问你换端口，改完这里要同步重启两个服务。\n'
		printf 'SKILLFORGE_OCR_URL=http://127.0.0.1:%s\n\n' "$OCR_PORT"
	fi
	printf '# ---- 离线自检用来证明「沙箱确实读不到机密」的证据文件 ----\n'
	printf 'SKILLFORGE_ENV_FILE=%s\n\n' "$ENV_FILE"
	printf '# ---- LLM（也可装好后登录管理端在网页上配，网页配置优先）----\n'
	printf 'SKILLFORGE_LLM_PROVIDER=%s\n' "$OLD_LLM_PROV"
	printf 'SKILLFORGE_LLM_BASE_URL=%s\n' "$OLD_LLM_URL"
	printf 'SKILLFORGE_LLM_MODEL=%s\n' "$OLD_LLM_MODEL"
	printf '%s=%s\n' SKILLFORGE_LLM_API_KEY "$OLD_LLM_KEY"
} >> "$ENV_FILE"
chmod 600 "$ENV_FILE"
umask 022
c_ok "配置：$ENV_FILE（权限 600，含随机密钥）"
if [ -n "$OLD_FONT" ] && [ "$OLD_FONT" != "$BUNDLED_FONT" ]; then
	c_warn "注意：上一版配置里指定的 PDF 字体是 $OLD_FONT，本次已改为包内自带字体。"
	c_warn "如果你当初显式换过字体，请在装后把 $ENV_FILE 里的 SKILLFORGE_PDF_FONT_FILE 改回去。"
fi

# ---------- 5. 安装 systemd 服务 ----------
step "5/7" "注册系统服务"

if [ -f "$HERE/skillforge.service.template" ]; then
	sed -e "s|__PREFIX__|$PREFIX|g" \
	    -e "s|__ENV_FILE__|$ENV_FILE|g" \
	    "$HERE/skillforge.service.template" > "$UNIT"
else
	cat > "$UNIT" <<EOF
[Unit]
Description=SkillForge — skill-centric AI document workspace
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$PREFIX
EnvironmentFile=$ENV_FILE
ExecStart=$PREFIX/skillforge
Restart=always
RestartSec=3
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
EOF
fi
chmod 644 "$UNIT"
systemctl daemon-reload
c_ok "服务单元：$UNIT"

# 文档解析服务的单元：独立单元（不是主服务的子进程）—— 它内存峰值高（50 页扫描件约 1.8G），
# 单独一个单元才能单独设 MemoryMax，也才能单独重启而不打断正在写作的主服务。
if [ "$DO_OCR" -eq 1 ]; then
	if [ -f "$HERE/skillforge-ocr.service.template" ]; then
		sed -e "s|__OCR_BIN__|$OCR_BIN|g" \
		    -e "s|__OCR_PORT__|$OCR_PORT|g" \
		    -e "s|__SERVICE_NAME__|$SERVICE_NAME|g" \
		    "$HERE/skillforge-ocr.service.template" > "$OCR_UNIT"
	else
		cat > "$OCR_UNIT" <<EOF
[Unit]
Description=SkillForge document parser ($SERVICE_NAME-ocr, RapidOCR PP-OCRv4)
After=network.target

[Service]
Type=simple
ExecStart=$OCR_BIN --port $OCR_PORT
Restart=on-failure
RestartSec=3
# 扫描件 OCR 峰值内存高（50 页约 1.8G），给 2G 余量，超了宁重启也不拖垮整机
MemoryMax=2G

[Install]
WantedBy=multi-user.target
EOF
	fi
	chmod 644 "$OCR_UNIT"
	systemctl daemon-reload
	c_ok "文档解析单元：$OCR_UNIT（端口 $OCR_PORT，内存上限 2G）"
fi

if [ "$DO_START" -eq 1 ]; then
	# 先起解析服务：主服务启动时会探测它，先起来能少一轮"解析服务不可用"
	if [ "$DO_OCR" -eq 1 ]; then
		systemctl enable "$SERVICE_NAME-ocr" >/dev/null 2>&1 || true
		systemctl restart "$SERVICE_NAME-ocr"
	fi
	systemctl enable "$SERVICE_NAME" >/dev/null 2>&1 || true
	systemctl restart "$SERVICE_NAME"
	c_ok "服务已启动（已设为开机自启）"
else
	c_warn "按 --no-start 要求，未启动服务"
fi

# ---------- 6. 等端口就绪 ----------
step "6/7" "等待服务就绪"

SELFTEST_RC=0
if [ "$DO_START" -eq 1 ]; then
	# 不依赖 curl/wget（离线机可能都没有），用 bash 自带的 /dev/tcp 探活
	wait_port() {
		local p="$1" label="$2" n="${3:-40}"
		local i=0
		while [ "$i" -lt "$n" ]; do
			if (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then
				exec 3<&- 2>/dev/null || true
				return 0
			fi
			i=$((i + 1))
			sleep 0.5
		done
		return 1
	}

	# 解析服务只等"端口在监听"，**不等模型加载完**：ocrd 的引擎是首次抽文本时才惰性加载的，
	# 在这里等它等于让用户对着一个卡住的安装界面等半分钟，而它根本不影响主服务启动。
	if [ "$DO_OCR" -eq 1 ]; then
		if wait_port "$OCR_PORT" "文档解析" 60; then
			c_ok "文档解析服务已监听 $OCR_PORT"
		else
			c_warn "文档解析服务 30 秒内没监听 $OCR_PORT（主服务仍可用，扫描件/Word/Excel 抽取会失败）"
			if command -v journalctl >/dev/null 2>&1; then
				journalctl -u "$SERVICE_NAME-ocr" -n 10 --no-pager 2>/dev/null | sed 's/^/      /' || true
			fi
		fi
	fi

	ready=0
	for _ in $(seq 1 40); do
		if (exec 3<>"/dev/tcp/127.0.0.1/$PORT") 2>/dev/null; then
			exec 3<&- 2>/dev/null || true
			ready=1
			break
		fi
		sleep 0.5
	done
	if [ "$ready" -eq 1 ]; then
		c_ok "端口 $PORT 已监听"
	else
		c_fail "等待 20 秒仍未监听 $PORT"
		# 直接把最后几行日志贴出来：常见原因（端口被占、PrivateTmp 导致的 203/EXEC、
		# 数据目录不可写）在日志里一眼可见，比让客户自己去翻 journalctl 高效得多。
		if command -v journalctl >/dev/null 2>&1; then
			c_info "最近日志："
			journalctl -u "$SERVICE_NAME" -n 15 --no-pager 2>/dev/null | sed 's/^/      /' || true
		fi
		c_info "看完整日志：journalctl -u $SERVICE_NAME -n 50 --no-pager"
		exit 1
	fi
else
	c_warn "未启动服务，跳过端口探活"
fi

# ---------- 7. 装后自检 ----------
step "7/7" "装后自检（离线、不联网、不调用 LLM）"

if [ "$DO_SELFTEST" -eq 0 ]; then
	c_warn "按 --skip-selftest 跳过了自检。客户环境请务必补跑一次：$BIN -selftest"
else
	# 用安全方式把 env 加载进当前 shell：逐行解析，绝不用 . 或 eval（值是密钥，不能当代码执行）
	while IFS='=' read -r k v; do
		case "$k" in
			SKILLFORGE_*) export "$k=$v" ;;
		esac
	done < "$ENV_FILE"

	set +e
	"$BIN" -selftest
	SELFTEST_RC=$?
	set -e
fi

printf '\n\033[1m────────────────────────────────────────────\033[0m\n'
if [ "$SELFTEST_RC" -eq 0 ]; then
	printf '\033[32m\033[1m安装完成\033[0m\n'
else
	printf '\033[33m\033[1m安装完成，但自检有未通过项（见上）\033[0m\n'
fi
printf '\033[1m────────────────────────────────────────────\033[0m\n'
c_info "访问地址 : http://<本机IP>:$PORT"
c_info "管理账号 : $ADMIN_USER"
if [ -z "$ADMIN_PASS" ] && [ -z "$OLD_ADMIN_PW" ]; then
	c_info "管理密码 : $FINAL_PW   ← 只显示这一次，请立刻记下"
else
	c_info "管理密码 : 与上次一致（未改动）"
fi
c_info "数据目录 : $DATA_DIR（数据库 skillforge.db 就在这里，备份 = 拷这个目录）"
c_info "配置文件 : $ENV_FILE"
c_info "看日志   : journalctl -u $SERVICE_NAME -f"
c_info "卸载     : sudo $HERE/uninstall.sh"
printf '\n'
if [ "$SELFTEST_RC" -ne 0 ]; then
	c_info "自检失败时先看「PDF 中文字体」一栏：它提示缺哪些字符，就换一个覆盖这些字符的 .ttf，"
	c_info "然后在 $ENV_FILE 里改 SKILLFORGE_PDF_FONT_FILE 并重启服务。"
fi
exit "$SELFTEST_RC"
