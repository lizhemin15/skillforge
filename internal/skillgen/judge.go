package skillgen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// 本文件负责训练期的「独立裁判 + 回炉重生成」里的**判定部分**：
// 评分卡、确定性硬校验、试用（拿手册里已知素材跑一遍）、独立裁判调用。
// 循环编排（试用→判分→回炉→止损）在 generator 的 Step8.5 里，见 judgeLoop。

// judgeDimSpec 是评分卡的一个维度规格。
// Key 是与模型约定的字段名（英文，避免中文标点在 JSON 里出岔子）；
// Label 只用于展示与写入 fidelity.md；Weight 是该维满分。
type judgeDimSpec struct {
	Key    string
	Label  string
	Weight int
}

// judgeScorecard 与设计稿 docs/writing-skill-pipeline-design.md 3.4 一致：
// 分类判定 25 / 要求依从 25 / 范文对齐 20 / 结构完整 15 / 无幻觉 15 = 100。
//
// 为什么「评分」与「审稿」用同一组关注点：运行时审稿人（reviewer.md）查的也是
// 这几件事，如果训练期的裁判另一套标准，就会出现「训练说好、线上被审稿人打回」
// 的分裂。同一套维度让训练分数对线上表现有预测力。
var judgeScorecard = []judgeDimSpec{
	{Key: "category_routing", Label: "分类判定", Weight: 25},
	{Key: "requirement_compliance", Label: "要求依从", Weight: 25},
	{Key: "example_alignment", Label: "范文对齐", Weight: 20},
	{Key: "structure_completeness", Label: "结构完整", Weight: 15},
	{Key: "no_hallucination", Label: "无幻觉", Weight: 15},
}

const (
	// judgePassLine 总分通过线：≥80 分。
	judgePassLine = 80
	// judgeDimFloorRatio 单维下限：某维得分低于该维满分的 60% 即不通过。
	//
	// 为什么是**比例**而不是「<15 分」这样的绝对分：绝对分不是尺度无关的。
	// 评分卡里 结构完整/无幻觉 满分就是 15，绝对下限 15 等于要求这两维满分——
	// 无幻觉丢 1 分（14/15，normal 得不能再正常）就直接不通过，而 25 分的
	// 分类判定却可以丢 10 分。实测这条会把几乎每一轮都判死，
	// 「回炉 3 轮全不通过」变成默认结果，裁判就成了摆设。
	// 60% 同时保住了设计稿里「单维 15 分」这个数字：15 正好是 25 分维度的 60%，
	// 所以设计稿那句话在它当时谈的维度上仍然逐字成立。
	//
	// 意图不变：拦住「某一维塌方」（比如完全跑题但文笔好），不是要求样样满分。
	judgeDimFloorRatio = 0.6
	// judgeMaxRounds 回炉上限：裁判最多判 3 轮，超限不再拦流程。
	judgeMaxRounds = 3
)

// judgeDimFloor 返回某维的塌方下限（向上取整，至少 1 分）：
// 得分 ≤ 下限-1 即视为该维塌方。
//
//	满分 25 → 下限 15（与设计稿写的 15 分一致）
//	满分 20 → 下限 12
//	满分 15 → 下限  9
func judgeDimFloor(weight int) int {
	f := int(float64(weight)*judgeDimFloorRatio + 0.5)
	if f < 1 {
		f = 1
	}
	return f
}

// judgeTotalWeight 是评分卡权重之和。写成函数而不是常量，是为了让它跟
// judgeScorecard 永远同步；权重和不是 100 时直接判定失败（见 judgeDraft），
// 免得评分卡被人改坏后静默按比例算出一个没人能解释的总分。
func judgeTotalWeight() int {
	n := 0
	for _, d := range judgeScorecard {
		n += d.Weight
	}
	return n
}

// JudgeDim 是一维的判分结果。
type JudgeDim struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Weight int    `json:"weight"`
	Score  int    `json:"score"`
	Reason string `json:"reason,omitempty"`
}

