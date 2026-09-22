package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/store"
)

// ===== 手册模式运行时 =====
//
// 训练期把写作手册抽成三份素材（categories/*.md 的要求、examples/<类别>/*.md 的
// 范文、reviewer.md 的审稿清单），并把这些素材的存在写进了 system_prompt 的工作协议。
// 但**运行时模型看不到文件**：技能提示词里那句「打开 categories/ 下该类对应的文件」
// 对纯对话的模型来说是一张空头支票，它只能凭分类名瞎猜要求、凭记忆编范文。
//
// 这个文件补的就是这一段：先判类，再把该类的真实要求与范文注入 prompt，最后让
// 一个上下文独立的审稿人对着草稿挑毛病。三段的顺序与 prompt 里写好的工作协议一致。

// WriteCategory 是一个分类在运行时的全部可用素材。
type WriteCategory struct {
	store.CategoryDoc
	Examples []store.CategoryExample
}

// WritePack 是一个「手册模式」写作技能在运行时的素材包。
type WritePack struct {
	Slug       string
	Categories []WriteCategory
	Reviewer   string // reviewer.md 原文（审稿清单），可能为空
}

// LoadWritePack 读出一个技能的手册素材。
//
// 返回 (nil, nil) 表示「这个技能不是手册模式」——不是错误，调用方据此走原来的
// 单段生成。这个判定就是设计里的「双开关」：skill_type=write 决定要不要读写文章
// 的路径（调用方判断），categories/ 目录在不在决定这条路径是否分三段。约定优于
// 配置：管理人不需要勾任何选项，训练期抽不出结构就不会建这个目录，两边口径天然一致。
func (e *Engine) LoadWritePack(slug string) (*WritePack, error) {
	if e.store == nil || strings.TrimSpace(slug) == "" {
		return nil, nil
	}
	if !e.store.HasCategories(slug) {
		return nil, nil
	}
	docs, err := e.store.ReadCategoryDocs(slug)
	if err != nil {
		return nil, fmt.Errorf("读取手册分类失败: %w", err)
	}
	if len(docs) == 0 {
		// 目录在、里面没有有效分类文件：按「不是手册模式」处理，而不是让后面
		// 路由到空表上。真出这种情况是训练期写坏了，由训练期报错，运行时不猜。
		return nil, nil
	}
	pack := &WritePack{Slug: slug}
	for _, d := range docs {
		ex, err := e.store.ReadCategoryExamples(slug, d)
		if err != nil {
			return nil, fmt.Errorf("读取分类「%s」的范文失败: %w", d.Name, err)
		}
		pack.Categories = append(pack.Categories, WriteCategory{CategoryDoc: d, Examples: ex})
	}
	rv, err := e.store.ReadReviewer(slug)
	if err != nil {
		return nil, fmt.Errorf("读取审稿清单失败: %w", err)
	}
	pack.Reviewer = strings.TrimSpace(rv)
	return pack, nil
}

// CategoryRoute 是一次分类判定的结果。
type CategoryRoute struct {
	Category   string `json:"category"`   // 命中的分类名（原文）
	Confidence string `json:"confidence"` // high | low，仅用于显性化把握程度
	Reason     string `json:"reason"`     // 一句话理由，给用户看

	// Ambiguous 为真表示模型没报出任何一个已知分类。这时**不能**硬着头皮写：
	// 猜错类别等于整篇按错的要求写，用户还得自己发现。调用方应反问用户。
	Ambiguous bool `json:"-"`
}

const routeSys = `你是写作手册的分类路由器。看清用户这次的写作需求，判断它属于下面哪一类。

只输出一个 JSON 对象，不要任何解释文字、不要代码围栏：

{"category":"分类名（必须与下表逐字一致）","confidence":"high 或 low","reason":"一句话说明依据"}

判定规则：
1. category 必须抄自下表，不要自己发明名字、不要改写。
2. 下表的「触发场景」是唯一依据；需求同时符合多类且无法取舍时，把 confidence 填 low，并选最贴近的一类。
3. 需求信息太少、看不出属于哪一类时，category 留空字符串。宁可留空，不要硬猜。`

