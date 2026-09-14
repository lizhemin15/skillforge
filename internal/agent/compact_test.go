package agent

// 上下文管理的回归测试。
//
// 这些用例锁的是用户真实反馈过的一句抱怨：「先让它生成一个新闻稿，再让它把
// 新闻稿整理成 word，它通常没有管之前生成的内容」。
//
// 双向自证：每条断言都先确认「修好之后是绿的」，再注入旧行为确认「照红」——
// 旧行为就是把 compactHistory 换回「每条只留头 400 字」，见 TestContextOldBehaviorIsRed。

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// newsBody 造一份「正文很长、头尾都有辨识特征」的产物。
// 尾部特征（NEWS_END）专门用来照「只保头」的旧实现——旧实现保不住尾。
func newsBody() string {
	var b strings.Builder
	b.WriteString("NEWS_HEAD 关于举办2026年度数据治理培训的通知\n")
	for i := 0; i < 400; i++ {
		b.WriteString("各单位要高度重视本次培训，按时组织人员参加，做好签到与考核记录。")
	}
	b.WriteString("\n联系人：张三，电话 13800138000。NEWS_END 特此通知。\n")
	return b.String()
}

func msgs(pairs ...string) []Message {
	var out []Message
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Message{Role: pairs[i], Content: pairs[i+1], At: time.Now()})
	}
	return out
}

// Test_产物正文逐字进上下文 —— 用户场景：写完新闻稿，下一轮要整理成 word。
func TestArtifactKeptVerbatim(t *testing.T) {
	e := New(nil, nil)
	body := newsBody()
	hist := []Message{
		{Role: "user", Content: "写一篇关于数据治理培训的新闻稿", At: time.Now()},
		{Role: "assistant", Content: body, SkillSlug: "news-writer", Kind: KindArtifact, At: time.Now()},
		{Role: "user", Content: "把它整理成 word", At: time.Now()},
	}
	got := e.ContextBlock(context.Background(), "s1", hist)

	if !strings.Contains(got, "NEWS_HEAD") {
		t.Fatalf("产物正文开头没进上下文（模型看不到稿子，就会另起炉灶重写）")
	}
	if !strings.Contains(got, "NEWS_END") {
		t.Fatalf("产物正文**结尾**丢失——旧实现只留头 400 字，尾部（含联系人/落款）全被砍掉")
	}
	// 关键句必须逐字在，不能被摘要化成「一篇新闻稿」。
	if !strings.Contains(got, "13800138000") {
		t.Fatalf("产物里的具体数字（电话）被压缩掉了——摘要里的数字进正式文档就是事故")
	}
	if !strings.Contains(got, "已有产物（原文") {
		t.Fatalf("缺少产物层标题，模型不知道下面这段是要以它为基础改的")
	}
}

// TestArtifactFromDiskHasNoKind —— 会话从磁盘恢复时没有 Kind 字段（老数据），
// 仍须被认成产物，否则「重启一次之后就忘了上一轮写了什么」。
func TestArtifactFromDiskHasNoKind(t *testing.T) {
	e := New(nil, nil)
	body := newsBody()
	hist := []Message{
		// 老落盘消息：没有 Kind，只有 SkillSlug + 长正文
		{Role: "assistant", Content: body, SkillSlug: "news-writer", At: time.Now()},
		{Role: "user", Content: "整理成 word", At: time.Now()},
	}
	got := e.ContextBlock(context.Background(), "s1", hist)
	if !strings.Contains(got, "NEWS_END") {
		t.Fatalf("磁盘恢复的产物没被当产物：历史兼容分支失效")
	}
}

// TestPlainChatLongAnswerIsArtifact —— 没命中技能时由普通对话产出的正文，
// 也必须是产物（否则「写篇新闻稿→整理成 word」在无技能场景下仍然丢稿子）。
func TestPlainChatLongAnswerIsArtifact(t *testing.T) {
	if ArtifactKind("短句，不算产物") != "" {
		t.Fatalf("短回答不该占用产物额度")
	}
	if ArtifactKind(strings.Repeat("字", 300)) != KindArtifact {
		t.Fatalf("长正文（≥200 字）应被认成产物")
	}
	e := New(nil, nil)
	hist := []Message{
		{Role: "assistant", Content: newsBody(), SkillSlug: "", Kind: ArtifactKind(newsBody()), At: time.Now()},
		{Role: "user", Content: "整理成 word", At: time.Now()},
	}
	if got := e.ContextBlock(context.Background(), "s1", hist); !strings.Contains(got, "NEWS_END") {
		t.Fatalf("无技能产出的长正文丢了尾部")
	}
}

