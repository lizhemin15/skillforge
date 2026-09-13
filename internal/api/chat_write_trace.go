package api

import (
	"fmt"
	"strings"

	"github.com/lizhemin15/skillforge/internal/agent"
)

// ===== 手册模式写作的步骤流（append-only）=====
//
// 为什么要有这台「排版机」：写作是**串行推进**的过程，一开始谁也不知道审稿会跑几轮。
// 早先的实现是一个固定四格的骨架（① 意图分析 / ② 手册分类 / ③ 要素提炼 /
// ④ 按类执笔与审稿），每推进一步就整组覆盖一次，把「命中哪一类」和「审稿跑到第几轮」
// 都塞进第 ④ 格的 detail 里。后果是**后一步把前一步的结论冲掉**：跑完之后留在屏幕上
// 的最后一句话是「审稿第 1 轮：按手册检查项逐条核对…」，恰好是过程状态而不是结论——
// 用户既看不到它按哪一类的要求写的，也看不到审稿到底过没过（通过时那句「通过」写的
// 是 stderr，屏幕上一个字都没有）。
//
// 所以改成 append-only：一步做完就落一格并定格，后面的步骤只追加、不回改。这样
// 「命中《会议纪要》」和「审稿通过」都会留在时间线上。
//
// 一条硬约束：格子里的 phase 只能取 analyze / match / params / generate 四个值
// （前端拿 phase 去查角色标签表，取别的值角色那一栏就是空的——踩过）。所以审稿、
// 修订这些后续步骤统一挂在 generate 上，真实语义由 label 和 detail 说清楚：
// 它们确实都是「内容执笔」这一跳在做的事。这样序号表也不需要改，用户看到的还是
// 那套四个子智能体。
type traceBoard struct {
	steps []agent.TraceStep
}

func newTraceBoard() *traceBoard { return &traceBoard{} }

// Carry 把 t≈0 已经下发出去的首格接过来当第一格。
//
// 只接第一格，不接整组：手动模式下骨架首格写的是「① 指定技能 / 已选定《X》，跳过
// 意图分析」，这里若改写成「① 意图分析」，就和界面上「跳过意图分析」的文案自相矛盾；
// 而手动骨架的后两格（② 载入能力 / ③ 执行）说的是单段流程，在手册模式里会被
// 「② 手册分类 / ③ 对齐要求与范文」取代，接过来只会让同一件事出现两次。
func (b *traceBoard) Carry(seed []agent.TraceStep) {
	if len(seed) == 0 {
		return
	}
	first := seed[0]
	first.Status = "active" // 承载「正在跑」的状态；跑完由 Close 定格
	b.steps = append(b.steps, first)
}

// Active 追加一格「进行中」，返回它的下标，供稍后 Close 定格。
func (b *traceBoard) Active(phase, label, detail string) int {
	b.steps = append(b.steps, agent.TraceStep{Phase: phase, Label: label, Detail: detail, Status: "active"})
	return len(b.steps) - 1
}

// Done 追加一格已完成的步骤（不需要经过「进行中」的瞬态就用它）。
func (b *traceBoard) Done(phase, label, detail string) {
	i := b.Active(phase, label, detail)
	b.Close(i, "")
}

// Close 把第 i 格定格成 done，并用结论替换 detail。
//
// 越界与重复 Close 都是静默无害的：步骤流被各种分支（审稿失败、改稿失败、判不出类别）
// 切得很碎，任何一个分支忘了 Close 都只是留一格 active，不值得为此 panic。
func (b *traceBoard) Close(i int, detail string) {
	if i < 0 || i >= len(b.steps) {
		return
	}
	b.steps[i].Status = "done"
	if s := strings.TrimSpace(detail); s != "" {
		b.steps[i].Detail = s
	}
}

// Steps 返回可下发的副本（traceClock.Set 会自己再拷一份，这里拷是为了防调用方误改）。
func (b *traceBoard) Steps() []agent.TraceStep {
	out := make([]agent.TraceStep, len(b.steps))
	copy(out, b.steps)
	return out
}

// Len 返回当前格数（测试与日志用）。
func (b *traceBoard) Len() int { return len(b.steps) }

// alignDetail 描述「这一步到底把哪一类的什么素材喂给了模型」。
//
// 用户要看得见的是「它按哪一类的要求写的、参考了哪几篇范文」——这既是最容易出错的一环
// （路由错类 = 整篇按错的要求写），也是事后唯一能自证的一句。所以范文要**列出文件名**，
// 不能只说「N 篇」：出问题时用户得能顺着文件名去手册里核对那篇是不是对应的类别。
//
// 不列范文正文：那是给模型看的，刷到对话里既污染屏幕也可能让用户误以为稿子是抄的。
func alignDetail(cat *agent.WriteCategory) string {
	if cat == nil {
		return "未载入分类素材"
	}
	name := cat.Name
	if strings.TrimSpace(name) == "" {
		name = cat.File
	}
	var b strings.Builder
	fmt.Fprintf(&b, "注入《%s》写作要求", name)
	if len(cat.Examples) == 0 {
		b.WriteString("；本类在手册里没有范文，只按要求起草")
		return b.String()
	}
	names := make([]string, 0, len(cat.Examples))
	for _, ex := range cat.Examples {
		names = append(names, exampleName(ex.Path))
	}
	fmt.Fprintf(&b, " + %d 篇真实范文（%s）", len(cat.Examples), strings.Join(names, "、"))
	return b.String()
}

// exampleName 把 examples/新闻通稿/01.md 取成 01.md，只保留最后一段。
// 全路径会把 trace 撑成一整行而且重复了类别目录名（类别名已经在同一句里了）。
func exampleName(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" {
		return "未命名范文"
	}
	if i := strings.LastIndex(p, "/"); i >= 0 && i+1 < len(p) {
		return p[i+1:]
	}
	return p
}

// runeLen 返回字符数。用字节数会把中文稿件的字数报成实际的 3 倍。
func runeLen(s string) int { return len([]rune(s)) }
