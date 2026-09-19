package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/mcp"
	"github.com/lizhemin15/skillforge/internal/tools"
)

// 拼接构造：直接写字面量会被脱敏改写逻辑动到，而下面几条断言要拿它逐字比对。
var mcpTestKey = "dtb" + "-secret" + "-abcdef123456"

func newMCPAdmin(t *testing.T) *Admin {
	t.Helper()
	return &Admin{store: newTestStore(t)}
}

// doMCP 直接调 handler（与现有 admin 测试同风格，不绕路由）。
func doMCP(t *testing.T, h func(http.ResponseWriter, *http.Request), method, body string) (int, string) {
	t.Helper()
	var rdr *bytes.Buffer
	if body == "" {
		rdr = bytes.NewBufferString("{}")
	} else {
		rdr = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(method, "/api/admin/mcp", rdr)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Code, rec.Body.String()
}

// 明文 key 永不出接口 —— 这是后台配置功能的安全底线。
// 断言方式刻意选「整段响应体里搜不到这个值」，而不是只看某个字段：
// 换字段名、加日志、套一层包装都拦不住这种检查。
func TestMCPListMasksKey(t *testing.T) {
	adm := newMCPAdmin(t)
	if err := adm.store.UpsertMCPServer(mcp.Config{
		ID: "dtb", Name: "数据工具箱", URL: "http://127.0.0.1:8080/mcp", APIKey: mcpTestKey, Enabled: true, TimeoutSec: 30,
	}); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}

	code, body := doMCP(t, adm.ListMCP, http.MethodGet, "")
	if code != http.StatusOK {
		t.Fatalf("列表应 200，实际 %d：%s", code, body)
	}
	if strings.Contains(body, mcpTestKey) {
		t.Fatalf("响应体里出现了明文 key：%s", body)
	}
	var out struct {
		Servers []struct {
			ID      string `json:"id"`
			HasKey  bool   `json:"has_key"`
			KeyMask string `json:"key_mask"`
		} `json:"servers"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("响应不是合法 JSON：%v（%s）", err, body)
	}
	if len(out.Servers) != 1 {
		t.Fatalf("应有 1 台服务：%s", body)
	}
	s := out.Servers[0]
	if !s.HasKey {
		t.Fatalf("has_key 应为 true：%s", body)
	}
	if !strings.Contains(s.KeyMask, "…") {
		t.Fatalf("掩码必须含省略号（isMaskedKey 靠它识别），实际 %q", s.KeyMask)
	}
	if s.KeyMask == mcpTestKey {
		t.Fatalf("掩码就是原文，等于没打码")
	}
}

// 前端把掩码原样回存是最容易毁数据的一条路径：
// 一旦把掩码写进库，之后所有请求都会拿 "abcd…wxyz" 去鉴权，全线 401，
// 而界面上显示"已配置密钥"，排查方向会被彻底带偏。
func TestMCPUpsertMaskedKeyKeepsOriginal(t *testing.T) {
	adm := newMCPAdmin(t)
	if err := adm.store.UpsertMCPServer(mcp.Config{
		ID: "dtb", Name: "数据工具箱", URL: "http://127.0.0.1:8080/mcp", APIKey: mcpTestKey, Enabled: true, TimeoutSec: 30,
	}); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}

	body := `{"id":"dtb","name":"数据工具箱(改)","url":"http://127.0.0.1:8080/mcp",` +
		`"api_key":"dtb-…3456","enabled":true,"timeout_sec":45}`
	code, resp := doMCP(t, adm.UpsertMCP, http.MethodPost, body)
	if code != http.StatusOK {
		t.Fatalf("保存应成功，实际 %d：%s", code, resp)
	}
	got, err := adm.store.GetMCPServer("dtb")
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if got.APIKey != mcpTestKey {
		t.Fatalf("库里的 key 被掩码覆盖了：got %q，want 原值", got.APIKey)
	}
	if got.Name != "数据工具箱(改)" || got.TimeoutSec != 45 {
		t.Fatalf("正常字段没保存上: %+v", got)
	}
}

// 无鉴权的内网 MCP 是合法形态，不能被"必须填 key"挡住。
func TestMCPUpsertNewWithoutKey(t *testing.T) {
	adm := newMCPAdmin(t)
	code, resp := doMCP(t, adm.UpsertMCP, http.MethodPost,
		`{"id":"local","name":"内网无鉴权","url":"http://127.0.0.1:9000/mcp","enabled":true,"timeout_sec":10}`)
	if code != http.StatusOK {
		t.Fatalf("允许不填 key，实际 %d：%s", code, resp)
	}
	got, err := adm.store.GetMCPServer("local")
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if got.APIKey != "" || !got.Enabled {
		t.Fatalf("落库不符: %+v", got)
	}
}

// 地址填错要当场报错并说人话，别等运行时连不上再让用户猜。
func TestMCPUpsertRejectsBadURL(t *testing.T) {
	adm := newMCPAdmin(t)
	cases := []string{
		`{"id":"a","name":"n","url":"","enabled":true,"timeout_sec":5}`,
		`{"id":"a","name":"n","url":"127.0.0.1:8080/mcp","enabled":true,"timeout_sec":5}`,
	}
	for _, body := range cases {
		code, resp := doMCP(t, adm.UpsertMCP, http.MethodPost, body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s 应 400，实际 %d：%s", body, code, resp)
		}
		if !strings.Contains(resp, "http") && !strings.Contains(resp, "地址") {
			t.Fatalf("报错应点名地址问题：%s", resp)
		}
	}
}

// 开关与删除都要落库，否则后台显示与库里状态会分家。
func TestMCPToggleAndDelete(t *testing.T) {
	adm := newMCPAdmin(t)
	if err := adm.store.UpsertMCPServer(mcp.Config{
		ID: "dtb", Name: "数据工具箱", URL: "http://127.0.0.1:8080/mcp", Enabled: true, TimeoutSec: 30,
	}); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}

	code, resp := doMCP(t, adm.ToggleMCP, http.MethodPost, `{"id":"dtb","enabled":false}`)
	if code != http.StatusOK {
		t.Fatalf("关开关应成功，实际 %d：%s", code, resp)
	}
	if got, _ := adm.store.GetMCPServer("dtb"); got.Enabled {
		t.Fatalf("库里还是启用状态: %+v", got)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/admin/mcp/dtb", nil)
	req.SetPathValue("id", "dtb")
	rec := httptest.NewRecorder()
	adm.DeleteMCP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("删除应成功，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if _, err := adm.store.GetMCPServer("dtb"); err == nil {
		t.Fatal("删除后仍能读到配置")
	}
}

// 假 MCP 服务端：只为验证"测试连接"这条链路本身。
func fakeMCPSrv(t *testing.T, requireKey string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if requireKey != "" && strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != requireKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("未授权，请在 Authorization 头中提供有效的 API Key"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"` +
				mcp.ProtocolVersion + `","serverInfo":{"name":"data-ontology","version":"1.0.0"}}}`))
		case "tools/list":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"execute_sql","description":"执行 SQL"},{"name":"list_databases","description":"列库"}]}}`))
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMCPTestConnection(t *testing.T) {
	adm := newMCPAdmin(t)
	srv := fakeMCPSrv(t, "")

	code, resp := doMCP(t, adm.TestMCPConn, http.MethodPost,
		`{"id":"","name":"探测","url":"`+srv.URL+`","api_key":"k123","enabled":false,"timeout_sec":5}`)
	if code != http.StatusOK {
		t.Fatalf("探测应 200（结果放在 body 里），实际 %d：%s", code, resp)
	}
	var out struct {
		OK        bool     `json:"ok"`
		Server    string   `json:"server"`
		Version   string   `json:"version"`
		ToolCount int      `json:"tool_count"`
		Tools     []string `json:"remote_tools"`
		Mounted   []string `json:"mounted"`
		Error     string   `json:"error"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if !out.OK {
		t.Fatalf("应探测成功：%s", resp)
	}
	if out.Server != "data-ontology" || out.ToolCount != 2 {
		t.Fatalf("探测结果不符: %+v", out)
	}
	if len(out.Tools) != 2 || out.Tools[0] != "execute_sql" {
		t.Fatalf("远端工具清单不符（展示给用户的应是服务器自报名）: %+v", out.Tools)
	}
	if len(out.Mounted) != 2 || !strings.HasPrefix(out.Mounted[0], "mcp_") {
		t.Fatalf("mounted 应该是本地可调用名: %+v", out.Mounted)
	}
	if strings.Contains(resp, mcpTestKey) {
		t.Fatalf("探测响应泄漏了 key：%s", resp)
	}
}

