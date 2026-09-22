package agent

import (
	"testing"

	"github.com/lizhemin15/skillforge/internal/model"
)

// 本文件守的是 2026-09-23 线上一起「同一份输入，交付形态在文件与正文之间翻」的故障。
//
// 现场（journalctl 原文，非二次解析）：同一份 134 字输入，末句「正文不少于 600 字，
// 直接输出正文」，连跑 8 次 —— 4 次分类器把「办公文档管家」(skill_type=docgen) 挑成
// 命中技能，而 intent=write（它其实听懂了用户只要正文），api/chat.go 的发文分支只看
// sc.SkillType==docgen 就直落出文件：屏幕上只有文件卡片，正文是「已为您生成《…docx》，
// 点击下方文件即可下载」39 字回执；另 4 次 skill=- 正常出 944~1078 字正文。
//
// 这类故障的可怕之处是**它不由代码的哪一行决定，而由模型投票决定**（4/8 命中），
// 所以修法不是调提示词，而是在路由上放一把确定性闸门：只要用户明说了交付形态，
// 就不许模型把这一轮抢去出文件。
//
// 下面这张表**两个方向都要守**：
//   - 正向：明说要正文 + 命中 docgen 型技能 → 必须弃技能（否则用户看不到正文，真故障）
//   - 反向：要文件的用户不许被这把闸门拦掉（拦错了是另一种真故障 —— 该给的 Word 没了）。
//     反向用例比正向更值钱：闸门写宽一格，用户要 Word 就拿不到文件，而这**在正向用例里
//     全绿**。所以第 4 条是这张表的压舱石，不许删。
func TestTextOnlyDropsDocGenSkill(t *testing.T) {
	// 线上那一跑的原文，逐字搬运（含末尾那句判据）。
	const liveMsg = "写一份关于开展数据治理专项工作的通知。背景与核心素材：根据《数据安全法》" +
		"要求，各单位需在年底前完成数据资产盘点。正文不少于 600 字，直接输出正文"

	cases := []struct {
		name      string
		msg       string
		skillType string
		manual    bool
		want      bool
	}{
		{
			name:      "线上原文+命中docgen型技能 → 弃技能（本次故障）",
			msg:       liveMsg,
			skillType: model.SkillTypeDocGen,
			manual:    false,
			want:      true,
		},
		{
			name:      "同句但用户手动点名了该技能 → 不拦（点名是最强指令，且点名 docgen 就是选了要文件）",
			msg:       liveMsg,
			skillType: model.SkillTypeDocGen,
			manual:    true,
			want:      false,
		},
		{
			name:      "同句命中的是写作型技能 → 不拦（它本来就不出文件，没有要救的东西）",
			msg:       liveMsg,
			skillType: model.SkillTypeWrite,
			manual:    false,
			want:      false,
		},
		{
			name:      "要文件的用户 → 不拦【反向故障守门，不许删】",
			msg:       "帮我写一份关于开展数据治理专项工作的通知，生成 Word 发我",
			skillType: model.SkillTypeDocGen,
			manual:    false,
			want:      false,
		},
		{
			name:      "否定式「不要生成文件」 → 拦（真语料里的写法，中间夹了动词）",
			msg:       "写个通知，不要生成文件，我复制走",
			skillType: model.SkillTypeDocGen,
			manual:    false,
			want:      true,
		},
		{
			name:      "空消息 → 不拦（没判据就不许动路由）",
			msg:       "",
			skillType: model.SkillTypeDocGen,
			manual:    false,
			want:      false,
		},
		{
			name:      "题材词「通知/报告」不许当判据 → 不拦（否则等于把闸门整个关掉）",
			msg:       "写一份通知，再出一份季度报告",
			skillType: model.SkillTypeDocGen,
			manual:    false,
			want:      false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := TextOnlyDropsDocGenSkill(c.msg, c.skillType, c.manual)
			if got != c.want {
				t.Fatalf("TextOnlyDropsDocGenSkill(%q, %q, manual=%v) = %v，期望 %v\n"+
					"（这条判错方向的后果：正向判错 = 用户明说要正文却拿到 .docx；"+
					"反向判错 = 用户要 Word 却只拿到正文）",
					c.msg, c.skillType, c.manual, got, c.want)
			}
		})
	}
}

// TestTextOnlyDropsDocGenSkill_ExplicitTextOnlyIsTheOnlyJudgement
// 把「弃技能」的**唯一**依据钉死为 ExplicitTextOnly：任何不满足它的输入都不许弃。
// 为什么单独钉：将来若有人把判据改成「题材词也算」或「docgen 一律弃」，这条会红 ——
// 那两种改法都会把要 Word 的用户挡在门外（反向故障）。
func TestTextOnlyDropsDocGenSkill_ExplicitTextOnlyIsTheOnlyJudgement(t *testing.T) {
	msgs := []string{
		"写一份关于开展数据治理专项工作的通知",
		"生成一份季度报告给我",
		"把这个整理成 Word",
		"帮我做一份 PPT",
	}
	for _, m := range msgs {
		if ExplicitTextOnly(m) {
			continue // 明说的，本来就该弃，交给上一条测试管
		}
		if TextOnlyDropsDocGenSkill(m, model.SkillTypeDocGen, false) {
			t.Fatalf("输入 %q 并没有明说要正文，却被判成「弃用文档生成技能」—— "+
				"这是反向故障方向：要文件的用户拿不到文件", m)
		}
	}
}
