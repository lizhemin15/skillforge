package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/lizhemin15/skillforge/internal/mcp"
	"github.com/lizhemin15/skillforge/internal/tools"
)

// MCP 服务管理（管理员后台统一配置 + 开关）。
//
// 三条铁律：
//  1. **明文 key 永不出接口**：列表只回掩码，前端拿掩码回存 = 不修改原 key。
//     这就要求前端把「没动过密钥框」和「要清空密钥」区分开——本文件用
//     「留空 = 沿用原值」表达，清空另有 ClearMCPServerKey。
//  2. **改了配置立刻重连**：保存/开关/删除都触发一次 Refresh，否则后台显示
//     已启用、对话里却没有这个工具，用户会以为功能坏了。
//  3. **重连不阻塞保存**：MCP 服务器在内网里可能很慢甚至没起来，同步等它
//     会把「保存」按钮卡死。所以保存立即返回、后台异步重连。

// mcpServerView 是后台列表里的一台 MCP 服务。
type mcpServerView struct {
	ID         string                 `json:"id"`
	Name       string                 `json:"name"`
	URL        string                 `json:"url"`
	Enabled    bool                   `json:"enabled"`
	TimeoutSec int                    `json:"timeout_sec"`
	HasKey     bool                   `json:"has_key"`
	KeyMask    string                 `json:"key_mask"`
	Status     *tools.MCPServerStatus `json:"status,omitempty"`
}

// maskKey 生成展示用掩码。
//
// 掩码里必须带 "…"：isMaskedKey 靠它判断「前端回传的是掩码」，
// 少了这个字符，一次普通保存就会把真 key 覆盖成掩码字面量。
func maskKey(k string) (string, bool) {
	if strings.TrimSpace(k) == "" {
		return "", false
	}
	r := []rune(k)
	if len(r) > 8 {
		return string(r[:4]) + "…" + string(r[len(r)-4:]), true
	}
	return "••••", true
}

// ListMCP 返回全部 MCP 服务（含禁用）及其最近一次连接状态。
func (a *Admin) ListMCP(w http.ResponseWriter, r *http.Request) {
	views, err := a.mcpViews()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"servers":    views,
		"tools":      a.mcpToolNames(),
		"tool_count": len(a.mcpToolNames()),
	})
}

// mcpViews 组装列表视图（配置 + 状态，key 打码）。
func (a *Admin) mcpViews() ([]mcpServerView, error) {
	cfgs, err := a.store.ListMCPServers()
	if err != nil {
		return nil, err
	}
	byID := map[string]tools.MCPServerStatus{}
	if a.mcp != nil {
		for _, st := range a.mcp.Statuses() {
			byID[st.ID] = st
		}
	}
	out := make([]mcpServerView, 0, len(cfgs))
	for _, c := range cfgs {
		mask, has := maskKey(c.APIKey)
		v := mcpServerView{
			ID: c.ID, Name: c.Name, URL: c.URL, Enabled: c.Enabled,
			TimeoutSec: c.TimeoutSec, HasKey: has, KeyMask: mask,
		}
		if st, ok := byID[c.ID]; ok {
			s := st
			v.Status = &s
		}
		out = append(out, v)
	}
	return out, nil
}

// mcpToolNames 返回当前已挂进注册表的 MCP 工具名（nil 管理器 = 空）。
func (a *Admin) mcpToolNames() []string {
	if a.mcp == nil {
		return []string{}
	}
	names := a.mcp.ToolNames()
	if names == nil {
		return []string{}
	}
	return names
}

// mcpUpsertReq 是保存表单。字段与前端一一对应（readBody 禁未知字段）。
type mcpUpsertReq struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	URL        string `json:"url"`
	APIKey     string `json:"api_key"`
	Enabled    bool   `json:"enabled"`
	TimeoutSec int    `json:"timeout_sec"`
}

// UpsertMCP 新增 / 更新一台 MCP 服务。
func (a *Admin) UpsertMCP(w http.ResponseWriter, r *http.Request) {
	var req mcpUpsertReq
	if err := readBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体无效")
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	req.Name = strings.TrimSpace(req.Name)
	req.URL = strings.TrimSpace(req.URL)
	if req.ID == "" {
		writeErr(w, http.StatusBadRequest, "请填写标识（用于工具名前缀，建议英文，如 datatoolbox）")
		return
	}
	if !validMCPID(req.ID) {
		writeErr(w, http.StatusBadRequest, "标识只能包含字母、数字、下划线、短横线（最多 32 位）")
		return
	}
	if req.URL == "" {
		writeErr(w, http.StatusBadRequest, "请填写 MCP 地址")
		return
	}
	if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
		writeErr(w, http.StatusBadRequest, "MCP 地址必须以 http:// 或 https:// 开头")
		return
	}
	if req.Name == "" {
		req.Name = req.ID
	}

	cfg := mcp.Config{
		ID: req.ID, Name: req.Name, URL: req.URL,
		Enabled: req.Enabled, TimeoutSec: req.TimeoutSec,
	}
	// 编辑已有服务时前端回传的是掩码（或留空）→ 沿用原 key，绝不把掩码写进库。
	// isMaskedKey("") 也为真，所以「新建 + 不填 key」会落到同一条分支，
	// 靠「库里有原值」区分「沿用」和「本来就没有」。
	if isMaskedKey(req.APIKey) {
		old, err := a.store.GetMCPServer(req.ID)
		switch {
		case err == nil:
			cfg.APIKey = old.APIKey
		case strings.TrimSpace(req.APIKey) != "":
			// 传了掩码但库里没有原值：宁可报错，也别把一个掩码字符串当 key 存进去。
			// 存进去的后果是运行期 401，而用户看到的表单里 key 显示得"很正常"。
			writeErr(w, http.StatusBadRequest, "该标识在库里没有原始 API Key，请重新填写完整 Key")
			return
		}
	} else {
		cfg.APIKey = strings.TrimSpace(req.APIKey)
	}
	if cfg.TimeoutSec <= 0 || cfg.TimeoutSec > 600 {
		cfg.TimeoutSec = 30
	}

	if err := a.store.UpsertMCPServer(cfg); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.refreshMCPAsync()

	views, err := a.mcpViews()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "refresh_started": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "refresh_started": true, "servers": views,
	})
}

