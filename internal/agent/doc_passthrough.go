package agent

import (
	"fmt"
	"strings"

	"github.com/lizhemin15/skillforge/internal/docgen"
)

// ---------------------------------------------------------------------------
// 「把上一轮的长文整理成 Word」的确定性直通兜底
//
// 背景：用户先让技能写了一篇新闻稿，再说「把上面那篇整理成 Word」。这条路此前
// 完全押在模型自觉上——prompt 里确实把上一轮产物原文塞进去了（见 GenerateDoc
// 里的 ContextBlock 注入），但模型仍然可能把稿子重写成「摘要版」、大幅压缩，
// 甚至另写一篇。用户的原话是「AI 通常没有管之前生成的内容」。
//
// 「整理成 Word」的语义是**搬运**：内容不变，只换容器。既然语义就是搬运，那就
// 不该把「内容会不会走形」交给模型决定。这里加一道确定性闸门：当上一轮产物是
// 长文、用户本轮说的是搬运类指令、而本轮模型产出的文档对上一轮正文覆盖率过低
// （说明它在重写而不是搬运）时，直接丢掉模型给出的具体内容，用上一轮正文原文
// 渲染 Word。
//
// 三条同时成立才动手，是为了不碰正常路径：新话题（无搬运意图）不触发；上一轮
// 只是短文本/闲聊（不是长文）不触发；模型确实老老实实搬运（覆盖率够高）也不
// 触发，仍用模型版本。
// ---------------------------------------------------------------------------

const (
	// passthroughMinArtifactRunes 上一轮产物去空白后至少这么长的「字」才算长文。
	// 门槛取 200，与 isArtifactMsg / ArtifactKind 认产物的门槛保持一致：短答、
	// 闲聊、以及「已为您生成《x.docx》」这类十来字的回执都不是可搬运的正文，
	// 为它们做覆盖校验只会增加误触发的机会。
	passthroughMinArtifactRunes = 200
	// passthroughCoverageMin 覆盖率低于这个值才认为模型在重写而非搬运。
	// 取 0.6：一份被重写/压缩的稿子几乎不可能有六成连续片段原样出现；反过来，
	// 只做了措辞微调（大量原句保留）的稿子能过线，不必夺走模型的输出。
	passthroughCoverageMin = 0.6
	// passthroughSliceRunes 覆盖率判定的连续片段长度（字）。
	// 取 32：这么长的连续中文片段在无关文本里偶合出现的概率可以忽略，这正是
	// 「抗短串巧合」的来源——不能用 strings.Contains(整篇) 或逐字符命中率。
	passthroughSliceRunes = 32
	// passthroughTitleMaxRunes 可被当成标题的行长上限（字）。
	// 标题是短句；正文段落普遍更长，所以用长度而不是靠「第一行」硬切，
	// 免得产物第一行就是一大段正文时把整段变成居中大标题。
	passthroughTitleMaxRunes = 40
)

// passthroughVerbs 是「搬运意图」词表。
//
// 为什么用关键词判定而不是再调一次模型：这道闸门的存在意义就是「模型已经跑偏
// 之后救场」，此时任何依赖模型判断的判据都可能跟着一起跑偏（刚把正文换掉的
// 模型，问它「用户是不是只要搬运」它多半也答「是，我搬了」）。词表是确定性的，
// 能被测试钉死，误判的代价也只是「少改写一次」。
//
// 词表只收「内容不动、只换形式」的说法；「润色/扩写/精简/改一版」这类**要动
// 内容**的词刻意不收——那些场景用户本来就要改动，夺走模型的输出反而是添乱。
var passthroughVerbs = []string{
	// 「整理 / 导出 / 转格式」：用户要的是把已有内容换个容器
	"整理", "归纳成", "导出", "另存为", "存成", "保存成",
	"转成", "转为", "转换成", "转化为", "变成", "弄成",
	"生成word", "word版", "word版本",
	// 「明确要求别动内容」：这类说法本身就是用户对模型改写行为的抱怨
	"保持原样", "保持原文", "保持内容", "原文照搬", "原样", "照搬", "照抄",
	"一字不改", "一个字不改", "不要重写", "别重写", "不要改写", "别改写",
	"不要修改", "不改内容", "不用改内容", "不要动内容", "内容不变",
}

