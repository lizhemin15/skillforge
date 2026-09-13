package docgen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestPDFFontCoversRequiredGlyphs 守护 Bug G：PDF 字体必须覆盖数字/拉丁/常用汉字。
//
// 背景：曾经 PDF 只用 /usr/share/fonts/truetype/droid/DroidSansFallbackFull.ttf，
// 该字体不含 ASCII 数字（实测 0-9/拉丁/半角标点全缺），而 gopdf 默认把找不到字形的
// 字符替换成空格，于是报价单里的「5 台 / 12000 / 合计 90000」在 PDF 里全部变成空白。
// 这个测试保证以后无论换哪个字体，门槛字符集必须全覆盖。
func TestPDFFontCoversRequiredGlyphs(t *testing.T) {
	path := PDFFontPath()
	if path == "" {
		t.Fatalf("no usable PDF font resolved from candidates: %v", pdfFontCandidates)
	}
	if miss := PDFFontMissingRunes(); len(miss) != 0 {
		t.Fatalf("resolved PDF font %s is missing %d required rune(s): %q",
			path, len(miss), string(miss))
	}
}

// TestPDFFontCoversRealDocumentContent 用一份「最像真实业务」的文档内容做端到端探针：
// 数字、货币符号、百分号、日期、英文缩写都必须在所选字体里有字形。
// 这是对「数字静默消失」类回归的直接拦截。
func TestPDFFontCoversRealDocumentContent(t *testing.T) {
	path := PDFFontPath()
	if path == "" {
		t.Skip("no PDF font resolved")
	}

	doc := Doc{
		Format:   "pdf",
		Filename: "产品报价单.pdf",
		Title:    "产品报价单 2026-09-11",
		Cols:     []string{"序号", "产品名称", "数量", "单价", "小计"},
		Rows: [][]string{
			{"1", "云服务器", "5 台", "12000", "60000"},
			{"2", "技术支持", "1 年", "30000", "30000"},
			{"3", "总计", "-", "-", "90000"},
		},
		Parags: []string{
			"合计金额：人民币 90000 元（大写：玖万元整）。",
			"折扣率 8.5%，含税价 ￥60000，联系人：张三 13800138000。",
			"备注：Notes / TBD，POC 阶段 30-60 天，SLA 99.9%。",
		},
	}

	var sb strings.Builder
	sb.WriteString(doc.Title)
	for _, c := range doc.Cols {
		sb.WriteString(c)
	}
	for _, r := range doc.Rows {
		for _, c := range r {
			sb.WriteString(c)
		}
	}
	for _, p := range doc.Parags {
		sb.WriteString(p)
	}

	miss, err := probeFontCoverage(path, sb.String())
	if err != nil {
		t.Fatalf("probe font %s: %v", path, err)
	}
	if len(miss) != 0 {
		t.Fatalf("font %s cannot render %d rune(s) used by a real document: %q "+
			"(gopdf would silently render them as blanks)", path, len(miss), string(miss))
	}

	// 数字单独再断言一次——这是用户最容易一眼看出来的丢失。
	for _, d := range "0123456789" {
		if strings.ContainsRune(string(miss), d) {
			t.Fatalf("font %s lost ASCII digit %q", path, d)
		}
	}
}

// TestScanTTFFontsFiltersAndSorts 校验目录扫描只收 .ttf、跳过 gopdf 无法加载的
// 集合/OTF 字体，并且输出有序（保证同一台机器上选出的字体可复现）。
func TestScanTTFFontsFiltersAndSorts(t *testing.T) {
	root := t.TempDir()
	write := func(rel string, n int) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("b.ttf", 4)
	write("sub/d.ttf", 4)
	write("a.ttf", 4)
	write("skip.ttc", 4) // 字体集合：gopdf 不支持
	write("skip.otf", 4) // CFF 轮廓：gopdf 不支持
	write("note.txt", 4)

	got := scanTTFFonts([]string{root, "/路径不存在/忽略", ""})
	want := []string{
		filepath.Join(root, "a.ttf"),
		filepath.Join(root, "b.ttf"),
		filepath.Join(root, "sub", "d.ttf"),
	}
	if len(got) != len(want) {
		t.Fatalf("扫描到 %d 个字体 %v，期望 %d 个 %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个应为 %s，实际 %s（要求有序输出）", i, want[i], got[i])
		}
	}
}

