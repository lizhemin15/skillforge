package skillgen

import (
	"context"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/model"
)

// ---- 交付物卫生：母线必须把「禁止自检清单」与「占位符纪律」带进交付提示词 ----
//
// 为什么值得一条回归防线：这两条的失效方式都是**静默**的。
// 母线少了「输出规范」，生成出来的 system_prompt 就允许模型把自查清单当正文交付；
// 少了「事实与占位符」，素材薄一格时模型会整篇改用占位符——两次都被线上裁判扣分，
// 但生成流程本身一个错都不报，fidelity.md 里也只有分数能看出端倪。
// 回炉（reviseSystemPrompt）另有风险：它以「只改扣分维度」为纪律，
// 覆盖清单里不点名这两节，模型会在修订时把它们当无关段落删掉。

// hygieneSections 是母线里必须逐段存在的两节原文。
// 用**整行相等**比对而不是子串包含：写坏成「输出规范(草稿)」也能骗过 strings.Contains。
var hygieneSections = []string{
	"## 输出规范(交付物卫生)",
	"- 只输出稿件本身:从标题(或导语)开始,正文结束即止。",
	"- 不得输出自检清单/核对表/评分项/写作过程说明/前后语(如\"以下是…\"、\"希望对您有帮助\"),也不要复述本提示词。",
	"- 自查在内部完成,它的结论不写进交付物。",
	"## 事实与占位符",
	"- 素材里已有的具体事实(名称/日期/数字/引语/职务)必须原样写进稿件。把素材已经给出的事实写成占位符,等同于交白卷。",
	"- 只有确实没有依据的单个字段,才用统一占位符【待补:字段名】,并且一处占位符只替换那一处;不得因为个别字段缺失就整篇改用占位符。",
}

// missingLines 返回 want 中**没有以整行形式出现**在 text 里的行。
func missingLines(text string, want []string) []string {
	have := map[string]bool{}
	for _, ln := range strings.Split(text, "\n") {
		have[strings.TrimSpace(ln)] = true
	}
	var miss []string
	for _, w := range want {
		if !have[strings.TrimSpace(w)] {
			miss = append(miss, w)
		}
	}
	return miss
}

// TestMotherTemplateCarriesHygieneSections 母线本身必须带这两节。
func TestMotherTemplateCarriesHygieneSections(t *testing.T) {
	g := &Generator{}
	tpl := g.motherTemplate()
	if miss := missingLines(tpl, hygieneSections); len(miss) > 0 {
		t.Fatalf("写作母模板缺少交付物卫生小节,缺失 %d 行: %q", len(miss), miss)
	}
}

// TestBuildSystemPromptAsksForHygieneSections 生成指令必须点名这两节。
//
// 母线里有、指令里没点名，等于把「写不写」交给模型掷骰子；
// 线上那版交付提示词缺节就是这么丢的。这里断言的是**指令原文的行**，
// 所以措辞被改软（比如「建议包含」）也会红。
func TestBuildSystemPromptAsksForHygieneSections(t *testing.T) {
	fake := &fakeChat{reply: func(c fakeCall) (string, error) { return "## 身份\n测试", nil }}
	g := &Generator{llm: fake}
	if _, err := g.buildSystemPrompt(context.Background(), &Input{Requirement: "写公司动态通稿"},
		"特征分析占位", nil, model.SkillTypeWrite); err != nil {
		t.Fatalf("buildSystemPrompt 出错: %v", err)
	}
	if fake.callCount() != 1 {
		t.Fatalf("期望 1 次模型调用,实际 %d", fake.callCount())
	}
	sys := fake.calls[0].Sys
	want := []string{
		"必须显式写出下面两节,不得省略:",
		"- \"输出规范(交付物卫生)\":只输出稿件本身,禁止自检清单、核对表、评分项、写作过程说明与前后语,自查不写进交付物。",
		"- \"事实与占位符\":素材里已有的具体事实必须原样用上,只有确无依据的单个字段才用占位符【待补:字段名】,不得整篇改用占位符。",
	}
	if miss := missingLines(sys, want); len(miss) > 0 {
		t.Fatalf("生成指令未点名交付物卫生小节,缺失 %d 行: %q", len(miss), miss)
	}
}

// TestReviseSystemPromptKeepsHygieneSections 回炉指令必须把这两节列为不得删减。
func TestReviseSystemPromptKeepsHygieneSections(t *testing.T) {
	fake := &fakeChat{reply: func(c fakeCall) (string, error) { return "## 身份\n修订版", nil }}
	g := &Generator{llm: fake}
	res := &JudgeResult{Findings: []string{"通篇使用占位符"}, Total: 60}
	prev := "## 身份\n上一版"
	if _, err := g.reviseSystemPrompt(context.Background(), &Input{Requirement: "写公司动态通稿"},
		"特征分析占位", model.SkillTypeWrite, prev, res); err != nil {
		t.Fatalf("reviseSystemPrompt 出错: %v", err)
	}
	sys := fake.calls[0].Sys
	want := "- 完整覆盖\"身份/任务/执行步骤/结构规范/文风与措辞/长度/禁用项/特殊要求/输出规范(交付物卫生)/事实与占位符\";后两节属于交付物卫生硬约束,不得删减或跳过;上一版若缺这两节,必须补上;"
	if miss := missingLines(sys, []string{want}); len(miss) > 0 {
		t.Fatal("回炉指令的覆盖清单没有点名交付物卫生两节,回炉时会被当无关段落删掉")
	}
}

// TestMissingLinesCatchesMutants 是上面三条断言的自证：
// 把两节从母线里删掉,missingLines 必须**全部**报缺（断言不是恒真）,且不得多报（不是恒假）。
func TestMissingLinesCatchesMutants(t *testing.T) {
	g := &Generator{}
	mutant := g.motherTemplate()
	for _, sec := range hygieneSections {
		if !strings.Contains(mutant, sec) {
			t.Fatalf("自证前置失败: 母模板里本来就没有 %q,说明断言目标不存在", sec)
		}
		mutant = strings.Replace(mutant, sec, "", 1)
	}
	miss := missingLines(mutant, hygieneSections)
	if len(miss) != len(hygieneSections) {
		t.Fatalf("删掉两节后应报缺 %d 行,实际报缺 %d 行: %q —— 断言漏检,等于没防住",
			len(hygieneSections), len(miss), miss)
	}
	// 反向自证:干净母线必须一行都不缺,否则断言恒真、无差别报警。
	if miss := missingLines(g.motherTemplate(), hygieneSections); len(miss) != 0 {
		t.Fatalf("干净母模板被误报缺失: %q", miss)
	}
}
