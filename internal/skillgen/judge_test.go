package skillgen

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 训练期裁判（judge.go）的回归防线。
//
// 为什么这一层必须有测试，而且必须有替身模型：
//   - 判分本身是模型给的，拿真模型只能看「像不像」、每次分数都不同，
//     没法把断言钉住；
//   - 但「残缺手册必须被判不通过并点名范文缺失」是**产品硬要求**，
//     属于确定性结论，必须能机械复现。
// 所以这里注入替身客户端，把「裁判输出什么 → 我们判成什么」这条链路
// 完整地钉死在合成样本上（不依赖本机任何真实素材）。
//
// 双向自证（本文件每条防线都做过）：
//   - 正常样本 → 绿；
//   - 注入故障（删范文 / 改词 / 去掉某维 / 分数越界）→ 对应用例变红；
//   - 还原 → 绿。
// 只做「注入故障」不算数：还得确认那个故障恰好打在断言的靶心上，
// 所以故障都造在**内容**上（删条目、改词），不只动标点——标点差异会被
// 归一化救回来，那样测出来的红是假的。

// ---- 替身模型 ----

type fakeCall struct {
	Sys     string
	User    string
	JSON    bool
	JSONSet bool
}

// fakeChat 是 chatClient 的替身：把每次调用记下来，按调用方给的 reply 出结果。
// 记 Sys/User 是为了断言「裁判看到的输入里有什么、没有什么」——
// 独立性（裁判看不到技能自述与审稿清单）不是靠注释声明的，是靠这里断出来的。
type fakeChat struct {
	calls []fakeCall
	reply func(call fakeCall) (string, error)
}

func (f *fakeChat) Chat(_ context.Context, sys, user string, jsonMode ...bool) (string, error) {
	c := fakeCall{Sys: sys, User: user}
	if len(jsonMode) > 0 {
		c.JSON, c.JSONSet = jsonMode[0], true
	}
	f.calls = append(f.calls, c)
	if f.reply == nil {
		return "", errors.New("fakeChat: 未设置 reply")
	}
	return f.reply(c)
}

// judgeJSON 拼一份合法的裁判输出：dims 五维全给。
func judgeJSON(scores map[string]int, findings ...string) string {
	order := []string{"category_routing", "requirement_compliance", "example_alignment", "structure_completeness", "no_hallucination"}
	var b strings.Builder
	b.WriteString(`{"dims":[`)
	for i, k := range order {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"key":"` + k + `","score":`)
		b.WriteString(itoa(scores[k]))
		b.WriteString(`,"reason":"r"}`)
	}
	b.WriteString(`],"findings":[`)
	for i, f := range findings {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"` + f + `"`)
	}
	b.WriteString(`]}`)
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	if neg {
		return "-" + string(d)
	}
	return string(d)
}

// ---- 合成手册样本 ----

// 合成手册：两章、两类，每类一段范文。范文用独特标记句包住，
// 逐字写进 Source —— 这样「范文是原文连续子串」这条断言是机械可验的，
// 不靠字数、不靠顺序（改排版不会假红）。
const (
	synMarkMinuteStart = "标记句-纪要甲-开始"
	synMarkMinuteEnd   = "标记句-纪要甲-结束"
	synMarkSummaryS    = "标记句-总结甲-开始"
	synMarkSummaryE    = "标记句-总结甲-结束"
	synReqMinute       = "写作要求：标题不超过二十二个字；正文分条列明议定事项。"
	synReqSummary      = "写作要求：须有数据支撑；结尾写明下一步计划。"
)

func synthSourceText() string {
	return "第一章 会议纪要\n\n" + synReqMinute + "\n\n范文一：\n" +
		synMarkMinuteStart + "\n关于压降库存的专题会会议纪要\n会议议定：一、压降库存周转天数；二、每周复盘。\n" + synMarkMinuteEnd + "\n\n" +
		"第二章 工作总结\n\n" + synReqSummary + "\n\n范文一：\n" +
		synMarkSummaryS + "\n二〇二六年上半年工作总结\n上半年库存周转天数下降十二天。\n" + synMarkSummaryE + "\n"
}

