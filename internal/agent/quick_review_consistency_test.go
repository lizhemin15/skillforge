package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// 自检（QuickReview）的两条不变量 —— 守「自检不能自相矛盾」。
//
// 线上实测到的矛盾（同一份写作要求，两轮给出互相打架的判词）：
//
//	第1轮：成稿按规范用了【待补充：xxx】占位
//	       → 自检判「包含了大量【待补充】占位符…导致成稿不可直接使用」  ✗ 惩罚了正确行为
//	第2轮：成稿在某处编了个具体值
//	       → 自检判「未按要求使用【待补充】标注」                      ✓ 这条对
//
// 两条判词合起来看就是：标了不行，不标也不行。用户观感是「我越补它越不满」。
// 根因有二：(1) 审稿规则没说清「占位符是正确行为，不是缺陷」；
// (2) 自检只拿到**本轮**那句话（多轮时往往只是「张老师」），却被要求审一整篇通知，
// 于是把用户第一轮说过的主题判成「自行编造」。
//
// 这里把两件事都钉在**实际发给审稿人的 prompt** 上。

// ① 用户说过的话必须进自检上下文，且明确标注「最后一条常常只是补充」。
func TestQuickReviewCarriesUserTurns(t *testing.T) {
	fp := &fakeProvider{content: `{"pass":true,"items":[]}`}
	eng := newTestEngine(t, fp)

	history := []Message{
		{Role: "user", Content: "写个员工培训通知，讲师和时间我还没定"},
		{Role: "assistant", Content: "好的，请问讲师是谁？"},
	}
	eng.QuickReviewWithHistory(context.Background(), "s1", "张老师", "成稿正文", "写作要求若干", history)

	if len(fp.bodies) == 0 {
		t.Fatal("没有发出自检请求")
	}
	sent := lastUserContent(t, fp.bodies[len(fp.bodies)-1])
	if !strings.Contains(sent, "员工培训通知") {
		t.Fatalf("自检上下文里没有用户第一轮的要求 —— 审稿人会把整篇通知判成「自行编造主题」：\n%s", sent)
	}
	if !strings.Contains(sent, "张老师") {
		t.Fatalf("自检上下文里没有本轮消息：\n%s", sent)
	}
	if !strings.Contains(sent, "不能只看最后一条") {
		t.Errorf("没有提示「最后一条往往只是补充」：\n%s", sent)
	}
	// 助手自己的话不该进判据：那是自述，会让起草者给自己的稿子放行。
	if strings.Contains(sent, "好的，请问讲师是谁") {
		t.Errorf("助手自己的话不应进自检判据（只取用户说过的话）：\n%s", sent)
	}
}

// ② 审稿规则必须把「占位符」定成正确行为，并禁止两种判法同时使用。
func TestQuickReviewRulesTreatPlaceholderAsCorrect(t *testing.T) {
	fp := &fakeProvider{content: `{"pass":true,"items":[]}`}
	eng := newTestEngine(t, fp)
	eng.QuickReviewWithHistory(context.Background(), "", "写个通知", "成稿（含【待补充：地点】）", "写作要求", nil)
	sent := lastUserContent(t, fp.bodies[len(fp.bodies)-1])
	_ = sent
	sys := fp.systemContent(t, len(fp.bodies)-1)
	for _, want := range []string{"占位符", "不算", "不得在同一份成稿上同时使用", "不许复用"} {
		if !strings.Contains(sys, want) {
			t.Fatalf("自检规则里缺关键约束 %q —— 审稿人会重新把占位符判成缺陷（线上实测过）：\n%s", want, sys)
		}
	}
}

// ③ 自检解析：pass=false 但 items 为空时不能当成「有 0 条不足」的死循环式追问。
func TestQuickVerdictEmptyItemsIsNotAHardFail(t *testing.T) {
	raw := `{"pass":false,"items":[]}`
	var v QuickVerdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// pass=false 且没有可执行的 ask，等于给用户一张什么都写不了的条子；
	// 调用方必须能识别出这种「无内容的不通过」。
	if v.Pass {
		t.Errorf("Pass 字段应如实反映 pass=false")
	}
	if len(v.Items) != 0 {
		t.Errorf("items 应为空")
	}
	note := QuickReviewNote(&v)
	if strings.TrimSpace(note) == "" {
		t.Logf("空 items 时附注为空 —— 调用方应视为无话可说（不输出「差这 0 处」这种残句）")
	}
	if strings.Contains(note, "差这 0 处") || strings.Contains(note, "差这 处") {
		t.Fatalf("空 items 不该拼出残句：%q", note)
	}
}

// systemContent 取第 i 次请求的 system 内容。
func (f *fakeProvider) systemContent(t *testing.T, i int) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < 0 || i >= len(f.bodies) {
		t.Fatalf("请求序号越界")
	}
	raw, _ := json.Marshal(f.bodies[i]["messages"])
	var msgs []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}
	_ = json.Unmarshal(raw, &msgs)
	for _, m := range msgs {
		if m.Role == "system" {
			if s, ok := m.Content.(string); ok {
				return s
			}
		}
	}
	return ""
}

var _ = fmt.Sprintf
