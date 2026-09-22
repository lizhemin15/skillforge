package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lizhemin15/skillforge/internal/agent"
	"github.com/lizhemin15/skillforge/internal/model"
	"github.com/lizhemin15/skillforge/internal/tools"
)

// chatHandler serves the conversation endpoint (POST /api/chat, SSE response).
// Intentionally public (no auth) — the site-facing assistant is open to users.
type chatHandler struct {
	eng      *agent.Engine
	gen      *genCache       // generates one-time download URLs for docgen skills
	tools    *tools.Registry // 工具注册表；nil = 工具能力关闭
	mcp      *tools.MCPManager
	maxRound int // 工具循环轮数上限
}

type chatReq struct {
	SessionID string `json:"session_id"`
	Message   string `json:"message"`
	// Mode selects how the skill is chosen:
	//   ""/"auto" → 自动调度：由分类器理解意图后从技能库里挑
	//   "manual"  → 手动指定：Skills 里点名的技能直接用，跳过意图分类
	Mode string `json:"mode"`
	// Skill 是 mode=manual 时锁定的技能 slug。
	Skill string `json:"skill"`
	// MCP 是用户在本轮**勾选**使用的 MCP 服务器 id 列表。
	//
	// 空/缺省 = 不调度任何 MCP（这是默认值，也是产品的默认行为）。
	// 取值必须是服务器配置主键，前端从 GET /api/mcp/servers 拿。
	MCP []string `json:"mcp"`
}

// needsAnswer is a mid-generation event the engine emits when a skill has
// unfilled required params that the caller should ask the user for.
const evNeeds = "needs"
const evSkill = "skill"
const evTrace = "trace"
const evMeta = "meta"
const evDelta = "delta"
const evReset = "reset" // 本轮作废重试：前端清掉已流的正文，避免重试答案接在一屏垃圾后面
const evFile = "file"
const evDone = "done"
const evError = "error"

