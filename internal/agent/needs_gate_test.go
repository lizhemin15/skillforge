package agent

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/model"
)

// mkNeed 造一条 needs 项；label 缺省时补成 name（真实 payload 里 label 一般都有）。
func mkNeed(name, label string, required bool) model.Param {
	if label == "" {
		label = name
	}
	return model.Param{Name: name, Label: label, Type: "text", Required: required}
}

// TestSplitNeedsOptionalDoesNotBlock 是**线上事故的回放**（2026-09-22）：
//
//	贴完一万字素材后提写作需求，第 2 轮被 needs 闸门拦住 —— 索要的字段是
//	「标题亮点（可选，未提供则生成）」，required=false。整轮 4.5s 结束、正文 0 字。
//	技能声明的参数表里这个字段也是 required=false。
//
// 断言：这一条**不许**进 blocking（否则写作跳又被短路），必须进 optional。
func TestSplitNeedsOptionalDoesNotBlock(t *testing.T) {
	declared := []model.Param{
		mkNeed("headline_highlight", "标题亮点（可选，未提供则生成）", false),
		mkNeed("company_name", "公司名称", true),
	}
	needs := []model.Param{
		mkNeed("headline_highlight", "标题亮点（可选，未提供则生成）", false),
	}
	blocking, optional := SplitNeeds(declared, needs)
	if len(blocking) != 0 {
		t.Fatalf("可选字段不该拦下写作：blocking=%v（线上事故回放：整轮被短路成一句追问）", blocking)
	}
	if len(optional) != 1 || optional[0].Name != "headline_highlight" {
		t.Fatalf("被放过的可选字段应进 optional 以便在中间材料里说明：got %v", optional)
	}
}

// TestSplitNeedsDeclaredIsGroundTruth 声明表压过模型自报：两个方向都要对。
func TestSplitNeedsDeclaredIsGroundTruth(t *testing.T) {
	declared := []model.Param{
		mkNeed("company_name", "公司名称", true),        // 声明：必填
		mkNeed("headline_highlight", "标题亮点", false), // 声明：可选
	}
	t.Run("声明必填但模型说可选→仍然拦", func(t *testing.T) {
		b, o := SplitNeeds(declared, []model.Param{mkNeed("company_name", "公司名称", false)})
		if len(b) != 1 || len(o) != 0 {
			t.Fatalf("拿空的公司名去套模板会产出一份字段全空的文档；此处应拦：blocking=%v optional=%v", b, o)
		}
	})
	t.Run("声明可选但模型说必填→不拦", func(t *testing.T) {
		b, o := SplitNeeds(declared, []model.Param{mkNeed("headline_highlight", "标题亮点", true)})
		if len(b) != 0 || len(o) != 1 {
			t.Fatalf("模型的误报不该把一轮写作拦成追问：blocking=%v optional=%v", b, o)
		}
	})
}

// TestSplitNeedsUnknownFallsBackToModel 声明里没有的名字（清单漂移）按自报 Required 判，
// 别因为查不到就一律放过 —— 那会让真正缺的必填字段被静默跳过。
func TestSplitNeedsUnknownFallsBackToModel(t *testing.T) {
	declared := []model.Param{mkNeed("company_name", "公司名称", true)}
	b, o := SplitNeeds(declared, []model.Param{
		mkNeed("deadline", "截稿时间", true),
		mkNeed("tone", "语气（可选）", false),
	})
	if len(b) != 1 || b[0].Name != "deadline" {
		t.Fatalf("声明里没有的必填字段仍要拦：blocking=%v", b)
	}
	if len(o) != 1 || o[0].Name != "tone" {
		t.Fatalf("声明里没有的可选字段不该拦：optional=%v", o)
	}
}

