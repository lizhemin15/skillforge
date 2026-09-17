package ocrsvc

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 「解析服务起不来」的归因层
// ---------------------------------------------------------------------------
//
// 为什么单独一层（2026-09-17 现场，客户离线部署）：服务起不来时，journal 里只有一条
// 加载器/内核的原文，例如
//
//	[PYI-16:ERROR] Failed to load Python shared library
//	  '/opt/skillforge/run/ocr-tmp/_MEI00247ee9m89rGw/libpython3.11.so.1.0':
//	  /lib64/libc.so.6: version `GLIBC_2.28' not found
//
// 客户读到的是「找不到 libpython3.11.so.1.0」→ 去找文件、去重装 —— 而文件就在那儿，
// 真因是**目标机 glibc 比 ocrd 的构建基线老**。同一句原文，两种读法，两条完全不同的路。
// 这层把原文归类，并给出「照着敲就能修」的话；原文一字不少地留在证据里。
//
// 设计约束：
//   - 只做归类与翻译，不改写原文（Evidence 必须能直接贴回给厂商）。
//   - 归类靠**具体特征串**，不用宽泛关键词（例如 glibc 类必须同时命中加载器报错与版本号）。
//   - 拿不准就归到 unknown 并把最后一行原文交出去，绝不编一个原因（误诊比不诊更贵）。
type StartupFailure struct {
	// Class 是稳定的机器可读分类（测试与上层断言都钉它，别改字面量）。
	Class string
	// Summary 是一句人话：这是什么、为什么。
	Summary string
	// Fixes 是修法，按「最省事」排序；每条都要能在离线环境里执行或判断。
	Fixes []string
	// Evidence 是命中的原始日志行（原样）。
	Evidence string
}

// glibcVersionRe 认 `ldd (GNU libc) 2.17` / `getconf GNU_LIBC_VERSION` 的 `glibc 2.17`。
var glibcVersionRe = regexp.MustCompile(`(\d+\.\d+)$`)

// glibcRequiredRe 认加载器报错里的「需要哪个版本」：version `GLIBC_2.28' not found。
var glibcRequiredRe = regexp.MustCompile("version `GLIBC_([0-9]+\\.[0-9]+)' not found")

// missingLibRe 从 `cannot open shared object file` 的上下文里抠库名/路径。
var missingLibRe = regexp.MustCompile(`(?:error while loading shared libraries:\s*)?([^\s:]+\.so(?:\.[0-9.]+)*)`)

// systemdBadOptRe 认 systemd-run 拒收参数的原文：unrecognized option '--pipe'。
var systemdBadOptRe = regexp.MustCompile(`unrecognized option '(--[A-Za-z0-9-]+)'`)

// DocumentedClasses 是整个归因层会给出的分类。跟 Class 字段一起构成对外契约：
// 自检/体检/doctor 都按这些串报结论，允许 unknown（但必须带原文）。
var DocumentedClasses = []string{
	"glibc-too-old",  // 目标机 glibc 比 ocrd 构建基线老（最隐蔽的一类：文件在，加载不了）
	"missing-lib",    // 真的缺某个动态库
	"denied",         // noexec 挂载 / SELinux / AppArmor 拦住 load 或 exec
	"wrong-arch",     // 架构不符（Exec format error）
	"oom",            // 被 cgroup OOM 杀掉
	"not-installed",  // 二进制根本不在
	"systemd-option", // 目标机 systemd 不认我们下发的 systemd-run 参数
	"runtime-lost",   // PyInstaller 解包目录被清理（守卫会自愈：重启即可）
	"unknown",
}