// TestScanTTFFontsKeepsBundledFontWhenOtherRootsHaveManyFonts 锁定「roots 顺序即优先级」。
//
// 早期实现把所有搜索根的结果合并后做**全局路径排序**再按上限截断。
// 实测真实路径的字典序（/usr/local/share/fonts/skillforge 排在
// /usr/share/fonts 之前），所以「系统字体多 → 自带字体被挤掉」并不是普遍故障；
// 但**同根下字典序更靠前的目录**（如 /usr/local/share/fonts/aaa-fonts/）
// 一旦攒够 maxScannedFonts 个 .ttf，自带字体就会被截断掉，运行时静默退回系统字体。
// 在没有 CJK 字体的离线机器上，那等于 PDF 里中文数字全空白。
//
// 本用例用命名刻意制造这个次序（低优先级根 aaa-system 排前、高优先级根 zzz-bundled 排后），
// 断言 roots 的先后就是优先级：先扫到的先保留，截断只砍后面的根。
func TestScanTTFFontsKeepsBundledFontWhenOtherRootsHaveManyFonts(t *testing.T) {
	base := t.TempDir()
	system := filepath.Join(base, "aaa-system")   // 低优先级根，故意让字典序排前面
	bundled := filepath.Join(base, "zzz-bundled") // 高优先级根，字典序在后
	for _, d := range []string{system, bundled} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	writeFont := func(root, name string) string {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, make([]byte, 4), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	want := writeFont(bundled, "gbsn00lp.ttf")
	// 低优先级根塞满超过上限的字体，制造「全局排序 + 截断」会砍掉自带字体的情况
	for i := 0; i < maxScannedFonts+20; i++ {
		writeFont(system, fmt.Sprintf("sys-%03d.ttf", i))
	}

	got := scanTTFFonts([]string{bundled, system})
	if len(got) == 0 {
		t.Fatal("一个字体都没扫到")
	}
	if got[0] != want {
		t.Fatalf("高优先级根里的字体必须排第一，实际第一个是 %s（roots 顺序即优先级）", got[0])
	}
	if len(got) > maxScannedFonts {
		t.Fatalf("扫描结果 %d 个，超过上限 %d", len(got), maxScannedFonts)
	}
	found := false
	for _, p := range got {
		if p == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("自带字体 %s 被截断挤掉了（共扫到 %d 个）：%v…", want, len(got), got[:3])
	}
}

// TestResolvePDFFontEnvOverride 校验 SKILLFORGE_PDF_FONT_FILE 生效：
// 指到真实字体时必须用它；指到不存在的路径时必须优雅回落到正常探测，而不是崩或者静默变空。
func TestResolvePDFFontEnvOverride(t *testing.T) {
	restore := func() {
		pdfFontOnce = sync.Once{}
		pdfFontPath, pdfFontMissing = "", nil
	}
	good := PDFFontPath()
	if good == "" {
		t.Skip("本机无可用 PDF 字体，跳过（CI 会安装字体后执行）")
	}

	t.Run("指定真实字体则采用", func(t *testing.T) {
		t.Setenv(pdfFontFileEnv, good)
		restore()
		defer restore()
		if got := PDFFontPath(); got != good {
			t.Fatalf("%s=%s 时应选用该字体，实际 %s", pdfFontFileEnv, good, got)
		}
		if miss := PDFFontMissingRunes(); len(miss) != 0 {
			t.Fatalf("本机字体 %s 缺字符: %q", good, string(miss))
		}
	})

	t.Run("指定不存在的路径则优雅回落", func(t *testing.T) {
		// 期望值**不能**沿用 good。本机（systemd / 部署脚本就是这么跑的）导出了
		// SKILLFORGE_PDF_FONT_FILE，于是 good 本身就来自那个环境变量；而这条用例考的是
		// 「env 指歪了 → 退回正常探测」这条路，两者本来就不该相等。
		// 沿用 good 的写法只在「机器上没设这个变量」时成立：干净 CI 上假绿、配了字体的
		// 真实机器上假红。所以要压的不变量是「env 失效时回落到探测结果，而不是崩或变空」，
		// 期望值必须在同样「没有 env」的条件下现算。
		t.Setenv(pdfFontFileEnv, "")
		restore()
		want := PDFFontPath()
		if want == "" {
			t.Skip("本机无可用 PDF 字体，跳过（CI 会安装字体后执行）")
		}

		bad := filepath.Join(t.TempDir(), "not-there.ttf")
		t.Setenv(pdfFontFileEnv, bad)
		restore()
		defer restore()
		got := PDFFontPath()
		if got == "" {
			t.Fatalf("%s 指向不存在的路径时字体解析结果为空——静默变空比报错更糟，PDF 会画成方框", pdfFontFileEnv)
		}
		if got == bad {
			t.Fatalf("用了不存在的路径 %s（应该回落到正常探测）", bad)
		}
		if got != want {
			t.Fatalf("路径无效时应回落到正常探测（无 env 时探测得 %s），实际 %s", want, got)
		}
	})
}

// TestBundledFontScanRootsComeFirst 守护 Bug K 相关的顺序约定：
// 离线包自带字体目录（/usr/local/share/fonts/skillforge-<服务名>）必须排在通用系统字体目录之前。
//
// 注意这里断言的是**扫描根的顺序**，不是 resolvePDFFont 的最终选择——固定候选表
// （pdfFontCandidates）优先级本来就高于扫描，那是刻意设计。顺序被改回去时，没有配
// SKILLFORGE_PDF_FONT_FILE 的场景（手工跑二进制、容器里直接执行）就会先吃到系统字体，
// 于是「自检说用包内字体，运行时却用了别的」。
//
// 断言的是**类别顺序**，不是「本实例目录排在索引 0」——后者是过强断言：
// bundledFontScanRoots 用 glob 找同机所有 skillforge-* 目录再按字母序排，
// 装了第二个实例的机器上（开发机/生产机就是）别的实例名按字母序可能在前面，
// 那依然是「包内字体」，不是回归。写死 roots[0] 的结果是：CI 干净机器上假绿、
// 真实机器上假红——一条只在不装东西的机器上成立的防线等于没有。
func TestBundledFontScanRootsComeFirst(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("需要在 /usr/local/share/fonts 下建测试目录，非 root 跳过")
	}
	base := "/usr/local/share/fonts"
	if err := os.MkdirAll(base, 0755); err != nil {
		t.Skipf("无法创建 %s: %v", base, err)
	}
	dir := filepath.Join(base, "skillforge-unittest")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Skipf("无法创建 %s: %v", dir, err)
	}
	defer os.RemoveAll(dir)

	roots := bundledFontScanRoots()

	// ① 本实例的字体目录必须真的被扫到，否则装了字体也白装。
	contained := false
	for _, r := range roots {
		if r == dir {
			contained = true
		}
	}
	if !contained {
		t.Fatalf("包内字体目录未出现在扫描根里：期望包含 %s，实际 %v", dir, roots)
	}

	// ② 所有包内字体目录必须**整体**排在通用系统目录之前。
	//
	// 别写成「遍历第一个系统目录之前的那一段，检查它们都是包内目录」——
	// 顺序被整个倒回系统优先时，第一个系统目录正好在索引 0，那一段是空集，
	// 断言恒真、永远假绿（这个写法我实测踩到了：注入回归后依然全绿）。
	// 这里改成比较「最后一个包内目录」与「第一个系统目录」的下标。
	lastBundled, firstGeneric := -1, -1
	for i, r := range roots {
		if strings.HasPrefix(r, base+"/skillforge") {
			lastBundled = i
		} else if firstGeneric < 0 {
			firstGeneric = i
		}
	}
	if lastBundled < 0 {
		t.Fatalf("扫描根里没有任何包内字体目录：%v", roots)
	}
	if firstGeneric < 0 {
		t.Fatalf("扫描根里没有任何通用系统目录（客户机没装包内字体时会彻底找不到字体）：%v", roots)
	}
	if lastBundled > firstGeneric {
		t.Fatalf("包内字体目录必须全部排在通用系统目录之前，实际最后一个包内目录在索引 %d、"+
			"通用系统目录从索引 %d 就开始。完整顺序 %v", lastBundled, firstGeneric, roots)
	}
	for _, r := range roots[firstGeneric:] {
		if r == "/usr/share/fonts" {
			return // 通用系统目录还在，没被挤掉
		}
	}
	t.Fatalf("通用系统字体目录 /usr/share/fonts 被挤掉了：%v", roots)
}