// TestSplitNeedsMixedAndEmpty 混合场景 + 空输入（空输入必须回 nil，不是空切片，
// 免得调用方 len()>0 之类的判据在空输入上翻车）。
func TestSplitNeedsMixedAndEmpty(t *testing.T) {
	declared := []model.Param{
		mkNeed("company_name", "公司名称", true),
		mkNeed("headline_highlight", "标题亮点（可选，未提供则生成）", false),
	}
	b, o := SplitNeeds(declared, []model.Param{
		mkNeed("company_name", "公司名称", false),       // 声明必填 → 拦
		mkNeed("headline_highlight", "标题亮点", false), // 可选 → 放过
	})
	if len(b) != 1 || b[0].Name != "company_name" || len(o) != 1 || o[0].Name != "headline_highlight" {
		t.Fatalf("混合场景分流错：blocking=%v optional=%v", b, o)
	}
	if b2, o2 := SplitNeeds(declared, nil); b2 != nil || o2 != nil {
		t.Fatalf("空 needs 应回 nil：blocking=%v optional=%v", b2, o2)
	}
}

// TestSplitNeedsNameWhitespaceAndDup 名字带空格要归一（模型回填常带空格），
// 声明表里同名重复以第一条为准（params 表 ORDER BY position）。
func TestSplitNeedsNameWhitespaceAndDup(t *testing.T) {
	declared := []model.Param{
		mkNeed("company_name", "公司名称", true),
		mkNeed("company_name", "公司名称（旧）", false),
		mkNeed("  ", "空名", false),
	}
	b, o := SplitNeeds(declared, []model.Param{mkNeed(" company_name ", "公司名称", false)})
	if len(b) != 1 || len(o) != 0 {
		t.Fatalf("带空格的名字应归一到声明里的必填字段：blocking=%v optional=%v", b, o)
	}
}

// TestNeedsSummaryTrimsLabel 中间材料那一格只够几个字：括号里的补充说明要砍掉。
func TestNeedsSummaryTrimsLabel(t *testing.T) {
	got := NeedsSummary([]model.Param{
		mkNeed("headline_highlight", "标题亮点（可选，未提供则生成）", false),
		mkNeed("tone", "语气(可选)", false),
		mkNeed("", "", false),
	})
	if got != "标题亮点、语气" {
		t.Fatalf("NeedsSummary=%q，期望「标题亮点、语气」", got)
	}
}

// TestMaterialInHandTakesMaxNotSum 是这条判据的地基：**取大者，不相加**。
//
// 素材轮的两个来源装的是同一份东西（消息里用户贴的素材 + 注入块里 ContextBlock 压缩
// 后的同一份），相加会把门槛推得虚高，判据就随「压缩后剩多少」抖起来。线上实测这两
// 个数是 10340/11235 字 —— 同一个量级，不是两个独立来源。
func TestMaterialInHandTakesMaxNotSum(t *testing.T) {
	cases := []struct {
		name, user, ctx string
		want            int
	}{
		{"消息更长（用户直接把素材贴在这一轮）", strings.Repeat("a", 900), strings.Repeat("b", 120), 900},
		{"注入块更长（「把上面那篇整理成 Word」型：消息十来个字，材料在上下文里）",
			"把上面那篇整理成 Word", strings.Repeat("b", 900), 900},
		{"两边都没有（一句话需求）", "帮我写篇新闻稿", "", 7},
		// 相加会得 1020 —— 这条用例就是守住「别写成相加」的那一行代码。
		{"两边都有：900/120 相加是 1020，必须取 900", strings.Repeat("a", 900), strings.Repeat("b", 120), 900},
	}
	for _, c := range cases {
		if got := MaterialInHand(c.user, c.ctx); got != c.want {
			t.Errorf("%s：MaterialInHand=%d，期望 %d", c.name, got, c.want)
		}
	}
}

// TestMaterialNoAskBoundary 钉住门槛本身，以及它两侧各差一个字的行为。
//
// 门槛不是随手取的数：一句话需求（线上实测 4~40 字）够不到，素材轮（实测
// 10340~10432 字）远超。这里把 799/800 两个相邻值都钉死，是为了让「谁把常量改了」
// 立刻在测试里现形 —— 它改的是产品行为（还拦不拦用户），不是性能参数。
func TestMaterialNoAskBoundary(t *testing.T) {
	cases := []struct {
		chars int
		want  bool
	}{
		{0, false},
		{7, false},   // 「帮我写篇新闻稿」
		{799, false}, // 门槛下沿：还是拦
		{800, true},  // 门槛上沿：不拦
		{10340, true},
	}
	for _, c := range cases {
		if got := MaterialNoAsk(c.chars); got != c.want {
			t.Errorf("MaterialNoAsk(%d)=%v，期望 %v", c.chars, got, c.want)
		}
	}
}

