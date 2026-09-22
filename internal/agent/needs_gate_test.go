package agent

import (
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
