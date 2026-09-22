package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lizhemin15/skillforge/internal/model"
)

// 疑点回执的两半：判据（ValidateDoubts / DoubtHaystack）和接线（RaiseDoubts 这一跳
// 到底发了什么请求）。
//
// 判据错的表现是「问了不该问的」或「该问的不问」，两者都只有用户体验能感知：
//   - 引用对不上原文却展示 → 用户去原文里找那句话，找不到 → 比不问更糟；
//   - 没有默认理解的「疑点」→ 就是旧版「请补充：标题亮点」换皮，用户投诉的原话；
//   - 真疑点被本地校验误杀 → 静默直写，用户以为材料被忽略了。
// 所以每一条规则都要有正反两侧的用例，不能只测「绿色的那面」。

// doubtsIn 造一份疑点回执（省得每条用例都手写 JSON 结构）。
func doubtsIn(ds ...Doubt) *DoubtReport { return &DoubtReport{Doubts: ds} }

// validQuote 是一段在 hay 里逐字存在的引用（长度 > minQuoteRunes）。
const validQuote = "华东制造基地二期项目"

func testHay() string {
	return "8月26日，华东制造基地二期项目正式投产，总投资3.2亿元。"
}

func TestValidateDoubtsKeepsAnchoredAmbiguity(t *testing.T) {
	got := ValidateDoubts(doubtsIn(Doubt{
		Kind: "ambiguity", Quote: validQuote,
		Inference: "按投产这件事写主稿", Impact: "主题口径",
	}), testHay())
	if len(got) != 1 {
		t.Fatalf("引用逐字命中原文、默认理解也够具体，却被丢了：%+v", got)
	}
	if got[0].Kind != "ambiguity" {
		t.Errorf("Kind 被改成 %q，展示层会走错分支", got[0].Kind)
	}
}

// 负向：编造的引用一律丢弃。这是「不许拿对不上原文的引用去拦用户」的判据本身。
func TestValidateDoubtsDropsQuoteNotInHaystack(t *testing.T) {
	got := ValidateDoubts(doubtsIn(Doubt{
		Kind: "ambiguity", Quote: "客户要求必须当天发布并署名",
		Inference: "按当天发布处理", Impact: "发布节奏",
	}), testHay())
	if len(got) != 0 {
		t.Fatalf("引用在原文里根本不存在却留下了（用户会去原文里找这句话）：%+v", got)
	}
}

// 负向：太短的引用锚不住语义（「的」「写一篇」），一律丢弃。
func TestValidateDoubtsDropsShortQuote(t *testing.T) {
	hay := "写一篇关于华东制造基地投产的公司新闻通稿"
	got := ValidateDoubts(doubtsIn(Doubt{
		Kind: "ambiguity", Quote: "通稿",
		Inference: "按公司新闻通稿来写", Impact: "文体",
	}), hay)
	if len(got) != 0 {
		t.Fatalf("引用只有 2 个字符也留下了（锚不住任何语义）：%+v", got)
	}
}

// 反向自证：不许问太多。用户要的是「交互着改」，不是填表。
//
// ⚠️ 这条用例踩过一次「假绿」，写下来防复发：最早的版本里 5 条疑点只有 3 条能通过
// 别的规则（第 4 条的 inference「写进产能段」只有 5 字 < minInferRunes，第 5 条的
// 引用「8月26日」只有 5 字 < minQuoteRunes），于是**把上限抬到 99 它照样绿** ——
// 测的是「别的规则顺手丢了两条」，不是封顶。所以先逐条自证每一条单独都留得住，
// 再断言封顶；前提不成立就不评封顶。
func TestValidateDoubtsCapsAtThree(t *testing.T) {
	hay := "华东制造基地二期项目正式投产，总投资3.2亿元，新增就业岗位260个，年产精密部件120万件，产品出口到东南亚六个国家。"
	ds := []Doubt{
		{Kind: "ambiguity", Quote: "华东制造基地二期项目", Inference: "按投产写主稿", Impact: "主题"},
		{Kind: "ambiguity", Quote: "总投资3.2亿元", Inference: "金额写进正文第二段", Impact: "数据"},
		{Kind: "ambiguity", Quote: "新增就业岗位260个", Inference: "放在结尾成效段里", Impact: "结构"},
		{Kind: "ambiguity", Quote: "年产精密部件120万件", Inference: "写进产能段的支撑数据", Impact: "结构"},
		{Kind: "ambiguity", Quote: "产品出口到东南亚六个国家", Inference: "按出口六个国家写市场段", Impact: "口径"},
	}
	// 前提：每一条单独都留得住，否则封顶断言是空跑绿。
	for i, d := range ds {
		if got := ValidateDoubts(doubtsIn(d), hay); len(got) != 1 {
			t.Fatalf("测试前提不成立：第 %d 条疑点单独跑就被别的规则丢了（%+v），封顶断言会变成假绿", i+1, d)
		}
	}
	got := ValidateDoubts(doubtsIn(ds...), hay)
	if len(got) != maxDoubts {
		t.Fatalf("疑点必须封顶 %d 条，实际 %d 条（多了就变回填表）：%+v", maxDoubts, len(got), got)
	}
}

