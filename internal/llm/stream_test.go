package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lizhemin15/skillforge/internal/model"
)

// 这一组用例守的是「点了发送之后几十秒没反应」这件事的两条腿：
//
//   A. 关思考链真的发出去了 —— 线上活跃模型是 reasoning 模型，同一段提示词实测
//      带思考 63.0s、关思考 4.8s。开关没发出去（或者发错族）就退化成几十秒静默，
//      而且**没有任何报错**：HTTP 200、JSON 合法、答案也对，只是慢。
//   B. 思考链和正文必须分开走 —— reasoning_content 是模型自己嘟囔的过程，混进
//      正文会污染用户的稿子（那是要下载成 Word 的东西）；反过来，正文被当材料
//      显示会重复一遍。两者是一体的两条断言。

type streamSrv struct {
	mu     sync.Mutex
	bodies []map[string]any
	frames func(n int, body map[string]any) (int, string) // 返回状态码与 SSE 体
}

func newStreamSrv(t *testing.T, s *streamSrv) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		n := len(s.bodies)
		s.mu.Unlock()

		code, out := http.StatusOK, defaultFrames()
		if s.frames != nil {
			code, out = s.frames(n, body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(out))
	}))
}

// defaultFrames：先推两片思考链，再推两片正文，最后 [DONE]。
func defaultFrames() string {
	return strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"先看手册要求，"}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"这是一份通知。"}}]}`,
		`data: {"choices":[{"delta":{"content":"关于开展"}}]}`,
		`data: {"choices":[{"delta":{"content":"数据治理的通知"}}]}`,
		`data: {"choices":[{"delta":{}}],"usage":{"total_tokens":9}}`,
		`data: [DONE]`,
		``,
	}, "\n\n")
}

func streamClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return New(&model.LLMConfig{BaseURL: srv.URL, APIKey: "k", Model: "test-model"})
}

func (s *streamSrv) bodyAt(t *testing.T, i int) map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.bodies) {
		t.Fatalf("只发生了 %d 次请求，取不到第 %d 次", len(s.bodies), i+1)
	}
	return s.bodies[i]
}

// A. 关思考链：两族开关都得带（Qwen 认 enable_thinking，astron 认 reasoning_effort）。
func TestStreamChatSendsBothThinkKnobs(t *testing.T) {
	srv := &streamSrv{}
	ts := newStreamSrv(t, srv)
	defer ts.Close()

	if _, err := streamClient(t, ts).StreamChat(context.Background(), "sys", "user", StreamOpts{DisableThinking: true}); err != nil {
		t.Fatalf("StreamChat 失败: %v", err)
	}
	body := srv.bodyAt(t, 0)
	if v, ok := body["enable_thinking"]; !ok || v != false {
		t.Fatalf("关思考链必须带 enable_thinking=false，实际 %v", body["enable_thinking"])
	}
	if v, ok := body["reasoning_effort"]; !ok || v != "none" {
		t.Fatalf("关思考链必须带 reasoning_effort=none，实际 %v", body["reasoning_effort"])
	}
	if body["stream"] != true {
		t.Fatalf("必须是流式请求，实际 stream=%v", body["stream"])
	}
}

// 执笔那一跳**不能**被顺手关掉思考链：那是质量来源，关掉等于悄悄降质。
func TestStreamChatLeavesThinkingOnByDefault(t *testing.T) {
	srv := &streamSrv{}
	ts := newStreamSrv(t, srv)
	defer ts.Close()

	if _, err := streamClient(t, ts).StreamChat(context.Background(), "sys", "user", StreamOpts{JSONMode: true}); err != nil {
		t.Fatalf("StreamChat 失败: %v", err)
	}
	body := srv.bodyAt(t, 0)
	if _, ok := body["enable_thinking"]; ok {
		t.Fatal("默认调用不该带 enable_thinking（执笔要保留思考链）")
	}
	if _, ok := body["reasoning_effort"]; ok {
		t.Fatal("默认调用不该带 reasoning_effort（执笔要保留思考链）")
	}
	if rf, ok := body["response_format"].(map[string]any); !ok || rf["type"] != "json_object" {
		t.Fatalf("JSONMode 必须带 response_format=json_object，实际 %v", body["response_format"])
	}
}

// 严格校验未知字段的网关（Azure/部分自建）会 400：摘掉 enable_thinking 再试一次。
func TestStreamChatRetriesWithoutEnableThinkingOn400(t *testing.T) {
	srv := &streamSrv{
		frames: func(n int, body map[string]any) (int, string) {
			if _, ok := body["enable_thinking"]; ok {
				return http.StatusBadRequest, `{"error":{"message":"unknown field enable_thinking"}}`
			}
			return http.StatusOK, defaultFrames()
		},
	}
	ts := newStreamSrv(t, srv)
	defer ts.Close()

	out, err := streamClient(t, ts).StreamChat(context.Background(), "sys", "user", StreamOpts{DisableThinking: true})
	if err != nil {
		t.Fatalf("400 后应当摘字段重试并成功，实际 %v", err)
	}
	if out != "关于开展数据治理的通知" {
		t.Fatalf("重试后正文不对: %q", out)
	}
	if len(srv.bodies) != 2 {
		t.Fatalf("应当恰好两次请求，实际 %d 次", len(srv.bodies))
	}
	if _, ok := srv.bodyAt(t, 1)["enable_thinking"]; ok {
		t.Fatal("第二次必须摘掉 enable_thinking")
	}
	if v := srv.bodyAt(t, 1)["reasoning_effort"]; v != "none" {
		t.Fatalf("摘字段重试时 reasoning_effort 必须留着（它才是真管用的那条），实际 %v", v)
	}
}

// B. 思考链与正文分流：material 里只有 reasoning、正文里只有 content。
func TestStreamChatSeparatesReasoningFromContent(t *testing.T) {
	srv := &streamSrv{}
	ts := newStreamSrv(t, srv)
	defer ts.Close()

	var reasons, contents []string
	out, err := streamClient(t, ts).StreamChat(context.Background(), "sys", "user", StreamOpts{
		OnReasoning: func(s string) { reasons = append(reasons, s) },
		OnContent:   func(s string) { contents = append(contents, s) },
	})
	if err != nil {
		t.Fatalf("StreamChat 失败: %v", err)
	}
	if out != "关于开展数据治理的通知" {
		t.Fatalf("正文拼接不对: %q", out)
	}
	if strings.Contains(out, "先看手册要求") {
		t.Fatalf("思考链漏进正文了（会被下载到用户的 Word 里）: %q", out)
	}
	if got := strings.Join(reasons, ""); got != "先看手册要求，这是一份通知。" {
		t.Fatalf("材料片段不对: %q", got)
	}
	if got := strings.Join(contents, ""); got != "关于开展数据治理的通知" {
		t.Fatalf("正文片段不对: %q", got)
	}
}

// 坏帧/心跳帧不能让整轮作废：provider 的 usage 尾帧与空 delta 都不是标准 delta。
func TestStreamChatSurvivesNoiseFrames(t *testing.T) {
	srv := &streamSrv{
		frames: func(int, map[string]any) (int, string) {
			return http.StatusOK, strings.Join([]string{
				": keep-alive",
				`data: {"choices":[]}`,
				`data: not-json-at-all`,
				`data: {"choices":[{"delta":{"content":"正文"}}]}`,
				`data: [DONE]`,
				``,
			}, "\n\n")
		},
	}
	ts := newStreamSrv(t, srv)
	defer ts.Close()

	out, err := streamClient(t, ts).StreamChat(context.Background(), "sys", "user", StreamOpts{DisableThinking: true})
	if err != nil {
		t.Fatalf("噪声帧不该让调用失败: %v", err)
	}
	if out != "正文" {
		t.Fatalf("正文应当照常拼出来，实际 %q", out)
	}
}

// 5xx 要打上「瞬时故障」标记，让上层能决定重试；否则会被当成永久失败直接报给用户。
func TestStreamChatMarksTransientOn5xx(t *testing.T) {
	srv := &streamSrv{
		frames: func(int, map[string]any) (int, string) {
			return http.StatusBadGateway, `{"error":"upstream hiccup"}`
		},
	}
	ts := newStreamSrv(t, srv)
	defer ts.Close()

	_, err := streamClient(t, ts).StreamChat(context.Background(), "sys", "user", StreamOpts{DisableThinking: true})
	if err == nil {
		t.Fatal("502 必须报错")
	}
	var te *TransientError
	if !errors.As(err, &te) {
		t.Fatalf("502 应当被标记成瞬时故障，实际 %v", err)
	}
}

// 没配 Key 时不发请求，直接给人话（与 Chat/Complete 的守卫一致）。
func TestStreamChatWithoutKey(t *testing.T) {
	c := New(&model.LLMConfig{BaseURL: "http://127.0.0.1:1", Model: "m"})
	if _, err := c.StreamChat(context.Background(), "s", "u", StreamOpts{}); err == nil {
		t.Fatal("没配 API Key 必须报错")
	}
}

// ===========================================================================
// 「流断在半路」这一组（R1 + R2）
//
// 守的是两种真实故障形态，它们的共同点是**HTTP 200、请求成功、没有 5xx**，
// 所以旧代码一条都没拦住：
//
//	R1 上游把连接挂住：答案流完之后既不关连接、也不再给字节。旧代码的 Read 一直
//	   阻塞，只能等 streamHTTPClient 的 10 分钟整体超时——线上实测一整轮 616.5s
//	   （600s 超时 + 16.5s 重试）。用户的原话就是「一直卡着计时」。
//	R2 上游流到一半就断：EOF 到了，但既没有 finish_reason 也没有 [DONE]。旧代码
//	   把已经收到的半截正文当结果返回，分类跳拿它去 json.Unmarshal，报出来的是
//	   「模型输出不是合法json」（线上 raw_out="{\""），把用户和运维都带向错的病。
//
// 两条腿的判据都必须在**没有 5xx** 的情况下成立，否则等于没测。
// ===========================================================================

type hangSrv struct {
	mu   sync.Mutex
	reqs int
	head string
}

// newHangSrv 起一个「把 head 吐完之后既不关连接、也不再给字节」的假上游，
// 精确复刻线上那一轮 616.5s 的形态。等客户端自己断开（看门狗会替我们断开）。
func newHangSrv(t *testing.T, s *hangSrv) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.reqs++
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(s.head))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
}

func (s *hangSrv) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqs
}

// R1-a：上游挂住 + 没有结束标记 ⇒ 必须在秒级（不是十分钟级）判故障，
// 而且**绝不能**把已经收到的半截正文当结果返回。
func TestStreamChatMarksStalledWhenUpstreamHangs(t *testing.T) {
	t.Setenv("SKILLFORGE_STREAM_IDLE_SEC", "1")
	srv := &hangSrv{head: strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"半截正文"}}]}`,
		``,
	}, "\n\n")}
	ts := newHangSrv(t, srv)
	defer ts.Close()

	start := time.Now()
	out, err := streamClient(t, ts).StreamChat(context.Background(), "sys", "user", StreamOpts{DisableThinking: true})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("上游挂住且没有结束标记时必须报错，绝不能把半截正文当结果（拿到 %q）", out)
	}
	if !IsStreamBroken(err) {
		t.Fatalf("必须被标记成「流断在半路」，实际 %v", err)
	}
	var te *TransientError
	if !errors.As(err, &te) {
		t.Fatalf("应当是可重试的瞬时故障，实际 %v", err)
	}
	if elapsed > 8*time.Second {
		t.Fatalf("看门狗 1s 就该动手，实际等了 %s —— 说明只剩 10 分钟整体超时在兜底（线上 600s 就是这么来的）", elapsed)
	}
	// 第一次挂住 → 当场重试一次 → 又挂住 → 放弃。两次，不多不少。
	if n := srv.requests(); n != 2 {
		t.Fatalf("流断在半路应当当场重试恰好一次（共 2 次请求），实际 %d 次", n)
	}
}

