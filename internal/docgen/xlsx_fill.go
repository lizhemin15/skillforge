package docgen

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Excel (xlsx) smart template fill.
//
// An xlsx is a zip containing worksheets (xl/worksheets/sheet{N}.xml) and
// optionally a shared string table (xl/sharedStrings.xml). Authoring tools
// differ: openpyxl writes string cells as inline strings
// (<c t="inlineStr"><is><t>…</t></is></c>) with no sharedStrings.xml, while
// other tools route repeated strings through the shared table
// (<c t="s"><v>idx</v></c>).
//
// Placeholders {{key}} are authored directly into a cell text. Because all
// layout (merged ranges, fills, borders, column widths, fonts) lives in the
// sheet's <mergeCells>/<cols>/<sheetFormatPr> and the cell style reference
// (s="N"), filling by editing only the <t> text preserves ALL layout.
//
// Fill replaces {{key}} in BOTH inline <t> runs and the shared string table,
// so templates from any tool fill correctly. Unknown / empty-valued keys are
// blanked (""), so a template still wraps cleanly.
// ---------------------------------------------------------------------------

var xlsxTextFiles = func() []struct{ prefix, match string } {
	return []struct{ prefix, match string }{
		{"xl/worksheets/sheet", ""},
		{"xl/sharedStrings.xml", ""},
	}
}

// xlsxFilesToTouch returns the zip entries whose <t> text may hold placeholders.
func xlsxFilesToTouch(zr *zip.Reader) []*zip.File {
	var out []*zip.File
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "xl/worksheets/sheet") {
			out = append(out, f)
			continue
		}
		if f.Name == "xl/sharedStrings.xml" {
			out = append(out, f)
		}
	}
	return out
}

