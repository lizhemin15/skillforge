package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/model"
	"github.com/lizhemin15/skillforge/internal/store"
)

// Skills serves public skill endpoints: list, detail, generate (SSE).
type Skills struct {
	store *store.SkillStore
}

func NewSkills(s *store.SkillStore) *Skills {
	return &Skills{store: s}
}

// List returns all enabled skills (public).
//
// 这是**公开**口径（对话页技能勾选层、首页卡片）：停用技能不出现在这里。
// 注意 `!s.Enabled && !s.IsCore` —— 核心技能即使停用也仍会列出来，于是点它会
// 拿到 Generate 的 403「该技能当前不可用」。这条历史行为本轮**没动**（不在
// 抱怨范围内，改动面越小越好），但它是个「看着能用、点了报错」的坑。
//
// ⚠️ 管理端别拿这个接口渲染管理列表 —— 见 ListAll。
func (h *Skills) List(w http.ResponseWriter, r *http.Request) {
	skills, err := h.store.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var out []model.Skill
	for _, s := range skills {
		if !s.Enabled && !s.IsCore {
			continue
		}
		out = append(out, s)
	}
	// Add core skill-generator to the listing tail
	writeJSON(w, http.StatusOK, map[string]any{"skills": out, "count": len(out)})
}

// ListAll returns every skill, **disabled ones included**. 管理端专用（挂鉴权）。
//
// 为什么必须有这个接口（Bug N，用户报「业务技能停用了就消失了」）：
// 管理端「技能管理」原先打的是上面的公开 List，而它自己却渲染了
// `<span class="pill-off">已停用</span>` 和「启用」按钮 —— 渲染代码永远
// 收不到停用技能，于是**停用一次技能就从界面上蒸发**，想再启用只能直接改
// sqlite，停用变成了不可逆操作。管理列表要的是全量，公开列表要的是可用，
// 两者口径不同，就不能共用一个 handler。
func (h *Skills) ListAll(w http.ResponseWriter, r *http.Request) {
	skills, err := h.store.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if skills == nil {
		skills = []model.Skill{}
	}
	disabled := 0
	for _, s := range skills {
		if !s.Enabled {
			disabled++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"skills": skills, "count": len(skills), "disabled_count": disabled,
	})
}

// Get returns one skill's detail.
func (h *Skills) Get(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	sk, err := h.store.Get(slug)
	if err != nil {
		writeErr(w, http.StatusNotFound, "技能不存在")
		return
	}
	writeJSON(w, http.StatusOK, sk)
}

// Generate streams an article via SSE. Body: {slug, input{...}}.
func (h *Skills) Generate(w http.ResponseWriter, r *http.Request) {
	var req model.GenerateRequest
	if err := readBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体无效")
		return
	}
	sk, err := h.store.Get(req.Slug)
	if err != nil {
		writeErr(w, http.StatusNotFound, "技能不存在")
		return
	}
	if !sk.Enabled {
		writeErr(w, http.StatusForbidden, "该技能当前不可用")
		return
	}
	// Build LLM client from active config (hot-swap every request).
	lcfg, err := h.store.GetActiveLLM()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "未配置 LLM 服务（请在管理端配置）")
		return
	}
	lcli := llm.New(lcfg)

	// Build user prompt from skill's template + dynamic inputs.
	userPrompt := buildUserPrompt(sk, req.Input)

	// SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "流式输出不可用")
		return
	}

	send := func(ev model.StreamEvent) {
		b, _ := json.Marshal(ev)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	send(model.StreamEvent{Type: "meta", Meta: &model.Message{Slug: sk.Slug, Title: sk.Name}})

	system, err := h.store.SystemPrompt(sk.Slug)
	if err != nil {
		send(model.StreamEvent{Type: "error", Data: "读取技能失败: " + err.Error()})
		return
	}

	var buf strings.Builder
	_, err = lcli.Complete(r.Context(), system, userPrompt, func(delta string) {
		buf.WriteString(delta)
		send(model.StreamEvent{Type: "delta", Data: delta})
	})
	if err != nil {
		send(model.StreamEvent{Type: "error", Data: err.Error()})
		return
	}
	id, _ := h.store.SaveArticle(sk.Slug, firstLine(buf.String()), buf.String())
	send(model.StreamEvent{Type: "done", Data: fmt.Sprintf("%d", id)})
}

// buildUserPrompt assembles the user-facing prompt for a generate request.
func buildUserPrompt(sk *model.Skill, input map[string]string) string {
	var sb strings.Builder
	// Fill inputs that the skill declares.
	for _, p := range sk.InputParams {
		if v, ok := input[p.Name]; ok && strings.TrimSpace(v) != "" {
			sb.WriteString(fmt.Sprintf("%s：%s\n", p.Label, strings.TrimSpace(v)))
		}
	}
	// Any extra keys the client sent but skill didn't declare.
	for k, v := range input {
		declared := false
		for _, p := range sk.InputParams {
			if p.Name == k {
				declared = true
				break
			}
		}
		if !declared && strings.TrimSpace(v) != "" {
			sb.WriteString(fmt.Sprintf("%s：%s\n", k, strings.TrimSpace(v)))
		}
	}
	if sb.Len() == 0 {
		return "请根据你的专业写出这篇文章（未提供额外要求）。"
	}
	return "请撰写文章。用户输入如下：\n" + sb.String()
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(strings.TrimPrefix(l, "#"))
		l = strings.TrimSpace(l)
		if l != "" {
			return l[:min(60, len(l))]
		}
	}
	return "生成的文章"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
