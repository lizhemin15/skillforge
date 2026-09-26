package skillgen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lizhemin15/skillforge/internal/model"
)

// ===== 极简创建（Lite）=====
//
// 场景（用户原话）：「只要用户提供某个写作的一个指南和范文，就可以一键生成一个
// 敏捷的、具有和用户多轮交互的 skill」。
//
// 与通用流水线（Generate，9 步 / 十几次模型调用）的根本差别不在步骤多少，而在
// **模型调用次数**：这套系统部署在内网，token 生成速度慢，一次调用就是几十秒的
// 静默计时。所以极简通道把「够用的可用性」和「模型润色」拆成两段：
//
//	阶段 A（0 次调用，秒级）：素材就地装盘。范文逐字落 examples/<类型>//NN.md、
//	    指南原文落 categories/ 的「写作要求」、system_prompt 按本地模板拼装。
//	    做完这一步技能就已经**能用**——运行时该注入的要求与范文一应俱全，
//	    分类只有一个所以不需要判定，缺参追问照样走 needs/doubts 链路。
//	阶段 B（1 次调用）：指南 + 范文样本喂给模型，一次拿回技能名/描述/触发场景/
//	    输入项/审稿清单/精炼提示词，覆盖 A 段的模板版本。
//	阶段 C（0 次调用）：本地硬门校验 + 注册。**不跑裁判循环**（judgeLoop 上限
//	    3 轮、每轮输出几千字，在内网就是几分钟），改为在交付物上显性标记
//	    「极简模式 · 未经裁判验收」，并留出用完整流水线重训的口子。
//
// 三条不变量，改这个文件时别破坏：
//  1. 阶段 B 失败**不能**让整次创建失败。A 段产物已经在盘上、已经注册、已经能用；
//     失败只降级为「未经 AI 精炼」并如实报给用户，而不是把他点的这一下退回去。
//  2. 范文只落 examples/<类型名>/ 子目录，**不要**落 examples/ 顶层。顶层 .md 会被
//     store.SystemPrompt 当 few-shot 再注入一遍，同一篇范文在 prompt 里出现两次既
//     浪费内网宝贵的 token，又会让模型以为有两篇不同的范文。
//  3. 分类名一经落盘就**不再更改**。categories/NN-<name>.md 与 examples/<name>/ 的
//     目录名是同一套清洗规则的产物（safeCatFileName），改名要同时搬目录，收益为零、
//     出错率不低，所以 B 段的输出里根本没有这个字段。

const (
	// liteMinGuideChars 是极简通道自己的素材硬门：指南是硬约束的来源，
	// 太短就等同于没有约束，生成出来的技能必然「和用户给的素材没关系」。
	liteMinGuideChars = 40
	// liteMinExampleChars 是范文总长下限：留一篇能看出文风的材料。
	liteMinExampleChars = 100
	// 喂给模型的样本上限。范文是「逐字落盘」的，不需要模型通读一遍再复述，
	// 样本只用来观察文风与结构；内网慢 token 下，输入同样要省。
	liteGuideSampleChars   = 4000
	liteExampleSampleChars = 3000
	// liteDefaultCategory 是极简技能落库时的分类列。极简通道不讲分类体系
	// （一个技能就一类），也没让用户选过，所以固定填 general —— 之前这里错填了
	// in.Name（技能名），用户在名称框里打字就会把「分类」写成技能名。
	liteDefaultCategory = "general"
)

// LiteInput 是极简创建的输入：一份写作指南 + 若干篇范文。
type LiteInput struct {
	// Slug / Name 都可空：Name 为空时从指南标题推断，Slug 为空时按 Name 生成。
	Slug string
	Name string
	// Guide 是粘贴的写作指南正文（优先级高于 GuideFile）。
	Guide string
	// Examples 是粘贴的范文，每项一篇（前端约定用一行 --- 分隔，见 SplitLiteExamples）。
	Examples []string
	// Files 是上传的文件；GuideFile 指名其中哪一个是「指南」，其余都当范文。
	Files     []*UploadedFile
	GuideFile string
}

