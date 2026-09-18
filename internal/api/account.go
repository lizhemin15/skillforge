package api

import (
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/lizhemin15/skillforge/internal/store"
)

// Account 管理「管理员账号本身」——改用户名、改密码。
//
// 为什么需要它：以前账号只在环境变量里，改密码得改 env 再重启服务，
// 而这套东西是给非运维用户用的单机工具。页面能改是刚需。
//
// 为什么改密必须验旧密码：这是个只有一个人用的工具，但 token 是 72 小时的
// 长效 token。如果拿着一个偷来的/借来的 token 就能静默改掉管理员密码，
// 那 72 小时的窗口里等于把机器交出去。验旧密码把「顺手改掉」的成本抬回
// 「得知道当前密码」。
type Account struct {
	store *store.SkillStore
	auth  *Auth
}

func NewAccount(s *store.SkillStore, a *Auth) *Account {
	return &Account{store: s, auth: a}
}

// 用户名白名单：字母数字 + 下划线/点/横杠/at，2~32 字符。
// 不用任意字符是为了避免「看不见的字符当用户名」这种自己都打不开的账号
// （前后空格、全角空格、零宽字符都能造出来，登录页一 trim 就再也登不上）。
var accountNameRe = regexp.MustCompile(`^[A-Za-z0-9_.@-]{2,32}$`)

const accountMinPwLen = 8

// Get 返回当前登录账号的名字。页面用它预填「用户名」输入框 ——
// 不给前端留一个「先猜再填」的死值。
func (h *Account) Get(w http.ResponseWriter, r *http.Request) {
	user, ok := h.auth.currentUsername(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "登录已失效，请重新登录")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"username": user})
}

// Update 改用户名和/或密码。
func (h *Account) Update(w http.ResponseWriter, r *http.Request) {
	user, ok := h.auth.currentUsername(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "登录已失效，请重新登录")
		return
	}
	var c struct {
		CurrentPassword string `json:"current_password"`
		NewUsername     string `json:"new_username"`
		NewPassword     string `json:"new_password"`
		ConfirmPassword string `json:"confirm_password"`
	}
	if err := readBody(r, &c); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体无效")
		return
	}
	c.NewUsername = strings.TrimSpace(c.NewUsername)
	// 用户名留空 = 不改名（页面预填了当前名，用户只改密码时名字原样回传）。
	if c.NewUsername == "" {
		c.NewUsername = user
	}

	// 1) 验旧密码。改的是凭据本身，属于「敏感操作」，不能只靠 token。
	hash, err := h.store.GetAdminHash(user)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "登录已失效，请重新登录")
		return
	}
	if HashPassword(c.CurrentPassword) != hash {
		writeErr(w, http.StatusBadRequest, "当前密码不正确")
		return
	}

	// 2) 校验新值。
	if c.NewUsername != user && !accountNameRe.MatchString(c.NewUsername) {
		writeErr(w, http.StatusBadRequest, "用户名只能用字母、数字、下划线、点、横杠或 @，长度 2~32 位")
		return
	}
	nameChanged := c.NewUsername != user
	pwChanged := c.NewPassword != ""
	if !nameChanged && !pwChanged {
		writeErr(w, http.StatusBadRequest, "没有需要修改的内容")
		return
	}
	if pwChanged {
		if utf8.RuneCountInString(c.NewPassword) < accountMinPwLen {
			writeErr(w, http.StatusBadRequest, "新密码至少 8 位")
			return
		}
		if c.ConfirmPassword != "" && c.ConfirmPassword != c.NewPassword {
			writeErr(w, http.StatusBadRequest, "两次输入的新密码不一致")
			return
		}
		if c.NewPassword == c.CurrentPassword {
			writeErr(w, http.StatusBadRequest, "新密码和当前密码一样，没有改动")
			return
		}
	}

	// 3) 落库。用户名 + 密码一次事务写完，不留「密码换了名字没换」的半截状态。
	newHash := ""
	if pwChanged {
		newHash = HashPassword(c.NewPassword)
	}
	if err := h.store.RenameAdmin(user, c.NewUsername, newHash); err != nil {
		// 唯一约束冲突单独说人话 —— 泛化成「保存失败」用户不知道该改什么。
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			writeErr(w, http.StatusBadRequest, "用户名 "+c.NewUsername+" 已被占用")
			return
		}
		writeErr(w, http.StatusInternalServerError, "保存失败："+err.Error())
		return
	}

	// 4) 换发新 token。
	// 改名后旧 token 的 sub 指向一个不存在的账号（Middleware 会核验 sub 是否还在），
	// 所以必须立刻发新的，否则用户改完名字下一次点任何按钮就被登出。
	// 改密码同理：自己验证过身份，没必要逼用户重登。
	tok, err := h.auth.issueToken(c.NewUsername)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "账号已保存，但签发新令牌失败，请重新登录")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"token":            tok,
		"username":         c.NewUsername,
		"password_changed": pwChanged,
		"username_changed": nameChanged,
	})
}
