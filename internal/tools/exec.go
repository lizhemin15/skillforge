package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ExecConfig 是代码执行沙箱的配置。默认值即「安全默认」。
type ExecConfig struct {
	// WorkRoot 是每次执行的隔离工作区根目录。必须以 root 身份可写、且对降权用户可进入。
	WorkRoot string
	// Python 是解释器绝对路径。
	Python string
	// Timeout 是墙钟超时。同时下发给 systemd（RuntimeMaxSec），双保险：
	// 只靠 Go 侧 kill 只杀 systemd-run 本身，systemd 起的实际进程会变孤儿。
	Timeout time.Duration
	// MemoryMax / CPUQuota / TasksMax 是 systemd 限额。TasksMax 同时是 fork 炸弹的闸门。
	MemoryMax string
	CPUQuota  string
	TasksMax  int
	// SandboxUser 是降权目标用户。
	SandboxUser string
	// SkillRoots 是额外暴露给沙箱的「只读数据」目录（如技能库素材）。
	MaxOutput int // 回给模型的输出上限（字节）
}

// EnvPythonOverride 是解释器路径的显式覆盖。
//
// 给「机器上有一份自己的解释器（conda / 自编译 / 便携包）」的现场用：
// 例如 SKILLFORGE_PYTHON=/opt/python/bin/python3。
const EnvPythonOverride = "SKILLFORGE_PYTHON"

// pythonCandidates 是解释器候选绝对路径，按优先级排列。
//
// 为什么不再是写死的单个 /usr/bin/python3（用户报的现场：「安装时提示目标机
// 没有 python3」）：装没装 ≠ 在不在这一个路径上。见过的真实形态——
//
//	· 发行版包管理器装的 → /usr/bin/python3（绝大多数）
//	· 源码编译 / pipx / conda 前缀 → /usr/local/bin/python3
//	· RHEL / CentOS / AlmaLinux 8 的系统解释器 → /usr/libexec/platform-python
//	  （它没有 python3 这个名字，但 `platform-python -c ...` 照样跑）
//
// 写死单一路径时，后两种会被一律误报成「目标机没有 python3」——用户照着提示
// 去装一个**已经装好**的东西，怎么装都「修不好」。所以：先按候选找，再退到
// $PATH 里的 python3。
//
// ⚠️ 改这里必须同步 deploy/offline/install.sh 的 PythonCandidates —— 安装脚本
// 要在解包前先探一次，它不能调这个二进制。两份名单脱钩时会变成「安装说没问题、
// 装完自检红」，所以有守卫盯着：internal/tools/python_resolve_test.go 解析两边
// 源码逐项比对（顺序即优先级），并由 web/tests/python_resolve_mutation_check.py
// 注入自证「这条守卫真能红」。
var pythonCandidates = []string{
	"/usr/bin/python3",
	"/usr/local/bin/python3",
	"/usr/libexec/platform-python",
}

// pythonUsablePath 判断一个路径是否真能当解释器用，能用则返回它的**绝对**路径。
//
// 光看文件存在（-x）不算数：见过 selinux 拦执行、见过同名目录、见过 0 字节的
// 残缺文件 —— 那些情况下探针跑不起来，用户看到的还是「沙箱失败」，等于白改。
//
// 为什么返回绝对路径而不是原样回：沙箱是 root 起的 systemd 进程，cwd 与调用方
// 无关。放一个相对路径进去，systemd-run 会去它自己的 cwd 找解释器 ——
// 「配置看着对、行为随机」的那类问题。
func pythonUsablePath(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", false
	}
	abs := p
	if !filepath.IsAbs(abs) {
		lp, err := exec.LookPath(abs)
		if err != nil {
			return "", false
		}
		abs = lp
	} else if _, err := exec.LookPath(abs); err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if exec.CommandContext(ctx, abs, "-c", "pass").Run() != nil {
		return "", false
	}
	return abs, true
}

// resolvePythonFrom 是 ResolvePython 的内核：override 与候选名单都从参数进来。
//
// 抽出来的唯一理由是可测：真机上造不出「候选全不可用」这种场景（/usr/bin/python3
// 就在那儿），而造不出来的场景 = 那条断言永远跑不到 = 等于没有断言。
func resolvePythonFrom(override string, candidates []string) string {
	if strings.TrimSpace(override) != "" {
		abs, ok := pythonUsablePath(override)
		if !ok {
			// 显式指定却不可用 → 不猜别的，交给自检报错。
			return ""
		}
		return abs
	}
	for _, c := range candidates {
		if abs, ok := pythonUsablePath(c); ok {
			return abs
		}
	}
	if abs, ok := pythonUsablePath("python3"); ok {
		return abs
	}
	return ""
}

