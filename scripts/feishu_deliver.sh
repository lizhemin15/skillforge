#!/usr/bin/env bash
# 飞书云盘交付收口：转移所有权 + 回读核对（owner / 字节数 / 下载比对）
# 用法：bash scripts/feishu_deliver.sh <file_token> <本地原件路径>
set -euo pipefail
FTK="${1:?用法: bash scripts/feishu_deliver.sh <file_token> <local_path>}"
LOCAL="${2:?需要本地原件路径}"
USER_OPENID="ou_dc8488ada00f1c9d40c4ca04f413155e"

echo "=== 1) 加协作者（转移所有权的前置）==="
lark api POST "/open-apis/drive/v1/permissions/$FTK/members" \
  --params '{"type":"file","need_notification":"false"}' \
  --data "{\"member_type\":\"openid\",\"member_id\":\"$USER_OPENID\",\"perm\":\"full_access\"}" \
  --as bot | head -c 300; echo

echo "=== 2) 转移所有权给用户 ==="
lark api POST "/open-apis/drive/v1/permissions/$FTK/members/transfer_owner" \
  --params '{"type":"file","need_notification":true,"remove_old_owner":false}' \
  --data "{\"member_type\":\"openid\",\"member_id\":\"$USER_OPENID\"}" \
  --as bot | head -c 300; echo

echo "=== 3) 回读核对 owner（不看返回码，看真值）==="
lark api POST /open-apis/drive/v1/metas/batch_query --as bot \
  --data "{\"request_docs\":[{\"doc_token\":\"$FTK\",\"doc_type\":\"file\"}],\"with_url\":true}" \
  | python3 -c "
import sys, json
d = json.load(sys.stdin)
want = '$USER_OPENID'
ms = d.get('data', {}).get('metas', [])
if not ms:
    print('  FAIL: batch_query 无 metas'); sys.exit(1)
m = ms[0]
ok = m.get('owner_id') == want
print('  title =', m.get('title'))
print('  url   =', m.get('url'))
print('  owner =', m.get('owner_id'), '->', '✅ 已是用户' if ok else '❌ 不是用户')
sys.exit(0 if ok else 1)
"

echo "=== 4) 端到端完整性：从云盘下回来逐字节比对 ==="
# 注意：lark 的 --output/-o 只接受「相对路径」，绝对路径会报 unsafe output path
# 且报错若被 2>&1||true 吞掉，会留下 0 字节文件 → 假红（误判上传被截断）。别吞 stderr。
TMPD="$(mktemp -d)"; ( cd "$TMPD" && lark drive +download --file-token "$FTK" --as bot --output ./dl.bin --overwrite >/dev/null )
DL="$TMPD/dl.bin"
L="$(stat -c%s "$LOCAL")"; R="$(stat -c%s "$DL" 2>/dev/null || echo 0)"
echo "  本地 $L B / 云盘 $R B"
[ "$L" = "$R" ] || { echo "  FAIL: 字节数不符 → 上传可能被截断"; rm -rf "$TMPD"; exit 2; }
cmp "$LOCAL" "$DL" && echo "  ok: 逐字节相同"
sha256sum "$LOCAL" "$DL" | awk '{print "  sha256 "$1}'
rm -rf "$TMPD"
echo "=== 交付收口完成 ==="
