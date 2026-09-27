package docgen

import (
	"regexp"
	"strings"
)

// ================= 公文排版（GB/T 9704-2012 党政机关公文格式）=================
//
// 背景（2026-09-27 用户反馈）：
//
//	「生成 word 的时候没有按公文格式去生成，而是直接把 markdown 放进去了，这很难看」
//
// 线上复现（把上一轮的 markdown 正文直通成 Word，或模型把 markdown 写进 parags）：
//
//	## 一、检查目的与意义         ← 井号原样进了文档
//	本次大检查旨在**压实责任**    ← 星号原样进了文档
//	| 阶段 | 时间 |               ← markdown 表格被当成一行普通文字
//	整篇宋体 12pt、无首行缩进、无行距、无层次字体
//
// 这里补两件事，缺一件用户看到的还是「难看」：
//
//  1. 结构还原：parags 里允许出现 markdown（标题 / 加粗 / 列表 / 表格 / 引用 / 分隔线），
//     渲染成真正的 Word 结构（居中大标题、粗体 run、真实表格），markdown 记号不再
//     出现在正文里。**判定放在 docgen 里而不是调用方**：docgen 是唯一出口，
//     无论正文来自模型 JSON 还是「上一轮正文直通」（doc_passthrough.go 逐行搬
//     markdown 原文），都会经过这里。
//
//  2. 公文版式：标题小标宋二号居中；正文仿宋_GB2312 三号、行距 28 磅、首行缩进 2 字符；
//     一级标题（一、）黑体、二级标题（（一））楷体；页边距按 GB/T 9704-2012
//     （上 3.7 / 下 3.5 / 左 2.8 / 右 2.6 cm）。
//
// 字体名按公文规范给（方正小标宋简体 / 仿宋_GB2312 / 黑体 / 楷体_GB2312）；
// 系统没装这些字体的机器由 Word/WPS 自行替换，字号与层次仍能看出公文形状。
const (
	fontTitle   = "方正小标宋简体"   // 公文标题（二号）
	fontBody    = "仿宋_GB2312" // 正文（三号）
	fontH1      = "黑体"        // 一级标题（三号）
	fontH2      = "楷体_GB2312" // 二级标题（三号）
	fontMono    = "Consolas"  // 行内代码的拉丁部分
	sizeTitle   = 44          // 二号 = 22pt
	sizeBody    = 32          // 三号 = 16pt
	lineSpacing = 560         // 固定行距 28 磅（twips 的二十分之一磅）
	firstLineIn = 640         // 首行缩进 2 字符 = 2 × 16pt = 32pt = 640
	// 页边距（twip，1cm = 566.93）：上 3.7 / 下 3.5 / 左 2.8 / 右 2.6 cm
	marTop    = "2098"
	marBottom = "1984"
	marLeft   = "1587"
	marRight  = "1474"
)

// mdRun 是一段带内联样式的文字。
type mdRun struct {
	Text   string
	Bold   bool
	Italic bool
	Strike bool
	Code   bool
}

// 内联 markdown：**粗** / __粗__ / *斜* / _斜_ / ~~删~~ / `代码` / [文字](链接) / ![图](链接)
var (
	reBold      = regexp.MustCompile(`\*\*(.+?)\*\*|__(.+?)__`)
	reItalic    = regexp.MustCompile(`\*(.+?)\*|_(.+?)_`)
	reStrike    = regexp.MustCompile(`~~(.+?)~~`)
	reCode      = regexp.MustCompile("`([^`]+)`")
	reLink      = regexp.MustCompile(`!?\[([^\]]*)\]\(([^)]*)\)`)
	reImgOnly   = regexp.MustCompile(`^!\[([^\]]*)\]\(([^)]*)\)$`)
	reHeadingMD = regexp.MustCompile(`^(#{1,6})\s*(.*)$`)
	reH1CN      = regexp.MustCompile(`^[一二三四五六七八九十百]+\s*、`)
	reH2CN      = regexp.MustCompile(`^[（(]\s*[一二三四五六七八九十百]+\s*[）)]`)
	reH3Num     = regexp.MustCompile(`^\d+\s*[.、]\s*`)
	reListItem  = regexp.MustCompile(`^[-*+]\s+(.*)$`)
	reOrdered   = regexp.MustCompile(`^(\d+)\s*[.)]\s+(.*)$`)
	reQuote     = regexp.MustCompile(`^>\s?(.*)$`)
	reHRule     = regexp.MustCompile(`^(?:-{3,}|\*{3,}|_{3,})$`)
	reTitlePref = regexp.MustCompile(`^#*\s*标题[：:]\s*(.*)$`)
	reDateLine  = regexp.MustCompile(`^\d{4}\s*年\s*\d{1,2}\s*月\s*\d{1,2}\s*日$`)
	reSignUnit  = regexp.MustCompile(`(公司|集团|委员会|办公室|管理局|管理局|局|厅|处|科|中心|大学|学院|医院|银行|部|署|院)$`)
)

