package skillgen

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/model"
	"github.com/lizhemin15/skillforge/internal/store"
)

// Generator is the "skill-generator" (女娲) pipeline: it ingests reference
// files + a requirement, and produces a complete, validated writing skill.
type Generator struct {
	llm       *llm.Client
	store     *store.SkillStore
	skillsDir string
}

func NewGenerator(l *llm.Client, s *store.SkillStore, dataDir string) *Generator {
	return &Generator{llm: l, store: s, skillsDir: filepath.Join(dataDir, "skills")}
}

// SetLLM swaps the underlying client (used to hot-swap provider per request).
func (g *Generator) SetLLM(l *llm.Client) { g.llm = l }

// Input bundles the admin's raw material for training a new skill.
type Input struct {
	// Slug / Name / Category / Description / Requirement come from the request.
	Slug        string
	Name        string
	Category    string
	Description string
	Requirement string
	// Files are uploaded reference documents (article samples, style guides...).
	Files []*UploadedFile
}

// UploadedFile is a reference file the admin uploaded.
type UploadedFile struct {
	Filename string
	Content  string
}

// Result reports what the generator produced.
type Result struct {
	Slug        string        `json:"slug"`
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Category    string        `json:"category"`
	Params      []model.Param `json:"input_params"`
	Steps       []string      `json:"steps"`
	PromptLen   int           `json:"prompt_len"`
	ExampleN    int           `json:"example_count"`
	SkillType   string        `json:"skill_type,omitempty"`
	Attachment  string        `json:"attachment,omitempty"`
}

// Generate runs the full 7-step pipeline and lands the skill into store + disk.
func (g *Generator) Generate(ctx context.Context, in *Input, onStep func(string)) (*Result, error) {
	steps := func(s string) {
		if onStep != nil {
			onStep(s)
		}
	}

	if in.Slug == "" {
		in.Slug = slugify(in.Name)
	}
	if in.Slug == "" {
		return nil, fmt.Errorf("无法从名称生成 slug")
	}
	dir := filepath.Join(g.skillsDir, in.Slug)

	// ---- Step 1: ingest files → extract key attributes ----
	steps("1/7 分析参考文件，提取写作特征…")
	attrs, err := g.extractAttributes(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("step1: %w", err)
	}

	// ---- Step 2: synthesize frontmatter (description + input params) ----
	steps("2/8 生成技能元数据与表单参数…")
	meta, err := g.synthesizeMetadata(ctx, in, attrs)
	if err != nil {
		return nil, fmt.Errorf("step2: %w", err)
	}

	// ---- Step 3: detect the skill type (write / query / template) ----
	steps("3/8 识别技能类型（写作 / 办事流程 / 模板下发）…")
	dtype, err := g.detectType(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("step3: %w", err)
	}
	steps("3/8 技能类型: " + dtype.Type)

	// ---- Step 4: build system_prompt.md from母线 (typed) ----
	steps("4/8 撰写系统提示词（按类型/文风/结构/禁忌/长度）…")
	sysPrompt, err := g.buildSystemPrompt(ctx, in, attrs, meta, dtype.Type)
	if err != nil {
		return nil, fmt.Errorf("step4: %w", err)
	}

	// ---- Step 5: build template.md skeleton ----
	steps("5/8 生成骨架模板…")
	tpl, err := g.buildTemplate(ctx, in, attrs, dtype.Type)
	if err != nil {
		return nil, fmt.Errorf("step5: %w", err)
	}

	// ---- Step 6: pick/forge examples ----
	steps("6/8 挑选范文 / 生成示例…")
	exFiles, err := g.buildExamples(ctx, in, attrs, dtype.Type)
	if err != nil {
		return nil, fmt.Errorf("step6: %w", err)
	}

	// ---- Step 7: local validation (hard gate) ----
	steps("7/8 本地校验（不齐不放行）…")
	if err := g.validate(sysPrompt, tpl, exFiles, dtype.Type); err != nil {
		return nil, err
	}

	// ---- Step 8: land to disk + register in DB ----
	steps("8/8 落盘并注册…")
	if err := g.land(dir, sysPrompt, tpl, exFiles, in, attrs, dtype); err != nil {
		return nil, err
	}
	sk := &model.Skill{
		Slug: in.Slug, Name: meta.Name, Description: meta.Description,
		Category: firstNonEmpty(in.Category, "general"), Version: 1,
		Enabled: true, IsCore: false,
		SkillType:  dtype.Type,
		Attachment: dtype.Attachment,
	}
	if err := g.store.Create(sk, meta.Params); err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}

	return &Result{
		Slug: in.Slug, Name: meta.Name, Description: meta.Description,
		Category: firstNonEmpty(in.Category, "general"),
		Params:   meta.Params, Steps: nil,
		PromptLen: len(sysPrompt), ExampleN: len(exFiles),
		SkillType: dtype.Type, Attachment: dtype.Attachment,
	}, nil
}

