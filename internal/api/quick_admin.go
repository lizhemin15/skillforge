package api

// 极速写作模式的系统提示词：管理端配置入口。
//
// 契约与 site.go 同构，差异点在「文本形态」：
//   - 站名/副标题是单行文本：cleanSiteText 拒绝换行与控制字符；
//   - 系统提示词天然是**多行**文本（文体要求 + 结构要求逐条列出），
//     换行合法，只拒控制字符。所以校验单写一个 cleanPromptText，
//     不复用 cleanSiteText —— 硬套会把每一份真实提示词判成非法。
//   - 同样「空串 = 恢复默认」：删键而不是写空值，与 site 的语义一致。
//
// GET/PUT 都走鉴权（管理动作）。用户端不读这个接口——生成时服务端现读
// store（见 agent.QuickPrompt），提示词内容不需要也不应该下发给访客。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/lizhemin15/skillforge/internal/store"
)

// maxQuickPromptRunes 管理端可写的提示词上限（按字符计，8000 字够写任何
// 文体规范还富余）。必须有上限：settings 表没长度约束，一次粘贴 50KB
// 会让每轮生成的 system prompt 比正文还长。
const maxQuickPromptRunes = 8000

type Quick struct{ store *store.SkillStore }

func NewQuick(s *store.SkillStore) *Quick { return &Quick{store: s} }

type quickView struct {
	Prompt   string `json:"prompt"`
	IsCustom bool   `json:"is_custom"`
	// DefaultPrompt 给管理端做「恢复默认」的回填与「当前为默认」的提示。
	// 与 siteView.Defaults 同一理由：让前端自己硬编码一份默认值，等于把
	// 同一份事实写两遍，改了 store.DefaultQuickPrompt 忘了改前端就开始骗人。
	DefaultPrompt string `json:"default_prompt"`
}

// snapshot 读一次全量（生效值 + 是否自定义）。is_custom 用 GetSettingsRaw
// 判（理由见 site.go snapshot：GetSettings 会补默认值，拿它判会把
// 「已恢复默认」误报成「已自定义」）。
func (h *Quick) snapshot() (quickView, error) {
	kv, err := h.store.GetSettings(store.SettingQuickPrompt)
	if err != nil {
		return quickView{}, err
	}
	raw, err := h.store.GetSettingsRaw(store.SettingQuickPrompt)
	if err != nil {
		return quickView{}, err
	}
	v := quickView{
		Prompt:        strings.TrimSpace(kv[store.SettingQuickPrompt]),
		IsCustom:      strings.TrimSpace(raw[store.SettingQuickPrompt]) != "",
		DefaultPrompt: store.DefaultQuickPrompt,
	}
	if v.Prompt == "" {
		v.Prompt = store.DefaultQuickPrompt
	}
	return v, nil
}

// GetAdmin GET /api/admin/quick —— 表单回填 + 「当前为默认/自定义」提示。
func (h *Quick) GetAdmin(w http.ResponseWriter, r *http.Request) {
	v, err := h.snapshot()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取极速写作设置失败")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// UpdateAdmin PUT /api/admin/quick —— 空串表示恢复默认。
func (h *Quick) UpdateAdmin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt *string `json:"prompt"`
	}
	// 上限放宽到 24KB（提示词 8000 汉字 ≈ 24KB UTF-8，正好兜住）。
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 24<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if req.Prompt == nil {
		writeErr(w, http.StatusBadRequest, "没有要修改的字段")
		return
	}
	prompt, err := cleanPromptText(req.Prompt)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if prompt == "" {
		// 空串 = 恢复默认：删键（「没设置过」和「设成空」是同一状态）。
		if err := h.store.ResetSettings(store.SettingQuickPrompt); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存极速写作设置失败")
			return
		}
	} else {
		if err := h.store.SetSettings(map[string]string{store.SettingQuickPrompt: prompt}); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存极速写作设置失败")
			return
		}
	}
	v, err := h.snapshot()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存后读取失败")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// cleanPromptText 去首尾空白、拒控制字符（换行除外——提示词天然多行）、限长。
func cleanPromptText(p *string) (string, error) {
	if p == nil {
		return "", nil
	}
	raw := strings.TrimSpace(*p)
	for _, r := range raw {
		if r == '\n' || r == '\t' {
			continue
		}
		if r == '\r' {
			return "", fmt.Errorf("提示词不能包含 CR 换行（用 LF 换行）")
		}
		if unicode.IsControl(r) {
			return "", fmt.Errorf("提示词不能包含控制字符")
		}
	}
	if n := utf8.RuneCountInString(raw); n > maxQuickPromptRunes {
		return "", fmt.Errorf("提示词最长 %d 个字，当前 %d 个字", maxQuickPromptRunes, n)
	}
	return raw, nil
}