func (h *chatHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req chatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" {
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}
	if req.SessionID == "" {
		req.SessionID = "anon"
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	req.Skill = strings.TrimSpace(req.Skill)
	// 手动模式：用户点名的技能必须真实存在，否则派生的动作/步骤全是错的。
	// 技能不存在（刚被删/被停用）或不给 slug 都退回自动调度 —— 悄悄换技能很糟，
	// 所以原因会挂到 meta 事件上让前端提示一句。
	manualSkill, mode, manualNote := resolveMode(mode, req.Skill, h.eng.LoadSkill)

	// MCP 门控 —— 「默认不调度 MCP」的实现点，每个请求都算一次。
	//
	// 三个值必须**一起**算出来（勾选集 / 裁剪后的工具表 / 提示词段落）：
	// 任何两个错配，模型都会去调一个它拿不到的工具，表现是空转到轮数上限、
	// 又慢又最后编答案。三个值分开算就等于把这种错配写进代码结构里。
	//
	// 注意这里不是「勾了才过滤」而是「永远过滤」：默认关是结构性的，
	// 就算后面有人把分类器的 NeedsTools 调成恒真，没勾的用户也拿不到 MCP 工具。
	mcpSel := []string(nil)
	if h.mcp != nil {
		mcpSel = tools.AllowedMCPServers(h.mcp.Selectable(), req.MCP)
	}
	toolReg := tools.FilterMCP(h.tools, tools.MCPServerSet(mcpSel))
	mcpHint := ""
	if h.mcp != nil {
		mcpHint = h.mcp.PromptHintFor(mcpSel)
	}

	// SSE plumbing
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	// SSE 帧必须串行写：心跳 goroutine 与主流程都会下发 trace 帧，不加锁会把
	// 「event:/data:」切碎，前端解析直接失败。
	var wmu sync.Mutex
	write := func(ev, data string) {
		wmu.Lock()
		defer wmu.Unlock()
		if _, err := w.Write([]byte("event: " + ev + "\ndata: " + data + "\n\n")); err != nil {
			return
		}
		fl.Flush()
	}

	// 0. 先画步骤骨架：第一件事就是让「思考中」出现在用户屏幕上。
	//    意图分类是一次几十秒的阻塞调用，这之前什么都不发 = 用户干瞪眼。
	//    手动模式没有这一步，骨架直接读作"已选定技能"。
	seed := []agent.TraceStep{{
		Phase:  "analyze",
		Label:  "① 意图分析",
		Detail: analyzeDetail(manualNote),
		Status: "active",
	}}
	if manualSkill != nil {
		seed = agent.ManualSteps(manualSkill)
	}
	clock := newTraceClock(write, seed)
	defer clock.Freeze() // 兜底：任何分支（含 error 早退）都必须让心跳停下

	// 中间材料从这里接上：模型侧流出来的思考链片段（执笔/审稿/改稿那几跳最长）
	// 会被 clock 挂到进行中的那一步上滚动显示。挂一次全链路可见，因为下游所有
	// 跳用的是同一条 ctx。
	ctx := agent.WithProgress(r.Context(), clock.Thinking)
	// 复读收手/断流重试时，StreamChat 经 OnReset 回调到这里：清掉气泡里已流
	// 出的正文再重试，别让用户看到「半屏复读 + 一份正常答案」缝合在一起。
	ctx = agent.WithReset(ctx, func() { write(evReset, "{}") })

	// 0. persist the user turn
	history := h.eng.Session(req.SessionID)
	h.eng.Push(req.SessionID, agent.Message{Role: "user", Content: req.Message, At: time.Now()})
	// Full persisted history (NOT trimmed to maxHist) for continuation-editing
	// recovery: a follow-up "把部门改成市场部" must still see the prior fill
	// markers even after a long conversation trimmed the in-memory window.
	fullHist := h.eng.FullSession(req.SessionID)

	// 1. orchestrator: decide skill
	//    手动模式下技能已经定了，直接合成 Eval —— 这一步是「指定技能」按钮的全部价值：
	//    省掉一次几十秒的阻塞分类调用，也不再让分类器推翻用户的选择。
	var (
		eval *agent.Eval
		err  error
	)
	if manualSkill != nil {
		eval = agent.ManualEval(manualSkill)
		if a := agent.ManualActionFor(manualSkill, req.Message); a != "" {
			eval.Action = a
		}
	} else {
		// 分类这一跳必须自带旁白：它是用户按下发送后的**第一跳**，而它的两个材料
		// 来源都可能一片都没有（思考链被关 / provider 整段缓冲）。线上实测这一跳
		// 静默 54.0s，屏幕上只有计时在跳（用户原话「一直卡着计时」）。
		// 旁白内容全是本地真值，模型一开口就让路（Narrate 内部判断）。
		stopNarrate := clock.Narrate(classifyNarration(req.Message, history))
		eval, err = h.eng.EvalTurn(ctx, req.SessionID, req.Message, history)
		stopNarrate()
	}
	if err != nil {
		write(evError, jsonSafe(map[string]string{"error": "调度失败: " + err.Error()}))
		return
	}
	write(evMeta, jsonSafe(map[string]string{
		"reason": eval.Reason, "skill": eval.SkillSlug, "intent": eval.Intent,
		"mode": mode, "note": manualNote,
		// 把「这一轮到底给了几个 MCP 工具」摊在明面上：用户投诉「勾了没用」时，
		// 有这一行就能一眼分清是「没勾上」「服务没连上」还是「模型没用」。
		"mcp": strings.Join(mcpSel, ","),
	}))

	// 把调度过程画给用户看。这里**不再**按意图过滤（闲聊也发）：过滤掉就等于
	// 回到「几十秒空白 + 最后啪一下出答案」，那正是用户抱怨的现象。
	// 手动模式的骨架在点选时就画好了，重复 Set 只会把已完成的步骤闪回 active。
	if manualSkill == nil {
		steps := eval.Steps
		if len(steps) == 0 {
			steps = fallbackSteps(*eval)
		}
		clock.Set(normalizeSteps(steps))
	}

	// 2a. 工具循环优先：任务需要外部实时数据或真实计算时，交给 Agent 自己组合工具
	//     （优先于技能快路径——否则模型会「凭记忆编数字」生成一份看着很像的文档）
	//
	// 勾了 MCP 时**必须**进循环，不能只等分类器点头：线上实测「用数据工具箱查一下
	// 有哪些库表」被判成「未命中技能手册，走通用写作」，于是根本没进循环、0 次工具调用，
	// 模型把库表名全编了一遍（crm_prod / erp_finance / …）还额外编了个 SQL。
	// 分类器不认识 MCP（它的提示词里没有 MCP 概念），是个单点误判，
	// 用户明确勾选属于「用户直接指令」，优先级高于分类器的猜测。
	// 命中技能时仍然让位：技能快路径交付文件/模板，强进循环反而会把交付物弄丢。
	if eval.NeedsTools || (len(mcpSel) > 0 && eval.SkillSlug == "") {
		if h.runAgentLoop(ctx, write, clock, req, *eval, history, toolReg, mcpHint) {
			return
		}
	}

	// 2. generation
	var full string
	if eval.SkillSlug != "" {
		sc, lerr := h.eng.LoadSkill(eval.SkillSlug)
		if lerr != nil {
			write(evError, jsonSafe(map[string]string{"error": lerr.Error()}))
			return
		}
		// Continuation relay: if the user is editing values from an earlier
		// template fill (e.g. "把手机号改成139…"), route back to THAT template
		// skill and redo a smart-fill instead of whatever the classifier picked
		// (which can drift to a generic docgen skill and wipe prior values).
		// 手动模式下用户已经点名了技能，接力逻辑必须让位（否则会被悄悄换掉）。
		if tslug := agent.TemplateFillSkillInHistory(fullHist, req.Message); manualSkill == nil && tslug != "" && tslug != eval.SkillSlug {
			tsc, terr := h.eng.LoadSkill(tslug)
			if terr == nil && tsc.SkillType == model.SkillTypeTemplate && tsc.Attachment != "" {
				sc = tsc
				eval.SkillSlug = tslug
				// keep the history on this skill so the fill logic can recover past values
			}
		}
		// 2a-pre. 出文件意图的纠偏闸门。
		// 	分类器会给出「intent=docgen（要出文件）却命中文章写作型技能」这种自相矛盾的
		// 	组合：技能名里带「公文/通知」字样的写作技能最容易被挑走，尽管清单里每个技能
		// 	都标着类型。而下面的发文分支只在**命中技能自己就是 docgen 型**时才会进 ——
		// 	于是这种组合会**静默降级成写正文**：用户要 Word，拿到一屏文字，没有文件、
		// 	没有报错、也没有一句说明。用户那句「生成的 skill 和我给的内容完全没关系」
		// 	有一部分就是这么来的。
		// 	取证：线上验收的 docgen 腿（intent=docgen + 命中 write 型技能 → 无 file 帧）。
		// 	纠偏只在「这一轮真出不了文件」时发生：命中 template 技能且带附件时，
		// 	fill / template_only 照样能交付文件，不许被抢。
		//
		// 2026-09-17 补一道**用户明说只看正文**的让路（真浏览器实测踩到）：
		// 	同一个闸门反向也咬人 —— 「写一份…通知…直接输出正文」会被分类器按题材判成
		// 	intent=docgen，于是用户明说了要正文，却只拿到一个 .docx 文件卡片（正文 94 字）。
		// 	明说的交付形态是用户亲手写的判据，不该由模型投票决定，所以这里用确定性
		// 	agent.ExplicitTextOnly 兜底让路；分类器提示词那侧也补了同一条规则（双保险）。
		if strings.EqualFold(strings.TrimSpace(eval.Intent), "docgen") &&
			sc.SkillType != model.SkillTypeDocGen && !agent.ExplicitTextOnly(req.Message) {
			act := strings.ToLower(strings.TrimSpace(eval.Action))
			deliversOwnFile := sc.Attachment != "" && (act == "fill" || act == "template_only")
			if !deliversOwnFile {
				if dsc := h.eng.PickDocGenSkill(req.Message); dsc != nil {
					fmt.Fprintf(os.Stderr, "[route] intent=docgen action=%q 命中 %s(%s) 出不了文件 → 纠偏到文档生成技能 %s\n",
						act, sc.Slug, sc.SkillType, dsc.Slug)
					// needs 是照另一个技能的参数名算出来的，跟着换技能就失效了；
					// docgen 类技能本身没有必填参数（见 EvalTurn 契约），清掉免得
					// 用户被追问一堆跟这份文档无关的字段。
					eval.Needs = nil
					oldName, oldType := sc.Name, sc.SkillType
					sc = dsc
					eval.SkillSlug = dsc.Slug
					// 换技能必须让用户看见，否则就是闷声改路由：
					// 走 meta.note（前端已有「降级说明」渲染通道，出现在回复开头）。
					write(evMeta, jsonSafe(map[string]string{
						"reason": eval.Reason, "skill": dsc.Slug, "intent": eval.Intent,
						"mode": mode,
						"note": "「" + oldName + "」是「" + skillTypeLabel(oldType) + "」型技能，产出正文而不是文件；" +
							"本轮要交付文件，已改用「" + dsc.Name + "」生成。",
					}))
				} else {
					// 一个能出文件的技能都没有：把话说明白，别把「要文件」当「要正文」。
					fmt.Fprintf(os.Stderr, "[route] intent=docgen 但技能库里没有可用的文档生成技能（命中 %s）\n", sc.Slug)
					write(evDelta, jsonSafe(map[string]string{
						"t": "当前技能库里没有可用的「文档生成」技能，无法产出 Word/Excel 文件；下面改用「" + sc.Name + "」输出正文。\n\n",
					}))
				}
			}
		}

		write(evSkill, jsonSafe(map[string]string{
			"slug": eval.SkillSlug, "name": sc.Name, "reason": eval.Reason,
			"type": sc.SkillType, "file": sc.Attachment,
		}))
		// merge skill params: eval extracted + needs fallback to empty
		// (FlattenParams converts possibly-array docgen params to strings)
		args := h.eng.FlattenParams(eval.Params)
		if args == nil {
			args = map[string]string{}
		}
		// 可选字段没填**不该**拦下这一轮：技能自己的标签就写着「未提供则生成」，
		// 产品反问用户就是自相矛盾（线上实测：整轮 4.5s 只回一句「我需要你补充
		// 标题亮点（可选…）」，正文一个字没有）。只用必填缺参拦；顺手把「哪几个
		// 可选字段放过了」写进中间材料 —— 这既是本地事实（t≈0 就能发），也正好
		// 补上用户抱怨的「中间没材料、只看到计时在跳」。
		blocking, optional := agent.SplitNeeds(h.declaredParams(eval.SkillSlug), eval.Needs)
		if len(optional) > 0 {
			clock.Thinking("技能「" + sc.Name + "」的可选信息没给（" + agent.NeedsSummary(optional) +
				"），按它自己的默认继续起草…")
		}
		if len(blocking) > 0 {
			// surface missing required params as a needs event, then skip gen
			msg := needsMessage(blocking)
			write(evNeeds, jsonSafe(blocking))
			// 让 generate 那一步读作「等补充」而不是「还在跑」
			clock.Awaiting("等待补充：" + needsShort(blocking))
			write(evDelta, jsonSafe(map[string]string{"t": msg}))
			// record assistant prompt asking for the fields
			h.eng.Push(req.SessionID, agent.Message{Role: "assistant", Content: msg, SkillSlug: eval.SkillSlug, At: time.Now()})
			write(evDone, jsonSafe(map[string]string{"skill": eval.SkillSlug, "asked": "true"}))
			return
		}
		// docgen skills produce an office file, not streamed text: drive the
		// generator, cache the bytes, and hand the client a one-time download.
		if sc.SkillType == model.SkillTypeDocGen {
			// 这一跳跟起草一样是**无上界的阻塞**：线上实测关掉思考链后这一跳裸跑
			// 16.8 秒（整轮 23.8s），期间屏幕上只有「已用 6s/9s/12s」在跳。
			// 模型侧的材料要等它开始吐 JSON 才有（contentSink 从流式 JSON 里抽正文），
			// 在那之前先把手里的事实报出去——这是本地事实，t≈0 就能发。
			clock.Thinking(noteFacts(sc, args, fullHist).noteDoing("正在生成文档…"))
			doc, derr := h.eng.GenerateDoc(ctx, req.SessionID, sc, args, req.Message, fullHist)
			if derr != nil {
				write(evError, jsonSafe(map[string]string{"error": "文档生成失败: " + derr.Error()}))
				return
			}
			tok := h.gen.put(doc.Filename, doc.ContentType, doc.Bytes)
			if doc.Summary != "" {
				write(evDelta, jsonSafe(map[string]string{"t": doc.Summary}))
			}
			write(evFile, jsonSafe(map[string]string{
				"name": doc.Filename,
				"url":  "/api/chat/gen/" + tok,
				"kind": "gen",
			}))
			clock.Finish()
			// Persist this turn's spec into session history (as a marker in the
			// assistant message) so a follow-up "再加一行 / 把单价改成 8000"
			// continues from THIS document instead of inventing a new one.
			h.eng.Push(req.SessionID, agent.Message{Role: "assistant", Content: doc.Summary + docSummarySuffix(doc.Spec), SkillSlug: eval.SkillSlug, Kind: agent.KindArtifact, At: time.Now()})
			write(evDone, jsonSafe(map[string]string{"skill": eval.SkillSlug}))
			return
		}

		// Tool dispatch driven by the orchestrator's intent judgment (eval.Action),
		// NOT by keyword matching. The classifier resolved what to do this turn:
		//   fill          → smart-fill the template attachment and deliver the filled file
		//   template_only → deliver the blank template file directly (no content generation)
		//   gen           → generate a fresh office file via the docgen skill below
		//   write/answer  → free-form text generation below
		//
		// Eval.Action is authoritative. WantsFill is kept ONLY as a last-resort
		// fallback for robustness: if a template skill was matched but the model
		// omitted/blanked action and the phrasing still clearly asks to fill,
		// do the smart-fill anyway instead of silently dropping the attachment.
		action := strings.ToLower(strings.TrimSpace(eval.Action))
		if action == "" && sc.SkillType == model.SkillTypeTemplate && agent.WantsFill(req.Message) && sc.Attachment != "" {
			action = "fill"
		}

		// Deliver the blank template when the user explicitly asked for a template
		// (short-circuit before any content generation — a "发我个空白模板" request
		// should hand back the file immediately, not stream unrelated prose).
		if action == "template_only" {
			if sc.Attachment == "" {
				action = "" // no template file to deliver → normal text generation
			} else {
				// Reuse the existing public attachment endpoint, which serves
				// the raw blank template file with a proper filename.
				write(evDelta, jsonSafe(map[string]string{"t": "这是「" + sc.Name + "」的空白模板，可直接下载使用。"}))
				write(evFile, jsonSafe(map[string]string{
					"slug": eval.SkillSlug, "name": sc.Attachment,
					"url": "/api/chat/attachment/" + eval.SkillSlug,
				}))
				clock.Finish()
				h.eng.Push(req.SessionID, agent.Message{Role: "assistant", Content: "已下发「" + sc.Name + "」空白模板。", SkillSlug: eval.SkillSlug, At: time.Now()})
				write(evDone, jsonSafe(map[string]string{"skill": eval.SkillSlug}))
				return
			}
		}

		if action == "fill" && sc.Attachment != "" {
			fres, ferr := h.eng.FillDoc(ctx, req.SessionID, sc, req.Message, fullHist)
			if ferr != nil {
				// fall back to the normal flow if the template can't be filled
				fmt.Fprintf(os.Stderr, "[fill] fallback reason=%v\n", ferr)
			} else if fres.Clarify != nil {
				// The fill model decided the request lacks key facts (user gave
				// a concrete edit intent but no specifics). Ask back instead of
				// inventing data — like any decent AI would.
				clock.Awaiting("信息不足，等待用户补充：" + joinQuestions(fres.Clarify.Questions))
				msg := "在填充前还需要确认几个信息：\n" + numberedQuestions(fres.Clarify.Questions)
				write(evDelta, jsonSafe(map[string]string{"t": msg}))
				h.eng.Push(req.SessionID, agent.Message{Role: "assistant", Content: msg, SkillSlug: eval.SkillSlug, At: time.Now()})
				write(evDone, jsonSafe(map[string]string{"skill": eval.SkillSlug, "asked": "true"}))
				return
			} else {
				tok := h.gen.put(fres.Filename, fres.ContentType, fres.Bytes)
				if fres.Summary != "" {
					write(evDelta, jsonSafe(map[string]string{"t": fres.Summary}))
				}
				write(evFile, jsonSafe(map[string]string{
					"name": fres.Filename,
					"url":  "/api/chat/gen/" + tok,
					"kind": "gen",
				}))
				clock.Finish()
				// Persist this turn's filled values INTO session history (as an
				// assistant fill summary) so a follow-up edit turn can recover them
				// and continue incrementally instead of wiping untouched fields.
				h.eng.Push(req.SessionID, agent.Message{
					Role:      "assistant",
					Content:   fres.Summary + fillSummarySuffix(fres.Vals),
					SkillSlug: eval.SkillSlug,
					Kind:      agent.KindArtifact,
					At:        time.Now(),
				})
				write(evDone, jsonSafe(map[string]string{"skill": eval.SkillSlug}))
				return
			}
		}

		// 2b. 手册模式（写作类技能 + 训练期抽出了分类素材）：判类 → 按类执笔 → 审稿。
		//     「双开关」：skill_type=write 决定要不要走写文章这条路，categories/
		//     目录在不在决定这条路是分三段还是单段。后者是训练期的抽取结果——
		//     手册抽不出结构就不会建这个目录，于是自动退回下面的单段生成，
		//     管理人不需要在任何地方勾选或配置。
		//     不在这里处理的话，system_prompt 里那句「打开 categories/ 下对应的
		//     文件」就是空头支票：模型看不到文件，只能凭分类名瞎猜要求。
		if sc.SkillType == model.SkillTypeWrite {
			pack, perr := h.eng.LoadWritePack(eval.SkillSlug)
			if perr != nil {
				write(evError, jsonSafe(map[string]string{"error": "载入手册素材失败: " + perr.Error()}))
				return
			}
			if pack != nil {
				if h.runManualWrite(ctx, write, clock, sc, pack, args, req.Message, req.SessionID, history) {
					return
				}
			}
		}

		// 技能链路以前**没有步骤板**：全程挂着启动骨架「① 意图分析」，于是「正在执笔」
		// 这件事在界面上读作「① 意图分析（已用 63s）」——标签本身就是错的，用户既看不到
		// 阶段推进，也看不到材料。这里补上它（手册链路的 chat_write.go 早有同样的板）。
		tb := newTraceBoard()
		tb.Carry(clock.Steps())
		tb.CloseCarried(analyzeCarriedDetail(manualSkill))
		tb.Done("match", "② 技能匹配", "命中技能《"+sc.Name+"》")
		tb.Done("params", "③ 要素提炼", noteFacts(sc, args, history).note())
		// 构思那一跳挂 generate，不新造 phase：前端 [web/js/chat.js] 的 AGENTS 表只认
		// analyze/match/params/generate 四个键，取不到就把 phase 原样印在编号栏上
		// （那格里会显示英文单词 "plan"、角色徽标空白）。同一阶段的多跳靠 label 区分，
		// 起草初稿/审稿/修订/交付也都是这么挂在 generate 上的。
		planIdx := tb.Active("generate", "④ 构思要点", "先把这一篇怎么写想清楚，要点会实时滚出来…")
		clock.Set(tb.Steps())

		// 执笔那一跳保留思考链时首字实测要等 63 秒，而这段 provider 一片 reasoning 都不推。
		// 所以先要一份「构思」：它关思考链、秒级出字，片段走 contentSink 变成中间材料。
		//
		// ★ 2026-09-20：旁白必须**先于**这一跳开。构思跳关思考链是有代价的——provider
		// 在「关思考」这条上实测 reason=0（一片都不推），所以它自己的整段耗时全是静默：
		// 线上实测这一跳 hop=50.0s / ttft=45.5s，整轮「最长无变化 49.2s」就是它。
		// 旁白滚的是本地事实（装进上下文的是什么、几篇范文、上文多少字），t≈0 就能发，
		// 正好填住这段——这也回答了「为什么不让旁白晚点开」：晚一秒就是多一秒死屏。
		facts := noteFacts(sc, args, history).note()
		stopPlanNarr := clock.Narrate(narrateLines("", facts))
		plan, _ := h.eng.PlanEssay(ctx, sc, args, req.Message)
		stopPlanNarr()
		if plan != "" {
			tb.Close(planIdx, "要点已定，按它落笔")
		} else {
			tb.Close(planIdx, "没拿到要点，直接落笔")
		}
		tb.Active("generate", "⑤ 按要点执笔", "按要点写正文，思考链片段会实时滚出来…")
		clock.Set(tb.Steps())

		// 起草这一跳是几十秒的阻塞调用。模型侧不给材料时（线上实测 provider 不推
		// reasoning，reasoning 片数=0）屏幕上只剩计时器在跳，所以先把「装进上下文的
		// 是什么」报出去——这是本地事实，t≈0 就能发。
		clock.Thinking(noteFacts(sc, args, history).note())
		stopNarr := clock.Narrate(narrateLines(plan, noteFacts(sc, args, history).note()))
		full, err = h.eng.GenerateWithPlan(ctx, sc, args, plan, func(delta string) {
			write(evDelta, jsonSafe(map[string]string{"t": delta}))
		})
		stopNarr()
		if err != nil {
			write(evError, jsonSafe(map[string]string{"error": "生成失败: " + err.Error()}))
			return
		}
		// Kind=artifact：这是技能生成的正文，是用户下一轮「改成…/整理成…」的指代对象，
		// 上下文注入时必须逐字保留（见 agent/compact.go 的产物层）。
		h.eng.Push(req.SessionID, agent.Message{Role: "assistant", Content: full, SkillSlug: eval.SkillSlug, Kind: agent.KindArtifact, At: time.Now()})
		clock.Finish()
		// typed flow skill with a declared template file → dispatch it for download
		if sc.Attachment != "" {
			write(evFile, jsonSafe(map[string]string{
				"slug": eval.SkillSlug, "name": sc.Attachment,
				"url": "/api/chat/attachment/" + eval.SkillSlug,
			}))
		}
		write(evDone, jsonSafe(map[string]string{"skill": eval.SkillSlug}))
		return
	}

	// 3. plain chat (no skill) — stream a conversational reply
	// 「没命中技能」这条路上，分类回来到首字之间同样是一次无上界的阻塞调用（线上实测
	// 那 54.7 秒就发生在这条路上），所以材料同样先由本地事实顶上。
	//
	// 但「本地事实」是**一次性**的：报出去之后到首字之间仍然是死寂。2026-09-19 的账本
	// 就是这么抓到的——第 1 轮走的正是这条道（分类判「通用写作、不生成文件」），
	// 首字 71.7s、**最大静默 60.3s**，材料停在装配那句上不动，屏幕上只有「已用 Ns」在跳。
	// 所以这里补两块：步骤板（标签跟着真实阶段走，不再全程挂着「① 意图分析」）＋
	// 构思跳（关思考链、秒级出字，把「这一篇怎么写」当 content 流出来当材料）。
	tb := newTraceBoard()
	tb.Carry(clock.Steps())
	tb.CloseCarried(analyzeCarriedDetail(manualSkill))
	tb.Done("params", "② 上下文装配", noteFacts(nil, nil, history).note())
	// 见上一条注释：phase 只能用前端 AGENTS 表认识的四个键，构思挂 generate。
	planIdx := tb.Active("generate", "③ 构思要点", "先把这一篇怎么写想清楚，要点会实时滚出来…")
	clock.Set(tb.Steps())
	plainFacts := noteFacts(nil, nil, history).note()
	clock.Thinking(plainFacts)

	// 旁白先于构思跳开，理由同上面技能链路那条：构思跳关思考链 → provider reason=0
	// → 这一跳全程静默（线上实测 50.0s / 首 token 45.5s）。
	stopPlanNarr := clock.Narrate(narrateLines("", plainFacts))
	plan, _ := h.eng.PlanEssay(ctx, nil, nil, req.Message)
	stopPlanNarr()
	if plan != "" {
		tb.Close(planIdx, "要点已定，按它落笔")
	} else {
		tb.Close(planIdx, "没拿到要点，直接落笔")
	}
	tb.Active("generate", "④ 按要点执笔", "按要点写正文，思考链片段会实时滚出来…")
	clock.Set(tb.Steps())

	// 这一跳实测能静默几分钟（provider 不推 reasoning、正文也要整篇想完才来），
	// 所以本地旁白必须开：滚的是构思要点与上下文事实，不是编出来的进度。
	stopNarr := clock.Narrate(narrateLines(plan, noteFacts(nil, nil, history).note()))
	full, err = h.eng.PlainChatWithPlan(ctx, req.SessionID, req.Message, history, plan, func(delta string) {
		write(evDelta, jsonSafe(map[string]string{"t": delta}))
	})
	stopNarr()
	if err != nil {
		write(evError, jsonSafe(map[string]string{"error": err.Error()}))
		return
	}
	// 无技能的普通对话产出也可能是「用户下一轮要指代的正文」（如「写篇新闻稿，
	// 再整理成 word」而新闻稿没命中任何写作技能），够长的按产物保留原文。
	h.eng.Push(req.SessionID, agent.Message{Role: "assistant", Content: full, Kind: agent.ArtifactKind(full), At: time.Now()})
	clock.Finish()
	write(evDone, jsonSafe(map[string]string{"skill": ""}))
}

