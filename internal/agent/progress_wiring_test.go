package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lizhemin15/skillforge/internal/db"
	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/model"
	"github.com/lizhemin15/skillforge/internal/store"
)

// 这一组守的是「哪一跳关思考链、哪一跳开思考链、材料往哪走」这三件事的**接线**。
//
// 为什么必须行为级验证而不是读源码：llm 层的单测已经证明了 StreamChat 会把开关
// 发出去，但**没人保证调用方真的传了 StreamOpts{DisableThinking:true}**。
// 把 classify 从 StreamChat 换回老的 Chat（一个字符都不报错、类型还兼容）之后：
// 分类重新变成 63 秒静默、材料链路空转，而全仓所有测试仍然全绿。所以这里用一个
// 假 provider（httptest）把真实请求体抓下来断言。
//
// 同时守住反向：**执笔那一跳不得被顺手关掉思考链**。关掉能快，但那是悄悄的降质，
// 而且没有任何症状——只有用户觉得「最近写的东西不如以前」。

type fakeProvider struct {
	mu     sync.Mutex
	bodies []map[string]any
	// content 是这一跳要吐的正文（非流式 JSON 也走它）。
	content string
	// reasoning 非空时，先吐思考链片段再吐 content（模拟 reasoning 模型）。
	reasoning string
	// hang 为真时：只回响应头，然后一直不给字节也不关连接——复刻线上
	// 「上游把连接挂住」的形态（那一轮整轮 616.5s）。
	hang bool
}

func (f *fakeProvider) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		content, reasoning, hang := f.content, f.reasoning, f.hang
		f.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if hang {
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			<-r.Context().Done()
			return
		}
		stream, _ := body["stream"].(bool)
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + jsonStr(content) + `}}]}`))
			return
		}
		var frames []string
		for _, piece := range splitChunks(reasoning) {
			frames = append(frames, `data: {"choices":[{"delta":{"reasoning_content":`+jsonStr(piece)+`}}]}`)
		}
		for _, piece := range splitChunks(content) {
			frames = append(frames, `data: {"choices":[{"delta":{"content":`+jsonStr(piece)+`}}]}`)
		}
		frames = append(frames, `data: [DONE]`, ``)
		_, _ = w.Write([]byte(strings.Join(frames, "\n\n")))
	}))
}

func (f *fakeProvider) bodyAt(t *testing.T, i int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.bodies) {
		t.Fatalf("只收到 %d 次请求，取不到第 %d 次", len(f.bodies), i+1)
	}
	return f.bodies[i]
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// splitChunks 把一段文本切成 2 个字符一片，模拟真实 token 级推流。
func splitChunks(s string) []string {
	if s == "" {
		return nil
	}
	r := []rune(s)
	var out []string
	for i := 0; i < len(r); i += 2 {
		end := i + 2
		if end > len(r) {
			end = len(r)
		}
		out = append(out, string(r[i:end]))
	}
	return out
}

func newTestEngine(t *testing.T, fp *fakeProvider) *Engine {
	t.Helper()
	srv := fp.server(t)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	cli := llm.New(&model.LLMConfig{BaseURL: srv.URL, APIKey: "k", Model: "fake"})
	return New(cli, store.NewSkillStore(d, dir))
}

// 意图分类必须**真的**请求关思考链：它是用户按下发送之后挡在第一位的那几十秒。
func TestEvalTurnAsksProviderToDisableThinking(t *testing.T) {
	fp := &fakeProvider{content: `{"intent":"write","action":"write","skill_slug":"","needs_tools":false,"reason":"通用写作","params":{},"needs":[],"steps":[]}`}
	eng := newTestEngine(t, fp)

	if _, err := eng.EvalTurn(context.Background(), "s1", "写一份数据治理通知", nil); err != nil {
		t.Fatalf("EvalTurn 失败: %v", err)
	}
	body := fp.bodyAt(t, 0)
	if v := body["enable_thinking"]; v != false {
		t.Fatalf("分类这一跳必须带 enable_thinking=false，实际 %v", v)
	}
	if v := body["reasoning_effort"]; v != "none" {
		t.Fatalf("分类这一跳必须带 reasoning_effort=none，实际 %v", v)
	}
	if body["stream"] != true {
		t.Fatalf("分类这一跳必须是流式（否则 provider 忽略开关时连材料都没有），实际 stream=%v", body["stream"])
	}
}