// ExtractFieldsXLSX returns the {{key}} fields declared across every worksheet
// (inline + shared strings), deduped, in first-seen order.
func ExtractFieldsXLSX(xlsx []byte) ([]string, error) {
	zr, err := zip.NewReader(bytes.NewReader(xlsx), int64(len(xlsx)))
	if err != nil {
		return nil, err
	}
	ph := regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)
	tRe := regexp.MustCompile(`(?s)<t(?:[^>]*)>(.*?)</t>`)
	seen := map[string]bool{}
	var out []string
	for _, f := range xlsxFilesToTouch(zr) {
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
		for _, m := range tRe.FindAllStringSubmatch(string(data), -1) {
			for _, p := range ph.FindAllStringSubmatch(m[1], -1) {
				k := strings.TrimSpace(p[1])
				if k == "" || seen[k] {
					continue
				}
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out, nil
}

// FillXLSX replaces every {{key}} in an xlsx (inline strings and the shared
// string table) with its value, preserving all cell styling / merges / layout.
func FillXLSX(xlsx []byte, vals map[string]string) ([]byte, error) {
	return rewriteZIPFiles(xlsx, func(zr *zip.Reader) map[string][]byte {
		ph := regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)
		out := map[string][]byte{}
		for _, f := range xlsxFilesToTouch(zr) {
			rc, err := f.Open()
			if err != nil {
				continue
			}
			data, _ := io.ReadAll(rc)
			rc.Close()
			s := string(data)
			ns := ph.ReplaceAllStringFunc(s, func(ph0 string) string {
				m := ph.FindStringSubmatch(ph0)
				key := strings.TrimSpace(m[1])
				v, ok := vals[key]
				if !ok {
					return "" // unknown → blank
				}
				var esc bytes.Buffer
				_ = xml.EscapeText(&esc, []byte(v))
				return esc.String()
			})
			if ns != s {
				out[f.Name] = []byte(ns)
			}
		}
		// 第二阶段才做「数字提升」：得等占位符都替换完，才知道某个格子
		// 最终落到的是不是一个纯数字。共享表要先解析成 索引→文本，
		// 工作表里 <c t="s"><v>N</v></c> 才能查回实际值。
		var ss []string
		if b, ok := out["xl/sharedStrings.xml"]; ok {
			ss = parseSharedStrings(string(b))
		}
		for _, f := range xlsxFilesToTouch(zr) {
			if !strings.HasPrefix(f.Name, "xl/worksheets/") {
				continue
			}
			s := ""
			if b, ok := out[f.Name]; ok {
				s = string(b)
			} else {
				rc, err := f.Open()
				if err != nil {
					continue
				}
				data, _ := io.ReadAll(rc)
				rc.Close()
				s = string(data)
			}
			if ns := promoteNumericCells(s, ss); ns != s {
				out[f.Name] = []byte(ns)
			}
		}
		return out
	})
}

// rewriteZIPFiles rebuilds a zip, replacing specific entries' bytes, copying all
// others verbatim. replacements is a map of entry-name → new content for entries
// whose text changed.
func rewriteZIPFiles(src []byte, mutate func(*zip.Reader) map[string][]byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(src), int64(len(src)))
	if err != nil {
		return nil, err
	}
	repl := mutate(zr)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		orig, _ := io.ReadAll(rc)
		rc.Close()

		hdr := &zip.FileHeader{
			Name:   f.Name,
			Method: zip.Deflate,
		}
		hdr.SetModTime(f.Modified)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return nil, err
		}
		if nb, ok := repl[f.Name]; ok {
			if _, err := w.Write(nb); err != nil {
				return nil, err
			}
		} else {
			if _, err := w.Write(orig); err != nil {
				return nil, err
			}
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var _ = io.EOF

// ---------------------------------------------------------------------------
// 数字提升（number promotion）
//
// 模板里 {{金额}} 这种占位符本身就是一段文本，把值替换进去之后，格子还是
// 文本格（t="inlineStr" 或 t="s"）。Excel/WPS 里 SUM 这种格子恒为 0，
// 财务模板直接就是废的——数字看着对，公式全不认。
//
// 所以替换完要回头扫一遍：如果某个格子**最终整格就是一个纯数字**，
// 就把它改写成数值格。判断「是不是数字」交给 numericCell，于是
// 前导零编号（"007"）、超长订单号、带单位的 "5台" 都会保持文本。
//
// 模板来源工具不同，文本格有两种形态，都要认：
//   - inlineStr（openpyxl 等）：<c r="B5" s="5" t="inlineStr"><is><t>12000</t></is></c>
//   - 共享表（Excel/WPS）：    <c r="B5" s="5" t="s"><v>37</v></c>  ← ss[37] == "12000"
//
// 只动 t 属性，样式（s=）、引用（r=）原样保留，所以版式不会跑。
// ---------------------------------------------------------------------------

var cellRe = regexp.MustCompile(`(?s)<c\b[^>]*>.*?</c>`)
var inlineCellRe = regexp.MustCompile(`(?s)^<is>\s*<t[^>]*>([^<]*)</t>\s*</is>$`)
var sharedCellRe = regexp.MustCompile(`(?s)^<v>\s*(\d+)\s*</v>$`)
var tAttrRe = regexp.MustCompile(`\s+t="[^"]*"`)

// promoteNumericCells 把「整格就是一个纯数字」的文本格改成数值格。
// ss 是共享字符串表（索引顺序）；为 nil 时只处理 inlineStr。
func promoteNumericCells(sheetXML string, ss []string) string {
	return cellRe.ReplaceAllStringFunc(sheetXML, func(cell string) string {
		gt := strings.Index(cell, ">")
		if gt < 0 || !strings.HasSuffix(cell, "</c>") {
			return cell
		}
		open := cell[:gt]
		body := strings.TrimSpace(cell[gt+1 : len(cell)-len("</c>")])

		var text string
		switch {
		case strings.Contains(open, `t="inlineStr"`):
			m := inlineCellRe.FindStringSubmatch(body)
			if m == nil {
				return cell // <is> 里是富文本多段，别碰
			}
			text = m[1]
		case strings.Contains(open, `t="s"`):
			m := sharedCellRe.FindStringSubmatch(body)
			if m == nil {
				return cell
			}
			idx, err := strconv.Atoi(m[1])
			if err != nil || idx < 0 || idx >= len(ss) {
				return cell
			}
			text = ss[idx]
		default:
			return cell // 已经是数值格 / 其他类型
		}

		v, ok := numericCell(text)
		if !ok {
			return cell
		}
		newOpen := tAttrRe.ReplaceAllString(open, "")
		if newOpen == "" {
			newOpen = "<c"
		}
		return newOpen + "><v>" + strconv.FormatFloat(v, 'f', -1, 64) + "</v></c>"
	})
}

// parseSharedStrings 按索引顺序取出共享字符串表的文本，
// <si> 内的多段 <r><t> 按序拼接（和 Excel 的解析一致）。
func parseSharedStrings(xmlDoc string) []string {
	siRe := regexp.MustCompile(`(?s)<si>(.*?)</si>`)
	tRe := regexp.MustCompile(`(?s)<t[^>]*>(.*?)</t>`)
	var out []string
	for _, si := range siRe.FindAllStringSubmatch(xmlDoc, -1) {
		var sb strings.Builder
		for _, t := range tRe.FindAllStringSubmatch(si[1], -1) {
			sb.WriteString(t[1])
		}
		out = append(out, sb.String())
	}
	return out
}
