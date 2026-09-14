package skillgen

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- 降级交付门禁：裁判没跑完 / 没过线时，交付必须显性标记 ----
//
// 这一组用例盯的是「未验收通过的技能会不会静默落盘」。此前的现场是这样的：
// 训练跑在 SSE 请求上，浏览器一关 → r.Context() 被 cancel → 3 轮回炉预算清零
// → fidelity.md 里留下一行「回炉失败: context canceled」，然后一份裁判判
// 10/100「不通过」的技能照常落盘交付，前端仍然显示「✔ 新技能已就绪」。
// 用户看到的就是「生成的技能和我给的素材完全没关系」而系统毫无提示。
//
// 三条断言分别对应三处必须写下来的地方：报告判定（Degraded）、SSE done 帧
// （Result.Degraded）、磁盘产物（meta.json + fidelity.md）。

// TestDegradedMarksJudgeFailure：裁判一轮都没跑成 → 降级，且原因要点名「没跑完」。
func TestDegradedMarksJudgeFailure(t *testing.T) {
	mp := synthPack()
	fake := &fakeChat{reply: func(c fakeCall) (string, error) {
		if c.JSON {
			return "", nil // 裁判返回空串 → 解析失败
		}
		return "草稿", nil
	}}
	g := &Generator{}
	g.SetChatClient(fake)

	rep := g.judgeLoop(context.Background(), mp, "PROMPT_V1", nil, func(string) {})

	deg, reason := rep.Degraded()
	if !deg {
		t.Fatalf("裁判没跑成必须判降级，实际 deg=false（reason=%q）", reason)
	}
	if !strings.Contains(reason, "一轮都没跑完") {
		t.Fatalf("降级原因应说明裁判没跑完，实际 %q", reason)
	}
	if rep.Passed() {
		t.Fatal("前置条件不成立：裁判没跑成不该算通过")
	}
}

// TestDegradedMarksBelowPassLine：裁判跑完了但最优一轮没过线 → 降级，
// 原因里要带分数与通过线，让管理员知道差多少。
func TestDegradedMarksBelowPassLine(t *testing.T) {
	mp := synthPack()
	fake := newLoopChat(loopHalfMarks())
	g := &Generator{}
	g.SetChatClient(fake)

	// revise 传 nil：单轮试用后停，够用来造「跑完了但不过线」。
	rep := g.judgeLoop(context.Background(), mp, "PROMPT_V1", nil, func(string) {})

	if rep.Passed() {
		t.Fatal("前置条件不成立：半量分数不该判通过")
	}
	if len(rep.Rounds) == 0 {
		t.Fatal("前置条件不成立：应该有至少一轮有效评分")
	}
	best := rep.Best()
	deg, reason := rep.Degraded()
	if !deg {
		t.Fatalf("最优轮 %d 分没过线必须判降级，实际 deg=false", best.Result.Total)
	}
	if !strings.Contains(reason, "未过线") {
		t.Fatalf("降级原因应点名未过线，实际 %q", reason)
	}
	if !strings.Contains(reason, itoa(best.Result.Total)) {
		t.Fatalf("降级原因应带上实际得分 %d，实际 %q", best.Result.Total, reason)
	}
}

// TestNotDegradedWhenJudgePasses：裁判判通过 → 不降级。
// 少了这条负例，「恒返回 true」的实现也能让上面两条绿。
func TestNotDegradedWhenJudgePasses(t *testing.T) {
	mp := synthPack()
	fake := newLoopChat(loopFullMarks())
	g := &Generator{}
	g.SetChatClient(fake)

	rep := g.judgeLoop(context.Background(), mp, "PROMPT_V1", nil, func(string) {})

	if !rep.Passed() {
		t.Fatal("前置条件不成立：满分应判通过")
	}
	if deg, reason := rep.Degraded(); deg {
		t.Fatalf("裁判通过不该判降级，实际 deg=true reason=%q", reason)
	}
	// 没跑裁判的技能（通用流程）不能被误标成降级，否则提示会变成噪音。
	var nilRep *JudgeReport
	if deg, reason := nilRep.Degraded(); deg {
		t.Fatalf("未跑裁判不该判降级，实际 deg=true reason=%q", reason)
	}
}

// TestLandMarksDegradedSkillOnDisk：降级必须落在磁盘产物上——meta.json 带
// degraded/degrade_reason，fidelity.md 顶部有醒目告示。
// 只看内存里的 Result 不够：管理员事后翻技能目录时，看到的只有这两个文件。
func TestLandMarksDegradedSkillOnDisk(t *testing.T) {
	land := func(t *testing.T, rep *JudgeReport) (meta map[string]any, fidelity string) {
		t.Helper()
		dir := t.TempDir()
		mp := synthPack()
		mp.Judge = rep
		g := &Generator{}
		in := &Input{Name: "降级验收用例", Category: "general", Description: "d"}
		if err := g.land(dir, strings.Repeat("提示词正文。", 80), "", nil, in, "", &typeOut{Type: "write"}, mp); err != nil {
			t.Fatalf("落盘失败: %v", err)
		}
		mb, err := os.ReadFile(filepath.Join(dir, "meta.json"))
		if err != nil {
			t.Fatalf("读 meta.json 失败: %v", err)
		}
		if err := json.Unmarshal(mb, &meta); err != nil {
			t.Fatalf("meta.json 不是合法 JSON: %v", err)
		}
		fb, err := os.ReadFile(filepath.Join(dir, "fidelity.md"))
		if err != nil {
			t.Fatalf("读 fidelity.md 失败: %v", err)
		}
		return meta, string(fb)
	}

	// ① 降级：裁判判 10/100 不通过（对齐线上现场的分数量级）
	degraded := &JudgeReport{
		Rounds:    []JudgeRound{{Round: 1, Result: &JudgeResult{Total: 10, Pass: false}}},
		BestRound: 1,
		Err:       "回炉失败: context canceled",
	}
	meta, fidelity := land(t, degraded)
	if meta["degraded"] != true {
		t.Fatalf("meta.json 应带 degraded=true，实际 %v", meta["degraded"])
	}
	reason, _ := meta["degrade_reason"].(string)
	if !strings.Contains(reason, "10/100") || !strings.Contains(reason, "未过线") {
		t.Fatalf("meta.json 降级原因应含分数与判定，实际 %q", reason)
	}
	// fidelity.md 的告示必须在正文最前面，翻到文件中部才看到等于没写。
	head := fidelity
	if i := strings.Index(head, "## 裁判评分"); i > 0 {
		head = head[:i]
	}
	if !strings.Contains(head, "降级交付") {
		t.Fatalf("fidelity.md 顶部应标注降级交付，实际前 %d 字为 %q", len(head), head)
	}
	if !strings.Contains(head, "未经裁判验收通过") {
		t.Fatalf("fidelity.md 应写明未经验收通过，实际 %q", head)
	}

	// ② 未降级：meta.json 不该出现 degraded 键（omitempty 之外还要真的不写，
	//    否则「字段存在但为 false」会让磁盘检查和前端判断都变含糊）。
	passed := &JudgeReport{
		Rounds:    []JudgeRound{{Round: 1, Result: &JudgeResult{Total: 95, Pass: true}}},
		BestRound: 1,
	}
	meta2, fidelity2 := land(t, passed)
	if _, ok := meta2["degraded"]; ok {
		t.Fatalf("验收通过的技能不该在 meta.json 里出现 degraded 键，实际 %v", meta2["degraded"])
	}
	if strings.Contains(fidelity2, "降级交付") {
		t.Fatal("验收通过的技能 fidelity.md 不该出现降级告示")
	}
}
