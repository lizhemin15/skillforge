package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lizhemin15/skillforge/internal/agent"
	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/model"
)

// 这一层测的是**接线**，不是判据本身（判据在 internal/agent/needs_gate_test.go）。
//
// 为什么必须有这一层：MaterialNoAsk 写得再对，调用处漏了 `&& agent.MaterialNoAsk(...)`
// 这个条件，线上行为一点都不会变。判据单测会全绿，用户投诉照旧 —— 这种「尺子绿、产品
// 病」的缺口只能靠一条走真 ServeHTTP 的用例封住。
//
// 用例是**用户投诉的回放**：贴一万字素材 + 提写作需求，第 2 轮整轮 2.5 秒结束，只回
// 一句「我需要你补充以下信息：标题亮点」，正文一个字没有。真因不是上下文丢了（素材
// 确实在模型手里），而是 needs 由分类器自报、同一输入连发 6 次报的是 0/1/3/4/6。

// classifyNeedsTitlePoint 是分类器照技能清单回填的判定：技能声明的必填参数
// 「标题亮点」没被显式点名，于是它报了一条 needs。required=true 让 SplitNeeds 判成
// blocking（技能声明的 input_params 也是必填，两边一致）。
const classifyNeedsTitlePoint = `{"skill_slug":"gongchang-news","intent":"写公司新闻通稿",` +
	`"reason":"命中公司新闻通稿技能","action":"write","params":{},` +
	`"needs":[{"name":"title_point","label":"标题亮点","required":true}],` +
	`"steps":[{"phase":"analyze","label":"① 意图分析"}]}`

// newNeedsGateHandler 建一只回复恒为 classifyNeedsTitlePoint 的 chatHandler，技能库里
// 自建一个**真的声明了必填参数**的技能。
//
// 为什么必须自建：内置技能一个必填参数都没有（seed.go 里写明了 docgen 类「永不拦」），
// 拿内置技能测，SplitNeeds 永远返回 blocking 为空 —— 用例会绿，但它绿在「根本没走到
// 闸门」上，等于没测。
//
// 必须走 CreateSkill 而不是 Create：只写库不写盘，引擎读 system_prompt.md 会失败，
// 整轮以一个 error 帧收场（第一版就这么红过：连 done 帧都等不到）。写盘这步在闸门
// **之前**，漏了就测不到闸门。
func newNeedsGateHandler(t *testing.T) (*chatHandler, *fakeLLM) {
	t.Helper()
	return newNeedsGateHandlerWith(t, func(usr string) string {
		return doubtsJSON("ambiguity", verbatimQuote(usr, 8), "按素材里的项目名与投产时间写", "主题口径")
	})
}

// newNeedsGateHandlerWith 同 newNeedsGateHandler，但疑点跳的回执由用例指定：
// doubtsReply 收到的是**疑点跳真正的 user 提示原文**，用例据此抠引用（而不是写死
// 一个常量再断言常量）—— 这样「引用必须逐字命中原文」这条判据才真的被走到。
//
// 跳感知靠 system 提示里的标记：疑点跳的 system 是 agent 的 doubtsSys（含「对齐疑点」），
// 其余跳仍回分类器的 JSON。为什么必须跳感知：旧版用例让所有跳回同一个常量，改了停问
// 分支之后它会「因为疑点跳解析不出疑点」而静默改走直写 —— 用例仍会红，但红在错的
// 原因上（分不清是接线断了还是假回执没造好）。
func newNeedsGateHandlerWith(t *testing.T, doubtsReply func(usr string) string) (*chatHandler, *fakeLLM) {
	t.Helper()
	sk := newStoreForTest(t, t.TempDir())
	if err := sk.CreateSkill(
		&model.Skill{
			Slug: "gongchang-news", Name: "公司新闻通稿", Description: "按素材写一篇公司新闻通稿",
			Category: "通用", Version: 1, Enabled: true, SkillType: model.SkillTypeWrite,
		},
		[]model.Param{{Name: "title_point", Label: "标题亮点", Type: "text", Required: true}},
		"# 公司新闻通稿\n\n按素材写，倒金字塔结构，标题不超过 20 字，不喊口号。",
	); err != nil {
		t.Fatalf("建测试技能失败（闸门的判据是技能声明的 input_params，没有它测不到闸门）：%v", err)
	}
	f := newFakeLLM(t, func(system, user string) string {
		if strings.Contains(system, doubtsHopMarker) {
			return doubtsReply(user)
		}
		return classifyNeedsTitlePoint
	})
	cli := llm.New(&model.LLMConfig{APIKey: "test", BaseURL: f.srv.URL, Model: "fake"})
	return &chatHandler{eng: agent.New(cli, sk), maxRound: 1, gen: newGenCache()}, f
}