// JudgeResult 是一轮裁判的完整结论。
type JudgeResult struct {
	Category string `json:"category"` // 本轮试用的分类
	Total    int    `json:"total"`    // 加权总分（0~100）
	// Pass 是**最终结论**：模型分达标（≥80 且无单维 <15）**且**无硬校验命中。
	// 上层一律以它为准——残缺手册即使模型给满分也必须不通过。
	Pass bool `json:"pass"`
	// ModelPass 是模型的原始判断（不含硬校验否决）。留下它只为把「模型被面子分
	// 骗了、但被硬校验拦住」这件事显性化：两者不一致时，fidelity.md 与人看的日志
	// 才不会把「不通过」误读成「模型说不行」。
	ModelPass bool       `json:"model_pass"`
	Dims      []JudgeDim `json:"dims"`
	// Findings 是给人看的扣分项（含硬校验命中），也是回炉时喂给
	// 「审稿清单重生成」的输入。空 findings + Pass=false 属于实现错误，
	// 所以 judgeDraft 保证不通过时 findings 至少有一条。
	Findings []string `json:"findings"`
	// Hard 只装确定性硬校验的命中项。与模型打分分开存，是为了在
	// fidelity.md 里能一眼区分「机器判的」与「模型判的」。
	Hard []string `json:"hard_findings,omitempty"`
}

// JudgeRound 记录一轮「试用 + 判分」，供 trace 帧与 fidelity.md 表格使用。
type JudgeRound struct {
	Round    int          `json:"round"`
	Category string       `json:"category"`
	Draft    string       `json:"-"`
	Result   *JudgeResult `json:"result"`
}

// ---- 确定性硬校验 ----

// judgeHardFindings 做**不依赖模型**的硬校验：手册结构里存在的缺陷，
// 由代码判定，命中即否决（见 judgeDraft）。
//
// 为什么必须有这一层：裁判是模型，模型会「给面子分」——一本范文全都没切出来的
// 残缺手册，模型完全可能给出 85 分（它看到的是「要求写得挺清楚」）。
// 而「残缺手册必须被判不通过、并点名范文缺失」是产品的硬要求，
// 不能赌模型的脾气。这三条正好是机械可验的：
//
//	① 手册给该类标了范文锚点，却一篇都没切出来；
//	② 切出来的范文不是原文连续子串（说明被模型改写，违反「范文原样」铁律）；
//	③ 全部类别的范文都为空（技能处于无范文可用状态）。
//
// 注意不要把它写成「比产品更严」的检查：手册里本就没有锚点的分类不算缺陷
// （有些章节只讲要求、不附范文），只有「有锚点却没切出」才算。
func judgeHardFindings(mp *manualPack) []string {
	if mp == nil || mp.Structure == nil {
		return []string{"手册结构为空：没有任何分类可供核对"}
	}
	var findings []string
	totalExamples, anchored := 0, 0
	for _, c := range mp.Structure.Categories {
		segs := mp.Examples[c.Name]
		totalExamples += len(segs)
		if len(c.Anchor) == 0 {
			continue
		}
		anchored++
		if len(segs) == 0 {
			findings = append(findings, fmt.Sprintf(
				"分类「%s」：手册标了 %d 处范文锚点，但一篇都没切出来（examples/%s/ 为空）",
				c.Name, len(c.Anchor), safeCatFileName(c.Name)))
		}
	}
	// 保真核对：范文必须是原文连续子串。Source 为空时不谎报通过——
	// 「没核对过」和「核对全过」是两件事，后者会让残缺数据看起来健康。
	for _, c := range mp.Structure.Categories {
		for i, seg := range mp.Examples[c.Name] {
			if mp.Source == "" {
				findings = append(findings, fmt.Sprintf(
					"无法核对保真：examples/%s/%02d.md 缺少核对源（手册原文未留存）",
					safeCatFileName(c.Name), i+1))
				continue
			}
			if !strings.Contains(mp.Source, seg) {
				findings = append(findings, fmt.Sprintf(
					"范文非原文：examples/%s/%02d.md 不是手册原文的连续子串（被模型改写或截断）",
					safeCatFileName(c.Name), i+1))
			}
		}
	}
	if totalExamples == 0 {
		if anchored > 0 {
			findings = append(findings, fmt.Sprintf(
				"全部 %d 个分类的范文均为空：手册有锚点却一篇未切出，技能实际处于无范文可用状态",
				len(mp.Structure.Categories)))
		} else {
			findings = append(findings, fmt.Sprintf(
				"全部 %d 个分类的范文均为空：手册未提供任何范文锚点，无范文可供对齐",
				len(mp.Structure.Categories)))
		}
	}
	return dedupStrings(findings)
}

