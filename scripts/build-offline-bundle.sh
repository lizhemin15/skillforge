#!/usr/bin/env bash
#
# build-offline-bundle.sh —— 把二进制 + 一键安装脚本 + 中文字体 + 便携 Python 打成一个「拷走就能装」的 tar.gz
#
# 为什么需要它：客户机器常常是内网、没外网、也没装中文字体、**连 python3 都没有**
# （最小安装、无本地源，也装不上）。分发包里必须自带
#   1) 编译好的二进制（不依赖客户装 Go）
#   2) install.sh / uninstall.sh / systemd 模板（一条命令装完）
#   3) 一个覆盖「中文 + 数字 + 拉丁」的 .ttf（否则 PDF 里的数字会静默变空白，见 Bug G）
#   4) 一份便携 CPython（否则内网机器装完「代码执行沙箱」必红，AI 跑不了代码）
#
# 用法（本地手工打）：
#   scripts/build-offline-bundle.sh --version v0.3.0 --arch amd64 --binary ./skillforge-linux-amd64 \
#     --ocr ./ocrd-linux-amd64 --python-runtime /path/to/cpython-<ver>-linux-x86_64-install_only_stripped.tar.gz
# 取便携运行时：scripts/fetch-python-runtime.sh --arch amd64 --outdir /tmp
# CI 里由 .github/workflows/release.yml 调用，逐架构产出并附到 Release。
#
# 输出：dist/skillforge-<version>-offline-linux-<arch>.tar.gz（含同名 .sha256）
#
# 设计要点：
#   - **确定性**：固定 mtime、固定属主、按名字排序打包、gzip -n，同样的输入产出同样的字节
#     （这样 Release 里的 sha256 才有可比性，也便于验证「我下的包没被改过」）
#   - **不联网**：字体从本机已装的包里拿、Python 运行时由调用方先下好传进来（--python-runtime），
#     打包脚本自己不做 apt install / 不下载
#   - 缺字体时**直接失败**而不是打一个残包：残包在客户机上表现为「PDF 里数字全没了」，
#     属于最难排查的那类故障（打开 PDF 看不出错，只是数据没了）

set -euo pipefail

VERSION=""
ARCH="$(go env GOARCH 2>/dev/null || uname -m)"
BINARY=""
OUTDIR="dist"
FONT_FILE=""
OCR_BIN=""
NO_OCR=""
NO_PY_RUNTIME=""
PY_RUNTIME=""
SOURCE_DATE=""

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

c_info() { printf '  %s\n' "$*"; }
c_ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; }
c_fail() { printf '  \033[31m✗\033[0m %s\n' "$*"; }
# c_warn 曾经漏定义：唯一调用点在 --no-ocr 分支里，而 amd64 永远走的是带 ocr 的分支，
# 于是这个分支在 CI 里从没被执行过 —— 直到 arm64 用它，报 "c_warn: command not found"
# （exit 127），把 arm64 离线包 job 打红 → needs 不满足 → 「创建 GitHub Release」被跳过
# → v0.3.21/v0.3.22/v0.3.23 连续三个 tag 一个 Release 产物都没有。
# 教训：没被执行过的分支就是坏的分支。现在 amd64 job 会额外跑一次 --no-ocr 冒烟。
c_warn() { printf '  \033[33m!\033[0m %s\n' "$*"; }
die()    { c_fail "$*" >&2; exit 1; }

usage() {
	# 行号必须跟着头部注释走：注释是给人看的用法说明，sed 范围一旦落后，
	# --help 就会把说明截断一半（或者把代码打出来）—— 加了 --python-runtime 说明后
	# 注释从 26 行变成 27 行。
	sed -n '2,27p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
	exit 0
}

while [ $# -gt 0 ]; do
	case "$1" in
		--version) VERSION="${2:?--version 需要值，如 v0.3.0}"; shift 2 ;;
		--arch)    ARCH="${2:?--arch 需要值，如 amd64/arm64}"; shift 2 ;;
		--binary)  BINARY="${2:?--binary 需要值，指向已编译的 linux 二进制}"; shift 2 ;;
		--ocr)     OCR_BIN="${2:?--ocr 需要值，指向已编译好的 linux ocrd}"; shift 2 ;;
		--no-ocr)  NO_OCR=1; shift ;;
		--python-runtime) PY_RUNTIME="${2:?--python-runtime 需要值，指向便携 CPython 的 .tar.gz}"; shift 2 ;;
		--no-python-runtime) NO_PY_RUNTIME=1; shift ;;
		--outdir)  OUTDIR="${2:?}"; shift 2 ;;
		--font)    FONT_FILE="${2:?}"; shift 2 ;;
		--source-date) SOURCE_DATE="${2:?}"; shift 2 ;;
		-h|--help) usage ;;
		*)         die "无法识别的参数：$1（用 --help 看用法）" ;;
	esac
done