// liteExamplesSep 是粘贴框里多篇范文的分隔符：单独一行的 3 个及以上短横线。
// 与 Markdown 水平分割线同形——用户在写材料时天然会用它分篇；若改用空行，
// 一篇范文内部的段落会被切开，那是更常见的输入形态。
var liteExamplesSep = regexp.MustCompile(`(?m)^[ \t]*-{3,}[ \t]*$`)

// liteNormalizeNewlines 把 CRLF / 孤立 CR 折成 LF。
//
// 浏览器 textarea 的 value 在不少环境下带 \r\n（HTML 规范允许），粘贴路径不折一下会连撞两个坑：
//  1. 分隔符正则用 (?m)$ 锚行尾，而 `---\r` 的行尾是 \r，`[ 	]*$` 匹配不上 ——
//     「粘贴 3 篇范文」会被当成 1 篇（用户看到「范文只有 1 篇」，却不知为什么）。
//  2. 存进 examples/ 的「逐字保真」范文里混进 \r，字数统计也跟着虚高。
//
// 上传文件不走这里：那条路要求字节级保真（fidelity 要拿 md5 对原文件）。
func liteNormalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// SplitLiteExamples 把「一个粘贴框里的多篇范文」拆成若干篇。
func SplitLiteExamples(s string) []string {
	parts := liteExamplesSep.Split(liteNormalizeNewlines(s), -1)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// liteMaterial 是阶段 A 取材的结果。
type liteMaterial struct {
	SkillName string
	CatName   string
	Guide     string
	Examples  []string
}

// GenerateLite 跑极简通道：本地装盘（0 调用）→ AI 精炼（1 调用）→ 校验注册（0 调用）。
//
// 返回的 Result 里 Mode="lite"，且**始终** Degraded=true ——极简模式没跑裁判验收，
// 前端必须把它显性标出来。这不是故障，是这个模式的固有属性（用户拍板时选的就是
// 「1 次调用 + 明确标记 + 一键升级」）。
func (g *Generator) GenerateLite(ctx context.Context, in *LiteInput, onStep func(string)) (*Result, error) {
	steps := func(s string) {
		if onStep != nil {
			onStep(s)
		}
	}
	if in == nil {
		return nil, errors.New("极简创建：输入为空")
	}

	// ---- 阶段 A-1：素材文本化（0 次模型调用）----
	// 上传件里可能是扫描 PDF，直接当文本读会得到乱码，所以共用完整流程那套 OCR 解析；
	// 同理共用素材门禁：传了文档却一个字都读不出来时必须硬失败，而不是生成一份
	// 「看起来完整、和素材毫无关系」的技能。
	steps("① 读素材（上传件文本化，扫描件走 OCR）…")
	base := &Input{Files: in.Files}
	rep := g.ingestFiles(ctx, base, steps)
	if err := g.enforceMaterialGate(rep); err != nil {
		steps("① ❌ " + err.Error())
		return nil, err
	}

	// ---- 阶段 A-2：取材与命名（0 次调用）----
	mat, err := collectLiteMaterial(in, base.Files)
	if err != nil {
		steps("② ❌ " + err.Error())
		return nil, err
	}
	steps(fmt.Sprintf("② 取材完成：指南 %d 字，范文 %d 篇（共 %d 字）",
		runeLen(mat.Guide), len(mat.Examples), runeLen(strings.Join(mat.Examples, ""))))

	// ---- 阶段 A-3：本地装盘 + 注册（0 次调用，做完就可用）----
	slug := strings.TrimSpace(in.Slug)
	if slug == "" {
		slug = slugify(mat.SkillName)
	}
	if strings.TrimSpace(slug) == "" {
		return nil, errors.New("极简创建：无法生成技能标识（slug）")
	}
	if old, _ := g.store.Get(slug); old != nil {
		return nil, fmt.Errorf("技能标识 %q 已被技能「%s」占用，请换个名字（或在管理端删除后重试）", slug, old.Name)
	}
	dir := filepath.Join(g.skillsDir, slug)

	sysPrompt := liteLocalPrompt(mat.SkillName, mat.CatName, mat.Guide, len(mat.Examples))
	if err := g.validate(sysPrompt, "", nil, len(mat.Examples), model.SkillTypeWrite); err != nil {
		// 极简通道的提示词是本地模板 + 用户指南直接拼的，走到这里说明指南短到
		// 连模板都撑不起硬门——如实说清是哪一头的问题，别让用户以为是系统坏了。
		return nil, fmt.Errorf("极简创建未过本地校验：%w（写作指南请给足%s字以上）", err, fmt.Sprint(liteMinGuideChars))
	}

	mp := &manualPack{
		Structure: &Structure{Categories: []Category{{
			Name:        mat.CatName,
			Trigger:     "需要撰写" + mat.CatName,
			Requirement: mat.Guide,
		}}},
		Examples: map[string][]string{mat.CatName: mat.Examples},
		Paths:    map[string][]string{},
		// 极简模式的审稿清单还没提炼出来（那是阶段 B 的事），此处留空；
		// reviewer.md 缺失时运行时照常写稿，只是没有自动核对这一环。
		Reviewer: "",
		Source:   strings.Join(mat.Examples, "\n\n"),
		// 极简说明挂在这里，由 writeFidelity 置顶渲染成「这份技能是怎么来的」。
		LiteNote: liteFidelityNote(),
	}
	in2 := &Input{
		Slug: slug, Name: mat.SkillName, Description: liteLocalDescription(mat.CatName),
		Category: liteDefaultCategory, Requirement: mat.Guide, Files: base.Files,
	}
	dtype := &typeOut{Type: model.SkillTypeWrite, Rationale: "极简创建：用户直接提供了写作指南与范文"}
	if err := g.land(dir, sysPrompt, "", nil, in2, "", dtype, mp); err != nil {
		return nil, fmt.Errorf("落盘失败: %w", err)
	}
	res := &Result{
		Slug: slug, Name: mat.SkillName, Description: liteLocalDescription(mat.CatName),
		Category: liteDefaultCategory,
		Params:   nil, PromptLen: runeLen(sysPrompt), ExampleN: len(mat.Examples),
		SkillType: model.SkillTypeWrite,
		Mode:      modeLite,
		// 极简模式**没有**跑裁判验收，标记必须是无条件的：让「未经验收」和
		// 「验收通过」在管理端长得一样，正是这个模块反复踩过的坑。
		Degraded:      true,
		DegradeReason: liteDegradeReason(),
	}
	sk := &model.Skill{
		Slug: slug, Name: mat.SkillName, Description: liteLocalDescription(mat.CatName),
		Category: liteDefaultCategory, Version: 1, Enabled: true,
		SkillType: model.SkillTypeWrite,
	}
	if err := g.store.Create(sk, nil); err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}
	steps("③ 已按素材装盘并注册（此刻技能就能用了）：范文 " + fmt.Sprint(len(mat.Examples)) +
		" 篇逐字落 examples/" + safeCatFileName(mat.CatName) + "/，指南原文进 categories/")

	// ---- 阶段 B：1 次模型调用做精炼 ----
	// 这一段失败**不算创建失败**（见文件头不变量 1）：A 段的东西已经落盘注册，
	// 用户拿到的是一份可用但未被 AI 精炼过的技能，如实告诉他即可。
	steps("④ 让 AI 读一遍指南与范文，精炼技能定义（极简模式只做这一次调用）…")
	out, berr := g.refineLite(ctx, mat)
	if berr != nil {
		steps("④ ⚠️ AI 精炼未完成（" + liteShortErr(berr) + "），保留按素材直装的版本——技能已可用，可稍后在管理端点「完整流水线重训」补齐")
		return res, nil
	}
	if werr := g.writeLiteRefinement(dir, mat, out, mp, &res.Params); werr != nil {
		steps("④ ⚠️ 精炼结果落盘失败（" + liteShortErr(werr) + "），保留按素材直装的版本")
		return res, nil
	}

	// ---- 阶段 C：回写元信息 + 收尾（0 次调用）----
	if n := strings.TrimSpace(out.SkillName); n != "" && n != mat.SkillName {
		res.Name = n
	}
	if d := strings.TrimSpace(out.Description); d != "" {
		res.Description = d
	}
	if len(out.InputParams) > 0 {
		if perr := g.store.ReplaceParams(slug, out.InputParams); perr != nil {
			steps("⑤ ⚠️ 表单参数写入失败（" + liteShortErr(perr) + "），技能照常可用，只是没有预填字段")
		} else {
			steps(fmt.Sprintf("⑤ 已写入 %d 个输入项（写稿时会按它们判断「缺什么才问你」）", len(out.InputParams)))
		}
	}
	if uerr := g.store.UpdateMeta(slug, res.Name, res.Description, res.Category); uerr != nil {
		steps("⑤ ⚠️ 元信息回写失败（" + liteShortErr(uerr) + "）")
	}
	// 精炼后的提示词长度要如实回传：前端要显示它，管理员据此判断这份技能「厚不厚」。
	if b, rerr := os.ReadFile(filepath.Join(dir, "system_prompt.md")); rerr == nil {
		res.PromptLen = runeLen(string(b))
	}
	steps("⑥ 精炼完成，技能可用（极简模式未跑裁判验收，已标记）")
	return res, nil
}

