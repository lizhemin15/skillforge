package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lizhemin15/skillforge/internal/agent"
	"github.com/lizhemin15/skillforge/internal/model"
)

// 起草这一跳的「中间材料」回归防线。
//
// 用户的原话是「速度过于慢了，中间可以流式输出思考的一些中间材料，现在一直卡着
// 计时，用户体验不佳」。之前这一跳的 onDelta 传的是 nil：起草是整条手册链路里最长
// 的一跳，而这段时间后端只发心跳（详情里那句「已用 Ns」），屏幕上没有**任何**
// 在动的内容，正是用户说的「卡着计时」。
//
// 这条测试守两件独立的事，缺一不可：
//  1. 起草的正文**真的作为材料滚出来**（onDelta 挂上了 clock.Thinking）；
//  2. 起草的正文**没有提前刷进正文区**（设计约束：后面还有审稿改稿，先刷初稿再刷
//     改稿会让用户以为生成了两篇）。第 2 条不是凑数的——把 onDelta 直接接到
//     evDelta 上也能让第 1 条变绿，但那就破坏了交付语义。

// 初稿的独特标记句。**故意很短**：材料只留尾部 160 字（materialCap），初稿太长
// 会把前面那句「已装配…」挤出窗口，断言就变成在测截断而不是在测挂接。
const fixDraftMark = "【初稿标记句】"

func newManualWriteHandler(t *testing.T, dataDir string, f *fakeLLM) *chatHandler {
	t.Helper()
	return &chatHandler{eng: newManualEngine(t, dataDir, f)}
}

// draftMaterialReply 按 system 提示词分流三种调用：判类 / 起草 / 审稿。
// 用 system 而不是 user 分流，是因为起草与审稿的 user 里都带用户需求原文，
// 靠它分流会在换样本时静默走错分支（走错了是假绿，不是红）。
func draftMaterialReply(sys, _ string) string {
	switch {
	case strings.Contains(sys, "审稿人"):
		return `{"verdict":"pass","issues":[]}`
	case strings.Contains(sys, "公文写作助手"):
		return fixDraftMark + "本稿首段包含时间、地点、主体、事件四要素。"
	default:
		return `{"category":"新闻通稿","confidence":"high","reason":"对外发布"}`
	}
}

func TestManualWriteStreamsDraftAsMaterial(t *testing.T) {
	dir, slug := newManualWriteFixture(t)
	f := newFakeLLM(t, draftMaterialReply)
	// 材料是 400ms 节流下发的：假模型两片正文几乎同时到达时，第二片必然落在节流
	// 窗口内不发帧，「材料里看到了初稿」就会假红。把分片间隔拉过窗口，测的才是
	// 「初稿进没进材料」，而不是「节流器有没有恰好放行」。
	f.chunkGap = 450 * time.Millisecond
	h := newManualWriteHandler(t, dir, f)

	eng := h.eng
	pack, err := eng.LoadWritePack(slug)
	if err != nil || pack == nil {
		t.Fatalf("准备素材失败：pack=%v err=%v", pack, err)
	}
	sc := &agent.SkillContent{Slug: slug, Name: "手册写作", SkillType: model.SkillTypeWrite,
		SystemPrompt: "你是公文写作助手，按手册要求写作。"}

	rec := &frameRec{}
	clock := newTraceClockBeat(rec.write, bootSteps(), time.Hour) // 心跳调远，只留材料帧
	defer clock.Freeze()

	done := h.runManualWrite(context.Background(), rec.write, clock, sc, pack,
		map[string]string{"主题": "数据中台3.0上线"}, "帮我写一份数据中台上线的新闻通稿", "sess-draft-mat", nil)
	if !done {
		t.Fatal("runManualWrite 应返回 true（本轮已处理完毕）")
	}

	// ① 材料里必须出现「已装配…」这句本地事实。
	// 它是「分类回来到首个正文字」之间屏幕上唯一的可见物，也是本改动最容易被
	// 后来者顺手删掉的一行（删了不影响任何功能，只影响观感——所以必须钉住）。
	if !anyMaterial(rec, "已装配") {
		t.Fatalf("材料里没有「已装配…」这句装配事实。\n%s\n落到的材料帧：\n%s",
			whyDraftMaterialMatters(), dumpMaterials(t, rec))
	}
	// ② 装配事实要报对东西：命中哪一类、上文多少条 —— 这正是用户判断「它到底
	// 看没看我给的东西」的依据。只报一句「正在起草」等于换了个说法的计时器。
	if !anyMaterial(rec, "新闻通稿") {
		t.Fatalf("装配事实里没报出命中的类别。\n材料帧：\n%s", dumpMaterials(t, rec))
	}

	// ③ 起草的正文必须作为材料滚出来（这一段就是旧实现缺的）。
	if !anyMaterial(rec, fixDraftMark) {
		t.Fatalf("起草正文没有作为中间材料下发（onDelta 大概又被传成 nil 了）。\n%s\n材料帧：\n%s",
			whyDraftMaterialMatters(), dumpMaterials(t, rec))
	}

	// ④ 交付语义：起草时**不能**把正文写进正文区，正文只能出现在「交付」之后。
	// 否则用户会看到同一篇稿子先刷一遍草稿、再刷一遍终稿，以为生成了两篇。
	deliverIdx := firstTraceWithLabel(rec, "交付")
	if deliverIdx < 0 {
		t.Fatalf("时间线里没有「交付」这一格：%s", dumpLabels(t, rec))
	}
	bodyIdx := firstDeltaWith(rec, fixDraftMark)
	if bodyIdx < 0 {
		t.Fatal("正文区里没收到终稿 —— 交付这一段没把稿子发出去")
	}
	if bodyIdx < deliverIdx {
		t.Fatalf("正文在「交付」之前就刷进正文区了（第 %d 帧 vs 交付第 %d 帧）：会把同一篇稿子刷成两遍，用户以为生成了两篇",
			bodyIdx, deliverIdx)
	}
}

