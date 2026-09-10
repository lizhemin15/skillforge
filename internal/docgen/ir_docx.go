package docgen

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"baliance.com/gooxml/color"
	"baliance.com/gooxml/document"
	"baliance.com/gooxml/measurement"
	"baliance.com/gooxml/schema/soo/wml"
)

// ================= 文档 IR(中间表示) =================
//
// IR 是"想怎么改就怎么改"的统一接口。它同时支撑两种文档诉求：
//   - Mode=="build"：不依赖模板，从零构造任意样式的文档（Blocks 渲染）。
//   - Mode=="edit" ：加载一个现有 docx 模板，按 Ops 对结构做任意增删改。
//
// AI 侧输出 JSON 化的 IR（见 agent 的 system_prompt），docgen 据此渲染。

// IR 是一次文档生成/编辑的完整指令。
type IR struct {
	Mode     string  `json:"mode"`     // "build" | "edit"
	Filename string  `json:"filename"` // 下载文件名
	Title    string  `json:"title"`    // build 模式主标题（可空）
	Blocks   []Block `json:"blocks"`   // build 模式文档块
	Ops      []Op    `json:"ops"`      // edit 模式编辑操作
	// EditOn   []byte  —— 由 agent 注入，不进 JSON。模板字节。
}

// Block 是从零生成的文档块。
type Block struct {
	Type  string `json:"type"`  // "heading"|"para"|"list"|"table"|"pagebreak"
	Text  string `json:"text"`  // heading/para/list 文本
	Level int    `json:"level"` // heading 1-3
	// 列表子项（type=list 时）
	Items []string `json:"items,omitempty"`
	// 表格内容（type=table 时）
	Cols []string   `json:"cols,omitempty"`
	Rows [][]string `json:"rows,omitempty"`
	// 样式
	Bold   bool    `json:"bold,omitempty"`
	Italic bool    `json:"italic,omitempty"`
	Size   int     `json:"size,omitempty"`  // 磅，0=默认
	Align  string  `json:"align,omitempty"` // "left"|"center"|"right"|"justify"
	Font   string  `json:"font,omitempty"`  // 字体族
	W      float64 `json:"w,omitempty"`     // 表格列宽（cm，可省略）
	Shade  string  `json:"shade,omitempty"` // 表头底色，如 "D9D9D9"
}

// Op 是模板编辑的一次操作。
type Op struct {
	Action string `json:"action"` // "add_row"|"del_row"|"set_cell"|"add_para"|"set_para"|"del_para"
	Table  int    `json:"table"`  // 目标表格索引（从0）
	Row    int    `json:"row"`    // 目标行索引（从0）；add_row 时为插入位置（负数=末尾追加）
	Col    int    `json:"col"`    // 目标列索引（从0）
	Text   string `json:"text"`   // set_cell / set_para / add_para 的新内容
	// Key 用于 set_cell 按占位符寻址：若非空，优先定位文本含 {{Key}} 的单元格再写。
	Key string `json:"key,omitempty"`
	// add_row 时填充本行各列内容（可选；缺则生成空行）
	Values []string `json:"values,omitempty"`
	// del_para 时指明按文本删除段落（优先于 Row）；空则按 Table/Row 走 deletion
	ParagraphText string `json:"paragraph_text,omitempty"`
	// InsertBefore 若为 true，add_row 不做"挪到合计行之前"的智能处理，直接把新行追加到表尾。
	InsertBefore bool `json:"insert_before,omitempty"`
}

// ================= 入口 =================

// RenderIR 渲染一次 IR。若 template 非 nil 且 Mode=="edit"，在其上编辑；否则按 Build 渲染。
// template 为 nil 时强制走 build 模式。
func RenderIR(ir IR, template []byte) ([]byte, error) {
	if ir.Mode == "edit" && len(template) > 0 {
		return renderEditIR(ir, template)
	}
	return renderBuildIR(ir)
}

