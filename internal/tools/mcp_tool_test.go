package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/mcp"
)

// 假 MCP 服务端（tools 包专用，故意做得比 mcp 包那份简陋）：
// 这里要验证的是「适配与注册」而不是「传输解析」，传输已经有自己的用例了。
func fakeMCPServer(t *testing.T, tools []map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": mcp.ProtocolVersion,
				"serverInfo":      map[string]any{"name": "fake-dtb", "version": "9.9"},
			}
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			result = map[string]any{"tools": tools}
		case "tools/call":
			var p struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if p.Name == "boom" {
				result = map[string]any{"isError": true,
					"content": []map[string]any{{"type": "text", "text": "远端说：表不存在"}}}
			} else {
				result = map[string]any{"content": []map[string]any{{"type": "text", "text": "RESULT-OF-" + p.Name}}}
			}
		default:
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func stdTools() []map[string]any {
	return []map[string]any{
		{"name": "execute_sql", "description": "执行 SQL",
			"inputSchema": map[string]any{"type": "object",
				"properties": map[string]any{"sql": map[string]any{"type": "string"}},
				"required":   []any{"sql"}}},
		{"name": "boom", "description": "会失败的"},
	}
}

type fakeSource struct {
	cfgs []mcp.Config
	err  error
}

func (f fakeSource) ListMCPServers() ([]mcp.Config, error) { return f.cfgs, f.err }

// 工具名必须是 OpenAI 那一套合法标识符，且带前缀便于模型与人工辨认来源。
func TestMCPTool_LocalNameIsValidIdentifier(t *testing.T) {
	re := regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	cases := []struct{ alias, remote string }{
		{"datatoolbox", "execute_sql"},
		{"中文服务名", "execute_sql"},
		{"srv", "工具名也是中文"},
		{"very-long-alias-name-here", strings.Repeat("x", 80)},
	}
	for _, c := range cases {
		got := mcpLocalName(c.alias, c.remote)
		if !re.MatchString(got) {
			t.Fatalf("工具名非法: %q（alias=%q remote=%q）", got, c.alias, c.remote)
		}
		if !strings.HasPrefix(got, "mcp_") {
			t.Fatalf("工具名缺 mcp_ 前缀: %q", got)
		}
		if len(got) > 64 {
			t.Fatalf("工具名超长: %d", len(got))
		}
	}
}

// 同名工具来自不同服务器时不能撞车，否则后挂的会静默覆盖先挂的。
func TestMCPTool_DifferentServersDoNotCollide(t *testing.T) {
	a := mcpLocalName("dtb", "execute_sql")
	b := mcpLocalName("crm", "execute_sql")
	if a == b {
		t.Fatalf("两个服务器的同名工具撞车了: %q", a)
	}
}

// 没写 inputSchema 的工具必须补成合法空对象 schema，
// 否则模型端会因 schema 非法直接 400，整个对话都发不出去。
func TestMCPTool_SchemaAlwaysValid(t *testing.T) {
	tool := NewMCPTool("假服务", "dtb", mcp.RemoteTool{Name: "boom"}, func(context.Context, map[string]any) (mcp.CallResult, error) {
		return mcp.CallResult{}, nil
	})
	s := tool.Schema()
	if s["type"] != "object" {
		t.Fatalf("schema.type 必须是 object: %+v", s)
	}
	if _, ok := s["properties"]; !ok {
		t.Fatalf("schema.properties 必须存在: %+v", s)
	}
}

// 模型要靠描述里点名的服务器名来判断该不该用这个工具。
func TestMCPTool_DescriptionMentionsServer(t *testing.T) {
	tool := NewMCPTool("数据工具箱", "dtb", mcp.RemoteTool{Name: "execute_sql", Description: "执行 SQL"},
		func(context.Context, map[string]any) (mcp.CallResult, error) { return mcp.CallResult{}, nil })
	d := tool.Description()
	if !strings.Contains(d, "数据工具箱") || !strings.Contains(d, "执行 SQL") {
		t.Fatalf("描述缺少服务器名或原文: %q", d)
	}
}

// 远端业务失败要变成 Go error，这样工具轨迹里会标红，模型也能读到原因。
func TestMCPTool_RunSurfacesBusinessError(t *testing.T) {
	tool := NewMCPTool("假服务", "dtb", mcp.RemoteTool{Name: "boom"},
		func(context.Context, map[string]any) (mcp.CallResult, error) {
			return mcp.CallResult{Text: "远端说：表不存在", IsError: true}, nil
		})
	_, err := tool.Run(context.Background(), nil)
	if err == nil {
		t.Fatal("业务失败没有变成 error")
	}
	if !strings.Contains(err.Error(), "表不存在") {
		t.Fatalf("错误里丢了远端原文: %v", err)
	}
}

