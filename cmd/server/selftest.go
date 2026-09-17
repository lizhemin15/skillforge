package main

import (
	"bufio"
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/lizhemin15/skillforge/internal/config"
	"github.com/lizhemin15/skillforge/internal/docgen"
	"github.com/lizhemin15/skillforge/internal/ocrsvc"
	"github.com/lizhemin15/skillforge/internal/tlsconf"
	"github.com/lizhemin15/skillforge/internal/tools"
	"github.com/lizhemin15/skillforge/internal/version"
)

// selftest.go 实现 `skillforge -selftest`：离线安装后的「四证据」自检。
//
// 为什么需要它：离线包交付到内网机器上时，那里既没有网络也没有 LLM，
// 「服务能起来」远远不等于「能干活」。真正的坑都藏在沉默失败里——
//   - PDF 里数字全消失（Bug G）：字体路径存在 ≠ 能渲染数字；
//   - 文档解析服务（ocrd）没在运行：主服务界面一切正常，但所有「读文档」的能力全废。
//     用户看到的只是「生成的技能跟我给的素材没关系」——素材压根没被解析出来，
//     和「解析服务坏了」没有任何直观联系，倒查成本极高（线上已发生过）；
//   - 代码沙箱形同虚设：服务一切正常，但沙箱若没降权/没断网，
//     公网用户就能以 root 跑代码读走密钥。
//
// 所以自检只回答这些能用证据说话的问题，任一不过就退出码 1
// （可直接接进 install.sh；也可给监控轮询当健康检查）：
//  1. 版本号是否被注入（证明跑的是 CI 产物，不是某次手工 dev 构建）；
//  2. 时区库能不能解析 TZ（没网、没 /usr/share/zoneinfo 时时间会静默差 8 小时）；
//  3. PDF 中文字体是否对门槛字符集全覆盖，且**真能从生成的 PDF 里回读出来**；
//  4. 文档解析服务是否真的能干活（读实例配置 → 找 systemd 单元 → 探 /health）；
//  5. https 出站所需的 CA 信任库在不在（缺了 TLS 握手会以各种面孔失败）；
//  6. 代码沙箱是否真的降权 + 断网 + 写不进敏感路径 + 读不到真实机密文件。
//
// 第 3 项可以判「跳过」：「这台机器按设计就没装解析服务」（--no-ocr）是合法形态，
// 判红会把预期行为误报成安装失败。跳过与通过的区分见 judgeParseService。
//
// 全程零网络、零 LLM 调用，可离线执行。
//
// 结构上刻意拆成两半：
//   - checkXxx() 负责「采集证据」（会碰字体、碰 systemd、发一次本机 HTTP 探活，环境相关）；
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
	// skip：这一项在**本机**按设计不适用（例：--no-ocr 安装没有解析服务）。
	// 跳过不算失败、也不计入「N/N 通过」的分母，但必须显式打印出来 ——
	// 静默跳过等于让用户以为自己有这项能力，那是最坏的一种误导。
	skip bool
	// unavail：这项能力在**本机的环境里根本给不出来**（例：systemd 219 的 systemd-run
	// 提供不了 PrivateNetwork=/ProtectSystem=strict，沙箱不成立）。
	//
	// 与另外两种状态的分界（2026-09-17 客户现场后加的）：
	//   * 失败   = 我们的东西坏了，客户按提示修就能好；
	//   * skip   = 按设计就不该跑（配置决定），不算缺件；
	//   * unavail= 环境给不了 —— 客户没有东西可修，只能换基座或接受缺这一项功能。
	// 这一类的历史形态是：客户看到一句 `systemd-run: unrecognized option '--pipe'`，
	// 既不知道该改什么，也没人告诉他「这台机器永远做不到」。
	// 所以它必须显式打印（绝不显示成 OK），又**不能算作「未通过」**（那会把一台正常
	// 装好的机器判成装失败）；判定理由与修法要一起给出来。
	unavail bool
}

