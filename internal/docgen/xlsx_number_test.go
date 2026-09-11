package docgen

import (
	"archive/zip"
	"bytes"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

// sheetXMLOf 从 xlsx 字节里取出工作表 XML，方便断言单元格类型。
func sheetXMLOf(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("xlsx 不是合法 zip: %v", err)
	}
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, "xl/worksheets/sheet") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("打开 %s 失败: %v", f.Name, err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		return string(data)
	}
	t.Fatal("xlsx 里没有工作表")
	return ""
}

// cellXML 取出指定引用单元格（如 C2）的完整 <c> 标签，找不到返回 ""。
func cellXML(sheet, ref string) string {
	needle := `r="` + ref + `"`
	i := strings.Index(sheet, needle)
	if i < 0 {
		return ""
	}
	start := strings.LastIndex(sheet[:i], "<c ")
	if start < 0 {
		return ""
	}
	end := strings.Index(sheet[start:], "</c>")
	if end < 0 {
		// 自闭合单元格写作 <c ... />
		end = strings.Index(sheet[start:], "/>")
		if end < 0 {
			return ""
		}
		return sheet[start : start+end+2]
	}
	return sheet[start : start+end+4]
}

// TestGeneratedXLSXNumbersAreNumeric 守住「Excel 里的数字必须是数值单元格」。
//
// 为什么值得一条测试：excelize 的 SetCellValue 收到 string 会写成**文本**单元格。
// 文本单元格在 Excel 里不能被 SUM 求和、不能参与公式——用户拿到一份
// 「看起来没问题、但求和全是 0」的报价单，而我们的交付说明还写着「生成成功」。
// 这类缺陷在命令行里完全看不出来，只有断言 XML 单元格类型才能钉死。
func TestGeneratedXLSXNumbersAreNumeric(t *testing.T) {
	b, err := Generate(Doc{
		Format:   "excel",
		Filename: "报价单.xlsx",
		Title:    "报价单",
		Cols:     []string{"产品", "数量", "单价"},
		Rows: [][]string{
			{"云服务器", "5", "12000"},
			{"技术支持", "1", "30000.50"},
		},
	})
	if err != nil {
		t.Fatalf("生成 xlsx 失败: %v", err)
	}
	sheet := sheetXMLOf(t, b)

	// 列 A=产品 是文字，必须还是文本（不能被强制转数字）；B/C 是数值列。
	cases := []struct {
		ref       string
		numeric   bool
		wantFloat float64 // numeric=true 时用数值比较（30000.50 与 30000.5 等价）
		wantText  string  // numeric=false 时用文本比较
	}{
		{"A2", false, 0, "云服务器"},
		{"B2", true, 5, ""},
		{"C2", true, 12000, ""},
		{"C3", true, 30000.50, ""},
	}
	for _, c := range cases {
		got := cellXML(sheet, c.ref)
		if got == "" {
			t.Errorf("%s 单元格不存在，工作表内容：%s", c.ref, truncate(sheet, 400))
			continue
		}
		isText := strings.Contains(got, `t="s"`) || strings.Contains(got, `t="str"`) || strings.Contains(got, `t="inlineStr"`)
		if c.numeric && isText {
			t.Errorf("%s 应为数值单元格，实际是文本：%s\n（Excel 里 SUM 会得 0）", c.ref, got)
			continue
		}
		if !c.numeric && !isText {
			t.Errorf("%s 应为文本单元格，实际是数值形式：%s", c.ref, got)
			continue
		}
		if !c.numeric {
			continue
		}
		// 数值单元格：从 <v> 里取值，按数值比较
		v := innerV(got)
		if v == "" {
			t.Errorf("%s 数值单元格里没有 <v>：%s", c.ref, got)
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			t.Errorf("%s 的 <v> 不是合法数字 %q：%s", c.ref, v, got)
			continue
		}
		if f != c.wantFloat {
			t.Errorf("%s 数值应为 %v，实际 %v：%s", c.ref, c.wantFloat, f, got)
		}
	}
}

