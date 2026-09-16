package tools

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestExecLimitsAreOverridable 钉住「限额是兜底、不是业务规则」。
//
// 事故背景：单机离线部署的客户让模型写脚本解析一份 17MB 的 PDF，写死的 256M
// 会在几秒内被 cgroup OOM 掉；客户手上没有任何旋钮，只能改我们的源码。
// 这条测试保证四个旋钮都在，且**非法值退回默认而不是半懂地吃进去**。
func TestExecLimitsAreOverridable(t *testing.T) {
	cases := []struct {
		name                string
		env                 map[string]string
		wantTimeout         time.Duration
		wantMemory, wantCPU string
		wantTasks           int
	}{
		{
			name:        "不设任何变量 → 安全默认",
			wantTimeout: 30 * time.Second, wantMemory: "256M", wantCPU: "50%", wantTasks: 32,
		},
		{
			name:        "客户按文档调大内存与大超时",
			env:         map[string]string{EnvExecMemory: "1G", EnvExecTimeout: "5m", EnvExecCPU: "200%", EnvExecTasks: "128"},
			wantTimeout: 5 * time.Minute, wantMemory: "1G", wantCPU: "200%", wantTasks: 128,
		},
		{
			name:        "裸数字按秒算（客户最容易这么写）",
			env:         map[string]string{EnvExecTimeout: "120"},
			wantTimeout: 120 * time.Second, wantMemory: "256M", wantCPU: "50%", wantTasks: 32,
		},
		{
			name:        "小写单位能认（1g 不该被当成非法值丢掉）",
			env:         map[string]string{EnvExecMemory: "1g"},
			wantTimeout: 30 * time.Second, wantMemory: "1G", wantCPU: "50%", wantTasks: 32,
		},
		{
			// 内存写百分数在沙箱兜底语义下没意义：退回默认，不做半懂的翻译。
			name:        "非法内存写法 → 退回默认",
			env:         map[string]string{EnvExecMemory: "50%"},
			wantTimeout: 30 * time.Second, wantMemory: "256M", wantCPU: "50%", wantTasks: 32,
		},
		{
			name:        "内存小于 64M（连解释器都起不来）→ 退回默认",
			env:         map[string]string{EnvExecMemory: "32M"},
			wantTimeout: 30 * time.Second, wantMemory: "256M", wantCPU: "50%", wantTasks: 32,
		},
		{
			name:        "超时超上限（>30m 该走后台任务）→ 退回默认",
			env:         map[string]string{EnvExecTimeout: "2h"},
			wantTimeout: 30 * time.Second, wantMemory: "256M", wantCPU: "50%", wantTasks: 32,
		},
		{
			name:        "超时太短（5s 以下解释器都起不来）→ 退回默认",
			env:         map[string]string{EnvExecTimeout: "1s"},
			wantTimeout: 30 * time.Second, wantMemory: "256M", wantCPU: "50%", wantTasks: 32,
		},
		{
			name:        "CPU 不写成百分数 → 退回默认",
			env:         map[string]string{EnvExecCPU: "0.5"},
			wantTimeout: 30 * time.Second, wantMemory: "256M", wantCPU: "50%", wantTasks: 32,
		},
		{
			name:        "任务数写成非数字 → 退回默认",
			env:         map[string]string{EnvExecTasks: "many"},
			wantTimeout: 30 * time.Second, wantMemory: "256M", wantCPU: "50%", wantTasks: 32,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, k := range []string{EnvExecTimeout, EnvExecMemory, EnvExecCPU, EnvExecTasks} {
				t.Setenv(k, "")
			}
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			got := DefaultExecConfig()
			if got.Timeout != c.wantTimeout {
				t.Errorf("Timeout=%s，期望 %s", got.Timeout, c.wantTimeout)
			}
			if got.MemoryMax != c.wantMemory {
				t.Errorf("MemoryMax=%q，期望 %q", got.MemoryMax, c.wantMemory)
			}
			if got.CPUQuota != c.wantCPU {
				t.Errorf("CPUQuota=%q，期望 %q", got.CPUQuota, c.wantCPU)
			}
			if got.TasksMax != c.wantTasks {
				t.Errorf("TasksMax=%d，期望 %d", got.TasksMax, c.wantTasks)
			}
		})
	}
}

