package skillgen

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 本文件是「手册章节」的确定性探测层：给定手册全文，纯代码（零模型参与）
// 找出每一章在原文里的字节区间。它同时服务两条降级路径：
//
//	L1 锚点兜底：模型给的锚点不可用时，用章节区间当范文（manual.go: packFromStructure）；
//	L2 按章抽取：全文一次抽取不可靠时，按章分片重新抽取（manual_repair.go）。
//
// 为什么这一层必须存在（实测事故，不是假想）：
// 手册 PDF 解析出的全文约 6 万字，一次性丢给 reasoning 模型做「找分类 + 摘要求 +
// 给锚点 + 定位范文」四件事时，思考 token 会吃掉输出预算的大头（实测一次调用
// completion 10853 token 里 9782 是 reasoning），可见输出被挤到 1K 字左右，
// 模型随即偷工：先砍锚点、再把类名抄重复，最极端时只抽出 0 个分类。后果有两个：
//   - 0 分类 → 整个手册链路被跳过（Step 8.5 裁判根本不走），退化成通用技能；
//   - 分类抽出来了、但部分锚点被写成带「…」的省略版 → SplitByAnchors 定位失败 →
//     Step 8.5 的确定性硬校验（「标了锚点却一篇没切出」）永久否决，模型分 100 也救不回。
//
// 而「第几章、标题是什么」这种结构信息**不需要模型来认**——它在原文里就是一个
// 正则能认出的东西。交给代码认：模型只做它擅长的摘录，结构由确定性代码兜住。

// 章节标记的两级正则。先认「第X章」（绝大多数中文手册的主结构），认不出足够的章
// 再退到「第X部分/篇/单元」。
//
// 章号允许阿拉伯数字与中文数字，标记内部允许空白——OCR 出来的文本里
// 「第 二 章」「第5 章」这类带空格的写法很常见（实测同一本手册的扫描版与电子版
// 一个带空格、一个不带），不归一化就会漏章。
//
// 刻意**不含**「第X节」：节是章内部更细的层级，把它当章节边界会把一章切碎，
// 兜底切出来的范文就不是「一章一篇」而是「一节一篇」了。
var chapterMarkerRes = []*regexp.Regexp{
	regexp.MustCompile(`第\s*[0-9０-９一二三四五六七八九十百零〇两]{1,6}\s*章`),
	regexp.MustCompile(`第\s*[0-9０-９一二三四五六七八九十百零〇两]{1,6}\s*(?:部分|篇|单元)`),
}

// chapterMarkerRe 是用于「从字符串里剥掉章号」的合并正则。
var chapterMarkerRe = regexp.MustCompile(`第\s*[0-9０-９一二三四五六七八九十百零〇两]{1,6}\s*(?:章|部分|篇|单元)`)

const (
	// maxHeadingRunes 是「标题行」的长度上限。超过它的行里带章号，多半是正文
	// 里提到「见第五章」这类引用，不是标题；拿它当章节起点会切出一堆假区间。
	maxHeadingRunes = 60
	// minChapterRunes 是章节正文的长度下限。目录条目后面紧跟着下一个目录条目，
	// 跨度只有几十字，靠这条门槛把它们筛掉；顺带也筛掉「第X章」出现在末尾
	// 却没有正文的残章。
	minChapterRunes = 120
)

// chapterHeading 是原文里一处章节标题。
type chapterHeading struct {
	// Marker 是归一化后的章号标记（如「第五章」，已去掉内部空白）；
	// 同一个 Marker 出现多次是常态（目录条目 + 正文标题 + 每页页眉），
	// 归一化后才能判定「这两处说的是同一章」。
	Marker string
	// Title 是标题整行（已 trim），保留原文形态，可直接当分类名。
	Title string
	// Start/End 是标题行在原文里的字节区间（End 不含行尾换行）。
	Start, End int
}

// chapterSpan 是一章的正文区间，是 L1 兜底与 L2 分章共用的最小单位。
type chapterSpan struct {
	Marker string
	Title  string
	// Start/End 是正文区间的字节范围（含标题行，End 已去掉尾部空白）；
	// Text 恒等于 src[Start:End]，因此永远是原文的连续子串（保真不破）。
	Start, End int
	Text       string
}