// doubtsHopMarker 出现在 agent 疑点跳的 system 提示里（「你是『对齐疑点』裁判」）。
const doubtsHopMarker = "对齐疑点"

// verbatimQuote 从疑点跳的 user 提示里抠出「用户这次给的原话」，取前 n 字。
// 这是引用校验的地面真值：引用合法与否，只能由这条路径决定。
func verbatimQuote(usr string, n int) string {
	const mark = "## 用户这次给的原话"
	i := strings.Index(usr, mark)
	if i < 0 {
		return ""
	}
	rest := strings.TrimLeft(usr[i+len(mark):], "\n")
	if j := strings.Index(rest, "\n## "); j >= 0 {
		rest = rest[:j]
	}
	return headRunes(strings.TrimSpace(rest), n)
}

// headRunes 取前 n 个字符（rune，不是字节 —— 中文按字节切会切出半个字）。
func headRunes(s string, n int) string {
	r := []rune(s)
	if len(r) < n {
		return s
	}
	return string(r[:n])
}

// doubtsJSON 拼疑点跳的 JSON 回执（一条疑点）。
func doubtsJSON(kind, quote, infer, impact string) string {
	b, _ := json.Marshal(map[string]any{"doubts": []map[string]string{
		{"kind": kind, "quote": quote, "inference": infer, "impact": impact},
	}})
	return string(b)
}

// doubtsHops 数一数这一轮疑点跳被调了几次（跳过=接线断了，用例必须能区分）。
func doubtsHops(f *fakeLLM) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.reqs {
		if strings.Contains(r.System, doubtsHopMarker) {
			n++
		}
	}
	return n
}

// eventWriter 是**保住事件名**的 SSE 收集器。
//
// 为什么不用现成的 frameWriter：它只把 data: 那一行塞进通道（事件名在 frameData 里被
// 丢掉）。本层用例的核心断言恰恰是「这一轮到底有没有下发 needs 事件」—— 只看 data
// 分不清 `event: needs / data: [...]` 和 `event: trace / data: {"steps":...needs...}`，
// 那样写出来的断言是假绿。
type eventWriter struct {
	mu     sync.Mutex
	hdr    http.Header
	code   int
	buf    strings.Builder
	frames chan string
}

func newEventWriter() *eventWriter {
	return &eventWriter{hdr: http.Header{}, frames: make(chan string, 512)}
}

func (f *eventWriter) Header() http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hdr
}

func (f *eventWriter) WriteHeader(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.code = code
}

func (f *eventWriter) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buf.Write(b)
	// 一次 Write 可能带多帧，一帧也可能被拆成多次 Write。
	for {
		s := f.buf.String()
		i := strings.Index(s, "\n\n")
		if i < 0 {
			break
		}
		frame := s[:i]
		f.buf.Reset()
		f.buf.WriteString(s[i+2:])
		select {
		case f.frames <- frame:
		default: // 缓冲满就丢；用例只读到 done 帧为止，够用
		}
	}
	return len(b), nil
}

func (f *eventWriter) Flush() {}