// mdInlineRuns 把一行文字切成若干带样式的 run，并摘掉内联 markdown 记号。
//
// 顺序有讲究：先取代码（反引号里的星号是普通字符），再取图片/链接、粗体、删除线、
// 斜体。每一步都把已处理的部分从文本里剪掉，避免 **粗** 里的 * 被当成斜体再来一遍。
func mdInlineRuns(line string) []mdRun {
	var runs []mdRun
	rest := line

	// 图片：只保留 alt（Word 里没有图片可放，链接本身对公文是噪音）
	if m := reImgOnly.FindStringSubmatch(strings.TrimSpace(rest)); m != nil {
		return []mdRun{{Text: strings.TrimSpace(m[1])}}
	}

	emit := func(s string, bold, italic, strike, code bool) {
		if s == "" {
			return
		}
		runs = append(runs, mdRun{Text: s, Bold: bold, Italic: italic, Strike: strike, Code: code})
	}

	type span struct {
		start, end int
		kind       int // 0 code 1 link 2 bold 3 strike 4 italic
		inner      string
	}
	for {
		var best *span
		consider := func(re *regexp.Regexp, kind int) {
			loc := re.FindStringSubmatchIndex(rest)
			if loc == nil {
				return
			}
			if best == nil || loc[0] < best.start {
				inner := ""
				if kind == 1 { // link：文字在 group 1
					if loc[2] >= 0 {
						inner = rest[loc[2]:loc[3]]
					}
				} else {
					for g := 2; g+1 < len(loc); g += 2 {
						if loc[g] >= 0 {
							inner = rest[loc[g]:loc[g+1]]
							break
						}
					}
				}
				best = &span{start: loc[0], end: loc[1], kind: kind, inner: inner}
			}
		}
		consider(reCode, 0)
		consider(reLink, 1)
		consider(reBold, 2)
		consider(reStrike, 3)
		consider(reItalic, 4)
		if best == nil {
			emit(rest, false, false, false, false)
			break
		}
		emit(rest[:best.start], false, false, false, false)
		switch best.kind {
		case 0:
			emit(best.inner, false, false, false, true)
		case 1:
			emit(best.inner, false, false, false, false)
		case 2:
			emit(best.inner, true, false, false, false)
		case 3:
			emit(best.inner, false, false, true, false)
		case 4:
			emit(best.inner, false, true, false, false)
		}
		rest = rest[best.end:]
	}
	return runs
}

// mdPlainText 返回一行去掉全部 markdown 记号后的纯文本（用于判定标题/落款/去重）。
// 块级记号（# / > / - / 1.）也要摘掉：标题行往往写成 "# 关于开展…的通知"，
// 不摘的话它就跟 Doc.Title 对不上，去重会失效、标题被排两遍。
func mdPlainText(line string) string {
	s := strings.TrimSpace(line)
	if m := reHeadingMD.FindStringSubmatch(s); m != nil {
		s = strings.TrimSpace(m[2])
	} else if m := reQuote.FindStringSubmatch(s); m != nil {
		s = strings.TrimSpace(m[1])
	} else if m := reListItem.FindStringSubmatch(s); m != nil {
		s = strings.TrimSpace(m[1])
	} else if m := reOrdered.FindStringSubmatch(s); m != nil {
		s = strings.TrimSpace(m[2])
	}
	var sb strings.Builder
	for _, r := range mdInlineRuns(s) {
		sb.WriteString(r.Text)
	}
	return strings.TrimSpace(sb.String())
}

// isTableRow 判断一行是否是 markdown 表格行（以 | 开头、以 | 结尾）。
func isTableRow(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, "|") && strings.HasSuffix(t, "|") && len(t) >= 2
}

// isTableSep 判断 markdown 表格的分隔行（|---|---|）。
func isTableSep(s string) bool {
	t := strings.TrimSpace(s)
	if !isTableRow(t) {
		return false
	}
	body := strings.Trim(t, "|")
	for _, cell := range strings.Split(body, "|") {
		c := strings.TrimSpace(cell)
		if c == "" {
			continue
		}
		if strings.Trim(c, ":-") != "" {
			return false
		}
	}
	return true
}

// splitTableRow 把 | a | b | 切成 [a b]。
func splitTableRow(s string) []string {
	t := strings.TrimSpace(s)
	t = strings.TrimPrefix(t, "|")
	t = strings.TrimSuffix(t, "|")
	parts := strings.Split(t, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

// headingKind 返回这一行在公文里的层次：
// 0=正文 1=一级标题（一、）2=二级标题（（一））3=三级标题（1.）4=大标题（#）
func headingKind(line string) int {
	s := strings.TrimSpace(line)
	if m := reHeadingMD.FindStringSubmatch(s); m != nil {
		switch len(m[1]) {
		case 1, 2:
			return 1
		case 3:
			return 2
		default:
			return 3
		}
	}
	switch {
	case reH1CN.MatchString(s):
		return 1
	case reH2CN.MatchString(s):
		return 2
	case reH3Num.MatchString(s):
		return 3
	}
	return 0
}
