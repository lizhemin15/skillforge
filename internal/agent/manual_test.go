package agent

import (
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/model"
)

func tplSkill() *SkillContent {
	return &SkillContent{Slug: "采购合同", Name: "采购合同", SkillType: model.SkillTypeTemplate, Attachment: "采购合同模板.docx"}
}

// 手动模式的全部价值就是「不再猜」：技能是用户点名的，动作必须跟着技能类型走。
func TestManualEvalBySkillType(t *testing.T) {
	cases := []struct {
		name       string
		sc         *SkillContent
		wantIntent string
		wantAction string
	}{
		{"模板技能→填空", tplSkill(), "docgen", "fill"},
		{"写作技能→写作", &SkillContent{Slug: "公司新闻通稿", Name: "公司新闻通稿", SkillType: model.SkillTypeWrite}, "write", "write"},
		{"文档生成→出文件", &SkillContent{Slug: "办公文档管家", Name: "办公文档管家", SkillType: model.SkillTypeDocGen}, "docgen", "gen"},
		{"办事查询→问答", &SkillContent{Slug: "公积金办事", Name: "公积金办事", SkillType: model.SkillTypeQuery}, "query", "answer"},
		// 模板技能但没附件：没有模板可填，硬走 fill 会去下载一个不存在的文件。
		{"模板技能无附件→退回写作", &SkillContent{Slug: "空模板", Name: "空模板", SkillType: model.SkillTypeTemplate}, "docgen", "write"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := ManualEval(c.sc)
			if ev.Intent != c.wantIntent || ev.Action != c.wantAction {
				t.Fatalf("ManualEval(%s) = intent %q action %q, want %q/%q",
					c.sc.Slug, ev.Intent, ev.Action, c.wantIntent, c.wantAction)
			}
			if ev.SkillSlug != c.sc.Slug {
				t.Fatalf("SkillSlug = %q, want %q", ev.SkillSlug, c.sc.Slug)
			}
		})
	}
}

// 手动模式不能把用户的话交给参数抽取：Needs 一旦非空，chat 处理器就会停下来
// 追问参数，用户点名的技能反而要等两轮 —— 这是产品上最不能接受的退化。
func TestManualEvalDoesNotGateOnParams(t *testing.T) {
	ev := ManualEval(tplSkill())
	if len(ev.Needs) != 0 {
		t.Fatalf("Needs should be empty in manual mode, got %v", ev.Needs)
	}
	if ev.NeedsTools {
		t.Fatal("NeedsTools must be false in manual mode")
	}
}

// 步骤骨架必须满足前端的三条硬约定，否则轨迹面板渲染出空白角色或"永远转圈"：
//  1. 恰好一个 active（前端靠它判断"还在跑"），且必须是最后一步；
//  2. 前面若干步是 done（用户点完技能就该看到"已选定"）；
//  3. phase 落在前端 AGENTS 映射表里。
func TestManualStepsShape(t *testing.T) {
	steps := ManualSteps(tplSkill())
	if len(steps) == 0 {
		t.Fatal("no steps")
	}
	active := 0
	for i, s := range steps {
		if s.Status == "active" {
			active++
			if i != len(steps)-1 {
				t.Fatalf("active step must be the last one, got index %d of %d", i, len(steps))
			}
		}
		if _, ok := map[string]bool{"analyze": true, "match": true, "params": true, "generate": true}[s.Phase]; !ok {
			t.Fatalf("phase %q not in frontend AGENTS map", s.Phase)
		}
		if s.Label == "" {
			t.Fatalf("step %d has empty label", i)
		}
	}
	if active != 1 {
		t.Fatalf("want exactly 1 active step, got %d", active)
	}
	// 技能名要出现在第一步，用户才能确认"我选的确实是这个"
	if !strings.Contains(steps[0].Detail, "采购合同") {
		t.Fatalf("first step should name the skill, got %q", steps[0].Detail)
	}
	// 空技能不能 panic（前端可能传来已被删除的 slug）
	if got := ManualSteps(nil); len(got) != 3 {
		t.Fatalf("ManualSteps(nil) = %d steps, want 3", len(got))
	}
}

// 手动模式唯一保留的关键词判断：点名的技能是模板，用户又明确说"要空白模板"，
// 那就不该往里填东西。只测这个二选一，不做通用意图识别。
func TestManualActionFor(t *testing.T) {
	cases := []struct {
		msg  string
		want string
	}{
		{"发我一份空白模板", "template_only"},
		{"给我空白表格", "template_only"},
		{"下载模板", "template_only"},
		{"把甲方写成北京华创，金额 12 万", "fill"},
		// 反话：说了"不要空白模板"就该填，不能因为命中"空白模板"就发空表
		{"不要空白模板，把材料填进去", "fill"},
		{"", "fill"},
	}
	for _, c := range cases {
		if got := ManualActionFor(tplSkill(), c.msg); got != c.want {
			t.Fatalf("ManualActionFor(%q) = %q, want %q", c.msg, got, c.want)
		}
	}
	// 写作类技能不该被这个函数改动作（返回空串 = 保持 ManualEval 的决定）
	if got := ManualActionFor(&SkillContent{SkillType: model.SkillTypeWrite}, "给我空白模板"); got != "" {
		t.Fatalf("non-template skill should not be overridden, got %q", got)
	}
	if got := ManualActionFor(nil, "给我空白模板"); got != "" {
		t.Fatalf("nil skill should be safe, got %q", got)
	}
}
