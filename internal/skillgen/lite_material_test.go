package skillgen

import (
	"strings"
	"testing"
)

// 素材自解析（SplitLiteMaterial）的回归。
//
// 背景：用户原话「不用分写作指南和范文，这些可能都是混在一个文档里面的……技能创建的
// 时候自行解析」。所以「一份材料切成指南 + 范文」这件事没有任何人力校验兜底了：
// 切错的后果不是报错，而是技能里少一半硬约束、或者范文里混进要求条款，用户看不出来。
// 这些用例钉的就是「切错的代价不对称」这条设计：
//   - 显式标记（`## 范文`）必须被尊重；
//   - 靠猜测切必须过分类器复核，复核不过宁可退回整篇判型；
//   - 每篇范文必须仍是材料原文的子串（保真声明的前提）。
//
// 反例（假对照）提醒：断言不能只写「不报错」。上面每条都断言了切出来的**内容边界**，
// 否则一个「整篇当指南」的实现也能让 len(examples)==0 的用例绿。

const liteTestGuide = `# 公司新闻通稿写作要求

一、受众与定位
面向行业客户与内部员工，语气客观中立，避免营销腔。

二、结构
标题 + 导语 + 正文三段 + 结尾一句话。导语不超过 80 字。

三、语言要求
必须使用第三人称，不得出现「我们」。全篇 600-900 字，禁止使用感叹号。
`

const liteTestArticleA = `# 某某集团年度技术峰会在杭州举行

某某集团年度技术峰会日前在杭州举行。来自全国的两百余名工程师参加了本次会议。

会上，集团技术负责人介绍了新一代数据平台的架构设计。与会者围绕平台稳定性展开了讨论。

本次峰会由集团技术委员会主办。
`

const liteTestArticleB = `# 城市轨道三期规划获批

城市轨道三期规划日前获得批复。三期规划共包含四条线路，总里程约六十公里。

相关部门表示，三期工程将分三个阶段推进，首段预计明年开工。
`

// 规则 1：用户写了显式标记，必须在此处切开，且标记行本身不落进范文。
func TestSplitLiteMaterial_ExplicitMarker(t *testing.T) {
	doc := liteTestGuide + "\n## 范文\n\n" + liteTestArticleA + "\n---\n" + liteTestArticleB
	guide, ex, how := SplitLiteMaterial(doc)

	if !strings.Contains(guide, "三、语言要求") {
		t.Errorf("指南被截断，丢了尾部要求：%q", guide)
	}
	if strings.Contains(guide, "某某集团年度技术峰会") {
		t.Error("范文正文漏进指南（指南里出现了范文的句子）")
	}
	if len(ex) != 2 {
		t.Fatalf("想要 2 篇范文，得到 %d 篇：%#v", len(ex), ex)
	}
	for i, e := range ex {
		if strings.Contains(e, "三、语言要求") {
			t.Errorf("第 %d 篇范文里混进了指南条款", i+1)
		}
		if strings.Contains(e, "## 范文") && !strings.Contains(e, "范文 ") {
			t.Errorf("第 %d 篇范文里残留了分节标记行", i+1)
		}
	}
	if !strings.Contains(how, "显式") {
		t.Errorf("how 没有说明切法，界面上就看不到判读依据了：%q", how)
	}
}

// 规则 1'：带标题的分节标记（`## 范文一：xxx`）既当分节线，标题本身也要保住。
func TestSplitLiteMaterial_MarkerWithTitle(t *testing.T) {
	doc := liteTestGuide + "\n## 范文一：某某集团年度技术峰会在杭州举行\n\n" + liteTestArticleA +
		"\n## 范文二：城市轨道三期规划获批\n\n" + liteTestArticleB
	_, ex, _ := SplitLiteMaterial(doc)
	if len(ex) != 2 {
		t.Fatalf("想要 2 篇范文，得到 %d 篇：%#v", len(ex), ex)
	}
	if !strings.Contains(ex[0], "范文一") {
		t.Errorf("带标题的标记行应当保留（范文标题是原文的一部分）：%q", ex[0])
	}
}

// 规则 2：没有显式标记，靠一行 `---` 分开——必须先用分类器复核两侧像不像。
func TestSplitLiteMaterial_DividerHeuristic(t *testing.T) {
	doc := liteTestGuide + "\n---\n" + liteTestArticleA + "\n---\n" + liteTestArticleB
	guide, ex, how := SplitLiteMaterial(doc)
	if !strings.Contains(guide, "三、语言要求") {
		t.Fatalf("指南切错：%q", guide)
	}
	if len(ex) != 2 {
		t.Fatalf("想要 2 篇范文，得到 %d 篇：%#v", len(ex), ex)
	}
	if strings.Contains(how, "显式") {
		t.Errorf("这一份没有显式标记，how 不该说「显式」：%q", how)
	}
}

