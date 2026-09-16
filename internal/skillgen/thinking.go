package skillgen

import "context"

// 本文件是训练链路的「关思考链」开关。
//
// 事故背景（用户投诉原话）：「现在速度过于慢了，中间可以流式输出思考的一些中间材料，
// 现在一直卡着计时，用户体验不佳。」流式材料接上之后计时不再空转，但**慢本身**还在，
// 只是从「看不见的黑箱」变成了「看得见的长篇推演」。
//
// 线上实测一轮训练 860.5s，按阶段拆开：
//
//	1/9 提取写作特征      94.8s   思考链 5026 字 / 正文 286 字
//	2/9 技能元数据        77.9s   思考链 4207 字 / 正文 44 字
//	3/9 识别技能类型      65.1s   思考链 8678 字 / 正文 1190 字
//	4/9 撰写系统提示词   117.6s   （保留思考，见下）
//	5/9 检测是否写作手册 430.4s   思考链 27287 字 / 正文 1868 字
//	6/9 骨架模板          74.7s   思考链 6314 字 / 正文 288 字
//	7/9~9/9（纯本地校验落盘） 0.1s
//
// 整轮吐出的字符里 **94% 是思考链**（约 5.6 万字思考链 / 3.7 千字正文）。最刺眼的是
// 5/9：430 秒、2.7 万字推演，结论是「只抽出 1 个分类 → 不是手册，降级通用流程」——
// 花了七分钟证明「这份素材不是写作手册」，而这一步的产出只是一段 JSON 摘录。
//
// 为什么这些阶段不需要思考链：产出是**结构化摘录**（特征 JSON、类型判定、表单字段、
// 分类结构、骨架模板）或**机械改写**（只删掉违规句子、其余逐字保留）。这类任务上
// 思考链不改变结果，只是把 4 分钟拉成 14 分钟。agent 链路（对话 / EvalTurn / 写作）
// 早就统一关掉了（见 internal/agent/agent.go、writing.go 的 StreamOpts），
// 训练链路是漏掉的那一条。
//
// 谁**不能**关：4/9 撰写系统提示词、返修重写、优化指令收紧、范文兜底、裁判试写 ——
// 这几处产出的是要给人读的成品文字，思考链直接影响质量。所以开关按调用点给，
// 不给整条链路一刀切：一刀切是「用所有技能的质量换速度」，那不叫提速，叫降级。
//
// 与 deltaKey 同理挂在 ctx 上而不是 Generator 字段：Generator 是常驻单例，
// 多个训练可能并发跑，在结构体上挂开关就是数据竞争。
type thinkingOffKey struct{}

// withoutThinking 标记「这次调用（及其下游）不需要思考链」。
//
// 用法：把 ctx 包一层再传进去，例如
//
//	st, err := ExtractStructure(withoutThinking(ctx), g.streamingChat(), src)
//
// 只影响流式那条链路（StreamChat 会带上 provider 开关）；没挂接收器的阻塞调用
// 走 go-openai，本来就带不了这类开关，行为与历史一字不变。
func withoutThinking(ctx context.Context) context.Context {
	return context.WithValue(ctx, thinkingOffKey{}, true)
}

// thinkingOff 读取标记；没标记过返回 false（默认保留思考链）。
func thinkingOff(ctx context.Context) bool {
	v, _ := ctx.Value(thinkingOffKey{}).(bool)
	return v
}