func TestMCPTool_RunReturnsText(t *testing.T) {
	tool := NewMCPTool("假服务", "dtb", mcp.RemoteTool{Name: "execute_sql"},
		func(context.Context, map[string]any) (mcp.CallResult, error) {
			return mcp.CallResult{Text: "RESULT-OF-execute_sql"}, nil
		})
	res, err := tool.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if !strings.Contains(res.Content, "RESULT-OF-execute_sql") {
		t.Fatalf("结果没有回传: %+v", res)
	}
}

// 禁用 / 删除服务器后，上一轮挂上去的工具必须消失。
// 这是最容易出僵尸工具的地方：工具还在列表里，但调不通。
func TestMCPManager_MountsAndUnmounts(t *testing.T) {
	srv := fakeMCPServer(t, stdTools())
	reg := NewRegistry()
	reg.Register(&fakeTool{name: "builtin_echo"})
	src := fakeSource{cfgs: []mcp.Config{{ID: "s1", Name: "服务一", URL: srv.URL, Enabled: true, TimeoutSec: 5}}}
	mgr := NewMCPManager(src, reg)

	sts := mgr.Refresh(context.Background())
	if len(sts) != 1 || !sts[0].OK {
		t.Fatalf("刷新后应连接成功: %+v", sts)
	}
	if sts[0].ToolCount != 2 || sts[0].Server != "fake-dtb" {
		t.Fatalf("状态不符: %+v", sts[0])
	}
	if _, ok := reg.Get("mcp_s1_execute_sql"); !ok {
		t.Fatalf("工具没挂进注册表，当前: %v", reg.Names())
	}
	if !strings.Contains(mgr.PromptHint(), "mcp_s1_execute_sql") {
		t.Fatalf("提示词段落没列出工具: %q", mgr.PromptHint())
	}

	// 关掉开关 → 工具必须立刻摘掉，内置工具不受影响。
	src.cfgs[0].Enabled = false
	sts = mgr.Refresh(context.Background())
	if sts[0].OK {
		t.Fatalf("禁用后不该是 OK: %+v", sts[0])
	}
	if _, ok := reg.Get("mcp_s1_execute_sql"); ok {
		t.Fatalf("禁用后工具仍在注册表（僵尸工具）: %v", reg.Names())
	}
	if _, ok := reg.Get("builtin_echo"); !ok {
		t.Fatalf("内置工具被误删: %v", reg.Names())
	}
	if mgr.PromptHint() != "" {
		t.Fatalf("没有可用工具时提示词段落应为空，实际 %q", mgr.PromptHint())
	}
}

// 一台挂掉不能拖垮其他台——内网里某台没起来是常态。
func TestMCPManager_OneBadServerDoesNotBreakOthers(t *testing.T) {
	good := fakeMCPServer(t, stdTools())
	reg := NewRegistry()
	src := fakeSource{cfgs: []mcp.Config{
		{ID: "bad", Name: "坏服务", URL: "http://127.0.0.1:1/mcp", Enabled: true, TimeoutSec: 1},
		{ID: "good", Name: "好服务", URL: good.URL, Enabled: true, TimeoutSec: 5},
	}}
	mgr := NewMCPManager(src, reg)

	sts := mgr.Refresh(context.Background())
	if len(sts) != 2 {
		t.Fatalf("应返回两台状态: %+v", sts)
	}
	var bad, ok bool
	for _, s := range sts {
		if s.ID == "bad" {
			bad = !s.OK && s.Error != ""
		}
		if s.ID == "good" {
			ok = s.OK && s.ToolCount == 2
		}
	}
	if !bad {
		t.Fatalf("坏服务应报错: %+v", sts)
	}
	if !ok {
		t.Fatalf("好服务应正常: %+v", sts)
	}
	if _, got := reg.Get("mcp_good_execute_sql"); !got {
		t.Fatalf("好服务的工具没挂上: %v", reg.Names())
	}
}

// 测试连接不能有副作用：只是点一下"测试"，不该把工具挂进生产注册表。
func TestTestMCP_NoSideEffects(t *testing.T) {
	srv := fakeMCPServer(t, stdTools())
	reg := NewRegistry()
	st := TestMCP(context.Background(), mcp.Config{ID: "x", Name: "服务", URL: srv.URL, TimeoutSec: 5})
	if !st.OK || st.ToolCount != 2 {
		t.Fatalf("探测结果不对: %+v", st)
	}
	if len(reg.Names()) != 0 {
		t.Fatalf("测试连接污染了注册表: %v", reg.Names())
	}
}

// 配置读不出来时要有明确状态，而不是静默当作"没有 MCP"。
func TestMCPManager_SourceErrorIsVisible(t *testing.T) {
	mgr := NewMCPManager(fakeSource{err: context.DeadlineExceeded}, NewRegistry())
	sts := mgr.Refresh(context.Background())
	if len(sts) != 1 || sts[0].Error == "" {
		t.Fatalf("配置读取失败应有状态: %+v", sts)
	}
}
