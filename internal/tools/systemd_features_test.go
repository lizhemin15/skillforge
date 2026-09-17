package tools

import (
	"os"
	"strings"
	"testing"
)

// 用**真实样例**当测试料，而不是手写一份「我以为的」help 文本：
//
//	systemd_run_help_219.txt —— systemd 219（CentOS 7 系的真实版本），
//	  这一份是 2026-09-17 现场事故的现场版本（`--pipe` 不存在 → `unrecognized option '--pipe'`）。
//	systemd_run_help_249.txt —— 本机（Ubuntu 22.04）的 systemd 249。
//
// 两份都由 `systemd-run --help` + `--version` 原样拼接（中间用 =====VERSION===== 分隔）。
func loadHelpFixture(t *testing.T, name string) (help string, version string) {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("读不到测试料 %s：%v", name, err)
	}
	parts := strings.SplitN(string(b), "=====VERSION=====", 2)
	if len(parts) != 2 {
		t.Fatalf("%s 里没有 =====VERSION===== 分隔符（测试料坏了）", name)
	}
	return parts[0], parts[1]
}

func TestParseSystemdRunHelpOldSystemd219(t *testing.T) {
	help, ver := loadHelpFixture(t, "systemd_run_help_219.txt")
	f := ParseSystemdRunHelp(help)
	f.Version = ParseSystemdVersion(ver)

	if f.Version != "219" {
		t.Fatalf("版本解析错：拿到 %q，期望 219", f.Version)
	}
	// 219 的真实面貌：有 -p/--setenv/-q/-G，但没有 --pipe / --wait。
	// 这两条 False 正是现场「unrecognized option '--pipe'」的来源。
	if f.Pipe {
		t.Errorf("systemd 219 不该被判定支持 --pipe（现场就是被它拒收的）")
	}
	if f.Wait {
		t.Errorf("systemd 219 不该被判定支持 --wait")
	}
	if !f.Property || !f.Setenv || !f.Quiet || !f.Collect {
		t.Errorf("systemd 219 的 -p/--setenv/--quiet/--collect 都该被认出来，实际 %+v", f)
	}
}

func TestParseSystemdRunHelpModern249(t *testing.T) {
	help, ver := loadHelpFixture(t, "systemd_run_help_249.txt")
	f := ParseSystemdRunHelp(help)
	f.Version = ParseSystemdVersion(ver)

	if f.Version != "249" {
		t.Fatalf("版本解析错：拿到 %q，期望 249", f.Version)
	}
	if !f.Pipe || !f.Wait || !f.Property || !f.Setenv {
		t.Errorf("249 上这些选项都存在，解析器漏认了：%+v", f)
	}
}

// 判据的**双向**都要钉住，否则「恒判不可用」这种尺子故障没人发现：
// 老机器必须判不可用，新机器必须判可用。
func TestJudgeSandboxEnv(t *testing.T) {
	old := JudgeSandboxEnv(SystemdRunFeatures{Version: "219", Property: true, Setenv: true, Quiet: true, Collect: true})
	if old.Usable {
		t.Fatalf("systemd 219 竟然判成可用 —— 这台机器上没有 PrivateNetwork/ProtectSystem=strict，沙箱不成立")
	}
	if !hasSub(old.Missing, "219 低于") {
		t.Errorf("缺能力清单里要点名版本太低，实际：%v", old.Missing)
	}
	if !hasSub(old.Missing, "--pipe") {
		t.Errorf("缺能力清单里要点名 --pipe（现场原话），实际：%v", old.Missing)
	}

	modern := JudgeSandboxEnv(SystemdRunFeatures{Version: "249", Pipe: true, Wait: true, Property: true, Setenv: true, Quiet: true, Collect: true})
	if !modern.Usable {
		t.Fatalf("现代 systemd 被判成不可用（Missing=%v）—— 这把尺子恒判红，等于没装", modern.Missing)
	}

	// 版本读不出来（systemd-run 不在）：不许当成可用。
	if unknown := JudgeSandboxEnv(SystemdRunFeatures{}); unknown.Usable || !hasSub(unknown.Missing, "读不出 systemd 版本") {
		t.Errorf("版本读不出来时应当判不可用并点名原因，实际 usable=%v missing=%v", unknown.Usable, unknown.Missing)
	}
}

func TestParseSystemdVersionVariants(t *testing.T) {
	cases := []struct{ in, want string }{
		{"systemd 249 (249.11-0ubuntu3.22)\n+PAM +AUDIT", "249"},
		{"systemd 219\n+PAM +AUDIT +SELINUX", "219"},
		{"systemd-run [OPTIONS...] {COMMAND} [ARGS...]\n\nRun the specified command", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := ParseSystemdVersion(c.in); got != c.want {
			t.Errorf("ParseSystemdVersion(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func hasSub(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