// 负向：没有具体默认理解的「疑点」= 旧版泛问换皮，必须丢。
// 这条是整套改造的命门：只要 inference 能空，「请补充：标题亮点」就会借尸还魂。
func TestValidateDoubtsDropsThinInference(t *testing.T) {
	for _, bad := range []string{"", "请补充", "待定"} {
		got := ValidateDoubts(doubtsIn(Doubt{
			Kind: "ambiguity", Quote: validQuote, Inference: bad, Impact: "主题口径",
		}), testHay())
		if len(got) != 0 {
			t.Fatalf("inference=%q 太泛（等于把问题原样丢回给用户）却留下了：%+v", bad, got)
		}
	}
}

// 负向：想当 missing 又不敢说自己是 missing（kind=ambiguity 且没引用）→ 丢。
// 防的是模型「编不出引用就把歧义偷偷降级成缺失」留在列表里。
func TestValidateDoubtsDropsAmbiguityWithoutQuote(t *testing.T) {
	got := ValidateDoubts(doubtsIn(Doubt{
		Kind: "ambiguity", Quote: "", Inference: "按行业惯例写", Impact: "口径",
	}), testHay())
	if len(got) != 0 {
		t.Fatalf("无引用的 ambiguity 被留下了（模型可以靠它规避引用校验）：%+v", got)
	}
}

// 正向：原文确实没写的点，允许没有引用，但必须给默认理解；Impact 空时兜底，
// 免得文案里出现「（影响到：）」这种空括号。
func TestValidateDoubtsKeepsMissingWithoutQuote(t *testing.T) {
	got := ValidateDoubts(doubtsIn(Doubt{
		Kind: "missing", Quote: "", Inference: "标题按主谓宾平铺，不加副标题", Impact: "",
	}), testHay())
	if len(got) != 1 {
		t.Fatalf("missing 型（原文确实没提）没引用是合法的，却被丢了：%+v", got)
	}
	if got[0].Impact == "" {
		t.Errorf("Impact 空时没兜底，展示出来会是空括号")
	}
}

// 模型给 missing 型却附了引用：引用必须照查（命中→按歧义型展示；不命中→丢）。
func TestValidateDoubtsMissingWithQuoteStillChecked(t *testing.T) {
	ok := ValidateDoubts(doubtsIn(Doubt{
		Kind: "missing", Quote: validQuote, Inference: "按投产当天写主稿", Impact: "主题",
	}), testHay())
	if len(ok) != 1 || ok[0].Kind != "ambiguity" {
		t.Fatalf("missing 型带了能命中的引用，应归一成 ambiguity 留下，实际：%+v", ok)
	}
	bad := ValidateDoubts(doubtsIn(Doubt{
		Kind: "missing", Quote: "客户要求必须当天发布并署名", Inference: "按当天发布处理", Impact: "节奏",
	}), testHay())
	if len(bad) != 0 {
		t.Fatalf("missing 型带了编造的引用，应整条丢掉，实际：%+v", bad)
	}
}

