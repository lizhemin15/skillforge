package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/model"
)

// 守 chat.go 里那道「出文件意图纠偏闸门」。
//
// 为什么需要：分类器会给出「intent=docgen（本轮要出文件）却命中 write 型技能」这种
// 自相矛盾的组合（技能名里带「公文/通知/报告」的写作技能最容易被挑走），而发文分支
// 只在**命中技能自己就是 docgen 型**时才会进 —— 于是这一轮**静默降级成写正文**：
// 用户要 Word，拿到一屏文字，没有文件、没有报错、也没有一句说明。
// 这是真故障（线上 acceptance 的 docgen 腿就是这么红的），不是审美问题。
//
// 断言分三层，缺一层都挡不住：
//
//	A1 真有 file 帧 —— 只测「有没有那句说明」的话，说明打了但文件仍没出来，
//	   用户照样拿不到 Word。
//	A2 发文请求真发给了带契约的生成技能（提示词里带 JSON 规格的那类）——
//	   拿写作技能的提示词去跑发文，最后必然 parseDocJSON 失败，所以这条守住
//	   「纠偏不是随便抓个技能凑数」。
//	A3 换技能对用户可见（meta.note 点名旧技能 + 讲原因）—— 闷声改路由等于骗人。
func TestRouteFileIntentCoercesWriteSkillToDocGen(t *testing.T) {
	f := newFakeLLM(t, func(system, user string) string {
		switch {
		case strings.Contains(system, "多智能体管线的调度器"):
			// 自相矛盾的组合：要出文件，却命中 write 型技能。
			return `{"intent":"docgen","action":"gen","skill_slug":"技能工厂",` +
				`"reason":"用户想下载文件","needs_tools":false}`
		// ⚠️ 锚点是真提示词里的自称（「办公文档管家」），不是 "DOCJSON" ——
		// 那个词只在 seed.go 的注释里存在，照着它写会永远匹配不上（本文件就被咬过一次）。
		case strings.Contains(system, "办公文档管家"):
			return `{"format":"word","filename":"园区开放日通知.docx","title":"园区开放日通知",` +
				`"parags":["各部门：","定于本周五举办园区开放日活动，请提前安排。"]}`
		}
		return ""
	})
	// gen 必须给：发文路径生成完要把字节塞进交付缓存（chat.go:262 genCache.put），
	// 生产里是 router.go 装的；测试里漏了就是 nil map 写，直接 panic。
	h := &chatHandler{eng: newManualEngine(t, t.TempDir(), f), maxRound: 1, gen: newGenCache()}

	sse := runRouteChat(t, h, `{"session_id":"s-file","message":"把下面这份内容生成一份 Word 文档","mode":"auto"}`)
	frames := routeFrames(sse)

	// A1：出文件了。
	fileData := findFrameData(frames, evFile)
	if fileData == "" {
		t.Fatalf("要 Word 却只拿到文字：SSE 里没有 file 帧（静默降级成写正文）。\n---- 实际 SSE ----\n%s", sse)
	}
	if !strings.Contains(fileData, ".docx") {
		t.Fatalf("file 帧不是 docx 交付物：%s", fileData)
	}

	// A2：发文请求发给了带 DOCJSON 契约的技能。
	if !anyRequestSystemContains(f, "办公文档管家") {
		t.Fatalf("纠偏后没有把请求发给带 DOCJSON 契约的生成技能 —— " +
			"拿写作技能的提示词去发文，parseDocJSON 必失败（等于换个姿势再坏一次）")
	}

	// A3：换技能可见。
	metaData := findFrameData(frames, evMeta)
	if !strings.Contains(metaData, "技能工厂") {
		t.Fatalf("换技能没点名被换下的技能，用户不知道自己锁的技能被绕过了：%s", metaData)
	}
	// ⚠️ 别锚具体措辞（初版锚字面词「文档生成」，真答案写的是「已改用「办公文档管家」生成」，
	// 于是尺子把好人当坏人拦下）。要的是三件语义：点名换上的技能 + 给了因果。
	if !strings.Contains(metaData, "办公文档管家") {
		t.Fatalf("换技能没点名换成谁，用户不知道实际是谁在干活：%s", metaData)
	}
	if !containsAny(metaData, "而不是", "无法", "不能", "只能", "不适合") {
		t.Fatalf("换技能没讲清为什么换（用户要的是「为什么」，不是「skill_type=docgen」）：%s", metaData)
	}

	// 路由帧要落在纠偏后的技能上，且类型读得出是文档生成。
	if sk := findFrameData(frames, evSkill); !strings.Contains(sk, model.SkillTypeDocGen) {
		t.Fatalf("skill 帧没报纠偏后的 docgen 类型：%s", sk)
	}
}

