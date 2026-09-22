#!/usr/bin/env bash
# 材料流式化的 A/B 对照：**同一把尺子**跑两臂二进制，一红一绿才算这把尺子有牙。
#
# 为什么必须有这个脚本（而不是「线上跑一次绿了就行」）：
#   `scripts/sse_material_log_gate.py` 只在线上跑得到（要真模型、真 SSE、~40s 一轮），
#   所以它进不了 CI/preflight —— 意味着它的判据在 CI 里**没有任何东西守着**。
#   要是哪天 L1/L4 的阈值写歪了（比如把门槛从 400 改成 0），CI 全绿、线上也全绿，
#   而尺子已经不再能区分「整段在长」和「160 字单行尾巴」了 —— 这正是它要防的那种
#   失效：**失效形态是「一切正常」**。唯一能在本地造出区分力的办法，就是拿修复前
#   那版二进制当反例跑一遍：它必须红。
#
# 两臂怎么定：
#   A) 旧件：`/opt/skillforge/skillforge.bak-*` 里指定的那个（默认取最新一个），
#      跑在 8199 + 数据目录副本上，**绝不碰线上 8092 的库**。注意 bak 会随每次部署
#      滚动 —— 部署了新件之后，最新那个 bak 才正好是「修复前」的那版。若哪天最新 bak
#      已经是修复后的件，两臂 commit 就会相同 → 本脚本会直接报 FAKE_CONTRAST 退出，
#      不让你用一份假对照去证明尺子有牙。
#   B) 新件：线上 8092 正在跑的那版。
#
# 判据（缺一不可）：
#   1. 两臂 `/api/version` 的 commit **必须不同**（否则是假对照，比没对照更毒）
#   2. 旧臂必须 RC=1 且在预期的那条判据上红
#   3. 新臂必须 RC=0（不绿 = 修复没上线 / 尺子写歪）
#
# 用法：bash scripts/ab_material_log.sh [bak 文件名]
set -uo pipefail
cd /opt/skillforge

NEW_PORT=${NEW_PORT:-8092}
OLD_PORT=${OLD_PORT:-8199}
OLD=${1:-$(ls -1t skillforge.bak-* 2>/dev/null | head -1)}
if [ -z "${OLD:-}" ]; then echo "没有 skillforge.bak-*，两臂只剩一臂 —— 无法做对照"; exit 2; fi
echo "旧臂文件: $OLD"

rm -rf /tmp/sf_matlog_old && mkdir -p /tmp/sf_matlog_old
cp -a /opt/skillforge/data /tmp/sf_matlog_old/data
set -a; . /opt/skillforge/skillforge.env; set +a
SKILLFORGE_ADDR=127.0.0.1:$OLD_PORT \
SKILLFORGE_DATA_DIR=/tmp/sf_matlog_old/data \
SKILLFORGE_DB=/tmp/sf_matlog_old/data/data-store.db \
  /opt/skillforge/$OLD > /tmp/sf_matlog_old_run.log 2>&1 &
OLDPID=$!
trap 'kill $OLDPID 2>/dev/null' EXIT
for i in $(seq 1 30); do
  curl -s -o /dev/null -m 2 "http://127.0.0.1:$OLD_PORT/" && break
  sleep 1
done

OLD_V=$(curl -s -m 5 "http://127.0.0.1:$OLD_PORT/api/version")
NEW_V=$(curl -s -m 5 "http://127.0.0.1:$NEW_PORT/api/version")
echo "旧臂版本: $OLD_V"
echo "新臂版本: $NEW_V"

# 判据 1：两臂必须是**不同 commit**。这里刻意不 print 完就往下走 —— 假对照比没对照更毒。
OLD_C=$(printf '%s' "$OLD_V" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("commit",""))' 2>/dev/null)
NEW_C=$(printf '%s' "$NEW_V" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("commit",""))' 2>/dev/null)
if [ -z "$OLD_C" ] || [ -z "$NEW_C" ]; then
  echo "FAKE_CONTRAST：有一臂拿不到 /api/version（旧=$OLD_C 新=$NEW_C），对照不成立"; exit 2
fi
if [ "$OLD_C" = "$NEW_C" ]; then
  echo "FAKE_CONTRAST：两臂同为 $OLD_C —— 这份对照证明不了任何事（要么换旧件，要么修复没上线）"; exit 2
fi
echo "对照成立：旧 $OLD_C  vs  新 $NEW_C"

echo "===== A) 旧件（修复前）跑尺子 ====="
cd /root/skillforge
SF_HOST=127.0.0.1 SF_PORT=$OLD_PORT SF_LIMIT=${SF_LIMIT:-170} SF_DUMP=/tmp/matlog_old.tsv \
  timeout 240 python3 scripts/sse_material_log_gate.py > /tmp/matlog_ab_old.log 2>&1
OLD_RC=$?
cat /tmp/matlog_ab_old.log
echo "OLD_RC=$OLD_RC"

echo "===== B) 新件（修复后，线上 $NEW_PORT）跑尺子 ====="
SF_HOST=127.0.0.1 SF_PORT=$NEW_PORT SF_LIMIT=${SF_LIMIT:-170} SF_DUMP=/tmp/matlog_new.tsv \
  timeout 240 python3 scripts/sse_material_log_gate.py > /tmp/matlog_ab_new.log 2>&1
NEW_RC=$?
cat /tmp/matlog_ab_new.log
echo "NEW_RC=$NEW_RC"

kill $OLDPID 2>/dev/null
FAIL=0
[ "$OLD_RC" = "1" ] || { echo "✗ 旧臂没有红（RC=$OLD_RC）—— 尺子丢了牙，或旧件拿错了"; FAIL=1; }
case "$(cat /tmp/matlog_ab_old.log)" in
  *"L1 ✗"*) : ;;
  *) echo "✗ 旧臂红的位置不对（预期 L1：执笔段一个非空 material_log 都没有）"; FAIL=1 ;;
esac
[ "$NEW_RC" = "0" ] || { echo "✗ 新臂没绿（RC=$NEW_RC）—— 修复没上线，或把尺子写歪了"; FAIL=1; }
if [ "$FAIL" = "1" ]; then echo "AB_MATERIAL_LOG=FAIL"; exit 1; fi
echo "AB_MATERIAL_LOG=OK（旧 $OLD_C 红在 L1、新 $NEW_C 全绿）"
