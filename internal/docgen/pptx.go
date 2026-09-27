package docgen

import (
	"bytes"
	"fmt"
	"strings"

	"baliance.com/gooxml/color"
	"baliance.com/gooxml/measurement"
	"baliance.com/gooxml/presentation"
	"baliance.com/gooxml/schema/soo/dml"
)

// buildPPTX generates a .pptx deck using the gooxml library. The first slide
// carries the title; the body is laid out from the same 公文块 as Word/PDF
// (gongwenBlocks), so markdown 记号不会原样出现在幻灯片上。
//
// 2026-09-27 之前这里同样逐条遍历 parags：`## 一、` 这种行会带着井号当项目符号
// 发到幻灯片上；而且正文超过 12 条就**静默截断**（第 13 条以后整段消失，
// 用户只会觉得"PPT 少了一半"）。现在：记号在块层摘掉、超长正文自动分页续张。
//
// NOTE: gooxml is AGPL-3.0 — see README for the licensing note before shipping
// this in a closed-source product.
func buildPPTX(d Doc) ([]byte, error) {
	ppt := presentation.New()

	// 标题页
	titleSlide := ppt.AddSlide()
	tb := titleSlide.AddTextBox()
	tb.Properties().SetWidth(11 * measurement.Inch)
	tb.Properties().SetPosition(0.6*measurement.Inch, 0.6*measurement.Inch)
	tp := tb.AddParagraph()
	tp.Properties().SetAlign(dml.ST_TextAlignTypeCtr)
	if d.Title != "" {
		tr := tp.AddRun()
		tr.SetText(d.Title)
		tr.Properties().SetSize(40 * measurement.Point)
		tr.Properties().SetBold(true)
		tr.Properties().SetSolidFill(color.RGB(0x1F, 0x1F, 0x1F))
	}
	subt := tb.AddParagraph()
	subt.Properties().SetAlign(dml.ST_TextAlignTypeCtr)
	sr := subt.AddRun()
	sr.SetText("办公文档 · " + d.Format)
	sr.Properties().SetSize(16 * measurement.Point)
	sr.Properties().SetSolidFill(color.RGB(0x66, 0x66, 0x66))

	// 把块摊成幻灯片条目：标题类条目加粗、不挂项目符号；表格按行铺开。
	type item struct {
		text string
		bold bool
	}
	var items []item
	for _, b := range gongwenBlocks(d.Parags, d.Title) {
		switch b.kind {
		case blockTable:
			for i, row := range b.rows {
				line := strings.Join(row, "　")
				if strings.TrimSpace(line) == "" {
					continue
				}
				// 表头加粗当小标题，其余行按条目排
				items = append(items, item{text: line, bold: i == 0})
			}
		case blockH1, blockH2, blockH3:
			items = append(items, item{text: b.text(), bold: true})
		default:
			if t := b.text(); strings.TrimSpace(t) != "" {
				items = append(items, item{text: t})
			}
		}
	}
	// 没有任何正文时退回结构化表格的行
	if len(items) == 0 {
		for _, row := range d.Rows {
			if line := strings.Join(row, "　"); strings.TrimSpace(line) != "" {
				items = append(items, item{text: line})
			}
		}
	}

	const perSlide = 8
	for i := 0; i < len(items); i += perSlide {
		slide := ppt.AddSlide()
		head := slide.AddTextBox()
		head.Properties().SetWidth(11 * measurement.Inch)
		head.Properties().SetPosition(0.5*measurement.Inch, 0.3*measurement.Inch)
		hp := head.AddParagraph()
		hr := hp.AddRun()
		heading := d.Title
		if i > 0 {
			heading = fmt.Sprintf("%s（续 %d）", d.Title, i/perSlide+1)
		}
		hr.SetText(heading)
		hr.Properties().SetSize(28 * measurement.Point)
		hr.Properties().SetBold(true)
		hr.Properties().SetSolidFill(color.RGB(0x1F, 0x1F, 0x1F))

		body := slide.AddTextBox()
		body.Properties().SetWidth(11 * measurement.Inch)
		body.Properties().SetPosition(0.5*measurement.Inch, 1.2*measurement.Inch)
		for _, it := range items[i:min(i+perSlide, len(items))] {
			p := body.AddParagraph()
			if it.bold {
				p.Properties().SetLevel(0)
			} else {
				p.Properties().SetBulletChar("•")
				p.Properties().SetLevel(0)
			}
			r := p.AddRun()
			r.SetText(it.text)
			if it.bold {
				r.Properties().SetSize(20 * measurement.Point)
			} else {
				r.Properties().SetSize(16 * measurement.Point)
			}
			r.Properties().SetBold(it.bold)
			r.Properties().SetSolidFill(color.RGB(0x1F, 0x1F, 0x1F))
		}
	}

	if err := ppt.Validate(); err != nil {
		return nil, fmt.Errorf("docgen pptx validate: %w", err)
	}
	var buf bytes.Buffer
	if err := ppt.Save(&buf); err != nil {
		return nil, fmt.Errorf("docgen pptx save: %w", err)
	}
	return buf.Bytes(), nil
}
