package config

import (
	"os"
	"strconv"
)

// Config holds runtime configuration loaded from env with sane defaults.
type Config struct {
	// Addr is the HTTP listen address, e.g. ":8092".
	Addr string
	// DataDir is where skills/ content lives and where skillforge.db is stored.
	DataDir string
	// DBPath is the SQLite database file (default <DataDir>/skillforge.db).
	DBPath string
	// JWTSecret signs admin auth tokens.
	JWTSecret string
	// PublicBaseURL is used to build absolute links if needed.
	PublicBaseURL string
	// AdminUser / AdminPass bootstrap the first admin account.
	AdminUser string
	AdminPass string
	// EnvLLM* provide a default LLM config if none is stored in DB yet.
	EnvLLMProvider string
	EnvLLMBaseURL  string
	EnvLLMAPIKey   string
	EnvLLMModel    string
}

// Load reads configuration from environment variables.
func Load() *Config {
	return &Config{
		Addr:           getenv("SKILLFORGE_ADDR", ":8092"),
		DataDir:        getenv("SKILLFORGE_DATA_DIR", "/root/skillforge"),
		DBPath:         getenv("SKILLFORGE_DB", ""),
		JWTSecret:      getenv("SKILLFORGE_JWT_SECRET", "change-me-skillforge-secret"),
		PublicBaseURL:  getenv("SKILLFORGE_PUBLIC_URL", "http://localhost:8092"),
		AdminUser:      getenv("SKILLFORGE_ADMIN_USER", "admin"),
		AdminPass:      getenv("SKILLFORGE_ADMIN_PASS", "skillforge123"),
		EnvLLMProvider: getenv("SKILLFORGE_LLM_PROVIDER", ""),
		EnvLLMBaseURL:  getenv("SKILLFORGE_LLM_BASE_URL", ""),
		EnvLLMAPIKey:   getenv("SKILLFORGE_LLM_API_KEY", ""),
		EnvLLMModel:    getenv("SKILLFORGE_LLM_MODEL", ""),
	}
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// Int returns an int env with default.
func Int(k string, d int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return d
}
