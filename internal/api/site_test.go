package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/model"
	"github.com/lizhemin15/skillforge/internal/store"
)

// 站点自定义名称（「整个网页叫什么名字」）的回归防线。
//
// 这一组测试守的不是「函数能不能跑」，而是三条**线上真的会静默坏掉**的契约：
//  1. GET /api/site 必须**不需要登录**就能读 —— 一旦被挪到鉴权后面，未登录访客
//     会 401，前端 catch 后静默保留默认名：功能像是"没生效"，日志里啥也看不出来。
//  2. 写接口必须**要求登录** —— 这是公网开放的服务，站名可改但不能匿名改。
//  3. 库里的空串/空白**永远不能**把站名渲染成空 —— 那会是一片空白标题（白屏级故障）。

func newSiteHandlerForTest(t *testing.T) (*Handler, *store.SkillStore) {
	t.Helper()
	s := newTestStore(t)
	h, err := NewHandler(s, llm.New(&model.LLMConfig{}), "test-secret")
	if err != nil {
		t.Fatalf("装配 Handler 失败：%v", err)
	}
	return h, s
}

// 只关心 HTTP 结果的用例用这个。
func newSiteHandlerOnly(t *testing.T) *Handler {
	t.Helper()
	h, _ := newSiteHandlerForTest(t)
	return h
}

func siteGetBody(t *testing.T, h *Handler, r *http.Request) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON：%v / %s", err, rec.Body.String())
	}
	return rec.Code, got
}

// putSite 直接调 handler（不穿中间件），用于构造"已经登录后保存"的状态。
func putSite(t *testing.T, h *Handler, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/admin/site", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Site.Update(rec, req)
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	return rec.Code, got
}

// TestSitePublicGetNeedsNoAuth 守护契约 1：匿名请求 GET /api/site 必须是 200。
// 如果这个路由被挂到 Auth.Middleware 后面，本测试会看到 401 而变红。
func TestSitePublicGetNeedsNoAuth(t *testing.T) {
	h := newSiteHandlerOnly(t)
	code, got := siteGetBody(t, h, httptest.NewRequest(http.MethodGet, "/api/site", nil))
	if code != http.StatusOK {
		t.Fatalf("匿名 GET /api/site 期望 200，实际 %d（公开接口不能挂在鉴权后面）", code)
	}
	if got["name"] != store.DefaultSiteName {
		t.Fatalf("空库期望默认站名 %q，实际 %v", store.DefaultSiteName, got["name"])
	}
	if got["tagline"] != store.DefaultSiteTagline {
		t.Fatalf("空库期望默认副标题 %q，实际 %v", store.DefaultSiteTagline, got["tagline"])
	}
	if _, leaked := got["is_custom"]; leaked {
		t.Fatalf("公开接口不应下发 is_custom（内部状态字段），实际字段表：%v", got)
	}
	if _, leaked := got["defaults"]; leaked {
		t.Fatalf("公开接口不应下发 defaults（管理端提示语用的内部字段），实际字段表：%v", got)
	}
}

