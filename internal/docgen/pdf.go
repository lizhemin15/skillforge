package docgen

import (
	"fmt"
	"log"

	"sync"

	"github.com/signintech/gopdf"
)

// buildPDF generates a PDF using the gopdf engine plus a CID-keyed TrueType
// CJK font. It renders a centered bold title, an optional data table with
// borders, and body paragraphs. 100% offline, no network calls.
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
	if err := pdf.SetFont("cjk", "", 11); err != nil {
		return nil, fmt.Errorf("docgen pdf font: %w", err)
	}

	pdf.AddPage()
	margin := 30.0
	width := gopdf.PageSizeA4.W - 2*margin
	contentW := width
	contentH := gopdf.PageSizeA4.H - 2*margin

	// Title
	y := margin
	if d.Title != "" {
		pdf.SetFont("cjk", "", 18)
		pdf.SetXY(margin, y)
		pdf.Cell(nil, d.Title)
		y += 26
		pdf.SetStrokeColor(180, 180, 180)
		// underline under title
		pdf.SetLineWidth(1.0)
		pdf.Line(margin, y-8, margin+contentW, y-8)
		y += 6
	}

	// Table
	if len(d.Cols) > 0 {
		pdf.SetFont("cjk", "", 9)
		colW := contentW / float64(len(d.Cols))
		table := pdf.NewTableLayout(margin, y, colW, 40)
		for _, c := range d.Cols {
			table.AddColumn(c, colW, "left")
		}
		for _, row := range d.Rows {
			table.AddRow(row)
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
			return nil, fmt.Errorf("docgen pdf table: %w", err)
		}
		y += 12
	}

	// Body paragraphs
	pdf.SetFont("cjk", "", 11)
	for _, para := range d.Parags {
		// Simple line wrapping by measuring string width against content width.
		lines := wrapText(pdf, para, contentW)
		for _, ln := range lines {
			if y > contentH { // page break
				pdf.AddPage()
				y = margin
			}
			pdf.SetXY(margin, y)
			pdf.Cell(nil, ln)
			y += 16
		}
		y += 4
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