[ -n "$VERSION" ] || die "必须指定 --version（会写进包名和自检输出）"
[ -n "$BINARY" ] || die "必须指定 --binary（指向已编译好的 linux/$ARCH 二进制）"
[ -f "$BINARY" ] || die "找不到二进制：$BINARY"
case "$ARCH" in
	amd64|arm64) ;;
	*) die "--arch 只支持 amd64 / arm64（离线包面向 linux 服务器），收到：$ARCH" ;;
esac

# 用文件内容判定架构，不靠文件名——文件名是 CI 里拼出来的，猜错会把 arm64 的包装成 amd64
BIN_DESC="$(file -bL "$BINARY" 2>/dev/null || true)"
case "$BIN_DESC" in
	*ARM\ aarch64*) BIN_ARCH=arm64 ;;
	*x86-64*)       BIN_ARCH=amd64 ;;
	*)              BIN_ARCH="" ;;
esac
if [ -n "$BIN_ARCH" ] && [ "$BIN_ARCH" != "$ARCH" ]; then
	die "二进制架构与 --arch 不符：--arch=$ARCH，但文件是 $BIN_ARCH（$BIN_DESC）"
fi
c_ok "二进制架构核对通过：$ARCH（$(du -h "$BINARY" | cut -f1)）"

# ocrd（文档解析服务）架构核对：跟主二进制同一套规矩 —— 用文件内容判定，不靠文件名。
# 装错架构的 ocrd 在目标机上表现为「Exec format error」→ 解析服务起不来 →
# 用户看到的是"扫描件抽不出文本"，而不是"包打错了"，极难往回追。
if [ -n "$OCR_BIN" ]; then
	[ -f "$OCR_BIN" ] || die "找不到 ocrd：$OCR_BIN"
	OCR_DESC="$(file -bL "$OCR_BIN" 2>/dev/null || true)"
	case "$OCR_DESC" in
		*ARM\ aarch64*) OCR_ARCH=arm64 ;;
		*x86-64*)       OCR_ARCH=amd64 ;;
		*)              OCR_ARCH="" ;;
	esac
	if [ -n "$OCR_ARCH" ] && [ "$OCR_ARCH" != "$ARCH" ]; then
		die "ocrd 架构与 --arch 不符：--arch=$ARCH，但文件是 $OCR_ARCH（$OCR_DESC）"
	fi
	c_ok "ocrd 架构核对通过：$ARCH（$(du -h "$OCR_BIN" | cut -f1)）"

	# 构建基线说明必须跟 ocrd 一起进包：主程序 `-diag` 与 install.sh 的装前体检都读它，
	# 用来说清「这份 ocrd 要求 glibc ≥X，本机是多少」。它是「产物起不来」时唯一能给出
	# 结论的东西 —— 缺了它，客户只能看到一句加载器报错，接着去删缓存 / 重装 / 满世界
	# 找那个 .so（都不会有用）。build-ocr.sh 本来就是随产物一起生成它的，所以这里缺了
	# 说明**出包流程错了**，不是「这台机器没这个文件」：直接 die，不许沉默地把包内体检
	# 能力降级成永远报「读不到基线」。
	#
	# 位置：放在**搭目录之前**（跟架构核对同一段）。它只是对入参的判据，
	# 真跑验证过：原来放在 install 之后，die 会留下一个名字完整、内容残缺的
	# 包目录（dist/x/skillforge-offline-v…-linux-amd64/），任谁看一眼都会以为
	# 「包已经打好了，只是 tar 那步失败了」——失败就必须什么都不留下。
	[ -s "$OCR_BIN.baseline" ] || die "ocrd 旁边没有构建基线说明：$OCR_BIN.baseline（build-ocr.sh 随产物生成；缺了包内体检报不出基线）"
	grep -q '^glibc=' "$OCR_BIN.baseline" || die "基线说明里没有 glibc 行：$OCR_BIN.baseline"
elif [ -n "$NO_OCR" ]; then
	c_warn "按 --no-ocr 打一个不含文档解析服务的包（扫描件/Office 抽文本会不可用）"
else
	# 默认**必须**带：用户拿到的包如果只缺它，装完才发现 = 一次无效交付。
	# 真要打不带 ocr 的包，显式加 --no-ocr。
	die "没有 --ocr 指定 ocrd 二进制。离线包默认必须带文档解析服务；确实不要请显式加 --no-ocr"
fi

# ---------- 便携 Python 运行时（默认必带）----------
#
# 为什么必带：客户机器常是内网最小安装 —— 没有 python3，而且**装不了**（没有源、
# 没有 ISO 可挂）。旧版把「目标机得先有 python3」写成前置要求，等于把「一键安装」
# 拆成「你先想办法弄个 python 去」，用户报的现场就是这个。
# 现在包里自带一份便携 CPython（python-build-standalone 的 install_only 包），
# install.sh 装的时候拷到 $PREFIX/python 并钉进配置，目标机完全不用预装。
if [ -z "$PY_RUNTIME" ]; then
	if [ -n "$NO_PY_RUNTIME" ]; then
		# 与 --no-ocr 同样处理：没被执行过的分支就是坏的分支 —— CI 里会显式跑一次
		# 这个分支冒烟，省得它哪天把某个 job 打红。
		c_warn "按 --no-python-runtime 打一个不带解释器的包（内网最小安装的机器装完 AI 跑不了代码）"
	else
		die "没有 --python-runtime 指定便携 Python 运行时。离线包默认必须自带解释器（否则内网最小安装的机器装完「代码执行沙箱」必红）；确实不要请显式加 --no-python-runtime"
	fi
