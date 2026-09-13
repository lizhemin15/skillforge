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
SRC=internal/skillgen/judge.go     # 默认注入目标
# 裁判层横跨两个文件：判分/循环在 judge.go，报告渲染在 manual.go 的 writeFidelity。
# 两边都要能注入，否则「报告里撒谎」那一类故障没人盯。
FILES="internal/skillgen/judge.go internal/skillgen/manual.go"
BAKDIR=$(mktemp -d)
PET=$(mktemp)
restore_all() {
  local f
  for f in $FILES; do cp "$BAKDIR/$(basename "$f")" "$f"; done
}
for f in $FILES; do cp "$f" "$BAKDIR/$(basename "$f")"; done
# trap 里必须还原：脚本中途炸掉也不能把注入留在源码里。
cleanup() { restore_all; rm -rf "$BAKDIR" "$PET"; }
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

# inject_in <目标文件> <说明> <测试名> ；mutation 从 stdin 读（python，对变量 s 做替换）
inject_in() {
  local src="$1" desc="$2" testname="$3"
  restore_all
  cat > "$PET"
  if ! python3 - "$src" "$PET" <<'PY' >/tmp/vja_inj.log 2>&1
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
    restore_all
    return
  fi
  if run_test "$testname"; then
    echo "  ✗ 注入故障但用例仍绿 —— $testname 没盯住：$desc"
    fails=$((fails+1))
  else
    echo "  ✓ 注入故障 → 红：$desc"
  fi
  restore_all
  if ! run_test "$testname"; then
    echo "  ✗ 还原后仍红 —— $testname 在干净代码上也过不了"
    fails=$((fails+1))
  fi
}

# inject <说明> <测试名>：注入点默认在 judge.go（绝大多数规则在这一层）。
inject() { inject_in "$SRC" "$1" "$2"; }

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

# ---- Step 8.5 裁判循环的止损规则与交付决策（s7c）----
# 循环跑偏不会报错，只会静默变差：白烧模型调用、交了个没验收过的技能、
# 或者把比手上更差的一轮当成产物交出去。所以每一条止损规则都要有注入盯着。

# 11. 「通过即停」被拆：过线后继续烧钱试用，还可能试出更差的一版。
inject "通过后不停手（继续白烧模型调用）" \
  TestJudgeLoopStopsWhenFirstRoundPasses <<'PY'
old = "\t\tif res.Pass {\n\t\t\tbreak\n\t\t}\n"
assert old in s, "pass-break 块变形"
s = s.replace(old, "\t\tif res.Pass {\n\t\t\t_ = res\n\t\t}\n", 1)
PY

# 12. 交付「最后一轮」而不是「最优一轮」：回炉把分数改低了也照交。
inject "交付最后一轮而非最优一轮" \
  TestJudgeLoopRevisesUntilRoundCap <<'PY'
s = s.replace("if best := rep.Best(); best != nil {",
              "if best := &rep.Rounds[len(rep.Rounds)-1]; best != nil {")
PY

# 13. 硬校验止损被拆：手册缺范文这种回炉改不动的问题，被摊薄成几轮低分。
inject "拆掉硬校验止损（手册缺陷被摊薄成低分）" \
  TestJudgeLoopEarlyStopsWhenAllFindingsAreHard <<'PY'
s = s.replace("\t\tif allFindingsHard(res) {", "\t\tif false {")
PY

# 14. 没有标尺时不报原因：静默放行（裁判层最不该有的沉默）。
inject "无手册结构时不报原因" \
  TestJudgeLoopErrorsWithoutManualInsteadOfPassing <<'PY'
s = s.replace('rep.Err = "无手册结构，裁判无从对齐标尺"', 'rep.Err = ""')
PY

# 15. trace 帧丢掉分类名：只剩「62 分」，管理员无从知道按哪一类的标尺量的。
inject "裁判 trace 帧丢掉分类名" \
  TestJudgeLoopStopsWhenFirstRoundPasses <<'PY'
s = s.replace('fmt.Sprintf("裁判第 %d 轮（%s）：%d/100 %s", r.Round, r.Category, r.Result.Total, verdict)',
              'fmt.Sprintf("裁判第 %d 轮（%s）：%d/100 %s", r.Round, "", r.Result.Total, verdict)')
PY

# 16. 回炉产出空提示词不拦：空壳顶替可用版本被交付出去。
inject "空提示词回炉产出被放行" \
  TestJudgeLoopRejectsEmptyReviseOutput <<'PY'