// RouteCategory 判定本次需求属于哪一类。
//
// 这是一次刻意的「便宜调用」：只喂分类名 + 触发场景的两列表格，不喂各类的完整
// 写作要求与范文——后者是几千字的量级，每次对话都背上会让分类判定变得又慢又贵。
func (e *Engine) RouteCategory(ctx context.Context, pack *WritePack, userMsg string, history []Message) (*CategoryRoute, error) {
	if pack == nil || len(pack.Categories) == 0 {
		return nil, errors.New("RouteCategory: 手册素材为空")
	}
	var b strings.Builder
	b.WriteString("| 分类 | 触发场景 |\n| --- | --- |\n")
	for _, c := range pack.Categories {
		b.WriteString("| " + mdCellInline(c.Name) + " | " + mdCellInline(c.Trigger) + " |\n")
	}

	user := "## 可选的分类\n\n" + b.String() + "\n\n## 用户这次的需求\n\n" + strings.TrimSpace(userMsg)
	if recent := recentUserLines(history, 3); recent != "" {
		// 补最近几轮用户消息，是为了接住「就是第一类」「我说的是通知那种」这类
		// 指代式回答——单看这一句看不出任何类别特征。
		user += "\n\n## 这次对话里用户先前说过的话（供理解指代，不要当成本次需求）\n\n" + recent
	}

	// 第 4 个参数 true = 开 JSON 模式。结构化使用模型输出时不开 JSON 模式是
	// 靠运气：中文内容里带个引号就能让 json 解析随机炸，而且炸得没法诊断。
	//
	// 关思考链 + 流式：分类判定与意图识别同类（按给定表格选一格），但它是
	// 「已经过了分类、正文一个字还没写」的**第二段静默**——实测 33s 分类回来之后
	// 又静默 40s 才出第一个正文字，用户在这 40s 里只看到一个跳秒的计时。
	out, err := e.llm.StreamChat(ctx, routeSys, user, llm.StreamOpts{
		DisableThinking: true,
		JSONMode:        true,
		OnReasoning:     reasoningSink(ctx),
		OnContent:       contentSink(ctx),
	})
	if err != nil {
		return nil, fmt.Errorf("分类判定失败: %w", err)
	}
	route := &CategoryRoute{}
	if err := json.Unmarshal([]byte(extractJSON(out)), route); err != nil {
		return nil, fmt.Errorf("分类判定结果解析失败: %w（原文：%s）", err, runeClip(out, 200))
	}
	route.Category = strings.TrimSpace(route.Category)
	route.Confidence = strings.ToLower(strings.TrimSpace(route.Confidence))
	route.Reason = strings.TrimSpace(route.Reason)

	// 模型偶尔会把分类名写歪（多个空格、加了书名号、写成「新闻通稿类」）。
	// 能唯一对上就认，对不上才算判不准——不能因为一个标点差异就把用户打断。
	if idx := matchCategory(pack.Categories, route.Category); idx >= 0 {
		route.Category = pack.Categories[idx].Name
	} else {
		route.Ambiguous = true
	}
	return route, nil
}

// matchCategory 在分类列表里找 name 对应的下标（-1 = 没找到）。
// 先逐字比，再退到归一化比较（去空白、去书名号/引号/「类」后缀）。
func matchCategory(cats []WriteCategory, name string) int {
	want := strings.TrimSpace(name)
	if want == "" {
		return -1
	}
	for i, c := range cats {
		if strings.TrimSpace(c.Name) == want {
			return i
		}
	}
	nw := normCatName(want)
	for i, c := range cats {
		if normCatName(c.Name) == nw {
			return i
		}
	}
	return -1
}

// normCatName 归一化分类名用于比对。
// 只用「去噪声 + 去后缀」这种不会把两个不同类别压成同一个名字的手段：
// 模糊到子串级别就会出现「通知」命中「通知与通报」这类误判。
func normCatName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "《》<>「」\"'`")
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "　", "")
	for _, suf := range []string{"类文章", "类稿件", "类文件", "类"} {
		if strings.HasSuffix(s, suf) && len([]rune(s)) > len([]rune(suf)) {
			s = strings.TrimSuffix(s, suf)
			break
		}
	}
	return s
}