fi

PY_VERSION=""
PY_RT_HOME=""
if [ -n "$PY_RUNTIME" ]; then
	[ -f "$PY_RUNTIME" ] || die "找不到便携 Python 运行时：$PY_RUNTIME"

	# 解到临时目录再定位，不写死层数：上游 install_only 包目前是 python/ 一层，
	# 但上游改布局时这里不该跟着改 —— 用「bin/python3 在哪」来定位。
	PY_RT_TMP="$(mktemp -d "${TMPDIR:-/tmp}/pyrt.XXXXXX")"
	trap 'rm -rf "$PY_RT_TMP"' EXIT
	tar -xzf "$PY_RUNTIME" -C "$PY_RT_TMP"
	# 找解释器：先认 bin/python3，退回版本化的 bin/python3.12。
	# ⚠️ 这个坑踩过一次：python-build-standalone 的 install_only 包里**只有**
	# python/bin/python3.12 这个真文件，bin/python3 常常压根不存在 —— 用
	# `-type f -name python3` 找会命中 0 次，脚本报「不是 install_only 包」，
	# 其实只是找法不对。同时排除 *-config（那是脚本，不是解释器）。
	PY_RT_BIN="$({ cd "$PY_RT_TMP" && find . -maxdepth 4 \( -type f -o -type l \) \
		\( -name 'python3' -o -name 'python3.[0-9]*' \) -path '*/bin/*' -print 2>/dev/null; } \
		| grep -v -- '-config$' | sort | head -n 1 || true)"
	[ -n "$PY_RT_BIN" ] || die "$PY_RUNTIME 里找不到 bin/python3* —— 这不是 python-build-standalone 的 install_only 包？"
	PY_RT_BIN="$PY_RT_TMP/${PY_RT_BIN#./}"
	PY_RT_HOME="$(cd "$(dirname "$PY_RT_BIN")/.." && pwd)"

	# 架构核对：用文件内容判定，不靠文件名（跟主二进制、ocrd 同一套规矩）。
	# 装错架构的运行时在目标机上表现为「代码执行沙箱失败」，用户根本看不出是包打错了。
	PY_DESC="$(file -bL "$PY_RT_BIN" 2>/dev/null || true)"
	case "$PY_DESC" in
		*ARM\ aarch64*) PY_RT_ARCH=arm64 ;;
		*x86-64*)       PY_RT_ARCH=amd64 ;;
		*)              PY_RT_ARCH="" ;;
	esac
	if [ -n "$PY_RT_ARCH" ] && [ "$PY_RT_ARCH" != "$ARCH" ]; then
		die "自带 Python 架构与 --arch 不符：--arch=$ARCH，但运行时是 $PY_RT_ARCH（$PY_DESC）"
	fi

	# 版本：同架构时真跑一次 `-V` 取（最准），跨架构只能从目录名推（lib/python3.12）。
	# 取不到不算错，但要留痕 —— VERSION 里 python=unknown 时能看出是这里没拿到。
	case "$(uname -m)" in
		aarch64|arm64) PY_HOST_ARCH=arm64 ;;
		x86_64|amd64)  PY_HOST_ARCH=amd64 ;;
		*)             PY_HOST_ARCH="" ;;
	esac
	if [ "$PY_HOST_ARCH" = "$ARCH" ]; then
		PY_VERSION="$("$PY_RT_BIN" -V 2>&1 | awk '{print $2}')" || true
		"$PY_RT_BIN" -c 'import json,ssl,sqlite3,hashlib' >/dev/null 2>&1 \
			|| die "自带 Python 跑不起基础模块（json/ssl/sqlite3/hashlib）—— 这份运行时是残件，别打进包"
	fi
	if [ -z "$PY_VERSION" ]; then
		PY_VERSION="$(cd "$PY_RT_HOME/lib" 2>/dev/null && ls -d python3.* 2>/dev/null | head -n 1 | sed 's/^python//')" || true
		[ -n "$PY_VERSION" ] || PY_VERSION="unknown"
	fi
	c_ok "自带 Python 运行时核对通过：$PY_VERSION / $ARCH（$(du -sh "$PY_RT_HOME" | cut -f1)）"
fi

# 版本核对：install.sh 的装后自检会核对版本号是「构建期注入」还是 "dev"，用来证明
# 客户跑的是 CI 产物。把 dev 二进制打进离线包 = 客户按一键安装装完，自检必然红，
# 整个「一键安装」体验当场崩掉，而且从包名上看不出来。所以在打包这一步就拦死。
HOST_ARCH="$(uname -m)"
case "$HOST_ARCH" in
	x86_64)  HOST_ARCH=amd64 ;;
	aarch64) HOST_ARCH=arm64 ;;
