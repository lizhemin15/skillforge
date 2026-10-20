package skillgen

import (
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/model"
)

// 这一组测试钉住「创建时手选产物类型」这条入口。
//
// 为什么必须单独钉：类型是**看不见的产物属性**——生成的技能无论被标成 write 还是
// query，文件都在、页面上都不报错。类型选错的唯一症状是「用户让它写稿，它开始答问」，
// 而那要等到第一次真实使用才暴露。所以断言全部打在类型码 + 判定理由 + 警示上，不靠肉眼。
//
// 另外这组测试顺带钉住一条容易想当然的事：极速通道的素材硬门（指南≥40字、范文≥100字）
// 是**写作型专属**的。查询型技能要的是事实来源，不是文风样板，所以手选非写作型时必须
// 免掉这两道门——否则界面上的类型选择按钮等于摆设（这是写测试时才发现的，见
// TestPickTypeQueryPlainTextPassesGate）。

// liteGuideOK 是一份合格的写作指南（≥ liteMinGuideChars）。
const liteGuideOK = "# 通知写作指南\n" +
	"写通知要讲清主体、事由、时间三要素。开头一句话点明事由，不要铺垫；正文分条列出安排，" +
	"每条一个意思，能落到时间与责任人的地方都写上去；结尾写明执行要求与联系方式。语气正式，不口语化。\n"

// liteExampleOK 是一篇合格的范文（≥ liteMinExampleChars）。
const liteExampleOK = "关于调整办公时间的通知\n" +
	"各部门：因设备检修需要，本周六办公时间调整为九时至十五时，食堂与班车同步调整。" +
	"请各部门提前安排当日工作，确有紧急事项的，联系值班室统一登记后处理。" +
	"检修期间门禁系统间歇性停用，请随身携带工牌以备人工核验。特此通知，请相互转告。\n"

// litePlainText 是一段够长但没有指南、没有范文的纯正文（模拟「一份制度/公告原文」）。
// 这类素材在自动模式下会被硬门拒掉——这本身就是设计（系统不替用户猜意图）。
const litePlainText = "市民卡办理须知\n" +
	"本人携带身份证到任一网点窗口办理，填写申请表并现场拍照。制卡周期为七个工作日，" +
	"期间可凭回执单查询进度。领卡时需本人持身份证签收，代领需提供委托书与双方证件复印件。" +
	"卡片遗失可挂失补办，补办费用按现行标准收取。未满十六周岁由监护人陪同办理。" +
	"办理时间为工作日九时至十七时，法定节假日暂停。以上事项如有调整，以窗口公告为准。\n"

// writingMaterial 返回一份「指南 + 范文」齐备的合格写作素材。
func writingMaterial() string {
	return liteGuideOK + "\n## 范文\n" + liteExampleOK
}

// 前提校验：这份素材在**没指定类型**时确实被判为写作型。
// 后面的「手选覆盖」测试都拿它当对照，前提不成立的话，测的就不是覆盖了。
func TestPickTypeAutoBaselineWrites(t *testing.T) {
	mat, err := collectLiteMaterial(&LiteInput{Name: "通知写作", Material: writingMaterial()}, nil)
	if err != nil {
		t.Fatalf("合格写作素材不该被拒: %v", err)
	}
	if mat.Type != model.SkillTypeWrite {
		t.Fatalf("前提不成立：指南+范文齐备应自动判为 write，实际 %q（理由：%s）", mat.Type, mat.TypeWhy)
	}
	if strings.Contains(mat.TypeWhy, "用户指定") {
		t.Errorf("没指定类型时理由里不该出现「用户指定」，实际：%s", mat.TypeWhy)
	}
}

// 手选写作型：类型必须是 write，且理由要写明**来源是用户**（管理端得能分清谁定的）。
func TestPickTypeWriteExplicit(t *testing.T) {
	mat, err := collectLiteMaterial(&LiteInput{
		Name:      "通知写作",
		Material:  writingMaterial(),
		SkillType: model.SkillTypeWrite,
	}, nil)
	if err != nil {
		t.Fatalf("拆素材失败: %v", err)
	}
	if mat.Type != model.SkillTypeWrite {
		t.Fatalf("用户指定 write，产物类型却是 %q", mat.Type)
	}
	if !strings.Contains(mat.TypeWhy, "用户指定") {
		t.Errorf("理由应写明类型来自用户指定，实际：%s", mat.TypeWhy)
	}
}

// 核心用例一：素材明明是合格写作素材（自动判定会给 write），用户选查询型 → 必须出查询型。
//
// 这是功能存在的意义本身。判据再准也只是启发式：同一份办理流程文档，做「答问助手」和做
// 「据此写办事通知的写作助手」都合理，素材本身答不了「用户要哪一个」这个题。
func TestPickTypeQueryOverridesWritingDetection(t *testing.T) {
	mat, err := collectLiteMaterial(&LiteInput{
		Name:      "办公事项咨询",
		Material:  writingMaterial(),
		SkillType: model.SkillTypeQuery,
	}, nil)
	if err != nil {
		t.Fatalf("拆素材失败: %v", err)
	}
	if mat.Type != model.SkillTypeQuery {
		t.Fatalf("用户指定 query，产物类型却是 %q —— 手选被忽略了", mat.Type)
	}
	if !strings.Contains(mat.TypeWhy, "用户指定") {
		t.Errorf("理由应写明来自用户指定，实际：%s", mat.TypeWhy)
	}
}

