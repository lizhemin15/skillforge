package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/db"
	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/model"
	"github.com/lizhemin15/skillforge/internal/store"
)

// 管理员账号（页面里改用户名/密码）的回归防线。
//
// 这组测试守的是几条**线上会静默坏**的契约，不是「函数能跑」：
//  1. 改完密码，旧密码必须失效、新密码必须能登 —— 反了就是「改了个寂寞」。
//  2. 改名之后，旧名字签出的令牌必须立刻作废；否则改密动作不回收任何凭据。
//  3. 改密码必须验当前密码 —— 不验等于「拿到浏览器=拿到机器」。
//  4. **重启不能被 env 覆盖回去**：这是最阴的一条，管理端改完看着成功，
//     服务一重启旧密码复活，用户只会以为自己记错了密码。
//
// 密码字面量一律拼出来（同 llm_upsert_test.go 的 realKey 做法）：直接写字面量会被
// 脱敏改写动到，而这里要拿它逐字比对，必须保证写进库的和断言里的是同一个串。

var (
	testPwOld = "pw" + "-old" + "-000111"
	testPwNew = "pw" + "-new" + "-222333"
)

func newAccountHandlerForTest(t *testing.T) (*Handler, *store.SkillStore) {
	t.Helper()
	s := newTestStore(t)
	if err := s.BootstrapAdmin("admin", HashPassword(testPwOld)); err != nil {
		t.Fatalf("种管理员失败：%v", err)
	}
	h, err := NewHandler(s, llm.New(&model.LLMConfig{}), "test-secret")
	if err != nil {
		t.Fatalf("装配 Handler 失败：%v", err)
	}
	return h, s
}

// doJSON 走完整路由（含 Auth.Middleware），返回状态码 + 解析后的 JSON。
// 必须走真路由：鉴权、context 传用户名、路由前缀都在这一层，绕过去测不出问题。
func doJSON(t *testing.T, h *Handler, method, path, body, bearer string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	return rec.Code, got
}

// login 走真登录接口拿令牌，返回 (状态码, token)。
func login(t *testing.T, h *Handler, user, pw string) (int, string) {
	t.Helper()
	code, got := doJSON(t, h, http.MethodPost, "/api/login",
		`{"username":"`+user+`","password":"`+pw+`"}`, "")
	tok, _ := got["token"].(string)
	return code, tok
}

// putAccount 是改账号的常用形态。
func putAccount(t *testing.T, h *Handler, tok, body string) (int, map[string]any) {
	t.Helper()
	return doJSON(t, h, http.MethodPut, "/api/admin/account", body, tok)
}

func pwBody(cur, newUser, newPw string) string {
	return `{"current_password":"` + cur + `","new_username":"` + newUser +
		`","new_password":"` + newPw + `","confirm_password":"` + newPw + `"}`
}

// TestAccountEndpointsNeedAuth 守护：这两个接口绝不能匿名可达。
func TestAccountEndpointsNeedAuth(t *testing.T) {
	h, s := newAccountHandlerForTest(t)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/admin/account", ""},
		{http.MethodPut, "/api/admin/account", pwBody(testPwOld, "hacker", testPwNew)},
	} {
		code, _ := doJSON(t, h, tc.method, tc.path, tc.body, "")
		if code == http.StatusOK {
			t.Fatalf("匿名 %s %s 返回 200（管理接口漏鉴权）", tc.method, tc.path)
		}
	}
	// 被拒之后库里不能有变化：旧密码照样能登。
	if code, _ := login(t, h, "admin", testPwOld); code != http.StatusOK {
		t.Fatalf("匿名改密被拒后旧密码应能登录，实际 %d", code)
	}
	if n, err := s.CountAdmins(); err != nil || n != 1 {
		t.Fatalf("管理员条数不该变：n=%d err=%v", n, err)
	}
}

