package docgen

import (
	"archive/zip"
	"bytes"
	"strings"
)

// buildDOCX generates a minimal but fully valid .docx using hand-rolled OOXML.
// It renders a title, optional column table and body paragraphs with 公文
// typography (see docx_gongwen.go / docx_gongwen_render.go) so Chinese text
// renders correctly in Word/WPS. Pure Go, no external library — keeps the
// binary lean.
func buildDOCX(d Doc) ([]byte, error) {
	var body bytes.Buffer
	W := func(s string) { body.WriteString(s) }

	if strings.TrimSpace(d.Title) != "" {
		W(docxTitle(d.Title))
	}

	// 结构化表格（规格里直接给了 cols/rows：名单、清单这类）
	if len(d.Cols) > 0 {
		rows := make([][]string, 0, len(d.Rows)+1)
		rows = append(rows, d.Cols)
		rows = append(rows, d.Rows...)
		W(docxTable(rows, "D9D9D9"))
	}

	// 正文：允许 markdown（标题记号 / 加粗 / 列表 / 表格），按公文版式铺开
	renderWordBody(W, d.Parags, d.Title)

	// Assemble the OOXML document.xml (body content + section settings).
	// 页边距按 GB/T 9704-2012 公文格式：上 3.7 / 下 3.5 / 左 2.8 / 右 2.6 cm。
	docXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
		`<w:body>` + body.String() +
		`<w:sectPr><w:pgSz w:w="11906" w:h="16838"/>` +
		`<w:pgMar w:top="` + marTop + `" w:right="` + marRight + `" w:bottom="` + marBottom + `" w:left="` + marLeft + `" w:header="708" w:footer="708" w:gutter="0"/>` +
		`</w:sectPr>` +
		`</w:body></w:document>`

	contentTypes := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="xml" ContentType="application/xml"/>` +
		`<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>` +
		`<Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/>` +
		`</Types>`

	rels := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>` +
		`</Relationships>`

	docRels := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>` +
		`</Relationships>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entries := map[string]string{
		"[Content_Types].xml":          contentTypes,
		"_rels/.rels":                  rels,
		"word/_rels/document.xml.rels": docRels,
		"word/document.xml":            docXML,
		"word/styles.xml":              stylesXML(),
	}
	// deterministic order
	for _, name := range []string{"[Content_Types].xml", "_rels/.rels", "word/_rels/document.xml.rels", "word/document.xml", "word/styles.xml"} {
		if err := zipAdd(zw, name, []byte(entries[name])); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
