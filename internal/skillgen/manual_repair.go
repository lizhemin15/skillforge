package skillgen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// 本文件实现 L2「按章小 prompt 抽取」：全文一次抽出不来时，按章分片重抽。
//
// 为什么按章就能救回来（不是猜的，是复现脚本量出来的）：
// 同一次调用、同一个模型（Qwen3.6-27B，reasoning），只把输入从 6 万字缩到 12000 字：
//
//	全文   ：categories=6，anchors 全缺，类名还重复（第二章出现两次）
//	12000 字：categories=2，每类 anchors=1 ✅
//
// 差别不在模型能力，在**输出预算**：reasoning token 吃掉了 completion 的 90%
// （10853 里 9782），可见输出只剩一千字，四件事（分类/要求/锚点/范文）里最先被
// 牺牲的就是「唯一 15-40 字的锚点」。按章把它变成「一件事」——每章只需要摘要求
// 和锚点，输入小了、输出短了，模型就有余量做对。
//
// 顺带解决第二个缺陷：**章节名不再由模型起**。全文抽取时模型会把类名抄重复
// （实测出现两个「第二章公司动态通稿」），按章抽取的名字直接取原文标题，
// 天然唯一、天然与手册一致。

// chapterExtractWorkers 是按章抽取的并发度。
//
// 为什么必须并发：一本手册十来章，顺序跑每章 30-60s（reasoning 模型），光这一步
// 就要 6-12 分钟；而客户端训练请求有总超时（实测脚本 timeout 2400s），按章抽取
// 又只是「全文抽取失败后的补救」，不能把预算全吃掉。
// 为什么是 3 而不是更高：provider 侧并发配额未知，抽得太猛会触发限流，
// 补救路径反而更容易失败——补救路径要的是「稳」，不是「快」。
const chapterExtractWorkers = 3

// maxChapterPromptRunes 是单章喂给模型的上限（rune）。
// 取 12000 的依据是上面那组实测数据：这个量级下模型能稳定给出锚点。
// 超过它的「章」多半是探测错了（例如整本手册被当成一章），此时截断比把预算
// 烧光更划算；截断会记进 warnings，不静默。
const maxChapterPromptRunes = 12000

// chapterExtractionPrompt 是单章抽取的 system prompt。
// 与全文抽取的 prompt 相比，它只要求四件事里的三件（触发场景/写作要求/锚点），
// 分类名由代码从标题给，模型不用管——要求越少，越不容易被输出预算牺牲掉。
const chapterExtractionPrompt = `你在读一份中文写作手册的**单独一章**。只做摘录与定位，绝不创作。

硬约束（违反即失败）：
1. 只允许从给定章节原文里原样摘录，禁止生成、改写、润色；你写下的每个字都必须在给定原文中逐字出现过。
2. requirement 是本章对写作要求的原文摘录，可选多句、可跨段，但每一句都必须在原文中逐字出现。不要总结、不要转写。
3. anchors[] 里的 start 与 end 必须是本章原文中**唯一出现一次**的连续片段，15-40 字，逐字复制。
   严禁用省略号（…… / ...）压缩中间内容，严禁补标点、换行或改字——被省略的锚点无法定位，那一类范文就整类丢失。
4. 若本章不是「某一类稿件的写法」（例如目录、前言、附录、索引、参考文献、致谢），把 is_category 设为 false，其余字段留空。
5. 只输出一个 JSON 对象，不要解释性文字，不要 Markdown 围栏。

输出结构：
{"is_category": true,
 "trigger": "什么样的写作需求该落到这一类",
 "requirement": "本章写作要求的原文摘录",
 "anchors": [{"start": "范文首句的原文片段", "end": "范文末句的原文片段"}]}`

// chapterExtraction 是单章抽取的返回结构。
//
// IsCategory 用指针：模型显式说 false 才跳过。缺席（nil）按 true 处理——
// 老的/小的模型经常漏字段，把「漏字段」当成「不是类别」会让整章被误杀，
// 而摘出来的 requirement/anchors 是不是可用，下游还有硬校验把关。
type chapterExtraction struct {
	IsCategory  *bool       `json:"is_category"`
	Trigger     string      `json:"trigger"`
	Requirement string      `json:"requirement"`
	Anchors     []CatAnchor `json:"anchors"`
}

// chapterNonCategoryTitles 是「标题本身就是非类别页」的词表。
//
// 只做**整段相等**判定，不做前缀、更不做裸子串：「目录类稿件的写法」是正经类别，
// 用前缀/包含判定会把它误杀；而「附录一 常见差错」这类带编号的，交给模型按
// prompt 第 4 条自己判 is_category=false 更稳妥。
var chapterNonCategoryTitles = map[string]bool{
	"目录": true, "前言": true, "序言": true, "自序": true, "附录": true,
	"后记": true, "参考文献": true, "索引": true, "致谢": true, "凡例": true,
	"编后记": true, "体例说明": true,
}