// innerV 取出 <v>...</v> 里的内容。
func innerV(cell string) string {
	i := strings.Index(cell, "<v>")
	if i < 0 {
		return ""
	}
	rest := cell[i+3:]
	j := strings.Index(rest, "</v>")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// TestNumericCellKeepsIdentifiersAsText 守住上面那个修复的**另一半**：
// 不能为了「让数字能求和」就把所有像数字的东西都强转。
// 转错方向的破坏更隐蔽——"007" 变 7、"订单号 20260911000123456789" 变 2.026e19，
// Excel 里再也看不回原文，而且 SUM 出来的数字还"看着挺对"。
func TestNumericCellKeepsIdentifiersAsText(t *testing.T) {
	asText := []string{
		"007",                   // 前导零编号
		"0138",                  // 同上
		"20260911000123456789",  // 20 位长号，超过 15 位有效数字
		"1234567890123456789",   // 19 位
		"5台",                    // 带单位
		"¥12000",                // 带货币
		"12%",                   // 百分号
		"1,200",                 // 千分位（转了就丢格式）
		"2026-09-11",            // 日期
		"",                      // 空
		"   ",                   // 空白
		"1.2.3",                 // 版本号
		"abc",                   // 纯文字
		"１２３",                   // 全角数字（不是 ASCII 数字）
		"1e6",                   // 科学计数法：要文本，否则显示成 1000000 面目全非
		"+",                     // 只有符号
		"1.2345678901234567890", // 有效数字超 15
	}
	for _, s := range asText {
		if v, ok := numericCell(s); ok {
			t.Errorf("%q 被误判为数字（%v），应当是文本——转过去就再也读不回原文了", s, v)
		}
	}

	asNumber := map[string]float64{
		"5":               5,
		"12000":           12000,
		"30000.50":        30000.5,
		"-1200":           -1200,
		"+8":              8,
		"0":               0,
		"0.5":             0.5,
		".5":              0.5,
		"1200.00":         1200,
		" 42 ":            42,              // 前后空格应被容忍
		"999999999999999": 999999999999999, // 正好 15 位，仍可安全转
	}
	for s, want := range asNumber {
		v, ok := numericCell(s)
		if !ok {
			t.Errorf("%q 应被识别为数字，实际判成文本（Excel 里就不能求和了）", s)
			continue
		}
		if v != want {
			t.Errorf("%q 解析为 %v，期望 %v", s, v, want)
		}
	}
}

// TestFillXLSXPromotesNumericCells 守住**模板填充**这条路：填进去的数字
// 必须落到数值单元格，否则采购单里「金额」列在 Excel 里 SUM 恒为 0。
//
// 为什么单列一条测试：生成（Generate）和填充（FillXLSX）是两条代码路径，
// 修了生成不代表填充也修了。真实模板（采购验收单）用的是 inlineStr，
// excelize 写出来的是共享表，两种形态都得过。
func TestFillXLSXPromotesNumericCells(t *testing.T) {
	f := excelize.NewFile()
	sh := "Sheet1"
	f.SetCellValue(sh, "A1", "{{name}}")
	f.SetCellValue(sh, "B1", "{{amount}}")
	f.SetCellValue(sh, "C1", "验收单编号：{{no}}")
	f.SetCellValue(sh, "D1", "{{code}}")
	f.SetCellValue(sh, "E1", "{{qty}}")
	f.SetCellValue(sh, "F1", "{{bigid}}")
	buf, err := f.WriteToBuffer()
	if err != nil {
		t.Fatalf("造模板失败: %v", err)
	}

	// 先确认 excelize 用的是共享表形态，否则这条测试没测到我们想测的东西。
	if sheet := sheetXMLOf(t, buf.Bytes()); !strings.Contains(sheet, `t="s"`) {
		t.Fatalf("excelize 生成的工作表里没有共享表引用 t=\"s\"，这条测试失去意义：%s", truncate(sheet, 300))
	}

	out, err := FillXLSX(buf.Bytes(), map[string]string{
		"name":   "云服务器",
		"amount": "12000",
		"no":     "HT-2026-001",
		"code":   "007",                  // 前导零编号
		"qty":    "5台",                   // 带单位
		"bigid":  "20260911000123456789", // 超长订单号
	})
	if err != nil {
		t.Fatalf("填模板失败: %v", err)
	}
	sheet := sheetXMLOf(t, out)

	// 纯数字 → 必须是数值单元格
	b1 := cellXML(sheet, "B1")
	if strings.Contains(b1, `t="s"`) || strings.Contains(b1, `t="inlineStr"`) {
		t.Errorf("B1（金额 12000）仍是文本单元格，Excel 里 SUM 会得 0：%s", b1)
	} else if v := innerV(b1); v != "12000" {
		t.Errorf("B1 的数值应为 12000，实际 %q：%s", v, b1)
	}

	// 混排文本、前导零、带单位、超长号 → 必须仍是文本
	for _, c := range []struct{ ref, why string }{
		{"A1", "普通文字"},
		{"C1", "整格是「验收单编号：HT-2026-001」这种混排"},
		{"D1", "前导零编号 007，转成数字就变 7"},
		{"E1", "带单位的 5台"},
		{"F1", "20 位订单号，超过 Excel 15 位有效数字"},
	} {
		got := cellXML(sheet, c.ref)
		if got == "" {
			t.Errorf("%s 单元格不存在：%s", c.ref, truncate(sheet, 400))
			continue
		}
		if !strings.Contains(got, `t="s"`) && !strings.Contains(got, `t="inlineStr"`) {
			t.Errorf("%s（%s）被转成了数值单元格，数据不可逆：%s", c.ref, c.why, got)
		}
	}
}

// TestPromoteNumericCellsInlineStr 覆盖另一种文本形态：inlineStr。
// 真实模板（采购验收单模板.xlsx）就是这种，和 excelize 的共享表不同。
func TestPromoteNumericCellsInlineStr(t *testing.T) {
	sheet := `<worksheet><sheetData>` +
		`<row r="1">` +
		`<c r="A1" s="1" t="inlineStr"><is><t>{{x}}</t></is></c>` + // 已被替换成文字
		`<c r="B1" s="2" t="inlineStr"><is><t>12000</t></is></c>` + // 纯数字 → 提升
		`<c r="C1" s="3" t="inlineStr"><is><t>007</t></is></c>` + // 前导零 → 保持
		`<c r="D1" s="4" t="inlineStr"><is><t>验收单编号：HT-1</t></is></c>` + // 混排 → 保持
		`<c r="E1" s="5" t="inlineStr"><is><t>5台</t></is></c>` + // 带单位 → 保持
		`<c r="F1" s="6" t="inlineStr"><is><r><t>富</t></r><r><t>文本</t></r></is></c>` + // 富文本 → 不碰
		`<c r="G1" s="7"><v>9</v></c>` + // 本来就是数值格 → 原样
		`<c r="H1" s="8"/>` + // 空胞 → 原样
		`</row></sheetData></worksheet>`

	got := promoteNumericCells(sheet, nil)

	b1 := cellXML(got, "B1")
	if strings.Contains(b1, `t="inlineStr"`) {
		t.Errorf("B1 应被提升为数值格，实际还是文本：%s", b1)
	}
	if v := innerV(b1); v != "12000" {
		t.Errorf("B1 的值应为 12000，实际 %q", v)
	}
	if !strings.Contains(b1, `s="2"`) {
		t.Errorf("B1 提升后丢了样式 s=\"2\"，版式会跑：%s", b1)
	}
	for _, ref := range []string{"A1", "C1", "D1", "E1", "F1"} {
		if c := cellXML(got, ref); !strings.Contains(c, `t="inlineStr"`) {
			t.Errorf("%s 不该被提升（值不是纯数字或不可逆），实际：%s", ref, c)
		}
	}
	if c := cellXML(got, "G1"); !strings.Contains(c, "<v>9</v>") {
		t.Errorf("G1 本来就是数值格，被改坏了：%s", c)
	}
	if c := cellXML(got, "H1"); c == "" {
		t.Errorf("H1 空胞被吃掉了")
	}
}
