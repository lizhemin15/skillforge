package api

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lizhemin15/skillforge/internal/agent"
)

// ===== 手册模式写作：分类 → 按类执笔 → 审稿 → 改稿 =====
//
// 训练期抽出的手册素材（分类要求 + 真实范文 + 审稿清单）本身是**文件**，挂在技能
// 目录下。运行时模型读不到文件，只能靠 system prompt 里那句「打开 categories/ 下
// 该类对应的文件」——那对纯对话的模型来说是张空头支票，实际结果是它凭分类名瞎猜
// 要求、凭记忆编范文。这个文件就是补上「把文件真的喂进去」这一段。
//
// 三道关的顺序与技能提示词里写好的工作协议一致，改动这里要同步改那边，否则模型
// 会以为自己在走另一套流程。

// maxReviewRounds 是审稿轮数上限（已获授权：审稿 2 轮为上限）。
// 设上限是因为「审→改→审」可以无限循环下去：模型每轮都能挑出新毛病，而稿件质量
// 在第二轮之后基本不再提升，用户看到的是无限等待。
const maxReviewRounds = 2

// runManualWrite 执行手册模式的三段式写作。返回 true 表示本轮已处理完毕
// （已下发 done 或 error），handler 应直接 return。
func (h *chatHandler) runManualWrite(
	ctx context.Context,
	write func(ev, data string),
	clock *traceClock,
	sc *agent.SkillContent,
	pack *agent.WritePack,
	args map[string]string,
	userMsg, sessionID string,
	history []agent.Message,
) bool {
	// ---- 第一段：判类 ----
	// 步骤流从骨架接手，往后只追加、不回改（为什么这么设计见 chat_write_trace.go）。
	tb := newTraceBoard()
	tb.Carry(clock.Steps())
	clock.Set(tb.Steps())

	// 判类先摆成「进行中」再发出去：它是一次几十秒的阻塞调用，让用户看见
	// 「正在按手册的触发场景判断走哪一类」，比让第一格一直空转有用。
	routeIdx := tb.Active("match", "② 手册分类", "按手册各分类的触发场景判断这条需求该走哪一类…")
	clock.Set(tb.Steps())

	route, err := h.eng.RouteCategory(ctx, pack, userMsg, history)
	if err != nil {
		write(evError, jsonSafe(map[string]string{"error": "分类判定失败: " + err.Error()}))
		return true
	}
	cat := pack.FindCategory(route.Category)
	if route.Ambiguous || cat == nil {
		// 判不出类别时**不能**硬着头皮写：猜错类别等于整篇按错的要求写，用户还得
		// 自己发现。手册里还留着一类自查清单，所以这里把类目摆出来让用户点。
		tb.Close(routeIdx, "没找到明确对应的一类，把类目列给你挑")
		tb.Active("generate", "等待你指定写作类别", "按你指定的那一类的要求与范文起草")
		clock.Set(tb.Steps())
		msg := ambiguousMessage(pack, route.Reason)
		clock.Awaiting("等待确认写作类别")
		write(evDelta, jsonSafe(map[string]string{"t": msg}))
		h.eng.Push(sessionID, agent.Message{Role: "assistant", Content: msg, SkillSlug: sc.Slug, At: time.Now()})
		write(evDone, jsonSafe(map[string]string{"skill": sc.Slug, "asked": "true"}))
		return true
	}

	// 分类命中要显性化：用户得看见「它按哪一类的要求写的」，否则稿件不合预期时
	// 无从判断是分类错了还是写作错了。这一格从此定格，后面的步骤不会再覆盖它。
	catName := strings.TrimSpace(cat.Name)
	if catName == "" {
		catName = cat.File
	}
	catDetail := hitDetail(catName, route.Confidence, route.Reason)
	tb.Close(routeIdx, catDetail)

	// ③ 把「喂进去的是什么素材」摊开：这一环最容易出错（路由错类 = 整篇按错的
	// 要求写），也是事后唯一能自证的一句。
	tb.Done("params", "③ 对齐要求与范文", alignDetail(cat))
	clock.Set(tb.Steps())

	// ---- 第二段：按类执笔 ----
	// 起草**不把初稿刷进正文区**（后面还有审稿和改稿，刷两遍用户会以为生成了两篇），
	// 但**必须把初稿当中间材料滚出来**：起草是整条链路最长的一跳，以前 onDelta 传 nil，
	// 于是这一跳几十秒里屏幕上只有步骤详情里的「已用 Ns」在跳——正是用户抱怨的
	// 「一直卡着计时」。材料挂在步骤面板里，不进正文，所以不会出现「两篇」的错觉。
	draftIdx := tb.Active("generate", "④ 起草初稿", fmt.Sprintf("按《%s》的写作要求与本类范文起草…", catName))
	clock.Set(tb.Steps())
	// 起草前先把本会话前文（尤其是上一轮产物的原文）交给模型——续改类请求
	// （「把语气改成公文」「在上一篇后面加一段」）全靠它，否则每轮都是重写。
	prior := h.eng.ContextBlock(ctx, sessionID, history)
	// 本地事实先报：命中哪一类、几篇范文、有没有审稿清单、上文多少字。
	// 模型侧的材料什么时候来不由我们决定（provider 不推 reasoning 时一片都没有）。
	facts := noteFacts(sc, args, history)
	facts.Category, facts.Examples = catName, len(cat.Examples)
	facts.HasReview = strings.TrimSpace(pack.Reviewer) != ""
	clock.Thinking(facts.note())
	// 起草同样是一次无上界的阻塞调用（实测静默几分钟、provider 一片 reasoning 都不推），
	// 本地旁白顶上：滚的是「命中哪一类、几篇范文、要素是什么」，全是真值。
	stopNarr := clock.Narrate(narrateLines("", facts.note()))
	draft, err := h.eng.GenerateWithPack(ctx, sc, pack, cat, args, prior, userMsg, clock.Thinking)
	stopNarr()
	if err != nil {
		write(evError, jsonSafe(map[string]string{"error": "生成失败: " + err.Error()}))
		return true
	}
	draft = strings.TrimSpace(draft)
	if draft == "" {
		write(evError, jsonSafe(map[string]string{"error": "生成失败: 模型返回了空稿件"}))
		return true
	}
	tb.Close(draftIdx, fmt.Sprintf("初稿 %d 字，转入审稿", runeLen(draft)))
	clock.Set(tb.Steps())

	// ---- 第三段：审稿（多轮，有上限）----
	final, leftover, reviewNote := h.reviewAndRevise(ctx, clock, tb, sc, pack, cat, userMsg, draft)

	// ---- 交付 ----
	// 交付这一格保持 active 直到 clock.Finish() 收口：中途若没有进行中的步骤，
	// 前端会以为跑完了自动把时间线收起来，而这时终稿还在一个字一个字地刷。
	tb.Active("generate", "交付", fmt.Sprintf("交付终稿 %d 字", runeLen(final)))
	clock.Set(tb.Steps())
	streamDeltas(write, final)
	if reviewNote != "" {
		// 提示只发给用户看，不写进会话历史：历史是给模型接着改稿用的，混进一堆
		// 审稿结论会让下一轮「把第 2 段改短点」被当成新的审稿要求。
		write(evDelta, jsonSafe(map[string]string{"t": "\n\n---\n" + reviewNote}))
		_ = leftover
	}
	h.eng.Push(sessionID, agent.Message{Role: "assistant", Content: final, SkillSlug: sc.Slug, Kind: agent.KindArtifact, At: time.Now()})
	clock.Finish()
	if strings.TrimSpace(sc.Attachment) != "" {
		write(evFile, jsonSafe(map[string]string{
			"slug": sc.Slug, "name": sc.Attachment,
			"url": "/api/chat/attachment/" + sc.Slug,
		}))
	}
	write(evDone, jsonSafe(map[string]string{"skill": sc.Slug}))
	return true
}

