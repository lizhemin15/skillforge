package tools

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestSystemdArgsNoHighVersionCLIFlags 是本轮的**真 bug 回归闸门**。
//
// 现场（Bug Y2）：离线包装在 AlmaLinux 8（systemd 239）上，装后自检报
// 「代码执行沙箱 失败 —— 探针的 python 解释器根本没跑起来」，
// 而包内自带的解释器其实好好的（nobody 都能跑）。真原因是 systemdArgs 里用了
//
//	--working-directory=<dir>
//
// 这个 CLI 选项是 systemd v243 才加的，239 上 systemd-run 直接
// `unrecognized option` 退出、探针一个字都不吐 → 被上层误解成「目标机缺 python3」。
// 后果不止自检假红：**「执行代码」工具在 RHEL/AlmaLinux 8 系机器上从来没成功过一次**。
//
// 这个断言锁两件事：
//  1. 不许再出现 --working-directory（高版本 CLI 选项）；
//  2. 工作目录必须用 239 就支持的 --property=WorkingDirectory= 表达。
func TestSystemdArgsNoHighVersionCLIFlags(t *testing.T) {
	tool := NewRunPythonTool(ExecConfig{Python: "/usr/bin/python3"})
	args := tool.systemdArgs("/var/lib/skillforge/work/exec-abc", "sfexec-abc")

	for _, a := range args {
		if strings.HasPrefix(a, "--working-directory") {
			t.Fatalf("systemdArgs 出现 --working-directory=%q：该选项 systemd v243 才有，"+
				"AlmaLinux/RHEL 8（systemd 239）上 systemd-run 会 unrecognized option 直接退出，"+
				"执行代码工具与装后自检全挂", a)
		}
	}

	want := "--property=WorkingDirectory=/var/lib/skillforge/work/exec-abc"
	found := false
	for _, a := range args {
		if a == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("systemdArgs 缺少 %q（工作目录在 239 上只能用属性表达）；实际参数：%v", want, args)
	}
}

// TestSystemdArgsPropertiesSupportedBy239 把参数面锁成白名单。
//
// 为什么值得钉：本工具的故障模式是「开发机（systemd 249+）全绿、客户机（239）全挂」，
// 而这种不兼容**只表现为一句 unrecognized option，不会在 CI 里红**。
// 白名单里的每一项都在 systemd 239 (239-82.el8) 真机跑通过
// （现场脚本 /out/run3.sh + 容器 almalinux:8）。
func TestSystemdArgsPropertiesSupportedBy239(t *testing.T) {
	// 239 上确实支持的 systemd-run 通用选项。
	okFlags := map[string]bool{
		"--pipe": true, "--wait": true, "--collect": true, "--quiet": true,
	}
	// 239 上确实支持的属性名（用 -p/--property 下发）。
	okProps := map[string]bool{
		"User": true, "Group": true, "PrivateNetwork": true, "ProtectSystem": true,
		"ProtectHome": true, "NoNewPrivileges": true, "MemoryMax": true,
		"CPUQuota": true, "TasksMax": true, "SystemCallFilter": true,
		"RuntimeMaxSec": true, "ReadWritePaths": true, "WorkingDirectory": true,
	}
	// 显式记下「千万不要加」的：PrivateTmp 在目标容器环境 exit 200（已实测），
	// 以及所有只有新 systemd 才有的 CLI 快捷选项。
	forbiddenFlags := map[string]bool{
		"--working-directory": true, "--property=PrivateTmp=yes": true,
		"--same-dir": true, "--working-directory=/": false,
	}

	tool := NewRunPythonTool(ExecConfig{Python: "/usr/bin/python3"})
	args := tool.systemdArgs("/w", "u")

	for _, a := range args {
		if forbiddenFlags[a] {
			t.Errorf("systemdArgs 用了被明令禁止的参数：%s", a)
		}
		switch {
		case strings.HasPrefix(a, "--property="):
			kv := strings.TrimPrefix(a, "--property=")
			key := kv
			if i := strings.Index(kv, "="); i > 0 {
				key = kv[:i]
			}
			if !okProps[key] {
				t.Errorf("属性 %q 不在 systemd 239 白名单里（先确认目标机 systemd 版本再加）", key)
			}
		case strings.HasPrefix(a, "--setenv="), strings.HasPrefix(a, "--unit="):
			// 239 支持，值随现场变化，不校验内容。
		case okFlags[a]:
			// 通用选项，放行。
		case strings.HasPrefix(a, "-"):
			t.Errorf("未知的长选项 %q：请先确认它在 systemd 239 上存在，或改成 --property=", a)
		}
	}
}

