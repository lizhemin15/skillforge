package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lizhemin15/skillforge/internal/agent"
	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/model"
)

// 推荐行（/api/chat/suggest）的回归防线。
//
// 分三层，各自只守一件事：
//   - TestSuggestParse*：纯函数。模型回什么形态都可能，这里穷举「回得很难看」的那些。
//   - TestSuggestPrompt*：提示词。抓的是「悄悄忘了带上下文」——它不会报错，
//     只会让建议越来越空，界面上看不出来。
//   - TestSuggestRoute*：接线。走真 mux，抓「handler 写了但没注册」这种
//     静默 404（前端 fetch 失败是 .catch 吞掉的，页面上一点动静都没有）。

func TestSuggestParse_CleanJSON(t *testing.T) {
	raw := `{"chips":[{"label":"再精简一版","send":"把刚才的通知精简到 300 字"},{"label":"换成表格","send":"把内容整理成表格"}]}`
	got := parseSuggestChips(raw, suggestMaxChips)
	if len(got) != 2 {
		t.Fatalf("应得 2 颗，实得 %d：%+v", len(got), got)
	}
	if got[0].Label != "再精简一版" || got[0].Send != "把刚才的通知精简到 300 字" {
		t.Fatalf("第一颗被改坏了：%+v", got[0])
	}
}

// 模型最常见的两种「包装」：```json 围栏、以及前面那句客套话。
// 这两种都必须能救回来——救不回来的表现是推荐行永远停在规则版，
// 而规则版看着「也还行」，于是没人会去查。
func TestSuggestParse_WrappedForms(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"围栏", "```json\n{\"chips\":[{\"label\":\"加一段结论\",\"send\":\"加一段结论\"}]}\n```"},
		{"围栏无语言", "```\n{\"chips\":[{\"label\":\"加一段结论\"}]}\n```"},
		{"有前言", "好的，这是建议：\n{\"chips\":[{\"label\":\"加一段结论\"}]}\n希望有帮助！"},
		{"裸数组", `[{"label":"加一段结论"}]`},
		{"前言+裸数组", `建议如下：[{"label":"加一段结论"}]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseSuggestChips(c.raw, suggestMaxChips)
			if len(got) != 1 || got[0].Label != "加一段结论" {
				t.Fatalf("这一形态没被救回来：%+v", got)
			}
			// send 缺省必须回落到 label：没有 send 的胶囊点下去是空消息。
			if got[0].Send != "加一段结论" {
				t.Fatalf("send 缺省时没有回落到 label：%+v", got[0])
			}
		})
	}
}

// 洗 label：编号、项目符号、末尾句号、markdown 强调、书名号。
// 这些都会让胶囊在一行里挤成两行，或者干脆显示成「1. 再精简一版。」这么丑的东西。
func TestSuggestParse_LabelCleaning(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1. 再精简一版", "再精简一版"},
		{"2、换成表格", "换成表格"},
		{"- 补一段结论", "补一段结论"},
		{"**加数据**", "加数据"},
		{"“更正式一点”", "更正式一点"},
		{"加一页结论。", "加一页结论"},
		{"再精简一版？", "再精简一版"},
		{"  压缩到300字  ", "压缩到300字"},
	}
	for _, c := range cases {
		if got := cleanChipLabel(c.in); got != c.want {
			t.Errorf("cleanChipLabel(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// 占位符与 schema 回声：**这些是字符串**，前端的 .filter(x => x.label) 挡不住，
// 它们真的会上屏成一颗写着 undefined / label 的胶囊。
func TestSuggestParse_DropsPlaceholders(t *testing.T) {
	raw := `{"chips":[
		{"label":"undefined"},
		{"label":"null"},
		{"label":"N/A"},
		{"label":"无"},
		{"label":"{\"label\":\"x\",\"send\":\"y\"}"},
		{"label":"再精简一版"}
	]}`
	got := parseSuggestChips(raw, suggestMaxChips)
	if len(got) != 1 || got[0].Label != "再精简一版" {
		t.Fatalf("占位符没被挡掉：%+v", got)
	}
	for _, c := range got {
		if strings.Contains(strings.ToLower(c.Label), "undefined") || strings.Contains(c.Label, "{") {
			t.Fatalf("脏 label 漏到上屏层：%+v", c)
		}
	}
}

// 去重与封顶：一张推荐行只放得下 4 颗，第 5 颗会换行——换行就等于这次极简改版失败。
func TestSuggestParse_DedupeAndCap(t *testing.T) {
	raw := `{"chips":[
		{"label":"换成表格","send":"a"},
		{"label":"换成表格","send":"b"},
		{"label":"再加一版"},
		{"label":"更正式些"},
		{"label":"补数据"},
		{"label":"缩到300字"},
		{"label":"做简报"}
	]}`
	got := parseSuggestChips(raw, suggestMaxChips)
	// 期望值必须写**字面量 4**，不能写 suggestMaxChips。
	// 第一版写的是常量，自证脚本把常量改成 5 之后这条用例照样绿——
	// 常量进、常量出，等于自己喂自己，封顶这件事根本没被断言守过。
	if len(got) != 4 {
		t.Fatalf("应封顶 4 颗（一行放得下 4 颗，第 5 颗会换行），实得 %d：%+v", len(got), got)
	}
	if got[0].Send != "a" {
		t.Fatalf("去重时应保留先出现的那颗：%+v", got[0])
	}
	seen := map[string]bool{}
	for _, c := range got {
		if seen[c.Label] {
			t.Fatalf("出现重复 label：%+v", got)
		}
		seen[c.Label] = true
	}
}

// label 截断按「字」不按「字节」：中文 3 字节，按字节切会切出乱码，
// 而乱码胶囊点下去是把乱码发给模型。
func TestSuggestParse_LabelTruncatesByRune(t *testing.T) {
	long := strings.Repeat("精", 30)
	raw := `{"chips":[{"label":"` + long + `","send":"把刚才那版精简到 300 字"}]}`
	got := parseSuggestChips(raw, suggestMaxChips)
	if len(got) != 1 {
		t.Fatalf("应得 1 颗：%+v", got)
	}
	if n := len([]rune(got[0].Label)); n != suggestLabelRunes {
		t.Fatalf("label 应按字符截到 %d，实得 %d：%q", suggestLabelRunes, n, got[0].Label)
	}
	// send 不能被一起截短：它要带走「哪一版」这种指代，短了就变成一句空话。
	if !strings.Contains(got[0].Send, "300") {
		t.Fatalf("send 被改坏了：%+v", got[0])
	}
}

// 完全救不回来时必须回 nil（= 前端保留规则版），**不能 panic、不能半截**。
func TestSuggestParse_GarbageReturnsNil(t *testing.T) {
	for _, raw := range []string{"", "   ", "你好，我不知道该建议什么。", `{"chips":[]}`, `{"foo":"bar"}`, `{"chips":"不是数组"}`, "```json\n没写完"} {
		if got := parseSuggestChips(raw, suggestMaxChips); got != nil {
			t.Errorf("垃圾输入 %q 应得 nil，实得 %+v", raw, got)
		}
	}
}