// ---- 试用取样 ----

// pickTrialCategory 按轮次选取本轮试用的分类，**确定性**而非随机：
// 「同一本手册跑两次，第 N 轮试用的分类完全一样」——否则 fidelity.md 里
// 记录的分数不可复核，出了问题只能靠重跑碰运气。
//
// 分类数 ≤ 轮数上限时按轮次依次取（保证每类至少被试一次）；
// 超过上限时按轮次均匀铺开（第 1 轮取头部、最后 1 轮取尾部），
// 避免总在头几个分类上打转、末尾分类永远没被验证过。
func pickTrialCategory(st *Structure, round int) (Category, bool) {
	if st == nil || len(st.Categories) == 0 {
		return Category{}, false
	}
	if round < 1 {
		round = 1
	}
	n := len(st.Categories)
	idx := 0
	if n <= judgeMaxRounds {
		idx = (round - 1) % n
	} else {
		idx = ((round - 1) * n) / judgeMaxRounds
		if idx >= n {
			idx = n - 1
		}
	}
	return st.Categories[idx], true
}

// trialPackBlock 拼出「本类写作要求 + 本类真实范文」注入块。
//
// 版式刻意与运行时 internal/agent.writing.go 的 packBlock 保持一致（同样的
// 「本次命中的手册分类（硬约束）」标题、同样的「范文里与主体无关的事实不得搬用」
// 提醒）。运行时是把这两样**注入**给写作模型的，试用只有带上同一份料，
// 量出来的分数才对线上有预测力。
//
// 这条是踩过的坑：早先试用只喂类名与场景，裁判却按「手册该类要求的每一条是否
// 做到」逐条扣分，而被测技能压根没看到要求——那量出来的不是「技能不达标」，
// 是「试用少喂了料」，几乎每一轮都会被判死。
func trialPackBlock(mp *manualPack, cat Category) string {
	if mp == nil || mp.Structure == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("==== 本次命中的手册分类（硬约束） ====\n")
	fmt.Fprintf(&b, "分类：%s\n", cat.Name)
	if r := strings.TrimSpace(cat.Requirement); r != "" {
		b.WriteString("\n【本类写作要求（手册原文，逐条遵守，不得取舍）】\n" + r + "\n")
	}
	if t := strings.TrimSpace(cat.Trigger); t != "" {
		b.WriteString("\n【本类适用场景】\n" + t + "\n")
	}
	segs := mp.Examples[cat.Name]
	if len(segs) > 0 {
		b.WriteString("\n【本类真实范文（手册原文，供参照结构、语气、措辞）】\n")
		b.WriteString("范文里与本篇主体无关的事实**不得搬用**——事实只能来自用户提供的素材。\n")
		for i, seg := range segs {
			fmt.Fprintf(&b, "\n----- 范文 %d -----\n%s\n", i+1, strings.TrimSpace(seg))
		}
	}
	b.WriteString("\n==== 手册分类约束结束 ====\n")
	return b.String()
}

// buildTrialInput 造一份「试用需求」：拿手册里已经存在的素材当输入，
// 让技能按该类要求写一篇，再用手册原文当标尺去量它写了什么。
//
// 为什么素材取该类范文本身：手册里唯一「已知正确答案」的素材就是它自己，
// 这样「无幻觉」这一维才有可核对的事实来源——草稿里出现素材之外的事实，
// 就是编造。代价是范文对齐这一维容易被拿高分（草稿与范文同源），
// 这是刻意接受的：另外三维（分类判定/要求依从/结构完整）与硬校验仍有区分度，
// 而「无范文可用」这类缺陷由 judgeHardFindings 确定性拦下。
func buildTrialInput(cat Category, material string) string {
	var b strings.Builder
	b.WriteString("请按「" + cat.Name + "」这一类的要求写一篇稿件。\n")
	if t := strings.TrimSpace(cat.Trigger); t != "" {
		b.WriteString("适用场景：" + t + "\n")
	}
	b.WriteString("\n可用素材（只能使用其中的事实，不得编造）：\n\n")
	b.WriteString(strings.TrimSpace(material))
	b.WriteString("\n")
	return b.String()
}