// TestMissingRequiredTakesDeclaredAsGroundTruth 钉住零材料强制门的地面真值来源。
//
// 为什么必须是**声明表**算、不是等模型自报：线上逮到的缺口就是「模型一条 needs 都
// 没报」（一句话指令 + 零材料，分类器自报 0 条），SplitNeeds 拿空的 needs 只能回空
// 的 blocking —— 闸门形同不存在，写作跳直通，产出 645 字带假日期、假参会人的会议纪要。
// 这一组用例把「模型没报也照样算得出缺哪项」写成断言。
func TestMissingRequiredTakesDeclaredAsGroundTruth(t *testing.T) {
	declared := []model.Param{
		mkNeed("subject", "会议主题", true),
		mkNeed("attendees", "参会人", true),
		mkNeed("highlight", "标题亮点（可选，未提供则生成）", false),
	}
	// 模型自报为空（缺口现场），但声明说两项必填没落实 → 两项都要算出来。
	got := MissingRequired(declared, map[string]string{})
	if len(got) != 2 || got[0].Name != "subject" || got[1].Name != "attendees" {
		t.Fatalf("模型自报为空时没按声明表算缺口：got %v（这是线上编造内容的那个缺口）", got)
	}
	// 可选字段**永远不许**进这一组：技能标签自己写着「未提供则生成」，问它就是自相矛盾
	// （2026-09-22 事故的同一根因，只是换到了零材料这一格）。
	for _, p := range got {
		if p.Name == "highlight" {
			t.Fatalf("可选字段被算成必填缺口，又会问用户要它自称会生成的东西：%v", got)
		}
	}
	// 声明里没有必填项（内置技能的常态）→ 回 nil，调用方按 len()==0 判不拦。
	if r := MissingRequired([]model.Param{mkNeed("x", "X", false)}, map[string]string{}); r != nil {
		t.Fatalf("没有必填项时应回 nil（内置技能一个必填项都没有，回空切片以外的值会改变它们的行为）：%v", r)
	}
}

// TestMissingRequiredValueEdgeCases 钉住「什么算给了值」。
//
// 期望值只能是本地事实：空串 / 纯空白不算给（前端表单空着提交就是 ""，模型回填也常带
// 空格），名字两边 TrimSpace 后相等才算命中（跟 SplitNeeds 同一套归一）。
func TestMissingRequiredValueEdgeCases(t *testing.T) {
	declared := []model.Param{mkNeed("subject", "会议主题", true), mkNeed("attendees", "参会人", true)}
	cases := []struct {
		name  string
		given map[string]string
		want  int
	}{
		{"空串算没给（表单空着提交）", map[string]string{"subject": "", "attendees": "张三、李四"}, 1},
		{"纯空白算没给", map[string]string{"attendees": "   \n "}, 2},
		{"名字带空格要能对上（模型回填常态）", map[string]string{" attendees ": "张三"}, 1},
		{"都给了", map[string]string{"subject": "Q3 复盘", "attendees": "张三"}, 0},
		{"名字在值表里但大小写不同 → 算没给（不做模糊匹配，宁可真算缺）",
			map[string]string{"Subject": "Q3 复盘", "attendees": "张三"}, 1},
	}
	for _, c := range cases {
		if got := len(MissingRequired(declared, c.given)); got != c.want {
			t.Errorf("%s：缺口 %d 项，期望 %d 项", c.name, got, c.want)
		}
	}
}