// TestOldBehaviorIsRed —— 注入旧行为（每条只留头 400 字）必须照红。
// 这是给「回归防线本身是假的」留的证据：如果哪天有人把 ContextBlock 改回截断，
// 这条用例会失败。
func TestOldBehaviorIsRed(t *testing.T) {
	body := newsBody()
	// 旧行为复刻：只留头 400 字
	c := body
	if len(c) > 400 {
		c = c[:400] + "…"
	}
	if strings.Contains(c, "NEWS_END") {
		t.Fatalf("旧行为的复刻写错了：断言无法区分新旧行为，这条防线是假的")
	}
	if !strings.Contains(newsBody(), "NEWS_END") {
		t.Fatalf("测试料本身没有尾部特征，防线无效")
	}
}

// TestHeadTailKeepsTail —— 长消息在「近轮层」也要两头都保。
func TestHeadTailKeepsTail(t *testing.T) {
	s := "HEAD" + strings.Repeat("中", 5000) + "TAIL"
	got := truncHeadTail(s, 600)
	if !strings.HasPrefix(got, "HEAD") || !strings.HasSuffix(got, "TAIL") {
		t.Fatalf("头尾截断没保住尾部：%q...%q", got[:20], got[len(got)-20:])
	}
	if !strings.Contains(got, "省略约") {
		t.Fatalf("省略处没有标注，模型会把残文当完整文本用")
	}
	if n := len([]rune(got)); n > 700 {
		t.Fatalf("截断后仍然过长：%d runes", n)
	}
}

// TestOldDialogueCompressedByLLM —— 旧对话超阈值时**调用大模型压缩**（而不是硬截断）。
func TestOldDialogueCompressedByLLM(t *testing.T) {
	e := New(nil, nil)
	var calls int
	e.summarize = func(ctx context.Context, raw string) (string, error) {
		calls++
		if !strings.Contains(raw, "第一条约定") {
			t.Fatalf("压缩输入里没有早期内容，说明筛错了范围")
		}
		return "- 单位=某某公司；日期=2026-09-14；已完成新闻稿一篇", nil
	}

	// 早期这条必须是**短**消息：长到 materialMinRunes(1200) 就会被认成「用户素材」
	// 并走素材层逐字保留（那是有意为之——素材不该被压缩成摘要），
	// 于是它不会出现在压缩输入里，这条用例也就测不到压缩路径了。
	hist := []Message{{Role: "user", Content: "第一条约定：按单位公文格式办", At: time.Now()}}
	for i := 0; i < 8; i++ {
		// 单条 600 字：旧对话总量要真的超过 compactTriggerRunes(4000)，
		// 否则压缩路径根本不会触发，这条用例会变成空跑（假绿）。
		hist = append(hist,
			Message{Role: "user", Content: "追问" + strings.Repeat("还", 600), At: time.Now()},
			Message{Role: "assistant", Content: "回答" + strings.Repeat("答", 600), At: time.Now()})
	}
	hist = append(hist, Message{Role: "user", Content: "继续", At: time.Now()})

	got := e.ContextBlock(context.Background(), "s2", hist)
	if calls != 1 {
		t.Fatalf("应恰好压缩一次，实际 %d 次", calls)
	}
	if !strings.Contains(got, "单位=某某公司") {
		t.Fatalf("压缩结果没进上下文：%s", got[:min(200, len(got))])
	}
	if !strings.Contains(got, "前情摘要") {
		t.Fatalf("缺少前情摘要标记，模型分不清哪些是原文哪些是摘要")
	}

	// 同一段旧对话再来一轮：走缓存，不该再压一次（否则每轮对话都多一次模型调用）。
	hist = append(hist, Message{Role: "assistant", Content: strings.Repeat("新", 200), At: time.Now()},
		Message{Role: "user", Content: "再问", At: time.Now()}) // 只多 2 条，不该重新压缩
	e.ContextBlock(context.Background(), "s2", hist)
	if calls != 1 {
		t.Fatalf("watermark 缓存失效：同样的旧对话被重复压缩（%d 次）", calls)
	}
}