// resolveMode 把手动模式解析成"真正要用的技能 + 生效模式 + 给用户的一句说明"。
// 抽成纯函数是为了能单独测：这里错一步，用户要么会看到"锁定了 A 技能却按 B 出结果"，
// 要么会看到"技能已经没了，界面一声不吭地换了技能"。
// load 传 nil 或返回错误都当成"技能不可用"，退回自动调度并给出原因。
func resolveMode(mode, slug string, load func(string) (*agent.SkillContent, error)) (*agent.SkillContent, string, string) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	slug = strings.TrimSpace(slug)
	if mode != "manual" || slug == "" || load == nil {
		return nil, "auto", ""
	}
	sc, err := load(slug)
	if err != nil || sc == nil {
		return nil, "auto", "指定的技能「" + slug + "」不可用，已改用自动调度"
	}
	return sc, "manual", ""
}

// analyzeDetail 是 t≈0 那一帧的步骤说明。
// 降级原因必须挂在这里，不能只等几十秒后那个 meta 事件：用户锁的技能被删了，
// 第一秒就该知道"你现在看到的是自动调度的结果"，而不是等结果出来后才发现不对。
// 顺序上把原因放前面、那句"正在理解…"放后面顶住心跳的「已用 Ns」后缀，
// 免得读成「…（原因）（已用 3s）」两个括号连着堆。
func analyzeDetail(note string) string {
	note = strings.TrimSpace(note)
	if note == "" {
		return "正在理解你的问题…"
	}
	return note + " · 正在理解你的问题…"
}