// TestToolDescriptionTracksEffectiveLimits 钉住「模型看到的限额 = 生效的限额」。
//
// 为什么单列一条：旋钮调大之后如果工具描述还是钉死的旧值，模型会继续拒绝写
// 「整份读进来算」的脚本 —— 客户调了旋钮、行为却一点没变，而这在日志里完全看不出来
// （描述是发给模型的一段文字，不落在任何日志里）。所以描述必须跟着 cfg 走。
func TestToolDescriptionTracksEffectiveLimits(t *testing.T) {
	for _, k := range []string{EnvExecTimeout, EnvExecMemory, EnvExecCPU, EnvExecTasks} {
		t.Setenv(k, "")
	}
	// 默认值：描述里必须出现三段真实数字（钉住人话格式，别退回 "30s"）。
	def := NewRunPythonTool(DefaultExecConfig()).Description()
	for _, want := range []string{"30 秒", "256M", "32 进程"} {
		if !strings.Contains(def, want) {
			t.Errorf("默认描述里没有 %q：\n%s", want, def)
		}
	}

	// 客户调大之后：描述必须跟着变，且**不能**再出现旧值。
	t.Setenv(EnvExecMemory, "1G")
	t.Setenv(EnvExecTimeout, "5m")
	t.Setenv(EnvExecTasks, "128")
	big := NewRunPythonTool(DefaultExecConfig()).Description()
	for _, want := range []string{"1G", "5 分钟", "128 进程"} {
		if !strings.Contains(big, want) {
			t.Errorf("调大限额后描述里没有 %q：\n%s", want, big)
		}
	}
	for _, stale := range []string{"256M", "30 秒", "32 进程"} {
		if strings.Contains(big, stale) {
			t.Errorf("调大限额后描述里还有旧值 %q —— 模型会按旧限额自我设限：\n%s", stale, big)
		}
	}

	// 非整分钟要写成 "90 秒"，而不是 "1m30s" 这种模型容易看错的半截值。
	t.Setenv(EnvExecTimeout, "90s")
	mid := NewRunPythonTool(DefaultExecConfig()).Description()
	if !strings.Contains(mid, "90 秒") {
		t.Errorf("90s 应写成人话 90 秒：\n%s", mid)
	}
}

