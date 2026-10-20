package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// 构思跳必须带会话历史 —— 守「用户补充信息后，AI 忘了最开始说的」这条。
//
// 用户原话：「很多时候我给的信息不完整，它给我意见说要补充啥，我补充了，它似乎把我
// 补充的话当成了第一句话，忘了我最开始说的以及它自己答的东西了。」
//
// 抓包实测（同一会话两轮：①「写个员工培训通知，讲师和时间我还没定」②「张老师」）：
//
//	[分类器] 上下文 = 【最近对话（原文）】用户：写个员工培训通知…   ← 它有历史
//	[构思]   上下文 = 用户本轮需求：张老师                        ← 历史全没了
//
// 分类器判「这是在续写培训通知」是对的，可真正决定「这一篇写什么」的构思跳只看到
// 「张老师」三个字，于是当成一篇新文章的题目（实测输出「张老师人物素描」构思）。
// 执笔跳拿到的要点已经是错的，再在后面补历史也救不回来。
//
// 这条断言打在**实际发给模型的 prompt** 上（fakeProvider 收 body），不是打在
// 「函数签名有没有 history 参数」上：传了参数但没拼进 prompt 是同一种病。
func TestPlanEssayCarriesConversationHistory(t *testing.T) {
	fp := &fakeProvider{content: "· 写一份员工培训通知"}
	eng := newTestEngine(t, fp)

	history := []Message{
		{Role: "user", Content: "写个员工培训通知，讲师和时间我还没定"},
		{Role: "assistant", Content: "好的，请问讲师是谁、培训时间定在哪天？"},
	}
	if _, err := eng.PlanEssayWithHistory(context.Background(), "s1", history,
		nil, nil, "张老师"); err != nil {
		t.Fatalf("构思失败: %v", err)
	}

	if len(fp.bodies) == 0 {
		t.Fatal("没有发出请求")
	}
	sent := lastUserContent(t, fp.bodies[len(fp.bodies)-1])

	// 1) 第一轮的原始要求必须还在
	if !strings.Contains(sent, "员工培训通知") {
		t.Fatalf("构思跳的输入里丢了第一轮的要求（「员工培训通知」）——\n"+
			"模型只看得到「张老师」，只能把它当成新题目重写。实际输入：\n%s", sent)
	}
	// 2) 本轮消息也要在（不能为了前情把它挤掉）
	if !strings.Contains(sent, "张老师") {
		t.Fatalf("构思跳的输入里丢了本轮消息：\n%s", sent)
	}
	// 3) 明确告知「本轮是补充，不是换题目」——措辞是判据的一部分，
	//    极速档栽的就是「你只需回应最新用户消息」这种反向诱导。
	if !strings.Contains(sent, "补充") {
		t.Errorf("没有说明本轮消息是对前情的补充，模型仍可能把它读成新题目：\n%s", sent)
	}
}

// 没有历史时不能凭空造一个「前情」块（会污染第一轮的措辞，也可能诱导模型编造上下文）。
func TestPlanEssayWithoutHistoryHasNoBackstoryBlock(t *testing.T) {
	fp := &fakeProvider{content: "· 要点"}
	eng := newTestEngine(t, fp)
	if _, err := eng.PlanEssay(context.Background(), nil, nil, "写一篇新闻稿"); err != nil {
		t.Fatalf("构思失败: %v", err)
	}
	sent := lastUserContent(t, fp.bodies[len(fp.bodies)-1])
	if strings.Contains(sent, "【前情") {
		t.Fatalf("第一轮不该有前情块：\n%s", sent)
	}
	if !strings.Contains(sent, "写一篇新闻稿") {
		t.Fatalf("本轮需求丢了：\n%s", sent)
	}
}

// lastUserContent 取出请求里最后一条 user 消息的文本。
func lastUserContent(t *testing.T, body map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(body["messages"])
	var msgs []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}
	if err := json.Unmarshal(raw, &msgs); err != nil {
		t.Fatalf("解析 messages 失败: %v", err)
	}
	last := ""
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		switch c := m.Content.(type) {
		case string:
			last = c
		case []any:
			// 多段 content：拼成一个字符串
			var sb strings.Builder
			for _, part := range c {
				if p, ok := part.(map[string]any); ok {
					if s, ok := p["text"].(string); ok {
						sb.WriteString(s)
					}
				}
			}
			last = sb.String()
		}
	}
	return last
}