// collectChat 发一轮聊天请求，收帧收到 done 帧（或 12 秒超时）为止，返回全部原始帧。
//
// 收到 done 就返回：闸门那条路的证据全在 done 之前（needs 事件 / 中间材料 / asked 标记），
// 而 done 之后 handler 只剩落盘收尾。
func collectChat(t *testing.T, h *chatHandler, body string) []string {
	t.Helper()
	fw := newEventWriter()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	req = req.WithContext(ctx)

	// handler 会往 dataDir/sessions 落盘。测试一返回，t.TempDir() 的清理就开始删树，
	// 撞上正在写的文件 → 清理失败红（红在 testing.go 的清理代码里，看不出跟被测逻辑
	// 有关）。所以必须等它退场；带超时是为了别把整个包挂死。
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(fw, req) }()
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Errorf("handler goroutine 3 秒内没退出：TempDir 清理会因此 flake")
		}
	})

	var frames []string
	deadline := time.After(12 * time.Second)
	for {
		select {
		case fr := <-fw.frames:
			frames = append(frames, fr)
			if strings.HasPrefix(fr, "event: done") {
				return frames
			}
		case <-deadline:
			t.Fatalf("12 秒内没等到 done 帧，只收到 %d 帧：\n%s", len(frames), strings.Join(frames, "\n---\n"))
			return frames
		}
	}
}

func joinedFrames(frames []string) string { return strings.Join(frames, "\n") }

// hasEvent 判是否下发过某个 SSE 事件（按帧首行的事件名判，不看 data 内容）。
func hasEvent(frames []string, ev string) bool {
	prefix := "event: " + ev
	for _, fr := range frames {
		if strings.HasPrefix(fr, prefix) {
			return true
		}
	}
	return false
}

// chatBody 拼一个 /api/chat 请求体（JSON 转义交给 encoding/json，别手拼）。
func chatBody(t *testing.T, sid, msg string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"session_id": sid, "message": msg, "mode": "auto"})
	if err != nil {
		t.Fatalf("拼请求体失败：%v", err)
	}
	return string(b)
}

// materialTurns 造一段「像投喂素材」的一万字级材料。
//
// 为什么非得这么长：门槛是 800 字，短材料测不到闸门（会掉回「拦」那一侧，用例绿在
// 别的原因上）。这里按线上素材的量级造（实测 10340~10432 字）。
//
// ⚠️ 计数必须按**字符**（rune）不能按 sb.Len()（字节）：中文一段 3 字节，按字节攒到
// 9000 只有 3000 来字，门槛够不到 —— 测试自己的前提断言会当场把它揪出来（第一版
// 就是这么被拦下的：3736 字 < 9000）。
func materialTurns(n int) string {
	para := "8 月 26 日，华东制造基地二期项目正式投产，园区管委会副主任到场致辞，项目总投资 3.2 亿元，" +
		"预计年产精密部件 120 万件，新增就业岗位 260 个。"
	var sb strings.Builder
	for len([]rune(sb.String())) < n {
		sb.WriteString(para)
	}
	return sb.String()
}

// grayMaterial 把一句话需求包成「灰区素材轮」：字数落在 [80,800)。
//
// 为什么必须落在灰区（2026-09-26 起）：材料真空（<80 字）这一格已经被**零材料强制门**
// 接管 —— 它不进疑点跳，本地算缺口直接停问（内网慢 token，省一次模型调用）。疑点回执
// 那条契约只有灰区才走得到：真空区走不到、材料在手（>=800）又会被放过去。
//
// 自带前提断言：谁动了 80/800 这两条线（判据在 internal/agent/needs_gate.go），这条
// 用例当场红在「前提不成立」上，而不是红在一个看不懂的断言里。
func grayMaterial(t *testing.T, msg string) string {
	t.Helper()
	full := msg + "\n\n素材：\n" + materialTurns(300)
	if n := len([]rune(full)); n < 80 || n >= 800 {
		t.Fatalf("测试前提不成立：灰区素材 %d 字，必须落在 [80,800)：否则这条用例测的不是疑点跳", n)
	}
	return full
}

