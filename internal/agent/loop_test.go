package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/tools"
)

// fakeChatter 按剧本依次返回响应，并记录每次收到的消息与工具定义。
type fakeChatter struct {
	script  []llm.Msg
	seen    [][]llm.Msg
	seenDef [][]llm.ToolDef
	err     error
}

func (f *fakeChatter) ChatTools(_ context.Context, msgs []llm.Msg, defs []llm.ToolDef) (llm.Msg, error) {
	f.seen = append(f.seen, append([]llm.Msg{}, msgs...))
	f.seenDef = append(f.seenDef, defs)
	if f.err != nil {
		return llm.Msg{}, f.err
	}
	if len(f.seen) > len(f.script) {
		return llm.Msg{}, errors.New("剧本用尽")
	}
	return f.script[len(f.seen)-1], nil
}

// 记录型工具替身
type spyTool struct {
	name    string
	ran     int
	content string
	files   []tools.File
	err     error
}

func (s *spyTool) Name() string        { return s.name }
func (s *spyTool) Description() string { return "spy" }
func (s *spyTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "required": []string{}}
}
func (s *spyTool) Run(_ context.Context, _ map[string]any) (tools.Result, error) {
	s.ran++
	return tools.Result{Content: s.content, Files: s.files, Display: "spy完成"}, s.err
}

func toolCall(id, name, args string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: name, Args: args}
}

func newSpyRegistry(names ...string) (*tools.Registry, map[string]*spyTool) {
	r := tools.NewRegistry()
	m := map[string]*spyTool{}
	for _, n := range names {
		s := &spyTool{name: n, content: n + " 的结果"}
		r.Register(s)
		m[n] = s
	}
	return r, m
}

// 最基本的组合：调一次工具 → 拿到结果 → 出最终回答。
func TestLoopToolThenAnswer(t *testing.T) {
	reg, spies := newSpyRegistry("http_request")
	spies["http_request"].files = []tools.File{{Name: "a.csv", ContentType: "text/csv", Bytes: []byte("x")}}
	ch := &fakeChatter{script: []llm.Msg{
		{Role: "assistant", ToolCalls: []llm.ToolCall{toolCall("c1", "http_request", `{"url":"https://x.com"}`)}},
		{Role: "assistant", Content: "已拿到数据，结论是 A。"},
	}}
	var events []LoopEvent
	out, err := NewLoop(ch, reg, 5).Run(context.Background(), "你是助手", nil, "拉数据", func(e LoopEvent) {
		events = append(events, e)
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "已拿到数据，结论是 A。" {
		t.Fatalf("最终回答不对: %q", out.Text)
	}
	if spies["http_request"].ran != 1 {
		t.Fatalf("工具应执行 1 次，实际 %d", spies["http_request"].ran)
	}
	if len(out.Files) != 1 || out.Files[0].Name != "a.csv" {
		t.Fatalf("文件应被收集: %+v", out.Files)
	}
	// 事件必须成对：running → done（前端靠它展示「正在调用什么」）
	if len(events) != 2 || events[0].Status != "running" || events[1].Status != "done" {
		t.Fatalf("事件序列不对: %+v", events)
	}
	if events[0].Tool != "http_request" || events[1].Note != "spy完成" {
		t.Fatalf("事件内容不对: %+v", events)
	}
	if out.Rounds != 2 {
		t.Fatalf("应记录 2 轮: %d", out.Rounds)
	}
}

// 关键细节：第二轮请求里必须带上 assistant(tool_calls) 与 tool(tool_call_id)，
// 否则真实 provider 会报「tool_call_id 找不到对应调用」。
func TestLoopHistoryShapeForProvider(t *testing.T) {
	reg, _ := newSpyRegistry("echo")
	ch := &fakeChatter{script: []llm.Msg{
		{Role: "assistant", ToolCalls: []llm.ToolCall{toolCall("call_9", "echo", `{}`)}},
		{Role: "assistant", Content: "done"},
	}}
	if _, err := NewLoop(ch, reg, 3).Run(context.Background(), "sys", nil, "hi", nil); err != nil {
		t.Fatal(err)
	}
	second := ch.seen[1]
	roles := []string{}
	for _, m := range second {
		roles = append(roles, m.Role)
	}
	want := []string{"system", "user", "assistant", "tool"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("第二轮消息结构不对: %v", roles)
	}
	if len(second[2].ToolCalls) != 1 || second[2].ToolCalls[0].ID != "call_9" {
		t.Fatalf("assistant 消息应保留 tool_calls: %+v", second[2])
	}
	if second[3].ToolCallID != "call_9" {
		t.Fatalf("tool 消息必须挂上 tool_call_id: %+v", second[3])
	}
}

// 一轮里模型可以同时要多个工具（实测该模型确实会这么干），必须都执行且顺序稳定。
func TestLoopParallelToolCalls(t *testing.T) {
	reg, spies := newSpyRegistry("http_request", "run_python")
	ch := &fakeChatter{script: []llm.Msg{
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			toolCall("a", "http_request", `{"url":"https://x"}`),
			toolCall("b", "run_python", `{"code":"print(1)"}`),
		}},
		{Role: "assistant", Content: "好了"},
	}}
	var events []LoopEvent
	out, err := NewLoop(ch, reg, 3).Run(context.Background(), "", nil, "干活", func(e LoopEvent) {
		events = append(events, e)
	})
	if err != nil {
		t.Fatal(err)
	}
	if spies["http_request"].ran != 1 || spies["run_python"].ran != 1 {
		t.Fatalf("两个工具都应执行: %+v", spies)
	}
	if strings.Join(out.Calls, ",") != "http_request,run_python" {
		t.Fatalf("调用顺序应稳定: %v", out.Calls)
	}
	if len(events) != 4 {
		t.Fatalf("应有 4 个事件（2 工具 × running/done）: %+v", events)
	}
}

