package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/lizhemin15/skillforge/internal/store"
)

// Auth handles admin JWT issuance/middleware.
type Auth struct {
	secret []byte
	store  *store.SkillStore
}

func NewAuth(secret string, s *store.SkillStore) *Auth {
	return &Auth{secret: []byte(secret), store: s}
}

// HashPassword deterministically hashes a password (double-SHA256 with salt
// suffix). Not meant for high-security; adequate for a single-admin tool.
func HashPassword(pw string) string {
	h := sha256.Sum256([]byte(pw + "::skillforge-salt"))
	h2 := sha256.Sum256([]byte(hex.EncodeToString(h[:])))
	return hex.EncodeToString(h2[:])
}

func (a *Auth) issueToken(username string) (string, error) {
	claims := jwt.MapClaims{
		"sub": username,
		"exp": time.Now().Add(72 * time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(a.secret)
}

// Login validates admin credentials and returns a token.
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	var c struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readBody(r, &c); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体无效")
		return
	}
	hash, err := a.store.GetAdminHash(c.Username)
	if err != nil {
		// generic error to avoid leaking whether user exists
		writeErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	if hash != HashPassword(c.Password) {
		writeErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	tok, err := a.issueToken(c.Username)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "签发令牌失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": tok, "username": c.Username})
}

// ctxKeyUser 是 Middleware 往 context 里塞当前用户名用的键。
// 不导出：只有同包拿得到，避免外部包往 context 里塞一个假的用户名。
type ctxKeyUser struct{}

// currentUsername 取出 Middleware 校验过的用户名。
// 拿不到就返回 false —— 调用方一律按「未登录」处理，别猜默认值。
func (a *Auth) currentUsername(r *http.Request) (string, bool) {
	u, ok := r.Context().Value(ctxKeyUser{}).(string)
	return u, ok && u != ""
}

// Middleware requires a valid Bearer token.
func (a *Auth) Middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			writeErr(w, http.StatusUnauthorized, "未登录")
			return
		}
		tokStr := strings.TrimPrefix(auth, "Bearer ")
		tok, err := jwt.Parse(tokStr, func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method")
			}
			return a.secret, nil
		})
		if err != nil || !tok.Valid {
			writeErr(w, http.StatusUnauthorized, "登录已失效，请重新登录")
			return
		}
		sub, _ := tok.Claims.GetSubject()
		// token 里的 sub 必须对得上库里还存在的账号。
		// 不然改名/删号之后，手里那枚 72 小时长效 token 还能继续用 ——
		// 改密码这个动作就等于没有回收旧凭据。
		if _, err := a.store.GetAdminHash(sub); err != nil {
			writeErr(w, http.StatusUnauthorized, "账号已变更，请重新登录")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKeyUser{}, sub)))
	}
}
