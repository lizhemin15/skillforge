package skillgen

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/model"
	"github.com/lizhemin15/skillforge/internal/store"
)

// chatClient 是生成器用到的**模型能力面**：一次不带历史的对话调用。
//
// 为什么这里要抽一个接口（而不是继续用 *llm.Client）：训练裁判的验收
// （「残缺手册必须被判不通过并点名范文缺失」）要求结果**确定性可复现**——
// 拿真模型跑只能看「像不像」，判分每次都不一样。只有能注入替身，
// 才能把断言钉在「产物与判定」上而不是「模型今天心情如何」。
type chatClient interface {
	Chat(ctx context.Context, sys, user string, jsonMode ...bool) (string, error)
}

// Generator is the "skill-generator" (女娲) pipeline: it ingests reference
// files + a requirement, and produces a complete, validated writing skill.
type Generator struct {
	llm       chatClient
	store     *store.SkillStore
	skillsDir string
	// ocrURL 是文档解析微服务（ocrd）的地址。放在结构体里而不是逐个调用点传参，
	// 是因为它和 llm 一样属于「生成器依赖的外部服务」，生命周期一致；为空时
	// 二进制素材无法文本化，会降级为告警而不是报错。
	ocrURL string
	// ocrTimeout 是单次文档解析的客户端超时上限；<=0 表示用 DefaultOCRTimeout。
	// 不写死常量：不同部署的机器算力差距很大，运维要能自己调。
	ocrTimeout time.Duration
}

func NewGenerator(l *llm.Client, s *store.SkillStore, dataDir string) *Generator {
	g := &Generator{store: s, skillsDir: filepath.Join(dataDir, "skills")}
	g.SetLLM(l)
	return g
}

// SetLLM swaps the underlying client (used to hot-swap provider per request).
//
// 显式处理 l == nil：把「nil 的 *llm.Client」赋给接口字段会得到一个**非 nil 的接口**
// （类型已知、值为 nil），于是所有 `g.llm == nil` 的守卫全部失效，后面第一步调用
// 就 panic 在解析地址上。未配置模型的部署路径必须能安全降级，所以这里把 typed-nil
// 归一化成真 nil。
func (g *Generator) SetLLM(l *llm.Client) {
	if l == nil {
		g.llm = nil
		return
	}
	g.llm = l
}