// 提示词必须真的带上「用户刚说了什么」和「这个站有什么技能」。
// 少了前者，模型给的是通用套话；少了后者，它会推荐站里根本做不到的事。
func TestSuggestPrompt_CarriesContext(t *testing.T) {
	// 用户那句话里要有一个**只在 last_user 里出现**的记号（这里：编号）。
	// 第一版用的是「防汛」——而 last_reply 里也写了「关于做好防汛工作的紧急通知」，
	// 于是「last_user 被丢掉」这个故障根本没被抓住（prompt 里照样有「防汛」）。
	// 这类假阳性的来源是「一个词同时出现在多处」，夹具必须自己保证唯一性。
	req := suggestReq{
		LastUser:  "帮我写一份关于防汛的紧急通知，编号 XJ-2026-114",
		LastReply: "好的，通知已生成：关于做好防汛工作的紧急通知……",
		UsedSkill: "公文写作",
		Skills:    []suggestSkill{{Slug: "gov", Name: "公文写作"}, {Slug: "data", Name: "数据表格"}},
	}
	sys, user := suggestPrompt(req)
	if !strings.Contains(user, "XJ-2026-114") {
		t.Fatalf("prompt 里没有用户那句话（只在 last_user 里出现的编号都没了）：\n%s", user)
	}
	if !strings.Contains(user, "公文写作") || !strings.Contains(user, "数据表格") {
		t.Fatalf("prompt 里没有技能清单：\n%s", user)
	}
	if !strings.Contains(sys, "label") || !strings.Contains(sys, "send") {
		t.Fatalf("system prompt 没交代输出格式：\n%s", sys)
	}
	// 不许编造具体事实 —— 线上验收时抓到的真实输出：助手刚问「产品名/卖点/人群」，
	// 模型给的 send 直接写成「产品名称：智能办公本，核心卖点：纸感屏幕防误触…」，
	// 而对话里从来没有这个产品。用户点一下，就等于自己认领了这条假事实。
	//
	// ⚠️ 断言必须锚**整句**（这条规则的第一句），不能只查「不许编」「占位符」这种词：
	// 第一版就是查两个词，而「占位符」在下面「宁可留占位符」里还有一份、压根没被删掉，
	// 于是注入删掉整条规则后断言照样绿 —— 又是裸子串假阳性（这个坑本项目记过账）。
	if !strings.Contains(sys, "send 里不许出现对话中没出现过的具体事实") {
		t.Fatalf("system prompt 没交代「send 不许编造具体事实」（整句都没了）：\n%s", sys)
	}
}

