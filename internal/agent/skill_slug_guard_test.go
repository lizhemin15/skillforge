package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/db"
	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/model"
	"github.com/lizhemin15/skillforge/internal/store"
)

// 这一组守的是「分类器给的技能名，进执行链之前必须真在可用清单里」这条确定性守卫。
//
// 为什么必须有（2026-09-26 线上取证）：分类器把 steps 里的**占位词**「通用能力」
// 当成 slug 写了回来，下游拿它查库撞 sql.ErrNoRows，而那一格是 write(evError, …)，
// 用户屏幕上就一行「⚠ sql: no rows in result set」，整轮一个字正文都没有。
// 提示词已收紧，但提示词是软约束：换模型/温度抖一下就会再犯，代价仍是整轮死。
//
// 三条尺子缺一不可：
//   1) 编出来的名字 → 中和掉 + **出声**（静默换路由和报错一样糟）
//   2) 清单里真有的启用技能 → 原样放行（没有这条，一个「无脑清空 slug」的假守卫也能全绿）
//   3) 停用技能 → 同样拦（清单里印不出来，模型从历史上下文翻出来也不该放行）
//
// 全部走行为：喂假 provider 的分类 JSON，看 EvalTurn 回来的 SkillSlug 与进度材料。

func newGuardTestEngine(t *testing.T, fp *fakeProvider) (*Engine, *store.SkillStore) {
	t.Helper()
	srv := fp.server(t)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	sk := store.NewSkillStore(d, dir)
	return New(llm.New(&model.LLMConfig{BaseURL: srv.URL, APIKey: "k", Model: "fake"}), sk), sk
}

// 分类器吐一个指定 slug 的 JSON。三处测试共用，保证「解析成功」是同一个前提。
func classifyJSONWith(slug string) string {
	return `{"intent":"write","action":"write","skill_slug":"` + slug + `","reason":"命中","needs_tools":false,"params":{},"needs":[],"steps":[]}`
}

// 尺子 1：模型编的名字（线上就是占位词「通用能力」）必须在进执行链之前被中和，且出声。
func TestEvalTurnNeutralizesSkillSlugNotInRoster(t *testing.T) {
	fp := &fakeProvider{content: classifyJSONWith("通用能力")}
	eng, _ := newGuardTestEngine(t, fp)

	var materials []string
	ctx := WithProgress(context.Background(), func(s string) { materials = append(materials, s) })

	ev, err := eng.EvalTurn(ctx, "s-guard-miss", "帮我写一份园区开放日的宣传文案", nil)
	if err != nil {
		t.Fatalf("EvalTurn 失败: %v", err)
	}
	// 前提：分类结果**真的解析出来了**。否则「slug 为空」可能只是整条链提前降级，
	// 这条尺子就变成对着错误的原因判绿。intent 是 JSON 里的字段，解析失败必为空。
	if ev.Intent != "write" {
		t.Fatalf("前提不成立：分类 JSON 没被解析（intent=%q）—— 这条尺子会对着"+
			"「意图识别失败」那条降级路判绿，结论没有意义", ev.Intent)
	}
	if strings.TrimSpace(ev.SkillSlug) != "" {
		t.Fatalf("分类器编的技能名 %q 被放行进了执行链 —— 下游拿它查库会撞 sql.ErrNoRows，"+
			"用户屏幕上就是「⚠ sql: no rows in result set」，整轮零正文", ev.SkillSlug)
	}
	joined := strings.Join(materials, "")
	if !strings.Contains(joined, "通用能力") {
		t.Fatalf("降级是静默的：进度材料里没有点名那个不存在的技能（材料=%q）—— "+
			"用户看到的是「正常写完了但在瞎写」，比报错还难查", joined)
	}
	if !strings.Contains(joined, "通用写作") {
		t.Fatalf("降级没讲清改走了哪条路（材料=%q）—— 只说不认识、不说怎么办，"+
			"用户不知道这轮的产出是通用兜底还是真命中了技能", joined)
	}
}

// 尺子 2：正向对照。清单里真有的启用技能必须原样放行。
// 没有这条，把守卫写成「无条件清空 SkillSlug」也能让尺子 1 全绿 —— 而那是把
// 技能路由整个废掉，比原来的 bug 更糟。
func TestEvalTurnKeepsSkillSlugPresentInRoster(t *testing.T) {
	fp := &fakeProvider{content: classifyJSONWith("技能工厂")}
	eng, _ := newGuardTestEngine(t, fp)

	var materials []string
	ctx := WithProgress(context.Background(), func(s string) { materials = append(materials, s) })

	ev, err := eng.EvalTurn(ctx, "s-guard-hit", "把这个重复活儿做成一个技能", nil)
	if err != nil {
		t.Fatalf("EvalTurn 失败: %v", err)
	}
	if ev.SkillSlug != "技能工厂" {
		t.Fatalf("清单里明明有「技能工厂」，却被守卫吃掉了：实际 %q —— "+
			"守卫扫得太宽，技能路由整个失效（用户会看到「它没按我的技能写」）", ev.SkillSlug)
	}
	if joined := strings.Join(materials, ""); strings.Contains(joined, "没找到") {
		t.Fatalf("命中了还在播「没找到」的降级说明（材料=%q）", joined)
	}
}

// 尺子 3：停用技能同样要拦。清单（buildRoster）里只印启用的那些，模型看不见它；
// 万一它从历史上下文里翻出一个已停用的 slug，一样该按未命中处理，而不是放行去撞库。
func TestEvalTurnNeutralizesDisabledSkillSlug(t *testing.T) {
	fp := &fakeProvider{content: classifyJSONWith("技能工厂")}
	eng, sk := newGuardTestEngine(t, fp)

	if err := sk.SetEnabled("技能工厂", false); err != nil {
		t.Fatalf("停用技能失败: %v", err)
	}

	var materials []string
	ctx := WithProgress(context.Background(), func(s string) { materials = append(materials, s) })

	ev, err := eng.EvalTurn(ctx, "s-guard-disabled", "把这个重复活儿做成一个技能", nil)
	if err != nil {
		t.Fatalf("EvalTurn 失败: %v", err)
	}
	if ev.Intent != "write" {
		t.Fatalf("前提不成立：分类 JSON 没被解析（intent=%q）", ev.Intent)
	}
	if ev.SkillSlug != "" {
		t.Fatalf("已停用的技能 %q 被放行了 —— 清单里印不出它，用户以为这能力已经关掉了，"+
			"结果又被静默启用一回", ev.SkillSlug)
	}
	if joined := strings.Join(materials, ""); !strings.Contains(joined, "通用写作") {
		t.Fatalf("拦下停用技能但没出声（材料=%q）", joined)
	}
}