esac
if [ -n "$BIN_ARCH" ] && [ "$BIN_ARCH" = "$HOST_ARCH" ]; then
	# -version 输出形如「skillforge v0.3.1 (commit abc, built ...)」，取第 2 个字段严格比对——
	# 不做前缀 glob 匹配：v0.3.1 会误匹配 v0.3.10。
	BIN_VER_LINE="$("$BINARY" -version 2>/dev/null || true)"
	set -- $BIN_VER_LINE
	BIN_VER="${2:-}"
	if [ "$BIN_VER" = "$VERSION" ]; then
		c_ok "版本核对通过：$BIN_VER"
	else
		die "二进制自报版本「${BIN_VER:-（执行失败：$BIN_VER_LINE）}」≠ --version=$VERSION。多半是 go build 手搓（-ldflags 没注入）或 -X 变量路径写错。正确注入：-X github.com/lizhemin15/skillforge/internal/version.Version=$VERSION（见 .github/workflows/release.yml）。dev 二进制打进离线包，客户装完自检必红。"
	fi
else
	c_info "跳过版本核对：$ARCH 二进制无法在 $HOST_ARCH 上执行；装后自检会在目标机上核这一条"
fi

# ---------- 1. 挑字体 ----------
# 必须同时覆盖：中文、ASCII 数字 0-9、拉丁字母、半角标点。
# 实测（Debian/Ubuntu 包）：
#   gbsn00lp.ttf（文鼎宋体）        覆盖全 ← 选它
#   DroidSansFallbackFull.ttf       **不含 0-9 和拉丁**（纯 CJK fallback），选了等于自残
CANDIDATES=(
	"$FONT_FILE"
	/usr/share/fonts/truetype/arphic-gbsn00lp/gbsn00lp.ttf
	/usr/share/fonts/truetype/arphic-gkai00mp/gkai00mp.ttf
	/usr/share/fonts/truetype/unifont/unifont.ttf
	/usr/local/share/fonts/skillforge/gbsn00lp.ttf
)

# font_missing：直接解析字体自带的 cmap 表，列出它缺哪些必需字符。
# 为什么要自己解 cmap：不能靠「文件名带 CJK 就当它能用」——DroidSansFallbackFull.ttf
# 是纯 CJK fallback，cmap 里连 ASCII 数字都没有，一旦装进包里，客户机器上所有 PDF
# 的数量/金额都会变成空白（Bug G）。覆盖率只能实测。
font_missing() {
	python3 - "$1" <<'PY'
import sys, struct

need = [ord(c) for c in "0123456789ABCXYZabcxyz.,:%()\uffe5$"
        "\u4ea7\u54c1\u62a5\u4ef7\u5355\u6570\u91cf\u5355\u4ef7\u5c0f\u8ba1\u5408\u8ba1\u91d1\u989d\u5143\u5e74\u6708\u65e5"
        "\u5408\u540c\u7532\u4e59\u53cc\u65b9\u7b7e\u7f72\u7f16\u59d3\u540d\u5730\u5740\u7535\u8bdd"]

data = open(sys.argv[1], "rb").read()
if len(data) < 12:
    sys.exit(2)
num_tables = struct.unpack(">H", data[4:6])[0]
cmap_off = None
for i in range(num_tables):
    off = 12 + i * 16
    if data[off:off + 4] == b"cmap":
        cmap_off = struct.unpack(">I", data[off + 8:off + 12])[0]
        break
if cmap_off is None:
    sys.exit(2)

n = struct.unpack(">H", data[cmap_off + 2:cmap_off + 4])[0]
best = None
for i in range(n):
    rec = cmap_off + 4 + i * 8
    pid, eid, off = struct.unpack(">HHI", data[rec:rec + 8])
    if (pid, eid) in ((3, 10), (0, 4), (0, 6)):
        best = cmap_off + off
        break
    if (pid, eid) in ((3, 1), (0, 3)) and best is None:
        best = cmap_off + off
if best is None:
    sys.exit(2)

fmt = struct.unpack(">H", data[best:best + 2])[0]
covered = set()
if fmt == 4:
    segx2 = struct.unpack(">H", data[best + 6:best + 8])[0]
    seg = segx2 // 2
    ends = struct.unpack(">" + "H" * seg, data[best + 14:best + 14 + segx2])
    starts = struct.unpack(">" + "H" * seg, data[best + 16 + segx2:best + 16 + 2 * segx2])
    for s, e in zip(starts, ends):
        if s == 0xFFFF and e == 0xFFFF:
            continue
        covered.update(range(s, e + 1))
elif fmt == 12:
    ngroups = struct.unpack(">I", data[best + 12:best + 16])[0]
    for i in range(ngroups):
        r = best + 16 + i * 12
        s, e, _ = struct.unpack(">III", data[r:r + 12])
        covered.update(range(s, e + 1))
else:
    # 只认最常见的 4 / 12 号子表；其它格式（如 14 号变体选择器）不作为主表，
    # 这种情况宁可判「不达标」让脚本换下一个候选，也不要冒险接受。
    print("unrecognized cmap subtable format %d" % fmt)
    sys.exit(2)

missing = [c for c in need if c not in covered]
if missing:
    print("缺 %d 个必需字符：%s" % (
        len(missing), " ".join("U+%04X(%s)" % (c, chr(c)) for c in missing[:16])))
    sys.exit(1)
PY
}

