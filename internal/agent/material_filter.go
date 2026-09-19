package agent

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

// 这个文件是**纯显示层**：把模型思考链里不该给用户看的片段挡在 ProgressSink 之前。
// 它不碰任何发往模型的东西（system prompt / user message / 请求参数），删掉它
// 只是材料变回原样，模型行为一个字节都不变。
//
// 为什么要有它——线上账本 /tmp/ledger_leg2.log 第 100~152 行（第 1 轮执笔跳，
// 真实 SSE 帧，整段 50 秒）。那 50 秒里「中间材料」滚的是模型**原始的英文自我对话**：
//
//	数字化底座。(178 chars) Total: ~650 Chinese characters. Well over400. - *Con
//	t Check:* "直接输出正文，不要任何解释。" -> Will output only the text. - *Mandatory
//	t: Ensure the exact string "锚串deadbeef" is strictly followed without a
//	ed text.✅ Proceed. Output matches the response. Self-Cor
//	Word count: ~660. Meets all criteria. Direct output. No extra text.
//	 system instructions. Proceeds. Output Generation. *(Done.)*
//
// 而且同一段中文正文片段还在被反复重发（账本 45.5s~50.4s 与 47.6s~49.6s 两段
// 是同一批正文，模型把稿子又念了一遍）。外面看起来就像程序卡死或者模型在乱说。
//
// 判据的演进（这段历史别丢，否则很容易退回「看起来在过滤、其实没测到」的状态）：
//
//	第一版判据是「整片含 ≥8 个汉字就算有信息量」。它是**同义反复**——它只是在
//	复述「汉字够多」这件事，而线上那些半英半中的帧照样过关，例如
//	`t Check:* "直接输出正文，不要任何解释。" -> Will output only the text.`
//	（汉字 10 个 ≥ 8，于是整条英文脚手架一起给用户看）。用变异自证也戳不疼：
//	把判据改成恒真，红的是「汉字够不够」，不是「用户还会不会看到英文」。
//
//	现在的判据换成**先抽段、再判段**：
//	  1. 从这一片里抽出「连续汉字（含中文标点）」的段，段首尾的中文标点去掉；
//	  2. 只有长度 ≥ runMin 个**汉字**（标点不算）的段才拿出来展示；
//	  3. 展示的就是这些段本身，英文脚手架一个字都不带出去。
//
// 于是「用户还会不会看到英文」可以被一条**可自证**的断言守住：展示出来的文本里
// 拉丁字母占比必须 ≤ 20%（见 material_filter_test.go，变异体是「直接返回原文」）。
//
// 为什么不把短碎片（「更具韧性」「口径」这种 4 字、2 字的追加片）攒起来再展示：
// 攒起来会把**噪声帧里的碎中文字**粘成中文乱码（线上噪声帧里夹着「数字化底座」
// 「锚串」这种碎片，攒一攒就成了「数字化底座锚串」），用户看到的是另一种困惑。
// 短中文片段丢掉不影响体感：活性由旁白兜着（silence 到了就出中文旁白）。
type materialFilter struct {
	// now 注入时钟。生产用 time.Now，测试注入假时钟——「静默多久算卡住」这件事
	// 必须能确定性断言；也正因为注入了时钟，这个过滤器**不需要 goroutine 和定时器**
	// （材料链路跑在 SSE 的写循环里，多一个后台 goroutine 就多一处泄漏和串台）。
	now func() time.Time

	// 下面几个是可调判据（测试里的变异自证直接改这里）：
	runMin  int           // 一个中文段至少要有几个**汉字**才值得展示（标点不计）
	simMin  float64       // 与上一条已展示片段的相似度到多少算重复
	dupMin  int           // 归一化后短于这个长度就不做「是不是子串」的重复判定（碎片太短会误杀）
	silence time.Duration // 静默多久没展示就给一句旁白（活性保证）
	seenWin int           // 「最近已展示内容」的归一化窗口长度

	started time.Time // 第一次 Feed 的时刻，旁白里的「已约 Ns」用它算
	// rawSeen = 累计吃进来的思考字数（含被挡掉的英文脚手架）。旁白报的是它，不是秒数：
	// 「秒」是墙钟，用户明确说过跳秒不算动；这个数字随模型每吐一片就涨，是真的进度。
	rawSeen   int
	lastShown time.Time // 最近一次**真的展示了**东西（含旁白）的时刻
	seen      string    // 最近已展示片段的归一化尾窗（判重复用）
	lastNorm  string    // 上一条已展示片段的归一化形式（判相似度用）

	// pend 是「还没攒够、不敢展示」的尾巴（跨 Feed 累积）。
	//
	// 为什么非要它不可——线上账本（2026-09-20，siliconflow Qwen3.6-27B 执笔跳）：
	// provider 的思考链是 **2.4~3 字一片** 流出来的（实测 1024 片 / 2447 字），
	// 而这一版的判据是「**单片**里要有 ≥runMin 个连续汉字」。单片 3 个字永远凑不够，
	// 于是整条思考链被静默丢掉：屏幕上只剩计时器在跳，正是用户投诉的那句
	// 「现在一直卡着计时，用户体验不佳」；实测整轮 79.9s 只展示出 22 帧材料。
	//
	// 而那片思考链里装的是**整篇中文草稿**（120 字/行，逐段写出来）：
	//	*(Para 1: 5Ws)* 2026年9月20日，专注于前沿计算架构…完成人民币X亿元B轮融资
	// 所以正确做法不是放宽单片门槛（放宽了会把英文脚手架里的碎汉字也放出去），
	// 而是**跨片攒、按边界吐**：攒到换行或句末标点才展示，读到的就是完整句子。
	pend []rune
}

