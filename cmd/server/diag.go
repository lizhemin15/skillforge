package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/lizhemin15/skillforge/internal/ocrsvc"
	"github.com/lizhemin15/skillforge/internal/tools"
	"github.com/lizhemin15/skillforge/internal/version"
)

// runDiag 是 `skillforge -diag`：**一条命令把「这台机器上什么能跑、什么不能跑、为什么」说清**。
//
// 为什么要有它（2026-09-17 客户离线部署现场）：
//
//	客户装完之后才自己发现两件事：① 解析服务反复重启、8093 一直 refused，journal 里
//	只有一行加载器报错（`Failed to load Python shared library … GLIBC_2.28 not found`）；
//	② 自检里「代码执行沙箱」红，原始报错是 `systemd-run: unrecognized option '--pipe'`。
//	这两条真因是同一个：**目标机比我们支持的基座老**（glibc 2.17 / systemd 219）。
//	而客户从这两句原文里看不出「该换基座」还是「该改配置」，只能靠人肉排查。
//
// 所以：装**之前**就能跑这一条命令，把基线、能力、结论、修法一次给全。
//   - install.sh 装前调用它（在解包里那份二进制上跑，还没落盘也能判）
//   - 客户/运维随时手敲
//   - 输出全部来自本机实测，不联网、不调模型 —— 离线机可用
//
// 退出码：0 = 没有需要动手的问题（含「本机环境给不了某能力」这一情形）；1 = 有问题要处理。
func runDiag() int {
	bin := diagOCRBinary()
	fmt.Println("SkillForge 基座体检（离线：不联网、不调用模型）")
	fmt.Printf("版本    : %s\n", version.String())
	fmt.Printf("系统    : %s（%s，kernel %s）\n", osPrettyName(), hostArch(), kernelRelease())
	// glibc：解析服务能不能加载的分水岭，必须最先报。
	glibc := ocrsvc.HostGlibcVersion()
	if glibc == "" {
		glibc = "读不出来（ldd/getconf 不可用）"
	}
	fmt.Printf("glibc   : %s\n", glibc)

	needFix := 0

	// ---- 代码执行沙箱：能力来自 systemd，缺就是缺，不能凑合 ----
	env := tools.ProbeSandboxEnv()
	fmt.Println("── 代码执行沙箱 ──")
	if env.Usable {
		fmt.Printf("  可用：systemd %s 支持所需能力（--pipe/--wait/-p，PrivateNetwork/ProtectSystem=strict）\n", env.Version)
	} else {
		ver := env.Version
		if ver == "" {
			ver = "读不出来"
		}
		fmt.Printf("  不可用（环境不支持）：本机 systemd %s，沙箱需要 ≥%d\n", ver, tools.MinSystemdVersion)
		for _, m := range env.Missing {
			fmt.Printf("    · %s\n", m)
		}
		fmt.Println("  影响：只有 AI「执行代码」不可用；写作/文档生成/素材解析/训练都不受影响。")
		fmt.Println("  修法：")
		for _, f := range tools.SandboxEnvFixes() {
			fmt.Printf("    · %s\n", f)
		}
		fmt.Println("  （这一项**不计入失败**：机器没坏、程序没坏，是这份能力在这台机器上不可能成立；")
		fmt.Println("    我们不会为了「看起来能跑」而降级成无隔离执行代码。）")
	}

	// ---- 解析服务（ocrd）：先比基线，再真跑一次 ----
	fmt.Println("── 文档解析服务（ocrd）──")
	switch {
	case bin == "":
		fmt.Println("  本包不带 ocrd（arm64 离线包按设计不带，或安装时用了 --no-ocr）。")
		fmt.Println("  影响：扫描件 PDF / Word / Excel 抽不出正文；其余功能不受影响。")
	default:
		fmt.Printf("  二进制  : %s\n", bin)
		if pkgBase := readBaselineFile(bin + ".baseline"); pkgBase != "" {
			fmt.Printf("  构建基线: glibc=%s（本包 ocrd 自己声明的基线）\n", pkgBase)
			if glibc != "" {
				if cmpVersion(glibc, pkgBase) < 0 {
					needFix++
					fmt.Printf("  ✗ 本机 glibc %s **低于**这个包里的 ocrd 要求的 %s —— 它一定起不来。\n", glibc, pkgBase)
					fmt.Println("    表现：服务反复重启重启失败，journal 里是 `Failed to load Python shared library …")
					fmt.Printf("      version `GLIBC_%s' not found`。**这与文件在不在无关**，重装/找文件都修不好。\n", pkgBase)
					fmt.Println("    修法：")
					fmt.Printf("      · 换一个 ocrd 构建基线 ≤ 本机 glibc 的离线包（本机 %s → 需要基线 ≤%s 的包）。\n", glibc, glibc)
					fmt.Println("      · 或把这台机器的基座升到 glibc ≥" + pkgBase + "（RHEL/Alma/Rocky/openEuler 8+、Ubuntu 18.10+、Debian 10+）。")
					fmt.Println("      · 只是这次先用起来：加 --no-ocr 重装（主服务/写作/训练照常，扫描件与 Office 素材抽不出正文）。")
					break
				}
				fmt.Printf("  ✓ 本机 glibc %s ≥ 要求 %s，基线匹配。\n", glibc, pkgBase)
			}
		} else {
			fmt.Println("  构建基线: 读不到（包内没带 bin/ocrd.baseline —— 老包）。")
		}
		out, _ := ocrsvc.BinaryPreflight(bin)
		v := ocrsvc.DiagnoseStartupFailure(out)
		switch {
		case v.Class == "unknown" && strings.Contains(out, `"ok": true`):
			fmt.Println("  启动自检: 通过（依赖可加载、解包目录正常）")
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				if strings.Contains(line, `"baseline"`) || strings.Contains(line, `"version"`) || strings.Contains(line, `"python"`) {
					fmt.Printf("    %s\n", strings.TrimSpace(line))
				}
			}
		case strings.Contains(out, "--preflight"):
			// 老包（加 --preflight 之前的版本）不支持自检开关：这不是故障，说清即可。
			// 不能据此判失败 —— 那样会把一台本来好好的老部署报成需要动手修。
			fmt.Println("  启动自检: 这个包里的 ocrd 还不支持 --preflight（老包），跳过实测。")
			fmt.Println("    用 /health 判活：`curl -s http://127.0.0.1:8093/health`；升级到新包后可用本命令直接自检。")
		default:
			needFix++
			fmt.Println("  启动自检: 失败")
			fmt.Printf("    归因: %s\n", v.Summary)
			for _, f := range v.Fixes {
				fmt.Printf("    · %s\n", f)
			}
			if v.Evidence != "" {
				fmt.Printf("    原始报错: %s\n", ocrsvc.FirstLine(v.Evidence))
			}
		}
	}

	fmt.Println("──────────────────────────────────────────────")
	if needFix > 0 {
		fmt.Println("结论：有需要处理的问题（见上）。")
		return 1
	}
	fmt.Println("结论：本机没有需要处理的问题。")
	return 0
}

