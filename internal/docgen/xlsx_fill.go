package docgen

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"regexp"
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