// newMaterialFilter 造一个过滤器。now 为 nil 时退回 time.Now。
func newMaterialFilter(now func() time.Time) *materialFilter {
	if now == nil {
		now = time.Now
	}
	return &materialFilter{
		now:     now,
		runMin:  6,
		simMin:  0.8,
		dupMin:  8,
		silence: 2500 * time.Millisecond,
		seenWin: 400,
	}
}

// Feed 吃一片模型思考链，返回 0~1 条要展示给用户的文本。
//
// 返回空不等于出错：绝大多数被丢掉的片段本来就该静默丢掉（英文自我对话、重复的
// 正文重发、长度不够的碎片）。唯一的例外是**静默太久**——那时返回一条短中文旁白，
// 让界面上的计时不是唯一在动的东西。
func (f *materialFilter) Feed(chunk string) []string {
	text := strings.TrimSpace(chunk)
	if text == "" {
		return nil
	}
	now := f.now()
	if f.started.IsZero() {
		f.started = now
		f.lastShown = now
	}
	// 先记账再过滤：被挡掉的英文脚手架同样是「模型在动」的证据，旁白靠它给出真进度。
	f.rawSeen += len([]rune(text))

	// 一次 Feed 可能吐出**多条**材料（抽出来的中文段各自成条）：不合并成一条是
	// 踩过线上才定下的——把隔着英文脚手架的几段接起来，会粘出「更具韧性数字化底座」
	// 这种谁都没说过的假话；而只挑最长的一段又会丢掉短的那段（真实料 45.9s
	// 「模化商用交付阶段。数据中台」就是这么被吃掉的）。分开发，两条都不犯。
	var out []string
	for _, shown := range f.collect(text) {
		norm := normalizeMaterial(shown)
		if f.duplicate(norm) {
			// 重复的片段按噪声处理：不给用户看第二遍，但它同样是「模型还在动」的证据，
			// 所以下面的心跳逻辑照样管它。
			continue
		}
		f.seen = materialTail(f.seen+norm, f.seenWin)
		f.lastNorm = norm
		f.lastShown = now
		out = append(out, shown)
	}
	if len(out) > 0 {
		return out
	}

	if now.Sub(f.lastShown) >= f.silence {
		f.lastShown = now // 旁白也算一次「有变化」，重置计时，否则每一片噪声都会刷一句
		return []string{f.narration(now)}
	}
	return nil
}

