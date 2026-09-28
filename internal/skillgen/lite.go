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
// 场景（用户原话）：「不用分写作指南和范文，这些可能都是混在一个文档里面的……
// 只需要用户给包含写作指南和范文的文档即可，技能创建的时候自行解析」。
// 所以素材侧只剩一个入口：用户给一份（或几份）材料，指南与范文由 SplitLiteMaterial
// 在本地切出来，不要求他先分好。
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
// 四条不变量，改这个文件时别破坏：
//  1. 阶段 B 失败**不能**让整次创建失败。A 段产物已经在盘上、已经注册、已经能用；
//     失败只降级为「未经 AI 精炼」并如实报给用户，而不是把他点的这一下退回去。
//  2. 范文只落 examples/<类型名>/ 子目录，**不要**落 examples/ 顶层。顶层 .md 会被
//     store.SystemPrompt 当 few-shot 再注入一遍，同一篇范文在 prompt 里出现两次既
//     浪费内网宝贵的 token，又会让模型以为有两篇不同的范文。
//  3. 分类名一经落盘就**不再更改**。categories/NN-<name>.md 与 examples/<name>/ 的
//     目录名是同一套清洗规则的产物（safeCatFileName），改名要同时搬目录，收益为零、
//     出错率不低，所以 B 段的输出里根本没有这个字段。
//  4. 「哪段是指南、哪几段是范文」必须**纯本地**切出来（SplitLiteMaterial，0 次调用）。
//     用户已经不区分这两者了，如果靠模型来分，阶段 A 就没法独立成立——而阶段 A 的
//     立身之本恰恰是「不等模型也能交付一份能用的技能」。

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
	// liteGuideMinHits 是「这段文字像不像写作指南」的要求型用词**种类**下限。
	// 取 4 是有意的保守值：一篇成稿里同时出现 4 类以上要求型用词（受众/结构/
	// 字数/禁用词…）的概率很低，而真指南随手就是十几个。
	liteGuideMinHits = 4
	// liteGuideListRatio 是备用判据：要求型用词只凑到 3 类时，再看是不是
	// 「列条款」的排版（指南用「一、」「- 」，成稿是成段行文）。
	liteGuideListRatio = 0.35
)

