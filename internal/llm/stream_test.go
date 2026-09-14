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