// ResolvePython 挑一个能用的解释器，返回绝对路径；找不到返回 ""。
//
// 顺序：$SKILLFORGE_PYTHON → pythonCandidates → $PATH 里的 python3。
//
// override 设了却不可用时**直接返回 ""，不去猜别的** —— 与「沙箱不可用就拒绝
// 执行，绝不静默降级」同一条原则：运维以为在用自己那份解释器、实际在用系统的
// 那份，是最难查的坑。宁可让自检红着告诉他路径写错了。
func ResolvePython() string {
	return resolvePythonFrom(os.Getenv(EnvPythonOverride), pythonCandidates)
}

// pythonLabel 把解析结果转成「给诊断用的路径」：解析不到时退回候选名单第一项。
//
// 退回的是**标签**不是解释器：有了它，缺解释器的现场才能拿到
// 「目标机没有可用的 python3 解释器（探针解释器 /usr/bin/python3）」这句真话；
// 留空的话用户看到的是一句谁也看不懂的 systemd 报错。
func pythonLabel(py string) string {
	if py == "" {
		return pythonCandidates[0]
	}
	return py
}

// DefaultExecConfig 返回经过实测验证的默认配置。
//
// 三个限额可用环境变量覆盖（SKILLFORGE_EXEC_TIMEOUT / _MEMORY / _CPU），
// 为什么必须可覆盖：单机离线部署的客户拿它跑自己的料，一份 17MB 的 PDF
// 让模型写脚本解析，256M 内存上限会在几秒内把它 cgroup OOM 掉，而回给模型的
// 是一句「退出码 -1」——模型不知道发生了什么事，只能反复重试，用户看到的就是
// 「卡着计时」。限额是**兜底**不是**业务规则**，业务规则得由客户按自己机器定。
//
// 非法值一律忽略并退回默认（不静默接受半懂的值——写错就按默认跑，
// 体检脚本会把生效值打印出来，运维能一眼看到自己写的那行没吃上）。
func DefaultExecConfig() ExecConfig {
	return ExecConfig{
		WorkRoot:    "/var/lib/skillforge/work",
		Python:      pythonLabel(ResolvePython()),
		Timeout:     envDuration(EnvExecTimeout, 30*time.Second),
		MemoryMax:   envSize(EnvExecMemory, "256M"),
		CPUQuota:    envCPUQuota(EnvExecCPU, "50%"),
		TasksMax:    envInt(EnvExecTasks, 32),
		SandboxUser: "nobody",
		MaxOutput:   65536,
	}
}

// 限额覆盖用的环境变量名。写进常量是为了体检/文档能引用同一份字面量。
const (
	EnvExecTimeout = "SKILLFORGE_EXEC_TIMEOUT"
	EnvExecMemory  = "SKILLFORGE_EXEC_MEMORY"
	EnvExecCPU     = "SKILLFORGE_EXEC_CPU"
	EnvExecTasks   = "SKILLFORGE_EXEC_TASKS"
)

// envDuration 读时长覆盖：接受 "90s" / "5m" / 裸数字（按秒算）。
// 下限 5s（再短连解释器都没起来），上限 30m（单机部署不该有一条工具调用
// 占住半小时——那已经该走后台任务而不是同步工具）。
func envDuration(name string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	var d time.Duration
	if n, err := strconv.Atoi(raw); err == nil {
		d = time.Duration(n) * time.Second
	} else if parsed, err := time.ParseDuration(raw); err == nil {
		d = parsed
	} else {
		return def
	}
	if d < 5*time.Second || d > 30*time.Minute {
		return def
	}
	return d
}

// envSize 读 systemd 内存限额：形如 256M / 1G / 2G。
// 认得出才用，认不出退回默认；下限 64M（小于它连 python 都起不来）。
func envSize(name, def string) string {
	raw := strings.ToUpper(strings.TrimSpace(os.Getenv(name)))
	if raw == "" {
		return def
	}
	m := sizeFormat.FindStringSubmatch(raw)
	if m == nil {
		return def
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return def
	}
	mult := map[string]int{"K": 1, "M": 1 << 10, "G": 1 << 20, "T": 1 << 30, "": 1}[m[2]]
	if n*mult < 64*1024 { // < 64M：连解释器都起不来
		return def
	}
	return raw
}

// sizeFormat 只认「数字 + 可选单位」这一种形态。systemd 还接受 50% / infinity，
// 但给内存设百分数或无限在「沙箱兜底」语义下没意义，直接不收。
var sizeFormat = regexp.MustCompile(`^([0-9]+)(K|M|G|T)?$`)

// envCPUQuota 读 CPU 配额：形如 "50%" / "200%"。
func envCPUQuota(name, def string) string {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	if !cpuQuotaFormat.MatchString(raw) {
		return def
	}
	return raw
}