// fillSummarySuffix renders the just-filled field values as a compact tracer
// appended to the assistant fill summary before it is persisted to history, so
// a later edit turn can recover and continue editing the same values.
func fillSummarySuffix(vals map[string]string) string {
	if len(vals) == 0 {
		return ""
	}
	var parts []string
	for k, v := range vals {
		parts = append(parts, k+"="+v)
	}
	return " 已填值: " + strings.Join(parts, ", ")
}

// docSummarySuffix renders a generated document's spec as a compact marker
// appended to the assistant summary before it is persisted to history, so a
// later edit turn can rebuild the same document with the change applied.
func docSummarySuffix(spec string) string {
	if strings.TrimSpace(spec) == "" {
		return ""
	}
	return " 已生成规格:" + spec
}

// declaredParams 取技能**声明**的参数表（input_params），给缺参判定当地面真值。
//
// 读不到就返回 nil，让 needs 自报的 Required 兜底 —— 读声明失败不该把一次写作
// 变成一句报错，那比误判一轮更糟。
func (h *chatHandler) declaredParams(slug string) []model.Param {
	if strings.TrimSpace(slug) == "" {
		return nil
	}
	sk, err := h.eng.Skill(slug)
	if err != nil || sk == nil {
		return nil
	}
	return sk.InputParams
}

