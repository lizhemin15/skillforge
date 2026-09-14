package api

import (
	"context"
	"testing"
	"time"
)

type ctxValKey struct{}

// TestTrainingCtxSurvivesRequestCancel：训练 ctx 必须脱离 HTTP 请求生命周期。
//
// 线上现场：训练是挂在 SSE 上的长任务（生成 → 校验 → 最多 3 轮回炉，十几分钟），
// 用户在浏览器里一关页面或刷新，r.Context() 立刻 cancel，cancel 一路传到裁判层，
// 3 轮回炉预算清零，fidelity.md 留下「回炉失败: context canceled」，
// 然后一份裁判判 10/100「不通过」的技能照常落盘交付。
// 这条断言钉的就是「断开连接 ≠ 放弃训练」。
func TestTrainingCtxSurvivesRequestCancel(t *testing.T) {
	reqCtx, cancelReq := context.WithCancel(context.WithValue(context.Background(), ctxValKey{}, "trace-42"))
	tctx, cancelTrain := trainingCtx(reqCtx)
	defer cancelTrain()

	cancelReq() // 模拟浏览器关页面

	select {
	case <-tctx.Done():
		t.Fatalf("请求 cancel 后训练 ctx 被连带取消（%v）——3 轮回炉预算会凭空消失", tctx.Err())
	case <-time.After(100 * time.Millisecond):
	}

	// 绝对上限必须还在：真卡死的训练不能永远攥着全局训练锁（Admin.mu 是串行的）。
	dl, ok := tctx.Deadline()
	if !ok {
		t.Fatal("训练 ctx 缺少绝对时长上限，卡死的训练会永远占着训练锁")
	}
	if d := time.Until(dl); d > trainMaxDuration || d <= 0 {
		t.Fatalf("训练上限异常：距截止 %v，期望 (0, %v]", d, trainMaxDuration)
	}

	// 解绑的是「取消」，不是「上下文数据」：请求里带的 trace 等 value 要留着，
	// 否则日志里会丢掉「这次训练属于哪个请求」的线索。
	if v, _ := tctx.Value(ctxValKey{}).(string); v != "trace-42" {
		t.Fatalf("训练 ctx 丢了请求内的 value，实际 %q", v)
	}

	// 自己的 cancel 依然要能停掉训练，否则解绑就变成了漏气。
	cancelTrain()
	select {
	case <-tctx.Done():
	default:
		t.Fatal("trainingCtx 返回的 cancel 必须能停掉训练")
	}
}
