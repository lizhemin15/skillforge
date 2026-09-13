package skillgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 裁判评分表的落盘断言（s7d）。
//
// 这一组的靶子是「没被验收过的技能看起来像被验收过」。fidelity.md 是管理员唯一的
// 事后凭证，一旦它在以下四种情况里含糊——没跑成、提前止损、带薄弱项交付、压根没启用——
// 读报告的人就没法区分「验过且过」和「没验」。
//
// 注意本文件里的 DIM 常量与 judgeScorecard 一一对应（权重和 100）：
// 故意用真实维度的 Key，而不是自造字段名，否则渲染层取错维度也不会红。

// judgeDimsFixture 造一组五维分数：routing/req 满分，例文对齐丢 5 分 → 总分 95。
// 但**不通过**的场景更常见（回炉一次），所以由调用方决定 Pass。
func judgeDimsFixture(exampleScore int) []JudgeDim {
	return []JudgeDim{
		{Key: "category_routing", Label: "分类判定", Weight: 25, Score: 25},
		{Key: "requirement_compliance", Label: "要求依从", Weight: 25, Score: 25},
		{Key: "example_alignment", Label: "范文对齐", Weight: 20, Score: exampleScore},
		{Key: "structure_completeness", Label: "结构完整", Weight: 15, Score: 15},
		{Key: "no_hallucination", Label: "无幻觉", Weight: 15, Score: 15},
	}
}

// judgePack 走真实切分路径把包落盘，返回 fidelity.md 正文（含裁判段落）。
func judgePack(t *testing.T, rep *JudgeReport) string {
	t.Helper()
	st := fidelityAnchors(false)
	mp := &manualPack{
		Structure: st,
		Examples:  map[string][]string{},
		Paths:     map[string][]string{},
		Source:    fidelityDoc,
		Judge:     rep,
	}
	for _, c := range st.Categories {
		segs, err := SplitByAnchors(fidelityDoc, c.Anchor)
		if err != nil {
			t.Fatalf("切分失败（用例构造问题）: %v", err)
		}
		mp.Examples[c.Name] = segs
	}
	dir := t.TempDir()
	if _, err := mp.WriteTo(dir); err != nil {
		t.Fatalf("WriteTo 失败: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "fidelity.md"))
	if err != nil {
		t.Fatalf("fidelity.md 未落盘: %v", err)
	}
	return string(b)
}

// 每轮一行表：轮次 / 分类 / 分数 / 结论 / 扣分项，缺一列就无从复核。
func TestFidelityJudgeTableListsEveryRound(t *testing.T) {
	rep := &JudgeReport{
		Rounds: []JudgeRound{
			{Round: 1, Category: "经营业绩", Result: &JudgeResult{
				Total: 62, Pass: false, Dims: judgeDimsFixture(12),
				Findings: []string{"范文对齐不足：未复用原文的数据句式"},
			}},
			{Round: 2, Category: "产品发布", Result: &JudgeResult{
				Total: 95, Pass: true, Dims: judgeDimsFixture(15),
			}},
		},
		BestRound: 2,
	}
	got := judgePack(t, rep)
	for _, want := range []string{
		"## 裁判评分（独立评审 · 上限 ",
		"| 第 1 轮 | 经营业绩 | 62/100 | 不通过 |",
		"| 第 2 轮 | 产品发布 | 95/100 | 通过 |",
		"范文对齐不足：未复用原文的数据句式",
		"交付轮次：第 2 轮",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("fidelity.md 缺少 %q\n---\n%s", want, got)
		}
	}
}

// 交付轮次必须与表里那轮**是同一轮**：表格说第 2 轮 95 分、交付却写第 1 轮，
// 报告就成了自相矛盾的摆设。
func TestFidelityJudgeDeliveredRoundMatchesTable(t *testing.T) {
	rep := &JudgeReport{
		Rounds: []JudgeRound{
			{Round: 1, Category: "经营业绩", Result: &JudgeResult{Total: 91, Pass: false, Dims: judgeDimsFixture(16)}},
			{Round: 2, Category: "产品发布", Result: &JudgeResult{Total: 74, Pass: false, Dims: judgeDimsFixture(9)}},
		},
		BestRound: 1,
	}
	got := judgePack(t, rep)
	if !strings.Contains(got, "交付轮次：第 1 轮") {
		t.Errorf("交付轮次应取更优的第 1 轮（91 分）\n---\n%s", got)
	}
}

// 裁定「没跑成」必须写进报告：这是裁判层最容易被静默掉的一格——
// 报告里干净整洁，读的人以为验过了，其实一次都没验。
//
// 这里额外盯一个坑：启用过但一次都没跑成时，报告**不能**写「未启用裁判评分」。
// 那是关于配置的断言、而且和「⚠️ 裁判未跑完」当场打架：读者会不知道该信哪句。
func TestFidelityJudgeFailureIsSurfacedNotSilenced(t *testing.T) {
	got := judgePack(t, &JudgeReport{Err: "裁判调用失败: 上游 502"})
	if !strings.Contains(got, "裁判未跑完") || !strings.Contains(got, "上游 502") {
		t.Errorf("裁判失败必须显性写明原因\n---\n%s", got)
	}
	if strings.Contains(got, "交付轮次") || strings.Contains(got, "| 通过 |") {
		t.Errorf("没跑成的裁判不得留下「通过」或交付轮次的痕迹\n---\n%s", got)
	}
	if strings.Contains(got, "未启用裁判评分") {
		t.Errorf("裁判启用过只是没跑成，不得写成「未启用」\n---\n%s", got)
	}
}

// 提前止损要写明原因，否则「只有 1 轮」会被读成「一轮就过了」。
func TestFidelityJudgeEarlyStopIsExplained(t *testing.T) {
	rep := &JudgeReport{
		Rounds: []JudgeRound{
			{Round: 1, Category: "经营业绩", Result: &JudgeResult{Total: 40, Pass: false, Dims: judgeDimsFixture(5)}},
		},
		BestRound: 1,
		EarlyStop: "扣分项全部来自确定性硬校验（手册数据缺陷），回炉改不动",
	}
	got := judgePack(t, rep)
	if !strings.Contains(got, "提前止损：") || !strings.Contains(got, "回炉改不动") {
		t.Errorf("提前止损必须写明原因\n---\n%s", got)
	}
}

// 压根没启用裁判时也要说一句：留白和「验过了」在报告里长得太像。
func TestFidelityJudgeDisabledIsStated(t *testing.T) {
	got := judgePack(t, nil)
	if !strings.Contains(got, "未启用裁判评分") {
		t.Errorf("未启用裁判时必须显性说明，不能留白\n---\n%s", got)
	}
}

// 薄弱维度要落到报告里：同分两轮的短板可能完全不同，只给总分等于让管理员无从下手。
func TestFidelityJudgeWeakDimsAreNamed(t *testing.T) {
	rep := &JudgeReport{
		Rounds: []JudgeRound{
			{Round: 1, Category: "经营业绩", Result: &JudgeResult{Total: 90, Pass: false, Dims: judgeDimsFixture(10)}},
		},
		BestRound: 1,
	}
	got := judgePack(t, rep)
	if !strings.Contains(got, "薄弱维度：") || !strings.Contains(got, "范文对齐 10/20") {
		t.Errorf("薄弱维度应点名维度与得分\n---\n%s", got)
	}
}
