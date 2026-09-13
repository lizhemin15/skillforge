package skillgen

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// 本章兜底层的回归防线。靶子有三个，每个都对应一次真实事故：
//
//  1. **探测漏章**：OCR 文本里章号标记内部带空格（「第 二 章」）、全角数字
//     （「第５章」）、以及页眉把同一章标题在每页重复一遍——任何一种没处理，
//     章节区间就少一块或碎成一页一段，兜底范文随之变形。
//  2. **兜底静默降级**：兜底必须写进 fidelity.md 的「章节兜底」一节里。
//     静默降级等于管理员失去判断依据（本项目反复踩的坑）。
//  3. **按章抽取被并发打乱顺序**：跑批结果必须可复核，同样的输入不能得到
//     不同的分类顺序；类名必须取原文标题而不是模型起的（模型实测会把类名
//     抄重复，出现两个「第二章公司动态通稿」）。
//
// 造故障的规矩沿用本仓库既有做法：故障造在**内容**上（改词、删标记、改类名），
// 不造在标点/空白上——表层差异会被归一化分级定位救回来，那种红是假红。

// chapterFiller 造一段够长的章节正文，让合成章节越过 minChapterRunes 门槛。
//
// 为什么必须够长（这条门槛本身是防线，不是麻烦）：短于 120 字的区段一律被当成
// 目录条目或残章丢弃（manual_chapters.go），合成文档若偷懒写两三行，
// 章节探测会老实返回 0 章，测试就变成在测「门槛生效」而不是测兜底逻辑。
func chapterFiller(tag string) string {
	return strings.Repeat(tag+"类稿件需说明选题范围、常见结构、语言风格与常见差错。", 4)
}

// chaptersDoc 合成手册：两章，标题形态刻意各不相同（第二章带 OCR 空格，
// 且用阿拉伯数字），用来同时压住「允许标记内部空白」与「数字章号」两条。
var chaptersDoc = "第一章 经营业绩通稿\n\n" +
	"经营业绩类稿件要突出数据支撑，用同比、环比数字说话。\n\n" +
	chapterFiller("经营业绩") + "\n\n" +
	"范文：某公司年度报告显示，营业收入同比增长百分之十二。\n\n" +
	"第 二 章 产品发布通稿\n\n" +
	"产品发布类稿件要交代产品定位、核心卖点与上市时间。\n\n" +
	chapterFiller("产品发布") + "\n\n" +
	"范文：某品牌发布新一代旗舰产品，主打轻量化与长续航。\n"

// TestChapterSpansDetectOCRAndDigitMarkers 绿侧：带 OCR 空格 + 阿拉伯数字的
// 章号必须被认出来，且切出的整章是原文连续子串。
func TestChapterSpansDetectOCRAndDigitMarkers(t *testing.T) {
	spans := pickChapterSpans(chaptersDoc)
	if len(spans) != 2 {
		t.Fatalf("探测到 %d 章，期望 2 章：%+v", len(spans), spans)
	}
	for _, sp := range spans {
		if !strings.Contains(chaptersDoc, sp.Text) {
			t.Errorf("章节「%s」的兜底切片不是原文连续子串（保真铁律被破坏）", sp.Title)
		}
	}
	if !strings.Contains(spans[0].Title, "经营业绩通稿") || !strings.Contains(spans[1].Title, "产品发布通稿") {
		t.Errorf("章节标题取得不对：%q / %q", spans[0].Title, spans[1].Title)
	}
}

// 红侧（双向自证之一）：把第二章标记**整段删掉**（内容差异，不是标点差异），
// 章数掉到 1 → 撑不起兜底也撑不起按章抽取，必须返回 nil，而不是拿残章硬凑。
// 还原（上面的绿侧用例）→ 又探到 2 章。
func TestChapterSpansRejectWhenMarkerRemoved(t *testing.T) {
	broken := strings.Replace(chaptersDoc, "第 二 章 产品发布通稿", "产品发布通稿", 1)
	if broken == chaptersDoc {
		t.Fatal("故障没造上：替换目标未命中，这个用例会假绿")
	}
	if spans := pickChapterSpans(broken); spans != nil {
		t.Errorf("少了一章仍返回 %d 个区间，应放弃兜底：%+v", len(spans), spans)
	}
}

