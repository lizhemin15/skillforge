package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/lizhemin15/skillforge/internal/store"
)

// 站点自定义设置：整个网页叫什么名字。
//
// 契约要点：
//   - GET /api/site 是**公开**的，且必须公开 —— 未登录用户打开首页也要看到正确站名。
//   - PUT 走鉴权（改站名是管理动作）。
//   - 空串 = 恢复默认（删键），所以「清空输入框再保存」语义明确，不需要单独的恢复接口。
//   - 只校验长度与换行；**转义责任在前端**（前端一律 textContent）。这里不剥 HTML ——
//     把用户输入的 `<b>` 静默改成 `b` 更难排查。
const (
	maxSiteNameRunes    = 30
	maxSiteTaglineRunes = 40
)

type Site struct{ store *store.SkillStore }

func NewSite(s *store.SkillStore) *Site { return &Site{store: s} }

// siteView 下发给前端。isCustom / defaults 只给管理端（公开接口少暴露一点信息面）。
//
// defaults 存在的理由：管理端的「当前为自定义名称（默认：X / Y）」提示语要显示默认值。
// 让前端自己硬编码一份默认值，等于把同一份事实写两遍 —— 改了 store.DefaultSiteName
// 忘了改前端，提示语就开始骗人（本字段第一版是 `defaulted map[string]bool`，结构体里
// 声明了却从没赋值，前端于是只能自己硬编码，正好踩了这个坑）。
type siteView struct {
	Name     string            `json:"name"`
	Tagline  string            `json:"tagline"`
	IsCustom map[string]bool   `json:"is_custom,omitempty"`
	Defaults map[string]string `json:"defaults,omitempty"`
}

// snapshot 一次读全，避免 view() 里读两遍 DB 还各自兜底。
//
// 注意 is_custom 的判据：必须用 **GetSettingsRaw**（DB 里真的存过的键），
// 不能用 GetSettings 的结果 —— 后者会给缺失的键补默认值，于是「恢复默认」
// 之后读到的仍是默认文案（非空），is_custom 会永远为 true，管理端于是显示
// 「当前为自定义名称」。site_test.go 的 TestSiteEmptyMeansRestoreDefault 守这条。
func (h *Site) snapshot() (siteView, error) {
	kv, err := h.store.GetSettings(store.SettingSiteName, store.SettingSiteTagline)
	if err != nil {
		return siteView{}, err
	}
	raw, err := h.store.GetSettingsRaw(store.SettingSiteName, store.SettingSiteTagline)
	if err != nil {
		return siteView{}, err
	}
	rawName := strings.TrimSpace(kv[store.SettingSiteName])
	rawTag := strings.TrimSpace(kv[store.SettingSiteTagline])
	v := siteView{
		Name:    rawName,
		Tagline: rawTag,
		IsCustom: map[string]bool{
			"name":    strings.TrimSpace(raw[store.SettingSiteName]) != "",
			"tagline": strings.TrimSpace(raw[store.SettingSiteTagline]) != "",
		},
		Defaults: map[string]string{
			"name":    store.DefaultSiteName,
			"tagline": store.DefaultSiteTagline,
		},
	}
	// DB 里存过空串（历史数据/手改）也不能把站名渲染成空 —— 名称永远有兜底。
	if v.Name == "" {
		v.Name = store.DefaultSiteName
	}
	return v, nil
}

// Get 公开：GET /api/site
func (h *Site) Get(w http.ResponseWriter, r *http.Request) {
	v, err := h.snapshot()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取站点设置失败")
		return
	}
	v.IsCustom = nil // 公开接口不下发「哪项被改过」
	v.Defaults = nil // 也不下发默认值：公开页面只需要渲染用的 name/tagline
	writeJSON(w, http.StatusOK, v)
}

// GetAdmin 鉴权：GET /api/admin/site —— 表单回填 + 「是否已自定义」提示。
func (h *Site) GetAdmin(w http.ResponseWriter, r *http.Request) {
	v, err := h.snapshot()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取站点设置失败")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// Update 鉴权：PUT /api/admin/site —— 空串表示恢复默认。
func (h *Site) Update(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    *string `json:"name"`
		Tagline *string `json:"tagline"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if req.Name == nil && req.Tagline == nil {
		writeErr(w, http.StatusBadRequest, "没有要修改的字段")
		return
	}
	name, err := cleanSiteText(req.Name, maxSiteNameRunes, "站点名称")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	tagline, err := cleanSiteText(req.Tagline, maxSiteTaglineRunes, "站点副标题")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// 空串 = 恢复默认：删键而不是写空值，让「没设置过」和「设成空」是同一状态。
	var toSet map[string]string
	var toReset []string
	if req.Name != nil {
		if name == "" {
			toReset = append(toReset, store.SettingSiteName)
		} else {
			if toSet == nil {
				toSet = map[string]string{}
			}
			toSet[store.SettingSiteName] = name
		}
	}
	if req.Tagline != nil {
		if tagline == "" {
			toReset = append(toReset, store.SettingSiteTagline)
		} else {
			if toSet == nil {
				toSet = map[string]string{}
			}
			toSet[store.SettingSiteTagline] = tagline
		}
	}
	if err := h.store.ResetSettings(toReset...); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存站点设置失败")
		return
	}
	if err := h.store.SetSettings(toSet); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存站点设置失败")
		return
	}

	v, err := h.snapshot()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存后读取失败")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// cleanSiteText 去首尾空白、拒换行/控制字符、限长。字段没提交（nil）返回空串。
func cleanSiteText(p *string, maxRunes int, label string) (string, error) {
	if p == nil {
		return "", nil
	}
	raw := strings.TrimSpace(*p)
	for _, r := range raw {
		if r == '\n' || r == '\r' || r == '\t' || unicode.IsControl(r) {
			return "", fmt.Errorf("%s不能包含换行或控制字符", label)
		}
	}
	if n := utf8.RuneCountInString(raw); n > maxRunes {
		return "", fmt.Errorf("%s最长 %s 个字", label, strconv.Itoa(maxRunes))
	}
	return raw, nil
}