// DiagnoseStartupFailure 把 journal / 探针输出归成一类。纯函数，单测直接喂真实原文。
func DiagnoseStartupFailure(log string) StartupFailure {
	lines := nonEmptyLines(log)
	last := ""
	if len(lines) > 0 {
		last = lines[len(lines)-1]
	}
	hit := func(sub string) string {
		for _, l := range lines {
			if strings.Contains(l, sub) {
				return l
			}
		}
		return ""
	}

	// ① 目标机 glibc 比基线老。
	// 判据必须同时命中「加载器说加载不了打包的 python」+「差的是 GLIBC_x.y」——
	// 只看 GLIBC 字样会被别人的构建日志骗到（CI 里就有大量 GLIBC 字样）。
	if m := glibcRequiredRe.FindStringSubmatch(log); m != nil {
		need := m[1]
		ev := hit("version `GLIBC_" + need + "' not found")
		if ev == "" {
			ev = last
		}
		host := HostGlibcVersion()
		hostTxt := "读不出来（本机 ldd 不可用）"
		if host != "" {
			hostTxt = host
		}
		return StartupFailure{
			Class: "glibc-too-old",
			Summary: fmt.Sprintf("解析服务要 glibc ≥%s，这台机器是 glibc %s —— 打包进来的运行时加载不了。"+
				"**这与文件在不在无关**：库就在解包目录里，是它的最低 glibc 要求比本机高，所以重装、找文件、删缓存都修不好。",
				need, hostTxt),
			Fixes: []string{
				"换成「ocrd 构建基线 ≤ 本机 glibc」的离线包（包里 bin/ocrd.baseline 写着该包 ocrd 的构建基线，比对本机 glibc 即可）。",
				fmt.Sprintf("或把这台机器的基座升到 glibc ≥%s：RHEL / AlmaLinux / Rocky / openEuler 8+、Ubuntu 18.10+、Debian 10+。", need),
				"只是这次先用起来：加 --no-ocr 重装。主服务、写作、技能训练照常，仅扫描件 / Word / Excel 抽不出正文。",
				"确认本机 glibc：`ldd --version | head -1` 或 `getconf GNU_LIBC_VERSION`。",
			},
			Evidence: ev,
		}
	}

	// ② systemd-run 拒收参数（老 systemd）。这条在沙箱探针里是主角，在解析服务里也可能出现。
	if m := systemdBadOptRe.FindStringSubmatch(log); m != nil {
		ev := hit("unrecognized option")
		return StartupFailure{
			Class: "systemd-option",
			Summary: fmt.Sprintf("目标机的 systemd-run 不认参数 %s —— 这不是 ocrd 的问题，是这台机器的 systemd 比我们要求的老。",
				m[1]),
			Fixes: []string{
				"看版本：`systemd-run --version`（我们需要 systemd ≥232）。",
				"把服务装在 systemd ≥232 的机器上（RHEL / AlmaLinux / Rocky / openEuler 8+、Ubuntu 18.10+、Debian 10+）。",
				"老基座（CentOS 7 / 中标麒麟 7 一类）无法提供等价隔离，代码沙箱功能会不可用（其余功能不受影响）。",
			},
			Evidence: ev,
		}
	}

	// ③ 缺库。注意与 ① 区分：① 是「库在、要求高」，这里是「库真的不在」。
	if strings.Contains(log, "cannot open shared object file") {
		lib := ""
		if m := missingLibRe.FindStringSubmatch(log); m != nil {
			lib = m[1]
		}
		ev := hit("cannot open shared object file")
		fixes := []string{}
		if strings.Contains(ev, "_MEI") {
			fixes = append(fixes,
				"缺的库在解包目录（_MEI…）里：解包不完整或运行中被清理 —— `systemctl restart "+UnitName()+"` 会重新解包，先试这条。",
				"若反复出现：解包分区可能满了（`df -h` 看 TMPDIR 所在分区），腾空间后重启服务。")
		} else if lib != "" {
			fixes = append(fixes, fmt.Sprintf("系统缺 %s：RHEL/AlmaLinux 用 `dnf install`、Debian/Ubuntu 用 `apt install` 装它（离线机挂 ISO 或配本地源）。", lib))
		}
		fixes = append(fixes, "完整日志：`journalctl -u "+UnitName()+" -n 50 --no-pager`。")
		return StartupFailure{
			Class:    "missing-lib",
			Summary:  "缺动态库：加载器报 cannot open shared object file" + libSuffix(lib) + "。",
			Fixes:    fixes,
			Evidence: ev,
		}
	}

	// ④ 权限类：noexec 挂载 / SELinux 策略。表现是「文件在、就是不让加载」。
	if ev := hit("Permission denied"); ev != "" {
		return StartupFailure{
			Class: "denied",
			Summary: "加载被拒绝（Permission denied）：文件在，但不让加载 —— 最常见是 TMPDIR 所在分区挂了 noexec，" +
				"或 SELinux/AppArmor 策略拦住了 exec/mmap。",
			Fixes: []string{
				"看挂载参数：`mount | grep \" $(df --output=target <解包目录> | tail -1) \"`，有 noexec 就换掉（把 TMPDIR 指到允许执行的分区，例如 /var/tmp 下的专用目录）。",
				"看 SELinux：`getenforce`；若为 Enforcing，用 `ausearch -m avc -ts recent` 看有没有 denied，再据此打标签或换目录。",
				"`systemctl show " + UnitName() + " -p Environment` 确认 TMPDIR 指向哪。",
			},
			Evidence: ev,
		}
	}

	// ⑤ 架构不符。
	if ev := hit("Exec format error"); ev != "" {
		return StartupFailure{
			Class:   "wrong-arch",
			Summary: "二进制架构与这台机器不符（Exec format error）—— 装错架构的离线包了。",
			Fixes: []string{
				"确认架构：`uname -m`（x86_64 → amd64 包；aarch64 → arm64 包）。",
				"用与机器架构一致的离线包重装。",
			},
			Evidence: ev,
		}
	}

	// ⑥ 被 OOM 杀。systemd 的原话是 "A process of this unit has been killed by the OOM killer."
	// ——第一版只认 "oom-kill"/"Out of memory"/"Killed process"，真实原话一个都没命中（单测抓到的）。
	ev := hit("OOM killer")
	if ev == "" {
		ev = hit("oom-kill")
	}
	if ev == "" {
		ev = hit("Out of memory")
	}
	if ev == "" {
		ev = hit("Killed process")
	}
	if ev != "" {
		return StartupFailure{
			Class:   "oom",
			Summary: "进程被 cgroup OOM 杀掉（大页数扫描件内存峰值高，单元上限 2G）。",
			Fixes: []string{
				"给这台机器更多内存，或分批上传扫描件（OCR 侧每 20 页回收引擎压低峰值）。",
				"确认单元内存上限：`systemctl show " + UnitName() + " -p MemoryMax`。",
			},
			Evidence: ev,
		}
	}

	// ⑦ 二进制不在。
	if ev := hit("No such file or directory"); ev != "" && strings.Contains(log, "ocrd") {
		return StartupFailure{
			Class:   "not-installed",
			Summary: "解析服务二进制不在（No such file or directory）—— 单元在，但它指向的程序没装（--no-ocr 装过？）。",
			Fixes: []string{
				"确认：`ls -l /opt/skillforge/bin/ocrd`。",
				"用自带 ocrd 的 amd64 离线包重装（不要加 --no-ocr）。",
			},
			Evidence: ev,
		}
	}

	// ⑧ 运行时解包目录被清理（老事故形态，守卫会自愈）。
	ev = hit("config.yaml")
	if ev == "" {
		ev = hit("运行时目录")
	}
	if ev != "" && (strings.Contains(log, "_MEI") || strings.Contains(log, "config.yaml")) {
		return StartupFailure{
			Class:   "runtime-lost",
			Summary: "PyInstaller 解包目录在运行中被清理了（模型/config 读不到）。守卫会自己退出让 systemd 重新拉起。",
			Fixes: []string{
				"`systemctl restart " + UnitName() + "`，约 10 秒后重试。",
				"若反复出现：确认单元里 TMPDIR 指向 __PREFIX__/run/ocr-tmp（不能是 /tmp，systemd-tmpfiles 会每天清 /tmp）。",
			},
			Evidence: ev,
		}
	}

	// ⑨ 拿不准：把最后一行原文交出去，不编原因。
	return StartupFailure{
		Class:    "unknown",
		Summary:  "没认出这一类失败（下面这条是日志里最后一行原文，贴回厂商即可）。",
		Fixes:    []string{"完整日志：`journalctl -u " + UnitName() + " -n 80 --no-pager`。"},
		Evidence: last,
	}
}

