package tools

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lizhemin15/skillforge/internal/mcp"
)

// MCPTool 把一个远端 MCP 工具适配成本地 Tool。
//
// 适配而不重写：远端工具的 schema 与描述直接从 MCP 服务器透传，本地只做
// 「改名字 + 包一层调用」两件事。这样接一个新 MCP 服务 = 后台填个地址，
// 不用改任何 Go 代码。
type MCPTool struct {
	name      string // 本地工具名（mcp_<alias>_<remote>），已 sanitize
	serverID  string // 归属服务器 id（用户勾选门控按它裁剪，必须与配置主键同源）
	server    string // 服务器可读名（写进描述，帮模型判断该不该用）
	remote    mcp.RemoteTool
	callFn    func(ctx context.Context, args map[string]any) (mcp.CallResult, error)
	resultMax int
}

// NewMCPTool 构造适配器。callFn 抽成函数是为了单测能塞假服务器，
// 不必真起一个 HTTP 服务。
//
// serverID 是配置表主键，也是「用户勾选」时前端的取值——三个环节（配置/门控/前端）
// 必须同源，否则勾了等于没勾。serverName 只用人看/模型看，不参与匹配。
func NewMCPTool(serverID, serverName, alias string, remote mcp.RemoteTool, callFn func(context.Context, map[string]any) (mcp.CallResult, error)) *MCPTool {
	return &MCPTool{
		name:      mcpLocalName(alias, remote.Name),
		serverID:  serverID,
		server:    serverName,
		remote:    remote,
		callFn:    callFn,
		resultMax: 6000,
	}
}

// Name 返回本地工具名。
func (t *MCPTool) Name() string { return t.name }

// ServerID 返回工具归属的 MCP 服务器 id。
//
// 存在的唯一理由是门控：注册表是全局长着一份的（管理员开着就有），
// 而「谁勾了哪台」是每请求的事。没有归属信息就只能按名字前缀猜，
// 服务器名是中文时前缀会退化成哈希，猜错就是「勾了 A 却调了 B」。
func (t *MCPTool) ServerID() string { return t.serverID }

// Description 拼「远端描述 + 归属说明」。
//
// 归属说明不是装饰：模型要靠它区分「这是外部数据中台的能力」还是本地能力，
// 否则遇到"查一下数据库有哪些表"会去写 http_request 而不是用现成工具。
func (t *MCPTool) Description() string {
	desc := strings.TrimSpace(t.remote.Description)
	var b strings.Builder
	if desc != "" {
		b.WriteString(desc)
	} else {
		b.WriteString("外部 MCP 工具 " + t.remote.Name)
	}
	b.WriteString("\n\n（该工具由 MCP 服务「")
	b.WriteString(t.server)
	b.WriteString("」提供，参数请严格按 JSON Schema 填写。）")
	return b.String()
}

// Schema 透传远端 inputSchema，并做两处净化。
//
// 净化是必须的，不是防御性编程：schema 里 required 写成字符串、
// 或顶层缺 type，都会被 provider 直接判 400 拒绝整个请求——那会连带
// 让本轮**所有**工具都不可用，不只是这一个。
func (t *MCPTool) Schema() map[string]any {
	in := t.remote.InputSchema
	out := map[string]any{}
	for k, v := range in {
		out[k] = v
	}
	if _, ok := out["type"]; !ok {
		out["type"] = "object"
	}
	if props, ok := out["properties"]; !ok || props == nil {
		out["properties"] = map[string]any{}
	}
	if req, ok := out["required"]; ok {
		arr, ok2 := req.([]any)
		if !ok2 {
			// 类型不对就整条删掉：宁可少一个约束，也不要整个请求被 400。
			delete(out, "required")
		} else {
			clean := make([]any, 0, len(arr))
			for _, v := range arr {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					clean = append(clean, s)
				}
			}
			if len(clean) == 0 {
				delete(out, "required")
			} else {
				out["required"] = clean
			}
		}
	}
	return out
}

// Run 调用远端工具。
func (t *MCPTool) Run(ctx context.Context, args map[string]any) (Result, error) {
	if t.callFn == nil {
		return Result{}, fmt.Errorf("MCP 工具 %s 未连接", t.name)
	}
	res, err := t.callFn(ctx, args)
	if err != nil {
		return Result{}, err
	}
	text := Truncate(strings.TrimSpace(res.Text), t.resultMax)
	if res.IsError {
		// 业务失败：把远端原话交给模型让它自我修正（循环会把 error 文本回灌），
		// 而不是当成空结果——空结果会让模型继续编。
		if text == "" {
			text = "远端工具返回失败，但没有给出原因"
		}
		return Result{}, fmt.Errorf("MCP 工具 %s 执行失败：%s", t.remote.Name, text)
	}
	if text == "" {
		text = "（工具执行成功，无返回内容）"
	}
	return Result{
		Content: text,
		Display: fmt.Sprintf("MCP「%s」· %s", t.server, t.remote.Name),
	}, nil
}