// trialMaterial 取本轮试用的素材：优先该类的第一篇范文（手册里唯一「已知正确
// 答案」的事实来源）；该类没有范文时退回该类写作要求原文——此时草稿的
// 「无幻觉」一维没有可核对的事实基准，而「无范文可用」本身会由 judgeHardFindings
// 确定性拦下，不需要在素材上再含糊一次。
func trialMaterial(mp *manualPack, cat Category) string {
	if mp != nil {
		if segs := mp.Examples[cat.Name]; len(segs) > 0 {
			return strings.TrimSpace(segs[0])
		}
	}
	return strings.TrimSpace(cat.Requirement)
}

// ---- 裁判调用 ----

// judgeSystemPrompt 是独立裁判的系统提示。
//
// 「独立」是这一层的全部意义：裁判**看不到**技能自己的 system_prompt，
// 也**看不到** reviewer.md。它只拿手册原文（该类要求 + 该类范文）当标尺。
// 若把技能的自述也喂给裁判，技能写得越自信、越能说服裁判自己没问题——
// 就变成「自己给自己判卷」。
const judgeSystemPrompt = `你是独立的稿件评审裁判。你没有参与写作，也不认识作者。

你的标尺只有下面的「手册原文」——其中「该类写作要求」与「该类范文」是权威依据。
凡是手册里没写的要求，不得作为扣分理由。

请按五个维度打分（满分即权重）：

- category_routing（满分 25）：稿件是否确实是该类别要求的文体与用途。
- requirement_compliance（满分 25）：手册该类写作要求里的每一条，稿件是否做到。
- example_alignment（满分 20）：结构、段落推进方式、措辞密度是否与该类范文同一路数。
- structure_completeness（满分 15）：该类应有的组成部分是否齐全（如标题、主体、落款等）。
- no_hallucination（满分 15）：稿件里的事实、数字、名称是否都能在给定素材中找到依据。素材之外的事实即为编造，每出现一处扣分。

打分规则：
1. 严格按依据打分，不要给安慰分、不要因为「整体还算通顺」就抬高某一维。
2. 只允许扣分说明扣在哪里，扣分项必须是具体可核对的（指出哪一句、缺哪一项）。
3. 只输出一个 JSON 对象，不要任何解释性文字、不要 Markdown 代码块。格式：
{"dims":[{"key":"category_routing","score":23,"reason":"..."},...],"findings":["...","..."]}
4. dims 必须包含上述五个 key，一个不少。
5. findings 是给人看的扣分项清单（可含多条）；没有扣分项就输出空数组。`