func needsMessage(needs []model.Param) string {
	labels := make([]string, 0, len(needs))
	for _, n := range needs {
		l := n.Label
		if l == "" {
			l = n.Name
		}
		if l != "" {
			labels = append(labels, l)
		}
	}
	if len(labels) == 0 {
		return "好的，我需要你补充一些信息才能开始写作，请告诉我。"
	}
	return "要开始写作，我需要你补充以下信息：" + strings.Join(labels, "、") + "。你可以直接告诉我。"
}

// skillTypeLabel 把技能类型翻成给用户看的说法。
// 用途：路由纠偏时那句「X 是『文章写作』型技能，产出正文而不是文件」必须说人话 ——
// 用户看到的是「为什么换了个技能」，不是「skill_type=write」。
func skillTypeLabel(t string) string {
	switch t {
	case model.SkillTypeDocGen:
		return "文档生成"
	case model.SkillTypeTemplate:
		return "模板填充"
	case model.SkillTypeWrite:
		return "文章写作"
	case model.SkillTypeQuery:
		return "查询问答"
	}
	if strings.TrimSpace(t) == "" {
		return "未标注类型"
	}
	return t
}

// needsShort returns a compact comma list of missing-param labels, for trace detail.
func needsShort(needs []model.Param) string {
	labels := make([]string, 0, len(needs))
	for _, n := range needs {
		l := n.Label
		if l == "" {
			l = n.Name
		}
		if l != "" {
			// strip trailing question mark for a tight trace line
			l = strings.TrimSuffix(l, "？")
			l = strings.TrimSuffix(l, "?")
			labels = append(labels, l)
		}
	}
	return strings.Join(labels, "、")
}