// refineLite 是阶段 B 的唯一一次模型调用。
//
// 输入刻意只放**样本**（指南 4000 字、范文合计 3000 字）：范文原文已经逐字落盘、
// 运行时按需注入，模型这一步的职责是「读文风、提炼约束」，不是复述全文。
func (g *Generator) refineLite(ctx context.Context, mat *liteMaterial) (*liteRefineOut, error) {
	var ex strings.Builder
	for i, e := range mat.Examples {
		ex.WriteString(fmt.Sprintf("\n--- 范文 %d ---\n%s\n", i+1, truncateRunes(e, liteExampleSampleChars/len(mat.Examples)+1)))
	}
	sys := `你是写作技能的提示词工程师。给你一份写作指南和若干篇范文，产出一个可以直接投产的写作技能定义。只输出 JSON：
{
  "skill_name": "技能名，不超过 12 字，如「新闻稿写作」",
  "description": "一句话说明这个技能写什么，不超过 40 字",
  "trigger": "什么需求该用这个技能，不超过 40 字",
  "input_params": [{"name":"英文小写下划线","label":"表单标签","type":"text|textarea|select|number","required":true,"placeholder":"示例","help":"一句提示"}],
  "style_profile_md": "Markdown：文风 / 结构骨架 / 长度 / 禁忌，不超过 600 字",
  "reviewer_md": "Markdown 审稿清单，每条以「- [ ] 」开头、能用是/否判定，不超过 800 字",
  "system_prompt_md": "system prompt 正文，不超过 1500 字：角色 + 工作方式 + 硬约束 + 范文用法 + 交付物卫生"
}
硬性要求：
1. **不得丢掉指南里的任何硬约束**（字数上限、必含要素、禁用词、格式要求）。可以压缩措辞，不能省略规则。
2. 不得新增指南与范文里没有的要求。
3. input_params 只列「写这类文章必须知道、但用户可能忘记提供」的信息项，最多 4 项，只把真正必须的标为 required；确实不需要就输出 []。
4. system_prompt_md 里不要写分类路由，不要要求成稿附带核对清单/自证附录/写作过程说明。
5. 只输出 JSON，不要任何解释。`
	user := "## 写作指南（原文）\n" + truncateRunes(mat.Guide, liteGuideSampleChars) +
		"\n\n## 范文样本（原文已存进技能，供运行时按需注入）\n" + ex.String()

	out, err := g.chatWithMaterial(withoutThinking(ctx), sys, user, true)
	if err != nil {
		return nil, err
	}
	var r liteRefineOut
	if err := json.Unmarshal([]byte(extractJSON(out)), &r); err != nil {
		return nil, jsonErrDetail("极简精炼", out, err)
	}
	if strings.TrimSpace(r.SystemPromptMD) == "" {
		return nil, errors.New("模型没有给出提示词正文")
	}
	return &r, nil
}

