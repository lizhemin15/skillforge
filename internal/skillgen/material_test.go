package skillgen

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lizhemin15/skillforge/internal/llm"
)

// ---- 替身：同时具备 Chat 与 StreamChat，用来验证「走哪条路」----
type fakeStreamer struct {
	mu           sync.Mutex
	chatCalls    int
	streamCalls  int
	lastOpts     llm.StreamOpts
	streamErr    error
	streamReturn string
	chatReturn   string
}

func (f *fakeStreamer) Chat(ctx context.Context, sys, user string, jsonMode ...bool) (string, error) {
	f.mu.Lock()
	f.chatCalls++
	f.mu.Unlock()
	return f.chatReturn, nil
}

func (f *fakeStreamer) StreamChat(ctx context.Context, sys, user string, o llm.StreamOpts) (string, error) {
	f.mu.Lock()
	f.streamCalls++
	f.lastOpts = o
	err := f.streamErr
	ret := f.streamReturn
	onThink, onText := o.OnReasoning, o.OnContent
	f.mu.Unlock()
	// 真流式是「边生成边回调」，替身必须也回调 —— 不回调就等于把被测行为（转发）
	// 整个跳过了，测试会假绿。
	if onThink != nil {
		for _, s := range []string{"先看", "素材结构"} {
			onThink(s)
		}
	}
	if onText != nil {
		for _, s := range []string{"正文", "开头"} {
			onText(s)
		}
	}
	return ret, err
}

// collect 把材料收进切片（单 goroutine 调用，无需加锁）。
func collectAll() (*MaterialRelay, *[]string) {
	var got []string
	r := NewMaterialRelay(time.Millisecond, 1<<20, func(kind, text string) {
		got = append(got, kind+":"+text)
	})
	return r, &got
}

// 用户诉求「中间可以流式输出思考的一些中间材料」的第一条：挂了接收器就必须真流式，
// 而且思考链与正文分别送达（混在一起就分不出「它在想」和「它在写」）。
func TestChatWithMaterialStreamsThinkAndTextWhenSinkInjected(t *testing.T) {
	f := &fakeStreamer{streamReturn: "done"}
	g := &Generator{llm: f}
	relay, got := collectAll()
	ctx := WithDelta(context.Background(), relay.Push)

	out, err := g.chatWithMaterial(ctx, "sys", "user", true)
	if err != nil {
		t.Fatalf("chatWithMaterial 报错: %v", err)
	}
	relay.Flush()
	if out != "done" {
		t.Fatalf("返回值丢了: %q", out)
	}
	if f.streamCalls != 1 || f.chatCalls != 0 {
		t.Fatalf("应走流式一次、不走阻塞：stream=%d chat=%d", f.streamCalls, f.chatCalls)
	}
	// JSONMode 必须原样透传：训练里抽取属性、生成元数据都靠它，掉了会静默解析失败。
	if !f.lastOpts.JSONMode {
		t.Fatalf("JSONMode 没透传到 StreamOpts")
	}
	joined := strings.Join(*got, "|")
	for _, want := range []string{"think:先看", "think:素材结构", "text:正文", "text:开头"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("材料缺片段 %q，实收: %s", want, joined)
		}
	}
}

