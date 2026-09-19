package agent

import (
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/store"
)

// 「默认关闭」在产品上必须等价于「模型看不见」。
//
// 这条尺子守的是一个很容易被顺手改坏的性质：内置业务技能（数据治理任务开发）
// 装完就该是停用态 —— 它只在 DataToolbox 场景下才有意义，新装实例不该被它污染
// 技能清单，更不该让模型在写公众号文章时突然开始聊 gov API。
//
// 而 buildRoster 只列 Enabled 技能，所以「停用」和「看不见」是同一件事；
// 哪天有人把 continue 去掉、或者改成语义上的「软停用」（照旧列进清单只是不自动选），
// 用户侧的表现就是「我明明没开这个技能，聊天里它老在掺和」——这条测试会先红。
func TestDisabledBuiltinGovSkillIsInvisibleToRoster(t *testing.T) {
	eng := newTestEngine(t, &fakeProvider{})

	roster, _, err := eng.buildRoster()
	if err != nil {
		t.Fatalf("buildRoster 失败：%v", err)
	}
	// 前提自证：清单得真有内容，否则下面的「不包含」恒真 —— 那是空跑绿。
	// 播种出来的核心技能（办公文档管家）默认启用，它就该在清单里。
	if !strings.Contains(roster, "办公文档管家") {
		t.Fatalf("前提不成立：启用的核心技能都没进清单，先修 roster 再谈停用技能：\n%s", roster)
	}

	if strings.Contains(roster, store.GovTaskDevSlug) {
		t.Errorf("内置业务技能默认停用，不该出现在技能清单里（模型会照着清单选技能）：\n%s", roster)
	}

	// 反向自证：启用之后必须真的出现。否则「默认关闭」就成了「永远用不了」，
	// 而这条测试如果只看停用态，那种坏法它照样全绿。
	if err := eng.store.SetEnabled(store.GovTaskDevSlug, true); err != nil {
		t.Fatalf("启用内置业务技能失败：%v", err)
	}
	roster2, _, err := eng.buildRoster()
	if err != nil {
		t.Fatalf("启用后 buildRoster 失败：%v", err)
	}
	if !strings.Contains(roster2, store.GovTaskDevSlug) {
		t.Errorf("管理员启用后技能仍未进清单 —— 停用态和「看不见」被混在一起实现了：\n%s", roster2)
	}
	// 进了清单还不够：模型是拿「描述」当路由依据的，那一行必须带出它的定位与触发场景，
	// 否则清单里只是多了一行 slug，模型仍然不知道该在什么场合喊它。
	line := rosterLineFor(roster2, store.GovTaskDevSlug)
	if !strings.Contains(line, "数据治理") || !strings.Contains(line, "触发场景") {
		t.Errorf("启用后清单里那一行没带出定位/触发场景，模型无法判断何时该用它：\n%s", line)
	}
}

// rosterLineFor 取清单里 slug=xxx 的那一行（找不到返回空串）。
func rosterLineFor(roster, slug string) string {
	for _, ln := range strings.Split(roster, "\n") {
		if strings.Contains(ln, "slug="+slug) {
			return ln
		}
	}
	return ""
}