// TestNeedsGateAnchoredDoubtStillAsks：有真疑点时闸门照旧拦，但**问法换成锚定原文**。
//
// 这条替代了旧的 TestNeedsGateShortMessageStillAsks。旧用例断言的是「照技能清单拼出
// 通用要素清单」（"我需要你补充以下信息：标题亮点"）—— 那正是用户投诉的东西：
// 「要的时候也不是根据我目前提供的信息的基础上来进一步补充，而是直接通用的补充」。
// 契约变了，守卫必须跟着变；留着旧断言等于留一条「改回旧行为会绿」的假守卫。
//
// 反向自证仍在（防「一改就改成一律不拦」）：有疑点就必须拦、必须 asked=true。
func TestNeedsGateAnchoredDoubtStillAsks(t *testing.T) {
	h, f := newNeedsGateHandler(t)
	const msg = "帮我写一篇公司新闻通稿"
	gmsg := grayMaterial(t, msg) // 灰区：疑点跳这条契约只有灰区才走得到（见 grayMaterial）
	frames := collectChat(t, h, chatBody(t, "s-needs-anchored", gmsg))
	all := joinedFrames(frames)

	if !hasEvent(frames, "needs") {
		t.Fatalf("有真疑点时闸门没拦，缺 needs 事件（改成一律不拦了？）：\n%s", all)
	}
	if !strings.Contains(all, `"asked":"true"`) {
		t.Fatalf("拦下了但没标 asked=true，前端会读成「还在跑」：\n%s", all)
	}
	// 接线证据：疑点跳真的被调了。没有这条，上面的断言也可能绿在别的路上。
	if n := doubtsHops(f); n == 0 {
		t.Fatalf("疑点跳一次都没调 —— 停问走的是别的路，用例绿在错的原因上：\n%s", all)
	}
	// 锚定原文：引用必须是用户这次真的说过的话（引用取自真提示，不是常量）。
	quote := headRunes(gmsg, 8)
	if quote == "" {
		t.Fatalf("测试前提不成立：没能从疑点跳提示里抠出用户原话")
	}
	if !strings.Contains(all, "「"+quote+"」") {
		t.Fatalf("停问文案里没有逐字引用用户原话 %q（又退回通用追问了？）：\n%s", quote, all)
	}
	// 旧病复发的直接特征串：通用要素清单。
	if strings.Contains(all, "我需要你补充以下信息") {
		t.Fatalf("停问又用回了通用要素清单（用户投诉的原话就是这个）：\n%s", all)
	}
	// 一句话可确认的出口：用户不想逐条答，回一句就能开工。
	if !strings.Contains(all, "就按你的") {
		t.Fatalf("停问没给出「回一句就继续」的出口，等于逼用户填表：\n%s", all)
	}
}

// TestNeedsGateFabricatedQuoteDoesNotAsk：模型编了一条原文里找不到的引用时，
// **不许**拿它去拦用户 —— 这条引用会在原文里找不着，比不问更糟。
// 拦不住就带假设直写，且必须把假设说出来（可感知、可纠正）。
func TestNeedsGateFabricatedQuoteDoesNotAsk(t *testing.T) {
	h, f := newNeedsGateHandlerWith(t, func(string) string {
		// 引用逐字写在原文里根本不存在的句子（"客户要求" 从未出现）
		return doubtsJSON("ambiguity", "客户要求必须当天发布且署名", "按当天发布处理", "发布节奏")
	})
	frames := collectChat(t, h, chatBody(t, "s-needs-fabricated", grayMaterial(t, "帮我写一篇公司新闻通稿")))
	all := joinedFrames(frames)

	if n := doubtsHops(f); n == 0 {
		t.Fatalf("疑点跳没被调用，这条用例测不到校验逻辑：\n%s", all)
	}
	if hasEvent(frames, "needs") {
		t.Fatalf("引用对不上原文却还是拦下了用户（用户会去原文里找这句话，找不到）：\n%s", all)
	}
	if !strings.Contains(all, "无疑点，开始写") {
		t.Fatalf("没拦但也没说「无疑点，开始写」，用户只会觉得它自作主张：\n%s", all)
	}
	if !strings.Contains(all, "标题亮点") {
		t.Fatalf("假设里没点名是哪一项（用户无法纠正）：\n%s", all)
	}
	if strings.Contains(all, "客户要求必须当天发布且署名") {
		t.Fatalf("把对不上原文的引用展示给用户了：\n%s", all)
	}
}