// R1-b：上游挂住**但已经给了结束标记** ⇒ 回答本身是完整的，按成功返回。
// 这一条防的是「修过头」：判据只看终止信号，不看连接有没有被好好关掉。
func TestStreamChatReturnsContentWhenUpstreamStallsAfterDone(t *testing.T) {
	t.Setenv("SKILLFORGE_STREAM_IDLE_SEC", "1")
	for _, tc := range []struct {
		name string
		head string
	}{
		{"带 finish_reason", strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"完整正文"},"finish_reason":"stop"}]}`,
			``,
		}, "\n\n")},
		{"带 DONE", strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"完整正文"}}]}`,
			`data: [DONE]`,
			``,
		}, "\n\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &hangSrv{head: tc.head}
			ts := newHangSrv(t, srv)
			defer ts.Close()

			out, err := streamClient(t, ts).StreamChat(context.Background(), "sys", "user", StreamOpts{DisableThinking: true})
			if err != nil {
				t.Fatalf("已经给了结束标记，挂住的只是连接，不该判失败: %v", err)
			}
			if out != "完整正文" {
				t.Fatalf("正文应当完整返回，实际 %q", out)
			}
			if n := srv.requests(); n != 1 {
				t.Fatalf("这种情况下不需要重试，应当只有 1 次请求，实际 %d 次", n)
			}
		})
	}
}