// FindCategory 按名字取回分类（供调用方在反问之后按用户答复重定位）。
func (p *WritePack) FindCategory(name string) *WriteCategory {
	if p == nil {
		return nil
	}
	if i := matchCategory(p.Categories, name); i >= 0 {
		return &p.Categories[i]
	}
	return nil
}

// packBlock 拼出注入 system prompt 末尾的那一段：本类写作要求 + 本类真实范文。
//
// 范文全部给全文而不是只给结构摘要：设计要求就是「引真实范文」，摘要恰恰是把
// 手册里的原话又过了一遍模型的嘴，等于自废武功。每类范文通常一到两篇，量可控。
func packBlock(pack *WritePack, cat *WriteCategory) string {
	var b strings.Builder
	b.WriteString("==== 本次命中的手册分类（硬约束） ====\n")
	fmt.Fprintf(&b, "分类：%s\n", cat.Name)
	if strings.TrimSpace(cat.Requirement) != "" {
		b.WriteString("\n【本类写作要求（手册原文，逐条遵守，不得取舍）】\n")
		b.WriteString(strings.TrimSpace(cat.Requirement) + "\n")
	}
	if strings.TrimSpace(cat.Trigger) != "" {
		b.WriteString("\n【本类适用场景】\n" + strings.TrimSpace(cat.Trigger) + "\n")
	}
	if len(cat.Examples) > 0 {
		b.WriteString("\n【本类真实范文（手册原文，供参照结构、语气、措辞）】\n")
		b.WriteString("范文里与本篇主体无关的事实**不得搬用**——事实只能来自用户提供的素材。\n")
		// 手册范文常带 {公司名称}/{日期} 这类占位符。模型的惯性是连占位符一起照抄：
		// 用户明明给了「发布单位星河科技、发布日期 2026 年 9 月 14 日」，正文里还是
		// {公司名称}/{日期}。这属于「用户给的信息没用上」，会被当成「没管我说的话」。
		b.WriteString("范文中的占位符（如 {公司名称}、{日期}、{xx}）必须替换成本轮用户给出的真实信息；" +
			"用户没给的，用中性表述绕开，**绝不允许把占位符原样写进正文**。\n")
		for i, ex := range cat.Examples {
			fmt.Fprintf(&b, "\n----- 范文 %d（%s）-----\n", i+1, ex.Path)
			b.WriteString(strings.TrimSpace(ex.Content) + "\n")
		}
	}
	b.WriteString("\n==== 手册分类约束结束 ====\n")
	return b.String()
}

// GenerateWithPack 起草：在技能提示词之后注入本类的真实要求与范文。
// prior 是本会话前文（含上一轮产物的原文）。写作技能最容易犯「不管上一轮」的错：
// 用户说「刚才那篇改成公文语气」「在上一篇基础上加一段」，如果起草时看不到上一篇
// 的正文，模型只会重新写一篇看起来差不多的东西——用户的原话就是「通常没有管之前
// 生成的内容」。所以这里把产物原文并进 extra（system 侧），而不是丢进 user 消息，
// 免得被模型当成「本轮的新素材」混进正文。
func (e *Engine) GenerateWithPack(ctx context.Context, sc *SkillContent, pack *WritePack, cat *WriteCategory, args map[string]string, prior, userMsg string, onDelta func(string)) (string, error) {
	extra := packBlock(pack, cat)
	if strings.TrimSpace(prior) != "" && prior != "（无历史）" {
		extra += "\n\n## 本会话前文（含用户已认可的产物原文）\n" +
			"用户说「改成…/在上一篇基础上…/接着写」时，指的就是下面的产物；" +
			"必须在它基础上修改，不要另起炉灶重写一遍。\n\n" + prior + "\n"
	}
	return e.generateWithExtra(ctx, sc, args, extra, userMsg, onDelta)
}