// TestSiteAdminShipsDefaults 守护「前端不硬编码默认站名」这条契约的后端半边：
// 管理端页面要显示「当前为自定义名称（默认：X / Y）」，X/Y 必须来自这里下发的
// defaults。一旦后端不再下发，前端就只能自己硬编码一份 —— 同一事实写两遍，
// 改了 store.DefaultSiteName 而前端没跟，提示语就开始骗人。
// 而公开接口不下发：匿名访客只需要渲染用的 name/tagline，不需要内部字段。
func TestSiteAdminShipsDefaults(t *testing.T) {
	h := newSiteHandlerOnly(t)

	tok, err := h.Auth.issueToken("admin")
	if err != nil {
		t.Fatalf("签发测试令牌失败：%v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/admin/site", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	code, adminBody := siteGetBody(t, h, req)
	if code != http.StatusOK {
		// 这里若拿到 401，说明读管理端接口忘了带令牌 —— 别把它误读成「字段没下发」。
		t.Fatalf("带令牌 GET /api/admin/site 期望 200，实际 %d", code)
	}
	defs, ok := adminBody["defaults"].(map[string]any)
	if !ok {
		t.Fatalf("管理端接口必须下发 defaults 对象，实际字段表：%v", adminBody)
	}
	if defs["name"] != store.DefaultSiteName {
		t.Fatalf("defaults.name 期望 %q，实际 %v（前端提示语会显示错）", store.DefaultSiteName, defs["name"])
	}
	if defs["tagline"] != store.DefaultSiteTagline {
		t.Fatalf("defaults.tagline 期望 %q，实际 %v", store.DefaultSiteTagline, defs["tagline"])
	}

	_, pubBody := siteGetBody(t, h, httptest.NewRequest(http.MethodGet, "/api/site", nil))
	if _, leaked := pubBody["defaults"]; leaked {
		t.Fatalf("公开接口不应下发 defaults，实际字段表：%v", pubBody)
	}
}

// TestSiteAdminWriteNeedsAuth 守护契约 2：匿名 GET/PUT /api/admin/site 都必须被拒。
func TestSiteAdminWriteNeedsAuth(t *testing.T) {
	h := newSiteHandlerOnly(t)
	mux := h.Routes()
	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/api/admin/site", ""},
		{http.MethodPut, "/api/admin/site", `{"name":"hacked"}`},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Fatalf("匿名 %s %s 竟然返回 200（管理接口漏鉴权）", tc.method, tc.path)
		}
	}
	// 顺带确认「被拒之后库里没被写脏」。
	code, got := siteGetBody(t, h, httptest.NewRequest(http.MethodGet, "/api/site", nil))
	if code != http.StatusOK || got["name"] != store.DefaultSiteName {
		t.Fatalf("匿名写入被拒后站名不该变化，实际 code=%d name=%v", code, got["name"])
	}
}

// TestSiteCustomNameVisibleOnPublicEndpoint 是这次需求的主路径：
// 管理端保存 → 匿名访客（未登录、首页）读到的就是新名字。
func TestSiteCustomNameVisibleOnPublicEndpoint(t *testing.T) {
	h := newSiteHandlerOnly(t)
	code, got := putSite(t, h, `{"name":"翻译工坊","tagline":"离线文档流水线"}`)
	if code != http.StatusOK {
		t.Fatalf("保存失败 code=%d got=%v", code, got)
	}
	if got["name"] != "翻译工坊" || got["tagline"] != "离线文档流水线" {
		t.Fatalf("保存响应回显不对：%v", got)
	}
	if ic, ok := got["is_custom"].(map[string]any); !ok || ic["name"] != true || ic["tagline"] != true {
		t.Fatalf("保存后 is_custom 应为 name/tagline 双 true，实际 %v", got["is_custom"])
	}

	code, pub := siteGetBody(t, h, httptest.NewRequest(http.MethodGet, "/api/site", nil))
	if code != http.StatusOK {
		t.Fatalf("匿名 GET 期望 200，实际 %d", code)
	}
	if pub["name"] != "翻译工坊" || pub["tagline"] != "离线文档流水线" {
		t.Fatalf("匿名访客读到的站名不对：%v", pub)
	}
}

// TestSiteOnlySubmittedFieldChanges 守护「只改一个字段」：
// 老前端可能只提交 name（tagline 字段缺省），此时副标题必须原样保留。
// 直觉写法（两个字段都用零值覆盖）会把副标题静默清空 —— 那正是本测试要抓的。
func TestSiteOnlySubmittedFieldChanges(t *testing.T) {
	h := newSiteHandlerOnly(t)
	if code, _ := putSite(t, h, `{"name":"甲","tagline":"乙"}`); code != http.StatusOK {
		t.Fatalf("前置保存失败 code=%d", code)
	}
	if code, got := putSite(t, h, `{"name":"丙"}`); code != http.StatusOK {
		t.Fatalf("单字段保存失败 code=%d got=%v", code, got)
	}
	_, pub := siteGetBody(t, h, httptest.NewRequest(http.MethodGet, "/api/site", nil))
	if pub["name"] != "丙" {
		t.Fatalf("name 应更新为 丙，实际 %v", pub["name"])
	}
	if pub["tagline"] != "乙" {
		t.Fatalf("未提交的 tagline 应原样保留 乙，实际 %v（被零值覆盖了）", pub["tagline"])
	}
}