// ---- Step 7: validation ----
func (g *Generator) validate(sysPrompt, tpl string, exFiles []string, typ string) error {
	var missing []string
	sysPrompt = strings.TrimSpace(sysPrompt)
	if len(sysPrompt) < 300 {
		missing = append(missing, "system_prompt.md 过短(<300字符),深度不足")
	}
	// write skills need at least one example范文 anchor; query/template skills
	// are flow-based and don't necessarily have one.
	if typ == model.SkillTypeWrite && len(exFiles) == 0 {
		missing = append(missing, "缺少示例范文(examples/)")
	}
	_ = tpl
	if len(missing) > 0 {
		// 触发LLM自省修正（此处简化：直接拒绝并提示）
		return fmt.Errorf("生成未通过校验: %s", strings.Join(missing, "; "))
	}
	return nil
}

// ---- Step 8: write files ----
func (g *Generator) land(dir, sysPrompt, tpl string, exFiles []string, in *Input, attrs string, dtype *typeOut) error {
	if err := os.MkdirAll(filepath.Join(dir, "examples"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "source"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "system_prompt.md"), []byte(sysPrompt), 0o644); err != nil {
		return err
	}
	// ---- style_profile.md: immutable style anchor (audience/style/structure/
	// tone/taboo/length/terms). Generated in step1, persisted here so the
	// review/optimize step can anchor on it and never let style drift. ----
	if attrs != "" && strings.TrimSpace(attrs) != "" && attrs != "{}" {
		if err := os.WriteFile(filepath.Join(dir, "style_profile.md"), []byte(styleProfileMarkdown(attrs)), 0o644); err != nil {
			return err
		}
	}
	if strings.TrimSpace(tpl) != "" {
		if err := os.WriteFile(filepath.Join(dir, "template.md"), []byte(tpl), 0o644); err != nil {
			return err
		}
	}
	if in.Requirement != "" {
		_ = os.WriteFile(filepath.Join(dir, "requirement.md"), []byte(in.Requirement), 0o644)
	}
	// persist raw uploaded reference files under source/ (knowledge-base style)
	seen := map[string]int{}
	for _, uf := range in.Files {
		base := filepath.Base(uf.Filename) // strip any path
		if strings.TrimSpace(base) == "" {
			base = "unnamed.txt"
		}
		n := seen[base]
		seen[base]++
		fn := base
		if n > 0 {
			ext := filepath.Ext(base)
			stem := strings.TrimSuffix(base, ext)
			fn = fmt.Sprintf("%s-%d%s", stem, n+1, ext)
		}
		_ = os.WriteFile(filepath.Join(dir, "source", fn), []byte(uf.Content), 0o644)
	}
	for i, ex := range exFiles {
		fname := fmt.Sprintf("example%02d.md", i+1)
		if err := os.WriteFile(filepath.Join(dir, "examples", fname), []byte(ex), 0o644); err != nil {
			return err
		}
	}
	meta := map[string]any{"name": in.Name, "category": in.Category, "description": in.Description, "created_at": time.Now().Format(time.RFC3339), "generator_version": 1}
	if dtype != nil {
		meta["skill_type"] = dtype.Type
		meta["rationale"] = dtype.Rationale
		if dtype.Attachment != "" {
			meta["attachment"] = "source/" + dtype.Attachment
		}
	}
	mb, _ := json.MarshalIndent(meta, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "meta.json"), mb, 0o644)
	return nil
}

