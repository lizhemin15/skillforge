package docgen

import (
	"fmt"
	"log"
	"strings"

	"sync"

	"github.com/signintech/gopdf"
)

// ================= PDF 版式 =================
//
// 与 Word 共用同一份「公文块」（gongwenBlocks，见 gongwen_block.go）：
// 正文来源只有一份（模型 parags / 上一轮 markdown 直通），出口有多份，
// 结构判定只做一次，各渲染器只负责「画成什么样」。
//
// 2026-09-27 修的那两个毛病，PDF 当时一个不落全有：
//   - "## 一、对账说明" / "**2026年第一季度**" / "| --- |" 原样画进页面；
//   - 表格布局参数传错（NewTableLayout 的第 3 个参数是行高，被塞了列宽，
//     第 4 个是 maxRows，被塞了 40）→ 表格高 41 行、约 7000pt，远超一页，
//     后画的正文直接压在空表格上，pdftotext 里能看到正文夹在表格行之间。
//
// 版式按 GB/T 9704-2012：A4、上 3.7 / 下 3.5 / 左 2.8 / 右 2.6 cm、
// 正文三号（16pt）、固定行距 28pt、首行缩进 2 字符（32pt）。
// 字体只有一份（pdf_font.go 的覆盖率探测选出来的唯一 CJK 字体），
// 所以黑体/楷体这种**字面层次**在 PDF 里做不到，只能靠缩进与分段体现。
const (
	pdfMarginTop    = 104.9 // 3.7cm
	pdfMarginBottom = 99.2  // 3.5cm
	pdfMarginLeft   = 79.4  // 2.8cm
	pdfMarginRight  = 73.7  // 2.6cm

	pdfSizeTitle = 22.0 // 二号
	pdfSizeBody  = 16.0 // 三号
	pdfLineH     = 28.0 // 28 磅固定行距
	pdfIndent    = 32.0 // 首行缩进 2 字符 = 2 × 16pt

	pdfTableFont = 12.0 // 表格内文字（五号，公文里表格通常比正文小）
	pdfTableRowH = 22.0
)

// buildPDF generates a PDF using the gopdf engine plus a CID-keyed TrueType
// CJK font. It renders a centered title, optional tables with borders, and body
// paragraphs laid out in 公文 style. 100% offline, no network calls.
//
// 字体不是写死的常量：见 pdf_font.go 的 Bug G 说明，必须用覆盖率探测选字体，
// 否则缺字形的字符会被 gopdf 静默替换成空格，导致数字整段消失。
func buildPDF(d Doc) ([]byte, error) {
	fontPath, _ := resolvePDFFont()
	if fontPath == "" {
		return nil, fmt.Errorf("docgen pdf: no usable CJK font found (candidates: %v)", pdfFontCandidates)
	}

	pdf := &gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: *gopdf.PageSizeA4})

	// 绘制期兜底：即使门槛字符集通过，具体文档也可能用到更生僻的字。
	// 记录下来并 WARN，避免「静默变空格」这种事再次无声发生。
	var (
		mu          sync.Mutex
		drawMissing []rune
	)
	fontOpt := gopdf.TtfOption{
		OnGlyphNotFound: func(r rune) { mu.Lock(); drawMissing = append(drawMissing, r); mu.Unlock() },
	}
	if err := pdf.AddTTFFontWithOption("cjk", fontPath, fontOpt); err != nil {
		return nil, fmt.Errorf("docgen pdf font %s: %w", fontPath, err)
	}

	pdf.AddPage()
	pageW := gopdf.PageSizeA4.W
	pageH := gopdf.PageSizeA4.H
	contentW := pageW - pdfMarginLeft - pdfMarginRight
	contentBottom := pageH - pdfMarginBottom

	y := pdfMarginTop

	// 标题：二号居中（超宽自动折行，逐行居中）
	if strings.TrimSpace(d.Title) != "" {
		pdf.SetFont("cjk", "", pdfSizeTitle)
		for _, ln := range wrapText(pdf, mdPlainText(d.Title), contentW) {
			if y+pdfLineH > contentBottom {
				pdf.AddPage()
				y = pdfMarginTop
			}
			w, _ := pdf.MeasureTextWidth(ln)
			pdf.SetXY(pdfMarginLeft+(contentW-w)/2, y)
			pdf.Cell(nil, ln)
			y += pdfLineH
		}
		y += pdfLineH / 2
	}

	// 结构化表格（Doc.Cols/Rows）：与正文里的 markdown 表格走同一套画法
	if len(d.Cols) > 0 {
		rows := append([][]string{d.Cols}, d.Rows...)
		var err error
		y, err = pdfDrawTable(pdf, rows, y, contentW, pdfMarginLeft, contentBottom, pdfMarginTop)
		if err != nil {
			return nil, err
		}
	}

	for _, b := range gongwenBlocks(d.Parags, d.Title) {
		switch b.kind {
		case blockTable:
			var err error
			y, err = pdfDrawTable(pdf, b.rows, y, contentW, pdfMarginLeft, contentBottom, pdfMarginTop)
			if err != nil {
				return nil, err
			}
		case blockSignoff:
			y = pdfDrawRight(pdf, b.text(), y, contentW, contentBottom)
		default:
			text := b.text()
			if b.kind == blockBullet {
				text = pdfRewriteBullet(text, fontPath)
			}
			indent := 0.0
			if b.kind == blockBody || b.kind == blockH1 || b.kind == blockH2 || b.kind == blockH3 ||
				b.kind == blockBullet || b.kind == blockOrdered {
				indent = pdfIndent
			}
			y = pdfDrawParagraph(pdf, text, indent, y, contentW, contentBottom)
		}
	}

	buf, err := pdf.GetBytesPdfReturnErr()
	if err != nil {
		return nil, fmt.Errorf("docgen pdf: %w", err)
	}

	// 收尾自检：把绘制期真正缺字形的字符报出来（gopdf 已按空格渲染，用户看不到）。
	mu.Lock()
	miss := dedupRunes(drawMissing)
	mu.Unlock()
	if len(miss) > 0 {
		log.Printf("[docgen/pdf] WARN %d rune(s) missing from font %s, rendered blank: %q",
			len(miss), fontPath, string(miss))
	}

	return buf, nil
}

