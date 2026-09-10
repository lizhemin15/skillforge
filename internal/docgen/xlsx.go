package docgen

import (
	"fmt"

	"github.com/xuri/excelize/v2"
)

// buildXLSX generates a styled .xlsx spreadsheet using excelize. It renders a
// sheet (named after the title) with a bold bordered header row and data rows,
// and sets the column widths so content is readable. Chinese text works natively.
func buildXLSX(d Doc) ([]byte, error) {
	f := excelize.NewFile()
	sheetName := "Sheet1"
	if d.Title != "" {
		// use the title as the sheet tab name (max 31 chars, no the chars Excel forbids)
		clean := d.Title
		r := []rune(clean)
		if len(r) > 31 {
			r = r[:31]
		}
		cand := string(r)
		for _, ch := range []rune{'\\', '/', '?', '*', '[', ']', ':'} {
			cand = replaceAll(cand, ch, '_')
		}
		if cand != "" {
			if err := f.SetSheetName("Sheet1", cand); err == nil {
				sheetName = cand
			}
		}
	}

	// Header row with bold font + light fill
	write := func(rowIdx int, values []string, header bool) error {
		for ci, val := range values {
			cellName, err := excelize.CoordinatesToCellName(ci+1, rowIdx)
			if err != nil {
				return err
			}
			if err := f.SetCellValue(sheetName, cellName, val); err != nil {
				return err
			}
			if header {
				style, _ := f.NewStyle(&excelize.Style{
					Font:      &excelize.Font{Bold: true, Size: 11},
					Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"D9D9D9"}},
					Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center"},
				})
				_ = f.SetCellStyle(sheetName, cellName, cellName, style)
			}
		}
		return nil
	}

	if len(d.Cols) > 0 {
		if err := write(1, d.Cols, true); err != nil {
			return nil, err
		}
		for ri, row := range d.Rows {
			if err := write(ri+2, row, false); err != nil {
				return nil, err
			}
		}
	}

	// Auto width (rough estimate: CJK char ~ 2 units)
	if len(d.Cols) > 0 {
		for ci := range d.Cols {
			colName, _ := excelize.ColumnNumberToName(ci + 1)
			w := 12.0
			if len(d.Cols[ci]) > 0 {
				est := float64(len([]rune(d.Cols[ci]))) * 2.1
				if est > w {
					w = est
				}
			}
			_ = f.SetColWidth(sheetName, colName, colName, w+2)
		}
	}

	buf, err := f.WriteToBuffer()
	if err != nil {
		return nil, fmt.Errorf("docgen xlsx: %w", err)
	}
	return buf.Bytes(), nil
}

// replaceAll is a small rune-replacement helper used to sanitise sheet names.
func replaceAll(s string, old rune, new rune) string {
	runes := []rune(s)
	for i, r := range runes {
		if r == old {
			runes[i] = new
		}
	}
	return string(runes)
}