// ReviewIssue 是审稿人挑出的一条问题。
type ReviewIssue struct {
	Severity string `json:"severity"` // blocking（必须改）| minor（建议改）
	Rule     string `json:"rule"`     // 违反了哪条要求（对齐审稿清单/本类要求）
	Quote    string `json:"quote"`    // 草稿里的原句，便于用户核对
	Fix      string `json:"fix"`      // 具体怎么改
}

// ReviewResult 是一轮审稿的结果。
type ReviewResult struct {
	Verdict string        `json:"verdict"` // pass | revise
	Issues  []ReviewIssue `json:"issues"`
}

// Blocking 返回必须改的问题条数。
func (r *ReviewResult) Blocking() int {
	if r == nil {
		return 0
	}
	n := 0
	for _, is := range r.Issues {
		if is.Severity == "blocking" {
			n++
		}
	}
	return n
}

const reviewSys = `你是审稿人，负责按手册要求验收一篇稿件。你不是写作者，不负责夸奖。

只输出一个 JSON 对象，不要任何解释文字、不要代码围栏：

{"verdict":"pass 或 revise","issues":[{"severity":"blocking 或 minor","rule":"违反了哪一条要求","quote":"草稿里的原句（照抄，便于作者定位）","fix":"具体怎么改"}]}

判定规则：
1. 逐条核对下面给出的检查项与本类写作要求。**每条问题都必须能在草稿里指到具体位置**，用 quote 照抄原句；指不到位置的不算问题。
2. 只有明确违反要求、或会误导读者的缺陷才算 blocking；文风偏好、可改可不改的算 minor。
3. 草稿完全达标时：verdict 填 pass，issues 填空数组 []。不要为了显得认真而硬凑问题。
4. 不得新增检查项之外的要求，也不得因为「信息不足」而扣分——素材不足是写作前该问清的，
   不是审稿阶段判定的。
5. 找不到任何问题就如实说没问题。审稿的价值在于指出真问题，瞎挑毛病比不审更糟。`

// Review 让审稿人对着草稿挑毛病。
//
// **故意用独立上下文**：不带起草时的对话历史，也不把「起草者是怎么想的」告诉它。
// 起草与审稿共享上下文时，模型通常会给自己的稿子放行（自我确认偏误），审稿就
// 退化成一个走过场的动作；换成「只给它稿件和标准」，它才可能真的挑出问题。
func (e *Engine) Review(ctx context.Context, pack *WritePack, cat *WriteCategory, userMsg, draft string) (*ReviewResult, error) {
	if pack == nil || cat == nil {
		return nil, errors.New("Review: 手册素材为空")
	}
	var b strings.Builder
	if strings.TrimSpace(pack.Reviewer) != "" {
		b.WriteString("## 检查项（来自手册审稿清单）\n\n" + pack.Reviewer + "\n\n")
	}
	if strings.TrimSpace(cat.Requirement) != "" {
		fmt.Fprintf(&b, "## 本类（%s）写作要求\n\n%s\n\n", cat.Name, strings.TrimSpace(cat.Requirement))
	}
	if len(cat.Examples) > 0 {
		// 范文给审稿人是必要的：手册里的要求往往很抽象，判「是否达标」得有个
		// 实际水准的参照。不给参照，审稿人会把普通稿子当成达标稿放行。
		fmt.Fprintf(&b, "## 本类真实范文（作为达标水准的参照）\n\n")
		for i, ex := range cat.Examples {
			fmt.Fprintf(&b, "----- 范文 %d -----\n%s\n\n", i+1, strings.TrimSpace(ex.Content))
		}
	}
	fmt.Fprintf(&b, "## 用户这次的写作需求\n\n%s\n\n", strings.TrimSpace(userMsg))
	fmt.Fprintf(&b, "## 待审稿件\n\n%s\n", strings.TrimSpace(draft))

	// 审稿这一跳**保留思考链**（要真挑得出问题才值），但思考片段当中间材料流出去：
	// 审稿发生在草稿之后、最终答案之前，静默期同样只有计时在跳。
	out, err := e.llm.StreamChat(ctx, reviewSys, b.String(), llm.StreamOpts{
		JSONMode:    true,
		OnReasoning: reasoningSink(ctx),
		OnContent:   contentSink(ctx),
	})
	if err != nil {
		return nil, fmt.Errorf("审稿失败: %w", err)
	}
	res := &ReviewResult{}
	if err := json.Unmarshal([]byte(extractJSON(out)), res); err != nil {
		return nil, fmt.Errorf("审稿结果解析失败: %w（原文：%s）", err, runeClip(out, 200))
	}
	res.Verdict = strings.ToLower(strings.TrimSpace(res.Verdict))
	for i := range res.Issues {
		res.Issues[i].Severity = strings.ToLower(strings.TrimSpace(res.Issues[i].Severity))
		if res.Issues[i].Severity != "blocking" {
			res.Issues[i].Severity = "minor"
		}
	}
	if res.Verdict != "pass" {
		res.Verdict = "revise"
	}
	// 说 revise 却一条问题都没给，等于给作者一张没法执行的条子。按「他其实挑不出
	// 问题」处理（即通过），而不是把一句「重写吧」丢给用户。
	if res.Verdict == "revise" && len(res.Issues) == 0 {
		res.Verdict = "pass"
	}
	return res, nil
}

