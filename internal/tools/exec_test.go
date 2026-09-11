package tools

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// sandboxUsable 探一次沙箱是否真的可用。CI（GitHub runner）里没有可用的 systemd
// 沙箱，此时必须跳过而不是失败——否则真正的安全回归测试会被 CI 噪音淹没。
func sandboxUsable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("systemd-run"); err != nil {
		t.Skip("无 systemd-run，跳过沙箱集成测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemd-run", "--pipe", "--wait", "--collect", "--quiet", "/bin/echo", "ok").Output()
	if err != nil {
		t.Skipf("systemd-run 不可用（%v），跳过沙箱集成测试", err)
	}
	if !strings.Contains(string(out), "ok") {
		t.Skip("systemd-run 无法回传输出，跳过")
	}
	if err := os.MkdirAll(DefaultExecConfig().WorkRoot, 0o711); err != nil {
		t.Skipf("无法创建工作区根目录（需 root）: %v", err)
	}
}

func newTestExecTool() *RunPythonTool {
	cfg := DefaultExecConfig()
	cfg.Timeout = 15 * time.Second
	return NewRunPythonTool(cfg)
}

// 最基本：代码能跑、能拿到输出。
func TestRunPythonBasic(t *testing.T) {
	sandboxUsable(t)
	tool := newTestExecTool()
	res, err := tool.Run(context.Background(), map[string]any{
		"code": "print('结果', 6*7)\nimport json; print(json.dumps({'a':[1,2,3]}))",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "结果 42") || !strings.Contains(res.Content, `"a": [1, 2, 3]`) {
		t.Fatalf("应拿到代码输出: %s", res.Content)
	}
	if !strings.Contains(res.Content, "退出码 0") {
		t.Fatalf("应报告退出码: %s", res.Content)
	}
}

// 核心安全断言：即使服务以 root 运行，沙箱里的代码也必须是降权身份。
func TestRunPythonDropsPrivileges(t *testing.T) {
	sandboxUsable(t)
	tool := newTestExecTool()
	res, err := tool.Run(context.Background(), map[string]any{
		"code": "import os\nprint('UID=%d' % os.getuid())\nprint('HOME=%s' % os.environ.get('HOME',''))",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Content, "UID=0") {
		t.Fatalf("沙箱里是 root！降权失效: %s", res.Content)
	}
	if !strings.Contains(res.Content, "UID=65534") {
		t.Fatalf("应降权到 nobody(65534): %s", res.Content)
	}
}

// 核心安全断言：沙箱无网络（联网必须走可审计的 http_request）。
func TestRunPythonHasNoNetwork(t *testing.T) {
	sandboxUsable(t)
	tool := newTestExecTool()
	res, err := tool.Run(context.Background(), map[string]any{
		"code": "import socket\ntry:\n    socket.create_connection(('1.1.1.1',80),timeout=3)\n    print('NET_OPEN')\nexcept Exception as e:\n    print('NET_BLOCKED', type(e).__name__)",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Content, "NET_OPEN") {
		t.Fatalf("沙箱竟能联网: %s", res.Content)
	}
	if !strings.Contains(res.Content, "NET_BLOCKED") {
		t.Fatalf("应报告网络被拦: %s", res.Content)
	}
}

// 核心安全断言：读不到服务自己的密钥/数据库/会话记录。
func TestRunPythonCannotReadSecrets(t *testing.T) {
	sandboxUsable(t)
	tool := newTestExecTool()
	code := `
paths = ["/opt/skillforge/skillforge.env", "/opt/skillforge/data/skillforge.db", "/root/.ssh/id_rsa"]
for p in paths:
    try:
        open(p, "rb").read(8); print("READABLE", p)
    except Exception:
        print("DENIED", p)
`
	res, err := tool.Run(context.Background(), map[string]any{"code": code})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Content, "READABLE") {
		t.Fatalf("沙箱能读到敏感文件！: %s", res.Content)
	}
	if !strings.Contains(res.Content, "DENIED") {
		t.Fatalf("应报告拒绝访问: %s", res.Content)
	}
}

// 核心安全断言：写不出工作区（不能改部署文件、不能往 /etc 塞东西）。
func TestRunPythonCannotWriteOutsideWorkspace(t *testing.T) {
	sandboxUsable(t)
	tool := newTestExecTool()
	code := `
for p in ["/opt/skillforge/pwned.txt", "/etc/pwned.txt", "/root/pwned.txt"]:
    try:
        open(p, "w").write("x"); print("WROTE", p)
    except Exception:
        print("DENIED", p)
open("inside.txt","w").write("ok"); print("WROTE_INSIDE")
`
	res, err := tool.Run(context.Background(), map[string]any{"code": code})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Content, "WROTE /") {
		t.Fatalf("沙箱写出了工作区！: %s", res.Content)
	}
	if !strings.Contains(res.Content, "WROTE_INSIDE") {
		t.Fatalf("工作区内应可写: %s", res.Content)
	}
}

// 超时必须真的被杀掉（死循环不能挂住服务）。
func TestRunPythonTimeout(t *testing.T) {
	sandboxUsable(t)
	cfg := DefaultExecConfig()
	cfg.Timeout = 3 * time.Second
	tool := NewRunPythonTool(cfg)

	start := time.Now()
	res, err := tool.Run(context.Background(), map[string]any{"code": "while True:\n    pass"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "超时") {
		t.Fatalf("死循环应被判为超时: %s", res.Content)
	}
	if elapsed > 25*time.Second {
		t.Fatalf("超时控制失效，耗时 %s", elapsed)
	}
	// 回归防线：超时后不能留孤儿进程。Go 侧只杀 systemd-run，不 kill unit 的话
	// 沙箱里的 python 会继续跑下去（正是这轮修掉的 bug）。
	time.Sleep(700 * time.Millisecond)
	leftover, _ := exec.Command("pgrep", "-f", "exec-[0-9a-f]+/main.py").Output()
	if strings.TrimSpace(string(leftover)) != "" {
		t.Fatalf("超时后仍有沙箱进程存活（孤儿）: %s", strings.TrimSpace(string(leftover)))
	}
	units, _ := exec.Command("systemctl", "list-units", "--all", "--no-legend", "sfexec-*").Output()
	if strings.TrimSpace(string(units)) != "" {
		t.Fatalf("超时后 systemd 里残留 unit: %s", strings.TrimSpace(string(units)))
	}
}

// 内存限额要拦住内存炸弹。
func TestRunPythonMemoryLimit(t *testing.T) {
	sandboxUsable(t)
	tool := newTestExecTool()
	res, err := tool.Run(context.Background(), map[string]any{
		"code": "b = bytearray(800*1024*1024)\nprint('ALLOCATED', len(b))",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Content, "ALLOCATED") {
		t.Fatalf("256MB 限额没拦住 800MB 分配: %s", res.Content)
	}
}

// 产出文件要能自动交付给用户（这是「写代码生成报表」的关键一环）。
func TestRunPythonCollectsArtifacts(t *testing.T) {
	sandboxUsable(t)
	tool := newTestExecTool()
	res, err := tool.Run(context.Background(), map[string]any{
		"code": "open('报表.csv','w').write('区域,金额\\n华东,100\\n')\nopen('note.txt','w').write('hi')",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("应收集到 2 个产出文件，实际 %d", len(res.Files))
	}
	names := map[string]bool{}
	for _, f := range res.Files {
		names[f.Name] = true
	}
	if !names["报表.csv"] || !names["note.txt"] {
		t.Fatalf("文件名不对: %v", names)
	}
	for _, f := range res.Files {
		if f.Name == "报表.csv" && !strings.Contains(f.ContentType, "csv") {
			t.Fatalf("csv 的 Content-Type 不对: %s", f.ContentType)
		}
	}
}

// 语法错误要如实回报（让模型能自我修正），而不是当成工具崩溃。
func TestRunPythonSyntaxError(t *testing.T) {
	sandboxUsable(t)
	tool := newTestExecTool()
	res, err := tool.Run(context.Background(), map[string]any{"code": "def broken(:\n  pass"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "SyntaxError") {
		t.Fatalf("应把 SyntaxError 回给模型: %s", res.Content)
	}
	if !strings.Contains(res.Content, "修正后重试") {
		t.Fatalf("应提示模型修正: %s", res.Content)
	}
}

// 空代码必须直接拒绝，不能白起一个沙箱进程。
func TestRunPythonEmptyCode(t *testing.T) {
	tool := newTestExecTool()
	if _, err := tool.Run(context.Background(), map[string]any{"code": "   "}); err == nil {
		t.Fatal("空 code 应报错")
	}
}

func TestSandboxDiagnosticsReportsSafe(t *testing.T) {
	sandboxUsable(t)
	diag := SandboxDiagnostics(context.Background())
	if diag["error"] != "" {
		t.Fatalf("自检失败: %s", diag["error"])
	}
	if diag["uid"] != "65534" {
		t.Fatalf("自检应报告降权 uid: %v", diag)
	}
	if !strings.Contains(diag["network"], "断") {
		t.Fatalf("自检应报告网络已断: %v", diag)
	}
	for k, v := range diag {
		if strings.Contains(k, "read:") && strings.Contains(v, "可读") {
			t.Fatalf("自检发现敏感文件可读: %s=%s", k, v)
		}
		if strings.Contains(k, "write:") && strings.Contains(v, "可写") {
			t.Fatalf("自检发现可写敏感路径: %s=%s", k, v)
		}
	}
}