// synthPack 造一份「健康」的手册产物：两类各有锚点、各有范文，范文逐字出自原文。
func synthPack() *manualPack {
	src := synthSourceText()
	seg := func(a, b string) string {
		i := strings.Index(src, a)
		j := strings.Index(src, b)
		return src[i : j+len(b)]
	}
	return &manualPack{
		Structure: &Structure{
			General: "总则：语言平实，不得使用网络流行语。",
			Categories: []Category{
				{Name: "会议纪要", Trigger: "需要记录会议议定事项时", Requirement: synReqMinute,
					Anchor: []CatAnchor{{Start: synMarkMinuteStart, End: synMarkMinuteEnd}}},
				{Name: "工作总结", Trigger: "需要汇报阶段性工作时", Requirement: synReqSummary,
					Anchor: []CatAnchor{{Start: synMarkSummaryS, End: synMarkSummaryE}}},
			},
		},
		Examples: map[string][]string{
			"会议纪要": {seg(synMarkMinuteStart, synMarkMinuteEnd)},
			"工作总结": {seg(synMarkSummaryS, synMarkSummaryE)},
		},
		Paths:    map[string][]string{},
		Reviewer: "# 稿件自查清单\n\n- " + synReviewerMark,
		Source:   src,
	}
}

// synReviewerMark 只出现在 reviewer.md 里：用来断言「裁判看不到审稿清单」。
const synReviewerMark = "独一无二的审稿清单标记"

// ---- 评分卡 ----

// TestJudgeScorecardIsWellFormed 守评分卡自身的健康：权重和 100、key 不重复、
// 下限比例落在 (0,1)、每维下限严格小于该维满分（否则「单维塌方」这条规则
// 在该维上等于要求满分——这正是本用例第一次跑就抓出来的真问题）。
func TestJudgeScorecardIsWellFormed(t *testing.T) {
	if got := judgeTotalWeight(); got != 100 {
		t.Fatalf("评分卡权重之和 = %d，应为 100", got)
	}
	if judgeDimFloorRatio <= 0 || judgeDimFloorRatio >= 1 {
		t.Fatalf("单维下限比例应在 (0,1) 之间，实际 %v", judgeDimFloorRatio)
	}
	seen := map[string]bool{}
	for _, d := range judgeScorecard {
		if d.Key == "" || d.Label == "" {
			t.Fatalf("评分卡维度缺 key/label: %+v", d)
		}
		if seen[d.Key] {
			t.Fatalf("评分卡维度 key 重复: %s", d.Key)
		}
		seen[d.Key] = true
		if floor := judgeDimFloor(d.Weight); floor >= d.Weight {
			t.Fatalf("维度 %s：下限 %d 不低于满分 %d，等于要求该维满分", d.Label, floor, d.Weight)
		}
	}
	// 设计稿写的是「单维 <15 分即不通过」。15 分是 25 分维度的 60%——
	// 用比例表达后，这个数字在它当时谈的维度上必须仍然逐字成立。
	if got := judgeDimFloor(25); got != 15 {
		t.Fatalf("满分 25 的维度下限应为 15（与设计稿一致），实际 %d", got)
	}
	if judgePassLine >= judgeTotalWeight() {
		t.Fatalf("通过线 %d 不低于满分 %d：这条线永远过不了", judgePassLine, judgeTotalWeight())
	}
}

// ---- 硬校验：健康样本必须干净 ----

func TestJudgeHardFindingsCleanOnHealthyPack(t *testing.T) {
	got := judgeHardFindings(synthPack())
	if len(got) != 0 {
		t.Fatalf("健康手册不应有任何硬校验命中，实际 %d 条：%v", len(got), got)
	}
}

func TestJudgeHardFindingsNilPack(t *testing.T) {
	got := judgeHardFindings(nil)
	if len(got) != 1 || !strings.Contains(got[0], "手册结构为空") {
		t.Fatalf("空手册应报「结构为空」，实际：%v", got)
	}
}