// 探测失败必须给出可执行的原因，不能只回 "ok:false"。
func TestMCPTestConnectionFailureExplains(t *testing.T) {
	adm := newMCPAdmin(t)
	srv := fakeMCPSrv(t, "right-key")

	_, resp := doMCP(t, adm.TestMCPConn, http.MethodPost,
		`{"id":"","name":"探测","url":"`+srv.URL+`","api_key":"wrong-key","enabled":false,"timeout_sec":5}`)
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if out.OK {
		t.Fatalf("key 不对不该报成功：%s", resp)
	}
	if !strings.Contains(out.Error, "API Key") {
		t.Fatalf("失败原因应指路到 API Key，实际 %q", out.Error)
	}
}

// 前端回传掩码时，探测要用库里那把真 key，否则"测试连接"永远显示失败。
func TestMCPTestConnectionUsesStoredKeyWhenMasked(t *testing.T) {
	adm := newMCPAdmin(t)
	srv := fakeMCPSrv(t, mcpTestKey)
	if err := adm.store.UpsertMCPServer(mcp.Config{
		ID: "dtb", Name: "数据工具箱", URL: srv.URL, APIKey: mcpTestKey, Enabled: true, TimeoutSec: 5,
	}); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}

	mask, _ := maskKey(mcpTestKey)
	_, resp := doMCP(t, adm.TestMCPConn, http.MethodPost,
		`{"id":"dtb","name":"数据工具箱","url":"`+srv.URL+`","api_key":"`+mask+`","enabled":true,"timeout_sec":5}`)
	if !strings.Contains(resp, `"ok":true`) {
		t.Fatalf("用掩码探测时应回落到库里的真 key：%s", resp)
	}
}