// diagOCRCandidates 列出「包里 / 装机后的 ocrd」可能出现的位置，顺序即优先级。
//
// 为什么必须有「同目录」这一条（2026-09-17 装前体检现场）：离线包里的布局是
// `<包根>/bin/skillforge` 与 `<包根>/bin/ocrd` **并列**。install.sh 在解包目录里跑
// `bin/skillforge -diag` 时，可执行文件自己就在 bin/ 下面，于是 `Dir(exe)/bin/ocrd`
// 会指到 `<包根>/bin/bin/ocrd`（不存在），只剩 /opt/skillforge/bin/ocrd 这个回退 ——
// 而那是**上一次安装留下的旧 ocrd**，体检会把「另一份产物的基线」当成结论交出去
// （最坏情形：旧基线恰好匹配，于是把「这个包在这台机器上装不起来」报成没问题）。
// 所以候选里既要有装机布局 `<前缀>/bin/ocrd`，也要有解包布局的同目录并列 `Dir(exe)/ocrd`。
func diagOCRCandidates(exe string) []string {
	cands := []string{}
	if exe != "" {
		dir := filepath.Dir(exe)
		cands = append(cands,
			filepath.Join(dir, "bin", "ocrd"), // 装机后：<前缀>/skillforge + <前缀>/bin/ocrd
			filepath.Join(dir, "ocrd"),        // 解包目录：<包根>/bin/skillforge + <包根>/bin/ocrd
		)
	}
	cands = append(cands, "/opt/skillforge/bin/ocrd")
	return cands
}

// diagOCRBinary 按 diagOCRCandidates 的顺序返回第一个真实存在的 ocrd；都没有返回空串。
func diagOCRBinary() string {
	exe := ""
	if p, err := os.Executable(); err == nil {
		exe = p
	}
	return pickOCRBinary(exe)
}

// pickOCRBinary 从候选里挑第一个真实存在的文件。
// exe 单独走参数（而不是在里面调 os.Executable）：这样能用**真临时目录**测候选顺序，
// 而不是只测一个字符串列表 —— 顺序错了正是这次真机事故的根因，必须被真文件布局钉住。
func pickOCRBinary(exe string) string {
	for _, c := range diagOCRCandidates(exe) {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

// readBaselineFile 读 `<ocrd>.baseline` 里的 glibc= 行；读不到返回空串（老包没这个文件）。
func readBaselineFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "glibc=") {
			return strings.TrimSpace(strings.TrimPrefix(line, "glibc="))
		}
	}
	return ""
}

// cmpVersion 比较 "2.17" 与 "2.28" 这类版本（返回 -1/0/1）；解析不了时返回 0（不据此下结论）。
func cmpVersion(a, b string) int {
	pa, oka := parseMajorMinor(a)
	pb, okb := parseMajorMinor(b)
	if !oka || !okb {
		return 0
	}
	switch {
	case pa[0] != pb[0]:
		if pa[0] < pb[0] {
			return -1
		}
		return 1
	case pa[1] != pb[1]:
		if pa[1] < pb[1] {
			return -1
		}
		return 1
	}
	return 0
}

func parseMajorMinor(v string) ([2]int, bool) {
	var out [2]int
	parts := strings.SplitN(strings.TrimSpace(v), ".", 2)
	if len(parts) == 0 {
		return out, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return out, false
	}
	out[0] = major
	if len(parts) == 2 {
		minor, err := strconv.Atoi(parts[1])
		if err != nil {
			return out, false
		}
		out[1] = minor
	}
	return out, true
}

func osPrettyName() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "未知（读不到 /etc/os-release）"
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "PRETTY_NAME=")), `"`)
		}
	}
	return "未知"
}

func kernelRelease() string {
	out, err := exec.Command("uname", "-r").Output()
	if err != nil {
		return "未知"
	}
	return strings.TrimSpace(string(out))
}

func hostArch() string {
	out, err := exec.Command("uname", "-m").Output()
	if err != nil {
		return "未知"
	}
	return strings.TrimSpace(string(out))
}
