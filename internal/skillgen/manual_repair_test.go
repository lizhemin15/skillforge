package skillgen

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

// L2「按章抽取」的回归防线。这里守的是三条容易被后人「优化掉」的性质：
//
//  1. 非类别页只做**整段相等**判定，不做前缀/子串——「目录类稿件的写法」是正经
//     类别，前缀判定会把它误杀（本项目已因裸子串/前缀误命中踩过四次）。
//  2. 单章失败不整体失败，但**全部**失败必须报错，不许产出一份空分类手册包冒充成功。
//  3. 截断、跳过、失败三种「信息损失」都必须留下告警：静默降级等于管理员失去判断依据。
//
// 替身模型用 fakeChat（judge_test.go 已定义），不依赖网络。

func TestShouldSkipChapterExactMatchOnly(t *testing.T) {
	// 绿侧：标题就是非类别页 → 跳过（省一次调用）。
	for _, title := range []string{"目录", "第一章 目录", "附录", "第七章 参考文献"} {
		if !shouldSkipChapter(title) {
			t.Errorf("「%s」应判为非类别页", title)
		}
	}
	// 红侧（前缀/子串误命中的反例）：这些是正经类别，跳过了就等于整类丢失。
	for _, title := range []string{"目录类稿件的写法", "附录类说明的写作规范", "前言稿的撰写要点"} {
		if shouldSkipChapter(title) {
			t.Errorf("「%s」被误判为非类别页（裸子串/前缀误命中）", title)
		}
	}
}

// TestExtractOneChapterTruncatesWithWarning 压「超长章节截断」这条降级路径：
// 截断可以，但必须记 note，让 fidelity/告警里看得见。
func TestExtractOneChapterTruncatesWithWarning(t *testing.T) {
	long := strings.Repeat("经营业绩类稿件要突出数据支撑。", 2000) // 远超人上限
	if r := []rune(long); len(r) <= maxChapterPromptRunes {
		t.Fatalf("前置条件不成立：造出来的正文只有 %d 字", len(r))
	}
	fc := &fakeChat{reply: func(call fakeCall) (string, error) {
		return `{"is_category":true,"trigger":"t","requirement":"经营业绩类稿件要突出数据支撑。","anchors":[]}`, nil
	}}
	cat, note, err := extractOneChapter(context.Background(), fc, chapterSpan{Title: "第一章 经营业绩通稿", Text: long})
	if err != nil {
		t.Fatalf("截断后应正常抽取: %v", err)
	}
	if cat == nil {
		t.Fatal("应抽出一个分类")
	}
	if note == "" || !strings.Contains(note, "超过单章上限") {
		t.Errorf("截断必须留告警，实际 note=%q", note)
	}
	if !strings.Contains(fc.calls[0].User, "第一章 经营业绩通稿") {
		t.Error("章节标题必须随正文一起喂给模型（否则模型认不出这是哪一类）")
	}
}

// 红侧：模型显式说「这章不是稿件类别」→ 跳过，但**不算失败**（不算失败才不会
// 让整章失败计数虚高、也不该在跑批里报错）。
func TestExtractOneChapterNonCategoryIsNotFailure(t *testing.T) {
	fc := &fakeChat{reply: func(call fakeCall) (string, error) {
		return `{"is_category":false,"trigger":"","requirement":"","anchors":[]}`, nil
	}}
	cat, note, err := extractOneChapter(context.Background(), fc, chapterSpan{Title: "第一章 前言", Text: "本篇说明手册的使用方法。"})
	if err != nil {
		t.Fatalf("显式非类别不该报错: %v", err)
	}
	if cat != nil {
		t.Errorf("显式非类别不该产出分类: %+v", cat)
	}
	if note != "" {
		t.Errorf("显式非类别不该产生告警（会淹没真失败）: %q", note)
	}
}

// 老/小模型漏 is_category 字段是常态：缺席按 true 处理，只有**显式 false** 才跳过。
// 把漏字段当 false 会让整章被静默误杀——那正是「手册跑完 0 分类」的成因之一。
func TestExtractOneChapterMissingIsCategoryTreatedAsTrue(t *testing.T) {
	fc := &fakeChat{reply: func(call fakeCall) (string, error) {
		return `{"trigger":"t","requirement":"产品发布类稿件要交代产品定位","anchors":[]}`, nil
	}}
	cat, _, err := extractOneChapter(context.Background(), fc, chapterSpan{Title: "第二章 产品发布通稿", Text: "产品发布类稿件要交代产品定位、核心卖点与上市时间。"})
	if err != nil {
		t.Fatalf("漏 is_category 不该当失败: %v", err)
	}
	if cat == nil || cat.Name != "第二章 产品发布通稿" {
		t.Fatalf("漏 is_category 应按类别处理且类名取原文标题，实际 %+v", cat)
	}
}

// 红侧：要求与锚点都空 → 报失败，不许产出一个会进路由表的空分类。
func TestExtractOneChapterEmptyResultIsFailure(t *testing.T) {
	fc := &fakeChat{reply: func(call fakeCall) (string, error) {
		return `{"is_category":true,"trigger":"t","requirement":"","anchors":[]}`, nil
	}}
	cat, _, err := extractOneChapter(context.Background(), fc, chapterSpan{Title: "第三章 会议纪要", Text: "会议纪要类稿件要交代时间地点与会人。"})
	if err == nil {
		t.Fatalf("空分类必须报失败，实际返回 cat=%+v", cat)
	}
	if cat != nil {
		t.Errorf("失败时不该同时返回分类")
	}
}