// ---- Step 1: extract attributes from reference files ----
func (g *Generator) extractAttributes(ctx context.Context, in *Input) (string, error) {
	corpus := in.Requirement + "\n\n===== 参考文件 =====\n"
	for _, f := range in.Files {
		corpus += "\n<<文件: " + f.Filename + ">>\n" + truncate(f.Content, 20000) + "\n"
	}
	if len(in.Files) == 0 {
		// no files, use LLM knowledge of the article type
	}
	sys := `你是资深写作研究员。请分析给定的需求与参考文件，提取这类文章的写作特征，输出 JSON。字段：
- audience: 目标读者
- style: 文风基调(如"正式公文/口语白话/学术严谨/新媒体活泼")
- structure: 常见结构要点的概括
- tone: 语气倾向
- taboo: 禁忌(若干条,如"不得出现网络流行语")
- length_guide: 长度建议
- term_note: 术语/措辞注意
只输出JSON对象，不要多余文字。`
	user := corpus
	out, err := g.llm.Chat(ctx, sys, user)
	if err != nil {
		return "", err
	}
	return jsonStringify(extractJSON(out)), nil
}

// typeOut is the output of the skill-type detector.
type typeOut struct {
	Type       string `json:"type"`       // write | query | template
	Attachment string `json:"attachment"` // relative source/ path, only for template
	Rationale  string `json:"rationale"`
}

// detectType classifies what this skill should do at run time. A "write" skill
// generates articles from the reference style; a "query"/"template" skill is an
// office-process assistant — it returns the迟程序 flow (and optionally pushes a
// downloadable form template). The agent uses this to pick the right behavior.
func (g *Generator) detectType(ctx context.Context, in *Input) (*typeOut, error) {
	corpus := in.Requirement + "\n\n===== 参考文件 =====\n"
	for _, f := range in.Files {
		corpus += "\n<<文件: " + f.Filename + ">>\n" + truncate(f.Content, 20000) + "\n"
	}
	sys := `你是系统设计专家。请研读下面的"需求 + 参考文件"，判断这个技能应该作为什么类型的助手工作，输出JSON（只输出JSON）：
{
  "type": "write" | "query" | "template",
  "attachment": "需要下发给用户的模板文件名，仅当参考文件中存在"需要用户填写/使用的表单、Word 模板、Excel 表格"等文件时填写文件名（不含路径）；否则填空字符串",
  "rationale": "一句话说明判断依据"
}
判定口径：
- type=write：参考文件是"文章范例/写作素材/文风指南"，目标是帮用户写一篇文章、通稿、公文、报告等。
- type=query：参考文件是"办事流程、业务步骤、审批环节、操作指导、政策问答"，目标是用户问什么时候，助手直接给出流程/步骤/答案，不写长篇大论。
- type=template：同 query，但参考文件里还包含"需要用户填写/签字的表单模板或 Word 文档"，用户命中时除了给流程，还要把该文件作为附件下发。
注意：若参考文件里明确有 .docx/.xlsx/.xls/.doc/.pdf 这类文件，多半是 template 型要下发的附件。`
	out, err := g.llm.Chat(ctx, sys, corpus)
	if err != nil {
		return nil, err
	}
	var t typeOut
	if err := json.Unmarshal([]byte(extractJSON(out)), &t); err != nil {
		return nil, fmt.Errorf("type json: %w", err)
	}
	if t.Type == "" {
		t.Type = model.SkillTypeWrite
	}
	// normalize to a known constant
	switch t.Type {
	case model.SkillTypeQuery, model.SkillTypeTemplate, model.SkillTypeWrite:
	default:
		t.Type = model.SkillTypeWrite
	}
	// if template but no attachment named, fall back to first candidate binary
	if t.Type == model.SkillTypeTemplate && strings.TrimSpace(t.Attachment) == "" {
		for _, f := range in.Files {
			if isOfficeFile(f.Filename) {
				t.Attachment = f.Filename
				break
			}
		}
	}
	return &t, nil
}

func isOfficeFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".docx", ".xlsx", ".xls", ".doc", ".pdf", ".pptx", ".xlsm", ".docm":
		return true
	}
	return false
}