// 没注入接收器（试用、Review、别人的调用）必须走原来的阻塞调用：
// 不进流式 = 不多花一次请求、不引入 provider 兼容性风险。
func TestChatWithMaterialKeepsBlockingWhenNoSink(t *testing.T) {
	f := &fakeStreamer{chatReturn: "plain"}
	g := &Generator{llm: f}
	out, err := g.chatWithMaterial(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if out != "plain" || f.chatCalls != 1 || f.streamCalls != 0 {
		t.Fatalf("未注入接收器时不该走流式：out=%q chat=%d stream=%d", out, f.chatCalls, f.streamCalls)
	}
}

// 流的中间断了不能让整条流水线陪葬：回退阻塞调用，并且必须留下旁白，
// 否则用户看到的是「材料忽然没了」——比从来没流过更费解。
func TestChatWithMaterialFallsBackOnStreamErrorAndSaysSo(t *testing.T) {
	f := &fakeStreamer{streamErr: errors.New("网关 502"), chatReturn: "retried"}
	g := &Generator{llm: f}
	relay, got := collectAll()
	ctx := WithDelta(context.Background(), relay.Push)

	out, err := g.chatWithMaterial(ctx, "sys", "user")
	relay.Flush()
	if err != nil || out != "retried" {
		t.Fatalf("回退失败: out=%q err=%v", out, err)
	}
	if f.streamCalls != 1 || f.chatCalls != 1 {
		t.Fatalf("应「流式失败一次 + 阻塞回退一次」：stream=%d chat=%d", f.streamCalls, f.chatCalls)
	}
	joined := strings.Join(*got, "|")
	if !strings.Contains(joined, MaterialNote+":") || !strings.Contains(joined, "回退阻塞调用") {
		t.Fatalf("缺回退旁白，实收: %s", joined)
	}
}

// 只有 Chat 的替身（老 fake / 别的实现）不能因为流式化而崩，直接退回阻塞。
type chatOnly struct{ calls int }

func (c *chatOnly) Chat(ctx context.Context, sys, user string, jsonMode ...bool) (string, error) {
	c.calls++
	return "only-chat", nil
}

func TestChatWithMaterialDegradesWhenClientCannotStream(t *testing.T) {
	c := &chatOnly{}
	g := &Generator{llm: c}
	ctx := WithDelta(context.Background(), func(kind, text string) {})
	out, err := g.chatWithMaterial(ctx, "s", "u")
	if err != nil || out != "only-chat" || c.calls != 1 {
		t.Fatalf("不支持流式的客户端应静默退回：out=%q err=%v calls=%d", out, err, c.calls)
	}
}

// ---- 攒批器：这是「不会被自己刷死」的全部保障 ----

func TestRelayFirstFragmentGoesOutImmediately(t *testing.T) {
	// 首次必须立刻吐：屏幕上「多久出现第一个字」就是用户体感的全部。
	var got []string
	r := NewMaterialRelay(time.Hour, 1<<20, func(k, s string) { got = append(got, s) })
	r.Push(MaterialThink, "第一片")
	if len(got) != 1 || got[0] != "第一片" {
		t.Fatalf("首片没立刻吐: %#v", got)
	}
}

func TestRelayBuffersFastFragmentsThenFlushesTail(t *testing.T) {
	// 一帧一片会把浏览器刷死：高频小片必须先攒住，阶段边界再一次性吐干净。
	var got []string
	r := NewMaterialRelay(time.Hour, 10000, func(k, s string) { got = append(got, s) })
	r.Push(MaterialText, "首片") // 立刻吐
	r.Push(MaterialText, "A")
	r.Push(MaterialText, "B")
	r.Push(MaterialText, "C")
	if len(got) != 1 {
		t.Fatalf("高频小片被一片一吐了（会刷死前端）: %#v", got)
	}
	r.Flush()
	if len(got) != 2 || got[1] != "ABC" {
		t.Fatalf("Flush 没把尾巴攒成一批吐出: %#v", got)
	}
}

func TestRelayFlushesWhenBufferHitsMax(t *testing.T) {
	var got []string
	r := NewMaterialRelay(time.Hour, 10, func(k, s string) { got = append(got, s) })
	r.Push(MaterialText, "aaaaa") // 首片立刻
	r.Push(MaterialText, "bbbbbbbbbbbb") // 12 >= 10 立即吐
	if len(got) != 2 || got[1] != "bbbbbbbbbbbb" {
		t.Fatalf("超过 max 没吐出: %#v", got)
	}
}

func TestRelayFlushesAfterInterval(t *testing.T) {
	var got []string
	r := NewMaterialRelay(30*time.Millisecond, 1<<20, func(k, s string) { got = append(got, s) })
	r.Push(MaterialText, "首片")
	r.Push(MaterialText, "尾")
	if len(got) != 1 {
		t.Fatalf("间隔没到不该吐: %#v", got)
	}
	time.Sleep(40 * time.Millisecond)
	r.Push(MaterialText, "续")
	// 攒批语义：超过间隔后，是「下一次 Push 把积压连同新片段一起带出去」，
	// 不是「定时器到点自动吐」—— 所以这里是 "尾续"，不是 "续"。
	// 没有定时器是刻意的：训练里模型偶尔几秒才吐一个片段，为这几秒常驻一个
	// ticker 只会平白多烧 CPU 和帧；而阶段边界有 Flush 兜底，不会积压到看不见。
	if len(got) != 2 || got[1] != "尾续" {
		t.Fatalf("超过间隔后没吐出: %#v", got)
	}
}

func TestRelayIgnoresEmptyAndNilSafe(t *testing.T) {
	var n int
	r := NewMaterialRelay(time.Millisecond, 10, func(k, s string) { n++ })
	r.Push(MaterialText, "")
	if n != 0 {
		t.Fatalf("空片段不该产帧")
	}
	var nilRelay *MaterialRelay
	nilRelay.Push(MaterialText, "x") // 不能 panic
	nilRelay.Flush()
}

// 攒批必须按类别分开：思考和正文共用一个缓冲的话，两路交错会出现
// 「思考里夹着正文片段」，前端分不清。
func TestRelayKeepsKindsSeparate(t *testing.T) {
	var mu sync.Mutex
	got := map[string]string{}
	r := NewMaterialRelay(time.Hour, 1<<20, func(k, s string) {
		mu.Lock()
		got[k] += s
		mu.Unlock()
	})
	r.Push(MaterialThink, "t1")
	r.Push(MaterialText, "x1")
	r.Push(MaterialThink, "t2")
	r.Push(MaterialText, "x2")
	r.Flush()
	if got[MaterialThink] != "t1t2" || got[MaterialText] != "x1x2" {
		t.Fatalf("类别串台: %#v", got)
	}
}

// 材料接口本身的自证：WithDelta(nil) 不该塞进一个 nil 值（会让 deltaOf 返回非 nil 的
// 不可调用函数，等于「有一条空的流」）。
func TestWithDeltaNilIsNoop(t *testing.T) {
	ctx := WithDelta(context.Background(), nil)
	if deltaOf(ctx) != nil {
		t.Fatalf("nil 接收器被塞进 ctx 了")
	}
	if fmt.Sprint(deltaOf(context.Background())) != "<nil>" {
		t.Fatalf("空 ctx 应取不到接收器")
	}
}
