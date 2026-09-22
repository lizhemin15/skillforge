package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/store"
)

// ===== 「本轮用户原话必须进执笔跳」的守卫 =====
//
// 这条守卫对应线上真实投诉：「我上传了素材、也把要素都写在话里了，生成的 skill 跟
// 我给的内容完全没关系」。根因是**原话从来没进过执笔跳的 prompt**：
//
//	chat.go  L147  history := h.eng.Session(id)      ← 在 Push 之前取，天然不含本轮原话
//	chat.go  L148  h.eng.Push(id, {role:"user", …})
//	GenerateWithPack / GenerateWithPlan 的签名里也从来没有 userMsg 这一项
//
// 于是执笔跳能看到的用户内容只剩 EvalTurn 抽出来的那几个参数（一次有损的模型调用），
// 抽丢的部分没有任何兜底 —— 实测成稿会把范文里的 {公司名称}/{日期} 原样吐出来。
//
// 为什么断言钉在「真发出去的请求体」上而不是钉函数返回值：返回值是模型吐的，测它等于
// 测模型；而「原话有没有进 prompt」是确定性的接线问题，只有请求体能一眼看出来 ——
// 与 TestWriteHopKnobActuallyReachesProvider 同一个路数。

// rawNewsMsg 模拟用户一条把事实说全的短消息。锚串（公司名/编号/数字）必须逐字活着到
// 请求体里，任何一个字对不上都算接线断了。
const rawNewsMsg = "写一篇公司新闻通稿：星禾云桥科技于 2026 年 9 月 18 日发布数据中台 3.0，" +
	"已覆盖 128 家客户、36 座城市，调度效率提升 91.5%，内部编号 KH-7Q2Z。"

// userAskOf 取出这一发请求里 role=user 的正文（fakeProvider 记下的原始请求体）。
func userAskOf(t *testing.T, body map[string]any) string {
	t.Helper()
	msgs, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("请求体里没有 messages：%v", body["messages"])
	}
	var last string
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if role, _ := mm["role"].(string); role == "user" {
			c, _ := mm["content"].(string)
			last = c
		}
	}
	if last == "" {
		t.Fatal("请求体里没有 role=user 的消息 —— 这条守卫自己就不成立")
	}
	return last
}

// assertRawWired 是三条链路共用的一套判据：原话逐字在、块头在、占位符禁令在、
// 且原话块排在抽参块**前面**（越靠前越不容易被后面的技能说明冲淡）。
func assertRawWired(t *testing.T, ask, who string) {
	t.Helper()
	if !strings.Contains(ask, rawNewsMsg) {
		t.Fatalf("%s：本轮用户原话没有逐字进 prompt —— 这正是「生成的 skill 跟我给的内容没关系」的根因。\n实际 prompt:\n%s",
			who, ask)
	}
	if !strings.Contains(ask, "# 本轮用户原话") {
		t.Fatalf("%s：原话进了 prompt 但没有块头（模型分不清哪段是一手材料）", who)
	}
	if !strings.Contains(ask, "把 {xxx} 原样写进正文视为不合格") {
		t.Fatalf("%s：没有禁止占位符原样输出 —— 范文里的 {公司名称}/{日期} 就是这么漏进成稿的", who)
	}
	iRaw := strings.Index(ask, "# 本轮用户原话")
	iArg := strings.Index(ask, "# 本次写作的具体要求")
	if iArg >= 0 && iRaw > iArg {
		t.Fatalf("%s：原话块排在了抽参块后面（raw=%d arg=%d）—— 一手材料必须在前", who, iRaw, iArg)
	}
}

