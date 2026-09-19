package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/mcp"
)

// 这一组用例守的是「用户勾选门控」——
//
//	默认不勾 = 模型手上 0 个 MCP 工具；勾了哪台 = 只有那台的工具。
//
// 为什么值得单独一组：门控失灵的两个方向后果完全不同。多给 = 没勾也能捅到
// 内网业务系统（安全问题，且用户看不见）；少给 = 用户看得见、能反馈。
// 所以下面每个「不该给」的断言都比「该给」的断言更硬：先证否，再证有。

// gateFixture 是「全局注册表已挂上两台 MCP 服务器」的现场。
//
// 特意用了两个**工具名相同**的服务器（都叫 execute_sql）：这正是最容易被
// 「按工具名过滤」的假实现蒙混过去的场景——那种实现看到 execute_sql 就一起放行。
func gateFixture(t *testing.T) (*Registry, *MCPManager) {
	t.Helper()
	srvA := fakeMCPServer(t, stdTools()) // dtb：execute_sql + boom
	srvB := fakeMCPServer(t, []map[string]any{
		{"name": "execute_sql", "description": "在客户库里执行 SQL"},
		{"name": "list_customers", "description": "列出客户"},
	})
	reg := NewRegistry()
	reg.Register(&fakeTool{name: "builtin_echo"})
	src := fakeSource{cfgs: []mcp.Config{
		{ID: "dtb", Name: "数据工具箱", URL: srvA.URL, Enabled: true, TimeoutSec: 5},
		{ID: "crm", Name: "客户系统", URL: srvB.URL, Enabled: true, TimeoutSec: 5},
	}}
	mgr := NewMCPManager(src, reg)
	mgr.Refresh(context.Background())

	// 前置断言：现场必须真的是「两台都挂上了」。没有这一步，后面「不勾 = 0 个」
	// 可能只是因为压根没挂上任何工具——那是空跑绿，尺子没在量东西。
	if len(mgr.ToolNames()) != 4 {
		t.Fatalf("前置不成立：应有 4 个 MCP 工具挂上，实际 %v", mgr.ToolNames())
	}
	return reg, mgr
}

// 默认不勾：MCP 工具一个都不能给，内置工具一个都不能少。
func TestGate_DefaultNoMCPTools(t *testing.T) {
	reg, _ := gateFixture(t)

	got := FilterMCP(reg, nil)
	for _, n := range got.Names() {
		if strings.HasPrefix(n, "mcp_") {
			t.Fatalf("没勾选却放行了 MCP 工具: %q（全部: %v）", n, got.Names())
		}
	}
	if _, ok := got.Get("builtin_echo"); !ok {
		t.Fatalf("内置工具被误杀: %v", got.Names())
	}

	// 空集合与 nil 必须同效：前端没勾时可能发 []、也可能不传这个字段，
	// 两条路给出不同结果的话，用户会遇到「换了个浏览器就变了」。
	got2 := FilterMCP(reg, map[string]bool{})
	for _, n := range got2.Names() {
		if strings.HasPrefix(n, "mcp_") {
			t.Fatalf("空勾选集放行了 MCP 工具: %q", n)
		}
	}
}

// 只勾一台：那台的工具在，另一台的（哪怕名字一样）必须不在。
func TestGate_OnlySelectedServerPasses(t *testing.T) {
	reg, _ := gateFixture(t)

	got := FilterMCP(reg, MCPServerSet([]string{"dtb"}))

	for _, want := range []string{"mcp_dtb_execute_sql", "mcp_dtb_boom"} {
		if _, ok := got.Get(want); !ok {
			t.Fatalf("勾了 dtb，%q 却没放行: %v", want, got.Names())
		}
	}
	for _, bad := range []string{"mcp_crm_execute_sql", "mcp_crm_list_customers"} {
		if _, ok := got.Get(bad); ok {
			t.Fatalf("只勾了 dtb，却放行了 crm 的 %q: %v", bad, got.Names())
		}
	}
	if _, ok := got.Get("builtin_echo"); !ok {
		t.Fatalf("内置工具不该受勾选影响: %v", got.Names())
	}
}

