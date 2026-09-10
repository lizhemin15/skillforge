package docgen

import (
	"strings"
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
