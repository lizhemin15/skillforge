package tools

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
)

// ---------------------------------------------------------------------------
// systemd-run 能力探测 —— 「这台机器能不能提供沙箱」
// ---------------------------------------------------------------------------
//
// 为什么必须有它（2026-09-17 现场，客户离线部署）：
//
//	目标机 systemd 219（CentOS 7 / 中标麒麟一类）上 `systemd-run` 不认 --pipe，
//	直接 `systemd-run: unrecognized option '--pipe'` 退出。探针一个字都没跑，
//	上层只看到「没有 uid」，客户手里只有一句 getopt 报错 ——
//	看不出真因是「这台机器的 systemd 太老，给不了等价隔离」，
//	更不知道该换基座还是该改参数。
//
//	同一个坑本项目已经踩过两次（先是 --working-directory=，后来是 --pipe），
//	两次都是「参数写死 + 老 systemd 拒收」。所以规矩定死：
//	能不能用先探测；不能用就把「缺什么 / 要什么版本 / 怎么修」一次说清，绝不裸跑。
//
// 版本依据（我们真正依赖的能力，取最大值 ⇒ 232）：
//
//	PrivateNetwork=yes / ProtectSystem=strict …… systemd 232（断网 + 只读根，这两条是沙箱的立身之本）
//	--pipe / --wait ………………………………… systemd 231（同步收子进程输出）
//	MemoryMax= ……………………………………… 231
//	RuntimeMaxSec= …… 229 / SystemCallFilter=@system-service … 228 / TasksMax= …… 227
//	ReadWritePaths= … 214 / ProtectHome= …… 214 / CPUQuota= … 213 / NoNewPrivileges= … 206
const MinSystemdVersion = 232

// SystemdRunFeatures 是「本机 systemd-run 支持哪些我们要用的东西」的实测结果。
type SystemdRunFeatures struct {
	Version  string // 形如 "249"；读不出来时为空
	Pipe     bool   // --pipe
	Wait     bool   // --wait
	Collect  bool   // --collect / -G
	Quiet    bool   // --quiet / -q
	Setenv   bool   // --setenv
	Property bool   // -p / --property
}

// systemdRunOpts 是我们要探测的 CLI 选项（名字 → 结构体字段指针）。
// 只探测真正会用到的：探测表越宽，误判「不支持」的概率越大。
var systemdRunOpts = []struct {
	name string
	ptr  func(*SystemdRunFeatures) *bool
}{
	{"pipe", func(f *SystemdRunFeatures) *bool { return &f.Pipe }},
	{"wait", func(f *SystemdRunFeatures) *bool { return &f.Wait }},
	{"collect", func(f *SystemdRunFeatures) *bool { return &f.Collect }},
	{"quiet", func(f *SystemdRunFeatures) *bool { return &f.Quiet }},
	{"setenv", func(f *SystemdRunFeatures) *bool { return &f.Setenv }},
	{"property", func(f *SystemdRunFeatures) *bool { return &f.Property }},
}

// ParseSystemdRunHelp 从 `systemd-run --help` 的文本里认出我们用到的选项。
//
// 纯函数：真样例是 internal/tools/testdata/systemd_run_help_219.txt
// （systemd 219，字段与选项组合来自真实的 CentOS 7 系容器，不是编的）。
// 匹配规则按「行首选项」而不是 Contains("--pipe")：帮助文本里
// 出现 `--pipe` 字样的地方只有选项行本身，但 Contains 会被未来的说明文字骗到
// （比如 help 里写「the opposite of --pipe is …」），行首锚定没这个风险。
func ParseSystemdRunHelp(text string) SystemdRunFeatures {
	var f SystemdRunFeatures
	for _, o := range systemdRunOpts {
		// 允许 `-p --property=…` / `-x, --foo` / `--pipe` 三种排版；
		// 末尾必须是空白或 `=`，避免把 `--pipelines` 这种将来才有的选项算成 `--pipe`。
		// 短选项后的分隔符两种都要认：systemd 219 打的是 `-p --property=…`（逗号都没有），
		// 249 打的是 `-p --property=…` / `-E --setenv=…`。第一版只认 `-x, `，
		// 结果把 219 上明明存在的 -p/-q/-G 全判成不存在。
		re := regexp.MustCompile(`(?m)^\s*(?:-\w,\s+|-\w\s+)?--` + o.name + `(?:\s|=|$)`)
		*o.ptr(&f) = re.MatchString(text)
	}
	f.Version = ParseSystemdVersion(text)
	return f
}