// styleProfileMarkdown renders the extracted-style JSON into a human-readable,
// immutable style anchor that the review/optimize step always references so
// style never drifts. Degrades gracefully to the raw text if not valid JSON.
func styleProfileMarkdown(attrs string) string {
	var v map[string]any
	if err := json.Unmarshal([]byte(attrs), &v); err != nil {
		return attrs // keep whatever was extracted; not fatal
	}
	var b strings.Builder
	b.WriteString("# 风格画像（style profile，不可变锚）\n\n")
	b.WriteString("> 这是训练时从参考文件中提炼出的**风格基线**。后续任何 prompt 优化都必须保留下表特征，")
	b.WriteString("只允许在文体措辞层面收窄/强化，不得推翻或泛化。\n\n")
	order := []struct{ key, label string }{
		{"audience", "目标读者"},
		{"style", "文风基调"},
		{"structure", "结构要点"},
		{"tone", "语气倾向"},
		{"taboo", "禁忌"},
		{"length_guide", "长度建议"},
		{"term_note", "术语 / 措辞注意"},
	}
	for _, kv := range order {
		if raw, ok := v[kv.key]; ok && raw != nil {
			s, _ := raw.(string)
			s = strings.TrimSpace(s)
			if s == "" {
				if j, err := json.Marshal(raw); err == nil {
					s = strings.TrimSpace(string(j))
				}
			}
			if s == "" || s == "null" || s == `""` {
				continue
			}
			b.WriteString("## " + kv.label + "\n")
			b.WriteString(s + "\n\n")
		}
	}
	return strings.TrimSpace(b.String()) + "\n"
}

// Review performs an *incremental* optimization of an existing skill's system
// prompt, anchored on the immutable style_profile. It reads the CURRENT prompt +
// style anchor + examples and only rewrites what the instruction touches — never
// a full rewrite — so style and structure stay stable across iterations.
// Returned string is the complete updated system_prompt (ready to land).
func (g *Generator) Review(ctx context.Context, slug, instruction, currentPrompt, styleProfile, templateText string, examples []string) (string, error) {
	if strings.TrimSpace(instruction) == "" {
		return "", fmt.Errorf("优化指令不能为空")
	}
	var exBlock strings.Builder
	for i, ex := range examples {
		if strings.TrimSpace(ex) == "" {
			continue
		}
		exBlock.WriteString(fmt.Sprintf("\n\n--- 示例范文%d ---\n%s", i+1, ex))
	}
	// Truncate to keep the call bounded.
	cur := currentPrompt
	if len(cur) > 16000 {
		cur = cur[:16000]
	}
	anchor := styleProfile
	if strings.TrimSpace(anchor) == "" {
		anchor = "（无风格画像，请按现有 system_prompt 推断并保持）"
	}
	tplPart := ""
	if strings.TrimSpace(templateText) != "" {
		tplPart = "\n\n== 现有文章骨架 template.md ==\n" + templateText
	}
	sys := `你是一位资深提示词工程师，负责对一个已经上线、风格稳定的写作技能做**增量优化**。
铁律：
1. **只改与优化指令直接相关的部分**；其余段落、用词、结构必须原样保留。
2. **绝不推翻风格画像**中的任何特征（目标读者/文风/语气/禁忌/长度/术语）——它们是不可变锚。
3. 优化指令要“收紧/强化”现有风格就收紧；要“增补能力/写某类文章”就只增补对应小节。
4. 若某处与风格画像冲突，以风格画像为准，并在输出前自查一遍不越界。
5. 直接输出**完整的更新后 system_prompt 正文**（可用 # 标题组织），不解释、不用代码块包裹。
优化指令：{INSTRUCTION}`
	sys = strings.ReplaceAll(sys, "{INSTRUCTION}", instruction)
	user := "== 风格画像（不可变） ==\n" + anchor + "\n\n== 现有 system_prompt.md ==\n" + cur + tplPart + exBlock.String()
	out, err := g.llm.Chat(ctx, sys, user)
	if err != nil {
		return "", err
	}
	res := cleanCodeFence(out)
	if strings.TrimSpace(res) == "" {
		return "", fmt.Errorf("优化结果为空")
	}
	return res, nil
}

// jsonStringify pretty-formats a raw JSON byte slice; degrades gracefully.
func jsonStringify(b []byte) string {
	var v any
	if b == nil {
		return "{}"
	}
	if err := json.Unmarshal(b, &v); err == nil {
		if out, err := json.MarshalIndent(v, "", "  "); err == nil {
			return string(out)
		}
	}
	return string(b)
}

// ---- Step 2: synthesize metadata ----
type metaOut struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Params      []model.Param `json:"input_params"`
}