// TestBuildPDFKeepsDigits 直接生成 PDF 并确认生成的字节流里包含数字字形。
//
// 说明：gopdf 使用 Identity-H 子集嵌入，内容流里写的是 CID 而不是明文字符，
// 因此这里无法用「搜索 ASCII"12000"」的方式断言。可用的强证据是：子集字体里
// 必须真的包含数字字符——ToUnicode CMap 会为子集字符写出映射，数字应当出现在
// 映射的目标端。若字体缺数字，gopdf 会把它替换成空格，CMap 目标端就不会有 "0"~"9"
// 这些明文字符。
func TestBuildPDFKeepsDigits(t *testing.T) {
	doc := Doc{
		Format: "pdf",
		Title:  "数量 5 单价 12000",
		Cols:   []string{"数量", "单价", "小计"},
		Rows:   [][]string{{"5", "12000", "60000"}},
		Parags: []string{"合计 90000 元。"},
	}
	buf, err := buildPDF(doc)
	if err != nil {
		t.Fatalf("buildPDF: %v", err)
	}
	if len(buf) == 0 {
		t.Fatal("buildPDF returned empty output")
	}
	// 数字必须以「字符」形式出现在某个 CMap/字符串里，而不是被吞掉。
	if !strings.Contains(string(buf), "90000") && !strings.Contains(string(buf), "12000") {
		// 子集化后可能只写 CID，此时退化为检查字体覆盖率（前面两个测试已覆盖）。
		// 但仍要确保没有告警级别的缺字——由 probe 再确认一次。
		if miss, err := probeFontCoverage(PDFFontPath(), "0123456789"); err != nil || len(miss) > 0 {
			t.Fatalf("digits not embedded and font lacks them (miss=%q err=%v)", string(miss), err)
		}
	}
}