// TestZeroMaterialAskBoundaries 把三段区间的边界钉死（0/79/80/799/800）。
//
// 这三段的判据都必须是**本地数得准**的事实，缺一不可：
//
//	材料真空（<80）        → 停问（不问就等于替用户编事实）
//	灰区 / 材料在手（>=80）→ 一个字都不改（走既有的 SplitNeeds + 疑点回执那条路）
//
// 80 这条线尤其不能松：「信息就在原文里还被问一遍」是用户投诉的原话，材料只要过了
// 真空线就不许再拿必填项拦人。
func TestZeroMaterialAskBoundaries(t *testing.T) {
	declared := []model.Param{mkNeed("subject", "会议主题", true)}
	given := map[string]string{}
	msg := "帮我写一份会议纪要"
	cases := []struct {
		chars int
		want  bool
	}{
		{0, true}, {14, true}, {79, true},
		{80, false}, {799, false}, {800, false}, {10000, false},
	}
	for _, c := range cases {
		got := len(ZeroMaterialAsk(declared, given, c.chars, msg, false)) > 0
		if got != c.want {
			t.Errorf("材料 %d 字：停问=%v，期望 %v", c.chars, got, c.want)
		}
	}
}

// TestZeroMaterialAskExemptions 钉住两个**必须放过**的格子（缩打击面）。
//
// ① action=template_only：用户点名要空白模板，交付物就是那个附件本身。模板交付分支在
//
//	闸门**之后**，不显式放过，「把 XX 模板发我」会被拦成一句反问 —— 那是把打击面
//	扩大到模板下载，属于本门不打算碰的路径。
//
// ② 用户已明确授权（GrantedFreeRein）：不放过就问成死循环 —— 停问文案让用户回
//
//	「就按你的」，那一轮消息只有 4 个字，材料依然真空、必填项依然没落实，于是又问一遍。
//	出口必须闭得上，否则用户永远走不到写作。
func TestZeroMaterialAskExemptions(t *testing.T) {
	declared := []model.Param{mkNeed("subject", "会议主题", true)}
	given := map[string]string{}
	if got := ZeroMaterialAsk(declared, given, 10, "把会议纪要模板发我", true); len(got) != 0 {
		t.Errorf("template_only 被拦了（用户要的是文件，不是反问）：%v", got)
	}
	for _, msg := range []string{"就按你的", "你看着办吧", "不用问了直接写", "自由发挥"} {
		if got := ZeroMaterialAsk(declared, given, len([]rune(msg)), msg, false); len(got) != 0 {
			t.Errorf("用户已授权（%q）却还在问，会问成死循环：%v", msg, got)
		}
	}
	// 反向：没授权的短指令必须照旧拦（防「一改就改成一律不问」）。
	if got := ZeroMaterialAsk(declared, given, 8, "帮我写篇通稿", false); len(got) == 0 {
		t.Error("没授权的零材料指令没拦 —— 那样又会替用户编事实")
	}
}

// TestGrantedFreeReinKeywords 钉住授权词的**边界**：宁少收，不放行没授权的轮次。
//
// 放行的代价是编事实（本门存在的理由），所以「直接写」这类会跟别的语境撞车的词一律
// 不收：实测反例是用户prompt里的「直接写进模板」「直接输出正文」，那些轮次用户并没有
// 授权编内容。
func TestGrantedFreeReinKeywords(t *testing.T) {
	yes := []string{"就按你的", "就按你说的办", "照你的理解写吧", "你看着办", "随便写", "不用问了"}
	for _, s := range yes {
		if !GrantedFreeRein(s) {
			t.Errorf("%q 是明确的「别问，照你的理解写」，必须认（不认就走不出去）", s)
		}
	}
	no := []string{"", "   ", "帮我写一篇公司新闻通稿", "把这些直接写进模板里", "直接输出正文",
		"会议主题是 Q3 复盘，参会人张三"}
	for _, s := range no {
		if GrantedFreeRein(s) {
			t.Errorf("%q 不是授权，不许放行（放行=照旧替用户编事实）", s)
		}
	}
}