// TestNeedsGateGarbageDoubtsStillWrites：疑点跳输出不是 JSON（模型抽风/被截断）时，
// 绝不退回通用清单，直接带假设写 —— 「坏输出」是危害不对称里最该放过的一侧。
func TestNeedsGateGarbageDoubtsStillWrites(t *testing.T) {
	h, f := newNeedsGateHandlerWith(t, func(string) string {
		return "嗯，这个我看看……还是直接写吧"
	})
	frames := collectChat(t, h, chatBody(t, "s-needs-garbage", grayMaterial(t, "帮我写一篇公司新闻通稿")))
	all := joinedFrames(frames)

	if n := doubtsHops(f); n == 0 {
		t.Fatalf("疑点跳没被调用，这条用例测不到容错：\n%s", all)
	}
	if hasEvent(frames, "needs") {
		t.Fatalf("疑点输出坏了却拦下了用户：\n%s", all)
	}
	if !strings.Contains(all, "无疑点，开始写") {
		t.Fatalf("坏输出时既没拦也没说明，用户白等一轮：\n%s", all)
	}
	if strings.Contains(all, "我需要你补充以下信息") {
		t.Fatalf("坏输出退回了通用要素清单（最该避免的回退）：\n%s", all)
	}
	// 真的继续写了：必须有正文流出来，不能「不拦也不写」。
	if !hasEvent(frames, "delta") {
		t.Fatalf("没拦但也没流正文，整轮空转：\n%s", all)
	}
}

// TestNeedsGateLongMaterialDoesNotAsk 是**用户投诉的回放**，也是本闸门存在的理由：
// 材料在手时不许拿必填项拦下这一轮，但必须把假设说出来。
//
// 断言三件事，缺一不可：
//
//	① 没有 needs 事件 —— 用户不会再被问一遍（投诉的直接症状）；
//	② 中间材料里出现「按素材自行推断」+ 参数名 + 真实字数千字位 —— 不是闷声放过去，
//	   假设摆在明面上（可感知、可纠正）；
//	③ 字数取自**本轮消息本身**（用例按 len([]rune(msg)) 算期望值）—— 防「把数字写死」，
//	   那个数必须是从消息里数出来的地面真值。
func TestNeedsGateLongMaterialDoesNotAsk(t *testing.T) {
	h, _ := newNeedsGateHandler(t)
	msg := "请根据以下素材写一篇公司新闻通稿。\n\n素材：\n" + materialTurns(9000)
	if n := len([]rune(msg)); n < 9000 {
		t.Fatalf("测试前提不成立：素材只有 %d 字，够不到门槛就测不到闸门", n)
	}
	frames := collectChat(t, h, chatBody(t, "s-needs-long", msg))
	all := joinedFrames(frames)

	if hasEvent(frames, "needs") {
		t.Fatalf("材料在手（%d 字）却还是拿必填项拦下了这一轮 —— 这就是用户投诉的「像没看到我给的信息」：\n%s",
			len([]rune(msg)), all)
	}
	if !strings.Contains(all, "按素材自行推断") {
		t.Fatalf("放过去了但中间材料没说明假设：既不拦、也不说，用户只会觉得它自作主张：\n%s", all)
	}
	if !strings.Contains(all, "标题亮点") {
		t.Fatalf("中间材料没点名是哪一项缺（用户无法纠正）：\n%s", all)
	}
	want := fmt.Sprintf("（%d 字）", len([]rune(msg)))
	if !strings.Contains(all, want) {
		t.Fatalf("中间材料里的字数不是从本轮消息数出来的（期望含 %q，防写死）：\n%s", want, all)
	}
}