// SetChatClient 注入替身模型（测试专用）。生产路径一律走 SetLLM。
func (g *Generator) SetChatClient(c chatClient) { g.llm = c }

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
	// Raw 保存上传的原始字节，仅在 Content 由 OCR 抽取得到时非空。
	// 为什么要留原始字节：source/ 是素材留档区，管理员需要能下载回原始扫描件
	// 做人工核对；而锚点定位只能用文本，两者必须分开存。
	Raw []byte
	// Extracted 标记 Content 是否来自 OCR 抽取（而非上传副本本身就是文本）。
	Extracted bool
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

	// ---- Step 0: 素材入库（二进制文档先过 OCR 解析） ----
	// 旧流程把上传字节直接当文本塞给 LLM：上传一本扫描版手册，模型看到的
	// 是二进制乱码，「抽取写作特征」自然全军覆没。这一步把素材真正文本化，
	// 也是后面手册结构抽取能成立的前提。
	g.ingestFiles(ctx, in, steps)

	// ---- Step 1: ingest files → extract key attributes ----
	steps("1/9 分析参考文件，提取写作特征…")
	attrs, err := g.extractAttributes(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("step1: %w", err)
	}

	// ---- Step 2: synthesize frontmatter (description + input params) ----
	steps("2/9 生成技能元数据与表单参数…")
	meta, err := g.synthesizeMetadata(ctx, in, attrs)
	if err != nil {
		return nil, fmt.Errorf("step2: %w", err)
	}

	// ---- Step 3: detect the skill type (write / query / template) ----
	steps("3/9 识别技能类型（写作 / 办事流程 / 模板下发）…")
	dtype, err := g.detectType(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("step3: %w", err)
	}
	steps("3/9 技能类型: " + dtype.Type)

	// ---- Step 4: build system_prompt.md from母线 (typed) ----
	steps("4/9 撰写系统提示词（按类型/文风/结构/禁忌/长度）…")
	sysPrompt, err := g.buildSystemPrompt(ctx, in, attrs, meta, dtype.Type)
	if err != nil {
		return nil, fmt.Errorf("step4: %w", err)
	}

	// ---- Step 5: 手册识别（分类结构 + 范文原样切分 + 审稿清单） ----
	// 只在写作类技能上尝试：办事流程/模板下发的素材不是写作手册，抽结构纯属浪费。
	// 抽不出来即降级回通用流程——这是设计好的退路，不是故障，所以只记 trace 不报错。
	var mp *manualPack
	if dtype.Type == model.SkillTypeWrite {
		steps("5/9 检测素材是否为写作手册（分类结构 / 范文锚点）…")
		cand, mErr := g.buildManual(ctx, in)
		if mErr != nil {
			steps("5/9 未按手册处理，走通用流程：" + mErr.Error())
		} else {
			mp = cand
			steps(fmt.Sprintf("5/9 识别到手册：%d 个分类，切出 %d 篇原文范文",
				len(mp.Structure.Categories), mp.ExampleCount()))
			for _, w := range mp.Warnings {
				steps("5/9 ⚠️ " + w)
			}
			// 分类路由与「拿不准就问」协议要写进 system_prompt，
			// 技能才能脱离本平台独立运行（路由表是轻量的，重的范文留给运行时按需注入）。
			sysPrompt = augmentPromptForManual(sysPrompt, mp.Structure)
			if rev, rErr := g.buildReviewer(ctx, mp.Structure); rErr != nil {
				steps("5/9 ⚠️ 审稿清单生成失败（不影响生成）：" + rErr.Error())
			} else {
				mp.Reviewer = rev
				steps("5/9 已生成审稿清单 reviewer.md（可逐条核对）")
			}
		}
	}

	// ---- Step 6: build template.md skeleton ----
	steps("6/9 生成骨架模板…")
	tpl, err := g.buildTemplate(ctx, in, attrs, dtype.Type)
	if err != nil {
		return nil, fmt.Errorf("step6: %w", err)
	}

	// ---- Step 7: 范文 ----
	// 手册模式下范文全部来自原文切分，**绝不再让 LLM 编**：编出来的「范文」
	// 手册里根本没有，等于给用户喂一份伪手册（这是旧流程最要命的问题）。
	var exFiles []string
	if mp != nil {
		steps("7/9 范文取自手册原文切分（不重写，可机械校验保真）")
	} else {
		steps("7/9 挑选范文 / 生成示例…")
		exFiles, err = g.buildExamples(ctx, in, attrs, dtype.Type)
		if err != nil {
			return nil, fmt.Errorf("step7: %w", err)
		}
	}

	// ---- Step 8: local validation (hard gate) ----
	steps("8/9 本地校验（不齐不放行）…")
	if err := g.validate(sysPrompt, tpl, exFiles, mp.ExampleCount(), dtype.Type); err != nil {
		return nil, err
	}

	// ---- Step 8.5: 裁判试用与回炉（仅手册模式） ----
	// 放在 validate **之后**：校验是本地硬门（提示词过短直接拒收），先拒掉明显残次品，
	// 免得把 3 轮模型调用花在一份明知不合格的提示词上。
	// 放在 land **之前**：交付的是「最优一轮」那份提示词（见 JudgeReport.Best），
	// 落盘必须在裁判跑完之后，否则磁盘上留的是没验收过的版本。
	if mp != nil && len(mp.Structure.Categories) > 0 {
		steps("8.5/9 裁判独立试用评分（上限 3 轮，不过线按扣分项回炉）…")
		rep := g.judgeLoop(ctx, mp, sysPrompt, func(ctx context.Context, res *JudgeResult, prev string) (string, error) {
			return g.reviseSystemPrompt(ctx, in, attrs, dtype.Type, prev, res)
		}, steps)
		mp.Judge = rep
		if rep.DeliveredPrompt != "" && rep.DeliveredPrompt != sysPrompt {
			// 回炉后的版本可能更短、甚至被模型删掉了范文注入段——它同样要过本地硬门，
			// 否则等于用「裁判说更好」换来一份校验没管过的提示词。
			if err := g.validate(rep.DeliveredPrompt, tpl, exFiles, mp.ExampleCount(), dtype.Type); err != nil {
				steps("8.5/9 回炉版本未过本地校验，保留原版：" + err.Error())
				rep.DeliveredPrompt = ""
			} else {
				sysPrompt = rep.DeliveredPrompt
				steps(fmt.Sprintf("8.5/9 交付第 %d 轮版本（%d 字）", rep.BestRound, len([]rune(sysPrompt))))
			}
		}
	}

	// ---- Step 9: land to disk + register in DB ----
	steps("9/9 落盘并注册…")
	if err := g.land(dir, sysPrompt, tpl, exFiles, in, attrs, dtype, mp); err != nil {
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
		PromptLen: len(sysPrompt), ExampleN: len(exFiles) + mp.ExampleCount(),
		SkillType: dtype.Type, Attachment: dtype.Attachment,
	}, nil
}

