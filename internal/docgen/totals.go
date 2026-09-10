package docgen

import (
	"archive/zip"
	"bytes"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// ReviseDocxTotals 对 docx 中带"合计/小写"合计行的表格，自动重新计算合计金额。
// LLM 心算合计经常算错（尤其分多轮追加品目后），引擎在这里兜底：定位每一张含
// 合计行的表格，用它上方所有明细行的「数量 × 单价」累加，把正确结果写回合计行
// 的「小写(数字)」与「大写(中文)」单元格。只改合计行，绝不动其它内容。
// 读取 word/document.xml 原始 XML 完成，不经过 gooxml（gooxml 无法重新读取
// Fill 再压缩出的 settings.xml，会报空 int 属性解析错误）。
func ReviseDocxTotals(src []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(src), int64(len(src)))
	if err != nil {
		return src, nil
	}
	// 定位并读取 document.xml
	var docXML []byte
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			rc, err := f.Open()
			if err != nil {
				return src, nil
			}
			docXML, _ = io.ReadAll(rc)
			rc.Close()
			break
		}
	}
	if len(docXML) == 0 {
		return src, nil
	}
	newXML, totalChanged := reviseDocTotalsXML(docXML)
	if !totalChanged {
		return src, nil
	}
	// 重新打包 zip，仅替换 word/document.xml
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, f := range zr.File {
		w, err := zw.Create(f.Name)
		if err != nil {
			return src, nil
		}
		if f.Name == "word/document.xml" {
			if _, err := w.Write(newXML); err != nil {
				return src, nil
			}
			continue
		}
		rc, err := f.Open()
		if err != nil {
			if _, err := w.Write(nil); err != nil {
				return src, nil
			}
			continue
		}
		if _, err := io.Copy(w, rc); err != nil {
			rc.Close()
			return src, nil
		}
		rc.Close()
	}
	if err := zw.Close(); err != nil {
		return src, nil
	}
	return out.Bytes(), nil
}

// ---------------------------------------------------------------------------
// 轻量表解析：把 document.xml 里的 <w:tbl> 解析成 行列文本 + 原始字节跨度，
// 以便精准替换合计行两个金额单元格的文本。
// ---------------------------------------------------------------------------

type xmlRowIndex struct {
	start, end int // 该 <w:tr> 标签整体（含开闭）在 docXML 中的字节区间
	cells      []*xmlCellIndex
	texts      []string
	isTableRow bool
}

type xmlCellIndex struct {
	start, end int
	text       string
}

// tTextRe 匹配单个 <w:t>（不带子标签名冲突：<w:tcPr> 等含 <w:t 前缀但非文本标签）。
// 要求 <w:t 后要么紧跟 >，要么是空白+属性再 >。
var tTextReStrict = regexp.MustCompile(`(?s)<w:t(?:\s[^>]*)?>(.*?)</w:t>`)

// collectText 提取一个字节区间内的所有 <w:t> 文本（按出现顺序拼接）
func collectText(b []byte, start, end int) string {
	var sb strings.Builder
	region := b[start:end]
	for _, m := range tTextReStrict.FindAllSubmatchIndex(region, -1) {
		sb.WriteString(string(region[m[2]:m[3]]))
	}
	return sb.String()
}

// tokenIndexOfTag 在 b[start:] 中查找首个 </w:tag> 的结束位置（字节绝对偏移）。
func tokenIndexOfTag(b []byte, start int, tag string) int {
	needle := []byte("</w:" + tag + ">")
	idx := bytes.Index(b[start:], needle)
	// 也兼容自闭合 <w:tag .../>
	return start + idx + len(needle)
}

// docTable 一张表的行，及该表 <w:tbl> 相对输入切片开头的偏移（用于换算全局 offset）
type docTable struct {
	Open int // 该表 <w:tbl> 在输入切片中的偏移
	Rows []xmlRowIndex
}

