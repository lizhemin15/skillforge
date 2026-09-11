package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeTool 是测试替身：记录收到的参数，可指定返回/panic。
type fakeTool struct {
	name    string
	gotArgs map[string]any
	content string
	files   []File
	err     error
	panics  bool
}

func (f *fakeTool) Name() string        { return f.name }
func (f *fakeTool) Description() string { return "测试用工具" }
func (f *fakeTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"text": map[string]any{"type": "string"},
	}}
}
func (f *fakeTool) Run(_ context.Context, args map[string]any) (Result, error) {
	f.gotArgs = args
	if f.panics {
		panic("工具内部炸了")
	}
	return Result{Content: f.content, Files: f.files, Display: "ok"}, f.err
}

func TestRegistryRegisterAndLookup(t *testing.T) {
	r := NewRegistry()
	a := &fakeTool{name: "alpha"}
	b := &fakeTool{name: "beta"}
	r.Register(a)
	r.Register(b)

	if _, ok := r.Get("alpha"); !ok {
		t.Fatal("alpha 应能取到")
	}
	if _, ok := r.Get("nope"); ok {
		t.Fatal("未注册的工具不该取到")
	}
	// 顺序必须稳定，否则 prompt 里的工具列表每次都不一样（不可复现）
	got := r.Names()
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Fatalf("工具顺序应稳定排序，实际 %v", got)
	}
	// 空名工具应被忽略（防止注册出模型永远调不到、又出现在列表里的幽灵工具）
	r.Register(&fakeTool{name: ""})
	if len(r.Names()) != 2 {
		t.Fatalf("空名工具应被忽略，实际 %v", r.Names())
	}
}

func TestRegistryExecuteUnknown(t *testing.T) {
	r := NewRegistry()
	_, err := r.Execute(context.Background(), "ghost", nil)
	if err == nil {
		t.Fatal("调用未注册工具必须报错")
	}
	// 报错信息要能指导模型改用正确工具名
	if !strings.Contains(err.Error(), "未知工具") {
		t.Fatalf("错误信息应说明是未知工具: %v", err)
	}
}

// 工具是「用户可间接驱动」的路径，panic 不能拖垮整个服务进程。
func TestRegistryRecoversPanic(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeTool{name: "boom", panics: true})
	_, err := r.Execute(context.Background(), "boom", nil)
	if err == nil || !strings.Contains(err.Error(), "内部错误") {
		t.Fatalf("panic 应被收敛为 error，实际 %v", err)
	}
}

func TestRegistryPassesArgs(t *testing.T) {
	r := NewRegistry()
	f := &fakeTool{name: "echo", content: "hi"}
	r.Register(f)
	res, err := r.Execute(context.Background(), "echo", map[string]any{"text": "你好"})
	if err != nil {
		t.Fatal(err)
	}
	if f.gotArgs["text"] != "你好" || res.Content != "hi" {
		t.Fatalf("参数应原样传给工具，实际 %v / %q", f.gotArgs, res.Content)
	}
}

func TestArgHelpersTolerateModelSlop(t *testing.T) {
	args := map[string]any{
		"f":      float64(5), // JSON 数字一律是 float64
		"n":      "7",        // 模型经常把数字写成字符串
		"absent": nil,
		"s":      "x",
		"h":      map[string]any{"A": "1"},
	}
	if got := Int(args, "f", 0); got != 5 {
		t.Fatalf("float64 → int 失败: %d", got)
	}
	if got := Int(args, "n", 0); got != 7 {
		t.Fatalf("数字字符串 → int 失败: %d", got)
	}
	if got := Int(args, "absent", 42); got != 42 {
		t.Fatalf("缺失参数应用默认值: %d", got)
	}
	if got := Int(args, "s", 9); got != 9 {
		t.Fatalf("非数字应回落默认值: %d", got)
	}
	if got := StrMap(args, "h")["A"]; got != "1" {
		t.Fatalf("StrMap 解析失败: %v", got)
	}
	if got := StrMap(args, "absent"); got != nil {
		t.Fatalf("缺失 map 应为 nil: %v", got)
	}
}

// 工具输出直接进 prompt，不截断会撑爆上下文。
func TestTruncate(t *testing.T) {
	if got := Truncate("abcdef", 3); !strings.HasPrefix(got, "abc") || !strings.Contains(got, "已截断") {
		t.Fatalf("截断结果不对: %q", got)
	}
	if got := Truncate("abc", 100); got != "abc" {
		t.Fatalf("短文本不该被改: %q", got)
	}
	if got := Truncate("abc", 0); got != "abc" {
		t.Fatalf("上限 0 表示不限制: %q", got)
	}
}

func TestExecutePropagatesToolError(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeTool{name: "bad", err: errors.New("上游 500")})
	_, err := r.Execute(context.Background(), "bad", nil)
	if err == nil || !strings.Contains(err.Error(), "上游 500") {
		t.Fatalf("工具错误应向上传递: %v", err)
	}
}