// reviewAndRevise 跑审稿循环，返回最终稿件、最后一轮仍存疑的问题、给用户的一句提示。
//
// 每轮审稿、每次改稿都各占一格（append-only）：跑完留下的时间线是「审稿·第1轮 →
// 2 条问题 → 修订·第1轮 → 审稿·第2轮 → 通过」这样一条账。以前这些信息全挤在一格里
// 反复覆盖，用户事后复盘「审了几轮、每轮挑出什么、最后过没过」时，屏幕上只剩最后
// 那句「正在核对…」——三件最该看见的事恰好都被覆盖掉了。
func (h *chatHandler) reviewAndRevise(
	ctx context.Context,
	clock *traceClock,
	tb *traceBoard,
	sc *agent.SkillContent,
	pack *agent.WritePack,
	cat *agent.WriteCategory,
	userMsg, draft string,
) (string, []agent.ReviewIssue, string) {
	cur := draft

	for round := 1; round <= maxReviewRounds; round++ {
		rv := tb.Active("generate", fmt.Sprintf("审稿 · 第 %d 轮", round), "按 reviewer.md 的检查项逐条核对…")
		clock.Set(tb.Steps())

		res, err := h.eng.Review(ctx, pack, cat, userMsg, cur)
		if err != nil {
			// 审稿失败不该把整轮对话判死：稿子已经写出来了，比「什么都不给」有用得多。
			// 但也不能装作审过了——如实说明，让用户自己看一眼。
			fmt.Fprintf(os.Stderr, "[manual-write] 审稿第 %d 轮失败: %v\n", round, err)
			tb.Close(rv, "本轮审稿未完成（调用失败），以下为未经审核的稿件")
			clock.Set(tb.Steps())
			return cur, nil, "（本轮审稿未完成，以上为未经审核的稿件）"
		}
		if res.Verdict == "pass" {
			// 过了就是过了：不附任何提示。审稿意见只在没通过时才值得占屏幕。
			// 但 trace 上要留一句「通过」——那句话以前只写在 stderr，屏幕上没有，
			// 用户只能靠「没看到审稿提示」倒推它审过了。
			verdict := "通过"
			if round > 1 {
				verdict = "通过（本轮无新问题）"
			}
			tb.Close(rv, verdict)
			clock.Set(tb.Steps())
			if round > 1 {
				fmt.Fprintf(os.Stderr, "[manual-write] 审稿第 %d 轮通过（共 %d 条问题已修）\n", round, len(res.Issues))
			}
			return cur, nil, ""
		}

		if round == maxReviewRounds {
			// 已经到上限、审稿人还有意见：最后再改一次然后交付，并如实说明
			// 「改完了但没再复审」。偷偷把稿子交出去而不提这件事，等于让用户
			// 以为这稿是审过的。
			tb.Close(rv, fmt.Sprintf("%d 条意见，已到审稿上限，最后一次修改（改完不再复审）", len(res.Issues)))
			fx := tb.Active("generate", "修订 · 最后一次", "按审稿意见改，改完直接交付…")
			clock.Set(tb.Steps())
			fixed, rerr := h.eng.Revise(ctx, sc, pack, cat, cur, res.Issues, clock.Thinking)
			if rerr != nil {
				fmt.Fprintf(os.Stderr, "[manual-write] 最终改稿失败: %v\n", rerr)
				tb.Close(fx, "改稿未成功，以下为未修改的稿件")
				clock.Set(tb.Steps())
				return cur, res.Issues, reviewNoteFor(res.Issues, "审稿提出以下问题，但自动修改未成功，以上为未修改的稿件：")
			}
			final := strings.TrimSpace(fixed)
			tb.Close(fx, fmt.Sprintf("已改，%d 字（未再复审）", runeLen(final)))
			clock.Set(tb.Steps())
			return final, res.Issues, reviewNoteFor(res.Issues, "以下问题已最后一次修改但未再复审，请留意：")
		}

		tb.Close(rv, fmt.Sprintf("发现 %d 条问题，需要修改", len(res.Issues)))
		fx := tb.Active("generate", fmt.Sprintf("修订 · 第 %d 轮", round), "按审稿意见改…")
		clock.Set(tb.Steps())
		fixed, rerr := h.eng.Revise(ctx, sc, pack, cat, cur, res.Issues, clock.Thinking)
		if rerr != nil {
			// 改稿失败：把问题如实列给用户，别丢一句「改稿失败」就完了——
			// 用户拿着一张问题清单至少能自己动手改。
			fmt.Fprintf(os.Stderr, "[manual-write] 第 %d 轮改稿失败: %v\n", round, rerr)
			tb.Close(fx, "改稿未成功，以下为未修改的稿件")
			clock.Set(tb.Steps())
			return cur, res.Issues, reviewNoteFor(res.Issues, "审稿提出以下问题，但自动修改未成功，以上为未修改的稿件：")
		}
		cur = strings.TrimSpace(fixed)
		tb.Close(fx, fmt.Sprintf("已改，%d 字", runeLen(cur)))
		clock.Set(tb.Steps())
	}
	return cur, nil, ""
}