// 第一轮（助手还没答）要明确标注，否则模型会把空的 last_reply 当成
// 「助手啥也没说」，给出「再改一版」这种没有指代对象的建议。
func TestSuggestPrompt_FirstTurn(t *testing.T) {
	_, user := suggestPrompt(suggestReq{LastUser: "你好"})
	if !strings.Contains(user, "第一轮") {
		t.Fatalf("第一轮没被标注：\n%s", user)
	}
}

// 长回复必须被截：推荐行是「两秒内」的东西，整段交付说明塞进去会让这个调用
// 贵十倍，而它只需要知道「刚才干了什么」。
func TestSuggestPrompt_ClipsLongReply(t *testing.T) {
	head := strings.Repeat("前", 100)
	tail := strings.Repeat("尾", 100)
	reply := head + strings.Repeat("中", suggestReplyRunes) + tail
	_, user := suggestPrompt(suggestReq{LastUser: "改一版", LastReply: reply})
	if !strings.Contains(user, head) {
		t.Fatal("回复开头应保留（那是「刚才干了什么」）")
	}
	if strings.Contains(user, tail) {
		t.Fatalf("回复尾部应被截掉（应 ≤ %d 字）", suggestReplyRunes)
	}
}

// 技能清单封顶 12 条：首页技能可能几十个，全塞进 prompt 既贵又噪。
func TestSuggestPrompt_CapsSkillList(t *testing.T) {
	skills := make([]suggestSkill, 0, 15)
	for i := 0; i < 15; i++ {
		skills = append(skills, suggestSkill{Slug: "s", Name: "技能" + string(rune('A'+i))})
	}
	_, user := suggestPrompt(suggestReq{LastUser: "写点东西", Skills: skills})
	if !strings.Contains(user, "技能A") {
		t.Fatal("前几条技能应在 prompt 里")
	}
	if strings.Contains(user, "技能O") {
		t.Fatal("第 15 条技能不该进 prompt（封顶 12）")
	}
}

// —— 接线层 ——

// 走真 mux：handler 写了不注册 = 前端 404，而前端把 fetch 失败吞在 .catch 里，
// 页面上完全没有动静（这是最像「功能已上线」的失败）。
func TestSuggestRoute_RegisteredAndServesChips(t *testing.T) {
	f := newFakeLLM(t, func(string, string) string {
		return `{"chips":[{"label":"再精简一版","send":"把刚才那版精简到 300 字"}]}`
	})
	sk := newStoreForTest(t, t.TempDir())
	h, err := NewHandler(sk, llm.New(&model.LLMConfig{APIKey: "test", BaseURL: f.srv.URL, Model: "fake"}), "test-secret")
	if err != nil {
		t.Fatalf("建 handler 失败：%v", err)
	}

	body := `{"session_id":"s1","last_user":"写一份防汛通知","last_reply":"通知已生成","used_skill":"公文写作","skills":[{"slug":"gov","name":"公文写作"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/chat/suggest", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code == http.StatusNotFound {
		t.Fatal("/api/chat/suggest 没被注册（前端会静默降级，看不出问题）")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d，期望 200（UI 糖不许报错）：%s", rec.Code, rec.Body.String())
	}
	var got suggestResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON：%v（%s）", err, rec.Body.String())
	}
	if len(got.Chips) != 1 || got.Chips[0].Label != "再精简一版" {
		t.Fatalf("胶囊没透出来：%+v（body=%s）", got.Chips, rec.Body.String())
	}

	// 必须走 jsonMode：关掉它，模型就开始回围栏和客套话，
	// 而宽解析能救回来的只是「部分形态」——源头能省的事不该留给解析器。
	if last := f.lastReq(t); !last.JSONMode {
		t.Fatal("这次调用没开 jsonMode（response_format=json_object）")
	}
	// 思考链必须关掉。线上实测（Qwen3.6-27B）：开着 6.24s、关掉 1.38s，
	// 而这条路的预算是 5s —— 开着等于每次超时、接口 200 空数组、
	// 前端静默退回规则版推荐行，界面上完全看不出这个功能已经不存在了。
	if last := f.lastReq(t); !last.ThinkingOff {
		t.Fatal("这次调用没关思考链（enable_thinking=false）—— 线上会必然超时")
	}
	if last := f.lastReq(t); last.MaxTokens <= 0 {
		t.Fatal("这次调用没有 max_tokens 上限（思考链可能吃满 completion 预算）")
	}
	// 上下文真的到了模型那边（不是只到 handler 就丢了）。
	if last := f.lastReq(t); !strings.Contains(last.User, "防汛") {
		t.Fatalf("发出去的请求里没有用户那句话：\n%s", last.User)
	}
}

// 没配模型（线上常见：管理端没填 key）／引擎根本没接线时，都必须 200 + 空数组：
// 5xx 会让前端控制台红一片，而用户其实什么都没坏。
//
// ⚠️ 这两半的**可观测性不一样**，所以拆成两个子用例、写法也不同 —— 这条是
// 2026-09-26 注入自证逼出来的（原写法只有「有引擎但没模型」一半，注入自证
// 把「短路整条删掉」注进去后测试仍然全绿：假断言）：
//
//	· 引擎 nil：短路被删后 h.eng.FastJSON 会一路走到 nil 接收者的 e.mu.Lock()
//	  空指针 panic —— 这是公网端点最贵的故障形态（路由漏接线就是一片 500 /
//	  连接重置）。这一半测得出来，必须断言；recover 成 FAIL 是为了让报错里
//	  带「为什么」，而不是把 panic 原样抛给 runner。
//	· 有引擎但库里没模型：短路被删后 ensureLLM 返回 error，接口**仍然** 200
//	  空数组 —— 与短路路径观测等价。这一半是纵深防御（省掉 sem/超时那段
//	  无用功），作为判据它天生测不出东西，所以注入自证打的是 nil 那一半。
func TestSuggestRoute_NoLLMIsEmpty200(t *testing.T) {
	assertEmpty200 := func(t *testing.T, h *suggestHandler) {
		t.Helper()
		rec := httptest.NewRecorder()
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("handler panic 了（取不到模型时必须静默降级成空数组，不许把端点打成 500）：%v", r)
				}
			}()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/chat/suggest",
				strings.NewReader(`{"last_user":"写一份通知"}`)))
		}()
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 %d，期望 200", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"chips":[]`) {
			t.Fatalf("应回空数组，实得 %s", rec.Body.String())
		}
	}

	t.Run("引擎根本没接线（nil）", func(t *testing.T) {
		assertEmpty200(t, &suggestHandler{eng: nil, timeout: time.Second, sem: make(chan struct{}, 1)})
	})
	t.Run("引擎在但库里没模型", func(t *testing.T) {
		assertEmpty200(t, &suggestHandler{eng: agent.New(nil, nil), timeout: time.Second, sem: make(chan struct{}, 1)})
	})
}

