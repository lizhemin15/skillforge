#!/usr/bin/env bash
#
# verify_stats_selfcheck.sh —— 给 C1（逐页统计断言）做注入自证：断言本身是不是真的。
#
# 起因（真实事故）：verify_live_train_material.sh 的 C1 原来是
#     grep -q 'text_pages' train.sse && grep -q 'ocr_pages' train.sse
# 它在线上**永远不可能通过** —— 进度流里写的是人话「共 4 页（文本层直取 2 / OCR 2）」，
# 原始字段名 text_pages/ocr_pages 从不出现在 SSE 里。于是这条「绿灯」永远不会亮，
# 每次线上验收都白报一个 FAIL，把真正的信号（C2/C3 已通过）淹没掉。
# 教训：**断言要对自己测的东西取证**，不能凭字段名想象产物长什么样。
#
# 本脚本用真实线上抓的 SSE（fixtures/live_train_progress_mixed.sse）做料：
#   S1) 原样                        → 必须绿（否则断言过严 / 解析写错）
#   S2) 文本层直取 2 改成 0         → 必须红（可选中页被当扫描页、直取逻辑没生效）
#   S3) OCR 2 改成 0                → 必须红（扫描页没走 OCR，正文会丢）
#   S4) 删掉整行统计                → 必须红（压根没上报，不能算过）
#   S5) 老断言（grep 字段名）跑真 SSE → 必须红（存证：老断言是假断言，不是「代理挂了」）
#
# 用法：bash testdata/mixed/verify_stats_selfcheck.sh
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FIX="$HERE/fixtures/live_train_progress_mixed.sse"
PARSER="$HERE/stats_of_sse.py"
W="$(mktemp -d /tmp/verify_stats_selfcheck.XXXXXX)"; trap 'rm -rf "$W"' EXIT

PASS=0; FAILED=0
ok()  { printf '  ok   %s\n' "$*"; PASS=$((PASS+1)); }
bad() { printf '  FAIL %s\n' "$*"; FAILED=$((FAILED+1)); }

[ -s "$FIX" ] || { printf 'FAIL fixture 不存在：%s\n' "$FIX"; exit 1; }
ok "fixture 就位（$(stat -c%s "$FIX")B，真实线上抓的进度流）"

# 期望通过的用例：跑一次，要求调用方给「必须绿」
want_pass() { # $1=用例名 $2=文件
  local out rc
  out="$(python3 "$PARSER" "$2" --expect 4 2 2 2>&1)"; rc=$?
  if [ "$rc" -eq 0 ]; then ok "$1 判绿（$out）"; else bad "$1 本应判绿却判红：$out"; fi
}
# 期望判红的用例
want_fail() { # $1=用例名 $2=文件 $3=期望在输出里出现的关键词
  local out rc
  out="$(python3 "$PARSER" "$2" --expect 4 2 2 2>&1)"; rc=$?
  if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q -- "$3"; then
    ok "$1 判红：$out"
  elif [ "$rc" -ne 0 ]; then
    bad "$1 判红了但原因不对（期待含「$3」，实际：$out）"
  else
    bad "$1 本应判红却判绿（$out）"
  fi
}

# S1 原样
cp "$FIX" "$W/s1.sse"; want_pass "S1 原样（4 页 = 直取 2 + OCR 2）" "$W/s1.sse"

# S2 可选中页被强行送去 OCR：文本层直取 2 → 0
sed 's/文本层直取 2/文本层直取 0/' "$FIX" > "$W/s2.sse"
want_fail "S2 文本层直取 2→0（可选中页白跑 OCR）" "$W/s2.sse" "对不上"

# S3 扫描页没走 OCR：OCR 2 → 0
sed 's|/ OCR 2|/ OCR 0|' "$FIX" > "$W/s3.sse"
want_fail "S3 OCR 2→0（扫描页正文会丢）" "$W/s3.sse" "对不上"

# S4 整行统计没了
grep -v '文本层直取' "$FIX" > "$W/s4.sse"
want_fail "S4 没有统计行（压根没上报）" "$W/s4.sse" "没有"

# S5 反向存证：老的字段名 grep 断言在真实 SSE 上必然是红的。
#    这条不测新代码，它证明「C1 原来那个 FAIL 是断言本身坏了」，而不是线上坏了。
if grep -q 'text_pages' "$FIX" && grep -q 'ocr_pages' "$FIX"; then
  bad "S5 老断言在真实 SSE 上竟然命中 —— fixture 不是真实产物？"
else
  ok "S5 老断言（grep text_pages/ocr_pages）在真实 SSE 上必然判红 → 确认原 C1 是假断言"
fi

printf '\n%d 项通过，%d 项失败' "$PASS" "$FAILED"
if [ "$FAILED" -eq 0 ]; then printf ' ✅\n'; exit 0; else printf ' ❌\n'; exit 1; fi
