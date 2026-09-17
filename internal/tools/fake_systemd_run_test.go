package tools

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFakeSystemdRun 写一个假 systemd-run：**能力探测那一面要仿真**，执行面按 body 来。
//
// 为什么假件也要认 --help/--version（2026-09-17 加的）：Run() 现在会先做能力探测
// （systemd-run 支持不支持 --pipe/--wait/-p），探测不过就 fail closed 直接拒绝执行。
// 一个只会 `exit 1` 的假件会被判成「老 systemd」，于是测试测到的是「拒绝执行」
// 而不是它本来要测的那条路径 —— 假件必须仿真到能过探测，否则它验证的不是真实调用链。
// （这也正是本次事故的形态：真实的 systemd 219 就是过不了探测的那种。）
func writeFakeSystemdRun(t *testing.T, path, body string) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  --help) printf '%s\\n' 'systemd-run [OPTIONS...] COMMAND [ARGUMENTS...]' " +
		"'  -p --property=NAME=VALUE   Set unit property' '  -P --pipe   Pass STDIN/STDOUT' " +
		"'     --wait   Wait until service stopped' '  -E --setenv=NAME=VALUE   Set environment' " +
		"'  -q --quiet   Suppress information messages' '  -G --collect   Unload unit after it ran'; exit 0 ;;\n" +
		"  --version) printf '%s\\n' 'systemd 249 (249.11)'; exit 0 ;;\n" +
		"esac\n" +
		body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("写假 systemd-run 失败：%v", err)
	}
	_ = filepath.Base(path)
}
