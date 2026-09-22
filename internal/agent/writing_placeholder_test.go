package agent

import (
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/model"
	"github.com/lizhemin15/skillforge/internal/store"
)

// 手册范文里的 {公司名称}/{日期} 这类占位符，必须被提示词显式禁止原样输出。
// 真实线上技能（公司新闻通稿）的范文就带这种占位符，实测模型会连占位符一起照抄，
// 即便本轮用户已经给了「发布单位星河科技、发布日期 2026 年 9 月 14 日」——
// 用户看到的观感就是「它没管我说的话」。
// generateSys 是把技能骨架模板喂给模型的那一处。上面那个 packBlock 用例只盖住了
// 「写作包/范文」这条路径；2026-09-22 线上实测证明**骨架模板这条路径会漏**：
// vague 腿我方只说「帮我写个新闻稿。」，成稿正文 36 个 {公司名称}/{关键数据} 占位符。
// 这条守卫钉的就是 generateSys 里必须有「模板 token 不许原样进正文」的硬规则，
// 以及规则必须说清合法退路（中文占位 【待补:字段名】/ 先问用户），否则模型只会照抄 token。
func TestGenerateSysBansTemplateTokens(t *testing.T) {
	sc := &SkillContent{
		Slug:         "公司新闻通稿",
		Name:         "公司新闻通稿",
		SystemPrompt: "你是企业新闻通稿撰写专家。",
		SkillType:    model.SkillTypeWrite,
		Template:     "**{公司名称}完成{关键数据}**{日期}，{公司名称}宣布{核心事件}。",
	}
	got := (&Engine{}).generateSys(sc)

	// 前提：模板真的被注入了 —— 否则下面的断言是在空气上通过
	if !strings.Contains(got, "{公司名称}完成{关键数据}") {
		t.Fatalf("generateSys 没注入骨架模板，用例自身不成立：%q", got)
	}
	for _, want := range []string{
		"占位符标记，不是稿子",     // 把 token 定性清楚，模型才知道不能照抄
		"视为交付失败",         // 后果要写死，软约束在实测里会被忽略
		"【待补:字段名】",       // 合法退路一：单个字段缺失
		"不要交稿",           // 合法退路二：主体都定不下来就先问
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("generateSys 的正文书写规则缺了 %q —— 骨架模板的 {xxx} 会再次漏进成稿", want)
		}
	}
	// 负向前提：没有骨架模板的写作技能，不该被塞这套规则（避免污染无关 prompt）
	if s := (&Engine{}).generateSys(&SkillContent{Name: "x", SystemPrompt: "y", SkillType: model.SkillTypeWrite}); strings.Contains(s, "占位符标记，不是稿子") {
		t.Fatalf("没有模板时也塞了占位符规则：%q", s)
	}
}

func TestPackBlockBansPlaceholders(t *testing.T) {
	pack := &WritePack{Categories: []WriteCategory{{
		CategoryDoc: store.CategoryDoc{
			File:        "categories/01-新闻通稿.md",
			Name:        "企业新闻通稿",
			Trigger:     "需要对外发布产品/技术消息时",
			Requirement: "正文分三段：发布背景、技术亮点、行业影响。",
		},
		Examples: []store.CategoryExample{{
			Path:    "examples/新闻通稿/01.md",
			Content: "{公司名称}于{日期}正式发布{产品名}。",
		}},
	}}}
	cat := pack.FindCategory("企业新闻通稿")
	if cat == nil {
		t.Fatal("FindCategory 没命中，用例自身不成立")
	}
	got := packBlock(pack, cat)

	for _, want := range []string{"企业新闻通稿", "发布背景", "{公司名称}于{日期}正式发布"} {
		if !strings.Contains(got, want) {
			t.Fatalf("packBlock 少了 %q（说明用例没读到真实注入块）", want)
		}
	}
	if !strings.Contains(got, "绝不允许把占位符原样写进正文") {
		t.Fatalf("packBlock 没禁止原样输出占位符 —— 这正是 {公司名称}/{日期} 漏到成稿的原因")
	}
	if !strings.Contains(got, "事实只能来自用户提供的素材") {
		t.Fatalf("原有的「范文事实不得搬用」约束被挤掉了")
	}
}
