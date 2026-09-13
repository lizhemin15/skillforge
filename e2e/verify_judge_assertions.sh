#!/usr/bin/env bash
# verify_judge_assertions.sh —— 裁判层断言的双向自证（注入故障 → 红；还原 → 绿）。
#
# 为什么需要这个脚本：一条恒绿的断言比没有断言更糟——它让人以为有防线。
# 本脚本往**产品代码**里逐个注入真实故障（不动断言），要求对应用例变红；
# 还原后要求变绿。注入红不起来的，就是没盯住那件事的假断言。
#
# 用法：bash e2e/verify_judge_assertions.sh
# 退出码：0 = 每条注入都被抓住；1 = 有注入没被抓住（假断言）；2 = 环境不满足
set -u -o pipefail

cd "$(dirname "$0")/.."
export PATH=/usr/local/go/bin:$PATH
SRC=internal/skillgen/judge.go
BAK=$(mktemp)
PET=$(mktemp)
cp "$SRC" "$BAK"
# trap 里必须还原：脚本中途炸掉也不能把注入留在源码里。
cleanup() { cp "$BAK" "$SRC"; rm -f "$BAK" "$PET"; }
trap cleanup EXIT

if ! command -v go >/dev/null 2>&1; then
  echo "找不到 go，跳过（退出码 2，不算通过）" >&2
  exit 2
fi

run_test() { go test ./internal/skillgen/ -run "^$1\$" -count=1 >/tmp/vja.log 2>&1; }

# 基线：不注入时必须全绿，否则后面红了也说不清是谁的锅。
if ! go test ./internal/skillgen/ -count=1 >/tmp/vja_base.log 2>&1; then
  echo "基线就是红的，先修基线：" >&2
  tail -20 /tmp/vja_base.log >&2
  exit 2
fi
echo "基线绿 ✓"

fails=0

# inject <说明> <测试名> ；mutation 从 stdin 读（python，对变量 s 做替换）
inject() {
  local desc="$1" testname="$2"
  cp "$BAK" "$SRC"
  cat > "$PET"
  if ! python3 - "$SRC" "$PET" <<'PY' >/tmp/vja_inj.log 2>&1
import io, sys
path, mut = sys.argv[1], sys.argv[2]
s = io.open(path, encoding='utf-8').read()
ns = {'s': s}
exec(io.open(mut, encoding='utf-8').read(), ns)
s = ns['s']
if s == io.open(path, encoding='utf-8').read():
    print("NOCHANGE")
    sys.exit(3)
io.open(path, 'w', encoding='utf-8').write(s)
print("OK")
PY
  then
    echo "  !! 注入没打上（代码已变形，脚本要跟着改）：$desc"
    cat /tmp/vja_inj.log
    fails=$((fails+1))
    cp "$BAK" "$SRC"
    return
  fi
  if run_test "$testname"; then
    echo "  ✗ 注入故障但用例仍绿 —— $testname 没盯住：$desc"
    fails=$((fails+1))
  else
    echo "  ✓ 注入故障 → 红：$desc"
  fi
  cp "$BAK" "$SRC"
  if ! run_test "$testname"; then
    echo "  ✗ 还原后仍红 —— $testname 在干净代码上也过不了"
    fails=$((fails+1))
  fi
}

echo "---- 注入故障 ----"

# 1. 硬校验不再是否决项：残缺手册 + 模型满分 → 会被判通过。
inject "删掉硬校验否决（Pass 只看模型分）" \
  TestJudgeDraftFailsOnBrokenManualEvenWithFullMarks <<'PY'
s = s.replace("\tres.Pass = res.Pass && len(res.Hard) == 0", "\tres.Pass = res.Pass")
PY

# 2. 单维塌方规则失效：总分够就通过。
inject "拆掉单维下限（某维塌方也能过）" \
  TestParseJudgeOutputThresholds <<'PY'
s = s.replace("if floor := judgeDimFloor(d.Weight); s.Score < floor {", "if false {")
PY

# 3. 裁判不再独立：把审稿清单塞进裁判输入（技能自己给自己判卷）。
inject "把审稿清单喂给裁判（自己判自己卷）" \
  TestJudgeDraftIsIndependentFromSkillAndReviewer <<'PY'
s = s.replace("user := buildJudgeUserPrompt(cat, mp, material, draft)",
              "user := buildJudgeUserPrompt(cat, mp, material, draft) + \"\\n\" + mp.Reviewer")
PY

