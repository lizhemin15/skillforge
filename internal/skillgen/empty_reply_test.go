package skillgen

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/llm"
)

// 这一组守的是用户实际看到的那句话：
//
//	生成 skill 时 step2 元数据失败：模型输出不是合法json
//	unexpected end of json input 原文=<<>>
//
// 「原文=<<>>」说明模型这一轮一个字都没写；而报错说的是「JSON 不合法」。
// 用户照着这句话查不出任何东西——格式没错，是模型没干活。责任链要指向真因，
// 并且必须给出用户自己能做的动作（他的提示词没问题，是 provider 侧的配置问题）。

// blankStreamer 模拟「只嘟囔不给正文」的 provider。
// streamEmpty=true 时返回 llm.ErrEmptyContent（守卫修好后真实客户端的行为）；
// =false 时返回 ("", nil)（守卫被拿掉时的行为，由注入用例自证这一条会把用户引偏）。
type blankStreamer struct {
	chatCalls   int
	streamCalls int
	streamEmpty bool
	chatEmpty   bool
	chatReturn  string
}

func (b *blankStreamer) Chat(ctx context.Context, sys, user string, jsonMode ...bool) (string, error) {
	b.chatCalls++
	if b.chatEmpty {
		return "", fmt.Errorf("%w（reasoning_tokens=2048）", llm.ErrEmptyContent)
	}
	if b.chatReturn != "" {
		return b.chatReturn, nil
	}
	return "{}", nil
}

func (b *blankStreamer) StreamChat(ctx context.Context, sys, user string, o llm.StreamOpts) (string, error) {
	b.streamCalls++
	// 只嘟囔不给正文：思考链照流（中间材料不能因为最终失败就消失）
	if o.OnReasoning != nil {
		o.OnReasoning("先想一下手册要求…")
	}
	if b.streamEmpty {
		return "", fmt.Errorf("%w（reasoning_tokens=2048）", llm.ErrEmptyContent)
	}
	return "", nil
}

// 用户看到的必须是可执行的中文说明，而不是一句 json 报错。
func TestBlankProviderTellsUserToFixProviderNotPrompt(t *testing.T) {
	f := &blankStreamer{streamEmpty: true, chatEmpty: true}
	g := &Generator{llm: f}
	relay, got := collectAll()
	ctx := WithDelta(context.Background(), relay.Push)

	out, err := g.chatWithMaterial(ctx, "sys", "user", true)
	relay.Flush()

	if err == nil {
		t.Fatalf("两个通道都空还当成功返回：out=%q", out)
	}
	msg := err.Error()
	for _, bad := range []string{"unexpected end of json input", "原文=<<>>", "不是合法json"} {
		if strings.Contains(msg, bad) {
			t.Fatalf("用户又被引到「格式不对」上去了（含 %q）：%s", bad, msg)
		}
	}
	for _, want := range []string{"思考过程", "推理模型", "管理端"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("报错里缺 %q，用户不知道该改什么：%s", want, msg)
		}
	}
	// 思考链必须留在材料流里：这一轮虽然失败，但用户至少看得到模型在想什么，
	// 而不是盯着一个计时器最后收到一句红字。
	joined := strings.Join(*got, "|")
	if !strings.Contains(joined, "think:先想一下手册要求…") {
		t.Fatalf("失败路径把思考链吞了，实收: %s", joined)
	}
	if !strings.Contains(joined, MaterialNote+":") || !strings.Contains(joined, "没有正文") {
		t.Fatalf("失败路径没把旁白说清（应指出「只回了思考过程、没有正文」），用户不知道系统在干什么：%s", joined)
	}
	if f.streamCalls != 1 || f.chatCalls != 1 {
		t.Fatalf("应「流式一次 + 非流式回退一次」：stream=%d chat=%d", f.streamCalls, f.chatCalls)
	}
}

// 守卫修好后主路径就报错了，但「同一个坑换个入口再踩」的兜底还得在：
// 任何一条流式实现返回空串时，都要退回非流式通道，并且留下旁白。
func TestEmptyStreamFallsBackAndSaysSo(t *testing.T) {
	f := &blankStreamer{chatReturn: `{"name":"x"}`}
	g := &Generator{llm: f}
	relay, got := collectAll()
	ctx := WithDelta(context.Background(), relay.Push)

	out, err := g.chatWithMaterial(ctx, "sys", "user", true)
	relay.Flush()
	if err != nil {
		t.Fatalf("非流式通道有正文却报错: %v", err)
	}
	if !strings.Contains(out, `"name"`) {
		t.Fatalf("没把非流式通道的结果交出来：%q", out)
	}
	joined := strings.Join(*got, "|")
	if !strings.Contains(joined, "非流式通道再问一次") {
		t.Fatalf("空正文回退没留旁白，材料会忽然中断：%s", joined)
	}
	if f.chatCalls != 1 {
		t.Fatalf("应退回非流式问一次，实际 %d 次", f.chatCalls)
	}
}

// 旁白是流水线自己说的话，不能混进思考链通道——
// 混了用户会以为「模型想到要重试」，把系统的动作读成模型的推理。
func TestEmptyRetryNoteNeverClaimsFakeReasoning(t *testing.T) {
	f := &blankStreamer{streamEmpty: true, chatEmpty: true}
	g := &Generator{llm: f}
	relay, got := collectAll()
	ctx := WithDelta(context.Background(), relay.Push)
	_, _ = g.chatWithMaterial(ctx, "sys", "user")
	relay.Flush()

	for _, m := range *got {
		if strings.HasPrefix(m, MaterialThink+":") && !strings.Contains(m, "先想一下手册要求…") {
			t.Fatalf("旁白混进思考链通道：%s", m)
		}
	}
}

// 守卫必须只对「真空」生效：非流式通道有正文时不能被误判，否则失败会被当成成功。
func TestNormalReplyUnaffectedByEmptyGuard(t *testing.T) {
	f := &blankStreamer{streamEmpty: true} // 流式空、非流式有
	g := &Generator{llm: f}
	out, err := g.chatWithMaterial(context.Background(), "sys", "user", true)
	if err != nil || out != "{}" {
		t.Fatalf("非流式有正文却报错：out=%q err=%v", out, err)
	}
}
