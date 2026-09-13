package skillgen

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

// 线上事故的直接防线：Step 8.5 裁判第一轮撞上供应商瞬时 503（「System is too busy now」），
// judgeLoop 当场 break —— 3 轮预算被一次网络抖动清空，技能照常落盘，fidelity.md 上
// 只留「裁判未跑完」。用户拿到的技能没被验收过，而这事完全可以靠等一下再试解决。
//
// 这一组用例盯的是重试的**边界**，不是「有没有重试」那句注释：
//   - 抖动之后能自己好起来（轮次预算不能被重试吃掉）；
//   - 抖动一直不好也要有上限（不能变成无限挂着）；
//   - 配置错了不许重试（别把「钥匙不对」拖成「20 秒后告诉你钥匙不对」）；
//   - 用户取消立刻停（别拖住取消）。

// stubJudgeRetry 把退避压到毫秒级——否则每个用例白等 20 秒。
//
// ⚠️ 只压**时长**、保留**长度**。长度（重试几次）是产品决策，也被断言依赖：
// 早期版本用固定 2 个毫秒值整体覆盖这个变量，结果「退避表被清空成 0 次」
// 这种产品级缺陷在本文件里永远测不出来——期望调用次数是从同一个变量推出来的，
// 注入清空后所有断言照旧成立（骑自己桩的空集，恒真绿）。靠注入实验才发现。
func stubJudgeRetry(t *testing.T) []time.Duration {
	t.Helper()
	old := judgeRetryDelays
	stub := make([]time.Duration, len(old))
	for i := range stub {
		stub[i] = time.Millisecond
	}
	judgeRetryDelays = stub
	t.Cleanup(func() { judgeRetryDelays = old })
	return stub
}

// TestJudgeRetryBudgetIsSane 不套桩，直接读产品真值：退避表本身是产品决策。
// 它决定「一次供应商抖动会不会报废整条训练线」（下限）与「用户要盯界面等多久」（上限），
// 所以两侧都要有约束——空表等于没修，一分钟以上的表等于把训练挂死。
func TestJudgeRetryBudgetIsSane(t *testing.T) {
	if len(judgeRetryDelays) < 2 {
		t.Fatalf("至少要能扛住两次连续抖动（线上那次 503 就发生在第一轮），实际退避表 %v", judgeRetryDelays)
	}
	total := time.Duration(0)
	prev := time.Duration(0)
	for i, d := range judgeRetryDelays {
		if d <= 0 {
			t.Fatalf("第 %d 次退避=%v：不退避就是原地连打上游", i+1, d)
		}
		if d < prev {
			t.Fatalf("退避应非递减（第 %d 次 %v < 上一次 %v），否则重试比首次更急", i+1, d, prev)
		}
		if d > 30*time.Second {
			t.Fatalf("第 %d 次退避=%v 太长，用户会当成卡死", i+1, d)
		}
		prev, total = d, total+d
	}
	if total > time.Minute {
		t.Fatalf("退避总时长 %v 超过 1 分钟——训练是交互式的，等不起", total)
	}
}

// liveJudgeErr503 复刻线上原文（外层是 judgeDraft 的 %w 包装，内层是 go-openai 的 RequestError）。
func liveJudgeErr503() error {
	return fmt.Errorf("judgeDraft: 裁判调用失败: %w", &openai.RequestError{
		HTTPStatusCode: 503,
		HTTPStatus:     "503 Service Unavailable",
		Err:            errors.New("openai: no choices"),
		Body:           []byte(`{"code":50508,"message":"System is too busy now. Please try again later.","data":null}`),
	})
}

// flakyJudgeChat：试用写稿永远成功，裁判（JSON 模式）按 judgeErrs 序列失败若干次后转正常。
// judgeErrs 用完之后一律返回满分 JSON —— 序列长度就是「抖动持续几次」。
func flakyJudgeChat(judgeErrs []error) (*fakeChat, *int, *int) {
	judgeN, trialN := new(int), new(int)
	return &fakeChat{reply: func(c fakeCall) (string, error) {
		if !c.JSON {
			*trialN++
			return fmt.Sprintf("第 %d 轮草稿正文。", *trialN), nil
		}
		i := *judgeN
		*judgeN++
		if i < len(judgeErrs) {
			return "", judgeErrs[i]
		}
		return judgeJSON(loopFullMarks()), nil
	}}, judgeN, trialN
}

