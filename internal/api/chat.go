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
	maxRound int             // 工具循环轮数上限
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
}

// needsAnswer is a mid-generation event the engine emits when a skill has
// unfilled required params that the caller should ask the user for.
const evNeeds = "needs"
const evSkill = "skill"
const evTrace = "trace"
const evMeta = "meta"
const evDelta = "delta"
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
		eval, err = h.eng.EvalTurn(ctx, req.SessionID, req.Message, history)
	}
	if err != nil {
		write(evError, jsonSafe(map[string]string{"error": "调度失败: " + err.Error()}))
		return
	}
	write(evMeta, jsonSafe(map[string]string{
		"reason": eval.Reason, "skill": eval.SkillSlug, "intent": eval.Intent,
		"mode": mode, "note": manualNote,
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
	if eval.NeedsTools {
		if h.runAgentLoop(ctx, write, clock, req, *eval, history) {
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
		if len(eval.Needs) > 0 {
			// surface missing required params as a needs event, then skip gen
			msg := needsMessage(eval.Needs)
			write(evNeeds, jsonSafe(eval.Needs))
			// 让 generate 那一步读作「等补充」而不是「还在跑」
			clock.Awaiting("等待补充：" + needsShort(eval.Needs))
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

		// 起草这一跳是几十秒的阻塞调用。模型侧不给材料时（线上实测 provider 不推
		// reasoning，reasoning 片数=0）屏幕上只剩计时器在跳，所以先把「装进上下文的
		// 是什么」报出去——这是本地事实，t≈0 就能发。
		clock.Thinking(noteFacts(sc, args, history).note())
		full, err = h.eng.Generate(ctx, sc, args, func(delta string) {
			write(evDelta, jsonSafe(map[string]string{"t": delta}))
		})
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
	clock.Thinking(noteFacts(nil, nil, history).note())
	full, err = h.eng.PlainChat(ctx, req.SessionID, req.Message, history, func(delta string) {
		write(evDelta, jsonSafe(map[string]string{"t": delta}))
	})
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