// shouldSkipChapter 判断某章是否压根不该送模型（标题就是非类别页的）。
func shouldSkipChapter(title string) bool {
	return chapterNonCategoryTitles[foldChapterTitle(title)]
}

// extractStructureByChapters 按章抽取手册结构，返回（结构, 告警, 错误）。
//
// 单章失败不整体失败：抽成 10/12 章仍是有用的（剩下两类落进 Warnings，人看得见），
// 只有一章都没抽成才算失败。顺序按章节排列（结果按下标写回），因此并发不改变
// 输出顺序——跑批结果必须可复核，「同样的输入得到不同的分类顺序」是不能接受的。
func extractStructureByChapters(ctx context.Context, c chatClient, spans []chapterSpan) (*Structure, []string, error) {
	if c == nil {
		return nil, nil, errors.New("按章抽取：未配置模型客户端")
	}
	cats := make([]*Category, len(spans))
	notes := make([]string, len(spans))
	var wg sync.WaitGroup
	sem := make(chan struct{}, chapterExtractWorkers)
	var mu sync.Mutex
	var firstErr error

	for i := range spans {
		sp := spans[i]
		if shouldSkipChapter(sp.Title) {
			notes[i] = fmt.Sprintf("章节「%s」标题属于非类别页（目录/附录等），已跳过", sp.Title)
			continue
		}
		wg.Add(1)
		go func(i int, sp chapterSpan) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cat, note, err := extractOneChapter(ctx, c, sp)
			mu.Lock()
			defer mu.Unlock()
			if note != "" {
				notes[i] = note
			}
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				notes[i] = fmt.Sprintf("章节「%s」抽取失败：%v", sp.Title, err)
				return
			}
			cats[i] = cat // cat == nil 表示模型判定这章不是稿件类别
		}(i, sp)
	}
	wg.Wait()

	st := &Structure{}
	for _, cat := range cats {
		if cat != nil {
			st.Categories = append(st.Categories, *cat)
		}
	}
	warns := make([]string, 0, len(notes))
	for _, n := range notes {
		if n != "" {
			warns = append(warns, n)
		}
	}
	if len(st.Categories) == 0 {
		if firstErr != nil {
			return nil, warns, fmt.Errorf("按章抽取全部失败：%w", firstErr)
		}
		return nil, warns, errors.New("按章抽取没抽出任何分类")
	}
	return st, warns, nil
}

// extractOneChapter 抽一章。
//
// 返回值约定：(cat, note, err) —— err == nil 且 cat == nil 表示「这章不是稿件类别，
// 正常跳过」，与「失败」区分开：跳过不该让跑批显示告警，失败必须显示。
func extractOneChapter(ctx context.Context, c chatClient, sp chapterSpan) (*Category, string, error) {
	text, note := sp.Text, ""
	if r := []rune(text); len(r) > maxChapterPromptRunes {
		text = string(r[:maxChapterPromptRunes])
		note = fmt.Sprintf("章节「%s」正文 %d 字超过单章上限 %d 字，抽取只用了前 %d 字",
			sp.Title, len(r), maxChapterPromptRunes, maxChapterPromptRunes)
	}
	user := "章节标题：" + sp.Title + "\n\n本章原文：\n\n" + text

	out, err := c.Chat(ctx, chapterExtractionPrompt, user, true)
	if err != nil {
		return nil, note, fmt.Errorf("调用模型失败: %w", err)
	}
	var ce chapterExtraction
	if err := json.Unmarshal(extractJSON(out), &ce); err != nil {
		return nil, note, fmt.Errorf("返回的不是合法 JSON: %w", err)
	}
	if ce.IsCategory != nil && !*ce.IsCategory {
		return nil, note, nil // 模型判定为非类别页
	}

	cat := &Category{
		Name:        sp.Title, // 名字取原文标题，不取模型起的（避免重复/漂移）
		Trigger:     strings.TrimSpace(ce.Trigger),
		Requirement: strings.TrimSpace(ce.Requirement),
		Anchor:      ce.Anchors,
	}
	// 要求和锚点一个都没有 → 这一章没给出任何可用信息，当失败报出来
	// （比默默产出一个空分类强：空分类会进路由表，运行时选中它等于没要求可用）。
	if cat.Requirement == "" && len(cat.Anchor) == 0 {
		return nil, note, errors.New("既没摘出写作要求也没给锚点")
	}
	return cat, note, nil
}

// chapterStructureNote 是走按章抽取时写进 meta/fidelity 的固定说明。
// 让管理员一眼看出这次结果的来源不是「全文一次抽取」，可复核。
const chapterStructureNote = "全文一次抽取不可靠（分类数不足或锚点全失效），已改用按章抽取"
