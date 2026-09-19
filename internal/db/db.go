package db

import (
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// Open opens (and initiates) the SQLite database. Pure Go driver, no CGO.
func Open(path string) (*sql.DB, error) {
	// _pragma=busy_timeout and foreign_keys via DSN attrs
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(DELETE)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite single-writer safety
	if err := migrate(db); err != nil {
		return nil, err
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS skills (
			slug        TEXT PRIMARY KEY,
			name        TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			category    TEXT NOT NULL DEFAULT 'general',
			version     INTEGER NOT NULL DEFAULT 1,
			enabled     INTEGER NOT NULL DEFAULT 1,
			is_core     INTEGER NOT NULL DEFAULT 0,
			created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS params (
			slug       TEXT NOT NULL REFERENCES skills(slug) ON DELETE CASCADE,
			name       TEXT NOT NULL,
			label      TEXT NOT NULL,
			type       TEXT NOT NULL DEFAULT 'text',
			required   INTEGER NOT NULL DEFAULT 0,
			placeholder TEXT NOT NULL DEFAULT '',
			help        TEXT NOT NULL DEFAULT '',
			options     TEXT NOT NULL DEFAULT '',
			default_val TEXT NOT NULL DEFAULT '',
			position   INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (slug, name)
		)`,
		`CREATE TABLE IF NOT EXISTS llm_config (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			provider   TEXT NOT NULL,
			base_url   TEXT NOT NULL,
			api_key    TEXT NOT NULL,
			model      TEXT NOT NULL,
			is_active  INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS articles (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			slug       TEXT NOT NULL REFERENCES skills(slug) ON DELETE CASCADE,
			title      TEXT NOT NULL DEFAULT '',
			content    TEXT NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS admins (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			username   TEXT UNIQUE NOT NULL,
			pass_hash  TEXT NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		// 站点级设置：key/value 通用表（当前只有 site_name / site_tagline）。
		// 用通用 KV 而不是给每项设置加一列 —— 以后加设置项不用再写迁移。
		`CREATE TABLE IF NOT EXISTS settings (
			key        TEXT PRIMARY KEY,
			val        TEXT NOT NULL DEFAULT '',
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		// MCP 服务器：管理员在后台统一配置/开关的外部工具来源。
		// api_key 存明文（与 llm_config 一致，整库本就等于凭据本体）；
		// 但**接口层永不回传明文**，只回掩码，见 internal/api/admin_mcp.go。
		`CREATE TABLE IF NOT EXISTS mcp_servers (
			id          TEXT PRIMARY KEY,
			name        TEXT NOT NULL DEFAULT '',
			url         TEXT NOT NULL DEFAULT '',
			api_key     TEXT NOT NULL DEFAULT '',
			enabled     INTEGER NOT NULL DEFAULT 0,
			timeout_sec INTEGER NOT NULL DEFAULT 30,
			created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	// Column migrations: CREATE TABLE IF NOT EXISTS is a no-op on existing
	// tables, so add new columns to `skills` explicitly. Ignore "duplicate
	// column" errors to keep migration idempotent.
	addCols := []struct{ tbl, ddl string }{
		{"skills", "ALTER TABLE skills ADD COLUMN skill_type TEXT NOT NULL DEFAULT 'write'"},
		{"skills", "ALTER TABLE skills ADD COLUMN attachment TEXT NOT NULL DEFAULT ''"},
	}
	for _, c := range addCols {
		if _, err := db.Exec(c.ddl); err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") &&
				!strings.Contains(err.Error(), "already exists") {
				return fmt.Errorf("migrate: %w", err)
			}
		}
	}
	return nil
}