// docgen 那一跳的「中间材料」回归防线。
//
// 用户的原话是「速度过于慢了，中间可以流式输出思考的一些中间材料，现在一直卡着计时」。
// 这条链路（news→word 走的就是它）在 GenerateDoc 期间不发正文、不发文件，只发心跳：
// 线上实测这一跳裸跑 16.8 秒（整轮 23.8s），屏幕上只有「正在生成…（已用 6s/9s/12s）」。
//
// 模型侧的材料要等它开始吐 JSON 才有（contentSink 从流式 JSON 抽正文，见
// agent/progress_wiring_test.go），所以这条尺子守的是**首片正文到达之前**：
// 必须有本地事实顶上，且它得排在 file 帧前面。
func TestDocGenEmitsAssemblyNoteBeforeFile(t *testing.T) {
	f := newFakeLLM(t, func(system, user string) string {
		switch {
		case strings.Contains(system, "多智能体管线的调度器"):
			// 要出文件 + 命中 write 型技能 → 走 chat.go 的纠偏闸门进 docgen 分支。
			return `{"intent":"docgen","action":"gen","skill_slug":"技能工厂",` +
				`"reason":"用户想下载文件","needs_tools":false}`
		case strings.Contains(system, "办公文档管家"):
			return `{"format":"word","filename":"园区开放日通知.docx","title":"园区开放日通知",` +
				`"parags":["各部门：","定于本周五举办园区开放日活动，请提前安排。"]}`
		}
		return ""
	})
	h := &chatHandler{eng: newManualEngine(t, t.TempDir(), f), maxRound: 1, gen: newGenCache()}
	// 材料按 400ms 节流下发（materialThrottle）：假模型是瞬回的，分类那片材料
	// （「用户想下载文件」）刚发完，紧接着本地事实就落进窗口被吃掉——真实一轮里
	// 分类要跑 ~5s，这两片差得远。把分片间隔拉过窗口，测的才是「本地事实有没有
	// 顶上去」，而不是「节流器有没有恰好放行」。
	f.chunkGap = 450 * time.Millisecond
	sse := runRouteChat(t, h, `{"session_id":"s-docnote","message":"把下面这份内容生成一份 Word 文档","mode":"auto"}`)
	frames := routeFrames(sse)

	// 前提：这一轮真的走到了发文（否则「材料在 file 帧之前」是没有意义的空跑绿）。
	fileIdx := frameIndexData(frames, evFile, ".docx")
	if fileIdx < 0 {
		t.Fatalf("本轮没产出 docx 交付帧，这条尺子的前提不成立（不许空跑判绿）。\n---- 实际 SSE ----\n%s", sse)
	}
	noteIdx := traceMaterialIndex(frames, "已装配")
	if noteIdx < 0 {
		t.Fatalf("docgen 这一跳没有本地事实材料：GenerateDoc 期间屏幕上只有计时器在跳。\n材料帧：\n%s",
			dumpTraceMaterials(frames))
	}
	if noteIdx > fileIdx {
		t.Fatalf("材料排在了 file 帧之后（材料第 %d 帧 vs 文件第 %d 帧）—— 阻塞期间用户还是只能看计时器。\n材料帧：\n%s",
			noteIdx, fileIdx, dumpTraceMaterials(frames))
	}
	// 尾巴要说清这一跳在做什么：写「正在起草…」会让用户去找一篇不存在的正文。
	if !strings.Contains(frames[noteIdx].data, "正在生成文档…") {
		t.Fatalf("docgen 这一跳的尾巴没说清在生成文档（第 %d 帧）：%s", noteIdx, frames[noteIdx].data)
	}
}