// 模型偶尔带 Markdown 围栏返回：解析要容忍，否则每次都白烧一次重试。
func TestExtractOneChapterToleratesFencedJSON(t *testing.T) {
	fc := &fakeChat{reply: func(call fakeCall) (string, error) {
		return "```json\n{\"is_category\":true,\"trigger\":\"t\",\"requirement\":\"会议纪要类稿件要交代时间地点与会人。\",\"anchors\":[]}\n```", nil
	}}
	cat, _, err := extractOneChapter(context.Background(), fc, chapterSpan{Title: "第三章 会议纪要", Text: "会议纪要类稿件要交代时间地点与会人。"})
	if err != nil {
		t.Fatalf("带围栏的 JSON 应能解析: %v", err)
	}
	if cat == nil {
		t.Fatal("应抽出分类")
	}
}

// TestExtractByChaptersNamesNeverDuplicate 是「类名取原文标题」的直接红线。
//
// 断言刻意做成**整段相等**而不是「不重复」：最初写的是「两个类名互不相同」，
// 注入「类名 = 原文标题 + 模型给的 trigger」这个故障后测试**仍然通过**——
// 带上前缀后名字照样唯一，假绿。类名必须逐字等于章节标题：既不许掺模型文本，
// 也不许重复（重复会让路由表出现两个同名分类，线上实测形态）。
func TestExtractByChaptersNamesNeverDuplicate(t *testing.T) {
	spans := pickChapterSpans(chaptersDoc)
	if len(spans) != 2 {
		t.Fatalf("前置条件不成立：只探到 %d 章", len(spans))
	}
	fc := &fakeChat{reply: func(call fakeCall) (string, error) {
		// 模型两章都返回同一个 trigger——类名若掺了模型文本，这里就是最坏形态。
		return `{"is_category":true,"trigger":"通用通稿","requirement":"经营业绩类稿件要突出数据支撑。","anchors":[]}`, nil
	}}
	st, _, err := extractStructureByChapters(context.Background(), fc, spans)
	if err != nil {
		t.Fatalf("按章抽取失败: %v", err)
	}
	if len(st.Categories) != 2 {
		t.Fatalf("应得 2 类，实际 %d 类", len(st.Categories))
	}
	want := []string{spans[0].Title, spans[1].Title}
	for i, c := range st.Categories {
		if c.Name != want[i] {
			t.Errorf("类名必须逐字等于原文标题：第 %d 类是 %q，期望 %q", i, c.Name, want[i])
		}
		if strings.Contains(c.Name, "通用通稿") {
			t.Errorf("类名掺进了模型文本：%q", c.Name)
		}
	}
	seen := map[string]int{}
	for _, c := range st.Categories {
		seen[c.Name]++
	}
	for name, n := range seen {
		if n > 1 {
			t.Errorf("类名「%s」重复 %d 次（路由表会废）", name, n)
		}
	}
}

// 红侧：模型调用整体不可用（provider 挂了）时，必须报错而不是返回空结构。
func TestExtractByChaptersNilClient(t *testing.T) {
	if _, _, err := extractStructureByChapters(context.Background(), nil, pickChapterSpans(chaptersDoc)); err == nil {
		t.Error("没有模型客户端时必须报错")
	}
}

// 跳过类章节要留告警：管理员需要知道「这本手册有几章被判定为非类别页」，
// 否则会以为手册只覆盖了剩下的章。
func TestExtractByChaptersSkipChapterLeavesWarning(t *testing.T) {
	doc := "第一章 目录\n\n" + chapterFiller("目录") + "\n\n" +
		"第二章 产品发布通稿\n\n" + chapterFiller("产品发布") + "\n"
	spans := pickChapterSpans(doc)
	if len(spans) != 2 {
		t.Fatalf("前置条件不成立：只探到 %d 章", len(spans))
	}
	fc := &fakeChat{reply: func(call fakeCall) (string, error) {
		return `{"is_category":true,"trigger":"t","requirement":"产品发布类稿件要交代产品定位","anchors":[]}`, nil
	}}
	_, warns, err := extractStructureByChapters(context.Background(), fc, spans)
	if err != nil {
		t.Fatalf("按章抽取失败: %v", err)
	}
	if len(fc.calls) != 1 {
		t.Errorf("跳过的章不该调用模型，实际调用 %d 次", len(fc.calls))
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "非类别页") {
		t.Errorf("跳过的章必须留告警，实际：%v", warns)
	}
}

// 抽一半失败 → 剩下的仍可用；这条保护了「补救路径不把已有成果全丢」。
func TestExtractByChaptersHalfFailsStillUseful(t *testing.T) {
	spans := pickChapterSpans(chaptersDoc)
	// 计数器必须原子：extractStructureByChapters 并发逐章调模型（worker 池），
	// 普通 n++ 在 -race 下是 READ/WRITE DATA RACE。
	// 语义上也要留意：并发下 n==1 不保证是「第一章」，只保证**恰好一章**失败——
	// 本用例的断言（剩 1 类 + 1 条解析失败告警）不依赖是哪一章，所以仍然成立。
	var n int64
	fc := &fakeChat{reply: func(call fakeCall) (string, error) {
		if atomic.AddInt64(&n, 1) == 1 {
			return "这不是 JSON", nil // 解析失败
		}
		return `{"is_category":true,"trigger":"t","requirement":"产品发布类稿件要交代产品定位","anchors":[]}`, nil
	}}
	st, warns, err := extractStructureByChapters(context.Background(), fc, spans)
	if err != nil {
		t.Fatalf("半数可用时不该整体失败: %v", err)
	}
	if len(st.Categories) != 1 {
		t.Errorf("应剩 1 类，实际 %d 类", len(st.Categories))
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "不是合法 JSON") {
		t.Errorf("解析失败必须点名告警，实际：%v", warns)
	}
}
