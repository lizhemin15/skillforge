package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lizhemin15/skillforge/internal/tools"
)

// base_url 该不该带 /v1 是纯人为约定，不该让用户踩坑：两种都试。
func TestModelEndpointCandidates(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"https://api.deepseek.com", []string{"https://api.deepseek.com/v1/models", "https://api.deepseek.com/models"}},
		{"https://api.deepseek.com/", []string{"https://api.deepseek.com/v1/models", "https://api.deepseek.com/models"}},
		{"https://api.siliconflow.cn/v1", []string{"https://api.siliconflow.cn/v1/models"}},
		{"https://api.siliconflow.cn/v1/", []string{"https://api.siliconflow.cn/v1/models"}},
		{"https://x.com/v1/models", []string{"https://x.com/v1/models"}},
		{"", nil},
		{"   ", nil},
	}
	for _, c := range cases {
		got := modelEndpointCandidates(c.in)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("modelEndpointCandidates(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// 编辑已保存的服务时，表单里显示的是掩码；把掩码当 key 发出去必然 401。
func TestIsMaskedKey(t *testing.T) {
	masked := []string{"", "   ", "••••••••", "sk-abcd…wxyz", "****"}
	for _, k := range masked {
		if !isMaskedKey(k) {
			t.Errorf("isMaskedKey(%q) 应为 true", k)
		}
	}
	real := []string{"sk-1234567890abcdef", "Bearer x", "abc"}
	for _, k := range real {
		if isMaskedKey(k) {
			t.Errorf("isMaskedKey(%q) 应为 false", k)
		}
	}
}

// 各家 provider 的模型列表形态不统一，解析必须宽容，否则换个服务就全空。
func TestModelNamesFromJSON(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"openai 标准", `{"object":"list","data":[{"id":"gpt-4o","object":"model"},{"id":"gpt-4o-mini"}]}`, []string{"gpt-4o", "gpt-4o-mini"}},
		{"硅基流动", `{"data":[{"id":"Qwen/Qwen3.6-27B"}]}`, []string{"Qwen/Qwen3.6-27B"}},
		{"纯字符串数组", `["a","b"]`, []string{"a", "b"}},
		{"models+name", `{"models":[{"name":"qwen"}]}`, []string{"qwen"}},
		{"data 是字符串数组", `{"data":["m1","m2"]}`, []string{"m1", "m2"}},
		{"裸对象就是模型", `{"id":"only-one","object":"model"}`, []string{"only-one"}},
		// 顶层 id 与列表同时存在时，不能把列表 id 误当成一个模型名
		{"顶层 id + data", `{"id":"list-1","data":[{"id":"m1"}]}`, []string{"m1"}},
		{"去重", `{"data":[{"id":"x"},{"id":"x"}]}`, []string{"x"}},
		{"unknown 键也钻", `{"result":{"items":[{"model":"mm"}]}}`, []string{"mm"}},
		{"非法 JSON", `not json at all`, nil},
		{"空对象", `{}`, nil},
	}
	for _, c := range cases {
		got := modelNamesFromJSON([]byte(c.body))
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRedactSecretAndClipBody(t *testing.T) {
	if s := redactSecret("key=sk-abcdefghijklmn is bad", "sk-abcdefghijklmn"); strings.Contains(s, "sk-abcdefghijklmn") {
		t.Errorf("凭据应被抹掉，实际: %s", s)
	}
	// 太短的串不替换，避免把正常文本整段吃掉
	if s := redactSecret("abc", "a"); s != "abc" {
		t.Errorf("短串不应替换，实际: %s", s)
	}
	long := strings.Repeat("x", 500)
	if s := clipBody([]byte(long)); len([]rune(s)) > 302 {
		t.Errorf("长正文应被截断，实际长度 %d", len([]rune(s)))
	}
	if s := clipBody(nil); s != "(空响应)" {
		t.Errorf("空正文提示不对: %q", s)
	}
}

// 地址由用户任意填写，暴露面等同 http_request 工具：默认必须拒绝内网。
func TestListProviderModelsBlocksInternalAddress(t *testing.T) {
	a := &Admin{} // toolAllow 为空 = 默认策略
	body := `{"base_url":"http://127.0.0.1:9999","api_key":"sk-abcdefghijklmn"}`
	rr := httptest.NewRecorder()
	a.ListProviderModels(rr, httptest.NewRequest(http.MethodPost, "/api/admin/llms/models", strings.NewReader(body)))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("访问内网地址应被拒（502），实际 %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "内网") {
		t.Fatalf("应给出被 SSRF 防护拦下的说明，实际: %s", rr.Body.String())
	}
	// 没填地址应给出可操作的提示，而不是去发请求
	rr2 := httptest.NewRecorder()
	a.ListProviderModels(rr2, httptest.NewRequest(http.MethodPost, "/api/admin/llms/models",
		strings.NewReader(`{"base_url":"","api_key":"sk-abcdefghijklmn"}`)))
	if rr2.Code != http.StatusBadRequest {
		t.Fatalf("空地址应 400，实际 %d", rr2.Code)
	}
	// key 缺失且没给 id，不该硬发请求
	rr3 := httptest.NewRecorder()
	a.ListProviderModels(rr3, httptest.NewRequest(http.MethodPost, "/api/admin/llms/models",
		strings.NewReader(`{"base_url":"https://api.deepseek.com","api_key":""}`)))
	if rr3.Code != http.StatusBadRequest {
		t.Fatalf("缺 key 应 400，实际 %d: %s", rr3.Code, rr3.Body.String())
	}
}

// 重定向是绕开 SSRF 防护的经典手法：白名单放行的是首个地址，302 之后仍须逐跳复检。
// 漏掉复检时，这里会变成「请求失败」（打不通 10.255.255.1）而不是「安全限制」。
func TestGuardedGetRechecksEachRedirectHop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.255.255.1/v1/models", http.StatusFound)
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_, _, err = tools.GuardedGet(ctx, srv.URL+"/v1/models", nil, []string{u.Hostname()}, 5*time.Second, 0)
	if err == nil {
		t.Fatal("302 到内网地址必须被拦下")
	}
	if !strings.Contains(err.Error(), "内网") {
		t.Fatalf("应为 SSRF 防护错误（含「内网」），实际: %v", err)
	}
}

// 端到端：开白环回后，handler 应能真的把 provider 的清单解析出来（含鉴权头与候选端点）。
func TestListProviderModelsParsesProviderList(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" { // 确认候选端点拼接正确
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]string{{"id": "Qwen/Qwen3.6-27B"}, {"id": "deepseek-chat"}},
		})
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	a := &Admin{toolAllow: []string{u.Hostname()}}

	rr := httptest.NewRecorder()
	a.ListProviderModels(rr, httptest.NewRequest(http.MethodPost, "/api/admin/llms/models",
		strings.NewReader(`{"base_url":"`+srv.URL+`","api_key":"sk-abcdefghijklmn"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("应成功，实际 %d: %s", rr.Code, rr.Body.String())
	}
	if gotAuth != "Bearer sk-abcdefghijklmn" {
		t.Fatalf("鉴权头不对: %q", gotAuth)
	}
	var got struct {
		Models   []string `json:"models"`
		Count    int      `json:"count"`
		Endpoint string   `json:"endpoint"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Count != 2 || len(got.Models) != 2 {
		t.Fatalf("应解析出 2 个模型，实际 %+v", got)
	}
	if !strings.HasSuffix(got.Endpoint, "/v1/models") {
		t.Fatalf("endpoint 不对: %s", got.Endpoint)
	}
	// 响应里绝不能回显 key
	if strings.Contains(rr.Body.String(), "sk-abcdefghijklmn") {
		t.Fatal("响应体里回显了 api_key")
	}
}
