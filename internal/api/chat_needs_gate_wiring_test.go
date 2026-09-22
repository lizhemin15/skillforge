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
func newNeedsGateHandler(t *testing.T) *chatHandler {
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
	f := newFakeLLM(t, func(system, user string) string { return classifyNeedsTitlePoint })
	cli := llm.New(&model.LLMConfig{APIKey: "test", BaseURL: f.srv.URL, Model: "fake"})
	return &chatHandler{eng: agent.New(cli, sk), maxRound: 1, gen: newGenCache()}
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

// TestNeedsGateShortMessageStillAsks：用户**没给材料**时闸门必须照旧拦。
//
// 这是闸门的反向自证，防的是「一改就改成一律不拦」：技能声明必填、素材里也没有、
// 用户只说了一句话 —— 这时候追问是对的，放过去才是拿空字段去套模板。
func TestNeedsGateShortMessageStillAsks(t *testing.T) {
	h := newNeedsGateHandler(t)
	frames := collectChat(t, h, chatBody(t, "s-needs-short", "帮我写一篇公司新闻通稿"))
	all := joinedFrames(frames)
	if !hasEvent(frames, "needs") {
		t.Fatalf("短消息（用户没给材料）时闸门没拦，缺 needs 事件：\n%s", all)
	}
	if !strings.Contains(all, `"asked":"true"`) {
		t.Fatalf("拦下了但没标 asked=true，前端会读成「还在跑」：\n%s", all)
	}
	if strings.Contains(all, "按素材自行推断") {
		t.Fatalf("短消息里不该出现「按素材自行推断」（没有素材可推）：\n%s", all)
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
	h := newNeedsGateHandler(t)
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
