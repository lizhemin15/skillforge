package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/lizhemin15/skillforge/internal/agent"
	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/ocrsvc"
	"github.com/lizhemin15/skillforge/internal/skillgen"
	"github.com/lizhemin15/skillforge/internal/store"
	"github.com/lizhemin15/skillforge/internal/tools"
	"github.com/lizhemin15/skillforge/internal/version"
	"github.com/lizhemin15/skillforge/web"
)

// Handler wires up all routes and static assets.
type Handler struct {
	Skills *Skills
	Admin  *Admin
	Auth   *Auth
	Acc    *Account
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
	// 文档解析服务（ocrd）：两条通道共用同一地址——管理端「给技能上传文件抽取文本」
	// 与训练时「上传扫描件识别写作手册」。此前 ocrURL 从未被赋值，两条通道都静默失效。
	ocrURL := ocrServiceURL()
	ocrTmo := ocrTimeout()
	admin.SetOCR(ocrURL)
	admin.SetOCRTimeout(ocrTmo)
	gen.SetOCR(ocrURL)
	gen.SetOCRTimeout(ocrTmo)
	genCache := newGenCache()
	// 工具能力：按环境变量装配（默认开，SKILLFORGE_TOOLS=off 可回退纯对话）
	toolReg := buildToolRegistry(s)

	// MCP：后台配置的外部工具来源，挂进同一个注册表，工具循环原样复用。
	// 工具关闭时不给管理器注册表（nil），此时后台仍可配置和测试连接——
	// 「能配」和「能调」是两件事，配置界面不该被工具开关连坐。
	var mcpMgr *tools.MCPManager
	if toolReg != nil {
		mcpMgr = tools.NewMCPManager(s, toolReg)
	}
	admin.SetMCP(mcpMgr)
	if mcpMgr != nil {
		// 异步首次连接：内网 MCP 服务慢或没起来时，不能拖着启动流程一起等。
		// 连上后工具自动出现在注册表里，对话当场可用。
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			for _, st := range mcpMgr.Refresh(ctx) {
				if st.Enabled && !st.OK {
					fmt.Fprintf(os.Stderr, "[mcp] %s(%s) 连接失败：%s\n", st.Name, st.URL, st.Error)
				} else if st.OK {
					fmt.Fprintf(os.Stderr, "[mcp] %s 已连接 %s v%s，工具 %d 个\n",
						st.Name, st.Server, st.Version, st.ToolCount)
				}
			}
		}()
	}

	return &Handler{
		Skills: skills, Admin: admin, Auth: auth, Acc: NewAccount(s, auth), Site: NewSite(s),
		Chat: &chatHandler{eng: eng, gen: genCache, tools: toolReg, mcp: mcpMgr, maxRound: toolMaxRounds()}, Eng: eng,
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

// ocrServiceURL 返回文档解析微服务（ocrd）的地址。
// 约定优于配置：不设置环境变量就走本机默认端口，运维不必额外配；
// 显式设为 off/-/none 表示禁用（返回空串），此时二进制素材降级为告警而不是报错。
//
// 实现只有一份（ocrsvc.URLFromEnv）：-selftest 的自检项读同一份。两处各写一套 switch
// 迟早会出现「主服务认为启用了、自检认为禁用了」这种互相打脸的状态，
// 而那种 bug 只会在客户机器上才现形 —— 本地怎么自测都是绿的。
func ocrServiceURL() string { return ocrsvc.URLFromEnv() }

// ocrTimeout 返回单次文档解析的客户端超时上限（默认 30 分钟，见 DefaultOCRTimeout）。
// 接受 "20m" / "90s" 这类时长写法，也接受纯秒数（"600" = 600 秒）；
// 非法值只会打一行警告并回退默认，不会让服务起不来——解析慢是性能问题，
// 不该升级成启动失败。
func ocrTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("SKILLFORGE_OCR_TIMEOUT"))
	if v == "" {
		return skillgen.DefaultOCRTimeout
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	fmt.Fprintf(os.Stderr, "[ocr] SKILLFORGE_OCR_TIMEOUT=%q 不合法（如 30m / 600），回退默认 %s\n",
		v, skillgen.DefaultOCRTimeout)
	return skillgen.DefaultOCRTimeout
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
			// 生效中的文档解析超时：运维改完 SKILLFORGE_OCR_TIMEOUT 重启后，
			// 不看日志也能一眼确认配置真的吃进去了（解析慢是最容易怀疑配置没生效的场景）。
			"ocr_timeout": ocrTimeout().String(),
		})
	})
	mux.HandleFunc("/admin", serveStatic("admin.html"))

	// ---- public API ----
	// 站点设置：GET 必须公开（未登录也要看到正确站名），写走鉴权。
	mux.HandleFunc("GET /api/site", h.Site.Get)
	mux.HandleFunc("GET /api/admin/site", h.Auth.Middleware(h.Site.GetAdmin))
	mux.HandleFunc("PUT /api/admin/site", h.Auth.Middleware(h.Site.Update))
	// 管理员账号：页面里改用户名/密码。改密要验当前密码（见 account.go 注释）。
	mux.HandleFunc("GET /api/admin/account", h.Auth.Middleware(h.Acc.Get))
	mux.HandleFunc("PUT /api/admin/account", h.Auth.Middleware(h.Acc.Update))
	mux.HandleFunc("GET /api/skills", h.Skills.List)
	mux.HandleFunc("GET /api/skills/{slug}", h.Skills.Get)
	mux.HandleFunc("POST /api/generate", h.Skills.Generate)
	mux.HandleFunc("POST /api/chat", h.Chat.ServeHTTP)
	mux.HandleFunc("GET /api/chat/attachment/{slug}", h.Chat.Attachment)
	mux.HandleFunc("GET /api/chat/gen/{token}", h.GenFile(h.gen))
	// 推荐行（输入框上方的小胶囊）的模型侧：见 suggest.go 的包注释。
	// 无鉴权（和 /api/chat 同级，面向首页访客），代价靠超时 + 并发闸门控制。
	// GET 也放行：SSR/预取/无 body 的探测都走它，语义同 POST（只读、无副作用）。
	// 只建**一个** handler：并发闸门在 handlers 内部，两个实例等于把闸门放宽一倍。
	//
	// 超时 9s 是算出来的，不是拍的。FastJSON 内部最多打 3 次请求：
	//   ① 带两族开关  →（400 就摘 enable_thinking）→ ② 换 knob 重试
	//   ③ 空 content 就 ×4 预算重试（封顶 4096）
	// 现线上（astron/astron-code-latest + reasoning_effort=none）单次实测 ~1s，
	// 最坏链路 ≈ 6s，9s 留了余量。**别再往回收** —— 卡在 5s 时放大预算那一次
	// 会被 ctx 掐断，等于白加兜底，症状又是「推荐行老是那几句」这种无声降级。
	//
	// 前端 web/js/chat.js 的 abort 必须比这里长（现 10.5s）：客户端先掐的话，
	// 服务端就算成功也没人接，退化成和网络错误一样的表现。
	suggest := newSuggestHandler(h.Eng, 9*time.Second)
	mux.Handle("POST /api/chat/suggest", suggest)
	mux.Handle("GET /api/chat/suggest", suggest)

	// ---- admin API ----
	mux.HandleFunc("POST /api/login", h.Auth.Login)
	mux.HandleFunc("POST /api/admin/llms/models", h.Auth.Middleware(h.Admin.ListProviderModels))
	mux.HandleFunc("GET /api/admin/llms", h.Auth.Middleware(h.Admin.ListLLM))
	mux.HandleFunc("POST /api/admin/llms", h.Auth.Middleware(h.Admin.UpsertLLM))
	mux.HandleFunc("POST /api/admin/llms/active", h.Auth.Middleware(h.Admin.SetActiveLLM))
	mux.HandleFunc("DELETE /api/admin/llms/{id}", h.Auth.Middleware(h.Admin.DeleteLLM))

	// ---- MCP 连接（管理员后台统一配置 + 开关）----
	// 测试连接是 POST 而非 GET：body 里要塞 API Key，放进 URL 会进日志和浏览器历史。
	mux.HandleFunc("GET /api/admin/mcp", h.Auth.Middleware(h.Admin.ListMCP))
	mux.HandleFunc("POST /api/admin/mcp", h.Auth.Middleware(h.Admin.UpsertMCP))
	mux.HandleFunc("POST /api/admin/mcp/toggle", h.Auth.Middleware(h.Admin.ToggleMCP))
	mux.HandleFunc("POST /api/admin/mcp/test", h.Auth.Middleware(h.Admin.TestMCPConn))
	mux.HandleFunc("POST /api/admin/mcp/refresh", h.Auth.Middleware(h.Admin.RefreshMCP))
	mux.HandleFunc("DELETE /api/admin/mcp/{id}", h.Auth.Middleware(h.Admin.DeleteMCP))
	mux.HandleFunc("POST /api/admin/train", h.Auth.Middleware(h.Admin.Train))
	mux.HandleFunc("POST /api/admin/skills/toggle", h.Auth.Middleware(h.Admin.ToggleSkill))
	mux.HandleFunc("POST /api/admin/skills/core", h.Auth.Middleware(h.Admin.SetSkillCore))
	mux.HandleFunc("DELETE /api/admin/skills/{slug}", h.Auth.Middleware(h.Admin.DeleteSkill))

	// ---- skill list (admin, 全量含停用) ----
	// 管理端「技能管理」必须用这条：公开 List 会过滤掉停用技能，拿它渲染管理
	// 列表会让停用变成不可逆操作（见 skills.go 的 ListAll 注释 / Bug N）。
	mux.HandleFunc("GET /api/admin/skills", h.Auth.Middleware(h.Skills.ListAll))

	// ---- knowledge-base style skill file management ----
	mux.HandleFunc("GET /api/admin/skills/{slug}/files", h.Auth.Middleware(h.Admin.ListSkillFiles))
	mux.HandleFunc("GET /api/admin/skills/{slug}/file", h.Auth.Middleware(h.Admin.ReadSkillFile))
	mux.HandleFunc("GET /api/admin/skills/{slug}/raw", h.Auth.Middleware(h.Admin.RawSkillFile))
	mux.HandleFunc("PUT /api/admin/skills/{slug}/file", h.Auth.Middleware(h.Admin.WriteSkillFile))
	mux.HandleFunc("POST /api/admin/skills/{slug}/file", h.Auth.Middleware(h.Admin.UploadSkillFile))
	mux.HandleFunc("POST /api/admin/skills/{slug}/example", h.Auth.Middleware(h.Admin.AddSkillExample))
	// 分类结构增删改（只对手册模式技能生效）。改名/删除会级联改多个文件，
	// 所以走独立接口而不是复用 WriteSkillFile——手写级联必然漏。
	mux.HandleFunc("POST /api/admin/skills/{slug}/categories", h.Auth.Middleware(h.Admin.CreateSkillCategory))
	mux.HandleFunc("POST /api/admin/skills/{slug}/categories/rename", h.Auth.Middleware(h.Admin.RenameSkillCategory))
	mux.HandleFunc("DELETE /api/admin/skills/{slug}/categories", h.Auth.Middleware(h.Admin.DeleteSkillCategory))
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