// 空回执 = 无疑点，是**合法结果**（禁硬凑），必须原样返回空。
func TestValidateDoubtsEmptyMeansNoDoubts(t *testing.T) {
	if got := ValidateDoubts(&DoubtReport{}, testHay()); len(got) != 0 {
		t.Fatalf("空回执被解读出疑点了：%+v", got)
	}
	if got := ValidateDoubts(nil, testHay()); len(got) != 0 {
		t.Fatalf("nil 回执被解读出疑点了：%+v", got)
	}
}

// 引用查找域必须包含**上一轮用户贴的材料**（两段式：先贴素材、再说「照这个写」），
// 且**不得包含模型自己以前的回答** —— 否则模型可以拿自己编的话当「原文引用」，
// 用户去原文里找同样找不到。
func TestDoubtHaystackCoversPriorUserTurnsOnly(t *testing.T) {
	hist := []Message{
		{Role: "user", Content: validQuote + "正式投产，总投资3.2亿元", At: time.Now()},
		{Role: "assistant", Content: "我先按这个写一稿：某某公司新闻稿", SkillSlug: "news", At: time.Now()},
	}
	hay := DoubtHaystack("照这个写一篇通稿", hist)
	if !strings.Contains(hay, validQuote) {
		t.Fatalf("上一轮用户贴的素材没进引用查找域（两段式对话会误判引用无效）：%q", hay)
	}
	if strings.Contains(hay, "某某公司新闻稿") {
		t.Fatalf("模型自己以前的回答进了引用查找域（可以自证式引用）：%q", hay)
	}
}

// 归一化要容忍换行/空白：用户原文里的排版不该成为「逐字命中」的障碍，
// 但语义字符一个都不能少。
func TestValidateDoubtsQuoteTolerantOfWhitespace(t *testing.T) {
	hay := "8月26日，华东制造基地\n二期项目 正式投产。"
	got := ValidateDoubts(doubtsIn(Doubt{
		Kind: "ambiguity", Quote: "华东制造基地二期项目",
		Inference: "按投产写主稿", Impact: "主题",
	}), hay)
	if len(got) != 1 {
		t.Fatalf("引用跨了原文的换行/空格就被判无效（用户会白等）：%+v", got)
	}
}

// 展示文案：每条必须落在用户能一句话回复的形态上（引用 + 我的理解 + 影响 + 一句话出口）。
func TestDoubtsMessageShape(t *testing.T) {
	msg := DoubtsMessage([]Doubt{
		{Kind: "ambiguity", Quote: validQuote, Inference: "按投产写主稿", Impact: "主题口径"},
		{Kind: "missing", Quote: "", Inference: "标题不加副标题", Impact: "标题结构"},
	})
	if !strings.Contains(msg, "「"+validQuote+"」") {
		t.Errorf("歧义型没把原文引用摆出来（用户对不上号）：%s", msg)
	}
	if !strings.Contains(msg, "按投产写主稿") || !strings.Contains(msg, "标题不加副标题") {
		t.Errorf("默认理解没展示（用户无法确认/纠正）：%s", msg)
	}
	if !strings.Contains(msg, "就按你的") {
		t.Errorf("没给「一句话就能继续」的出口：%s", msg)
	}
	if strings.Contains(msg, "请补充") || strings.Contains(msg, "我需要你") {
		t.Errorf("文案退回了泛问口吻（旧版投诉的原话）：%s", msg)
	}
	if DoubtsMessage(nil) != "" {
		t.Errorf("无疑点时必须返回空串，调用方据此判断「无疑点，开始写」")
	}
}

func TestDoubtsShortCounts(t *testing.T) {
	if got := DoubtsShort(nil); !strings.Contains(got, "无") {
		t.Errorf("无疑点时的短标签要说清「无疑点」，别让用户以为在等它问，实际 %q", got)
	}
	if got := DoubtsShort([]Doubt{{Kind: "ambiguity", Quote: validQuote, Inference: "按投产写"}}); !strings.Contains(got, "1") {
		t.Errorf("1 条疑点的短标签应含 1，实际 %q", got)
	}
}

// ---- 接线层：这一跳到底发了什么请求 ----

