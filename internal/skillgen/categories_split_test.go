package skillgen

import (
	"strings"
	"testing"
	"unicode"
)

// 合成样本：刻意复刻真实手册里踩到的两个坑——
//  ① PDF 解析出的文本保留物理换行，模型「摘抄」跨行句子时会把它连成一行；
//  ② 模型会顺手把中文标点换成空格、或把全角空格吃掉。
//
// 实测一本 50 页手册的 12 个分类里，有 7 类因为这俩原因整类范文被丢弃。
const sampleDoc = `第一章 公司动态通稿

【标题】示例科技发布年度回顾

【导语】示例科技今日发布年度回顾，全年营收 3.14 亿元，
同比增长 42.7%。

【正文】公司表示将继续投入研发，重点方向包括边缘计算与数据治
理平台。

第二章 产品发布新闻稿

【标题】示例科技发布「星云」平台

【导语】示例科技今日发布「星云」平台，支持离线部署，
已通过国家信息中心测试。

第三章 荣誉新闻稿

【标题】示例科技入选行业百强榜

【导语】示例科技今日入选行业百强榜，位列第 37 位。
`

// normLoose 去掉空白与**本测试关心的**标点，仅用于断言语义，不参与被测代码。
//
// 必须和产品承诺的容差对齐：产品明确允许锚点与原文字有空白/标点差异
// （这正是第三级匹配存在的意义），所以断言语义时也不能死抠标点。
//
// 刻意自带一份标点表、不复用生产代码的 punctCutset：实现若把白名单放宽到
// 吃掉正文内容，测试要能独立发现，而不是跟着一起放水。
func normLoose(s string) string {
	const puncts = "，。【】「」、：；！？"
	var b strings.Builder
	for _, r := range s {
		if unicode.IsSpace(r) || strings.ContainsRune(puncts, r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// checkSegments 是共用断言，两条都与实现无关（重构不会假红）：
//
//	① 保真：每段必须是原文的连续子串——一个字符都不能是模型补的；
//	② 语义：每段覆盖对应锚点区间（以 Start 开头、以 End 结尾）。
func checkSegments(t *testing.T, doc string, anchors []CatAnchor, segs []string) {
	t.Helper()
	if len(segs) != len(anchors) {
		t.Fatalf("切出 %d 段，期望 %d 段", len(segs), len(anchors))
	}
	for i, seg := range segs {
		if !strings.Contains(doc, seg) {
			t.Errorf("第 %d 段不是原文连续子串（保真被破坏）：%q", i+1, seg)
			continue
		}
		if !strings.HasPrefix(normLoose(seg), normLoose(anchors[i].Start)) {
			t.Errorf("第 %d 段未以 Start 锚点开头：\n  片段 = %q\n  锚点 = %q", i+1, seg, anchors[i].Start)
		}
		if !strings.HasSuffix(normLoose(seg), normLoose(anchors[i].End)) {
			t.Errorf("第 %d 段未以 End 锚点结尾：\n  片段 = %q\n  锚点 = %q", i+1, seg, anchors[i].End)
		}
	}
}

// 锚点跨行：模型把原文两行连成一行。这是真实手册失败的主因，必须能切。
func TestSplitByAnchorsAnchorAcrossLines(t *testing.T) {
	anchors := []CatAnchor{{
		Start: "【标题】示例科技发布「星云」平台",
		End:   "示例科技今日发布「星云」平台，支持离线部署，已通过国家信息中心测试。",
	}}
	segs, err := SplitByAnchors(sampleDoc, anchors)
	if err != nil {
		t.Fatalf("跨行锚点应能定位，却报错：%v", err)
	}
	checkSegments(t, sampleDoc, anchors, segs)
	// 保真：切片内部必须保留原文换行，而不是被「修好」成一行。
	if !strings.Contains(segs[0], "\n") {
		t.Errorf("切片应保留原文换行，实际被折叠：%q", segs[0])
	}
}

// 标点差异：锚点把中文逗号写成空格，且原文还跨行——需要最宽一级匹配。
//
// 这里必须用黄金值逐字断言，而不是「忽略标点后前缀/后缀相等」这类宽松检查：
// 后者对「切片漏掉末尾句号」完全不敏感（归一化把句号也删了，两边照样相等），
// 实测过——把 absorbTail 改成恒等返回，宽松断言全绿，一个字都没报。
func TestSplitByAnchorsAnchorPunctDiff(t *testing.T) {
	anchors := []CatAnchor{{
		Start: "【标题】示例科技发布年度回顾",
		End:   "公司表示将继续投入研发 重点方向包括边缘计算与数据治理平台。",
	}}
	segs, err := SplitByAnchors(sampleDoc, anchors)
	if err != nil {
		t.Fatalf("仅标点/换行有差异的锚点应能定位，却报错：%v", err)
	}
	checkSegments(t, sampleDoc, anchors, segs)

	// 黄金值：切片必须逐字等于原文这一段，含末位句号、含原文换行。
	const want = "【标题】示例科技发布年度回顾\n\n" +
		"【导语】示例科技今日发布年度回顾，全年营收 3.14 亿元，\n同比增长 42.7%。\n\n" +
		"【正文】公司表示将继续投入研发，重点方向包括边缘计算与数据治\n理平台。"
	if segs[0] != want {
		t.Errorf("切片边界不正确：\n  实际 = %q\n  期望 = %q", segs[0], want)
	}
}

// 锚点开头有被归一化丢掉的字符（【），且锚点与原文标点不同（、 vs ，）：
// 切片必须把开头的【 一并带上，不能只剩「正文】……」。这条专盯 absorbHead。
func TestSplitByAnchorsBoundaryKeepsLeadingPunct(t *testing.T) {
	anchors := []CatAnchor{{
		Start: "【正文】公司表示将继续投入研发、重点方向包括边缘计算与数据治理平台。",
		End:   "理平台。",
	}}
	segs, err := SplitByAnchors(sampleDoc, anchors)
	if err != nil {
		t.Fatalf("应能定位，却报错：%v", err)
	}
	checkSegments(t, sampleDoc, anchors, segs)

	const want = "【正文】公司表示将继续投入研发，重点方向包括边缘计算与数据治\n理平台。"
	if segs[0] != want {
		t.Errorf("切片起点被吞掉（应保留开头的【）：\n  实际 = %q\n  期望 = %q", segs[0], want)
	}
}

// 数字保真：定位可以放宽（忽略空白/标点），但切片必须逐字取自原文——
// 数字、百分号、小数点一个都不能变。这条是「范文原样」承诺的硬核。
func TestSplitByAnchorsKeepsNumbersIntact(t *testing.T) {
	anchors := []CatAnchor{{
		Start: "【标题】示例科技发布年度回顾",
		End:   "同比增长 42.7%。",
	}}
	segs, err := SplitByAnchors(sampleDoc, anchors)
	if err != nil {
		t.Fatalf("切分失败：%v", err)
	}
	for _, want := range []string{"3.14 亿元", "42.7%"} {
		if !strings.Contains(segs[0], want) {
			t.Errorf("切片丢失原文数字/符号 %q（归一化必须只用于定位，不得改写切片）：\n%q", want, segs[0])
		}
	}
}

// 三级匹配都找不到 → 必须报错。静默降级会切出错位的脏数据，比失败更危险。
func TestSplitByAnchorsNotFoundErrors(t *testing.T) {
	anchors := []CatAnchor{{
		Start: "【标题】绝不存在的标题",
		End:   "也绝不存在的内容。",
	}}
	if _, err := SplitByAnchors(sampleDoc, anchors); err == nil {
		t.Fatal("锚点在原文中不存在时必须报错，实际却成功了——静默降级会产出脏数据")
	}
}

// 锚点片段在原文中出现多次 → 必须报错，不能随便取第一个。
func TestSplitByAnchorsAmbiguousErrors(t *testing.T) {
	const dup = "甲段内容。共同片段A。乙段内容。共同片段A。丙段内容。"
	anchors := []CatAnchor{{Start: "共同片段A。", End: "乙段内容。"}}
	if _, err := SplitByAnchors(dup, anchors); err == nil {
		t.Fatal("锚点在原文中出现多次时必须报错（取第一个会切错位置）")
	}
}

// 锚点顺序颠倒 → 必须报错。
func TestSplitByAnchorsReversedOrderErrors(t *testing.T) {
	anchors := []CatAnchor{
		{Start: "第二章 产品发布新闻稿", End: "【标题】示例科技发布「星云」平台"},
		{Start: "第一章 公司动态通稿", End: "【标题】示例科技发布年度回顾"},
	}
	if _, err := SplitByAnchors(sampleDoc, anchors); err == nil {
		t.Fatal("锚点顺序颠倒时必须报错")
	}
}

// 相邻两段重叠 → 必须报错。
func TestSplitByAnchorsOverlapErrors(t *testing.T) {
	anchors := []CatAnchor{
		{Start: "【标题】示例科技发布年度回顾", End: "【正文】"},
		{Start: "【导语】示例科技今日发布年度回顾", End: "同比增长 42.7%。"},
	}
	if _, err := SplitByAnchors(sampleDoc, anchors); err == nil {
		t.Fatal("两段锚点区间重叠时必须报错")
	}
}

// 空锚点列表 / 空锚点字符串 → 必须报错（不能返回空切片冒充成功）。
func TestSplitByAnchorsEmptyInputErrors(t *testing.T) {
	if _, err := SplitByAnchors(sampleDoc, nil); err == nil {
		t.Error("锚点列表为空时应报错")
	}
	blank := []CatAnchor{{Start: "   ", End: "结尾。"}}
	if _, err := SplitByAnchors(sampleDoc, blank); err == nil {
		t.Error("Start 为空白串时应报错")
	}
}