// LiteInput 是极简创建的输入：一份（或几份）素材，里面同时含写作指南和范文。
// 指南与范文的分界由 SplitLiteMaterial 本地自解析，前端不再要求用户先分好。
type LiteInput struct {
	// Slug / Name 都可空：Name 为空时从指南标题推断，Slug 为空时按 Name 生成。
	Slug string
	Name string
	// Material 是粘贴的素材全文：写作指南与范文**可以混在同一份里**，由
	// SplitLiteMaterial 本地切开，前端不再区分两个输入框。
	Material string
	// Files 是上传的素材文档（可多份）。每一份都当成「一份可能同时含指南和范文的
	// 材料」独立自解析——常见形态有两种，都要吃得下：
	//   a. 一份大文档 = 指南 + 数篇范文（自己带「## 范文」标记或 --- 分隔）；
	//   b. 一个文件夹：一份指南 + 每篇范文各一个文件（没有标记可分）。
	// 所以「哪份是指南」由 collectLiteMaterial 按可信度选，而不是由前端字段指定。
	Files []*UploadedFile
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
// 上传文件也走这里（GenerateLite 在 ingest 之后统一折一次）：自解析是**行级**的
// （分节标记、`---` 分篇），一个 Windows 导出的素材文档里 `---\r` 匹配不上，
// 会把「一个文件里的三篇范文」当成一篇 —— 跟粘贴路径是同一个坑，不能只修一半。
// 原始字节仍留在 UploadedFile.Raw 里，source/ 的留档不受影响；保真核对是对
// mp.Source（由折行后的范文拼成）做的，两者同源，不会因此虚报。
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

// ===== 素材自解析：从「一份材料」里切出指南与范文（0 次模型调用）=====
//
// 用户原话：「不用分写作指南和范文，这些可能都是混在一个文档里面的……只需要用户给
// 包含写作指南和范文的文档即可，技能创建的时候自行解析」。
//
// 切错的代价不对称，规则因此按「可信度从高到低」排：
//   - 漏切（该切没切）：指南里混进范文正文，或范文里混进要求条款。注入内容不干净，
//     但技能仍然「和素材有关」，而且步骤里报的字数/篇数会让管理员看出来。
//   - 错切（把指南从中间劈开）：指南后半截的硬约束**静默消失**，技能看着完整却少了
//     要求 —— 这正是「生成的技能和我给的素材没关系」那类投诉的典型形态。
//
// 所以：只有用户显式写的标记（规则 1）才直接信；靠猜测切（规则 2）必须先过分类器
// 验证；验证不过就退回整篇判型（规则 3），宁可少切也不乱切。切法（how）会跟着
// 步骤日志回到界面上——**猜的过程可见，错切就不再是静默的**。
//
//	规则 1：显式「范文区」分节标记（`## 范文`、`范文一`、`以下为示例`……）→ 在此切开。
//	规则 2：候选边界（宽松标记 + 单独一行的 ---）切开后用分类器验证；通过才用。
//	规则 3：整篇判型——像指南就当指南（范文为空，交给硬门报「缺范文」），
//	        否则整篇当一篇范文（「一个文件一篇范文」是最常见的上传形态）。
func SplitLiteMaterial(text string) (guide string, examples []string, how string) {
	s := liteNormalizeNewlines(text)
	if strings.TrimSpace(s) == "" {
		return "", nil, ""
	}
	lines := strings.Split(s, "\n")

	// 规则 1
	if idx, _, ok := liteFirstMarker(lines, true); ok {
		g := liteGuideText(lines[:idx])
		ex := liteExamplesFromLines(lines[idx:])
		if len(ex) > 0 {
			return g, ex, "按显式「范文」分节标记切开"
		}
		return g, nil, "只看到「范文」分节标记，标记后面没有内容"
	}

	// 规则 2
	groups := liteSplitGroups(lines)
	if len(groups) > 1 {
		cut := 0
		for cut < len(groups) && groups[cut].guideLike {
			cut++
		}
		if cut > 0 && cut < len(groups) {
			hasNonGuide := false
			for _, grp := range groups[cut:] {
				if !grp.guideLike {
					hasNonGuide = true
					break
				}
			}
			if hasNonGuide {
				g := liteJoinTexts(liteGroupTexts(groups[:cut]))
				ex := liteGroupTexts(groups[cut:])
				if g != "" && len(ex) > 0 {
					return g, ex, "按分隔线/分段切开（前半截像要求、后半截像成稿）"
				}
			}
		}
	}

	// 规则 3
	t := strings.TrimSpace(s)
	if liteLooksLikeGuide(t) {
		return t, nil, "整篇判为写作指南（材料里没找到范文部分）"
	}
	if ex := liteGroupTexts(groups); len(ex) > 1 {
		return "", ex, "整篇判为范文（材料里没找到指南部分）"
	}
	return "", []string{t}, "整篇判为一篇范文（材料里没找到指南部分）"
}

// liteReqWords 是「要求型」用词表：指南是下命令的，成稿不是。
// 只做**可数**的近似，不做语义判断——判错了也只会落回硬门报错，不会静默改内容。
var liteReqWords = []string{
	"要求", "必须", "应当", "不得", "禁止", "严禁", "避免", "不宜",
	"受众", "读者", "结构", "篇幅", "字数", "标题", "导语", "结尾", "开头",
	"格式", "排版", "风格", "语气", "语言", "要素", "禁忌", "忌讳", "规范", "定位",
}

// liteListLineRe 命中「列条款」的行首：- / 1. / 一、 /（二）/ 标题。
var liteListLineRe = regexp.MustCompile(`^(?:[-*+•]\s|#{1,6}\s|[0-9]+[.、)）]|[一二三四五六七八九十]+[、.．]|[（(][一二三四五六七八九十0-9]+[）)])`)

// liteLooksLikeGuide 判断一段文字像不像「写作要求」而不是一篇成稿。
func liteLooksLikeGuide(s string) bool {
	hits, listRatio := liteGuideSignals(s)
	return hits >= liteGuideMinHits || (hits >= liteGuideMinHits-1 && listRatio >= liteGuideListRatio)
}

func liteGuideSignals(s string) (hits int, listRatio float64) {
	if strings.TrimSpace(s) == "" {
		return 0, 0
	}
	for _, w := range liteReqWords {
		if strings.Contains(s, w) {
			hits++
		}
	}
	total, listed := 0, 0
	for _, ln := range strings.Split(s, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		total++
		if liteListLineRe.MatchString(t) {
			listed++
		}
	}
	if total > 0 {
		listRatio = float64(listed) / float64(total)
	}
	return hits, listRatio
}

// liteMarkerRe 匹配「范文区」标记词开头。例：范文 / 范文一 / 示例2 / 以下为参考范文 / 附件。
var liteMarkerRe = regexp.MustCompile(`^(?:以下|下面|附|参考|精选|推荐的)?\s*(?:范文|范例|样例|样文|示例|例子|例文|附件|参考文章|参考范文|优秀范文|成稿示例|样张)\s*[0-9一二三四五六七八九十]*\s*(?:如下)?\s*[:：]?\s*`)

// liteMarkerNoiseRe 是标记残句里的排除词。指南自己也会有小节叫「范文的写法 /
// 范文使用说明」——把它当分节线会把指南从中间劈开，只留前半截当指南，
// 这是最坏的一种静默错误（要求凭空少一半），所以标题里出现这些词一律不算标记。
var liteMarkerNoiseRe = regexp.MustCompile(`说明|要求|规范|标准|用法|使用|如何|怎么|怎样|写法|技巧|要点|注意|原则|作用|选择|指引|格式|来源|下载|评析|点评`)

// liteMarkerSentenceRe 是宽松模式（规则 2）的额外排除：残句像句子就不算标记。
var liteMarkerSentenceRe = regexp.MustCompile(`应当|应该|需要|可以|不宜|不要|必须|建议|尽量|的是|中的`)

// liteMarkerAt 判断一行是不是「范文区」分节标记，是则返回标记词之后的残句（多为标题，可为空）。
//
// strict=true 时只认「标题行」或「整行就是一个标记词」两种写法（`## 范文一`、
// `范文一`、`以下为示例`）——用户显式写的，可以直接信。
// strict=false 时额外接受 `范文：某公司年度总结` 这类带短标题的行，但它只在规则 2 里
// 当作**候选**边界使用，劈得对不对由分类器复核。
func liteMarkerAt(line string, strict bool) (ok bool, title string) {
	t := strings.TrimSpace(line)
	if t == "" {
		return false, ""
	}
	heading := strings.HasPrefix(t, "#")
	if heading {
		t = strings.TrimSpace(strings.TrimLeft(t, "#"))
	}
	if t == "" {
		return false, ""
	}
	limit := 40
	if strict {
		limit = 30
	}
	if !heading && len([]rune(t)) > limit {
		return false, ""
	}
	loc := liteMarkerRe.FindStringIndex(t)
	if loc == nil || loc[0] != 0 {
		return false, ""
	}
	rest := strings.TrimSpace(t[loc[1]:])
	rest = strings.TrimSpace(strings.TrimLeft(rest, "：:、-—· "))
	if liteMarkerNoiseRe.MatchString(rest) {
		return false, ""
	}
	if !heading {
		// 非标题行：严格模式要求整行就是标记（残句为空）；宽松模式要求残句短且不像句子。
		if strict {
			return rest == "", ""
		}
		if len([]rune(rest)) > 14 || liteMarkerSentenceRe.MatchString(rest) {
			return false, ""
		}
	}
	return true, rest
}

// liteFirstMarker 找第一个（严格 / 宽松）范文区标记行。
func liteFirstMarker(lines []string, strict bool) (idx int, title string, ok bool) {
	for i, ln := range lines {
		if o, ti := liteMarkerAt(ln, strict); o {
			return i, ti, true
		}
	}
	return -1, "", false
}

// liteGroup 是按候选边界切出来的一个块，并已经判过「像不像指南」。
type liteGroup struct {
	lines     []string
	guideLike bool
}

// liteSplitGroups 在候选边界处切块：宽松标记行、单独一行的 ---。
// 带标题的标记行留在新块的第一行（保住范文标题，也保住「范文是原文子串」这条保真前提）；
// 纯标记行只起分割作用，本身不落进正文。
func liteSplitGroups(lines []string) []liteGroup {
	var groups []liteGroup
	cur := []string{}
	flush := func() {
		if len(cur) > 0 {
			groups = append(groups, liteGroup{lines: cur, guideLike: liteLooksLikeGuide(strings.Join(cur, "\n"))})
			cur = nil
		}
	}
	for _, ln := range lines {
		if ok, title := liteMarkerAt(ln, false); ok {
			flush()
			if title != "" {
				cur = append(cur, ln)
			}
			continue
		}
		if liteExamplesSep.MatchString(ln) {
			flush()
			continue
		}
		cur = append(cur, ln)
	}
	flush()
	return groups
}

// liteGroupTexts 把若干块拼成若干段文本，空块丢掉。
func liteGroupTexts(groups []liteGroup) []string {
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		if t := strings.TrimSpace(strings.Join(g.lines, "\n")); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func liteJoinTexts(parts []string) string {
	keep := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			keep = append(keep, t)
		}
	}
	return strings.Join(keep, "\n\n")
}