// R2-a：EOF 到了但没有结束标记 ⇒ 判不完整，半截正文绝不返回给调用方。
func TestStreamChatRejectsTruncatedStream(t *testing.T) {
	srv := &streamSrv{
		frames: func(int, map[string]any) (int, string) {
			// 注意：没有 finish_reason，也没有 [DONE]，然后连接就关了。
			return http.StatusOK, strings.Join([]string{
				`data: {"choices":[{"delta":{"content":"{\""}}]}`,
				``,
			}, "\n\n")
		},
	}
	ts := newStreamSrv(t, srv)
	defer ts.Close()

	out, err := streamClient(t, ts).StreamChat(context.Background(), "sys", "user", StreamOpts{DisableThinking: true})
	if err == nil {
		t.Fatalf("没有结束标记的流必须判错，绝不能把半截正文（%q）交给 json.Unmarshal", out)
	}
	if !errors.Is(err, ErrStreamTruncated) {
		t.Fatalf("应当判成「流不完整」，实际 %v", err)
	}
	if out != "" {
		t.Fatalf("半截正文必须被丢弃，实际返回了 %q", out)
	}
	if n := len(srv.bodies); n != 2 {
		t.Fatalf("截断应当当场重试恰好一次（共 2 次请求），实际 %d 次", n)
	}
}