// TestCompressFailureFallsBack —— 压缩失败必须退回截断并留痕，不能整轮对话挂掉。
func TestCompressFailureFallsBack(t *testing.T) {
	e := New(nil, nil)
	e.summarize = func(ctx context.Context, raw string) (string, error) {
		return "", errors.New("model down")
	}
	hist := []Message{{Role: "user", Content: "开场：" + strings.Repeat("事", 6000), At: time.Now()}}
	for i := 0; i < 8; i++ {
		hist = append(hist,
			Message{Role: "user", Content: "问" + strings.Repeat("x", 600), At: time.Now()},
			Message{Role: "assistant", Content: "答" + strings.Repeat("y", 600), At: time.Now()})
	}
	hist = append(hist, Message{Role: "user", Content: "收尾：最新要求", At: time.Now()})

	got := e.ContextBlock(context.Background(), "s3", hist)
	if !strings.Contains(got, "压缩失败") {
		t.Fatalf("降级没有留痕，模型和用户都不知道发生了什么")
	}
	if !strings.Contains(got, "最新要求") {
		t.Fatalf("降级后最近一轮丢了——降级也必须保住近轮原文")
	}
}

// TestSmallTalkNoModelCall —— 小会话不触发压缩调用（省一次模型调用的钱和延迟）。
func TestSmallTalkNoModelCall(t *testing.T) {
	e := New(nil, nil)
	e.summarize = func(ctx context.Context, raw string) (string, error) {
		t.Fatalf("几句闲聊不该触发压缩调用")
		return "", nil
	}
	hist := msgs("user", "你好", "assistant", "你好，有什么可以帮你", "user", "没事")
	if got := e.ContextBlock(context.Background(), "s4", hist); !strings.Contains(got, "你好") {
		t.Fatalf("小会话内容应原样注入：%q", got)
	}
}

// TestNoHistory —— 空历史不 panic、不产出半截块。
func TestNoHistory(t *testing.T) {
	e := New(nil, nil)
	if got := e.ContextBlock(context.Background(), "s5", nil); got != "（无历史）" {
		t.Fatalf("空历史渲染异常：%q", got)
	}
}

// ---------------------------------------------------------------------------
// 用户素材层（与「助手产物层」对称）
//
// 用户实测原话（2026-09）：「我手动添加了一个一万字的写作要求和示例来创建 skill，
// 然后直接提出一个写作需求的时候，有时候似乎像是没看到我的信息一样，还在问我
// 要信息。要的时候也不是根据我目前提供的信息的基础上来进一步补充，而是直接
// 通用的补充。还有就是我多轮对话的时候，似乎就忘了我之前问了什么，以及你自己
// 回答了什么。」
//
// 两个根因都锁在这里：
//  1. 用户贴的长素材落进「近轮层」被 truncHeadTail 砍成 600 字头尾
//     （实测 11992 字 → 678 字，中段条款全丢）→ 模型只看得见头尾，于是「像没
//     看到我的信息一样」回头追问通用的主题/篇幅/语气；
//  2. maxHist=12 比压缩层先动手，超出 6 轮的内容**轮不到被压缩就整段消失**。
// ---------------------------------------------------------------------------

// longRequirement 造一份「用户贴进来的写作要求 + 范文」：
// 开头是背景，中段才是真正的条款，尾部是范文。中段这条必须能被逐字看到——
// 看不到，模型就只能问「请告诉我具体要求」，也就是用户抱怨的那句
// 「不是根据我目前提供的信息的基础上来进一步补充，而是直接通用的补充」。
func longRequirement() string {
	var b strings.Builder
	b.WriteString("REQ_HEAD 这是我们单位的公文写作要求和范文，请照此办理。\n")
	for i := 0; i < 120; i++ {
		b.WriteString("一、标题用二号方正小标宋，正文三号仿宋，行距 28 磅；二、主送机关顶格；三、成文日期用阿拉伯数字。\n")
	}
	b.WriteString("REQ_MID 第十五条：涉及金额的必须写明人民币大写；第十九条：落款须加盖公章。\n")
	for i := 0; i < 120; i++ {
		b.WriteString("四、正文层次序数依次用「一、」「（一）」「1.」「（1）」表示；五、附件说明另起一行左空二字。\n")
	}
	b.WriteString("\n范文示例：关于开展年度数据治理专项工作的通知……REQ_TAILEND\n")
	return b.String()
}

