package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestClassifyReadEvidence 锁住「原始事实 → 结论」的解读规则（Bug H 的语义半场）。
func TestClassifyReadEvidence(t *testing.T) {
	cases := []struct {
		raw        string
		wantOK     bool
		wantDetail string
	}{
		{"denied", true, "拒绝"},
		// 父目录 0700 → 沙箱连 stat 都进不去。这是**更强的**证据，不能判失败。
		{"parent_denied", true, "拒绝"},
		{"readable", false, "危险"},
		{"enoent", false, "证据无效"},
		{"err:ValueError", false, "证据无效"},
		{"", false, "证据无效"},
	}
	for _, c := range cases {
		got := classifyReadEvidence(c.raw)
		if !strings.Contains(got, c.wantDetail) {
			t.Errorf("classifyReadEvidence(%q) = %q，期望含 %q", c.raw, got, c.wantDetail)
		}
		if strings.Contains(got, "OK") != c.wantOK {
			t.Errorf("classifyReadEvidence(%q) = %q，期望 OK=%v", c.raw, got, c.wantOK)
		}
		if c.wantOK && strings.Contains(got, "证据无效") {
			t.Errorf("classifyReadEvidence(%q) = %q：安全的拒绝结论被误判成无效证据（正是 Bug H）", c.raw, got)
		}
	}
}

// TestParseProbeOutput 解析器要能吃掉探针的真实输出形态。
func TestParseProbeOutput(t *testing.T) {
	out := parseProbeOutput("uid=65534|network=断(OK)\nread:/a/b=denied|write:work=可以(OK)\n")
	for k, want := range map[string]string{
		"uid":        "65534",
		"network":    "断(OK)",
		"read:/a/b":  "denied",
		"write:work": "可以(OK)",
	} {
		if out[k] != want {
			t.Errorf("parseProbeOutput[%q] = %q，期望 %q", k, out[k], want)
		}
	}
}

// TestProbeSeesThroughUnreadableParentDir 端到端复现 Bug H：真文件 + 不可进入的父目录。
//
// 直接跑探针模板本身（不套 systemd 沙箱——这里测的是「分类」不是「隔离」），
// 断言它给出 parent_denied 而不是把文件当成不存在。旧代码（os.path.exists）
// 在这里必然给 "文件不存在(证据无效)"，测试会红。
func TestProbeSeesThroughUnreadableParentDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 无视目录权限，无法复现 EACCES")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("没有 python3，跳过")
	}
	base := t.TempDir()
	locked := filepath.Join(base, "data")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(locked, "skillforge.db")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 父目录不可进入（无 x 位）→ 沙箱拿到的就是 EACCES
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(locked, 0o755) }()

	encoded, _ := json.Marshal([]string{secret})
	probe := strings.Replace(probeTemplate, "__SECRET_PATHS__", string(encoded), 1)
	cmd := exec.Command(py, "-c", probe)
	cmd.Dir = base
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("探针执行失败：%v\n%s", err, raw)
	}
	rawCode := parseProbeOutput(string(raw))["read:"+secret]
	if rawCode == "" {
		t.Fatalf("探针没给出 read: 证据，输出：\n%s", raw)
	}
	if rawCode != "parent_denied" {
		t.Fatalf("父目录不可进入时探针原始判定 = %q，期望 parent_denied（旧代码给 enoent 就是 Bug H）\n输出：\n%s", rawCode, raw)
	}
	got := classifyReadEvidence(rawCode)
	if strings.Contains(got, "证据无效") {
		t.Fatalf("判定 = %q，不该是无效证据", got)
	}
	if !strings.Contains(got, "OK") {
		t.Fatalf("判定 = %q，应判通过", got)
	}
	fmt.Printf("Bug H 回归：父目录 0700 下的真实机密 → %s\n", got)
}
