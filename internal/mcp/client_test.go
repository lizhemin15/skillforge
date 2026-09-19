package mcp

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, f *fakeMCP, key string) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c, err := NewClient(Config{Name: "假服务", URL: srv.URL, APIKey: key, TimeoutSec: 5})
	if err != nil {
		t.Fatalf("NewClient 失败: %v", err)
	}
	return c, srv
}

// 握手要带上鉴权、协议版本，并且 Accept 必须同时包含 json 与事件流。
// 少任何一条，规范型服务端都会直接 406/401——这是接第三方 MCP 最常见的坑。
func TestInitialize_SendsAuthAndProtocolHeaders(t *testing.T) {
	f := &fakeMCP{wantKey: "k-123"}
	c, _ := newTestClient(t, f, "k-123")

	info, err := c.Initialize(context.Background())
	if err != nil {
		t.Fatalf("Initialize 失败: %v", err)
	}
	if info.Name != "data-ontology" || info.Version != "1.0.0" {
		t.Fatalf("服务器自述解析错: %+v", info)
	}
	if info.ProtocolVersion != ProtocolVersion {
		t.Fatalf("协议版本应为 %s，实际 %s", ProtocolVersion, info.ProtocolVersion)
	}
	_, methods, auth, _, proto, accept := f.snapshot()
	if len(methods) == 0 || methods[0] != "initialize" {
		t.Fatalf("首个方法应为 initialize，实际 %v", methods)
	}
	if len(auth) == 0 || auth[0] != "k-123" {
		t.Fatalf("Authorization 未正确携带: %v", auth)
	}
	if len(proto) == 0 || proto[0] != ProtocolVersion {
		t.Fatalf("MCP-Protocol-Version 未携带: %v", proto)
	}
	if len(accept) == 0 || !strings.Contains(accept[0], "application/json") || !strings.Contains(accept[0], "text/event-stream") {
		t.Fatalf("Accept 必须同时含 json 与事件流，实际 %v", accept)
	}
}

// 事件流回包必须能解析——很多 MCP 服务端（含 JS 实现）默认走 SSE。
func TestInitialize_SSETransport(t *testing.T) {
	f := &fakeMCP{sse: true}
	c, _ := newTestClient(t, f, "")

	info, err := c.Initialize(context.Background())
	if err != nil {
		t.Fatalf("SSE 握手失败: %v", err)
	}
	if info.Name != "data-ontology" {
		t.Fatalf("SSE 下服务器自述解析错: %+v", info)
	}
}

// 翻页：漏掉 nextCursor 会静默只剩第一页工具，表现是"工具莫名其妙少了几个"。
func TestListTools_FollowsPagination(t *testing.T) {
	f := &fakeMCP{sse: true, pageSize: 1}
	c, _ := newTestClient(t, f, "")

	if _, err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize 失败: %v", err)
	}
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools 失败: %v", err)
	}
	if len(tools) != 3 {
		t.Fatalf("应翻页取到 3 个工具，实际 %d 个: %+v", len(tools), tools)
	}
	if tools[1].Name != "execute_sql" {
		t.Fatalf("工具顺序/内容错: %+v", tools)
	}
	schema := tools[1].InputSchema
	if schema["type"] != "object" {
		t.Fatalf("inputSchema 未透传: %+v", schema)
	}
	if _, ok := schema["required"]; !ok {
		t.Fatalf("inputSchema.required 丢失: %+v", schema)
	}
}

// 传参不能被吞：参数丢了模型会以为工具"不听话"。
func TestCallTool_PassesArgumentsThrough(t *testing.T) {
	f := &fakeMCP{}
	c, _ := newTestClient(t, f, "")

	res, err := c.CallTool(context.Background(), "execute_sql", map[string]any{
		"db": "sales", "sql": "select 1",
	})
	if err != nil {
		t.Fatalf("CallTool 失败: %v", err)
	}
	if res.IsError {
		t.Fatalf("不该被判定为错误: %+v", res)
	}
	if !strings.Contains(res.Text, `"db":"sales"`) || !strings.Contains(res.Text, `"sql":"select 1"`) {
		t.Fatalf("参数没有原样送达远端: %q", res.Text)
	}
	if len(f.calls) != 1 || f.calls[0] != "execute_sql" {
		t.Fatalf("远端收到的工具名不对: %v", f.calls)
	}
}