func jsonSafe(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// joinQuestions joins clarification questions for a compact trace line.
func joinQuestions(qs []string) string {
	return strings.Join(qs, "；")
}

// numberedQuestions formats clarification questions as a numbered, user-facing list.
func numberedQuestions(qs []string) string {
	var b strings.Builder
	for i, q := range qs {
		if s := strings.TrimSpace(q); s != "" {
			fmt.Fprintf(&b, "%d. %s\n", i+1, s)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// Attachment serves a skill's declared template/policy file for public download.
// It is intentionally single-file whitelisted: only the exact relative path the
// skill records in its `attachment` column is served, resolved against the
// skill directory with a path-traversal guard — arbitrary files are never exposed.
func (h *chatHandler) Attachment(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" {
		http.Error(w, "missing slug", http.StatusBadRequest)
		return
	}

	sk, err := h.eng.Skill(slug)
	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "no such skill", http.StatusNotFound)
		} else {
			http.Error(w, "skill error", http.StatusInternalServerError)
		}
		return
	}
	if sk == nil || sk.Attachment == "" {
		http.Error(w, "no attachment", http.StatusNotFound)
		return
	}

	path, ok := h.eng.AttachmentPath(slug, sk.Attachment)
	if !ok {
		http.Error(w, "attachment unavailable", http.StatusNotFound)
		return
	}

	name := filepath.Base(path)
	w.Header().Set("Content-Disposition", "attachment; filename="+quoteEscaped(name))
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, path)
}

// quoteEscaped renders a filename for a Content-Disposition value, quoting and
// escaping non-ASCII characters so Chinese template filenames download cleanly.
// Note: many download clients (Feishu, WPS, some browsers) only honor the plain
// `filename=` fallback and ignore `filename*`. So the ASCII fallback MUST keep
// the real extension — otherwise `download` (no ext) is saved and later opened
// as text/plain (i.e. "下载下来是txt"). filename* carries the true CJK name.
func quoteEscaped(name string) string {
	if len(name) > 0 && strconv.IsPrint(rune(name[0])) && name[0] < 0x80 {
		// pure-ASCII name: plain quoted form
		return `"` + strings.ReplaceAll(name, `"`, `\"`) + `"`
	}
	// non-ASCII: ASCII fallback that preserves the extension, plus RFC 5987 name.
	ext := filepath.Ext(name)
	fallback := "download" + ext
	return `"` + fallback + `"; filename*=UTF-8''` + url.PathEscape(name)
}