// TestJudgeHardFindingsMissingCategoryExamples 是最核心的一条：
// 「手册标了锚点、却一篇范文都没切出来」必须被确定性地判出来，并且**点名是哪个分类**。
// 断言用双条件（分类名 + 「为空」），只断分类名会被别处出现的同名文字假绿。
func TestJudgeHardFindingsMissingCategoryExamples(t *testing.T) {
	for _, cat := range []string{"会议纪要", "工作总结"} {
		mp := synthPack()
		delete(mp.Examples, cat)
		got := judgeHardFindings(mp)
		if !hasFinding(got, cat, "为空") {
			t.Fatalf("删掉「%s」的范文后，硬校验应点名该分类且指出为空，实际：%v", cat, got)
		}
		// 另一类仍是健康的，不该被连坐。
		other := "会议纪要"
		if cat == "会议纪要" {
			other = "工作总结"
		}
		if hasFinding(got, other, "为空") {
			t.Fatalf("删「%s」范文却连坐报了「%s」：%v", cat, other, got)
		}
	}
}

// TestJudgeHardFindingsAllExamplesEmpty：全部类别都没范文时，除逐类点名外
// 还要有一条「整体处于无范文可用状态」的结论——这是最该被拦住的情形。
func TestJudgeHardFindingsAllExamplesEmpty(t *testing.T) {
	mp := synthPack()
	mp.Examples = map[string][]string{}
	got := judgeHardFindings(mp)
	if !hasFinding(got, "全部 2 个分类", "无范文可用") {
		t.Fatalf("范文全空时应给出整体结论，实际：%v", got)
	}
	if len(got) < 3 {
		t.Fatalf("范文全空时应逐类点名 + 整体结论（≥3 条），实际 %d 条：%v", len(got), got)
	}
}

// TestJudgeHardFindingsNonOriginalExample：范文被模型改写（改词，不改标点）时必须抓出来。
// 故障造在**词**上而不是标点上——标点差异属于表层，别处有归一化会把它救回来，
// 那样「抓到」的就可能是假红。
func TestJudgeHardFindingsNonOriginalExample(t *testing.T) {
	mp := synthPack()
	orig := mp.Examples["会议纪要"][0]
	mp.Examples["会议纪要"][0] = strings.Replace(orig, "每周复盘", "每月复盘", 1)
	if mp.Examples["会议纪要"][0] == orig {
		t.Fatal("故障没造上：样本里找不到被替换的词，测试本身失效")
	}
	got := judgeHardFindings(mp)
	if !hasFinding(got, "范文非原文", "会议纪要/01.md") {
		t.Fatalf("改写后的范文应被判「范文非原文」并给出路径，实际：%v", got)
	}
}

// TestJudgeHardFindingsNoSourceToCheck：「没核对过」不能表现成「核对全过」。
func TestJudgeHardFindingsNoSourceToCheck(t *testing.T) {
	mp := synthPack()
	mp.Source = ""
	got := judgeHardFindings(mp)
	if len(got) != 2 || !hasFinding(got, "无法核对保真", "会议纪要/01.md") {
		t.Fatalf("缺少核对源时应对每篇范文报「无法核对保真」，实际：%v", got)
	}
}

