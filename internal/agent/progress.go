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

// contentSink 返回可直接当 llm 的 OnContent 用的闭包：把流式 JSON 正文里
// **人话文字**抽出来当中间材料。
//
// 与 reasoningSink 的分工是踩过线上才划清的：reasoningSink 收模型的思考链，
// 而 astron 上关思考链的开关是真管用的（实测 reasoning 片数 = 0），所以那些
// 「关了思考链」的跳**一片材料都不可能产出**——材料挂接没错，是根本没料。
// 同一跳的 content 却在按片段流（实测 440 片 / 2350 字节、首片 382ms），
// 而这些 JSON 里装的就是用户最终要的那份文档的文字。docgen 那一跳实测裸跑
// 16.8 秒（整轮 23.8s），这 16.8 秒里屏幕上只有计时在跳——contentSink 就是
// 为这 16.8 秒存在的。
//
// 每调用一次生成一个独立的 jsonPreview：它是有状态的（容器栈 / 当前字段），
// 跨 LLM 调用复用会把上一份 JSON 的字段归属带进下一份。
func contentSink(ctx context.Context) func(string) {
	sink := ProgressOf(ctx)
	if sink == nil {
		return nil
	}
	p := &jsonPreview{}
	return func(s string) {
		if text := p.Feed(s); text != "" {
			sink(text)
		}
	}
}

// rawSink 返回可直接当 llm 的 OnContent 用的闭包：把模型吐的人话**原样**当材料转发。
//
// 与 contentSink 的分工按「这一跳吐的是不是 JSON」划：contentSink 只从 JSON 字符串里
// 抽字（jsonPreview 遇到未加引号的裸文本一个字都不吐），所以**本来就是人话**的跳挂它
// 等于挂了个黑洞。构思跳（PlanEssay）就是这种：它吐的是「· 首段写五要素」这类纯文本
// 条目，挂了 contentSink 的结果是材料恒为空——设计意图（关掉思考链后，构思就是这一跳
// 全部的可见产出）被静默吃掉，而外面看只像是「模型没吐东西」。
func rawSink(ctx context.Context) func(string) {
	return reasoningSink(ctx)
}
