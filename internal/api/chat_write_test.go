package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lizhemin15/skillforge/internal/agent"
	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/model"
)

// ===== 手册模式写作的回归测试 =====
//
// 全部用**合成样本**：本机那份真实素材是 54MB 扫描件 + OCR 中间产物，不进版本库
// （见 .gitignore），测试骑在它上面等于在 CI 上必红。合成样本只保留结构：两个分类、
// 每类一篇范文、一份审稿清单，每处带一句**独特标记句**——断言靠标记句定位，不靠
// 字数或顺序，这样改排版不会假红。
//
// 这一组测试守的是三件必须成立的事：
//  1. 本类要求与范文**真的进了 prompt**（否则「引真实范文」是空头支票）；
//  2. 注入**只带本类**，不串类（串了就等于按错的要求写，是设计里最怕的失效）；
//  3. 判不准类别时**不硬写**（宁可反问用户，不可猜错类别写一整篇）。

const (
	fixReqNews   = "首段必须包含时间、地点、主体、事件四要素"
	fixReqSum    = "总结必须分「完成情况/存在问题/下一步计划」三段"
	fixTriggerNw = "对外发布的新闻通告、事件通稿"
	fixExNews    = "【范文甲】某市于三月十二日举行开工仪式，市长出席并致辞。"
	fixExSum     = "【范文乙】本季度共完成产值三亿元，同比增长百分之七。"
	fixReview    = "是否出现第一人称「我」"
)

// newManualWriteFixture 造一棵手册模式技能的目录树，返回 (dataDir, slug)。
func newManualWriteFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	const slug = "手册写作"
	root := filepath.Join(dir, "skills", slug)
	put := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("建目录失败：%v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("写素材失败：%v", err)
		}
	}
	put("categories/01-新闻通稿.md", "# 新闻通稿\n\n"+
		"## 触发场景\n"+fixTriggerNw+"时使用。\n\n"+
		"## 写作要求\n- "+fixReqNews+"\n- 全文不得出现第一人称\n\n"+
		"## 参考范文\n- `examples/新闻通稿/01.md`\n")
	put("categories/02-工作总结.md", "# 工作总结\n\n"+
		"## 触发场景\n季度或年度收尾、向上汇报工作时使用。\n\n"+
		"## 写作要求\n- "+fixReqSum+"\n\n"+
		"## 参考范文\n- `examples/工作总结/01.md`\n")
	// 索引文件必须被忽略：它和各类的触发场景重复，读进来就是两份可能打架的路由表。
	put("categories/_index.md", "# 分类索引\n\n- 01 新闻通稿\n- 02 工作总结\n")
	put("examples/新闻通稿/01.md", fixExNews+"\n\n仪式于上午九时开始，共三百余人参加。\n")
	put("examples/工作总结/01.md", fixExSum+"\n\n下一步将重点压降库存周转天数。\n")
	put("reviewer.md", "# 稿件自查清单\n\n- "+fixReview+"\n- 数字是否与素材一致\n")
	return dir, slug
}

// fakeLLM 是一只能记录请求、按需作答的假模型。
//
// 记录请求是这组测试的关键：要断言的是「本类要求与范文进了 prompt」，而 prompt
// 只存在于发出去的请求体里，不抓下来就只能断言间接结果（比如「输出里提到了范文
// 里的话」），那是骑在模型行为上的假绿。
type fakeLLM struct {
	mu    sync.Mutex
	reqs  []fakeLLMReq
	reply func(system, user string) string
	srv   *httptest.Server
}

type fakeLLMReq struct {
	System   string
	User     string
	JSONMode bool
	// 推荐行（llm.FastJSON）走的是裸 HTTP，不经过 go-openai，所以这两项由
	// 假模型自己从原始 body 里读。它们必须被断言守住：一旦有人把这条路改回
	// Chat，思考链就关不掉了（实测 6.24s vs 1.38s），而超时后前端会静默退回
	// 规则版推荐行——页面上完全看不出功能已经死了。
	ThinkingOff bool
	MaxTokens   int
}