// pdfBulletCandidates 是候选项目符号：优先 U+2022 •，再退回字体里确实有字形的符号。
var pdfBulletCandidates = []string{"•", "●", "○", "·", "-"}

var (
	pdfBulletOnce sync.Once
	pdfBulletPick string
)

// pdfBulletGlyph 选出 PDF 字体里**真的有字形**的项目符号。
//
// 本机实际用的字体（文鼎简宋 gbsn00lp）没有 U+2022 •：直接画下去，
// gopdf 会静默渲染成空格 —— 用户看到的是一行「没有项目符号」的列表，
// 而绘制期只留一条 WARN（Bug G 那类「静默变空格」）。
// 所以这里先探测覆盖率再选符号，和 pdf_font.go 选字体是同一个道理。
func pdfBulletGlyph(fontPath string) string {
	pdfBulletOnce.Do(func() {
		for _, c := range pdfBulletCandidates {
			if miss, err := probeFontCoverage(fontPath, c); err == nil && len(miss) == 0 {
				pdfBulletPick = c
				return
			}
		}
		pdfBulletPick = "-"
	})
	return pdfBulletPick
}

// pdfRewriteBullet 把「• xxx」换成字体支持的项目符号。
func pdfRewriteBullet(text, fontPath string) string {
	if !strings.HasPrefix(text, "• ") {
		return text
	}
	return pdfBulletGlyph(fontPath) + " " + strings.TrimPrefix(text, "• ")
}

// pdfDrawParagraph 画一段正文：首行缩进 indent，固定行距 28pt，超页自动翻页。
// 返回下一次可用的 y。
func pdfDrawParagraph(pdf *gopdf.GoPdf, text string, indent, y, contentW, contentBottom float64) float64 {
	if strings.TrimSpace(text) == "" {
		return y
	}
	pdf.SetFont("cjk", "", pdfSizeBody)
	lines := wrapIndented(pdf, text, contentW, indent)
	for i, ln := range lines {
		if y+pdfLineH > contentBottom {
			pdf.AddPage()
			y = pdfMarginTop
		}
		x := pdfMarginLeft
		if i == 0 {
			x += indent
		}
		pdf.SetXY(x, y)
		pdf.Cell(nil, ln)
		y += pdfLineH
	}
	return y
}

// pdfDrawRight 右对齐画一段（落款：单位名 / 成文日期）。
func pdfDrawRight(pdf *gopdf.GoPdf, text string, y, contentW, contentBottom float64) float64 {
	if strings.TrimSpace(text) == "" {
		return y
	}
	pdf.SetFont("cjk", "", pdfSizeBody)
	for _, ln := range wrapText(pdf, text, contentW) {
		if y+pdfLineH > contentBottom {
			pdf.AddPage()
			y = pdfMarginTop
		}
		w, _ := pdf.MeasureTextWidth(ln)
		pdf.SetXY(pdfMarginLeft+contentW-w, y)
		pdf.Cell(nil, ln)
		y += pdfLineH
	}
	return y
}