// frameIndexData 返回**第一个**该事件且 data 含 marker 的帧下标（-1 = 没有）。
func frameIndexData(frames []routeFrame, event, marker string) int {
	for i, fr := range frames {
		if fr.event == event && strings.Contains(fr.data, marker) {
			return i
		}
	}
	return -1
}

// traceMaterialIndex 返回**第一个**材料里含 marker 的 trace 帧下标（-1 = 没有）。
func traceMaterialIndex(frames []routeFrame, marker string) int {
	for i, fr := range frames {
		if fr.event != evTrace || !strings.Contains(fr.data, marker) {
			continue
		}
		var st []agent.TraceStep
		if json.Unmarshal([]byte(fr.data), &st) != nil {
			continue
		}
		for _, s := range st {
			if strings.Contains(s.Material, marker) {
				return i
			}
		}
	}
	return -1
}

func dumpTraceMaterials(frames []routeFrame) string {
	var sb strings.Builder
	for _, fr := range frames {
		if fr.event != evTrace {
			continue
		}
		var st []agent.TraceStep
		if json.Unmarshal([]byte(fr.data), &st) != nil {
			continue
		}
		for _, s := range st {
			if s.Material != "" {
				sb.WriteString("- [" + s.Label + "] " + s.Material + "\n")
			}
		}
	}
	if sb.Len() == 0 {
		return "（一帧材料都没有）"
	}
	return sb.String()
}

// anyMaterial 在**所有** trace 帧里找材料子串（不只看最后一帧）。
// 材料是尾部滚动的：后面的步骤（审稿/交付）会把它顶出去，只看最后一帧会假红。
// 这里不走 rec.traces(t)（它在 JSON 坏掉时会 t.Fatalf），因为断言路径上不该
// 因为「另一帧解析失败」把材料这条断言判死——那是两件事。
func anyMaterial(rec *frameRec, want string) bool {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for i, ev := range rec.evs {
		if ev != evTrace {
			continue
		}
		var st []agent.TraceStep
		if json.Unmarshal([]byte(rec.datas[i]), &st) != nil {
			continue
		}
		for _, s := range st {
			if strings.Contains(s.Material, want) {
				return true
			}
		}
	}
	return false
}

// firstTraceWithLabel 返回第一帧「出现该标签步骤」的帧下标（-1 = 没出现）。
func firstTraceWithLabel(rec *frameRec, label string) int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for i, ev := range rec.evs {
		if ev != evTrace {
			continue
		}
		var st []agent.TraceStep
		if json.Unmarshal([]byte(rec.datas[i]), &st) != nil {
			continue
		}
		for _, s := range st {
			if s.Label == label {
				return i
			}
		}
	}
	return -1
}

// firstDeltaWith 返回第一帧「正文流里有该子串」的帧下标（-1 = 没出现）。
func firstDeltaWith(rec *frameRec, want string) int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for i, ev := range rec.evs {
		if ev != evDelta {
			continue
		}
		var d struct {
			T string `json:"t"`
		}
		if json.Unmarshal([]byte(rec.datas[i]), &d) != nil {
			continue
		}
		if strings.Contains(d.T, want) {
			return i
		}
	}
	return -1
}

func dumpMaterials(t *testing.T, rec *frameRec) string {
	t.Helper()
	var sb strings.Builder
	for n, frames := range rec.traces(t) {
		for _, s := range frames {
			if s.Material != "" {
				sb.WriteString("- [" + s.Label + "] " + s.Material + "\n")
			}
		}
		if n > 40 { // 帧很多时不刷屏，前面几十帧足够定位
			break
		}
	}
	if sb.Len() == 0 {
		return "（一帧材料都没有）"
	}
	return sb.String()
}

func dumpLabels(t *testing.T, rec *frameRec) string {
	t.Helper()
	seen := map[string]bool{}
	var out []string
	for _, frames := range rec.traces(t) {
		for _, s := range frames {
			if !seen[s.Label] {
				seen[s.Label] = true
				out = append(out, s.Label)
			}
		}
	}
	return strings.Join(out, " → ")
}

func whyDraftMaterialMatters() string {
	return "这一跳是整条链路最长的阻塞调用，材料挂了才有东西在动；旧实现（onDelta=nil）这几十秒里屏幕上只有计时器在跳。"
}
