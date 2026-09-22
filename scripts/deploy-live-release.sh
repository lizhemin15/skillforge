#!/usr/bin/env bash
# 线上部署：从 GitHub Release 取裸二进制 → 校验 → 原子替换 → 重启 → 健康检查 →（失败自动回滚）。
#
# 为什么要写成脚本（而不是每次手敲一串命令）：
#   部署这条链路有**四个各自独立的失败点**，手敲时任何一处漏掉都不会当场报错，
#   而是在下一次用户点「生成」时才暴露：
#     1. tag 推了但 Release 资产还没传上来（Release 是**草稿态**建、各矩阵 job 各自上传，
#        裸二进制到得比离线包早）→ 手敲时很容易 `gh release download` 落空或下到 0 字节；
#     2. 下载件与仓里代码不是同一个 commit（tag 可能打歪）→ 装上线才发现版本对不上；
#     3. 二进制是坏的/架构不对 → `mv` 完服务起不来。**这一步必须在停服务之前验**：
#        直接 mv 再 restart，坏件已经把好件覆盖掉了，回滚全靠备份文件在不在；
#     4. 起来了但功能没生效（配置读的是别的路径）→ 只看「进程活着」等于没验。
#
# 判据（fail-closed，任何一条不过就 mv 回备份并 exit 1）：
#   * sha256 与 Release 上的 .sha256 逐字节一致
#   * 新件 `-version` 能跑且 Commit 前缀 == 期望 commit（防止 tag 打歪）
#   * 新件 Version == 期望 tag
#   * 停服务 → 原子 mv → 启动，服务 10 秒内 active
#   * `/` 返回 200 且 `-version` 报告的就是新 tag
#
# 用法：
#   scripts/deploy-live-release.sh <tag> [期望 commit 短 sha]
#   例：scripts/deploy-live-release.sh v2609220540 4017089
#
# 环境变量（都有默认值，默认就是本机线上那套）：
#   DEST=/opt/skillforge/skillforge   部署目标（二进制路径，不是目录）
#   SVC=skillforge                    systemd 单元名
#   BASE=http://127.0.0.1:8092        健康检查地址
#   ASSET_TIMEOUT=1800                等 Release 资产出现的最长秒数
set -uo pipefail

TAG="${1:?用法: deploy-live-release.sh <tag> [期望 commit 短 sha]}"
WANT_COMMIT="${2:-}"
DEST="${DEST:-/opt/skillforge/skillforge}"
SVC="${SVC:-skillforge}"
BASE="${BASE:-http://127.0.0.1:8092}"
ASSET_TIMEOUT="${ASSET_TIMEOUT:-1800}"

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STAGE="$(mktemp -d /tmp/deploy-stage.XXXXXX)"
ASSET="skillforge-${TAG}-linux-amd64"
DEST_BAK="${DEST}.bak-$(date +%H%M%S)"
INSTALLED=0

say()  { echo "[$(date +%H:%M:%S)] $*"; }
die()  { echo "✗ $*" >&2; exit 1; }

# 回滚：只有真的动过 $DEST 才回滚，且必须确认备份还在（备份没了就宁可别乱动）。
rollback() {
  local why="$1"
  echo "✗ 部署失败（$why）→ 回滚" >&2
  if [ "$INSTALLED" = "1" ] && [ -f "$DEST_BAK" ]; then
    systemctl stop "$SVC" >/dev/null 2>&1
    mv -f "$DEST_BAK" "$DEST" || { echo "✗✗ 回滚 mv 都失败了，手工处理：$DEST_BAK" >&2; exit 9; }
    systemctl start "$SVC" >/dev/null 2>&1
    sleep 3
    # 回滚诊断本身必须容错：备份件不可执行 / 服务真起不来时，这里要给出**可读的原因**，
    # 而不是把 `Permission denied` 之类的解释器噪音当成「版本号」打出来（第一次自证就撞到）。
    local code rb_ver
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$BASE/" || true)"
    rb_ver="$("$DEST" -version 2>/dev/null | head -1 || true)"
    echo "   回滚后健康检查 HTTP=${code:-000}，版本=${rb_ver:-（$DEST 不可执行）}" >&2
    [ "$code" = "200" ] || echo "✗✗ 回滚后服务也没起来，人工介入！" >&2
  else
    echo "   尚未替换线上件，无需回滚。" >&2
  fi
  exit 1
}