// ---------------------------------------------------------------------------
// 零材料强制门（2026-09-26）：用户零材料发指令时技能直接编内容
// ---------------------------------------------------------------------------
//
// 这一组守的是**新缺口**，和上面三条不是同一格：
//
//	上面三条：模型自报了 needs → 闸门有反应（拦，但换锚定问法）
//	这一组：模型自报 0 条 needs（一句话指令 + 零材料，线上连发 6 次报 0/1/3/4/6 的那一端）
//	        → 旧实现里 SplitNeeds 拿到空 needs 只能回空 blocking，闸门形同不存在，
//	          写作跳直通，产出 645 字带假日期、假参会人的会议纪要 —— 用户读到的是「它替我编了」。
//
// 判据来源换成**技能声明的 input_params**（地面真值，本地算得准），代价是 0 次模型调用。

// zeroMaterialMsg 是线上那次的形状：一句话指令（14 字）+ 零材料。
const zeroMaterialMsg = "帮我写一份会议纪要"

// classifyNoNeedsJSON 拼「一条 needs 都不报」的分类器回执（缺口现场的形状）。
// 用 %q 拼 slug 而不是写死常量：技能是每个用例自建的，写死会让用例绿在「slug 对不上、
// 直接降级成通用对话」这种跟被测逻辑无关的原因上。
func classifyNoNeedsJSON(slug, action string) string {
	return fmt.Sprintf(`{"skill_slug":%q,"intent":"写会议纪要","reason":"命中会议纪要技能",`+
		`"action":%q,"params":{},"needs":[],`+
		`"steps":[{"phase":"analyze","label":"① 意图分析"}]}`, slug, action)
}

// newZeroMaterialHandler 建一只「零材料现场」的 handler：技能**真的声明了必填项**，
// 分类器却一条 needs 都不报。
//
// 为什么技能必须自己建：内置技能一个必填参数都没有（seed.go 写明 docgen 类「永不拦」），
// 拿内置技能测，缺口算出来恒为空 —— 用例会绿，但它绿在「判据根本没输入」上。
func newZeroMaterialHandler(t *testing.T) (*chatHandler, *fakeLLM) {
	t.Helper()
	return newZeroMaterialHandlerWith(t, "huiyi-jiyao", model.SkillTypeWrite, "", "write")
}

// newZeroMaterialHandlerWith 同 newZeroMaterialHandler，但技能类型 / 附件可指定
// （template_only 那条豁免要用模板技能 + 附件才测得到）。
func newZeroMaterialHandlerWith(t *testing.T, slug, skillType, attachment, action string) (*chatHandler, *fakeLLM) {
	t.Helper()
	sk := newStoreForTest(t, t.TempDir())
	if err := sk.CreateSkill(
		&model.Skill{
			Slug: slug, Name: "会议纪要", Description: "按素材写会议纪要",
			Category: "通用", Version: 1, Enabled: true,
			SkillType: skillType, Attachment: attachment,
		},
		[]model.Param{
			{Name: "subject", Label: "会议主题", Type: "text", Required: true},
			{Name: "attendees", Label: "参会人", Type: "text", Required: true},
		},
		"# 会议纪要\n\n按素材写，只写素材里出现过的事实，不补日期不补人名。",
	); err != nil {
		t.Fatalf("建测试技能失败（缺了声明的必填项就没判据可算）：%v", err)
	}
	f := newFakeLLM(t, func(system, user string) string { return classifyNoNeedsJSON(slug, action) })
	cli := llm.New(&model.LLMConfig{APIKey: "test", BaseURL: f.srv.URL, Model: "fake"})
	return &chatHandler{eng: agent.New(cli, sk), maxRound: 1, gen: newGenCache()}, f
}