// pdfTablePlan 算出表格的几何参数。
//
// 单独抽出来是为了**能被断言**：2026-09-27 之前这里把
// NewTableLayout(startX, startY, rowHeight, maxRows) 的第 3、4 个参数写反了
// （列宽当行高、40 当表体行数），3 行的表格被撑成 41 行 ≈ 7000pt，
// 后画的正文全压在空表格上。这类"参数顺序错了但代码不报错"的 bug，
// 只有把几何量写成可测的纯函数才守得住。
func pdfTablePlan(rows [][]string) (cols int, colW, rowH, height float64, bodyRows int) {
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	if cols == 0 || len(rows) == 0 {
		return 0, 0, 0, 0, 0
	}
	rowH = pdfTableRowH
	bodyRows = len(rows) - 1
	height = float64(len(rows)) * rowH
	return cols, 0, rowH, height, bodyRows
}

// pdfDrawTable 画一张带边框的表格（首行为表头，灰底居中）。
//
// 参数顺序在这里是**有意**写全的：gopdf 的 NewTableLayout(startX, startY, rowHeight, maxRows)
// 第 3 个是行高、第 4 个是「表体行数下限」——两者写反过一次，表格被撑成 41 行、
// 约 7000pt 高，后画的正文全压在空表格上（2026-09-27 一并修掉）。
func pdfDrawTable(pdf *gopdf.GoPdf, rows [][]string, y, contentW, x0, contentBottom, topMargin float64) (float64, error) {
	cols, _, rowH, height, bodyRows := pdfTablePlan(rows)
	if cols == 0 {
		return y, nil
	}
	colW := contentW / float64(cols)
	// 表头 + 数据行的实际高度；翻页时表格整体挪到下一页，不跨页拆行。
	if y+height > contentBottom {
		pdf.AddPage()
		y = topMargin
	}

	pdf.SetFont("cjk", "", pdfTableFont)
	table := pdf.NewTableLayout(x0, y, rowH, bodyRows)
	for i := 0; i < cols; i++ {
		head := ""
		if i < len(rows[0]) {
			head = rows[0][i]
		}
		table.AddColumn(head, colW, "center")
	}
	for _, row := range rows[1:] {
		cells := make([]string, cols)
		for i := 0; i < cols; i++ {
			if i < len(row) {
				cells[i] = row[i]
			}
		}
		table.AddRow(cells)
	}
	table.SetHeaderStyle(gopdf.CellStyle{
		BorderStyle: gopdf.BorderStyle{Top: true, Left: true, Bottom: true, Right: true, Width: 0.5},
		FillColor:   gopdf.RGBColor{R: 240, G: 240, B: 240},
		TextColor:   gopdf.RGBColor{R: 0, G: 0, B: 0},
	})
	table.SetTableStyle(gopdf.CellStyle{
		BorderStyle: gopdf.BorderStyle{Top: true, Left: true, Bottom: true, Right: true, Width: 0.5},
		FillColor:   gopdf.RGBColor{R: 255, G: 255, B: 255},
		TextColor:   gopdf.RGBColor{R: 0, G: 0, B: 0},
	})
	if err := table.DrawTable(); err != nil {
		return y, fmt.Errorf("docgen pdf table: %w", err)
	}
	return y + height + pdfLineH/2, nil
}

// wrapIndented 按「首行缩进 indent」折行：第一行可用宽度少 indent。
func wrapIndented(pdf *gopdf.GoPdf, text string, contentW, indent float64) []string {
	if indent <= 0 {
		return wrapText(pdf, text, contentW)
	}
	// 先按整宽折行，再把第一行挤到缩进后的宽度里：实现简单，且不会让
	// 第一行短得离谱（逐字符重排的代价更高，收益只有一两个字的差别）。
	lines := wrapText(pdf, text, contentW)
	if len(lines) == 0 {
		return lines
	}
	if w, _ := pdf.MeasureTextWidth(lines[0]); w+indent <= contentW {
		return lines
	}
	rest := strings.Join(lines, "")
	return wrapText(pdf, rest, contentW-indent)
}

// wrapText splits a paragraph into lines that fit within the given width by
// measuring each run with the active PDF font.
func wrapText(pdf *gopdf.GoPdf, text string, maxW float64) []string {
	runes := []rune(text)
	if len(runes) == 0 {
		return []string{""}
	}
	var lines []string
	var cur []rune
	curW := 0.0
	space := rune(' ')
	oneCharW := func(ch rune) float64 {
		w, _ := pdf.MeasureTextWidth(string(ch))
		return w
	}
	for _, ch := range runes {
		cw := oneCharW(ch)
		if ch == space {
			// allow break before next long word; simplest: commit line if too wide
			if curW+cw > maxW && len(cur) > 0 {
				lines = append(lines, string(cur))
				cur = nil
				curW = 0.0
				continue
			}
		}
		if curW+cw > maxW && len(cur) > 0 {
			lines = append(lines, string(cur))
			cur = nil
			curW = 0.0
		}
		cur = append(cur, ch)
		curW += cw
	}
	if len(cur) > 0 {
		lines = append(lines, string(cur))
	}
	return lines
}