func (g *Generator) synthesizeMetadata(ctx context.Context, in *Input, attrs string) (*metaOut, error) {
	// For query/template skills, the form captures the concrete matter to
	// handle / objects involved, rather than writing variables.
	sys := `你负责为一个新的"技能"设计元数据。该技能会出现在一个公开网站上,用户挑选后填写表单触发服务。
判定场景:若参考文件/需求是"办事流程、业务规则、操作指导、模板下发"，则该技能是办事/查询助手，表单字段应聚焦"要办的具体事项/对象/关键参数";若否则是文章写作技能,表单字段聚焦"标题/主题/素材/篇幅/语气"。
请输出JSON:
{
  "name": "简短技能名(中文,不带文件扩展名)",
  "description": "一句话介绍该技能能做什么、适合谁(<=80字)",
  "input_params": [表单字段数组,每项 {name,label,type(取值 text|textarea|select|number),required,placeholder,help,options(仅select)}]
}
input_params 通常3-6项。只输出JSON。`
	user := "需求:\n" + in.Requirement + "\n\n分析:\n" + attrs
	out, err := g.llm.Chat(ctx, sys, user)
	if err != nil {
		return nil, err
	}
	var m metaOut
	if err := json.Unmarshal([]byte(extractJSON(out)), &m); err != nil {
		return nil, fmt.Errorf("meta json: %w", err)
	}
	if m.Name == "" {
		m.Name = in.Name
	}
	if m.Description == "" {
		m.Description = firstNonEmpty(in.Description, in.Requirement)
	}
	// normalize bools/ints
	for i := range m.Params {
		if m.Params[i].Type == "" {
			m.Params[i].Type = "text"
		}
		if m.Params[i].Label == "" {
			m.Params[i].Label = m.Params[i].Name
		}
		if m.Params[i].Options != nil && len(m.Params[i].Options) == 0 {
			m.Params[i].Options = nil
		}
	}
	return &m, nil
}

// ---- Step 4: system prompt (typed) ----
func (g *Generator) buildSystemPrompt(ctx context.Context, in *Input, attrs string, _ *metaOut, typ string) (string, error) {
	var tpl string
	var extra string
	if typ == model.SkillTypeWrite {
		tpl = g.motherTemplate()
	} else {
		tpl = g.flowMotherTemplate()
		extra = "\n参考文件可能包含办事流程/步骤/表单/政策条款，你要把它们转成用户能直接照着执行的清单步骤；若需要下发附件，明确告诉用户“我已为你准备好模板《xxx》”。"
	}
	sys := `你是一位顶尖的提示词工程师。请基于下面的"母模板"和需求,为这个技能撰写完整的 system_prompt.md 内容。
要求:完整覆盖"身份/任务/执行步骤/结构规范/文风与措辞/长度/禁用项/特殊要求";引用参考文件里体现的具体风格与流程;要具体可执行,不要空泛。
直接输出正文,不要用代码块包裹,不要输出解释。` + extra
	user := "需求:\n" + in.Requirement + "\n\n特征分析:\n" + attrs + "\n\n母模板:\n" + tpl
	out, err := g.llm.Chat(ctx, sys, user)
	if err != nil {
		return "", err
	}
	return cleanCodeFence(out), nil
}

// flowMotherTemplate is the母模板 for query/template (office-process) skills.
// Unlike the write mother template, the runtime output is an actionable flow
// delivered as clear steps — not a long-form article.
func (g *Generator) flowMotherTemplate() string {
	return `# 办事流程 / 规则查询 System Prompt 母模板
你是{角色}——专精于{业务领域}办事流程的资深顾问。
## 任务
根据用户询问的具体事项,结合参考文件里的办事流程/规则/模板,给出可直接照做的回答。
## 回答形式(关键)
- 结构化的流程步骤(1. 2. 3. ...),每步写明动作、所需材料、办理渠道、时限。
- 涉及模板/表单时,明确提示用户"参考模板《{文件名}》"，并说明该文件已随回答下发。
## 处理步骤
1. 理解用户要办的具体事项
2. 匹配参考文件里对应的流程/条款
3. 组织成步骤清单(必要时按"前置条件/步骤/注意/常见问题"分层)
4. 有模板就指认模板文件
## 文风
- 简洁、可直接执行,不堆砌客套
- 关键条件/时限要突出
## 禁用项
- 不得编造参考文件里没有的流程或条款
- 不得长篇大论写成文章,用户要的是怎么办事
## 特殊要求
(引用具体流程细节与模板文件)`
}