func libSuffix(lib string) string {
	if lib == "" {
		return ""
	}
	return "（" + lib + "）"
}

func nonEmptyLines(s string) []string {
	out := []string{}
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// ParseGlibcVersion 从 `ldd --version` 首行（`ldd (GNU libc) 2.17`）或
// `getconf GNU_LIBC_VERSION`（`glibc 2.17`）里取出主次版本号。纯函数。
func ParseGlibcVersion(text string) string {
	for _, l := range nonEmptyLines(text) {
		if !strings.Contains(strings.ToLower(l), "libc") && !strings.Contains(strings.ToLower(l), "glibc") {
			continue
		}
		if m := glibcVersionRe.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			return m[1]
		}
	}
	return ""
}

// HostGlibcVersion 取本机 glibc 版本；取不到返回空串（musl 系统、ldd 缺失等）。
func HostGlibcVersion() string {
	for _, c := range [][]string{{"ldd", "--version"}, {"getconf", "GNU_LIBC_VERSION"}} {
		out, err := exec.Command(c[0], c[1:]...).CombinedOutput()
		if err != nil {
			continue
		}
		if v := ParseGlibcVersion(string(out)); v != "" {
			return v
		}
	}
	return ""
}

// JournalTail 取单元的最近日志；拿不到（没有 systemd、没有 journalctl、没权限）返回空串。
// 不做任何解读 —— 解读是 DiagnoseStartupFailure 的事，取证与判断必须能分别被测试。
func JournalTail(unit string, n int) string {
	if unit == "" {
		return ""
	}
	if _, err := exec.LookPath("journalctl"); err != nil {
		return ""
	}
	out, err := exec.Command("journalctl", "-u", unit, "-n", fmt.Sprint(n), "--no-pager").CombinedOutput()
	if err != nil && len(out) == 0 {
		return ""
	}
	return string(out)
}

