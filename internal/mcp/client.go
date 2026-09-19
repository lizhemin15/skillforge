// Package mcp 实现 MCP（Model Context Protocol）客户端的最小可用子集：
// initialize / tools/list / tools/call，走 Streamable HTTP 传输。
//
// 为什么自己写而不是引官方 SDK：
//  1. skillforge 要求离线一键部署，go.mod 里多一个依赖就要多 vendor 一坨，
//     而这里真正需要的只是「发 JSON-RPC、收 JSON-RPC」三个方法。
//  2. 传输层必须同时吃下 application/json 与 text/event-stream 两种响应，
//     自己写才能把这条逻辑收在一个文件里、被单测直接覆盖。
//
// 已实测服务器：DataToolbox 的 data-ontology MCP（/mcp，Bearer 鉴权，无状态）。
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ProtocolVersion 是我们声明的 MCP 协议版本。
// 服务器可以回一个它支持的版本（实测 DataToolbox 回同一个）。
const ProtocolVersion = "2025-06-18"

// Config 是一个 MCP 服务器的连接配置（与后台表单一一对应）。
type Config struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	URL        string `json:"url"`
	APIKey     string `json:"api_key"`
	Enabled    bool   `json:"enabled"`
	TimeoutSec int    `json:"timeout_sec"`
}

// Timeout 返回生效的超时时间（配错/未配就退回默认值，不要变成 0 秒自杀）。
func (c Config) Timeout() time.Duration {
	s := c.TimeoutSec
	if s <= 0 || s > 600 {
		s = 30
	}
	return time.Duration(s) * time.Second
}

// ServerInfo 是 initialize 握手拿到的服务器自述。
type ServerInfo struct {
	Name            string `json:"name"`
	Version         string `json:"version"`
	ProtocolVersion string `json:"protocol_version"`
}

// RemoteTool 是服务器暴露的一个工具（远端原名 + 入参 JSON Schema）。
type RemoteTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// CallResult 是一次 tools/call 的结果。
type CallResult struct {
	Text    string // 已把 content 数组里各段文字拼好的纯文本
	IsError bool   // 服务器自报的业务失败（协议层成功、业务层失败）
}

// Client 是单个 MCP 服务器的客户端。
//
// 并发安全：sessionID 在 initialize 时写入，之后只读，加锁只是为了让
// 「后台点测试连接」与「前台对话调工具」不会同时写同一字段。
type Client struct {
	cfg Config
	hc  *http.Client

	mu        sync.Mutex
	sessionID string
	info      ServerInfo
}

// NewClient 构造客户端。URL 为空直接报错，避免拿个空地址去发请求。
func NewClient(cfg Config) (*Client, error) {
	u := strings.TrimSpace(cfg.URL)
	if u == "" {
		return nil, errors.New("MCP 地址为空")
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return nil, fmt.Errorf("MCP 地址必须以 http:// 或 https:// 开头：%s", u)
	}
	cfg.URL = u
	return &Client{cfg: cfg, hc: &http.Client{Timeout: cfg.Timeout()}}, nil
}

// Info 返回握手得到的服务器自述（未握手时为零值）。
func (c *Client) Info() ServerInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

// Initialize 完成 MCP 握手，返回服务器自述。
func (c *Client) Initialize(ctx context.Context) (ServerInfo, error) {
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "skillforge", "version": "1.0.0"},
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := c.call(ctx, "initialize", params, &res); err != nil {
		return ServerInfo{}, err
	}
	info := ServerInfo{
		Name:            res.ServerInfo.Name,
		Version:         res.ServerInfo.Version,
		ProtocolVersion: res.ProtocolVersion,
	}
	c.mu.Lock()
	c.info = info
	c.mu.Unlock()

	// 按规范补一条 initialized 通知。服务器普遍回 202 无体；
	// 失败不影响后续调用（实测 DataToolbox 无状态，直接忽略即可），所以只做尽力而为。
	_ = c.notify(ctx, "notifications/initialized", map[string]any{})
	return info, nil
}

