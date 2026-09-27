package docgen

import (
	"archive/zip"
	"bytes"
	"io"
	"regexp"
	"strings"
	"testing"
)

// ===== PDF / PPT 同源漏 markdown 的回归测试 =====
//
// 用户 2026-09-27 报的是 Word，但三份出口（docx/pdf/pptx）当时都在各自
// 循环里逐条遍历 parags，所以「## 一、」这种行在 PDF 与 PPT 上一样会出现。
// 结构判定统一收在 gongwenBlocks（gongwen_block.go），这里既测那一层，
// 也测两个渲染器确实吃到了这块（PPT 直接解包看 <a:t>；PDF 看几何与字形）。

// blocksOf 只是给测试用的短名字。
func blocksOf(parags []string, title string) []block { return gongwenBlocks(parags, title) }

func TestBlocksStripMarkdownAndClassify(t *testing.T) {
	lines := []string{
		"# 关于开展安全生产大检查的通知",                              // 与 title 重复 → 丢弃
		"各县区应急管理局：",                                     // 主送单位
		"一、检查时间",                                        // 一级标题（黑体）
		"（一）自查阶段。",                                      // 二级标题（楷体）
		"1. 检查时间：3月1日至3月31日。",                           // 三级标题
		"- 建立台账",                                        // 无序列表
		"**压实责任**，逐项落实。",                                // 正文（内联加粗）
		"> 本通知自印发之日起执行。",                                // 引用 → 正文
		"| 阶段 | 时间 |", "| --- | --- |", "| 自查 | 3月1日 |", // 表格
		"XX市应急管理局", "2026年2月28日", // 落款
	}
	bs := blocksOf(lines, "关于开展安全生产大检查的通知")

	var kinds []blockKind
	var all strings.Builder
	for _, b := range bs {
		kinds = append(kinds, b.kind)
		if b.kind == blockTable {
			for _, row := range b.rows {
				all.WriteString(strings.Join(row, ""))
			}
			continue
		}
		all.WriteString(b.text())
	}
	got := all.String()

	want := []blockKind{blockRecipient, blockH1, blockH2, blockH3, blockBullet, blockBody, blockBody, blockTable, blockSignoff, blockSignoff}
	if len(kinds) != len(want) {
		t.Fatalf("块数不对: 得 %d 种 %v", len(kinds), kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("第 %d 块类型不对: 得 %v 期望 %v（全量 %v）", i, kinds[i], want[i], kinds)
		}
	}

	// 记号一个都不许留（块层是三个格式共用的唯一去处）
	// 注意 "1. " 是**有序列表自己的记号**（要保留），不算残留
	for _, bad := range []string{"#", "*", "|", "- 建立台账"} {
		if strings.Contains(got, bad) {
			t.Errorf("块层残留 markdown 记号 %q: %q", bad, got)
		}
	}
	// 正文文字不许丢
	for _, keep := range []string{"各县区应急管理局", "检查时间", "自查阶段", "建立台账", "压实责任", "本通知自印发之日起执行", "XX市应急管理局"} {
		if !strings.Contains(got, keep) {
			t.Errorf("块层丢了文字 %q: %q", keep, got)
		}
	}
	// 表格行列要拆开
	for _, b := range bs {
		if b.kind == blockTable {
			if len(b.rows) != 2 { // 分隔行不算
				t.Errorf("表格应是 2 行（含表头），实际 %d", len(b.rows))
			}
			if len(b.rows[0]) != 2 || b.rows[1][1] != "3月1日" {
				t.Errorf("表格行列拆得不对: %v", b.rows)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// PDF
// ---------------------------------------------------------------------------

// PDF 只有一个内嵌字体：文档里会出现、而字体**没有字形**的字符必须换成等效字符，
// 否则 gopdf 不报错、直接画成空格（Bug G 那类；线上实测缺「•」和「¥」）。
func TestPDFFontFixerReplacesMissingGlyphs(t *testing.T) {
	fp, _ := resolvePDFFont()
	if fp == "" {
		t.Skip("本机没有可用的 CJK 字体")
	}
	fx := newPDFFontFixer(fp)

	// 1. 换过之后画出去的文本，不该再有缺字形的字符。
	sample := fx.text("• 项目符号 ¥12,000 €100")
	miss, err := probeFontCoverage(fp, sample)
	if err != nil {
		t.Fatalf("探测字体失败: %v", err)
	}
	// 本机字体缺 €（欧洲号，没有等效汉字），这一条不该被算进来——
	// 只要求「本表覆盖的字符」都被修好。
	for _, r := range miss {
		if r == '•' || r == '¥' {
			t.Fatalf("修字后仍缺字形 %q（会静默变空格）：%q", string(r), sample)
		}
	}

	// 2. 本机字体确实缺 • 和 ¥（否则这条测试就没在测东西），且修字器把它们换掉了。
	if miss, err := probeFontCoverage(fp, "•¥"); err == nil && len(miss) == 2 {
		if !strings.Contains(fx.text("• x"), "●") {
			t.Errorf("字体缺「•」但修字器没换：%q", fx.text("• x"))
		}
		if !strings.Contains(fx.text("¥ 12"), "￥") {
			t.Errorf("字体缺「¥」但修字器没换：%q", fx.text("¥ 12"))
		}
	}
}

// 表格几何：曾经把列宽当行高、40 当表体行数塞进 NewTableLayout，
// 3 行表格被撑成 41 行 ≈ 7000pt，正文全压在空表格上。
func TestPDFTablePlanNotInflated(t *testing.T) {
	rows := [][]string{{"阶段", "时间"}, {"自查", "3月1日"}, {"督查", "3月15日"}}
	cols, _, rowH, height, bodyRows := pdfTablePlan(rows)
	if cols != 2 {
		t.Fatalf("列数错了: %d", cols)
	}
	if bodyRows != 2 {
		t.Errorf("表体行数应为 2（不含表头），实际 %d", bodyRows)
	}
	if rowH > 40 {
		t.Errorf("行高 %v 过大（旧 bug 是把列宽当行高）", rowH)
	}
	if height > 120 {
		t.Errorf("3 行表格高 %vpt，一页装得下才对", height)
	}
}

func TestPDFMarkdownNotLeakedToRenderedText(t *testing.T) {
	// gopdf 的 PDF 是 CID 编码，Go 侧读不出文字；这里断言的是「画之前」那层
	// 已经干净 —— 也就是 pdfDrawParagraph 收到的文本。真正的线上判据在
	// e2e/docgen_regression.py（用 pdftotext 读产物）。
	d := Doc{Format: "pdf", Title: "对账说明", Parags: []string{
		"## 一、对账范围", "本次覆盖 **2026年第一季度**。", "| 项目 | 金额 |", "| --- | --- |", "| 合计 | 20600 |",
	}}
	for _, b := range blocksOf(d.Parags, d.Title) {
		txt := b.text()
		for _, bad := range []string{"##", "**", "| ---"} {
			if strings.Contains(txt, bad) {
				t.Errorf("PDF 渲染前文本残留 %q: %q", bad, txt)
			}
		}
	}
	if _, err := Generate(d); err != nil {
		t.Fatalf("带 markdown 的 PDF 生成失败: %v", err)
	}
}

// ---------------------------------------------------------------------------
// PPT
// ---------------------------------------------------------------------------

// pptxText 解包 pptx，按幻灯片顺序拼出所有 <a:t> 文字。
func pptxText(t *testing.T, data []byte) (string, int) {
	t.Helper()
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("解 pptx 失败: %v", err)
	}
	slides := 0
	var sb strings.Builder
	reT := regexp.MustCompile(`<a:t>(.*?)</a:t>`)
	for _, f := range z.File {
		if !regexp.MustCompile(`^ppt/slides/slide\d+\.xml$`).MatchString(f.Name) {
			continue
		}
		slides++
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("打开 %s 失败: %v", f.Name, err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		for _, m := range reT.FindAllStringSubmatch(string(b), -1) {
			sb.WriteString(unescXML(m[1]))
			sb.WriteString("\n")
		}
	}
	return sb.String(), slides
}

func TestPPTStripsMarkdown(t *testing.T) {
	d := Doc{Format: "ppt", Title: "数据治理专场汇报", Parags: []string{
		"## 一、工作背景",
		"本次治理覆盖 **三个区县**，见下表：",
		"| 分组 | 人数 |",
		"| --- | --- |",
		"| 一组 | 12 |",
		"- 建立台账",
	}}
	b, err := buildPPTX(d)
	if err != nil {
		t.Fatalf("buildPPTX: %v", err)
	}
	text, slides := pptxText(t, b)
	if slides < 2 {
		t.Fatalf("至少应有标题页 + 内容页，实际 %d 张", slides)
	}
	for _, bad := range []string{"##", "**", "| ---"} {
		if strings.Contains(text, bad) {
			t.Errorf("PPT 上残留 markdown 记号 %q:\n%s", bad, text)
		}
	}
	for _, keep := range []string{"工作背景", "三个区县", "建立台账"} {
		if !strings.Contains(text, keep) {
			t.Errorf("PPT 丢了文字 %q:\n%s", keep, text)
		}
	}
	// markdown 表格要摊成一行行条目（表头 + 数据），不是把 | 当成文字
	if !strings.Contains(text, "分组　人数") || !strings.Contains(text, "一组　12") {
		t.Errorf("PPT 表格没铺成行:\n%s", text)
	}
}

// 正文超过 12 条时旧实现**静默截断**（第 13 条以后直接消失）。
func TestPPTDoesNotTruncateLongBody(t *testing.T) {
	parags := make([]string, 0, 20)
	for i := 1; i <= 20; i++ {
		parags = append(parags, itoa(i)+"、第 "+itoa(i)+" 项工作要点。")
	}
	d := Doc{Format: "ppt", Title: "年度工作要点", Parags: parags}
	b, err := buildPPTX(d)
	if err != nil {
		t.Fatalf("buildPPTX: %v", err)
	}
	text, slides := pptxText(t, b)
	if slides < 4 {
		t.Errorf("20 条要点应分到多张内容页，实际共 %d 张", slides)
	}
	for i := 1; i <= 20; i++ {
		if !strings.Contains(text, "第 "+itoa(i)+" 项工作要点") {
			t.Errorf("第 %d 条要点被截断了（旧实现只发前 12 条）", i)
		}
	}
}
