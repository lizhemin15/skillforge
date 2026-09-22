package api

import (
	"strings"
	"testing"
	"time"

	"github.com/lizhemin15/skillforge/internal/agent"
)

// 「中间材料」这条链路的回归防线。
//
// 用户抱怨的原话：「速度过于慢了，中间可以流式输出思考的一些中间材料，现在一直
// 卡着计时，用户体验不佳」。也就是说：光盘面上有个跳秒的计时器不算「有进度」，
// 用户要看到**正在动的内容**。后端这一半的职责是：把模型流出来的思考片段挂到
// 进行中的那一步上，并且不能因此把 SSE 刷爆或把状态条撑成一堵墙。

// thinkNow 绕过节流窗口调一次 Thinking。
//
// 为什么需要它：材料是 400ms 节流下发的，测试里连着写两片，第二片**本来就**
// 还在窗口内没发出去。断言「帧里看到了第二片」会假红；反过来把断言放宽成
// 「看到了第一片」，就会把「第二片丢了」这种真 bug 放绿。所以这里显式把
// 节流时间戳往回拨——测的是状态与下发内容，不是计时。
func thinkNow(c *traceClock, text string) {
	c.mu.Lock()
	c.lastMat = time.Now().Add(-materialThrottle - time.Millisecond)
	c.mu.Unlock()
	c.Thinking(text)
}

// 材料必须真的进到帧里，且只挂在 active 那一步上。
func TestTraceMaterialAttachesToActiveStep(t *testing.T) {
	rec := &frameRec{}
	steps := []agent.TraceStep{
		{Phase: "analyze", Label: "① 意图分析", Detail: "已完成", Status: "done"},
		{Phase: "generate", Label: "④ 内容执笔", Detail: "正在撰写内容…", Status: "active"},
	}
	c := newTraceClockBeat(rec.write, steps, time.Hour) // 心跳调到不影响断言
	defer c.Freeze()

	c.Thinking("先看手册要求：")
	thinkNow(c, "这是一份通知，需要标题、正文、落款。")

	frames := rec.traces(t)
	last := frames[len(frames)-1]
	if last[1].Material == "" {
		t.Fatalf("材料没有挂到 active 步骤上：%+v", last)
	}
	if !strings.Contains(last[1].Material, "这是一份通知") {
		t.Fatalf("材料内容不对: %q", last[1].Material)
	}
	if last[0].Material != "" {
		t.Fatalf("已完成的步骤不该带材料: %q", last[0].Material)
	}
	// 材料是**独立字段**，不能混进 detail：状态条读的就是 detail，
	// 混进去会让顶部那行变成一堵不断变长的墙。
	if strings.Contains(last[1].Detail, "这是一份通知") {
		t.Fatalf("材料污染了 detail: %q", last[1].Detail)
	}
}

// 材料只留尾部：思考链是几万字的长文，整段下发既刷爆 SSE 又没人读。
func TestTraceMaterialKeepsTailOnly(t *testing.T) {
	rec := &frameRec{}
	c := newTraceClockBeat(rec.write, bootSteps(), time.Hour)
	defer c.Freeze()

	c.Thinking(strings.Repeat("甲", 300))
	frames := rec.traces(t)
	got := frames[len(frames)-1][0].Material
	if n := len([]rune(got)); n != materialCap {
		t.Fatalf("材料应截到 %d 字，实际 %d 字", materialCap, n)
	}
	// 尾部语义：最后一片一定在。头部被丢掉不算 bug，尾部丢了才是（滚动的意义就在尾部）。
	thinkNow(c, "★尾片★")
	frames = rec.traces(t)
	if !strings.Contains(frames[len(frames)-1][0].Material, "★尾片★") {
		t.Fatalf("最新的材料片段必须还在: %q", frames[len(frames)-1][0].Material)
	}
}

// 按字符截，不按字节截：按字节切会把一个汉字劈成两半，前端显示成乱码。
func TestTraceMaterialDoesNotSplitRunes(t *testing.T) {
	rec := &frameRec{}
	c := newTraceClockBeat(rec.write, bootSteps(), time.Hour)
	defer c.Freeze()

	c.Thinking(strings.Repeat("中", materialCap+50))
	got := rec.traces(t)[1][0].Material
	for _, r := range got {
		if r != '中' {
			t.Fatalf("材料里出现半个汉字（乱码）: %q", got)
		}
	}
}

