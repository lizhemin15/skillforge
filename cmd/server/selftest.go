package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lizhemin15/skillforge/internal/config"
	"github.com/lizhemin15/skillforge/internal/docgen"
	"github.com/lizhemin15/skillforge/internal/tools"
	"github.com/lizhemin15/skillforge/internal/version"
)

// selftest.go 实现 `skillforge -selftest`：离线安装后的「三证据」自检。
//
// 为什么需要它：离线包交付到内网机器上时，那里既没有网络也没有 LLM，
// 「服务能起来」远远不等于「能干活」。真正的坑都藏在沉默失败里——
//   - PDF 里数字全消失（Bug G）：字体路径存在 ≠ 能渲染数字；
//   - 代码沙箱形同虚设：服务一切正常，但沙箱若没降权/没断网，
//     公网用户就能以 root 跑代码读走密钥。
//
// 所以自检只回答三个能用证据说话的问题，任一不过就退出码 1
// （可直接接进 install.sh；也可给监控轮询当健康检查）：
//  1. 版本号是否被注入（证明跑的是 CI 产物，不是某次手工 dev 构建）；
//  2. PDF 中文字体是否对门槛字符集全覆盖，且**真能从生成的 PDF 里回读出来**；
//  3. 代码沙箱是否真的降权 + 断网 + 写不进敏感路径 + 读不到真实机密文件。
//
// 全程零网络、零 LLM 调用，可离线执行。
//
// 结构上刻意拆成两半：
//   - checkXxx() 负责「采集证据」（会碰字体、碰 systemd，环境相关）；
//   - judgeXxx() 负责「判定证据」（纯函数，可用合成输入单测）。
//
// 这样「自检在 Bug G 复发时到底会不会变红」这件事本身有测试守着（见 selftest_test.go），
// 而不是靠人工盯着。

// 回读断言用的样本：这正是 Bug G 里会消失的两类字符（数字 / 中文）。
const (
	selfTestDigits  = "0123456789"
	selfTestChinese = "产品报价单"
)

type selfCheck struct {
	name   string
	detail []string
	ok     bool
}

// runSelfTest 执行自检，返回进程退出码。
func runSelfTest() int {
	fmt.Printf("SkillForge 自检（离线可用：不联网、不调用 LLM）\n")
	fmt.Printf("二进制  : %s\n", os.Args[0])
	fmt.Printf("版本    : %s\n", version.String())
	fmt.Printf("──────────────────────────────────────────────\n")

	checks := []selfCheck{
		judgeVersion(version.Version, version.Commit),
		checkPDFFont(),
		checkSandbox(),
	}

	failed := 0
	for i, c := range checks {
		status := "OK"
		if !c.ok {
			status = "失败"
			failed++
		}
		fmt.Printf("[%d/%d] %s …… %s\n", i+1, len(checks), c.name, status)
		for _, line := range c.detail {
			fmt.Printf("      %s\n", line)
		}
	}

	fmt.Printf("──────────────────────────────────────────────\n")
	if failed > 0 {
		fmt.Printf("自检结果：%d/%d 项未通过，请按上面提示修复\n", failed, len(checks))
		return 1
	}
	fmt.Printf("自检结果：全部通过（%d/%d）\n", len(checks), len(checks))
	return 0
}

// judgeVersion 确认版本号是构建期注入的（即跑的是 CI 产物，而非本地 dev 构建）。
func judgeVersion(v, commit string) selfCheck {
	c := selfCheck{name: "版本注入"}
	if v == "" || v == "dev" {
		c.detail = append(c.detail,
			"版本号仍是 \"dev\"，说明这个二进制没经过 CI 注入版本（本地 go build？）",
			"离线安装包应当使用 Release 里带版本号的产物")
		return c
	}
	c.ok = true
	c.detail = append(c.detail, fmt.Sprintf("版本 %s（commit %s）", v, commit))
	return c
}

// fontEvidence 是一次字体体检的全部原始证据。
type fontEvidence struct {
	Path    string // resolvePDFFont() 选中的字体文件（空 = 一个可加载的都没有）
	Missing []rune // 门槛字符集里缺的字形
	Text    string // 从试渲染的 PDF 里回读出的文本
	Bytes   int    // 试渲染出的 PDF 字节数
	Err     error  // 生成/回读过程中的错误
}

