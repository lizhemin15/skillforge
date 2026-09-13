package skillgen

import (
	"context"
	"strings"
	"testing"
)

// ---- Step 8.5 裁判循环：止损规则与交付决策 ----
//
// 这一组用例盯的都是「循环会不会跑偏」，而不是「分数算得对不对」（后者在 judge_test.go）。
// 循环跑偏的三种表现都很贵：该停不停（白烧模型调用）、该跑不跑（悄悄交了个没验收过的
// 技能）、交付错了那一轮（用户拿到比手上更差的版本）。三种都不会报错，只会静默变差。

// loopFullMarks 五维满分，用于「模型判通过」的场景。
func loopFullMarks() map[string]int {
	return map[string]int{
		"category_routing": 25, "requirement_compliance": 25, "example_alignment": 20,
		"structure_completeness": 15, "no_hallucination": 15,
	}
}

// loopHalfMarks 五维各拿一半左右，总分不到 80 且多维低于下限，稳定判不通过。
func loopHalfMarks() map[string]int {
	return map[string]int{
		"category_routing": 12, "requirement_compliance": 12, "example_alignment": 10,
		"structure_completeness": 8, "no_hallucination": 8,
	}
}

// loopChat 造一个能分辨「试用写稿」与「裁判打分」两种调用的替身。
// 分辨依据是 jsonMode：裁判必须开 JSON 模式（judgeDraft 的硬约定），试用写稿不开。
// scores 按试用次数依次取值，用完取最后一个——这样才能造出「第几轮得几分」的序列。
type loopChat struct {
	*fakeChat
	trials []string
	scores []map[string]int
	trialN int
}

func newLoopChat(scores ...map[string]int) *loopChat {
	l := &loopChat{scores: scores}
	l.fakeChat = &fakeChat{reply: func(c fakeCall) (string, error) {
		if c.JSON {
			i := l.trialN - 1
			if i < 0 {
				i = 0
			}
			if i >= len(l.scores) {
				i = len(l.scores) - 1
			}
			return judgeJSON(l.scores[i]), nil
		}
		l.trials = append(l.trials, c.Sys)
		l.trialN++
		return "第 " + itoa(l.trialN) + " 轮草稿正文。", nil
	}}
	return l
}

// TestJudgeLoopStopsWhenFirstRoundPasses：① 通过即停。
// 过线后再试一轮只烧钱，还可能试出更差的版本。
func TestJudgeLoopStopsWhenFirstRoundPasses(t *testing.T) {
	mp := synthPack()
	fake := newLoopChat(loopFullMarks())
	g := &Generator{}
	g.SetChatClient(fake)
	var steps []string

	rep := g.judgeLoop(context.Background(), mp, "PROMPT_V1", nil, func(s string) { steps = append(steps, s) })

	if len(rep.Rounds) != 1 {
		t.Fatalf("首轮通过应只跑 1 轮，实际 %d 轮", len(rep.Rounds))
	}
	if rep.Err != "" {
		t.Fatalf("不该报错：%s", rep.Err)
	}
	if !rep.Passed() {
		t.Fatal("Passed() 应为 true")
	}
	if rep.BestRound != 1 || rep.DeliveredPrompt != "PROMPT_V1" {
		t.Fatalf("交付决策错：round=%d prompt=%q", rep.BestRound, rep.DeliveredPrompt)
	}
	if rep.EarlyStop != "" {
		t.Fatalf("通过不该有止损文案，实际 %q", rep.EarlyStop)
	}
	// 通过即停的实质：只发生「一次试用 + 一次裁判」，不多跑。
	if len(fake.trials) != 1 || len(fake.calls) != 2 {
		t.Fatalf("调用次数不对：试用 %d 次 / 总调用 %d 次", len(fake.trials), len(fake.calls))
	}
	// 试用必须拿到本轮提示词原文——少了它，量的是「类名 + 场景」而不是技能。
	if !strings.Contains(fake.trials[0], "PROMPT_V1") {
		t.Fatalf("试用没拿到本轮提示词，实际收到：%q", fake.trials[0])
	}
	// trace 帧双条件：分类名 + 轮次都要出现，只断一个会被用户原话里的分类名带偏。
	line := strings.Join(steps, "\n")
	if !strings.Contains(line, "8.5/9") || !strings.Contains(line, "裁判第 1 轮") {
		t.Fatalf("trace 缺裁判帧：%q", line)
	}
	if !strings.Contains(line, "会议纪要") || !strings.Contains(line, "通过") {
		t.Fatalf("trace 帧未显性化本轮分类/结论：%q", line)
	}
}

