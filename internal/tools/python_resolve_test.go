package tools

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 用户报的现场：「skillforge 安装时提示 目标机没有 python3 沙箱探针执行失败」。
//
// 这条链上有两件事必须分开看：
//   ① 机器上**真的**没有解释器（内网最小安装，装不了）→ 产品得说真话、给可执行的
//      修复路径，而且不能把它报成「沙箱降权失败」（那是两回事，吓人且误导排查）；
//   ② 机器上有解释器、只是**不在 /usr/bin/python3**（自己编译 / conda /
//      RHEL8 的 platform-python）→ 旧逻辑写死单一路径，会把 ② 误报成 ①。
//      用户照着「没装 python3」的提示去装一个已经装好的东西，怎么装都修不好。

// fakePython 写一个「能当解释器用」的假解释器：存在、可执行、接受 `-c pass`。
// 用它来构造确定性的环境，不去动真机的 /usr/bin/python3。
func fakePython(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), mode); err != nil {
		t.Fatalf("写假解释器失败：%v", err)
	}
	return p
}

func TestResolvePython_OverrideWins(t *testing.T) {
	dir := t.TempDir()
	fake := fakePython(t, dir, "python3", 0o755)
	t.Setenv(EnvPythonOverride, fake)

	got := ResolvePython()
	// 整段比较，不用「包含」——包含会把 /usr/bin/python3 之类的系统解释器也算通过，
	// 那样这条断言就证明不了 override 真的生效了。
	if got != fake {
		t.Fatalf("%s=%s 时应该选它，实际选到 %q —— override 没生效（条件：存在、可执行、能跑 -c pass）",
			EnvPythonOverride, fake, got)
	}
}

// override 指了个用不了的路径时**不许猜别的**：与「沙箱不可用就拒绝执行」同一条
// 原则。猜的后果是运维以为在用自己那份解释器、实际在用系统那份。
func TestResolvePython_BadOverrideRefusesInsteadOfFallingBack(t *testing.T) {
	dir := t.TempDir()
	// 存在但不可执行：最容易发生的一种写错（chmod 忘了加 x，或者指到了目录）
	noexec := fakePython(t, dir, "python3", 0o644)
	t.Setenv(EnvPythonOverride, noexec)

	if got := ResolvePython(); got != "" {
		t.Fatalf("override 指向不可执行的 %s 时应当返回空（暴露问题），实际返回 %q"+
			"——这说明它悄悄回退到别的解释器了，运维会以为在用自己那份", noexec, got)
	}
}

func TestResolvePython_OverrideMissingPathRefuses(t *testing.T) {
	t.Setenv(EnvPythonOverride, "/nonexistent/definitely/not/here/python3")
	if got := ResolvePython(); got != "" {
		t.Fatalf("override 指向不存在的路径时应返回空，实际返回 %q", got)
	}
}

// 现役候选名单必须都是「真跑得起来」的解释器 —— 名单里塞一个跑不起来的路径，
// 等于给它一个永久性的探测失败。
func TestResolvePython_CandidatesAreUsable(t *testing.T) {
	if len(pythonCandidates) < 3 {
		t.Fatalf("候选名单只有 %d 项，太短：至少要有发行版包管理器位置、本机前缀位置、"+
			"RHEL8 platform-python 三类，实得 %v", len(pythonCandidates), pythonCandidates)
	}
	for _, c := range pythonCandidates {
		if !filepath.IsAbs(c) {
			t.Errorf("候选 %q 不是绝对路径：探针与沙箱都以 root 起进程，相对路径会随 cwd 漂移", c)
		}
	}
	// 本机至少要有一个能用（开发机/CI 都装了 python3）。一个都没有说明
	// 下面那些断言全在空转。
	if ResolvePython() == "" {
		t.Skip("本机没有任何可用解释器，跳过「候选可用性」这一组（不是通过，是没条件测）")
	}
}

// 相对路径（如 SKILLFORGE_PYTHON=python3）必须返回**绝对**路径。
// 原样回相对路径的话，systemd 起的沙箱进程会去它自己的 cwd 找解释器 ——
// 「配置看着对、行为随机」。这条断言最初就是红的，抓出了这个真缺陷。
func TestResolvePython_RelativeOverrideResolvesThroughPath(t *testing.T) {
	dir := t.TempDir()
	fakePython(t, dir, "python3", 0o755)
	t.Setenv("PATH", dir)
	t.Setenv(EnvPythonOverride, "python3")

	got := ResolvePython()
	if want := filepath.Join(dir, "python3"); got != want {
		t.Fatalf("相对 override 应经 $PATH 解析成绝对路径 %q，实际 %q", want, got)
	}
}

// 「候选全不可用」在真机上造不出来（/usr/bin/python3 就在那儿），所以内核函数
// 的候选名单从参数进来 —— 造不出来的场景 = 断言永远跑不到 = 等于没有断言。
func TestResolvePythonFrom_AllCandidatesUnusable(t *testing.T) {
	dir := t.TempDir()
	missing := []string{
		filepath.Join(dir, "not-here-1"),
		filepath.Join(dir, "not-here-2"),
	}
	t.Setenv("PATH", dir) // $PATH 里也没有 python3，别让兜底那步救回来

	if got := resolvePythonFrom("", missing); got != "" {
		t.Fatalf("候选全不可用时应返回空，实际 %q", got)
	}
}

