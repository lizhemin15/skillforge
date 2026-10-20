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
	// liteBodyEmbedChars 是材料正文内嵌进提示词的上限（字符数）。
	// 超长只截断、不报错：极简通道的底线是「先交付一份能用的」，整篇 5 万字公文
	// 塞进去会把每轮上下文额度吃光，反而让模型忽略真正相关的那几段。
	liteBodyEmbedChars = 6000
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

	// SkillType 是**创建者在界面上亲手选的**产物类型（write | query | template）。
	// 空串 = 自动判定（走 liteDecideType 的本地判据）。
	//
	// 为什么这个字段在两个通道里各有一份而不是共用：极速通道（LiteInput）与完整通道
	// （Input）本来就是两套入参——前者只有「素材」，后者要需求/分类/描述。硬抽公共基类
	// 会把两边的字段语义搅在一起，而这里一共就一个字段。
	SkillType string
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

	// Body 是材料的正文（有指南就是指南，没有就是范文拼起来的原文）。
	//
	// 为什么和 Guide 分两个字段：写作型里 Guide 是「硬约束」（字数、结构、禁用词），
	// 下游的校验、保真核对都按规则读它；而查询/模板型要的是「事实来源」，两者在
	// 提示词里的位置完全不同（一个当规则、一个当唯一依据）。混成一个字段，就得靠
	// Type 去猜这段文字当前扮演什么角色，读代码的人永远猜不对。
	Body string

	// Type 是产物类型判定（write | query | template），TypeWhy 是判定理由。
	// 两者必须一起落进 steps 与 meta.json：用户看到「template」才知道自己拿到的是
	// 「发文件的技能」，而不是「写稿的技能」，也才知道该不该点重训。
	Type    string
	TypeWhy string

	// Extracted 是取材阶段从源文件里拆出的内嵌附件（**尚未落盘**）。
	// Attachments 是落盘后的登记（rel/大小/来源），由调用方在 land 之后回填。
	//
	// 为什么要拆成两个字段：A 段的提示词在落盘**之前**就得知道「有哪些附件」才能
	// 写清单、定类型，而那时技能目录还不存在（落盘只能等 land）；等 land 写完再
	// 回头改提示词，就得多写一次文件、还得处理「写了一半失败」的中间态。
	Extracted   []ExtractedAttachment
	Attachments []model.Attachment
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
	// 这一步的文案按类型给：写作型报「指南/范文」是用户能对上的口径；查询/模板型报
	// 「指南 0 字」只会让人以为素材有问题（可这类素材本来就没有指南，报 0 是误导）。
	if mat.Type == model.SkillTypeWrite {
		steps(fmt.Sprintf("② 素材自解析：类型=写作；指南 %d 字（%s；%s），范文 %d 篇（共 %d 字）",
			runeLen(mat.Guide), mat.GuideFrom, mat.How,
			len(mat.Examples), runeLen(strings.Join(mat.Examples, ""))))
	} else {
		txt := fmt.Sprintf("② 素材自解析：类型=%s（%s）；材料正文 %d 字（%s）",
			liteTypeLabel(mat.Type), mat.TypeWhy, runeLen(mat.Body), mat.How)
		if n := len(mat.Extracted); n > 0 {
			txt += fmt.Sprintf("；拆出内嵌附件 %d 个：%s", n, liteAttachNameList(mat.Extracted))
		}
		steps(txt)
	}
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

	// 类型判定是**本地**做的（见 liteDecideType），提示词、校验口径、落盘注册全部
	// 跟着这个类型走：三处只要有一处还写死 write，产物就会出现「提示词是查办件的、
	// 管理端显示是写作技能」这种自相矛盾的技能，用户按类型找技能时会找不到。
	sysPrompt := litePromptFor(mat)
	if err := g.validate(sysPrompt, "", nil, len(mat.Examples), mat.Type); err != nil {
		// 极简通道的提示词是本地模板 + 用户材料直接拼的，走到这里说明材料短到
		// 连模板都撑不起硬门——如实说清是哪一头的问题，别让用户以为是系统坏了。
		return nil, fmt.Errorf("极简创建未过本地校验：%w（极简创建的材料请给足 %d 字以上）", err, liteMinGuideChars)
	}

	mp := &manualPack{
		Structure: &Structure{Categories: []Category{{
			Name:        mat.CatName,
			Trigger:     liteCategoryTrigger(mat.Type, mat.CatName),
			Requirement: liteRequirement(mat),
		}}},
		Examples: map[string][]string{mat.CatName: mat.Examples},
		Paths:    map[string][]string{},
		// 极简模式的审稿清单还没提炼出来（那是阶段 B 的事），此处留空；
		// reviewer.md 缺失时运行时照常执行，只是没有自动核对这一环。
		Reviewer: "",
		Source:   strings.Join(mat.Examples, "\n\n"),
		// 拆出来的内嵌附件挂在这里，land 据此落盘并在 mp.Attachments 里登记。
		// **必须传**：land 内部会优先复用 mp.Extracted（避免落盘用的字节与提示词里
		// 承诺的文件不是同一份）。不传就会重新拆一次，两处结果一旦有细微差异，
		// 用户拿到的就是「提示词说有三份、实际只落了两份」这种当场就穿帮的技能。
		Extracted: mat.Extracted,
		// 极简说明挂在这里，由 writeFidelity 置顶渲染成「这份技能是怎么来的」。
		LiteNote: liteFidelityNote(),
	}
	in2 := &Input{
		Slug: slug, Name: mat.SkillName, Description: liteLocalDescription(mat),
		Category: liteDefaultCategory, Requirement: liteRequirement(mat), Files: base.Files,
	}
	// Attachment（单个）是完整流水线那套「官方模板」字段，极简通道拆出来的多附件走
	// Attachments（数组列 + meta.json）。这里留空，避免同一概念在库里有两个互相打架
	// 的来源——下游只认 Attachments，旧列清了才好回滚（见 store/skill_attachments.go）。
	dtype := &typeOut{Type: mat.Type, Rationale: mat.TypeWhy}
	if err := g.land(dir, sysPrompt, "", nil, in2, "", dtype, mp); err != nil {
		return nil, fmt.Errorf("落盘失败: %w", err)
	}
	// land 里已完成附件落盘与登记，取回来给 steps / meta / DB 用同一份清单。
	mat.Attachments = mp.Attachments
	res := &Result{
		Slug: slug, Name: mat.SkillName, Description: liteLocalDescription(mat),
		Category: liteDefaultCategory,
		Params:   nil, PromptLen: runeLen(sysPrompt), ExampleN: len(mat.Examples),
		// 类型与理由都回给前端：用户看到「查询 + 下发文件 / 素材里拆出 2 个内嵌附件」
		// 才知道自己拿到的是消耗他素材的那件东西，而不是一个长得像写作技能的技能。
		SkillType:   mat.Type,
		Attachments: mat.Attachments,
		Mode:        modeLite,
		// 极简模式**没有**跑裁判验收，标记必须是无条件的：让「未经验收」和
		// 「验收通过」在管理端长得一样，正是这个模块反复踩过的坑。
		Degraded:      true,
		DegradeReason: liteDegradeReason(),
	}
	sk := &model.Skill{
		Slug: slug, Name: mat.SkillName, Description: liteLocalDescription(mat),
		Category: liteDefaultCategory, Version: 1, Enabled: true,
		SkillType: mat.Type,
		// 附件登记必须进技能记录：运行时的下发按 DB 里的清单走（store.Create 从
		// sk.Attachments 编码数组列）。只落盘不登记，就是「文件在磁盘上、用户下载不到」，
		// 而且产物页面看起来一切正常——这类静默错最难被发现，也最像「这功能是假的」。
		Attachments: mat.Attachments,
	}
	if err := g.store.Create(sk, nil); err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}
	if mat.Type == model.SkillTypeWrite {
		steps("③ 已按素材装盘并注册（此刻技能就能用了）：类型=写作；范文 " + fmt.Sprint(len(mat.Examples)) +
			" 篇逐字落 examples/" + safeCatFileName(mat.CatName) + "/，指南原文进 categories/")
	} else {
		steps("③ 已按素材装盘并注册（此刻技能就能用了）：类型=" + liteTypeLabel(mat.Type) +
			"；材料正文进 categories/ 与提示词")
	}
	if len(mat.Attachments) > 0 {
		// 附件单独占一行：它是这类技能的主要交付物，用户最需要在这里就确认「拆到了哪几份」。
		steps("③ 随技能下发的内嵌附件 " + fmt.Sprint(len(mat.Attachments)) + " 份（已落 " +
			AttachDir + "/ 并登记）：" + liteAttachNameList(mat.Extracted))
	}

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
	// 范文为空是**合法**的（查询/模板型素材本来就没有范文），所以先判空再除：
	// `liteExampleSampleChars/len(mat.Examples)` 在 0 篇时是整数除零 panic，
	// 而 panic 恰好发生在新开的「非写作素材」这条路径上——本地能复现一个技能都建不出来。
	if n := len(mat.Examples); n > 0 {
		for i, e := range mat.Examples {
			ex.WriteString(fmt.Sprintf("\n--- 范文 %d ---\n%s\n", i+1, truncateRunes(e, liteExampleSampleChars/n+1)))
		}
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
	// 写作型继续用上面这份提示词（措辞反复调过，不动）；非写作型换一份 schema 说明——
	// 拿写作型的提示词去精炼附件型素材，模型会把「回答依据」写成「写作要求」。
	if mat.Type != model.SkillTypeWrite {
		sys = liteRefineSystemPrompt(mat.Type)
	}
	// 交「材料」而不是交「指南」：查询/模板型素材本就没有写作指南（凭正文与附件成事），
	// 沿用 Guide 会送出一个空字符串，模型只能凭空编这个技能是干嘛的。
	var material strings.Builder
	if mat.Type == model.SkillTypeWrite {
		material.WriteString("## 写作指南（原文）\n")
	} else {
		material.WriteString("## 材料（原文）\n")
	}
	if s := strings.TrimSpace(mat.Body); s != "" {
		material.WriteString(truncateRunes(s, liteGuideSampleChars))
	} else {
		material.WriteString("（源文件没有解析出可用文本）")
	}
	if len(mat.Extracted) == 0 {
		material.WriteString("\n\n## 随技能下发的文件清单\n无\n")
	} else {
		// 清单进精炼输入，模型才可能把「文件名要逐字一致」写进提示词；不给清单而要求
		// 它别编文件名，等于让它猜。
		material.WriteString("\n\n## 随技能下发的文件清单（已落盘，运行时按此下发）\n")
		for _, a := range mat.Extracted {
			material.WriteString("- " + a.Name + "（" + humanSize(a.Size) + "）\n")
		}
	}
	if ex.Len() > 0 {
		material.WriteString("\n## 范文样本（原文已存进技能，供运行时按需注入）\n" + ex.String())
	}
	user := liteRefineUserPrompt(mat, material.String())

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
	sys = strings.TrimRight(sys, "\n") + liteInteractionProtocol(mat.Type, mat.CatName)
	if len([]rune(sys)) < 300 {
		// 精炼版比本地模板还短到过不了硬门：宁可保留 A 段版本，也不交付一份
		// 过不了校验的提示词（本地模板一定 ≥300 字，这是它的下限保证）。
		return fmt.Errorf("精炼后的提示词仅 %d 字，未过校验下限", len([]rune(sys)))
	}
	// 文件名卫生：模型偶尔会在提示词里"承诺"一个技能里并不存在的文件（把材料里提到的
	// 「报名表」当成已落盘的附件来写）。用户照着问却下不到东西，而这个错从产物上完全
	// 看不出来（提示词读起来一切正常）。附件登记是唯一权威：越界就丢弃这次精炼、
	// 保留 A 段按素材直装的版本——A 段永远不会犯这个错，它是本地拼的。
	if bad := liteInventedDocNames(sys, mat); bad != "" {
		return fmt.Errorf("精炼稿提到了技能里没有的文件 %q（附件登记：%s），已保留按素材直装的版本",
			bad, liteAttachOrNone(mat))
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
	// 内嵌附件**在取材阶段**就拆出来（纯本地解析字节，0 次模型调用）：
	// 类型判定要用它（有附件 → 查询 + 下发文件型）、提示词要把文件名写进清单、
	// 门禁要在「既没有指南也没有范文」时靠它决定放宽。三件事都发生在落盘之前。
	//
	// 传一份空 pack：extractFromFiles 只在 pack 里已有结果时当缓存复用，此刻还没有 pack。
	// 落盘时 land 会复用 mat.Extracted 传过去的那一份，保证「提示词里承诺的文件」与
	// 「磁盘上真落的文件」是同一批字节——拆两次就可能不一致（解压路径里的时间戳、
	// 重名去重顺序），用户看到清单是三份、实际只落了两份。
	atts := extractFromFiles(files, &manualPack{})

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

	// 材料正文：没有指南时它是唯一的材料来源（查询/模板型就靠它答问、列要件）。
	body := strings.TrimSpace(guide)
	if body == "" {
		body = strings.TrimSpace(strings.Join(examples, "\n\n"))
	}
	total := 0
	for _, e := range examples {
		total += runeLen(e)
	}
	guideShort := runeLen(guide) < liteMinGuideChars
	examplesShort := len(examples) == 0 || total < liteMinExampleChars

	// 硬门的松紧：**只有**从源文件里拆出内嵌附件时才放宽。
	//
	// 起因是旧门禁只认「指南 + 范文」，等于只支持「写作」这一件事。素材是
	// 「一份制度文件 + 里面内嵌的报名表」时，制度正文既不是写作指南、也没有范文，
	// 旧口径会把一件明明能做好的素材判成「素材不合格」——用户的原话是
	// 「现在都是写作，不能满足我的需求」。
	//
	// 为什么只认「有附件」、不认「材料有正文」：正文够长就放行的话，用户上传一份
	// 只有指南没有范文的素材也会被放行、然后被判成查询型技能——他要的是写作，
	// 拿到的是答问，比直接报错更让人摸不着头脑。放宽必须有一个客观、可核对的依据，
	// 「字节流里真躺着一个附件」就是这个依据。
	//
	// 放宽的代价必须显性付清：判定理由进 steps + meta.json，缺了什么进 Warn。
	// 缺什么说什么，不做静默降级（这是本模块反复踩的坑）。
	// 放宽有两个**客观依据**，满足任一即可：
	//   a. 源文件里真拆出了内嵌附件（附件本身就是交付物）；
	//   b. 创建者在界面上亲手选了非写作型（query/template）。
	//
	// 为什么 (b) 也能放宽，而这跟上面那条「不认正文够长」不冲突：那条反对的是**系统自己
	// 猜**用户意图——正文够长就替他决定「你要的是答问」，于是上传「只有指南没有范文」的
	// 用户被静默改成答问技能。(b) 不是猜，是用户明说的：他要的是答问/发文件，那么
	// 「没找到写作指南」「没找到范文」这两条要求本身就问错了题——查询型技能的素材是
	// 事实来源，不是文风样板。少了 (b)，用户在界面上选了「查询型」却仍被回一句
	// 「没找到够长的写作指南」，等于选了个不起作用的按钮。
	pickedNonWrite := in.SkillType == model.SkillTypeQuery || in.SkillType == model.SkillTypeTemplate
	relaxed := len(atts) > 0 || pickedNonWrite
	if !relaxed && guideShort {
		// 这条错误必须写成「怎么改」：自解析是猜的，用户看到「指南不足」时最需要的
		// 是「怎么告诉我哪段是指南」，而不是再猜一次。
		return nil, fmt.Errorf("没找到够长的写作指南（%d 字，至少要 %d 字）——指南是硬约束的唯一来源，"+
			"缺了它生成的技能会跟素材脱节。如果指南和范文混在同一份文档里，在范文前面单独一行写"+
			"「## 范文」（或「范文一」），或者用一行 --- 把两者分开，就能被准确切开。"+
			"如果你要的本来就不是写作技能（例如「回答这份制度里的问题」「把源文件里的附件发给用户」），"+
			"两个办法：①把上方的产物类型选成「查询型」或「模板型」——那样就不要求写作指南和范文了；"+
			"②把**原始文件**原样上传（docx/PDF 直接传，不要另存成 txt 再传）：源文件里带的附件会被一并拆出来。"+
			"本次判读：%s",
			runeLen(guide), liteMinGuideChars, how)
	}
	if !relaxed && examplesShort {
		return nil, fmt.Errorf("没找到范文（当前 %d 篇 / %d 字，至少要 1 篇合计 %d 字）。"+
			"范文决定了技能学到的是不是你想要的文风。同一份文档里可以把范文放在指南后面，"+
			"在前面加一行「## 范文」或「---」隔开；范文是好几篇时，篇与篇之间也用一行 --- 分开。"+
			"如果你要的本来就不是写作技能（例如「回答这份制度里的问题」「把源文件里的附件发给用户」），"+
			"两个办法：①把上方的产物类型选成「查询型」或「模板型」——那样就不要求写作指南和范文了；"+
			"②把**原始文件**原样上传（docx/PDF 直接传，不要另存成 txt 再传）：源文件里带的附件会被一并拆出来。"+
			"本次判读：%s",
			len(examples), total, liteMinExampleChars, how)
	}
	var relaxNotes []string
	if guideShort {
		relaxNotes = append(relaxNotes, fmt.Sprintf("没有成篇的写作指南（%d 字，低于 %d 字）", runeLen(guide), liteMinGuideChars))
	}
	if examplesShort {
		relaxNotes = append(relaxNotes, fmt.Sprintf("没有可用范文（%d 篇 / %d 字）", len(examples), total))
	}
	if len(relaxNotes) > 0 {
		if warn != "" {
			warn += "；"
		}
		// 放宽依据必须按**实际**来源写。原来这里写死「源文件里确实带着可下发的附件」，
		// 那是当时唯一的放宽依据；现在用户手选非写作型也会走到这里，写死就成了假话——
		// 而这条正是管理端事后追查「这个技能凭什么放宽了」的唯一线索，它不能撒谎。
		basis := "源文件里确实带着可下发的附件"
		typHow := "按素材实际内容判定类型"
		if pickedNonWrite {
			basis = "创建者已明确指定产物类型为「" + liteTypeLabel(in.SkillType) + "」"
			typHow = "按创建者指定的类型"
		}
		warn += "材料里" + strings.Join(relaxNotes, "、") +
			"——本次" + typHow + "后继续生成（这是极简通道对「非写作素材」的放宽：" + basis + "）；" +
			"补齐指南与范文后可在管理端重训成完整技能"
	}

	name, cat := inferLiteNames(in.Name, guide, fromFiles, examples)
	typ, why := liteDecideType(len(atts), guide, len(examples), total, runeLen(body))
	// 用户亲手选了的类型优先，本地判据只在「用户没说」时兜底。
	//
	// 为什么不是「用户选了也仍按判据复核一遍」：判据再准也只是启发式，而用户是拿着
	// 用途来的——同一份素材，他想让技能答问还是出稿，素材本身答不了这个题。反过来
	// 若他要的是查询型而判据给了写作型，产出的技能会在第一次被使用时才露馅。
	if want := strings.TrimSpace(in.SkillType); want != "" {
		typ, why = want, "创建时由用户指定为「"+liteTypeLabel(want)+"」（未走素材自动判定）"
		// 指定「模板型」但素材里一个附件都没拆出来：技能会承诺下发文件而实际拿不出。
		// 不硬失败（可在管理端补传模板源文件后重训），但必须留痕——静默的承诺落差
		// 就是这个技能唯一的缺陷，而它在产物文件里看不见。
		if typ == model.SkillTypeTemplate && len(atts) == 0 {
			if warn != "" {
				warn += "；"
			}
			warn += "已指定为模板型，但当前素材里没有拆出任何可下发的附件——" +
				"用户索要文件时技能拿不出来，请在管理端补传模板源文件后重训"
		}
	}
	if typ != model.SkillTypeWrite && strings.TrimSpace(in.Name) == "" {
		// 非写作型不能顶着「XX写作」的名字：名字是用户唯一一眼能看到的产物说明，
		// 叫「XX写作」而实际只会答问/发文件，等于在名字上撒谎。只在名字是本地推断
		// 出来的时候改——用户自己填的名字，错了也是他的选择。
		if base := strings.TrimSuffix(strings.TrimSpace(name), "写作"); strings.TrimSpace(base) != "" {
			name = strings.TrimSpace(base)
		}
	}
	return &liteMaterial{
		SkillName: name, CatName: cat,
		Guide: guide, GuideFrom: guideFrom, How: how, Warn: warn,
		Examples: examples, Body: body,
		Type: typ, TypeWhy: why, Extracted: atts,
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
func liteInteractionProtocol(typ, catName string) string {
	// 查询/模板型的协议与写作型**不共用**：写作型那五条讲的是「怎么成稿、怎么改稿」，
	// 对答问类技能全是废话；而答问类最要紧的一条（材料没有就直说，不要硬答）在写作型里
	// 根本不存在。共用一份，等于交付一个把「查办件」当「写文章」的技能。
	if typ == model.SkillTypeQuery || typ == model.SkillTypeTemplate {
		p := "\n\n---\n\n## 交互协议（每轮都必须遵守）\n\n" +
			"1. **一次答清**：材料里有依据就直接给结论 + 原文依据，不要只说一句就反问。\n" +
			"2. **缺关键信息才问**：只有当" + catName + "的办理要件（如办理主体、事项类型）在材料里确实找不到依据时才先提问；一次最多 3 条，每条都带上你的默认理解，用户回「就按你的」即可继续。\n" +
			"3. **问过就记住**：用户回答过的信息当既定事实，后续轮次不得再问同一件事，也不要重列一遍通用要件清单。\n"
		if typ == model.SkillTypeTemplate {
			p += "4. **要文件就直接给**：不解释技术细节、不说「请自行下载」，也不把表格里的内容抄成正文代替文件。\n" +
				"5. **没有的别答应**：材料与文件清单里都没有的，如实说这份技能里没有，并指出最接近的一份；不许用「已发送」搪塞。\n"
		} else {
			p += "4. **越界就直说**：用户问的事材料里完全没有、不属于" + catName + "时，说明这份技能只依据这份材料，" +
				"不要拿通用知识硬答。\n"
		}
		return p
	}
	return "\n\n---\n\n## 交互协议（每轮都必须遵守）\n\n" +
		"1. **一次成稿**：信息够就直接交付完整成稿，不要写一段就反问。\n" +
		"2. **缺关键信息才问**：只有当" + catName + "的必备要素（如主体、时间、核心事实）确实没有依据时才先提问；一次最多 3 条，每条都带上你的默认理解，用户回「就按你的」即可继续。\n" +
		"3. **问过就记住**：用户回答过的信息当既定事实，后续轮次不得再问同一件事，也不要重新列一遍通用要素清单。\n" +
		"4. **改稿只动被指出的地方**：用户提出修改时，其余段落保持原样，改完给出全文。\n" +
		"5. **核对是内部动作**：自检在内部完成，结论不写进交付物。\n"
}

// liteLocalDescription 是阶段 A 的描述兜底（B 段会覆盖）。
//
// 按类型给不同说法：描述是技能列表里唯一的说明行，写成「按写作指南撰写」而技能实际
// 只会答问、发文件，用户在自己的技能列表里根本认不出它——而它明明正是他要的那个。
func liteLocalDescription(mat *liteMaterial) string {
	switch mat.Type {
	case model.SkillTypeQuery:
		return "回答" + mat.CatName + "材料里的问题，只依据材料原文（极简模式）"
	case model.SkillTypeTemplate:
		return "回答" + mat.CatName + "材料里的问题，并按需下发随技能的 " +
			fmt.Sprint(len(mat.Extracted)) + " 份表格模板（极简模式）"
	}
	return "按你提供的写作指南与范文撰写" + mat.CatName + "（极简模式）"
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

// ---------------------------------------------------------------------------
// 极简通道：产物类型判定 + 非写作型（查询 / 查询+下发文件）的本地提示词
//
// 背景：极简通道原来只支持「写作」——门禁要指南 + 范文，提示词全是「怎么写得像范文」，
// 类型写死 model.SkillTypeWrite。于是素材是「一份制度文件 + 里面内嵌的报名表」时，
// 用户要的是「能问、能拿表」，拿到的却是一个写作技能（或者干脆被判素材不合格）。
// 下面这组函数是「按素材实际内容定类型」的落地，全部纯本地、0 次模型调用。
// ---------------------------------------------------------------------------

// liteDecideType 判定产物类型，纯本地（0 次模型调用）。
//
// 为什么不复用完整流水线的 detectType：那是一步模型调用，而极简通道的立身之本
// 是「阶段 A 不等模型也能交付」（见文件头不变量）。用一次调用定类型，用户点完按钮的
// 秒级交付就没了。而定类型这件事本地判据比模型更可靠——「拆出内嵌附件」是个字节层面的
// 客观事实，不需要模型「感觉」一下。
//
// 判据（自上而下，先命中先定）：
//  1. 拆出内嵌附件、且素材不构成写作技能 → template（查询 + 下发文件）：
//     源文件里带着要发给用户的文件，技能的核心交付物就是那几个文件。
//  2. 素材构成写作技能（有指南 + 有范文）→ write：即使带了附件也仍是写作技能，
//     附件一并下发。上传「写作指南 + 参考模板」是很常见的组合，不能因为拆出附件
//     就把用户要的写作能力判没了。
//  3. 无附件但材料有正文 → query：能答问；产不出稿件，也不该假装能写。
//  4. 兜底 → write（走到这里说明有指南有范文，见 collectLiteMaterial 的门禁）。
//
// why 是给人看的（进 steps 与 meta.json）：类型判定是这次交付里最需要被解释的决策，
// 用户看到「为什么给我生成一个查询技能」时，答案就在这一行里。
func liteDecideType(attachN int, guide string, exampleN, exampleChars, bodyChars int) (string, string) {
	writing := runeLen(strings.TrimSpace(guide)) >= liteMinGuideChars &&
		exampleN > 0 && exampleChars >= liteMinExampleChars
	// 顺序不是随手排的：**先判写作，再判附件，最后才看正文长度**。
	//
	// 写作必须排第一：Body 在「有指南」时就等于指南本身（见 liteMaterial.Body 的注释），
	// 所以一份正常的写作素材必然满足 bodyChars >= liteMinGuideChars。把正文长度放前面，
	// 每一次正常的写作创建都会被判成查询型——技能列表里看不出异常，只是用户让他写稿时
	// 收到的是一段答问。这条是写测试时抓出来的（lite_type_test.go 的矩阵里有一格红了）。
	switch {
	case writing && attachN > 0:
		return model.SkillTypeWrite, fmt.Sprintf(
			"素材有写作指南与范文（按写作生成），同时拆出 %d 个内嵌附件，会随技能一起下发给用户", attachN)
	case writing:
		return model.SkillTypeWrite, "用户直接提供了写作指南与范文，按「写作」生成"
	case attachN > 0:
		// 附件排在正文长度之前：附件是**客观事实**（字节流里真躺着一份文件），
		// 而「正文够长」只说明材料有内容、不说明该怎么用。两者都不是写作素材时，
		// 以附件定类型——不然用户按提示去要文件，技能却只会答问。
		// 生成成 query 而不是 template：用户侧只有「写作 / 问答」两种模式，
		// 「能下发附件」是问答能力的一部分（运行时按「有没有附件 + 用户有没有点名」发），
		// 不是一个需要单独选的类型。理由里仍写清「会下发文件」，便于管理员核对。
		return model.SkillTypeQuery, fmt.Sprintf(
			"素材里拆出 %d 个内嵌附件，且素材不是写作类（没有成篇的写作指南/范文），按「问答（可下发文件）」生成", attachN)
	case bodyChars >= liteMinGuideChars:
		return model.SkillTypeQuery, fmt.Sprintf(
			"素材有正文（%d 字）但没有成篇的写作指南与范文，按「查询」生成", bodyChars)
	}
	return model.SkillTypeWrite, "素材既没有写作指南范文也没有可下发的附件，按「写作」生成（可在管理端补齐指南与范文后重训）"
}

// liteTypeLabel 把类型码翻成人话，只用在给人看的步骤 / 元数据里。
func liteTypeLabel(typ string) string {
	switch typ {
	case model.SkillTypeQuery:
		return "查询"
	case model.SkillTypeTemplate:
		return "查询 + 下发文件"
	case model.SkillTypeWrite:
		return "写作"
	}
	return typ
}

// liteCategoryTrigger 是分类的触发语（运行时按需求找技能的那句话）。
//
// 写作型沿用「需要撰写X」的老口径；查询型绝不能写「需要撰写」——那会把
// 「这个手续怎么办」这类问题导向一个不会写文章的技能，用户拿到答非所问的回复，
// 而这从产物上完全看不出来（技能列表里只是多了一行字）。
func liteCategoryTrigger(typ, catName string) string {
	switch typ {
	case model.SkillTypeQuery, model.SkillTypeTemplate:
		return "需要查询/了解" + catName + "的办理要求，或索取相关表格模板"
	}
	return "需要撰写" + catName
}

// liteRequirement 是分类里的「硬约束」文本（写进 categories/ 与 meta.json）。
//
// 查询/模板型的硬约束不是文风，而是「只依据材料原文」+「文件怎么给」这两条。它们必须
// 落到这里，而不是只躺在 system_prompt.md 里：管理员在左树里打开这份技能时，看到的
// 应该是它凭什么答、给什么文件——那里空着，他就只能点进去读提示词才知道。
func liteRequirement(mat *liteMaterial) string {
	switch mat.Type {
	case model.SkillTypeQuery:
		return "只依据材料原文回答用户关于「" + mat.CatName + "」的问题：材料里有的照原文答，" +
			"材料里没写的直接说没写，不得编造条款号，也不得凭常识补齐。"
	case model.SkillTypeTemplate:
		return "只依据材料原文回答用户关于「" + mat.CatName + "」的问题，并按用户点名把随技能下发的文件发给他：" +
			"材料里有的照原文答、没写的说没写；文件以技能内的附件登记为准，不得改名或自行虚构文件名。"
	}
	return strings.TrimSpace(mat.Guide)
}

// litePromptFor 按类型挑阶段 A 的本地兜底提示词。
//
// 阶段 B（精炼）会覆盖它，但 B 失败时它是唯一还在线的提示词（极简模式允许 B 失败，
// 见文件头不变量），所以三种类型都得给足、不能是半成品。
func litePromptFor(mat *liteMaterial) string {
	if mat.Type == model.SkillTypeWrite {
		base := liteLocalPrompt(mat.SkillName, mat.CatName, mat.Guide, len(mat.Examples))
		// 写作技能也可能带着附件（「指南 + 参考模板」的组合）：清单必须进提示词，
		// 否则模型根本不知道手上有文件，用户要模板时它只会把表格内容抄一遍当交付物。
		if len(mat.Extracted) > 0 {
			base += liteAttachSection(mat)
		}
		return base
	}
	return liteMaterialPrompt(mat)
}

// liteMaterialPrompt 生成查询/模板型的本地提示词。
//
// 为什么不复用 liteLocalPrompt：写作型模板的骨架是「怎么写得像范文」，查询型的骨架是
// 「说得准 + 文件给到位」，两者的硬约束没有一条重合。硬套写作模板会丢掉「只依据材料
// 原文」这条最要紧的规矩，技能就会开始凭常识编条款——办事场景里最不能接受的一种错。
func liteMaterialPrompt(mat *liteMaterial) string {
	var b strings.Builder
	b.WriteString("# " + mat.SkillName + "\n\n")
	if n := len(mat.Extracted); n > 0 {
		b.WriteString("你是「" + mat.CatName + "」的办事助手。你手上有这份材料的原文，还带着 " +
			fmt.Sprint(n) + " 个可下发的文件。用户来找你通常是两类事：**问材料里的事**、" +
			"**要一份能填的表/模板**。\n\n")
	} else {
		b.WriteString("你是「" + mat.CatName + "」的办事助手。用户来找你，是要**问清楚材料里的事**。\n\n")
	}
	b.WriteString("## 工作方式（硬约束）\n\n")
	b.WriteString("1. **只依据下面「材料原文」回答**：条款、时限、金额、要件、办理地点，材料里怎么写的就怎么说，不得凭常识补齐。\n")
	b.WriteString("2. 材料里没写的，直接说「材料里没写」，并指出最接近的那一节；**不要编条款号**，也不要给一个听起来合理的数字。\n")
	b.WriteString("3. 能一句话答的就别凑三段；用户问哪一条就答哪一条。\n")
	b.WriteString("4. 用户问「我要办某事，需要准备什么」时，先把材料里该事项的**要件清单**照原文列出来" +
		"（有几项列几项），再问他要不要对应的表格。\n")
	b.WriteString("5. 材料里有好几套流程/好几类情形时，先确认他属于哪一类再答；他描述不清就照材料把分类列出来让他选。\n")
	b.WriteString("\n## 材料原文（回答的唯一依据）\n\n")
	b.WriteString(liteEmbedBody(mat.Body))
	b.WriteString(liteAttachSection(mat))
	b.WriteString(liteInteractionProtocol(mat.Type, mat.CatName))
	return b.String()
}

// liteEmbedBody 把材料正文内嵌进提示词。
//
// 内嵌而不是「留给运行时按需注入」：阶段 B 失败时（极简模式允许失败）这份提示词是唯一
// 还在线的产物，材料不在里面，技能就退化成一台只会寒暄的空壳——而用户从产物上只能看到
// 「提示词 800 字、看着挺像样」。
func liteEmbedBody(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return "（材料原文缺失：源文件没有解析出可用文本，请管理员重新上传原始文件）\n"
	}
	if runeLen(body) > liteBodyEmbedChars {
		// 超长只截断，不报错：极简通道的底线是「先交付一份能用的」。整篇 5 万字公文塞进去
		// 会把每一轮的上下文额度吃光，模型反而更容易忽略真正相关的那几段。
		return truncateRunes(body, liteBodyEmbedChars) +
			"\n\n（原文较长，此处截断；材料原文的完整副本已存入技能的 source/ 目录，可下载核对）\n"
	}
	return body + "\n"
}

// liteAttachSection 生成「随技能一起下发的文件」清单段（没有附件时返回空串）。
//
// 为什么清单必须进提示词，而不是只存数据库：下发动作由运行时执行（chat.go 的附件下发），
// 但**对话里的措辞是模型写的**。模型不知道有哪几个文件时会做两件都很糟的事——把表格内容
// 抄成正文（用户要的是能填的表，抄一段文字等于没给），或者说「已为你发送」而系统其实一份
// 都没发。清单进提示词，这两件事就都堵住了。
func liteAttachSection(mat *liteMaterial) string {
	if mat == nil || len(mat.Extracted) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## 随技能下发的文件（用户点名要哪个就发哪个）\n\n")
	for _, a := range mat.Extracted {
		b.WriteString("- " + a.Name + "（" + humanSize(a.Size) + "）\n")
	}
	b.WriteString("\n发文件的规矩：\n")
	b.WriteString("1. 称呼文件名时与上面**逐字一致**：不要翻译、不要改扩展名、不要自己另起一个名字。\n")
	b.WriteString("2. 正文只写一两句「这次发的是哪一份、能用来做什么」；**不要**用文字把表格/条款复述一遍当作交付物。\n")
	b.WriteString("3. 只有上面清单里的文件才可以说「已发送/已附上」；清单里没有的，如实说技能里没有这份，并指出最接近的那一份。\n")
	b.WriteString("4. 用户没点名而要哪份时，先把清单列出来让他挑一行，不要猜着发。\n")
	return b.String()
}

// liteAttachNameList 把内嵌附件压成「报名表.docx(12KB)、汇总表.xlsx(3KB)」这类一行文案，
// 只用于步骤日志（带大小是为了在日志里一眼看出是否拆错了层级）。
func liteAttachNameList(atts []ExtractedAttachment) string {
	parts := make([]string, 0, len(atts))
	for _, a := range atts {
		parts = append(parts, a.Name+"("+humanSize(a.Size)+")")
	}
	return strings.Join(parts, "、")
}

// liteRefineSystemPrompt 是阶段 B 那唯一一次调用的系统提示词，按类型给两份。
//
// 为什么不能共用一份：写作型要求「不得丢掉指南里的硬约束」，查询型要求「不得把材料事实
// 改写成通用常识」，这两条红线不重合。拿写作型的提示词去精炼一份附件型素材，模型会把
// 「回答依据」写成「写作要求」，产物一眼看过去就错位。
func liteRefineSystemPrompt(typ string) string {
	if typ == model.SkillTypeWrite {
		return `你是写作技能的提示词工程师。给你一份写作指南和若干篇范文，产出一个可以直接投产的写作技能定义。只输出 JSON：
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
	}
	return `你是「办事问答」类技能的提示词工程师。给你一份材料原文（可能带若干可下发的文件），产出可以直接投产的助理技能定义。只输出 JSON：
{
  "skill_name": "技能名，不超过 12 字，如「公租房申请咨询」",
  "description": "一句话说明这个技能能回答什么、能给什么文件，不超过 40 字",
  "trigger": "什么需求该用这个技能，不超过 40 字",
  "input_params": [{"name":"英文小写下划线","label":"表单标签","type":"text|textarea|select|number","required":false,"placeholder":"示例","help":"一句提示"}],
  "style_profile_md": "Markdown：回答口径（怎么答、答多长、要不要先列要件清单），不超过 600 字",
  "reviewer_md": "Markdown 回答自检清单，每条以「- [ ] 」开头、能用是/否判定（例：是否只依据材料原文、是否编了材料里没有的条款号、文件名是否与清单逐字一致），不超过 800 字",
  "system_prompt_md": "system prompt 正文，不超过 1500 字：角色 + 只依据材料原文 + 要件清单怎么列 + 文件怎么给 + 交付物卫生"
}
硬性要求：
1. **不得丢掉材料里的任何可答事实**（条款、时限、金额、要件、办理地点）。可以重排结构，不能省略事实。
2. 不得新增材料里没有的规则或常识；材料没写的，要引导成「材料里没写」而不是给一个像样的答案。
3. input_params 只列「回答必须知道、但用户可能忘记提供」的信息项（如办理主体、事项类型），最多 3 项，确实不需要就输出 []。
4. system_prompt_md 里必须保留「只依据材料原文回答、材料没写就说没写、不编条款号」这条硬约束；不要写分类路由，不要要求回复附带自证附录。
5. 文件清单原样保留：**不得新增、改名或猜测文件名**；清单为空时不要凭空提任何文件。
6. 只输出 JSON，不要任何解释。`
}

// liteRefineUserPrompt 把「产出类型 + 判定理由 + 材料」拼成阶段 B 的用户侧提示词。
func liteRefineUserPrompt(mat *liteMaterial, material string) string {
	kind := "查询型：只回答材料里的事，不下发文件"
	if n := len(mat.Extracted); n > 0 {
		kind = fmt.Sprintf("查询 + 下发文件型：回答材料里的事，并在用户点名时下发随技能的 %d 份文件", n)
	}
	if mat.Type == model.SkillTypeWrite {
		kind = "写作型：按指南与范文写作（材料里若带可下发文件，按清单原样保留）"
	}
	return "## 本技能的产出类型（已判定，不要改）\n" + kind +
		"\n\n## 判定理由\n" + mat.TypeWhy + "\n\n" + material
}

// liteDocNameRe 抓提示词里被当成可下载文件提到的正文档名（带常见交付格式后缀）。
var liteDocNameRe = regexp.MustCompile(`[A-Za-z0-9_\p{Han}\-（）()]{1,40}\.(?:docx?|xlsx?|pptx?|pdf|zip|rar|txt|csv)`)

// liteInventedDocNames 返回精炼稿里「技能里没有、材料里也没提过」的文件名，空串表示没问题。
//
// 为什么要查材料原文：材料本身完全可能提到外部文件名（「填写《报名表》后交回」），
// 那是材料的事实、不是模型编的。只有附件登记与材料正文两处都查不到的名字才算虚构。
func liteInventedDocNames(prompt string, mat *liteMaterial) string {
	body := mat.Body
	for _, n := range liteDocNameRe.FindAllString(prompt, -1) {
		if liteInAttachments(n, mat.Attachments) || strings.Contains(body, n) {
			continue
		}
		stem := strings.TrimSuffix(n, filepath.Ext(n))
		// 材料里提过「报名表」而提示词写成「报名表.docx」：是同一份东西的完整写法，
		// 不算虚构。反过来材料里从没出现过的名字才是真编的。
		if stem != "" && strings.Contains(body, stem) {
			continue
		}
		return n
	}
	return ""
}

// liteInAttachments 判断某个名字是否已在附件登记里（忽略大小写，允许带目录前缀的写法）。
func liteInAttachments(name string, regs []model.Attachment) bool {
	for _, r := range regs {
		if strings.EqualFold(strings.TrimSpace(r.Name), name) || strings.HasSuffix(r.Rel, name) {
			return true
		}
	}
	return false
}

// liteAttachOrNone 给出附件登记的一句话清单，用于报错文案。
func liteAttachOrNone(mat *liteMaterial) string {
	if len(mat.Attachments) == 0 {
		return "无"
	}
	ns := make([]string, 0, len(mat.Attachments))
	for _, a := range mat.Attachments {
		ns = append(ns, a.Name)
	}
	return strings.Join(ns, "、")
}
