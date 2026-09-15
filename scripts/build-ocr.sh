#!/usr/bin/env bash
#
# build-ocr.sh —— 构建 ocrd（文档解析服务）单文件可执行，并**在干净镜像里验它真能 OCR**。
#
# 为什么不用本机 venv 直接 pyinstaller：
#   本机 venv 会把 PyInstaller 看得见的所有 .so 一起打包，包括 opencv 的 GUI 依赖
#   （libGL/libX11）。开发机上有这些库，客户机的裸 server 上没有 → 包看着 176MB 很完整，
#   到客户机上 cv2 加载失败、OCR 静默失效。所以构建与验证都在容器里做，一次说清。
#
# 用法：
#   scripts/build-ocr.sh --arch amd64 --out dist-bin/ocrd-amd64
#   scripts/build-ocr.sh --arch arm64 --out dist-bin/ocrd-arm64     # 走 QEMU，慢（10-30 分钟）
#   scripts/build-ocr.sh --arch amd64 --selfcheck                   # 自证：故意让断言失配
#
# 产出：--out 指定的文件（可执行），并打印 sha256。
#
# 自证（--selfcheck）在做什么：
#   把验证用的关键词换成一个绝不可能出现在样张里的词，此时验证阶段**必须失败**。
#   如果它照样"通过"，说明那道验证根本没有断言 OCR 结果 —— 那种绿是假绿，
#   等于给一个可能坏掉的二进制盖章放行（这个项目栽过太多"断言本身是假的"）。
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ARCH="amd64"
OUT=""
SELFCHECK=0
BUILD_CONTEXT="$REPO_ROOT/deploy/ocr"

c_info() { printf '  %s\n' "$*"; }
c_ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; }
c_fail() { printf '  \033[31m✗\033[0m %s\n' "$*"; }
die()    { c_fail "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
	case "$1" in
		--arch)      ARCH="${2:?--arch 需要 amd64/arm64}"; shift 2 ;;
		--out)       OUT="${2:?--out 需要路径}"; shift 2 ;;
		--context)   BUILD_CONTEXT="${2:?}"; shift 2 ;;
		--selfcheck) SELFCHECK=1; shift ;;
		-h|--help)   sed -n '2,25p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
		*)           die "无法识别的参数：$1" ;;
	esac
done

case "$ARCH" in amd64|arm64) ;; *) die "--arch 只支持 amd64 / arm64"; esac
command -v docker >/dev/null 2>&1 || die "需要 docker（构建在容器里跑，理由见脚本头部）"
docker buildx version >/dev/null 2>&1 || die "需要 docker buildx"

PLATFORM="linux/$ARCH"
# manylinux_2_28 是 glibc 2.28 的构建基线（见 Dockerfile 头部"坑 2"）。
# 镜像名后缀是 x86_64 / aarch64，跟 GOARCH 的 amd64 / arm64 不同名，所以要显式映射 ——
# 写错后缀的后果是 docker 直接拉不到镜像（不会静默出错），但映射表还是集中在这里好。
case "$ARCH" in
	amd64) MANYLINUX="quay.io/pypa/manylinux_2_28_x86_64" ;;
	arm64) MANYLINUX="quay.io/pypa/manylinux_2_28_aarch64" ;;
esac
printf '\033[1m构建 ocrd · %s（基线 %s）\033[0m\n' "$PLATFORM" "$MANYLINUX"

# ---------- 0. 守卫语义单测（不依赖二进制，几秒级，先跑省得白等容器构建）----------
# 背景：verify_runtime_loss.sh 的 S4 在本轮真二进制上抓到「守卫固定 sleep 0.4s 就 os._exit，
# 而调用方还在传 body → 对端拿到连接中断（curl rc=55）」。并发时序靠造故障难稳定复现，
# 这里从出货文件抠真函数（打桩可选依赖，CI runner 无需 pymupdf/rapidocr）把语义钉死。
GUARD_TEST="$REPO_ROOT/deploy/ocr/test_guard_wait.py"
if [ -f "$GUARD_TEST" ]; then
	[ -n "$(command -v python3 || true)" ] || die "缺 python3，跑不了守卫语义单测：$GUARD_TEST"
	echo "== 守卫语义单测：正跑 =="
	python3 "$GUARD_TEST" || die "守卫语义单测未通过：$GUARD_TEST"
	echo "== 守卫语义单测：--break 自证（换回旧实现必须转红）=="
	python3 "$GUARD_TEST" --break \
		|| die "--break 自证未通过：换回旧实现后单测竟然还是绿的 → 这道尺子是假的"
else
	die "缺 $GUARD_TEST（守卫退出语义的确定性单测）"
fi

# ---------- 1. 干净镜像里跑真 OCR（构建的 verify 阶段就是这道门）----------
c_info "在 almalinux:8（glibc 2.28 基线、无 GUI 库）里验证自包含性 + OCR 真能出字…"
set +e
docker buildx build --progress plain --platform "$PLATFORM" \
	--build-arg "MANYLINUX_IMAGE=$MANYLINUX" \
	--target verify -f "$BUILD_CONTEXT/Dockerfile" "$BUILD_CONTEXT" \
	2>&1 | tail -25
