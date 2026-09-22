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

// 中间材料的三个常数。
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

// materialLogCap：材料**滚动日志**（MaterialLog）的窗口，1200 字。
//
// 为什么单开一个比 materialCap 大得多的窗口：160 字的尾巴在屏幕上是一行原地
// 替换的文本，用户实际看到的是「一行字在抖 + 计时在涨」——比只有计时更糟，
// 因为抖动让人以为程序坏了。实测的 25 帧全是同一个尾巴，每帧开头还被从词中间
// 劈开（真实 dump：「兰察布风电场的…」「确的时间节点…」）。
//
// 1200 字 ≈ 3~5 句连续思考，够前端铺成一个多行滚动区，读起来是「整段在长」，
// 而不是「一行在闪」。上限仍然要有：思考链是几万字，无上限会让每帧 SSE 膨胀到
// 几十 KB。
const materialLogCap = 1200

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
	// narrateEvery 是本地旁白的换行间隔（构造期定型，理由见 newTraceClockTuned）。
	narrateEvery time.Duration

	mu    sync.Mutex
	steps []agent.TraceStep
	start time.Time
	// material 是**当前进行中那一步**累积的中间材料尾部；lastMat 是上次因材料
	// 而下发的时间戳（节流用）。
	material string
	// materialLog 是同一份材料的**滚动日志**（尾部 materialLogCap 字）。与 material
	// 同源同节流，只是窗口大得多、且对齐到句首——前端拿它铺多行滚动区，材质就从
	// 「一行在原地抖」变成「整段在长」。
	materialLog string
	lastMat     time.Time

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
	return newTraceClockTuned(write, steps, beat, narrateGap)
}