// runSelfTest 执行自检，返回进程退出码。
func runSelfTest() int {
	fmt.Printf("SkillForge 自检（离线可用：不联网、不调用 LLM）\n")
	fmt.Printf("二进制  : %s\n", os.Args[0])
	fmt.Printf("版本    : %s\n", version.String())
	// 关键：先把实例 env 文件里的配置搬进本进程环境。
	//
	// 离线安装把 SKILLFORGE_OCR_URL / SKILLFORGE_PDF_FONT_FILE / SKILLFORGE_DB 这些
	// 写在 <prefix>/skillforge.env 里（由 systemd 注入服务进程），而 `-selftest` 是
	// 运维手动敲的，环境里什么都没有。不读它就会发生两类误判：
	//   - 换了 OCR_PORT 的实例 → 自检去探默认 8093 → 明明好的却报红；
	//   - 字体/数据库换成专门位置 → 自检拿默认位置找不到文件 → 报「证据无效」。
	// 那种红是最费客户时间的红：他要先证明自己没装错，才能开始排查。
	if src := loadInstanceEnv(); src != "" {
		fmt.Printf("配置    : %s\n", src)
	}
	fmt.Printf("──────────────────────────────────────────────\n")

	checks := []selfCheck{
		judgeVersion(version.Version, version.Commit),
		checkTimezone(),
		checkPDFFont(),
		checkParseService(),
		checkTrustStore(),
		checkSandbox(),
	}

	failed, skipped, unavail := 0, 0, 0
	for i, c := range checks {
		status := "OK"
		switch {
		case c.unavail:
			// 环境给不了：显式打印，但不算「未通过」（那是我们的东西坏了才用的词）。
			status = "不可用（环境）"
			unavail++
		case c.skip:
			status = "跳过"
			skipped++
		case !c.ok:
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
	if unavail > 0 {
		// 有「环境给不了」的项：既不能报「全部通过」（客户会以为这项能力在跑），
		// 也不能报「未通过」（他按提示修不了，而且这台机器是正常装好的）。
		line := fmt.Sprintf("自检结果：%d/%d 项通过", len(checks)-skipped-unavail, len(checks))
		if skipped > 0 {
			line += fmt.Sprintf("，另有 %d 项按本机配置跳过", skipped)
		}
		line += fmt.Sprintf("；%d 项因本机环境不支持未启用（不影响其它功能，详见上）", unavail)
		fmt.Println(line)
		return 0
	}
	if skipped > 0 {
		fmt.Printf("自检结果：全部通过（%d/%d，另有 %d 项按本机配置跳过）\n",
			len(checks)-skipped, len(checks), skipped)
		return 0
	}
	fmt.Printf("自检结果：全部通过（%d/%d）\n", len(checks), len(checks))
	return 0
}

// ---------- 时区 / 时间 ----------
//
// 这一项为什么值得进自检：时区坏了**不会报错**，只会让所有时间偏 8 小时。
// 客户能看到的现象是「日志时间不对」「导出的文档里日期是昨天」，然后去翻代码。
// 真因通常两个：
//   a. TZ 写成了系统认不出的名字（比如写成 Asia/shanghai 大小写错了、或写成 CST ——
//      CST 在 tzdata 里是「中国标准时间是 +08」还是「美国中部时间」取决于实现，
//      这种歧义名绝不能算可用配置）；
//   b. 机器上根本没有时区库（最小化安装、精简容器）而二进制也没内嵌一份。
//
// 采集证据的部分（checkTimezone）与环境相关；判定部分（judgeTimezone）是纯函数。

// tzSystemDirs 是 Go/glibc 会去找时区文件的系统目录（按查找顺序）。
// 只要其一有内容，就说明这台机器「自带时区库」；全都没有，就只能靠二进制内嵌的 tzdata。
var tzSystemDirs = []string{
	"/usr/share/zoneinfo",
	"/usr/share/lib/zoneinfo",
	"/usr/lib/locale/TZ",
	"/etc/zoneinfo",
}

// tzProbeZone 是与业务无关的固定探测点：它只回答「时区库到底能不能用」。
// 用固定值而不是 TZ 本身，是为了把两件事分开判定 ——
//
//	· 探测点解析失败 = 时区库缺失（环境问题）；
//	· 探测点能解析、TZ 解析失败 = 客户把 TZ 写错了（配置问题）。
//
// 两者的修法完全不同，混在一起报就会把客户指到错的方向。
const tzProbeZone = "Asia/Shanghai"

type tzEvidence struct {
	tzEnv      string   // 实例配置/环境里的 TZ（空 = 没配）
	tzResolved string   // TZ 解析出的时区名（空 = 解析失败或没配）
	utcOffset  string   // 当前偏移，如 "+08:00"
	localNow   string   // 按该时区算出来的当前时间
	probeOK    bool     // 固定探测点能否解析
	probeErr   string   // 探测点解析失败时的原始错误
	systemDirs []string // 存在且有内容的系统时区目录
	gorootZip  string   // 本机 Go 安装目录里的 lib/time/zoneinfo.zip（存在才非空）
}

func checkTimezone() selfCheck {
	ev := tzEvidence{
		tzEnv:      strings.TrimSpace(os.Getenv("TZ")),
		probeErr:   "",
		systemDirs: nonEmptyDirs(tzSystemDirs),
		gorootZip:  gorootZoneinfoZip(),
	}
	if _, err := time.LoadLocation(tzProbeZone); err == nil {
		ev.probeOK = true
	} else {
		ev.probeErr = err.Error()
	}
	// 解析实例配置里的 TZ；没配就按「跟随系统」处理（time.Local）。
	loc := time.Local
	if ev.tzEnv != "" {
		if l, err := time.LoadLocation(ev.tzEnv); err == nil {
			loc = l
			ev.tzResolved = l.String()
		}
	} else {
		ev.tzResolved = time.Local.String()
	}
	now := time.Now().In(loc)
	_, off := now.Zone()
	ev.utcOffset = fmt.Sprintf("%+03d:%02d", off/3600, abs(off%3600)/60)
	ev.localNow = now.Format("2006-01-02 15:04:05")
	return judgeTimezone(ev)
}

// judgeTimezone 是纯函数：给定证据判定「时间会不会错」，并说清是谁的问题、怎么修。
func judgeTimezone(ev tzEvidence) selfCheck {
	c := selfCheck{name: "时区 / 时间"}

	if !ev.probeOK {
		// 时区库没了：这是环境问题，不是配置问题。
		c.ok = false
		c.detail = append(c.detail,
			fmt.Sprintf("时区库不可用：连固定探测点 %s 都解析不了（%s）", tzProbeZone, firstRunes(ev.probeErr, 160)))
		c.detail = append(c.detail, "后果：所有时间按 UTC 走，日志与页面时间会比北京时间差 8 小时，"+
			"且**不会报任何错**，只看现象极难定位。")
		c.detail = append(c.detail, "修法（任选其一）：")
		c.detail = append(c.detail,
			"  · 装系统时区库（联网机器）：Debian/Ubuntu `apt-get install -y tzdata`；"+
				"RHEL/AlmaLinux/CentOS `dnf install -y tzdata`；")
		c.detail = append(c.detail,
			"  · 离线机器：用官方离线包里**内嵌时区库**的版本（本栏存在且为 OK 即说明内嵌生效），"+
				"或把另一台同架构机器的 /usr/share/zoneinfo 整个目录拷过来。")
		return c
	}

	// 时区库能用。区分「配置写错」与「没配（=UTC 静默）」。
	if ev.tzEnv != "" && ev.tzResolved == "" {
		c.ok = false
		c.detail = append(c.detail,
			fmt.Sprintf("配置里写了 TZ=%s，但这个时区名本机解析不了 —— 请改成「区域/城市」形式，如 %s、Asia/Shanghai、UTC。",
				ev.tzEnv, tzProbeZone))
		c.detail = append(c.detail, "别用 CST / GMT+8 / PRC 这类歧义名或缩写："+
			"CST 在不同实现里会被解析成美国中部时间（-06:00），时间会差 14 小时。")
		c.detail = append(c.detail, "改完重启服务生效；本栏复查：skillforge -selftest")
		return c
	}

	c.ok = true
	zoneName := ev.tzResolved
	if zoneName == "" {
		zoneName = "(系统默认)"
	}
	c.detail = append(c.detail, fmt.Sprintf("当前时区：%s（UTC%s，现在 %s）", zoneName, ev.utcOffset, ev.localNow))
	switch {
	case len(ev.systemDirs) > 0:
		c.detail = append(c.detail, "时区库来源：系统目录 "+strings.Join(ev.systemDirs, ", ")+
			"（二进制另内嵌了一份作离线兜底）")
	case ev.gorootZip != "":
		// 这一级是「本机装了 Go」才有的兜底，不是客户机上的常态。
		// 老实现直接报「二进制内嵌已兜住」——那是猜的：在打包机/开发机上命中的
		// 往往是这一级，报「内嵌」会让客户以为客户机也安全。
		c.detail = append(c.detail, "时区库来源：本机 Go 安装目录的 "+ev.gorootZip+"（或二进制内嵌的兜底 —— "+
			"两者都在，这里无法再细分）")
		c.detail = append(c.detail, "注意：上面这一级只在**装了 Go 的机器**上存在（打包机/开发机常见）。"+
			"客户机上通常没有 Go 安装目录，所以「本机 OK」不等于「客户机 OK」；"+
			"要判定交付是否安全，请在没有 Go、也没有 /usr/share/zoneinfo 的机器上复查本栏。")
	default:
		c.detail = append(c.detail, "时区库来源：**二进制内嵌**（本机没有 /usr/share/zoneinfo，"+
			"也没有 Go 安装目录里的 zoneinfo.zip —— 这正是离线机器上时间会静默错掉的场景，内嵌已兜住）")
	}
	// 没配 TZ 且当前就是 UTC：不算失败（这是合法配置），但必须让客户知道差 8 小时，
	// 否则他会在「日志时间不对」上再花一轮时间。
	if ev.tzEnv == "" && ev.utcOffset == "+00:00" {
		c.detail = append(c.detail, "提示：TZ 没配，当前按 UTC 走。国内看日志/页面时间会比北京时间早 8 小时；"+
			"要改就在实例 env 里加一行 `TZ=Asia/Shanghai` 并重启服务。")
	}
	return c
}

// gorootZoneinfoZip 返回本机 Go 安装目录里的 zoneinfo.zip（不存在则空串）。
//
// 为什么要查它：time.LoadLocation 的兜底顺序是
// ZONEINFO → 系统时区目录 → $GOROOT/lib/time/zoneinfo.zip → 二进制内嵌的 tzdata。
// 第三级只在「装了 Go 的机器」上存在 —— 也就是打包机和开发机，偏偏是最容易拿来验收的机器。
// 不把这一级单独报出来，就会得出「本机自检 OK = 交付安全」的错误结论。
func gorootZoneinfoZip() string {
	root := runtime.GOROOT()
	if root == "" {
		return ""
	}
	p := filepath.Join(root, "lib", "time", "zoneinfo.zip")
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		return p
	}
	return ""
}

// nonEmptyDirs 返回「存在且非空」的目录（只看一层，够用即可 ——
// 要证明的是「这里躺着一套时区文件」，不是清点内容）。
func nonEmptyDirs(dirs []string) []string {
	var out []string
	for _, d := range dirs {
		ents, err := os.ReadDir(d)
		if err != nil || len(ents) == 0 {
			continue
		}
		out = append(out, d)
	}
	return out
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
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

// parseEvidence 是一次「文档解析服务体检」的全部原始证据。
type parseEvidence struct {
	RawEnv   string // SKILLFORGE_OCR_URL 的原始值（用来区分「显式禁用」和「压根没配」）
	URL      string // 实际要探的地址（空 = 显式禁用）
	Loopback bool   // 探的是本机地址
	UnitName string // 解析服务单元名（默认 skillforge-ocr，可被 SKILLFORGE_SERVICE_NAME 改名）
	UnitPath string // 本机 systemd 单元文件路径（空 = 这台机器没有这个单元）
	Health   ocrsvc.Health
}

// checkParseService 采集解析服务证据：读实例配置 → 找 systemd 单元 → 探一次 /health。
//
// 为什么自检要管它：离线装机的沉默失败里它排第二 —— 主服务起得来、界面打得开，
// 但所有「读文档」的能力（上传写作手册训练技能、给技能传 PDF 抽正文）全废。
// 用户的感受是「生成的技能跟我给的素材没关系」，跟「解析服务坏了」隔着好几层，
// 倒查极贵（线上真发生过，最后是顺 8093 端口才挖出来的）。装完就把它证死最便宜。
func checkParseService() selfCheck {
	ev := parseEvidence{
		RawEnv:   strings.TrimSpace(os.Getenv(ocrsvc.EnvVarURL)),
		URL:      ocrsvc.URLFromEnv(),
		UnitName: ocrsvc.UnitName(),
	}
	if ev.URL != "" {
		ev.Loopback = ocrsvc.Loopback(ev.URL)
		ev.UnitPath = ocrUnitFile(ev.UnitName)
		ctlCtx, cancel := context.WithTimeout(context.Background(), ocrsvc.ProbeTimeout+2*time.Second)
		defer cancel()
		ev.Health = ocrsvc.Check(ctlCtx, ev.URL)
	}
	return judgeParseService(ev)
}

// judgeParseService 判定解析服务证据（纯函数，可用合成输入单测）。
//
// 五种结论，核心是**把「坏了」和「这台机器按设计没有」分开**：
//   - /health 自报 ok → 通过；
//   - 显式禁用（SKILLFORGE_OCR_URL=off，--no-ocr 安装就会写）→ 跳过；
//   - 指向本机、连不上、且本机根本没有这个 systemd 单元 → 跳过（老版本 --no-ocr 安装
//     没有写 off，只能靠这个特征识别）。判红会把预期行为误报成「安装失败」；
//   - 其余一律失败：单元在但端口不通、地址指向别的机器却连不上、端口在听但运行时已损坏。
//
// 「端口在听」不算健康：会漏掉僵尸服务（200 但 ok:false）和冒名服务（别的程序占着 8093），
// 线上就是这么误报过的 —— 判据必须是 /health 自报 ok（见 ocrsvc.Check 的注释）。
//
// 写法上刻意**不用 return c 提前返回**：底层原始错误由末尾统一挂上。早先每支自己 return，
// 结果「指向远端却连不上」那一支把原始错误吞了 —— 客户只看到「连不上」，分不清是 DNS 解析
// 不了、连接被拒还是超时，三种原因三种修法，还得回头来问我们。（单测 TestJudgeParseService
// 抓到的就是这个：结构上少一条通路，比写错一条 if 更难看见。）
func judgeParseService(ev parseEvidence) selfCheck {
	c := selfCheck{name: "文档解析服务"}

	switch {
	case ev.URL == "":
		c.skip = true
		c.detail = append(c.detail,
			fmt.Sprintf("配置里显式禁用（%s=%s）：这是 --no-ocr 安装的正常形态，不算失败。", ocrsvc.EnvVarURL, ev.RawEnv),
			"影响：扫描件 PDF / Word / Excel 抽不出正文，训练时这类素材会被跳过；需要时去掉 --no-ocr 重装。")

	case ev.Health.Healthy():
		c.ok = true
		ver := ev.Health.Version
		if ver == "" {
			ver = "未上报版本"
		}
		c.detail = append(c.detail,
			fmt.Sprintf("%s 正常，版本 %s", ocrsvc.Endpoint(ev.URL), ver))

	case ev.Health.Running:
		// 「端口在听」必须**先于**「本机没有 unit」判定 —— 顺序反了会出事：
		// 在没装 skillforge-ocr.service 的机器（容器 / 手工起进程 / CI）上，僵尸服务与
		// 冒名服务会先命中「没有 unit」那一支、被吞成「跳过」，而那支明细还写着「也连不上」
		// ——连探测结论都没看就下断言的假话。
		// 2026-09-16 CI 红抓到的就是这个：本地装了 unit 走的是这一支（判失败，绿），
		// CI 上被上一支吞掉（判跳过，红），**同一份代码两个结论**。本地绿是环境把 bug 遮住了。
		c.detail = append(c.detail,
			fmt.Sprintf("%s 端口在听，但服务自报运行时已损坏（版本 %s）", ocrsvc.Endpoint(ev.URL), ev.Health.Version),
			"端口在听 ≠ 能干活：这种状态下每次解析都会失败，用户看到的是「上传的素材抽不出文字」。",
			fmt.Sprintf("修复：`systemctl restart %s`（运行时守卫也会自己退出让 systemd 拉起），约 10 秒后重试。", ev.UnitName),
			fmt.Sprintf("排查：`journalctl -u %s -n 50`。", ev.UnitName))
		if ev.UnitPath == "" {
			// 按证据报「来源」：上两条 systemd 命令在这台机器上用不上，明说，别让客户敲了没用。
			c.detail = append(c.detail,
				fmt.Sprintf("注意：本机没有 %s.service，上两条 systemd 命令只适用于 systemd 安装；"+
					"容器 / 手工起进程的部署请重启解析服务进程本身。", ev.UnitName))
		}

	case !ev.Loopback:
		c.detail = append(c.detail,
			fmt.Sprintf("连不上远端解析服务 %s：地址写错了，或对端机器/服务挂了。", ocrsvc.Endpoint(ev.URL)),
			fmt.Sprintf("排查：在目标机 `curl -sS %s/health`；地址本身见 %s。", ocrsvc.Endpoint(ev.URL), ocrsvc.EnvVarURL))

	case ev.UnitPath == "":
		c.skip = true
		c.detail = append(c.detail,
			fmt.Sprintf("本机没有解析服务单元（%s.service 不存在），%s 也连不上。",
				ev.UnitName, ocrsvc.Endpoint(ev.URL)),
			"这与 --no-ocr 安装（或离线包里缺 bin/ocrd）的形态一致，因此不算失败。",
			"影响：扫描件 PDF / Word / Excel 抽不出正文；需要时重装并启用解析服务（去掉 --no-ocr）。",
			fmt.Sprintf("确认：`systemctl status %s`（提示 unit not found = 确实没装）。", ev.UnitName))

	case ev.Health.Running:
		c.detail = append(c.detail,
			fmt.Sprintf("%s 端口在听，但服务自报运行时已损坏（版本 %s）", ocrsvc.Endpoint(ev.URL), ev.Health.Version),
			"端口在听 ≠ 能干活：这种状态下每次解析都会失败，用户看到的是「上传的素材抽不出文字」。",
			fmt.Sprintf("修复：`systemctl restart %s`（运行时守卫也会自己退出让 systemd 拉起），约 10 秒后重试。", ev.UnitName),
			fmt.Sprintf("排查：`journalctl -u %s -n 50`。", ev.UnitName))

	default:
		c.detail = append(c.detail,
			fmt.Sprintf("连不上 %s，但本机有 %s（%s）：这台机器本该有解析服务，说明它没在跑。",
				ocrsvc.Endpoint(ev.URL), ev.UnitName, ev.UnitPath),
			fmt.Sprintf("修复：`systemctl restart %s`。", ev.UnitName),
			fmt.Sprintf("排查：`systemctl status %s` / `journalctl -u %s -n 50`。", ev.UnitName, ev.UnitName))
		// 起不来 ≠ 重启就能好。把 journal 里的死因归类后再说话：
		// 2026-09-17 客户现场（CentOS 7 系）服务反复重启，客户能看到的只有
		//   Failed to load Python shared library '…/_MEI…/libpython3.11.so.1.0':
		//   version `GLIBC_2.28' not found
		// 而真因是「目标机 glibc 比产物的构建基线老」—— 文件在不在根本不重要。
		// 归因与修法在 internal/ocrsvc 里（同一份逻辑给 -diag / doctor 用，不重复实现）。
		if d := ocrsvc.DiagnoseUnitStartup(ev.UnitName, ocrBinaryFromUnit(ev.UnitPath)); d.Class != "unknown" {
			c.detail = append(c.detail, "归因："+d.Summary)
			for _, f := range d.Fixes {
				c.detail = append(c.detail, "· "+f)
			}
			if d.Evidence != "" {
				c.detail = append(c.detail, "原始日志："+ocrsvc.FirstLine(d.Evidence))
			}
		} else if d.Evidence != "" {
			c.detail = append(c.detail, "归因：没认出这一类失败，journal 最后一行原文："+ocrsvc.FirstLine(d.Evidence))
		}
		if d := doctorHint(); d != "" {
			c.detail = append(c.detail, fmt.Sprintf("一条命令分诊：`%s`。", d))
		}
	}

	// 底层原始错误统一挂在这里（只留一行）。跳过项不挂：那种形态下「连不上」是预期的，
	// 挂出来只会让人以为出了事。
	if ev.Health.Err != nil && !c.skip {
		c.detail = append(c.detail, "原始错误："+ocrsvc.FirstLine(ev.Health.Err.Error()))
	}
	return c
}

// ocrUnitFile 找解析服务的 systemd 单元文件（空串 = 本机没有这个单元）。
//
// 为什么看文件而不问 `systemctl show`：一是自检要能在 systemd 不可用（容器里调 -selftest）
// 时也给结论；二是「单元文件不存在」正是 --no-ocr 安装的确定特征，比解析 systemctl 的
// 人话输出可靠。目录列表覆盖发行版默认位与 /usr/local 位（离线包用不到后者，但别漏）。
func ocrUnitFile(name string) string {
	if name == "" {
		return ""
	}
	for _, dir := range []string{
		"/etc/systemd/system",
		"/run/systemd/system",
		"/usr/local/lib/systemd/system",
		"/usr/lib/systemd/system",
		"/lib/systemd/system",
	} {
		p := filepath.Join(dir, name+".service")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// doctorHint 给出「能直接敲」的解析服务体检命令。
// sf-ocr-doctor.sh 只随离线包安装，目录被挪过就当没有 —— 指一个不存在的路径
// 比不给建议更糟（用户会以为是自己把文件弄丢了）。
func doctorHint() string {
	var cands []string
	if exe, err := os.Executable(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(exe), "scripts", "sf-ocr-doctor.sh"))
	}
	cands = append(cands, "/opt/skillforge/scripts/sf-ocr-doctor.sh")
	for _, p := range cands {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// instanceEnvKeys 是自检要从实例 env 文件「补齐」的配置键 —— 与主服务共用的那部分。
//
// 刻意用白名单，而不是把整个文件读进来：env 文件里还躺着 LLM API key、JWT secret、
// 管理员口令。自检没有理由把它们塞进自己的进程环境，多一处出现就多一处泄漏面。
// 只加「会影响自检判定」的键。
var instanceEnvKeys = []string{
	ocrsvc.EnvVarURL,
	"SKILLFORGE_OCR_TIMEOUT",
	"SKILLFORGE_SERVICE_NAME",
	"SKILLFORGE_ADDR",
	"SKILLFORGE_DATA_DIR",
	"SKILLFORGE_DB",
	"SKILLFORGE_PDF_FONT_FILE",
	"SKILLFORGE_PUBLIC_URL",
	"SKILLFORGE_PYTHON",
	// TZ 会影响「现在几点」这项自检证据，且 install.sh 会把它写进实例 env，
	// 所以必须补进来，否则自检报的时间和 systemd 里跑的服务不是同一个时区。
	"TZ",
}

// loadInstanceEnv 把实例 env 文件里的配置补进本进程环境，返回实际读到的文件路径（空 = 没找到）。
//
// 已存在的环境变量优先：运维临时 `SKILLFORGE_OCR_URL=... skillforge -selftest` 排查时，
// 他的显式覆盖不能被文件里的旧值顶掉。返回路径仅用于打印「配置来源」，任何值都不回显。
func loadInstanceEnv() string {
	var cands []string
	if p := strings.TrimSpace(os.Getenv("SKILLFORGE_ENV_FILE")); p != "" {
		cands = append(cands, p)
	}
	cands = append(cands, candidateEnvFiles()...)

	for _, p := range cands {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, ok := parseEnvLine(sc.Text())
			if !ok || !inStringList(k, instanceEnvKeys) {
				continue
			}
			if _, exists := os.LookupEnv(k); exists {
				continue // 进程环境优先
			}
			os.Setenv(k, v)
		}
		f.Close()
		return p
	}
	return ""
}

// parseEnvLine 解析 env 文件里的一行：KEY=VALUE / export KEY=VALUE / # 注释 / 空行。
// 只做自检够用的那点 shell 语义：去引号，**不做**变量替换。
// 自检要的是「路径字符串原样是什么」，猜错了给出的结论就是错的。
func parseEnvLine(line string) (key, val string, ok bool) {
	s := strings.TrimSpace(line)
	if s == "" || strings.HasPrefix(s, "#") {
		return "", "", false
	}
	s = strings.TrimPrefix(s, "export ")
	i := strings.IndexByte(s, '=')
	if i <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(s[:i])
	val = strings.TrimSpace(s[i+1:])
	if len(val) >= 2 {
		if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		}
	}
	if key == "" {
		return "", "", false
	}
	return key, val, true
}

// inStringList 判断 key 是否在白名单里（键名很少，线性扫足够；不引依赖）。
func inStringList(key string, list []string) bool {
	for _, e := range list {
		if e == key {
			return true
		}
	}
	return false
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
// checkTrustStore 采集本机「TLS 信任库」证据。
//
// 为什么值得单独一项：离线内网里最别扭的一类故障是「https 请求全失败，
// 但没人知道是机器没装 ca-certificates、还是内网证书是自签的没配 CA」。
// 这两种原因的修法完全不同（装包 vs 配 SKILLFORGE_CA_BUNDLE），而界面上的
// 错误信息一模一样。自检直接把机器的信任现状摊开，让客户一眼看到自己属于哪种。
func checkTrustStore() selfCheck {
	return judgeTrustStore(trustEvidence{
		st:             tlsconf.Load(),
		httpsEndpoints: httpsEndpointsInEnv(),
	})
}

// trustEvidence 是判定信任库要用到的全部证据。
type trustEvidence struct {
	st tlsconf.Status
	// httpsEndpoints 是本实例配置里真正会用 https 的端点（已脱敏成 "KEY 主机"）。
	//
	// 为什么必须带上它：一台纯离线机器可能所有端点都是 http://内网IP，
	// 此时「没有系统根证书」不影响任何功能。不看端点就判失败，等于让客户
	// 去修一个跟他无关的问题 —— 自检的可信度就是这么被消耗掉的。
	httpsEndpoints []string
}

// httpsEndpointsInEnv 找出实例配置里以 https:// 开头的端点。
// 只回显「变量名 + 主机」，不带路径与查询串：URL 里可能夹着 token。
func httpsEndpointsInEnv() []string {
	watched := []string{
		"SKILLFORGE_LLM_BASE_URL",
		ocrsvc.EnvVarURL,
		tlsconf.EnvVarProbe,
	}
	var out []string
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !inStringList(k, watched) {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "https://") {
			continue
		}
		host := v
		if u, err := url.Parse(v); err == nil && u.Host != "" {
			host = u.Host
		}
		out = append(out, k+"（"+host+"）")
	}
	sort.Strings(out)
	return out
}

// judgeTrustStore 判定信任库证据。纯函数：给定证据，结论必须确定。
func judgeTrustStore(ev trustEvidence) selfCheck {
	st := ev.st
	c := selfCheck{name: "TLS 信任库"}

	// 1) 自定义 CA：配了就一定要装进去。配了不生效比不配更坏 ——
	//    客户会认为「我按文档做了还是不行」，然后去怀疑程序本身。
	if st.Configured() && st.BundleCerts == 0 {
		c.detail = append(c.detail, tlsconf.EnvVarCA+" 配了，但一份证书都没加载进来")
		if st.Err != nil {
			c.detail = append(c.detail, st.Err.Error())
		}
		c.detail = append(c.detail,
			"修复：确认给的是**签发服务端证书的 CA** 证书（PEM 或 DER 均可），不是服务端自己的证书")
		return c
	}

	// 2) 系统根证书库：读不到/为空 → 所有 https 都过不了校验。
	//    这里必须报失败，因为客户接下来必然撞上它，而错误信息会指引他去折腾 CA。
	if !st.SystemOK && st.BundleCerts == 0 {
		if len(ev.httpsEndpoints) == 0 {
			// 全部端点都是 http（纯内网 http 部署的常态）：现在不受影响。
			// 报失败会让客户去修一个不影响他的东西，这里如实标成「暂不影响」。
			c.ok = true
			c.detail = append(c.detail,
				"系统根证书库不可用，但本实例配置的端点都是 http，当前不受影响",
				"提醒：将来把模型服务/接口地址改成 https 之前，必须先解决这一项（见下）")
		} else {
			c.detail = append(c.detail,
				"系统根证书库不可用，且没有配置自定义 CA —— 下列 https 端点会证书校验失败：")
			for _, e := range ev.httpsEndpoints {
				c.detail = append(c.detail, "  · "+e)
			}
		}
		if !c.ok {
			if st.SystemNote != "" {
				c.detail = append(c.detail, st.SystemNote)
			}
			c.detail = append(c.detail,
				"修复（离线机）：从离线包的 packages/ 里装 ca-certificates（rpm/deb），",
				"          或把内网 CA 证书文件路径写进 "+tlsconf.EnvVarCA+"（多个用冒号分隔）")
			return c
		}
	}

	// 3) 有信任来源：通过。把来源说清楚，客户排障时不必再猜。
	if !c.ok {
		c.ok = true
	}
	if st.BundleCerts > 0 {
		c.detail = append(c.detail,
			fmt.Sprintf("自定义 CA 已加载 %d 份证书（%s）", st.BundleCerts, st.BundlePath))
	} else if st.SystemOK {
		c.detail = append(c.detail, fmt.Sprintf("系统根证书库可用（%d 份）", st.SystemCerts))
	}
	if len(ev.httpsEndpoints) > 0 {
		c.detail = append(c.detail, "本实例的 https 端点："+strings.Join(ev.httpsEndpoints, "、"))
	}

	if st.Insecure {
		// 不是失败：客户可能是刻意跳过校验。但绝不能静默 —— 他需要知道
		// 这等于对所有 https 关掉了防篡改，只能临时用。
		c.detail = append(c.detail,
			"⚠️ "+tlsconf.EnvVarInsecure+" 已开启：https 证书校验被**跳过**，任何人都能冒充内网服务",
			"   仅调试用；生产请去掉这一项，改用 "+tlsconf.EnvVarCA+" 配置真正的 CA")
	}
	if st.SystemNote != "" {
		c.detail = append(c.detail, st.SystemNote)
	}
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
	// 先问「这台机器给不给得了沙箱」，再谈探针结果。
	// 顺序不能反：systemd 太老时 systemd-run 直接拒收参数退出，探针一个字都不吐，
	// 上层只能报「没有 uid」—— 而客户拿到的那句 `unrecognized option '--pipe'`
	// 既看不出原因，也看不出「这台机器永远做不到」。见 tools.ProbeSandboxEnv。
	if env := tools.ProbeSandboxEnv(); !env.Usable {
		return judgeSandboxEnv(env)
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

// judgeSandboxEnv 渲染「本机环境给不了沙箱」这一结论。
//
// 为什么单独一个函数：这条路径跟「探针发现危险」是完全不同的两件事，混在一起写就会
// 让客户以为「沙箱坏了、要去修沙箱」。这里要说的恰恰相反：机器没坏、程序没坏，
// 是这份能力在这台机器上不可能成立，且我们**故意**不降级执行代码。
func judgeSandboxEnv(env tools.SandboxEnvVerdict) selfCheck {
	c := selfCheck{name: "代码执行沙箱", unavail: true}
	ver := env.Version
	if ver == "" {
		ver = "读不出来"
	}
	c.detail = append(c.detail,
		fmt.Sprintf("本机 systemd：%s（沙箱需要 ≥%d）。", ver, tools.MinSystemdVersion),
		"缺的能力："+strings.Join(env.Missing, "；")+"。",
		"为什么不能让一步：降权（User=nobody）、断网（PrivateNetwork=yes）、写不进敏感路径"+
			"（ProtectSystem=strict + ReadWritePaths）这三条必须同时成立才算沙箱 —— "+
			"前两条是 systemd 232 才有的能力，删掉任何一条就不是沙箱了，所以**不会降级执行代码**。",
		"影响：只有 AI「执行代码」这一项不可用；写作、文档生成、素材解析（PDF/Word/Excel 抽文本）、技能训练都不受影响。",
		"修法：")
	for _, f := range tools.SandboxEnvFixes() {
		c.detail = append(c.detail, "· "+f)
	}
	c.detail = append(c.detail, "自查：`systemd-run --version`、`systemd-run --help | grep -E -- '--(pipe|wait)'`。")
	return c
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
		// 这一支现在几乎不可达——探针跑完却没有 uid 的两种情况都已被上游点名：
		// ①解释器不存在：SandboxDiagnosticsFor 在跑探针之前就报（带 dnf/apt 提示）；
		// ②systemd-run 拒收参数：probeFailureDetail 把原始报错塞进 error，走上面那支。
		// 留着它当最后一道闸：**没有 uid 证据就绝不能判通过**，也不许替客户编一个原因。
		//
		// 历史教训（Bug O 的误诊版）：这里原来一口咬定「最常见原因是目标机缺 python3」。
		// 真现场是 AlmaLinux 8（systemd 239）不认 systemd-run 的 --working-directory
		// ——python3 好好地在包里躺着。客户照着那句话装了几遍 python3，怎么装都修不好。
		c.detail = append(c.detail, "沙箱探针没有报告 uid —— 这是「证据缺失」，不是「降权失败」，"+
			"也不能据此断定原因。请先看探针的原始回执（systemd-run 的 stdout/stderr）："+
			"已知过的两类真因是 ①目标机没有可用的 python3 解释器（RHEL/AlmaLinux 最小安装默认不带）；"+
			"②目标机 systemd 版本较老、不认我们下发的某个 systemd-run 选项"+
			"（例：AlmaLinux 8 的 systemd 239 不认 --working-directory，只认属性 --property=WorkingDirectory=）。"+
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

// ocrBinaryFromUnit 从单元文件里取 ExecStart 的可执行文件路径；取不到返回空串。
//
// 用途：体检/自检要「真跑一次产物」时必须知道它在哪。取不到不致命 ——
// 归因层会退化成只读 journal（见 ocrsvc.DiagnoseUnitStartup）。
// 只认绝对路径且跳过 systemd 的前缀修饰符（-, @, +, !），否则会拿到一个奇怪的名字。
func ocrBinaryFromUnit(unitPath string) string {
	if unitPath == "" {
		return ""
	}
	b, err := os.ReadFile(unitPath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		for _, f := range strings.Fields(strings.TrimPrefix(line, "ExecStart=")) {
			f = strings.TrimLeft(f, "-@+!")
			if strings.HasPrefix(f, "/") {
				return f
			}
		}
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