// TestJudgeLoopRetriesTransientJudgeFailure：抖动两下之后成功，裁判照样出结论。
// 关键断言不只是「最终通过」——而是「重试没有吃掉轮次预算、也没有重跑试用」：
// 重试是同一轮里的重发，不是一次新的评审。
func TestJudgeLoopRetriesTransientJudgeFailure(t *testing.T) {
	stubJudgeRetry(t)
	mp := synthPack()
	fake, judgeN, trialN := flakyJudgeChat([]error{liveJudgeErr503(), liveJudgeErr503()})
	g := &Generator{}
	g.SetChatClient(fake)
	var steps []string

	rep := g.judgeLoop(context.Background(), mp, "PROMPT_V1", nil, func(s string) { steps = append(steps, s) })

	if rep.Err != "" {
		t.Fatalf("抖动两下之后应当自己好起来，不该报错：%s", rep.Err)
	}
	if !rep.Passed() || len(rep.Rounds) != 1 {
		t.Fatalf("应拿到 1 轮通过的结论，实际 rounds=%d passed=%v", len(rep.Rounds), rep.Passed())
	}
	// 重试不吃轮次：结论必须仍然标在第 1 轮。
	if rep.Rounds[0].Round != 1 || rep.BestRound != 1 {
		t.Fatalf("重试不该消耗裁判轮次：round=%d best=%d", rep.Rounds[0].Round, rep.BestRound)
	}
	// 裁判调用 3 次（1 次失败 + 2 次重试），试用只有 1 次（重试不重跑写稿）。
	if *judgeN != 3 {
		t.Fatalf("裁判应被调用 3 次（首次 + 两次重试），实际 %d 次", *judgeN)
	}
	if *trialN != 1 {
		t.Fatalf("试用写稿不该被重跑，实际 %d 次", *trialN)
	}
	line := strings.Join(steps, "\n")
	if !strings.Contains(line, "遇到瞬时故障") || !strings.Contains(line, "重试") {
		t.Fatalf("重试必须写进 trace（否则界面静止十几秒像卡死）：%q", line)
	}
	if !strings.Contains(line, "503") {
		t.Fatalf("重试文案应带上原始错误便于定位：%q", line)
	}
	// 正常结论帧不能被重试文案顶掉。
	if !strings.Contains(line, "裁判第 1 轮") {
		t.Fatalf("trace 缺裁判结论帧：%q", line)
	}
}

// TestJudgeLoopGivesUpAfterRetryBudget：抖动一直不好，重试到预算为止（1 + len(delays) 次）。
// 上限是必须的：没有上限就是一个挂在 503 上的训练任务，用户看不到头。
func TestJudgeLoopGivesUpAfterRetryBudget(t *testing.T) {
	stubJudgeRetry(t)
	mp := synthPack()
	fake, judgeN, _ := flakyJudgeChat([]error{liveJudgeErr503(), liveJudgeErr503(), liveJudgeErr503(), liveJudgeErr503()})
	g := &Generator{}
	g.SetChatClient(fake)
	var steps []string

	rep := g.judgeLoop(context.Background(), mp, "PROMPT_V1", nil, func(s string) { steps = append(steps, s) })

	wantCalls := len(judgeRetryDelays) + 1
	if *judgeN != wantCalls {
		t.Fatalf("重试耗尽应为 %d 次调用，实际 %d 次（多试=无限挂着，少试=白白放弃）", wantCalls, *judgeN)
	}
	if rep.Err == "" {
		t.Fatal("抖动一直不好必须写进 Err ——「裁判没跑成」和「裁判判通过」是两件事")
	}
	if !strings.Contains(rep.Err, "503") {
		t.Fatalf("Err 应保留原始故障信息，实际 %q", rep.Err)
	}
	// 必须交代「已经重试过」：这句话进 fidelity.md 的「⚠️ 裁判未跑完」，用户据此决定
	// 「等 5 分钟再跑」还是「去查上游配置」。少了它，抖动耗尽预算和一把错钥匙长得一样。
	// 这里写死 2（= 源码里退避表的真实长度），不写 len(judgeRetryDelays)：
	// 从同一个变量推期望值，表被清空也照样绿——骑自己桩的空集。
	if !strings.Contains(rep.Err, "已按退避重试 2 次") {
		t.Fatalf("Err 应交代已重试次数，实际 %q", rep.Err)
	}
	if !strings.Contains(rep.Err, "累计等待") {
		t.Fatalf("Err 应交代累计等待时长（让用户判断「等一会儿再来」是否划算），实际 %q", rep.Err)
	}
	if len(rep.Rounds) != 0 || rep.Passed() {
		t.Fatalf("没拿到分数就不该有轮次：rounds=%d passed=%v", len(rep.Rounds), rep.Passed())
	}
	if !strings.Contains(strings.Join(steps, "\n"), "裁判失败") {
		t.Fatalf("trace 应显性写出裁判失败：%q", strings.Join(steps, "\n"))
	}
}