// writeLiteRefinement 把精炼结果覆盖到 A 段的产物上。
//
// 只覆盖**文本产物**，不碰 examples/<类型>/ 里的范文：范文是逐字落盘的原文，
// 让模型经手一遍等于主动引入改写（用户投诉过的「生成的技能和我给的素材没关系」，
// 一半来自范文被模型重写）。
func (g *Generator) writeLiteRefinement(dir string, mat *liteMaterial, out *liteRefineOut, mp *manualPack, params *[]model.Param) error {
	sys := strings.TrimSpace(out.SystemPromptMD)
	// 交互协议由本地追加（不交给模型）：这五条是「和用户多轮交互」的落点，
	// 模型漏写一次，技能就退化成一次性模板机，且管理员从产物上看不出来。
	sys = strings.TrimRight(sys, "\n") + liteInteractionProtocol(mat.CatName)
	if len([]rune(sys)) < 300 {
		// 精炼版比本地模板还短到过不了硬门：宁可保留 A 段版本，也不交付一份
		// 过不了校验的提示词（本地模板一定 ≥300 字，这是它的下限保证）。
		return fmt.Errorf("精炼后的提示词仅 %d 字，未过校验下限", len([]rune(sys)))
	}
	if err := os.WriteFile(filepath.Join(dir, "system_prompt.md"), []byte(sys), 0o644); err != nil {
		return err
	}
	if s := strings.TrimSpace(out.StyleProfileMD); s != "" {
		if err := os.WriteFile(filepath.Join(dir, "style_profile.md"), []byte(s), 0o644); err != nil {
			return err
		}
	}
	if s := strings.TrimSpace(out.ReviewerMD); s != "" {
		if err := os.WriteFile(filepath.Join(dir, "reviewer.md"), []byte(s), 0o644); err != nil {
			return err
		}
		mp.Reviewer = s
	}
	// Trigger 用精炼结果更新（「什么需求该用这个技能」是运行时路由表的一列，
	// 由模型写出来比「需要撰写X」这种模板句有用得多）；Requirement 仍是指南原文，
	// 不动——硬约束的保真比措辞漂亮重要。
	if t := strings.TrimSpace(out.Trigger); t != "" {
		mp.Structure.Categories[0].Trigger = t
	}
	if _, err := WriteCategories(dir, mp.Structure, mp.Paths); err != nil {
		return err
	}
	*params = out.InputParams
	return g.updateLiteMeta(dir, mp, mat, out)
}