// ---- Step 5: template ----
func (g *Generator) buildTemplate(ctx context.Context, in *Input, attrs string, typ string) (string, error) {
	var sys string
	if typ == model.SkillTypeWrite {
		sys = `你是写作大纲专家。请为这个写作技能生成一个通用的"文章骨架模板"(template.md),给出该类型文章通常的结构层级(chapter/section),每部分用一两句说明写什么、注意啥。
用Markdown标题组织,保持通用(用占位如{{主题}}),不要写死具体内容。直接输出正文,不要代码块。`
	} else {
		sys = `你是流程梳理专家。请为这个办事/查询技能生成一个通用的"回答骨架模板"(template.md),给出回答时通常用到的分层(前置条件/步骤/注意/常见问题/模板文件),每部分一两句说明怎么写。
用Markdown标题组织,保持通用,不要写死具体办事内容。直接输出正文,不要代码块。`
	}
	user := "需求:\n" + in.Requirement + "\n\n特征:\n" + attrs
	out, err := g.llm.Chat(ctx, sys, user)
	if err != nil {
		return "", err
	}
	return cleanCodeFence(out), nil
}

// ---- Step 6: examples (pick from files if good, else forge one) ----
func (g *Generator) buildExamples(ctx context.Context, in *Input, attrs string, typ string) ([]string, error) {
	// query/template skills are flow-based; a forged sample anchors the output
	// shape regardless, and is not required for validation.
	var sys string
	if typ == model.SkillTypeWrite {
		sys = `为这个写作技能生成一篇简短的示例范文(400-900字),严格体现你为该类型总结的文风与结构,作为少样本示例锚点。直接输出正文。`
	} else {
		sys = `为这个办事/查询技能生成一条简短的示例回答(以步骤清单形式,包含前置条件/步骤/注意/常见问题/模板文件),严格体现参考文件里的真实流程,作为少样本示例锚点。直接输出正文。`
	}
	// If admin uploaded a complete article-ish sample, reuse it for write skills.
	if typ == model.SkillTypeWrite {
		var chosen []string
		for _, f := range in.Files {
			if len(chosen) >= 2 {
				break
			}
			if len(f.Content) > 500 && looksLikeArticle(f.Content) {
				chosen = append(chosen, f.Content)
			}
		}
		if len(chosen) > 0 {
			return chosen, nil
		}
	}
	// Otherwise forge one demonstration snippet to anchor style.
	user := "需求:\n" + in.Requirement + "\n\n特征:\n" + attrs
	out, err := g.llm.Chat(ctx, sys, user)
	if err != nil {
		return nil, err
	}
	return []string{cleanCodeFence(out)}, nil
}

// ---- mother template ----
func (g *Generator) motherTemplate() string {
	return `# 写作技能 System Prompt 母模板
你是{角色}——专精于撰写{文章类型}的资深写作专家。
## 任务
根据用户输入的{输入变量},产出一篇符合要求的{文章类型}。
## 写作步骤
1. 理解需求与素材
2. 搭建结构(参考骨架)
3. 拟写各部分
4. 整体润色与校对
## 结构规范
(骨架层级与每部分要点)
## 文风与措辞
(具体风格要求)
## 长度
(按用户要求或默认)
## 禁用项
(不得...)
## 特殊要求
(引用具体风格细节)`
}

// Slugify converts a name into a filesystem-safe slug.
func Slugify(s string) string { return slugify(s) }

func slugify(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	// keep letters, digits and CJK (Han); everything else collapses to '-'
	// (Chinese skill names are the primary use case; filesystem-safe UTF-8 slugs)
	re := regexp.MustCompile(`[^a-z0-9\p{Han}]+`)
	s = re.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	// fallback to timestamp-based for empty / too-short names
	if len(s) < 2 {
		s = fmt.Sprintf("skill-%d", time.Now().Unix())
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func cleanCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		lines := strings.Split(s, "\n")
		lines = lines[1:]
		if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), "```") {
			lines = lines[:len(lines)-1]
		}
		s = strings.Join(lines, "\n")
	}
	return strings.TrimSpace(s)
}

func looksLikeArticle(s string) bool {
	words := len(strings.Fields(s))
	hasTitle := strings.Count(s, "#") >= 1 || len(s) > 800
	return words > 150 && hasTitle
}

// extractJSON pulls the first {...} JSON object from a string.
func extractJSON(s string) []byte {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start == -1 || end == -1 || end < start {
		return []byte(s)
	}
	return []byte(s[start : end+1])
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
