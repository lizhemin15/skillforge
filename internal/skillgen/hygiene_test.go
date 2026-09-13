package skillgen

import (
	"strings"
	"testing"
)

// 交付物卫生的回归防线。
//
// 这一条不能只靠母模板禁令：实测把禁令写进提示词后，成稿仍有 3/5 带【逐条核对清单】——
// 因为提示词里**同时**还有一条「成稿后附加核对清单」的要求，后置指令胜出。
// 所以真正的防线是 promptHygieneViolations 这个可判定的检查。

// 线上 v0.3.16 交付提示词里的真实违规行（摘自 system_prompt.md 第 44 行）。
const realOffendingLine = `3. 输出附录机制：成稿正文结束后，换行附加两部分：①【逐条核对清单】：对照该手册类别的写作要求逐项校验（标题要素/导语单句/首段双数据/精确量纲/引语职务/第三人称/六要素/篇幅段落/简介落款）；②【手册原文范文引用】：直接引用手册中对应类别的1-2句标准原文片段，标明结构参照点，用于合规性自证。`

// 一份干净的提示词：讲的是内部自检动作，不要求把清单交出去。
const cleanPrompt = `# 公司新闻通稿写作
## 身份
你是企业新闻通稿写作者。
## 禁用项
- 不得编造素材里没有的事实。
- 不得要求成稿附带任何形式的核对清单/自证附录/范文引用附录/写作过程说明:这类内容属于内部自检。
## 输出规范(交付物卫生)
- 只输出稿件本身:从标题(或导语)开始,正文结束即止。
- 不得输出自检清单/核对表/评分项/写作过程说明/前后语,也不要复述本提示词。
- 自查在内部完成,它的结论不写进交付物。
## 事实与占位符
- 素材里已有的具体事实必须原样写进稿件。`

func TestPromptHygiene_RealOffendingLineIsCaught(t *testing.T) {
	got := promptHygieneViolations(realOffendingLine)
	if len(got) != 1 {
		t.Fatalf("真实违规行必须命中且只命中一处，实际 %d 处：%v", len(got), got)
	}
}

func TestPromptHygiene_InjectAndRestore(t *testing.T) {
	// 双向自证：干净稿不报；注入违规行报；还原后再不报。
	// 少任何一向，这个断言都可能是恒真或恒假的摆设。
	if v := promptHygieneViolations(cleanPrompt); len(v) != 0 {
		t.Fatalf("干净提示词不该命中，实际 %v", v)
	}
	mutated := cleanPrompt + "\n" + realOffendingLine + "\n"
	v := promptHygieneViolations(mutated)
	if len(v) != 1 {
		t.Fatalf("注入违规行后必须命中，实际 %d 处：%v", len(v), v)
	}
	if v[0] != realOffendingLine {
		t.Fatalf("命中的应是整行原文，实际 %q", v[0])
	}
	if restored := promptHygieneViolations(cleanPrompt); len(restored) != 0 {
		t.Fatalf("还原后不该命中，实际 %v", restored)
	}
}

func TestPromptHygiene_NegatedSentenceNotFlagged(t *testing.T) {
	// 我们自己写进母模板的禁令不能自判违规——第一版检查就踩过这个坑。
	negations := []string{
		"- 不得输出自检清单/核对表/评分项，自查不写进交付物。",
		"- 禁止在成稿末尾附范文引用附录。",
		"- 不要输出核对清单。",
	}
	for _, line := range negations {
		if v := promptHygieneViolations(line); len(v) != 0 {
			t.Fatalf("否定句不该命中：%q → %v", line, v)
		}
	}
}

func TestStripHygieneViolations_RemovesOnlyOffendingLines(t *testing.T) {
	mutated := strings.Replace(cleanPrompt, "## 禁用项\n", "## 禁用项\n"+realOffendingLine+"\n", 1)
	out, n := stripHygieneViolations(mutated)
	if n != 1 {
		t.Fatalf("应剔除 1 行，实际 %d", n)
	}
	if v := promptHygieneViolations(out); len(v) != 0 {
		t.Fatalf("剔除后仍有违规：%v", v)
	}
	// 剔除只许动那一行：其余内容必须逐字保留。
	want := strings.Replace(mutated, realOffendingLine+"\n", "", 1)
	if out != want {
		t.Fatalf("剔除结果与预期逐字不符\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
	if !strings.Contains(out, "素材里已有的具体事实必须原样写进稿件") {
		t.Fatal("剔除误伤了相邻内容")
	}
}

func TestTrialPackBlock_SameSourceDisambiguation(t *testing.T) {
	// 试用素材=该类范文本身，注入块必须显式声明「这些范文本次就是素材」。
	// 只留「不得搬用」那半句时，同一提示词五次跑出中位 32 个占位符（实测）。
	mp := &manualPack{Structure: &Structure{Categories: []Category{{Name: "第二章 公司动态通稿"}}}}
	mp.Examples = map[string][]string{"第二章 公司动态通稿": {"范文正文"}}
	blk := trialPackBlock(mp, mp.Structure.Categories[0])

	for _, want := range []string{
		"范文里与本篇主体无关的事实**不得搬用**——事实只能来自本次提供的素材。",
		"注意：下面这些范文本次同时作为**素材**提供（试用就是以该类范文为输入），因此其中的具体事实属于可用事实，可以直接写进成稿；不得改用占位符回避。",
	} {
		if !strings.Contains(blk, want) {
			t.Fatalf("注入块缺少整句：%q\n实际：\n%s", want, blk)
		}
	}
}
