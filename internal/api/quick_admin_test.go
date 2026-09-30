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

// 极速写作系统提示词（GET/PUT /api/admin/quick）的回归防线。
//
// 这是「极速版」唯一的管理入口，守护三条线上会静默坏掉的契约：
//  1. 路由必须真的注册在 Auth.Middleware 后面 —— 漏注册 = 管理端整个卡片
//     永远「读取失败」，而 Go 侧编译期根本发现不了。
//  2. GET 必须下发 default_prompt —— 管理端「恢复默认」的回填与「当前为默认」
//     提示全靠它，不下发前端就只能硬编码一份，同一事实写两遍必骗人。
//  3. 空串保存 = 恢复默认（is_custom 归 false）——「清空保存」是用户摆脱
//     一段坏提示词的唯一路径，如果空串被当成「设成空字符串」存下去，
//     QuickPrompt() 会回落默认，但 is_custom 还挂在那，界面从此谎报状态。

func newQuickHandlerForTest(t *testing.T) *Handler {
	t.Helper()
	s := newTestStore(t)
	if err := s.BootstrapAdmin("admin", HashPassword("pw"+"-quick"+"-test0001")); err != nil {
		t.Fatalf("种管理员失败：%v", err)
	}
	h, err := NewHandler(s, llm.New(&model.LLMConfig{}), "test-secret")
	if err != nil {
		t.Fatalf("装配 Handler 失败：%v", err)
	}
	return h
}

func quickToken(t *testing.T, h *Handler) string {
	t.Helper()
	tok, err := h.Auth.issueToken("admin")
	if err != nil {
		t.Fatalf("签发测试令牌失败：%v", err)
	}
	return tok
}

func doQuick(t *testing.T, h *Handler, method, body, token string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, "/api/admin/quick", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req) // 走完整路由：顺带验证注册与中间件
	var got map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("响应不是 JSON：%v / %s", err, rec.Body.String())
		}
	}
	return rec.Code, got
}

// 契约 1：路由在鉴权后面，匿名读写都得 401。
func TestQuickAdminRoutesBehindAuth(t *testing.T) {
	h := newQuickHandlerForTest(t)
	if code, _ := doQuick(t, h, http.MethodGet, "", ""); code != http.StatusUnauthorized {
		t.Fatalf("匿名 GET /api/admin/quick 期望 401，实际 %d（路由没挂鉴权或没注册）", code)
	}
	if code, _ := doQuick(t, h, http.MethodPut, `{"prompt":"x"}`, ""); code != http.StatusUnauthorized {
		t.Fatalf("匿名 PUT /api/admin/quick 期望 401，实际 %d", code)
	}
}

// 契约 2 + 空库默认态：带令牌 GET 下发 default_prompt，is_custom=false，
// prompt 回落内置默认（表单回填用的就是这份，不能空）。
func TestQuickAdminGetShipsDefault(t *testing.T) {
	h := newQuickHandlerForTest(t)
	code, got := doQuick(t, h, http.MethodGet, "", quickToken(t, h))
	if code != http.StatusOK {
		t.Fatalf("带令牌 GET 期望 200，实际 %d", code)
	}
	if got["default_prompt"] != store.DefaultQuickPrompt {
		t.Fatalf("default_prompt 必须下发且等于内置默认，实际字段表：%v", got)
	}
	if got["prompt"] != store.DefaultQuickPrompt {
		t.Fatalf("空库 prompt 应回落默认，实际 %q", got["prompt"])
	}
	if v, _ := got["is_custom"].(bool); v {
		t.Fatalf("空库 is_custom 应为 false，实际字段表：%v", got)
	}
}

// 契约 3 + 回环：保存自定义 → 回读生效且 is_custom=true；空串保存 → 恢复默认。
func TestQuickAdminSaveAndResetRoundtrip(t *testing.T) {
	h := newQuickHandlerForTest(t)
	tok := quickToken(t, h)

	code, got := doQuick(t, h, http.MethodPut, `{"prompt":"政务风格，三段以内"}`, tok)
	if code != http.StatusOK {
		t.Fatalf("保存自定义提示词期望 200，实际 %d: %v", code, got)
	}
	if got["prompt"] != "政务风格，三段以内" {
		t.Fatalf("保存响应应回显生效值，实际 %q", got["prompt"])
	}
	if v, _ := got["is_custom"].(bool); !v {
		t.Fatalf("保存自定义后 is_custom 应为 true，实际字段表：%v", got)
	}

	_, got = doQuick(t, h, http.MethodGet, "", tok)
	if got["prompt"] != "政务风格，三段以内" {
		t.Fatalf("保存后回读不一致（写没进库或读错键），实际 %q", got["prompt"])
	}

	_, got = doQuick(t, h, http.MethodPut, `{"prompt":"  "}`, tok) // 纯空白也按空串
	if got["prompt"] != store.DefaultQuickPrompt {
		t.Fatalf("空串保存应恢复默认，实际 %q", got["prompt"])
	}
	if v, _ := got["is_custom"].(bool); v {
		t.Fatalf("恢复默认后 is_custom 应为 false（界面从此谎报「已自定义」），实际字段表：%v", got)
	}
}

// 上限与脏输入：超 8000 字拒绝；缺 prompt 字段拒绝；控制字符拒绝。
// 这些如果静默吞掉，坏数据会一路存进 settings 表，每轮生成都带着它。
func TestQuickAdminRejectsBadInput(t *testing.T) {
	h := newQuickHandlerForTest(t)
	tok := quickToken(t, h)

	if code, _ := doQuick(t, h, http.MethodPut, `{}`, tok); code != http.StatusBadRequest {
		t.Fatalf("缺 prompt 字段期望 400，实际 %d", code)
	}
	if code, _ := doQuick(t, h, http.MethodPut, `not-json`, tok); code != http.StatusBadRequest {
		t.Fatalf("非法 JSON 期望 400，实际 %d", code)
	}
	over := `{"prompt":"` + strings.Repeat("长", 8001) + `"}`
	if code, _ := doQuick(t, h, http.MethodPut, over, tok); code != http.StatusBadRequest {
		t.Fatalf("超长提示词期望 400，实际 %d", code)
	}
	if code, _ := doQuick(t, h, http.MethodPut, `{"prompt":"带\x00控制字符"}`, tok); code != http.StatusBadRequest {
		t.Fatalf("控制字符期望 400，实际 %d", code)
	}
}