// Test_用户素材逐字进上下文 —— 用户贴的一万字要求，下一轮必须还是那一万字。
func TestUserMaterialKeptVerbatim(t *testing.T) {
	e := New(nil, nil)
	req := longRequirement()
	if n := len([]rune(req)); n < 9000 {
		t.Fatalf("测试料前提不成立：只有 %d 字，够不到用户实测的「一万字」量级", n)
	}
	hist := []Message{
		{Role: "user", Content: req, At: time.Now()},
		{Role: "assistant", Content: "好的，我已了解这份写作要求。", At: time.Now()},
		{Role: "user", Content: "按这个要求写一份关于开展数据治理专项工作的通知", At: time.Now()},
	}
	got := e.ContextBlock(context.Background(), "mat1", hist)

	if !strings.Contains(got, "【用户提供的要求与素材（原文，逐字保留）】") {
		t.Fatalf("缺少素材层标题，模型不知道下面那段是用户给的要求：\n%s", runeClip(got, 300))
	}
	// 逐字判据落在「中段」上：只保头尾的实现也能命中头尾标记，命中不了中段条款。
	for _, marker := range []string{"REQ_HEAD", "REQ_MID", "第十九条", "人民币大写", "REQ_TAILEND"} {
		if !strings.Contains(got, marker) {
			t.Errorf("素材里的 %s 没进上下文——模型看不到这条要求，只能通用追问", marker)
		}
	}
	// 长度判据（结构化事实，不扫「要求/范文」这类关键词：关键词随文案一变就漏）：
	// 注入块不可能比素材本身还短，短了就是被砍过。
	if n, m := len([]rune(got)), len([]rune(req)); n < m {
		t.Errorf("注入块 %d 字 < 素材 %d 字，说明素材又被截断了", n, m)
	}
}

// Test_用户素材在Push时打标 —— 打标是唯一入口，漏掉就等于素材层不存在。
// 只认 role=user：助手的长回复归产物层，两者额度独立，不能互相顶掉。
func TestUserMaterialMarkedByPush(t *testing.T) {
	e := New(nil, nil)
	req := longRequirement()

	e.Push("mat2", Message{Role: "user", Content: req, At: time.Now()})
	e.Push("mat2", Message{Role: "user", Content: "你好", At: time.Now()})

	sess := e.Session("mat2")
	if len(sess) != 2 {
		t.Fatalf("Push 后会话条数异常：%d", len(sess))
	}
	if sess[0].Kind != KindMaterial {
		t.Errorf("用户贴的长素材没有被打成素材（Kind=%q）——下一轮它就会被砍成 600 字头尾", sess[0].Kind)
	}
	if sess[1].Kind != "" {
		t.Errorf("短消息被误判成素材（Kind=%q）：素材判定一松，闲聊也会占满 24000 字额度", sess[1].Kind)
	}
	if got := e.ContextBlock(context.Background(), "mat2", sess); !strings.Contains(got, "REQ_MID") {
		t.Errorf("经 Push 落库的素材，中段条款没进上下文")
	}
}

// Test_素材从对话流里摘干净 —— 既不重复注入两遍，也不在近轮层被截成残文。
func TestMaterialNotDoubleInjected(t *testing.T) {
	e := New(nil, nil)
	req := longRequirement()
	hist := []Message{
		{Role: "user", Content: req, At: time.Now()},
		{Role: "user", Content: "按上面要求写通知", At: time.Now()},
	}
	got := e.ContextBlock(context.Background(), "mat3", hist)
	if n := strings.Count(got, "REQ_MID"); n != 1 {
		t.Errorf("素材在上下文里出现 %d 次（期望 1 次）：重复注入既费额度，也让模型以为用户给了两遍", n)
	}
	if !strings.Contains(got, "按上面要求写通知") {
		t.Errorf("素材被摘走后，同一条真实对话消息也不能丢")
	}
}

// Test_多轮之后最初的素材仍在 —— 用户：「多轮对话的时候，似乎就忘了我之前问过什么」。
//
// 走**真实**的 Session() 裁剪路径（把完整历史塞进引擎，让它自己裁），
// 不在测试里复刻裁剪逻辑——复刻出来的实现等于自己给自己发合格证。
func TestEarlyMaterialSurvivesManyTurns(t *testing.T) {
	e := New(nil, nil)
	req := longRequirement()
	hist := []Message{{Role: "user", Content: req, At: time.Now()}}
	// 聊到远超旧 maxHist=12 的轮数。
	for i := 0; i < 20; i++ {
		hist = append(hist,
			Message{Role: "user", Content: "补充第 " + string(rune('一'+i)) + " 点：请把落款写成单位全称", At: time.Now()},
			Message{Role: "assistant", Content: "收到，已记录。", At: time.Now()})
	}
	hist = append(hist, Message{Role: "user", Content: "按最开始那份要求写一份通知", At: time.Now()})

	if len(hist) <= e.maxHist {
		t.Fatalf("测试料前提不成立：%d 条没超过 maxHist=%d，这条防线测不到东西", len(hist), e.maxHist)
	}
	e.full["mat4"] = hist
	win := e.Session("mat4")

	got := e.ContextBlock(context.Background(), "mat4", win)
	if !strings.Contains(got, "REQ_MID") {
		t.Errorf("聊了 %d 条之后，最开始贴的要求整个消失了（Session 裁剪所致）", len(hist))
	}
	if !strings.Contains(got, "按最开始那份要求写一份通知") {
		t.Errorf("用户的最新一句没进上下文")
	}
	// 窗口仍必须有上界：钉住证据 ≠ 窗口无限膨胀。
	if len(win) > e.maxHist+4 {
		t.Errorf("裁剪后窗口 %d 条（maxHist=%d）：钉住的消息不该让窗口失去上界", len(win), e.maxHist)
	}
}

