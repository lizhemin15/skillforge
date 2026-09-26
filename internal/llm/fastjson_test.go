package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
//
// 2026-09-14 加的这一批守的是最阴的一层：**provider 会换，而关思考链的开关
// 是两族互不通用的**。旧线上（siliconflow / Qwen3.6-27B）认 enable_thinking=false、
// 无视 reasoning_effort；现线上（astron / astron-code-latest）**正好反过来** ——
// enable_thinking 收下、HTTP 200、然后默默无视，思考链把 max_tokens 吃光，
// content 回空串。全链路零报错，接口 200 + 空数组，功能等于不存在。
// 所以下面既有「两个开关都得带」的断言，也有「空 content 就放大预算」的兜底断言。

type fastSrv struct {
	mu        sync.Mutex
	bodies    []map[string]any
	status    func(n int, body map[string]any) int
	reply     func(n int) string
	reasoning func(n int) int
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
		st, rep, rsn := s.status, s.reply, s.reasoning
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
		// reasoning(n) > 0 表示「这次开关被 provider 无视了，预算全花在思考链上」。
		// 真实 provider 在这种情况下的指纹是 reasoning_tokens ≈ completion_tokens，
		// 而 content 是空串 —— 这里把两个数都摆成同一个值，模拟那个指纹。
		rt := 0
		if rsn != nil {
			rt = rsn(n)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(
			`{"choices":[{"message":{"content":%s}}],"usage":{"completion_tokens":%d,"completion_tokens_details":{"reasoning_tokens":%d}}}`,
			jsonStr(out), rt, rt)))
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

