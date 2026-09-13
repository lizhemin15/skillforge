package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/lizhemin15/skillforge/internal/model"
)

// 这一组用例守的是「推荐行的延迟预算能不能成立」。
// 它们全是「静默降级」型：FastJSON 挂了，接口回 200 + 空数组，前端退回规则版，
// 页面上没有任何异常 —— 只有模型账单和「动态推荐怎么老是那几句」能看出来。

type fastSrv struct {
	mu     sync.Mutex
	bodies []map[string]any
	status func(n int, body map[string]any) int
	reply  func(n int) string
}

func newFastSrv(t *testing.T, s *fastSrv) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		n := len(s.bodies)
		st, rep := s.status, s.reply
		s.mu.Unlock()

		code := http.StatusOK
		if st != nil {
			code = st(n, body)
		}
		if code != http.StatusOK {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":{"message":"unknown field enable_thinking"}}`))
			return
		}
		out := `{"chips":[{"label":"再精简一版","send":"把刚才那版精简到 300 字"}]}`
		if rep != nil {
			out = rep(n)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + jsonStr(out) + `}}],"usage":{"completion_tokens":28}}`))
	}))
}

// jsonStr 把字符串变成合法的 JSON 字面量（重名会撞标准库 strconv，所以叫 jsonStr）。
func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func fastClient(t *testing.T, url string) *Client {
	t.Helper()
	return New(&model.LLMConfig{BaseURL: url, APIKey: "test-key", Model: "fake-model"})
}

func (s *fastSrv) body(n int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > len(s.bodies) {
		return nil
	}
	return s.bodies[n-1]
}

func (s *fastSrv) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

// 关掉思考链是这个调用能落进 5s 预算的唯一原因（实测 6.24s → 1.38s）。
// 顺手守住 response_format 与 max_tokens：前者少一层围栏，后者防思考链吃满预算。
func TestFastJSON_SendsKnobsThatKeepItFast(t *testing.T) {
	s := &fastSrv{}
	srv := newFastSrv(t, s)
	defer srv.Close()

	got, err := fastClient(t, srv.URL).FastJSON(context.Background(), "sys", "user", 600)
	if err != nil {
		t.Fatalf("FastJSON 失败：%v", err)
	}
	if !strings.Contains(got, "再精简一版") {
		t.Fatalf("返回内容没透出来：%q", got)
	}
	b := s.body(1)
	if v, ok := b["enable_thinking"]; !ok || v != false {
		t.Fatalf("请求体没带 enable_thinking=false（思考链关不掉 → 线上必然超时）：%v", b)
	}
	rf, _ := b["response_format"].(map[string]any)
	if rf == nil || rf["type"] != "json_object" {
		t.Fatalf("请求体没要求 json_object：%v", b)
	}
	if mt, _ := b["max_tokens"].(float64); mt != 600 {
		t.Fatalf("请求体 max_tokens 不是 600：%v", b)
	}
}

// 严格网关（不认识 enable_thinking 会 400）也必须能用：摘掉该字段重试一次。
// 没有这条重试，换 provider 就等于功能消失，而表现只是「推荐行老是那几句」。
func TestFastJSON_RetriesWithoutKnobOn400(t *testing.T) {
	s := &fastSrv{status: func(n int, body map[string]any) int {
		if _, ok := body["enable_thinking"]; ok {
			return http.StatusBadRequest
		}
		return http.StatusOK
	}}
	srv := newFastSrv(t, s)
	defer srv.Close()

	got, err := fastClient(t, srv.URL).FastJSON(context.Background(), "sys", "user", 600)
	if err != nil {
		t.Fatalf("400 之后应摘掉 enable_thinking 重试成功，实际报错：%v", err)
	}
	if !strings.Contains(got, "再精简一版") {
		t.Fatalf("重试成功但内容不对：%q", got)
	}
	if s.count() != 2 {
		t.Fatalf("应恰好重试一次（共 2 次请求），实际 %d 次", s.count())
	}
	if _, ok := s.body(2)["enable_thinking"]; ok {
		t.Fatalf("重试时仍带着 enable_thinking（重试等于白做）：%v", s.body(2))
	}
	// 别的字段不能因为重试而丢：思考链可以不要，json_object / max_tokens 不行。
	if rf, _ := s.body(2)["response_format"].(map[string]any); rf == nil || rf["type"] != "json_object" {
		t.Fatalf("重试把 response_format 弄丢了：%v", s.body(2))
	}
	if mt, _ := s.body(2)["max_tokens"].(float64); mt != 600 {
		t.Fatalf("重试把 max_tokens 弄丢了：%v", s.body(2))
	}
}

