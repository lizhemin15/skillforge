#!/usr/bin/env bash
# 上线 ocrd 新二进制：从 Release 离线包里取出 ocrd → 换 /opt/skillforge/bin/ocrd → 重启 → 复验
# 用法：bash scripts/deploy_ocrd.sh v260916.0147
set -euo pipefail
TAG="${1:?用法: bash scripts/deploy_ocrd.sh <tag>}"
# ROOT 必须在**任何 cd 之前**按绝对路径定死，而且**只算这一次**。本脚本第 1 步就
# `cd "$WORK"`；6b 里曾经又算了一次 `$(dirname "$0")/..` —— 那时 $0 已是相对路径
# "scripts/deploy_ocrd.sh"，于是 `cd scripts/..` 失败、脚本当场中止，整个逐页流门禁
# **静默跳过**（外层 `| tail` 又吞掉退出码：屏幕一切正常，门禁一次都没跑）。2026-09-23 真上线踩到。
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
echo "$PREH" | grep -q "ocrd-v6-page-stream" || { echo "FAIL: 新件版本串不对（期望 ocrd-v6-page-stream）→ 拒绝上线"; exit 3; }
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
echo "$H" | grep -q 'ocrd-v6-page-stream' || { echo "FAIL: 线上版本串不是新件 → 回滚"; systemctl stop skillforge-ocr; install -m 0755 "$BAK" "$LIVE"; systemctl start skillforge-ocr; exit 4; }
echo "  ok: 线上已是 ocrd-v6-page-stream 且 runtime_ok=true"
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

echo "=== 6b) 逐页流复验（v6 新能力：解析期屏幕要滚材料，而不是滚裸计时）==="
# 为什么这条必须是**上线门禁**而不是躺在 CI 里的单测：逐页流是「屏幕上有真材料在动」
# 这个体验修复的**唯一**来源。它若在路上（反向代理缓冲、老件没换、Accept 头没被认），
# 现象就是回到「一直卡着计时」——用户投诉的原话，而所有既有断言照样全绿。
# 判据**只有一份**：scripts/verify_ocr_stream.py（真验收腿与尺子自证共用同一个 judge()）。
# 此前这里抄过第二份判据，而且写成 `curl … | python3 - <<'PY'` —— heredoc 与管道抢 stdin，
# python3 把程序文本当输入、sys.stdin 是空的，于是所有断言都在空数据上跑：看着「跑了」，
# 实际一条都没量到。抄出来的判据不会自动跟着变，这就是它烂掉的方式。
# 料单：仓里那份（3 页纯扫描）**总是**跑，它覆盖「只扫扫描页」这条路由；本机若有混合重料
# （可选 45 页 + 扫描 15 页）一并跑 —— 只有混合料能覆盖「可选中页直取」（纯扫描料的
# text_pages 恒为 0），而且 30s 量级才把「边解析边发」与「解析完再拆行」彻底分开。
# 之前写成「命中第一个就 break」是个坑：仓里那份排在前面，于是本机重料永远轮不到，
# 直取路由在这条腿上等于从没量过 —— 绿灯亮着，缝在外面。
rollback_stream() {
  echo "FAIL: 逐页流复验没过（$1）→ 回滚"
  systemctl stop skillforge-ocr; install -m 0755 "$BAK" "$LIVE"; systemctl start skillforge-ocr; exit 5
}
STREAM_PDFS=""
for c in "$ROOT/testdata/ocr/scan3-rtloss.pdf" "${FIXTURE_PDF:-}" /tmp/heavy_mixed_s15_t45.pdf; do
  [ -n "$c" ] || continue
  [ -f "$c" ] || continue
  case " $STREAM_PDFS " in *" $c "*) continue ;; esac   # 去重：同一份料不跑两遍
  STREAM_PDFS="$STREAM_PDFS $c"
done
# 逐页流是本次发布的核心能力：一份料都量不到就不许发布（SKIP 不算过）。
[ -f "$ROOT/scripts/verify_ocr_stream.py" ] \
  || rollback_stream "找不到尺子 $ROOT/scripts/verify_ocr_stream.py（路径算错了？）→ 这条腿没跑，不许算通过"
[ -n "$STREAM_PDFS" ] || rollback_stream "本机没有任何可用 PDF 试料，逐页流未验证"
RAN=0
for c in $STREAM_PDFS; do
  # NONCE=1：ocrd 按内容 hash 缓存，命中缓存时 on_page 根本不回调 → 同料复跑会量成
  # 「零逐页行」的假红。尺子会给试料尾部追加唯一注释改掉内容 hash，保证每次都量到冷解析。
  NONCE=1 python3 "$ROOT/scripts/verify_ocr_stream.py" http://127.0.0.1:8093 "$c" 2 || rollback_stream "$c"
  RAN=$((RAN + 1))
done
echo "  逐页流复验通过：$RAN 份料（覆盖「扫描页 OCR」与「文本层直取」两种路由）"

echo "=== 上线完成：$TAG ==="

