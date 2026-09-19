package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 这一组守的是「执笔那 50 秒里，界面上滚的是什么」——纯显示层的行为。
//
// 真实料：/tmp/ledger_leg2.log 第 100~152 行（第 1 轮执笔跳，真实 SSE 帧）。
// 抽出来的 fixture 在 testdata/material_ledger_leg2.tsv，每行 `时间戳<TAB>片段`。
//
// **关于 fixture 的近似与它的局限**（读测试的人必须先看这段，否则会以为 fixture
// 就是原始 SSE 帧）：
//   - 账本里显示的「材料：xxx」是**累积缓冲的尾部窗口**（实测约 70 个字），
//     不是这一帧新增的内容。所以用它之前必须还原「这一帧追加了什么」：
//     对相邻两条取「前一条的后缀 == 后一条的前缀」的最大重叠，重叠之后的部分
//     才算这一帧的追加；同时用「最长公共后缀」这个朴素近似，两者取大。
//   - 局限一：窗口左端被截断时（前一条的开头已被挤出窗口）重叠探测会退化，
//     一个字节都对不上，于是整条窗口被当成「新片段」——落在大段正文中间时这几乎
//     不会发生，但账本里 36.4s、51.0s 那几行确实发生了（前一条尾部挂着
//     "(178 chars)" 这类计数尾巴）。这几行会把已展示过的内容再喂一遍。
//     这**不是** fixture 的缺陷，恰好是线上真实形态（模型确实在重发），
//     而重复抑制本来就该吃掉这种输入。
//   - 局限二：账本是 0.4s 一次的采样，帧与帧之间看不到；时间戳只用于推进注入时钟，
//     不断言真实墙钟，也不会真的等待。
//   - 局限三：账本第 150 行「材料：」后面是空的，抽料时跳过。

// fakeClock 确定性时钟。整个测试不许依赖真实时间，也不许起 goroutine/定时器——
// materialFilter 的活性保证必须是「每次 Feed 时看注入的时钟」这种纯逻辑实现。
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// feedMaterial 按固定间隔喂一串片段，返回展示出来的文本（含旁白）与各自的时刻。
// （不叫 feedAll：jsonpreview_test.go 里已经有一个同名的小工具，同包会撞名。）
func feedMaterial(f *materialFilter, clk *fakeClock, chunks []string, step time.Duration) ([]string, []time.Time) {
	var shown []string
	var at []time.Time
	for _, c := range chunks {
		for _, s := range f.Feed(c) {
			shown = append(shown, s)
			at = append(at, clk.now())
		}
		clk.advance(step)
	}
	return shown, at
}

// ledgerNoiseSamples 是从 /tmp/ledger_leg2.log 第 100~152 行**逐帧**抄下来的、
// 近乎纯英文/拉丁字母的模型自我对话（就是用户抱怨「像卡死/乱说」的那批东西）。
// 注意它们是逐帧原样，不是账本里几帧拼起来的那一大段。
var ledgerNoiseSamples = []string{
	`数字化底座。(178 chars) Total: ~650 Chinese characters. Well over400. - *Con`,
	`deadbeef" is at the start of P1. Unchanged. - All constraints satisfie`,
	`t: Ensure the exact string "锚串deadbeef" is strictly followed without a`,
	`ny punctuation immediately attached if possible, but the prompt `,
	`ed text.✅ Proceed. Output matches the response. Self-Cor`,
	`not a number, but I'll follow it exactly. I'll add a space or punctuat`,
	`ce it at the very beginning as requested. To be safe,`,
	`just paste it exactly at the start. It's fine.`,
	`refined draft. Generating. [Output Generation] -> *Proceeds* *(Self-C`,
	`orrection/Verification during drafting)*:`,
	`will place it exactly as the first characters. `,
	`ct `,
	`Word count: ~660. Meets all criteria. Direct output. No extra text. Matches system instructions`,
	`output. No extra text. Matches system instructions`,
	`(Done.)*`,
	`Proceed. Output matches the response. Self-Correction/Verification during drafting.`,
	`Let me verify... I'll just paste it exactly at the start. It's fi...`,
}

