package docgen

import (
	"bytes"
	"fmt"

	"baliance.com/gooxml/color"
	"baliance.com/gooxml/measurement"
	"baliance.com/gooxml/presentation"
	"baliance.com/gooxml/schema/soo/dml"
)

// buildPPTX generates a .pptx deck using the gooxml library.
// The first slide carries the title; each body paragraph (or table row) becomes
// a bullet on the content slide. NOTE: gooxml is AGPL-3.0 — see README for the
// licensing note before shipping this in a closed-source product.
func buildPPTX(d Doc) ([]byte, error) {
	ppt := presentation.New()

	// reusable text box bullet
	addBullet := func(slide presentation.Slide, text string, y measurement.Distance, size measurement.Distance) {
		tb := slide.AddTextBox()
		tb.Properties().SetWidth(11 * measurement.Inch)
		tb.Properties().SetPosition(0.5*measurement.Inch, y)
		p := tb.AddParagraph()
		p.Properties().SetBulletChar("•")
		r := p.AddRun()
		r.SetText(text)
		r.Properties().SetSize(size)
	}

	// Title slide
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
		tr.Properties().SetSolidFill(color.RGB(0x1F, 0x1F, 0x1F))
	}
	subt := tb.AddParagraph()
	subt.Properties().SetAlign(dml.ST_TextAlignTypeCtr)
	sr := subt.AddRun()
	sr.SetText("办公文档 · " + d.Format)
	sr.Properties().SetSize(16 * measurement.Point)
	sr.Properties().SetSolidFill(color.RGB(0x66, 0x66, 0x66))

	// Content slide: bullets
	contentSlide := ppt.AddSlide()
	mk := contentSlide.AddTextBox()
	mk.Properties().SetWidth(11 * measurement.Inch)
	mk.Properties().SetPosition(0.5*measurement.Inch, 0.3*measurement.Inch)
	mtp := mk.AddParagraph()
	mtr := mtp.AddRun()
	mtr.SetText(d.Title)
	mtr.Properties().SetSize(28 * measurement.Point)
	mtr.Properties().SetSolidFill(color.RGB(0x1F, 0x1F, 0x1F))

	y := measurement.Distance(1.2 * float64(measurement.Inch))
	count := 0
	for _, p := range d.Parags {
		if count >= 12 {
			break
		}
		addBullet(contentSlide, p, y, 16*measurement.Point)
		y += measurement.Distance(0.7 * float64(measurement.Inch))
		count++
	}
	if count == 0 {
		for _, row := range d.Rows {
			if count >= 12 {
				break
			}
			line := ""
			for i, cell := range row {
				if i > 0 {
					line += "　"
				}
				line += cell
			}
			if line != "" {
				addBullet(contentSlide, line, y, 16*measurement.Point)
				y += measurement.Distance(0.7 * float64(measurement.Inch))
				count++
			}
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