// checkPDFFont 采集字体证据：解析字体 → 查覆盖率 → 真渲染一次 → 回读文本。
func checkPDFFont() selfCheck {
	ev := fontEvidence{Path: docgen.PDFFontPath()}
	if ev.Path != "" {
		ev.Missing = docgen.PDFFontMissingRunes()
		if len(ev.Missing) == 0 {
			pdf, err := docgen.BuildSelfTestPDF()
			if err != nil {
				ev.Err = fmt.Errorf("试渲染 PDF 失败：%w", err)
			} else {
				ev.Bytes = len(pdf)
				text, err := docgen.ExtractPDFText(pdf)
				if err != nil {
					ev.Err = fmt.Errorf("回读 PDF 文本失败：%w", err)
				}
				ev.Text = text
			}
		}
	}
	return judgePDFFont(ev)
}

// judgePDFFont 判定字体证据。纯函数：给定证据，结论必须是确定的。
func judgePDFFont(ev fontEvidence) selfCheck {
	c := selfCheck{name: "PDF 中文字体"}

	if ev.Path == "" {
		c.detail = append(c.detail,
			"系统里找不到任何可加载的 .ttf 中文字体（gopdf 不支持 .ttc / .otf）",
			"修复：把离线包 fonts/ 里的 .ttf 装到 /usr/share/fonts/truetype/ 下，",
			"      或用 SKILLFORGE_PDF_FONT_FILE=<绝对路径.ttf> 显式指定")
		return c
	}
	c.detail = append(c.detail, "选用字体："+ev.Path)

	if len(ev.Missing) > 0 {
		c.detail = append(c.detail,
			fmt.Sprintf("缺 %d 个必备字符，这些字在 PDF 里会渲染成空白：%q", len(ev.Missing), string(ev.Missing)),
			"修复：换一个覆盖中文+数字+拉丁的 .ttf，并用 SKILLFORGE_PDF_FONT_FILE 指定")
		return c
	}

	if ev.Err != nil {
		c.detail = append(c.detail, ev.Err.Error())
		return c
	}
	if ev.Text == "" {
		c.detail = append(c.detail, "试渲染的 PDF 里回读不出任何文本（提取器空手而归，不能当通过）")
		return c
	}
	if !strings.Contains(ev.Text, selfTestDigits) {
		c.detail = append(c.detail,
			"字体覆盖率看着合格，但生成的 PDF 里**回读不到数字** "+selfTestDigits+"——这就是 Bug G（数字静默变空白）的症状",
			fmt.Sprintf("回读到的文本：%q", firstRunes(ev.Text, 60)),
			"常见原因：字体解析对了但绘制路径把字符吞了，或用了不含 ASCII 数字的字体")
		return c
	}
	if !strings.Contains(ev.Text, selfTestChinese) {
		c.detail = append(c.detail,
			"生成的 PDF 里回读不到中文 "+selfTestChinese,
			fmt.Sprintf("回读到的文本：%q", firstRunes(ev.Text, 60)))
		return c
	}

	c.ok = true
	c.detail = append(c.detail,
		fmt.Sprintf("门槛字符集 100%% 覆盖（数字 0-9 / 拉丁 / 中文高频字）"),
		fmt.Sprintf("试渲染 %d 字节 PDF 并回读成功：%q ……", ev.Bytes, firstRunes(ev.Text, 30)))
	return c
}

