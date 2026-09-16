#!/usr/bin/env bash
# 看守：等 tag 对应的 Release workflow 跑完，打印每条 job 的结论
set -u
TAG="${1:?用法: watch_release.sh <tag>}"
cd /root/skillforge || exit 1
for i in $(seq 1 300); do
  J="$(gh run list --workflow Release --limit 15 \
        --json databaseId,headBranch,status,conclusion \
        --jq "[.[] | select(.headBranch==\"$TAG\")] | .[0]" 2>/dev/null)"
  [ -z "$J" ] || [ "$J" = "null" ] && { sleep 20; continue; }
  ST="$(printf '%s' "$J" | python3 -c 'import json,sys;print(json.load(sys.stdin)["status"])')"
  ID="$(printf '%s' "$J" | python3 -c 'import json,sys;print(json.load(sys.stdin)["databaseId"])')"
  if [ "$ST" = "completed" ]; then
    echo "=== $TAG 的 Release run $ID 已完成 ==="
    gh run view "$ID" --json conclusion,displayTitle \
      --jq '"总结论: \(.conclusion)\n标题: \(.displayTitle)"'
    echo "--- 各 job 结论 ---"
    gh run view "$ID" --json jobs \
      --jq '.jobs[] | "\(.conclusion // .status)\t\(.name)"'
    exit 0
  fi
  sleep 20
done
echo "=== 看守超时：200 分钟内没等到 $TAG 的 run 完成 ==="
exit 1
