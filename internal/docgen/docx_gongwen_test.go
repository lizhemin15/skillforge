package docgen

import (
	"archive/zip"
	"bytes"
	"io"
	"regexp"
	"strings"
	"testing"
)

// ===== 公文排版 / markdown 还原的回归测试 =====
//
// 守的是用户 2026-09-27 反馈的那件事：
//
//	「生成 word 的时候没有按公文格式去生成，而是直接把 markdown 放进去了，这很难看」
//
// 断言落在**解出来的 document.xml** 上，不落在中间结构上：用户拿到的是 .docx，
// 字体、字号、缩进、表格是不是真的写进了文件，只有解开 zip 看得到。

// docxXML 取出 .docx 里的 word/document.xml。
func docxXML(t *testing.T, data []byte) string {
	t.Helper()
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("解 zip 失败: %v", err)
	}
	for _, f := range z.File {
		if f.Name == "word/document.xml" {
			rc, err := f.Open()
			if err != nil {
				t.Fatalf("打开 document.xml 失败: %v", err)
			}
			defer rc.Close()
			b, _ := io.ReadAll(rc)
			return string(b)
		}
	}
	t.Fatalf("docx 里没有 word/document.xml")
	return ""
}

// docxZipNames 列出包内文件名。
func docxZipNames(t *testing.T, data []byte) []string {
	t.Helper()
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("解 zip 失败: %v", err)
	}
	out := make([]string, 0, len(z.File))
	for _, f := range z.File {
		out = append(out, f.Name)
	}
	return out
}

