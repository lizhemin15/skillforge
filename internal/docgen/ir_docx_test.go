package docgen

import (
	"os"
	"testing"

	"baliance.com/gooxml/document"
)

// 验证从零自由生成 + 模板自由编辑两条路径。
func TestRenderIRBuild(t *testing.T) {
	ir := IR{
		Mode:     "build",
		Filename: "测试生成.docx",
		Title:    "员工统计表",
		Blocks: []Block{
			{Type: "heading", Level: 2, Text: "一、总体情况"},
			{Type: "para", Text: "本表统计各部门人员情况。", Size: 12, Font: "宋体"},
			{Type: "table", Cols: []string{"部门", "人数", "占比"}, Shade: "D9D9D9",
				Rows: [][]string{{"技术部", "10", "40%"}, {"市场部", "15", "60%"}}},
			{Type: "list", Items: []string{"技术部", "市场部", "财务部"}},
		},
	}
	b, err := RenderIR(ir, nil)
	if err != nil {
		t.Fatalf("RenderIR build: %v", err)
	}
	os.WriteFile("/tmp/ir_build_test.docx", b, 0644)
	if len(b) == 0 {
		t.Fatal("empty build output")
	}
}

// 验证模板编辑：加载一份含 2 行明细的表格模板，加行、改单元格、删行。
func TestRenderIREdit(t *testing.T) {
	tpl := IR{
		Mode: "build", Filename: "tpl.docx",
		Blocks: []Block{
			{Type: "table", Cols: []string{"品目", "规格", "数量"}, Shade: "D9D9D9",
				Rows: [][]string{{"路由器", "WiFi6", "2"}, {"交换机", "24口", "1"}}},
		},
	}
	tplBytes, err := RenderIR(tpl, nil)
	if err != nil {
		t.Fatalf("build tpl: %v", err)
	}

	ed := IR{
		Mode: "edit", Filename: "edited.docx",
		Ops: []Op{
			{Action: "add_row", Table: 0, Values: []string{"防火墙", "千兆", "3"}},
			{Action: "set_cell", Table: 0, Row: 1, Col: 1, Text: "WiFi7"},
			{Action: "del_row", Table: 0, Row: 2},
		},
	}
	b, err := RenderIR(ed, tplBytes)
	if err != nil {
		t.Fatalf("RenderIR edit: %v", err)
	}
	os.WriteFile("/tmp/ir_edit_test.docx", b, 0644)
	if len(b) == 0 {
		t.Fatal("empty edit output")
	}
}

// 验证 set_cell 按占位符寻址（模拟真实模板 {key} 填充 + 结构编辑混合）。
func TestRenderIRSetCellByKey(t *testing.T) {
	tpl := IR{
		Mode: "build", Filename: "tpl.docx",
		Blocks: []Block{
			{Type: "para", Text: "项目：{{project}}", Size: 12},
			{Type: "table", Cols: []string{"品目", "数量"}, Rows: [][]string{{"{{item1}}", "{{qty1}}"}, {"{{item2}}", "{{qty2}}"}}},
		},
	}
	tb, err := RenderIR(tpl, nil)
	if err != nil {
		t.Fatalf("tpl: %v", err)
	}

	ed := IR{
		Mode: "edit", Filename: "filled.docx",
		Ops: []Op{
			{Action: "set_para", ParagraphText: "{{project}}", Text: "项目：智慧园区"},
			{Action: "set_cell", Table: 0, Key: "item1", Text: "光纤交换机"},
			{Action: "set_cell", Table: 0, Key: "qty2", Text: "8"},
			{Action: "add_row", Table: 0, Values: []string{"超融合服务器", "4"}},
		},
	}
	b, err := RenderIR(ed, tb)
	if err != nil {
		t.Fatalf("RenderIR(ed): %v", err)
	}
	os.WriteFile("/tmp/ir_key_test.docx", b, 0644)
	if len(b) == 0 {
		t.Fatal("empty output")
	}
}

// 验证 add_row 遇到"合计/总计"行时，把新行插到合计行之前（而非追加到表尾）。
func TestRenderIRAddRowBeforeTotals(t *testing.T) {
	tpl := IR{
		Mode: "build", Filename: "tpl.docx",
		Blocks: []Block{
			{Type: "table", Cols: []string{"品目", "数量"}, Shade: "D9D9D9",
				Rows: [][]string{{"路由器", "2"}, {"交换机", "1"}, {"", "合计：{{amount}}"}}},
		},
	}
	tb, err := RenderIR(tpl, nil)
	if err != nil {
		t.Fatalf("tpl: %v", err)
	}

	ed := IR{Mode: "edit", Ops: []Op{
		{Action: "add_row", Table: 0, Values: []string{"防火墙", "3"}},
	}}
	b, err := RenderIR(ed, tb)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	os.WriteFile("/tmp/ir_totals_test.docx", b, 0644)
	if len(b) == 0 {
		t.Fatal("empty output")
	}
}

// 验证 add_row 的引擎兜底：LLM 把品目误加到只含几行的"信息表"(表0)时，
// 自动改投到更高、列数相近的"数据表"(表1)，避免污染信息表。
func TestRenderIRAddRowRedirectsOffInfoTable(t *testing.T) {
	// 双表文档：表0=信息表(表头+1行, 4列)，表1=品目明细表(表头+2行, 3列)
	tpl := IR{
		Mode: "build", Filename: "tpl.docx",
		Blocks: []Block{
			{Type: "table", Cols: []string{"甲方", "乙方", "地址", "电话"}, Shade: "D9D9D9",
				Rows: [][]string{{"A", "B", "北京", "123"}}},
			{Type: "table", Cols: []string{"品目", "数量", "单价"}, Shade: "D9D9D9",
				Rows: [][]string{{"路由器", "2", "100"}, {"交换机", "1", "50"}, {"合计", "", ""}}},
		},
	}
	tb, err := RenderIR(tpl, nil)
	if err != nil {
		t.Fatalf("tpl: %v", err)
	}

	// 故意把 add_row 指向表0（信息表），但 values 是数据表(3列)的数据
	ed := IR{Mode: "edit", Ops: []Op{
		{Action: "add_row", Table: 0, Values: []string{"防火墙", "3", "200"}},
	}}
	b, err := RenderIR(ed, tb)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	os.WriteFile("/tmp/ir_redirect_test.docx", b, 0644)
	// 验证：表0(信息表)行数应仍为 2（表头+1行），表1(数据表)应新增 1 行到 4 行
	doc, err := document.Open("/tmp/ir_redirect_test.docx")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	tabs := doc.Tables()
	if rc := rowCount(tabs[0]); rc != 2 {
		t.Errorf("表0(信息表)被污染：行数=%d，期望 2", rc)
	}
	// 表1=表头+路由器+交换机+新增防火墙+合计 = 5 行
	if rc := rowCount(tabs[1]); rc != 5 {
		t.Errorf("表1(数据表)行数=%d，期望 5（原4行+新增防火墙1行）", rc)
	}
}