// 节流：token 级推流每秒几十片，逐片下发会把 SSE 刷屏。
func TestTraceMaterialThrottled(t *testing.T) {
	rec := &frameRec{}
	c := newTraceClockBeat(rec.write, bootSteps(), time.Hour)
	defer c.Freeze()

	base := rec.count()
	for i := 0; i < 50; i++ {
		c.Thinking("片")
	}
	if got := rec.count() - base; got > 1 {
		t.Fatalf("400ms 内 50 片材料只该下发 1 帧，实际 %d 帧", got)
	}

	// 节流窗口过去后必须能继续下发，否则材料会永远停住 —— 那比没有材料更糟
	//（用户看到的是「卡住了」而不是「在思考」）。
	c.mu.Lock()
	c.lastMat = time.Now().Add(-materialThrottle - time.Millisecond)
	c.mu.Unlock()
	before := rec.count()
	c.Thinking("后续片段")
	if rec.count() == before {
		t.Fatal("节流窗口结束后材料必须继续下发")
	}
}

// 没有进行中的步骤时不收材料：等待用户补充的阶段不该继续滚。
func TestTraceMaterialIgnoredWhenNothingActive(t *testing.T) {
	rec := &frameRec{}
	steps := []agent.TraceStep{{Phase: "params", Label: "③ 要素提炼", Detail: "等你补充", Status: "waiting"}}
	c := newTraceClockBeat(rec.write, steps, time.Hour)
	defer c.Freeze()

	before := rec.count()
	c.Thinking("这时不该有材料")
	if rec.count() != before {
		t.Fatal("没有 active 步骤时不该下发材料帧")
	}
	if m := rec.traces(t)[0][0].Material; m != "" {
		t.Fatalf("waiting 步骤不该带材料: %q", m)
	}
}

// 换阶段要清材料：上一阶段的思考挂在新步骤下面会误导人。
func TestTraceMaterialClearedOnSetAndFinish(t *testing.T) {
	rec := &frameRec{}
	c := newTraceClockBeat(rec.write, bootSteps(), time.Hour)
	defer c.Freeze()

	c.Thinking("第一阶段的思考")
	c.Set(bootSteps())
	frames := rec.traces(t)
	if m := frames[len(frames)-1][0].Material; m != "" {
		t.Fatalf("Set 之后材料应清空，实际 %q", m)
	}

	c.Thinking("第二阶段的思考")
	c.Finish()
	frames = rec.traces(t)
	if m := frames[len(frames)-1][0].Material; m != "" {
		t.Fatalf("Finish 之后材料应清空，实际 %q", m)
	}
}

// 心跳帧也必须带上材料：节流窗口内的材料靠心跳兜底送出，否则最后一片会丢。
func TestTraceMaterialRidesHeartbeat(t *testing.T) {
	rec := &frameRec{}
	c := newTraceClockBeat(rec.write, bootSteps(), 40*time.Millisecond)
	defer c.Freeze()

	c.Thinking("心跳期间的材料")
	time.Sleep(120 * time.Millisecond)

	frames := rec.traces(t)
	if len(frames) < 2 {
		t.Fatalf("心跳应当补帧，实际 %d 帧", len(frames))
	}
	last := frames[len(frames)-1][0]
	if !strings.Contains(last.Material, "心跳期间的材料") {
		t.Fatalf("心跳帧丢了材料: %+v", last)
	}
	// 材料不能把「已用 Ns」挤掉：两者是不同的信息。
	if !strings.Contains(last.Detail, "已用 ") {
		t.Fatalf("active 步骤的 detail 仍应带秒数: %q", last.Detail)
	}
}

// tailRunes 的边界：短串原样返回、空串不炸、n<=0 不 panic。
func TestTailRunes(t *testing.T) {
	if got := tailRunes("abc", 5); got != "abc" {
		t.Fatalf("短串应原样返回，实际 %q", got)
	}
	if got := tailRunes("", 3); got != "" {
		t.Fatalf("空串应返回空串，实际 %q", got)
	}
	if got := tailRunes("abcdef", 3); got != "def" {
		t.Fatalf("应取尾部 3 字，实际 %q", got)
	}
}

