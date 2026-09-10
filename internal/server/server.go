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
	"github.com/lizhemin15/skillforge/internal/model"
	"github.com/lizhemin15/skillforge/internal/store"
)

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

	h, err := api.NewHandler(st, nil, cfg.JWTSecret)
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