// collect 把这一片接进尾巴，再按**边界**把能展示的中文段吐出来。
//
// 三条「敢吐」的判据（任一成立即可）：
//
//	① 尾巴里出现换行——思考链是按行组织的（草稿一行一段），行到齐就能整行读；
//	② 尾巴里出现句末标点——句子写完了，边界安全（吐到最后一个句末标点为止，
//	   后面没写完的那半句接着攒）；
//	③ 这一片**自身**就有 ≥runMin 个汉字——说明它不是被 provider 切碎的 2~3 字小片，
//	   而是当作完整单位送进来的（构思跳的中文条目「· 首段写五要素」、测试里的整句）。
//	   线上思考链实测 2.4~3 字/片，永远走不到这条；踩不到它，就不会再漏出残句。
//
// 攒的边界为什么要卡死在「行/句」而不是「够长就吐」：碎句拼在一起是另一种困惑
// （线上把「数字化底座」和「锚串」两块碎片粘成过一句假话），见文件头那段历史。
func (f *materialFilter) collect(text string) []string {
	f.pend = append(f.pend, []rune(text)...)

	var out []string
	// ① 完整行
	for {
		i := runeIndex(f.pend, '\n')
		if i < 0 {
			break
		}
		line := string(f.pend[:i])
		f.pend = f.pend[i+1:]
		out = append(out, f.materialOf(line)...)
	}
	// ② 行内的句末标点
	if i := lastSentenceEnd(f.pend); i >= 0 {
		head := string(f.pend[:i+1])
		f.pend = f.pend[i+1:]
		out = append(out, f.materialOf(head)...)
	}
	// ③ 整片送进来的够长内容
	if len(out) == 0 && countCJK([]rune(text)) >= f.runMin && len(f.pend) > 0 {
		whole := string(f.pend)
		f.pend = f.pend[:0]
		out = append(out, f.materialOf(whole)...)
	}
	if len(f.pend) > pendCap {
		f.pend = f.pend[len(f.pend)-pendKeep:]
	}
	return out
}

// pendCap / pendKeep：尾巴的封顶与截留。只有「一直凑不出边界」的输入会碰到它
// （纯英文长段落），此时丢掉最老的部分——留着它也只是等着被 overflow 掉。
const (
	pendCap  = 4096
	pendKeep = 512
)

// runeIndex 是 []rune 版的 strings.IndexRune。
func runeIndex(rs []rune, r rune) int {
	for i, x := range rs {
		if x == r {
			return i
		}
	}
	return -1
}

// lastSentenceEnd 返回尾巴里**最后一个**句末标点的下标（没有就 -1）。
//
// 只认句末标点（。！？；），不认逗号顿号：逗号是句内停顿，在逗号处切断还是残句。
// 半角 `.!?;` 不收：思考链里它们几乎只出现在英文脚手架里（`Total: ~650.`），
// 收进来会把英文句子误当成中文句子的边界。
func lastSentenceEnd(rs []rune) int {
	for i := len(rs) - 1; i >= 0; i-- {
		switch rs[i] {
		case '。', '！', '？', '；':
			return i
		}
	}
	return -1
}