// llmHops 数这一轮总共发了多少次模型调用（内网 token 慢，调用次数是硬指标）。
func llmHops(f *fakeLLM) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

// TestNeedsGateZeroMaterialAsksInsteadOfFabricating 是缺口回放 + 封口。
//
// 断言六件事，缺一不可：
//
//	① 必须有 needs 事件 —— 零材料时不许直通写作（那就会编事实）；
//	② asked=true —— 前端据此把这一轮读成「等你补信息」，不是「还在跑」；
//	③ 逐字引用用户这轮的原话 —— 「像没看到我给的信息」那个投诉的正面回应；
//	④ 点名缺的是哪几项（取自技能声明，不是模型自报）；
//	⑤ 「就按你的」出口在 —— 不逼用户填表；
//	⑥ **只发 1 次模型调用**（分类那一跳）—— 零材料停问纯本地，不额外花调用。
//	   内网模型 token 慢，新增一次调用就是用户白等十几秒，所以这条也钉死。
func TestNeedsGateZeroMaterialAsksInsteadOfFabricating(t *testing.T) {
	h, f := newZeroMaterialHandler(t)
	frames := collectChat(t, h, chatBody(t, "s-zero-material", zeroMaterialMsg))
	all := joinedFrames(frames)

	if !hasEvent(frames, "needs") {
		t.Fatalf("零材料（%d 字）+ 必填项一项没落实，却没拦 —— 这就是产出假日期假参会人的那一格：\n%s",
			len([]rune(zeroMaterialMsg)), all)
	}
	if !strings.Contains(all, `"asked":"true"`) {
		t.Fatalf("拦下了但没标 asked=true，前端会读成「还在跑」：\n%s", all)
	}
	if !strings.Contains(all, "「"+zeroMaterialMsg+"」") {
		t.Fatalf("停问没逐字引用用户这轮的话（旧病就是跟用户说了什么无关的通用清单）：\n%s", all)
	}
	for _, want := range []string{"会议主题", "参会人"} {
		if !strings.Contains(all, want) {
			t.Fatalf("缺口没点名 %q（取自技能声明表，用户不知道要补什么）：\n%s", want, all)
		}
	}
	if !strings.Contains(all, "就按你的") {
		t.Fatalf("没给一句话出口，等于逼用户填表：\n%s", all)
	}
	if strings.Contains(all, "我需要你补充以下信息") {
		t.Fatalf("又用回了被投诉的通用清单句式：\n%s", all)
	}
	// 省调用：不许进疑点跳（真空区没有可锚的素材，疑点回执的引用对不上原文）。
	if n := doubtsHops(f); n != 0 {
		t.Fatalf("零材料停问进了疑点跳（白花 %d 次模型调用）：\n%s", n, all)
	}
	if n := llmHops(f); n != 1 {
		t.Fatalf("这一轮发了 %d 次模型调用，期望 1 次（只分类）—— 内网 token 慢，多一次都是用户白等：\n%s",
			n, all)
	}
}

// TestNeedsGateZeroMaterialEscapeWordStillWrites 守住**出口必须闭得上**。
//
// 停问文案让用户回「就按你的」，那一轮消息只有 4 个字：材料依然真空、必填项依然没落实。
// 如果授权词不被认，用户会被再问一遍，永远走不到写作 —— 这一格必须自己走出来。
func TestNeedsGateZeroMaterialEscapeWordStillWrites(t *testing.T) {
	h, f := newZeroMaterialHandler(t)
	frames := collectChat(t, h, chatBody(t, "s-zero-material-escape", "就按你的"))
	all := joinedFrames(frames)

	if hasEvent(frames, "needs") {
		t.Fatalf("用户已授权（「就按你的」）却还在问，出口闭不上会问成死循环：\n%s", all)
	}
	if !hasEvent(frames, "delta") {
		t.Fatalf("授权后既没问也没写，整轮空转：\n%s", all)
	}
	// 真的走了写作跳（分类之外还有调用），不是「不拦也不写」。
	if n := llmHops(f); n < 2 {
		t.Fatalf("这一轮只发了 %d 次模型调用，没走写作跳：\n%s", n, all)
	}
}