// mcpLocalName 生成符合 provider 约束的工具名。
//
// provider 要求函数名匹配 ^[a-zA-Z0-9_-]{1,64}$：中文名、点号、空格都会让它 400，
// 所以一律 sanitize；超过 64 字符则截断（前缀保留，便于看出是哪台服务器）。
func mcpLocalName(alias, remote string) string {
	name := "mcp_" + sanitizeIdent(alias) + "_" + sanitizeIdent(remote)
	r := []rune(name)
	if len(r) > 64 {
		name = string(r[:64])
	}
	return name
}

// sanitizeIdent 只留下 [a-zA-Z0-9_-]，其余换成下划线并去掉首尾下划线。
func sanitizeIdent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		out = "srv"
	}
	return out
}

// mcpAlias 决定工具名前缀用哪个标识。
//
// 中文服务器名 sanitize 后会是空的（全下划线），这时不能都退化成 "srv"，
// 否则两个中文名的服务器工具名会撞在一起（后注册的静默覆盖前一个）。
// 所以顺序是：ASCII 化的 id → ASCII 化的 name → id 的短哈希。
func mcpAlias(cfg mcp.Config) string {
	if a := sanitizeIdent(cfg.ID); a != "srv" {
		return a
	}
	if a := sanitizeIdent(cfg.Name); a != "srv" {
		return a
	}
	sum := sha1.Sum([]byte(cfg.ID + "|" + cfg.Name))
	return "srv" + hex.EncodeToString(sum[:3])
}

// MCPSource 是管理器读取配置的来源（由 store 实现；单测里用假实现）。
type MCPSource interface {
	ListMCPServers() ([]mcp.Config, error)
}

// MCPServerStatus 是一台 MCP 服务器的当前状态（后台列表与自检都用它）。
type MCPServerStatus struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Enabled   bool   `json:"enabled"`
	OK        bool   `json:"ok"`
	Server    string `json:"server,omitempty"`
	Version   string `json:"version,omitempty"`
	ToolCount int    `json:"tool_count"`
	// Tools 是挂进注册表后的本地名（真正能被模型调用的名字）。
	Tools []string `json:"tools,omitempty"`
	// RemoteTools 是服务器自报的原始工具名。后台"测试连接"要展示这一份——
	// 本地名带 mcp_ 前缀，服务名是中文时前缀还会退化成哈希，对用户是噪声。
	RemoteTools []string  `json:"remote_tools,omitempty"`
	Error       string    `json:"error,omitempty"`
	CheckedAt   time.Time `json:"checked_at"`
}

// MCPManager 负责把「后台配置的 MCP 服务器」变成「注册表里可调用的工具」。
//
// 生命周期：启动时 Refresh 一次；后台改动配置后再 Refresh。
// Refresh 是「先摘旧名、再挂新名」的全量替换——增量更新会在配置被删掉
// 或工具被禁用时留下僵尸工具，那种 bug 现场极难查（工具还在列表里但调不通）。
type MCPManager struct {
	mu       sync.Mutex
	src      MCPSource
	reg      *Registry
	mounted  []string // 上一轮挂进注册表的工具名
	statuses []MCPServerStatus
	hint     string // 给系统提示词用的工具清单摘要

	// mountedByServer / hintByServer 是上面两份数据的「按服务器切片」。
	//
	// 为什么非要多存一份：注册表是全局的（管理员开着，所有请求都看得见），
	// 而提示词段落是每请求生成的。用户只勾了 A 的时候，提示词里出现 B 的工具
	// 就是在教模型去调一个它拿不到的工具——模型会连着几轮重试再放弃，
	// 用户看到的就是「勾了也没用 / 更慢了」。切片必须与门控同源，不能事后过滤字符串。
	mountedByServer map[string][]string
	hintByServer    map[string]string
}

// NewMCPManager 构造管理器。reg 为 nil 时只做探测不注册（便于后台单独测连接）。
func NewMCPManager(src MCPSource, reg *Registry) *MCPManager {
	return &MCPManager{src: src, reg: reg}
}