// wantsCarryOver 判断用户本轮请求是否属于「搬运/转格式」意图。
// 去空格后再匹配，「转 成 word」「W ord 版」这类输入也能命中。
func wantsCarryOver(userMsg string) bool {
	s := strings.ToLower(strings.Join(strings.Fields(userMsg), ""))
	if s == "" {
		return false
	}
	for _, kw := range passthroughVerbs {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// stripAllWS 去掉所有空白字符。
//
// 空白在 Word 里是可再生的排版信息，不该参与「内容是否被搬过来」的判定：
// 模型把段落合并、把换行换成空格、把全角空格去掉，内容其实仍在。所以比较
// 之前两边都做一次去空白，比较的是字符内容本身。
func stripAllWS(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r', '\u3000', '\u00a0':
			return -1
		}
		return r
	}, s)
}

// docCoverage 计算「上一轮产物」被「本轮 doc 文本」覆盖的比例，返回 0~1。
//
// 定义（定长切片命中率）：把上一轮产物去空白后得到字符序列 P，把本轮 doc 的
// 全部可见文本（标题 + 表头 + 表格单元格 + 段落）去空白后得到 T；把 P 从头按
// passthroughSliceRunes 切成连续的定长片段，覆盖率 = **在 T 中作为连续子串出现
// 的片段数 / 总片段数**。不足一片的尾巴按「也算一片」处理（整段拿去比），
// 这样短 P 也能得到 0 或 1 的明确结论。
//
// 为什么不逐字符算命中率、也不用 strings.Contains(P, T) 一把梭：
//   - 逐字符命中率会被常见字污染——一份跟原文毫无关系的公文只因共用了
//     「的/公司/2026/工作」这些字就能拿到 0.5 以上，「重写」会被误判成「搬运」；
//   - 整串 Contains 太脆且没有程度之分：模型只改了一个标点就会掉到 0，直接
//     触发兜底，把「微调过但基本忠实」的输出也丢掉，还掩盖了真实覆盖率数字。
//
// 32 字的连续片段保证了「命中」是强证据（不是单字巧合），按片段数归一化又给出
// 可解释的连续分数（能直接写进给用户看的那行提示里）。
func docCoverage(prevText, docText string) float64 {
	p := stripAllWS(prevText)
	if p == "" {
		// 没有可搬运的正文，谈不上覆盖：算满分，别让调用方误触发。
		return 1
	}
	t := stripAllWS(docText)
	pr := []rune(p)
	total := len(pr) / passthroughSliceRunes
	if total == 0 {
		// 比一片还短：整段作为唯一一片比对。
		if strings.Contains(t, string(pr)) {
			return 1
		}
		return 0
	}
	hit := 0
	for i := 0; i+passthroughSliceRunes <= len(pr); i += passthroughSliceRunes {
		if strings.Contains(t, string(pr[i:i+passthroughSliceRunes])) {
			hit++
		}
	}
	return float64(hit) / float64(total)
}

// docTextOf 把一份 doc 规格里的**全部可见文本**拼在一起（标题、表头、每个表格
// 单元格、每个段落），顺序即为渲染顺序。
//
// 必须带上表格文字：判定口径要与「用户最终在文件里能看到什么」一致。只比 parags
// 会两头出错——模型把正文拆进表格原样保留时漏判成「跑偏」，而模型把正文塞进
// 单元格里改写成「摘要表」时又会被表格文字救回高分。
func docTextOf(d docgen.Doc) string {
	parts := make([]string, 0, len(d.Parags)+len(d.Cols)+2)
	if strings.TrimSpace(d.Title) != "" {
		parts = append(parts, d.Title)
	}
	parts = append(parts, d.Cols...)
	for _, row := range d.Rows {
		parts = append(parts, row...)
	}
	parts = append(parts, d.Parags...)
	return strings.Join(parts, "\n")
}

// lastArtifactText 取本会话最近一份产物的**正文原文**。
//
// 来源复用 compact.go 的 splitArtifacts/isArtifactMsg（产物层），不另造一套历史
// 结构：产物层的定义（结构化 KindArtifact + 历史兼容分支）已经承担了「哪条消息
// 是上一轮的产物」这件事，这里再用一套判据只会两边不一致。
func lastArtifactText(history []Message) string {
	artifacts, _ := splitArtifacts(history)
	if len(artifacts) == 0 {
		return ""
	}
	return artifactBodyText(artifacts[len(artifacts)-1].Content)
}

