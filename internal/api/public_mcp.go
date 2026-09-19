package api

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/lizhemin15/skillforge/internal/tools"
)

// ListMCP 返回「用户可以在聊天界面勾选使用」的 MCP 服务器。
//
// 只列**管理员已启用 且 本轮握手成功**的服务器：列一台连不上的，
// 用户勾了必然失败，然后来投诉「勾了没用」——那正是这个功能上一轮的投诉内容。
// 连不上的服务器在管理端后台照样可见可排查，只是不该出现在用户的选择面板里。
//
// 对外结构用 tools.MCPServerChoice（只有 id/name/tool_count），不是后台那份
// mcpServerView：后者带 URL / 密钥掩码 / 错误原文，而这条路由与 /api/chat 同级
// （无鉴权）。内网地址和报错片段不该给到站点访客。字段裁在类型定义处，
// 而不是「序列化时记得挑字段」——挑字段那套迟早被下一个人改掉。
//
// 无可用服务器时返回空数组（不是 null）：前端据此决定「按钮要不要出现」，
// 两者都空的话前端得写两套判断，迟早漏一个。
func (h *chatHandler) ListMCP(w http.ResponseWriter, r *http.Request) {
	views := []tools.MCPServerChoice{}
	if h.mcp != nil {
		views = h.mcp.Selectable()
	}
	// 稳定排序：同一批服务器每次返回同一顺序，面板不会每次刷新都换位置。
	sort.Slice(views, func(i, j int) bool {
		if views[i].Name != views[j].Name {
			return views[i].Name < views[j].Name
		}
		return views[i].ID < views[j].ID
	})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"servers": views,
	})
}