// renderBuildIR 从零构造一个 docx。
func renderBuildIR(ir IR) ([]byte, error) {
	if ir.Filename == "" {
		ir.Filename = "文档.docx"
	}
	d := document.New()

	// 标题
	if strings.TrimSpace(ir.Title) != "" {
		h := d.AddParagraph()
		h.Properties().SetStyle("Title")
		r := h.AddRun()
		r.AddText(ir.Title)
	}

	blocks := ir.Blocks
	// 若未显式给 blocks 但给了 Title/Cols/Rows/Parags 兼容逻辑（略，直接走 blocks）
	for _, b := range blocks {
		switch b.Type {
		case "heading":
			p := d.AddParagraph()
			switch b.Level {
			case 1:
				p.Properties().SetStyle("Heading1")
			case 2:
				p.Properties().SetStyle("Heading2")
			case 3:
				p.Properties().SetStyle("Heading3")
			default:
				p.Properties().SetStyle("Heading1")
			}
			run := p.AddRun()
			run.AddText(b.Text)
			applyRunStyle(run, b)

		case "para", "p":
			p := d.AddParagraph()
			run := p.AddRun()
			run.AddText(b.Text)
			applyRunStyle(run, b)

		case "list", "ul", "ol":
			// 用 ListParagraph 样式 + 前缀
			for _, item := range b.Items {
				p := d.AddParagraph()
				p.Properties().SetStyle("ListParagraph")
				run := p.AddRun()
				run.AddText("• " + item)
				applyRunStyle(run, b)
			}

		case "table", "t":
			tbl := d.AddTable()
			tbl.Properties().SetStyle("TableGrid")
			// 表头
			if len(b.Cols) > 0 {
				row := makeRowWidths(tbl, len(b.Cols), b)
				fillRowTexts(row, b.Cols)
				// 画表头底色
				shadeRow(row, b.Shade)
			}
			// 表体
			for _, rowData := range b.Rows {
				rowCells := len(rowData)
				if len(b.Cols) > rowCells {
					rowCells = len(b.Cols)
				}
				row := makeRowWidths(tbl, rowCells, b)
				fillRowTexts(row, rowData)
			}

		case "pagebreak", "pb":
			p := d.AddParagraph()
			p.AddRun().AddBreak()
		}
	}

	if len(blocks) == 0 && strings.TrimSpace(ir.Title) != "" {
		// 只有标题的兜底
	}
	var buf bytes.Buffer
	if err := d.Save(&buf); err != nil {
		return nil, fmt.Errorf("docgen build: %w", err)
	}
	return buf.Bytes(), nil
}

// renderEditIR 在 template 上应用 Ops。
func renderEditIR(ir IR, template []byte) ([]byte, error) {
	if len(template) == 0 {
		return renderBuildIR(ir)
	}
	d, err := document.Read(bytes.NewReader(template), int64(len(template)))
	if err != nil {
		return nil, fmt.Errorf("docgen load template: %w", err)
	}

	// 先记录现有表格，供 ops 定位
	tables := d.Tables()

	for _, op := range ir.Ops {
		switch op.Action {
		case "add_row":
			if op.Table >= len(tables) {
				return nil, fmt.Errorf("add_row: table index %d 超出范围(共%d张)", op.Table, len(tables))
			}
			ti := op.Table
			// 引擎兜底：LLM 可能报错表索引，把品目加进了甲乙方/签字区那张只有几行的
			// 信息表。若目标表行数很少(≤2 表头+表头/2行)而另有更高、列数相近的"数据表"
			// (品目明细表通常行数最多)，改投那个表，避免污染信息表。
			if rowCount(tables[ti]) <= 2 {
				best := ti
				bestScore := -1
				for j, tj := range tables {
					if j == ti {
						continue
					}
					rc := rowCount(tj)
					diff := absI(cellCountOf(tj) - len(op.Values))
					if rc > 2 && (diff <= 1 || len(op.Values) == 0) {
						score := rc*10 - diff
						if score > bestScore {
							best, bestScore = j, score
						}
					}
				}
				if bestScore > 0 {
					ti = best
				}
			}
			t := tables[ti]
			newRow := addRowWithCells(t, cellCountOf(t), op)
			if !op.InsertBefore && insertBeforeTotals(t, newRow) {
				// 有合计行：把新行挪到合计行之前（合计行通常在各明细行之后）
			}
			vals := op.Values
			// 序号列补齐：LLM 常漏掉「序号」列（只给 品目/规格/数量/单价 4 个值，
			// 而明细表有 5 列）。若恰好少一列，自动在行首补下一个递增的序号，
			// 让追加行对齐表式（序号 1,2,3…）。
			if len(vals) > 0 && len(vals)+1 == cellCountOf(t) {
				next := nextRowSeq(t)
				vals = append([]string{next}, vals...)
			}
			fillRowTexts(newRow, vals)

		case "del_row":
			if op.Table >= len(tables) {
				return nil, fmt.Errorf("del_row: table index %d 超出范围", op.Table)
			}
			t := tables[op.Table]
			if err := deleteTableRow(t, op.Row); err != nil {
				return nil, err
			}

		case "set_cell":
			if op.Table >= len(tables) {
				return nil, fmt.Errorf("set_cell: table index %d 超出范围", op.Table)
			}
			// 按占位符寻址优先：Text 含 {{key}} 的格，或 key 字段命中
			if op.Key != "" || strings.Contains(op.Text, "{{") {
				if err := setCellByKey(tables[op.Table], op.Key, op.Text); err != nil {
					// 找不到占位符则回退按位置写
					if op.Row < 0 {
						return nil, err
					}
					if err2 := setCellByPos(tables[op.Table], op.Row, op.Col, op.Text); err2 != nil {
						return nil, err
					}
				}
				break
			}
			if err := setCellByPos(tables[op.Table], op.Row, op.Col, op.Text); err != nil {
				return nil, err
			}

		case "add_para":
			p := d.AddParagraph()
			p.AddRun().AddText(op.Text)

		case "set_para":
			// 按段落文本定位（仅支持正文段落文本替换）
			paras := d.Paragraphs()
			done := false
			for _, para := range paras {
				var full string
				for _, r := range para.Runs() {
					full += textOfRun(r)
				}
				if strings.Contains(full, op.ParagraphText) || (op.ParagraphText == "" && op.Row >= 0) {
					// 只替换第一段（近似）；用 op.Row 精确时可对齐
					replaceParaText(para, op.Text)
					done = true
					break
				}
			}
			if !done {
				// 未找到则追加新段
				d.AddParagraph().AddRun().AddText(op.Text)
			}

		case "del_para":
			paras := d.Paragraphs()
			for _, para := range paras {
				var full string
				for _, r := range para.Runs() {
					full += textOfRun(r)
				}
				if op.ParagraphText != "" && strings.Contains(full, op.ParagraphText) {
					d.RemoveParagraph(para)
					break
				}
			}
		}
	}

	var buf bytes.Buffer
	if err := d.Save(&buf); err != nil {
		return nil, fmt.Errorf("docgen edit: %w", err)
	}
	return buf.Bytes(), nil
}