// 关掉思考链是这个调用能落进预算的唯一原因。
// 顺手守住 response_format 与 max_tokens：前者少一层围栏，后者防思考链吃满预算。
//
// 断言「两族开关都带」是核心：它们互不通用，少带哪一族就等于对那一族 provider
// 主动放弃（实测量级是 6s+ 超时 vs 1s 返回），而症状只是「推荐行老是那几句」。
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
		t.Fatalf("请求体没带 enable_thinking=false（Qwen 系那族 provider 的开关）：%v", b)
	}
	if v, ok := b["reasoning_effort"]; !ok || v != "none" {
		t.Fatalf("请求体没带 reasoning_effort=none（astron 那类 provider 只认这个，缺了必然返空）：%v", b)
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
//
// 摘的时候**只能摘非标准的那个**：reasoning_effort 是 OpenAI 官方字段，
// 留着它才救得回 astron 那种 provider。一起摘掉 = 重试等于白做（还是 6s 超时）。
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
	if v, ok := s.body(2)["reasoning_effort"]; !ok || v != "none" {
		t.Fatalf("重试时把 reasoning_effort 一起摘了 —— 它才是对 astron 那类 provider 管用的开关：%v", s.body(2))
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
// 同时它得能被 errors.Is 认出来：放大预算那条兜底就是靠这个哨兵触发的。
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
	if !errors.Is(err, errEmptyContent) {
		t.Fatalf("空 content 必须能被 errors.Is(err, errEmptyContent) 认出（否则兜底永远不触发）：%v", err)
	}
}

// 200 但 content 空 ⇒ 思考链把预算吃光了 ⇒ 放大预算（×4）再问一次。
// 这条兜底是给「两族开关都被无视」的 provider 的：慢，但比「推荐行永远只有
// 规则版」强。少了它，这类 provider 上动态推荐就是个永久摆设。
func TestFastJSON_EmptyContentRetriesWithBiggerBudget(t *testing.T) {
	s := &fastSrv{
		reasoning: func(n int) int {
			if n == 1 {
				return 600 // 第一次：预算全花在思考链上
			}
			return 0
		},
		reply: func(n int) string {
			if n == 1 {
				return "" // 空 content
			}
			return `{"chips":[{"label":"换个角度","send":"换个角度再写一版"}]}`
		},
	}
	srv := newFastSrv(t, s)
	defer srv.Close()

	got, err := fastClient(t, srv.URL).FastJSON(context.Background(), "sys", "user", 600)
	if err != nil {
		t.Fatalf("空 content 之后放大预算应能救回来，实际报错：%v", err)
	}
	if !strings.Contains(got, "换个角度") {
		t.Fatalf("救回来但内容不对：%q", got)
	}
	if s.count() != 2 {
		t.Fatalf("应恰好放大预算重试一次（共 2 次），实际 %d 次", s.count())
	}
	// 600 × 4 = 2400。预算是这条兜底唯一的杠杆，涨了才有机会让回答出现在思考之后。
	if mt, _ := s.body(2)["max_tokens"].(float64); mt != 2400 {
		t.Fatalf("重试没有放大预算（期望 max_tokens=2400）：%v", s.body(2)["max_tokens"])
	}
	// 放大预算那一次不能把开关弄丢，否则更慢（每次都得靠「想」出答案）。
	if v, ok := s.body(2)["reasoning_effort"]; !ok || v != "none" {
		t.Fatalf("放大预算那次把 reasoning_effort 丢了：%v", s.body(2))
	}
}

// 放大预算还空 ⇒ 放弃。不许无限重试：这条路的每一次尝试都可能是在为一个
// 不听话的 provider 烧钱，且调用方那边推荐行早就退回规则版了，再等也没人看。
func TestFastJSON_GivesUpAfterEmptyContentRetry(t *testing.T) {
	s := &fastSrv{
		reasoning: func(int) int { return 600 },
		reply:     func(int) string { return "" },
	}
	srv := newFastSrv(t, s)
	defer srv.Close()

	_, err := fastClient(t, srv.URL).FastJSON(context.Background(), "sys", "user", 600)
	if err == nil {
		t.Fatal("一直空 content 应该报错")
	}
	if s.count() != 2 {
		t.Fatalf("空 content 只该放大预算法一次（共 2 次请求），实际 %d 次", s.count())
	}
	if !errors.Is(err, errEmptyContent) {
		t.Fatalf("放弃时也要把「空 content」这个归因透出来：%v", err)
	}
}

// 放大预算要有天花板：误配一次不该烧掉一笔意外的账。
// 2000 × 4 = 8000 → 封顶 4096。
func TestFastJSON_EmptyRetryBudgetIsCapped(t *testing.T) {
	s := &fastSrv{
		reasoning: func(int) int { return 2000 },
		reply:     func(int) string { return "" },
	}
	srv := newFastSrv(t, s)
	defer srv.Close()

	_, _ = fastClient(t, srv.URL).FastJSON(context.Background(), "sys", "user", 2000)
	if s.count() != 2 {
		t.Fatalf("应放大预算法一次，实际 %d 次请求", s.count())
	}
	if mt, _ := s.body(2)["max_tokens"].(float64); mt != 4096 {
		t.Fatalf("放大预算没封顶（期望 4096，实际 %v）", s.body(2)["max_tokens"])
	}
}

// 预算已经在天花板上了 ⇒ 不再重试。否则「×4 再封顶」会变成每次多跑一趟白工。
func TestFastJSON_NoRetryWhenBudgetAlreadyAtCap(t *testing.T) {
	s := &fastSrv{
		reasoning: func(int) int { return 4096 },
		reply:     func(int) string { return "" },
	}
	srv := newFastSrv(t, s)
	defer srv.Close()

	_, err := fastClient(t, srv.URL).FastJSON(context.Background(), "sys", "user", 4096)
	if err == nil {
		t.Fatal("空 content 应该报错")
	}
	if s.count() != 1 {
		t.Fatalf("预算已在 4096 上限时不该再重试，实际 %d 次请求", s.count())
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
		// 控制台常把整条端点给用户，SDK 还会再拼一次 /chat/completions。
		// 不剥尾巴 → .../chat/completions/v1/chat/completions（实测讯飞maas 就是这么拼坏的）。
		"https://maas-api.cn-huabei-1.xf-yun.com/v2/chat/completions": "https://maas-api.cn-huabei-1.xf-yun.com/v2",
		"https://host/v1/chat/completions":                            "https://host/v1",
		// 版本段不只有 v1：智谱 /v4、讯飞 /v2、Google /v1beta。
		// 以前只认 /v1，遇到 /v2 会补成 .../v2/v1 —— 凭空多一层路径，上游只回 404。
		"https://api.siliconflow.cn/v2":        "https://api.siliconflow.cn/v2",
		"https://open.bigmodel.cn/api/paas/v4": "https://open.bigmodel.cn/api/paas/v4",
		"https://host/v1beta":                  "https://host/v1beta",
		"https://host/compatible-mode/v1":      "https://host/compatible-mode/v1",
		// 非版本段仍要补 /v1（保持旧行为）。
		"https://host/maas": "https://host/maas/v1",
		// 主机名里的 v1 不是版本段。
		"https://v1.example.com": "https://v1.example.com/v1",
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