// updateLiteMeta 刷新 meta.json 里的名称/描述与极简标记。
//
// 为什么不复用 land 写的那份：land 落的是 A 段的推断名，精炼后名称可能变了；
// 而 meta.json 是管理员在左树里唯一能一眼看到「这份技能是什么、怎么来的」的地方。
func (g *Generator) updateLiteMeta(dir string, mp *manualPack, mat *liteMaterial, out *liteRefineOut) error {
	p := filepath.Join(dir, "meta.json")
	meta := map[string]any{}
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &meta)
	}
	if n := strings.TrimSpace(out.SkillName); n != "" {
		meta["name"] = n
	}
	if d := strings.TrimSpace(out.Description); d != "" {
		meta["description"] = d
	}
	if t := strings.TrimSpace(out.Trigger); t != "" {
		meta["trigger"] = t
	}
	meta["mode"] = modeLite
	meta["lite_refined_at"] = time.Now().Format(time.RFC3339)
	meta["degraded"] = true
	meta["degrade_reason"] = liteDegradeReason()
	if mp != nil {
		meta["manual"] = map[string]any{
			"categories":     mp.CategoryNames(),
			"category_count": len(mp.Structure.Categories),
			"example_count":  mp.ExampleCount(),
		}
	}
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

// liteRefineOut 是阶段 B 的 JSON 产物。
type liteRefineOut struct {
	SkillName      string        `json:"skill_name"`
	Description    string        `json:"description"`
	Trigger        string        `json:"trigger"`
	InputParams    []model.Param `json:"input_params"`
	StyleProfileMD string        `json:"style_profile_md"`
	ReviewerMD     string        `json:"reviewer_md"`
	SystemPromptMD string        `json:"system_prompt_md"`
}