var cpuQuotaFormat = regexp.MustCompile(`^[0-9]+%$`)

// envInt 读正整数覆盖（TasksMax）。
func envInt(name string, def int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// RunPythonTool 让模型现场写 Python 处理数据。这是「工具组合」的粘合剂。
//
// 安全前提（本项目服务以 root 运行且公网可达，因此这里是唯一防线）：
//   - 降权：systemd User=nobody，绝不以 root 执行
//   - 断网：PrivateNetwork=yes（要联网必须走 http_request，那条路径可审计、有 SSRF 拦截）
//   - 只读根：ProtectSystem=strict，仅工作区可写；ProtectHome=yes 挡掉 /root
//   - 限额：MemoryMax/CPUQuota/TasksMax/RuntimeMaxSec
//   - 提权封堵：NoNewPrivileges + SystemCallFilter=@system-service
//
// 若沙箱不可用（systemd-run 缺失等）**拒绝执行**，绝不静默降级到裸跑。
type RunPythonTool struct {
	cfg ExecConfig
}

// NewRunPythonTool 构造 run_python 工具。
func NewRunPythonTool(cfg ExecConfig) *RunPythonTool {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultExecConfig().Timeout
	}
	if cfg.MaxOutput <= 0 {
		cfg.MaxOutput = DefaultExecConfig().MaxOutput
	}
	if cfg.WorkRoot == "" {
		cfg.WorkRoot = DefaultExecConfig().WorkRoot
	}
	if cfg.Python == "" {
		cfg.Python = DefaultExecConfig().Python
	}
	if cfg.SandboxUser == "" {
		cfg.SandboxUser = DefaultExecConfig().SandboxUser
	}
	if cfg.MemoryMax == "" {
		cfg.MemoryMax = DefaultExecConfig().MemoryMax
	}
	if cfg.CPUQuota == "" {
		cfg.CPUQuota = DefaultExecConfig().CPUQuota
	}
	if cfg.TasksMax <= 0 {
		cfg.TasksMax = DefaultExecConfig().TasksMax
	}
	return &RunPythonTool{cfg: cfg}
}

func (t *RunPythonTool) Name() string { return "run_python" }

func (t *RunPythonTool) Description() string {
	// 限额要**按生效值**写出来，不能钉死成「30 秒 / 256MB」：
	// 客户调大 SKILLFORGE_EXEC_MEMORY 之后，如果模型看到的仍是 256MB，
	// 它会继续拒绝写「整份读进来算」的脚本 —— 旋钮调了等于没调。
	return fmt.Sprintf("在沙箱里执行一段 Python 代码来处理数据、做计算、生成文件。沙箱限制：无网络、文件系统只读（当前工作目录可写）、%s超时、%s 内存、最多 %d 进程。",
		humanDuration(t.cfg.Timeout), t.cfg.MemoryMax, t.cfg.TasksMax) +
		"用 print() 输出结果（会被截断到 64KB）；需要交文件给用户就用 open('结果.xlsx','wb') 写到当前目录，运行结束后会自动作为附件交付。" +
		"因为是离线沙箱，不要写需要联网的代码（如 requests）；需要联网请改用 http_request 工具。可用标准库：csv/json/math/statistics/datetime/re 等；第三方库不保证存在。"
}

// humanDuration 把限额时长写成人话（模型读的是这段描述，别给它 "30s" 这种半截值）。
func humanDuration(d time.Duration) string {
	switch {
	case d%(time.Minute) == 0 && d >= time.Minute:
		return fmt.Sprintf("%d 分钟", int(d/time.Minute))
	case d%(time.Second) == 0:
		return fmt.Sprintf("%d 秒", int(d/time.Second))
	default:
		return d.Truncate(time.Second).String()
	}
}

func (t *RunPythonTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"code": map[string]any{
				"type":        "string",
				"description": "要执行的 Python 源码。建议先算再打印，输出尽量精简（只 print 关键结果）。",
			},
		},
		"required": []string{"code"},
	}
}

