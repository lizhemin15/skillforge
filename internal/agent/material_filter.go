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

	if shown := f.displayText(text); shown != "" {
		norm := normalizeMaterial(shown)
		if !f.duplicate(norm) {
			f.seen = materialTail(f.seen+norm, f.seenWin)
			f.lastNorm = norm
			f.lastShown = now
			return []string{shown}
		}
		// 重复的片段按噪声处理：不给用户看第二遍，但它同样是「模型还在动」的证据，
		// 所以下面的心跳逻辑照样管它。
	}

	if now.Sub(f.lastShown) >= f.silence {
		f.lastShown = now // 旁白也算一次「有变化」，重置计时，否则每一片噪声都会刷一句
		return []string{f.narration(now)}
	}
	return nil
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
	runs := chineseRuns(text, f.runMin)
	if len(runs) == 0 {
		return ""
	}
	return strings.Join(runs, "")
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
