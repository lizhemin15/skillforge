#!/usr/bin/env bash
# 线上两条 leg 顺序跑（并行会抢同一个模型，谁也测不准），带时间线。
cd /root/skillforge
export BASE=http://127.0.0.1:8092
export DUMP_TIMELINE=1
export MAXW=420

echo "############ LEG A: chat_material_e2e.py writing（材料必须早于正文出现在屏幕上）"
PROMPT_KEY=writing python3 web/tests/chat_material_e2e.py
echo "MAT_RC=$?"

echo
echo "############ LEG B: chat_followup_artifact_e2e.py news2word（新闻稿 → 转 Word，搬运锚串）"
python3 web/tests/chat_followup_artifact_e2e.py
echo "FUP_RC=$?"