// TestAccountGetReturnsLoggedInUser 页面预填用：GET 必须给出当前登录名，
// 而不是让前端硬编码 "admin"。
func TestAccountGetReturnsLoggedInUser(t *testing.T) {
	h, _ := newAccountHandlerForTest(t)
	code, tok := login(t, h, "admin", testPwOld)
	if code != http.StatusOK {
		t.Fatalf("前置登录失败 code=%d", code)
	}
	code, got := doJSON(t, h, http.MethodGet, "/api/admin/account", "", tok)
	if code != http.StatusOK {
		t.Fatalf("GET account 期望 200，实际 %d / %v", code, got)
	}
	if got["username"] != "admin" {
		t.Fatalf("期望回显当前用户名 admin，实际 %v", got["username"])
	}
}

// TestAccountChangePasswordTakesEffect 主路径：改密后新密码能登、旧密码不能登。
func TestAccountChangePasswordTakesEffect(t *testing.T) {
	h, _ := newAccountHandlerForTest(t)
	_, tok := login(t, h, "admin", testPwOld)

	code, got := putAccount(t, h, tok, pwBody(testPwOld, "admin", testPwNew))
	if code != http.StatusOK {
		t.Fatalf("改密失败 code=%d / %v", code, got)
	}
	if got["password_changed"] != true {
		t.Fatalf("响应应标记 password_changed=true，实际 %v", got)
	}
	if newTok, _ := got["token"].(string); newTok == "" {
		t.Fatalf("改密后必须换发新令牌（否则用户下一步就被登出），响应：%v", got)
	}

	if code, _ := login(t, h, "admin", testPwNew); code != http.StatusOK {
		t.Fatalf("新密码应能登录，实际 %d", code)
	}
	if code, _ := login(t, h, "admin", testPwOld); code != http.StatusUnauthorized {
		t.Fatalf("旧密码必须失效，实际 %d（改密没生效）", code)
	}
}

// TestAccountRenameRevokesOldTokens 改名后旧令牌必须失效，新令牌必须可用。
//
// 为什么较真：令牌是 72 小时长效。若改名（或改密）后旧令牌照旧能用，
// 那这个功能就只是改了张显示名 —— 手里有一枚旧令牌的人照样进来。
func TestAccountRenameRevokesOldTokens(t *testing.T) {
	h, _ := newAccountHandlerForTest(t)
	_, oldTok := login(t, h, "admin", testPwOld)

	code, got := putAccount(t, h, oldTok, pwBody(testPwOld, "boss", testPwNew))
	if code != http.StatusOK {
		t.Fatalf("改名失败 code=%d / %v", code, got)
	}
	newTok, _ := got["token"].(string)
	if newTok == "" {
		t.Fatalf("改名后必须换发新令牌，响应：%v", got)
	}

	if code, _ := doJSON(t, h, http.MethodGet, "/api/admin/account", "", newTok); code != http.StatusOK {
		t.Fatalf("新令牌应可用，实际 %d", code)
	}
	if code, _ := doJSON(t, h, http.MethodGet, "/api/admin/account", "", oldTok); code != http.StatusUnauthorized {
		t.Fatalf("旧用户名的令牌必须作废，实际 %d（旧凭据没被回收）", code)
	}
	if code, _ := login(t, h, "boss", testPwNew); code != http.StatusOK {
		t.Fatalf("新用户名+新密码应能登录，实际 %d", code)
	}
	if code, _ := login(t, h, "admin", testPwNew); code != http.StatusUnauthorized {
		t.Fatalf("旧用户名必须不能登录，实际 %d", code)
	}
	if code, _ := login(t, h, "admin", testPwOld); code != http.StatusUnauthorized {
		t.Fatalf("旧用户名+旧密码必须不能登录，实际 %d", code)
	}
}

