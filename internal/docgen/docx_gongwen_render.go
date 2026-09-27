package docgen

import (
	"bytes"
	"encoding/xml"
	"strings"
)

// ================= 公文段落的 OOXML 拼装 =================
//
// 每个段落都写**直接格式**（rPr / pPr），不依赖 styles.xml 里的命名样式：
// 公文要在 Word、WPS、以及各种在线预览里都长一个样，命名样式被各家的
// 默认样式表一覆盖就变形了。styles.xml 仍然会给一份，作用是文档默认值
// （复制粘贴出去的文字不至于退回系统默认字体）。

// runXML 拼一个 run。<w:sz> 以半磅为单位。
func runXML(r mdRun, font string, sz int, boldAll bool) string {
	ascii := font
	if r.Code {
		ascii = fontMono
	}
	var b strings.Builder
	b.WriteString(`<w:r><w:rPr><w:rFonts w:ascii="` + xmlAttr(ascii) + `" w:eastAsia="` + xmlAttr(font) + `" w:hAnsi="` + xmlAttr(ascii) + `"/>`)
	if boldAll || r.Bold {
		b.WriteString(`<w:b/><w:bCs/>`)
	}
	if r.Italic {
		b.WriteString(`<w:i/><w:iCs/>`)
	}
	if r.Strike {
		b.WriteString(`<w:strike/>`)
	}
	b.WriteString(`<w:sz w:val="` + itoa(sz) + `"/><w:szCs w:val="` + itoa(sz) + `"/></w:rPr>`)
	b.WriteString(`<w:t xml:space="preserve">` + xmlEsc(r.Text) + `</w:t></w:r>`)
	return b.String()
}

// paraOpt 描述一个段落的版式。
type paraOpt struct {
	font   string // eastAsia 字体
	sz     int    // 半磅
	align  string // "" | "center" | "right"
	indent bool   // 首行缩进 2 字符
	bold   bool   // 整段加粗
	hang   int    // 左侧缩进（twip，用于列表）
}

// docxPara 拼一个段落。
func docxPara(runs []mdRun, o paraOpt) string {
	if o.font == "" {
		o.font = fontBody
	}
	if o.sz == 0 {
		o.sz = sizeBody
	}
	var p strings.Builder
	p.WriteString(`<w:p><w:pPr>`)
	if o.align != "" {
		p.WriteString(`<w:jc w:val="` + o.align + `"/>`)
	}
	p.WriteString(`<w:spacing w:line="` + itoa(lineSpacing) + `" w:lineRule="exact" w:before="0" w:after="0"/>`)
	ind := ""
	if o.hang > 0 {
		ind = ` w:left="` + itoa(o.hang) + `" w:hanging="` + itoa(o.hang) + `"`
	}
	if o.indent {
		// firstLineChars 让 Word 按"字符"算缩进（2 字符），firstLine 是给不支持
		// 字符单位的渲染器（在线预览、部分 WPS 版本）的等效 twip 值。
		p.WriteString(`<w:ind w:firstLineChars="200" w:firstLine="` + itoa(firstLineIn) + `"` + ind + `/>`)
	} else if ind != "" {
		p.WriteString(`<w:ind` + ind + `/>`)
	}
	p.WriteString(`</w:pPr>`)
	for _, r := range runs {
		p.WriteString(runXML(r, o.font, o.sz, o.bold))
	}
	p.WriteString(`</w:p>`)
	return p.String()
}

// docxTitle 拼文档大标题：二号小标宋、居中、不缩进。
func docxTitle(title string) string {
	return docxPara(mdInlineRuns(title), paraOpt{font: fontTitle, sz: sizeTitle, align: "center"})
}