// 跳过不可用的候选、继续往后找：第 1 项是个「存在但不可执行」的残件。
func TestResolvePythonFrom_SkipsUnusableCandidate(t *testing.T) {
	dir := t.TempDir()
	broken := fakePython(t, dir, "python3", 0o644) // 存在，不可执行
	good := fakePython(t, dir, "python3-good", 0o755)
	t.Setenv("PATH", t.TempDir())

	got := resolvePythonFrom("", []string{broken, good})
	if got != good {
		t.Fatalf("第 1 项不可执行时应继续用第 2 项 %q，实际 %q（只看文件存在就会选中第 1 项，"+
			"装完自检才红）", good, got)
	}
}

// 缺解释器时的「标签」逻辑：DefaultExecConfig().Python 永远非空。
// 空字符串的后果不是「没解释器」这么直白：RunPythonTool 会把它拼进 systemd-run
// 的命令行，用户拿到的是一句看不懂的 systemd 报错；而自检那条「缺解释器」的真话
// 依赖这个字段非空才说得出来。
func TestPythonLabel_NeverEmpty(t *testing.T) {
	if got := pythonLabel(""); got != pythonCandidates[0] {
		t.Fatalf("解析不到解释器时标签应为候选第一项 %q，实际 %q", pythonCandidates[0], got)
	}
	if got := pythonLabel("/opt/python/bin/python3"); got != "/opt/python/bin/python3" {
		t.Fatalf("解析到解释器时应原样返回，实际 %q", got)
	}
	if cfg := DefaultExecConfig(); cfg.Python == "" {
		t.Fatal("DefaultExecConfig().Python 为空")
	}
}

// 漂移守卫：install.sh 要在**解包前**探一次解释器，它调不了这个二进制，只能自己
// 维护一份候选名单。两份名单脱钩的后果是「安装说没问题、装完自检红」——用户拿着
// 一个自相矛盾的现场，最难查。
//
// 这里解析两份**源码**比对，而不是相信注释里那句「必须同步」。
func TestPythonCandidates_MatchInstaller(t *testing.T) {
	shPath := filepath.Join("..", "..", "deploy", "offline", "install.sh")
	sh, err := os.ReadFile(shPath)
	if err != nil {
		t.Fatalf("读不到 %s：%v（它挪位置了？这条守卫会静默失效，所以必须硬失败）", shPath, err)
	}

	// install.sh：PythonCandidates="/a /b /c"
	m := regexp.MustCompile(`(?m)^PythonCandidates="([^"]+)"`).FindAllStringSubmatch(string(sh), -1)
	if len(m) != 1 {
		t.Fatalf("install.sh 里应恰好有 1 行 PythonCandidates=\"...\"，实际 %d 行。"+
			"（0 行 = 变量被改名/删掉，这条守卫会空转通过；多行 = 有人复制了一份，选哪份都不确定）", len(m))
	}
	shCands := strings.Fields(m[0][1])

	// 反空转：两边都不许是空名单，否则「相等」是假的。
	if len(shCands) == 0 {
		t.Fatal("install.sh 的 PythonCandidates 解析出来是空的")
	}
	if len(pythonCandidates) == 0 {
		t.Fatal("exec.go 的 pythonCandidates 是空的")
	}
	if len(shCands) != len(pythonCandidates) {
		t.Fatalf("候选名单条数不一致：exec.go %d 项 %v / install.sh %d 项 %v。"+
			"按优先级顺序逐项对齐，别只往一边加", len(pythonCandidates), pythonCandidates, len(shCands), shCands)
	}
	for i := range pythonCandidates {
		if pythonCandidates[i] != shCands[i] {
			t.Fatalf("候选名单第 %d 项不一致：exec.go=%q / install.sh=%q（顺序即优先级，不能只比集合）",
				i+1, pythonCandidates[i], shCands[i])
		}
	}

	// 名单一致还不够：install.sh 必须**真的用这份名单**去循环探测。
	// 老版本是 `if [ ! -x /usr/bin/python3 ]` —— 名单摆在上面没人用的话，
	// 上面那条比对全绿、行为照旧误报。
	//
	// 这一段刻意把循环体抠出来整段看，而不是在全文里找 `-c pass`：
	// override 分支里也有一个 `-c pass`，全文找的话「循环里的探活被删掉」照样绿
	// ——注入自证脚本第一条就抓出了这个假断言。
	shStr := string(sh)
	loopAt := strings.Index(shStr, "for _cand in $PythonCandidates")
	if loopAt < 0 {
		t.Fatal("install.sh 没有用 $PythonCandidates 做循环探测：名单同步了但行为没改，" +
			"写死单一路径的误报照旧（`for _cand in $PythonCandidates` 这一行不见了）")
	}
	loopTail := strings.Index(shStr[loopAt:], "done")
	if loopTail < 0 {
		t.Fatal("install.sh 的候选循环没有结尾的 done")
	}
	loopBody := shStr[loopAt : loopAt+loopTail]
	if !strings.Contains(loopBody, `-x "$_cand"`) {
		t.Fatal("install.sh 的候选循环没有做可执行性检查（-x \"$_cand\"）")
	}
	// 必须真跑解释器探活，不能只看文件存在 —— 光 -x 挡不住 selinux 拦执行 /
	// 0 字节残缺文件那几种「看着有、跑不起来」。
	if !strings.Contains(loopBody, "-c pass") {
		t.Fatal("install.sh 的候选循环没有实际执行解释器探活（循环体里缺 `-c pass`）：" +
			"只看 -x 会把「文件在但跑不起来」判成可用，装完自检才红")
	}
	if strings.Contains(shStr, "[ ! -x /usr/bin/python3 ]") {
		t.Fatal("install.sh 里又出现了写死的单路径探测 `[ ! -x /usr/bin/python3 ]`")
	}
}