// 疑点跳是用户按下发送后的第二个网络调用，必须是「快 + 只看原文」：
//   - enable_thinking=false / reasoning_effort=none：思考链在这跳是纯粹的白等
//     （线上 35B-A3B 实测思考链空转 19k 字），且疑点跳要的是短 JSON，不需要它；
//   - response_format=json_object：要机器可读的回执，不是散文；
//   - stream=true：provider 忽略开关时也还有内容在动（不然又是一个空跳的计时）；
//   - 提示里必须带上**用户这次的原话**和**本对话中用户先前说过的话** —— 用户投诉
//     「像没看到我给的信息」「多轮忘了前面说过什么」的正解就在这里：疑点只能锚原文。
func TestRaiseDoubtsHopIsFastAndAnchoredOnRawText(t *testing.T) {
	fp := &fakeProvider{content: `{"doubts":[]}`}
	eng := newTestEngine(t, fp)

	hist := []Message{
		{Role: "user", Content: "素材：" + validQuote + "正式投产，总投资3.2亿元。", At: time.Now()},
		{Role: "assistant", Content: "我打算这样开头：某某公司讯", SkillSlug: "gongchang-news", At: time.Now()},
	}
	missing := []model.Param{{Name: "title_point", Label: "标题亮点", Type: "text", Required: true}}
	if _, err := eng.RaiseDoubts(context.Background(), "公司新闻通稿", missing,
		"照上面素材写一篇，标题要平实", hist); err != nil {
		t.Fatalf("RaiseDoubts 失败: %v", err)
	}

	body := fp.bodyAt(t, 0)
	if v, ok := body["enable_thinking"]; !ok || v != false {
		t.Errorf("疑点跳必须带 enable_thinking=false，实际 %v", body["enable_thinking"])
	}
	if v := body["reasoning_effort"]; v != "none" {
		t.Errorf("疑点跳必须带 reasoning_effort=none，实际 %v", v)
	}
	if body["stream"] != true {
		t.Errorf("疑点跳必须是流式（否则 provider 忽略开关时连材料都没有），实际 stream=%v", body["stream"])
	}
	rf, _ := body["response_format"].(map[string]any)
	if rf == nil || rf["type"] != "json_object" {
		t.Errorf("疑点跳要的是机器可读回执，必须带 json_object，实际 %v", body["response_format"])
	}

	user := lastUserPrompt(t, body)
	if !strings.Contains(user, "## 用户这次给的原话") || !strings.Contains(user, "照上面素材写一篇，标题要平实") {
		t.Errorf("提示里没带用户这次的原话（疑点就无从锚起）：%s", user)
	}
	if !strings.Contains(user, validQuote) {
		t.Errorf("提示里没带上一条用户贴的素材（两段式对话会误判「没给信息」）：%s", user)
	}
	if !strings.Contains(user, "标题亮点") {
		t.Errorf("提示里没点名本次要核对的字段，模型只能泛问：%s", user)
	}
}

// 反面：疑点跳输出不是 JSON（模型抽风/被截断）必须**返回错误**，
// 让调用方走「不拦、带假设直写」那一侧 —— 绝不能在这里回退成通用清单。
func TestRaiseDoubtsGarbageOutputReturnsError(t *testing.T) {
	fp := &fakeProvider{content: "嗯，这个我看看……还是直接写吧"}
	eng := newTestEngine(t, fp)
	rep, err := eng.RaiseDoubts(context.Background(), "公司新闻通稿",
		[]model.Param{{Name: "title_point", Label: "标题亮点"}}, "写一篇通稿", nil)
	if err == nil {
		t.Fatalf("坏输出必须报错（调用方据此放过这一轮），实际 rep=%+v", rep)
	}
}

// lastUserPrompt 取最后一次请求里的 user 内容（fakeProvider 只记 body，不切片）。
func lastUserPrompt(t *testing.T, body map[string]any) string {
	t.Helper()
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("请求里没有 messages：%v", body["messages"])
	}
	var out string
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if r, _ := mm["role"].(string); r == "user" {
			s, _ := mm["content"].(string)
			out = s
		}
	}
	if out == "" {
		t.Fatalf("请求里没有 user 消息：%v", msgs)
	}
	return out
}