// 用户没说过话（首页刚打开）→ 不该花一次模型调用去猜。
func TestSuggestRoute_EmptyInputShortCircuits(t *testing.T) {
	f := newFakeLLM(t, func(string, string) string { return `{"chips":[{"label":"瞎猜"}]}` })
	eng := newManualEngine(t, t.TempDir(), f)
	h := &suggestHandler{eng: eng, timeout: time.Second, sem: make(chan struct{}, 1)}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/chat/suggest", strings.NewReader(`{}`)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"chips":[]`) {
		t.Fatalf("空请求应短路成空数组，实得 %d %s", rec.Code, rec.Body.String())
	}
}

// 模型慢 → 必须按 handler 的超时放弃并回空数组（前端 6s 就 abort 了，
// 服务端不算超时就是在烧 token 给一个没人看的响应）。
func TestSuggestRoute_TimesOutAsEmpty(t *testing.T) {
	f := newFakeLLM(t, func(string, string) string {
		time.Sleep(1200 * time.Millisecond)
		return `{"chips":[{"label":"迟到的建议"}]}`
	})
	eng := newManualEngine(t, t.TempDir(), f)
	h := &suggestHandler{eng: eng, timeout: 150 * time.Millisecond, sem: make(chan struct{}, 1)}

	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/chat/suggest", strings.NewReader(`{"last_user":"写一份通知"}`)))
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d，期望 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"chips":[]`) {
		t.Fatalf("超时应回空数组，实得 %s", rec.Body.String())
	}
	if elapsed > 900*time.Millisecond {
		t.Fatalf("超时没生效：handler 等了 %s（上限 150ms）", elapsed)
	}
}

// 坏 body（前端发了个畸形 JSON）不能让推荐行 500，更不能 panic。
func TestSuggestRoute_BadBodyIsEmpty200(t *testing.T) {
	f := newFakeLLM(t, func(string, string) string { return "" })
	eng := newManualEngine(t, t.TempDir(), f)
	h := &suggestHandler{eng: eng, timeout: time.Second, sem: make(chan struct{}, 1)}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/chat/suggest", strings.NewReader(`{"last_user":`)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"chips":[]`) {
		t.Fatalf("畸形 body 应回空数组 200，实得 %d %s", rec.Code, rec.Body.String())
	}
}
