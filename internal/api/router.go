package api

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"runtime"
	"strings"

	"github.com/lizhemin15/skillforge/internal/agent"
	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/skillgen"
	"github.com/lizhemin15/skillforge/internal/store"
	"github.com/lizhemin15/skillforge/internal/version"
	"github.com/lizhemin15/skillforge/web"
)

// Handler wires up all routes and static assets.
type Handler struct {
	Skills *Skills
	Admin  *Admin
	Auth   *Auth
	Site   *Site
	Chat   *chatHandler
	Eng    *agent.Engine // exposed so admin can hot-swap the engine LLM
	gen    *genCache     // one-time generated-file store for docgen delivery
}

func NewHandler(s *store.SkillStore, l *llm.Client, secret string) (*Handler, error) {
	gen := skillgen.NewGenerator(l, s, dataDirFor(s))
	auth := NewAuth(secret, s)
	skills := NewSkills(s)
	admin := NewAdmin(s, gen)
	eng := agent.New(l, s)
	// Keep generator + engine in sync with the live LLM.
	admin.SetEngine(eng)
	genCache := newGenCache()
	// 工具能力：按环境变量装配（默认开，SKILLFORGE_TOOLS=off 可回退纯对话）
	toolReg := buildToolRegistry(s)
	return &Handler{
		Skills: skills, Admin: admin, Auth: auth, Site: NewSite(s),
		Chat: &chatHandler{eng: eng, gen: genCache, tools: toolReg, maxRound: toolMaxRounds()}, Eng: eng,
		gen: genCache,
	}, nil
}

// toolMaxRounds 是工具循环轮数上限，可用环境变量调（默认 6）。
// 上限的意义：公网开放场景下必须有硬性终止条件，否则一次请求能无限烧 token。
func toolMaxRounds() int {
	if v := strings.TrimSpace(os.Getenv("SKILLFORGE_TOOL_ROUNDS")); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 && n <= 20 {
			return n
		}
		fmt.Fprintf(os.Stderr, "[tools] SKILLFORGE_TOOL_ROUNDS=%q 不合法（1-20），回退默认 6\n", v)
	}
	return 6
}

func dataDirFor(s *store.SkillStore) string {
	// derive data dir from skills dir (parent of skills/)
	dir := s.SkillsDir()
	if len(dir) > 7 && dir[len(dir)-7:] == "/skills" {
		return dir[:len(dir)-7]
	}
	if len(dir) > 8 && dir[len(dir)-8:] == "/skills/" {
		return dir[:len(dir)-8]
	}
	return dir
}

func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()

	// ---- static assets ----
	sub, err := fs.Sub(web.FS, ".")
	if err == nil {
		mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(sub))))
	}

	// ---- public pages ----
	mux.HandleFunc("/", serveIndex())

	// ---- build info（运维用：确认线上跑的是哪个构建）----
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"version": version.Version,
			"commit":  version.Commit,
			"date":    version.Date,
			"go":      runtime.Version(),
		})
	})
	mux.HandleFunc("/admin", serveStatic("admin.html"))

	// ---- public API ----
	// 站点设置：GET 必须公开（未登录也要看到正确站名），写走鉴权。
	mux.HandleFunc("GET /api/site", h.Site.Get)
	mux.HandleFunc("GET /api/admin/site", h.Auth.Middleware(h.Site.GetAdmin))
	mux.HandleFunc("PUT /api/admin/site", h.Auth.Middleware(h.Site.Update))
	mux.HandleFunc("GET /api/skills", h.Skills.List)
	mux.HandleFunc("GET /api/skills/{slug}", h.Skills.Get)
	mux.HandleFunc("POST /api/generate", h.Skills.Generate)
	mux.HandleFunc("POST /api/chat", h.Chat.ServeHTTP)
	mux.HandleFunc("GET /api/chat/attachment/{slug}", h.Chat.Attachment)
	mux.HandleFunc("GET /api/chat/gen/{token}", h.GenFile(h.gen))

	// ---- admin API ----
	mux.HandleFunc("POST /api/login", h.Auth.Login)
	mux.HandleFunc("POST /api/admin/llms/models", h.Auth.Middleware(h.Admin.ListProviderModels))
	mux.HandleFunc("GET /api/admin/llms", h.Auth.Middleware(h.Admin.ListLLM))
	mux.HandleFunc("POST /api/admin/llms", h.Auth.Middleware(h.Admin.UpsertLLM))
	mux.HandleFunc("POST /api/admin/llms/active", h.Auth.Middleware(h.Admin.SetActiveLLM))
	mux.HandleFunc("DELETE /api/admin/llms/{id}", h.Auth.Middleware(h.Admin.DeleteLLM))
	mux.HandleFunc("POST /api/admin/train", h.Auth.Middleware(h.Admin.Train))
	mux.HandleFunc("POST /api/admin/skills/toggle", h.Auth.Middleware(h.Admin.ToggleSkill))
	mux.HandleFunc("POST /api/admin/skills/core", h.Auth.Middleware(h.Admin.SetSkillCore))
	mux.HandleFunc("DELETE /api/admin/skills/{slug}", h.Auth.Middleware(h.Admin.DeleteSkill))

	// ---- knowledge-base style skill file management ----
	mux.HandleFunc("GET /api/admin/skills/{slug}/files", h.Auth.Middleware(h.Admin.ListSkillFiles))
	mux.HandleFunc("GET /api/admin/skills/{slug}/file", h.Auth.Middleware(h.Admin.ReadSkillFile))
	mux.HandleFunc("GET /api/admin/skills/{slug}/raw", h.Auth.Middleware(h.Admin.RawSkillFile))
	mux.HandleFunc("PUT /api/admin/skills/{slug}/file", h.Auth.Middleware(h.Admin.WriteSkillFile))
	mux.HandleFunc("POST /api/admin/skills/{slug}/file", h.Auth.Middleware(h.Admin.UploadSkillFile))
	mux.HandleFunc("POST /api/admin/skills/{slug}/example", h.Auth.Middleware(h.Admin.AddSkillExample))
	mux.HandleFunc("DELETE /api/admin/skills/{slug}/file", h.Auth.Middleware(h.Admin.DeleteSkillFile))
	mux.HandleFunc("PATCH /api/admin/skills/{slug}", h.Auth.Middleware(h.Admin.UpdateSkillMeta))
	mux.HandleFunc("POST /api/admin/skills", h.Auth.Middleware(h.Admin.CreateSkill))
	// ---- skill optimize (review) + version rollback ----
	mux.HandleFunc("POST /api/admin/skills/{slug}/review", h.Auth.Middleware(h.Admin.ReviewSkill))
	mux.HandleFunc("GET /api/admin/skills/{slug}/versions", h.Auth.Middleware(h.Admin.ListSkillVersions))
	mux.HandleFunc("POST /api/admin/skills/{slug}/rollback", h.Auth.Middleware(h.Admin.RollbackSkill))

	return mux
}

func serveIndex() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		b, _ := web.FS.ReadFile("index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		_, _ = w.Write(b)
	}
}

func serveStatic(fname string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := web.FS.ReadFile(fname)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		_, _ = w.Write(b)
	}
}