font_pick=""
for f in "${CANDIDATES[@]}"; do
	[ -n "$f" ] || continue
	[ -f "$f" ] || continue
	# 只认 .ttf：gopdf 读不了 .ttc（集合）和 .otf（CFF 轮廓）
	case "$f" in *.ttf) ;; *) c_info "跳过非 .ttf：$f"; continue ;; esac
	if MISS_OUT="$(font_missing "$f")"; then
		font_pick="$f"
		break
	fi
	# 显式 --font 指定的字体不达标就硬失败：静默换别的字体，用户会以为「我指定的
	# 生效了」，直到客户机器上 PDF 数字变空白才发现不是。
	if [ -n "$FONT_FILE" ] && [ "$f" = "$FONT_FILE" ]; then
		c_fail "指定的字体覆盖不足，拒绝出包：$f"
		printf '%s\n' "$MISS_OUT" | sed 's/^/      /'
		die "换一个同时覆盖中文+数字+拉丁的 .ttf，或去掉 --font 让脚本自动挑"
	fi
	c_info "覆盖不足，跳过：$f"
	printf '%s\n' "$MISS_OUT" | sed 's/^/      /'
done

[ -n "$font_pick" ] || die "本机找不到同时覆盖中文+数字+拉丁的 .ttf 字体。装一个再重试：apt-get install fonts-arphic-gbsn00lp（或 --font 指定路径）"
c_ok "字体：$font_pick（$(du -h "$font_pick" | cut -f1)）"

# ---------- 2. 搭目录 ----------
DIST_ABS="$OUTDIR"
[ "${OUTDIR#/}" = "$OUTDIR" ] && DIST_ABS="$REPO_ROOT/$OUTDIR"
STAGE_NAME="skillforge-offline-$VERSION-linux-$ARCH"
STAGE="$DIST_ABS/$STAGE_NAME"
rm -rf "$STAGE"
mkdir -p "$STAGE/bin" "$STAGE/fonts" "$STAGE/licenses"

install -m 0755 "$BINARY" "$STAGE/bin/skillforge"
# 用 if 而不是 `[ -n ] && install`：后者在 OCR_BIN 为空时整条列表状态非零，
# 跟 set -e 的交互容易被误读（有的 shell 布局下会中断打包）。
if [ -n "$OCR_BIN" ]; then install -m 0755 "$OCR_BIN" "$STAGE/bin/ocrd"; fi
# 构建基线说明（ocrd.baseline）：判据在**上面入参核对那一档**就过完了（缺件 / 没 glibc 行
# 都在动产物之前 die），这里只负责把它拷进包，不再重复判一遍 —— 同一件事两处实现，
# 迟早分叉成「一处拦、一处放」。
if [ -n "$OCR_BIN" ]; then
	install -m 0644 "$OCR_BIN.baseline" "$STAGE/bin/ocrd.baseline"
fi
install -m 0644 "$font_pick" "$STAGE/fonts/$(basename "$font_pick")"
install -m 0755 "$REPO_ROOT/deploy/offline/install.sh" "$STAGE/install.sh"
install -m 0755 "$REPO_ROOT/deploy/offline/uninstall.sh" "$STAGE/uninstall.sh"
# 体检脚本必须随包走：install.sh 在解析服务探测失败时会把它写成「下一步」给用户
# （sudo $PREFIX/bin/sf-ocr-doctor.sh），旧包漏装过这份文件 —— 提示指向不存在的路径比不给提示更糟。
install -m 0755 "$REPO_ROOT/scripts/sf-ocr-doctor.sh" "$STAGE/sf-ocr-doctor.sh"
install -m 0644 "$REPO_ROOT/deploy/offline/skillforge.service.template" "$STAGE/skillforge.service.template"
install -m 0644 "$REPO_ROOT/deploy/offline/skillforge-ocr.service.template" "$STAGE/skillforge-ocr.service.template"
install -m 0644 "$REPO_ROOT/deploy/offline/skillforge.env.example" "$STAGE/skillforge.env.example"
install -m 0644 "$REPO_ROOT/deploy/offline/README.md" "$STAGE/README.md"

