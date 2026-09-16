#!/usr/bin/env bash
# 上线 ocrd 新二进制：从 Release 离线包里取出 ocrd → 换 /opt/skillforge/bin/ocrd → 重启 → 复验
# 用法：bash scripts/deploy_ocrd.sh v260916.0147
set -euo pipefail
TAG="${1:?用法: bash scripts/deploy_ocrd.sh <tag>}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO="$(git -C "$ROOT" remote get-url origin)"
WORK="/tmp/sfdeploy-$TAG"
LIVE="/opt/skillforge/bin/ocrd"
PORT=18099

echo "=== 1) 下载离线包（含 ocrd）==="
mkdir -p "$WORK"; cd "$WORK"
gh release download "$TAG" -R "$REPO" --pattern "skillforge-offline-${TAG}-linux-amd64.tar.gz*" --clobber
ls -l --time-style=long-iso

echo "=== 2) 校验 sha256 ==="
EXP="$(awk '{print $1}' "skillforge-offline-${TAG}-linux-amd64.tar.gz.sha256")"
GOT="$(sha256sum "skillforge-offline-${TAG}-linux-amd64.tar.gz" | awk '{print $1}')"
[ "$EXP" = "$GOT" ] || { echo "FAIL: sha256 不符 exp=$EXP got=$GOT"; exit 1; }
echo "  ok: sha256 $GOT"

echo "=== 3) 解包并定位 ocrd ==="
tar -xzf "skillforge-offline-${TAG}-linux-amd64.tar.gz"
NEW="$(find "$WORK" -type f -name ocrd -perm -u+x | head -1)"
[ -n "$NEW" ] || { echo "FAIL: 离线包里找不到 ocrd"; exit 2; }
echo "  新件：$NEW  $(stat -c%s "$NEW")B"
echo "  线上旧件：$LIVE  $(stat -c%s "$LIVE")B  mtime=$(stat -c%y "$LIVE")"

echo "=== 4) 起飞前预验（临时端口，不动生产）==="
PRETMP="$WORK/pre-tmp"; mkdir -p "$PRETMP"; chmod 700 "$PRETMP"
TMPDIR="$PRETMP" "$NEW" --port $PORT >"$WORK/pre.log" 2>&1 &
PREPID=$!
for i in $(seq 1 60); do curl -s --max-time 2 "http://127.0.0.1:$PORT/health" >/dev/null 2>&1 && break; sleep 1; done
PREH="$(curl -s --max-time 5 "http://127.0.0.1:$PORT/health" || true)"
kill "$PREPID" 2>/dev/null || true; wait "$PREPID" 2>/dev/null || true
echo "  预验 /health：$PREH"
echo "$PREH" | grep -q "ocrd-v5-runtime-guard" || { echo "FAIL: 新件版本串不对（期望 ocrd-v5-runtime-guard）→ 拒绝上线"; exit 3; }
echo "$PREH" | grep -q '"runtime_ok": *true' || { echo "FAIL: 新件 /health 没报 runtime_ok=true"; exit 3; }
echo "  ok: 新件自带运行时守卫且健康可见"

echo "=== 5) 备份 + 停服 + 换件 + 启动 ==="
BAK="$LIVE.bak-$(date +%Y%m%d%H%M%S)"
cp -a "$LIVE" "$BAK"; echo "  备份：$BAK"
# 必须先停服：install 覆盖「正在运行」的可执行文件会 ETXTBSY
systemctl stop skillforge-ocr
sleep 1
install -m 0755 "$NEW" "$LIVE"; echo "  已换件：$(stat -c%s "$LIVE")B"
systemctl start skillforge-ocr
for i in $(seq 1 60); do curl -s --max-time 2 http://127.0.0.1:8093/health >/dev/null 2>&1 && break; sleep 1; done

echo "=== 6) 线上复验 ==="
H="$(curl -s --max-time 5 http://127.0.0.1:8093/health)"
echo "  /health：$H"
echo "$H" | grep -q '"runtime_ok": *true' || { echo "FAIL: 线上 /health 无 runtime_ok=true → 回滚"; systemctl stop skillforge-ocr; install -m 0755 "$BAK" "$LIVE"; systemctl start skillforge-ocr; exit 4; }
echo "$H" | grep -q 'ocrd-v5-runtime-guard' || { echo "FAIL: 线上版本串不是新件 → 回滚"; systemctl stop skillforge-ocr; install -m 0755 "$BAK" "$LIVE"; systemctl start skillforge-ocr; exit 4; }
echo "  ok: 线上已是 ocrd-v5-runtime-guard 且 runtime_ok=true"
systemctl is-active skillforge-ocr
systemctl show skillforge-ocr -p NRestarts --no-pager

echo "=== 7) 真实抽两件（③ 的 30 页扫描 + ② 的混合料）==="
if [ -f /tmp/scan30.pdf ]; then
  curl -s --max-time 300 -F "file=@/tmp/scan30.pdf" http://127.0.0.1:8093/extract \
    | python3 -c "import json,sys; d=json.load(sys.stdin); print('  30页扫描:', json.dumps(d.get('stats'), ensure_ascii=False), 'chars=', d.get('chars'), 'ok=', d.get('ok'))"
fi
if [ -f /tmp/live_mixed/mixed_wm.pdf ]; then
  curl -s --max-time 300 -F "file=@/tmp/live_mixed/mixed_wm.pdf" http://127.0.0.1:8093/extract \
    | python3 -c "import json,sys; d=json.load(sys.stdin); print('  混合料:', json.dumps(d.get('stats'), ensure_ascii=False), 'chars=', d.get('chars'), 'ok=', d.get('ok'))"
fi

echo "=== 上线完成：$TAG ==="
