package api

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lizhemin15/skillforge/internal/agent"
)

// traceBeat 是「思考中」心跳的间隔。3 秒是权衡后的取值：更密只是刷屏，更疏
// 用户就开始怀疑卡死——人对 <4s 的静默不敏感，对 >5s 就会去点刷新。
const traceBeat = 3 * time.Second

// 中间材料的两个常数。
//
// materialCap：材料只留**尾部** 160 字。思考链是几万字的长文，整段下发既刷爆
// SSE 又没人读；用户要的是「它在动、动的是什么」这一个信号。
//
// materialThrottle：模型按 token 推片段，一片一片往 SSE 里塞会每秒几十帧。
// 400ms 一帧是「看起来在连续滚动」的下限，也是帧数的上界（≈2.5 帧/秒）。
const (
	materialCap      = 160
	materialThrottle = 400 * time.Millisecond
)

// traceClock 把「阻塞等待」变成「看得见的步骤流」。
//
// 为什么需要它：一轮对话里，意图分类和首 token 都是**几十秒的阻塞 LLM 调用**，
// 期间后端一个事件都不发。前端于是只能转圈，用户看到的就是「一直加载状态，
// 最后显示一个最终答案」（实测时间线：56 秒里 55 秒屏幕上空白）。
//
// 修法不是在 LLM 层造假流，而是：
//  1. 请求一进来就下发步骤骨架（① 意图分析 = active）——t≈0 就有东西看；
//  2. 之后每 traceBeat 把当前步骤的 detail 补上「已用 Ns」重发一次——心跳可见，
//     用户既知道它没死，也知道等了多久；
//  3. 阻塞阶段结束再下发完整拆解（①②③④），最后 Finish 收口。
//
// 两个安全性前提：
//   - 前端 renderTrace 是幂等原地刷新（同一步骤重复下发不会堆 DOM），重发安全；
//   - 心跳顺带给代理层保活——Cloudflare / Nginx 对长时间无输出的 SSE 会掐连接。
//
// 它同时是**唯一的 trace 下发口**：并发写 SSE 帧会被切碎，所以所有步骤更新
// 都必须经过它（chat.go / agent_loop.go 里不再直接 write(evTrace, …)）。
type traceClock struct {
	write func(ev, data string)
	beat  time.Duration

	mu    sync.Mutex
	steps []agent.TraceStep
	start time.Time
	// material 是**当前进行中那一步**累积的中间材料尾部；lastMat 是上次因材料
	// 而下发的时间戳（节流用）。
	material string
	lastMat  time.Time

	stopOnce sync.Once
	done     chan struct{}
	wg       sync.WaitGroup
}

// newTraceClock 立刻下发首帧并起心跳。
func newTraceClock(write func(ev, data string), steps []agent.TraceStep) *traceClock {
	return newTraceClockBeat(write, steps, traceBeat)
}

// newTraceClockBeat 是带节拍参数的构造器。存在的唯一理由是**可测性**：
// beat 必须在起 goroutine 之前定下来。曾经测试是在 newTraceClock 之后直接改
// c.beat，于是 loop() 读 c.beat 与测试写 c.beat 构成数据竞争——本地不带 -race
// 全绿，CI 上 `go test -race` 当场红（真踩过）。参数注入比给 beat 加锁干净。
func newTraceClockBeat(write func(ev, data string), steps []agent.TraceStep, beat time.Duration) *traceClock {
	c := &traceClock{
		write: write,
		beat:  beat,
		start: time.Now(),
		done:  make(chan struct{}),
	}
	c.mu.Lock()
	c.steps = cloneSteps(steps)
	c.mu.Unlock()
	c.emit()
	c.wg.Add(1)
	go c.loop()
	return c
}

// Set 用新的步骤组覆盖当前内容（阻塞阶段结束后下发完整拆解时用）。
func (c *traceClock) Set(steps []agent.TraceStep) {
	c.mu.Lock()
	c.steps = cloneSteps(steps)
	// 换阶段了：上一阶段攒的材料属于上一件事，留着会挂在新步骤下面误导人。
	c.material = ""
	c.mu.Unlock()
	c.emit()
}

