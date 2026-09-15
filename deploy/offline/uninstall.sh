#!/usr/bin/env bash
#
# SkillForge 卸载脚本
# ==================
#
# 对应 install.sh，把装进去的东西全部摘掉。默认**保留数据**（数据库、模板、上传文件），
# 因为客户卸载常常是为了重装/迁移，数据不该跟着消失。
#
# 用法：
#   sudo ./uninstall.sh                # 卸载程序与服务，保留数据目录
#   sudo ./uninstall.sh --purge        # 连数据一起删（会二次确认）
#   sudo ./uninstall.sh --purge --yes  # 连数据一起删，不再确认
#   sudo ./uninstall.sh --prefix /srv/sf --purge
#   sudo ./uninstall.sh --force        # 单元属于别的前缀时强拆（见下）
#   sudo ./uninstall.sh --no-ocr       # 不动文档解析服务（装了多份实例混用时用）
#
# 与 install.sh 用同一套参数名，参数不写就按默认值（改过前缀的机器一定要带 --prefix）。
# 如果 $SERVICE_NAME 的服务单元已存在、但指向的前缀跟本次不同，脚本会拒绝卸载——
# 因为那说明这个单元是另一份实例的，动手会把它停掉并删除单元文件，害它起不来。

set -euo pipefail

PREFIX=/opt/skillforge
SERVICE_NAME=skillforge
# 与 install.sh 对齐：字体目录按实例隔离，卸载只动自己那一份（Bug K）
FONT_DIR=""
FONT_DIR_SET=0
DATA_DIR=""
DO_PURGE=0
ASSUME_YES=0
# 停下来自别的前缀的同名单元需要显式 --force（见步骤 1 的占用检查）。
FORCE=0

SELF="${BASH_SOURCE[0]}"

c_info() { printf '  %s\n' "$*"; }
c_ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; }
c_warn() { printf '  \033[33m!\033[0m %s\n' "$*"; }
c_fail() { printf '  \033[31m✗\033[0m %s\n' "$*"; }
step()   { printf '\n\033[1m[%s]\033[0m %s\n' "$1" "$2"; }
die()    { c_fail "$*"; exit 1; }

usage() { sed -n '2,18p' "$SELF" | sed 's/^# \{0,1\}//'; exit 0; }

while [ $# -gt 0 ]; do
	case "$1" in
		--prefix)     PREFIX="${2:?--prefix 需要一个目录}"; shift 2 ;;
		--service|--service-name) SERVICE_NAME="${2:?}"; shift 2 ;;
		--data-dir)   DATA_DIR="${2:?}"; shift 2 ;;
		--font-dir)   FONT_DIR="${2:?}"; FONT_DIR_SET=1; shift 2 ;;
		--purge)      DO_PURGE=1; shift ;;
		--yes|-y)     ASSUME_YES=1; shift ;;
		--force)      FORCE=1; shift ;;
		--no-ocr)     DO_OCR=0; shift ;;
		-h|--help)    usage ;;
		*)            die "无法识别的参数：$1（用 --help 看用法）" ;;
	esac
done

[ -n "$DATA_DIR" ] || DATA_DIR="$PREFIX/data"
[ "$FONT_DIR_SET" -eq 1 ] || FONT_DIR="/usr/local/share/fonts/skillforge-$SERVICE_NAME"
ENV_FILE="$PREFIX/skillforge.env"
BIN="$PREFIX/skillforge"
UNIT="/etc/systemd/system/$SERVICE_NAME.service"
# 文档解析服务（ocrd）：install.sh 装在同一前缀的 bin/ 下，卸载必须一起清，
# 否则删了主服务却留着一个吃 2G 内存上限的解析进程在监听端口（残留得很难看，
# 而且下次装的时候 8093 被自己占着 → 又被端口冲突拦一次）。
OCR_BIN="$PREFIX/bin/ocrd"
OCR_UNIT="/etc/systemd/system/$SERVICE_NAME-ocr.service"
DO_OCR=1

printf '\033[1mSkillForge 卸载\033[0m\n'
c_info "安装前缀 : $PREFIX"
c_info "服务名   : $SERVICE_NAME"
c_info "数据目录 : $DATA_DIR（$([ "$DO_PURGE" -eq 1 ] && echo '本次一并删除' || echo '本次保留'))"

[ "$(id -u)" -eq 0 ] || die "需要用 root 运行：sudo $SELF"

# ---------- 1. 停服务 ----------
step "1/4" "停止并注销服务"