// Run 执行代码。
func (t *RunPythonTool) Run(ctx context.Context, args map[string]any) (Result, error) {
	code := Str(args, "code")
	if strings.TrimSpace(code) == "" {
		return Result{}, errors.New("code 参数为空")
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		// fail closed：沙箱不可用就不执行
		return Result{}, errors.New("沙箱不可用（缺少 systemd-run），已拒绝执行代码")
	}

	dir, err := t.makeWorkspace()
	if err != nil {
		return Result{}, fmt.Errorf("准备工作区失败: %w", err)
	}
	defer os.RemoveAll(dir)

	script := filepath.Join(dir, "main.py")
	if err := os.WriteFile(script, []byte(code), 0o644); err != nil {
		return Result{}, fmt.Errorf("写入代码失败: %w", err)
	}
	// 交给降权用户
	_ = os.Chown(script, sandboxUID(t.cfg.SandboxUser), sandboxUID(t.cfg.SandboxUser))

	// 给本次执行起一个自己可控的 unit 名：超时要靠它把 systemd 里的实际进程杀掉。
	// 只管 Go 侧的 cmd 是不够的——杀掉 systemd-run 不会带走它启动的进程，会留孤儿。
	unit := "sfexec-" + randomHex(6)
	runCtx, cancel := context.WithTimeout(ctx, t.cfg.Timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "systemd-run", t.systemdArgs(dir, unit)...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()

	out := buf.String()
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded) ||
		strings.Contains(out, "Timeout") || strings.Contains(out, "timeout")
	if timedOut {
		// 兜底清理：SIGKILL 整个 unit 的 cgroup（子进程一起带走），再清掉 failed 状态
		_ = exec.Command("systemctl", "kill", "--signal=SIGKILL", unit).Run()
		_ = exec.Command("systemctl", "reset-failed", unit).Run()
	}
	// 收尾：unit 失败/残留时清理，避免 systemd 里堆一堆 failed 单元
	_ = exec.Command("systemctl", "reset-failed", unit).Run()

	// 收集产出文件
	files := t.collectArtifacts(dir)

	exitCode := 0
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}
	body := Truncate(strings.TrimSpace(out), t.cfg.MaxOutput)
	if body == "" {
		body = "(无输出)"
	}
	status, hint := classifySandboxFailure(exitCode, out, timedOut, t.cfg)

	content := fmt.Sprintf("沙箱执行结果（%s）：\n%s", status, body)
	if hint != "" {
		content += "\n" + hint
	}
	if len(files) > 0 {
		names := make([]string, 0, len(files))
		for _, f := range files {
			names = append(names, f.Name)
		}
		content += fmt.Sprintf("\n产出文件已交付给用户: %s", strings.Join(names, ", "))
	}
	if runErr != nil && exitCode != 0 && !timedOut && hint == "" {
		content += "\n（代码有报错，请根据上面的 stderr 修正后重试）"
	}

	return Result{
		Content: content,
		Files:   files,
		Display: status,
	}, nil
}

// classifySandboxFailure 把沙箱的失败形态翻译成「模型能照着改、人也能照着改」的一句话。
//
// 为什么值得单独一个函数：改造前所有非超时的失败都挤成一句「退出码 -1」。
// 单机离线部署的真实场景是——模型写了段解析 17MB PDF 的脚本，被 cgroup OOM 秒杀，
// 拿回一句「退出码 -1」，它只能猜（「代码有报错，请修正后重试」）然后原样重试，
// 用户界面上看到的是一动不动跳秒的计时器。给不出原因，就会变成死循环烧时间。
//
// 判据只用**日志里真出现的字符串**（systemd 的 OOM 记录 / 内核的 Killed），
// 不靠调用方传"我猜是内存问题"——猜错的话等于把另一种故障引到错的修法上。
func classifySandboxFailure(exitCode int, out string, timedOut bool, cfg ExecConfig) (status, hint string) {
	if timedOut {
		return fmt.Sprintf("超时被杀（上限 %s）", cfg.Timeout),
			fmt.Sprintf("（脚本跑太久了。上限可用 %s 调大；更稳的做法是分页/分块处理，"+
				"不要一次把整份文件读进内存。）", EnvExecTimeout)
	}

	oom := looksLikeOOM(out)
	pyMem := !oom && looksLikePythonMemoryError(out)
	switch {
	case oom:
		// 137 = 128+9（SIGKILL），是 cgroup OOM 与 systemd RuntimeMaxSec 的共用码，
		// 所以判 OOM 优先看日志串，退出码只作为旁证。
		return fmt.Sprintf("被内存上限杀掉（MemoryMax=%s）", cfg.MemoryMax),
			fmt.Sprintf("（脚本吃内存超过了 %s。调大：在 %s 里设 %s=1G（或更大）后重启服务；"+
				"同时建议改成流式/分块解析，别一次读整份文件。）",
				cfg.MemoryMax, "/opt/skillforge/skillforge.env", EnvExecMemory)
	case pyMem:
		// 与上面区分开：进程**没被**上限杀，是自己申请不到内存抛的异常。
		// 两种现场的修法不一样，混成一句「被上限杀掉」会把人引到调限额上，
		// 而真正该做的是不要一次性把整份文件读进内存。
		return "脚本内存不足（自己抛的 MemoryError，不是被沙箱上限杀掉）",
			"（多半是一次性读了整份文件。改成流式/分页读取，或先抽样再全量；" +
				"确实需要大内存时可让运维调大 " + EnvExecMemory + "。）"
	case exitCode == -1:
		// 走到这里说明不是超时、日志里也没有 OOM 记录：多半是沙箱没能启动
		// （systemd-run 缺失、unit 名冲突、SandboxUser 不存在）。这类故障
		// 模型自己改代码永远修不好，必须明说「不是你的代码问题」。
		return "沙箱未正常结束（不是脚本语法问题）",
			"（进程没有正常退出，且没有超时/内存记录：通常是沙箱自身没起来" +
				"（systemd-run 缺失、降权用户不存在、unit 冲突）。请让运维跑 " +
				"`skillforge -selftest` 看沙箱一节，不要靠改代码重试。）"
	default:
		return fmt.Sprintf("退出码 %d", exitCode), ""
	}
}

