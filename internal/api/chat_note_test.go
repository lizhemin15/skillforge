package api

import (
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/agent"
)

// TestAssemblyNoteNeverEmpty 是这把尺子的**存在理由**：这句材料是「分类回来到首个
// 正文字」之间屏幕上唯一的可见物（线上实测那段空白 54.7 秒）。它一旦返回空串
// （或只剩后缀），Clock.Thinking 会因为 TrimSpace 为空直接丢弃，那几十秒又变成
// 纯计时器——也就是用户抱怨的那件事原样回来。所以先钉死「任何输入下都非空」。
func TestAssemblyNoteNeverEmpty(t *testing.T) {
	cases := []struct {
		name  string
		facts assemblyFacts
	}{
		{"全空（未命中技能、无要素、无上文）", assemblyFacts{}},
		{"只有技能名", assemblyFacts{Skill: "办公文档管家"}},
		{"只有上文", assemblyFacts{HistMsgs: 3, HistChars: 1200}},
		{"要素全空值", assemblyFacts{Args: map[string]string{"主题": "", "篇幅": "  "}}},
		{"手册模式", assemblyFacts{Skill: "办公文档管家", Category: "新闻通稿", Examples: 2, HasReview: true}},
	}
	for _, c := range cases {
		got := c.facts.note()
		if strings.TrimSpace(got) == "" {
			t.Fatalf("%s：材料为空串 —— 那几十秒会退回纯计时器", c.name)
		}
		// 前缀与后缀都是「给用户看的语义」的一部分：没有「已装配」用户不知道这行是什么，
		// 没有「正在起草」用户不知道已经开始写了。
		if !strings.HasPrefix(got, "已装配：") || !strings.HasSuffix(got, "正在起草…") {
			t.Fatalf("%s：材料缺头或尾：%q", c.name, got)
		}
	}
}

// TestAssemblyNoteCarriesFacts 钉住「报的是真事实，不是一句套话」：技能名、类别、
// 范文篇数、上文条数字数、要素项数都得出现——用户就是靠这些判断「它到底看没看我给的东西」。
func TestAssemblyNoteCarriesFacts(t *testing.T) {
	f := assemblyFacts{
		Skill: "办公文档管家", Category: "新闻通稿", Examples: 2, HasReview: true,
		Args:     map[string]string{"主题": "数据中台3.0", "篇幅": "400字", "空值": ""},
		HistMsgs: 12, HistChars: 5317,
	}
	got := f.note()
	for _, want := range []string{
		"办公文档管家", "新闻通稿", "范文 2 篇", "审稿清单 有",
		"要素 2 项", "篇幅", "主题", "会话上文 12 条 5317 字",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("材料里没有 %q：%q", want, got)
		}
	}
	// 空值要素是噪音：列出来会让用户以为它拿到了这个信息。
	if strings.Contains(got, "空值") {
		t.Fatalf("空值要素不该出现在材料里：%q", got)
	}
	// materialCap=160：材料留尾 160 字，超了会把「上文多少字」这类关键事实挤掉。
	if n := len([]rune(got)); n > materialCap {
		t.Fatalf("材料 %d 字，超过 materialCap=%d，尾部事实会被截掉：%q", n, materialCap, got)
	}
}

// TestArgsBriefTruncates 钉住要素过多时**仍然报出真实项数**：只列前 6 个名字，
// 但「要素 N 项」必须是真实总数，否则用户看到一个比实际小的数字。
func TestArgsBriefTruncates(t *testing.T) {
	args := map[string]string{}
	names := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	for _, n := range names {
		args[n] = "v"
	}
	got := argsBrief(args)
	if !strings.Contains(got, "要素 8 项") {
		t.Fatalf("项数应是真实总数 8：%q", got)
	}
	if !strings.Contains(got, "等 8 项") {
		t.Fatalf("截断时要如实说明还有多少项：%q", got)
	}
	if strings.Contains(got, "g、") || strings.Contains(got, "h、") {
		t.Fatalf("只该列前 %d 个名字：%q", maxNoteArgs, got)
	}
}

// TestNoteFactsCountsHistory 钉住上文统计口径：条数取窗口长度，字数按 rune 数
// （按字节数会让中文虚高到三倍，用户会以为模型吃了三倍上下文）。
func TestNoteFactsCountsHistory(t *testing.T) {
	hist := []agent.Message{{Content: "你好"}, {Content: "一二三"}}
	f := noteFacts(&agent.SkillContent{Name: "写手", Slug: "writer"}, map[string]string{"k": "v"}, hist)
	if f.HistMsgs != 2 || f.HistChars != 5 {
		t.Fatalf("上文统计错了：条数=%d（要 2）字数=%d（要 5）", f.HistMsgs, f.HistChars)
	}
	if f.Skill != "写手" {
		t.Fatalf("技能名应取 Name：%q", f.Skill)
	}
	// Name 为空时必须退回 Slug，否则材料会写成「技能《》」——空书名号看着像 bug。
	empty := noteFacts(&agent.SkillContent{Slug: "writer"}, nil, nil)
	if !strings.Contains(empty.note(), "writer") {
		t.Fatalf("Name 为空应退回 Slug：%q", empty.note())
	}
	if strings.Contains(empty.note(), "《》") {
		t.Fatalf("不该出现空书名号：%q", empty.note())
	}
}