// 反向尺子：闸门开错方向同样是故障 —— 用户要写文章，却被塞一份文件下载。
// 这条同时守住「补上纠偏」时别顺手把 write 路径也改掉。
//
// 带前提断言（防「空跑绿」）：必须证明这一轮真的跑过了分类、走到了生成，
// 否则「没有 file 帧」可能只是因为整条链路在更早的地方就断了。
func TestRouteWriteIntentNotCoercedToDocGen(t *testing.T) {
	f := newFakeLLM(t, func(system, user string) string {
		switch {
		case strings.Contains(system, "多智能体管线的调度器"):
			return `{"intent":"write","action":"write","skill_slug":"技能工厂",` +
				`"reason":"用户要写一份东西","needs_tools":false}`
		}
		return "这是一段正文。"
	})
	h := &chatHandler{eng: newManualEngine(t, t.TempDir(), f), maxRound: 1}

	sse := runRouteChat(t, h, `{"session_id":"s-write","message":"帮我写一段园区开放日的宣传文案","mode":"auto"}`)

	// 前提：真的走到了生成（有一次非分类请求发出）。不成立就不许判绿。
	if !anyRequestNotSystem(f, "多智能体管线的调度器") {
		t.Fatalf("本轮没走到生成，这条尺子的前提不成立（不许空跑判绿）。\n---- 实际 SSE ----\n%s", sse)
	}
	if strings.Contains(sse, "已改用") {
		t.Fatalf("write 意图被纠偏了：用户要文章却拿到文件下载。\n---- 实际 SSE ----\n%s", sse)
	}
	if d := findFrameData(routeFrames(sse), evFile); d != "" {
		t.Fatalf("write 意图不该产出文件交付帧：%s", d)
	}
}

// 反向的第三个方向：用户**明说**只要正文，却被塞一个文件下载。
//
// 这不是假想：2026-09-17 真浏览器实测线上，「写一份关于开展数据治理专项工作的通知…
// 正文不少于 600 字，直接输出正文」被分类器按题材判成 intent=docgen，纠偏闸门当场
// 把这一轮抢去生成 .docx —— 用户明说了要正文，屏幕上只有一个文件卡片，正文 94 字。
// 所以闸门不能只看 intent，得给「明说的交付形态」让路（agent.ExplicitTextOnly）。
//
// 这条断言同时也守反向故障：把闸门整个删掉能过这条，但 TestRouteFileIntentCoerces…
// 会红 —— 两条合起来才是完整的闸门契约。
func TestRouteExplicitTextOnlyNotCoerced(t *testing.T) {
	// 正文契约写在变量里：既喂给假模型，又当断言锚 —— 避免「两边各写一份、改一边就假绿」。
	want := "各部门：\n为规范数据治理专项工作，现就有关事项通知如下：一、建立统一台账。"
	f := newFakeLLM(t, func(system, user string) string {
		switch {
		case strings.Contains(system, "多智能体管线的调度器"):
			// 分类器按题材误判：标题像公文 → 判成本轮要出文件。
			return `{"intent":"docgen","action":"gen","skill_slug":"技能工厂",` +
				`"reason":"题材像公文，本轮出文件","needs_tools":false}`
		}
		return want
	})
	// gen 必须给：闸门若失效会走发文路径，末尾要往交付缓存塞字节（chat.go genCache.put），
	// nil map 写会直接 panic —— 注入自证时那就成了「红在崩溃上」，不算断言有效。
	h := &chatHandler{eng: newManualEngine(t, t.TempDir(), f), maxRound: 1, gen: newGenCache()}

	msg := `写一份关于开展数据治理专项工作的通知。背景与核心素材：已建成数据中台，覆盖 12 个业务域。` +
		`正文不少于 600 字，直接输出正文。`
	sse := runRouteChat(t, h, `{"session_id":"s-textonly","message":"`+msg+`","mode":"auto"}`)
	frames := routeFrames(sse)

	// 前提：真走到了生成（有一次非分类请求发出）。不成立就不许判绿。
	if !anyRequestNotSystem(f, "多智能体管线的调度器") {
		t.Fatalf("本轮没走到生成，这条尺子的前提不成立（不许空跑判绿）。\n---- 实际 SSE ----\n%s", sse)
	}
	// A1：明说了只看正文，就不许产出文件交付帧。
	if d := findFrameData(routeFrames(sse), evFile); d != "" {
		t.Fatalf("用户明说了「直接输出正文」，却拿到文件下载卡片（文件=%s）—— "+
			"交付形态是用户亲手写的判据，不该被题材判断盖掉", d)
	}
	// A2：也不许打「已改用」的换技能说明 —— 明说了要正文时换技能本身就是误解。
	if strings.Contains(sse, "已改用") {
		t.Fatalf("这一轮不该纠偏（用户只要正文），却打了换技能说明。\n---- 实际 SSE ----\n%s", sse)
	}
	// A3：正文真的写出来了。少了这条，「整轮什么都没发生」也会被判绿。
	//
	// ⚠️ 必须拿**拼回来的正文**断言，不能拿原始 SSE 直接 Contains：流式正文是按增量切帧发的
	// （实测一轮里「…现」和「就有关事项通知如下…」分在两帧），跨帧断句会让这条假红 ——
	// 那是尺子自己坏，不是交付坏了（本条断言第一次跑就是这么红的）。
	body := deltaText(frames)
	if !strings.Contains(body, want) {
		t.Fatalf("没有 file 帧 ≠ 走对了路：拼回来的正文里没有交付文字，这一轮等于什么都没交付。\n"+
			"---- 拼回的正文（%d 字）----\n%s\n---- 实际 SSE ----\n%s", len([]rune(body)), body, sse)
	}
}