// Thinking 收模型侧流出来的**中间材料**（思考链 / 分析片段），挂到进行中的那一步上。
//
// 它解决的是「计时器在跳，但屏幕上没有任何内容」：关掉思考链的那几跳本来就不产
// 材料（也不需要，4~5 秒就回来了），保留思考链的执笔/审稿/改稿几跳才是几十秒的
// 长活——那些跳的材料在这里滚动显示，用户看到的是「它在想什么」，不是「它花了多久」。
//
// 两个安全阀：只留尾部 materialCap 字（思考链是长文，整段下发既刷爆 SSE 又没人看），
// 只按 materialThrottle 节流下发（token 级推流每秒几十片）。没有进行中的步骤时
// 直接丢弃——等待用户补充的阶段不该继续滚材料。
func (c *traceClock) Thinking(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	c.mu.Lock()
	if activePos(c.steps) < 0 {
		c.mu.Unlock()
		return
	}
	c.material = tailRunes(c.material+text, materialCap)
	now := time.Now()
	if !c.lastMat.IsZero() && now.Sub(c.lastMat) < materialThrottle {
		c.mu.Unlock()
		return
	}
	c.lastMat = now
	c.mu.Unlock()
	c.emit()
}

// tailRunes 取**尾部** n 个字符。按字符切而不是按字节：按字节切会把一个汉字
// 劈成两半，前端显示成乱码。
func tailRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// Awaiting 把步骤组推进到「等用户补充」：进行中那一步转 waiting，其余转 done。
// 没有 active 步骤之后心跳自然安静下来（等待态不该继续跳秒）。
func (c *traceClock) Awaiting(detail string) {
	c.mu.Lock()
	for i := range c.steps {
		if c.steps[i].Status == "active" {
			c.steps[i].Status = "waiting"
			c.steps[i].Detail = detail
		} else {
			c.steps[i].Status = "done"
		}
	}
	c.material = ""
	c.mu.Unlock()
	c.emit()
}

// Finish 把所有步骤标成 done、下发终帧并停掉心跳。
func (c *traceClock) Finish() {
	c.mu.Lock()
	for i := range c.steps {
		c.steps[i].Status = "done"
	}
	// 收口这帧是给用户看「都做完了」的，挂着最后一段思考材料反而像还没完。
	c.material = ""
	c.mu.Unlock()
	c.emit()
	c.stop()
}

// Freeze 停掉心跳但不下发任何东西。给 handler 兜底用：任何分支（含 error 早退）
// 都必须让心跳停下，否则连接关了还在写。
func (c *traceClock) Freeze() { c.stop() }

// Elapsed 返回本轮已用秒数。
func (c *traceClock) Elapsed() int { return int(time.Since(c.start).Seconds()) }

// Steps 返回当前步骤组的副本（调用方想基于现状改造时用）。
func (c *traceClock) Steps() []agent.TraceStep {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneSteps(c.steps)
}

func (c *traceClock) loop() {
	defer c.wg.Done()
	t := time.NewTicker(c.beat)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			c.beatOnce()
		}
	}
}

func (c *traceClock) stop() {
	c.stopOnce.Do(func() {
		close(c.done)
		c.wg.Wait() // 等心跳 goroutine 退净，避免终帧之后又冒出一帧
	})
}

// emit 原样下发一帧（若有 active 步骤则带上「已用 Ns」）。
func (c *traceClock) emit() {
	st := c.snapshot(true)
	if len(st) == 0 {
		return
	}
	c.write(evTrace, jsonSafe(st))
}

// beatOnce 是心跳：没有进行中的步骤就什么都不发（等待补充 / 已完成的帧不该再跳秒）。
func (c *traceClock) beatOnce() {
	if activePos(c.snapshot(false)) < 0 {
		return
	}
	c.emit()
}

func (c *traceClock) snapshot(decorate bool) []agent.TraceStep {
	c.mu.Lock()
	out := cloneSteps(c.steps)
	mat := c.material
	c.mu.Unlock()
	if !decorate || len(out) == 0 {
		return out
	}
	if i := activePos(out); i >= 0 {
		out[i].Detail = withElapsed(out[i].Detail, c.Elapsed())
		// 材料只挂进行中的那一步：它是「这一步正在干什么」的证据。
		out[i].Material = mat
	}
	return out
}