// 勾两台：两台都在。
func TestGate_BothSelected(t *testing.T) {
	reg, _ := gateFixture(t)
	got := FilterMCP(reg, MCPServerSet([]string{"dtb", "crm"}))
	if len(got.Names()) != 5 { // 4 个 MCP + 1 个内置
		t.Fatalf("勾两台应得 5 个工具，实际 %d: %v", len(got.Names()), got.Names())
	}
}

// 名字像 MCP、但没标归属的工具，一律**不放行**。
//
// 这条是朝关闭方向倒的兜底：将来有人新写一个 MCP 适配器忘了实现 ServerID，
// 后果是「这台服务器永远勾不动」（用户看得见、能反馈），而不是「没勾也能调」
// （安全问题，且没人会发现）。方向选错的话，这个兜底本身就是漏洞。
func TestGate_UnlabeledMCPNameFailsClosed(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&fakeTool{name: "mcp_ghost"}) // 有 mcp_ 前缀，但没 ServerID
	reg.Register(&fakeTool{name: "builtin_echo"})

	for _, allow := range []map[string]bool{nil, {}, {"dtb": true}} {
		got := FilterMCP(reg, allow)
		if _, ok := got.Get("mcp_ghost"); ok {
			t.Fatalf("归属不明的 mcp_ 工具被放行了（allow=%v）: %v", allow, got.Names())
		}
	}
}

// 门控必须**只**产出新表，不许动全局注册表。
//
// 注册表是进程级共享对象，而勾选是每请求的。往共享表上加过滤开关，并发下
// A 的勾选会作用到 B 身上（极难复现）。这条断言就是拦住那种「顺手改全局」的实现。
func TestGate_DoesNotTouchGlobalRegistry(t *testing.T) {
	reg, mgr := gateFixture(t)

	_ = FilterMCP(reg, nil)

	if n := len(reg.Names()); n != 5 {
		t.Fatalf("全局注册表被改动了：应有 5 个工具，实际 %d: %v", n, reg.Names())
	}
	if len(mgr.ToolNames()) != 4 {
		t.Fatalf("管理器快照被改动了: %v", mgr.ToolNames())
	}
}

// 过期 / 重复 / 脏的勾选值要被悄悄收敛掉（用户换过浏览器、管理员删过服务器）。
//
// 入参是**已经过滤好的可选清单**，所以「管理员关了 / 连不上」这两类在 Selectable
// 那一步就没了，这里只负责交集与去重——两级各管一段，不互相代劳。
func TestAllowedServers_DropsStaleAndDupes(t *testing.T) {
	selectable := []MCPServerChoice{
		{ID: "a", Name: "A"},
		{ID: "c", Name: "C"},
	}
	got := AllowedMCPServers(selectable, []string{"a", "zzz", "a", " a ", "", "c"})
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("应收敛为 [a c]（去重、去脏、保持原顺序），实际 %v", got)
	}

	// 一个都没勾 / 什么都没得勾 → nil（不是空切片：调用方统一用 len==0 判）。
	if AllowedMCPServers(selectable, nil) != nil {
		t.Fatalf("没勾选应返回 nil")
	}
	if AllowedMCPServers(nil, []string{"a"}) != nil {
		t.Fatalf("没有可勾的服务器应返回 nil")
	}
	if AllowedMCPServers(selectable, []string{"已删除的服务器"}) != nil {
		t.Fatalf("全是过期 id 时应返回 nil")
	}
}

