#!/usr/bin/env bash
#
# fetch-python-runtime.sh —— 下载便携 CPython（python-build-standalone 的 install_only_stripped）
#
# 给 build-offline-bundle.sh 的 --python-runtime 供料。CI 里由 release.yml 按架构调用，
# 本地手工打包时也可以先跑它。
#
# 为什么单独一个脚本、而且**资产名从 API 现取**：
#   资产名形如 cpython-<版本>+<8位日期>-<triple>-install_only_stripped.tar.gz。
#   把这种「<版本>+<8位日期>」的字面量写进脚本或工作流，在本机环境里会被改写成带掩码
#   的形式 —— 落盘的就是错的，CI 里 curl 直接 404，而且看着像上游删了包。
#   所以这里只放一个裸日期 tag（--tag，形如 20260901，不带加号），资产名与 sha256 摘要
#   全部从 GitHub API 现取。
#
# 用法：
#   scripts/fetch-python-runtime.sh --arch amd64 --outdir /tmp
#   scripts/fetch-python-runtime.sh --arch arm64 --outdir /tmp --tag 20260901
#   scripts/fetch-python-runtime.sh --arch amd64 --series 3.11 --outdir /tmp
#
# 输出：把 tar.gz 的绝对路径打到 stdout（方便 CI 里 PKG=$(...) 接住），其余信息走 stderr。
# 设计要点：
#   - 下载后必须校验 sha256（API 没给摘要就失败退出）：一个被截断/替换的解释器打进包，
#     客户装完表现为「AI 跑不了代码」，而原因（包里的运行时是坏的）从任何输出里都看不出来。
#
set -euo pipefail

ARCH=""
OUTDIR="."
TAG="latest"
SERIES="3.12"

die()    { printf '\033[31m✗\033[0m %s\n' "$*" >&2; exit 1; }
c_info() { printf '  %s\n' "$*" >&2; }
c_ok()   { printf '  \033[32m✓\033[0m %s\n' "$*" >&2; }

usage() {
	sed -n '2,24p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
	exit 0
}

while [ $# -gt 0 ]; do
	case "$1" in
		--arch)   ARCH="${2:?--arch 需要值，amd64 或 arm64}"; shift 2 ;;
		--outdir) OUTDIR="${2:?--outdir 需要值}"; shift 2 ;;
		--tag)    TAG="${2:?--tag 需要值，形如 20260901}"; shift 2 ;;
		--series) SERIES="${2:?--series 需要值，形如 3.12}"; shift 2 ;;
		-h|--help) usage ;;
		*) die "无法识别的参数：$1（用 --help 看用法）" ;;
	esac
done

[ -n "$ARCH" ] || die "必须指定 --arch（amd64 / arm64）—— 打哪个架构的离线包就取哪个架构的运行时"
case "$ARCH" in
	amd64) TRIPLE="x86_64-unknown-linux-gnu" ;;
	arm64) TRIPLE="aarch64-unknown-linux-gnu" ;;
	*) die "--arch 只支持 amd64 / arm64（离线包面向 linux 服务器），收到：$ARCH" ;;
esac

command -v python3 >/dev/null 2>&1 || die "本脚本需要 python3 解析 GitHub API 的 JSON（CI runner 自带）"

mkdir -p "$OUTDIR"
OUTDIR="$(cd "$OUTDIR" && pwd)"

if [ "$TAG" = "latest" ]; then
	API="https://api.github.com/repos/astral-sh/python-build-standalone/releases/latest"
else
	API="https://api.github.com/repos/astral-sh/python-build-standalone/releases/tags/$TAG"
fi

c_info "查询上游 release：$TAG（series=$SERIES，triple=$TRIPLE）"
# 资产名与摘要从 API 现取（见文件头注释：写死会被掩码改写）。
META="$(curl -fsSL --retry 3 --retry-delay 2 "$API" | python3 -c '
import json, sys
series, triple = sys.argv[1], sys.argv[2]
d = json.load(sys.stdin)
want_prefix = "cpython-" + series + "."
want_suffix = "-" + triple + "-install_only_stripped.tar.gz"
for a in d.get("assets", []):
    n = a.get("name", "")
    if n.startswith(want_prefix) and n.endswith(want_suffix):
        print(n)
        print(a.get("browser_download_url", ""))
        print(a.get("digest") or "")
        break
else:
    sys.exit("上游 release 里没有匹配的资产（series=%s，triple=%s）" % (series, triple))
' "$SERIES" "$TRIPLE")" || die "解析 GitHub API 失败（$API）"

ASSET_NAME="$(printf '%s\n' "$META" | sed -n '1p')"
ASSET_URL="$(printf '%s\n' "$META" | sed -n '2p')"
ASSET_DIGEST="$(printf '%s\n' "$META" | sed -n '3p')"
[ -n "$ASSET_NAME" ] && [ -n "$ASSET_URL" ] || die "没从 API 拿到资产名/下载地址"

DEST="$OUTDIR/$ASSET_NAME"
if [ -f "$DEST" ]; then
	c_info "本地已有，跳过下载：$DEST"
else
	c_info "下载：$ASSET_NAME"
	curl -fL --retry 3 --retry-delay 2 -o "$DEST.part" "$ASSET_URL" \
		|| die "下载失败：$ASSET_URL"
	mv "$DEST.part" "$DEST"
fi

# 校验：只认 API 给的 sha256 摘要。拿不到摘要就直接失败 —— 宁可这一步红，
# 也不要往离线包里塞一份来路不明/可能被截断的解释器。
case "$ASSET_DIGEST" in
	sha256:*) WANT="${ASSET_DIGEST#sha256:}" ;;
	*) die "上游没给 sha256 摘要（digest=$ASSET_DIGEST）—— 拒绝在无校验的情况下继续" ;;
esac
GOT="$(sha256sum "$DEST" | cut -d' ' -f1)"
[ "$GOT" = "$WANT" ] || die "sha256 不符：期望 $WANT，实际 $GOT（文件：$DEST）"
c_ok "sha256 校验通过：$(du -h "$DEST" | cut -f1)"

printf '%s\n' "$DEST"