// newTraceClockTuned 让**心跳节奏与旁白节奏都可注入**。旁白节奏必须能在构造期定下来，
// 理由同 beat：goroutine 起来之后再改字段就是数据竞争（CI 的 -race 会当场红）。
func newTraceClockTuned(write func(ev, data string), steps []agent.TraceStep, beat, narrateEvery time.Duration) *traceClock {
	c := &traceClock{
		write:        write,
		beat:         beat,
		narrateEvery: narrateEvery,
		start:        time.Now(),
		done:         make(chan struct{}),
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
	c.materialLog = ""
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
	c.material = appendMaterial(c.material, text)
	// 日志窗口必须**同时**更新：它与 material 共用同一次节流判断，分开算会出现
	// 「单行在动、日志不动」（或反之）——两条线上的内容对不上，读起来更像坏了。
	// align=true：日志是给人连续读的，从词中间劈开会被读成乱码。
	c.materialLog = appendMaterialWindow(c.materialLog, text, materialLogCap, true)
	now := time.Now()
	if !c.lastMat.IsZero() && now.Sub(c.lastMat) < materialThrottle {
		c.mu.Unlock()
		return
	}
	c.lastMat = now
	c.mu.Unlock()
	c.emit()
}

// appendMaterial 是材料窗口**唯一**的写入规则。
//
// 抽成具名函数是为了让测试驱动出货代码：上一版测试自己手搭窗口（把旁白直接 += 上去），
// 结果红在一个线上不可能出现的场景上 —— 测试里重写实现，测的就不是实现。
//
// 规则一句话：追加之前一律先摘掉尾巴上那条旧旁白。
//
//	· 新来的是旁白 → 摘旧的放新的，窗口里永远只剩 1 条旁白（不叠链）；
//	· 新来的是真材料 → 摘掉那句兜底话术，别把「模型思考中…已产出 571 字」和
//	  「· 取素材中具体企业…」黏成一句，读起来像模型在念叨自己的进度。
//
// 不摘老的会得到线上 dump /tmp/mat_dump_final.json 里那个样子：同一句话黏八遍。
func appendMaterial(mat, text string) string {
	return appendMaterialWindow(mat, text, materialCap, false)
}

// appendMaterialWindow 是材料窗口**唯一**的写入实现（materialCap / materialLogCap
// 两个窗口共用）。align=true 时把窗口首字对齐到句首。
//
// 为什么要对齐：线上 dump 里 25 帧全是同一个 160 字尾巴，每帧开头都被劈在词中间
// ——「兰察布风电场的…」「确的时间节点…」「026年9月20日…」。用户读到的是乱码在抖，
// 第一反应是「这程序坏了」，比只跳计时更伤。对齐的代价是每次多丢半句残句，换来
// 每一帧都是完整可读的句子。
//
// 保底：如果第一处句读离窗口尾部不到半个窗口（模型一口气写了个超长句），就不对齐、
// 原样给尾巴——宁可读半句，也不能把窗口丢成几行字。
func appendMaterialWindow(mat, text string, cap int, align bool) string {
	out := strings.TrimLeft(dropTrailingNarration(mat)+text, "…")
	r := []rune(out)
	if len(r) <= cap {
		return out
	}
	r = r[len(r)-cap:]
	if align {
		if i := firstSentenceBreak(r); i >= 0 && len(r)-(i+1) >= cap/2 {
			return "…" + string(r[i+1:])
		}
	}
	return string(r)
}

// firstSentenceBreak 找窗口里第一处**句读**的位置，找不到返回 -1。
//
// 只认中文句读与换行：ASCII 的 '.' 不能进这个集合——中文技术文本里「2026.09」
// 「3.5%」「v1.2」到处都是，按 '.' 对齐会把数字/版本号劈成两半，比不对齐更糟。
func firstSentenceBreak(r []rune) int {
	for i, ch := range r {
		switch ch {
		case '。', '！', '？', '；', '\n':
			return i
		}
	}
	return -1
}

// visibleMaterial 从材料窗口里取出该给用户看的那一段。
//
// 材料窗口是「所有已展示文本的滚动尾巴」；旁白带前导换行（见 agent.materialFilter
// 的 narration），于是窗口形如 `真材料\n旁白\n旁白\n旁白`。只取最后一个换行之后
// 的内容，新旁白就**替换**旧的而不是叠成链 —— 线上 dump 实测没这一步时是这样：
//
//	模型思考中…已产出 239 字（已 2s）模型思考中…已产出 571 字（已 5s）模型思考中…
//
// 用户看到的是同一句话说八遍，而不是「进度在涨」。真材料（中文段）永不含换行。
func visibleMaterial(mat string) string {
	i := strings.LastIndex(mat, "\n")
	if i >= 0 {
		return mat[i+1:]
	}
	return mat
}

// dropTrailingNarration 摘掉窗口尾巴上的那条旁白（如果有）。
// 返回值的**尾部**是最后一段真材料；窗口里只有旁白时返回空串。
func dropTrailingNarration(mat string) string {
	i := strings.LastIndex(mat, "\n")
	if i < 0 {
		return mat
	}
	if agent.IsMaterialNarration(mat[i+1:]) {
		return mat[:i]
	}
	return mat
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
	c.materialLog = ""
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
	c.materialLog = ""
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
	log := c.materialLog
	c.mu.Unlock()
	// 材料窗口是滚动尾巴，旁白（带前导换行）会黏在真材料后面。只把**最后一段**
	// 给用户看：新旁白替换旧旁白，真材料照旧跟在后面。不这么做的话窗口会变成
	// 一条不断变长的旁白链，看起来像复读而不是进度。
	mat = visibleMaterial(mat)
	if !decorate || len(out) == 0 {
		return out
	}
	if i := activePos(out); i >= 0 {
		out[i].Detail = withElapsed(out[i].Detail, c.Elapsed())
		// 材料只挂进行中的那一步：它是「这一步正在干什么」的证据。
		out[i].Material = mat
		// 日志同样只挂进行中的那一步；旁白在日志里是**最后一行**（appendMaterialWindow
		// 每条旁白都带前导换行、且新旁白先摘掉旧的），所以这里不用再 visibleMaterial
		// 收一次——收了就只剩旁白一行，正好把要展示的整段思考丢掉。
		out[i].MaterialLog = strings.TrimSpace(log)
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

// narrateGap 是「本地旁白」的换行间隔。取 3s 与心跳同拍：屏幕上「已用 Ns」在跳的
// 同时，材料区也换一行，用户读到的是「它在按要点推进」，而不是「卡住了」。
const narrateGap = 3 * time.Second

// materialNow 取当前材料区内容。旁白靠它判断「模型刚才有没有在说话」：
// 模型在推材料时旁白让路，别去挤掉读者正在看的那段。
func (c *traceClock) materialNow() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.material
}

// Narrate 用**本地已知事实**当旁白，填满一段没有任何模型材料的阻塞跳。
//
// 为什么需要它：执笔那一跳（chat.go 的「按要点执笔」、chat_write.go 的「起草初稿」）
// 是纯阻塞调用，而线上 provider 实测**一片 reasoning 都不推**（reasoning 片数=0），
// 正文也要等整篇想完才来。于是那几十秒到几分钟里，屏幕上只有步骤标签和不断 +3 的
// 「已用 Ns」——用户原话就是「一直卡着计时」。
//
// 修法不是造假流，而是把**已经在我们手里的事实**（构思要点、装配了什么上下文）
// 定时滚出来：内容全部本地真值，模型没说话时也不会撒谎。
// 走的是同一条 material 通道（Thinking），所以前端不用改、也不会和正文打架。
//
// 返回的 stop 必须被调用（否则 goroutine 会跨轮泄漏，继续往下一个会话的流里写字）。
func (c *traceClock) Narrate(lines []string) (stop func()) {
	clean := make([]string, 0, len(lines))
	for _, ln := range lines {
		if s := strings.TrimSpace(ln); s != "" {
			clean = append(clean, tailRunes(s, narrateLineCap))
		}
	}
	if len(clean) == 0 {
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	// 进跳第一行：只有在材料区还空着（模型确实什么都没推）时才立刻补，
	// 否则等于把读者正在看的那段挤掉。剩下交给 tick 的让路判断。
	if c.materialNow() == "" {
		c.Thinking("· " + clean[0])
	}
	go func() {
		gap := c.narrateEvery
		if gap <= 0 {
			gap = narrateGap
		}
		t := time.NewTicker(gap)
		defer t.Stop()
		i := 0
		seen := c.materialNow()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				// 让路：材料区自上次旁白以来被模型写过，就这一拍不插话。
				// 只有真静默（模型一片都不推）时，旁白才顶上——它本来就是为了填静默。
				if cur := c.materialNow(); cur != seen {
					seen = cur
					continue
				}
				i++
				// 一轮走完就从头再滚，并标上「第 N 遍」：重复的是真事实，
				// 但要让人看出这是回顾而不是新进展（否则等于在骗人）。
				if i%len(clean) == 0 {
					c.Thinking(fmt.Sprintf("（要点回顾 %d/%d）· %s", len(clean), len(clean), clean[0]))
					seen = c.materialNow()
					continue
				}
				c.Thinking("· " + clean[i%len(clean)])
				seen = c.materialNow()
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// narrateLineCap 是旁白单行上限。材料只留尾部 materialCap 字，一行太长会把
// 别的行挤掉、只剩半句；48 字上下正好是「一眼读懂」的量。
const narrateLineCap = 48

// narrateLines 把「构思要点 / 装配事实」切成旁白行。
// 要点文本是模型写的自由文本（带 -、1.、· 等前缀的短行混着长段），直接整段滚
// 会变成一坨。这里优先取列表行；一条列表行都没有时（要点是一段话），按句号切。
func narrateLines(plan, fallback string) []string {
	var out []string
	for _, ln := range strings.Split(plan, "\n") {
		s := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(ln), "-*·•0123456789.、)（("))
		if strings.TrimSpace(s) == "" {
			continue
		}
		// 列表行特征：原行以符号/编号开头，且去掉标记后仍然不长。
		if len([]rune(s)) <= narrateLineCap && (strings.HasPrefix(strings.TrimSpace(ln), "-") ||
			strings.HasPrefix(strings.TrimSpace(ln), "*") ||
			strings.HasPrefix(strings.TrimSpace(ln), "·") ||
			strings.HasPrefix(strings.TrimSpace(ln), "•") ||
			(len(ln) > 0 && ln[0] >= '0' && ln[0] <= '9')) {
			out = append(out, s)
		}
	}
	if len(out) == 0 && strings.TrimSpace(plan) != "" {
		for _, s := range strings.FieldsFunc(plan, func(r rune) bool { return r == '。' || r == '\n' }) {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, tailRunes(s, narrateLineCap))
			}
		}
	}
	if len(out) == 0 && strings.TrimSpace(fallback) != "" {
		// 装配事实是一整句（「技能：X｜要素 3 项｜上文 1.2k 字」这类），
		// 按分隔符拆开才有多行可滚；拆不出来就整句重复（至少不等于静止）。
		for _, s := range strings.FieldsFunc(fallback, func(r rune) bool {
			return r == '；' || r == '｜' || r == ';'
		}) {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, tailRunes(s, narrateLineCap))
			}
		}
	}
	if len(out) > 8 { // 面板不是阅读器：超过 8 行一轮太久，等于没换过
		out = out[:8]
	}
	return out
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