# ---------- 1.0 单元占用检查（Bug M，卸载侧的对称防护）----------
# 卸载比安装更危险：这里会**真的 stop + disable + 删单元文件**。如果不看一眼这个单元
# 到底属于谁，那么在装了多份实例的机器上跑一次默认参数的卸载，就会把别人那份正在
# 对外服务的实例直接停掉、单元删掉——程序文件还在、端口没了、开机也不自启，
# 而且报错信息是「服务已停止并取消开机自启」，看起来像卸载成功。
# 所以：单元存在、且它指向的前缀不是本次要卸的前缀 → 拒绝，除非 --force。
if [ -f "$UNIT" ]; then
	EXIST_PREFIX="$(sed -n 's|^[[:space:]]*WorkingDirectory=||p' "$UNIT" | head -n 1)"
	if [ -n "$EXIST_PREFIX" ] && [ "$EXIST_PREFIX" != "$PREFIX" ]; then
		if [ "$FORCE" -ne 1 ]; then
			c_fail "服务单元「$UNIT」属于另一个实例，不是本次要卸载的前缀："
			c_fail "  单元指向：$EXIST_PREFIX"
			c_fail "  本次卸载：$PREFIX"
			c_fail "继续下去会把「$EXIST_PREFIX」那份正在跑的服务停掉并删除单元文件，"
			c_fail "而它自己的程序文件、数据都还留在原地——等于把它打成既不自启也起不来的残废状态。"
			c_fail "要卸它：加 --prefix $EXIST_PREFIX"
			c_fail "确认就是要强拆这个单元：加 --force"
			exit 1
		fi
		c_warn "按 --force 强拆不属于「$PREFIX」的单元：$EXIST_PREFIX"
	fi
fi

# 单元是否被 systemd 认识。
# 注意：**不能**写成 `systemctl list-unit-files | grep -q "^$SERVICE_NAME\.service"`。
# grep -q 命中即退出 → systemctl 收到 SIGPIPE → 在 set -o pipefail 下整条管道被判失败，
# 检测恒为假，stop/disable 全被跳过：卸载完程序文件删了、服务却还在跑还占着端口。
# 这里改成不带管道的写法（给 systemctl 一个单元名参数，输出就一两行）。
unit_known() {
	[ -f "$UNIT" ] && return 0
	command -v systemctl >/dev/null 2>&1 || return 1
	local out
	out="$(systemctl list-unit-files --no-legend --no-pager "$SERVICE_NAME.service" 2>/dev/null || true)"
	[ -n "$out" ]
}

if command -v systemctl >/dev/null 2>&1; then
	# 无条件先停一次：除了「正常在跑」之外，还有一种状态必须覆盖——
	# 服务处于崩溃重启循环（activating）时 is-active 为假、list-unit-files 也可能查不到，
	# 但 Restart=always 会让它一直重启。停一次是幂等的，没装过的机器上失败也无所谓。
	systemctl stop "$SERVICE_NAME" >/dev/null 2>&1 || true
	systemctl disable "$SERVICE_NAME" >/dev/null 2>&1 || true
	systemctl reset-failed "$SERVICE_NAME" >/dev/null 2>&1 || true
	if unit_known; then
		c_ok "服务已停止并取消开机自启"
	else
		c_info "没找到 $SERVICE_NAME.service（可能本来就没装成服务）"
	fi
else
	c_warn "这台机器没有 systemctl，跳过服务处理"
fi

if [ -f "$UNIT" ]; then
	rm -f "$UNIT"
	command -v systemctl >/dev/null 2>&1 && systemctl daemon-reload
	c_ok "已删除服务单元：$UNIT"
else
	c_info "没有服务单元文件需要删除"
fi