// hasFinding 判断 findings 里是否存在同时包含所有关键词的一条。
func hasFinding(findings []string, kws ...string) bool {
	for _, f := range findings {
		ok := true
		for _, kw := range kws {
			if !strings.Contains(f, kw) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// ---- 裁判输出解析 ----

func TestParseJudgeOutputAcceptsArrayAndObjectShapes(t *testing.T) {
	full := map[string]int{"category_routing": 25, "requirement_compliance": 25, "example_alignment": 20, "structure_completeness": 15, "no_hallucination": 15}
	cases := []struct {
		name string
		in   string
	}{
		{"数组形状", judgeJSON(full)},
		{"对象带理由", `{"dims":{"category_routing":{"score":25,"reason":"好"},"requirement_compliance":{"score":25},"example_alignment":{"score":20},"structure_completeness":{"score":15},"no_hallucination":{"score":15}}}`},
		{"对象裸数字", `{"dims":{"category_routing":25,"requirement_compliance":25,"example_alignment":20,"structure_completeness":15,"no_hallucination":15}}`},
		{"外面裹寒暄", "好的，我的判分如下：\n" + judgeJSON(full) + "\n以上。"},
		{"包在代码块里", "```json\n" + judgeJSON(full) + "\n```"},
	}
	for _, c := range cases {
		got, err := parseJudgeOutput(c.in)
		if err != nil {
			t.Fatalf("%s：解析失败 %v", c.name, err)
		}
		if got.Total != 100 || !got.Pass {
			t.Fatalf("%s：满分输出应得 100 分且通过，实际 total=%d pass=%v", c.name, got.Total, got.Pass)
		}
		if len(got.Dims) != len(judgeScorecard) {
			t.Fatalf("%s：维度数应为 %d，实际 %d", c.name, len(judgeScorecard), len(got.Dims))
		}
	}
}

// TestParseJudgeOutputRejectsMissingDim：漏维度必须**报错**，不能按 0 记也不能
// 按满分补——后者会让残缺手册悄悄通过。
func TestParseJudgeOutputRejectsMissingDim(t *testing.T) {
	in := `{"dims":[{"key":"category_routing","score":25},{"key":"requirement_compliance","score":25},{"key":"example_alignment","score":20}],"findings":[]}`
	_, err := parseJudgeOutput(in)
	if err == nil {
		t.Fatal("漏两个维度却解析成功：静默降级")
	}
	for _, kw := range []string{"structure_completeness", "no_hallucination"} {
		if !strings.Contains(err.Error(), kw) {
			t.Fatalf("报错应点名缺失维度 %s，实际：%v", kw, err)
		}
	}
}

// TestParseJudgeOutputClampsOutOfRange：模型给超过满分的分数时夹回区间并留痕。
// 直接采信会让总分超过 100（评分卡就没有意义了）。
func TestParseJudgeOutputClampsOutOfRange(t *testing.T) {
	in := `{"dims":{"category_routing":120,"requirement_compliance":25,"example_alignment":20,"structure_completeness":15,"no_hallucination":15},"findings":[]}`
	got, err := parseJudgeOutput(in)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if got.Total != 100 {
		t.Fatalf("越界分数应夹回满分，总分应为 100，实际 %d", got.Total)
	}
	if !hasFinding(got.Findings, "评分越界", "分类判定") {
		t.Fatalf("越界应留下痕迹，实际 findings：%v", got.Findings)
	}
}

func TestParseJudgeOutputRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "我觉得写得不错，但不方便打分。", `{"dims":"五个维度"}`} {
		if _, err := parseJudgeOutput(in); err == nil {
			t.Fatalf("输入 %q 应报错，实际通过", in)
		}
	}
}

// TestParseJudgeOutputThresholds 钉住两条判定线：总分 ≥80 且无单维 <15。
// 单维下限是防「总分凑得够、某一维塌方」的假通过。
func TestParseJudgeOutputThresholds(t *testing.T) {
	cases := []struct {
		name  string
		dims  map[string]int
		pass  bool
		total int
	}{
		{"各维达标且总分 85", map[string]int{"category_routing": 20, "requirement_compliance": 20, "example_alignment": 15, "structure_completeness": 15, "no_hallucination": 15}, true, 85},
		{"15 分维丢 1 分（99）仍应通过", map[string]int{"category_routing": 25, "requirement_compliance": 25, "example_alignment": 20, "structure_completeness": 15, "no_hallucination": 14}, true, 99},
		{"单维塌方（无幻觉 5/15）", map[string]int{"category_routing": 25, "requirement_compliance": 25, "example_alignment": 20, "structure_completeness": 15, "no_hallucination": 5}, false, 90},
		{"各维均达标但总分只有 78", map[string]int{"category_routing": 16, "requirement_compliance": 16, "example_alignment": 16, "structure_completeness": 15, "no_hallucination": 15}, false, 78},
	}
	for _, c := range cases {
		got, err := parseJudgeOutput(judgeJSON(c.dims))
		if err != nil {
			t.Fatalf("%s：解析失败 %v", c.name, err)
		}
		if got.Total != c.total {
			t.Fatalf("%s：总分应为 %d，实际 %d", c.name, c.total, got.Total)
		}
		if got.Pass != c.pass {
			t.Fatalf("%s：结论应为 pass=%v，实际 %v（总分 %d）", c.name, c.pass, got.Pass, got.Total)
		}
	}
}

// ---- 裁判调用：独立性、JSON 模式、不通过必有理由 ----

func TestJudgeDraftCallsWithJSONModeAndScores(t *testing.T) {
	mp := synthPack()
	cat := mp.Structure.Categories[0]
	fake := &fakeChat{reply: func(fakeCall) (string, error) {
		return judgeJSON(map[string]int{"category_routing": 18, "requirement_compliance": 12, "example_alignment": 10, "structure_completeness": 10, "no_hallucination": 15}), nil
	}}
	g := &Generator{}
	g.SetChatClient(fake)

	res, err := g.judgeDraft(context.Background(), mp, cat, "素材：库存周转天数压降。", "草稿正文。")
	if err != nil {
		t.Fatalf("judgeDraft 报错：%v", err)
	}
	if res.Total != 65 || res.Pass {
		t.Fatalf("低分应判不通过，实际 total=%d pass=%v", res.Total, res.Pass)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("应恰好调用一次裁判，实际 %d 次", len(fake.calls))
	}
	if !fake.calls[0].JSONSet || !fake.calls[0].JSON {
		t.Fatal("裁判调用必须显式开 JSON 模式（裸奔会在中文理由上随机炸 json）")
	}
	// 不变量：不通过 ⇒ findings 非空（回炉要有输入）。
	if len(res.Findings) == 0 {
		t.Fatal("判不通过却没有扣分项：回炉没有输入，等于白判")
	}
	if res.Category != cat.Name {
		t.Fatalf("结果应记住本轮分类，实际 %q", res.Category)
	}
}

func TestJudgeDraftPassesOnFullMarks(t *testing.T) {
	mp := synthPack()
	cat := mp.Structure.Categories[1]
	fake := &fakeChat{reply: func(fakeCall) (string, error) {
		return judgeJSON(map[string]int{"category_routing": 25, "requirement_compliance": 25, "example_alignment": 20, "structure_completeness": 15, "no_hallucination": 15}), nil
	}}
	g := &Generator{}
	g.SetChatClient(fake)

	res, err := g.judgeDraft(context.Background(), mp, cat, "素材。", "草稿。")
	if err != nil {
		t.Fatalf("judgeDraft 报错：%v", err)
	}
	if !res.Pass || res.Total != 100 {
		t.Fatalf("满分应通过，实际 total=%d pass=%v", res.Total, res.Pass)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("健康手册 + 满分草稿不应有扣分项，实际：%v", res.Findings)
	}
}

// TestJudgeDraftFailsOnBrokenManualEvenWithFullMarks 是 s7 那条产品硬要求的
// 直接防线：**残缺手册即使模型给满分也必须不通过**。
// 模型会「给面子分」，所以硬校验是否决项，不是参考项。
func TestJudgeDraftFailsOnBrokenManualEvenWithFullMarks(t *testing.T) {
	mp := synthPack()
	delete(mp.Examples, "会议纪要")
	cat := mp.Structure.Categories[0]
	fake := &fakeChat{reply: func(fakeCall) (string, error) {
		return judgeJSON(map[string]int{"category_routing": 25, "requirement_compliance": 25, "example_alignment": 20, "structure_completeness": 15, "no_hallucination": 15}), nil
	}}
	g := &Generator{}
	g.SetChatClient(fake)

	res, err := g.judgeDraft(context.Background(), mp, cat, "素材。", "草稿。")
	if err != nil {
		t.Fatalf("judgeDraft 报错：%v", err)
	}
	if len(res.Hard) == 0 {
		t.Fatalf("残缺手册应产生硬校验命中，实际 Hard 为空（模型分会把关卡放过去）")
	}
	if !hasFinding(res.Findings, "会议纪要", "为空") {
		t.Fatalf("扣分项应点名缺失范文的分类，实际：%v", res.Findings)
	}
	// 这里刻意断言「模型分仍是 100 但最终结论是不通过」：
	// 两个字段不一致正是这条防线的证据——不是模型说不行，是硬校验拦下的。
	if !res.ModelPass {
		t.Fatalf("本用例模型应给满分（ModelPass=true），实际 false：故障没打在模型分上，测试失效")
	}
	if res.Pass {
		t.Fatalf("有硬校验命中时最终结论必须是不通过，实际通过：%+v", res)
	}
}

// TestJudgeDraftIsIndependentFromSkillAndReviewer 断言裁判的输入边界：
// 只看手册原文，看不到技能自述、也看不到审稿清单。
// 若把审稿清单喂给裁判，技能就能「自己给自己判卷」。
func TestJudgeDraftIsIndependentFromSkillAndReviewer(t *testing.T) {
	mp := synthPack()
	cat := mp.Structure.Categories[0]
	fake := &fakeChat{reply: func(fakeCall) (string, error) {
		return judgeJSON(map[string]int{"category_routing": 25, "requirement_compliance": 25, "example_alignment": 20, "structure_completeness": 15, "no_hallucination": 15}), nil
	}}
	g := &Generator{}
	g.SetChatClient(fake)

	if _, err := g.judgeDraft(context.Background(), mp, cat, "素材：库存周转天数压降。", "草稿正文。"); err != nil {
		t.Fatalf("judgeDraft 报错：%v", err)
	}
	u := fake.calls[0].User
	s := fake.calls[0].Sys
	// 正向：标尺必须在场（否则「独立性」变成「什么都没给」也照样绿）。
	if !strings.Contains(u, synReqMinute) || !strings.Contains(u, synMarkMinuteStart) {
		t.Fatalf("裁判输入里必须包含该类要求与范文原文，实际：%s", snippet(u, 300))
	}
	// 反向：审稿清单不得出现。
	if strings.Contains(u, synReviewerMark) || strings.Contains(s, synReviewerMark) {
		t.Fatalf("裁判输入里出现了审稿清单内容：裁判被技能自己判卷了")
	}
}

func TestJudgeDraftErrorsWhenClientMissing(t *testing.T) {
	g := &Generator{}
	if _, err := g.judgeDraft(context.Background(), synthPack(), Category{Name: "x"}, "m", "d"); err == nil {
		t.Fatal("未配置模型客户端时应报错，而不是返回空结果")
	}
	if _, err := g.trialDraft(context.Background(), "sys", synthPack(), Category{Name: "x"}, "m"); err == nil {
		t.Fatal("trialDraft 在未配置模型客户端时应报错")
	}
}

// TestSetLLMNormalizesTypedNil 守一个很容易踩的接口坑：把 nil 的 *llm.Client
// 赋给接口字段会得到「非 nil 的接口」，于是所有 nil 守卫失效、后续 panic。
func TestSetLLMNormalizesTypedNil(t *testing.T) {
	g := NewGenerator(nil, nil, t.TempDir())
	if g.llm != nil {
		t.Fatalf("NewGenerator(nil) 后 llm 应为真 nil，实际 %#v", g.llm)
	}
	if _, err := g.trialDraft(context.Background(), "sys", synthPack(), Category{Name: "x"}, "m"); err == nil {
		t.Fatal("未配置模型时应安全报错")
	}
}

// ---- 试用 ----

func TestTrialDraftUsesSkillPromptAndCategoryMaterial(t *testing.T) {
	cat := Category{Name: "会议纪要", Trigger: "需要记录议定事项时", Requirement: synReqMinute}
	fake := &fakeChat{reply: func(fakeCall) (string, error) { return "  草稿正文  ", nil }}
	g := &Generator{}
	g.SetChatClient(fake)

	skillPrompt := "技能自述标记：你是公文写作助手。"
	got, err := g.trialDraft(context.Background(), skillPrompt, synthPack(), cat, "素材标记：库存周转天数压降。")
	if err != nil {
		t.Fatalf("trialDraft 报错：%v", err)
	}
	if got != "草稿正文" {
		t.Fatalf("试用草稿应去掉首尾空白，实际 %q", got)
	}
	// 正向：技能自述在前（试用走技能自己的 system_prompt 才对线上有预测力）。
	if !strings.HasPrefix(fake.calls[0].Sys, skillPrompt) {
		t.Fatalf("试用必须走技能自己的 system_prompt（才对线上有预测力），实际：%s", snippet(fake.calls[0].Sys, 80))
	}
	// 正向：运行时会注入的本类要求与范文必须在场——否则量的是「试用少喂料」
	// 而不是「技能不达标」（见 trialPackBlock 注释里的坑）。
	if !strings.Contains(fake.calls[0].Sys, synReqMinute) {
		t.Fatalf("试用必须带上本类写作要求（运行时会注入同款），实际：%s", snippet(fake.calls[0].Sys, 300))
	}
	if !strings.Contains(fake.calls[0].Sys, synMarkMinuteStart) {
		t.Fatalf("试用必须带上本类真实范文，实际：%s", snippet(fake.calls[0].Sys, 300))
	}
	if !strings.Contains(fake.calls[0].User, "会议纪要") || !strings.Contains(fake.calls[0].User, "素材标记") {
		t.Fatalf("试用输入应带类名与素材，实际：%s", snippet(fake.calls[0].User, 200))
	}
}

// TestTrialPackBlockDegradesCleanly 守边角：没手册、或该类没切出范文时，
// 注入块不能凭空长出「范文」段（有段无文比没段更坏——模型会照着空段编）。
func TestTrialPackBlockDegradesCleanly(t *testing.T) {
	cat := Category{Name: "会议纪要", Trigger: "需要记录议定事项时", Requirement: synReqMinute}

	// 无手册：不注入任何东西，sys 原样（不能拼出半个空壳块）。
	if blk := trialPackBlock(nil, cat); blk != "" {
		t.Fatalf("没有手册时不应注入，实际：%s", snippet(blk, 120))
	}
	if blk := trialPackBlock(&manualPack{}, cat); blk != "" {
		t.Fatalf("手册结构为空时不应注入，实际：%s", snippet(blk, 120))
	}

	// 有权要求但该类没范文：仍注入要求，但绝不出现范文段落标题。
	noEx := &manualPack{Structure: &Structure{}}
	blk := trialPackBlock(noEx, cat)
	if !strings.Contains(blk, synReqMinute) {
		t.Fatalf("该类有写作要求时必须注入，实际：%s", snippet(blk, 200))
	}
	if strings.Contains(blk, "本类真实范文") {
		t.Fatalf("该类没有范文时不得出现范文段落：%s", snippet(blk, 200))
	}
}

// ---- 取样确定性 ----

func TestPickTrialCategoryIsDeterministic(t *testing.T) {
	cats := make([]Category, 12)
	for i := range cats {
		cats[i] = Category{Name: "类" + itoa(i+1)}
	}
	st := &Structure{Categories: cats}

	// 同一轮取两次必须一样（确定性，fidelity.md 的分数才可复核）。
	for round := 1; round <= judgeMaxRounds; round++ {
		a, ok1 := pickTrialCategory(st, round)
		b, ok2 := pickTrialCategory(st, round)
		if !ok1 || !ok2 || a.Name != b.Name {
			t.Fatalf("第 %d 轮取样不确定：%s vs %s", round, a.Name, b.Name)
		}
	}
	// 12 类 > 上限 3：按轮次均匀铺开，首轮在头部、末轮在尾部。
	first, _ := pickTrialCategory(st, 1)
	last, _ := pickTrialCategory(st, judgeMaxRounds)
	if first.Name != "类1" {
		t.Fatalf("第 1 轮应取首个分类，实际 %s", first.Name)
	}
	if last.Name == first.Name {
		t.Fatalf("末轮与首轮撞在同一分类（%s），末尾分类永远没被验证过", last.Name)
	}
	if last.Name != "类9" {
		t.Fatalf("12 类按 3 轮均匀铺开时末轮应落在「类9」，实际 %s", last.Name)
	}
	// 越界轮次也要能用（上层止损计数万一多走一轮，不能崩）。
	if _, ok := pickTrialCategory(st, 99); !ok {
		t.Fatal("超大轮次应仍返回一个分类")
	}
	// 分类数 ≤ 上限时逐轮走遍：每类至少被试一次。
	small := &Structure{Categories: cats[:3]}
	names := map[string]bool{}
	for round := 1; round <= 3; round++ {
		c, ok := pickTrialCategory(small, round)
		if !ok {
			t.Fatalf("第 %d 轮取样失败", round)
		}
		if names[c.Name] {
			t.Fatalf("3 类跑 3 轮出现了重复分类 %s", c.Name)
		}
		names[c.Name] = true
	}
	if _, ok := pickTrialCategory(&Structure{}, 1); ok {
		t.Fatal("空结构应返回 ok=false")
	}
}

// ---- 轮次表格 ----

// TestJudgeRoundTable 断言 fidelity.md 里那张表的内容，而不是靠人肉看落盘文件。
func TestJudgeRoundTable(t *testing.T) {
	// 第 1 轮：结构完整 6/15 —— 低于该维下限 9，属真塌方，总分 76 也不够，
	// 两种否决理由同时成立。
	r1, _ := parseJudgeOutput(judgeJSON(map[string]int{"category_routing": 20, "requirement_compliance": 20, "example_alignment": 15, "structure_completeness": 6, "no_hallucination": 15}))
	r1.Category = "会议纪要"
	// 第 2 轮：无幻觉 12/15 —— 丢了 3 分但没塌方（下限 9），总分 82 够线，应通过。
	// 这一轮专门盯「别把正常扣分判死」：换成绝对下限 15 它就会被误判不通过。
	r2, _ := parseJudgeOutput(judgeJSON(map[string]int{"category_routing": 20, "requirement_compliance": 20, "example_alignment": 15, "structure_completeness": 15, "no_hallucination": 12}))
	r2.Category = "工作总结"
	// 第 3 轮：满分。
	r3, _ := parseJudgeOutput(judgeJSON(map[string]int{"category_routing": 25, "requirement_compliance": 25, "example_alignment": 20, "structure_completeness": 15, "no_hallucination": 15}))
	r3.Category = "新闻通稿"

	out := judgeRoundTable([]JudgeRound{
		{Round: 1, Category: "会议纪要", Result: r1},
		{Round: 2, Category: "工作总结", Result: r2},
		{Round: 3, Category: "新闻通稿", Result: r3},
	})
	for _, kw := range []string{"第 1 轮", "会议纪要", "76/100", "第 2 轮", "工作总结", "82/100", "第 3 轮", "新闻通稿", "100/100"} {
		if !strings.Contains(out, kw) {
			t.Fatalf("轮次表应包含 %q，实际：\n%s", kw, out)
		}
	}
	// 「不通过」必须挂在第 1 轮那一行上，而不是表里随便某处出现——
	// 否则「有轮次被判死」和「就是这一轮被判死」就不是同一件事。
	var line1, line2 string
	for _, ln := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(ln, "第 1 轮"):
			line1 = ln
		case strings.Contains(ln, "第 2 轮"):
			line2 = ln
		}
	}
	if !strings.Contains(line1, "不通过") {
		t.Fatalf("第 1 轮（结构完整 6/15 塌方、总分 76）应记为不通过，实际行：%s", line1)
	}
	if !strings.Contains(line2, "通过") || strings.Contains(line2, "不通过") {
		t.Fatalf("第 2 轮（无幻觉 12/15 未塌方、总分 82）应记为通过，实际行：%s", line2)
	}
	// 单元格里不能出现竖线，否则表格会被撕成错误的列。
	if strings.Count(strings.Split(out, "\n")[2], "|") != 6 {
		t.Fatalf("表格行应有 6 个竖线分隔，实际：%s", strings.Split(out, "\n")[2])
	}
	// 空轮次不能渲染成一张空表壳（看起来像「判过了但没记录」）。
	if got := judgeRoundTable(nil); strings.Contains(got, "|") {
		t.Fatalf("无轮次时应给一句说明而不是空表，实际：%s", got)
	}
}