// TestWriteHopCarriesUserWordsVerbatim 守技能链路（构思 + 执笔）：两跳都必须见到原话。
func TestWriteHopCarriesUserWordsVerbatim(t *testing.T) {
	fp := &fakeProvider{content: "正文"}
	eng := newTestEngine(t, fp)
	sc := agentSkillContent("公司新闻通稿")

	if _, err := eng.GenerateWithPlan(context.Background(), sc, map[string]string{"company_name": "星禾云桥科技"},
		"· 首段写五要素", rawNewsMsg, func(string) {}); err != nil {
		t.Fatalf("GenerateWithPlan: %v", err)
	}
	assertRawWired(t, userAskOf(t, fp.bodyAt(t, 0)), "执笔跳 GenerateWithPlan")

	// 构思跳单发一发：它定的是「这一篇写什么」，抽参丢了什么它就跟着丢什么。
	fp2 := &fakeProvider{content: "· 要点"}
	eng2 := newTestEngine(t, fp2)
	if _, err := eng2.PlanEssay(context.Background(), sc, map[string]string{"company_name": "星禾云桥科技"}, rawNewsMsg); err != nil {
		t.Fatalf("PlanEssay: %v", err)
	}
	assertRawWired(t, userAskOf(t, fp2.bodyAt(t, 0)), "构思跳 PlanEssay(命中技能)")
}

// TestManualDraftHopCarriesUserWordsVerbatim 守手册链路（判类 → 按类执笔 → 审稿）。
//
// 手册链路最容易被误判成「安全」：它确实注入了本类要求与本类范文，看起来素材很足。
// 但范文里带的是 {公司名称} 这种占位符，真正的值只在用户原话里 —— 原话不进 prompt，
// 模型能做的就只有照抄占位符或者自己编。这条 A/B 就是为它钉的。
func TestManualDraftHopCarriesUserWordsVerbatim(t *testing.T) {
	pack := &WritePack{Categories: []WriteCategory{{
		CategoryDoc: store.CategoryDoc{
			File:        "categories/01-新闻通稿.md",
			Name:        "企业新闻通稿",
			Trigger:     "需要对外发布产品/技术消息时",
			Requirement: "正文分三段：发布背景、技术亮点、行业影响。",
		},
		Examples: []store.CategoryExample{{
			Path:    "examples/新闻通稿/01.md",
			Content: "{公司名称}于{日期}正式发布{产品名}，覆盖{客户数}家客户。",
		}},
	}}}
	cat := pack.FindCategory("企业新闻通稿")
	if cat == nil {
		t.Fatal("FindCategory 没命中，用例自身不成立")
	}

	fp := &fakeProvider{content: "初稿正文"}
	eng := newTestEngine(t, fp)
	sc := &SkillContent{Slug: "公司新闻通稿", Name: "公司新闻通稿",
		SkillType: "write", SystemPrompt: "你是公司新闻通稿的执行者。"}

	if _, err := eng.GenerateWithPack(context.Background(), sc, pack, cat,
		map[string]string{"company_name": "星禾云桥科技"}, "", rawNewsMsg, nil); err != nil {
		t.Fatalf("GenerateWithPack: %v", err)
	}
	assertRawWired(t, userAskOf(t, fp.bodyAt(t, 0)), "手册链路起草跳 GenerateWithPack")
}

// TestUserRawBlockOmittedWhenNoUserText 守等价性：本轮确实没有用户文本时，prompt 必须
// 与改造前逐字等价 —— 不许凭空塞一个空的「原话」块（那会多出一段没有内容的指令）。
func TestUserRawBlockOmittedWhenNoUserText(t *testing.T) {
	fp := &fakeProvider{content: "正文"}
	eng := newTestEngine(t, fp)
	sc := agentSkillContent("公司新闻通稿")
	if _, err := eng.GenerateWithPlan(context.Background(), sc, nil, "要点", "", func(string) {}); err != nil {
		t.Fatalf("GenerateWithPlan: %v", err)
	}
	ask := userAskOf(t, fp.bodyAt(t, 0))
	if strings.Contains(ask, "# 本轮用户原话") {
		t.Fatalf("userMsg 为空时不该出现原话块：\n%s", ask)
	}
	if !strings.Contains(ask, "# 本次写作的具体要求") {
		t.Fatalf("抽参块没了 —— 这条用例没读到真实 prompt：\n%s", ask)
	}
}