// findChapterHeadings 扫出给定正则认到的所有章节标题。
func findChapterHeadings(src string, re *regexp.Regexp) []chapterHeading {
	locs := re.FindAllStringIndex(src, -1)
	out := make([]chapterHeading, 0, len(locs))
	for _, loc := range locs {
		ls := strings.LastIndexByte(src[:loc[0]], '\n') + 1
		// 标题必须独占行首：行首到章号之间只允许空白。这一条同时挡住
		// 「详见第五章」这种行内引用（前面还有别的字）。
		if strings.TrimSpace(src[ls:loc[0]]) != "" {
			continue
		}
		le := len(src)
		if i := strings.IndexByte(src[loc[1]:], '\n'); i >= 0 {
			le = loc[1] + i
		}
		title := strings.TrimSpace(src[ls:le])
		if utf8.RuneCountInString(title) > maxHeadingRunes {
			continue
		}
		out = append(out, chapterHeading{
			Marker: normalizeChapterMarker(src[loc[0]:loc[1]]),
			Title:  title,
			Start:  ls,
			End:    le,
		})
	}
	return out
}

// normalizeChapterMarker 把章号标记归一化成可比较的形式：去掉所有空白
// （含全角空格），并把全角数字折成半角（「第５章」与「第5章」是同一章）。
func normalizeChapterMarker(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsSpace(r) {
			continue
		}
		if r >= '０' && r <= '９' {
			r = '0' + (r - '０')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// pickChapterSpans 探出手册的章节区间；探不出（或不足门槛）时返回 nil。
//
// 门槛用 manualMinCategories：分出来的章少于「算手册」的分类门槛时，这些区间
// 既撑不起按章抽取、也不值得当兜底，直接放弃比返回一堆假章节更安全。
func pickChapterSpans(src string) []chapterSpan {
	for _, re := range chapterMarkerRes {
		if spans := buildChapterSpans(src, re); len(spans) >= manualMinCategories {
			return spans
		}
	}
	return nil
}

// buildChapterSpans 把标题位置整理成互不重叠的章节正文区间。
func buildChapterSpans(src string, re *regexp.Regexp) []chapterSpan {
	hs := findChapterHeadings(src, re)
	if len(hs) == 0 {
		return nil
	}
	// 到「下一个不同章号」的距离。同名章号不算边界：页眉会把同一章的标题
	// 在每一页重复一遍，若把同名出现当边界，一章会被切成一页一段。
	spanToNext := func(i int) int {
		for j := i + 1; j < len(hs); j++ {
			if hs[j].Marker != hs[i].Marker {
				return hs[j].Start - hs[i].Start
			}
		}
		return len(src) - hs[i].Start
	}
	// 同一个章号取「跨度最大」的那处：目录里的条目紧挨着下一条目录（跨度极小），
	// 正文标题那处跨度是一整章，所以最大跨度必然落在正文上——这样不需要
	// 单独写一套「目录识别」启发式，目录页多长、有没有点线都不影响判定。
	bestAt := map[string]int{}
	for i := range hs {
		m := hs[i].Marker
		cur, ok := bestAt[m]
		if !ok || spanToNext(i) > spanToNext(cur) {
			bestAt[m] = i
		}
	}
	picked := make([]int, 0, len(bestAt))
	for _, i := range bestAt {
		picked = append(picked, i)
	}
	sort.Ints(picked)

	out := make([]chapterSpan, 0, len(picked))
	prevEnd := -1
	for _, i := range picked {
		start := hs[i].Start
		end := len(src)
		for j := i + 1; j < len(hs); j++ {
			if hs[j].Marker != hs[i].Marker {
				end = hs[j].Start
				break
			}
		}
		text := strings.TrimRight(src[start:end], " \t\r\n\u3000")
		end = start + len(text) // 尾部空白不进范文；切片仍是原文连续子串
		if start < prevEnd {
			continue // 与上一章重叠（页眉/重排导致的乱序），丢掉更安全
		}
		if utf8.RuneCountInString(text) < minChapterRunes {
			continue // 太短：不是正文（多半是目录条目或残章）
		}
		out = append(out, chapterSpan{
			Marker: hs[i].Marker,
			Title:  hs[i].Title,
			Start:  start,
			End:    end,
			Text:   text,
		})
		prevEnd = end
	}
	return out
}

// foldChapterTitle 把章节标题折成可比较的形式：剥掉章号标记，去掉空白与中英标点。
// 分类名与标题两边都折一遍再比，才能吃掉「第五章 公司动态通稿」vs「公司动态通稿：」
// 这类修饰差异。折叠只用于**比较**，落到磁盘上的名字永远是原文标题。
func foldChapterTitle(s string) string {
	s = chapterMarkerRe.ReplaceAllString(s, "")
	var b strings.Builder
	for _, r := range s {
		if unicode.IsSpace(r) || strings.ContainsRune(punctCutset, r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// titleOverlap 给两个折叠后的标题打分：完全相等最高，互相包含次之，
// 再退到公共前缀。返回的分数是「重叠的字符数」，供调用方设门槛。
func titleOverlap(a, b string) int {
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return utf8.RuneCountInString(a)
	}
	if strings.Contains(a, b) {
		return utf8.RuneCountInString(b)
	}
	if strings.Contains(b, a) {
		return utf8.RuneCountInString(a)
	}
	n, br := 0, []rune(b)
	for i, r := range []rune(a) {
		if i >= len(br) || br[i] != r {
			break
		}
		n++
	}
	return n
}

// matchChapterForName 给一个分类名找对应的章节。
//
// 两级匹配，从严到宽：
//  1. 章号对齐（最强证据）：「第五章 经营业绩与财报解读新闻稿」里的章号与章节标题
//     的章号**整段相等**才算命中。这里刻意不做子串匹配——「第五章」是「第十五章」
//     的子串，裸 Contains 会把十五当成五（本项目已因这类前缀误命中踩过四次坑）。
//  2. 标题文本对齐：折叠后取重叠最长的那一章，重叠不足 4 字视为没匹配上，
//     宁可兜底失败（记进 Warnings 让人看见）也不硬塞一个「看起来像」的章节——
//     切错章节的代价比切不出来大得多（错范文会被当成手册原文喂给模型）。
func matchChapterForName(spans []chapterSpan, name string) (chapterSpan, bool) {
	if m := normalizeChapterMarker(chapterMarkerRe.FindString(name)); m != "" {
		for _, s := range spans {
			if s.Marker == m {
				return s, true
			}
		}
	}
	folded := foldChapterTitle(name)
	best, bestScore := chapterSpan{}, 0
	for _, s := range spans {
		if sc := titleOverlap(foldChapterTitle(s.Title), folded); sc > bestScore {
			best, bestScore = s, sc
		}
	}
	if bestScore >= 4 {
		return best, true
	}
	return chapterSpan{}, false
}

// chapterFallbackText 在「模型给的锚点不可用」时给出该分类的兜底范文。
//
// 兜底片段是**整章原文切片**（章节标题 → 下一章标题），因此：
//   - 它一定是原文连续子串，保真核对（fidelity.md 的逐字核对）照样通过；
//   - 但它不是手册里精确的那一篇范文，可能混入该章的写法说明——所以调用方
//     必须把这个降级动作记进 Fallbacks（fidelity.md 里显性列出），不能静默。
//
// 返回的 why 是给人看的说明（含命中章节与字数），直接进 fidelity.md。
func chapterFallbackText(spans []chapterSpan, name string) (text, why string, ok bool) {
	sp, ok := matchChapterForName(spans, name)
	if !ok {
		return "", "", false
	}
	n := utf8.RuneCountInString(sp.Text)
	why = "已按原文章节标题兜底切出"
	if sp.Title != "" {
		why += "（" + sp.Title + "）"
	}
	why += "，" + strconv.Itoa(n) + " 字；此为整章切片，非手册精确范文"
	return sp.Text, why, true
}