// docxPlain 把 document.xml 里所有 <w:t> 的文字按顺序拼起来（段落之间补换行）。
func docxPlain(t *testing.T, data []byte) string {
	t.Helper()
	x := docxXML(t, data)
	var sb strings.Builder
	for _, m := range regexp.MustCompile(`(?s)<w:p[ >].*?</w:p>`).FindAllString(x, -1) {
		for _, tm := range regexp.MustCompile(`(?s)<w:t[^>]*>(.*?)</w:t>`).FindAllStringSubmatch(m, -1) {
			sb.WriteString(unescXML(tm[1]))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func unescXML(s string) string {
	r := strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&apos;", "'")
	return r.Replace(s)
}

// mustDocx 生成一份 Word 并解包。
func mustDocx(t *testing.T, d Doc) []byte {
	t.Helper()
	if d.Format == "" {
		d.Format = "word"
	}
	b, err := Generate(d)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	return b
}

// paraInfo 是一个段落：原始 XML + 可见文字。
type paraInfo struct {
	XML  string
	Text string
}

// docxParagraphs 按文档顺序取出所有段落（表格单元格里的段落也在其中，顺序即出现顺序）。
//
// 用顺序扫描而不是正则：Go 的 regexp（RE2）不支持 (?!...) 这类环视，
// 段落里又嵌着成对的标签，拿正则框段落只会写出更难懂的表达式。
func docxParagraphs(t *testing.T, data []byte) []paraInfo {
	t.Helper()
	x := docxXML(t, data)
	var out []paraInfo
	for {
		i := strings.Index(x, "<w:p>")
		if i < 0 {
			break
		}
		j := strings.Index(x[i:], "</w:p>")
		if j < 0 {
			break
		}
		raw := x[i : i+j+len("</w:p>")]
		var sb strings.Builder
		for _, tm := range regexp.MustCompile(`(?s)<w:t[^>]*>(.*?)</w:t>`).FindAllStringSubmatch(raw, -1) {
			sb.WriteString(unescXML(tm[1]))
		}
		out = append(out, paraInfo{XML: raw, Text: sb.String()})
		x = x[i+j+len("</w:p>"):]
	}
	return out
}

// findPara 找第一个文字含 needle 的段落。
func findPara(t *testing.T, data []byte, needle string) (paraInfo, bool) {
	t.Helper()
	for _, p := range docxParagraphs(t, data) {
		if strings.Contains(p.Text, needle) {
			return p, true
		}
	}
	return paraInfo{}, false
}

// ---------------------------------------------------------------------------
// 一、markdown 不许原样进文档
// ---------------------------------------------------------------------------

func TestWordStripsMarkdownMarkers(t *testing.T) {
	md := "# 关于开展安全生产大检查的通知\n\n" +
		"各县区应急管理局：\n\n" +
		"本次检查旨在**压实各级责任**，重点包括：\n\n" +
		"- 风险分级管控\n- 隐患排查治理\n\n" +
		"| 阶段 | 时间 |\n| --- | --- |\n| 自查 | 3月1日 |\n\n" +
		"> 本通知自印发之日起执行。\n\n" +
		"特此通知。\n\nXX市应急管理局\n\n2026年2月28日"
	d := Doc{Title: "关于开展安全生产大检查的通知", Parags: strings.Split(md, "\n")}
	text := docxPlain(t, mustDocx(t, d))

	for _, bad := range []string{"#", "**", "| --- |", "> ", "- 风险"} {
		if strings.Contains(text, bad) {
			t.Errorf("markdown 记号 %q 原样进了文档:\n%s", bad, text)
		}
	}
	if !strings.Contains(text, "压实各级责任") {
		t.Errorf("去掉 ** 的内容丢了:\n%s", text)
	}
	if !strings.Contains(text, "本通知自印发之日起执行。") {
		t.Errorf("引用块的文字丢了:\n%s", text)
	}
	if !strings.Contains(text, "风险分级管控") || !strings.Contains(text, "隐患排查治理") {
		t.Errorf("列表项文字丢了:\n%s", text)
	}
}

func TestWordMarkdownTableBecomesRealTable(t *testing.T) {
	md := "各县区：\n\n| 阶段 | 时间 |\n| --- | --- |\n| 自查 | 3月1日 |\n| 督查 | 3月15日 |\n"
	d := Doc{Title: "检查安排", Parags: strings.Split(md, "\n")}
	x := docxXML(t, mustDocx(t, d))

	rows := regexp.MustCompile(`(?s)<w:tr>.*?</w:tr>`).FindAllString(x, -1)
	if len(rows) != 3 {
		t.Fatalf("markdown 表格应变成 3 行真表格（含表头），实际 %d 行", len(rows))
	}
	for i, r := range rows {
		if n := strings.Count(r, "<w:tc>"); n != 2 {
			t.Errorf("第 %d 行应有 2 个单元格，实际 %d", i, n)
		}
	}
	if !strings.Contains(rows[0], `w:fill="D9D9D9"`) {
		t.Errorf("表头没有底色")
	}
	if !strings.Contains(rows[0], "<w:b/>") {
		t.Errorf("表头没有加粗")
	}
	if strings.Contains(docxPlain(t, mustDocx(t, d)), "| ---") {
		t.Errorf("分隔行应当丢弃，不该出现在正文里")
	}
}

func TestWordTitleNotDuplicated(t *testing.T) {
	// 模型常把标题同时写进 title 和第一条 parags；用户看到的是标题排两遍。
	d := Doc{Title: "关于开展安全生产大检查的通知",
		Parags: []string{"# 关于开展安全生产大检查的通知", "正文内容。"}}
	text := docxPlain(t, mustDocx(t, d))
	if n := strings.Count(text, "关于开展安全生产大检查的通知"); n != 1 {
		t.Errorf("标题应只出现一次，实际 %d 次:\n%s", n, text)
	}
	// 「标题：xxx」这种重复信息同样要丢掉
	d2 := Doc{Title: "员工培训方案", Parags: []string{"标题：员工培训方案（送审稿）", "正文内容。"}}
	text2 := docxPlain(t, mustDocx(t, d2))
	if strings.Contains(text2, "标题：") {
		t.Errorf("「标题：」行应被丢弃:\n%s", text2)
	}
}

// ---------------------------------------------------------------------------
// 二、公文版式（GB/T 9704-2012）
// ---------------------------------------------------------------------------

func TestWordGongwenTypography(t *testing.T) {
	md := "各县区应急管理局：\n\n为贯彻落实上级部署，现将有关事项通知如下。\n\n" +
		"一、检查时间\n\n（一）自查阶段。\n\n特此通知。\n\nXX市应急管理局\n\n2026年2月28日"
	d := Doc{Title: "关于开展安全生产大检查的通知", Parags: strings.Split(md, "\n")}
	x := docxXML(t, mustDocx(t, d))

	// 标题：小标宋 二号（sz 44）居中
	if !strings.Contains(x, `w:eastAsia="`+fontTitle+`"`) {
		t.Errorf("标题字体不是 %s", fontTitle)
	}
	tp, ok := findPara(t, mustDocx(t, d), "关于开展安全生产大检查的通知")
	if !ok {
		t.Fatalf("找不到标题段落")
	}
	if !strings.Contains(tp.XML, `<w:jc w:val="center"/>`) {
		t.Errorf("标题没有居中:\n%s", tp.XML)
	}
	if !strings.Contains(tp.XML, `w:sz w:val="44"`) {
		t.Errorf("标题不是二号（sz 44）:\n%s", tp.XML)
	}
	if !strings.Contains(tp.XML, `w:eastAsia="`+fontTitle+`"`) {
		t.Errorf("标题没用小标宋:\n%s", tp.XML)
	}
	// 正文：仿宋_GB2312 三号（sz 32）+ 固定行距 28 磅 + 首行缩进 2 字符
	if !strings.Contains(x, `w:eastAsia="`+fontBody+`"`) {
		t.Errorf("正文字体不是 %s", fontBody)
	}
	if !strings.Contains(x, `<w:spacing w:line="560" w:lineRule="exact"`) {
		t.Errorf("正文没有 28 磅固定行距")
	}
	if !strings.Contains(x, `<w:ind w:firstLineChars="200" w:firstLine="640"/>`) {
		t.Errorf("正文没有首行缩进 2 字符")
	}
	// 层次字体：一级黑体、二级楷体
	if !strings.Contains(x, `w:eastAsia="`+fontH1+`"`) {
		t.Errorf("一级标题（一、）不是 %s", fontH1)
	}
	if !strings.Contains(x, `w:eastAsia="`+fontH2+`"`) {
		t.Errorf("二级标题（（一））不是 %s", fontH2)
	}
	// 页边距：上 3.7 / 下 3.5 / 左 2.8 / 右 2.6 cm
	for _, want := range []string{`w:top="2098"`, `w:bottom="1984"`, `w:left="1587"`, `w:right="1474"`} {
		if !strings.Contains(x, want) {
			t.Errorf("页边距缺少 %s", want)
		}
	}
}

func TestWordMainRecipientFlushLeftAndSignOffRight(t *testing.T) {
	md := "各县区应急管理局，各有关单位：\n\n现将有关事项通知如下。\n\n特此通知。\n\nXX市应急管理局\n\n2026年2月28日"
	d := Doc{Title: "通知", Parags: strings.Split(md, "\n")}

	paras := docxParagraphs(t, mustDocx(t, d))

	// 主送单位顶格：不带首行缩进
	recv, ok := findPara(t, mustDocx(t, d), "各县区应急管理局")
	if !ok {
		t.Fatalf("找不到主送单位段落")
	}
	if strings.Contains(recv.XML, "firstLine") {
		t.Errorf("主送单位应当顶格，实际带了首行缩进:\n%s", recv.XML)
	}
	// 落款（单位名 + 成文日期）右对齐
	var rights []paraInfo
	for _, p := range paras {
		if strings.Contains(p.XML, `<w:jc w:val="right"/>`) {
			rights = append(rights, p)
		}
	}
	if len(rights) != 2 {
		t.Fatalf("落款应有 2 段右对齐（单位名 + 日期），实际 %d", len(rights))
	}
	joined := rights[0].Text + "|" + rights[1].Text
	if !strings.Contains(joined, "XX市应急管理局") || !strings.Contains(joined, "2026年2月28日") {
		t.Errorf("右对齐的不是落款: %s", joined)
	}
}

func TestWordPackageHasStylesPart(t *testing.T) {
	d := Doc{Title: "通知", Parags: []string{"正文。"}}
	names := docxZipNames(t, mustDocx(t, d))
	var hasStyles, hasRels bool
	for _, n := range names {
		if n == "word/styles.xml" {
			hasStyles = true
		}
		if n == "word/_rels/document.xml.rels" {
			hasRels = true
		}
	}
	if !hasStyles {
		t.Errorf("缺少 word/styles.xml（文档默认值）。包内文件：%v", names)
	}
	if !hasRels {
		t.Errorf("styles.xml 没有被 document 的关系引用。包内文件：%v", names)
	}
}

// ---------------------------------------------------------------------------
// 三、内联样式：加粗/斜体/行内代码
// ---------------------------------------------------------------------------

func TestWordInlineEmphasis(t *testing.T) {
	runs := mdInlineRuns("这里是**重点**和*强调*以及`code`，还有~~删除~~。")
	var bold, italic, code, strike string
	for _, r := range runs {
		switch {
		case r.Bold:
			bold = r.Text
		case r.Italic:
			italic = r.Text
		case r.Code:
			code = r.Text
		case r.Strike:
			strike = r.Text
		}
	}
	if bold != "重点" || italic != "强调" || code != "code" || strike != "删除" {
		t.Fatalf("内联解析不对: bold=%q italic=%q code=%q strike=%q", bold, italic, code, strike)
	}
	d := Doc{Title: "T", Parags: []string{"这里是**重点**。"}}
	x := docxXML(t, mustDocx(t, d))
	if !strings.Contains(x, "<w:b/><w:bCs/>") {
		t.Errorf("加粗没有落成 <w:b/>")
	}
}

// ---------------------------------------------------------------------------
// 四、直通路径（上一轮 markdown 正文 → Word）也不许漏 markdown
// ---------------------------------------------------------------------------

func TestWordPassthroughMarkdownNotLeaked(t *testing.T) {
	// 这条正文就是 doc_passthrough.go 逐行搬进 parags 的形态：
	// 上一轮聊天里的 markdown 原文，一行一个元素。
	lines := []string{
		"## 数据治理专项培训安排",
		"",
		"各县区数据管理部门：",
		"",
		"1. **时间**：九月二十日至九月三十日。",
		"2. **地点**：市政务中心三层。",
		"",
		"| 分组 | 人数 |",
		"| --- | --- |",
		"| 一组 | 12 |",
		"",
		"特此通知。",
	}
	d := Doc{Title: "数据治理专项培训安排", Format: "word", Parags: lines}
	text := docxPlain(t, mustDocx(t, d))
	for _, bad := range []string{"##", "**", "|"} {
		if strings.Contains(text, bad) {
			t.Errorf("直通路径把 markdown 记号 %q 写进了文件:\n%s", bad, text)
		}
	}
	if !strings.Contains(text, "数据治理专项培训安排") || !strings.Contains(text, "市政务中心三层") {
		t.Errorf("正文内容丢了:\n%s", text)
	}
}

func TestWordMultilineSingleParag(t *testing.T) {
	// 模型有时把整篇塞进一个 parags 元素，行内的 markdown 结构也要认出来。
	d := Doc{Title: "通知", Parags: []string{"## 一、工作目标\n\n提高数据质量。"}}
	x := docxXML(t, mustDocx(t, d))
	if strings.Contains(docxPlain(t, mustDocx(t, d)), "##") {
		t.Errorf("整篇塞进一个元素时，井号漏了出来")
	}
	if !strings.Contains(x, `w:eastAsia="`+fontH1+`"`) {
		t.Errorf("整篇塞进一个元素时，一级标题没识别出来")
	}
}