// TestAccountRenameRejectsDuplicateName 重名必须被拒，且不能把原账号改坏。
//
// 这里不走 BootstrapAdmin 造第二个账号：它只在**空库**时种号（刻意的语义），
// 库里有 admin 时是空操作 —— 拿它当"插入第二个账号"用会静默无效，测试就变成
// 假绿（改名撞上一个根本不存在的名字，当然 200）。所以直接用同一个 *sql.DB
// 句柄插一行，前提才是真的。
func TestAccountRenameRejectsDuplicateName(t *testing.T) {
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败：%v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	s := store.NewSkillStore(d, dir)
	if err := s.BootstrapAdmin("admin", HashPassword(testPwOld)); err != nil {
		t.Fatalf("种管理员失败：%v", err)
	}
	if _, err := d.Exec(`INSERT INTO admins(username,pass_hash) VALUES(?,?)`, "taken", HashPassword(testPwOld)); err != nil {
		t.Fatalf("插第二个账号失败：%v", err)
	}
	h, err := NewHandler(s, llm.New(&model.LLMConfig{}), "test-secret")
	if err != nil {
		t.Fatalf("装配 Handler 失败：%v", err)
	}
	_, tok := login(t, h, "admin", testPwOld)

	code, got := putAccount(t, h, tok, pwBody(testPwOld, "taken", testPwNew))
	if code != http.StatusBadRequest {
		t.Fatalf("改成已占用的用户名应 400，实际 %d / %v", code, got)
	}
	// 事务不能留半截：原名 + 原密码必须都还在（密码没被偷偷改掉）。
	if c, _ := login(t, h, "admin", testPwOld); c != http.StatusOK {
		t.Fatalf("重名失败后原名+原密码必须仍可用，实际 %d", c)
	}
	if c, _ := login(t, h, "admin", testPwNew); c == http.StatusOK {
		t.Fatalf("重名失败后密码不该被改掉")
	}
}

// TestBootstrapAdminIsNoopWhenAccountExists 记录 env 的定位：
// 「第一次开机的初始密码」，不是「持续生效的配置」。
// 库里有账号时，再拿 env 种一次必须是空操作（不新增账号、不动密码）。
func TestBootstrapAdminIsNoopWhenAccountExists(t *testing.T) {
	s := newTestStore(t)
	if err := s.BootstrapAdmin("admin", HashPassword(testPwOld)); err != nil {
		t.Fatalf("首次种账号失败：%v", err)
	}
	if err := s.BootstrapAdmin("another", HashPassword(testPwNew)); err != nil {
		t.Fatalf("非空库 BootstrapAdmin 应静默返回 nil，实际 %v", err)
	}
	if n, _ := s.CountAdmins(); n != 1 {
		t.Fatalf("库非空时不该新增账号，实际 %d 个", n)
	}
	if _, err := s.GetAdminHash("another"); err == nil {
		t.Fatalf("env 指定的第二个用户名不该被建出来")
	}
}

// TestAccountRequiresCurrentPassword 守护：当前密码错/空一律拒绝，且不改库里任何东西。
func TestAccountRequiresCurrentPassword(t *testing.T) {
	h, _ := newAccountHandlerForTest(t)
	_, tok := login(t, h, "admin", testPwOld)

	for _, tc := range []struct{ name, body string }{
		{"当前密码错", pwBody("wrong"+"-pw-999", "admin", testPwNew)},
		{"当前密码空", pwBody("", "admin", testPwNew)},
		{"当前密码空但改名", pwBody("", "boss", "")},
	} {
		if code, got := putAccount(t, h, tok, tc.body); code == http.StatusOK {
			t.Fatalf("%s：不该成功（改凭据必须验当前密码），响应 %v", tc.name, got)
		}
	}
	// 三次都被拒 → 库里的密码必须还是原来那个。
	if code, _ := login(t, h, "admin", testPwOld); code != http.StatusOK {
		t.Fatalf("被拒的请求不该改动密码，旧密码登录实际 %d", code)
	}
	if code, _ := login(t, h, "boss", testPwNew); code == http.StatusOK {
		t.Fatalf("改名请求被拒后不该产生 boss 账号")
	}
}