// Test_会话裁剪钉住产物 —— 用户：「似乎就忘了我…以及你自己回答了什么」。
func TestSessionTrimPinsArtifact(t *testing.T) {
	e := New(nil, nil)
	long := "ART_HEAD " + strings.Repeat("这是上一轮写好的新闻稿正文。", 200) + " ART_TAIL"
	hist := []Message{{Role: "assistant", Content: long, Kind: KindArtifact, At: time.Now()}}
	for i := 0; i < 30; i++ {
		hist = append(hist,
			Message{Role: "user", Content: "嗯嗯，我再想想", At: time.Now()},
			Message{Role: "assistant", Content: "好的。", At: time.Now()})
	}
	hist = append(hist, Message{Role: "user", Content: "把刚才那篇改成正式一点的语气", At: time.Now()})

	e.full["mat6"] = hist
	got := e.ContextBlock(context.Background(), "mat6", e.Session("mat6"))
	if !strings.Contains(got, "ART_HEAD") || !strings.Contains(got, "ART_TAIL") {
		t.Errorf("聊了 %d 条之后，之前写好的稿子被窗口切走了——用户只好说「它不管我上一轮写的东西」", len(hist))
	}
	if !strings.Contains(got, "把刚才那篇改成正式一点的语气") {
		t.Errorf("用户的最新一句没进上下文")
	}
}

// Test_素材感知的反问 —— 用户已经贴了素材时，「信息不足」不能问通用清单。
func TestClarifyQuestionIsMaterialAware(t *testing.T) {
	if hasMaterial(nil) {
		t.Fatalf("空历史被判成「有素材」")
	}
	short := msgs("user", "帮我填一下验收单", "assistant", "好的")
	if hasMaterial(short) {
		t.Fatalf("短对话被判成「有素材」——没给素材时问通用清单是合理的，判错会把正常反问也改掉")
	}
	withMat := append([]Message{{Role: "user", Content: longRequirement(), At: time.Now()}}, short...)
	if !hasMaterial(withMat) {
		t.Fatalf("贴了一万字要求却没被认成「有素材」——这时的通用追问就是在让用户从头再说一遍")
	}
}

// TestContextOldBehaviorIsRed —— 注入旧行为必须照红。
//
// 旧行为复刻两件事，缺一不可：
//  1. 素材当普通对话消息，被近轮层 truncHeadTail 砍成 600 字头尾；
//  2. 窗口纯按时间切，超过 maxHist 的素材整条掉出去。
//
// 这条用例证明上面几条断言**真的能区分新旧行为**——不是恰好都绿的假防线。
func TestMaterialOldBehaviorIsRed(t *testing.T) {
	req := longRequirement()
	old := strings.TrimSpace(truncHeadTail(req, compactMsgRunes))

	if strings.Contains(old, "REQ_MID") {
		t.Fatalf("旧行为的复刻写错了：截断后中段条款居然还在，这条防线是假的")
	}
	if !strings.HasPrefix(old, "REQ_HEAD") || !strings.HasSuffix(old, "REQ_TAILEND") {
		t.Fatalf("旧行为复刻不像头尾截断：头尾都保不住说明复刻写歪了")
	}

	// 旧裁剪：纯时间窗，素材在窗口外就没了。
	hist := []Message{{Role: "user", Content: req, At: time.Now()}}
	for i := 0; i < 30; i++ {
		hist = append(hist, Message{Role: "user", Content: "嗯", At: time.Now()})
	}
	oldWin := hist[len(hist)-12:] // 旧 maxHist=12
	e := New(nil, nil)
	if got := e.ContextBlock(context.Background(), "mat5", oldWin); strings.Contains(got, "REQ_MID") {
		t.Fatalf("旧行为的窗口复刻写错了：素材早就该被切出窗口，这条防线是假的")
	}

	// 反向自证：新行为在同一份料上必须保住中段（逐字，不是头尾）。
	got := e.ContextBlock(context.Background(), "mat5", []Message{
		{Role: "user", Content: req, At: time.Now()},
		{Role: "user", Content: "按这个写", At: time.Now()},
	})
	if !strings.Contains(got, "REQ_MID") {
		t.Fatalf("新行为也没保住中段，那这条「双向自证」是空转")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = os.Stderr