// 模型给出非法 JSON 参数时：不执行工具，把原因回给模型让它自纠，循环不中断。
func TestLoopInvalidArgsFedBack(t *testing.T) {
	reg, spies := newSpyRegistry("echo")
	ch := &fakeChatter{script: []llm.Msg{
		{Role: "assistant", ToolCalls: []llm.ToolCall{toolCall("x", "echo", `{不是 json`)}},
		{Role: "assistant", Content: "修好了"},
	}}
	out, err := NewLoop(ch, reg, 3).Run(context.Background(), "", nil, "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	if spies["echo"].ran != 0 {
		t.Fatal("参数非法时不该执行工具")
	}
	if out.Text != "修好了" {
		t.Fatalf("循环应继续并在下一轮收口: %q", out.Text)
	}
	// 回给模型的 tool 消息里必须说明是参数问题
	second := ch.seen[1]
	last := second[len(second)-1]
	if last.Role != "tool" || !strings.Contains(last.Content, "参数解析失败") {
		t.Fatalf("应把参数错误回给模型: %+v", last)
	}
}

// 工具报错不能炸掉循环：错误要喂回模型，让它换个做法。
func TestLoopToolErrorIsFedBack(t *testing.T) {
	reg, spies := newSpyRegistry("http_request")
	spies["http_request"].err = errors.New("上游 503")
	ch := &fakeChatter{script: []llm.Msg{
		{Role: "assistant", ToolCalls: []llm.ToolCall{toolCall("e1", "http_request", `{}`)}},
		{Role: "assistant", Content: "接口挂了，我换方案"},
	}}
	var events []LoopEvent
	out, err := NewLoop(ch, reg, 3).Run(context.Background(), "", nil, "hi", func(e LoopEvent) {
		events = append(events, e)
	})
	if err != nil {
		t.Fatal(err)
	}
	second := ch.seen[1]
	last := second[len(second)-1]
	if !strings.Contains(last.Content, "上游 503") {
		t.Fatalf("工具错误应喂回模型: %+v", last)
	}
	if events[1].Status != "error" {
		t.Fatalf("事件应标记 error: %+v", events)
	}
	if out.Text != "接口挂了，我换方案" {
		t.Fatalf("循环应继续: %q", out.Text)
	}
}

// 模型卡住（同参数重复调同一工具）时必须打断，不能陪着烧 token。
func TestLoopBreaksRepeatLoop(t *testing.T) {
	reg, spies := newSpyRegistry("echo")
	same := llm.Msg{Role: "assistant", ToolCalls: []llm.ToolCall{toolCall("r", "echo", `{"text":"same"}`)}}
	ch := &fakeChatter{script: []llm.Msg{same, same, same, same, same, {Role: "assistant", Content: "收口答案"}}}
	out, err := NewLoop(ch, reg, 8).Run(context.Background(), "", nil, "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "打断") {
		t.Fatalf("应提示工具重复调用被打断: %q", out.Text)
	}
	if spies["echo"].ran > 3 {
		t.Fatalf("重复调用最多执行 3 次，实际 %d", spies["echo"].ran)
	}
	// 打断后要收口：最后一次调用不再提供工具
	lastDefs := ch.seenDef[len(ch.seenDef)-1]
	if lastDefs != nil {
		t.Fatalf("收口时应不带工具定义: %+v", lastDefs)
	}
}