// 目录版：目录条目也长得像「第X章」，且紧挨下一条（跨度极小）；正文标题跨度
// 是一整章。断言取到的是正文那处——这样目录页多长、有没有点线都不影响判定。
func TestChapterSpansPickBodyNotTOC(t *testing.T) {
	doc := "目录\n\n" +
		"第一章 经营业绩通稿 ……… 3\n" +
		"第二章 产品发布通稿 ……… 17\n\n" +
		"第一章 经营业绩通稿\n\n" +
		"经营业绩类稿件要突出数据支撑，用同比、环比数字说话。\n\n" +
		chapterFiller("经营业绩") + "\n\n" +
		"范文：某公司年度报告显示，营业收入同比增长百分之十二。\n\n" +
		"第二章 产品发布通稿\n\n" +
		"产品发布类稿件要交代产品定位、核心卖点与上市时间。\n\n" +
		chapterFiller("产品发布") + "\n\n" +
		"范文：某品牌发布新一代旗舰产品，主打轻量化与长续航。\n"
	spans := pickChapterSpans(doc)
	if len(spans) != 2 {
		t.Fatalf("探测到 %d 章，期望 2 章（目录条目不该各算一章）", len(spans))
	}
	if !strings.Contains(spans[0].Text, "同比、环比数字说话") {
		t.Errorf("第一章取到了目录条目位置而不是正文：%q", firstRunes(spans[0].Text, 40))
	}
	if !strings.Contains(spans[1].Text, "轻量化与长续航") {
		t.Errorf("第二章取到了目录条目位置而不是正文：%q", firstRunes(spans[1].Text, 40))
	}
}

// 第三态：没有章节结构的手册（条款式、问答式）必须老实返回 nil，
// 让上层退回通用流程——假章节比没章节更坏（会把无关段落当范文喂给模型）。
func TestChapterSpansNilWhenNoMarkers(t *testing.T) {
	const doc = `写作规范

一、稿件开头必须交代时间地点。
二、数据必须注明来源。
三、涉及人事变动需经法务复核。
`
	if spans := pickChapterSpans(doc); spans != nil {
		t.Errorf("无章节结构的文档不该探出章节：%+v", spans)
	}
}