// collectLiteMaterial 取材：区分指南与范文，命名，过素材硬门。
//
// 取材顺序（也就是范文的注入顺序）：先粘贴的，后上传的（按文件名排序）。
// 与 manualSourceText 同口径的排序是刻意的：范文的「自然前后顺序」在
// categories/*.md 的「参考范文」小节里要跟落盘顺序对得上。
func collectLiteMaterial(in *LiteInput, files []*UploadedFile) (*liteMaterial, error) {
	// 粘贴路径统一折成 LF（见 liteNormalizeNewlines）；上传件保持原字节，
	// fidelity 要拿它跟原文件对 md5，动一个字节就等于毁掉保真声明。
	guide := strings.TrimSpace(liteNormalizeNewlines(in.Guide))
	var examples []string
	for _, e := range in.Examples {
		if t := strings.TrimSpace(liteNormalizeNewlines(e)); t != "" {
			examples = append(examples, t)
		}
	}
	guideName := filepath.Base(strings.TrimSpace(in.GuideFile))
	var fromFiles []*UploadedFile
	for _, f := range files {
		if f == nil {
			continue
		}
		fromFiles = append(fromFiles, f)
	}
	sort.SliceStable(fromFiles, func(i, j int) bool {
		return filepath.Base(fromFiles[i].Filename) < filepath.Base(fromFiles[j].Filename)
	})
	for _, f := range fromFiles {
		c := strings.TrimSpace(f.Content)
		if c == "" {
			continue
		}
		fn := filepath.Base(strings.TrimSpace(f.Filename))
		// 指南文件：粘贴的指南优先，粘贴为空时才用它。
		// （两边都给的情况不合并：合并会把「文档版」和「粘贴版」拼成一份没人写过的
		// 指南，硬约束出现两套说法时模型只会挑一套遵守，管理员却以为自己给了两份。）
		if guide == "" && guideName != "" && fn == guideName {
			guide = c
			continue
		}
		examples = append(examples, c)
	}

	if runeLen(guide) < liteMinGuideChars {
		return nil, fmt.Errorf("写作指南内容不足 %d 字（当前 %d 字）：指南是硬约束的唯一来源，"+
			"太短生成出来的技能会跟素材脱节", liteMinGuideChars, runeLen(guide))
	}
	total := 0
	for _, e := range examples {
		total += runeLen(e)
	}
	if len(examples) == 0 || total < liteMinExampleChars {
		return nil, fmt.Errorf("范文不足：至少需要 1 篇（合计 %d 字以上），当前 %d 篇 / %d 字。"+
			"范文决定了技能学到的是不是你想要的文风，缺了就只能生成通用模板",
			liteMinExampleChars, len(examples), total)
	}

	name, cat := inferLiteNames(in.Name, guide, fromFiles, examples)
	return &liteMaterial{SkillName: name, CatName: cat, Guide: guide, Examples: examples}, nil
}

// liteSuffixWords / litePrefixWords 用于把「XX写作指南」「关于XX的写作要求」这类
// 标题削成干净的文章类型名。
var (
	liteSuffixRe = regexp.MustCompile(`(的)?(写作|撰写|编写|创作)?\s*(指南|指引|要求|规范|标准|细则|说明|手册|要点|技巧|教程|模板|样例|范例|范文)$`)
	litePrefixRe = regexp.MustCompile(`^(关于|有关|本|该|我的|我们的)+`)
	liteTrimRe   = regexp.MustCompile(`[《》【】\[\]（）()"'"'：:，,。.、\s]+`)
)

// liteWrapCut 是包裹标题的成对符号：脱壳时只从两端剥，不动中间（正文里的书名号是内容）。
const liteWrapCut = "《》【】[]（）()「」『』\"'“”‘’"

// inferLiteNames 从材料里推断「技能名」与「文章类型名（分类名）」。
//
// 为什么要推断而不是让用户先填：用户的原话是「只要提供指南和范文」，所以他手上有
// 的是材料、不是技能命名方案。逼他先想名字属于本末倒置——顺手把「XX写作指南.pdf」
// 这种现成的信息用好，比多一个必填框有用。填错了也能在管理端改（B 段还会再修一次）。
func inferLiteNames(userName, guide string, files []*UploadedFile, examples []string) (skillName, catName string) {
	raw := strings.TrimSpace(userName)
	if raw == "" {
		raw = liteTitleFromText(guide)
	}
	if raw == "" {
		for _, f := range files {
			n := filepath.Base(strings.TrimSpace(f.Filename))
			if n == "" || strings.HasPrefix(n, ".") {
				continue
			}
			raw = strings.TrimSuffix(n, filepath.Ext(n))
			break
		}
	}
	cat := cleanLiteTypeName(raw)
	if cat == "" {
		cat = "通用写作"
	}
	if strings.TrimSpace(userName) != "" {
		// 用户填了名字就用他填的（他被问到了就说明他在意），只在没填时才补后缀。
		return strings.TrimSpace(userName), cat
	}
	if strings.Contains(cat, "写作") {
		return cat, strings.TrimSuffix(cat, "写作")
	}
	return cat + "写作", cat
}