// 提示词必须跟着勾选走。
//
// 只拦工具表、不拦提示词 = 模型满脑子想着去调一个不存在的工具：
// 空转到轮数上限、又慢、最后编答案。这正是「勾了没生效」的典型表现之一。
func TestPromptHint_DefaultEmptyAndScopedToSelection(t *testing.T) {
	_, mgr := gateFixture(t)

	if mgr.PromptHint() == "" {
		t.Fatalf("前置不成立：全局提示词段落不该为空")
	}
	if got := mgr.PromptHintFor(nil); got != "" {
		t.Fatalf("没勾选时提示词里不该出现 MCP: %q", got)
	}
	if got := mgr.PromptHintFor([]string{"不存在"}); got != "" {
		t.Fatalf("勾了不存在的服务器应给空段落: %q", got)
	}

	one := mgr.PromptHintFor([]string{"dtb"})
	if !strings.Contains(one, "mcp_dtb_execute_sql") {
		t.Fatalf("勾了 dtb 却没说有哪些工具: %q", one)
	}
	if strings.Contains(one, "mcp_crm_") || strings.Contains(one, "客户系统") {
		t.Fatalf("只勾 dtb，提示词里却有 crm 的内容: %q", one)
	}

	both := mgr.PromptHintFor([]string{"dtb", "crm"})
	if !strings.Contains(both, "mcp_dtb_") || !strings.Contains(both, "mcp_crm_") {
		t.Fatalf("勾两台应两台的清单都在: %q", both)
	}
	// 同一批勾选必须产出同一份提示词（顺序稳定），否则提示词缓存永远不命中。
	if again := mgr.PromptHintFor([]string{"dtb", "crm"}); again != both {
		t.Fatalf("同序勾选产出了不同提示词：\n%q\n%q", both, again)
	}
}

// 归属快照要准：某台挂了哪些工具，必须能从管理器查出来（线上验收与自证都靠它）。
func TestServerTools_Ownership(t *testing.T) {
	_, mgr := gateFixture(t)

	got := mgr.ServerTools("dtb")
	if len(got) != 2 {
		t.Fatalf("dtb 应有 2 个工具，实际 %v", got)
	}
	for _, n := range got {
		if !strings.HasPrefix(n, "mcp_dtb_") {
			t.Fatalf("dtb 名下混进了别的工具: %q", n)
		}
	}
	// 两台的工具名不能互相串（都叫 execute_sql，串了就是「勾 A 调 B」）。
	crm := mgr.ServerTools("crm")
	for _, n := range crm {
		if strings.HasPrefix(n, "mcp_dtb_") {
			t.Fatalf("crm 名下混进了 dtb 的工具: %q", n)
		}
	}
	if len(mgr.ServerTools("不存在")) != 0 {
		t.Fatalf("不存在的服务器不该有工具")
	}
}

// 用户面板只列「启用且连得上」的服务器，且**不能漏内网信息**。
func TestSelectable_OnlyUsableAndNoLeak(t *testing.T) {
	good := fakeMCPServer(t, stdTools())
	reg := NewRegistry()
	src := fakeSource{cfgs: []mcp.Config{
		{ID: "good", Name: "好服务", URL: good.URL, Enabled: true, TimeoutSec: 5},
		{ID: "down", Name: "坏服务", URL: "http://127.0.0.1:1/mcp", Enabled: true, TimeoutSec: 1},
		{ID: "off", Name: "关了的", URL: good.URL, Enabled: false, TimeoutSec: 5},
	}}
	mgr := NewMCPManager(src, reg)
	mgr.Refresh(context.Background())

	sel := mgr.Selectable()
	if len(sel) != 1 || sel[0].ID != "good" {
		t.Fatalf("只应列出可用服务器，实际 %+v", sel)
	}
	if sel[0].ToolCount != 2 {
		t.Fatalf("工具数不对: %+v", sel[0])
	}

	// 这条路由与 /api/chat 同级（无鉴权）：内网地址、密钥、报错原文都不能出去。
	b, err := json.Marshal(sel)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	js := string(b)
	for _, forbidden := range []string{"url", "127.0.0.1", good.URL, "key", "api"} {
		if strings.Contains(strings.ToLower(js), strings.ToLower(forbidden)) {
			t.Fatalf("公开结构里漏了 %q: %s", forbidden, js)
		}
	}
}