func newFakeLLM(t *testing.T, reply func(system, user string) string) *fakeLLM {
	t.Helper()
	f := &fakeLLM{reply: reply}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			ResponseFormat *struct {
				Type string `json:"type"`
			} `json:"response_format"`
			EnableThinking *bool `json:"enable_thinking"`
			MaxTokens      int   `json:"max_tokens"`
		}
		_ = json.Unmarshal(body, &req)

		var sys, usr string
		for _, m := range req.Messages {
			switch m.Role {
			case "system":
				sys = m.Content
			case "user":
				usr = m.Content
			}
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, fakeLLMReq{
			System:      sys,
			User:        usr,
			JSONMode:    req.ResponseFormat != nil && req.ResponseFormat.Type == "json_object",
			ThinkingOff: req.EnableThinking != nil && !*req.EnableThinking,
			MaxTokens:   req.MaxTokens,
		})
		replyFn := f.reply
		f.mu.Unlock()

		out := replyFn(sys, usr)
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			emit := func(part string) {
				chunk := map[string]any{
					"id":      "fake",
					"object":  "chat.completion.chunk",
					"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": part}}},
				}
				b, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", b)
				if fl != nil {
					fl.Flush()
				}
			}
			// 切成两片发：顺带覆盖「流式分片要拼成整段」这条路径。
			r := []rune(out)
			half := len(r) / 2
			emit(string(r[:half]))
			emit(string(r[half:]))
			fmt.Fprint(w, "data: [DONE]\n\n")
			if fl != nil {
				fl.Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "fake",
			"object": "chat.completion",
			"choices": []any{map[string]any{
				"index":   0,
				"message": map[string]string{"role": "assistant", "content": out},
			}},
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// setReply 换掉作答函数（一个测试里要走多种模型行为时用）。
func (f *fakeLLM) setReply(reply func(system, user string) string) {
	f.mu.Lock()
	f.reply = reply
	f.mu.Unlock()
}

// prompts 返回所有已捕获请求的「system + user」拼接，供包含性断言。
func (f *fakeLLM) prompts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.reqs))
	for _, r := range f.reqs {
		out = append(out, r.System+"\n@@@\n"+r.User)
	}
	return out
}

func (f *fakeLLM) lastReq(t *testing.T) fakeLLMReq {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("假模型一个请求都没收到")
	}
	return f.reqs[len(f.reqs)-1]
}

func newManualEngine(t *testing.T, dataDir string, f *fakeLLM) *agent.Engine {
	t.Helper()
	sk := newStoreForTest(t, dataDir)
	return agent.New(llm.New(&model.LLMConfig{APIKey: "test", BaseURL: f.srv.URL, Model: "fake"}), sk)
}

func mustContain(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("%s：prompt 里找不到 %q\n---- 实际 prompt（前 1200 字）----\n%s",
			what, needle, runeClipForTest(haystack, 1200))
	}
}

func mustNotContain(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Fatalf("%s：prompt 里不该出现 %q，但它出现了\n---- 实际 prompt（前 1200 字）----\n%s",
			what, needle, runeClipForTest(haystack, 1200))
	}
}