// trialDraft 用被测技能自己写一篇草稿（= 试用）。
// sysPrompt 是刚生成好的技能 system_prompt——这里就是「拿成品跑一遍」的那一步。
//
// 试用走的是**技能自己的 system_prompt**（再拼上运行时会注入的本类要求与范文），
// 不是裁判的：只有让技能按它将要上线的那套上下文写，量出来的分数才对线上表现
// 有预测力。注入块放在 sys 末尾，与运行时 GenerateWithPack 的做法一致——
// 提示词在前、硬约束在后，模型对末尾的约束依从性更稳。
func (g *Generator) trialDraft(ctx context.Context, sysPrompt string, mp *manualPack, cat Category, material string) (string, error) {
	if g.llm == nil {
		return "", errors.New("trialDraft: 未配置模型客户端")
	}
	sys := sysPrompt
	if blk := trialPackBlock(mp, cat); blk != "" {
		sys = strings.TrimRight(sys, "\n") + "\n\n" + blk
	}
	out, err := g.llm.Chat(ctx, sys, buildTrialInput(cat, material))
	if err != nil {
		return "", fmt.Errorf("trialDraft: 试用写稿失败: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// judgeDraft 让独立裁判给草稿打分。返回的 JudgeResult 一定带非空 findings
// （不通过时至少有一条扣分理由），供回炉使用。
func (g *Generator) judgeDraft(ctx context.Context, mp *manualPack, cat Category, material, draft string) (*JudgeResult, error) {
	if g.llm == nil {
		return nil, errors.New("judgeDraft: 未配置模型客户端")
	}
	if mp == nil || mp.Structure == nil {
		return nil, errors.New("judgeDraft: 手册结构为空")
	}
	// 评分卡自身健康检查：权重和必须等于 100。改坏了就在这里炸，
	// 而不是按某个奇怪比例算出总分、让 fidelity.md 出现没人能解释的分数。
	if sum := judgeTotalWeight(); sum != 100 {
		return nil, fmt.Errorf("judgeDraft: 评分卡权重之和=%d，应为 100", sum)
	}

	user := buildJudgeUserPrompt(cat, mp, material, draft)
	// jsonMode=true：裁判输出必须结构化使用（逐维扣分要进 fidelity.md、
	// findings 要喂回炉），裸奔靠运气会在中文理由里随机炸 json。
	out, err := g.llm.Chat(ctx, judgeSystemPrompt, user, true)
	if err != nil {
		return nil, fmt.Errorf("judgeDraft: 裁判调用失败: %w", err)
	}
	res, err := parseJudgeOutput(out)
	if err != nil {
		return nil, fmt.Errorf("judgeDraft: %w", err)
	}
	res.Category = cat.Name
	res.Hard = judgeHardFindings(mp)
	res.Findings = dedupStrings(append(res.Hard, res.Findings...))
	// 硬校验是否决项：模型分达标也救不回来（模型会「给面子分」，见 judgeHardFindings）。
	res.ModelPass = res.Pass
	res.Pass = res.Pass && len(res.Hard) == 0
	// 不变量：不通过 ⇒ findings 非空。由构造保证——「总分 <80」或「某维 <15」
	// 必然意味着至少有一维没拿满分，而每一维没拿满分都会写进 findings；
	// 硬校验命中本身也是 findings 的一部分。
	// 之所以写成注释而不是运行时判断：它不可达，写成 if 就是死代码；
	// 但这确实是回炉能跑起来的前提（没意见就只能原样重生成一遍），
	// 所以有测试专门盯这条不变量，别当注释看。
	return res, nil
}

// buildJudgeUserPrompt 拼裁判的输入：手册标尺 + 素材 + 草稿。
func buildJudgeUserPrompt(cat Category, mp *manualPack, material, draft string) string {
	var b strings.Builder
	b.WriteString("## 手册原文（唯一标尺）\n\n")
	b.WriteString("### 本类名称\n" + cat.Name + "\n\n")
	if t := strings.TrimSpace(cat.Trigger); t != "" {
		b.WriteString("### 本类触发场景（原文摘录）\n" + t + "\n\n")
	}
	b.WriteString("### 该类写作要求（原文摘录）\n" + strings.TrimSpace(cat.Requirement) + "\n\n")

	if segs := mp.Examples[cat.Name]; len(segs) > 0 {
		b.WriteString("### 该类范文（原文，未改写）\n\n")
		for i, s := range segs {
			fmt.Fprintf(&b, "#### 范文 %02d\n%s\n\n", i+1, strings.TrimSpace(s))
		}
	} else {
		// 范文缺失时明确告知裁判「没有范文可对齐」——但不要让它因此给
		// 范文对齐这一维判高分：硬校验已经会否决这一轮，模型分只作展示。
		b.WriteString("### 该类范文\n（手册未切出该类范文）\n\n")
	}

	b.WriteString("### 写作素材（事实来源，稿件只能使用其中事实）\n" + strings.TrimSpace(material) + "\n\n")
	b.WriteString("### 待评审稿件\n" + strings.TrimSpace(draft) + "\n\n")
	b.WriteString("请按约定 JSON 格式给出五维打分与扣分项。")
	return b.String()
}

// ---- 输出解析 ----

// judgeScore 是模型给出的单个维度得分。
type judgeScore struct {
	Score  int
	Reason string
}

// parseJudgeOutput 解析裁判输出，并**校验五个维度一个不少**。
//
// 缺维度一律报错而不是按缺失记 0 或忽略：裁判只回三个维度、剩下两个默认
// 满分，就会算出一个虚高的总分并把残缺手册放过去——这正是「静默降级」的
// 经典形态，必须让它响亮地失败。
//
// 兼容两种形状：dims 是数组（[{key,score,reason}]）或对象（{"key":{"score":..,"reason":".."}}
// 或 {"key":23}）。模型对 JSON 形状的偏好不稳定，两种都收，但语义校验不放水。
func parseJudgeOutput(out string) (*JudgeResult, error) {
	raw := extractJSON(out)
	if len(raw) == 0 {
		return nil, fmt.Errorf("裁判输出里找不到 JSON 对象（原文：%s）", snippet(out, 160))
	}
	var probe struct {
		Dims     json.RawMessage `json:"dims"`
		Findings []string        `json:"findings"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("裁判 JSON 解析失败: %v（原文：%s）", err, snippet(string(raw), 160))
	}
	got := map[string]judgeScore{}

	if len(probe.Dims) > 0 {
		// 先试数组
		var arr []struct {
			Key    string `json:"key"`
			Score  int    `json:"score"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(probe.Dims, &arr); err == nil && len(arr) > 0 {
			for _, d := range arr {
				got[strings.TrimSpace(d.Key)] = judgeScore{Score: d.Score, Reason: strings.TrimSpace(d.Reason)}
			}
		} else {
			// 再试对象：值可以是数字，也可以是 {score,reason}
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(probe.Dims, &obj); err != nil {
				return nil, fmt.Errorf("裁判 dims 既不是数组也不是对象: %v（原文：%s）", err, snippet(string(probe.Dims), 160))
			}
			for k, v := range obj {
				var n int
				if err := json.Unmarshal(v, &n); err == nil {
					got[strings.TrimSpace(k)] = judgeScore{Score: n}
					continue
				}
				var o struct {
					Score  int    `json:"score"`
					Reason string `json:"reason"`
				}
				if err := json.Unmarshal(v, &o); err != nil {
					return nil, fmt.Errorf("裁判维度 %s 的值无法解析: %v（原文：%s）", k, err, snippet(string(v), 80))
				}
				got[strings.TrimSpace(k)] = judgeScore{Score: o.Score, Reason: strings.TrimSpace(o.Reason)}
			}
		}
	}

	var missing []string
	for _, d := range judgeScorecard {
		if _, ok := got[d.Key]; !ok {
			missing = append(missing, d.Key)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("裁判漏评维度 %s（评分卡要求五个维度全给）", strings.Join(missing, "/"))
	}

	res := &JudgeResult{}
	total := 0
	floorHit := false
	for _, d := range judgeScorecard {
		s := got[d.Key]
		// 越界分数一律夹回 [0, weight] 并留下痕迹：模型给 120 分这种事
		// 说明它没在看权重，直接信它会让总分超过 100。
		if s.Score < 0 || s.Score > d.Weight {
			res.Findings = append(res.Findings, fmt.Sprintf(
				"评分越界：「%s」模型给 %d 分（满分 %d），已夹回有效区间", d.Label, s.Score, d.Weight))
		}
		if s.Score < 0 {
			s.Score = 0
		}
		if s.Score > d.Weight {
			s.Score = d.Weight
		}
		dim := JudgeDim{Key: d.Key, Label: d.Label, Weight: d.Weight, Score: s.Score, Reason: s.Reason}
		res.Dims = append(res.Dims, dim)
		total += s.Score
		if floor := judgeDimFloor(d.Weight); s.Score < floor {
			floorHit = true
		}
		// 扣分项进 findings：只收「真扣了分」的维度，满分维度的理由
		// （通常是「无问题」）不该混进回炉输入里去干扰。
		if s.Score < d.Weight {
			line := fmt.Sprintf("%s %d/%d", d.Label, s.Score, d.Weight)
			if s.Reason != "" {
				line += "：" + s.Reason
			}
			res.Findings = append(res.Findings, line)
		}
	}
	res.Total = total
	// 通过条件与硬校验无关地先算「模型视角」，随后由调用方与硬校验合并；
	// 这里让 Pass 只反映模型分与单维下限，便于 fidelity.md 分开呈现。
	res.Pass = total >= judgePassLine && !floorHit

	if len(probe.Findings) > 0 {
		for _, f := range probe.Findings {
			f = strings.TrimSpace(f)
			if f != "" {
				res.Findings = append(res.Findings, f)
			}
		}
	}
	res.Findings = dedupStrings(res.Findings)
	return res, nil
}

// dedupStrings 去重并保持首次出现顺序，同时剔除空串。
// 顺序重要：fidelity.md 里的扣分项按「硬校验 → 高权重维度 → 低权重维度」
// 排列才能让人顺着读下去，用 map 去重会把顺序打乱。
func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// ---- 裁判报告的交付决策 ----

// JudgeReport 是一场裁判的完整结论：每轮明细 + 交付决策。
//
// 它同时承担两件事：喂给 fidelity.md 做人可读的质量报告（s7d），
// 以及决定**交付哪一轮的产物**。把决策留在数据结构里而不是散在循环里，
// 是为了让「交付的是第几轮、为什么是它」可被测试断言。
type JudgeReport struct {
	Rounds []JudgeRound `json:"rounds"`
	// BestRound 是最优轮次序号（1 基）；0 表示没有产生任何有效轮次。
	BestRound int `json:"best_round"`
	// Err 非空表示裁判**没能跑完**（模型调用失败、输出无法解析等）。
	// 它不阻断落盘（技能本身已经能用），但必须写进 fidelity.md 与 trace：
	// 「裁判没跑成」和「裁判判通过」是两件事，混起来会让没验收过的技能
	// 看起来经过了验收——这正是静默降级最爱藏身的地方。
	Err string `json:"err,omitempty"`
	// EarlyStop 非空表示提前止损回炉的原因（如扣分项全是手册数据缺陷）。
	EarlyStop string `json:"early_stop,omitempty"`
	// DeliveredPrompt 是交付用的 system_prompt（最优一轮那份）。
	DeliveredPrompt string `json:"-"`
	// DeliveredReviewer 是交付用的审稿清单（最优一轮那份），可能为空。
	DeliveredReviewer string `json:"-"`
}

// Passed 表示是否有一轮通过。
func (r *JudgeReport) Passed() bool {
	if r == nil {
		return false
	}
	for _, rd := range r.Rounds {
		if rd.Result != nil && rd.Result.Pass {
			return true
		}
	}
	return false
}

// Best 返回最优一轮（无有效轮次时返回 nil）。
//
// 为什么交付「最优」而不是「最后一轮」：裁判的另一半是模型，分数有噪声。
// 第 3 轮回炉后的提示词完全可能比第 1 轮差——按「最后一轮优先」交付，
// 等于把更差的版本发给用户。同分取更早的轮次，保证结果确定、可复核。
func (r *JudgeReport) Best() *JudgeRound {
	if r == nil {
		return nil
	}
	var best *JudgeRound
	for i := range r.Rounds {
		rd := &r.Rounds[i]
		if rd.Result == nil {
			continue
		}
		if best == nil || rd.Result.Total > best.Result.Total {
			best = rd
		}
	}
	return best
}

// WeakDims 列出不通过那一轮里丢分最多的维度标签（供 trace 与报告点名薄弱项）。
func (r *JudgeReport) WeakDims(n int) []string {
	b := r.Best()
	if b == nil || b.Result == nil {
		return nil
	}
	var labels []string
	for _, d := range judgeDimsSorted(b.Result) {
		if d.Score >= d.Weight {
			continue
		}
		labels = append(labels, fmt.Sprintf("%s %d/%d", d.Label, d.Score, d.Weight))
	}
	return labels[:min(len(labels), n)]
}

// judgeRoundLine 把一轮判分压成一行 trace 帧文案，例如：
//
//	裁判第 1 轮：62/100 不通过 · 分类判定 18/25 · 范文对齐 12/20 · 硬校验 1 项
//
// 显性写出「哪几维丢了分」而不是只报总分：总分相同的两轮，问题可能完全不同，
// 只留总分等于让管理员无从下手。
func judgeRoundLine(r JudgeRound) string {
	if r.Result == nil {
		return fmt.Sprintf("裁判第 %d 轮：无结论", r.Round)
	}
	verdict := "不通过"
	if r.Result.Pass {
		verdict = "通过"
	}
	parts := []string{fmt.Sprintf("裁判第 %d 轮：%d/100 %s", r.Round, r.Result.Total, verdict)}
	var weak []string
	for _, d := range judgeDimsSorted(r.Result) {
		if d.Score < d.Weight {
			weak = append(weak, fmt.Sprintf("%s %d/%d", d.Label, d.Score, d.Weight))
		}
	}
	if len(weak) > 0 {
		parts = append(parts, strings.Join(weak, " · "))
	} else {
		parts = append(parts, "五维满分")
	}
	if n := len(r.Result.Hard); n > 0 {
		parts = append(parts, fmt.Sprintf("硬校验 %d 项", n))
	}
	return strings.Join(parts, " · ")
}

// allFindingsHard 判断一轮的扣分项是否**全部**来自确定性硬校验。
//
// 用途是止损：硬校验命中的是手册数据缺陷（范文没切出来、范文非原文），
// 回炉重生成提示词改不动它——继续烧两轮只会得到一个更差的提示词
// 和被掩盖的真实问题。此时应当停下并把「去核对 categories/ 与 examples/」
// 明确告诉管理员。
func allFindingsHard(res *JudgeResult) bool {
	if res == nil || len(res.Findings) == 0 || len(res.Hard) == 0 {
		return false
	}
	hard := map[string]bool{}
	for _, h := range res.Hard {
		hard[h] = true
	}
	for _, f := range res.Findings {
		if !hard[f] {
			return false
		}
	}
	return true
}

// judgeRoundTable 把多轮判分结果渲染成 Markdown 表格行，供 fidelity.md 使用。
// 单独抽出来是为了让「表格长什么样」可被测试直接断言，而不是靠人肉看落盘文件。
func judgeRoundTable(rounds []JudgeRound) string {
	if len(rounds) == 0 {
		return "（未启用裁判评分）\n"
	}
	var b strings.Builder
	b.WriteString("| 轮次 | 试用分类 | 总分 | 结论 | 扣分项 |\n")
	b.WriteString("| --- | --- | --- | --- | --- |\n")
	for _, r := range rounds {
		if r.Result == nil {
			continue
		}
		verdict := "通过"
		if !r.Result.Pass {
			verdict = "不通过"
		}
		fmt.Fprintf(&b, "| 第 %d 轮 | %s | %d/100 | %s | %s |\n",
			r.Round, mdCell(r.Category), r.Result.Total, verdict, mdCell(joinLimit(r.Result.Findings, 3)))
	}
	return b.String()
}

// joinLimit 取前 n 条用「；」连接，其余折叠成「等 N 项」。
// 表格单元格放不下几十条扣分项，但不能因此丢掉「还有多少项」这个信息。
func joinLimit(items []string, n int) string {
	if len(items) == 0 {
		return "—"
	}
	if len(items) <= n {
		return strings.Join(items, "；")
	}
	return strings.Join(items[:n], "；") + fmt.Sprintf("；等 %d 项", len(items))
}

// judgeDimsSorted 是给测试与调试用的稳定视图：按评分卡顺序返回维度。
// parseJudgeOutput 已按顺序写入 Dims，这里再兜一次，避免调用方自行排序出错。
func judgeDimsSorted(r *JudgeResult) []JudgeDim {
	if r == nil {
		return nil
	}
	order := map[string]int{}
	for i, d := range judgeScorecard {
		order[d.Key] = i
	}
	out := append([]JudgeDim(nil), r.Dims...)
	sort.SliceStable(out, func(i, j int) bool { return order[out[i].Key] < order[out[j].Key] })
	return out
}