// ---- Step 7: validation ----
func (g *Generator) validate(sysPrompt, tpl string, exFiles []string, manualExamples int, typ string) error {
	var missing []string
	sysPrompt = strings.TrimSpace(sysPrompt)
	if len(sysPrompt) < 300 {
		missing = append(missing, "system_prompt.md 过短(<300字符),深度不足")
	}
	// write skills need at least one example范文 anchor; query/template skills
	// are flow-based and don't necessarily have one.
	//
	// manualExamples 必须计入：手册模式下范文由锚点切分产生、落在
	// examples/<分类>/ 而不是 exFiles，少算这一项会让一本明明切出 12 篇原文
	// 范文的手册被判「缺少示例范文」而拒收。
	if typ == model.SkillTypeWrite && len(exFiles)+manualExamples == 0 {
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
func (g *Generator) land(dir, sysPrompt, tpl string, exFiles []string, in *Input, attrs string, dtype *typeOut, mp *manualPack) error {
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
		// 原始字节优先写回原文件：source/ 是素材留档区，管理员要能下载回原始
		// 扫描件做人工核对；OCR 抽出的文本另存为 <stem>.txt。两者分开存，
		// 避免出现「留档只剩文本、溯源时拿不到原件」的情况。
		if len(uf.Raw) > 0 {
			_ = os.WriteFile(filepath.Join(dir, "source", fn), uf.Raw, 0o644)
			stem := strings.TrimSuffix(fn, filepath.Ext(fn))
			_ = os.WriteFile(filepath.Join(dir, "source", stem+".txt"), []byte(uf.Content), 0o644)
			continue
		}
		_ = os.WriteFile(filepath.Join(dir, "source", fn), []byte(uf.Content), 0o644)
	}
	for i, ex := range exFiles {
		fname := fmt.Sprintf("example%02d.md", i+1)
		if err := os.WriteFile(filepath.Join(dir, "examples", fname), []byte(ex), 0o644); err != nil {
			return err
		}
	}
	// 手册模式：范文按分类落进 examples/<分类>/，同时写 categories/ 与 reviewer.md。
	// 放在通用 examples 之后写，是为了让 source/ 与 example01.md 这类通用产物先就位，
	// 分类目录再补上——人工翻目录时的顺序符合「先素材、后结论」的直觉。
	if mp != nil {
		if _, err := mp.WriteTo(dir); err != nil {
			return fmt.Errorf("落盘手册结构: %w", err)
		}
	}
	meta := map[string]any{"name": in.Name, "category": in.Category, "description": in.Description, "created_at": time.Now().Format(time.RFC3339), "generator_version": 1}
	// 手册模式的元信息也落进 meta.json：排查「这个技能到底按哪一类回答」时，
	// 打开 meta.json 就能看到分类清单，不必去反解 system_prompt。
	if mp != nil {
		meta["manual"] = map[string]any{
			"categories":     mp.CategoryNames(),
			"category_count": len(mp.Structure.Categories),
			"example_count":  mp.ExampleCount(),
			"warnings":       mp.Warnings,
		}
	}
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
	// 显式开 JSON 模式（第 4 个参数 true）。本函数的输出经 jsonStringify 直接作为
	// 「特征分析」注入后续所有 prompt，而模型只要在字符串值里写出未转义的英文双引号
	// （例如 "生成一篇"标准"格式的公文"），产出的就是坏 JSON；坏 JSON 不报错，只以
	// 原文形态静默流进下游。实测事故：线上训练挂在 step2 的
	//   `invalid character 'ä' after object key:value pair`
	// —— 'ä' 是 Go 把中文首字节 0xE4 按 latin1 打印出来的，真因是 JSON 语法在中文
	// 字符处崩了，而报错本身看不出模型写坏了哪里。
	out, err := g.llm.Chat(ctx, sys, user, true)
	if err != nil {
		return "", err
	}
	raw := extractJSON(out)
	attrs := jsonStringify(raw)
	if !json.Valid(raw) {
		// 不静默降级：必须留痕。但 attrs 是给模型看的分析文本、不是结构化数据，
		// 原文注入仍可用，所以返回它并记警告，而不是让整条流水线失败。
		log.Printf("[skillgen] 警告：特征分析不是合法 JSON，已回退为原文注入（可能影响生成质量）：%s", snippet(string(raw), 160))
	}
	return attrs, nil
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
	out, err := g.llm.Chat(ctx, sys, corpus, true)
	if err != nil {
		return nil, err
	}
	var t typeOut
	if err := json.Unmarshal([]byte(extractJSON(out)), &t); err != nil {
		return nil, jsonErrDetail("类型判定", out, err)
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
	out, err := g.llm.Chat(ctx, sys, user, true)
	if err != nil {
		return nil, err
	}
	var m metaOut
	if err := json.Unmarshal([]byte(extractJSON(out)), &m); err != nil {
		return nil, jsonErrDetail("step2 元数据", out, err)
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

// reviseSystemPrompt 是 Step 8.5 的回炉动作：拿裁判的扣分项把 system_prompt 改好。
//
// 为什么是「改」而不是「重新生成一份」：重生成会连已经拿满分的部分一起洗掉
// （分类路由表、禁用项、范文注入段都是上一版对了的东西）。用一次模型调用换一份
// 随机重写，那不叫优化，叫掷骰子。所以这里明确要求**只改被扣分的维度**，
// 并且把上一版原文交给模型，让它做差别最小的修订。
//
// reviewer.md 不参与回炉：它是「按手册条款逐条核对」的清单，条款来自手册原文，
// 裁判扣的是稿子/提示词的分，改提示词不该顺手改标尺——那等于让被判的人改考卷。
func (g *Generator) reviseSystemPrompt(ctx context.Context, in *Input, attrs, typ, prev string, res *JudgeResult) (string, error) {
	if res == nil || len(res.Findings) == 0 {
		// 没扣分项就无从改起。宁可停下并让 fidelity.md 写明「回炉失败」，
		// 也不要原样再跑一轮：那会白烧一轮模型调用，还让 trace 看起来很忙。
		return "", fmt.Errorf("裁判未给出扣分项，无法回炉")
	}
	var tpl string
	if typ == model.SkillTypeWrite {
		tpl = g.motherTemplate()
	} else {
		tpl = g.flowMotherTemplate()
	}
	sys := `你是一位顶尖的提示词工程师。这是一次**修订**任务,不是重写任务。
上一版 system_prompt 已经能跑,但它在一份独立评审里丢了分。请只针对评审扣分项做修改,输出完整的新版本。
硬性要求:
- 保留上一版中已经拿满分的部分,不要在无关段落上改写措辞;
- 逐条回应扣分项,并在被改段落里补上评审要求的具体内容;
- 完整覆盖"身份/任务/执行步骤/结构规范/文风与措辞/长度/禁用项/特殊要求";
- 直接输出正文,不要用代码块包裹,不要输出解释,不要写"修改说明"。`
	var b strings.Builder
	b.WriteString("需求:\n" + in.Requirement + "\n\n特征分析:\n" + attrs + "\n\n母模板:\n" + tpl)
	b.WriteString("\n\n【上一版 system_prompt】\n" + prev)
	b.WriteString("\n\n【独立评审扣分项(逐条改掉)】\n")
	for i, f := range res.Findings {
		fmt.Fprintf(&b, "%d. %s\n", i+1, f)
	}
	if res.Total > 0 {
		fmt.Fprintf(&b, "\n本轮总分 %d/100,未过线。\n", res.Total)
	}
	for _, d := range judgeDimsSorted(res) {
		if d.Score < d.Weight {
			fmt.Fprintf(&b, "- %s 丢分 %d/%d：%s\n", d.Label, d.Score, d.Weight, strings.TrimSpace(d.Reason))
		}
	}
	out, err := g.llm.Chat(ctx, sys, b.String())
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

// jsonErrDetail 把「模型输出不是合法 JSON」做成查得动的错误。
//
// 为什么需要它：Go 的 json 解析错误对非 ASCII 字节是按 latin1 打印的，中文首字节
// 0xE4 会显示成 'ä'。所以线上只看得到
//
//	meta json: invalid character 'ä' after object key:value pair
//
// 完全看不出模型把哪里写坏了。这里把原文（rune 安全截断）一起回带出来。
func jsonErrDetail(step, raw string, err error) error {
	return fmt.Errorf("%s：模型输出不是合法 JSON：%w；原文=<<%s>>", step, err, snippet(raw, 400))
}

// snippet 按 rune 截断，避免把半个中文字符切进日志或错误信息。
func snippet(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…（原文共 " + fmt.Sprint(len(r)) + " 字符）"
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
