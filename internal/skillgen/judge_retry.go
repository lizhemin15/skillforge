package skillgen

import (
	"context"
	"fmt"
	"time"

	"github.com/lizhemin15/skillforge/internal/llm"
)

// 这一文件的由来（线上事故）：训练期 Step 8.5 的裁判第一轮撞上供应商一次瞬时
// 503（「System is too busy now」），judgeLoop 当场 break —— 3 轮预算被一次网络抖动
// 清空，技能照常落盘，fidelity.md 只留下「裁判未跑完」。判分这一层就白跑了。
//
// 设计取舍——为什么重试放在循环外而不是塞进 judgeDraft：
//
//	「重试算不算一轮裁判」这种语义只有循环自己知道。答案是不算：重试是同一轮
//	里的调用失败，不是一次拿到了分数的评审。把它塞进 judgeDraft 会让「轮次」
//	这个概念从循环里漏出去，后面没人说得清 fidelity.md 里的「第 2 轮」到底是
//	第 2 篇稿子还是第 2 次网络请求。
//
// 为什么只重试瞬时故障：401/403/400 这类是配置或请求本身的问题，重试三次
// 只是把「马上告诉用户钥匙不对」拖成「20 秒后才告诉用户钥匙不对」。

// judgeRetryDelays 是瞬时故障的退避间隔，长度即最大重试次数（总尝试 = 长度 + 1）。
// 选 5s/15s：上游过载通常是秒级到十几秒的窗口，再长会拖垮整条训练线（用户坐在
// 那里等），且训练一旦彻底失败要整本手册重来，代价远大于多等 20 秒。
// 测试里会被压成毫秒级——所以它是变量而非常量，但只允许测试改。
var judgeRetryDelays = []time.Duration{5 * time.Second, 15 * time.Second}

// retryTransient 跑 fn，遇到瞬时故障按 judgeRetryDelays 退避重试；其余错误立即返回。
// onRetry 用来向用户交代「正在重试」（不静默等待），可为 nil。
func retryTransient[T any](ctx context.Context, onRetry func(attempt int, wait time.Duration, err error), fn func() (T, error)) (T, error) {
	var zero T
	for attempt := 1; ; attempt++ {
		v, err := fn()
		if err == nil {
			return v, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			// 上层已经放弃（用户点了取消/请求断了），重试没有意义。
			return zero, ctxErr
		}
		if attempt > len(judgeRetryDelays) || !llm.IsTransient(err) {
			// 「预算耗尽」和「不可重试」必须分开交代：前者让用户知道等一会儿再跑就行，
			// 后者让他去查配置。这句话会原样进 fidelity.md 的「⚠️ 裁判未跑完」，
			// 少了它，一次抖动留下的痕迹和一把错钥匙留下的痕迹长得一模一样。
			if attempt > len(judgeRetryDelays) && len(judgeRetryDelays) > 0 {
				var waited time.Duration
				for _, d := range judgeRetryDelays {
					waited += d
				}
				return zero, fmt.Errorf("已按退避重试 %d 次（累计等待 %s）仍失败：%w",
					len(judgeRetryDelays), waited, err)
			}
			return zero, err
		}
		wait := judgeRetryDelays[attempt-1]
		if onRetry != nil {
			onRetry(attempt, wait, err)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return zero, ctx.Err()
		case <-timer.C:
		}
	}
}

// judgeRetryNote 生成「正在重试」的 trace 文案。
// 必须让用户看得见重试：不然界面会静止十几秒，看起来像卡死。
func judgeRetryNote(what string, round int, steps func(string)) func(int, time.Duration, error) {
	if steps == nil {
		return nil
	}
	return func(attempt int, wait time.Duration, err error) {
		steps(fmt.Sprintf("8.5/9 第 %d 轮%s遇到瞬时故障，%s 后重试（第 %d/%d 次）：%s",
			round, what, wait, attempt, len(judgeRetryDelays)+1, clipRunes(err.Error(), 120)))
	}
}

// clipRunes 按字符（不是字节）截断，避免把中文截成半个字——
// 这段文字会进 SSE 帧和 fidelity.md，切出乱码等于往用户看板里塞垃圾。
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