// ================= gooxml helpers =================

func textOfRun(r document.Run) string {
	var sb strings.Builder
	for _, ic := range r.X().EG_RunInnerContent {
		if ic.T != nil {
			sb.WriteString(ic.T.Content)
		}
	}
	return sb.String()
}

func replaceParaText(p document.Paragraph, text string) {
	// 删除所有 run，重新写一个 run
	for _, run := range p.Runs() {
		p.RemoveRun(run)
	}
	p.AddRun().AddText(text)
}

func applyRunStyle(run document.Run, b Block) {
	rp := run.Properties()
	if b.Bold {
		rp.SetBold(true)
	}
	if b.Italic {
		rp.SetItalic(true)
	}
	if b.Size > 0 {
		rp.SetSize(measurement.Distance(b.Size) * measurement.Point)
	}
	if b.Font != "" {
		rp.SetFontFamily(b.Font)
	}
	if b.Align != "" {
		// 段落的对齐，run 不持有；此处由外层段落设置，单独处理
	}
}

// makeRowWidths 创建一个 n 列表格行。
func makeRowWidths(t document.Table, n int, _ Block) document.Row {
	r, _ := addEmptyRow(t, n)
	return r
}

// addEmptyRow 追加 n 列的空行。
func addEmptyRow(t document.Table, n int) (document.Row, error) {
	row := t.AddRow()
	for i := 0; i < n; i++ {
		row.AddCell()
	}
	return row, nil
}

// addRowWithCells 追加一行，列数与现有行对齐。
func addRowWithCells(t document.Table, n int, _ Op) document.Row {
	row := t.AddRow()
	for i := 0; i < n; i++ {
		row.AddCell()
	}
	return row
}

// cellCountOf 返回表格首行的列数（用于对齐新行）。
func cellCountOf(t document.Table) int {
	rows := t.Rows()
	if len(rows) == 0 {
		return 0
	}
	return len(rows[0].Cells())
}

// rowCount 返回表格行数。
func rowCount(t document.Table) int { return len(t.Rows()) }