func runeClipForTest(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// TestWritePackLoadsCategoriesAndExamples 守素材装载：分类按文件序、_index.md 不参与、
// 范文按「参考范文」里列的路径读回来、审稿清单非空。
func TestWritePackLoadsCategoriesAndExamples(t *testing.T) {
	dir, slug := newManualWriteFixture(t)
	f := newFakeLLM(t, func(string, string) string { return "{}" })
	eng := newManualEngine(t, dir, f)

	pack, err := eng.LoadWritePack(slug)
	if err != nil {
		t.Fatalf("LoadWritePack: %v", err)
	}
	if pack == nil {
		t.Fatal("应识别为手册模式，却返回 nil")
	}
	if len(pack.Categories) != 2 {
		names := make([]string, 0, len(pack.Categories))
		for _, c := range pack.Categories {
			names = append(names, c.Name)
		}
		t.Fatalf("分类数应为 2（_index.md 不计），实得 %d：%v", len(pack.Categories), names)
	}
	if pack.Categories[0].Name != "新闻通稿" || pack.Categories[1].Name != "工作总结" {
		t.Fatalf("分类名或顺序不对：%q, %q", pack.Categories[0].Name, pack.Categories[1].Name)
	}
	if !strings.Contains(pack.Categories[0].Trigger, fixTriggerNw) {
		t.Fatalf("新闻通稿的触发场景没解析出来：%q", pack.Categories[0].Trigger)
	}
	if got := len(pack.Categories[0].Examples); got != 1 {
		t.Fatalf("新闻通稿应有 1 篇范文，实得 %d", got)
	}
	if !strings.Contains(pack.Categories[0].Examples[0].Content, fixExNews) {
		t.Fatalf("范文内容不对：%q", pack.Categories[0].Examples[0].Content)
	}
	if !strings.Contains(pack.Reviewer, fixReview) {
		t.Fatalf("审稿清单没读回来：%q", pack.Reviewer)
	}

	// 不是手册模式的技能必须返回 (nil, nil)：调用方靠这个走回单段生成，
	// 若这里报错，普通写作技能会变成一条报错而不是正常工作。
	noPack, err := eng.LoadWritePack("不存在的技能")
	if err != nil || noPack != nil {
		t.Fatalf("非手册技能应返回 (nil, nil)，实得 (%v, %v)", noPack, err)
	}
}

// TestGenerateWithPackInjectsOnlyThisCategory 是这组测试的核心：
// 本类要求与本类范文必须进 prompt，且**别的类的素材一句都不能进**。
// 后半句不是凑数的——串类注入（比如把整本手册塞进去）会让模型按错的要求写，
// 而输出看上去依然像模像样，用户很难发现，所以必须被钉住。
func TestGenerateWithPackInjectsOnlyThisCategory(t *testing.T) {
	dir, slug := newManualWriteFixture(t)
	f := newFakeLLM(t, func(string, string) string { return "初稿正文" })
	eng := newManualEngine(t, dir, f)

	pack, err := eng.LoadWritePack(slug)
	if err != nil || pack == nil {
		t.Fatalf("准备素材失败：pack=%v err=%v", pack, err)
	}
	cat := pack.FindCategory("新闻通稿")
	if cat == nil {
		t.Fatal("FindCategory(新闻通稿) 返回 nil")
	}

	sc := &agent.SkillContent{Slug: slug, Name: "手册写作", SkillType: model.SkillTypeWrite,
		SystemPrompt: "你是公文写作助手，按手册要求写作。"}
	got, err := eng.GenerateWithPack(context.Background(), sc, pack, cat, map[string]string{}, "", nil)
	if err != nil {
		t.Fatalf("GenerateWithPack: %v", err)
	}
	if !strings.Contains(got, "初稿正文") {
		t.Fatalf("生成结果没透传模型输出：%q", got)
	}

	all := strings.Join(f.prompts(), "\n===REQ===\n")
	mustContain(t, all, sc.SystemPrompt, "技能自身的提示词")
	mustContain(t, all, fixReqNews, "本类写作要求")
	mustContain(t, all, fixExNews, "本类真实范文")
	mustContain(t, all, "新闻通稿", "命中分类名")
	mustNotContain(t, all, fixReqSum, "串类防线（别类要求）")
	mustNotContain(t, all, fixExSum, "串类防线（别类范文）")
}

// TestRouteCategoryThreeStates 守判类的三态：命中、留空、表外名字。
// 后两态都必须标成 Ambiguous ——「判不准也别猜」是这套流程的前提，猜错类别是
// 整篇按错的要求写，用户还得自己发现。
func TestRouteCategoryThreeStates(t *testing.T) {
	dir, slug := newManualWriteFixture(t)
	f := newFakeLLM(t, func(string, string) string { return `{"category":"","confidence":"low"}` })
	eng := newManualEngine(t, dir, f)
	pack, err := eng.LoadWritePack(slug)
	if err != nil || pack == nil {
		t.Fatalf("准备素材失败：pack=%v err=%v", pack, err)
	}

	cases := []struct {
		name        string
		reply       string
		wantCat     string
		wantAmbig   bool
		wantUnknown bool
	}{
		{name: "逐字命中", reply: `{"category":"新闻通稿","confidence":"high","reason":"对外发布"}`, wantCat: "新闻通稿"},
		// 模型写歪一点（加「类」后缀 + 书名号）要能认回来：这种差异不该把用户打断。
		{name: "写歪但唯一命中", reply: `{"category":"《新闻通稿类》","confidence":"high"}`, wantCat: "新闻通稿"},
		{name: "留空表示判不出", reply: `{"category":"","confidence":"low","reason":"信息不足"}`, wantAmbig: true},
		{name: "表外名字不许猜", reply: `{"category":"财经简报","confidence":"high"}`, wantAmbig: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.setReply(func(string, string) string { return tc.reply })
			route, err := eng.RouteCategory(context.Background(), pack, "写个东西", nil)
			if err != nil {
				t.Fatalf("RouteCategory: %v", err)
			}
			if route.Ambiguous != tc.wantAmbig {
				t.Fatalf("Ambiguous=%v，期望 %v（category=%q）", route.Ambiguous, tc.wantAmbig, route.Category)
			}
			if tc.wantCat != "" && route.Category != tc.wantCat {
				t.Fatalf("命中的分类应为 %q，实得 %q", tc.wantCat, route.Category)
			}
		})
	}

	// 路由是刻意的「便宜调用」：只喂分类名 + 触发场景，不喂完整要求与范文。
	// 这条不是性能洁癖——每次对话都背上几千字手册会又慢又贵。
	f.setReply(func(string, string) string { return `{"category":"新闻通稿","confidence":"high"}` })
	if _, err := eng.RouteCategory(context.Background(), pack, "写篇通稿", nil); err != nil {
		t.Fatalf("RouteCategory: %v", err)
	}
	last := f.lastReq(t)
	if !last.JSONMode {
		t.Error("判类必须开 JSON 模式：结构化解 JSON 时靠裸输出是运气，中文引号就能炸")
	}
	mustContain(t, last.User, fixTriggerNw, "路由用的触发场景表")
	mustNotContain(t, last.User, fixReqNews, "路由不该背上完整写作要求")
	mustNotContain(t, last.User, fixExNews, "路由不该背上范文全文")
}