// 业务错误的文字必须原样带出来：模型要靠它自我纠正（比如改 SQL 语法）。
func TestCallTool_MarksBusinessError(t *testing.T) {
	f := &fakeMCP{toolErr: "broken_tool"}
	c, _ := newTestClient(t, f, "")

	res, err := c.CallTool(context.Background(), "broken_tool", nil)
	if err != nil {
		t.Fatalf("isError 不该变成传输层错误: %v", err)
	}
	if !res.IsError {
		t.Fatalf("isError 未被识别: %+v", res)
	}
	if !strings.Contains(res.Text, "SQL 语法错误") {
		t.Fatalf("业务错误文字丢了: %q", res.Text)
	}
}

// 401 的提示必须落到"去后台核对 API Key"，否则管理员只会看到一串英文。
func TestAuthFailure_MentionsAPIKey(t *testing.T) {
	f := &fakeMCP{wantKey: "right", errCode: 0}
	c, _ := newTestClient(t, f, "wrong")

	_, err := c.Initialize(context.Background())
	if err == nil {
		t.Fatal("key 不对却握手成功了")
	}
	// 两条都要查：
	//  - "API Key" 是给管理员的指路；
	//  - "鉴权失败" 是**本实现自己**加的话。
	// 只查前者是假绿：假服务端的响应原文里就有 "API Key"（真实 MCP 服务端也常这么写），
	// 于是 httpErr 里那段友好提示整个删掉，这条用例照样绿 —— 它证明不了任何东西。
	if !strings.Contains(err.Error(), "API Key") {
		t.Fatalf("401 提示没指路到 API Key: %v", err)
	}
	if !strings.Contains(err.Error(), "鉴权失败") {
		t.Fatalf("401 没落到本实现的人话提示（把原文裸抛给管理员了）: %v", err)
	}
}

// 有状态服务端会下发 Mcp-Session-Id，后续请求必须回传，否则服务端按规范回 404。
func TestSessionID_Propagated(t *testing.T) {
	f := &fakeMCP{sessionID: "sess-abc"}
	c, _ := newTestClient(t, f, "")

	if _, err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize 失败: %v", err)
	}
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatalf("带会话的 tools/list 失败（会话头没回传）: %v", err)
	}
	_, _, _, sess, _, _ := f.snapshot()
	var found bool
	for _, s := range sess {
		if s == "sess-abc" {
			found = true
		}
	}
	if !found {
		t.Fatalf("从未回传 Mcp-Session-Id: %v", sess)
	}
}

// 地址校验要在建客户端时就拦住，别等发起请求才报错。
func TestNewClient_RejectsBadURL(t *testing.T) {
	cases := []string{"", "   ", "127.0.0.1:8080/mcp", "ftp://x/mcp"}
	for _, in := range cases {
		if _, err := NewClient(Config{URL: in}); err == nil {
			t.Fatalf("地址 %q 应该被拒绝", in)
		}
	}
}

// 超时必须有兜底，否则后台点"测试连接"会挂死在页面上。
func TestConfig_DefaultTimeout(t *testing.T) {
	if d := (Config{}).Timeout(); d != 30*time.Second {
		t.Fatalf("默认超时应为 30s，实际 %v", d)
	}
	if d := (Config{TimeoutSec: 3}).Timeout(); d != 3*time.Second {
		t.Fatalf("配置超时未生效: %v", d)
	}
}

// 事件流里夹心跳/注释是常态，不能被当成解析失败。
func TestParseSSE_IgnoresNonJSONEvents(t *testing.T) {
	body := "event: ping\ndata: not-json-at-all\n\n" +
		"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"ok\":true}}\n\n"
	msg, err := parseSSE(strings.NewReader(body))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(msg.Result) == 0 {
		t.Fatalf("没取到 result: %+v", msg)
	}
}

// JSON-RPC 层的 error 必须冒泡成 Go error，否则调用方会拿到空结果当成功。
func TestCallTool_JSONRPCError(t *testing.T) {
	f := &fakeMCP{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c, _ := NewClient(Config{URL: srv.URL, TimeoutSec: 5})

	if _, err := c.CallTool(context.Background(), "", nil); err == nil {
		t.Fatal("空工具名应报错")
	}
}