// TestJudgeLoopRevisesUntilRoundCap：③ 到上限即停，且交付「最优一轮」。
// 第 3 轮回炉后反而更差（40 分），交付必须是第 2 轮（90 分）——这就是
// 「交付最优而非最后一轮」这条决策的防线；少了它，用户拿到的是更差的版本。
func TestJudgeLoopRevisesUntilRoundCap(t *testing.T) {
	mp := synthPack()
	fake := newLoopChat(
		// 60 分：总分不到线（判定为不通过，且没有任何单维塌方）。
		map[string]int{"category_routing": 15, "requirement_compliance": 15, "example_alignment": 12, "structure_completeness": 9, "no_hallucination": 9},
		// 79 分：差 1 分过线 —— 最容易被误当成「够好了」的一轮，也必须是交付轮。
		map[string]int{"category_routing": 20, "requirement_compliance": 19, "example_alignment": 16, "structure_completeness": 12, "no_hallucination": 12},
		// 32 分：回炉后反而崩了（模型有噪声，这是常态）。绝不能因此交付它。
		map[string]int{"category_routing": 8, "requirement_compliance": 8, "example_alignment": 6, "structure_completeness": 5, "no_hallucination": 5},
	)
	g := &Generator{}
	g.SetChatClient(fake)
	var revised []string

	rep := g.judgeLoop(context.Background(), mp, "PROMPT_V1",
		func(_ context.Context, res *JudgeResult, prev string) (string, error) {
			revised = append(revised, prev)
			return "PROMPT_V" + itoa(len(revised)+1), nil
		},
		func(string) {})

	if len(rep.Rounds) != judgeMaxRounds {
		t.Fatalf("一直不通过应跑满 %d 轮，实际 %d 轮", judgeMaxRounds, len(rep.Rounds))
	}
	if len(revised) != judgeMaxRounds-1 {
		t.Fatalf("回炉次数应为 %d 次（末轮不再回炉），实际 %d 次", judgeMaxRounds-1, len(revised))
	}
	if rep.Passed() {
		t.Fatal("从未过线，Passed() 应为 false")
	}
	if !strings.Contains(rep.EarlyStop, "上限") {
		t.Fatalf("止损文案应写明到达上限，实际 %q", rep.EarlyStop)
	}
	// 每轮回炉必须拿到「上一轮那一版」，否则回炉是在改一份已经不存在的稿子。
	if revised[0] != "PROMPT_V1" || revised[1] != "PROMPT_V2" {
		t.Fatalf("回炉入参不是上一版：%v", revised)
	}
	// 每轮试用的提示词必须逐轮换：第 3 轮还在用 V1 说明回炉结果没被接上。
	if len(fake.trials) != 3 || !strings.Contains(fake.trials[2], "PROMPT_V3") {
		t.Fatalf("第 3 轮试用应拿到回炉后的 V3，实际 %v", fake.trials)
	}
	if rep.BestRound != 2 || rep.DeliveredPrompt != "PROMPT_V2" {
		t.Fatalf("应交付最优的第 2 轮（90 分），实际 round=%d prompt=%q", rep.BestRound, rep.DeliveredPrompt)
	}
}

// TestJudgeLoopEarlyStopsWhenAllFindingsAreHard：② 扣分项全来自确定性硬校验时立刻停。
// 手册本身缺范文，回炉改的是提示词，改不动这件事——继续跑只会把「手册有问题」
// 掩盖成「提示词不够好」，还把手册缺陷摊薄成几轮低分。
func TestJudgeLoopEarlyStopsWhenAllFindingsAreHard(t *testing.T) {
	mp := synthPack()
	delete(mp.Examples, "会议纪要") // 有锚点却一篇没切出来 → 硬校验命中
	fake := newLoopChat(loopFullMarks()) // 模型甚至给满分，硬校验照样否决
	g := &Generator{}
	g.SetChatClient(fake)
	reviseCalled := false

	rep := g.judgeLoop(context.Background(), mp, "PROMPT_V1",
		func(context.Context, *JudgeResult, string) (string, error) {
			reviseCalled = true
			return "PROMPT_V2", nil
		},
		func(string) {})

	if len(rep.Rounds) != 1 {
		t.Fatalf("硬校验缺陷应立即止损，实际跑了 %d 轮", len(rep.Rounds))
	}
	if reviseCalled {
		t.Fatal("手册数据缺陷不该触发回炉：改提示词改不动范文没切出来")
	}
	if !strings.Contains(rep.EarlyStop, "硬校验") {
		t.Fatalf("止损文案应点明硬校验，实际 %q", rep.EarlyStop)
	}
	if rep.Passed() {
		t.Fatal("硬校验命中时绝不能判通过")
	}
	if len(rep.Rounds[0].Result.Hard) == 0 {
		t.Fatal("用例构造失败：本该有硬校验命中")
	}
	// 交付仍取第 1 轮（唯一一轮）：止损不等于不交付。
	if rep.BestRound != 1 {
		t.Fatalf("止损后仍应记录交付轮次，实际 BestRound=%d", rep.BestRound)
	}
}

