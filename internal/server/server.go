package server

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/lizhemin15/skillforge/internal/api"
	"github.com/lizhemin15/skillforge/internal/config"
	"github.com/lizhemin15/skillforge/internal/db"
	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/model"
	"github.com/lizhemin15/skillforge/internal/store"
)

// buildHandler 装配路由。单独抽出来是为了让测试能断言「启动时激活模型真的进了引擎」——
// 线上事故：这里曾经硬写 nil，重启后第一条模型请求就空指针 panic（见上方注释）。
func buildHandler(st *store.SkillStore, cfg *config.Config) (*api.Handler, error) {
	return api.NewHandler(st, newLLMClient(st), cfg.JWTSecret)
}

// newLLMClient 用库里"已激活"的那条配置建客户端。
// 没有任何激活配置时返回 nil —— 此时引擎/生成器会给出可读的「未配置 LLM」错误，
// 而不是让请求在 (*Client).Chat 上 panic 掉（llm.ErrNoLLM）。
func newLLMClient(st *store.SkillStore) *llm.Client {
	act, err := st.GetActiveLLM()
	if err != nil || act == nil {
		log.Printf("⚠️ 启动时库里没有激活的模型：聊天/生成会提示「未配置 LLM」，请在管理端选择模型")
		return nil
	}
	log.Printf("已装载激活模型：%s / %s", act.Provider, act.Model)
	return llm.New(act)
}

// Run starts the whole server (called from main).
func Run() error {
	cfg := config.Load()

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	sqlDB, err := db.Open(filepath.Join(cfg.DataDir, "skillforge.db"))
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	st := store.NewSkillStore(sqlDB, cfg.DataDir)

	// ensure admin with env-provided credentials on first run
	adminUser := cfg.AdminUser
	adminPass := cfg.AdminPass
	if adminUser == "" {
		adminUser = "admin"
	}
	if adminPass == "" {
		adminPass = "admin"
	}
	if err := st.EnsureAdmin(adminUser, api.HashPassword(adminPass)); err != nil {
		return err
	}

	// bootstrap a default LLM config from env if none exists yet
	if cfg.EnvLLMBaseURL != "" && cfg.EnvLLMAPIKey != "" {
		if _, err := st.GetActiveLLM(); err != nil {
			prov := cfg.EnvLLMProvider
			if prov == "" {
				prov = "custom"
			}
			mdl := cfg.EnvLLMModel
			if mdl == "" {
				mdl = "default"
			}
			_, _ = st.UpsertLLMConfig(&model.LLMConfig{
				Provider: prov,
				BaseURL:  cfg.EnvLLMBaseURL,
				Model:    mdl,
				APIKey:   cfg.EnvLLMAPIKey,
				IsActive: true,
			})
		}
	}

	// ⚠️ 原来这里是 api.NewHandler(st, nil, ...) —— 设计上指望管理端再点一次"启用模型"
	// 走 SetActiveLLM 热插拔。结果：**每次重启后引擎手里是 nil**，第一条走到模型调用的
	// 请求（比如指定「采购合同」这种模板类技能）直接在 llm.(*Client).Chat 上空指针 panic，
	// 连接被掐断，前端只显示"连接失败：network error"，看起来像网络问题。
	// 而 env 引导（上面那段）只在"库里一个都没有"时才写默认配置 → 库里早已有激活配置的
	// 部署，重启后必然踩中，且不点管理端就永远修不好。
	h, err := buildHandler(st, cfg)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           h.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("skillforge listening on %s (data: %s)", cfg.Addr, cfg.DataDir)
	return srv.ListenAndServe()
}