// looksLikeOOM 判断输出里有没有「被内存上限杀掉」的实证。
//
// 只认内核/systemd 层面的证据。python 自己抛的异常不算——那是另一类现场
// （见 looksLikePythonMemoryError），修法不同，不能混。
func looksLikeOOM(out string) bool {
	needles := []string{
		"oom-kill",               // 内核：Memory cgroup out of memory: Killed process
		"Out of memory",          // systemd：unit ... failed with result 'oom-kill'
		"Memory cgroup out of",   // 内核行首
		"cannot allocate memory", // 系统调用层（
	}
	for _, n := range needles {
		if strings.Contains(out, n) {
			return true
		}
	}
	return false
}

// looksLikePythonMemoryError 认 python 层的内存不足异常。
//
// ⚠️ 必须带词边界：`numpy.core._exceptions._ArrayMemoryError` 里含 "MemoryError"
// 子串，用 Contains 会把它当普通 MemoryError 抓住——但 numpy 的分配失败与
// 解释器抛 MemoryError 是两种现场。这条边界是单测逼出来的（第一版就是 Contains，
// 被自己的反例测试抓住）。`\b` 在 `_ArrayMemoryError` 这种前面紧跟字母的写法上
// 不成立，正好把 numpy 的类名排除在外。
var pyMemoryError = regexp.MustCompile(`\bMemoryError\b`)

func looksLikePythonMemoryError(out string) bool {
	return pyMemoryError.MatchString(out)
}

// systemdArgs 组装沙箱命令行。属性组合已在本机实测验证（见 docs/agent-tools-plan.md）。
func (t *RunPythonTool) systemdArgs(dir, unit string) []string {
	uid := fmt.Sprint(sandboxUID(t.cfg.SandboxUser))
	args := []string{
		"--pipe", "--wait", "--collect", "--quiet",
		"--unit=" + unit,
		"--property=User=" + t.cfg.SandboxUser,
		"--property=Group=" + sandboxGroup(t.cfg.SandboxUser),
		"--property=PrivateNetwork=yes",   // 断网
		"--property=ProtectSystem=strict", // 全盘只读
		"--property=ProtectHome=yes",      // 挡 /root /home
		"--property=NoNewPrivileges=yes",
		"--property=MemoryMax=" + t.cfg.MemoryMax,
		"--property=CPUQuota=" + t.cfg.CPUQuota,
		"--property=TasksMax=" + fmt.Sprint(t.cfg.TasksMax),
		"--property=SystemCallFilter=@system-service",
		// +2s 缓冲：正常路径由 Go 侧 ctx 先超时并显式 kill unit，这里只是最后一道保险
		"--property=RuntimeMaxSec=" + fmt.Sprint(int(t.cfg.Timeout.Seconds())+2),
		"--property=ReadWritePaths=" + dir,
		// 工作目录必须走 --property=WorkingDirectory=，**不能用 --working-directory=**：
		// 后者是 systemd-run 在 systemd v243 才加的 CLI 选项，而 RHEL/CentOS/AlmaLinux 8
		// 带的是 systemd 239 —— 在那些机器上 systemd-run 直接
		// `unrecognized option '--working-directory=...'` 退出，一次代码都执行不了。
		// 「执行代码」工具会全部失败，装后自检也只报「探针没有报告 uid」（真现场）。
		// 属性名 WorkingDirectory 从 systemd v203 就有，239 上实测可用（uid=65534 拿到）。
		"--property=WorkingDirectory=" + dir,
		"--setenv=PYTHONIOENCODING=utf-8",
		"--setenv=PYTHONDONTWRITEBYTECODE=1",
		"--setenv=LANG=C.UTF-8",
		"--setenv=HOME=" + dir,
		"--setenv=TMPDIR=" + dir,
		"--setenv=SANDBOX_UID=" + uid,
	}
	// 注意：不要加 PrivateTmp=yes（本机容器环境不支持，会 exit 200，已实测）
	return append(args, t.cfg.Python, "main.py")
}