// cloneSteps 深拷贝，杜绝把带秒数的副本写回 c.steps（那会叠成「已用 3s（已用 6s）」）。
func cloneSteps(in []agent.TraceStep) []agent.TraceStep {
	out := make([]agent.TraceStep, len(in))
	copy(out, in)
	return out
}

// activePos 只认 status=active 的那一步（waiting / done 都算「没在跑」）。
func activePos(st []agent.TraceStep) int {
	for i := len(st) - 1; i >= 0; i-- {
		if st[i].Status == "active" {
			return i
		}
	}
	return -1
}

const (
	elapsedOpen  = "（已用 "
	elapsedClose = "s）"
)

// withElapsed 在文案尾部挂「（已用 Ns）」。已有秒数会先剥掉，保证反复心跳不叠字。
func withElapsed(detail string, sec int) string {
	base := detail
	if i := strings.LastIndex(base, elapsedOpen); i >= 0 {
		base = strings.TrimRight(base[:i], " ")
	}
	return fmt.Sprintf("%s%s%d%s", base, elapsedOpen, sec, elapsedClose)
}

// normalizeSteps 兜一层状态：模型给全 done（或状态缺失）时，把最后一步标成
// active，否则心跳没有落点、前端也没得显示「进行中」。
func normalizeSteps(steps []agent.TraceStep) []agent.TraceStep {
	out := cloneSteps(steps)
	if len(out) == 0 {
		return out
	}
	for i := range out {
		if out[i].Status == "" {
			out[i].Status = "done"
		}
	}
	if activePos(out) < 0 {
		out[len(out)-1].Status = "active"
	}
	return out
}

// fallbackSteps 在分类器没给 steps 时兜出一套骨架。
// 必须有：分类器偶尔会漏 steps，而「没有步骤」就等于回到用户抱怨的「一直转圈」，
// 所以宁可给通用骨架也不要空面板。phase 取值必须与前端 AGENTS 映射表一致
// （analyze/match/params/generate），否则角色标签会空。
func fallbackSteps(ev agent.Eval) []agent.TraceStep {
	switch strings.TrimSpace(ev.Intent) {
	case "chat":
		return []agent.TraceStep{
			{Phase: "analyze", Label: "① 意图分析", Detail: "识别为闲聊问答", Status: "done"},
			{Phase: "match", Label: "② 能力匹配", Detail: "无需技能，直接回答", Status: "done"},
			{Phase: "generate", Label: "④ 组织回答", Detail: "正在组织回答…", Status: "active"},
		}
	case "query":
		return []agent.TraceStep{
			{Phase: "analyze", Label: "① 意图分析", Detail: "识别为办事/流程查询", Status: "done"},
			{Phase: "match", Label: "② 流程检索", Detail: "匹配办事流程规则", Status: "done"},
			{Phase: "params", Label: "③ 要素提炼", Detail: "提取办理所需信息", Status: "done"},
			{Phase: "generate", Label: "④ 给出步骤", Detail: "正在整理办理步骤…", Status: "active"},
		}
	case "docgen":
		return []agent.TraceStep{
			{Phase: "analyze", Label: "① 意图分析", Detail: "识别为办公文档任务", Status: "done"},
			{Phase: "match", Label: "② 工具匹配", Detail: "选择模板填充 / 文档生成", Status: "done"},
			{Phase: "params", Label: "③ 参数提取", Detail: "抽取文档要素", Status: "done"},
			{Phase: "generate", Label: "④ 执行中", Detail: "正在生成文档…", Status: "active"},
		}
	default:
		return []agent.TraceStep{
			{Phase: "analyze", Label: "① 意图分析", Detail: "识别为写作任务", Status: "done"},
			{Phase: "match", Label: "② 技能检索", Detail: "匹配写作技能", Status: "done"},
			{Phase: "params", Label: "③ 要素提炼", Detail: "抽取写作要素", Status: "done"},
			{Phase: "generate", Label: "④ 内容执笔", Detail: "正在撰写内容…", Status: "active"},
		}
	}
}
