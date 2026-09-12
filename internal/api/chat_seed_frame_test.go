package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"encoding/json"

	"github.com/lizhemin15/skillforge/internal/agent"
	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/model"
)

// 这是聊天 SSE 的接线测试台。存在的原因：resolveMode / analyzeDetail 这类纯函数
// 测得再细，也只能证明"函数本身对"。真正会坑用户的是**接线**——原因算出来了，
// 却在调用处传了空字符串，纯函数测试照样全绿（这一点已被突变注入逮到过）。
//
// 手法：走真的 ServeHTTP。步骤骨架在任何模型调用之前就写出去了，所以配一只
// "永远不回话"的假模型，就能只测 t≈0 那一帧，既不用等几十秒也不花钱。
//
// ⚠️ 不能用 httptest.ResponseRecorder 然后另开 goroutine 读 Body：
// handler 在写、测试在读，go test -race 会直接报数据竞争。所以自己实现一个
// 把字节往 channel 里推的 ResponseWriter，写和读各在各的 goroutine里。
type frameWriter struct {
	mu     sync.Mutex
	hdr    http.Header
	code   int
	buf    strings.Builder
	frames chan string
}

func newFrameWriter() *frameWriter {
	return &frameWriter{hdr: http.Header{}, frames: make(chan string, 64)}
}

func (f *frameWriter) Header() http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hdr
}

func (f *frameWriter) WriteHeader(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.code = code
}

func (f *frameWriter) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buf.Write(b)
	// SSE 帧以空行分隔；可能一次 Write 里带多帧，也可能一帧分多次 Write。
	for {
		s := f.buf.String()
		i := strings.Index(s, "\n\n")
		if i < 0 {
			break
		}
		f.push(frameData(s[:i]))
		f.buf.Reset()
		f.buf.WriteString(s[i+2:])
	}
	return len(b), nil
}

// Flush 必须实现：handler 靠它把骨架帧立刻顶出去。
func (f *frameWriter) Flush() {}

func (f *frameWriter) push(frame string) {
	if frame == "" {
		return
	}
	select {
	case f.frames <- frame:
	default: // 缓冲满就丢，测试只关心最早那几帧
	}
}

// frameData 从一帧 SSE 里取出 data: 那一行（没有 data 行则返回整帧）。
func frameData(frame string) string {
	var out []string
	for _, line := range strings.Split(frame, "\n") {
		if strings.HasPrefix(line, "data:") {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if len(out) == 0 {
		return strings.TrimSpace(frame)
	}
	return strings.Join(out, "\n")
}

// newChatTestHandler 建一只假模型永远挂起的 chatHandler。
// 技能库里已有 seedCoreSkills 建好的「办公文档管家」「技能工厂」。
func newChatTestHandler(t *testing.T, dataDir string) *chatHandler {
	t.Helper()
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 慢模型：不写任何东西直到客户端取消。额外加 5 秒硬上限 —— 假模型
		// 万一等不到取消（比如某个变异让请求丢了 context），它也必须自己退场，
		// 否则 httptest 的 Close 会一直等它，测试变成挂死而不是失败。
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(hang.Close)

	sk := newStoreForTest(t, dataDir)
	cli := llm.New(&model.LLMConfig{APIKey: "test", BaseURL: hang.URL, Model: "test-model"})
	return &chatHandler{eng: agent.New(cli, sk), maxRound: 1}
}

// firstFrame 发一个聊天请求，返回 SSE 里第一帧（JSON 字符串）。
func firstFrame(t *testing.T, h *chatHandler, body string) string {
	t.Helper()
	fw := newFrameWriter()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	req = req.WithContext(ctx)
	go h.ServeHTTP(fw, req)

	select {
	case frame := <-fw.frames:
		cancel()
		return frame
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatalf("3 秒内没等到第一帧（说明又变成先等模型再出画面了）")
		return ""
	}
}

// 手动档 + 技能不存在：t≈0 的骨架帧就要点名"这个技能不可用、已改用自动"。
// 这是接线断言：原因算出来了，调用处也得真的传下去。
func TestChatSeedFrameNamesDegradedSkill(t *testing.T) {
	h := newChatTestHandler(t, t.TempDir())

	frame := firstFrame(t, h,
		`{"session_id":"s1","message":"写点东西","mode":"manual","skill":"这个技能不存在"}`)

	if !strings.Contains(frame, "这个技能不存在") {
		t.Fatalf("首帧没点名失效的技能（用户要等几十秒才知道被降级）：%s", frame)
	}
	if !strings.Contains(frame, "已改用自动调度") {
		t.Fatalf("首帧没说清已降级到自动调度：%s", frame)
	}
}

// 手动档 + 技能真实存在：骨架要读作"已选定技能"，且不许出现降级字样。
func TestChatSeedFrameManualSkillNoDegradeNote(t *testing.T) {
	h := newChatTestHandler(t, t.TempDir())

	frame := firstFrame(t, h,
		`{"session_id":"s2","message":"写点东西","mode":"manual","skill":"技能工厂"}`)

	if strings.Contains(frame, "不可用") || strings.Contains(frame, "改用自动") {
		t.Fatalf("技能好好的却报了降级：%s", frame)
	}
	if !strings.Contains(frame, "技能工厂") {
		t.Fatalf("手动档骨架没锁定技能：%s", frame)
	}
}

// 自动档：骨架是 ① 意图分析，且不得凭空冒出降级说明。
func TestChatSeedFrameAutoHasNoDegradeNote(t *testing.T) {
	h := newChatTestHandler(t, t.TempDir())

	frame := firstFrame(t, h, `{"session_id":"s3","message":"你好","mode":"auto"}`)

	if strings.Contains(frame, "不可用") {
		t.Fatalf("自动档不该出现降级说明：%s", frame)
	}
	var steps []struct {
		Phase  string `json:"phase"`
		Label  string `json:"label"`
		Detail string `json:"detail"`
	}
	// ⚠️ trace 帧的 data 是**裸数组**（[{phase,label,detail,status}]），不是
	// {"steps":[...]}。别照着自己的想象写断言 —— 这条形状是实测出来的。
	if err := json.Unmarshal([]byte(frame), &steps); err != nil {
		t.Fatalf("首帧不是合法 JSON（%v）：%s", err, frame)
	}
	if len(steps) == 0 || steps[0].Phase != "analyze" {
		t.Fatalf("首帧骨架不是 ① 意图分析：%s", frame)
	}
	// 心跳会在 detail 尾部补「已用 Ns」，所以只锚前缀。
	if !strings.HasPrefix(steps[0].Detail, "正在理解你的问题…") {
		t.Fatalf("自动档骨架说明被改了：%q", steps[0].Detail)
	}
}

// 空消息必须在碰引擎之前就被挡掉（否则会白跑一轮模型）。
// ⚠️ 必须跑在 goroutine 里 + 超时兜底：这个请求的 context 永远不会被取消，
// 一旦这条守卫回归，它会直接冲进假模型死等 —— 测试就变成挂死（90 秒超时），
// 而不是一条红。挂死的测试比没有测试更糟。
func TestChatRejectsEmptyMessage(t *testing.T) {
	h := newChatTestHandler(t, t.TempDir())
	fw := newFrameWriter()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"message":"   "}`))
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(fw, req) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("空消息没被挡：请求冲进引擎里去了（被挂起的假模型拖住）")
	}
	if fw.code != http.StatusBadRequest {
		t.Fatalf("空消息没被挡：code=%d", fw.code)
	}
}