// makeWorkspace 建本次执行的隔离工作区：随机名 + 仅沙箱用户可访问。
func (t *RunPythonTool) makeWorkspace() (string, error) {
	root := t.cfg.WorkRoot
	// 根目录 0711：可进入但不可列目录 → 别人猜不到、也列举不出会话目录
	if err := os.MkdirAll(root, 0o711); err != nil {
		return "", err
	}
	_ = os.Chmod(root, 0o711)

	var dir string
	for i := 0; i < 3; i++ {
		dir = filepath.Join(root, "exec-"+randomHex(8))
		if err := os.MkdirAll(dir, 0o700); err == nil {
			break
		}
	}
	if _, err := os.Stat(dir); err != nil {
		return "", err
	}
	uid, gid := sandboxUID(t.cfg.SandboxUser), sandboxGID(t.cfg.SandboxUser)
	if err := os.Chown(dir, uid, gid); err != nil {
		return "", err
	}
	return dir, nil
}

// collectArtifacts 收集沙箱里产出的文件（排除脚本自身）。
func (t *RunPythonTool) collectArtifacts(dir string) []File {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	const maxFile = 20 << 20
	var out []File
	for _, e := range entries {
		if e.IsDir() || e.Name() == "main.py" || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if len(out) >= 5 {
			break
		}
		p := filepath.Join(dir, e.Name())
		fi, err := e.Info()
		if err != nil || fi.Size() == 0 || fi.Size() > maxFile {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		out = append(out, File{Name: e.Name(), ContentType: contentTypeOf(e.Name()), Bytes: b})
	}
	return out
}

func contentTypeOf(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case ".pdf":
		return "application/pdf"
	case ".csv":
		return "text/csv; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".svg":
		return "image/svg+xml"
	case ".txt", ".md", ".log":
		return "text/plain; charset=utf-8"
	}
	return "application/octet-stream"
}

// randomHex 生成 n 字节的随机十六进制串（工作区名与 unit 名都用它，避免可预测/撞名）。
func randomHex(n int) string {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		// 随机源不可用时退回时间戳，绝不能返回固定值（固定名 = 可预测路径 = 可被抢占）
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw)
}

// sandboxGroup 给出降权用户对应的组名。Ubuntu 上 nobody 的组叫 nogroup，
// 别处可能就叫 nobody——探测一下，拿不到再用 nogroup 兜底。
func sandboxGroup(name string) string {
	if name != "" && name != "nobody" {
		return name
	}
	for _, g := range []string{"nogroup", "nobody"} {
		if err := exec.Command("getent", "group", g).Run(); err == nil {
			return g
		}
	}
	return "nogroup"
}

// sandboxUID 把用户名解析成 uid（解析失败兜底 65534=nobody）。
func sandboxUID(name string) int {
	if name == "nobody" || name == "" {
		return 65534
	}
	return lookupID(name, true)
}

func sandboxGID(name string) int {
	if name == "nobody" || name == "" {
		return 65534
	}
	return lookupID(name, false)
}

// lookupID 从 getent 里解析 uid/gid（第 3 列，格式 name:passwd:id:...）。
func lookupID(name string, isUID bool) int {
	what := "passwd"
	if !isUID {
		what = "group"
	}
	out, err := exec.Command("getent", what, name).Output()
	if err != nil {
		return 65534
	}
	parts := strings.Split(strings.TrimSpace(string(out)), ":")
	if len(parts) > 2 {
		var id int
		if _, err := fmt.Sscanf(parts[2], "%d", &id); err == nil {
			return id
		}
	}
	return 65534
}

// probeTemplate 是沙箱自检探针：在沙箱里跑一遍，回答「我是谁 / 能不能联网 /
// 能不能读机密 / 能不能乱写」，每条都带结论词（OK / 危险 / 证据无效）。
//
// __SECRET_PATHS__ 会被替换成一个 JSON 数组字面量（同时是合法的 Python 字面量）。
//
// ⚠️ 探针必须先 os.path.exists 再 open：路径不存在时 open 抛 FileNotFoundError，
// 会被同一个 except 捕获成「拒绝(OK)」——那是假证据，会把「根本没检查」伪装成「检查通过」。
const probeTemplate = `
import os, socket, json
paths = json.loads('__SECRET_PATHS__')
r = {}
r["uid"] = str(os.getuid())
try:
    socket.create_connection(("1.1.1.1", 80), timeout=2); r["network"] = "通(危险)"
except Exception:
    r["network"] = "断(OK)"
for p in paths:
    # ⚠️ 这里**不能**用 os.path.exists()：它会吞掉 EACCES 一律返回 False。
    # 离线包装完后 data/ 是 0700 root（install.sh 故意的），沙箱是 nobody，
    # 于是「存在但父目录不让进」被写成「文件不存在」——自检在真机上假红（Bug H）。
    # 必须问内核要 errno，把「真不存在」和「存在但不可达」分开。
    # 探针只报原始事实，怎么解读交给 Go 侧 classifyReadEvidence（可被单测覆盖）。
    try:
        os.stat(p); state = "stat_ok"
    except FileNotFoundError:
        state = "enoent"
    except NotADirectoryError:
        state = "enoent"
    except PermissionError:
        state = "parent_denied"
    except Exception as e:
        state = "err:" + type(e).__name__
    if state == "stat_ok":
        try:
            open(p, "rb").read(1); state = "readable"
        except Exception:
            state = "denied"
    r["read:" + p] = state
try:
    open("/etc/evil", "w"); r["write:/etc"] = "可写(危险)"
except Exception:
    r["write:/etc"] = "拒绝(OK)"
try:
    open("probe.txt", "w").write("ok"); r["write:work"] = "可以(OK)"
except Exception as e:
    r["write:work"] = "失败(异常): " + type(e).__name__
print("|".join("%s=%s" % (k, v) for k, v in r.items()))
`

