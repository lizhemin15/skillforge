package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lizhemin15/skillforge/internal/model"
)

// probeSeen 记录假上游收到的东西。断言必须基于「真的发了一次 HTTP」这个事实，
// 而不是基于我们自己算出来的地址字符串 —— 后者只能证明代码自洽，证明不了打对了地方。
type probeSeen struct {
	path string
	body map[string]any
}

func newProbeServer(t *testing.T, status int, body string) (*httptest.Server, *probeSeen) {
	t.Helper()
	seen := &probeSeen{body: map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&seen.body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

const probeOKBody = `{"choices":[{"message":{"role":"assistant","content":"正常"}}]}`

// TestProbeEndpointMatchesRuntime 守的是整个「测试」按钮的可信度。
//
// base_url 用的是「整条粘进来的完整端点」—— 平台控制台给的就是整条，
// 例如讯飞maas 的 https://maas-api.cn-huabei-1.xf-yun.com/v2/chat/completions，
// 这类地址以前会被拼成 .../v2/chat/completions/v1/chat/completions（运行期真在打这个）。
//
// 两条判据：
//  1. 实际打出去的路径必须正好是 /v2/chat/completions —— 不多一层也不许少一层；
//  2. 回执里报给用户的地址必须等于实际打出去的地址。
//
// 两者一旦不一致，就会出现「测试通过但聊天打不通」（或反过来），
// 按钮立刻从「省时间」变成「骗人」—— 而它存在的意义正是让用户不必去猜。
func TestProbeEndpointMatchesRuntime(t *testing.T) {
	srv, seen := newProbeServer(t, 200, probeOKBody)
	c := New(&model.LLMConfig{
		BaseURL: srv.URL + "/v2/chat/completions",
		Model:   "xopqwen36v35b",
		APIKey:  "sk-test-1234",
	})

	res := c.Probe(context.Background())

	if want := "/v2/chat/completions"; seen.path != want {
		t.Fatalf("实际请求路径 = %q，期望 %q（路径段多一层就是上游 404 的来源）", seen.path, want)
	}
	if want := srv.URL + "/v2/chat/completions"; res.Endpoint != want {
		t.Errorf("回执里的地址 %q 与实际打出去的 %q 不一致 —— 用户会照着错的地址去排查", res.Endpoint, want)
	}
	if !res.OK || res.Status != http.StatusOK {
		t.Fatalf("应判连通，实际 ok=%v status=%d msg=%s", res.OK, res.Status, res.Message)
	}
	if res.Reply != "正常" {
		t.Errorf("reply = %q，期望「正常」", res.Reply)
	}
	if got := seen.body["model"]; got != "xopqwen36v35b" {
		t.Errorf("上游收到的 model = %v，期望 xopqwen36v35b", got)
	}
	// 内网 token 慢，按一次按钮不该让用户等一整篇文章：必须是小预算。
	if mt, _ := seen.body["max_tokens"].(float64); mt <= 0 || mt > 64 {
		t.Errorf("max_tokens = %v，期望 1..64 之间的小预算", seen.body["max_tokens"])
	}
}

// TestProbeEmptyContentStillOK 是判定口径的守卫，别把它改成「有正文才算通」。
//
// 内网思考型模型很常见：HTTP 200，但 max_tokens 被思考链吃光、content 为空。
// 这是回答质量/预算问题，不是连通性问题。若判成红，用户会看到
// 「配置明明是好的，但按钮说它坏了」，于是整排假红 —— 那比没有按钮更糟：
// 从此没人信这个按钮，真正坏的配置也一起被忽略。
func TestProbeEmptyContentStillOK(t *testing.T) {
	cases := []struct{ name, body string }{
		{"content 为空", `{"choices":[{"message":{"role":"assistant","content":""}}]}`},
		{"choices 为空数组", `{"choices":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newProbeServer(t, 200, tc.body)
			res := New(&model.LLMConfig{BaseURL: srv.URL + "/v1", Model: "m", APIKey: "sk-test-1234"}).
				Probe(context.Background())
			if !res.OK || res.Status != http.StatusOK {
				t.Fatalf("HTTP 200 就该判连通，实际 ok=%v status=%d msg=%s", res.OK, res.Status, res.Message)
			}
			if !strings.Contains(res.Message, "连通正常") {
				t.Errorf("结论文案应说明连通正常，实际：%s", res.Message)
			}
		})
	}
}

// TestProbeUpstreamErrorsAreAttributed：四种错的用户观感一样（对话里没反应），
// 但该做的事完全不同（换 key / 充值 / 开授权 / 改模型名 / 改地址）。
// 所以每个状态码都必须给出「该动哪一项」+ 上游原话（用户要拿原话去问客服）。
func TestProbeUpstreamErrorsAreAttributed(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		body         string
		wantCode     string
		wantWord     string
		wantUpstream string // 上游原话必须被带出来，不能只给我们的猜测
	}{
		{"401 key 无效", 401, `{"error":{"message":"Invalid API key provided","code":"invalid_api_key"}}`,
			"invalid_api_key", "API Key 无效", "Invalid API key"},
		{"402 欠费", 402, `{"error":{"message":"Insufficient Balance","code":30001}}`,
			"30001", "余额", "Insufficient Balance"},
		{"403 无有效授权", 403, `{"error":{"message":"no valid authorization","code":11200}}`,
			"11200", "授权", "no valid authorization"},
		// 网关回的不是 OpenAI 形状（HTML/纯文本）时也必须保住状态码与归因。
		{"404 地址不对", 404, `<html>404 page not found</html>`, "", "地址不对", "404 page not found"},
		{"429 限流", 429, `{"error":{"message":"rate limit exceeded"}}`, "", "限流", "rate limit"},
		// 讯飞maas 实测原话：它要的是「路由名」，不是模型本名。
		{"400 路由名不对", 400, `{"error":{"message":"no category route found","code":10404}}`,
			"10404", "路由名", "no category route found"},
		{"400 泛化", 400, `{"error":{"message":"model not exist"}}`, "", "模型名", "model not exist"},
		{"422 参数不合法", 422, `{"error":{"message":"unprocessable entity"}}`, "", "参数不合法", "unprocessable"},
		{"503 上游故障", 503, `{"error":{"message":"upstream busy"}}`, "", "上游故障", "upstream busy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newProbeServer(t, tc.status, tc.body)
			res := New(&model.LLMConfig{BaseURL: srv.URL + "/v1", Model: "m", APIKey: "sk-test-1234"}).
				Probe(context.Background())

			if res.OK {
				t.Fatalf("HTTP %d 不该判连通（msg=%s）", tc.status, res.Message)
			}
			if res.Status != tc.status {
				t.Errorf("status = %d，期望 %d（状态码必须原样透出，它是用户查平台的凭据）", res.Status, tc.status)
			}
			if tc.wantCode != "" && res.APICode != tc.wantCode {
				t.Errorf("api_code = %q，期望 %q", res.APICode, tc.wantCode)
			}
			if !strings.Contains(res.Message, tc.wantWord) {
				t.Errorf("归因里应出现 %q（用户靠它知道该改什么），实际：%s", tc.wantWord, res.Message)
			}
			if !strings.Contains(res.Message, tc.wantUpstream) {
				t.Errorf("应带出上游原话 %q，实际：%s", tc.wantUpstream, res.Message)
			}
			// 地址也要带上：内网常有多套网关，用户得知道我们打的是哪一台。
			if !strings.Contains(res.Message, res.Endpoint) {
				t.Errorf("归因里应包含实际地址 %q，实际：%s", res.Endpoint, res.Message)
			}
		})
	}
}

// TestProbeLocalPrecheck：缺 key / 缺模型名要在本地就说清，别打出去换一个 401 回来 ——
// 401 的归因是「key 无效」，会把「根本没填」误导成「填错了」。
func TestProbeLocalPrecheck(t *testing.T) {
	t.Run("没填 key", func(t *testing.T) {
		res := New(&model.LLMConfig{BaseURL: "http://127.0.0.1:1/v1", Model: "m"}).Probe(context.Background())
		if res.OK || !strings.Contains(res.Message, "API Key") {
			t.Fatalf("应在本地提示缺 API Key，实际 ok=%v msg=%s", res.OK, res.Message)
		}
		if res.Status != 0 {
			t.Errorf("没发请求就不该有状态码，实际 %d", res.Status)
		}
	})
	t.Run("没填模型名", func(t *testing.T) {
		res := New(&model.LLMConfig{BaseURL: "http://127.0.0.1:1/v1", APIKey: "sk-test-1234"}).Probe(context.Background())
		if res.OK || !strings.Contains(res.Message, "模型名") {
			t.Fatalf("应在本地提示缺模型名，实际 ok=%v msg=%s", res.OK, res.Message)
		}
	})
}

// TestProbeTimeoutReported：地址不通时最常见的长相就是「一直没回应」。
// 这条要确认两件事：判成不通、并且把耗时量出来（用户靠它区分「秒回被拒」和「根本连不上」）。
func TestProbeTimeoutReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		_, _ = w.Write([]byte(probeOKBody))
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	res := New(&model.LLMConfig{BaseURL: srv.URL + "/v1", Model: "m", APIKey: "sk-test-1234"}).Probe(ctx)

	if res.OK {
		t.Fatal("超时应判不通")
	}
	if !strings.Contains(res.Message, "没等到响应") {
		t.Errorf("应说明是「没等到响应」，实际：%s", res.Message)
	}
	if res.LatencyMS < 50 {
		t.Errorf("耗时 = %dms，应与上下文超时量级相符（否则用户无法区分秒回被拒 vs 连不上）", res.LatencyMS)
	}
}

// TestProbeNeverLeaksKey：个别网关会把请求头抄进错误消息，而这条回执要显示在管理端、
// 还会被用户复制去问客服。key 是配置里唯一不能外流的东西。
func TestProbeNeverLeaksKey(t *testing.T) {
	const key = "sk-super-secret-abcdef123456"
	srv, _ := newProbeServer(t, 401, `{"error":{"message":"invalid key: Bearer `+key+`","code":401}}`)
	res := New(&model.LLMConfig{BaseURL: srv.URL + "/v1", Model: "m", APIKey: key}).
		Probe(context.Background())

	if strings.Contains(res.Message, key) {
		t.Fatalf("回执里出现了 key 原文：%s", res.Message)
	}
	if !strings.Contains(res.Message, "***") {
		t.Errorf("应把 key 擦成 ***，实际：%s", res.Message)
	}
}

// TestChatEndpointIsWhatRuntimeUses：ChatEndpoint 报的必须是 New() 归一化后的地址 ——
// 它是「测试」按钮和「聊天」两条链路共用的同一处拼法，分开写就会各自漂移。
func TestChatEndpointIsWhatRuntimeUses(t *testing.T) {
	cases := map[string]string{
		"https://api.deepseek.com":                     "https://api.deepseek.com/v1/chat/completions",
		"https://api.siliconflow.cn/v1":                "https://api.siliconflow.cn/v1/chat/completions",
		"https://maas.example.com/v2/chat/completions": "https://maas.example.com/v2/chat/completions",
	}
	for in, want := range cases {
		got := New(&model.LLMConfig{BaseURL: in, Model: "m", APIKey: "k"}).ChatEndpoint()
		if got != want {
			t.Errorf("ChatEndpoint(%q) = %q，期望 %q", in, got, want)
		}
	}
}