// Refresh 重新读取配置并同步注册表。返回每台服务器的状态。
//
// 单台服务器失败不影响其他台（内网里某台没起来是常态，不该拖垮整个工具面）。
func (m *MCPManager) Refresh(ctx context.Context) []MCPServerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()

	var cfgs []mcp.Config
	if m.src != nil {
		list, err := m.src.ListMCPServers()
		if err != nil {
			m.statuses = []MCPServerStatus{{
				Name: "配置读取失败", Error: err.Error(), CheckedAt: time.Now(),
			}}
			return m.statuses
		}
		cfgs = list
	}

	newly := map[string]Tool{}
	owner := map[string]string{}   // 工具名 → 服务器 id
	hintByServer := map[string]string{}
	statuses := make([]MCPServerStatus, 0, len(cfgs))
	var hintParts []string

	for _, cfg := range cfgs {
		st := MCPServerStatus{
			ID: cfg.ID, Name: cfg.Name, URL: cfg.URL,
			Enabled: cfg.Enabled, CheckedAt: time.Now(),
		}
		if !cfg.Enabled {
			st.Error = "未启用"
			statuses = append(statuses, st)
			continue
		}
		alias := mcpAlias(cfg)
		client, err := mcp.NewClient(cfg)
		if err != nil {
			st.Error = err.Error()
			statuses = append(statuses, st)
			continue
		}
		info, err := client.Initialize(ctx)
		if err != nil {
			st.Error = err.Error()
			statuses = append(statuses, st)
			continue
		}
		remote, err := client.ListTools(ctx)
		if err != nil {
			st.Error = "握手成功但拉取工具失败：" + err.Error()
			statuses = append(statuses, st)
			continue
		}
		names := make([]string, 0, len(remote))
		remoteNames := make([]string, 0, len(remote))
		var lines []string
		for _, rt := range remote {
			tool := NewMCPTool(cfg.ID, cfg.Name, alias, rt, func(ctx context.Context, args map[string]any) (mcp.CallResult, error) {
				return client.CallTool(ctx, rt.Name, args)
			})
			newly[tool.Name()] = tool
			owner[tool.Name()] = cfg.ID
			names = append(names, tool.Name())
			remoteNames = append(remoteNames, rt.Name)
			desc := strings.TrimSpace(rt.Description)
			if len([]rune(desc)) > 60 {
				desc = string([]rune(desc)[:60]) + "…"
			}
			lines = append(lines, fmt.Sprintf("- %s：%s", tool.Name(), desc))
		}
		sort.Strings(names)
		sort.Strings(remoteNames)
		sort.Strings(lines)
		st.OK = true
		st.Server = info.Name
		st.Version = info.Version
		st.ToolCount = len(names)
		st.Tools = names
		st.RemoteTools = remoteNames
		statuses = append(statuses, st)
		if len(lines) > 0 {
			part := fmt.Sprintf("【%s】\n%s", cfg.Name, strings.Join(lines, "\n"))
			hintParts = append(hintParts, part)
			hintByServer[cfg.ID] = part
		}
	}

	// 摘旧挂新：先摘上一轮挂的（含本轮已消失的、已禁用的），再挂本轮解析出来的。
	if m.reg != nil {
		m.reg.Remove(m.mounted...)
		keys := make([]string, 0, len(newly))
		for k := range newly {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		mounted := make([]string, 0, len(keys))
		for _, k := range keys {
			m.reg.Register(newly[k])
			mounted = append(mounted, k)
		}
		m.mounted = mounted
		// 按服务器切片只在**真挂上**之后记录：被 Remove 掉的名字留在切片里，
		// 会让「勾了这台」的提示词里出现一个注册表里并不存在的工具。
		byServer := map[string][]string{}
		for _, n := range mounted {
			if sid := owner[n]; sid != "" {
				byServer[sid] = append(byServer[sid], n)
			}
		}
		for _, names := range byServer {
			sort.Strings(names)
		}
		m.mountedByServer = byServer
		m.hintByServer = hintByServer
	}

	m.statuses = statuses
	if m.reg == nil {
		// 只探测不注册（后台「测试连接」走的是 TestMCP，但单测会这么构造）：
		// 门控无从谈起，可是提示词切片仍必须与 PromptHint 一致——
		// 两条路给出不一样的世界，是后面最难查的那种 bug。
		byServer := map[string][]string{}
		for n, sid := range owner {
			byServer[sid] = append(byServer[sid], n)
		}
		for _, names := range byServer {
			sort.Strings(names)
		}
		m.mountedByServer = byServer
		m.hintByServer = hintByServer
	}
	if len(hintParts) > 0 {
		m.hint = strings.Join(hintParts, "\n")
	} else {
		m.hint = ""
	}
	return statuses
}

