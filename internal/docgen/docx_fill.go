package docgen

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// 智能模板填充：对 template 类技能的 docx attachment 做"原位填充"。
//
// 设计：模板由 buildFormDOCX 生成，每个待填空单元格内放一个隐藏占位符
// {{字段名}}（白色小字，视觉不干扰）。填充时对 word/document.xml 做精确的
// 占位符→真实值 替换，再重新打包。因为是字符串替换而非 OOXML 树操作，天然
// 规避 run 拆分 / 格式继承等 docx 渲染坑（模板是我们自己生成的，占位符必然
// 落在单个 <w:t> 内）。用户无感占位符，拿到的是干净填好的表。
// ---------------------------------------------------------------------------

// FormField describes one fillable slot in a form template. The key is the
// placeholder token（不含花括号）; the label is a human-friendly prompt used to
// drive the LLM (and is what sits next to/above the empty cell in the doc).
type FormField struct {
	Key   string // placeholder token, e.g. "name"
	Label string // Chinese prompt, e.g. "姓名"
}

// buildFormDOCX renders a fillable form template (title + per-field rows, each
// empty value cell carrying a hidden {{key}} placeholder). Pure Go, offline.
func buildFormDOCX(title string, fields []FormField) ([]byte, error) {
	var body bytes.Buffer
	xmlEsc := func(s string) string {
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	W := func(s string) { body.WriteString(s) }

	if title != "" {
		W(`<w:p><w:pPr><w:pStyle w:val="1"/><w:jc w:val="center"/></w:pPr>` +
			`<w:r><w:rPr><w:rFonts w:ascii="SimSun" w:eastAsia="宋体"/><w:b/><w:sz w:val="32"/></w:rPr>` +
			`<w:t xml:space="preserve">` + xmlEsc(title) + `</w:t></w:r></w:p>`)
	}

	// helper: a cell. If field is nil it's a header/static label cell (drawn in
	// middle-grey, bold, centered). If non-nil it's the value cell holding the
	// hidden placeholder.
	mk := func(text string, isLabel bool) string {
		shd := ""
		b := ""
		if isLabel {
			shd = `<w:shd w:val="clear" w:color="auto" w:fill="F2F2F2"/>`
			b = `<w:b/>`
		}
		return `<w:tc><w:tcPr>` + shd + `<w:vAlign w:val="center"/></w:tcPr>` +
			`<w:p><w:pPr><w:jc w:val="center"/></w:pPr><w:r><w:rPr>` + b + `</w:rPr>` +
			`<w:t xml:space="preserve">` + xmlEsc(text) + `</w:t></w:r></w:p></w:tc>`
	}
	// cell with a hidden placeholder: placeholder rendered in near-white so it
	// is invisible to the reader but still a unique, exact-match anchor.
	mkPH := func(key string) string {
		return `<w:tc><w:tcPr><w:vAlign w:val="center"/></w:tcPr>` +
			`<w:p><w:pPr><w:jc w:val="center"/></w:pPr><w:r><w:rPr><w:color w:val="FFFFFF"/><w:sz w:val="4"/></w:rPr>` +
			`<w:t xml:space="preserve">{{` + xmlEsc(key) + `}}</w:t></w:r></w:p></w:tc>`
	}

	// Two-column form rows: label | value. Grid spans 2 columns of 4 total so
	// two fields fit per line; use a 4-col table where label takes 1 col and
	// value takes 1 col… simpler: fixed 4 columns, label(1)+value(1) pairs.
	W(`<w:tbl><w:tblPr><w:tblW w:w="9600" w:type="dxa"/>` +
		`<w:tblBorders><w:top w:val="single" w:sz="4"/>` +
		`<w:left w:val="single" w:sz="4"/>` +
		`<w:bottom w:val="single" w:sz="4"/>` +
		`<w:right w:val="single" w:sz="4"/>` +
		`<w:insideH w:val="single" w:sz="4"/>` +
		`<w:insideV w:val="single" w:sz="4"/></w:tblBorders>` +
		`<w:tblGrid><w:gridCol w:w="1800"/><w:gridCol w:w="3000"/><w:gridCol w:w="1800"/><w:gridCol w:w="3000"/></w:tblGrid></w:tblPr>`)
	// pair fields two per row
	for i := 0; i < len(fields); i += 2 {
		W(`<w:tr>`)
		f1 := fields[i]
		W(mk(f1.Label, true))
		W(mkPH(f1.Key))
		if i+1 < len(fields) {
			f2 := fields[i+1]
			W(mk(f2.Label, true))
			W(mkPH(f2.Key))
		} else {
			// last odd field: empty spacer cells
			W(mk("", false))
			W(mk("", false))
		}
		W(`</w:tr>`)
	}
	W(`</w:tbl>`)

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

var phRe = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)

// ExtractFields scans a docx and returns the field keys it declares as
// {{keys}} in word/document.xml (deduped, in first-seen order).
// docxFillableFiles returns the word entries whose <w:t> text may hold
// placeholders: the main document, plus every header/footer part.
func docxFillableFiles(zr *zip.Reader) []*zip.File {
	var out []*zip.File
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, "word/") {
			continue
		}
		if f.Name == "word/document.xml" ||
			(strings.HasPrefix(f.Name, "word/header") && strings.HasSuffix(f.Name, ".xml")) ||
			(strings.HasPrefix(f.Name, "word/footer") && strings.HasSuffix(f.Name, ".xml")) {
			out = append(out, f)
		}
	}
	return out
}