// TestAccountValidation 边界：弱密码、两次不一致、没改动、非法用户名、重名。
func TestAccountValidation(t *testing.T) {
	h, _ := newAccountHandlerForTest(t)
	_, tok := login(t, h, "admin", testPwOld)

	bad := []struct{ name, body string }{
		{"新密码太短", pwBody(testPwOld, "admin", "short")},
		{"两次不一致", `{"current_password":"` + testPwOld + `","new_username":"admin","new_password":"` + testPwNew + `","confirm_password":"other-` + testPwNew + `"}`},
		{"什么都没改", `{"current_password":"` + testPwOld + `","new_username":"admin","new_password":""}`},
		{"用户名带空格", pwBody(testPwOld, "a b", "")},
		{"用户名太短", pwBody(testPwOld, "a", "")},
		{"用户名带斜杠", pwBody(testPwOld, "a/b", "")},
		{"新密码和当前一样", pwBody(testPwOld, "admin", testPwOld)},
		{"非法 JSON", `{"current_password":`},
	}
	for _, tc := range bad {
		if code, got := putAccount(t, h, tok, tc.body); code != http.StatusBadRequest {
			t.Fatalf("%s：期望 400，实际 %d / %v", tc.name, code, got)
		}
	}

	// 卡边界的合法输入必须放行：32 位用户名。
	long := strings.Repeat("u", 32)
	if code, got := putAccount(t, h, tok, pwBody(testPwOld, long, "")); code != http.StatusOK {
		t.Fatalf("32 位用户名应放行，实际 %d / %v", code, got)
	}
}

// TestBootstrapAdminDoesNotOverwritePageChangedPassword 是本轮修的核心 bug：
// env 只在**首次开机**种账号，之后不许覆盖。
//
// 旧行为：每次启动 SetAdminPassword(env 密码) → 管理端改的密码活不过重启。
func TestBootstrapAdminDoesNotOverwritePageChangedPassword(t *testing.T) {
	s := newTestStore(t)

	// 首次开机：env 密码生效。
	if err := s.BootstrapAdmin("admin", HashPassword(testPwOld)); err != nil {
		t.Fatalf("首次种账号失败：%v", err)
	}
	// 用户在页面上改了密码。
	if err := s.SetAdminPassword("admin", HashPassword(testPwNew)); err != nil {
		t.Fatalf("改密失败：%v", err)
	}
	// 服务重启：同样的 env 再种一次。
	if err := s.BootstrapAdmin("admin", HashPassword(testPwOld)); err != nil {
		t.Fatalf("重启时 BootstrapAdmin 失败：%v", err)
	}

	got, err := s.GetAdminHash("admin")
	if err != nil {
		t.Fatalf("读密码哈希失败：%v", err)
	}
	if got != HashPassword(testPwNew) {
		t.Fatalf("重启后密码被打回 env 值了 —— 用户页面改的密码活不过重启（这是本轮要修的死因）")
	}
	if got == HashPassword(testPwOld) {
		t.Fatalf("重启后密码变回 env 里的初始密码")
	}
}

// TestBootstrapAdminSeedsEmptyStore 守住「首次开机仍要能建号」这一半：
// 修成「永不覆盖」时最容易顺手把「空库也不建」写进去，那新部署直接登不进去。
func TestBootstrapAdminSeedsEmptyStore(t *testing.T) {
	s := newTestStore(t)
	if n, err := s.CountAdmins(); err != nil || n != 0 {
		t.Fatalf("新库应为空，实际 n=%d err=%v", n, err)
	}
	if err := s.BootstrapAdmin("admin", HashPassword(testPwOld)); err != nil {
		t.Fatalf("空库种账号失败：%v", err)
	}
	if n, _ := s.CountAdmins(); n != 1 {
		t.Fatalf("空库种完应有 1 个账号，实际 %d", n)
	}
	if h, err := s.GetAdminHash("admin"); err != nil || h != HashPassword(testPwOld) {
		t.Fatalf("种出来的密码不对：h=%q err=%v", h, err)
	}
}

// TestSetAdminPasswordMissingUserFails 记录一条实现约定：改一个不存在的账号必须报错。
// 静默成功会让「改密码成功、登录还是旧密码」这种幽灵故障流到用户面前。
func TestSetAdminPasswordMissingUserFails(t *testing.T) {
	s := newTestStore(t)
	if err := s.BootstrapAdmin("admin", HashPassword(testPwOld)); err != nil {
		t.Fatalf("种账号失败：%v", err)
	}
	if err := s.SetAdminPassword("ghost", HashPassword(testPwNew)); err == nil {
		t.Fatalf("改不存在的账号应报错，实际 nil（打中 0 行却报成功）")
	}
}
