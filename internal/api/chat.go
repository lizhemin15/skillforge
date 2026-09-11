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

	// SSE plumbing
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	write := func(ev, data string) {
		if _, err := w.Write([]byte("event: " + ev + "\ndata: " + data + "\n\n")); err != nil {
			return
		}
		fl.Flush()
	}

	ctx := r.Context()

	// 0. persist the user turn
	history := h.eng.Session(req.SessionID)
	h.eng.Push(req.SessionID, agent.Message{Role: "user", Content: req.Message, At: time.Now()})
	// Full persisted history (NOT trimmed to maxHist) for continuation-editing
	// recovery: a follow-up "把部门改成市场部" must still see the prior fill
	// markers even after a long conversation trimmed the in-memory window.
	fullHist := h.eng.FullSession(req.SessionID)

	// 1. orchestrator: decide skill
	eval, err := h.eng.EvalTurn(ctx, req.SessionID, req.Message, history)
	if err != nil {
		write(evError, jsonSafe(map[string]string{"error": "调度失败: " + err.Error()}))
		return
	}
	write(evMeta, jsonSafe(map[string]string{"reason": eval.Reason, "skill": eval.SkillSlug, "intent": eval.Intent}))

	// surface the orchestrator's reasoning pipeline for writing tasks
	// (skill hit OR general write/open-query intent), so users see the
	// multi-agent scheduling even when no skill matches; chatter (chat) stays quiet.
	showTrace := eval.SkillSlug != "" || eval.Intent == "write" || eval.Intent == "docgen" || eval.Intent == "query" || eval.NeedsTools
	if showTrace && len(eval.Steps) > 0 {
		write(evTrace, jsonSafe(eval.Steps))
	}

	// 2a. 工具循环优先：任务需要外部实时数据或真实计算时，交给 Agent 自己组合工具
	//     （优先于技能快路径——否则模型会「凭记忆编数字」生成一份看着很像的文档）
	if eval.NeedsTools {
		if h.runAgentLoop(ctx, write, req, *eval, history) {
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
		if tslug := agent.TemplateFillSkillInHistory(fullHist, req.Message); tslug != "" && tslug != eval.SkillSlug {
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
			// refresh trace so the generate step reads as "awaiting params" not active
			if len(eval.Steps) > 0 {
				waitSteps := make([]agent.TraceStep, len(eval.Steps))
				copy(waitSteps, eval.Steps)
				for i := range waitSteps {
					if waitSteps[i].Status == "active" {
						waitSteps[i].Status = "waiting"
						waitSteps[i].Detail = "等待补充：" + needsShort(eval.Needs)
					} else {
						waitSteps[i].Status = "done"
					}
				}
				write(evTrace, jsonSafe(waitSteps))
			}
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
			// mark the generate step done so the trace panel shows completion
			if len(eval.Steps) > 0 {
				doneSteps := make([]agent.TraceStep, len(eval.Steps))
				copy(doneSteps, eval.Steps)
				for i := range doneSteps {
					doneSteps[i].Status = "done"
				}
				write(evTrace, jsonSafe(doneSteps))
			}
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
				if len(eval.Steps) > 0 {
					doneSteps := make([]agent.TraceStep, len(eval.Steps))
					copy(doneSteps, eval.Steps)
					for i := range doneSteps {
						doneSteps[i].Status = "done"
					}
					write(evTrace, jsonSafe(doneSteps))
				}
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
				if len(eval.Steps) > 0 {
					waitSteps := make([]agent.TraceStep, len(eval.Steps))
					copy(waitSteps, eval.Steps)
					for i := range waitSteps {
						if waitSteps[i].Status == "active" {
							waitSteps[i].Status = "waiting"
							waitSteps[i].Detail = "信息不足，等待用户补充：" + joinQuestions(fres.Clarify.Questions)
						} else {
							waitSteps[i].Status = "done"
						}
					}
					write(evTrace, jsonSafe(waitSteps))
				}
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
				// mark generate step done for the trace panel
				if len(eval.Steps) > 0 {
					doneSteps := make([]agent.TraceStep, len(eval.Steps))
					copy(doneSteps, eval.Steps)
					for i := range doneSteps {
						doneSteps[i].Status = "done"
					}
					write(evTrace, jsonSafe(doneSteps))
				}
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
		// mark the generate step done so the trace panel shows completion
		if len(eval.Steps) > 0 {
			doneSteps := make([]agent.TraceStep, len(eval.Steps))
			copy(doneSteps, eval.Steps)
			for i := range doneSteps {
				doneSteps[i].Status = "done"
			}
			write(evTrace, jsonSafe(doneSteps))
		}
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
	// for general write intent (no skill matched), close the trace panel as done
	if showTrace && len(eval.Steps) > 0 {
		doneSteps := make([]agent.TraceStep, len(eval.Steps))
		copy(doneSteps, eval.Steps)
		for i := range doneSteps {
			doneSteps[i].Status = "done"
		}
		write(evTrace, jsonSafe(doneSteps))
	}
	write(evDone, jsonSafe(map[string]string{"skill": ""}))
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