// TestSandboxFailureSaysWhy 钉住「失败要说得出原因」。
//
// 事故背景：改造前所有非超时的失败都挤成一句「退出码 -1」，模型只能猜着重试，
// 用户在界面上看到的就是「卡着计时」。这条测试要求：
//
//	· OOM 现场必须点名内存上限 + 给出调大的旋钮（而不是让模型去改代码骗过限额）；
//	· 非 OOM 的普通报错**不许**被说成 OOM（诊断错方向比不诊断更坏）；
//	· 真语法错误仍要提示按 stderr 修正。
func TestSandboxFailureSaysWhy(t *testing.T) {
	cfg := ExecConfig{Timeout: 30 * time.Second, MemoryMax: "256M"}
	cases := []struct {
		name       string
		exit       int
		out        string
		timedOut   bool
		wantStatus []string // 必须全部命中
		wantHint   []string
		denyStatus []string // 必须一个都不命中
		denyHint   []string
	}{
		{
			name:       "cgroup OOM（内核 oom-kill 行）",
			exit:       137,
			out:        "Killed\nMemory cgroup out of memory: Killed process 12345 (python3)",
			wantStatus: []string{"内存上限", "256M"},
			wantHint:   []string{"SKILLFORGE_EXEC_MEMORY", "1G"},
		},
		{
			name:       "systemd 把 unit 结果报成 oom-kill",
			exit:       137,
			out:        "unit run-abc.service: failed with result 'oom-kill'",
			wantStatus: []string{"内存上限"},
			wantHint:   []string{"SKILLFORGE_EXEC_MEMORY"},
		},
		{
			// python 自己抛的 MemoryError 与 cgroup 杀进程是两种现场：修法不同
			// （改代码分块 vs 调限额），所以状态行必须区分开，不能都说成「被上限杀掉」。
			name:       "python 自己抛 MemoryError（没被 cgroup 杀）",
			exit:       1,
			out:        "Traceback (most recent call last):\nMemoryError",
			wantStatus: []string{"脚本内存不足", "不是被沙箱上限杀掉"},
			wantHint:   []string{"流式", "分页"},
			denyStatus: []string{"MemoryMax="},
		},
		{
			// 关键反例：普通语法错必须仍然走「改代码」，绝不能被误诊成内存问题。
			name:       "普通语法错误不许冒充 OOM",
			exit:       1,
			out:        "Traceback (most recent call last):\n  File \"main.py\", line 3\nSyntaxError: invalid syntax",
			wantStatus: []string{"退出码 1"},
			denyStatus: []string{"内存"},
			denyHint:   []string{"SKILLFORGE_EXEC_MEMORY"},
		},
		{
			// 另一个反例：超时必须说超时，且点名超时的旋钮（别让人去调内存）。
			name: "超时不许冒充 OOM",
			exit: 137, timedOut: true, out: "Timeout",
			wantStatus: []string{"超时"},
			wantHint:   []string{"SKILLFORGE_EXEC_TIMEOUT"},
			denyHint:   []string{"SKILLFORGE_EXEC_MEMORY"},
		},
		{
			// 沙箱自身没起来：模型改代码永远修不好，必须明说是环境问题。
			name: "沙箱未启动不许让模型改代码重试",
			exit: -1, out: "(无输出)",
			wantStatus: []string{"沙箱"},
			wantHint:   []string{"selftest", "不要靠改代码"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, hint := classifySandboxFailure(c.exit, c.out, c.timedOut, cfg)
			for _, w := range c.wantStatus {
				if !strings.Contains(status, w) {
					t.Errorf("status=%q，应含 %q", status, w)
				}
			}
			for _, w := range c.wantHint {
				if !strings.Contains(hint, w) {
					t.Errorf("hint=%q，应含 %q", hint, w)
				}
			}
			for _, d := range c.denyStatus {
				if strings.Contains(status, d) {
					t.Errorf("status=%q，不该含 %q（诊断跑偏）", status, d)
				}
			}
			for _, d := range c.denyHint {
				if strings.Contains(hint, d) {
					t.Errorf("hint=%q，不该含 %q（把人引到错的旋钮上）", hint, d)
				}
			}
		})
	}
}

// TestOOMGateHasNoFalsePositives 单独把 OOM 探针钉一遍：它是「说得出原因」的地基，
// 一旦放宽（比如改成 strings.Contains(out, "memory") 这种），上面那条测试里的
// 语法错反例还会过，但真实日志里带 memory 字样的正常报错就会被误诊。
func TestOOMGateHasNoFalsePositives(t *testing.T) {
	normalLogs := []string{
		"SyntaxError: invalid syntax",
		"ValueError: memory view has 1 exported buffer",
		"processed in-memory buffer of 1024 rows",   // 正文里出现 in-memory
		"numpy.core._exceptions._ArrayMemoryError",  // 名字里带 Memory 但不是 OOM 判据
		"Killed process by our own SIGTERM handler", // 自己杀的，不是 OOM
	}
	for _, l := range normalLogs {
		if looksLikeOOM(l) {
			t.Errorf("误判成 OOM：%q", l)
		}
	}
	oomLogs := []string{
		"Memory cgroup out of memory: Killed process 4711 (python3) total-vm:2000000kB",
		"run-xyz.service: Failed with result 'oom-kill'.",
		"python3: cannot allocate memory",
	}
	for _, l := range oomLogs {
		if !looksLikeOOM(l) {
			t.Errorf("漏判 OOM：%q", l)
		}
	}
	// 解释器抛的 MemoryError 归另一条探针，不能落进「被上限杀掉」这一桶。
	if looksLikeOOM("Traceback (most recent call last):\nMemoryError") {
		t.Error("python 的 MemoryError 不该被判成 cgroup OOM")
	}
	if !looksLikePythonMemoryError("Traceback (most recent call last):\nMemoryError") {
		t.Error("python 的 MemoryError 漏判")
	}
	// numpy 的类名带 MemoryError 子串，但它不是解释器的 MemoryError。
	if looksLikePythonMemoryError("numpy.core._exceptions._ArrayMemoryError: Unable to allocate 512 MiB") {
		t.Error("numpy 的 _ArrayMemoryError 被误判成 python MemoryError（需要词边界）")
	}
}

