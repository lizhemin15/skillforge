package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 核心技能（= 通用能力）的后端契约。
//
// 守三条会**静默坏掉**的契约：
//  1. /api/admin/skills/core 必须真的落库，且不存在的技能不许返回 200
//     （返回 200 会让管理端显示"已设为核心"，而库里什么都没有）。
//  2. /api/skills 必须把 is_core 透出来 —— 前端靠它把"通用能力"分组置顶，
//     字段一丢，核心技能会混进业务技能里，功能像"没做"。
//  3. 核心技能必须排在列表最前面，且**删不掉**（防手滑删掉办公文档管家）。
func coreTestHandler(t *testing.T) *Handler {
	t.Helper()
	return newSiteHandlerOnly(t)
}

func postCore(t *testing.T, h *Handler, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/skills/core", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	// 直调 handler，跳过鉴权中间件（登录态本身由 site_test 那组覆盖）。
	h.Admin.SetSkillCore(rec, req)
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	return rec.Code, got
}

// 列表里按 slug 找技能。
func findSkill(t *testing.T, list []map[string]any, slug string) map[string]any {
	t.Helper()
	for _, it := range list {
		if it["slug"] == slug {
			return it
		}
	}
	t.Fatalf("列表里找不到技能 %q", slug)
	return nil
}

// isCoreOf 读 is_core。注意 Go 的 omitempty：**false 会被整个省略**，
// 所以"键不存在"和"值为 false"是同一件事 —— 断言写成 != true 才安全，
// 直接比 == false 会在字段缺失时拿到 nil 而失败（本轮实测踩过）。
func isCoreOf(t *testing.T, it map[string]any) bool {
	t.Helper()
	return it["is_core"] == true
}

func publicSkills(t *testing.T, h *Handler) []map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Skills.List(rec, httptest.NewRequest(http.MethodGet, "/api/skills", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/skills = %d", rec.Code)
	}
	var body struct {
		Skills []map[string]any `json:"skills"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("技能列表不是 JSON：%v / %s", err, rec.Body.String())
	}
	return body.Skills
}

func TestSetSkillCorePersistsAndExposesFlag(t *testing.T) {
	h := coreTestHandler(t)

	// 内置核心技能：办公文档管家 / 技能工厂（seed 时就该在）
	list := publicSkills(t, h)
	for _, slug := range []string{"办公文档管家", "技能工厂"} {
		it := findSkill(t, list, slug)
		if !isCoreOf(t, it) {
			t.Fatalf("通用能力 %s 必须是核心，实际 is_core=%v", slug, it["is_core"])
		}
	}

	// 造一个业务技能，把它设为核心，再取消
	code, body := postCore(t, h, `{"slug":"办公文档管家","is_core":false}`)
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("取消核心失败：%d %v", code, body)
	}
	if got := findSkill(t, publicSkills(t, h), "办公文档管家"); isCoreOf(t, got) {
		t.Fatalf("取消核心没落库，is_core=%v", got["is_core"])
	}
	if code, _ := postCore(t, h, `{"slug":"办公文档管家","is_core":true}`); code != http.StatusOK {
		t.Fatalf("设回核心失败：%d", code)
	}
	if got := findSkill(t, publicSkills(t, h), "办公文档管家"); !isCoreOf(t, got) {
		t.Fatalf("设为核心没落库，is_core=%v", got["is_core"])
	}
}

// 不存在的技能必须 404，不能静默成功。
func TestSetSkillCoreUnknownSlugIs404(t *testing.T) {
	h := coreTestHandler(t)
	code, body := postCore(t, h, `{"slug":"根本不存在的技能","is_core":true}`)
	if code != http.StatusNotFound {
		t.Fatalf("对不存在的技能设核心应返回 404，实际 %d %v", code, body)
	}
}

// 缺字段/坏 JSON 一律 400：把"没选技能"和"选了不存在的技能"分开报。
func TestSetSkillCoreBadRequest(t *testing.T) {
	h := coreTestHandler(t)
	cases := []struct{ name, body string }{
		{"缺 is_core", `{"slug":"办公文档管家"}`},
		{"缺 slug", `{"is_core":true}`},
		{"空 slug", `{"slug":"","is_core":true}`},
		{"坏 JSON", `{`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if code, body := postCore(t, h, c.body); code != http.StatusBadRequest {
				t.Fatalf("应返回 400，实际 %d %v", code, body)
			}
		})
	}
}

// is_core 必须排在前面：前端第一屏就是技能列表，顺序错了核心技能会被淹掉。
func TestPublicSkillsOrderCoreFirst(t *testing.T) {
	h := coreTestHandler(t)
	list := publicSkills(t, h)
	if len(list) < 2 {
		t.Fatalf("技能数太少，测不出顺序：%d", len(list))
	}
	seenBiz := false
	for i, it := range list {
		if isCoreOf(t, it) {
			if seenBiz {
				t.Fatalf("核心技能排在业务技能后面（第 %d 位 %v）", i, it["slug"])
			}
		} else {
			seenBiz = true
		}
	}
}