// 规则 3a：整份就是一份指南（用户另外传范文文件）→ 指南=全文，范文为空，
// 由素材硬门去报「缺范文」。这里绝不能把指南判成范文。
func TestSplitLiteMaterial_GuideOnly(t *testing.T) {
	guide, ex, _ := SplitLiteMaterial(liteTestGuide)
	if !strings.Contains(guide, "三、语言要求") {
		t.Fatalf("整篇指南应当整篇当指南：%q", guide)
	}
	if len(ex) != 0 {
		t.Fatalf("指南文档不该被切出范文：%#v", ex)
	}
}

// 规则 3b：一个文件一篇范文（最常见的上传形态）→ 范文=全文，指南为空。
// 这条是「不再分两个输入框」之后新增的关键行为：没有它，用户拖进来的范文文件
// 会被当成指南，然后硬门报「缺范文」——明明范文就在手上。
func TestSplitLiteMaterial_ArticleOnly(t *testing.T) {
	guide, ex, _ := SplitLiteMaterial(liteTestArticleA)
	if guide != "" {
		t.Errorf("范文文件不该被判成指南：%q", guide)
	}
	if len(ex) != 1 || !strings.Contains(ex[0], "某某集团年度技术峰会") {
		t.Fatalf("想要整篇当一篇范文，得到 %#v", ex)
	}
}

// 错切防护：指南自己的小节叫「范文的写法 / 范文使用说明」时不能被当成范文区分节线
// ——那会把指南从中间劈开，后半截的硬约束静默消失（最坏的一种错误）。
func TestSplitLiteMaterial_GuideSectionNamedFanwen(t *testing.T) {
	doc := `# 公文写作要求

一、总体要求
必须使用第三人称，不得出现口语词。

二、范文使用说明
本节讲过的东西仅供参考，篇幅控制在 800 字以内。

三、格式要求
标题居中，结尾不加落款。
`
	guide, ex, _ := SplitLiteMaterial(doc)
	if !strings.Contains(guide, "三、格式要求") {
		t.Errorf("指南被「范文使用说明」这个小节劈开了，尾部要求丢失：%q", guide)
	}
	if len(ex) != 0 {
		t.Errorf("指南不该被切出范文：%#v", ex)
	}
}

// CRLF：上传件也要折行（一个文件里三篇范文，Windows 写的 .md 很常见）。
// 不折的话 `---\r` 匹配不上，三篇被当成一篇 —— 静默的数据损失。
func TestCollectLiteMaterial_CRLFFiles(t *testing.T) {
	mat, err := collectLiteMaterial(&LiteInput{}, []*UploadedFile{
		{Filename: "指南.md", Content: strings.ReplaceAll(liteTestGuide, "\n", "\r\n")},
		{Filename: "范文.md", Content: strings.ReplaceAll(liteTestArticleA+"\n---\n"+liteTestArticleB, "\n", "\r\n")},
	})
	if err != nil {
		t.Fatalf("取材失败：%v", err)
	}
	if !strings.Contains(mat.Guide, "三、语言要求") {
		t.Errorf("指南切错：%q", mat.Guide)
	}
	if len(mat.Examples) != 2 {
		t.Fatalf("CRLF 下范文没分篇：想要 2 篇，得到 %d 篇 %#v", len(mat.Examples), mat.Examples)
	}
	for i, e := range mat.Examples {
		if strings.ContainsAny(e, "\r") {
			t.Errorf("第 %d 篇残留 \\r", i+1)
		}
	}
}

// 多份材料：一份指南文件 + 两个范文文件。指南只能取一份，范文要全收。
func TestCollectLiteMaterial_OneGuideManyArticles(t *testing.T) {
	mat, err := collectLiteMaterial(&LiteInput{}, []*UploadedFile{
		{Filename: "新闻通稿写作指南.md", Content: liteTestGuide},
		{Filename: "01.md", Content: liteTestArticleA},
		{Filename: "02.md", Content: liteTestArticleB},
	})
	if err != nil {
		t.Fatalf("取材失败：%v", err)
	}
	if mat.GuideFrom != "新闻通稿写作指南.md" {
		t.Errorf("指南应当取自指南文件，实际取自 %q", mat.GuideFrom)
	}
	if len(mat.Examples) != 2 {
		t.Fatalf("想要 2 篇范文，得到 %d 篇 %#v", len(mat.Examples), mat.Examples)
	}
	if mat.Warn != "" {
		t.Errorf("只有一份指南，不该有「多份指南」告警：%q", mat.Warn)
	}
}

