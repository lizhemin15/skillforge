package api

import (
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
		next(w, r)
	}
}
