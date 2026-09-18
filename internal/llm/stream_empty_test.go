package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 这一组守的是线上那条报错：
//
//	生成 skill 时 step2 元数据失败：模型输出不是合法json
//	unexpected end of json input 原文=<<>>
//
// 原文是空的 —— 真正的故障是「200 但正文一个字都没有」，而报错却把它说成
// 「JSON 格式不对」。用户拿着这句话既不知道发生了什么，也不知道该改什么。
//
// 两条断言钉住修法：
//  1. 空正文必须是**有身份的错误**（ErrEmptyContent），不能被当成成功结果返回空串；
//  2. 空正文必须先试一次放大输出预算再放弃 —— reasoning 模型把 completion 预算
//     吃光是最常见的成因，一次重试常常就活了（下一条用例证明它真的能活）。

// reasoningOnlyFrames：模型全程只嘟囔、一个字的正文都不给，usage 显示预算全花在思考上。
func reasoningOnlyFrames() string {
	return strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"先看手册要求，"}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"再定文风。"}}]}`,
		`data: {"choices":[{"delta":{}}],"usage":{"completion_tokens":2048,"completion_tokens_details":{"reasoning_tokens":2048}}}`,
		`data: [DONE]`,
		``,
	}, "\n\n")
}

// TestStreamChatEmptyContentIsAnErrorNotASilentEmptyString
// 修前：返回 ("", nil) —— 空串就这么流到调用方的 json.Unmarshal。
// 修后：返回 ErrEmptyContent，并且已经自动放大预算重试过一次。
func TestStreamChatEmptyContentIsAnErrorNotASilentEmptyString(t *testing.T) {
	s := &streamSrv{frames: func(int, map[string]any) (int, string) {
		return http.StatusOK, reasoningOnlyFrames()
	}}
	srv := newStreamSrv(t, s)
	defer srv.Close()
	c := streamClient(t, srv)

	var reasonSeen, notes []string
	out, err := c.StreamChat(context.Background(), "sys", "user", StreamOpts{
		MaxTokens:       512,
		DisableThinking: true,
		OnReasoning: func(s string) {
			reasonSeen = append(reasonSeen, s)
		},
		OnNote: func(s string) { notes = append(notes, s) },
	})

	if err == nil {
		t.Fatalf("空正文被当成成功返回了（out=%q）：调用方会拿空串去 json.Unmarshal，"+
			"于是用户看到的是「模型输出不是合法json unexpected end of json input 原文=<<>>」", out)
	}
	if !IsEmptyContent(err) {
		t.Fatalf("错误没有 ErrEmptyContent 身份，上层没法判定「模型没干活」：%v", err)
	}
	if strings.Contains(err.Error(), "unexpected end of json input") {
		t.Fatalf("错误里还带着 json 解析文案，等于把真因说成格式问题：%v", err)
	}
	// 诊断信息必须指出「预算被思考链吃光」，这是运维改配置的唯一线索。
	for _, want := range []string{"思考链", "reasoning_tokens=2048"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误里缺 %q，运维看不出是 provider 侧的问题：%v", want, err)
		}
	}
	// 思考链必须一边流一边给用户看：盯着计时器的时候，材料是唯一的「它在干活」证据。
	// 次数不去卡死——第二次重试会把思考链再念一遍，那是重试的正常副产品；
	// 但第一遍的两片必须原样到达，且旁白要把「重念一遍」的原因说清楚。
	if len(reasonSeen) < 2 || reasonSeen[0] != "先看手册要求，" || reasonSeen[1] != "再定文风。" {
		t.Fatalf("思考链没按序流给用户（收到 %d 片）：用户盯着计时器时，中间材料是唯一的反馈", len(reasonSeen))
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "没有正文") || !strings.Contains(notes[0], "2048") {
		t.Fatalf("重试没有旁白（或旁白没说清在干什么）：notes=%q。"+
			"材料流莫名其妙从头再念一遍，用户会以为程序卡了又重启", notes)
	}

	// 必须重试一次，且这一次是「放大预算」的重试，不是原样重放。
	if len(s.bodies) != 2 {
		t.Fatalf("期望重试 1 次（共 2 次请求），实际 %d 次", len(s.bodies))
	}
	if got := s.bodyAt(t, 1)["max_tokens"]; got != float64(2048) {
		t.Fatalf("第二次请求的 max_tokens = %v，期望 2048（512×4）", got)
	}
	// 第一次的 knob 该带还得带：不能因为要放大预算就把「关思考链」丢掉。
	if got := s.bodyAt(t, 1)["reasoning_effort"]; got != "none" {
		t.Fatalf("重试把关思考链的开关丢了，等于让模型想得更久：reasoning_effort=%v", got)
	}
}

// TestStreamChatEmptyContentThenRecoversOnRetry
// 只报错不够 —— 常见情形是一次重试就活（第一次的思考链把预算吃光了）。
// 这条用例证明放大预算的重试真的能救回来，而不是「报错报得更漂亮」。
func TestStreamChatEmptyContentThenRecoversOnRetry(t *testing.T) {
	s := &streamSrv{frames: func(n int, body map[string]any) (int, string) {
		if n == 1 {
			return http.StatusOK, reasoningOnlyFrames()
		}
		// 第二次：预算放大后终于吐正文。
		return http.StatusOK, strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"# 数据治理通知\n\n各部门："}}]}`,
			`data: {"choices":[{"delta":{"content":"请于月底前完成核对。"}}]}`,
			`data: [DONE]`,
			``,
		}, "\n\n")
	}}
	srv := newStreamSrv(t, s)
	defer srv.Close()
	c := streamClient(t, srv)

	out, err := c.StreamChat(context.Background(), "sys", "user", StreamOpts{MaxTokens: 512})
	if err != nil {
		t.Fatalf("重试后拿到正文却还报错：%v", err)
	}
	if !strings.Contains(out, "数据治理通知") {
		t.Fatalf("正文没回来：%q", out)
	}
	if len(s.bodies) != 2 {
		t.Fatalf("期望 2 次请求，实际 %d 次", len(s.bodies))
	}
}

