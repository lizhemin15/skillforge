package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/lizhemin15/skillforge/internal/mcp"
)

// MCP 服务器配置的持久化。
//
// 落库的是「连什么、开不开」这两件事；「连上之后有哪些工具」不落库——
// 那是运行时的真相，缓存下来只会在服务器升级后变成骗人的旧数据。

// ListMCPServers 返回全部配置（含禁用），供 MCPManager 做全量刷新。
// 实现 tools.MCPSource 接口。
func (s *SkillStore) ListMCPServers() ([]mcp.Config, error) {
	rows, err := s.db.Query(
		`SELECT id, name, url, COALESCE(api_key,''), enabled, COALESCE(timeout_sec,30)
		 FROM mcp_servers ORDER BY name ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list mcp servers: %w", err)
	}
	defer rows.Close()

	out := []mcp.Config{}
	for rows.Next() {
		var (
			c       mcp.Config
			enabled int
		)
		if err := rows.Scan(&c.ID, &c.Name, &c.URL, &c.APIKey, &enabled, &c.TimeoutSec); err != nil {
			return nil, fmt.Errorf("scan mcp server: %w", err)
		}
		c.Enabled = enabled != 0
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetMCPServer 按 id 取单条配置。
func (s *SkillStore) GetMCPServer(id string) (mcp.Config, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return mcp.Config{}, errors.New("id 不能为空")
	}
	var (
		c       mcp.Config
		enabled int
	)
	err := s.db.QueryRow(
		`SELECT id, name, url, COALESCE(api_key,''), enabled, COALESCE(timeout_sec,30)
		 FROM mcp_servers WHERE id=?`, id).
		Scan(&c.ID, &c.Name, &c.URL, &c.APIKey, &enabled, &c.TimeoutSec)
	if err == sql.ErrNoRows {
		return mcp.Config{}, fmt.Errorf("MCP 服务 %q 不存在", id)
	}
	if err != nil {
		return mcp.Config{}, fmt.Errorf("get mcp server: %w", err)
	}
	c.Enabled = enabled != 0
	return c, nil
}

// UpsertMCPServer 新增或更新一条配置。
//
// api_key 传空串表示「保留原值」——这是必须的，因为列表接口只回掩码，
// 前端拿着掩码回存会把真钥匙覆盖成 "sk-…abcd" 这种废字符串。同理 name/url
// 传空也保留原值。要真正清空钥匙得用 ClearMCPServerKey。
func (s *SkillStore) UpsertMCPServer(c mcp.Config) error {
	id := strings.TrimSpace(c.ID)
	if id == "" {
		return errors.New("MCP 服务标识（id）不能为空")
	}
	if strings.TrimSpace(c.URL) == "" {
		return errors.New("MCP 地址不能为空")
	}
	if c.TimeoutSec <= 0 {
		c.TimeoutSec = 30
	}
	_, err := s.db.Exec(
		`INSERT INTO mcp_servers (id, name, url, api_key, enabled, timeout_sec, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(id) DO UPDATE SET
		   name        = CASE WHEN excluded.name    = '' THEN mcp_servers.name    ELSE excluded.name    END,
		   url         = CASE WHEN excluded.url     = '' THEN mcp_servers.url     ELSE excluded.url     END,
		   api_key     = CASE WHEN excluded.api_key = '' THEN mcp_servers.api_key ELSE excluded.api_key END,
		   enabled     = excluded.enabled,
		   timeout_sec = excluded.timeout_sec,
		   updated_at  = CURRENT_TIMESTAMP`,
		id, strings.TrimSpace(c.Name), strings.TrimSpace(c.URL), c.APIKey, mcpBoolToInt(c.Enabled), c.TimeoutSec)
	if err != nil {
		return fmt.Errorf("upsert mcp server: %w", err)
	}
	return nil
}

// SetMCPEnabled 开关一台服务器。返回受影响行数供调用方判断是否存在。
func (s *SkillStore) SetMCPEnabled(id string, enabled bool) (int64, error) {
	res, err := s.db.Exec(
		`UPDATE mcp_servers SET enabled=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		mcpBoolToInt(enabled), strings.TrimSpace(id))
	if err != nil {
		return 0, fmt.Errorf("toggle mcp server: %w", err)
	}
	return res.RowsAffected()
}

// DeleteMCPServer 删除一条配置。
func (s *SkillStore) DeleteMCPServer(id string) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM mcp_servers WHERE id=?`, strings.TrimSpace(id))
	if err != nil {
		return 0, fmt.Errorf("delete mcp server: %w", err)
	}
	return res.RowsAffected()
}

// mcpBoolToInt 把 bool 写成 SQLite 认的 0/1。
//
// 名字带 mcp 前缀是刻意的：store 包已经有一个同名的测试辅助函数，
// 重名会让整个 store 包（含全部回归测试）编译不过。
func mcpBoolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