// parseDocTables 解析 docXML 返回所有表格（含其内行/单元格跨度）。
func parseDocTables(docXML []byte) []docTable {
	var tables []docTable
	i := 0
	for {
		// 找下一个 <w:tbl>
		bi := bytes.Index(docXML[i:], []byte("<w:tbl>"))
		if bi < 0 {
			break
		}
		open := i + bi
		// 找对应 </w:tbl>
		ci := bytes.Index(docXML[open:], []byte("</w:tbl>"))
		if ci < 0 {
			break
		}
		tblEnd := open + ci + len("</w:tbl>")
		segment := docXML[open:tblEnd]

		var rows []xmlRowIndex
		cur := 0 // 相对表(segment)开头的游标
		for {
			ri := bytes.Index(segment, []byte("<w:tr>"))
			if ri < 0 {
				break
			}
			rowStart := cur + ri
			ci2 := bytes.Index(segment[ri:], []byte("</w:tr>"))
			if ci2 < 0 {
				break
			}
			rowEnd := rowStart + ci2 + len("</w:tr>")
			rowSeg := segment[ri : ri+ci2+len("</w:tr>")]

			var cells []*xmlCellIndex
			var texts []string
			cseg := rowSeg // 相对本行起点
			ccur := 0
			for {
				ci3 := bytes.Index(cseg, []byte("<w:tc>"))
				if ci3 < 0 {
					break
				}
				cellStart := ccur + ci3
				ci4 := bytes.Index(cseg[ci3:], []byte("</w:tc>"))
				if ci4 < 0 {
					break
				}
				cellEnd := cellStart + ci4 + len("</w:tc>")
				cellSeg := cseg[ci3 : ci3+ci4+len("</w:tc>")]
				text := collectText(cellSeg, 0, len(cellSeg))
				cells = append(cells, &xmlCellIndex{start: cellStart, end: cellEnd, text: text})
				texts = append(texts, text)
				cseg = cseg[ci4+len("</w:tc>"):]
				ccur = cellEnd
			}

			rows = append(rows, xmlRowIndex{
				start: rowStart, end: rowEnd, cells: cells, texts: texts, isTableRow: true,
			})
			segment = segment[ci2+len("</w:tr>"):]
			cur = rowEnd
		}

		tables = append(tables, docTable{Open: open, Rows: rows})
		i = tblEnd
	}
	return tables
}

// reviseDocTotalsXML 在原始 document.xml 上重算所有合计表的合计金额。
func reviseDocTotalsXML(docXML []byte) ([]byte, bool) {
	tables := parseDocTables(docXML)
	if len(tables) == 0 {
		return docXML, false
	}
	changed := false
	// 用一个 (start,end,repl) 列表暂存本表的替换，全部算好后从后往前应用，
	// 避免前面替换导致后续索引偏移。
	type rep struct {
		s, e int
		repl []byte
	}
	var reps []rep
	apply := func() {
		for i := len(reps) - 1; i >= 0; i-- {
			docXML = replaceBytesRegion(docXML, reps[i].s, reps[i].e, reps[i].repl)
		}
		reps = reps[:0]
	}

	for _, tbl := range tables {
		tblRows := tbl.Rows
		if len(tblRows) < 3 {
			continue
		}

		// 定位合计行（首个含"合计"或"大写+小写"的行）
		totalIdx := -1
		for i := 1; i < len(tblRows); i++ {
			s := strings.Join(tblRows[i].texts, "|")
			if strings.Contains(s, "合计") || (strings.Contains(s, "大写") && strings.Contains(s, "小写")) {
				totalIdx = i
				break
			}
		}
		if totalIdx <= 1 {
			continue
		}

		// 找数量列 & 单价列：统计明细行各列是否为数值列
		dataRows := tblRows[:totalIdx]
		if len(dataRows) == 0 {
			continue
		}
		nCols := 0
		for _, r := range dataRows {
			if len(r.texts) > nCols {
				nCols = len(r.texts)
			}
		}
		if nCols < 3 {
			continue
		}
		numCount := make([]int, nCols)
		for _, r := range dataRows {
			for c := 0; c < nCols; c++ {
				txt := ""
				if c < len(r.texts) {
					txt = r.texts[c]
				}
				if txt == "" {
					continue
				}
				if _, ok := parseNum(txt); ok {
					numCount[c]++
				}
			}
		}
		amountCol := -1 // 单价 = 最右的数值列
		for c := nCols - 1; c >= 0; c-- {
			if numCount[c] > 0 {
				amountCol = c
				break
			}
		}
		qtyCol := -1 // 数量 = amountCol 左侧最近数值列
		for c := amountCol - 1; c >= 0; c-- {
			if numCount[c] > 0 {
				qtyCol = c
				break
			}
		}
		if amountCol < 0 {
			continue
		}
		var sum float64
		for _, r := range dataRows {
			amount := 0.0
			if amountCol < len(r.texts) {
				if v, ok := parseNum(r.texts[amountCol]); ok {
					amount = v
				}
			}
			qty := 1.0
			if qtyCol >= 0 && qtyCol < len(r.texts) {
				if v, ok := parseNum(r.texts[qtyCol]); ok && v > 0 {
					qty = v
				}
			}
			sum += amount * qty
		}
		intSum := int(math.Round(sum))
		numStr := strconv.Itoa(intSum)
		upStr := cnUppercaseMoney(intSum)

		// 用「内容定位」而非行偏移：合计行必然包含「合计（大写）」与「小写」标签，
		// 在该行文本区内直接找对应标签后的下一个 <w:t>…</w:t> 金额单元格。
		// 行文本区 = texts 对应的原始 XML 段（用 totalRow 的文本而不是不靠谱的行偏移）。
		rowText := strings.Join(tblRows[totalIdx].texts, "\x00")
		if !strings.Contains(rowText, "合计（大写）") && !strings.Contains(rowText, "小写") {
			continue
		}
		// 直接全文档：找「合计（大写）」后第一个 <w:t> 的文本作为大写金额（严格匹配 <w:t>…</w:t>）
		upperLabelAt := bytes.Index(docXML, []byte("合计（大写）</w:t>"))
		if upperLabelAt < 0 {
			upperLabelAt = bytes.Index(docXML, []byte("合计（大写）"))
		}
		var upperRepl, lowerRepl []byte = nil, nil
		if upperLabelAt >= 0 {
			if m := tTextReStrict.FindSubmatchIndex(docXML[upperLabelAt+len("合计（大写）"):]); m != nil && len(m) >= 4 {
				s := upperLabelAt + len("合计（大写）") + m[2]
				e := upperLabelAt + len("合计（大写）") + m[3]
				upperRepl = docXML[s:e]
				reps = append(reps, rep{s, e, []byte(upStr)})
			}
		}
		lowerLabelAt := bytes.Index(docXML, []byte("小写：</w:t>"))
		if lowerLabelAt < 0 {
			lowerLabelAt = bytes.Index(docXML, []byte("小写："))
		}
		if lowerLabelAt >= 0 {
			if m := tTextReStrict.FindSubmatchIndex(docXML[lowerLabelAt+len("小写："):]); m != nil && len(m) >= 4 {
				s := lowerLabelAt + len("小写：") + m[2]
				e := lowerLabelAt + len("小写：") + m[3]
				lowerRepl = docXML[s:e]
				reps = append(reps, rep{s, e, []byte(numStr)})
			}
		}
		if upperRepl == nil && lowerRepl == nil {
			continue
		}
		changed = true
	}
	apply()
	return docXML, changed
}

