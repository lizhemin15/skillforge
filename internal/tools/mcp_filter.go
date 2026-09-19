package tools

import "strings"

// mcpOwned 是「工具知道自己归属哪台服务器」的能力声明。
//
// 用接口而不是具体类型断言：单测里的假工具（不 import mcp 包）也能参与门控，
// 否则门控这把尺子只能靠真起一个 MCP 服务器来验，跑不动也跑不稳。
type mcpOwned interface {
	ServerID() string
}

// mcpOwner 判定一个工具是不是 MCP 工具，是则给出归属服务器 id。
//
// 注意「名字像 MCP 但没标归属」这一支：它返回 ok=true 而 id 为空。
// 这是**故意朝关闭方向倒**——门控这种东西一旦失灵，方向必须是「少给」
// 而不是「多给」：多给的后果是用户没勾也能捅到内网业务系统，那是安全问题；
// 少给的后果只是这次调用不可用，用户看得见、能反馈。
func mcpOwner(t Tool) (id string, isMCP bool) {
	if t == nil {
		return "", false
	}
	if o, ok := t.(mcpOwned); ok {
		if sid := o.ServerID(); sid != "" {
			return sid, true
		}
	}
	if strings.HasPrefix(t.Name(), "mcp_") {
		return "", true
	}
	return "", false
}

// FilterMCP 返回一份「按用户勾选裁剪过的工具表」。
//
// 语义（这就是「默认不调度 MCP」的实现本体）：
//   - allow 为空/nil → 结果里 **0 个** MCP 工具，内置工具一个不少；
//   - allow 含某服务器 id → 只放行那台的工具，其余 MCP 工具照样排除；
//   - 内置工具（非 MCP）永远放行，与勾选无关。
//
// 为什么是「复制一份」而不是「在全局注册表上加过滤标志」：
// 注册表是进程级共享的，每个请求的勾选都不一样。往共享对象上写每请求的开关，
// 并发下就是 A 用户的勾选作用到 B 用户身上（而且极难复现）。
// 复制是一请求一份，天然隔离——工具数量是几十级，复制成本可忽略。
func FilterMCP(reg *Registry, allow map[string]bool) *Registry {
	if reg == nil {
		return nil
	}
	out := NewRegistry()
	for _, t := range reg.All() {
		sid, isMCP := mcpOwner(t)
		if !isMCP {
			out.Register(t)
			continue
		}
		if sid != "" && allow[sid] {
			out.Register(t)
		}
	}
	return out
}

// AllowedMCPServers 把「用户传来的勾选」收敛成「真正有效的服务器 id 集合」。
//
// 为什么要过这一道：前端传上来的是裸字符串数组，可能来自过期的 localStorage
// （服务器被删了/被管理员关了/这台今天没起来）。直接用会让每个环节各自处理
// 脏数据，最终表现千奇百怪。这里一次收敛，后面只认「集合里的 id」。
//
// 入参故意收窄成 MCPServerChoice（而不是完整状态）：这里只该看到「可勾选的东西」，
// 连带 URL 的状态表塞进来，早晚有人顺手用了它。收窄后这个函数天然拿不到内网信息。
//
// 顺带保证返回值是「去重 + 原顺序 + 只留可用的」，这样提示词段落可复现。
func AllowedMCPServers(selectable []MCPServerChoice, want []string) []string {
	if len(want) == 0 || len(selectable) == 0 {
		return nil
	}
	ok := make(map[string]bool, len(selectable))
	for _, st := range selectable {
		ok[st.ID] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, id := range want {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] || !ok[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// MCPServerSet 把 id 列表变成 FilterMCP 要的集合。
func MCPServerSet(ids []string) map[string]bool {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			out[id] = true
		}
	}
	return out
}