// TestJudgeLoopDoesNotRetryNonTransientError：钥匙不对/请求非法这类错误立刻返回。
// 重试它们等于把「马上告诉你钥匙不对」拖成「20 秒后告诉你钥匙不对」。
func TestJudgeLoopDoesNotRetryNonTransientError(t *testing.T) {
	stubJudgeRetry(t)
	mp := synthPack()
	authErr := fmt.Errorf("judgeDraft: 裁判调用失败: %w", &openai.RequestError{
		HTTPStatusCode: 401,
		HTTPStatus:     "401 Unauthorized",
		Body:           []byte(`{"message":"invalid api key"}`),
	})
	fake, judgeN, _ := flakyJudgeChat([]error{authErr, authErr, authErr, authErr})
	g := &Generator{}
	g.SetChatClient(fake)
	var steps []string

	rep := g.judgeLoop(context.Background(), mp, "PROMPT_V1", nil, func(s string) { steps = append(steps, s) })

	if *judgeN != 1 {
		t.Fatalf("鉴权类错误不该重试，实际调用 %d 次", *judgeN)
	}
	if !strings.Contains(rep.Err, "401") {
		t.Fatalf("Err 应保留 401 原文，实际 %q", rep.Err)
	}
	if strings.Contains(strings.Join(steps, "\n"), "遇到瞬时故障") {
		t.Fatal("不该给用户看「正在重试」——这次重试根本不会发生")
	}
	// 也不许写成「已重试 N 次仍失败」：401 一次都没重试过，说重试过等于把用户往
	// 「上游在抖，等一下就好」的错方向引——他会白等 20 秒再撞同一面墙。
	if strings.Contains(rep.Err, "已按退避重试") {
		t.Fatalf("鉴权错误不许声称重试过，实际 %q", rep.Err)
	}
}

// TestJudgeLoopTransientThenNonTransientStops：先抖动（重试）后鉴权错（立刻停）。
// 这条防的是「一旦开始重试就把所有错误都当瞬时」的偷懒写法：那会把 401 也拖满退避。
func TestJudgeLoopTransientThenNonTransientStops(t *testing.T) {
	stubJudgeRetry(t)
	mp := synthPack()
	authErr := fmt.Errorf("judgeDraft: 裁判调用失败: %w", &openai.RequestError{HTTPStatusCode: 403})
	fake, judgeN, _ := flakyJudgeChat([]error{liveJudgeErr503(), authErr})
	g := &Generator{}
	g.SetChatClient(fake)

	rep := g.judgeLoop(context.Background(), mp, "PROMPT_V1", nil, func(string) {})

	if *judgeN != 2 {
		t.Fatalf("应为「1 次失败 + 1 次重试 → 403 立刻停」，实际 %d 次", *judgeN)
	}
	if !strings.Contains(rep.Err, "403") {
		t.Fatalf("最终错误应是 403，实际 %q", rep.Err)
	}
}

// TestJudgeLoopCancelStopsRetrying：用户取消后立刻停，不把退避走完。
// 判据是「耗时」+「调用次数」双条件：只看调用次数会被「重试了但很快返回」骗过。
func TestJudgeLoopCancelStopsRetrying(t *testing.T) {
	// 这里故意用长退避：如果实现忽略了 ctx，这个用例会真的等 5 秒。
	old := judgeRetryDelays
	judgeRetryDelays = []time.Duration{5 * time.Second, 15 * time.Second}
	t.Cleanup(func() { judgeRetryDelays = old })

	mp := synthPack()
	fake, judgeN, _ := flakyJudgeChat([]error{liveJudgeErr503(), liveJudgeErr503(), liveJudgeErr503()})
	g := &Generator{}
	g.SetChatClient(fake)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 进循环前就已取消

	start := time.Now()
	rep := g.judgeLoop(ctx, mp, "PROMPT_V1", nil, func(string) {})
	elapsed := time.Since(start)

	if *judgeN != 1 {
		t.Fatalf("已取消的上下文不该继续重试，实际调用 %d 次", *judgeN)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("取消后仍在退避等待（耗时 %s）——重试会拖住用户的取消操作", elapsed)
	}
	if !strings.Contains(rep.Err, "context canceled") {
		t.Fatalf("应把取消作为终止原因，实际 Err=%q", rep.Err)
	}
}
