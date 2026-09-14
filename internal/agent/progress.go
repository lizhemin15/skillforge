package agent

import "context"

// ProgressSink 收「中间材料」——模型侧正在流出来的思考链 / 分析片段。
//
// 为什么是 ctx 而不是 Engine 的字段：同一条 ctx 贯穿分类、路由、抽取、执笔四跳，
// 挂一次就全链路可见；挂在 Engine 上则要处理并发会话互相串台（一个进程同时服务
// 多个 SSE 连接，Engine 是共享的单例）。
type ProgressSink func(text string)

type progressKeyT struct{}

// WithProgress 把材料接收器挂到 ctx 上。sink 为 nil 时原样返回（等价于没挂）。
func WithProgress(ctx context.Context, sink ProgressSink) context.Context {
	if ctx == nil || sink == nil {
		return ctx
	}
	return context.WithValue(ctx, progressKeyT{}, sink)
}

// ProgressOf 取出材料接收器；没挂就返回 nil。
func ProgressOf(ctx context.Context) ProgressSink {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(progressKeyT{}).(ProgressSink); ok {
		return v
	}
	return nil
}

// ReportProgress 安全上报一段材料。没挂接收器时什么都不做——**不能**为了凑一个
// 回调就在每条调用路径上写 if：巡检脚本、批处理、单测都跑在同一批函数上，
// 它们不关心过程展示，而漏判一处就是一个 nil 调用崩溃。
func ReportProgress(ctx context.Context, text string) {
	if text == "" {
		return
	}
	if sink := ProgressOf(ctx); sink != nil {
		sink(text)
	}
}

// reasoningSink 返回可直接当 llm 回调用的闭包；没挂接收器时返回 nil，
// 让 LLM 层省掉每片一次的函数调用。
func reasoningSink(ctx context.Context) func(string) {
	sink := ProgressOf(ctx)
	if sink == nil {
		return nil
	}
	return func(s string) { sink(s) }
}
