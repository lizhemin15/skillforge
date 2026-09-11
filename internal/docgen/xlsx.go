package docgen

import (
	"fmt"
	"strconv"
	"strings"

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
			if v, ok := numericCell(val); ok {
				if err := f.SetCellValue(sheetName, cellName, v); err != nil {
					return err
				}
			} else if err := f.SetCellValue(sheetName, cellName, val); err != nil {
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

// numericCell 判断一个字符串是否该写成 Excel 数值单元格，返回解析后的值。
//
// 为什么必须区分：excelize 的 SetCellValue 收到 string 会写成共享字符串
// （t="s"），也就是**文本**单元格。文本单元格在 Excel 里不能参与 SUM 和公式——
// 用户拿到一份"每列都对、求和恒为 0"的报价单，而生成日志写的是"成功"。
// 这种缺陷在命令行里完全看不出来，只有断言 XML 才能发现。
//
// 反过来也不能一律强转：以下都必须是文本，转了就是数据损坏——
//   - 前导零编号："007"、"0138" 转身就变 7 / 138
//   - 超过 15 位有效数字（身份证号、长订单号）会丢精度（Excel 自身也只有 15 位）
//   - 带单位/货币/百分号："5台"、"¥12000"、"12%"
func numericCell(s string) (float64, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, false
	}
	body := t
	if body[0] == '+' || body[0] == '-' {
		body = body[1:]
	}
	if body == "" {
		return 0, false
	}
	digits, dots := 0, 0
	for i := 0; i < len(body); i++ {
		switch ch := body[i]; {
		case ch >= '0' && ch <= '9':
			digits++
		case ch == '.':
			dots++
		default:
			return 0, false
		}
	}
	if digits == 0 || dots > 1 || digits > 15 {
		return 0, false
	}
	intPart := body
	if i := strings.IndexByte(body, '.'); i >= 0 {
		intPart = body[:i]
	}
	if len(intPart) > 1 && intPart[0] == '0' {
		return 0, false // 前导零 = 编号，不是数字
	}
	v, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, false
	}
	return v, true
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