// TestSiteEmptyMeansRestoreDefault 守护「清空 = 恢复默认」：
// 空串要**删键**而不是写空值，恢复后匿名接口必须重新给出默认文案。
func TestSiteEmptyMeansRestoreDefault(t *testing.T) {
	h := newSiteHandlerOnly(t)
	putSite(t, h, `{"name":"临时名","tagline":"临时副标题"}`)
	code, got := putSite(t, h, `{"name":"","tagline":""}`)
	if code != http.StatusOK {
		t.Fatalf("恢复默认失败 code=%d got=%v", code, got)
	}
	if got["name"] != store.DefaultSiteName {
		t.Fatalf("恢复默认后 name 应为 %q，实际 %v", store.DefaultSiteName, got["name"])
	}
	if ic, _ := got["is_custom"].(map[string]any); ic["name"] != false || ic["tagline"] != false {
		t.Fatalf("恢复默认后 is_custom 应为双 false，实际 %v", got["is_custom"])
	}
	// 纯空白也算空。
	putSite(t, h, `{"name":"带空格"}`)
	if code, got := putSite(t, h, `{"name":"   "}`); code != http.StatusOK || got["name"] != store.DefaultSiteName {
		t.Fatalf("纯空白应等同清空，实际 code=%d name=%v", code, got["name"])
	}
}

// TestSiteEmptyDeletesRowNotWritesBlank 压住「空串 = 恢复默认」的实现细节：
// 清空必须**删键**，不能在 settings 表里留一行 val=”。
//
// 为什么较真：留下空行会让「这行存在」这件事失去含义。当前 snapshot 靠
// GetSettingsRaw 判 is_custom，存空串恰好也能得到 is_custom=false 而看不出问题；
// 但这意味着两处判据（有没有行 / 值是不是空）会漂移，将来任何「列出已配置项」
// 之类的逻辑都会把这条空行当成用户配置过。删键让两种判据永远一致。
func TestSiteEmptyDeletesRowNotWritesBlank(t *testing.T) {
	h, s := newSiteHandlerForTest(t)
	if code, _ := putSite(t, h, `{"name":"甲","tagline":"乙"}`); code != http.StatusOK {
		t.Fatalf("前置保存失败 code=%d", code)
	}
	raw, err := s.GetSettingsRaw(store.SettingSiteName, store.SettingSiteTagline)
	if err != nil {
		t.Fatalf("读库失败：%v", err)
	}
	if _, ok := raw[store.SettingSiteName]; !ok {
		t.Fatalf("保存后库里应该有一行，实际 %v", raw)
	}

	if code, _ := putSite(t, h, `{"name":"","tagline":""}`); code != http.StatusOK {
		t.Fatalf("清空失败 code=%d", code)
	}
	raw, err = s.GetSettingsRaw(store.SettingSiteName, store.SettingSiteTagline)
	if err != nil {
		t.Fatalf("读库失败：%v", err)
	}
	if _, ok := raw[store.SettingSiteName]; ok {
		t.Fatalf("清空后 settings 表里不该留下 %s 这行（应删键，不是写空值），实际 %v",
			store.SettingSiteName, raw)
	}
	if _, ok := raw[store.SettingSiteTagline]; ok {
		t.Fatalf("清空后 settings 表里不该留下 %s 这行，实际 %v", store.SettingSiteTagline, raw)
	}
}

