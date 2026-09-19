#!/usr/bin/env bash
# A/B 同题对照：旧二进制（本次修复前）vs 新二进制，跑同一把尺子。
# 旧件跑在 8199 + 数据目录副本，绝不碰线上 8092 的库。
set -u
cd /opt/skillforge
OLD=$(ls -1t skillforge.bak-* | head -1)
echo "旧件: $OLD"
rm -rf /tmp/sf_old && mkdir -p /tmp/sf_old
cp -a /opt/skillforge/data /tmp/sf_old/data
set -a; . /opt/skillforge/skillforge.env; set +a
SKILLFORGE_ADDR=127.0.0.1:8199 \
SKILLFORGE_DATA_DIR=/tmp/sf_old/data \
SKILLFORGE_DB=/tmp/sf_old/data/data-store.db \
  /opt/skillforge/$OLD > /tmp/sf_old_run.log 2>&1 &
OLDPID=$!
echo "旧件 pid=$OLDPID"
for i in $(seq 1 30); do
  curl -s -o /dev/null -m 2 http://127.0.0.1:8199/ && break
  sleep 1
done
echo "===== A) 旧件（修复前）====="
cd /root/skillforge
SF_PORT=8199 SF_LIMIT=130 SF_DUMP=/tmp/frames_old.tsv timeout 240 python3 scripts/sse_material_gate.py > /tmp/gate_ab_old.log 2>&1
echo "OLD_RC=$?" >> /tmp/gate_ab_old.log
kill $OLDPID 2>/dev/null
sleep 2
echo "===== B) 新件（修复后，线上 8092）====="
SF_PORT=8092 SF_LIMIT=130 SF_DUMP=/tmp/frames_new2.tsv timeout 240 python3 scripts/sse_material_gate.py > /tmp/gate_ab_new.log 2>&1
echo "NEW_RC=$?" >> /tmp/gate_ab_new.log
echo "===== A 结果 ====="; cat /tmp/gate_ab_old.log
echo "===== B 结果 ====="; cat /tmp/gate_ab_new.log