// TestRunReportsSandboxOOMToModel 走**真实 Run() 路径**，钉住「模型最终读到的那几个字」。
//
// 为什么必须走 Run() 而不是再测一遍 classifySandboxFailure：
// 诊断函数自己是对的，但**模型读到的是调用点拼出来的那段话**。有人把调用点换成
// 一句 `fmt.Sprintf("退出码 %d", exitCode)`（这正是改造前的历史写法）时，单测诊断函数
// 依然全绿——而客户界面上又回到「一动不动跳秒 + 模型瞎猜重试」。
// 这条负向自证（exec_limits_mutation_check.py 第 3 例）就是这么抓出来的：
// 它把调用点改回历史写法，旧尺子没红，于是补了这条端到端断言。
//
// 用一个假 systemd-run 顶替真沙箱：Run() 只通过 PATH 找它，且判据只看它打印的日志串，
// 所以钉住「日志里有 OOM 证据 → 模型拿到 OOM 诊断 + 改哪个变量」不需要真起 cgroup。
func TestRunReportsSandboxOOMToModel(t *testing.T) {
	for _, k := range []string{EnvExecTimeout, EnvExecMemory, EnvExecCPU, EnvExecTasks} {
		t.Setenv(k, "")
	}
	// 客户按装机文档调大了内存：诊断里必须体现**生效值**，不能还是写死的 256M。
	t.Setenv(EnvExecMemory, "1G")

	bin := t.TempDir()
	fake := filepath.Join(bin, "systemd-run")
	// 内核 OOM 现场的真串。刻意不含 Timeout/timeout：Run() 用这两个子串判超时，
	// 混进去会把 OOM 现场误判成超时。
	logLine := "Memory cgroup out of memory: Killed process 4711 (python3) total-vm:2000000kB, anon-rss:1500000kB\n"
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s' '"+logLine+"' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("写假 systemd-run 失败：%v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	me, err := user.Current()
	if err != nil {
		t.Fatalf("取当前用户失败：%v", err)
	}
	cfg := DefaultExecConfig()
	cfg.WorkRoot = t.TempDir()
	// 降权到「当前用户」：测试里没有沙箱用户也照样能建工作区（chown 到自己总是允许），
	// 于是这条断言不依赖运行测试的人是不是 root，也就不会在 CI 里被跳过。
	cfg.SandboxUser = me.Username

	res, err := NewRunPythonTool(cfg).Run(context.Background(), map[string]any{"code": "print(1)"})
	if err != nil {
		t.Fatalf("Run 返回错误（说明沙箱前置检查挡住或工作区没建起来）：%v", err)
	}

	for _, want := range []string{
		"被内存上限杀掉",      // ① 类别说清了
		"MemoryMax=1G", // ② 用的是生效值，不是写死的 256M
		EnvExecMemory,  // ③ 告诉人改哪个变量
	} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("模型读到的诊断里缺 %q：\n%s", want, res.Content)
		}
	}
	// 反面：不许退回历史那句没有信息量的组合（模型只能原样重试）。
	for _, bad := range []string{"退出码 1", "请根据上面的 stderr 修正后重试"} {
		if strings.Contains(res.Content, bad) {
			t.Errorf("诊断退回历史写法（%q）——模型会瞎猜重试：\n%s", bad, res.Content)
		}
	}
	if res.Display == "" {
		t.Error("Display 为空：工具链在 UI 上看不见这次失败")
	}
}