// Statuses 返回最近一次 Refresh 的状态快照。
func (m *MCPManager) Statuses() []MCPServerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MCPServerStatus, len(m.statuses))
	copy(out, m.statuses)
	return out
}

// ToolNames 返回当前已挂进注册表的 MCP 工具名。
func (m *MCPManager) ToolNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.mounted))
	copy(out, m.mounted)
	return out
}

// PromptHint 返回「可用 MCP 工具清单」段落，供系统提示词拼接。
// 没有可用工具时返回空串，调用方不要写空段落。
func (m *MCPManager) PromptHint() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.TrimSpace(m.hint) == "" {
		return ""
	}
	return "以下是已接入的外部系统工具（MCP），涉及这些系统的问题必须优先用它们，不要自己编数据：\n" + m.hint
}

// PromptHintFor 返回**只含指定服务器**的工具清单段落。
//
// ids 为空 → 空串（用户一个都没勾，提示词里就不该提 MCP）。这条是「默认不调度」
// 在提示词层的落实：门控只拦工具表、不拦提示词的话，模型仍会满脑子想着去调
// 一个不存在的工具，表现是绕圈、变慢、最后编答案。
func (m *MCPManager) PromptHintFor(ids []string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(ids) == 0 {
		return ""
	}
	// 用 ids 的顺序而不是 map 顺序：同一批勾选必须产出同一份提示词，
	// 否则提示词缓存永远命中不了，每轮都是冷启动（已经吃过这个亏）。
	seen := map[string]bool{}
	var parts []string
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if p := m.hintByServer[id]; strings.TrimSpace(p) != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "以下是已接入的外部系统工具（MCP），涉及这些系统的问题必须优先用它们，不要自己编数据：\n" + strings.Join(parts, "\n")
}

// ServerTools 返回某台服务器已挂进注册表的工具名（门控自证用）。
func (m *MCPManager) ServerTools(id string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.mountedByServer[id]))
	copy(out, m.mountedByServer[id])
	return out
}

// MCPServerChoice 是**能出到公开接口**的服务器信息，就这三个字段。
//
// 刻意与 MCPServerStatus 分开而不是复用它：那个结构带 URL / 错误原文 / 远端
// 服务名，是给后台和有鉴权的自检用的。公开接口（站点访客可访问）一旦复用，
// 只要有人往后加个字段、或者序列化时直接 return 了状态对象，内网地址就出去了。
// 类型分开之后，这种事在编译期就被挡住——想漏也漏不出去。
type MCPServerChoice struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ToolCount int    `json:"tool_count"`
}

// Selectable 返回**用户可以勾选**的服务器：配置里启用、且本轮握手成功。
//
// 返回副本，调用方随便改；内网地址与报错原文在这里就被丢掉，不依赖调用方自觉。
func (m *MCPManager) Selectable() []MCPServerChoice {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MCPServerChoice, 0, len(m.statuses))
	for _, st := range m.statuses {
		if st.Enabled && st.OK {
			out = append(out, MCPServerChoice{
				ID: st.ID, Name: st.Name, ToolCount: st.ToolCount,
			})
		}
	}
	return out
}

// TestMCP 只做一次连接探测并返回状态，不碰注册表——后台「测试连接」用。
func TestMCP(ctx context.Context, cfg mcp.Config) MCPServerStatus {
	st := MCPServerStatus{
		ID: cfg.ID, Name: cfg.Name, URL: cfg.URL,
		Enabled: cfg.Enabled, CheckedAt: time.Now(),
	}
	client, err := mcp.NewClient(cfg)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	info, err := client.Initialize(ctx)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	remote, err := client.ListTools(ctx)
	if err != nil {
		st.Error = "握手成功但拉取工具失败：" + err.Error()
		return st
	}
	names := make([]string, 0, len(remote))
	remoteNames := make([]string, 0, len(remote))
	for _, rt := range remote {
		names = append(names, mcpLocalName(mcpAlias(cfg), rt.Name))
		remoteNames = append(remoteNames, rt.Name)
	}
	sort.Strings(names)
	sort.Strings(remoteNames)
	st.OK = true
	st.Server = info.Name
	st.Version = info.Version
	st.ToolCount = len(names)
	st.Tools = names
	st.RemoteTools = remoteNames
	return st
}