// ---------------------------------------------------------------------------
// 材料**滚动日志**（MaterialLog）的防线。
//
// 为什么单开一组：Material 是 160 字单行尾巴，前端每帧原地替换它，屏幕上就是
// 「一行字在原地抖 + 计时器在跳」，实测被用户读成「程序坏了」。日志窗口要的是
// 「整段在长」。两者的窗口大小与语义不同，必须各守各的，否则改一个字段的逻辑
// 悄悄把另一个搞坏，界面上没有任何报错。

// 日志必须远长于单行窗口，且**开头不能被滚掉**（那正是「整段在长」的意思）。
func TestTraceMaterialLogKeepsWholeProgress(t *testing.T) {
	rec := &frameRec{}
	c := newTraceClockBeat(rec.write, bootSteps(), time.Hour)
	defer c.Freeze()

	c.Thinking("开头第一句。")
	// 30 片 × 12 字 ≈ 360 字：远小于日志窗口 1200，但已远超单行窗口 160。
	// 选这个长度就是要让「单行尾巴丢掉开头、日志保住开头」这件事可观测。
	for i := 0; i < 30; i++ {
		thinkNow(c, "中间内容一点一点长出来。")
	}

	last := rec.traces(t)[len(rec.traces(t))-1][0]
	if got := len([]rune(last.Material)); got != materialCap {
		t.Fatalf("单行材料应被截到 %d 字，实际 %d 字", materialCap, got)
	}
	if n := len([]rune(last.MaterialLog)); n < 360 {
		t.Fatalf("日志窗口应保住整段进度（≥360 字），实际只有 %d 字：%q", n, last.MaterialLog)
	}
	if !strings.Contains(last.MaterialLog, "开头第一句") {
		t.Fatalf("日志被当成单行尾巴截了，开头丢了：%q", last.MaterialLog)
	}
	if strings.Contains(last.Material, "开头第一句") {
		t.Fatalf("单行窗口不该还留着开头（说明截断逻辑被改了，断言失去区分力）：%q", last.Material)
	}
}

// 日志窗口超过上限时按字符截尾，且不能把汉字劈开（劈开=前端乱码）。
func TestTraceMaterialLogCappedAndRunSafe(t *testing.T) {
	log := appendMaterialWindow("", strings.Repeat("中", materialLogCap+200), materialLogCap, true)
	if n := len([]rune(log)); n > materialLogCap {
		t.Fatalf("日志窗口应截到 %d 字以内，实际 %d 字", materialLogCap, n)
	}
	for _, r := range strings.TrimLeft(log, "…") {
		if r != '中' {
			t.Fatalf("日志窗口里出现半个汉字（前端会显示成乱码）: %q", log)
		}
	}
}

// 截断后窗口首字要对齐到句首：线上 dump 里 25 帧全被劈在词中间（「兰察布风电场的…」
// 「026年9月20日…」），用户读成乱码在抖。
func TestAppendMaterialWindowAlignsToSentenceStart(t *testing.T) {
	// 200 句 × 11 字 = 2200 字 > 1200，必然触发截断。每句都是「甲乙丙丁戊己庚辛壬癸。」，
	// 所以句首字恒为「甲」——对齐了首字就是「甲」，没对齐就是 1/11 的概率碰对。
	sent := "甲乙丙丁戊己庚辛壬癸。"
	got := appendMaterialWindow("", strings.Repeat(sent, 200), materialLogCap, true)
	if !strings.HasPrefix(got, "…") {
		t.Fatalf("窗口被截断后应带省略号（表示前面还有），实际: %q", string([]rune(got)[:min(20, len([]rune(got)))]))
	}
	body := []rune(strings.TrimPrefix(got, "…"))
	if len(body) == 0 || body[0] != '甲' {
		t.Fatalf("窗口首字应对齐到句首「甲」，实际以 %q 开头", string(body[:min(8, len(body))]))
	}
	if len(body) > materialLogCap {
		t.Fatalf("对齐不应让窗口超过上限：%d 字", len(body))
	}
}