const reviseSys = `你是稿件修改者。按审稿意见逐条修改，输出**修改后的完整稿件**。

规则：
1. 只改审稿意见指出的地方。审稿意见没提到的段落、句子、措辞一律保持原样——
   不许顺手润色、不许顺手重写、不许增删事实。
2. 全部问题（含 minor）都要处理；实在无法处理的，在稿件最后单独起一行以
   「待确认：」开头说明原因，不要偷偷忽略。
3. 直接输出稿件全文，不要输出审稿意见的复述、不要开场白、不要代码围栏。`

// Revise 按审稿意见改稿，返回修改后的全文。
func (e *Engine) Revise(ctx context.Context, sc *SkillContent, pack *WritePack, cat *WriteCategory, draft string, issues []ReviewIssue, onDelta func(string)) (string, error) {
	extra := packBlock(pack, cat)
	var b strings.Builder
	b.WriteString("## 待修改的稿件（完整）\n\n" + strings.TrimSpace(draft) + "\n\n")
	b.WriteString("## 审稿意见（逐条修改）\n\n")
	for i, is := range issues {
		fmt.Fprintf(&b, "%d. [%s] %s\n", i+1, is.Severity, strings.TrimSpace(is.Rule))
		if strings.TrimSpace(is.Quote) != "" {
			fmt.Fprintf(&b, "   原文：%s\n", strings.TrimSpace(is.Quote))
		}
		if strings.TrimSpace(is.Fix) != "" {
			fmt.Fprintf(&b, "   改法：%s\n", strings.TrimSpace(is.Fix))
		}
	}
	b.WriteString("\n请输出修改后的完整稿件。")

	sys := e.generateSys(sc) + "\n\n" + extra + "\n\n" + reviseSys
	// 改稿同样是执笔，同样保留思考链 + 把思考片段当中间材料。
	return e.llm.CompleteEx(ctx, sys, b.String(), onDelta, reasoningSink(ctx))
}

// ===== 小工具 =====

// runeClip 按字符（不是字节）截断，用于错误信息里回显模型原文。
// 按字节切会把一个汉字劈成两半，日志里显示成乱码，等于把「模型到底返回了什么」
// 这条最关键的线索毁掉。
func runeClip(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…（已截断）"
}

// mdCellInline 把内容压成能放进 Markdown 表格一行的形式。
// 表格是给模型看的路由表，单元格里混进换行或竖线会让行列错位，宁可压平。
func mdCellInline(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "／")
	return strings.TrimSpace(s)
}

// recentUserLines 取最近 n 条用户消息，拼成一段。
func recentUserLines(history []Message, n int) string {
	var lines []string
	for i := len(history) - 1; i >= 0 && len(lines) < n; i-- {
		if history[i].Role != "user" {
			continue
		}
		if t := strings.TrimSpace(history[i].Content); t != "" {
			lines = append(lines, "- "+mdCellInline(t))
		}
	}
	// 倒着收集的，翻回来保持时间顺序
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return strings.Join(lines, "\n")
}

// recentUserLines 取最近 n 条用户消息，拼成一段。