// ToggleMCPReq 是开关请求。
type ToggleMCPReq struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
}

// ToggleMCP 启用 / 停用一台 MCP 服务。
func (a *Admin) ToggleMCP(w http.ResponseWriter, r *http.Request) {
	var req ToggleMCPReq
	if err := readBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体无效")
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" {
		writeErr(w, http.StatusBadRequest, "缺少 id")
		return
	}
	n, err := a.store.SetMCPEnabled(req.ID, req.Enabled)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n == 0 {
		writeErr(w, http.StatusNotFound, "MCP 服务不存在："+req.ID)
		return
	}
	a.refreshMCPAsync()
	views, err := a.mcpViews()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "servers": views})
}

// DeleteMCP 删除一台 MCP 服务（并摘掉它已挂上的工具）。
func (a *Admin) DeleteMCP(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少 id")
		return
	}
	n, err := a.store.DeleteMCPServer(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n == 0 {
		writeErr(w, http.StatusNotFound, "MCP 服务不存在："+id)
		return
	}
	a.refreshMCPAsync()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// TestMCPConn 同步探测一台 MCP 服务，返回服务器自述 + 工具清单。
//
// 请求体刻意复用 mcpUpsertReq（保存表单的同一个形状）：
// 后台界面天然会把整张表单发过来，探测只认其中几个字段的话，
// 多出来的字段会被 readBody 的 DisallowUnknownFields 判成 400「请求体无效」——
// 报错完全不提字段名，用户只看到"测试连接"按钮永远失败。
// 允许只填 URL 不填 id（配之前先试通不通），也允许 id + 掩码 key（编辑态直接测）。
//
// 同步而非异步：这是用户主动点的「测试」，必须当场看到结果。
func (a *Admin) TestMCPConn(w http.ResponseWriter, r *http.Request) {
	var req mcpUpsertReq
	if err := readBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体无效")
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	req.URL = strings.TrimSpace(req.URL)

	// 探测用的展示名优先取表单里的名字，回落 id —— 状态里要显示人看得懂的名字。
	nm := strings.TrimSpace(req.Name)
	if nm == "" {
		nm = req.ID
	}
	cfg := mcp.Config{ID: req.ID, Name: nm, URL: req.URL, TimeoutSec: req.TimeoutSec}
	if req.ID != "" {
		if old, err := a.store.GetMCPServer(req.ID); err == nil {
			if cfg.URL == "" {
				cfg.URL = old.URL
			}
			if strings.TrimSpace(req.Name) == "" {
				cfg.Name = old.Name
			}
			if cfg.TimeoutSec <= 0 {
				cfg.TimeoutSec = old.TimeoutSec
			}
			if isMaskedKey(req.APIKey) {
				cfg.APIKey = old.APIKey
			}
		}
	}
	if !isMaskedKey(req.APIKey) {
		cfg.APIKey = strings.TrimSpace(req.APIKey)
	}
	if cfg.URL == "" {
		writeErr(w, http.StatusBadRequest, "请填写 MCP 地址")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), cfg.Timeout()+5*time.Second)
	defer cancel()

	st := tools.TestMCP(ctx, cfg)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         st.OK,
		"server":     st.Server,
		"version":    st.Version,
		"tool_count": st.ToolCount,
		// remote_tools：服务器自报的名字，后台展示这一份（人看得懂）。
		// mounted：挂上之后真正能被模型调用的本地名，供排查用。
		"remote_tools": st.RemoteTools,
		"mounted":      st.Tools,
		"error":        st.Error,
	})
}

// RefreshMCP 手动重连全部已启用的 MCP 服务。
func (a *Admin) RefreshMCP(w http.ResponseWriter, r *http.Request) {
	if !a.mcpRefreshMu.TryLock() {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "busy": true})
		return
	}
	defer a.mcpRefreshMu.Unlock()
	if a.mcp != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		a.mcp.Refresh(ctx)
	}
	views, err := a.mcpViews()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "servers": views, "tools": a.mcpToolNames(),
	})
}

// refreshMCPAsync 后台重连，不阻塞当前请求。
//
// TryLock 的用处：连点几次保存不该叠出几次重连（每次都要重新握手 + 拉清单）。
// 拿不到锁说明已经有一次在跑，直接跳过。
func (a *Admin) refreshMCPAsync() {
	if a.mcp == nil {
		return
	}
	if !a.mcpRefreshMu.TryLock() {
		return
	}
	go func() {
		defer a.mcpRefreshMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		a.mcp.Refresh(ctx)
	}()
}

// validMCPID 限制标识字符集。
//
// 它会被拼进工具名（mcp_<id>_<tool>），而 provider 只接受
// ^[a-zA-Z0-9_-]{1,64}$：放进中文或点号会让**整个**请求被 400，
// 连带把内置工具一起废掉。所以在这一层就拦住，别等到调用时才炸。
func validMCPID(id string) bool {
	if len(id) == 0 || len(id) > 32 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}