// 一份材料里全都有（用户的主要用法）：指南 + 范文混在一份文档里。
func TestCollectLiteMaterial_SingleMixedDoc(t *testing.T) {
	doc := liteTestGuide + "\n## 范文\n\n" + liteTestArticleA + "\n---\n" + liteTestArticleB
	mat, err := collectLiteMaterial(&LiteInput{Material: doc}, nil)
	if err != nil {
		t.Fatalf("取材失败：%v", err)
	}
	if len(mat.Examples) != 2 {
		t.Fatalf("想要 2 篇范文，得到 %d 篇 %#v", len(mat.Examples), mat.Examples)
	}
	if !strings.Contains(mat.Guide, "三、语言要求") {
		t.Errorf("指南切错：%q", mat.Guide)
	}
	if mat.GuideFrom != "粘贴的素材" {
		t.Errorf("指南来源报错：%q", mat.GuideFrom)
	}
}

// 两份都像指南时：只取一份（不合并），而且必须**说出来**——另外那份被当范文处理
// 这件事不能静默发生。
func TestCollectLiteMaterial_TwoGuidesWarns(t *testing.T) {
	other := strings.ReplaceAll(liteTestGuide, "600-900 字", "1200 字")
	mat, err := collectLiteMaterial(&LiteInput{}, []*UploadedFile{
		{Filename: "a-指南.md", Content: liteTestGuide},
		{Filename: "b-指南.md", Content: other},
		{Filename: "c-范文.md", Content: liteTestArticleA},
	})
	if err != nil {
		t.Fatalf("取材失败：%v", err)
	}
	if mat.Warn == "" {
		t.Fatal("有两份指南时必须给出告警，否则用户不知道哪份生效了")
	}
	if !strings.Contains(mat.Warn, "b-指南.md") {
		t.Errorf("告警里要点名落选的那份：%q", mat.Warn)
	}
	// 落选的那份不能进范文：一份要求清单当 few-shot 喂进去，模型学出来的就是
	// 「一、总体要求……」这种腔调。它只该出现在告警里，让用户自己删。
	if len(mat.Examples) != 1 {
		t.Fatalf("只有 c-范文.md 该成为范文，实际 %d 篇 %#v", len(mat.Examples), mat.Examples)
	}
	if strings.Contains(strings.Join(mat.Examples, "\n"), "三、语言要求") {
		t.Error("落选的指南污染了范文（要求条款混进 few-shot）")
	}
}

// 保真前提：切出来的每一篇范文都必须是材料原文的子串（范文是逐字落盘的，
// 期间不允许经过任何改写；一旦这里不成立，界面上「未经模型改写」的声明就是假的）。
func TestSplitLiteMaterial_ExamplesAreVerbatim(t *testing.T) {
	doc := liteTestGuide + "\n## 范文\n\n" + liteTestArticleA + "\n---\n" + liteTestArticleB
	_, ex, _ := SplitLiteMaterial(doc)
	for i, e := range ex {
		if !strings.Contains(doc, e) {
			t.Errorf("第 %d 篇不是原文子串（被改写过了）：%q", i+1, e)
		}
	}
}

// 缺范文时，报错必须是「怎么改」：用户看到的是「没找到范文」，他需要知道
// 加一行 `## 范文` 就能切开，而不是再猜一次。
func TestCollectLiteMaterial_MissingExampleHint(t *testing.T) {
	_, err := collectLiteMaterial(&LiteInput{Material: liteTestGuide}, nil)
	if err == nil {
		t.Fatal("只有指南没有范文时应当报错（硬门）")
	}
	msg := err.Error()
	if !strings.Contains(msg, "范文") || !strings.Contains(msg, "---") {
		t.Errorf("报错没给出可操作的改法：%q", msg)
	}
}

// 指南太短时同理：要么补内容，要么告诉它哪段是指南。
func TestCollectLiteMaterial_MissingGuideHint(t *testing.T) {
	_, err := collectLiteMaterial(&LiteInput{Material: liteTestArticleA}, nil)
	if err == nil {
		t.Fatal("只有范文没有指南时应当报错（硬门）")
	}
	if !strings.Contains(err.Error(), "指南") {
		t.Errorf("报错没提指南：%q", err.Error())
	}
}

// 空输入：不 panic，一路走到硬门报错。
func TestCollectLiteMaterial_Empty(t *testing.T) {
	if _, err := collectLiteMaterial(&LiteInput{}, nil); err == nil {
		t.Fatal("空素材应当报错")
	}
	if _, err := collectLiteMaterial(&LiteInput{Material: "   \n\n  "}, nil); err == nil {
		t.Fatal("纯空白素材应当报错")
	}
}
