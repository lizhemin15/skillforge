package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/model"
	"github.com/lizhemin15/skillforge/internal/store"
)

// 「停用」必须是可逆的 —— 后端契约。
//
// 用户报的原文：「业务技能停用了就消失了」。
//
// 现场是这样的：管理端「技能管理」列表打的是**公开**的 /api/skills
// （Skills.List），而公开列表按设计把停用技能过滤掉。于是管理员点一次
// 「停用」，那一行就从界面上蒸发 —— 连它自己的「启用」按钮和「已停用」
// 药丸一起带走（渲染代码还在 admin.js 里，只是永远收不到数据）。
// 结果：停用 = 不可逆操作，想恢复只能直接改 sqlite。
//
// 修法：管理端走 ListAll（全量，含停用），公开口径保持不动。
// 这个用例守的就是「两个口径不许再被合成一个」。

// mkBizSkill 造一个业务技能（非核心），带 0 个参数即可 —— 本用例只关心列表。
func mkBizSkill(t *testing.T, s *store.SkillStore, slug, name string) {
	t.Helper()
	err := s.Create(&model.Skill{
		Slug: slug, Name: name, Description: "验收造的业务技能",
		Category: "验收", Version: 1, Enabled: true, IsCore: false,
	}, nil)
	if err != nil {
		t.Fatalf("造业务技能 %s 失败：%v", slug, err)
	}
}

// listSlugs 从列表响应里取出 (slug -> enabled)，顺便把 count 一起要回来。
func listSlugs(t *testing.T, h http.HandlerFunc) (map[string]bool, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("列表接口返回 %d：%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Skills []struct {
			Slug    string `json:"slug"`
			Enabled bool   `json:"enabled"`
		} `json:"skills"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON：%v / %s", err, rec.Body.String())
	}
	out := map[string]bool{}
	for _, s := range body.Skills {
		out[s.Slug] = s.Enabled
	}
	return out, body.Count
}

// toggleSkill 走真实 handler（不穿中间件，与 putSite 同一约定）。
func toggleSkill(t *testing.T, h *Handler, slug string, enabled bool) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"slug": slug, "enabled": enabled})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/skills/toggle", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Admin.ToggleSkill(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle(%s,%v) 返回 %d：%s", slug, enabled, rec.Code, rec.Body.String())
	}
}

func TestDisabledBizSkillStillListedForAdmin(t *testing.T) {
	h, s := newSiteHandlerForTest(t)
	const slug = "biz-toggle-roundtrip"
	mkBizSkill(t, s, slug, "停用往返验收技能")

	// 前置断言：造出来的技能必须真在**全量**列表里。
	// 少了这一条，后面「公开列表不含它」在技能压根没建成的世界里也恒真 —— 空跑绿。
	adminSlugs, _ := listSlugs(t, h.Skills.ListAll)
	if _, ok := adminSlugs[slug]; !ok {
		t.Fatalf("全量列表里没有刚造出来的 %s —— 前置就不成立，后面的断言全是空跑", slug)
	}

	toggleSkill(t, h, slug, false)

	// ① 公开口径照旧：停用技能不出现在对话页/首页（这条**不该**被本次修复改坏）
	pubSlugs, _ := listSlugs(t, h.Skills.List)
	if _, ok := pubSlugs[slug]; ok {
		t.Errorf("停用后 %s 仍出现在公开列表 /api/skills —— 公开口径不该放行停用技能", slug)
	}

	// ② 管理口径：必须还在，且 enabled=false（管理端要拿它渲染「启用」按钮）
	adminSlugs, count := listSlugs(t, h.Skills.ListAll)
	enabled, ok := adminSlugs[slug]
	if !ok {
		t.Fatalf("停用后 %s 从管理端列表消失了 —— 用户报的就是这个（停用变成不可逆）", slug)
	}
	if enabled {
		t.Errorf("管理端列的 %s 仍报 enabled=true，与库里的停用状态不一致", slug)
	}
	if count != len(adminSlugs) {
		t.Errorf("count=%d 与 skills 长度 %d 不一致", count, len(adminSlugs))
	}

	// ③ 往返：再启用回来，两个口径都要恢复
	toggleSkill(t, h, slug, true)
	pubSlugs, _ = listSlugs(t, h.Skills.List)
	if _, ok := pubSlugs[slug]; !ok {
		t.Errorf("重新启用后 %s 没回到公开列表 —— 停用/启用不是可逆闭环", slug)
	}
	adminSlugs, _ = listSlugs(t, h.Skills.ListAll)
	if enabled, ok := adminSlugs[slug]; !ok || !enabled {
		t.Errorf("重新启用后管理端仍看不到 %s（or enabled=false）", slug)
	}
}

// 管理端「编辑技能元信息」也要走全量列表：拿公开列表预填表单时，编辑一个
// **已停用**技能会读到空对象 → `if (!sk) return;` 静默跳过 → 输入框全空，
// 用户一保存就把名称/描述清空（同一个根因的第二个症状）。
// 这里守的是后端侧的那一半：停用技能的字段必须拿得到。
func TestDisabledSkillFieldsReadableForAdmin(t *testing.T) {
	h, s := newSiteHandlerForTest(t)
	const slug = "biz-meta-prefill"
	mkBizSkill(t, s, slug, "元信息预填验收技能")

	toggleSkill(t, h, slug, false)

	rec := httptest.NewRecorder()
	h.Skills.ListAll(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("全量列表返回 %d", rec.Code)
	}
	var body struct {
		Skills []struct {
			Slug string `json:"slug"`
			Name string `json:"name"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON：%v", err)
	}
	for _, sk := range body.Skills {
		if sk.Slug != slug {
			continue
		}
		if strings.TrimSpace(sk.Name) == "" {
			t.Fatalf("停用技能的 name 是空的 —— 编辑表单预填会拿到空值，一保存就清空元信息")
		}
		return
	}
	t.Fatalf("停用技能 %s 不在全量列表里，管理端取不到它的 name/description", slug)
}