// liteTitleFromText 从前 20 行里找标题：优先 Markdown 标题，其次第一行短句。
// 只看头部是有意的——文档中部的小标题（如「一、格式要求」）不是这篇文章的名字。
func liteTitleFromText(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) > 20 {
		lines = lines[:20]
	}
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "#") {
			t = strings.TrimSpace(strings.TrimLeft(t, "#"))
			if len([]rune(t)) >= 2 && len([]rune(t)) <= 30 {
				return t
			}
		}
	}
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		r := []rune(t)
		// 短、且不像句子（不以标点收尾）才当标题用。
		if len(r) >= 2 && len(r) <= 20 && !strings.ContainsAny(t, "。！？；") {
			return t
		}
		break
	}
	return ""
}

// cleanLiteTypeName 把标题削成文章类型名：「公司新闻通稿写作指南」→「公司新闻通稿」。
func cleanLiteTypeName(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "（("); i > 0 {
		s = s[:i] // 括号里通常是单位名/日期，不是类型名
	}
	// 先脱壳、再削尾缀：`【周报写作指南】` 这种带壳标题的行尾是「】」，
	// 尾缀正则的 $ 锚不到「指南」，于是「指南」被当成类型名留下来 ——
	// 分类名、examples/ 与 categories/ 的目录名会一起歪成「周报写作指南」，
	// 而 AI 精炼阶段又给出「周报写作」，两处对不上，用户看到的是自相矛盾的名字。
	shelled := strings.Trim(s, liteWrapCut)
	s = litePrefixRe.ReplaceAllString(shelled, "")
	s = liteSuffixRe.ReplaceAllString(s, "")
	s = liteTrimRe.ReplaceAllString(s, "")
	r := []rune(s)
	if len(r) > 20 {
		r = r[:20]
	}
	s = strings.TrimSpace(string(r))
	// 削得只剩 1 个字（「周报写作指南」被连「写作」一起削掉）不如不削：过短的类型名
	// 在分类路由里容易误命中，退回脱壳后的原名更可读。
	if len([]rune(s)) < 2 {
		if rr := []rune(shelled); len(rr) > 20 {
			s = strings.TrimSpace(string(rr[:20]))
		} else {
			s = strings.TrimSpace(shelled)
		}
	}
	return s
}

// liteLocalPrompt 是阶段 A 的本地提示词模板（≥300 字，是极简通道「秒级可用」的底气）。
//
// 刻意把**指南原文整段**嵌进硬约束一节：指南是用户给的写作规则，任何一次模型改写
// 都可能悄悄丢一条（字数上限、禁用词最容易被抹掉），而这一版不需要模型经手——
// 用户点完按钮的那一秒，硬约束就已经在提示词里了。
func liteLocalPrompt(skillName, catName, guide string, exampleN int) string {
	var b strings.Builder
	b.WriteString("# " + skillName + "\n\n")
	b.WriteString("你是专精于撰写" + catName + "的资深写作专家。用户给你需求与素材，你产出成稿。\n\n")
	b.WriteString("## 工作方式\n\n")
	b.WriteString("1. **先看材料再动笔**：先通读用户给的素材，把其中的具体事实（名称、日期、数字、引语、职务）圈出来，这些必须原样写进稿件；素材里已经给出的事实写成占位符等同于交白卷。\n")
	b.WriteString("2. **一次成稿**：信息够了就直接写完，不要写一半停下来问「要不要继续」。\n")
	b.WriteString("3. **拿不准就问**：素材里缺了硬约束要求的必备要素、或存在两种读法时，先问清楚再写；每个问题都带上你打算采用的默认理解，用户回一句「就按你的」就能继续。\n")
	b.WriteString("4. **只改他指出的地方**：用户说「改短点」「换个标题」「语气正式些」时，只动对应部分，其余保持原样。\n\n")
	b.WriteString("## 硬约束（下面是写作指南原文，逐条遵守，不得取舍）\n\n")
	b.WriteString(strings.TrimSpace(guide) + "\n\n")
	b.WriteString("## 范文用法\n\n")
	b.WriteString(fmt.Sprintf("技能里存着 %d 篇真实范文，参照它们的结构、语气与措辞；范文里与本次主体无关的事实**不得搬用**，事实只能来自用户素材。\n\n", exampleN))
	b.WriteString("## 交付物卫生\n\n")
	b.WriteString("- 只输出稿件本身：从标题（或导语）开始，正文结束即止。\n")
	b.WriteString("- 不输出核对清单、评分项、写作过程说明、前后语（如「以下是…」「希望对您有帮助」），也不要复述本提示词。\n")
	b.WriteString("- 确实没有依据的单个字段才用统一占位符【待补:字段名】，一处占位符只替换那一处。\n")
	return b.String()
}