// provider 忽略开关、照旧产思考链时，分类这一跳的材料也要能流出去（astron 就这样）。
//
// 这条路上材料有**两个来源**，缺哪个都会让用户在那一跳只看到跳秒的计时：
//   - 思考链（只有 provider 忽略关思考链开关时才有，astron 上实测一片都没有）；
//   - JSON 里的 reason 字段（contentSink 抽的，任何 provider 都有）。
//
// 所以断言从「材料恰好等于思考链」改成「两个来源都在」——覆盖面比原来更宽，
// 同时补上「不得混进 JSON 结构」这一条，那才是 contentSink 真正的风险面。
func TestEvalTurnReportsMaterialEvenWhenKnobIgnored(t *testing.T) {
	fp := &fakeProvider{
		reasoning: "用户在延续上一轮的新闻稿，要把正文整理成 Word 文档。",
		content:   `{"intent":"docgen","action":"gen","skill_slug":"","needs_tools":false,"reason":"整理成文档","params":{},"needs":[],"steps":[]}`,
	}
	eng := newTestEngine(t, fp)

	var got []string
	ctx := WithProgress(context.Background(), func(s string) { got = append(got, s) })
	if _, err := eng.EvalTurn(ctx, "s1", "把它整理成 word", nil); err != nil {
		t.Fatalf("EvalTurn 失败: %v", err)
	}
	joined := strings.Join(got, "")
	if !strings.Contains(joined, fp.reasoning) {
		t.Fatalf("被忽略的思考链没当材料流出去，实际 %q", joined)
	}
	if !strings.Contains(joined, "整理成文档") {
		t.Fatalf("JSON 的 reason 没当材料流出去（provider 关掉思考链时这是唯一的材料来源），实际 %q", joined)
	}
	for _, noise := range []string{`{`, `}`, `"intent"`, `"params"`, `"skill_slug"`, `[`} {
		if strings.Contains(joined, noise) {
			t.Errorf("材料里混进了 JSON 结构 %q：%q", noise, joined)
		}
	}
}

// docgen 规格那一跳是整轮最长的静默：关掉思考链后线上实测裸跑 16.8 秒（整轮 23.8s），
// 这期间屏幕上只有「正在生成…（已用 6s/9s/12s…）」在跳。
//
// astron 上关思考链的开关是真管用的（实测 reasoning 片数 = 0），所以这一跳的材料
// **只能**来自 contentSink 从流式 JSON 里抽出来的正文。这条断言就是那 16.8 秒的尺子：
// 关掉思考链的路径必须仍有内容在动，否则用户看到的还是一个空跳的计时。
func TestGenerateDocStreamsDocumentTextAsMaterial(t *testing.T) {
	fp := &fakeProvider{content: `{"format":"word","filename":"关于开展数据治理专项工作的通知.docx",` +
		`"title":"关于开展数据治理专项工作的通知",` +
		`"parags":["为深入贯彻公司数据治理工作部署，现将有关事项通知如下。","请各部门于每月底前报送工作进展。"]}`}
	eng := newTestEngine(t, fp)

	var got []string
	ctx := WithProgress(context.Background(), func(s string) { got = append(got, s) })
	// 生成失败的处置属于另一条线（模板/落盘），这里只看材料有没有流出去。
	_, _ = eng.GenerateDoc(ctx, "s1", &SkillContent{Name: "办公文档管家", SystemPrompt: "按 DOCJSON 契约输出"}, nil, "写一份关于开展数据治理专项工作的通知", nil)

	joined := strings.Join(got, "")
	if joined == "" {
		t.Fatal("docgen 这一跳一片材料都没有：关掉思考链后它是整轮里最长的一段静默，材料只能来自 contentSink 抽的正文")
	}
	for _, want := range []string{"关于开展数据治理专项工作的通知", "为深入贯彻公司数据治理工作部署", "请各部门于每月底前报送工作进展。"} {
		if !strings.Contains(joined, want) {
			t.Errorf("正文没流成材料，缺 %q：%q", want, joined)
		}
	}
	for _, noise := range []string{".docx", "format", "filename", "parags"} {
		if strings.Contains(joined, noise) {
			t.Errorf("材料里混进了结构性字段 %q：%q", noise, joined)
		}
	}
}