// TestMaterialFilterDropsSelfTalkKeepsHeartbeat 一件事两半：
//  1. 喂一串真实料里的英文自我对话，展示流里**一条噪声都不许有**；
//  2. 同一串喂法下，展示间隔必须 ≤ 2.5s（活性保证：机器不吭声时也要让用户看到
//     「还在动」的旁白），间隔用注入时钟断言，不依赖真实时间。
func TestMaterialFilterDropsSelfTalkKeepsHeartbeat(t *testing.T) {
	clk := newFakeClock()
	f := newMaterialFilter(clk.now)

	shown, at := feedMaterial(f, clk, ledgerNoiseSamples, 500*time.Millisecond)

	// 断言 1a：线上那批英文自我对话，一片都抽不出可展示的中文。
	// 为什么不是「展示流里 != 样本原文」：抽取之后两者本来就不相等，那种断言是空跑。
	// 这里要求的是过滤器对**每一片噪声**都判空——把 displayText 改成返回原文即转红。
	for _, n := range ledgerNoiseSamples {
		if got := f.displayText(n); got != "" {
			t.Errorf("英文自我对话里抽出了可展示文本：%q → %q", logHead(n, 30), logHead(got, 30))
		}
	}

	// 断言 1b：展示出来的非旁白文本必须是**干净中文**——汉字够多，且拉丁字母占比
	// ≤ 20%。这是「用户还会不会看到英文脚手架」的量化尺子。
	// 注意旧判据是「只要含拉丁字母就必须同时含 ≥8 个汉字」，那是同义反复：它只是在
	// 复述实现，而线上那批半英半中的帧（汉字 10 个）照样过关，实测看到的原文是
	// `t Check:* "直接输出正文，不要任何解释。" -> Will output only the text.`。
	beats := 0
	for _, s := range shown {
		if isNarration(s) {
			beats++
			continue
		}
		if ratio := latinRuneRatio(s); ratio > 0.2 {
			t.Errorf("展示的文本仍挂着英文脚手架（拉丁占比 %.0f%%）：%q", ratio*100, s)
		}
		if cjk, _ := cjkStats(s); cjk < runMinDefault {
			t.Errorf("展示的文本不是有信息量的中文：%q（CJK=%d）", s, cjk)
		}
	}
	if beats == 0 {
		t.Errorf("全程噪声、静默远超 2.5s，却一条旁白都没有——界面在这段时间里只会是死的")
	}

	// 断言 2：活性——任何两次展示之间不得超过 2.5s。
	if len(at) < 2 {
		t.Fatalf("展示次数 %d，无法谈间隔", len(at))
	}
	maxGap := time.Duration(0)
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap > maxGap {
			maxGap = gap
		}
	}
	if maxGap > 2500*time.Millisecond {
		t.Errorf("最大展示间隔 %s > 2.5s：静默保护失效", maxGap)
	}
	t.Logf("喂了 %d 片噪声，展示 %d 条（含 %d 条旁白），最大间隔 %s",
		len(ledgerNoiseSamples), len(shown), beats, maxGap)
}

// TestMaterialFilterNeverEatsChinese 反向的守门：**不许过度过滤**。
// 喂进去的中文片段（含 ≥8 个汉字）一条都不能丢；构思跳那种短中文条目
// （「· 首段写五要素」）也必须继续能展示——rawSink 的注释里记着它被静默吃掉的教训。
func TestMaterialFilterNeverEatsChinese(t *testing.T) {
	chunks := []string{
		"针对传统企业在数字化转型中普遍面临的历史系统耦合度高、跨部门数据流转效率低的痛点",
		"公司路线图将重点聚焦AI辅助数据建模与多云混合部署能力的深化，持续迭代产品演进",
		"底层引入流批一体的实时计算引擎，可在双十一大促等高频交易场景中实现秒级数据洞察",
		"业务人员仅需通过拖拽组件即可快速生成定制化数据看板与自动化报表，缩短上线周期",
		"该版本提供了一套标准化的数据治理与集成方案，通过统一数据口径与自动清洗管道",
		"· 首段写五要素",
		"· 结尾必须落回品牌名",
	}
	clk := newFakeClock()
	f := newMaterialFilter(clk.now)
	shown, _ := feedMaterial(f, clk, chunks, 400*time.Millisecond)

	// 比的是**汉字序列**：显示层会丢掉标点（标点不是内容），所以「· 首段写五要素」
	// 展示成「首段写五要素」是预期行为；而少一个汉字就是过度过滤。
	stream := cjkOnly(strings.Join(shown, ""))
	for _, c := range chunks {
		if !strings.Contains(stream, cjkOnly(c)) {
			t.Errorf("有价值的中文片段被吃掉了：%q", c)
		}
	}
	// 每个片段恰好展示一次：重复抑制不许把不同内容判成重复，也不许多展示。
	if len(shown) != len(chunks) {
		t.Errorf("展示 %d 条，喂了 %d 条：有内容被吞或多出旁白", len(shown), len(chunks))
		for _, s := range shown {
			t.Logf("  展示：%q", logHead(s, 40))
		}
	}
}