# ---------- 1.5 文档解析服务（ocrd）----------
# 与主服务分开处理，理由：它可能本来就没装（--no-ocr 安装 / 旧版离线包不带），
# 也可能属于另一份实例（多实例同机）。判据用 ExecStart 里的路径，不看文件名 ——
# 同机两份实例的单元名不可能相同（名字里带 --service），但为了不误删别人家的，
# 还是按"这个单元到底在启动哪个前缀下的二进制"来判断。
if [ "$DO_OCR" -eq 1 ] && [ -f "$OCR_UNIT" ]; then
	OCR_EXEC="$(sed -n 's|^[[:space:]]*ExecStart=||p' "$OCR_UNIT" | head -n 1 | awk '{print $1}')"
	case "$OCR_EXEC" in
		"$PREFIX"/*) ;;
		"")
			c_warn "单元 $OCR_UNIT 里读不到 ExecStart，跳过（请人工确认后手动删）"
			;;
		*)
			if [ "$FORCE" -ne 1 ]; then
				c_fail "文档解析单元「$OCR_UNIT」启动的是另一个前缀的程序：$OCR_EXEC"
				c_fail "本次要卸的是 $PREFIX。继续会把别人的解析服务停掉。"
				c_fail "确认要强拆：加 --force；只是想跳过它：加 --no-ocr"
				exit 1
			fi
			c_warn "按 --force 强拆文档解析单元（它启动的是 $OCR_EXEC）"
			;;
	esac
	if command -v systemctl >/dev/null 2>&1; then
		systemctl stop "$SERVICE_NAME-ocr" >/dev/null 2>&1 || true
		systemctl disable "$SERVICE_NAME-ocr" >/dev/null 2>&1 || true
		systemctl reset-failed "$SERVICE_NAME-ocr" >/dev/null 2>&1 || true
	fi
	rm -f "$OCR_UNIT"
	command -v systemctl >/dev/null 2>&1 && systemctl daemon-reload
	c_ok "文档解析服务已停止并删除单元：$OCR_UNIT"
elif [ "$DO_OCR" -eq 1 ]; then
	c_info "没有文档解析单元需要删除"
fi

# ---------- 2. 删程序与配置 ----------
step "2/4" "删除程序与配置"

removed=0
for f in "$BIN" "$BIN.new" "$ENV_FILE" "$OCR_BIN" "$OCR_BIN.new"; do
	if [ -e "$f" ]; then
		rm -f "$f"
		c_ok "已删除 $f"
		removed=1
	fi
done
# bin/ 是给解析服务用的子目录：里面空了就顺手删掉（非空说明有用户自己放的东西，留着）
if [ -d "$PREFIX/bin" ] && [ -z "$(ls -A "$PREFIX/bin" 2>/dev/null)" ]; then
	rmdir "$PREFIX/bin"
	c_ok "已删除空的 $PREFIX/bin"
fi

# 自带 Python 运行时（install.sh 从包里拷到 $PREFIX/python）：整整一个运行时，
# 不删就永远留在客户机上（几百 MB）。它是程序文件、不是数据，所以 --purge 之外也要清。
PY_DIR="$PREFIX/python"
if [ -d "$PY_DIR" ]; then
	# 目录串按整段锚定，别用子串：$PREFIX/python-old 这类名字不该被这里匹配到。
	case "$PY_DIR" in
		/*/python) ;;
		*) c_warn "跳过非常规解释器目录（名字不以 /python 结尾）：$PY_DIR" ; PY_DIR="" ;;
	esac
fi
if [ -n "${PY_DIR:-}" ] && [ -d "$PY_DIR" ]; then
	rm -rf "$PY_DIR"
	c_ok "已删除自带的 Python 运行时：$PY_DIR"
fi
[ "$removed" -eq 0 ] && c_info "前缀目录里本来就没有程序文件"

# 把目录转成「整段路径」匹配用的正则：
# 路径必须按整段比对，不能用子串——/usr/local/share/fonts/skillforge 是
# /usr/local/share/fonts/skillforge-<实例名> 的前缀，子串比对会把别的实例的字体
# 目录误判成本目录仍被引用 → 旧的共享目录永远清不掉（Bug O）。
# 边界规则：目录串后面只允许出现 /（目录内文件）、引号、空白或行尾；
# 紧跟字母/数字/点/连字符都说明那是另一个更长的名字，不算引用。
font_ref_regex() {
	printf '%s([^A-Za-z0-9._-]|$)' "$(printf '%s' "$1" | sed 's/[][\\.^$*+?(){}|]/\\&/g')"
}