// SandboxDiagnostics 跑一次探针，返回人类可读的沙箱自检报告（用于验收测试与 /api 自检）。
// 默认探测 /opt/skillforge 下的机密文件；自定义安装前缀请用 SandboxDiagnosticsFor。
func SandboxDiagnostics(ctx context.Context) map[string]string {
	return SandboxDiagnosticsFor(ctx, []string{
		"/opt/skillforge/skillforge.env",
		"/opt/skillforge/data/skillforge.db",
		"/root/.ssh/id_rsa",
	})
}

// SandboxDiagnosticsFor 与 SandboxDiagnostics 相同，但由调用方指定「必须读不到」的机密路径。
//
// ⚠️ 为什么必须让调用方传真实路径：探针里写死一个**不存在的路径**时，Python 抛的是
// FileNotFoundError，也会被同一段 except 捕获成「拒绝(OK)」——装在不同前缀下的实例
// 会拿一个假证据宣称「读不到机密」。证据必须落在真实文件上。
func SandboxDiagnosticsFor(ctx context.Context, secretPaths []string) map[string]string {
	if len(secretPaths) == 0 {
		secretPaths = []string{"/root/.ssh/id_rsa"}
	}
	// JSON 字符串字面量同时是合法的 Python 字面量（双引号 + JSON 转义），
	// 用它把路径安全嵌进代码；用占位符替换而不是 Sprintf——探针里有 `%s` 会打架。
	encoded, _ := json.Marshal(secretPaths)
	probe := strings.Replace(probeTemplate, "__SECRET_PATHS__", string(encoded), 1)
	out := map[string]string{}
	if py := DefaultExecConfig().Python; py != "" {
		// 探针解释器缺失必须在这里单独报：缺 python3 时 systemd-run 只把
		// 「Failed to find executable …」写进 display，不算 error，上层拿到的是
		// 一份没有 uid 的「正常」输出，于是把「环境缺 python3」误诊成「沙箱没降权」。
		// 与其让上层去猜，不如在跑探针之前就查一次，报一句真话。
		// 只在「所有候选 + $PATH 都找不到」时才报这句。ResolvePython 已经跑过一轮
		// 真实探测（含 -c pass），所以走到这里基本就是真没装解释器；如果运维用
		// SKILLFORGE_PYTHON 显式指了一个用不了的解释器，也走这里 —— 那时下面这句
		// 会把 override 点名，免得他去系统里找一个根本不该找的东西。
		if _, err := exec.LookPath(py); err != nil {
			hint := "修复：Debian/Ubuntu 用 apt install python3；" +
				"RHEL/AlmaLinux 最小安装用 dnf install -y python3（离线机挂 ISO 或配本地源）"
			if v := strings.TrimSpace(os.Getenv(EnvPythonOverride)); v != "" {
				hint = fmt.Sprintf("注意：%s=%s 是你显式指定的解释器，它不存在或跑不起来（%s 要能接受 `-c pass`）。"+
					"要么改正这个路径，要么 unset %s 让程序按候选名单自己找",
					EnvPythonOverride, v, v, EnvPythonOverride)
			}
			out["error"] = fmt.Sprintf(
				"目标机没有可用的 python3 解释器（找过 %s 和 $PATH 里的 python3，探针解释器停在 %s）："+
					"代码沙箱的探针与「执行代码」工具都依赖它，**这不是沙箱降权失败**，写作等主功能不受影响。"+
					"解释器装在别处时可以直接指定：SKILLFORGE_PYTHON=/usr/local/bin/python3（写进 %s 最省事）。%s",
				strings.Join(pythonCandidates, " / "), py, "/opt/skillforge/skillforge.env", hint)
			return out
		}
	}
	t := NewRunPythonTool(DefaultExecConfig())
	res, err := t.Run(ctx, map[string]any{"code": probe})
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	parsed := parseProbeOutput(res.Content)
	// 探针没吐 uid 时，**必须把 systemd-run 的原始输出带上去**。
	// 真现场（Bug Y2，AlmaLinux 8 / systemd 239）：systemd-run 报
	// `unrecognized option '--working-directory=...'` 退出，探针一个字都没打，
	// 上层只看到「没有 uid」，于是判成「目标机缺 python3」——客户拿着一个
	// 明明自带的解释器去 dnf install python3，怎么装都修不好。
	// 归因错比不报还坏：把「我们的参数与目标机 systemd 不兼容」伪装成「环境缺件」。
	if parsed["uid"] == "" {
		if d := probeFailureDetail(res.Content); d != "" {
			out["error"] = d
			return out
		}
	}
	for k, v := range parsed {
		if strings.HasPrefix(k, "read:") {
			v = classifyReadEvidence(v)
		}
		out[k] = v
	}
	out["display"] = res.Display
	// 生效限额必须出现在体检报告里。
	// 为什么：客户按文档调了 SKILLFORGE_EXEC_MEMORY=1G，但 env 文件没被服务读到、
	// 或者值写错被退回默认，现场从输出上完全看不出来。把生效值打出来，
	// 「我明明调大了」和「实际还是 256M」就能一眼对上，不用去翻 /proc/<pid>/environ。
	cfg := DefaultExecConfig()
	out["limits"] = fmt.Sprintf("超时=%s 内存=%s CPU=%s 任务数=%d（可用 %s / %s / %s / %s 覆盖）",
		cfg.Timeout, cfg.MemoryMax, cfg.CPUQuota, cfg.TasksMax,
		EnvExecTimeout, EnvExecMemory, EnvExecCPU, EnvExecTasks)
	return out
}