// 核心用例二：手选查询型 + **纯正文**（无指南、无范文）→ 必须能建出来。
//
// 修之前这里必然报「没找到够长的写作指南」。两道素材门是写作型专属的：查询型技能的素材
// 是事实来源，不需要文风样板。若不放宽，用户在界面上郑重选了「查询型」，仍旧被回一句
// 「没找到写作指南」——那个按钮就成了摆设，正是这次要消灭的体验。
func TestPickTypeQueryPlainTextPassesGate(t *testing.T) {
	mat, err := collectLiteMaterial(&LiteInput{
		Name:      "市民卡办理咨询",
		Material:  litePlainText,
		SkillType: model.SkillTypeQuery,
	}, nil)
	if err != nil {
		t.Fatalf("手选查询型时纯正文素材必须放行，却报错: %v", err)
	}
	if mat.Type != model.SkillTypeQuery {
		t.Fatalf("类型应为 query，实际 %q", mat.Type)
	}
	// 放宽了，就必须说清凭什么放宽——这是管理端事后追查的唯一线索。
	if !strings.Contains(mat.Warn, "放宽") {
		t.Errorf("放宽了素材门却没有留痕，Warn=%q", mat.Warn)
	}
	if !strings.Contains(mat.Warn, "创建者已明确指定") {
		t.Errorf("放宽依据必须写「创建者已明确指定」（而不是写成「源文件里带着附件」那种假话），Warn=%q", mat.Warn)
	}
	if strings.Contains(mat.Warn, "源文件里确实带着可下发的附件") {
		t.Errorf("本次放宽跟附件无关，Warn 里不该出现附件作为依据（留痕不能撒谎）：%q", mat.Warn)
	}
}

// 手选模板型 + 纯正文：类型照用户走，但必须警示「没有可下发的附件」。
// 模板型技能会承诺「把文件发给你」，而素材里一个附件都没有 = 承诺落差。
// 不硬失败（可事后补传模板源文件重训），但绝不能静默。
func TestPickTypeTemplateWithoutAttachmentWarns(t *testing.T) {
	mat, err := collectLiteMaterial(&LiteInput{
		Name:      "市民卡模板下发",
		Material:  litePlainText,
		SkillType: model.SkillTypeTemplate,
	}, nil)
	if err != nil {
		t.Fatalf("拆素材失败: %v", err)
	}
	if mat.Type != model.SkillTypeTemplate {
		t.Fatalf("类型应为用户指定的 template，实际 %q", mat.Type)
	}
	if !strings.Contains(mat.Warn, "没有拆出任何可下发的附件") {
		t.Errorf("模板型却没有附件时必须留警示，Warn=%q", mat.Warn)
	}
}

// 兼容底线：类型留空（老前端 / 「自动判断」）时，纯正文素材仍然要被拒。
//
// 这条守着的是那条老口径——放宽只认「有附件」或「用户明说」，**不认**「正文够长」。
// 一旦这里变绿（即开始放行），上传「只有指南没有范文」的写作意图用户会被静默改成答问技能，
// 比直接报错更让人摸不着头脑。同时报错文案要指出「选类型」这个新出口。
func TestPickTypeEmptyStillRejectsPlainText(t *testing.T) {
	_, err := collectLiteMaterial(&LiteInput{Name: "市民卡办理", Material: litePlainText}, nil)
	if err == nil {
		t.Fatal("没指定类型时纯正文素材必须仍被拒（放宽只认「有附件」或「用户明说」）")
	}
	if !strings.Contains(err.Error(), "写作指南") {
		t.Errorf("报错应说明缺的是写作指南，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "产物类型") {
		t.Errorf("报错文案应指出「把产物类型选成查询型/模板型」这个出口，实际: %v", err)
	}
}

// 类型解析白名单：只认界面上真能选的三项 + 空/auto；认不出的一律拒绝。
//
// 为什么拒绝而不是回落成自动：用户点了「查询型」但前端传了个拼错的值，回落会自动生成一个
// 类型未知的技能，而用户以为自己选的就是那种——产物上看不出来，只能等用错才发现。
func TestParsePickedSkillType(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", true},     // 没选 → 自动判定
		{"auto", "", true}, // 界面上的「自动判断」
		{"AUTO", "", true}, // 大小写不敏感（表单值不该因大小写失效）
		{" write ", "write", true},
		{"query", "query", true},
		// template 已并入 query：老前端/老脚本仍可能传它，接受但折算成 query
		// （语义不变：问答 + 有附件就下发）。见 model.NormalizeSkillType。
		{"template", "query", true},
		{"TEMPLATE", "query", true},
		{"Query", "query", true},
		{"docgen", "", false}, // 未开放的类型 → 拒绝，不许默默当自动
		{"writ", "", false},   // 拼错
		{"写作", "", false},     // 中文标签不是协议值
		{"write;drop", "", false},
	}
	for _, c := range cases {
		got, ok := model.ParsePickedSkillType(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("ParsePickedSkillType(%q) = (%q, %v)，期望 (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