// artifactBodyText 取产物消息中**面向用户的正文**部分。
//
// 产物消息除了正文，尾部还挂着机器用的标记（「已生成规格:{…}」「已填值:…」），
// 那是给下一轮回放的规格 JSON，不是文章内容。若把它当成「上一轮原文」去做覆盖
// 率或直通，用户会在 Word 里看到一段 JSON 被当成正文排进去。
func artifactBodyText(content string) string {
	s := content
	for _, marker := range []string{docSpecMarker, "已填值:"} {
		if i := strings.Index(s, marker); i >= 0 {
			s = s[:i]
		}
	}
	return strings.TrimSpace(s)
}

// buildPassthroughDoc 用上一轮产物原文构造一份确定的 word 规格：首个短行作标题，
// 其余行按原顺序作正文段落，格式强制 word。
//
// 只决定「哪一行当标题」这种结构问题，**不改一个字**：内容原样搬运正是这条路径
// 要保证的事。渲染仍走 docgen.Generate（同一个现有渲染器），不新写渲染器。
func buildPassthroughDoc(prevText string, model docgen.Doc) docgen.Doc {
	title, paras := splitArtifactLines(prevText)
	name := strings.TrimSpace(model.Filename)
	if name == "" {
		name = title
	}
	if name == "" {
		name = "上一轮正文"
	}
	return docgen.Doc{
		Format:   "word", // 需求就是转成 Word：格式强制，不让模型决定
		Filename: name,
		Title:    title,
		Parags:   paras,
	}
}

// splitArtifactLines 把产物原文按换行切成非空行，再从开头三行里挑首个短行当标题。
//
// 为什么只在开头三行里挑、且要求足够短：正文中间的短句（「联系人：张三」
// 「特此通知」）不是标题；产物开头的第一行通常是文章标题，但也可能先来一行
// 空行或一行引语，所以给三行的窗口而不是死认第一行。
func splitArtifactLines(text string) (title string, paras []string) {
	var lines []string
	for _, ln := range strings.Split(text, "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			lines = append(lines, ln)
		}
	}
	if len(lines) == 0 {
		return "", nil
	}
	for i := 0; i < len(lines) && i < 3; i++ {
		if len([]rune(lines[i])) <= passthroughTitleMaxRunes {
			rest := make([]string, 0, len(lines)-1)
			rest = append(rest, lines[:i]...)
			rest = append(rest, lines[i+1:]...)
			return lines[i], rest
		}
	}
	// 没有短行可用（例如产物本身就是一坨长段）：不硬造标题，全都当正文。
	return "", lines
}

// docPassthroughNote 生成给用户看的那行说明。
// 必须让用户看得见：否则「文件里的内容跟模型说的不一样」会被当成 bug 或幻觉，
// 而不是一次可见的兜底。数字（覆盖率）也一起给出，方便用户判断要不要重来。
func docPassthroughNote(cov float64) string {
	return fmt.Sprintf("上一轮正文覆盖率 %.0f%% < %.0f%%，已改用确定性直通渲染，保证内容不被改写。",
		cov*100, passthroughCoverageMin*100)
}

// shouldPassthrough 判定是否需要启用直通兜底，并按需返回兜底说明与覆盖率。
//
// 三条与（AND），缺一不可，理由都在常量注释里：
//  1. 上一轮产物是长文（去空白后 >= 200 字）——短产物/闲聊没有「搬运」可言；
//  2. 本轮用户请求是搬运意图——新话题（「帮我做一份员工信息表」）即使模型覆盖率
//     很低也不能夺走它的输出；
//  3. 本轮 doc 对上一轮正文覆盖率 < 0.6——模型确实在重写而不是搬运。
func shouldPassthrough(prevText string, model docgen.Doc, userMsg string) (bool, float64, string) {
	prev := strings.TrimSpace(prevText)
	if len([]rune(stripAllWS(prev))) < passthroughMinArtifactRunes {
		// 上一轮不是长文：没有可搬运的正文，覆盖率也失去比较对象，记 0（不记 1，
		// 免得像「完美搬运」一样出现在日志里误导排查）。
		return false, 0, ""
	}
	cov := docCoverage(prev, docTextOf(model))
	if !wantsCarryOver(userMsg) {
		// 新话题：即使覆盖率很低也不能夺走模型的输出。覆盖率照常回传，
		// 让调用方/日志看得见「这一轮模型确实没在搬运」，而不是一个伪造的满分。
		return false, cov, ""
	}
	if cov >= passthroughCoverageMin {
		return false, cov, ""
	}
	return true, cov, docPassthroughNote(cov)
}