// TestZeroMaterialMessageContract 钉住停问文案的两条契约。
//
// ① **逐字引用用户这次的原话**：被投诉「像没看到我给的信息」的那次就是照技能清单拼
//
//	通用要素清单（「我需要你补充以下信息：标题亮点」），跟用户说了什么无关。零材料这
//	一跳尤其要引用 —— 用户这一轮唯一的输入就是那句话。
//
// ② **给一句话出口**，且那个词必须正是 GrantedFreeRein 认的词：文案改了判词没改（或
//
//	反过来）就会问成死循环，所以两条一起钉。
func TestZeroMaterialMessageContract(t *testing.T) {
	const msg = "帮我写一份会议纪要"
	missing := []model.Param{mkNeed("subject", "会议主题", true), mkNeed("attendees", "参会人", true)}
	got := ZeroMaterialMessage(msg, missing)

	if !strings.Contains(got, "「"+msg+"」") {
		t.Errorf("没有逐字引用用户原话（旧病就是跟用户说了什么无关的通用清单）：\n%s", got)
	}
	if !strings.Contains(got, "会议主题") || !strings.Contains(got, "参会人") {
		t.Errorf("没点名缺哪几项，用户不知道要补什么：\n%s", got)
	}
	if !strings.Contains(got, "就按你的") {
		t.Errorf("没给一句话出口（逼用户填表）：\n%s", got)
	}
	// 出口词必须与判定同源：文案里的那一个就是 GrantedFreeRein 认的那一个。
	if !GrantedFreeRein("就按你的") {
		t.Error("文案给的出口词 GrantedFreeRein 不认 —— 用户照着回会被再问一遍")
	}
	if strings.Contains(got, "我需要你补充以下信息") {
		t.Errorf("又用回了被投诉的通用清单句式：\n%s", got)
	}
	if !strings.Contains(got, strconv.Itoa(len([]rune(msg)))) {
		t.Errorf("文案里的字数不是从本轮消息数出来的（防写死）：\n%s", got)
	}
}

// TestZeroMaterialMessageClipsLongMessage 长消息只引用开头，别把整段贴回去。
func TestZeroMaterialMessageClipsLongMessage(t *testing.T) {
	msg := strings.Repeat("会议纪要", 40) // 160 字
	got := ZeroMaterialMessage(msg, []model.Param{mkNeed("subject", "会议主题", true)})
	if !strings.Contains(got, "…") {
		t.Errorf("长消息没有截断（把用户整段贴回去会显得没读进去）：\n%s", got)
	}
	if n := len([]rune(got)); n > 400 {
		t.Errorf("停问文案太长（%d 字），内网慢 token 下用户读得累", n)
	}
}

// TestZeroMaterialMessageQuoteStaysVerbatimWhenClipped 钉「引号里那截逐字来自原话」。
//
// 为什么单拎出来：62 字的指令照样落在真空区（<80），文案会把原话截到 40 字 ——
// 截断时省略号一旦塞进引号内，线上尺子（web/tests/chat_doubts_e2e.py 的 Q4b：文案里
// 每个「」都要能在用户原话里逐字查到）就会红在「编造引用」上，而那是比不引用更糟的
// 失败形态：用户会以为自己说过那句话。
func TestZeroMaterialMessageQuoteStaysVerbatimWhenClipped(t *testing.T) {
	msg := "帮我写一份会议纪要，这次是上周三那个项目的复盘会，参会的有产品和研发两边的同学，地点在三楼会议室，主持人是张工，时长大约九十分钟。"
	if n := len([]rune(msg)); n <= 40 || n >= 80 {
		t.Fatalf("测试前提不成立：原话 %d 字，必须落在 (40,80) —— 落在别的区间这条用例根本不测截断", n)
	}
	out := ZeroMaterialMessage(msg, []model.Param{{Name: "subject", Label: "会议主题", Required: true}})

	got := ""
	for _, m := range regexp.MustCompile(`「([^」]*)」`).FindAllStringSubmatch(out, -1) {
		if m[1] == "就按你的" { // 出口词是文案自己给的，不算引用原话
			continue
		}
		got = m[1]
	}
	if got == "" {
		t.Fatalf("文案里没有引用用户原话：%q", out)
	}
	if !strings.Contains(msg, got) {
		t.Fatalf("引号里的 %q 不是用户原话的逐字片段 —— 截断时省略号必须留在引号外：%q", got, out)
	}
	if !strings.Contains(out, "…") {
		t.Fatalf("原话被截断了却没有省略号，读起来像用户只说了半句：%q", out)
	}
}
