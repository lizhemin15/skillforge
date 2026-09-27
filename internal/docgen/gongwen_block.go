package docgen

import "strings"

// ================= 公文「块」中间层 =================
//
// 为什么要有这一层：正文来源只有一份（模型的 parags / 上一轮 markdown 直通），
// 但出口有三份（Word / PDF / PPT）。2026-09-27 用户报的「word 里全是 markdown」
// 只是最先撞上的那个出口 —— PDF（pdf.go）和 PPT（pptx.go）当时同样逐条遍历
// parags，同样会把 #、**、| --- | 原样吐出来。谁在各自的渲染循环里各写一遍
// 判定，谁就会各自漏一遍。
//
// 所以：**结构判定只在这里做一次**，三个渲染器都消费 []block，各自只负责
// 「把这一块画成什么样」。加一条判定（或修一个漏）对所有格式同时生效。

type blockKind int

const (
	blockBody      blockKind = iota // 正文段落（首行缩进 2 字符）
	blockH1                         // 一级标题「一、」（黑体）
	blockH2                         // 二级标题「（一）」（楷体）
	blockH3                         // 三级标题「1.」
	blockRecipient                  // 主送单位（顶格）
	blockSignoff                    // 落款（右对齐）
	blockBullet                     // 无序列表项
	blockOrdered                    // 有序列表项
	blockTable                      // 表格（已拆成行列）
)

// block 是排版单位：文字（含内联样式）+ 结构含义。
type block struct {
	kind blockKind
	runs []mdRun    // 文字块（表格块为空）
	rows [][]string // 仅 blockTable
}

// blockText 返回块去掉样式与记号后的纯文字。
func (b block) text() string {
	var sb strings.Builder
	for _, r := range b.runs {
		sb.WriteString(r.Text)
	}
	return sb.String()
}

// gongwenBlocks 把 parags 摊平成行，再按公文规则归类成块。
//
// title 用来去重：模型经常把标题同时写进 title 字段和第一条 parags，
// 不做这件事，用户看到的是一份标题出现两遍的文档。
func gongwenBlocks(parags []string, title string) []block {
	lines := flattenLines(parags)
	titleNorm := mdPlainText(title)

	// 落款识别：末尾最多两行里，形如单位名或成文日期的行右对齐。
	signFrom := -1
	{
		nonEmpty := make([]int, 0, len(lines))
		for i, l := range lines {
			if strings.TrimSpace(l) != "" {
				nonEmpty = append(nonEmpty, i)
			}
		}
		if n := len(nonEmpty); n >= 2 {
			start := nonEmpty[n-2]
			ok := true
			for _, idx := range nonEmpty[n-2:] {
				t := mdPlainText(lines[idx])
				if !reDateLine.MatchString(t) && !(len([]rune(t)) <= 30 && reSignUnit.MatchString(t)) {
					ok = false
					break
				}
			}
			if ok {
				signFrom = start
			}
		}
	}

	var out []block
	bodyStarted := false
	emit := func(b block) { out = append(out, b) }

	for i := 0; i < len(lines); i++ {
		raw := strings.TrimSpace(lines[i])
		if raw == "" || reHRule.MatchString(raw) {
			continue
		}

		// markdown 表格：连续若干 | 行合成一张真表格
		if isTableRow(raw) {
			rows := make([][]string, 0, 4)
			j := i
			for j < len(lines) && isTableRow(lines[j]) {
				if !isTableSep(lines[j]) {
					rows = append(rows, splitTableRow(lines[j]))
				}
				j++
			}
			emit(block{kind: blockTable, rows: rows})
			bodyStarted = true
			i = j - 1
			continue
		}

		// 引用：去掉 > 记号，按正文排
		if m := reQuote.FindStringSubmatch(raw); m != nil {
			raw = strings.TrimSpace(m[1])
			if raw == "" {
				continue
			}
		}

		text := mdPlainText(raw)

		// 标题去重：与 title 完全一致的行不再重复排一遍；
		// 「标题：xxx」这类行同样是重复信息（文档已经有标题了），整行丢掉。
		if reTitlePref.MatchString(raw) || text == titleNorm || (titleNorm != "" && strings.HasPrefix(titleNorm, text) && len([]rune(text)) >= 8) {
			continue
		}

		// 主送单位：正文开始前、以冒号结尾的短行，顶格排，不缩进。
		if !bodyStarted && strings.HasSuffix(text, "：") && len([]rune(text)) <= 40 && !reH1CN.MatchString(text) {
			emit(block{kind: blockRecipient, runs: mdInlineRuns(raw)})
			continue
		}

		if signFrom >= 0 && i >= signFrom {
			emit(block{kind: blockSignoff, runs: mdInlineRuns(raw)})
			continue
		}

		switch headingKind(raw) {
		case 1:
			emit(block{kind: blockH1, runs: mdInlineRuns(strings.TrimSpace(reHeadingMD.ReplaceAllString(raw, "$2")))})
			bodyStarted = true
			continue
		case 2:
			emit(block{kind: blockH2, runs: mdInlineRuns(strings.TrimSpace(reHeadingMD.ReplaceAllString(raw, "$2")))})
			bodyStarted = true
			continue
		case 3:
			emit(block{kind: blockH3, runs: mdInlineRuns(strings.TrimSpace(reHeadingMD.ReplaceAllString(raw, "$2")))})
			bodyStarted = true
			continue
		}

		// 无序列表：marker 换成圆点
		if m := reListItem.FindStringSubmatch(raw); m != nil {
			emit(block{kind: blockBullet, runs: append([]mdRun{{Text: "• "}}, mdInlineRuns(m[1])...)})
			bodyStarted = true
			continue
		}
		if m := reOrdered.FindStringSubmatch(raw); m != nil {
			emit(block{kind: blockOrdered, runs: append([]mdRun{{Text: m[1] + ". "}}, mdInlineRuns(m[2])...)})
			bodyStarted = true
			continue
		}

		emit(block{kind: blockBody, runs: mdInlineRuns(raw)})
		bodyStarted = true
	}
	return out
}