# 4. 试用不再走技能的提示词，改走裁判的（分数失去线上预测力）。
inject "试用改用裁判 prompt（分数失去预测力）" \
  TestTrialDraftUsesSkillPromptAndCategoryMaterial <<'PY'
s = s.replace("out, err := g.llm.Chat(ctx, sys, buildTrialInput(cat, material))",
              "out, err := g.llm.Chat(ctx, judgeSystemPrompt, buildTrialInput(cat, material))")
PY

# 4b. 试用不带本类要求/范文（回到「试用少喂料」那个坑：量出来的是缺料，不是技能差）。
#     注入后 sys 只剩技能自述，两条「带料」断言必须同时变红。
inject "试用不注入本类要求与范文（量的是缺料而非技能差）" \
  TestTrialDraftUsesSkillPromptAndCategoryMaterial <<'PY'
s = s.replace('''	if blk := trialPackBlock(mp, cat); blk != "" {
		sys = strings.TrimRight(sys, "\\n") + "\\n\\n" + blk
	}''', "	_ = mp // INJECT：故意不注入")
PY

# 5. 裁判调用不开 JSON 模式（裸奔会在中文理由上随机炸 json）。
inject "裁判调用不再开 JSON 模式" \
  TestJudgeDraftCallsWithJSONModeAndScores <<'PY'
s = s.replace("out, err := g.llm.Chat(ctx, judgeSystemPrompt, user, true)",
              "out, err := g.llm.Chat(ctx, judgeSystemPrompt, user)")
PY

# 6. 判了不通过却不留意见：回炉没有输入。
inject "判不通过但清空扣分项（回炉空手）" \
  TestJudgeDraftCallsWithJSONModeAndScores <<'PY'
s = s.replace("res.Findings = dedupStrings(append(res.Hard, res.Findings...))",
              "res.Findings = nil")
PY

# 7. 硬校验漏掉「某类一篇范文都没切出来」。
inject "硬校验漏掉范文缺失" \
  TestJudgeHardFindingsMissingCategoryExamples <<'PY'
old = "\t\tif len(segs) == 0 {\n\t\t\tfindings = append(findings, fmt.Sprintf(\n\t\t\t\t\"分类「%s」：手册标了 %d 处范文锚点，但一篇都没切出来（examples/%s/ 为空）\",\n\t\t\t\tc.Name, len(c.Anchor), safeCatFileName(c.Name)))\n\t\t}\n"
assert old in s, "missing-examples block 变形"
s = s.replace(old, "")
PY

# 8. 「没核对过」表现成「核对全过」：缺少核对源时报「无法核对保真」的分支被删。
inject "缺核对源时假装核对通过" \
  TestJudgeHardFindingsNoSourceToCheck <<'PY'
old = "\t\t\tif mp.Source == \"\" {\n\t\t\t\tfindings = append(findings, fmt.Sprintf(\n\t\t\t\t\t\"无法核对保真：examples/%s/%02d.md 缺少核对源（手册原文未留存）\",\n\t\t\t\t\tsafeCatFileName(c.Name), i+1))\n\t\t\t\tcontinue\n\t\t\t}\n"
assert old in s, "no-source block 变形"
s = s.replace(old, "")
PY

# 9. 保真核对放宽：「长度不是 0 就算原文」——被模型改写的范文不再被抓。
inject "保真核对放宽（改词也放行）" \
  TestJudgeHardFindingsNonOriginalExample <<'PY'
s = s.replace("if !strings.Contains(mp.Source, seg) {", "if len(seg) == 0 {")
PY

# 10. 轮次表把结论写反（fidelity.md 里「通过」和「不通过」颠倒）。
inject "轮次表结论写反" \
  TestJudgeRoundTable <<'PY'
s = s.replace("if !r.Result.Pass {\n\t\t\tverdict = \"不通过\"", "if r.Result.Pass {\n\t\t\tverdict = \"不通过\"")
PY

echo "---- 结果 ----"
cp "$BAK" "$SRC"
if ! go test ./internal/skillgen/ -count=1 >/tmp/vja_final.log 2>&1; then
  echo "还原后整包测试红，脚本自己坏了：" >&2
  tail -20 /tmp/vja_final.log >&2
  exit 2
fi
if [ "$fails" -ne 0 ]; then
  echo "有 $fails 条断言没抓住对应故障 —— 这些断言是假的。" >&2
  exit 1
fi
echo "全部注入都被抓住、还原后全绿 ✓"