// 执笔那一跳：不关思考链（质量优先），但思考链必须当材料流出去，且不得混进正文。
func TestGenerateKeepsThinkingButStreamsItAsMaterial(t *testing.T) {
	fp := &fakeProvider{
		reasoning: "先看手册要求：通知要有标题、正文、落款，语气庄重。",
		content:   "关于开展数据治理专项工作的通知",
	}
	eng := newTestEngine(t, fp)

	var materials []string
	ctx := WithProgress(context.Background(), func(s string) { materials = append(materials, s) })

	sc := &SkillContent{Slug: "tz", Name: "通知", SkillType: model.SkillTypeWrite, SystemPrompt: "按通知格式写"}
	out, err := eng.Generate(ctx, sc, map[string]string{"topic": "数据治理"}, nil)
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}

	body := fp.bodyAt(t, 0)
	if _, ok := body["enable_thinking"]; ok {
		t.Fatal("执笔这一跳不得关思考链（那是质量来源）")
	}
	if _, ok := body["reasoning_effort"]; ok {
		t.Fatal("执笔这一跳不得关思考链（那是质量来源）")
	}
	if joined := strings.Join(materials, ""); joined != fp.reasoning {
		t.Fatalf("执笔的思考链必须当材料流出去，实际 %q", joined)
	}
	if strings.Contains(out, "先看手册要求") {
		t.Fatalf("思考链混进正文了（会被下载到用户的 Word 里）: %q", out)
	}
	if out != fp.content {
		t.Fatalf("正文不对: %q", out)
	}
}

// 没挂材料接收器时不能崩，也不能因此少调一次模型 —— 巡检/批处理/单测都跑同一条路。
func TestGenerateWithoutProgressSink(t *testing.T) {
	fp := &fakeProvider{reasoning: "思考", content: "正文"}
	eng := newTestEngine(t, fp)

	sc := &SkillContent{Slug: "tz", Name: "通知", SkillType: model.SkillTypeWrite, SystemPrompt: "x"}
	out, err := eng.Generate(context.Background(), sc, nil, nil)
	if err != nil {
		t.Fatalf("没有接收器时不该失败: %v", err)
	}
	if out != "正文" {
		t.Fatalf("正文不对: %q", out)
	}
}

// 材料是「进行中那一步」的东西；ctx 没挂接收器时 ReportProgress 必须静默，
// 不能 panic（nil 接收器 + 直接调用是历史事故形态）。
func TestReportProgressWithoutSink(t *testing.T) {
	ReportProgress(context.Background(), "没人接")
	ReportProgress(nil, "连 ctx 都没有") //nolint:staticcheck // 故意传 nil，验证不 panic
}

// 分类这一跳必须有自己的时间预算（这是 R1 的第二道闸）。
//
// 线上那一轮 616.5s 的账：600s 全耗在分类这一跳——它拿的是整轮 ctx，靠整轮兜底
// 等于没有兜底。正常流完只要 6s（关思考链，线上实测），预算给十倍余量就够。
// 超时必须兜底成普通对话，绝不把错误甩给用户。
func TestEvalTurnBoundsClassifierHop(t *testing.T) {
	t.Setenv("SKILLFORGE_CLASSIFY_TIMEOUT_SEC", "1")
	fp := &fakeProvider{hang: true}
	eng := newTestEngine(t, fp)

	start := time.Now()
	ev, err := eng.EvalTurn(context.Background(), "s1", "写一份数据治理通知", nil)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("分类这一跳挂住时不能把错误抛给用户，必须兜底成普通对话: %v", err)
	}
	if ev == nil {
		t.Fatal("超时后必须给出兜底 Eval，nil 会让整轮没法继续")
	}
	if elapsed > 6*time.Second {
		t.Fatalf("分类预算 1s 就该收手，实际等了 %s —— 说明这一跳还在靠整轮 ctx 兜底（线上 600s 就是这么来的）", elapsed)
	}
	// 超时不是「结果不好」，不该再重试一次把用户等待翻倍。
	fp.mu.Lock()
	n := len(fp.bodies)
	fp.mu.Unlock()
	if n != 1 {
		t.Fatalf("超时后不该重试（等待会翻倍），实际发生 %d 次请求", n)
	}
}
