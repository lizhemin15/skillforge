#!/usr/bin/env bash
#
# assert_skill_material.sh <技能目录> [主题词]
#
# B/C 两条断言的**独立实现**，单独成文件的原因是可被「注入自证」复用：
# 把一份训练产物复制出来、人为抠掉扫描页正文，再拿这份断言去跑，必须变红。
# 断言与被测物同在一处、无法被单独注入破坏，是这道防线能不能抓住 bug 的分水岭。
#
#   B) 素材层：skill 的 source/*.txt 必须同时含「扫描页正文」与「文字层页正文」
#   C) 产物层：skill 产出（system_prompt.md / template.md / meta.json / examples/…）
#              必须出现素材独有主题词 —— 技能得真的由这份素材长出来
#
# 退出码：0 全部通过；1 有断言不通过。
set -uo pipefail

SK="${1:?用法: assert_skill_material.sh <技能目录> [主题词]}"
TOPIC="${2:-蒲公英月报}"
SCAN_SENTENCE="落款联系人固定写"   # 只存在于扫描页图片里
TEXT_SENTENCE="写作检查清单"       # 只存在于文字层页

FAILED=0
[ -d "$SK" ] || { echo "FAIL: 技能目录不存在 $SK"; exit 1; }

scan_hits=0
text_hits=0
for f in "$SK"/source/*.txt; do
  [ -f "$f" ] || continue
  grep -q "$SCAN_SENTENCE" "$f" && { echo "  ok: 扫描页正文已落地 $(basename "$f")"; scan_hits=$((scan_hits+1)); }
  grep -q "$TEXT_SENTENCE" "$f" && text_hits=$((text_hits+1))
done
[ "$scan_hits" -gt 0 ] || { echo "FAIL: B) 没有素材文本含扫描页正文（$SCAN_SENTENCE）—— 扫描页被静默丢了"; FAILED=1; }
[ "$text_hits" -gt 0 ] || { echo "FAIL: B) 没有素材文本含文字层页正文（$TEXT_SENTENCE）"; FAILED=1; }

# 产物层：把 source/ 排除掉 —— 素材原文含主题词是理所当然的，证明不了技能与素材相关。
HITS="$(grep -rl "$TOPIC" "$SK" 2>/dev/null | grep -v '/source/' | head -10)"
if [ -n "$HITS" ]; then
  echo "  ok: 产物由素材长出来（命中 $TOPIC）："
  printf '    %s\n' $HITS
else
  echo "FAIL: C) 产物里找不到素材主题词「$TOPIC」—— 技能与素材无关"
  FAILED=1
fi

exit "$FAILED"