# 同机其它实例是否还在引用某个字体目录？
# 只报事实（有没有、谁引用），不猜、不擅自删。
font_dir_in_use() {
	_ref_dir="$1"
	_hits=""
	for _f in /opt/*/skillforge.env /srv/*/skillforge.env /home/*/skillforge.env \
		/var/lib/*/skillforge.env /usr/local/*/skillforge.env \
		/etc/systemd/system/*.service; do
		[ -f "$_f" ] || continue
		[ "$_f" = "$ENV_FILE" ] && continue
		[ "$_f" = "$UNIT" ] && continue
		_hits="$_hits$(grep -E -- "$(font_ref_regex "$_ref_dir")" "$_f" 2>/dev/null || true)"
	done
	[ -n "$_hits" ]
}

# 字体只删我们自己装的那一份（独立子目录，绝不碰系统字体目录里的其它文件）
if [ -d "$FONT_DIR" ] && [ "$FONT_DIR" != "/usr" ] && [ "$FONT_DIR" != "/usr/share" ] && [ "$FONT_DIR" != "/usr/share/fonts" ]; then
	if font_dir_in_use "$FONT_DIR"; then
		c_warn "字体目录 $FONT_DIR 仍被同机其它实例引用，保留不删"
	else
		rm -rf "$FONT_DIR"
		c_ok "已删除自带字体目录 $FONT_DIR"
		command -v fc-cache >/dev/null 2>&1 && fc-cache -f >/dev/null 2>&1 || true
		c_info "已刷新 fontconfig 缓存（系统其它字体未受影响）"
	fi
else
	c_warn "字体目录 $FONT_DIR 看起来是系统目录，跳过删除"
fi

# 旧版本（≤ v0.3.1）把字体装在所有实例共享的 /usr/local/share/fonts/skillforge 里。
# 那种布局下卸载任一实例都会把别人正在用的字体删掉（Bug K），所以这里只在确认
# 本机再没有别的实例引用它时才清理，否则留着——多留几 MB 好过让别人的 PDF 静默变空白。
LEGACY_FONT_DIR=/usr/local/share/fonts/skillforge
if [ -d "$LEGACY_FONT_DIR" ] && [ "$FONT_DIR" != "$LEGACY_FONT_DIR" ]; then
	if font_dir_in_use "$LEGACY_FONT_DIR"; then
		c_warn "旧版共享字体目录 $LEGACY_FONT_DIR 仍被其它实例引用，保留"
	else
		rm -rf "$LEGACY_FONT_DIR"
		c_ok "已清理旧版遗留的共享字体目录 $LEGACY_FONT_DIR（本机已无实例引用）"
	fi
fi

# ---------- 3. 数据目录 ----------
step "3/4" "数据目录"

if [ ! -d "$DATA_DIR" ]; then
	c_info "没有数据目录 $DATA_DIR"
elif [ "$DO_PURGE" -eq 1 ]; then
	if [ "$ASSUME_YES" -ne 1 ]; then
		printf '  \033[33m!\033[0m 即将永久删除 %s（数据库、技能、模板、上传文件都在里面）\n' "$DATA_DIR"
		printf '  确认删除请输 yes：'
		read -r ans
		[ "$ans" = "yes" ] || die "已取消，数据未删除。不带 --purge 重跑可只卸载程序。"
	fi
	rm -rf "$DATA_DIR"
	c_ok "已删除数据目录 $DATA_DIR"
else
	c_info "已保留数据目录：$DATA_DIR"
	c_info "想彻底清掉：sudo $SELF --prefix $PREFIX --purge"
fi

# 前缀目录本身：空了就顺手删掉，非空就留着并告诉用户里面还有什么
if [ -d "$PREFIX" ]; then
	if [ -z "$(ls -A "$PREFIX" 2>/dev/null)" ]; then
		rmdir "$PREFIX"
		c_ok "已删除空的前缀目录 $PREFIX"
	else
		c_info "前缀目录 $PREFIX 里还有内容，保留不动："
		ls -A "$PREFIX" | sed 's/^/      /'
	fi
fi

# ---------- 4. 收尾核对 ----------
step "4/4" "收尾核对"

leftover=0
for f in "$BIN" "$ENV_FILE" "$UNIT" "$FONT_DIR" "$OCR_BIN" "$OCR_UNIT"; do
	if [ -e "$f" ]; then
		c_warn "仍然存在：$f"
		leftover=1
	fi
done
# 崩溃重启循环（activating）也算残留：只查 is-active 会放过它
if command -v systemctl >/dev/null 2>&1; then
	for svc in "$SERVICE_NAME" "$SERVICE_NAME-ocr"; do
		state="$(systemctl is-active "$svc" 2>/dev/null || true)"
		case "$state" in
			active|activating|reloading)
				c_warn "服务 $svc 仍在运行（状态：$state）"
				leftover=1 ;;
		esac
	done
fi

printf '\n'
if [ "$leftover" -eq 0 ]; then
	printf '\033[32m\033[1m卸载完成，无残留\033[0m\n'
else
	printf '\033[33m\033[1m卸载完成，但有上面列出的残留项\033[0m\n'
fi