// liteGuideText 收尾指南：丢掉紧跟范文区的分隔线/空行（`---` 属于分隔符，
// 不属于任何一篇正文；留在指南尾部会被当成硬约束原文的一部分）。
func liteGuideText(lines []string) string {
	end := len(lines)
	for end > 0 {
		t := strings.TrimSpace(lines[end-1])
		if t == "" || liteExamplesSep.MatchString(t) {
			end--
			continue
		}
		break
	}
	return strings.TrimSpace(strings.Join(lines[:end], "\n"))
}

// liteExamplesFromLines 把「范文区」的行切成若干篇（规则 1 之后的收尾）。
func liteExamplesFromLines(lines []string) []string {
	var out []string
	cur := []string{}
	flush := func() {
		if t := strings.TrimSpace(strings.Join(cur, "\n")); t != "" {
			out = append(out, t)
		}
		cur = nil
	}
	for _, ln := range lines {
		if ok, title := liteMarkerAt(ln, false); ok {
			flush()
			if title != "" {
				cur = append(cur, ln)
			}
			continue
		}
		if liteExamplesSep.MatchString(ln) {
			flush()
			continue
		}
		cur = append(cur, ln)
	}
	flush()
	return out
}

// liteMaterial 是阶段 A 取材的结果。
//
// Guide 与 Examples 是自解析切出来的两堆；GuideFrom / How / Warn 是**切法的体检报告**，
// 会原样进 SSE 步骤日志。自解析本质上是猜的（用户没标记，我们替他标），所以猜的
// 依据和猜出来的样子都必须看得见——错切不再是静默的。
type liteMaterial struct {
	SkillName string
	CatName   string
	Guide     string
	// GuideFrom 是「指南取自哪份材料」（粘贴的素材 / 文件名）。
	GuideFrom string
	// How 是每个材料块各自的切法说明，Warn 是「材料里有不止一份像指南」这类提醒。
	How      string
	Warn     string
	Examples []string
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
	// 文本化之后再折一次换行：自解析是行级的（分节标记 / `---` 分篇），Windows 导出的
	// 文档里 `---\r` 会让「一个文件里三篇范文」被当成一篇。原始字节在 Raw 里不动，
	// source/ 留档照旧（见 liteNormalizeNewlines 注释）。
	for _, f := range base.Files {
		if f != nil {
			f.Content = liteNormalizeNewlines(f.Content)
		}
	}

	// ---- 阶段 A-2：取材与命名（0 次调用）----
	mat, err := collectLiteMaterial(in, base.Files)
	if err != nil {
		steps("② ❌ " + err.Error())
		return nil, err
	}
	steps(fmt.Sprintf("② 素材自解析：指南 %d 字（%s；%s），范文 %d 篇（共 %d 字）",
		runeLen(mat.Guide), mat.GuideFrom, mat.How,
		len(mat.Examples), runeLen(strings.Join(mat.Examples, ""))))
	if mat.Warn != "" {
		steps("② ⚠️ " + mat.Warn)
	}

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

// litePiece 是一份材料（粘贴的一段 / 上传的一个文件）的自解析结果。
type litePiece struct {
	// Src 是人话来源，出错和提示里要直接给用户看（「粘贴的素材」/ 文件名）。
	Src      string
	Guide    string
	Examples []string
	How      string
	// sure 是「这份指南有多可信」。用户显式写了分节标记切出来的最高，靠整篇判型
	// 猜出来的最低；多份材料都给出指南时按它挑一份，而不是按文件名的先后。
	sure int
}

// liteParsePiece 把一份材料切成指南 + 范文，并给出可信度。
func liteParsePiece(src, text string) *litePiece {
	g, ex, how := SplitLiteMaterial(text)
	p := &litePiece{Src: src, Guide: g, Examples: ex, How: how}
	switch {
	case strings.Contains(how, "显式"):
		p.sure = liteSureExplicit
	case g != "" && len(ex) > 0:
		p.sure = liteSureDivided
	case g != "":
		p.sure = liteSureGuessedGuide
	default:
		p.sure = liteSureGuessedExample
	}
	return p
}

const (
	// liteSureExplicit：显式「范文」标记切出来的，最可信。
	liteSureExplicit = 300
	// liteSureDivided：靠分隔线切开、并且两侧都通过了分类器复核。
	liteSureDivided = 200
	// liteSureGuessedGuide：整篇被判成指南。
	liteSureGuessedGuide = 100
	// liteSureGuessedExample：整篇被判成范文（不提供指南）。
	liteSureGuessedExample = 0
)

// collectLiteMaterial 取材：**自解析**出指南与范文，命名，过素材硬门。
//
// 每份材料独立自解析（粘贴的素材 + 每个上传文件），再把结果合并：
//   - 范文：所有材料切出来的范文按顺序拼起来（粘贴的在前，文件按文件名排序）。
//   - 指南：只取**一份**，取可信度最高的；同分取先出现的。
//
// 为什么不合并多份指南：两份指南的硬约束经常互相矛盾（一份 800 字、一份 1500 字），
// 合并出一份「没人写过」的指南，模型只会挑一套遵守，管理员却以为自己给了两份。
//
// 落选的那份**不采用、但也不装作没看见**：它的内容不会进范文（一份要求清单当 few-shot
// 喂进去，模型学出来的就是「一、总体要求……」这种腔调），而是在 Warn 里被点名，
// 让用户自己决定删哪份。静默丢弃和静默合并一样糟——所以「落选」这件事必须出现在
// 步骤日志里。
//
// 排序口径与 manualSourceText 一致（文件名序）：范文在 categories/*.md 的
// 「参考范文」里要跟落盘顺序对得上。
func collectLiteMaterial(in *LiteInput, files []*UploadedFile) (*liteMaterial, error) {
	var pieces []*litePiece
	if t := strings.TrimSpace(liteNormalizeNewlines(in.Material)); t != "" {
		pieces = append(pieces, liteParsePiece("粘贴的素材", t))
	}
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
		pieces = append(pieces, liteParsePiece(filepath.Base(strings.TrimSpace(f.Filename)), c))
	}

	// 选指南：可信度最高的那一份；并列时保留先出现的（粘贴优先、文件按名排序，
	// 都是确定性的，同一次输入切出来的结果可复现）。
	var chosen *litePiece
	for _, p := range pieces {
		if strings.TrimSpace(p.Guide) == "" {
			continue
		}
		if chosen == nil || p.sure > chosen.sure {
			chosen = p
		}
	}

	// 落选的那份也要处理：它自带范文就保留范文，只有指南就整份不用——但必须在
	// Warn 里点名，绝不能让用户以为自己给了两份指南、其实只生效了一份。
	var guide, guideFrom string
	var examples []string
	howParts := make([]string, 0, len(pieces))
	for _, p := range pieces {
		examples = append(examples, p.Examples...)
		howParts = append(howParts, p.Src+"："+p.How)
	}
	var dropped []string
	if chosen != nil {
		guide, guideFrom = chosen.Guide, chosen.Src
		for _, p := range pieces {
			if p != chosen && strings.TrimSpace(p.Guide) != "" && len(p.Examples) == 0 {
				dropped = append(dropped, p.Src)
			}
		}
	}
	warn := ""
	if len(dropped) > 0 {
		warn = fmt.Sprintf("材料里有 %d 份看着都像写作指南：只用了「%s」，%s 没有采用——"+
			"指南只放一份，多份的硬约束（字数、结构）会互相打架，请自己删掉多余的那些",
			len(dropped)+1, guideFrom, strings.Join(dropped, "、"))
	}
	how := strings.Join(howParts, "；")

	if runeLen(guide) < liteMinGuideChars {
		// 这条错误必须写成「怎么改」：自解析是猜的，用户看到「指南不足」时最需要的
		// 是「怎么告诉我哪段是指南」，而不是再猜一次。
		return nil, fmt.Errorf("没找到够长的写作指南（%d 字，至少要 %d 字）——指南是硬约束的唯一来源，"+
			"缺了它生成的技能会跟素材脱节。如果指南和范文混在同一份文档里，在范文前面单独一行写"+
			"「## 范文」（或「范文一」），或者用一行 --- 把两者分开，就能被准确切开。本次判读：%s",
			runeLen(guide), liteMinGuideChars, how)
	}
	total := 0
	for _, e := range examples {
		total += runeLen(e)
	}
	if len(examples) == 0 || total < liteMinExampleChars {
		return nil, fmt.Errorf("没找到范文（当前 %d 篇 / %d 字，至少要 1 篇合计 %d 字）。"+
			"范文决定了技能学到的是不是你想要的文风。同一份文档里可以把范文放在指南后面，"+
			"在前面加一行「## 范文」或「---」隔开；范文是好几篇时，篇与篇之间也用一行 --- 分开。"+
			"本次判读：%s",
			len(examples), total, liteMinExampleChars, how)
	}

	name, cat := inferLiteNames(in.Name, guide, fromFiles, examples)
	return &liteMaterial{
		SkillName: name, CatName: cat,
		Guide: guide, GuideFrom: guideFrom, How: how, Warn: warn,
		Examples: examples,
	}, nil
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