// TestNeedsGateZeroMaterialBlankTemplateStillDelivered 守**缩打击面**那一侧：
// action=template_only（用户点名要空白模板）不许被零材料门拦成一句反问 ——
// 交付物就是那个附件本身，跟必填项有没有落实无关。
//
// 为什么必须显式测：模板交付分支在闸门**之后**（chat.go 里 template_only 那段），
// 少写一个豁免条件，「把会议纪要模板发我」就变成一句「请补充会议主题、参会人」。
func TestNeedsGateZeroMaterialBlankTemplateStillDelivered(t *testing.T) {
	const slug = "huiyi-jiyao-tpl"
	h, _ := newZeroMaterialHandlerWith(t, slug, model.SkillTypeTemplate, "meeting-minutes.docx", "template_only")
	frames := collectChat(t, h, chatBody(t, "s-zero-material-tpl", "把会议纪要的空白模板发我"))
	all := joinedFrames(frames)

	if hasEvent(frames, "needs") {
		t.Fatalf("用户点名要空白模板却被零材料门拦下（打击面扩大了）：\n%s", all)
	}
	if !hasEvent(frames, "file") {
		t.Fatalf("没下发模板文件：\n%s", all)
	}
	if !strings.Contains(all, "meeting-minutes.docx") {
		t.Fatalf("文件事件里没有附件名：\n%s", all)
	}
}

// TestNeedsGateZeroMaterialParamsGivenDoesNotAsk 反向：必填项**在指令里说清了**就不许问。
//
// 这是「扩打击面」的第二个方向：用户把要素写进一句话里（分类器也回填进 params），
// 本地算出来没有缺口 —— 一个必填项都不缺却还停问，就是回到 2026-09-22 那个投诉。
func TestNeedsGateZeroMaterialParamsGivenDoesNotAsk(t *testing.T) {
	slug := "huiyi-jiyao-given"
	sk := newStoreForTest(t, t.TempDir())
	if err := sk.CreateSkill(
		&model.Skill{Slug: slug, Name: "会议纪要", Description: "按素材写会议纪要",
			Category: "通用", Version: 1, Enabled: true, SkillType: model.SkillTypeWrite},
		[]model.Param{{Name: "subject", Label: "会议主题", Type: "text", Required: true}},
		"# 会议纪要\n\n按素材写。",
	); err != nil {
		t.Fatalf("建测试技能失败：%v", err)
	}
	// 分类器把两项都回填了（用户这一句话里确实有）。
	reply := fmt.Sprintf(`{"skill_slug":%q,"intent":"写会议纪要","reason":"命中技能","action":"write",`+
		`"params":{"subject":"Q3 复盘"},"needs":[],`+
		`"steps":[{"phase":"analyze","label":"① 意图分析"}]}`, slug)
	f := newFakeLLM(t, func(system, user string) string { return reply })
	cli := llm.New(&model.LLMConfig{APIKey: "test", BaseURL: f.srv.URL, Model: "fake"})
	h := &chatHandler{eng: agent.New(cli, sk), maxRound: 1, gen: newGenCache()}

	frames := collectChat(t, h, chatBody(t, "s-zero-material-given", "会议主题是 Q3 复盘，帮我写份纪要"))
	all := joinedFrames(frames)

	if hasEvent(frames, "needs") {
		t.Fatalf("必填项已经从 params 拿到了值，却还是停问（这就是「像没看到我给的信息」）：\n%s", all)
	}
	if !hasEvent(frames, "delta") {
		t.Fatalf("没拦也没写，整轮空转：\n%s", all)
	}
}
