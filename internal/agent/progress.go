package agent

import (
	"context"
	"time"
)

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

// ---- 重试重置通道 ----
// OnReset 的 ctx 版：StreamChat 在「本轮作废、即将重试」时回调（复读收手 / 断流），
// 调用方据此清掉已经流到前端的正文。不挂就返回 nil，llm 层自己判 nil，零成本。
//
// 为什么走 ctx 不走参数：write-skip / write-plain 的函数签名已经带 onDelta，
// 再加一个 onReset func() 参数，每个调用方（含单测、docgen-test）都要跟着改；
// 而 ctx 链路本来就贯穿全跳，挂一次全链路可见——与 ProgressSink 同一套理由。
type resetKeyT struct{}

func WithReset(ctx context.Context, fn func()) context.Context {
	if ctx == nil || fn == nil {
		return ctx
	}
	return context.WithValue(ctx, resetKeyT{}, fn)
}

func ResetOf(ctx context.Context) func() {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(resetKeyT{}).(func()); ok {
		return v
	}
	return nil
}

// reasoningSink 返回可直接当 llm 回调用的闭包；没挂接收器时返回 nil，
// 让 LLM 层省掉每片一次的函数调用。
//
// 思考链不再原样转发：先过一遍 materialFilter（见 material_filter.go）。
// 线上账本 /tmp/ledger_leg2.log 第 100~152 行记着原样转发的后果——执笔那 50 秒里
// 界面上滚的是 `Word count: ~660. Meets all criteria.`、`Self-Correction/Verification
// during drafting)*:`、`*(Done.)*` 这些英文自我对话，加上反复重发的中文正文，
// 用户看着像程序卡死/乱说。**只改显示**：发往模型的内容（system prompt、user
// message、请求参数）一个字都不动。
//
// 每调用一次生成一个独立的 materialFilter：它是有状态的（最近展示窗口 / 静默计时），
// 跨 LLM 调用复用会把上一跳的「刚说过的话」带进下一跳，把新内容误判成重复。
func reasoningSink(ctx context.Context) func(string) {
	sink := ProgressOf(ctx)
	if sink == nil {
		return nil
	}
	f := newMaterialFilter(time.Now)
	return func(s string) {
		for _, text := range f.Feed(s) {
			sink(text)
		}
	}
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
//
// 现在它就是 reasoningSink，所以自动跟着过 materialFilter。判断标准是「这一片
// 抽得出连续 ≥runMin（6）个汉字的中文段」——构思条目正好卡在门槛上（「· 首段写五要素」
// 是 6 个汉字，`·` 作为中文标点被去掉、不算长度），所以它活得下来。
// 这层过滤加进来时特意守住了这条：任何只看「整片汉字数够不够 8」或「只留最长段」的
// 实现都会把它静默吃掉，又一次退回到上面那个坑。material_filter_test.go 里有两个
// 专门的用例盯着它（NeverEatsChinese / RealLedger）。
func rawSink(ctx context.Context) func(string) {
	return reasoningSink(ctx)
}