// 模型一口气写了个超长句（窗口尾部半程内找不到句读）时不许对齐——宁可读半句，
// 也不能为了对齐把窗口丢成几行字（那等于把「整段在长」改回「一行在抖」）。
func TestAppendMaterialWindowFallsBackWhenNoSentenceBreak(t *testing.T) {
	got := appendMaterialWindow("", strings.Repeat("字", materialLogCap+50), materialLogCap, true)
	if strings.HasPrefix(got, "…") {
		t.Fatalf("找不到句读时不该强行对齐（会把窗口丢短）: %q", got)
	}
	if n := len([]rune(got)); n != materialLogCap {
		t.Fatalf("兜底路径应给满窗口 %d 字，实际 %d 字", materialLogCap, n)
	}
}

// 句读集合里不许有 ASCII 的 '.'：中文技术文本里「2026.09」「3.5%」「v1.2」到处都是，
// 按 '.' 对齐会把数字/版本号劈成两半，比不对齐更糟。
func TestFirstSentenceBreakIgnoresAsciiDot(t *testing.T) {
	r := []rune("版本 2026.09 发布，占比 3.5%。")
	i := firstSentenceBreak(r)
	if i < 0 {
		t.Fatal("含有中文句号的串必须能找到句读")
	}
	if r[i] != '。' {
		t.Fatalf("句读应命中末尾的「。」，实际命中 %q（ASCII '.' 混进句读集合了）", string(r[i]))
	}
	if got := firstSentenceBreak([]rune("版本 v1.2 无句读")); got != -1 {
		t.Fatalf("没有中文句读时应返回 -1，实际 %d", got)
	}
}

// 日志窗口与单行窗口必须**分叉**：日志保住整段思考，单行只给最后一段。
// 这条断言直接钉住 snapshotSteps 里 MaterialLog 没过 visibleMaterial 这个决定——
// 过了的话日志就只剩旁白一行，正好把要展示的整段思考丢掉。
func TestTraceMaterialLogKeepsRealThinkingWhileMaterialShowsNarration(t *testing.T) {
	rec := &frameRec{}
	c := newTraceClockBeat(rec.write, bootSteps(), time.Hour)
	defer c.Freeze()

	c.Thinking("真材料：这是一份通知，需要标题、正文、落款。")
	thinkNow(c, "\n模型思考中…已产出 239 字（已 2s）")

	last := rec.traces(t)[len(rec.traces(t))-1][0]
	if !strings.Contains(last.Material, "已产出 239 字") {
		t.Fatalf("单行材料应显示最新旁白（进度口径）: %q", last.Material)
	}
	if strings.Contains(last.Material, "这是一份通知") {
		t.Fatalf("单行材料应只留最后一段（旁白），不该黏成链: %q", last.Material)
	}
	if !strings.Contains(last.MaterialLog, "这是一份通知") {
		t.Fatalf("日志必须保住真材料（不能也走 visibleMaterial，否则只剩旁白一行）: %q", last.MaterialLog)
	}
}

// 三个重置点（Set / Awaiting / Finish）都要清日志。漏一个就会出现「上一步的思考
// 挂在新阶段下面」——比没有日志更误导人。
func TestTraceMaterialLogClearedAtEveryResetPoint(t *testing.T) {
	logNow := func(c *traceClock) string {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.materialLog
	}
	cases := []struct {
		name  string
		reset func(c *traceClock)
	}{
		{"Set（换阶段）", func(c *traceClock) { c.Set(bootSteps()) }},
		{"Awaiting（等用户补充）", func(c *traceClock) { c.Awaiting("等你补充") }},
		{"Finish（收尾）", func(c *traceClock) { c.Finish() }},
	}
	for _, tc := range cases {
		rec := &frameRec{}
		c := newTraceClockBeat(rec.write, bootSteps(), time.Hour)
		c.Thinking("上一阶段的思考")
		if logNow(c) == "" {
			t.Fatalf("%s: 前提不成立——Thinking 之后日志本应非空", tc.name)
		}
		tc.reset(c)
		if got := logNow(c); got != "" {
			t.Errorf("%s: 日志应清空，实际残留 %q", tc.name, got)
		}
		c.Freeze()
	}
}