// R2-b：第一次被截断、第二次完整 ⇒ 用户拿到的是完整正文，不该因为一次抖动失败。
func TestStreamChatRetriesTruncatedStreamOnce(t *testing.T) {
	srv := &streamSrv{
		frames: func(n int, _ map[string]any) (int, string) {
			if n == 1 {
				return http.StatusOK, `data: {"choices":[{"delta":{"content":"半截"}}]}` + "\n\n"
			}
			return http.StatusOK, defaultFrames()
		},
	}
	ts := newStreamSrv(t, srv)
	defer ts.Close()

	out, err := streamClient(t, ts).StreamChat(context.Background(), "sys", "user", StreamOpts{DisableThinking: true})
	if err != nil {
		t.Fatalf("截断重试一次后应当成功，实际 %v", err)
	}
	if out != "关于开展数据治理的通知" {
		t.Fatalf("应当返回第二次的完整正文，实际 %q", out)
	}
	if n := len(srv.bodies); n != 2 {
		t.Fatalf("应当恰好两次请求，实际 %d 次", n)
	}
}

// 关思考链时再带一道 thinking_budget：有的 provider 对 enable_thinking 和
// reasoning_effort 都免疫（关了照想），那时预算就是唯一的闸。
func TestStreamChatSendsDisabledThinkingBudget(t *testing.T) {
	srv := &streamSrv{}
	ts := newStreamSrv(t, srv)
	defer ts.Close()

	if _, err := streamClient(t, ts).StreamChat(context.Background(), "sys", "user", StreamOpts{DisableThinking: true}); err != nil {
		t.Fatalf("StreamChat 失败: %v", err)
	}
	if v := srv.bodyAt(t, 0)["thinking_budget"]; v != float64(disabledThinkingBudget) {
		t.Fatalf("关思考链时必须带 thinking_budget=%d，实际 %v", disabledThinkingBudget, v)
	}
}