// isAlnumASCII 判断一个字符是不是 ASCII 字母或数字（「连接件」的候选字符）。
func isAlnumASCII(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// glueMax 是「连接件」的长度上限：夹在中文中间的短拉丁/数字串最长几个字符，
// 还算这个中文段的一部分、不把段切开。
//
// 定这条是因为线上思考链草稿长这样（实测原文）：
//
//	*(Para 1: 5Ws)* 2026年9月20日，…圆满完成人民币X亿元B轮融资
//
// `2026`、`X`、`B` 都是**内容**，但按「连续汉字才算同一段」的判据，它们每一个
// 都把句子断成两截，用户拿到的是「星澜科技成功完成数亿元」+「轮融资，加速推进」
// 这种残句。取 4 是为了容下 `2026` 这种四位年份，同时把 `deadbeef`（8 位锚串）、
// `chars`、`Total` 这类英文词挡在外面——它们比 4 长，照样是段边界。
const glueMax = 4

// markupRunes 是模型思考链里的 Markdown 排版符号：`**粗体**`、`# 标题`、“ `代码` “、
// `| 表格 |`、`> 引用`。它们既不是内容也不是中文段边界（`**标题**：正文` 里的
// `**` 会把「标题」和「正文」之间的段切碎），所以先抹成空格再抽段。
func stripMarkup(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch r {
		case '*', '#', '`', '_', '|', '>', '~':
			b.WriteRune(' ')
		case '	', '\r':
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// displayText 从一片思考链里抽出**值得展示给用户的那部分**：这一片里所有
// 长度 ≥ runMin 个汉字的中文段，按出现顺序接起来。抽不出来（纯英文自我对话、
// 纯标点、碎片太短）就返回空串，这一片对用户静默。
//
// 为什么是「所有段接起来」而不是「只留最长的那个段」：中文正文里夹着拉丁术语
// 是常态（「公司路线图将重点聚焦AI辅助数据建模…」），ASCII 会把一段中文切成两段。
// 只留最长段会真的丢内容——测试 fixture 里那句的后半截就是这么被吃掉的。
// 片内两段中文之间只隔着一个拉丁术语（十几个字符），接起来读得通；跨片不接
// （每次 Feed 各自展示），所以不会把隔着几十秒的两句话拼成一句。
func (f *materialFilter) displayText(text string) string {
	runs := f.materialOf(text)
	if len(runs) == 0 {
		return ""
	}
	// 单值版本：只返回最长的一段，给「这一片里到底有没有值得展示的中文」这类
	// 无状态断言用（见 material_filter_test.go 的噪声样本检查）。
	// 真正展示走 materialOf（**逐段分别发**），理由见 Feed 里那段注释。
	best := runs[0]
	for _, r := range runs[1:] {
		if countCJK([]rune(r)) > countCJK([]rune(best)) {
			best = r
		}
	}
	return best
}

// materialOf 抽出这段文本里**所有**值得展示的中文段，各自成条。
//
// 为什么不合成一条：段与段之间可能隔着几十个字符的英文脚手架，接起来就是粘假话
// （真实料回归里当场粘出过「更具韧性数字化底座」）；为什么不只留最长的那条：
// 会丢内容（同一份回归里吃掉了「模化商用交付阶段。数据中台」）。分成多条，
// 两个坑都不踩——材料窗口本来就是「按条追加」的（见 chat_trace.go 的 appendMaterial）。
func (f *materialFilter) materialOf(text string) []string {
	return materialRuns(text, f.runMin, glueMax)
}

// materialRuns 抽出「值得展示的中文段」。与 chineseRuns 是同一套「段」的概念
// （汉字 + 中文标点，标点只当连接件、不算长度、段首尾去掉），差别只有一条：
//
//	**夹在中文中间的短拉丁/数字串（≤ glueMax 个字符）算段内内容，不断段。**
//
// 判据为什么必须放宽这一条——线上思考链草稿（实测原文，汉字 33%）：
//
//	*(Para 1: 5Ws)* 2026年9月20日，专注于前沿计算架构…完成人民币X亿元B轮融资
//
// 严格版判据在这里切出四个残句（「年」「月」「日」各自成段，`X`、`B` 各断一刀），
// 用户看到的是「轮融资，加速推进」这种没头没尾的碎片。放宽后这一行原样读得通。
//
// 放宽的**代价**被两道闸门限住：① 连接件最长 4 个字符，英文词（`chars`、`Total`、
// `deadbeef`）依旧断段；② 段首尾的中文标点照旧去掉。所以「用户还会不会看到英文
// 脚手架」这条性质没有被松掉——material_filter_test.go 的拉丁占比 ≤20% 断言照样守着。
//
// chineseRuns 留着不动：它是测试里的**尺子**（TestMaterialFilterRealLedger 用它
// 声称「实现说要展示的段」），尺子和实现必须各写一份，共用就等于自己量自己。
func materialRuns(text string, min, glue int) []string {
	rs := []rune(stripMarkup(text))
	var out []string
	var cur []rune
	flush := func() {
		for len(cur) > 0 && isCJKPunct(cur[0]) {
			cur = cur[1:]
		}
		for len(cur) > 0 && isCJKPunct(cur[len(cur)-1]) {
			cur = cur[:len(cur)-1]
		}
		if countCJK(cur) >= min {
			out = append(out, string(cur))
		}
		cur = cur[:0]
	}
	for i := 0; i < len(rs); {
		if r := rs[i]; isCJK(r) || isCJKPunct(r) {
			cur = append(cur, r)
			i++
			continue
		}
		// 非中文字符：先看它是不是「连接件」——一段短的字母数字，且前后都是中文。
		j := i
		for j < len(rs) && isAlnumASCII(rs[j]) {
			j++
		}
		if j > i && j-i <= glue && countCJK(cur) > 0 && j < len(rs) && (isCJK(rs[j]) || isCJKPunct(rs[j])) {
			cur = append(cur, rs[i:j]...)
			i = j
			continue
		}
		flush()
		i++
	}
	flush()
	return out
}

// chineseRuns 抽出文本里所有长度 ≥ min 个**汉字**的「连续中文段」。
//
// 段的字符集 = 汉字 + 中文标点（标点只做段的“连接件”，不算长度，段首尾的标点
// 会被去掉）：`t Check:* "直接输出正文，不要任何解释。" -> Will output only the text.`
// 抽出来的是 `直接输出正文，不要任何解释`——用户看到的就是干净中文，不再是
// 半英半中。ASCII 引号、括号、空格都是段边界，所以英文脚手架天然被切开。
func chineseRuns(text string, min int) []string {
	var out []string
	var cur []rune
	flush := func() {
		for len(cur) > 0 && isCJKPunct(cur[0]) {
			cur = cur[1:]
		}
		for len(cur) > 0 && isCJKPunct(cur[len(cur)-1]) {
			cur = cur[:len(cur)-1]
		}
		if countCJK(cur) >= min {
			out = append(out, string(cur))
		}
		cur = cur[:0]
	}
	for _, r := range text {
		if isCJK(r) || isCJKPunct(r) {
			cur = append(cur, r)
			continue
		}
		flush()
	}
	flush()
	return out
}

// narration 静默太久时的那句短中文旁白。
//
// 它必须带上**真的在变的数字**，否则用户看到的就只是「跳秒」。用户原话：
// 「中间可以流式输出思考的一些中间材料，现在一直卡着计时，用户体验不佳」——
// 旧版这句是 `模型正在自检措辞…（已约 Ns）`，从 22s 到 48s 连续 8 帧只换秒数，
// 界面上除了墙钟没有一处变化，读起来就是卡死。线上实测那段空窗有 26 秒。
//
// 现在报的是**累计吃进来的思考字数**（rawSeen）：模型每吐一片它就涨，是真的进度，
// 不是把时间换个说法再念一遍。秒数保留在括号里当辅助，不再是唯一变量。
// 思考字数用截断而不是四舍五入：和秒数同一个理由（不让用户觉得被多算）。
// narrationPrefix 是旁白的固定开头。**识别的唯一来源**：显示层要用它判断窗口尾巴
// 是不是旁白（api.visibleMaterial / traceClock.Thinking），两处各写一份字符串迟早不齐。
const narrationPrefix = "模型思考中…"

// IsMaterialNarration 报告一段材料是不是「静默兜底旁白」（而不是模型产出的中文料）。
// 带不带前导换行都算 —— 旁白本体带 \n 是为了在窗口里做分隔，识别时不该依赖它。
func IsMaterialNarration(s string) bool {
	return strings.HasPrefix(strings.TrimPrefix(s, "\n"), narrationPrefix)
}

func (f *materialFilter) narration(now time.Time) string {
	// 开头这个 \n 是**旁白分隔符**，不是排版：材料窗口是「所有已展示文本的滚动尾巴」，
	// 旁白要是不加分隔符就会和旧旁白叠成一条越来越长的链（线上 dump 实测：
	// `已产出 239 字（已 2s）模型思考中…已产出 571 字（已 5s）…` 一直黏下去），
	// 用户看到的是「同一句话说八遍」而不是「进度在涨」。显示层只取最后一段
	// （见 chat_trace.go 的 snapshot），于是新旁白**替换**旧旁白，链条不会长。
	// 思考链抽出来的中文段永远不含换行（displayText 遇非中文即 flush），
	// 所以「换行 = 旁白」这条约定不会误伤真材料。
	return fmt.Sprintf("\n"+narrationPrefix+"已产出 %d 字（已 %ds）",
		f.rawSeen, int(now.Sub(f.started).Seconds()))
}

// duplicate 判定这一片是不是「刚说过的话」。
//
// 两道判据，对应线上两种重复形态：
//  1. 归一化后是最近已展示内容的子串——模型把同一段正文原样再吐一遍
//     （账本 43.9s 那片就是 39.3s 那片的重发）；
//  2. 与上一条已展示片段相似度 ≥ 0.8——尾部窗口每次只往后挪几个字，
//     相邻两片会高度重合。
//
// 碎片（归一化后 < dupMin 字）不参与判定：两三字的碎片本来就容易是窗口里
// 某处的子串，用它判重等于误杀。
func (f *materialFilter) duplicate(norm string) bool {
	if len([]rune(norm)) < f.dupMin {
		return false
	}
	if f.seen != "" && strings.Contains(f.seen, norm) {
		return true
	}
	if len([]rune(f.lastNorm)) < f.dupMin {
		return false
	}
	return materialSimilarity(f.lastNorm, norm) >= f.simMin
}

// countCJK 数一串 rune 里的**汉字**个数（不含标点）。段长判据只认这个，
// 否则一串「……！！」也会被当成「有信息量的中文」。
func countCJK(rs []rune) int {
	n := 0
	for _, r := range rs {
		if isCJK(r) {
			n++
		}
	}
	return n
}

// isCJK 只认基本汉字区与扩展 A 区：这两处覆盖了中文正文的全部常用字，
// 而 CJK 标点（U+3000 段）不在其中——「。」「，」不该被算成「有信息量的中文」。
func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3400 && r <= 0x4DBF)
}

// isCJKPunct 是中文文本里常见的标点集合。它只做「中文段」的连接件：
// 不计段长，段首尾会被去掉。U+00B7（·）收进来是因为中文人名/书名里常用它
// （「维克多·雨果」），不收会把一个段劈成两半。
func isCJKPunct(r rune) bool {
	switch r {
	case '，', '。', '、', '；', '：', '？', '！',
		'“', '”', '‘', '’', '（', '）', '《', '》', '〈', '〉',
		'【', '】', '「', '」', '『', '』', '—', '…', '～', '　', '·':
		return true
	}
	return false
}

// normalizeMaterial 归一化：只留字母和数字，去掉空白与全部标点符号。
// 账本里的重复片段常常只差一个空格或一个全角逗号，不归一化就判不出来。
// unicode.IsLetter 对汉字为真，所以汉字不会被这条规则削掉。
func normalizeMaterial(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// cjkOnly 只留汉字，去掉标点、空白与全部拉丁字母。用于「展示流有没有吃掉中文」
// 这类断言：显示层本来就会丢掉标点（标点不是内容），断言比的是**汉字序列**。
func cjkOnly(text string) string {
	var b strings.Builder
	for _, r := range text {
		if isCJK(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// latinRuneRatio 数**拉丁字母**（a-z A-Z）在整条文本里占的 rune 比例。断言「展示的文本
// 不许是半英半中」用它当尺子：汉字 10 个、英文 40 个的脚手架帧，比例 0.8，直接判红。
//
// 为什么只数字母、不数数字与英文标点 —— 这两次都是**真的踩到的假红**，不是假想：
//
//	· 收到你的需求（本轮 10408 字）          → 旧口径 0.25，判「噪声帧」
//	模型思考中…已产出 399 字（已 2s）        → 旧口径 0.38，判「挂着英文脚手架」
//
// 两条都是纯中文进度句，唯一的「非汉字」是阿拉伯数字。用户要守的性质是「还会不会看到
// **英文**自我对话」，而「10408」「399」是内容本身，不是英文。把数字算进拉丁，尺子就
// 会把「我在报进度」判成「我在说英文」——弱/错尺子比没尺子更毒。
//
// 收敛口径后仍然抓得住真脚手架：`Here's a thinking process:` / `(178 chars) Total: ~650
// Chinese characters` 这类帧字母依旧压倒性多数，比例远高于 0.2。负向对照见
// material_filter_test.go 的 TestLatinRuneRatioCountsLettersNotDigits。
func latinRuneRatio(text string) float64 {
	total, latin := 0, 0
	for _, r := range text {
		total++
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			latin++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(latin) / float64(total)
}

// cjkStats 数文本里的 CJK 字数与占比。占比按 rune 数算（不是字节数），
// 否则一个汉字 3 字节会把比例算飞。
func cjkStats(text string) (int, float64) {
	if text == "" {
		return 0, 0
	}
	cjk, total := 0, 0
	for _, r := range text {
		total++
		if isCJK(r) {
			cjk++
		}
	}
	if total == 0 {
		return 0, 0
	}
	return cjk, float64(cjk) / float64(total)
}

// materialTail 取字符串尾部窗口（按 rune 数），超长就从左边裁掉。
func materialTail(s string, n int) string {
	if n <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[len(rs)-n:])
}

// materialSimilarity 用最长公共子序列算两条归一化文本的相似度：
// 2*LCS/(|a|+|b|)。片段长度在几百字以内，O(n*m) 完全够用，
// 而它比「公共前缀/后缀」更能抓住「中间插了几个字」的重复。
func materialSimilarity(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 || len(rb) == 0 {
		return 0
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for i := 1; i <= len(ra); i++ {
		for j := range cur {
			cur[j] = 0
		}
		for j := 1; j <= len(rb); j++ {
			if ra[i-1] == rb[j-1] {
				cur[j] = prev[j-1] + 1
			} else if prev[j] > cur[j-1] {
				cur[j] = prev[j]
			} else {
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	lcs := prev[len(rb)]
	return 2 * float64(lcs) / float64(len(ra)+len(rb))
}
