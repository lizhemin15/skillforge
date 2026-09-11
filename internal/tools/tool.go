// Package tools 提供 Agent 可调用的工具：注册表 + 内置工具实现。
//
// 设计原则：工具是「数据」（名字 + 描述 + JSON Schema + 执行函数），不是散落在
// 业务代码里的 if/else 分支。新增能力 = 注册一个 Tool，不用改循环、不用改接口层。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// File 是工具产出的、需要交付给用户的文件（文档/表格/图片等）。
type File struct {
	Name        string
	ContentType string
	Bytes       []byte
}

// Result 是工具执行结果。
type Result struct {
	Content string // 回给模型的文本（模型据此决定下一步）
	Files   []File // 交付给用户的文件
	Display string // UI 上显示的一行摘要（让工具链「看得见」）
}

// Tool 是一个可被模型调用的能力。
type Tool interface {
	Name() string
	Description() string    // 决定模型会不会用对，写清楚「什么时候用、参数怎么给」
	Schema() map[string]any // JSON Schema：required 必须是数组
	Run(ctx context.Context, args map[string]any) (Result, error)
}

// Registry 是工具注册表。
type Registry struct {
	mu    sync.RWMutex
	items map[string]Tool
}

// NewRegistry 建一个空注册表。
func NewRegistry() *Registry {
	return &Registry{items: map[string]Tool{}}
}

// Register 注册工具（重名覆盖，便于测试替身）。
func (r *Registry) Register(t Tool) {
	if t == nil || t.Name() == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items[t.Name()] = t
}

// Get 按名取工具。
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.items[name]
	return t, ok
}

// All 返回全部工具（按名字排序，保证 prompt 里顺序稳定、可测试）。
func (r *Registry) All() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.items))
	for n := range r.items {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]Tool, 0, len(names))
	for _, n := range names {
		out = append(out, r.items[n])
	}
	return out
}

// Names 返回工具名列表。
func (r *Registry) Names() []string {
	out := make([]string, 0)
	for _, t := range r.All() {
		out = append(out, t.Name())
	}
	return out
}

// Execute 执行一次工具调用。任何 panic 都被收成 error——
// 工具是「用户可间接驱动」的代码路径，不能让它拖垮整个服务进程。
func (r *Registry) Execute(ctx context.Context, name string, args map[string]any) (res Result, err error) {
	t, ok := r.Get(name)
	if !ok {
		return Result{}, fmt.Errorf("未知工具 %q（可用: %s）", name, strings.Join(r.Names(), ", "))
	}
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("工具 %s 内部错误: %v", name, p)
			res = Result{}
		}
	}()
	return t.Run(ctx, args)
}

// ---- 参数读取helper：模型给的 JSON 类型不总符合预期，读参数一律走这里，避免 panic ----

// Str 读字符串参数。
func Str(args map[string]any, key string) string {
	v, ok := args[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// Int 读整数参数（容忍 float64 与数字字符串，模型经常给 "5" 或 5.0）。
func Int(args map[string]any, key string, def int) int {
	v, ok := args[key]
	if !ok || v == nil {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	case string:
		var i int
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%d", &i); err == nil {
			return i
		}
	}
	return def
}

// StrMap 读字符串键值表（headers 之类）。
func StrMap(args map[string]any, key string) map[string]string {
	v, ok := args[key]
	if !ok || v == nil {
		return nil
	}
	out := map[string]string{}
	switch m := v.(type) {
	case map[string]any:
		for k, vv := range m {
			out[k] = fmt.Sprint(vv)
		}
	case map[string]string:
		for k, vv := range m {
			out[k] = vv
		}
	}
	return out
}

// Truncate 截断过长文本（工具输出直接进 prompt，不截断会撑爆上下文）。
func Truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n...[已截断，原长 %d 字节]", len(s))
}