// reviewNoteFor 把审稿意见整理成给用户看的一段提示。
func reviewNoteFor(issues []agent.ReviewIssue, head string) string {
	if len(issues) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("**审稿提示**：" + head + "\n")
	for i, is := range issues {
		fmt.Fprintf(&b, "%d. ", i+1)
		if strings.TrimSpace(is.Rule) != "" {
			b.WriteString(strings.TrimSpace(is.Rule))
		}
		if strings.TrimSpace(is.Fix) != "" {
			b.WriteString(" → " + strings.TrimSpace(is.Fix))
		}
		if strings.TrimSpace(is.Quote) != "" {
			b.WriteString("\n   > " + strings.TrimSpace(is.Quote))
		}
		if i < len(issues)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// 注：原先这里有一个 manualWriteSteps，负责拼固定四格骨架。改 append-only 步骤流
// （traceBoard）后它没用了——固定骨架每推进一步就把上一格的结论覆盖掉，正好是要修
// 的病。phase 只能取 analyze/match/params/generate 的约束不变，那条规则现在写在
// chat_write_trace.go 里 traceBoard.Active/Close 的注释上。

// hitDetail 拼「命中「X」」这一格的内容。
//
// 「命中」这两个字是**必须的**，不是装饰：用户的需求原文里常常就带着类别名（「帮我写
// 一篇会议纪要」），如果这一格只写光秃秃的类别名，屏幕上分不清这是「系统判定命中了
// 这一类」还是「用户自己说了这几个字」。回归测试同样靠「类别名 + 命中」双条件断言，
// 单条件会被需求原文假绿（踩过）。
func hitDetail(catName, confidence, reason string) string {
	detail := "命中「" + catName + "」"
	if confidence == "low" {
		detail += "（把握不大，需你核对是否为这一类）"
	}
	if why := oneLineClip(reason, 60); why != "" {
		detail += "：" + why
	}
	return detail
}

// ambiguousMessage 是「判不出类别」时反问用户的那段话。
func ambiguousMessage(pack *agent.WritePack, reason string) string {
	var b strings.Builder
	b.WriteString("这次要写的内容我没能确定属于哪一类，怕按错的要求写，先跟你确认一下。请指定一个类别：\n\n")
	for i, c := range pack.Categories {
		fmt.Fprintf(&b, "%d. %s", i+1, c.Name)
		if t := strings.TrimSpace(c.Trigger); t != "" {
			b.WriteString("——" + oneLineClip(t, 60))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n你也可以直接描述内容，我再判断一次。")
	if r := strings.TrimSpace(reason); r != "" {
		b.WriteString("\n\n（我上次的判断理由：" + r + "）")
	}
	return b.String()
}

// streamDeltas 把一份已经生成完的文本按小块下发。
//
// 只为了前端的打字观感：手册模式下用户看到的必须是**终稿**（审稿后的），而终稿是
// 一次性拿到的；不发 delta 的话屏幕上会突然整篇蹦出来，比逐字流显得像卡了一下。
// 这里不产生任何模型调用。
func streamDeltas(write func(ev, data string), text string) {
	const chunk = 48
	r := []rune(text)
	for i := 0; i < len(r); i += chunk {
		j := i + chunk
		if j > len(r) {
			j = len(r)
		}
		write(evDelta, jsonSafe(map[string]string{"t": string(r[i:j])}))
		time.Sleep(12 * time.Millisecond)
	}
}

// oneLineClip 取第一行并限长，供反问清单里的一句话说明。
// 名字不叫 firstLine：api 包里已经有一个同名的（单参版），语义也不同，
// 复用名字会把那边两处调用点的签名一起改坏。
func oneLineClip(s string, max int) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}