// 执笔跳（要思考链）的思考预算：**默认就掐**（1024），要「不限」得显式写 0。
//
// 这条测试守的是默认值的**方向**，不是某个数字。方向搞反的代价线上实测过：
// 默认不掐 = 首片正文前空转 226.7s（用户投诉「一直卡着计时」就是这么来的）。
func TestStreamChatWritingThinkBudgetDefaultIsBounded(t *testing.T) {
	srv := &streamSrv{}
	ts := newStreamSrv(t, srv)
	defer ts.Close()
	c := streamClient(t, ts)

	// 默认（不设环境变量）= 真带 1024，而不是「什么都不带」。
	if _, err := c.StreamChat(context.Background(), "sys", "user", StreamOpts{}); err != nil {
		t.Fatalf("StreamChat 失败: %v", err)
	}
	if v := srv.bodyAt(t, 0)["thinking_budget"]; v != float64(1024) {
		t.Fatalf("不设 SKILLFORGE_THINK_BUDGET 时该用默认 1024（线上实测这一档又快又长），实际 %v", v)
	}

	// 显式 0 = 不限，且必须是**真不带参数**（不是带 0 —— 有些网关会把它当非法值报错）
	t.Setenv("SKILLFORGE_THINK_BUDGET", "0")
	if _, err := c.StreamChat(context.Background(), "sys", "user", StreamOpts{}); err != nil {
		t.Fatalf("StreamChat 失败: %v", err)
	}
	if v, ok := srv.bodyAt(t, 1)["thinking_budget"]; ok {
		t.Fatalf("显式设 0 是「我要不限」的意思，请求体不该带 thinking_budget，实际 %v", v)
	}

	// 非法值回落到默认，不静默变成「不限」——静默变不限就是把 226 秒又还回去了
	t.Setenv("SKILLFORGE_THINK_BUDGET", "不是数字")
	if _, err := c.StreamChat(context.Background(), "sys", "user", StreamOpts{}); err != nil {
		t.Fatalf("StreamChat 失败: %v", err)
	}
	if v := srv.bodyAt(t, 2)["thinking_budget"]; v != float64(1024) {
		t.Fatalf("非法值必须回落到默认 1024，实际 %v（回落成「不限」是最坏的静默降级）", v)
	}

	t.Setenv("SKILLFORGE_THINK_BUDGET", "2048")
	if _, err := c.StreamChat(context.Background(), "sys", "user", StreamOpts{}); err != nil {
		t.Fatalf("StreamChat 失败: %v", err)
	}
	if v := srv.bodyAt(t, 3)["thinking_budget"]; v != float64(2048) {
		t.Fatalf("配了 SKILLFORGE_THINK_BUDGET=2048 就必须带上，实际 %v", v)
	}
}
