package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/agent"
	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/model"
)

// 写作链路的**步骤板接线**测试（真走 /api/chat，不看中间件内部状态）。
//
// 为什么需要它：线上那一轮「写新闻稿 → 再整理成 word」里，用户盯着看的唯一东西就是
// 这块板子。板子要是「两个 ① 同时在转」「编号栏印着英文字面量 plan」「标签全程停在
// ① 意图分析」，用户读到的就是「卡住了」——即使后台一直在干活、材料一直在滚。
//
// 所以这里不锚具体文案，锚四条**渲染契约**：
//
//	R1 两条写作路（命中 write 型技能 / 没命中技能的普通对话）都必须有
//	   「构思要点 → 按要点执笔」两格，且执笔那格**真的进过 active**（进没进过 = 有没有
//	   真跑到那一跳，这是防「空跑绿」的前提断言）；
//	R2 任何一帧里最多一个 active 格 —— Carry 接过首格之后又追加同名步骤就是这个症状；
//	R3 每一格的 phase 必须是**出货前端** AGENTS 表里存在的键，否则前端 [chat.js:1371]
//	   会把 phase 原样印在编号栏上（"plan"）、角色徽标空白；
//	R4 同一圈号（①..⑤）在一轮里只出现一次。
//
// 这四条都是「用户能看见的坏」：R2/R4 是屏幕上出现两个同名的圈，R3 是编号栏里冒出
// 一个英文单词。

// draftMarker 是假模型"正文"的开头。断言它出现 = 这一轮真跑到了执笔并流出了正文，
// 否则后面那些格子怎么变都说明不了问题（尺子先量前提，再量产品）。
const draftMarker = "【正文标记】"

// 步骤板契约测试共用的开板断言。
func assertBoardContract(t *testing.T, sse string, table string) [][]agent.TraceStep {
	t.Helper()
	boards := boardFromFrames(t, sse)
	assertAtMostOneLive(t, boards)
	assertPhasesRenderable(t, boards, table)
	return boards
}

// boardFromFrames 收集整轮里所有 trace 帧，逐帧解析成步骤组。
func boardFromFrames(t *testing.T, sse string) [][]agent.TraceStep {
	t.Helper()
	var out [][]agent.TraceStep
	for _, fr := range routeFrames(sse) {
		if fr.event != evTrace {
			continue
		}
		var st []agent.TraceStep
		if err := json.Unmarshal([]byte(fr.data), &st); err != nil {
			t.Fatalf("trace 帧解不回步骤组（尺子自己坏了，不是产品坏了）：%v\n---- 帧 ----\n%s", err, fr.data)
		}
		out = append(out, st)
	}
	if len(out) == 0 {
		t.Fatalf("整轮一个 trace 帧都没有 —— 步骤板根本没下发，用户看到的就是一直转圈\n---- SSE ----\n%s", sse)
	}
	return out
}

// assertAtMostOneLive 是 R2：一帧里不该有两个格子同时在转。
func assertAtMostOneLive(t *testing.T, boards [][]agent.TraceStep) {
	t.Helper()
	for i, st := range boards {
		var live []string
		for _, s := range st {
			if s.Status == "active" {
				live = append(live, s.Label)
			}
		}
		if len(live) > 1 {
			t.Fatalf("第 %d 帧有 %d 格同时「进行中」：%v\n"+
				"屏幕上就是两个圈一起转，其中一个还永远不会停（Carry 接过 t≈0 的首格之后，"+
				"又用 Done 追加了一格同名步骤）。\n---- 该帧 ----\n%s", i, len(live), live, dumpSteps(st))
		}
	}
}

// assertPhasesRenderable 是 R3：phase 必须在前端表里有映射。
// 尺子对着**出货的前端文件**量，不抄一份表进来 —— 抄一份就测不到「后端发明了一个
// 前端认不得的 phase」这件事本身。
func assertPhasesRenderable(t *testing.T, boards [][]agent.TraceStep, table string) {
	t.Helper()
	keys := frontendPhaseKeys(t, table)
	for _, st := range boards {
		for _, s := range st {
			if keys[s.Phase] {
				continue
			}
			t.Fatalf("步骤 %q 的 phase=%q 在前端 %s 表里没有映射。\n"+
				"前端 [web/js/chat.js:1362] 的写法是 agents[s.phase] || {role:'',act:''}，"+
				"取不到就退回 s.phase 本身当编号（[chat.js:1371] agent.n || s.phase）："+
				"用户会看到编号栏里印着英文单词 %q、角色徽标空白。\n---- 该帧 ----\n%s",
				s.Label, s.Phase, table, s.Phase, dumpSteps(st))
		}
	}
}