// TestMaterialFilterSuppressesRepeats 重复抑制：同一段中文正文连喂 5 次，只展示 1 次。
// 线上账本 45.5s~50.4s 与 47.6s~49.6s 就是模型把同一批正文又念了一遍。
func TestMaterialFilterSuppressesRepeats(t *testing.T) {
	body := "随着企业数据资产化管理趋势的加速，数据中台已从单一的技术工具演进为驱动业务决策的核心基础设施"
	clk := newFakeClock()
	f := newMaterialFilter(clk.now)

	shown := 0
	for i := 0; i < 5; i++ {
		for _, s := range f.Feed(body) {
			shown++
			t.Logf("第 %d 次喂入展示了：%q", i+1, s)
		}
		clk.advance(100 * time.Millisecond)
	}
	if shown != 1 {
		t.Errorf("同一段正文连喂 5 次，展示了 %d 次（必须只展示 1 次）", shown)
	}
}

// ledgerRow 是 fixture 的一行：整轮开始后的秒数 + 还原出的「这一帧追加的片段」。
type ledgerRow struct {
	ts   float64
	text string
}

// TestMaterialFilterRealLedger 真实料回归：把账本 100~152 行还原出的 50 片按原时间戳
// 喂进去（时间戳用于推进注入时钟），断言两件事：
//
//	① 展示出来的非旁白文本里没有英文垃圾——判据是「只要含拉丁字母，就必须同时含
//	   ≥8 个汉字」（线上那批英文垃圾全是 0~2 个汉字）。
//	② 那些含 ≥8 个汉字的原始片段一条不少地被展示——判据用「归一化后是展示流的子串」，
//	   因为被重复抑制吃掉的那条内容早已展示过，按子串判定才既守得住「不许丢内容」，
//	   又不跟重复抑制打架。
func TestMaterialFilterRealLedger(t *testing.T) {
	rows := loadLedgerFixture(t, "material_ledger_leg2.tsv")
	if len(rows) < 40 {
		t.Fatalf("fixture 只有 %d 行，抽料脚本没跑对", len(rows))
	}

	clk := newFakeClock()
	f := newMaterialFilter(clk.now)
	// 时间戳是「整轮开始后的秒数」：先落到第一帧，之后按相邻差值推进。
	clk.advance(time.Duration(rows[0].ts * float64(time.Second)))

	var shown []string
	var shownAt []float64
	droppedNoise, droppedDup := 0, 0
	for i, r := range rows {
		if i > 0 {
			d := r.ts - rows[i-1].ts
			if d < 0 {
				d = 0
			}
			clk.advance(time.Duration(d * float64(time.Second)))
		}
		out := f.Feed(r.text)
		if len(out) == 0 {
			// 记下每一片为什么没展示：中文不够（真噪声）还是中文够但重复（模型重发）。
			// 这两类的配比正是「用户看到多少乱说被挡掉」的量化口径。
			reason := "噪声（中文不够）"
			if enoughChinese(r.text) {
				reason = "重复抑制"
				droppedDup++
			} else {
				droppedNoise++
			}
			t.Logf("  %.1fs 丢弃（%s）：%s", r.ts, reason, logHead(r.text, 30))
		}
		for _, s := range out {
			shown = append(shown, s)
			shownAt = append(shownAt, r.ts)
			t.Logf("  %.1fs 展示：%s", r.ts, logHead(s, 40))
		}
	}

	// ① 英文垃圾必须被判为噪声，而且展示出来的东西不能是半英半中。
	//   两条判据：拉丁字母占比 ≤ 20%（**可自证**：把 displayText 改成返回原文即转红）
	//   和汉字数 ≥ runMinDefault。
	//   旧判据是「含拉丁字母则必须同时含 ≥8 汉字」——同义反复，半英半中的帧照样过关。
	kept, beats := 0, 0
	for _, s := range shown {
		if isNarration(s) {
			beats++
			continue
		}
		kept++
		if ratio := latinRuneRatio(s); ratio > 0.2 {
			t.Errorf("真实料上展示了半英半中：拉丁占比 %.0f%% %q", ratio*100, logHead(s, 40))
		}
		if cjk, _ := cjkStats(s); cjk < runMinDefault {
			t.Errorf("真实料上展示了不足 %d 个汉字的内容：%q（CJK=%d）", runMinDefault, s, cjk)
		}
	}
	// ②a 过滤器**声称要展示**的中文段，一个都不能少。判据与实现同源（同一把
	//     chineseRuns 尺子）、方向相反：实现挑出来的段必须真的出现在展示流里，
	//     少的那个就是被吃掉的内容。比汉字序列——显示层丢标点属于预期行为。
	stream := cjkOnly(strings.Join(shown, ""))
	chunks, dropped := 0, 0
	for _, r := range rows {
		runs := chineseRuns(r.text, runMinDefault)
		if len(runs) == 0 {
			dropped++
			continue
		}
		chunks++
		for _, run := range runs {
			if !strings.Contains(stream, cjkOnly(run)) {
				t.Errorf("真实料里的中文段被吃掉了：%.1fs %q", r.ts, run)
			}
		}
	}
	// ②b 独立的「丢内容预算」：展示流里的汉字总量不得少于真实料汉字总量的一半。
	//     这条不依赖 chineseRuns，专防「判据本身过严」——把 runMin 调到 20 这类
	//     过拟合变异，②a 反而是绿的（它只保证「实现说要展示的都展示了」），②b 会红。
	//     门槛取 50%：实测线上这批料留存约 7 成，重复重发的正文本来就会因去重丢掉。
	rawCJK := 0
	for _, r := range rows {
		n, _ := cjkStats(r.text)
		rawCJK += n
	}
	keptCJK := len([]rune(stream))
	if rawCJK > 0 && float64(keptCJK) < 0.5*float64(rawCJK) {
		t.Errorf("展示流只留下 %d 个汉字（真实料 %d 个，留存 %.0f%% < 50%%）：过度过滤",
			keptCJK, rawCJK, 100*float64(keptCJK)/float64(rawCJK))
	}
	t.Logf("汉字留存：%d/%d（%.0f%%）", keptCJK, rawCJK, 100*float64(keptCJK)/float64(rawCJK))

	maxGap := 0.0
	maxGapAt := 0.0
	for i := 1; i < len(shownAt); i++ {
		if g := shownAt[i] - shownAt[i-1]; g > maxGap {
			maxGap = g
			maxGapAt = shownAt[i]
		}
	}
	// 旁白只能在「有新片段到达」时才发出——不起 goroutine/定时器是硬约束，所以
	// 展示间隔的上界必然是「2.5s + 该旁白到达前的那一次帧到达间隔」。账本 50.4s→53.5s
	// 本来就有 2.5s 一个帧都没有，界面那 2.5s 谁都更新不了，那不是过滤器失职。
	// 所以断言写成「扣掉到达间隔之后，剩下的迟滞不得超过 2.5s」；心跳一旦失效，
	// 迟滞会立刻顶到几秒以上，这条照样转红。
	arrival := maxArrivalDeltaAt(rows, maxGapAt)
	t.Logf("真实料：%d 片（含 ≥8 汉字的 %d 片、汉字不足的 %d 片）→ 展示 %d 条（正文 %d 条 + 旁白 %d 条）；"+
		"判为噪声 %d 片（其中重复抑制 %d 片）；最大展示间隔 %.1fs（其中 %.1fs 那一带根本没有帧到达），扣掉到达间隔后 %.1fs",
		len(rows), chunks, dropped, len(shown), kept, beats,
		droppedNoise+droppedDup, droppedDup, maxGap, arrival, maxGap-arrival)

	if kept == 0 {
		t.Errorf("真实料上一条正文都没展示——过度过滤")
	}
	if maxGap-arrival > 2.5 {
		t.Errorf("真实料上最大展示间隔 %.1fs，扣掉到达间隔 %.1fs 仍剩 %.1fs > 2.5s：静默保护失效",
			maxGap, arrival, maxGap-arrival)
	}
}

