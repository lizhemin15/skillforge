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
#   sudo ./install.sh --prefix /srv/sf      # 换安装前缀
#   sudo ./install.sh --service sf-test     # 换服务名（同一台机器装第二份实例用）
#   sudo ./install.sh --force               # 顶掉同名的、别的前缀的既有实例（危险，见下）
#   sudo ./install.sh --skip-selftest       # 跳过装后自检（仅开发调试用）
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
		--data-dir)    DATA_DIR="${2:?--data-dir 需要一个目录}"; shift 2 ;;
		--admin-user)  ADMIN_USER="${2:?}"; shift 2 ;;
		--admin-pass)  ADMIN_PASS="${2:?}"; shift 2 ;;
		--font-dir)    FONT_DIR="${2:?}"; FONT_DIR_SET=1; shift 2 ;;
		--public-url)  PUBLIC_URL="${2:?}"; shift 2 ;;
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
BUNDLED_FONT=""

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
	command -v ss >/dev/null 2>&1 || return 1
	local out
	out="$(ss -ltn 2>/dev/null || true)"
	case "$out" in
		*":$PORT "*) return 0 ;;
		*)           return 1 ;;
	esac
}

# 注意：这里**不能**写成 `ss | awk | grep -q`。grep -q 命中即退出会让上游进程收到 SIGPIPE，
# 在 set -o pipefail 下整条管道被判失败 → 端口占用检测恒为「空闲」，冲突被静默放过。
if port_in_use; then
	if ! systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null; then
		die "端口 $PORT 已被别的进程占用（且不是本服务）。换端口：sudo $SELF --port 9000"
	fi
	c_warn "端口 $PORT 已被本服务占用——按「升级」处理"
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

if [ "$DO_START" -eq 1 ]; then
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