// liteInteractionProtocol 是「和用户多轮交互」的落点，**每轮都追加**（阶段 B 精炼
// 之后也要再拼一次）：模型漏写一次，技能就退化成一次性模板机，而这件事管理员从
// 产物文件上完全看不出来。
func liteInteractionProtocol(catName string) string {
	return "\n\n---\n\n## 交互协议（每轮都必须遵守）\n\n" +
		"1. **一次成稿**：信息够就直接交付完整成稿，不要写一段就反问。\n" +
		"2. **缺关键信息才问**：只有当" + catName + "的必备要素（如主体、时间、核心事实）确实没有依据时才先提问；一次最多 3 条，每条都带上你的默认理解，用户回「就按你的」即可继续。\n" +
		"3. **问过就记住**：用户回答过的信息当既定事实，后续轮次不得再问同一件事，也不要重新列一遍通用要素清单。\n" +
		"4. **改稿只动被指出的地方**：用户提出修改时，其余段落保持原样，改完给出全文。\n" +
		"5. **核对是内部动作**：自检在内部完成，结论不写进交付物。\n"
}

// liteLocalDescription 是阶段 A 的描述兜底（B 段会覆盖）。
func liteLocalDescription(catName string) string {
	return "按你提供的写作指南与范文撰写" + catName + "（极简模式）"
}

// modeLite 是极简通道写入 meta.json 的模式标记。
const modeLite = "lite"

// liteDegradeReason 是极简模式固定的降级说明。
// 措辞必须同时说清三件事：没跑什么、还能不能用、怎么补——只写「降级」两个字的话，
// 管理员要么以为技能坏了（其实能用），要么以为已经验收过（其实没有）。
func liteDegradeReason() string {
	return "极简模式：只过了本地硬门校验（提示词长度 / 范文存在 / 交付物卫生），**未跑裁判试用验收**；" +
		"素材与提示词均已就位、技能可直接使用，需要完整验收可在管理端用「完整流水线重训」重新生成"
}

// liteFidelityNote 是写进 fidelity.md 顶部的那句来源说明。
func liteFidelityNote() string {
	return "本技能由**极简创建**通道生成：素材（指南 + 范文）由用户直接提供，范文逐字落盘、未经模型改写，" +
		"提示词在 1 次模型调用内完成精炼；未跑裁判试用验收（极简模式的固有取舍，见下方降级说明）。"
}

// truncateRunes 按字符（而非字节）截断，避免把中文字符切成乱码。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n…（样本已截断，全文已存入技能）"
}

// liteShortErr 把错误压成一行短语给进度日志用。
//
// 内网模型的报错经常几百字（带 prompt 回显、整段 JSON），原样贴进日志会把这句话
// 的重点（是「模型超时」还是「解析失败」）挤出屏幕；进度窗口一屏只有十来行，
// 而用户正是靠着它判断该等还是该重试。
func liteShortErr(err error) string {
	if err == nil {
		return ""
	}
	s := strings.Join(strings.Fields(err.Error()), " ")
	r := []rune(s)
	if len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
}

// runeLen 数的是字符数：中文素材按字节算会让「300 字下限」变成 100 字。
func runeLen(s string) int { return len([]rune(s)) }
