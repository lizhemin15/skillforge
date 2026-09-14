package api

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lizhemin15/skillforge/internal/agent"
	"github.com/lizhemin15/skillforge/internal/model"
)

// 守用户报的第一个问题。原话：「先让生成一个新闻稿，之后让 AI 把新闻稿整理成 word，
// 通常没有管之前的生成内容」。
//
// 三个刻意的设计，缺一个这道防线就是绿的假防线：
//
//  1. 锚点放在正文**尾部**（tailMark）。老实现 compactHistory 对每条消息只留头 400 字，
//     而新闻稿的结论/落款/联系方式都在后面。锚点放尾部，截断这种老行为才会变红；
//     放开头的话，被砍到 400 字的文本里照样有那段字，测试照样通过。
//  2. 断言「**整段正文**逐字出现」，不是只看关键词。关键词命中可能来自范文、
//     分类说明或审稿清单，证明不了「上一轮的产物被带过来了」。
//  3. 走**真接线**：runManualWrite → Push(KindArtifact) → 下一轮 Session() 读回 →
//     ContextBlock 注入 prompt。只测 ContextBlock 这个纯函数，就漏掉「调用处忘了把
//     history 传进去 / 忘了标 Kind」这一类真正会坑用户的错（本仓库已被突变量逮到过
//     同类问题，见 chat_seed_frame_test.go 开头的说明）。
func TestSecondTurnCarriesPreviousArticleVerbatim(t *testing.T) {
	dir, slug := newManualWriteFixture(t)

	const (
		tailMark = "【尾部标记-9f3a】"
		sent     = "第一轮生成的新闻稿正文，用于验证多轮上下文。"
	)
	body := strings.Repeat(sent, 20) + tailMark // > 400 字，尾部落在老实现的截断线之外

	var mu sync.Mutex
	var frames []string
	write := func(ev, data string) {
		mu.Lock()
		frames = append(frames, ev+":"+data)
		mu.Unlock()
	}

	f := newFakeLLM(t, func(system, user string) string {
		switch {
		case strings.Contains(system, "分类路由器"):
			return `{"category":"新闻通稿","confidence":"high","reason":"测试命中"}`
		case strings.Contains(system, "审稿人"):
			return `{}` // 零问题 → 审稿通过，不触发改稿轮
		case strings.Contains(system, "修改者"):
			return "（不该被调用）"
		}
		// 起草调用：第二轮（要求整理成 Word）返回一份排版后的稿子，
		// 断言的是「发出去的 prompt」而不是它，所以这里只需可区分。
		if strings.Contains(strings.ToLower(user), "word") {
			return "（Word 版）" + body
		}
		return body
	})
	eng := newManualEngine(t, dir, f)

	pack, err := eng.LoadWritePack(slug)
	if err != nil || pack == nil {
		t.Fatalf("准备素材失败：pack=%v err=%v", pack, err)
	}
	sc := &agent.SkillContent{Slug: slug, Name: "手册写作", SkillType: model.SkillTypeWrite,
		SystemPrompt: "你是公文写作助手，按手册要求写作。"}
	h := &chatHandler{eng: eng, maxRound: 1}

	const session = "s-multiturn"
	call := func(userMsg string) {
		t.Helper()
		// 与 chat.go 一致：用户消息先入会话，再进本轮流程。少这一步，测试里的
		// 会话就比线上少一条消息，「历史注入」测的就不是线上那份历史了。
		eng.Push(session, agent.Message{Role: "user", Content: userMsg, At: time.Now()})
		clock := newTraceClockBeat(write, nil, time.Hour) // 心跳调到 1h：测试不该被节拍影响
		if !h.runManualWrite(context.Background(), write, clock, sc, pack,
			map[string]string{}, userMsg, session, eng.Session(session)) {
			t.Fatalf("runManualWrite 未处理本轮请求（userMsg=%q）", userMsg)
		}
	}

	// ---- 第一轮：写新闻稿 ----
	call("写一篇园区开放日活动的新闻通稿")
	hist := eng.Session(session)
	if len(hist) == 0 {
		t.Fatal("第一轮结束后会话里没有消息")
	}
	artifact := false
	for _, m := range hist {
		if m.Role == "assistant" && strings.Contains(m.Content, tailMark) {
			artifact = true
			if m.Kind != agent.KindArtifact {
				t.Fatalf("第一轮产物没标 Kind=artifact（Kind=%q）—— 下一轮注入时会走对话层被截断", m.Kind)
			}
		}
	}
	if !artifact {
		t.Fatal("第一轮产物没进会话历史：第二轮「把它整理成 word」就没有可指代的对象")
	}

	// ---- 第二轮：把上面那篇整理成 Word ----
	n1 := len(f.prompts())
	call("把上面这篇新闻稿整理成 Word 文档")

	second := f.since(n1)
	if len(second) == 0 {
		t.Fatal("第二轮一个模型请求都没发出")
	}
	// ⚠️ 断言必须锚定**起草那一次请求**，不能用「第二轮所有 prompt 拼起来找一遍」。
	// 判类请求也吃 history，正文从那条路进去同样能被找到——于是把起草处的
	// ContextBlock 整个换成空字符串，测试照样是绿的（实测逮到过：注入旧行为后
	// 依然 ok）。一条谁都拦不住的断言不是防线，是装饰。
	draftReq := findRequestBySystem(second, sc.SystemPrompt)
	if draftReq == nil {
		t.Fatalf("第二轮没有发出起草请求（收到的 system 前缀：%s）", systemPrefixes(second))
	}
	draftPrompt := draftReq.System + "\n" + draftReq.User
	mustContain(t, draftPrompt, body, "第二轮**起草** prompt 里的上一轮正文（逐字、含尾部）")
	mustContain(t, draftPrompt, tailMark, "第二轮起草 prompt 里的正文尾部锚点")
	// 光有正文还不够：得是**走产物层**进来的。产物层是「逐字保留、不压缩」的那一条路，
	// 正文从近轮层混进来时看着也有，但下一条长消息就会把它挤掉。
	mustContain(t, draftPrompt, "【已有产物（原文，逐字保留）】", "第二轮起草 prompt 里的产物层标题")
	mustContain(t, draftPrompt, "把上面这篇新闻稿整理成 Word 文档", "第二轮起草 prompt 里了解到的本轮诉求")

	// 反向自证（注入旧行为必须变红）放在 agent 包的 TestOldBehaviorIsRed 里做：
	// 那里能直接调被替换掉的截断逻辑复现旧行为，证明「尾部锚点」这个断言有牙齿。
	// 这里不重复，是因为从 api 包去调 agent 的内部实现只能靠导出测试专用函数——
	// 为了断言方便给生产代码开后门，是拿架构换测试的便宜，不划算。
	//
	// 另一条自证已经在本地做过：把 chat_write.go 起草处的 ContextBlock 换成 ""，
	// 本用例必须 red（第一次写的时候它居然是绿的，才逼出上面那条「锚定起草请求」）。
}

// since 取第 n 个请求之后的所有请求（换轮次时用来切分「这一轮发了什么」）。
func (f *fakeLLM) since(n int) []fakeLLMReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n >= len(f.reqs) {
		return nil
	}
	out := make([]fakeLLMReq, 0, len(f.reqs)-n)
	out = append(out, f.reqs[n:]...)
	return out
}

// findRequestBySystem 在若干请求里找 system 里带 marker 的那一个（起草调用的 system
// 里必然含技能自身的提示词，这是它区别于判类/审稿调用的稳定特征）。
func findRequestBySystem(reqs []fakeLLMReq, marker string) *fakeLLMReq {
	for i := range reqs {
		if strings.Contains(reqs[i].System, marker) {
			return &reqs[i]
		}
	}
	return nil
}

// systemPrefixes 给失败信息用：断言找不到目标请求时，得知道这轮实际发了哪些请求，
// 否则只能靠再跑一遍加打印来猜。
func systemPrefixes(reqs []fakeLLMReq) string {
	parts := make([]string, 0, len(reqs))
	for _, r := range reqs {
		parts = append(parts, runeClipForTest(strings.ReplaceAll(r.System, "\n", " "), 60))
	}
	return strings.Join(parts, " | ")
}