// ListTools 拉取全部工具（自动翻页）。
func (c *Client) ListTools(ctx context.Context) ([]RemoteTool, error) {
	var out []RemoteTool
	cursor := ""
	for page := 0; page < 50; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var res struct {
			Tools []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.call(ctx, "tools/list", params, &res); err != nil {
			return nil, err
		}
		for _, t := range res.Tools {
			if strings.TrimSpace(t.Name) == "" {
				continue
			}
			out = append(out, RemoteTool{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: t.InputSchema,
			})
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	return out, nil
}

// CallTool 调用一个远端工具，并把 content 数组拍平成纯文本。
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (CallResult, error) {
	if strings.TrimSpace(name) == "" {
		return CallResult{}, errors.New("工具名为空")
	}
	if args == nil {
		args = map[string]any{}
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &res); err != nil {
		return CallResult{}, err
	}
	parts := make([]string, 0, len(res.Content))
	for _, seg := range res.Content {
		if strings.TrimSpace(seg.Text) != "" {
			parts = append(parts, seg.Text)
		}
	}
	text := strings.Join(parts, "\n")
	if strings.TrimSpace(text) == "" {
		// 有些工具只回结构化数据不回 text：原样带上 JSON，
		// 否则模型看到的是空，会误判成"没查到"。
		if b, err := json.Marshal(res); err == nil {
			text = string(b)
		}
	}
	return CallResult{Text: text, IsError: res.IsError}, nil
}

// ---- JSON-RPC 传输层 ----

// rpcResp 是 JSON-RPC 2.0 响应信封。
type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// call 发一次请求并把 result 解到 out。
func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	env := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("序列化请求失败: %w", err)
	}
	msg, err := c.post(ctx, body)
	if err != nil {
		return err
	}
	if msg.Error != nil {
		return fmt.Errorf("MCP 错误 %d: %s", msg.Error.Code, msg.Error.Message)
	}
	if len(msg.Result) == 0 {
		return fmt.Errorf("MCP 响应缺少 result（method=%s）", method)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(msg.Result, out); err != nil {
		return fmt.Errorf("解析 MCP 响应失败（method=%s）: %w", method, err)
	}
	return nil
}

// notify 发一条通知（无 id、无响应体）。
func (c *Client) notify(ctx context.Context, method string, params any) error {
	env := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}
	_, err = c.post(ctx, body)
	return err
}

// post 发一次 Streamable HTTP 请求，返回解析好的 JSON-RPC 响应。
//
// 响应可能是两种形态，都要吃下：
//   - Content-Type: application/json  → 整个 body 就是一条响应
//   - Content-Type: text/event-stream → SSE，逐事件 data: 里是响应
func (c *Client) post(ctx context.Context, body []byte) (rpcResp, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return rpcResp{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	// 两个 Accept 缺一不可：只给 application/json 会被规范型服务器判 406。
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	if k := strings.TrimSpace(c.cfg.APIKey); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	c.mu.Lock()
	sid := c.sessionID
	c.mu.Unlock()
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return rpcResp{}, fmt.Errorf("连接 MCP 失败: %w", err)
	}
	defer resp.Body.Close()

	if sid2 := resp.Header.Get("Mcp-Session-Id"); sid2 != "" {
		c.mu.Lock()
		c.sessionID = sid2
		c.mu.Unlock()
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return rpcResp{}, httpErr(resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	// 202/204：通知已被接收，没有响应体。
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		return rpcResp{}, nil
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "text/event-stream") {
		return parseSSE(resp.Body)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return rpcResp{}, fmt.Errorf("读取 MCP 响应失败: %w", err)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return rpcResp{}, nil
	}
	var msg rpcResp
	if err := json.Unmarshal(trimmed, &msg); err != nil {
		return rpcResp{}, fmt.Errorf("MCP 响应不是合法 JSON: %w（原文: %.200s）", err, trimmed)
	}
	return msg, nil
}

// httpErr 把 HTTP 状态码翻成人能看懂的提示（后台点"测试连接"时会直接显示）。
func httpErr(code int, snippet string) error {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("鉴权失败（HTTP %d）：请在管理后台核对该 MCP 服务的 API Key。%s", code, snippet)
	case http.StatusNotFound:
		return fmt.Errorf("地址不存在（HTTP 404）：请确认 MCP 地址路径正确（常见为 /mcp）。%s", snippet)
	case http.StatusMethodNotAllowed:
		return fmt.Errorf("该地址不接受 POST（HTTP 405）：确认填写的是 MCP 端点而不是普通网页地址。%s", snippet)
	}
	if snippet == "" {
		return fmt.Errorf("MCP 服务返回 HTTP %d", code)
	}
	return fmt.Errorf("MCP 服务返回 HTTP %d：%s", code, snippet)
}

// parseSSE 从事件流里取第一条带 result/error 的 JSON-RPC 消息。
//
// 不依赖具体 event 名（规范只约定 message，实测服务器也常省略），
// 只要 data: 里的 JSON 能认出是响应就算数。
func parseSSE(r io.Reader) (rpcResp, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	var data []string
	var lastErr error

	flush := func() (rpcResp, bool) {
		if len(data) == 0 {
			return rpcResp{}, false
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		if strings.TrimSpace(payload) == "" {
			return rpcResp{}, false
		}
		var msg rpcResp
		if err := json.Unmarshal([]byte(payload), &msg); err != nil {
			// 事件流里可能夹着非 JSON 的心跳/注释，记下不致命
			lastErr = fmt.Errorf("SSE 事件不是合法 JSON: %w", err)
			return rpcResp{}, false
		}
		if msg.Error != nil || len(msg.Result) > 0 {
			return msg, true
		}
		return rpcResp{}, false
	}

	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			continue
		}
		if strings.TrimSpace(line) == "" {
			if msg, ok := flush(); ok {
				return msg, nil
			}
		}
	}
	if err := sc.Err(); err != nil {
		return rpcResp{}, fmt.Errorf("读取 SSE 失败: %w", err)
	}
	if msg, ok := flush(); ok {
		return msg, nil
	}
	if lastErr != nil {
		return rpcResp{}, lastErr
	}
	return rpcResp{}, errors.New("SSE 流结束但没收到 JSON-RPC 响应")
}