// probeFailureDetail 从探针回执里抠出「到底哪一步失败了」。
//
// 规则刻意保守：先找 systemd-run / systemd 自己的报错行（这类信息一眼能定位）；
// 找不到也绝不返回空 —— 只要探针一个字都没吐 uid，就把原始回执原样交上去。
// 宁可让用户看一段生硬的 systemd 输出，也不要给他一句编好的、指向错误方向的
// 「你没装 python3」。纯函数，单测直接打它。
func probeFailureDetail(content string) string {
	const maxLen = 400
	c := strings.TrimSpace(content)
	if c == "" {
		return "沙箱探针没有任何输出（systemd-run 连探针都没跑起来，且未给出原因）"
	}
	markers := []string{
		"unrecognized option",
		"Failed to ",
		"Failed at step",
		"not found",
		"Permission denied",
		"Operation not permitted",
	}
	for _, line := range strings.Split(c, "\n") {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		for _, m := range markers {
			if strings.Contains(l, m) {
				d := "沙箱探针未返回证据，systemd-run 原始报错：" + l
				if len(d) > maxLen {
					d = d[:maxLen] + "…"
				}
				return d
			}
		}
	}
	d := "沙箱探针未返回证据，原始回执：" + strings.Join(strings.Fields(c), " ")
	if len(d) > maxLen {
		d = d[:maxLen] + "…"
	}
	return d
}

// parseProbeOutput 解析探针输出的 `k=v|k=v` 行。
//
// 单独拆出来是为了让「原始证据 → 结论」这条链在 CI 里可测：探针侧只能给原始事实
// （读成功了 / errno 是啥），解读放在 Go 里才锁得住。
func parseProbeOutput(content string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		for _, kv := range strings.Split(line, "|") {
			if i := strings.Index(kv, "="); i > 0 {
				out[strings.TrimSpace(kv[:i])] = strings.TrimSpace(kv[i+1:])
			}
		}
	}
	return out
}

// classifyReadEvidence 把探针给的原始判定翻译成结论词。纯函数，单测直接打它。
//
// 历史坑（Bug H）：原先探针用 os.path.exists() 判存在性，它吞掉 EACCES 返回 False，
// 于是「文件在、但父目录 0700 让沙箱连 stat 都进不去」被写成「文件不存在(证据无效)」，
// 自检在真机上直接判失败、客户以为系统坏了。EACCES 恰恰是**更强的**安全证据：
// 内核只在对象存在时才会因权限拒绝访问。所以 parent_denied 必须判通过。
func classifyReadEvidence(raw string) string {
	switch raw {
	case "readable":
		return "可读(危险)"
	case "denied":
		return "拒绝(OK)"
	case "parent_denied":
		return "拒绝(OK)（父目录不可达，沙箱连 stat 都进不去）"
	case "enoent":
		return "文件不存在(证据无效)"
	default:
		return "探针未给出有效判定(证据无效): " + raw
	}
}