// assertOnePerOrdinal 是 R4：同一圈号在一轮里只出现一次。
func assertOnePerOrdinal(t *testing.T, final []agent.TraceStep) {
	t.Helper()
	seen := map[rune]string{}
	for _, s := range final {
		r := firstCircledDigit(s.Label)
		if r == 0 {
			continue
		}
		if prev, dup := seen[r]; dup {
			t.Fatalf("同一圈号 %c 出现了两格：%q 与 %q\n"+
				"用户读到的步骤数是错的（两格 ① = 分不清到底走到第几步）。\n---- 终帧 ----\n%s",
				r, prev, s.Label, dumpSteps(final))
		}
		seen[r] = s.Label
	}
}

// firstCircledDigit 取标签开头的圈号（①②③…）；不是圈号开头返回 0。
func firstCircledDigit(label string) rune {
	for _, r := range strings.TrimSpace(label) {
		if strings.ContainsRune("①②③④⑤⑥⑦⑧⑨⑩", r) {
			return r
		}
		return 0
	}
	return 0
}

// dumpSteps 把步骤组打成可读的多行，失败信息里直接用。
func dumpSteps(st []agent.TraceStep) string {
	var b strings.Builder
	for i, s := range st {
		mid := " "
		if s.Status == "active" {
			mid = ">"
		}
		detail := s.Detail
		if len([]rune(detail)) > 60 {
			detail = string([]rune(detail)[:60]) + "…"
		}
		b.WriteString("  " + string(rune('0'+i)) + mid + s.Status + "  phase=" + s.Phase +
			"  label=" + s.Label + "  detail=" + detail + "\n")
	}
	return b.String()
}