# ---- 0. tag 必须真在仓库里（打歪的 tag 会让「已发布」和「已部署」错位）----
cd "$REPO_DIR"
git rev-parse -q --verify "refs/tags/$TAG" >/dev/null \
  || die "仓库里没有 tag $TAG —— 先 push tag 再部署"

# ---- 1. 等 Release 资产出现（草稿态就开始传，早于 Release 转正）----
say "等 Release 资产 $ASSET（最长 ${ASSET_TIMEOUT}s）…"
deadline=$(( $(date +%s) + ASSET_TIMEOUT ))
while :; do
  if gh release download "$TAG" -p "$ASSET" -p "$ASSET.sha256" -D "$STAGE" --clobber >/dev/null 2>&1 \
     && [ -s "$STAGE/$ASSET" ] && [ -s "$STAGE/$ASSET.sha256" ]; then
    break
  fi
  [ "$(date +%s)" -lt "$deadline" ] || die "等不到资产 $ASSET（Release 可能还停在草稿且没用 --clobber 传上来）"
  sleep 20
done
say "资产已下载：$(ls -lh "$STAGE/$ASSET" | awk '{print $5}')"

# ---- 2. sha256 逐字节一致 ----
( cd "$STAGE" && sha256sum -c "$ASSET.sha256" >/dev/null ) \
  || die "sha256 校验不过 —— 下载件被截断或被改过，绝不装上线上"
say "sha256 ✓"

# ---- 3. 版本与 commit 对得上（tag 打歪/下错件的常见死法）----
chmod +x "$STAGE/$ASSET"
NEW_VER="$("$STAGE/$ASSET" -version 2>&1 | head -1)"
say "新件自报：$NEW_VER"
case "$NEW_VER" in
  *"$TAG"*) ;;
  *) die "新件 Version 与 tag 不符（期望含 $TAG）：$NEW_VER" ;;
esac
if [ -n "$WANT_COMMIT" ]; then
  case "$NEW_VER" in
    *"$WANT_COMMIT"*) ;;
    *) die "新件 Commit 不是 $WANT_COMMIT —— tag 打在了别的提交上：$NEW_VER" ;;
  esac
fi

# ---- 4. 停服务 → 备份 → 原子替换（mv 同文件系统，无半截态）----
say "停 $SVC …"
systemctl stop "$SVC" || die "停服务失败"
INSTALLED=1
# 首次部署时 $DEST 还不存在 —— 那不是「备份失败」，不要因此拒绝部署。
if [ -f "$DEST" ]; then
  cp -f "$DEST" "$DEST_BAK" || { INSTALLED=0; die "备份当前件失败，不敢继续"; }
  say "旧件已备份 → $DEST_BAK"
else
  INSTALLED=0   # 没有可回滚的目标：失败时就地留证据，别谎称回滚成功
  say "首次部署（$DEST 不存在），无可备份"
fi

mv -f "$STAGE/$ASSET" "$DEST" || rollback "替换二进制失败"
chmod +x "$DEST"
systemctl start "$SVC" || rollback "启动 systemd 失败"

# ---- 5. 健康检查：进程活着 **且** 跑的是新版本 **且** HTTP 通 ----
for i in $(seq 1 10); do
  systemctl is-active --quiet "$SVC" && break
  sleep 1
done
systemctl is-active --quiet "$SVC" || rollback "服务未能 active"

code=""
for i in $(seq 1 10); do
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$BASE/" || true)"
  [ "$code" = "200" ] && break
  sleep 1
done
[ "$code" = "200" ] || rollback "$BASE/ 返回 $code（不是 200）"

LIVE_VER="$("$DEST" -version 2>&1 | head -1)"
case "$LIVE_VER" in
  *"$TAG"*) ;;
  *) rollback "线上自报版本不是 $TAG：$LIVE_VER" ;;
esac

say "✓ 部署完成：$LIVE_VER"
say "  HTTP=$code  备份=$DEST_BAK"
say "  回滚：systemctl stop $SVC && mv $DEST_BAK $DEST && systemctl start $SVC"
echo "DEPLOY_OK tag=$TAG"