// 5xx / 429 必须打上瞬时标记（调用方按同一套判据决定「这次别推荐了」还是「重试」）。
func TestFastJSON_TransientOn5xx(t *testing.T) {
	s := &fastSrv{status: func(int, map[string]any) int { return http.StatusServiceUnavailable }}
	srv := newFastSrv(t, s)
	defer srv.Close()

	_, err := fastClient(t, srv.URL).FastJSON(context.Background(), "sys", "user", 600)
	if err == nil {
		t.Fatal("503 应该报错")
	}
	if !IsTransient(err) {
		t.Fatalf("503 应被标成瞬时错误（否则会被当成本地 bug 反复排查）：%v", err)
	}
}

// 400 且摘掉 knob 仍然 400 → 不该无限重试，也不该把错误吞掉。
func TestFastJSON_SecondFailureSurfaces(t *testing.T) {
	s := &fastSrv{status: func(int, map[string]any) int { return http.StatusBadRequest }}
	srv := newFastSrv(t, s)
	defer srv.Close()

	_, err := fastClient(t, srv.URL).FastJSON(context.Background(), "sys", "user", 600)
	if err == nil {
		t.Fatal("两次 400 应该报错")
	}
	if s.count() != 2 {
		t.Fatalf("400 只该重试一次（防重试风暴），实际 %d 次", s.count())
	}
	if !strings.Contains(err.Error(), "LLM HTTP 400") {
		t.Fatalf("错误里应带上状态码，实际：%v", err)
	}
}

// 空 content 的错误必须说清「空」而不是「格式坏」：前者是 max_tokens 被思考链
// 吃掉，后者是解析器的问题 —— 两种修法完全不同（线上踩过一次，排查方向全错）。
func TestFastJSON_EmptyContentExplainsReasoningBudget(t *testing.T) {
	s := &fastSrv{reply: func(int) string { return "" }}
	srv := newFastSrv(t, s)
	defer srv.Close()

	_, err := fastClient(t, srv.URL).FastJSON(context.Background(), "sys", "user", 600)
	if err == nil {
		t.Fatal("空 content 应该报错")
	}
	if !strings.Contains(err.Error(), "思考链") {
		t.Fatalf("空 content 的报错要指向思考链/预算，实际：%v", err)
	}
}

// BaseURL 归一化与 Chat 共用一处：两条路各写一份的话，某天只改一条，
// 同一个 provider 在两条路上会打到不同地址。
func TestNormalizeBaseURL(t *testing.T) {
	cases := map[string]string{
		"":                              "https://api.openai.com/v1",
		"https://api.deepseek.com":      "https://api.deepseek.com/v1",
		"https://api.deepseek.com/":     "https://api.deepseek.com/v1",
		"https://api.siliconflow.cn/v1": "https://api.siliconflow.cn/v1",
		"https://x.com/v1/":             "https://x.com/v1",
		"  https://y.com/v1  ":          "https://y.com/v1",
	}
	for in, want := range cases {
		if got := normalizeBaseURL(in); got != want {
			t.Errorf("normalizeBaseURL(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// 没配 key 时必须在本地就报错（不能打出去一个 401 才发现）。
func TestFastJSON_NoKeyFailsFast(t *testing.T) {
	c := New(&model.LLMConfig{BaseURL: "http://127.0.0.1:1/v1", Model: "m"})
	if _, err := c.FastJSON(context.Background(), "s", "u", 100); err == nil {
		t.Fatal("没配 API Key 应该报错")
	}
}