old = '\t\tif strings.TrimSpace(next) == "" {\n\t\t\trep.Err = "回炉产出的提示词为空"\n\t\t\tsteps("8.5/9 回炉产出的提示词为空（保留当前版本）")\n\t\t\tbreak\n\t\t}\n'
assert old in s, "empty-revise 块变形"
s = s.replace(old, "")
PY

# 17. 没有回炉通道却继续循环：在同一份提示词上重复试用，产出一堆同分轮次。
#     注入点要同时改两处（判断 + 回炉调用位置），否则会变成 nil 调用崩溃，
#     那样红的是「崩了」而不是「断言抓住了」，不算证据。
inject "无回炉通道仍继续循环（同稿重复试用）" \
  TestJudgeLoopWithoutReviseChannelStops <<'PY'
s = s.replace("\t\tif revise == nil {", "\t\tif revise == nil && false {")
s = s.replace("next, rErr := revise(ctx, res, cur)",
              "next, rErr := func(context.Context, *JudgeResult, string) (string, error) { return cur, nil }(ctx, res, cur)")
PY

# ---- fidelity.md 的裁判评分表（s7d）----
# 报告是管理员唯一的事后凭证。它撒谎的方式很安静：不写、少写、或者写一句
# 与事实相反的结论——从报告上看，没验过的技能和验过的长得一模一样。

# 18. 裁判失败不写进报告：报告干净整洁，读的人以为验过了。
inject_in internal/skillgen/manual.go "裁判失败不写进报告（假装验过了）" \
  TestFidelityJudgeFailureIsSurfacedNotSilenced <<'PY'
old = '\t\tif mp.Judge.Err != "" {\n\t\t\tfmt.Fprintf(&b, "- ⚠️ 裁判未跑完：%s\\n", mp.Judge.Err)\n\t\t}\n'
assert old in s, "judge-err 块变形"
s = s.replace(old, "")
PY

# 19. 压根没启用裁判时留白：留白和「验过了」在报告里长得太像。
inject_in internal/skillgen/manual.go "未启用裁判时留白" \
  TestFidelityJudgeDisabledIsStated <<'PY'
s = s.replace('b.WriteString("（未启用裁判评分）\\n")', 'b.WriteString("")')
PY

# 20. 无轮次时写「未启用裁判评分」：和「⚠️ 裁判未跑完」当场打架，
#     报告一边说没启用、一边说没跑完，读的人不知道该信哪句。
inject_in internal/skillgen/judge.go "无轮次谎报「未启用裁判评分」" \
  TestFidelityJudgeFailureIsSurfacedNotSilenced <<'PY'
s = s.replace('return "（没有可展示的判分记录）\\n"', 'return "（未启用裁判评分）\\n"')
PY

# 21. 轮次表丢分类名：只报分数，管理员不知道按哪一类的标尺量的。
inject "fidelity 轮次表丢掉分类名" \
  TestFidelityJudgeTableListsEveryRound <<'PY'
s = s.replace("r.Round, mdCell(r.Category), r.Result.Total, verdict, mdCell(joinLimit(r.Result.Findings, 3)))",
              "r.Round, \"\", r.Result.Total, verdict, mdCell(joinLimit(r.Result.Findings, 3)))")
PY

# 22. 交付轮次写成最后一轮：报告里的交付轮次与分数最优的那轮对不上。
inject_in internal/skillgen/manual.go "交付轮次写成最后一轮" \
  TestFidelityJudgeDeliveredRoundMatchesTable <<'PY'
s = s.replace("if mp.Judge.BestRound > 0 {", "if len(mp.Judge.Rounds) > 0 {\n\t\t\tmp.Judge.BestRound = len(mp.Judge.Rounds)\n\t\t}\n\t\tif mp.Judge.BestRound > 0 {")
PY

# 23. 不写薄弱维度：只给总分，管理员不知道短板在哪、也无从下手改。
inject_in internal/skillgen/manual.go "薄弱维度不落报告" \
  TestFidelityJudgeWeakDimsAreNamed <<'PY'
old = '\t\tif w := mp.Judge.WeakDims(3); len(w) > 0 {\n\t\t\tfmt.Fprintf(&b, "- 薄弱维度：%s\\n", strings.Join(w, " · "))\n\t\t}\n'
assert old in s, "weak-dims 块变形"
s = s.replace(old, "")
PY

echo "---- 结果 ----"
restore_all
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
