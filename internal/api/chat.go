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

	ctx := r.Context()

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
			doc, derr := h.eng.GenerateDoc(ctx, sc, args, req.Message, fullHist)
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
			h.eng.Push(req.SessionID, agent.Message{Role: "assistant", Content: doc.Summary + docSummarySuffix(doc.Spec), SkillSlug: eval.SkillSlug, At: time.Now()})
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
			fres, ferr := h.eng.FillDoc(ctx, sc, req.Message, fullHist)
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
					At:        time.Now(),
				})
				write(evDone, jsonSafe(map[string]string{"skill": eval.SkillSlug}))
				return
			}
		}

		full, err = h.eng.Generate(ctx, sc, args, func(delta string) {
			write(evDelta, jsonSafe(map[string]string{"t": delta}))
		})
		if err != nil {
			write(evError, jsonSafe(map[string]string{"error": "生成失败: " + err.Error()}))
			return
		}
		h.eng.Push(req.SessionID, agent.Message{Role: "assistant", Content: full, SkillSlug: eval.SkillSlug, At: time.Now()})
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
	full, err = h.eng.PlainChat(ctx, req.SessionID, req.Message, history, func(delta string) {
		write(evDelta, jsonSafe(map[string]string{"t": delta}))
	})
	if err != nil {
		write(evError, jsonSafe(map[string]string{"error": err.Error()}))
		return
	}
	h.eng.Push(req.SessionID, agent.Message{Role: "assistant", Content: full, At: time.Now()})
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
