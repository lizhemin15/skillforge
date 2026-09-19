package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// fakeMCP 是可配置的假 MCP 服务端，用来在无网络、无真服务的情况下
// 覆盖客户端的每条分支：JSON 回包、SSE 回包、鉴权失败、分页、协议头。
//
// 刻意不追求"实现 MCP"，只回客户端真正会读的那几个字段——
// 假服务端一旦写得比客户端聪明，测试就会开始测假服务端而不是客户端。
type fakeMCP struct {
	mu         sync.Mutex
	wantKey    string   // 非空则校验 Bearer
	sse        bool     // tools/list 与 tools/call 用事件流回包
	pageSize   int      // >0 且工具数超过它时返回 nextCursor
	sessionID  string   // 非空则 initialize 时下发，之后校验必须回传
	errCode    int      // 非 0 时所有请求直接回该 HTTP 状态码
	errBody    string   // 配合 errCode
	toolErr    string   // 该工具名走 result.isError = true
	gotAuth    []string // 每次请求收到的 Authorization（去前缀）
	gotSession []string // 每次请求收到的 Mcp-Session-Id
	gotProto   []string // 每次请求收到的 MCP-Protocol-Version
	gotAccept  []string // 每次请求收到的 Accept
	gotMethods []string // 收到的 JSON-RPC method 序列
	calls      []string // 收到的 tools/call 工具名
	hits       int
}

func (f *fakeMCP) record(r *http.Request, method string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	f.gotAuth = append(f.gotAuth, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	f.gotSession = append(f.gotSession, r.Header.Get("Mcp-Session-Id"))
	f.gotProto = append(f.gotProto, r.Header.Get("MCP-Protocol-Version"))
	f.gotAccept = append(f.gotAccept, r.Header.Get("Accept"))
	if method != "" {
		f.gotMethods = append(f.gotMethods, method)
	}
}

func (f *fakeMCP) snapshot() (hits int, methods []string, auth []string, sess []string, proto []string, accept []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := func(s []string) []string { return append([]string(nil), s...) }
	return f.hits, cp(f.gotMethods), cp(f.gotAuth), cp(f.gotSession), cp(f.gotProto), cp(f.gotAccept)
}

// toolSet 是假服务端对外提供的工具，最后一个走 isError 分支。
func (f *fakeMCP) toolSet() []map[string]any {
	return []map[string]any{
		{
			"name":        "list_databases",
			"description": "列出所有数据库名",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "execute_sql",
			"description": "在指定数据库上执行 SQL",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"db":  map[string]any{"type": "string"},
					"sql": map[string]any{"type": "string"},
				},
				"required": []any{"db", "sql"},
			},
		},
		{
			"name":        "broken_tool",
			"description": "永远返回业务错误",
			"inputSchema": map[string]any{"type": "object"},
		},
	}
}

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (f *fakeMCP) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req rpcReq
		_ = json.Unmarshal(body, &req)
		f.record(r, req.Method)

		if f.errCode != 0 {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(f.errCode)
			_, _ = io.WriteString(w, f.errBody)
			return
		}
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if f.wantKey != "" && key != f.wantKey {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, "未授权，请在 Authorization 头中提供有效的 API Key")
			return
		}
		// 会话校验：下发了 session 就必须回传，否则按规范 404。
		if f.sessionID != "" && req.Method != "initialize" && r.Header.Get("Mcp-Session-Id") != f.sessionID {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "session not found")
			return
		}

		switch req.Method {
		case "initialize":
			f.mu.Lock()
			sid := f.sessionID
			f.mu.Unlock()
			if sid != "" {
				w.Header().Set("Mcp-Session-Id", sid)
			}
			f.reply(w, req.ID, map[string]any{
				"protocolVersion": ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "data-ontology", "version": "1.0.0"},
			}, f.sse)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			f.replyToolsList(w, req, f.sse)
		case "tools/call":
			f.replyToolCall(w, req, f.sse)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}
}

func (f *fakeMCP) writeMsg(w http.ResponseWriter, id json.RawMessage, payload string, sse bool) {
	if sse {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: message\ndata: "+payload+"\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, payload)
}

func (f *fakeMCP) reply(w http.ResponseWriter, id json.RawMessage, result any, sse bool) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result})
	f.writeMsg(w, id, string(b), sse)
}

func (f *fakeMCP) replyToolsList(w http.ResponseWriter, req rpcReq, sse bool) {
	var p struct {
		Cursor string `json:"cursor"`
	}
	_ = json.Unmarshal(req.Params, &p)
	all := f.toolSet()
	start := 0
	if p.Cursor != "" {
		start, _ = strconv.Atoi(p.Cursor)
	}
	end := len(all)
	next := ""
	if f.pageSize > 0 && start+f.pageSize < len(all) {
		end = start + f.pageSize
		next = strconv.Itoa(start + f.pageSize)
	}
	if start > len(all) {
		start = len(all)
	}
	page := all[start:end]
	out := map[string]any{"tools": page}
	if next != "" {
		out["nextCursor"] = next
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(req.ID), "result": out})
	f.writeMsg(w, req.ID, string(b), sse)
}

func (f *fakeMCP) replyToolCall(w http.ResponseWriter, req rpcReq, sse bool) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	_ = json.Unmarshal(req.Params, &p)
	f.mu.Lock()
	f.calls = append(f.calls, p.Name)
	errName := f.toolErr
	f.mu.Unlock()

	if p.Name == errName {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(req.ID),
			"result": map[string]any{
				"isError": true,
				"content": []map[string]any{{"type": "text", "text": "SQL 语法错误：near \"SELCT\""}},
			}})
		f.writeMsg(w, req.ID, string(b), sse)
		return
	}
	// 用参数回显当结果：能直接验证「传参一路没被吞掉」。
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(req.ID),
		"result": map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "工具 " + p.Name + " 收到参数 " + stableJSON(p.Arguments)},
			},
		}})
	f.writeMsg(w, req.ID, string(b), sse)
}

// stableJSON 按键排序序列化：测试里要拿它做子串断言，
// map 的随机序会让断言时绿时红（这种 flaky 最容易被人用「重跑一遍」掩盖掉）。
func stableJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return string(b)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(m[k])
		parts = append(parts, string(kb)+":"+string(vb))
	}
	return "{" + strings.Join(parts, ",") + "}"
}