VERIFY_RC=${PIPESTATUS[0]}
set -e
if [ "$VERIFY_RC" -ne 0 ]; then
	die "验证失败：这个 ocrd 在干净最小系统上跑不起来或 OCR 出不了字 —— 不许出包"
fi
c_ok "验证通过：干净镜像里能启动、能对纯图像 PDF 跑出预期文字"

# ---------- 2. 自证：把关键词改成绝不可能出现的词，验证必须变红 ----------
if [ "$SELFCHECK" -eq 1 ]; then
	c_info "自证：用错误关键词重跑验证（必须失败，否则说明断言没有牙齿）…"
	set +e
	docker buildx build --progress plain --platform "$PLATFORM" \
		--build-arg "MANYLINUX_IMAGE=$MANYLINUX" \
		--target verify --build-arg EXPECT=绝不可能出现的词zzz \
		-f "$BUILD_CONTEXT/Dockerfile" "$BUILD_CONTEXT" >/tmp/ocr-selfcheck.log 2>&1
	RED_RC=$?
	set -e
	if [ "$RED_RC" -eq 0 ]; then
		tail -20 /tmp/ocr-selfcheck.log | sed 's/^/      /'
		die "自证失败（假绿）：换成错误关键词后验证仍然通过 —— 那道断言根本没在检查 OCR 结果"
	fi
	if ! grep -q '结果里没有' /tmp/ocr-selfcheck.log; then
		c_fail "自证时确实失败了，但不是因为断言失配（看不出是哪一步红的）"
		tail -20 /tmp/ocr-selfcheck.log | sed 's/^/      /'
		die "自证无效：红在崩溃上不算红"
	fi
	c_ok "自证通过：错误关键词下验证确实红，且红在断言上（不是崩溃）"
fi

# ---------- 3. 导出产物 ----------
ART_TAG="sf-ocr-art:$ARCH"
docker buildx build --platform "$PLATFORM" \
	--build-arg "MANYLINUX_IMAGE=$MANYLINUX" \
	--target artifact --load \
	-t "$ART_TAG" -f "$BUILD_CONTEXT/Dockerfile" "$BUILD_CONTEXT" >/dev/null
CID="$(docker create --platform "$PLATFORM" "$ART_TAG")"
trap 'docker rm -f "$CID" >/dev/null 2>&1 || true' EXIT

if [ -z "$OUT" ]; then
	OUT="$REPO_ROOT/dist-bin/ocrd-$ARCH"
fi
mkdir -p "$(dirname "$OUT")"
docker cp "$CID:/ocrd" "$OUT"
chmod 0755 "$OUT"
docker rm -f "$CID" >/dev/null 2>&1 || true
trap - EXIT

# ---------- 4. 产物核对：架构必须对（装错架构在客户机上表现为"解析服务起不来"）----------
DESC="$(file -b "$OUT")"
case "$ARCH:$DESC" in
	amd64:*x86-64*) ;;
	arm64:*ARM\ aarch64*) ;;
	*) die "产物架构不对：--arch=$ARCH，但 file 说是「$DESC」" ;;
esac
c_ok "产物：$OUT（$(du -h "$OUT" | cut -f1)，$DESC）"
c_ok "sha256：$(sha256sum "$OUT" | cut -d' ' -f1)"

# ---------- 5. 运行时事故防线：解包目录被 tmpfiles 清掉时要能自己发现、说人话、重启自愈 ----------
# 这道防线**必须**对冻结二进制跑：非冻结运行没有 $TMPDIR/_MEI* 解包目录，没有这个事故形态。
# 本机架构与产物架构一致时真跑（正绿 + --break 转红）；不一致时明确跳过，不假绿。
HOST_M="$(uname -m)"
VERIFY="$REPO_ROOT/deploy/ocr/verify_runtime_loss.sh"
case "$ARCH:$HOST_M" in
	amd64:x86_64|arm64:aarch64)
		[ -f "$VERIFY" ] || die "缺 $VERIFY（运行时事故防线脚本）"
		echo "== 运行时防线：正绿 =="
		OCRD_BIN="$OUT" OCRD_PORT="${OCRD_PORT:-18094}" bash "$VERIFY" \
			|| die "运行时防线未通过：$VERIFY（产物 $OUT）"
		echo "== 运行时防线：--break 自证（关掉守卫必须变红）=="
		OCRD_BIN="$OUT" OCRD_PORT="${OCRD_PORT:-18094}" bash "$VERIFY" --break \
			|| die "--break 自证未通过：去掉守卫后防线竟然还是绿的 → 这道尺子是假的"
		;;
	*)
		echo "  · 跳过运行时防线（产物 $ARCH，本机 $HOST_M，跑不了）：$VERIFY"
		;;
esac