// TestStreamChatEmptyRetryBudgetIsCapped
// 放大不能没有上限：这条路上跑的是长文执笔，调用方给的预算可能本身就不小。
func TestStreamChatEmptyRetryBudgetIsCapped(t *testing.T) {
	s := &streamSrv{frames: func(int, map[string]any) (int, string) {
		return http.StatusOK, reasoningOnlyFrames()
	}}
	srv := newStreamSrv(t, s)
	defer srv.Close()
	c := streamClient(t, srv)

	if _, err := c.StreamChat(context.Background(), "sys", "user", StreamOpts{MaxTokens: 3000}); err == nil {
		t.Fatal("空正文该报错")
	}
	if got := s.bodyAt(t, 1)["max_tokens"]; got != float64(maxEmptyRetryTokens) {
		t.Fatalf("第二次 max_tokens = %v，期望被夹到上限 %d", got, maxEmptyRetryTokens)
	}
}

// TestStreamChatEmptyRetryDoesNotAmplifyTwice
// 最多放大一次：provider 一直只吐思考链时，别把预算一轮轮往上叠着烧钱。
func TestStreamChatEmptyRetryDoesNotAmplifyTwice(t *testing.T) {
	s := &streamSrv{frames: func(int, map[string]any) (int, string) {
		return http.StatusOK, reasoningOnlyFrames()
	}}
	srv := newStreamSrv(t, s)
	defer srv.Close()
	c := streamClient(t, srv)

	if _, err := c.StreamChat(context.Background(), "sys", "user", StreamOpts{}); !IsEmptyContent(err) {
		t.Fatalf("期望 ErrEmptyContent，实际 %v", err)
	}
	if len(s.bodies) != 2 {
		t.Fatalf("期望「首次 + 放大一次」共 2 次请求，实际 %d 次（多试一次就是多烧一次钱）", len(s.bodies))
	}
	// 调用方没给预算时也得显式给一个下限，否则「放大」无从谈起。
	if got := s.bodyAt(t, 1)["max_tokens"]; got != float64(4096) {
		t.Fatalf("未指定预算时第二次 max_tokens = %v，期望 4096", got)
	}
}

// 非流式那条路（chatWithMaterial 的回退通道）同样不能把空串当成功结果。
func TestChatEmptyContentIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":""}}],
			"usage":{"completion_tokens":1500,"completion_tokens_details":{"reasoning_tokens":1500}}}`))
	}))
	defer srv.Close()
	c := streamClient(t, srv)

	out, err := c.Chat(context.Background(), "sys", "user", true)
	if err == nil {
		t.Fatalf("空正文被当成成功返回了：out=%q", out)
	}
	if !IsEmptyContent(err) {
		t.Fatalf("非流式空正文没有 ErrEmptyContent 身份：%v", err)
	}
	if !strings.Contains(err.Error(), "reasoning_tokens=1500") {
		t.Fatalf("错误里缺 token 指纹：%v", err)
	}
}

// 反向用例：正文正常时守卫不许误伤（否则会把成功的调用判成失败）。
func TestChatNonEmptyContentUnaffected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"name\":\"x\"}"}}]}`))
	}))
	defer srv.Close()
	c := streamClient(t, srv)

	out, err := c.Chat(context.Background(), "sys", "user", true)
	if err != nil {
		t.Fatalf("正常返回被判失败：%v", err)
	}
	if out != `{"name":"x"}` {
		t.Fatalf("正文被改写：%q", out)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("正文居然不是合法 json：%v", err)
	}
}