# 自带 Python 运行时：整个目录拷进包（cp -a 保留可执行位与符号链接 —— 便携 Python 的
# lib 下有大量 symlink，丢了等于把标准库连根拔掉）。install.sh 装的时候会把它拷到
# $PREFIX/python 并钉进配置，客户机器不需要预装任何解释器。
if [ -n "$PY_RT_HOME" ]; then
	cp -a "$PY_RT_HOME" "$STAGE/python"
	# 包内必须有一个叫 bin/python3 的解释器：install.sh 只认这个名字（约定少一个是一个）。
	# python-build-standalone 只给了版本化的 bin/python3.12，这里补一个**相对** symlink ——
	# 相对链接才能随包换目录（绝对链接一搬路径就断，而「拷到别的机器」正是本包的用法）。
	if [ ! -e "$STAGE/python/bin/python3" ]; then
		PY_RT_REAL="$(cd "$STAGE/python/bin" && ls python3.[0-9]* 2>/dev/null | grep -v -- '-config$' | head -n 1 || true)"
		[ -n "$PY_RT_REAL" ] || die "自带 Python 的 bin/ 下既没有 python3 也没有 python3.<版本> —— 包里没有可用的解释器"
		ln -s "$PY_RT_REAL" "$STAGE/python/bin/python3"
		c_info "已补 bin/python3 → $PY_RT_REAL（相对链接）"
	fi
	[ -x "$STAGE/python/bin/python3" ] \
		|| die "包内 python/bin/python3 不可执行 —— 装到目标机会「AI 跑不了代码」，别发这个包"
	c_ok "自带 Python 运行时已放进包：python/（$PY_VERSION）"
fi

# 字体许可证：文鼎字体走 Arphic Public License，允许原样再分发，但**必须随附许可证文本**。
# 漏了这一条就等于违约分发，所以是硬性步骤而不是「有就带上」。
font_base="$(basename "$font_pick")"
case "$font_base" in
	gbsn00lp.ttf)  DOCDIR=/usr/share/doc/fonts-arphic-gbsn00lp ;;
	gkai00mp.ttf)  DOCDIR=/usr/share/doc/fonts-arphic-gkai00mp ;;
	*)             DOCDIR="" ;;
esac
if [ -n "$DOCDIR" ] && [ -d "$DOCDIR" ]; then
	[ -f "$DOCDIR/copyright" ] && install -m 0644 "$DOCDIR/copyright" "$STAGE/licenses/FONT-ARPHIC-PUBLIC-LICENSE.txt"
	# Debian 里英文原版是 .gz（中文版 gb/big5 未压缩）。两种都认，解压后统一叫 ARPHICPL.txt
	# ——许可证要求随附「原样」文本，所以 gz 要先解开成纯文本再进包。
	if [ -f "$DOCDIR/license/english/ARPHICPL.TXT" ]; then
		install -m 0644 "$DOCDIR/license/english/ARPHICPL.TXT" "$STAGE/licenses/ARPHICPL.txt"
	elif [ -f "$DOCDIR/license/english/ARPHICPL.TXT.gz" ]; then
		gzip -dc "$DOCDIR/license/english/ARPHICPL.TXT.gz" > "$STAGE/licenses/ARPHICPL.txt"
		chmod 0644 "$STAGE/licenses/ARPHICPL.txt"
	fi
elif [ -n "$DOCDIR" ]; then
	die "找不到字体许可证目录 $DOCDIR —— 不许无许可证分发字体，请先装 fonts-arphic-* 包"
fi

# 许可证文本没落地就要拦下来：包里带字体却不带许可证 = 违约分发。
[ -s "$STAGE/licenses/ARPHICPL.txt" ] || die "字体许可证文本没写进包（$DOCDIR/license/english/ARPHICPL.TXT[.gz] 缺失）——拒绝产出不合规的包"

# 包内自述：客户拿到 tar 包第一眼该看到什么
# 打包时间戳先算出来：VERSION 里的 built 字段必须由它派生，不能用 date 取当前时间，
# 否则同样输入两次打包的字节不同，「可复现」当场失效。
if [ -n "$SOURCE_DATE" ]; then
	MTIME="$SOURCE_DATE"
else
	MTIME="${SOURCE_DATE_EPOCH:-0}"
fi
[ "$MTIME" = "0" ] && MTIME="$(git -C "$REPO_ROOT" log -1 --format=%ct 2>/dev/null || echo 0)"
[ "$MTIME" != "0" ] || MTIME="1600000000" # git 不可用时的兜底常量，保证可复现
BUILD_STAMP="$(date -u -d "@$MTIME" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$MTIME" +%Y-%m-%dT%H:%M:%SZ)"

cat > "$STAGE/VERSION" <<EOF
version=$VERSION
arch=$ARCH
built=$BUILD_STAMP
font=$(basename "$font_pick")
ocr=$([ -n "$OCR_BIN" ] && echo yes || echo no)
python=$([ -n "$PY_RT_HOME" ] && echo "${PY_VERSION:-unknown}" || echo no)
EOF

cat > "$STAGE/INSTALL.txt" <<'EOF'
SkillForge 离线安装包
=====================

三步装完：

  1. 把整个目录拷到目标机器（root 权限）
  2. cd 进这个目录，执行：  sudo ./install.sh
  3. 装完会打印访问地址和管理员密码（密码只显示一次，记下来）

安装脚本不联网：全程不 curl / wget / apt / pip，需要的东西都在这个包里。

目标机要求（装之前请先确认）：
  - systemd（含 systemd-run）—— 代码执行沙箱靠它隔离；没有就直接拒绝安装
EOF

