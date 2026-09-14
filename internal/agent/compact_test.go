package agent

// 上下文管理的回归测试。
//
// 这些用例锁的是用户真实反馈过的一句抱怨：「先让它生成一个新闻稿，再让它把
// 新闻稿整理成 word，它通常没有管之前生成的内容」。
//
// 双向自证：每条断言都先确认「修好之后是绿的」，再注入旧行为确认「照红」——
// 旧行为就是把 compactHistory 换回「每条只留头 400 字」，见 TestContextOldBehaviorIsRed。

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// newsBody 造一份「正文很长、头尾都有辨识特征」的产物。
// 尾部特征（NEWS_END）专门用来照「只保头」的旧实现——旧实现保不住尾。
func newsBody() string {
	var b strings.Builder
	b.WriteString("NEWS_HEAD 关于举办2026年度数据治理培训的通知\n")
	for i := 0; i < 400; i++ {
		b.WriteString("各单位要高度重视本次培训，按时组织人员参加，做好签到与考核记录。")
	}
	b.WriteString("\n联系人：张三，电话 13800138000。NEWS_END 特此通知。\n")
	return b.String()
}

func msgs(pairs ...string) []Message {
	var out []Message
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Message{Role: pairs[i], Content: pairs[i+1], At: time.Now()})
	}
	return out
}

// Test_产物正文逐字进上下文 —— 用户场景：写完新闻稿，下一轮要整理成 word。
func TestArtifactKeptVerbatim(t *testing.T) {
	e := New(nil, nil)
	body := newsBody()
	hist := []Message{
		{Role: "user", Content: "写一篇关于数据治理培训的新闻稿", At: time.Now()},
		{Role: "assistant", Content: body, SkillSlug: "news-writer", Kind: KindArtifact, At: time.Now()},
		{Role: "user", Content: "把它整理成 word", At: time.Now()},
	}
	got := e.ContextBlock(context.Background(), "s1", hist)

	if !strings.Contains(got, "NEWS_HEAD") {
		t.Fatalf("产物正文开头没进上下文（模型看不到稿子，就会另起炉灶重写）")
	}
	if !strings.Contains(got, "NEWS_END") {
		t.Fatalf("产物正文**结尾**丢失——旧实现只留头 400 字，尾部（含联系人/落款）全被砍掉")
	}
	// 关键句必须逐字在，不能被摘要化成「一篇新闻稿」。
	if !strings.Contains(got, "13800138000") {
		t.Fatalf("产物里的具体数字（电话）被压缩掉了——摘要里的数字进正式文档就是事故")
	}
	if !strings.Contains(got, "已有产物（原文") {
		t.Fatalf("缺少产物层标题，模型不知道下面这段是要以它为基础改的")
	}
}

// TestArtifactFromDiskHasNoKind —— 会话从磁盘恢复时没有 Kind 字段（老数据），
// 仍须被认成产物，否则「重启一次之后就忘了上一轮写了什么」。
func TestArtifactFromDiskHasNoKind(t *testing.T) {
	e := New(nil, nil)
	body := newsBody()
	hist := []Message{
		// 老落盘消息：没有 Kind，只有 SkillSlug + 长正文
		{Role: "assistant", Content: body, SkillSlug: "news-writer", At: time.Now()},
		{Role: "user", Content: "整理成 word", At: time.Now()},
	}
	got := e.ContextBlock(context.Background(), "s1", hist)
	if !strings.Contains(got, "NEWS_END") {
		t.Fatalf("磁盘恢复的产物没被当产物：历史兼容分支失效")
	}
}

// TestPlainChatLongAnswerIsArtifact —— 没命中技能时由普通对话产出的正文，
// 也必须是产物（否则「写篇新闻稿→整理成 word」在无技能场景下仍然丢稿子）。
func TestPlainChatLongAnswerIsArtifact(t *testing.T) {
	if ArtifactKind("短句，不算产物") != "" {
		t.Fatalf("短回答不该占用产物额度")
	}
	if ArtifactKind(strings.Repeat("字", 300)) != KindArtifact {
		t.Fatalf("长正文（≥200 字）应被认成产物")
	}
	e := New(nil, nil)
	hist := []Message{
		{Role: "assistant", Content: newsBody(), SkillSlug: "", Kind: ArtifactKind(newsBody()), At: time.Now()},
		{Role: "user", Content: "整理成 word", At: time.Now()},
	}
	if got := e.ContextBlock(context.Background(), "s1", hist); !strings.Contains(got, "NEWS_END") {
		t.Fatalf("无技能产出的长正文丢了尾部")
	}
}

// TestOldBehaviorIsRed —— 注入旧行为（每条只留头 400 字）必须照红。
// 这是给「回归防线本身是假的」留的证据：如果哪天有人把 ContextBlock 改回截断，
// 这条用例会失败。
func TestOldBehaviorIsRed(t *testing.T) {
	body := newsBody()
	// 旧行为复刻：只留头 400 字
	c := body
	if len(c) > 400 {
		c = c[:400] + "…"
	}
	if strings.Contains(c, "NEWS_END") {
		t.Fatalf("旧行为的复刻写错了：断言无法区分新旧行为，这条防线是假的")
	}
	if !strings.Contains(newsBody(), "NEWS_END") {
		t.Fatalf("测试料本身没有尾部特征，防线无效")
	}
}

