package docgen

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
)

// buildDOCX generates a minimal but fully valid .docx using hand-rolled OOXML.
// It renders a title, optional column table and body paragraphs, all in 宋体
// (SimSun) with modern east-asian font fallback so Chinese text renders
// correctly in Word/WPS. Pure Go, no external library — keeps the binary lean.
func buildDOCX(d Doc) ([]byte, error) {
	var body bytes.Buffer
	xmlEsc := func(s string) string {
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	W := func(s string) { body.WriteString(s) }

	// Title paragraph
	if d.Title != "" {
		W(`<w:p><w:pPr><w:pStyle w:val="1"/><w:jc w:val="center"/></w:pPr>` +
			`<w:r><w:rPr><w:rFonts w:ascii="SimSun" w:eastAsia="宋体"/><w:b/><w:sz w:val="32"/></w:rPr>` +
			`<w:t xml:space="preserve">` + xmlEsc(d.Title) + `</w:t></w:r></w:p>`)
	}

	// Table (if cols+rows present)
	if len(d.Cols) > 0 {
		makeCell := func(text string, header bool) string {
			shd := ""
			if header {
				shd = `<w:shd w:val="clear" w:color="auto" w:fill="D9D9D9"/>`
			}
			b := ""
			if header {
				b = `<w:b/>`
			}
			return `<w:tc><w:tcPr>` + shd + `<w:vAlign w:val="center"/></w:tcPr>` +
				`<w:p><w:pPr><w:jc w:val="center"/></w:pPr><w:r><w:rPr>` + b + `</w:rPr>` +
				`<w:t xml:space="preserve">` + xmlEsc(text) + `</w:t></w:r></w:p></w:tc>`
		}
		W(`<w:tbl><w:tblPr><w:tblW w:w="0" w:type="auto"/>` +
			`<w:tblBorders><w:top w:val="single" w:sz="4"/>` +
			`<w:left w:val="single" w:sz="4"/>` +
			`<w:bottom w:val="single" w:sz="4"/>` +
			`<w:right w:val="single" w:sz="4"/>` +
			`<w:insideH w:val="single" w:sz="4"/>` +
			`<w:insideV w:val="single" w:sz="4"/></w:tblBorders></w:tblPr>`)
		// header row
		W(`<w:tr>`)
		for _, c := range d.Cols {
			W(makeCell(c, true))
		}
		W(`</w:tr>`)
		// data rows
		for _, row := range d.Rows {
			W(`<w:tr>`)
			for _, cell := range row {
				W(makeCell(cell, false))
			}
			W(`</w:tr>`)
		}
		W(`</w:tbl>`)
	}

	// Body paragraphs
	for _, p := range d.Parags {
		W(`<w:p><w:pPr><w:rPr><w:rFonts w:ascii="SimSun" w:eastAsia="宋体"/><w:sz w:val="24"/></w:rPr></w:pPr>` +
			`<w:r><w:rPr><w:rFonts w:ascii="SimSun" w:eastAsia="宋体"/><w:sz w:val="24"/></w:rPr>` +
			`<w:t xml:space="preserve">` + xmlEsc(p) + `</w:t></w:r></w:p>`)
	}

	// Assemble the OOXML document.xml (body content + section settings).
	docXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
		`<w:body>` + body.String() +
		`<w:sectPr><w:pgSz w:w="11906" w:h="16838"/><w:pgMar w:top="1440" w:right="1800" w:bottom="1440" w:left="1800" w:header="708" w:footer="708" w:gutter="0"/></w:sectPr>` +
		`</w:body></w:document>`

	contentTypes := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="xml" ContentType="application/xml"/>` +
		`<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>` +
		`</Types>`

	rels := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>` +
		`</Relationships>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entries := map[string]string{
		"[Content_Types].xml": contentTypes,
		"_rels/.rels":         rels,
		"word/document.xml":   docXML,
	}
	// deterministic order
	for _, name := range []string{"[Content_Types].xml", "_rels/.rels", "word/document.xml"} {
		if err := zipAdd(zw, name, []byte(entries[name])); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
