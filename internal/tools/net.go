package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HTTPConfig 是 http_request 工具的配置。
type HTTPConfig struct {
	Timeout     time.Duration
	MaxBody     int      // 读取上限（字节）
	MaxForModel int      // 回给模型的正文上限（字节）
	AllowHosts  []string // 内网白名单：精确主机名或 CIDR。默认空 = 一律禁止访问内网
}

// DefaultHTTPConfig 默认配置。
func DefaultHTTPConfig() HTTPConfig {
	return HTTPConfig{
		Timeout:     20 * time.Second,
		MaxBody:     2 << 20,
		MaxForModel: 32 << 10,
	}
}

// HTTPRequestTool 让模型调用外部 HTTP API（用户核心场景：对接外部数据中台）。
//
// 公网开放环境下必须防 SSRF：不拦的话任何人可用它探内网（127.0.0.1:8092 就是本服务自身）。
// 默认禁止访问环回/私网/链路本地（含云元数据 169.254.169.254）；确需访问内网时由管理员
// 用 SKILLFORGE_TOOL_HTTP_ALLOW 显式开白（支持主机名或 CIDR），并每次跳转都重新校验。
type HTTPRequestTool struct {
	cfg HTTPConfig
}

// NewHTTPRequestTool 构造工具。
func NewHTTPRequestTool(cfg HTTPConfig) *HTTPRequestTool {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultHTTPConfig().Timeout
	}
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = DefaultHTTPConfig().MaxBody
	}
	if cfg.MaxForModel <= 0 {
		cfg.MaxForModel = DefaultHTTPConfig().MaxForModel
	}
	return &HTTPRequestTool{cfg: cfg}
}

func (t *HTTPRequestTool) Name() string { return "http_request" }

func (t *HTTPRequestTool) Description() string {
	return "发起一次 HTTP 请求调用外部接口（GET/POST/PUT/DELETE），用于从外部系统或数据中台取数、调用第三方 API。" +
		"返回状态码、Content-Type 和响应正文（过长会被截断）。" +
		"限制：仅支持 http/https；出于安全考虑默认禁止访问内网地址（环回、10./172.16./192.168./169.254. 等）；超时 20 秒；响应最大 2MB。" +
		"拿到 JSON 后如果要做统计计算，把数据交给 run_python 处理，不要把大段原始数据直接抄进回答。"
}

func (t *HTTPRequestTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url":    map[string]any{"type": "string", "description": "完整 URL，含协议，例如 https://api.example.com/v1/items?page=1"},
			"method": map[string]any{"type": "string", "description": "HTTP 方法，默认 GET", "enum": []string{"GET", "POST", "PUT", "PATCH", "DELETE"}},
			"headers": map[string]any{
				"type":        "object",
				"description": "可选请求头，例如 {\"Authorization\": \"Bearer xxx\", \"Content-Type\": \"application/json\"}",
			},
			"body": map[string]any{"type": "string", "description": "可选请求体（字符串，POST/PUT 时常用 JSON 文本）"},
		},
		"required": []string{"url"},
	}
}

// Run 执行请求。
func (t *HTTPRequestTool) Run(ctx context.Context, args map[string]any) (Result, error) {
	rawURL := strings.TrimSpace(Str(args, "url"))
	if rawURL == "" {
		return Result{}, errors.New("url 参数为空")
	}
	method := strings.ToUpper(strings.TrimSpace(Str(args, "method")))
	if method == "" {
		method = http.MethodGet
	}
	if err := t.guard(rawURL); err != nil {
		return Result{}, err
	}

	bodyStr := Str(args, "body")
	var body io.Reader
	if bodyStr != "" {
		body = strings.NewReader(bodyStr)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return Result{}, fmt.Errorf("构造请求失败: %w", err)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "SkillForge-Agent/1.0")
	}
	if bodyStr != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range StrMap(args, "headers") {
		req.Header.Set(k, v)
	}

	cli := &http.Client{
		Timeout: t.cfg.Timeout,
		// 每一跳都重新校验，防止用 302 绕过初始检查打到内网
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("重定向次数过多")
			}
			return t.guard(r.URL.String())
		},
	}
	resp, err := cli.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(t.cfg.MaxBody)))
	if err != nil {
		return Result{}, fmt.Errorf("读取响应失败: %w", err)
	}
	ct := resp.Header.Get("Content-Type")
	text := Truncate(string(raw), t.cfg.MaxForModel)

	content := fmt.Sprintf("HTTP %d %s\nContent-Type: %s\n正文（%d 字节，已截断到 %d）:\n%s",
		resp.StatusCode, http.StatusText(resp.StatusCode), ct, len(raw), t.cfg.MaxForModel, text)
	return Result{
		Content: content,
		Display: fmt.Sprintf("%s %s → %d, %d 字节", method, shortHost(rawURL), resp.StatusCode, len(raw)),
	}, nil
}

// guard 校验目标地址：仅 http/https，且默认禁止内网。
func (t *HTTPRequestTool) guard(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("URL 不合法: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("只支持 http/https，收到 %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("URL 缺少主机名")
	}
	if hostAllowed(host, t.cfg.AllowHosts) {
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("域名解析失败 %s: %w", host, err)
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return fmt.Errorf("安全限制：%s 解析到内网地址 %s，已阻止。"+
				"如确需访问内网接口，请让管理员把它加入 SKILLFORGE_TOOL_HTTP_ALLOW 白名单", host, ip)
		}
	}
	return nil
}

func shortHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	p := u.Path
	if len(p) > 24 {
		p = p[:24] + "…"
	}
	return u.Host + p
}

// hostAllowed 判断主机是否在显式白名单内（支持精确主机名与其子域、CIDR）。
func hostAllowed(host string, allow []string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	for _, a := range allow {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if strings.Contains(a, "/") {
			if _, cidr, err := net.ParseCIDR(a); err == nil {
				if ip := net.ParseIP(h); ip != nil && cidr.Contains(ip) {
					return true
				}
			}
			continue
		}
		if h == a || strings.HasSuffix(h, "."+a) {
			return true
		}
	}
	return false
}

// isPublicIP 判断是否为可公开访问的地址（挡掉环回/私网/链路本地/组播/未指定）。
func isPublicIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	// IPv4 广播
	if ip4 := ip.To4(); ip4 != nil && ip4.Equal(net.IPv4bcast) {
		return false
	}
	return true
}