# 「目标机要不要自己装 python3」这句必须与包内实际内容一致：带了就写「不用装」，
# 没带（--no-python-runtime）就如实说清怎么补。说明书与产物不一致时，用户是照说明书
# 装的，别的检查全绿也没用 —— 跟下面 ocr 那条断言同一个道理。
if [ -n "$PY_RT_HOME" ]; then
	cat >> "$STAGE/INSTALL.txt" <<'EOF'
  - 不需要预装 python3 —— 这个包自带一份便携 CPython（见下面的 python/ 目录）。
      安装时会把它拷到安装前缀下（<前缀>/python）并写进配置，目标机无需任何解释器：
      内网、无本地源、RHEL/AlmaLinux 最小安装都直接可用。
EOF
else
	cat >> "$STAGE/INSTALL.txt" <<'EOF'
  - python3 解释器之一 —— 代码执行沙箱的探针与「执行代码」工具的解释器
      按顺序找这三个位置（任一个能跑 `-c pass` 即可）：
        /usr/bin/python3          发行版包管理器装的（Debian/Ubuntu/AlmaLinux 默认自带）
        /usr/local/bin/python3    自己编译 / conda / pipx 装的
        /usr/libexec/platform-python   RHEL / CentOS / AlmaLinux 8 自带（没有 python3 这个名字）
      再找不到才退到 $PATH 上的 python3。RHEL / AlmaLinux / CentOS 最小安装默认不带，
      补：dnf install -y python3（离线机挂发行版 ISO 或配本地源）。
      解释器在别处（自定义前缀）：装完在 skillforge.env 里加一行
        SKILLFORGE_PYTHON=/opt/python/bin/python3
      缺了它安装仍会继续（写作功能不受影响），但自检里「代码执行沙箱」一项会失败，
      AI 也就无法跑代码/脚本校验。注意那**不是**沙箱降权/加固失败，就是缺个解释器。
EOF
fi

# 包内容清单必须与包内实际文件一致。
# 旧版把「包里带了两个程序（含 bin/ocrd）」写死在这个单引号 heredoc 里，而 arm64 离线包
# 按设计不带 ocrd（CI 里 arm64 不编解析服务，见 release.yml 的注释），VERSION 也已经
# 写着 ocr=no。结果说明与产物自相矛盾：arm64 用户照说明书以为有 OCR，装完才发现没有。
if [ -n "$OCR_BIN" ]; then
	cat >> "$STAGE/INSTALL.txt" <<'EOF'

包里带了两个程序：
  bin/skillforge  主服务（Web 界面 + 技能引擎）
  bin/ocrd        文档解析服务（扫描件/Word/Excel 抽文本，含 OCR 模型，无需额外依赖）
  bin/ocrd.baseline  解析服务的构建基线说明（它要求的最低 glibc 版本）。装前体检与
                     `bin/skillforge -diag` 用它回答「这份 ocrd 在这台机器上能不能起来」。
                     解析服务起不来时先看它，别删。
安装时会先扫描端口：默认主服务 8092、文档解析 8093。端口被占时会提示你指定新端口。
EOF
else
	cat >> "$STAGE/INSTALL.txt" <<'EOF'

包里带了一个程序：
  bin/skillforge  主服务（Web 界面 + 技能引擎）

注意：这个包里**没有** bin/ocrd（文档解析服务）—— 本包所在架构没有预编译的 ocrd。
影响：扫描件 PDF / Word / Excel / PPT 的文本抽取不可用；写作、技能库、代码执行等
其余功能不受影响。要补齐解析能力，请换用自带 ocrd 的那份离线包（amd64）。
安装时会先扫描端口：默认主服务 8092、文档解析 8093。端口被占时会提示你指定新端口。
EOF
fi

if [ -n "$PY_RT_HOME" ]; then
	cat >> "$STAGE/INSTALL.txt" <<EOF

包里还带了一个运行时：
  python/         便携 CPython $PY_VERSION（含标准库），装的时候拷到 <前缀>/python，
                  并把绝对路径写进 skillforge.env 的 SKILLFORGE_PYTHON。
                  「执行代码」工具与沙箱探针用它 —— 所以目标机**不需要**预装 python3。
EOF
fi

cat >> "$STAGE/INSTALL.txt" <<'EOF'

装完会自动跑一次自检（skillforge -selftest），逐项验证：
  - 程序版本
  - PDF 中文字体是否覆盖中文+数字（缺字符会明确告诉你缺哪些）
  - 代码执行沙箱是否真的隔离（无网络、读不到机密文件）

卸载：sudo ./uninstall.sh         （保留数据，便于重装/迁移）
      sudo ./uninstall.sh --purge （连数据一起删）

详细说明见 README.md。
EOF