// maxArrivalDeltaAt 返回 rows 里、结束时间正好落在 at 的那一段帧到达间隔（秒）。
// 用于把「账本本身没有帧」这段时间从展示间隔里扣出去。
func maxArrivalDeltaAt(rows []ledgerRow, at float64) float64 {
	best := 0.0
	for i := 1; i < len(rows); i++ {
		if rows[i].ts <= at && rows[i].ts >= at-5 {
			if d := rows[i].ts - rows[i-1].ts; d > best {
				best = d
			}
		}
	}
	return best
}

// firstRunes 只用于日志：拉长片段会淹掉测试输出。
func logHead(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// loadLedgerFixture 读 `时间戳<TAB>片段` 的 fixture。空行与 # 开头的注释行跳过。
func loadLedgerFixture(t *testing.T, name string) []ledgerRow {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读 fixture 失败：%v", err)
	}
	var rows []ledgerRow
	for _, ln := range strings.Split(string(raw), "\n") {
		ln = strings.TrimRight(ln, "\r")
		if strings.TrimSpace(ln) == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		parts := strings.SplitN(ln, "\t", 2)
		if len(parts) != 2 {
			t.Fatalf("fixture 行不合格式（需要 `时间戳<TAB>片段`）：%q", ln)
		}
		ts, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		if err != nil {
			t.Fatalf("fixture 时间戳解不出：%q", ln)
		}
		rows = append(rows, ledgerRow{ts: ts, text: parts[1]})
	}
	return rows
}