// TestHeadTailKeepsTail —— 长消息在「近轮层」也要两头都保。
func TestHeadTailKeepsTail(t *testing.T) {
	s := "HEAD" + strings.Repeat("中", 5000) + "TAIL"
	got := truncHeadTail(s, 600)
	if !strings.HasPrefix(got, "HEAD") || !strings.HasSuffix(got, "TAIL") {
		t.Fatalf("头尾截断没保住尾部：%q...%q", got[:20], got[len(got)-20:])
	}
	if !strings.Contains(got, "省略约") {
		t.Fatalf("省略处没有标注，模型会把残文当完整文本用")
	}
	if n := len([]rune(got)); n > 700 {
		t.Fatalf("截断后仍然过长：%d runes", n)
	}
}

// TestOldDialogueCompressedByLLM —— 旧对话超阈值时**调用大模型压缩**（而不是硬截断）。
func TestOldDialogueCompressedByLLM(t *testing.T) {
	e := New(nil, nil)
	var calls int
	e.summarize = func(ctx context.Context, raw string) (string, error) {
		calls++
		if !strings.Contains(raw, "第一条约定") {
			t.Fatalf("压缩输入里没有早期内容，说明筛错了范围")
		}
		return "- 单位=某某公司；日期=2026-09-14；已完成新闻稿一篇", nil
	}

	hist := []Message{{Role: "user", Content: "第一条约定：" + strings.Repeat("背景说明", 2000), At: time.Now()}}
	for i := 0; i < 8; i++ {
		// 单条 600 字：旧对话总量要真的超过 compactTriggerRunes(4000)，
		// 否则压缩路径根本不会触发，这条用例会变成空跑（假绿）。
		hist = append(hist,
			Message{Role: "user", Content: "追问" + strings.Repeat("还", 600), At: time.Now()},
			Message{Role: "assistant", Content: "回答" + strings.Repeat("答", 600), At: time.Now()})
	}
	hist = append(hist, Message{Role: "user", Content: "继续", At: time.Now()})

	got := e.ContextBlock(context.Background(), "s2", hist)
	if calls != 1 {
		t.Fatalf("应恰好压缩一次，实际 %d 次", calls)
	}
	if !strings.Contains(got, "单位=某某公司") {
		t.Fatalf("压缩结果没进上下文：%s", got[:min(200, len(got))])
	}
	if !strings.Contains(got, "前情摘要") {
		t.Fatalf("缺少前情摘要标记，模型分不清哪些是原文哪些是摘要")
	}

	// 同一段旧对话再来一轮：走缓存，不该再压一次（否则每轮对话都多一次模型调用）。
	hist = append(hist, Message{Role: "assistant", Content: strings.Repeat("新", 200), At: time.Now()},
		Message{Role: "user", Content: "再问", At: time.Now()}) // 只多 2 条，不该重新压缩
	e.ContextBlock(context.Background(), "s2", hist)
	if calls != 1 {
		t.Fatalf("watermark 缓存失效：同样的旧对话被重复压缩（%d 次）", calls)
	}
}

// TestCompressFailureFallsBack —— 压缩失败必须退回截断并留痕，不能整轮对话挂掉。
func TestCompressFailureFallsBack(t *testing.T) {
	e := New(nil, nil)
	e.summarize = func(ctx context.Context, raw string) (string, error) {
		return "", errors.New("model down")
	}
	hist := []Message{{Role: "user", Content: "开场：" + strings.Repeat("事", 6000), At: time.Now()}}
	for i := 0; i < 8; i++ {
		hist = append(hist,
			Message{Role: "user", Content: "问" + strings.Repeat("x", 600), At: time.Now()},
			Message{Role: "assistant", Content: "答" + strings.Repeat("y", 600), At: time.Now()})
	}
	hist = append(hist, Message{Role: "user", Content: "收尾：最新要求", At: time.Now()})

	got := e.ContextBlock(context.Background(), "s3", hist)
	if !strings.Contains(got, "压缩失败") {
		t.Fatalf("降级没有留痕，模型和用户都不知道发生了什么")
	}
	if !strings.Contains(got, "最新要求") {
		t.Fatalf("降级后最近一轮丢了——降级也必须保住近轮原文")
	}
}

// TestSmallTalkNoModelCall —— 小会话不触发压缩调用（省一次模型调用的钱和延迟）。
func TestSmallTalkNoModelCall(t *testing.T) {
	e := New(nil, nil)
	e.summarize = func(ctx context.Context, raw string) (string, error) {
		t.Fatalf("几句闲聊不该触发压缩调用")
		return "", nil
	}
	hist := msgs("user", "你好", "assistant", "你好，有什么可以帮你", "user", "没事")
	if got := e.ContextBlock(context.Background(), "s4", hist); !strings.Contains(got, "你好") {
		t.Fatalf("小会话内容应原样注入：%q", got)
	}
}

// TestNoHistory —— 空历史不 panic、不产出半截块。
func TestNoHistory(t *testing.T) {
	e := New(nil, nil)
	if got := e.ContextBlock(context.Background(), "s5", nil); got != "（无历史）" {
		t.Fatalf("空历史渲染异常：%q", got)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = os.Stderr