// BinaryPreflight 真跑一次二进制自检（ocrd --preflight），拿「这台机器上它到底能不能起来」的直接证据。
//
// 为什么用 --preflight 而不是 --port：起不来的机器上必须**不占端口、不长期驻留**，
// 一次 import + 解包目录检查就够，几秒即返回。它跑不起来时的 stderr 就是归因层的输入。
func BinaryPreflight(binPath string) (string, error) {
	if binPath == "" {
		return "", fmt.Errorf("没有二进制路径")
	}
	if _, err := os.Stat(binPath); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp("", "sf-ocr-preflight-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	cmd := exec.Command(binPath, "--preflight")
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
	done := make(chan struct{})
	var out []byte
	go func() {
		out, _ = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(120 * time.Second):
		_ = cmd.Process.Kill()
		return "自检超时（120s）：解包或加载卡住了", fmt.Errorf("preflight 超时")
	}
	return string(out), nil
}

// DiagnoseUnitStartup 组合取证：先读 journal（那是故障的第一现场），
// **只在 journal 取不到时**才真跑一次产物自检（几秒到十几秒，不该让自检白等）。
func DiagnoseUnitStartup(unit, binPath string) StartupFailure {
	journal := JournalTail(unit, 80)
	v := DiagnoseStartupFailure(journal)
	if v.Class != "unknown" {
		v.Evidence = strings.TrimSpace(v.Evidence)
		return v
	}
	if strings.TrimSpace(journal) == "" {
		if out, _ := BinaryPreflight(binPath); strings.TrimSpace(out) != "" {
			if v2 := DiagnoseStartupFailure(out); v2.Class != "unknown" {
				return v2
			} else if strings.TrimSpace(v.Evidence) == "" {
				v.Evidence = strings.TrimSpace(out)
			}
		}
	}
	v.Evidence = strings.TrimSpace(v.Evidence)
	return v
}