// absI 返回整数绝对值。
func absI(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// upToTotalsRowIndex 返回首个"合计/总计/大写/小写/金额汇总"行的索引；无则 -1。
func totalsRowIndex(t document.Table) int {
	for i, row := range t.Rows() {
		txt := rowText(row)
		if containsAny(txt, "合计", "总计", "小写", "大写", "金额合计") {
			return i
		}
	}
	return -1
}

// rowText 拼接一行的所有单元格文本。
func rowText(row document.Row) string {
	var sb strings.Builder
	for _, c := range row.Cells() {
		sb.WriteString(cellText(c))
	}
	return sb.String()
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// insertBeforeTotals 若表尾存在"合计/总计"行，把 newRow（当前在表尾）挪到合计行之前。
// 返回是否发生了移动。
func insertBeforeTotals(t document.Table, newRow document.Row) bool {
	list := t.X().EG_ContentRowContent
	if len(list) < 2 {
		return false
	}
	// newRow 由 AddRow() 追加，必然位于切片末尾
	newIdx := len(list) - 1
	// 找首个合计行下标（基于行的展示顺序）
	tot := totalsRowIndex(t)
	if tot < 0 {
		return false
	}
	// 若新增行已在合计行之前则无需移动
	if newIdx <= tot {
		return false
	}
	// 把 newIdx 处的元素移到 tot 处
	item := list[newIdx]
	copy(list[tot+1:newIdx+1], list[tot:newIdx])
	list[tot] = item
	return true
}

func fillRowTexts(row document.Row, texts []string) {
	cells := row.Cells()
	for i, v := range texts {
		if i >= len(cells) {
			break
		}
		setCellText(cells[i], v)
	}
}

// nextRowSeq 计算明细表下一个「序号」：统计首列为纯数字的行数，取最大值+1。
// 表头首列是"序号"，合计行首列通常是空串，都不会被计入。
func nextRowSeq(t document.Table) string {
	maxN := 0
	for _, r := range t.Rows() {
		cs := r.Cells()
		if len(cs) == 0 {
			continue
		}
		first := strings.TrimSpace(cellText(cs[0]))
		if first == "" {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(first, "%d", &n); err == nil {
			if n > maxN {
				maxN = n
			}
		}
	}
	return strconv.Itoa(maxN + 1)
}

func setCellText(cell document.Cell, text string) error {
	ps := cell.Paragraphs()
	if len(ps) == 0 {
		p := cell.AddParagraph()
		p.AddRun().AddText(text)
		return nil
	}
	p := ps[0]
	// 优先改写已有 run 的文本节点（POC 验证过 ic.T.Content 方式有效）
	replaced := false
	for _, run := range p.Runs() {
		for _, ic := range run.X().EG_RunInnerContent {
			if ic.T != nil {
				ic.T.Content = text
				replaced = true
				break
			}
		}
		if replaced {
			break
		}
	}
	if !replaced {
		p.AddRun().AddText(text)
	}
	return nil
}

// deleteTableRow 删除表格第 idx 行。gooxml 每行一个 EG_ContentRowContent。
func deleteTableRow(t document.Table, idx int) error {
	x := t.X()
	list := x.EG_ContentRowContent
	if idx < 0 || idx >= len(list) {
		return fmt.Errorf("del_row: row index %d 超出范围(共%d行)", idx, len(list))
	}
	x.EG_ContentRowContent = append(list[:idx], list[idx+1:]...)
	return nil
}

// setCellByPos 按 (行,列) 写单元格文本。
func setCellByPos(t document.Table, row, col int, text string) error {
	rows := t.Rows()
	if row < 0 || row >= len(rows) {
		return fmt.Errorf("set_cell: row %d 超出范围", row)
	}
	cells := rows[row].Cells()
	if col < 0 || col >= len(cells) {
		return fmt.Errorf("set_cell: col %d 超出范围", col)
	}
	return setCellText(cells[col], text)
}

// setCellByKey 找出文本含 {{key}}（或指定 key）的单元格并写入 text（保留占位符则替换）。
// 优先用 op.Key；若为空则用 text 里的 {{key}} 前缀。
func setCellByKey(t document.Table, key, text string) error {
	// 解析 key：若 key 为空，尝试从 text 提取 {{...}}
	target := key
	if target == "" {
		if i := strings.Index(text, "{{"); i >= 0 {
			if j := strings.Index(text[i:], "}}"); j >= 0 {
				target = text[i+2 : i+j]
			}
		}
	}
	if target == "" {
		return fmt.Errorf("set_cell by key: 缺少 key")
	}
	needle := "{{" + target + "}}"
	for _, row := range t.Rows() {
		for _, cell := range row.Cells() {
			cur := cellText(cell)
			if strings.Contains(cur, needle) {
				// 替换：把占位符换掉 = 清掉整个格文本再写 text
				return setCellText(cell, text)
			}
		}
	}
	return fmt.Errorf("set_cell by key: 未找到占位符 %s", needle)
}

// cellText 读取一个单元格的全部文本。
func cellText(cell document.Cell) string {
	var sb strings.Builder
	for _, p := range cell.Paragraphs() {
		for _, r := range p.Runs() {
			sb.WriteString(textOfRun(r))
		}
	}
	return sb.String()
}

func shadeRow(row document.Row, hex string) {
	if hex == "" {
		return
	}
	fill := color.FromHex(hex)
	for _, c := range row.Cells() {
		cp := c.Properties()
		cp.SetShading(wml.ST_ShdClear, fill, fill)
	}
}