// ExtractFields returns the {{key}} fields declared across the main document,
// headers and footers, deduped, in first-seen order.
func ExtractFields(docx []byte) ([]string, error) {
	zr, err := zip.NewReader(bytes.NewReader(docx), int64(len(docx)))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range docxFillableFiles(zr) {
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		raw, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
		for _, m := range phRe.FindAllStringSubmatch(string(raw), -1) {
			k := strings.TrimSpace(m[1])
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, k)
		}
	}
	return out, nil
}

// Fill replaces every {{key}} placeholder in a docx (main document, headers,
// footers) with the corresponding value from vals, writing a fresh valid docx.
//
// It works on ANY docx (not just ones built by buildFormDOCX) by editing text at
// the <w:t> level and keeping the surrounding rich structure untouched:
//
//   - Complex layout (merged cells, borders, headers/footers, paragraph style,
//     run font/center/bold) lives in <w:pPr>/<w:tcPr>/<w:tblPr>/<w:rPr> which we
//     never touch — so a filled template keeps its original polished layout.
//   - Hidden white-tiny placeholders (buildFormDOCX legacy) are made visible by
//     stripping the invisible <w:color w:val="FFFFFF"/><w:sz w:val="4"/> pair.
//   - Unknown keys are replaced with the empty string (leave the cell blank)
//     rather than left as {{…}}, so a template still wraps cleanly.
func Fill(docx []byte, vals map[string]string) ([]byte, error) {
	out, err := rewriteZIPFiles(docx, func(zr *zip.Reader) map[string][]byte {
		out := map[string][]byte{}
		for _, f := range docxFillableFiles(zr) {
			rc, err := f.Open()
			if err != nil {
				continue
			}
			data, _ := io.ReadAll(rc)
			rc.Close()
			s := string(data)
			// 1) reveal any hidden white-tiny placeholder colour/size (legacy
			//    buildFormDOCX empty-cell markers) so filled values are visible.
			s = strings.ReplaceAll(s, `<w:color w:val="FFFFFF"/><w:sz w:val="4"/>`, ``)
			// 2) replace every {{key}} with its value at the <w:t> text level,
			//    preserving all surrounding structure/formatting.
			s = tRe.ReplaceAllStringFunc(s, func(tag string) string {
				m := tRe.FindStringSubmatch(tag)
				if m == nil || len(m) < 3 {
					return tag
				}
				open, txt := m[1], m[2]
				repl := phRe.ReplaceAllStringFunc(txt, func(ph string) string {
					p := phRe.FindStringSubmatch(ph)
					key := strings.TrimSpace(p[1])
					v, ok := vals[key]
					if !ok {
						return "" // unknown → blank
					}
					var esc bytes.Buffer
					_ = xml.EscapeText(&esc, []byte(v))
					return esc.String()
				})
				return "<w:t" + open + ">" + repl + `</w:t>`
			})
			if s != string(data) {
				out[f.Name] = []byte(s)
			}
		}
		return out
	})
	if err != nil {
		return nil, err
	}
	// 3) 合计金额自动重算：占位符填完后，按明细行的「数量 × 单价」重算合计表的
	//    大写/小写金额。没有合计表（或算不出）时原样返回。
	//    这一步必须在 Fill 内完成，否则续改时明细变了、合计还留着上一轮的数
	//    （LLM 也常常只改明细、把 amount 置空）。
	if rev, rerr := ReviseDocxTotals(out); rerr == nil && len(rev) > 0 {
		return rev, nil
	}
	return out, nil
}

// tRe matches a single <w:t …>text</w:t>. Group1 = opening tag, Group2 = text.
var tRe = regexp.MustCompile(`(?s)<w:t([^>]*)>(.*?)</w:t>`)

// documentXML returns the raw word/document.xml entry from a docx zip.
func documentXML(docx []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(docx), int64(len(docx)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, errors.New("docgen: docx has no word/document.xml")
}

// rewriteZIP copies every entry of a docx zip, replacing the named one with
// the transformed bytes. Preserves the rest byte-for-byte.
func rewriteZIP(docx []byte, replaceName string, fn func([]byte) ([]byte, error)) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(docx), int64(len(docx)))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	sort.Strings(names) // deterministic zip output
	for _, name := range names {
		for i := range zr.File {
			if zr.File[i].Name != name {
				continue
			}
			f := zr.File[i]
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			data, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return nil, err
			}
			if name == replaceName {
				data, err = fn(data)
				if err != nil {
					return nil, err
				}
			}
			if err := zipAdd(zw, name, data); err != nil {
				return nil, err
			}
			break
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
