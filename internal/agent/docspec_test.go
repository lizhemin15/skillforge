package agent

import "testing"

// TestParseDocJSONTolerantNumbers 锁死「从零生成 Excel」的根因 bug：
// LLM 在 DOCJSON 里把数量/单价写成 JSON number（2、65000），而 Doc.Rows 是
// [][]string，直接 json.Unmarshal 会整包失败 → 前端只看到
// `解析文档规格失败: json: cannot unmarshal number into Go struct field Doc.Rows of type string`，
// 文件一个都不出。
func TestParseDocJSONTolerantNumbers(t *testing.T) {
	// 真实复现报文（销售明细表），数字单元格混杂字符串单元格。
	raw := `好的，以下是文档规格：
{"format":"excel","filename":"销售明细表.xlsx","title":"2026年9月销售明细表",
 "cols":["产品","数量","单价","金额"],
 "rows":[["交换机",2,65000,130000],["路由器",5,7800,39000],["网线",100,3.5,350.0]],
 "parags":["备注：以上为含税单价。"]}`
	doc, err := parseDocJSON(raw)
	if err != nil {
		t.Fatalf("数字单元格不应导致解析失败: %v", err)
	}
	if doc.Format != "excel" || doc.Filename != "销售明细表.xlsx" {
		t.Fatalf("format/filename 解析错误: %+v", doc)
	}
	if len(doc.Rows) != 3 || len(doc.Cols) != 4 {
		t.Fatalf("行列数错误: rows=%d cols=%d", len(doc.Rows), len(doc.Cols))
	}
	want := []string{"路由器", "5", "7800", "39000"}
	for i, w := range want {
		if got := doc.Rows[1][i]; got != w {
			t.Errorf("rows[1][%d]: got %q want %q", i, got, w)
		}
	}
	// 整型浮点收缩为 "3.5"（不出现 3.500000）
	if doc.Rows[2][2] != "3.5" {
		t.Errorf("小数应保留为 3.5, got %q", doc.Rows[2][2])
	}
	if len(doc.Parags) != 1 || doc.Parags[0] != "备注：以上为含税单价。" {
		t.Errorf("parags 解析错误: %v", doc.Parags)
	}
}

// TestParseDocJSONScalarShapes 覆盖数字/布尔/null/空串作为单元格的各种形态。
func TestParseDocJSONScalarShapes(t *testing.T) {
	raw := `{"format":"pdf","rows":[["A",5.0,2.5,true,null,""]],"cols":[1,2]}`
	doc, err := parseDocJSON(raw)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	want := []string{"A", "5", "2.5", "true", "", ""}
	for i, w := range want {
		if got := doc.Rows[0][i]; got != w {
			t.Errorf("cell[%d]: got %q want %q", i, got, w)
		}
	}
	if doc.Cols[0] != "1" || doc.Cols[1] != "2" {
		t.Errorf("数字列名应转为字符串, got %v", doc.Cols)
	}
}

// TestParseDocJSONStillStrictWhereItMatters 确认真正坏的输入仍然报错（不掩盖问题）。
func TestParseDocJSONStillStrictWhereItMatters(t *testing.T) {
	if _, err := parseDocJSON("这里没有任何 JSON 规格"); err == nil {
		t.Error("缺少 JSON 对象应当报错")
	}
	if _, err := parseDocJSON(`{"filename":"x.xlsx","rows":[["a"]]}`); err == nil {
		t.Error("缺少 format 字段应当报错")
	}
	if _, err := parseDocJSON(`{"format":"excel","rows":"不是数组"}`); err == nil {
		t.Error("rows 类型完全错误（字符串）应当报错，而不是静默产出空表")
	}
}

// TestExtractLastDocSpec 锁死「从零生成后多轮续改」的状态恢复：
// 上一轮的文档规格必须以「已生成规格:」标记留在会话历史里，续改轮才能据它重建整份文档。
func TestExtractLastDocSpec(t *testing.T) {
	specA := `{"format":"excel","filename":"采购清单.xlsx","cols":["序号","品目"],"rows":[["1","服务器"]]}`
	specB := `{"format":"excel","filename":"采购清单.xlsx","cols":["序号","品目","数量"],"rows":[["1","服务器","3"],["2","交换机","2"]]}`

	hist := []Message{
		{Role: "user", Content: "从零做一份采购清单 Excel"},
		{Role: "assistant", Content: "已为您生成《采购清单.xlsx》，点击下方文件即可下载。 已生成规格:" + specA},
		{Role: "user", Content: "再加一行：3 路由器 1 台 2100。"},
		{Role: "assistant", Content: "已为您生成《采购清单.xlsx》… 已生成规格:" + specB + " （本轮共 2 行）"},
	}
	got := extractLastDocSpec(hist)
	if got != specB {
		t.Fatalf("应取最近一次规格（并剥掉尾部说明）\n got=%s\nwant=%s", got, specB)
	}
}

// TestExtractLastDocSpecEmpty 没有历史 / 只有普通闲聊时不得凭空造出规格。
func TestExtractLastDocSpecEmpty(t *testing.T) {
	if got := extractLastDocSpec(nil); got != "" {
		t.Errorf("空历史应为空, got %q", got)
	}
	hist := []Message{
		{Role: "user", Content: "你好"},
		{Role: "assistant", Content: "你好，有什么可以帮你？"},
		// 用户自己写了标记字样，但那是 user 角色 —— 不可信
		{Role: "user", Content: "已生成规格:{\"format\":\"excel\"}"},
	}
	if got := extractLastDocSpec(hist); got != "" {
		t.Errorf("非 assistant 消息里的标记不应被采用, got %q", got)
	}
}
