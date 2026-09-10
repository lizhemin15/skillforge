// Package docgen generates common office documents (Word/Excel/PDF/PPT)
// entirely offline in Go. It is the backend engine behind the built-in
// "办公文档管家" skill: given a structured Doc spec it returns the raw bytes of
// a valid .docx / .xlsx / .pdf / .pptx file.
//
// Every generator is pure Go, no external services, no cgo — compatible with
// the single-binary offline deployment constraint of SkillForge.
package docgen

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
)

// Doc describes a document to generate. Format selects which file kind is
// produced; the remaining fields are shared across formats so the engine can
// be driven by one generic instruction.
type Doc struct {
	// Format is one of "word", "excel", "pdf", "ppt".
	Format string
	// Filename is the final download name, e.g. "员工信息表.xlsx".
	Filename string
	// Title is the document heading (Word/PDF/PPT title, Excel sheet title).
	Title string
	// Cols are the table column headers (Excel/Word/PDF table).
	Cols []string
	// Rows are the table body rows. For Word/PDF/PPT, rows may be empty and
	// Parags is used instead.
	Rows [][]string
	// Parags are body paragraphs (Word/PDF/PPT) rendered as lines of text.
	Parags []string
}

// Generate returns the bytes of an office file for the given doc.
func Generate(d Doc) ([]byte, error) {
	switch strings.ToLower(d.Format) {
	case "word", "docx", "doc":
		return buildDOCX(d)
	case "excel", "xlsx", "xls":
		return buildXLSX(d)
	case "pdf":
		return buildPDF(d)
	case "ppt", "pptx":
		return buildPPTX(d)
	default:
		return nil, fmt.Errorf("docgen: unsupported format %q", d.Format)
	}
}

// ContentType returns the HTTP content type for a generated doc format.
func ContentType(format string) string {
	switch strings.ToLower(format) {
	case "word", "docx", "doc":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case "excel", "xlsx", "xls":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case "pdf":
		return "application/pdf"
	case "ppt", "pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	default:
		return "application/octet-stream"
	}
}

// BuildForm renders a fillable form template (title + label/value rows with a
// hidden {{key}} placeholder in each value cell). It is the template generator
// behind the smart-fill flow for template skills with a docx attachment.
// FormField and Fill are defined in docx_fill.go.
func BuildForm(title string, fields []FormField) ([]byte, error) {
	return buildFormDOCX(title, fields)
}

// SafeFilename normalises a user-suggested filename, stripping path separators
// so it can be used verbatim as a download name. Kept for callers that do not
// know the target format; prefer SafeFilenameAs when the format is known.
func SafeFilename(name string) string {
	return SafeFilenameAs(name, "")
}

// canonicalExt maps a docgen format to the extension the rendered bytes really
// have. Used to stop a user/LLM-suggested name from lying about the payload
// (asking for "报告.exe" must not produce a download named *.exe).
func canonicalExt(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "word", "docx", "doc":
		return "docx"
	case "excel", "xlsx", "xls":
		return "xlsx"
	case "pdf":
		return "pdf"
	case "ppt", "pptx":
		return "pptx"
	}
	return ""
}

// SafeFilenameAs normalises a user-suggested filename AND forces its extension
// to match the format actually rendered:
//   - path separators and traversal dots are stripped ("../../etc/x" → "etc_x")
//   - a whitelisted extension that matches the format is preserved
//   - any other extension (".exe", ".mp4", or a mismatched one) is replaced by
//     the canonical extension for format, so name and bytes never disagree
//   - an empty name falls back to "document.<ext>"
func SafeFilenameAs(name, format string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "/", "_")
	// collapse traversal pairs ("..") then drop any leading dot/underscore
	// residue, so "../../etc/passwd" becomes "etc_passwd".
	for strings.Contains(name, "..") {
		name = strings.ReplaceAll(name, "..", "_")
	}
	// squeeze the separators left behind so "../../x" doesn't become "____x"
	for strings.Contains(name, "__") {
		name = strings.ReplaceAll(name, "__", "_")
	}
	name = strings.TrimLeft(name, "._")
	ext := canonicalExt(format)
	if name == "" {
		if ext == "" {
			return "document.pdf"
		}
		return "document." + ext
	}
	got := strings.TrimPrefix(strings.ToLower(PathExt(name)), ".")
	switch got {
	case "docx", "doc", "xlsx", "xls", "pdf", "pptx", "ppt":
		// known office extension: keep the name unless it contradicts the
		// rendered format (an .xlsx name on PDF bytes confuses every client).
		if ext != "" && got != ext {
			name = name[:len(name)-len(PathExt(name))] + "." + ext
		}
		return name
	}
	// unknown / missing extension → strip whatever tail exists and attach the
	// extension matching the real payload.
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		name = name[:i]
	}
	if ext == "" {
		return name
	}
	return name + "." + ext
}

// PathExt is a tiny extension retriever (avoid importing path/filepath just
// for this). It is kept internal so callers can change SafeFilename's rules
// without affecting generation.
func PathExt(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return ""
	}
	return name[i:]
}

// Helper for building zip-based Office formats (docx/xlsx/pptx are all zips).
// zipAdd appends a file entry to a zip writer from raw bytes.
func zipAdd(zw *zip.Writer, name string, data []byte) error {
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// buffer is a convenience alias so callers don't import bytes.
type buffer = bytes.Buffer