// deltaText 把整轮的 delta 帧拼回正文（客户端就是这么拼的）。
// 流式正文按增量切帧，跨帧断句让「拿原始 SSE 搜连续串」变成假红，故统一走这里。
func deltaText(frames []routeFrame) string {
	var b strings.Builder
	for _, fr := range frames {
		if fr.event != evDelta {
			continue
		}
		var d struct {
			T string `json:"t"`
		}
		if json.Unmarshal([]byte(fr.data), &d) == nil {
			b.WriteString(d.T)
		}
	}
	return b.String()
}

// containsAny 命中任一即可 —— 用来断言「讲清了因果」，而不是断言某个具体措辞。
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// runRouteChat 同步跑一次 /api/chat，返回整段 SSE 原文。
//
// 为什么能同步跑：这里的假模型立刻回话，handler 会把整轮走完再 return。
// （chat_seed_frame_test.go 那套 channel + goroutine 是为了「只取 t≈0 第一帧、
// 不等模型」，本文件要的是**整轮**的帧，同步收全比抢第一帧更合适。）
func runRouteChat(t *testing.T, h *chatHandler, body string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	sse := rec.Body.String()
	if strings.TrimSpace(sse) == "" {
		t.Fatalf("整轮一个 SSE 帧都没有 —— 请求根本没被处理（body=%s）", body)
	}
	return sse
}

type routeFrame struct{ event, data string }

// routeFrames 把 SSE 原文切成帧（event: / data: 成对出现，空行分隔）。
func routeFrames(sse string) []routeFrame {
	var out []routeFrame
	for _, block := range strings.Split(sse, "\n\n") {
		var fr routeFrame
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event:"):
				fr.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				fr.data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		if fr.event != "" {
			out = append(out, fr)
		}
	}
	return out
}

// findFrameData 返回**最后一个**该事件帧的 data。
// 取最后一个而不是第一个：纠偏那条 meta 帧是整轮里第二只 meta（骨架先发过一只），
// 只认第一只会让这条断言永远看着「没打上」——实测踩过。
func findFrameData(frames []routeFrame, event string) string {
	out := ""
	for _, fr := range frames {
		if fr.event == event {
			out = fr.data
		}
	}
	return out
}

func anyRequestSystemContains(f *fakeLLM, marker string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.reqs {
		if strings.Contains(r.System, marker) {
			return true
		}
	}
	return false
}

func anyRequestNotSystem(f *fakeLLM, marker string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.reqs {
		if !strings.Contains(r.System, marker) {
			return true
		}
	}
	return false
}
