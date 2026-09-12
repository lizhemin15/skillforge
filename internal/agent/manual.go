package agent

import (
	"strings"

	"github.com/lizhemin15/skillforge/internal/model"
)

// 手动模式（聊天界面「指定技能」）——用户点名要哪个技能，就不再让分类器猜。
//
// 这么做的收益不只是「更准」：
//   - 意图分类是一次几十秒的阻塞 LLM 调用。手动模式下这一跳整个省掉，
//     用户按下发送就能看到步骤，而不是先干等半分钟。
//   - 分类器有漂移的可能（把「采购合同」认成通用 docgen），手动模式不存在这个问题。
//
// 代价是参数槽位不再由模型抽取（Needs 留空）：模板类技能走 FillDoc，
// 它会自己从对话里读数据、信息不够时回问；写作类技能直接吃原始消息。
func ManualEval(sc *SkillContent) *Eval {
	ev := &Eval{
		SkillSlug: sc.Slug,
		Reason:    "手动指定技能：" + sc.Name,
		Params:    map[string]interface{}{},
	}
	switch sc.SkillType {
	case model.SkillTypeDocGen:
		ev.Intent = "docgen"
		ev.Action = "gen"
	case model.SkillTypeTemplate:
		ev.Intent = "docgen"
		// 用户既然点名了带模板的技能，默认就是把材料填进模板。
		// 只有明确说"要空白模板"时才改成直接下发文件，见 ManualActionFor。
		ev.Action = "fill"
		if sc.Attachment == "" {
			ev.Action = "write" // 没有附件可填，退回文字生成
		}
	case model.SkillTypeQuery:
		ev.Intent = "query"
		ev.Action = "answer"
	default:
		ev.Intent = "write"
		ev.Action = "write"
	}
	return ev
}

// ManualActionFor 在手动模式下按用户那句话微调动作：点名了模板技能但说的是
// 「发我一份空白模板」，那就不该往模板里填东西。
//
// 只在**手动模式**用。自动模式下的动作必须由分类器判断（关键词匹配会把
// 「这份合同不要空白模板」这类反话读错），这里能成立是因为用户已经用下拉框
// 表达了主意图，剩下的只是"填"还是"发空表"这一个二选一。
func ManualActionFor(sc *SkillContent, msg string) string {
	if sc == nil || sc.SkillType != model.SkillTypeTemplate || sc.Attachment == "" {
		return ""
	}
	if WantsBlankTemplate(msg) {
		return "template_only"
	}
	return "fill"
}

// WantsBlankTemplate 判断用户是不是在明确索取**空白**模板。
// 与 WantsFill 一样只做保守的词面判断：拿不准就返回 false（宁可填也不要发空表）。
func WantsBlankTemplate(msg string) bool {
	h := strings.ToLower(strings.TrimSpace(msg))
	if h == "" {
		return false
	}
	// 出现"不要/别/非"这类否定词时一律不认，避免把「不要空白模板」当成要空表。
	for _, neg := range []string{"不要空", "别给空", "不是空", "非空", "不要模", "别发空"} {
		if strings.Contains(h, neg) {
			return false
		}
	}
	for _, kw := range []string{"空白模板", "空白的模板", "空白表", "空白表格", "空表", "空模板",
		"模板发我", "发我模板", "发个模板", "下发模板", "给我模板", "模板给我", "要模板", "下载模板", "模板下载"} {
		if strings.Contains(h, kw) {
			return true
		}
	}
	return false
}

// ManualSteps 手动模式的步骤骨架：角色标签与前端 AGENTS 映射表保持一致
// （analyze/match/params/generate），否则前端会渲染出空角色名。
// 第一步直接是 done —— 技能已经在点选时就定了，用户应该立刻看到这一点。
func ManualSteps(sc *SkillContent) []TraceStep {
	name := "已选技能"
	if sc != nil && sc.Name != "" {
		name = "「" + sc.Name + "」"
	}
	load := "系统提示词已注入"
	if sc != nil && sc.Template != "" {
		load = "系统提示词 + 模板已注入"
	}
	return []TraceStep{
		{Phase: "analyze", Label: "① 指定技能", Detail: "已选定" + name + "，跳过意图分析", Status: "done"},
		{Phase: "match", Label: "② 载入能力", Detail: load, Status: "done"},
		{Phase: "generate", Label: "③ 执行", Detail: "按" + name + "的约定处理这条输入", Status: "active"},
	}
}