// TestReviewParsesIssuesAndIsDiagnosable 守审稿：正常 JSON 能解析、severity 归一化、
// 「说 revise 却没问题」按通过处理、坏 JSON 报错要能诊断（带原文片段，不是 latin1 乱码）。
func TestReviewParsesIssuesAndIsDiagnosable(t *testing.T) {
	dir, slug := newManualWriteFixture(t)
	f := newFakeLLM(t, func(string, string) string { return "{}" })
	eng := newManualEngine(t, dir, f)
	pack, err := eng.LoadWritePack(slug)
	if err != nil || pack == nil {
		t.Fatalf("准备素材失败：pack=%v err=%v", pack, err)
	}
	cat := pack.FindCategory("新闻通稿")

	f.setReply(func(string, string) string {
		return `{"verdict":"REVISE","issues":[{"severity":"BLOCKING","rule":"` + fixReqNews + `","quote":"某单位举行活动","fix":"补上时间地点"},{"severity":"","rule":"别的问题","quote":"x","fix":"y"}]}`
	})
	res, err := eng.Review(context.Background(), pack, cat, "写篇通稿", "某单位举行活动。")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if res.Verdict != "revise" {
		t.Fatalf("verdict 应归一化为 revise，实得 %q", res.Verdict)
	}
	if len(res.Issues) != 2 || res.Blocking() != 1 {
		t.Fatalf("应有 2 条问题、其中 1 条 blocking，实得 %d 条 / %d blocking", len(res.Issues), res.Blocking())
	}
	if res.Issues[1].Severity != "minor" {
		t.Fatalf("severity 空值应归一化为 minor，实得 %q", res.Issues[1].Severity)
	}

	// 审稿提示必须带上「检查项 / 本类要求」，否则审稿人是拿自己想的标准在挑毛病。
	last := f.lastReq(t)
	mustContain(t, last.User, fixReview, "审稿清单")
	mustContain(t, last.User, fixReqNews, "本类写作要求")
	mustContain(t, last.User, "某单位举行活动", "待审稿件")
	if !last.JSONMode {
		t.Error("审稿必须开 JSON 模式")
	}

	// 说 revise 却一条问题都不给 = 一张没法执行的条子：按「其实挑不出问题」处理，
	// 而不是把一句「重写吧」丢给用户。
	f.setReply(func(string, string) string { return `{"verdict":"revise","issues":[]}` })
	res, err = eng.Review(context.Background(), pack, cat, "写篇通稿", "稿子")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if res.Verdict != "pass" {
		t.Fatalf("revise 但零问题时应收敛为 pass，实得 %q", res.Verdict)
	}

	// 坏 JSON：错误信息必须能定位到原文，否则线上只能看到一个 json 语法错误。
	f.setReply(func(string, string) string { return `{"verdict":"revise","issues":[{"rule":"` + fixReqNews })
	_, err = eng.Review(context.Background(), pack, cat, "写篇通稿", "稿子")
	if err == nil {
		t.Fatal("坏 JSON 必须报错，不能静默当成审稿通过")
	}
	if !strings.Contains(err.Error(), "解析失败") {
		t.Fatalf("错误信息应说明是解析失败，实得：%v", err)
	}
	if !strings.Contains(err.Error(), fixReqNews) {
		t.Fatalf("错误信息应带原文片段便于诊断，实得：%v", err)
	}
}

// TestGenerateWithPackFailureIsNotSilent 守「不许静默降级」：模型报错时必须显式返回
// 错误，不能返回个空字符串让人以为「写完了但内容为空」。
func TestGenerateWithPackFailureIsNotSilent(t *testing.T) {
	dir, slug := newManualWriteFixture(t)
	f := newFakeLLM(t, func(string, string) string { return "" })
	eng := newManualEngine(t, dir, f)
	pack, err := eng.LoadWritePack(slug)
	if err != nil || pack == nil {
		t.Fatalf("准备素材失败：pack=%v err=%v", pack, err)
	}
	sc := &agent.SkillContent{Slug: slug, Name: "手册写作", SkillType: model.SkillTypeWrite, SystemPrompt: "按手册写作。"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 让底层 HTTP 调用直接失败
	if _, err := eng.GenerateWithPack(ctx, sc, pack, pack.FindCategory("新闻通稿"), map[string]string{}, "", nil); err == nil {
		t.Fatal("底层调用失败时必须返回错误，不能静默返回空稿")
	}
}