// TestSiteStoredEmptyNeverRendersBlank 守护契约 3：DB 里被写进空串/空白的极端数据，
// 公开接口也不能把站名渲染成空（空白标题是白屏级故障）。
func TestSiteStoredEmptyNeverRendersBlank(t *testing.T) {
	s := newTestStore(t)
	h, err := NewHandler(s, llm.New(&model.LLMConfig{}), "test-secret")
	if err != nil {
		t.Fatalf("装配 Handler 失败：%v", err)
	}
	if err := s.SetSettings(map[string]string{
		store.SettingSiteName:    "   ",
		store.SettingSiteTagline: "",
	}); err != nil {
		t.Fatalf("直接写库失败：%v", err)
	}
	_, pub := siteGetBody(t, h, httptest.NewRequest(http.MethodGet, "/api/site", nil))
	if name, _ := pub["name"].(string); strings.TrimSpace(name) == "" {
		t.Fatalf("库里是空白时公开接口不能返回空站名，实际 %q", name)
	}
}

// TestSiteUpdateValidation 边界：换行/控制字符拒绝、超长拒绝、正好卡上限放行。
func TestSiteUpdateValidation(t *testing.T) {
	h := newSiteHandlerOnly(t)

	bad := []struct {
		name string
		body string
	}{
		{"含换行", `{"name":"第一行\n第二行"}`},
		{"含制表符", `{"name":"a\tb"}`},
		{"名称超长", `{"name":"` + strings.Repeat("字", 31) + `"}`},
		{"副标题超长", `{"tagline":"` + strings.Repeat("字", 41) + `"}`},
		{"字段全缺", `{}`},
		{"非法 JSON", `{"name":`},
	}
	for _, tc := range bad {
		if code, got := putSite(t, h, tc.body); code != http.StatusBadRequest {
			t.Fatalf("%s：期望 400，实际 %d / %v", tc.name, code, got)
		}
	}

	// 正好 30 / 40 字必须放行（边界不能多拒一个字）。
	okBody := `{"name":"` + strings.Repeat("名", 30) + `","tagline":"` + strings.Repeat("副", 40) + `"}`
	code, got := putSite(t, h, okBody)
	if code != http.StatusOK {
		t.Fatalf("正好卡上限应放行，实际 %d / %v", code, got)
	}
	if n := len([]rune(got["name"].(string))); n != 30 {
		t.Fatalf("站名应为 30 字，实际 %d", n)
	}

	// 存储里的旧值不能被失败请求污染：上面最后一个成功请求是 30 字名，仍然保持。
	_, pub := siteGetBody(t, h, httptest.NewRequest(http.MethodGet, "/api/site", nil))
	if pub["name"] != got["name"] {
		t.Fatalf("校验失败的请求不该改动已存值：期望 %v，实际 %v", got["name"], pub["name"])
	}
}

// TestSiteNameKeepsMarkupVerbatim 记录一条**有意的**设计决定：
// 后端不剥 HTML，`<b>x</b>` 原样存、原样下发，转义责任在前端（前端一律 textContent）。
// 谁哪天想在后端"顺手消毒"，请先看这条测试和 site.go 的注释 —— 静默改写用户输入
// 会让用户更难排查，而且前端的 textContent 才是真正的 XSS 防线。
func TestSiteNameKeepsMarkupVerbatim(t *testing.T) {
	h := newSiteHandlerOnly(t)
	const raw = `<b>甲</b>`
	if code, got := putSite(t, h, `{"name":"`+raw+`"}`); code != http.StatusOK || got["name"] != raw {
		t.Fatalf("后端应原样保留输入，实际 code=%d name=%v", code, got["name"])
	}
	_, pub := siteGetBody(t, h, httptest.NewRequest(http.MethodGet, "/api/site", nil))
	if pub["name"] != raw {
		t.Fatalf("公开接口也应原样下发，实际 %v", pub["name"])
	}
}