// TestJudgeLoopErrorsWithoutManualInsteadOfPassing：没有标尺就绝不编「通过」。
// 裁判的独立性建立在「拿手册原文当尺」上；没有手册时无声放行，
// 等于在最该说不的地方说了是。
func TestJudgeLoopErrorsWithoutManualInsteadOfPassing(t *testing.T) {
	fake := newLoopChat(loopFullMarks())
	g := &Generator{}
	g.SetChatClient(fake)
	var steps []string

	rep := g.judgeLoop(context.Background(), &manualPack{}, "PROMPT_V1", nil, func(s string) { steps = append(steps, s) })

	if rep.Err == "" {
		t.Fatal("无手册结构必须报出原因，不能静默通过")
	}
	if rep.Passed() || len(rep.Rounds) != 0 {
		t.Fatalf("无标尺不该产生任何轮次：rounds=%d passed=%v", len(rep.Rounds), rep.Passed())
	}
	if len(fake.calls) != 0 {
		t.Fatalf("无标尺不该调用模型，实际 %d 次", len(fake.calls))
	}
	if rep.DeliveredPrompt != "" {
		t.Fatalf("无标尺不该交付任何提示词，实际 %q", rep.DeliveredPrompt)
	}
}

// TestJudgeLoopRejectsEmptyReviseOutput：回炉产出空提示词时必须停手并保留当前版本。
// 空提示词不是「改坏了」那么轻——空壳会被当成技能交付，用户拿到的技能里
// 一条分类路由都没有。这类静默损坏必须当场拦下。
func TestJudgeLoopRejectsEmptyReviseOutput(t *testing.T) {
	mp := synthPack()
	fake := newLoopChat(loopHalfMarks())
	g := &Generator{}
	g.SetChatClient(fake)

	rep := g.judgeLoop(context.Background(), mp, "PROMPT_V1",
		func(context.Context, *JudgeResult, string) (string, error) { return "   \n\t ", nil },
		func(string) {})

	if len(rep.Rounds) != 1 {
		t.Fatalf("回炉产出为空应立即停手，实际跑了 %d 轮", len(rep.Rounds))
	}
	if !strings.Contains(rep.Err, "空") {
		t.Fatalf("应报出「回炉产出为空」，实际 Err=%q", rep.Err)
	}
	// 保留当前版本：不能让空壳顶替已经可用的一版。
	if rep.DeliveredPrompt != "PROMPT_V1" || rep.BestRound != 1 {
		t.Fatalf("应保留第 1 轮版本，实际 round=%d prompt=%q", rep.BestRound, rep.DeliveredPrompt)
	}
}

// TestJudgeLoopWithoutReviseChannelStops：没有回炉通道时不许「假装重试」。
// 没有回炉通道却继续循环，只会在同一个提示词上重复花模型调用、并产出一堆同分轮次。
func TestJudgeLoopWithoutReviseChannelStops(t *testing.T) {
	mp := synthPack()
	fake := newLoopChat(loopHalfMarks())
	g := &Generator{}
	g.SetChatClient(fake)

	rep := g.judgeLoop(context.Background(), mp, "PROMPT_V1", nil, func(string) {})

	if len(rep.Rounds) != 1 {
		t.Fatalf("无回炉通道应跑 1 轮就停，实际 %d 轮", len(rep.Rounds))
	}
	if !strings.Contains(rep.EarlyStop, "回炉通道") {
		t.Fatalf("止损文案应点明缺少回炉通道，实际 %q", rep.EarlyStop)
	}
	if len(fake.trials) != 1 {
		t.Fatalf("不该重复试用同一版提示词，实际试用 %d 次", len(fake.trials))
	}
}

// TestJudgeLoopRecordsJudgeFailureWithoutBlocking：裁判/试用调用失败时不阻断流程，
// 但必须写进 Err ——「裁判没跑成」和「裁判判通过」是两件事。
// 这里让试用写稿直接报错（模型不通），断言循环立刻停、Err 有值、没有半截轮次。
func TestJudgeLoopRecordsJudgeFailureWithoutBlocking(t *testing.T) {
	mp := synthPack()
	fake := &fakeChat{reply: func(c fakeCall) (string, error) {
		if c.JSON {
			return "", nil // 裁判返回空串 → 解析失败
		}
		return "草稿", nil
	}}
	g := &Generator{}
	g.SetChatClient(fake)
	var steps []string

	rep := g.judgeLoop(context.Background(), mp, "PROMPT_V1", nil, func(s string) { steps = append(steps, s) })

	if rep.Err == "" {
		t.Fatal("裁判输出解析失败必须写进 Err")
	}
	if len(rep.Rounds) != 0 {
		t.Fatalf("解析失败的轮次不该入库（否则 fidelity 里会出现一行没结论的评分），实际 %d 轮", len(rep.Rounds))
	}
	if rep.Passed() {
		t.Fatal("裁判没跑成不能算通过")
	}
	if !strings.Contains(strings.Join(steps, "\n"), "裁判失败") {
		t.Fatalf("trace 应显性写出裁判失败，实际 %q", strings.Join(steps, "\n"))
	}
}