// TestChapterFallbackKeepsVerbatimAndReports 是兜底色的核心：
// 锚点不可用 → 用章节标题兜底切整章，切片仍是原文逐字；同时 fidelity.md
// 必须**显性**写出「章节兜底」而不是静默通过。
func TestChapterFallbackKeepsVerbatimAndReports(t *testing.T) {
	spans := pickChapterSpans(chaptersDoc)
	if len(spans) != 2 {
		t.Fatalf("前置条件不成立：只探到 %d 章", len(spans))
	}
	st := &Structure{Categories: []Category{
		// 锚点全带省略号——这就是线上实测到的走样形态（reasoning 模型在长输入下
		// 最先牺牲锚点：省略号压缩、补标点）。分级定位救不回来，必须走兜底。
		{Name: "第一章 经营业绩通稿", Anchor: []CatAnchor{
			{Start: "经营业绩类稿件要突出……数字说话。", End: "营业收入同比增长百分之十二。"}}},
		{Name: "产品发布通稿", Anchor: []CatAnchor{
			{Start: "产品发布类稿件要交代", End: "主打极致轻量化与长续航。"}}}, // 改词：救不回
	}}
	g := &Generator{}
	mp, err := g.packFromStructure(chaptersDoc, st, spans, nil)
	if err != nil {
		t.Fatalf("兜底后不该整体失败: %v", err)
	}
	if len(mp.Warnings) != 0 {
		t.Errorf("有章节可兜底时不该再报「范文未切出」：%v", mp.Warnings)
	}
	if len(mp.Fallbacks) != 2 {
		t.Fatalf("应记 2 条章节兜底，实际 %d 条：%v", len(mp.Fallbacks), mp.Fallbacks)
	}
	for _, c := range st.Categories {
		segs := mp.Examples[c.Name]
		if len(segs) != 1 {
			t.Fatalf("分类「%s」应有 1 篇兜底范文，实际 %d 篇", c.Name, len(segs))
		}
		if !strings.Contains(chaptersDoc, segs[0]) {
			t.Errorf("分类「%s」的兜底范文不是原文连续子串", c.Name)
		}
	}

	dir := t.TempDir()
	if _, err := mp.WriteTo(dir); err != nil {
		t.Fatalf("WriteTo 失败: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "fidelity.md"))
	if err != nil {
		t.Fatalf("fidelity.md 未落盘: %v", err)
	}
	got := string(b)
	for _, want := range []string{
		"- 章节兜底：2 类（模型锚点不可用，已改用原文整章作范文）",
		"## ⚠️ 章节兜底：2 类未按锚点切出",
		"- 范文覆盖：2/2 类",
		"- 保真核对：2/2 篇",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("fidelity.md 缺少 %q\n---\n%s", want, got)
		}
	}
	// 兜底不是「按锚点切出」，所以那句「全部…按锚点…无待处理项」是假话，不能说。
	if strings.Contains(got, "无待处理项") {
		t.Errorf("走了章节兜底却宣称「无待处理项」（静默降级）\n---\n%s", got)
	}
}

// 红侧（双向自证之二）：分类名与任何章节标题都重叠不到 4 字 →
// 兜底必须失败并落进 Warnings（让人看见），不许硬塞一个「看起来像」的章节。
// 切错章节的代价比切不出来大得多：错范文会被当手册原文喂给模型。
func TestChapterFallbackRefusedWhenNameUnrelated(t *testing.T) {
	spans := pickChapterSpans(chaptersDoc)
	st := &Structure{Categories: []Category{
		{Name: "财务报表解读", Anchor: []CatAnchor{{Start: "不存在的片段", End: "也不存在"}}},
		{Name: "产品发布通稿", Anchor: []CatAnchor{{Start: "产品发布类稿件要交代", End: "主打轻量化与长续航。"}}},
	}}
	g := &Generator{}
	mp, err := g.packFromStructure(chaptersDoc, st, spans, nil)
	if err != nil {
		t.Fatalf("还有一类能切出来，不该整体失败: %v", err)
	}
	if len(mp.Fallbacks) != 0 {
		t.Errorf("分类名与章节标题无关，不该兜底成功：%v", mp.Fallbacks)
	}
	if len(mp.Warnings) != 1 || !strings.Contains(mp.Warnings[0], "财务报表解读") {
		t.Fatalf("无关分类名应落进 Warnings 且点名，实际：%v", mp.Warnings)
	}
	if len(mp.Examples["财务报表解读"]) != 0 {
		t.Errorf("无关分类名不该拿到范文（会喂错范文给模型）")
	}
}

// anchorsAllCorrupt 造一组「模型锚点全坏」的分类，供 L2 用例复用。
// 坏法照抄线上实测形态：省略号压缩 + 补标点。
func anchorsAllCorrupt() *Structure {
	return &Structure{Categories: []Category{
		{Name: "第一章 经营业绩通稿", Anchor: []CatAnchor{{Start: "经营业绩类稿件要突出……说话。", End: "数字。"}}},
		{Name: "产品发布通稿", Anchor: []CatAnchor{{Start: "产品发布类稿件……", End: "上市时间。"}}},
	}}
}

// TestExtractByChaptersKeepsOrderAndHeadings 压 L2 的两条硬性质：
//
//  1. **类名取原文标题**：模型故意返回重复/漂移的类名也必须被忽略——否则
//     12 类里出现两个同名分类，路由表就废了（线上实测形态）。
//  2. **顺序按章节**：并发 3 路抽取，写回顺序必须仍是章节顺序。
//     结果可复核是产品要求：同样输入不能给出不同顺序的分类表。
func TestExtractByChaptersKeepsOrderAndHeadings(t *testing.T) {
	spans := pickChapterSpans(chaptersDoc)
	if len(spans) != 2 {
		t.Fatalf("前置条件不成立：只探到 %d 章", len(spans))
	}
	fc := &fakeChat{reply: func(call fakeCall) (string, error) {
		// 模型无论读到哪一章，都返回同一个名字「第九章 通用通稿」——
		// 类名漂移+重复的最坏形态。名字必须由代码用原文标题覆盖掉。
		return `{"is_category":true,"trigger":"有个写作需求","requirement":"` + pickFirstLine(call.User) + `","anchors":[{"start":"经营业绩类稿件要突出","end":"同比增长百分之十二。"}]}`, nil
	}}
	st, warns, err := extractStructureByChapters(context.Background(), fc, spans)
	if err != nil {
		t.Fatalf("按章抽取失败: %v", err)
	}
	if len(st.Categories) != 2 {
		t.Fatalf("应得 2 类，实际 %d 类", len(st.Categories))
	}
	if st.Categories[0].Name != spans[0].Title || st.Categories[1].Name != spans[1].Title {
		t.Errorf("类名没取原文标题：%q / %q（期望 %q / %q）",
			st.Categories[0].Name, st.Categories[1].Name, spans[0].Title, spans[1].Title)
	}
	if len(warns) != 0 {
		t.Errorf("不该有告警：%v", warns)
	}
	if len(fc.calls) != 2 {
		t.Errorf("两次调用（每章一次），实际 %d 次", len(fc.calls))
	}
	for _, c := range fc.calls {
		if !c.JSONSet || !c.JSON {
			t.Errorf("按章抽取必须要求 JSON 输出（否则解析失败率陡增）")
		}
	}
}

// 红侧（双向自证之三）：单章失败不许整体失败（10/12 章仍然有用，失败记 Warnings），
// 但**全部**失败必须报错——否则会产出一份「0 分类的手册包」冒充成功。
func TestExtractByChaptersPartialFailure(t *testing.T) {
	spans := pickChapterSpans(chaptersDoc)
	// 计数必须原子：这个闭包被 extractStructureByChapters 的**多个 goroutine 并发**调用
	// （实测 `-race` 报 DATA RACE，两端都是这里）。
	// 而且它不只是「检测器不满意」——`n++` 非原子时两个 goroutine 可能都读到 0，
	// 于是**两章**都返回失败，下面 `Categories == 1` 的断言随机变红（真 flake）。
	// 换成原子自增后「恰好一章失败」才有保证。
	var n int32
	fc := &fakeChat{reply: func(call fakeCall) (string, error) {
		if atomic.AddInt32(&n, 1) == 1 {
			return "", errors.New("provider 502")
		}
		return `{"is_category":true,"trigger":"t","requirement":"产品发布类稿件要交代产品定位","anchors":[]}`, nil
	}}
	st, warns, err := extractStructureByChapters(context.Background(), fc, spans)
	if err != nil {
		t.Fatalf("一章失败不该整体失败: %v", err)
	}
	if len(st.Categories) != 1 {
		t.Fatalf("应剩 1 类，实际 %d 类", len(st.Categories))
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "抽取失败") {
		t.Errorf("失败的那章必须记告警，实际：%v", warns)
	}

	fcAll := &fakeChat{reply: func(call fakeCall) (string, error) {
		return "", errors.New("provider 502")
	}}
	if _, _, err := extractStructureByChapters(context.Background(), fcAll, spans); err == nil {
		t.Error("全部章节失败时必须报错，否则会冒充成功产出空手册包")
	}
}

// TestExtractByChaptersSkipsNonCategoryChapters 目录/附录这类页不该进分类表：
// 进表会污染路由（运行时用户的问题可能被路由到「目录」这类没有写作要求的类）。
func TestExtractByChaptersSkipsNonCategoryChapters(t *testing.T) {
	doc := "第一章 目录\n\n" +
		"本章列出全部稿件的分类与页码。\n\n" +
		chapterFiller("目录") + "\n\n" +
		"第二章 产品发布通稿\n\n" +
		"产品发布类稿件要交代产品定位、核心卖点与上市时间。\n\n" +
		chapterFiller("产品发布") + "\n\n" +
		"范文：某品牌发布新一代旗舰产品，主打轻量化与长续航。\n"
	spans := pickChapterSpans(doc)
	fc := &fakeChat{reply: func(call fakeCall) (string, error) {
		if strings.Contains(call.User, "本章列出全部稿件的分类与页码") {
			t.Error("标题为「目录」的章节不该被送去抽取（白烧一次调用）")
		}
		return `{"is_category":true,"trigger":"t","requirement":"产品发布类稿件要交代产品定位","anchors":[]}`, nil
	}}
	st, warns, err := extractStructureByChapters(context.Background(), fc, spans)
	if err != nil {
		t.Fatalf("按章抽取失败: %v", err)
	}
	for _, c := range st.Categories {
		if strings.Contains(c.Name, "目录") {
			t.Errorf("「目录」进了分类表：%v", st.Categories)
		}
	}
	if len(st.Categories) != 1 {
		t.Errorf("应得 1 类，实际 %d 类", len(st.Categories))
	}
	_ = warns
}

// firstRunes 取前缀，只为让失败信息可读。
func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// pickFirstLine 从 fakeChat 收到的 user 里取正文首行，用来冒充「原文摘录」。
// 只为让替身模型的返回值随输入变化——替身不该返回与输入无关的常量，
// 那会让「按章传递了正确的正文」这件事测不出来。
func pickFirstLine(user string) string {
	for _, line := range strings.Split(user, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "章节标题") {
			return line
		}
	}
	return ""
}