// systemdVersionRe 认 `systemd 249` / `systemd 219` 这类版本行。
// 取**最后一条**匹配：`systemd-run --help` 的正文里可能出现 "systemd-run" 字样，
// 而版本行是最后才打印的；用最后一条既覆盖 help 文本，也覆盖单独的 --version 输出。
var systemdVersionRe = regexp.MustCompile(`(?m)^systemd\s+(\d+)\b`)

// ParseSystemdVersion 从 `systemd-run --version`（或 help 文本）里取出主版本号。
func ParseSystemdVersion(text string) string {
	all := systemdVersionRe.FindAllStringSubmatch(text, -1)
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1][1]
}

// SandboxEnvVerdict 是「这台机器能不能跑沙箱」的结论。
type SandboxEnvVerdict struct {
	Version  string
	Features SystemdRunFeatures
	Usable   bool
	// Missing 是缺的能力，每条都是「客户看了知道要干什么」的话。
	Missing []string
}

// JudgeSandboxEnv 把探测结果判成结论。纯函数，单测直接打它（含 219 的真样例）。
func JudgeSandboxEnv(f SystemdRunFeatures) SandboxEnvVerdict {
	v := SandboxEnvVerdict{Version: f.Version, Features: f}
	if f.Version == "" {
		v.Missing = append(v.Missing,
			"读不出 systemd 版本（systemd-run 不存在或跑不起来 —— 沙箱靠它创建降权子进程）")
		return v
	}
	if n, err := strconv.Atoi(f.Version); err == nil && n < MinSystemdVersion {
		v.Missing = append(v.Missing, fmt.Sprintf(
			"systemd %s 低于沙箱所需的最低版本 %d", f.Version, MinSystemdVersion))
	}
	if !f.Pipe {
		v.Missing = append(v.Missing, "systemd-run 不支持 --pipe（同步收子进程输出，systemd 231 起才提供）")
	}
	if !f.Wait {
		v.Missing = append(v.Missing, "systemd-run 不支持 --wait（等子进程结束并回收，systemd 231 起才提供）")
	}
	if !f.Property {
		v.Missing = append(v.Missing, "systemd-run 不支持 -p/--property（下发降权与隔离属性）")
	}
	v.Usable = len(v.Missing) == 0
	return v
}

// ProbeSandboxEnv 实测本机能力。
//
// 为什么不缓存（踩过）：先前用 sync.Once 缓存，结果单测进程里只要有一个用例把 PATH
// 换成临时目录（python 解析那几条就是），后面所有用例都会继承「找不到 systemd-run」的
// 缓存结论 —— 判定全错、而且错得没有任何提示。每次现探的代价是两次短命进程（~5ms），
// 而「执行代码」这条路径本来就重（起 systemd 单元），不值得为 5ms 引入一个会撒谎的缓存。
func ProbeSandboxEnv() SandboxEnvVerdict {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return JudgeSandboxEnv(SystemdRunFeatures{})
	}
	// --help 在 219 上也有（v205 起），比 --version 更能说明「支持哪些选项」。
	help, _ := exec.Command("systemd-run", "--help").CombinedOutput()
	ver, _ := exec.Command("systemd-run", "--version").CombinedOutput()
	f := ParseSystemdRunHelp(string(help))
	if v := ParseSystemdVersion(string(ver)); v != "" {
		f.Version = v
	}
	return JudgeSandboxEnv(f)
}

// SandboxEnvFixes 是「老基座」这一类问题的修法（离线可执行，不给要联网的命令）。
//
// 为什么把修法写成固定三条而不是让调用方自由发挥：这两条边界（网络隔离、只读根）
// 没有「凑合能跑」的中间档 —— 少一条就不叫沙箱。所以要么换基座，要么接受这项不可用，
// 中间那些「把属性删掉先跑起来」的建议必须被明确否掉。
func SandboxEnvFixes() []string {
	return []string{
		"把服务装在 systemd ≥232 的机器上：RHEL / AlmaLinux / Rocky / openEuler 8+、Ubuntu 18.10+、Debian 10+、麒麟 V10 SP1+。",
		"老基座（CentOS 7 / 中标麒麟 7 一类，systemd 219）给不了等价隔离 —— PrivateNetwork=（断网）与 ProtectSystem=strict（只读根）都是 systemd 232 才有的能力。",
		"其余功能不受影响（写作、文档生成、素材解析、技能训练都不依赖它）；沙箱宁可不跑，也绝不在无隔离的环境里执行代码。",
	}
}