// TestSandboxNeverBareRuns 守住「沙箱不可用就拒绝执行」这条底线：
// 参数里必须带上降权用户与断网属性，缺任何一条都不能算沙箱。
func TestSandboxNeverBareRuns(t *testing.T) {
	tool := NewRunPythonTool(ExecConfig{Python: "/usr/bin/python3", SandboxUser: "nobody"})
	joined := strings.Join(tool.systemdArgs("/w", "u"), " ")
	for _, must := range []string{
		"--property=User=nobody",
		"--property=PrivateNetwork=yes",
		"--property=ProtectSystem=strict",
		"--property=NoNewPrivileges=yes",
	} {
		if !strings.Contains(joined, must) {
			t.Errorf("沙箱参数缺少 %q：这是安全底线，缺了就等于裸跑", must)
		}
	}
}

// TestProbeFailureDetailSystemdRunError 锁「归因要说真话」。
//
// 历史：探针没吐 uid 时上层统一判成「目标机缺 python3」，让客户去装一个
// 本来就不需要装（包里自带）的东西，怎么装都修不好。现在必须把 systemd-run
// 的原始报错带出来。
func TestProbeFailureDetailSystemdRunError(t *testing.T) {
	raw := "systemd-run: unrecognized option '--working-directory=/var/lib/skillforge/work/exec-x'\n"
	got := probeFailureDetail(raw)
	if !strings.Contains(got, "unrecognized option") {
		t.Fatalf("探针失败归因没带出 systemd-run 原始报错：%q", got)
	}
	if strings.Contains(got, "python3") {
		t.Fatalf("探针失败归因又去扯 python3（本次真原因与 python 无关）：%q", got)
	}

	if got := probeFailureDetail("   \n\n"); got == "" {
		t.Fatal("探针零输出时必须给出「没有任何输出」的明确结论，不能返回空")
	}

	// 非 marker 的原始回执也必须原样交上去，不能吞掉。
	odd := probeFailureDetail("some weird output\nsecond line")
	if !strings.Contains(odd, "some weird output") {
		t.Fatalf("异常回执被吞掉了：%q", odd)
	}

	// 超长回执要截断，避免把自检输出撑爆。
	long := probeFailureDetail(strings.Repeat("x", 2000))
	if len(long) > 460 {
		t.Fatalf("超长回执没有截断（len=%d）", len(long))
	}
}

// TestSystemdArgsLifecycleInRealSystemd 在真有 systemd 的机器上真跑一次参数。
//
// 为什么不由纯字符串断言替代：本次 bug 的本质是「参数在目标 systemd 版本上不认」，
// 只有真把参数喂给 systemd-run 才算验到。CI 无 systemd 时 Skip（不计失败）。
// 判定的是**真拿到了降权 uid**，不是「命令没报错」。
func TestSystemdArgsLifecycleInRealSystemd(t *testing.T) {
	sandboxUsable(t)
	if os.Geteuid() != 0 {
		t.Skip("非 root，无法验证降权")
	}
	dir, err := os.MkdirTemp("/tmp", "sfargs-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	// 必须直接建在 /tmp 下：t.TempDir() 的父目录是 0700 root，
	// 降权后的 nobody 连目录都进不去，systemd 会以 EXIT_CHDIR(200) 失败 ——
	// 那是测试自己造出来的假红，跟出货参数无关（本轮踩过）。
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/main.py", []byte("import os\nprint(\"uid=\"+str(os.getuid()))\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	chownToSandbox(t, dir)

	tool := NewRunPythonTool(ExecConfig{
		Python:      DefaultExecConfig().Python,
		WorkRoot:    dir,
		SandboxUser: "nobody",
	})
	if tool.cfg.Python == "" {
		t.Skip("本机没有可用解释器")
	}
	args := tool.systemdArgs(dir, "sfargs-lifecycle")
	// -c 换成 main.py 之前的解释器路径即为出货形态；这里直接跑它。
	args = args[:len(args)-1]
	args = append(args, dir+"/main.py")

	out, err := exec.Command("systemd-run", args...).CombinedOutput()
	body := string(out)
	if err != nil {
		t.Fatalf("systemd-run 拒绝了出货参数（err=%v）：%s", err, body)
	}
	if !strings.Contains(body, "uid=65534") {
		t.Fatalf("没拿到降权 uid 证据，实际输出：%s", body)
	}
}

func chownToSandbox(t *testing.T, path string) {
	t.Helper()
	if err := os.Chown(path, sandboxUID("nobody"), sandboxGID("nobody")); err != nil {
		t.Logf("chown 失败（不致命，脚本 0644 + 目录 0700 的场景自会暴露）：%v", err)
	}
}
