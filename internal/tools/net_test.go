package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// SSRF 是公网开放工具的头号风险：不拦的话任何人都能借它探内网
// （127.0.0.1:8092 就是本服务自身，169.254.169.254 是云元数据）。
func TestGuardBlocksInternalTargets(t *testing.T) {
	tool := NewHTTPRequestTool(DefaultHTTPConfig())
	cases := []struct{ url, why string }{
		{"http://127.0.0.1:8092/api/version", "环回"},
		{"http://localhost:8092/", "localhost 域名"},
		{"http://10.0.0.5/x", "私网 10/8"},
		{"http://192.168.1.1/x", "私网 192.168/16"},
		{"http://172.16.3.4/x", "私网 172.16/12"},
		{"http://169.254.169.254/latest/meta-data/", "云元数据（链路本地）"},
		{"http://[::1]:9000/", "IPv6 环回"},
		{"file:///etc/passwd", "非 http 协议"},
		{"http:///nohost", "缺主机名"},
		{"gopher://127.0.0.1:70/", "危险协议"},
	}
	for _, c := range cases {
		if err := tool.guard(c.url); err == nil {
			t.Errorf("%s (%s) 应被拦截，但放行了", c.url, c.why)
		}
	}
}

func TestGuardAllowsPublicTargets(t *testing.T) {
	tool := NewHTTPRequestTool(DefaultHTTPConfig())
	for _, u := range []string{"https://example.com/x", "http://1.1.1.1/", "https://api.deepseek.com/v1"} {
		if err := tool.guard(u); err != nil {
			t.Errorf("%s 是公网地址，不该拦: %v", u, err)
		}
	}
}

// 内网接口（用户的数据中台）必须能通过显式白名单放行，否则核心场景不可用。
func TestGuardAllowlistUnblocksInternal(t *testing.T) {
	cfg := DefaultHTTPConfig()
	cfg.AllowHosts = []string{"127.0.0.1", "10.20.0.0/16", "datacenter.internal"}
	tool := NewHTTPRequestTool(cfg)

	ok := []string{
		"http://127.0.0.1:8092/api/version",
		"http://10.20.30.40:8080/api",
		"http://datacenter.internal/query",
		"http://api.datacenter.internal/query", // 子域也应放行
	}
	for _, u := range ok {
		if err := tool.guard(u); err != nil {
			t.Errorf("白名单内 %s 应放行: %v", u, err)
		}
	}
	// 白名单不该变成「全放开」：范围内的其他内网地址仍要拦
	if err := tool.guard("http://10.21.0.1/x"); err == nil {
		t.Error("10.21.0.1 不在 10.20.0.0/16 内，应仍被拦截")
	}
	if err := tool.guard("http://192.168.0.1/x"); err == nil {
		t.Error("192.168.0.1 未在白名单，应仍被拦截")
	}
}

func TestHTTPRequestSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true,"v":1}`))
	}))
	defer srv.Close()

	cfg := DefaultHTTPConfig()
	cfg.AllowHosts = []string{"127.0.0.1"} // 测试服务器在环回，必须开白
	tool := NewHTTPRequestTool(cfg)

	res, err := tool.Run(context.Background(), map[string]any{"url": srv.URL, "method": "get"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, `"ok":true`) || !strings.Contains(res.Content, "HTTP 200") {
		t.Fatalf("结果应包含状态码与正文: %s", res.Content)
	}
	if !strings.Contains(res.Display, "200") {
		t.Fatalf("Display 应含状态码便于 trace: %s", res.Display)
	}
}

// 用 302 跳转到内网是经典绕过手法：每一跳都要重新校验。
func TestHTTPRequestBlocksRedirectToInternal(t *testing.T) {
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.255.255.1/secret", http.StatusFound)
	}))
	defer evil.Close()

	cfg := DefaultHTTPConfig()
	cfg.AllowHosts = []string{"127.0.0.1"}
	tool := NewHTTPRequestTool(cfg)

	_, err := tool.Run(context.Background(), map[string]any{"url": evil.URL})
	if err == nil {
		t.Fatal("跳转到内网必须失败")
	}
	if !strings.Contains(err.Error(), "内网") && !strings.Contains(err.Error(), "安全限制") {
		t.Fatalf("应给出安全拦截原因: %v", err)
	}
}

func TestHTTPRequestEmptyURL(t *testing.T) {
	tool := NewHTTPRequestTool(DefaultHTTPConfig())
	if _, err := tool.Run(context.Background(), map[string]any{}); err == nil {
		t.Fatal("空 url 必须报错")
	}
}

// 超长响应必须截断，否则会把模型上下文塞爆。
func TestHTTPRequestTruncatesHugeBody(t *testing.T) {
	big := strings.Repeat("A", 200000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()

	cfg := DefaultHTTPConfig()
	cfg.AllowHosts = []string{"127.0.0.1"}
	cfg.MaxForModel = 1024
	tool := NewHTTPRequestTool(cfg)

	res, err := tool.Run(context.Background(), map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) > 4096 {
		t.Fatalf("回给模型的正文应被截断，实际 %d 字节", len(res.Content))
	}
	if !strings.Contains(res.Content, "已截断") {
		t.Fatal("截断应有明确提示，让模型知道数据不完整")
	}
}