// docxTable 拼一张带边框的表格；首行为表头（灰底加粗）。
func docxTable(rows [][]string, shade string) string {
	if len(rows) == 0 {
		return ""
	}
	cols := 0
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	if shade == "" {
		shade = "D9D9D9"
	}
	var b strings.Builder
	b.WriteString(`<w:tbl><w:tblPr><w:tblW w:w="0" w:type="auto"/><w:jc w:val="center"/>` +
		`<w:tblBorders><w:top w:val="single" w:sz="4"/><w:left w:val="single" w:sz="4"/>` +
		`<w:bottom w:val="single" w:sz="4"/><w:right w:val="single" w:sz="4"/>` +
		`<w:insideH w:val="single" w:sz="4"/><w:insideV w:val="single" w:sz="4"/></w:tblBorders></w:tblPr>`)
	for ri, row := range rows {
		header := ri == 0
		b.WriteString(`<w:tr>`)
		for ci := 0; ci < cols; ci++ {
			text := ""
			if ci < len(row) {
				text = row[ci]
			}
			b.WriteString(`<w:tc><w:tcPr>`)
			if header {
				b.WriteString(`<w:shd w:val="clear" w:color="auto" w:fill="` + shade + `"/>`)
			}
			b.WriteString(`<w:vAlign w:val="center"/></w:tcPr>`)
			b.WriteString(docxPara(mdInlineRuns(text), paraOpt{font: fontBody, sz: 28, align: "center", bold: header}))
			b.WriteString(`</w:tc>`)
		}
		b.WriteString(`</w:tr>`)
	}
	b.WriteString(`</w:tbl>`)
	// 表格与后续正文之间留一个空行，否则表格会紧贴下一段
	b.WriteString(docxPara(nil, paraOpt{sz: sizeBody}))
	return b.String()
}

// renderWordBody 把 parags 铺成 Word 正文块。
//
// 结构判定全部在 gongwenBlocks（gongwen_block.go）里做完，这里只负责
// 「把这一块画成什么 Word 样式」——PDF / PPT 用的是同一份块，见那边的注释。
func renderWordBody(W func(string), parags []string, title string) {
	for _, b := range gongwenBlocks(parags, title) {
		switch b.kind {
		case blockTable:
			W(docxTable(b.rows, ""))
		case blockRecipient:
			W(docxPara(b.runs, paraOpt{}))
		case blockSignoff:
			W(docxPara(b.runs, paraOpt{align: "right"}))
		case blockH1:
			W(docxPara(b.runs, paraOpt{font: fontH1, indent: true}))
		case blockH2:
			W(docxPara(b.runs, paraOpt{font: fontH2, indent: true}))
		case blockH3:
			W(docxPara(b.runs, paraOpt{indent: true}))
		case blockBullet, blockOrdered:
			W(docxPara(b.runs, paraOpt{indent: true}))
		default:
			W(docxPara(b.runs, paraOpt{indent: true}))
		}
	}
}

// flattenLines 把 parags 摊平成行：单个元素里带 \n 的（模型常把整篇塞进一个
// 元素）也要按行拆开，否则 markdown 表格、标题都识别不出来。
func flattenLines(parags []string) []string {
	var out []string
	for _, p := range parags {
		out = append(out, strings.Split(p, "\n")...)
	}
	return out
}

// 预编译的转义器（docx.go 里的 xmlEsc 就是它，这里复用同一实现保持字节一致）
func xmlEsc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func xmlAttr(s string) string { return xmlEsc(s) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// stylesXML 给文档一份默认值：正文仿宋_GB2312 三号、固定行距 28 磅。
// 直接格式已经写在每个段落上，这里管的是「用户复制正文到别的文档」和
// 「渲染器没读直接格式」两种情况，让它们也不至于退回宋体小四。
func stylesXML() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:docDefaults><w:rPrDefault><w:rPr><w:rFonts w:ascii="` + fontMono + `" w:eastAsia="` + fontBody + `" w:hAnsi="` + fontMono + `"/>` +
		`<w:sz w:val="` + itoa(sizeBody) + `"/><w:szCs w:val="` + itoa(sizeBody) + `"/></w:rPr></w:rPrDefault>` +
		`<w:pPrDefault><w:pPr><w:spacing w:line="` + itoa(lineSpacing) + `" w:lineRule="exact"/></w:pPr></w:pPrDefault></w:docDefaults>` +
		`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/><w:qFormat/>` +
		`<w:pPr><w:spacing w:line="` + itoa(lineSpacing) + `" w:lineRule="exact"/></w:pPr>` +
		`<w:rPr><w:rFonts w:eastAsia="` + fontBody + `"/><w:sz w:val="` + itoa(sizeBody) + `"/></w:rPr></w:style>` +
		`</w:styles>`
}