// 到达轮数上限时必须收口（不再给工具），而不是无限循环。
func TestLoopMaxRoundsWrapUp(t *testing.T) {
	reg, spies := newSpyRegistry("echo")
	// 恰好 MaxRound 次工具调用 + 一次收口回答
	script := []llm.Msg{}
	for i := 0; i < 3; i++ {
		script = append(script, llm.Msg{Role: "assistant", ToolCalls: []llm.ToolCall{
			toolCall("c", "echo", `{"text":"r`+string(rune('a'+i))+`"}`),
		}})
	}
	script = append(script, llm.Msg{Role: "assistant", Content: "归纳完了"})
	ch := &fakeChatter{script: script}

	out, err := NewLoop(ch, reg, 3).Run(context.Background(), "", nil, "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	if spies["echo"].ran != 3 {
		t.Fatalf("3 轮应各执行一次工具，实际 %d", spies["echo"].ran)
	}
	if out.Text != "归纳完了" {
		t.Fatalf("上限后必须给出收口回答: %q", out.Text)
	}
	if ch.seenDef[len(ch.seenDef)-1] != nil {
		t.Fatal("收口时不该再提供工具")
	}
}

// 上限内每轮都调不同工具 → 到上限后收口。
func TestLoopMaxRoundsWithDistinctCalls(t *testing.T) {
	reg, _ := newSpyRegistry("echo")
	script := []llm.Msg{}
	for i := 0; i < 3; i++ {
		script = append(script, llm.Msg{Role: "assistant", ToolCalls: []llm.ToolCall{
			toolCall("c", "echo", `{"text":"r`+string(rune('a'+i))+`"}`),
		}})
	}
	script = append(script, llm.Msg{Role: "assistant", Content: "上限了，这是我的结论"})
	ch := &fakeChatter{script: script}

	out, err := NewLoop(ch, reg, 3).Run(context.Background(), "", nil, "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "上限了，这是我的结论" {
		t.Fatalf("应返回收口内容: %q", out.Text)
	}
	if out.Rounds != 3 {
		t.Fatalf("应记录 3 轮: %d", out.Rounds)
	}
}

// 模型报错要向上传，不能假装成功。
func TestLoopPropagatesLLMError(t *testing.T) {
	reg, _ := newSpyRegistry("echo")
	ch := &fakeChatter{err: errors.New("配额用尽")}
	_, err := NewLoop(ch, reg, 3).Run(context.Background(), "", nil, "hi", nil)
	if err == nil || !strings.Contains(err.Error(), "配额用尽") {
		t.Fatalf("应向上传递模型错误: %v", err)
	}
}

// 未注册任何工具时也要能正常对话（纯文本路径不能被破坏）。
func TestLoopNoToolsRegistered(t *testing.T) {
	reg := tools.NewRegistry()
	ch := &fakeChatter{script: []llm.Msg{{Role: "assistant", Content: "直接回答"}}}
	out, err := NewLoop(ch, reg, 3).Run(context.Background(), "sys", nil, "hi", nil)
	if err != nil || out.Text != "直接回答" {
		t.Fatalf("无工具时应正常回答: %q %v", out.Text, err)
	}
	if len(ch.seenDef[0]) != 0 {
		t.Fatalf("无工具时不该传工具定义: %+v", ch.seenDef[0])
	}
}

// 历史对话要带上，多轮上下文不能丢。
func TestLoopCarriesHistory(t *testing.T) {
	reg, _ := newSpyRegistry("echo")
	ch := &fakeChatter{script: []llm.Msg{{Role: "assistant", Content: "ok"}}}
	hist := []llm.Msg{
		{Role: "user", Content: "上一轮问题"},
		{Role: "assistant", Content: "上一轮回答"},
	}
	if _, err := NewLoop(ch, reg, 3).Run(context.Background(), "sys", hist, "这轮问题", nil); err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, m := range ch.seen[0] {
		got = append(got, m.Role+"="+m.Content)
	}
	want := "system=sys,user=上一轮问题,assistant=上一轮回答,user=这轮问题"
	if strings.Join(got, ",") != want {
		t.Fatalf("消息拼装不对:\n got %v\nwant %v", got, want)
	}
}

func TestSummarizeArgs(t *testing.T) {
	long := strings.Repeat("字", 300)
	got := summarizeArgs(`{"code":"` + long + `"}`)
	if len([]rune(got)) > 130 {
		t.Fatalf("参数摘要应被截断: %d 字", len([]rune(got)))
	}
	if got := summarizeArgs("  {}  "); got != "" {
		t.Fatalf("空参数应返回空串: %q", got)
	}
	if got := summarizeArgs("{\n \"a\": 1\n}"); strings.Contains(got, "\n") {
		t.Fatalf("摘要应压成单行: %q", got)
	}
}