// replaceBytesRegion 替换 b[s:e] 区间为 repl（长度可不一致）。
func replaceBytesRegion(b []byte, s, e int, repl []byte) []byte {
	var out []byte
	out = append(out, b[:s]...)
	out = append(out, repl...)
	out = append(out, b[e:]...)
	return out
}

// parseNum 解析一个文本中的数字（去掉可能的千分位逗号、单位）。
func parseNum(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	// 去掉逗号/全角逗号
	s = strings.ReplaceAll(s, ",", "")
	s = strings.ReplaceAll(s, "，", "")
	s = strings.ReplaceAll(s, " ", "")
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// cnUppercaseMoney 把整数金额转成中文大写（元），如 960500 -> 玖拾陆万零伍佰元整。
func cnUppercaseMoney(n int) string {
	if n == 0 {
		return "零元整"
	}
	digits := []string{"零", "壹", "贰", "叁", "肆", "伍", "陆", "柒", "捌", "玖"}
	units := []string{"仟", "佰", "拾", ""} // units[i] 对应千/百/十/个位
	bigUnits := []string{"", "万", "亿"}

	// 按4位一段切分，secs[0]为最高段
	var secs []int
	for nn := n; nn > 0; nn /= 10000 {
		secs = append([]int{nn % 10000}, secs...)
	}
	var b strings.Builder
	for idx, sec := range secs {
		// 段首补零：仅当本段最高位(千位)空缺、被跳过时补"零"。
		// 例如 100050 -> 万段10/个段50，个段<1000 需补零；85000 -> 个段5000 不必补。
		if idx > 0 && sec > 0 && b.Len() > 0 && sec < 1000 && !strings.HasSuffix(b.String(), "零") {
			b.WriteString("零")
		}
		pos := []int{sec / 1000, (sec / 100) % 10, (sec / 10) % 10, sec % 10}
		segStarted := false
		for i, d := range pos {
			if d == 0 {
				continue
			}
			// 段内补零：仅当本段内更高位已输出过非零数字、且其间有空位（如 1005 -> 壹仟零伍）
			if i > 0 && segStarted && pos[i-1] == 0 && !strings.HasSuffix(b.String(), "零") {
				b.WriteString("零")
			}
			b.WriteString(digits[d])
			b.WriteString(units[i])
			segStarted = true
		}
		b.WriteString(bigUnits[len(secs)-1-idx])
	}
	return b.String() + "元整"
}