// checkSandbox 采集沙箱证据：起一个沙箱进程跑探针，看它到底能干什么。
func checkSandbox() selfCheck {
	secretPaths := secretEvidencePaths()
	if len(secretPaths) == 0 {
		return judgeSandbox(nil, nil, fmt.Errorf(
			"找不到任何真实存在的机密文件可作证据（env 文件没装？）——"+
				"拿不存在路径测「读不到」是假证据，不存在的文件当然打不开"))
	}
	if !systemdRunAvailable() {
		return judgeSandbox(nil, secretPaths, fmt.Errorf(
			"缺少 systemd-run，代码沙箱不可用（服务会拒绝执行代码，绝不裸跑）"+
				"；修复：使用带 systemd 的发行版（Debian/Ubuntu 默认都有）"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	return judgeSandbox(tools.SandboxDiagnosticsFor(ctx, secretPaths), secretPaths, nil)
}

// judgeSandbox 判定沙箱证据。纯函数：任何一条「危险」或「证据无效」都必须判失败。
func judgeSandbox(diag map[string]string, secretPaths []string, err error) selfCheck {
	c := selfCheck{name: "代码执行沙箱"}

	if err != nil {
		c.detail = append(c.detail, err.Error())
		return c
	}
	if len(secretPaths) > 0 {
		c.detail = append(c.detail, "机密不可读证据（真实存在的文件）："+strings.Join(secretPaths, ", "))
	}
	if diag == nil {
		c.detail = append(c.detail, "沙箱探针没有返回任何证据")
		return c
	}
	if diag["error"] != "" {
		c.detail = append(c.detail, "沙箱探针执行失败："+diag["error"])
		return c
	}
	if diag["uid"] == "" {
		// 真的发生过：almalinux:8 最小安装（默认不带 python3）上装完，自检报的是
		// 「没有降权」，客户以为沙箱漏了、要开安全事故复盘，实际只是探针解释器不存在。
		// 归因错了比不报还坏——它把「环境缺件」伪装成「安全缺陷」。
		c.detail = append(c.detail, "沙箱探针没有报告 uid —— 这是「证据缺失」，不是「降权失败」："+
			"探针的 python 解释器根本没跑起来，所以 uid 无从得知。"+
			"最常见原因是目标机缺 python3（RHEL/AlmaLinux 最小安装默认不带它）。"+
			"先装 python3 再重跑自检：dnf install -y python3（离线机挂 ISO 或配本地源）。"+
			"在拿到 uid 证据之前，既不能说沙箱安全，也不能把责任算在降权上。")
		return c
	}
	if diag["uid"] != "65534" {
		c.detail = append(c.detail, fmt.Sprintf("没有降权：uid=%s（应为 65534 nobody）；沙箱内进程能以高权限读走机密", diag["uid"]))
		return c
	}
	if !strings.Contains(diag["network"], "断") {
		c.detail = append(c.detail, fmt.Sprintf("沙箱内网络是通的：%s；PrivateNetwork 没生效", diag["network"]))
		return c
	}
	if !strings.Contains(diag["write:work"], "可以") {
		c.detail = append(c.detail, "沙箱内写不了工作目录（工具链会跑不起来）："+diag["write:work"])
		return c
	}

	// 逐条列出证据；任何「危险 / 证据无效」都是失败——最后一道闸。
	c.detail = append(c.detail,
		fmt.Sprintf("uid=%s，网络=%s，写工作区=%s", diag["uid"], diag["network"], diag["write:work"]))
	keys := make([]string, 0, len(diag))
	for k := range diag {
		if k == "display" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch k {
		case "uid", "network", "error":
			continue
		}
		c.detail = append(c.detail, fmt.Sprintf("%s = %s", k, diag[k]))
		if strings.Contains(diag[k], "危险") {
			c.detail = append(c.detail, "↑ 这一条不达标，沙箱没拦住，属于严重问题")
			return c
		}
		if strings.Contains(diag[k], "证据无效") {
			c.detail = append(c.detail, "↑ 这一条是无意义证据（文件不存在），不能算通过")
			return c
		}
	}

	c.ok = true
	return c
}

// secretEvidencePaths 给出「沙箱必须读不到」的真实存在文件列表。
//
// 只收真实存在的文件：不存在的路径在探针里同样会报「打不开」，
// 那样的 OK 是假证据（曾经把 /opt/skillforge/skillforge.env 写死在探针里，
// 换一个安装前缀就会拿一个不存在路径的「拒绝」当通过）。
func secretEvidencePaths() []string {
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		if _, err := os.Stat(p); err != nil {
			return
		}
		for _, e := range out {
			if e == p {
				return
			}
		}
		out = append(out, p)
	}

	add(os.Getenv("SKILLFORGE_ENV_FILE"))
	for _, p := range candidateEnvFiles() {
		add(p)
	}
	add(config.Load().DBPath)
	add(defaultDBPath())
	// 底裤级探针：任何 Linux 上都存在的 root 私密文件。
	add("/etc/shadow")
	add("/root/.ssh/id_rsa")
	return out
}

// candidateEnvFiles 给出「这台机器上 env 文件可能在的地方」。
//
// 离线包把二进制和 skillforge.env 装在同一目录，所以先看二进制旁边——
// 这样客户装到 /opt/xxx、/srv/yyy 任意前缀都不需要额外配置。
func candidateEnvFiles() []string {
	var out []string
	if exe, err := os.Executable(); err == nil {
		out = append(out, filepath.Join(filepath.Dir(exe), "skillforge.env"))
	}
	out = append(out,
		"/opt/skillforge/skillforge.env",
		"/etc/skillforge/skillforge.env")
	return out
}

// defaultDBPath 返回「没显式配 SKILLFORGE_DB 时」数据库落点，供自检找到真实证据文件。
func defaultDBPath() string {
	if p := os.Getenv("SKILLFORGE_DB"); p != "" {
		return p
	}
	if d := config.Load().DataDir; d != "" {
		return filepath.Join(d, "skillforge.db")
	}
	return ""
}

// systemdRunAvailable 判断宿主上是否有 systemd-run。
func systemdRunAvailable() bool {
	for _, p := range []string{"/usr/bin/systemd-run", "/bin/systemd-run"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// firstRunes 截取字符串前 n 个字符，用于回读证据的简短展示。
func firstRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