// frontendPhaseKeys 从出货的 web/js/chat.js 里抠出某张 AGENTS 表的键集合。
func frontendPhaseKeys(t *testing.T, table string) map[string]bool {
	t.Helper()
	p := filepath.Join("..", "..", "web", "js", "chat.js")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读不到出货前端 %s：%v —— 这条尺子必须对着真前端量", p, err)
	}
	src := string(b)
	i := strings.Index(src, "const "+table+" = {")
	if i < 0 {
		t.Fatalf("前端脚本里没有 %s 表：表名被改了就要同步这条尺子，否则它变成一把空尺子", table)
	}
	rest := src[i:]
	j := strings.Index(rest, "\n  };")
	if j < 0 {
		t.Fatalf("%s 表没找到收尾的 \"  };\" —— 解析失败，尺子失效", table)
	}
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s{4}([A-Za-z_]\w*):\s*\{`).FindAllStringSubmatch(rest[:j], -1) {
		out[m[1]] = true
	}
	if len(out) < 4 {
		t.Fatalf("%s 表只解析出 %d 个 phase（%v）：解析器坏了，这条断言会恒绿", table, len(out), out)
	}
	return out
}

// assertDraftStepContract 检查「构思要点 → 按要点执笔」这两格。
//
//	final：终帧（用来判格子的最终状态与先后顺序）
//	boards：整轮所有帧（用来判执笔那格**是否真进过 active**）
func assertDraftStepContract(t *testing.T, sse string, boards [][]agent.TraceStep, planLabel, draftLabel string) {
	t.Helper()
	if !strings.Contains(deltaText(routeFrames(sse)), draftMarker) {
		t.Fatalf("整轮没有流出正文（找 %q）—— 前提不成立，后面那些格子怎么变都说明不了问题\n---- 拼回的正文 ----\n%s",
			draftMarker, deltaText(routeFrames(sse)))
	}
	final := boards[len(boards)-1]
	posOf := func(label string) int {
		for i, s := range final {
			if s.Label == label {
				return i
			}
		}
		return -1
	}
	pi, di := posOf(planLabel), posOf(draftLabel)
	if pi < 0 {
		t.Fatalf("终帧里没有 %q —— 构思那一跳在板子上不存在\n---- 终帧 ----\n%s", planLabel, dumpSteps(final))
	}
	if di < 0 {
		t.Fatalf("终帧里没有 %q —— 执笔那一跳在板子上不存在\n---- 终帧 ----\n%s", draftLabel, dumpSteps(final))
	}
	if pi > di {
		t.Fatalf("%q 排在 %q 后面：构思还没做完就执笔了，步骤顺序是反的\n---- 终帧 ----\n%s",
			planLabel, draftLabel, dumpSteps(final))
	}
	if final[pi].Status != "done" {
		t.Fatalf("%q 定格时不是 done（%s）—— 屏幕上它还在转，用户会以为卡在这一步\n---- 终帧 ----\n%s",
			planLabel, final[pi].Status, dumpSteps(final))
	}
	if di != len(final)-1 {
		t.Fatalf("%q 不是最后一格（下标 %d/%d）：执笔之后的步骤必须自己追加，否则板子会在执笔那格停住\n---- 终帧 ----\n%s",
			draftLabel, di, len(final)-1, dumpSteps(final))
	}
	// 执笔那格必须**真进过 active**：不然它只是被写死在终帧里的一个标签。
	liveSeen := false
	for _, st := range boards {
		for _, s := range st {
			if s.Label == draftLabel && s.Status == "active" {
				liveSeen = true
			}
		}
	}
	if !liveSeen {
		t.Fatalf("%q 从头到尾没进过 active：这一跳根本没在板子上跑过，标签是摆设\n---- 终帧 ----\n%s",
			draftLabel, dumpSteps(final))
	}
	assertOnePerOrdinal(t, final)
}

// T1：命中 write 型技能（技能没有手册分类 → 退化成单段执笔）的路。
func TestWriteSkillBoardShowsPlanThenDraft(t *testing.T) {
	dir := t.TempDir()
	f := newFakeLLM(t, func(system, user string) string {
		switch {
		case strings.Contains(system, "本轮构思要点"): // 执笔那一跳（构思要点并进了 system 末尾）
			return draftMarker + strings.Repeat("园区开放日的补充说明。", 12)
		case strings.Contains(system, "先帮用户把这一篇的写法想清楚") ||
			strings.Contains(system, "新闻稿写作"): // 构思那一跳（write 型技能走的是 generateSys）
			return "· 开头交代时间地点与主办方\n· 中段写活动安排\n· 结尾不喊口号"
		}
		return draftMarker + strings.Repeat("园区开放日的补充说明。", 12)
	})
	sk := newStoreForTest(t, dir)
	// 关键：write 型技能 + **没有手册分类目录** → LoadWritePack 返回 nil，
	// 于是走 chat.go 里那条「构思要点 / 按要点执笔」的退化路。
	// 必须走 CreateSkill（写 system_prompt.md + examples/source 目录）而不是 Create：
	// 只写库不写盘，LoadSkill 会在 SystemPrompt() 这步失败，技能静默退回通用写作 ——
	// 测试会「看起来跑过了」，其实压根没测到 write 型技能那条路（第一版就这么红过：）
	// 那两帧里 step ② 是「上下文装配」而不是「技能匹配」）。
	if err := sk.CreateSkill(&model.Skill{
		Slug: "新闻稿写作", Name: "新闻稿写作", SkillType: model.SkillTypeWrite, Enabled: true,
	}, nil, "# 新闻稿写作\n\n按倒金字塔写，标题不超过 20 字。"); err != nil {
		t.Fatalf("建 write 型技能失败：%v", err)
	}
	h := &chatHandler{
		eng:      agent.New(llm.New(&model.LLMConfig{APIKey: "test", BaseURL: f.srv.URL, Model: "fake"}), sk),
		maxRound: 1,
		gen:      newGenCache(),
	}

	sse := runRouteChat(t, h, `{"session_id":"s-board-write","message":"写一篇园区开放日的新闻稿",`+
		`"mode":"manual","skill":"新闻稿写作"}`)
	boards := assertBoardContract(t, sse, "AGENTS_MANUAL")
	assertDraftStepContract(t, sse, boards, "④ 构思要点", "⑤ 按要点执笔")
}

// T2：没命中技能（分类判「通用写作、不生成文件」）的普通对话路。
// 线上那一轮「写新闻稿 → 再整理成 word」的第 1 轮走的就是这条道（首字 71.7s、
// 最大静默 60.3s 的账本就是在它身上记的），所以它必须和 T1 一样有板子。
func TestPlainPathBoardShowsPlanThenDraft(t *testing.T) {
	dir := t.TempDir()
	f := newFakeLLM(t, func(system, user string) string {
		switch {
		case strings.Contains(system, "多智能体管线的调度器"):
			return `{"intent":"write","action":"","skill_slug":"","reason":"通用写作，不生成文件",` +
				`"needs_tools":false,"needs":[]}`
		case strings.Contains(system, "本轮构思要点"): // 执笔那一跳
			return draftMarker + strings.Repeat("园区开放日的补充说明。", 12)
		case strings.Contains(system, "先帮用户把这一篇的写法想清楚"): // 构思那一跳
			return "· 先写会议背景\n· 再写参会范围\n· 落款不写套话"
		}
		return draftMarker + strings.Repeat("园区开放日的补充说明。", 12)
	})
	h := &chatHandler{eng: newManualEngine(t, dir, f), maxRound: 1, gen: newGenCache()}

	sse := runRouteChat(t, h, `{"session_id":"s-board-plain","message":"写一篇园区开放日的新闻稿","mode":"auto"}`)
	boards := assertBoardContract(t, sse, "AGENTS")
	assertDraftStepContract(t, sse, boards, "③ 构思要点", "④ 按要点执笔")
}