# 出包前自检：INSTALL.txt 对「有没有 bin/ocrd」的说法必须与包内实际内容一致。
# 为什么值得单独一条断言：说明与产物不一致时，别的检查全绿也没用——用户是照说明书装的。
# 断言必须按形态锚定，不能只 grep 'bin/ocrd'：不含 ocrd 的那份说明里也会出现「bin/ocrd」
# 字样（「这个包里没有 bin/ocrd」），裸子串会把正确的话判成错误的话。
OCR_CLAIM='bin/ocrd[[:space:]]*文档解析服务'
if [ -n "$OCR_BIN" ]; then
	grep -q "$OCR_CLAIM" "$STAGE/INSTALL.txt" \
		|| die "说明与产物不一致：包里有 ocrd（$OCR_BIN），INSTALL.txt 却没把它列为随包程序"
	# 基线说明同理：包里有 ocrd 就必须有它，且说明书里必须提到它 —— 说明书没提，
	# 客户就不知道那是个有用的文件（清理磁盘时第一个被删）。
	[ -s "$STAGE/bin/ocrd.baseline" ] \
		|| die "包里有 bin/ocrd 却没有 bin/ocrd.baseline —— 客户机上的装前体检/诊断会永远报「读不到基线」"
	grep -q 'bin/ocrd.baseline' "$STAGE/INSTALL.txt" \
		|| die "INSTALL.txt 没说明 bin/ocrd.baseline 是干什么的"
else
	if grep -q "$OCR_CLAIM" "$STAGE/INSTALL.txt"; then
		die "说明与产物不一致：这次打了 --no-ocr，INSTALL.txt 却声称随包提供 bin/ocrd"
	fi
	grep -q '没有' "$STAGE/INSTALL.txt" \
		|| die "说明与产物不一致：包里没有 ocrd，INSTALL.txt 却没如实告诉用户"
	[ ! -e "$STAGE/bin/ocrd.baseline" ] \
		|| die "说明与产物不一致：这次打了 --no-ocr，包里却躺着 bin/ocrd.baseline（会让体检按「有 ocrd」去判）"
fi

# 出包前自检：INSTALL.txt 对「要不要目标机自备 python3」的说法必须与包内实际内容一致。
# 同上：说明书与产物不一致时，用户是照说明书装的 —— 别的检查全绿也救不回来。
# 这里同样按形态锚定（'不需要预装 python3' 整句），不用裸 'python3'：
# 不带运行时的说明里也会出现 python3 字样（教用户怎么装），裸子串必然假绿。
PY_CLAIM='不需要预装 python3'
if [ -n "$PY_RT_HOME" ]; then
	grep -q "$PY_CLAIM" "$STAGE/INSTALL.txt" \
		|| die "说明与产物不一致：包里有自带 Python 运行时，INSTALL.txt 却还在要求目标机自备 python3"
	grep -q 'SKILLFORGE_PYTHON' "$STAGE/INSTALL.txt" \
		|| die "说明与产物不一致：包里有自带 Python，INSTALL.txt 却没告诉用户它会被写进配置"
	[ -x "$STAGE/python/bin/python3" ] \
		|| die "包内 python/bin/python3 不存在或不可执行 —— 这种包装到目标机上就是「AI 跑不了代码」"
else
	if grep -q "$PY_CLAIM" "$STAGE/INSTALL.txt"; then
		die "说明与产物不一致：这次打了 --no-python-runtime，INSTALL.txt 却声称不需要预装 python3"
	fi
	grep -q 'dnf install -y python3' "$STAGE/INSTALL.txt" \
		|| die "说明与产物不一致：包里没有自带解释器，INSTALL.txt 却没告诉用户怎么补上"
fi

# ---------- 3. 确定性打包 ----------
# 固定时间戳 + 固定属主 + 名字排序：同样的输入产出逐字节相同的 tar.gz。
# 不这么做的话，每次 CI 跑出来的 sha256 都不一样，Release 里的校验和就没意义了。
# （MTIME / BUILD_STAMP 在上面写 VERSION 时已经算好，这里直接用。）
TARBALL="$DIST_ABS/$STAGE_NAME.tar.gz"
rm -f "$TARBALL"
# --sort=name 让顺序稳定；--mtime/--owner/--group/--numeric-owner 抹掉环境差异；
# gzip -n 不写原始文件名与时间戳——这三样是「可复现打包」的常规做法。
tar --sort=name \
    --mtime="@$MTIME" \
    --owner=0 --group=0 --numeric-owner \
    --format=gnu \
    -C "$DIST_ABS" -cf - "$STAGE_NAME" \
	| gzip -9n > "$TARBALL"

( cd "$DIST_ABS" && sha256sum "$(basename "$TARBALL")" > "$(basename "$TARBALL").sha256" )

c_ok "离线包：$TARBALL（$(du -h "$TARBALL" | cut -f1)）"
c_ok "校验和：$(cut -d' ' -f1 "$TARBALL.sha256")"
c_info "解压后目录：$STAGE_NAME/"
printf '\n'
# python/ 里有几千个标准库文件：逐条列会把 CI 日志刷爆、也淹掉真正该看的文件。
# 汇总成一行（文件数 + 体积），要查明细看包内 VERSION 里的 python= 字段。
( cd "$STAGE" && find . -type f -not -path './python/*' | sort | sed 's|^\./|  |' )
if [ -n "$PY_RT_HOME" ]; then
	printf '  python/  （自带解释器 %s：%s 个文件，%s）\n' \
		"$PY_VERSION" \
		"$(find "$STAGE/python" -type f | wc -l)" \
		"$(du -sh "$STAGE/python" | cut -f1)"
fi