func isNarration(s string) bool { return strings.HasPrefix(s, "模型思考中…") }

// 这把尺子自己错过两次，所以给它留一组**负向对照**：报进度的中文句不算英文脚手架，
// 真脚手架一个字也不许放过。少任何一半都会退化成坏尺子 ——
// 只留前一半 → 尺子太松，半英半中的帧重新漏给用户看；
// 只留后一半 → 尺子太紧，把「已产出 399 字」判成英文，产品明明在报进度却天天红。
func TestLatinRuneRatioCountsLettersNotDigits(t *testing.T) {
	cases := []struct {
		name string
		text string
		want float64 // 判据阈值 0.2
		over bool    // true = 期望 > 0.2（判成脚手架）
	}{
		{"进度旁白带数字", "模型思考中…已产出 399 字（已 2s）", 0.2, false},
		{"收到需求带数字", "· 收到你的需求（本轮 10408 字）", 0.2, false},
		{"要点回顾带数字", "· 引述规范：领导讲话必须使用直接引号且内容源自素材，严禁编造；机构首次出现用全称。", 0.2, false},
		{"英文自我对话", "Here's a thinking process: Analyze User Input:", 0.2, true},
		{"英文脚手架带数字", "(178 chars) Total: ~650 Chinese characters. Well over 400.", 0.2, true},
		{"半英半中帧（线上原样）", "t Check:* \"直接输出正文，不要任何解释。\" -> Will output only the text.", 0.2, true},
	}
	for _, c := range cases {
		got := latinRuneRatio(c.text)
		if c.over && got <= c.want {
			t.Errorf("%s：拉丁占比 %.2f，期望 > %.2f —— 真脚手架漏过去了，尺子太松：%q",
				c.name, got, c.want, c.text)
		}
		if !c.over && got > c.want {
			t.Errorf("%s：拉丁占比 %.2f，期望 ≤ %.2f —— 纯中文进度句被判成英文，尺子太紧：%q",
				c.name, got, c.want, c.text)
		}
	}
}

// 空串不该除零，也不该被判成「全是英文」。
func TestLatinRuneRatioEmptyIsZero(t *testing.T) {
	if got := latinRuneRatio(""); got != 0 {
		t.Errorf("空串的拉丁占比应为 0，实际 %v", got)
	}
}

// runMinDefault 与 newMaterialFilter 的默认 runMin 对齐。断言里凡是「中文够不够」
// 的地方都用它：测试里写死一个数、实现改成另一个数却两边都绿，是典型的假绿。
const runMinDefault = 6

// enoughChinese 是「这一片抽得出可展示的中文吗」的无状态版本（阈值同 runMinDefault），
// 只用于测试日志里区分「中文不够」与「重复」这两种丢弃原因。
func enoughChinese(s string) bool { return len(chineseRuns(s, runMinDefault)) > 0 }
