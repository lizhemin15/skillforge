package agent

import (
	"strings"
	"testing"
)

// 极速写作模式（quick）的纯函数防线。
//
// 这一档链路极短（起草→自检），但正因为短，每一环都是单点：
// verdict 解析挂了 = 自检永远「通过」，用户再也不会看到「需要补充」提示，
// 整个阶段二静默失效 —— 这种坏法在线上没有任何报错，只有防线能逮住。

// parseQuickVerdict：正常 JSON、围栏包裹、垃圾输入三条路径。
func TestParseQuickVerdict(t *testing.T) {
	t.Run("裸 JSON 原样解析", func(t *testing.T) {
		v := parseQuickVerdict(`{"pass":false,"items":[{"problem":"语气不正式","ask":"目标读者是谁"}]}`)
		if v == nil {
			t.Fatalf("合法 JSON 解析成了 nil（会被当通过，自检静默失效）")
		}
		if v.Pass {
			t.Fatalf("pass=false 被解析成了 true")
		}
		if len(v.Items) != 1 || v.Items[0].Problem != "语气不正式" || v.Items[0].Ask != "目标读者是谁" {
			t.Fatalf("items 解析错了: %+v", v.Items)
		}
	})
	t.Run("模型爱加 markdown 围栏，得剥掉", func(t *testing.T) {
		v := parseQuickVerdict("审稿结论：\n```json\n{\"pass\":true,\"items\":[]}\n```\n以上。")
		if v == nil || !v.Pass {
			t.Fatalf("围栏内的 JSON 没解析出来: %+v", v)
		}
	})
	t.Run("垃圾输出返回 nil（调用方按通过处理）", func(t *testing.T) {
		if v := parseQuickVerdict("我觉得写得不错"); v != nil {
			t.Fatalf("非 JSON 输出不该产出 verdict: %+v", v)
		}
	})
	t.Run("空 problem/ask 的条目要清掉", func(t *testing.T) {
		v := parseQuickVerdict(`{"pass":false,"items":[{"problem":"  ","ask":""},{"problem":"缺少数据","ask":"给出具体数字"}]}`)
		if v == nil || len(v.Items) != 1 {
			t.Fatalf("空条目没被清洗: %+v", v)
		}
		if v.Items[0].Problem != "缺少数据" {
			t.Fatalf("留下的条目不对: %+v", v.Items)
		}
	})
}

// QuickReviewNote：提示段只在「不达标且有具体条目」时出现。
// pass/nil/空 items 一律出空串 —— 否则每篇稿后面都挂着一句
// 「自检未通过」却什么都说不出，用户会以为站点坏了。
func TestQuickReviewNote(t *testing.T) {
	if QuickReviewNote(nil) != "" {
		t.Fatalf("nil verdict（自检抖动）不该渲染提示段")
	}
	if QuickReviewNote(&QuickVerdict{Pass: true}) != "" {
		t.Fatalf("通过时不该渲染提示段")
	}
	if QuickReviewNote(&QuickVerdict{Pass: false}) != "" {
		t.Fatalf("pass=false 但拿不出条目，按通过处理，不该渲染提示段")
	}

	got := QuickReviewNote(&QuickVerdict{Pass: false, Items: []QuickGap{
		{Problem: "缺少数据支撑", Ask: "去年的采购总额是多少"},
		{Problem: "", Ask: "目标读者是谁"},
		{Problem: "结尾太仓促", Ask: ""},
	}})
	for _, want := range []string{"自检未通过", "差这 3 处", "1. 缺少数据支撑", "需要你补充", "2. **需要你补充**：目标读者是谁",
		"3. 结尾太仓促", "下一版直接落实"} {
		if !strings.Contains(got, want) {
			t.Fatalf("提示段缺 %q:\n%s", want, got)
		}
	}
}

// quickDraftSys：管理员配的是「要求清单」，直接当 system prompt 用会被模型
// 原样复述（那等于什么都没写）——必须补上「产出成稿」的任务指令。
func TestQuickDraftSysAppendsTask(t *testing.T) {
	got := quickDraftSys("三段以内。")
	if !strings.HasPrefix(got, "三段以内。") {
		t.Fatalf("管理端提示词必须原样打头: %q", got)
	}
	if !strings.Contains(got, "产出成稿") || !strings.Contains(got, "不要复述要求") {
		t.Fatalf("缺少任务指令: %q", got)
	}
}