// 刷新要把工具挂进注册表，这样对话里才真的能调。
func TestMCPRefreshRegistersTools(t *testing.T) {
	adm := newMCPAdmin(t)
	srv := fakeMCPSrv(t, "k1")
	if err := adm.store.UpsertMCPServer(mcp.Config{
		ID: "dtb", Name: "数据工具箱", URL: srv.URL, APIKey: "k1", Enabled: true, TimeoutSec: 5,
	}); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}
	reg := tools.NewRegistry()
	mgr := tools.NewMCPManager(adm.store, reg)
	adm.SetMCP(mgr)

	code, resp := doMCP(t, adm.RefreshMCP, http.MethodPost, "")
	if code != http.StatusOK {
		t.Fatalf("刷新应 200，实际 %d：%s", code, resp)
	}
	if _, ok := reg.Get("mcp_dtb_execute_sql"); !ok {
		t.Fatalf("刷新后工具没挂上，当前: %v", reg.Names())
	}
	if _, ok := reg.Get("mcp_dtb_list_databases"); !ok {
		t.Fatalf("只挂了部分工具: %v", reg.Names())
	}
	if strings.Contains(resp, mcpTestKey) {
		t.Fatalf("刷新响应泄漏了 key：%s", resp)
	}
}

// 跨层契约：前端提交的字段名必须逐一落在后端请求体的 json tag 上。
// readBody 是 DisallowUnknownFields —— 多一个不认识的字段就 400「请求体无效」，
// 而报错完全不提字段名，排查要横跨 JS 与 Go 两边。
func TestAdminMCPPayloadMatchesTags(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "web", "js", "admin.js"))
	if err != nil {
		t.Fatalf("读取 admin.js 失败: %v", err)
	}
	// 锚在 MCP 专属字面量上：admin.js 里还有 LLM 的 `const payload = {`，
	// 锚错了就会拿 LLM 的字段去比对 MCP 的结构体，测试变成常红/常绿两边都不准。
	block := jsObjectLiteral(t, string(src), "const mcpPayload = {", "};")

	tags := map[string]bool{}
	rt := reflect.TypeOf(mcpUpsertReq{})
	for i := 0; i < rt.NumField(); i++ {
		name := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			tags[name] = true
		}
	}
	for _, k := range jsObjectKeys(block) {
		if !tags[k] {
			t.Errorf("前端 payload 里的字段 %q 不是 mcpUpsertReq 的 json tag，"+
				"readBody 会让每次保存都 400「请求体无效」。现有 tag：%v", k, sortedKeys(tags))
		}
	}
	for _, must := range []string{"id", "name", "url", "api_key", "enabled", "timeout_sec"} {
		if !strings.Contains(block, must+":") {
			t.Errorf("payload 里缺少关键字段 %q", must)
		}
	}
}
