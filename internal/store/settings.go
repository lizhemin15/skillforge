package store

import (
	"errors"
	"fmt"
	"strings"
)

// 站点级设置（管理端可改的全局配置）。
//
// 用一张通用 key/value 表而不是给每项设置加列：加设置项不用写 schema 迁移。
// **默认值在 Go 侧兜底、不写进 DB** —— 升级到本版本的老库（settings 表是空的）
// 也能拿到正确文案，不会前端白屏。

const (
	SettingSiteName    = "site_name"
	SettingSiteTagline = "site_tagline"
)

// 站点名称默认值。
const (
	DefaultSiteName    = "SkillForge"
	DefaultSiteTagline = "智能写作工坊"
)

// settingsDefaults 是「DB 里没有这个键时」的兜底值。
// 只有列在这里的键才有默认值；不在表里的键读出来是空串。
var settingsDefaults = map[string]string{
	SettingSiteName:    DefaultSiteName,
	SettingSiteTagline: DefaultSiteTagline,
}

// GetSettings 读出给定的设置项，缺失的键用默认值兜底（没有默认值则为空串）。
// 不传 keys 时返回全部「有默认值的键」。
func (s *SkillStore) GetSettings(keys ...string) (map[string]string, error) {
	if len(keys) == 0 {
		for k := range settingsDefaults {
			keys = append(keys, k)
		}
	}
	// 先铺默认值，再用 DB 里的覆盖。DB 里存了空串也算「有这条记录」——
	// 空值是否合法由调用方（API 校验）判断，store 层不做业务判断。
	out := make(map[string]string, len(keys))
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = settingsDefaults[k] // 不存在则零值 ""
		want[k] = true
	}
	rows, err := s.db.Query(`SELECT key, val FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("get settings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("get settings: %w", err)
		}
		if want[k] {
			out[k] = v
		}
	}
	return out, rows.Err()
}

// GetSettingsRaw 只回「DB 里真的存过」的键，不铺默认值：缺失的键**不出现在返回的 map 里**。
//
// 用途：区分「用户改过」和「回落到默认」。GetSettings 会给你补上默认值，于是
// 「删键恢复默认」之后读到的仍是默认文案（非空），拿它判 is_custom 会把
// 「已恢复默认」误报成「已自定义」—— 这正是 site_test 抓到的那个 bug。
func (s *SkillStore) GetSettingsRaw(keys ...string) (map[string]string, error) {
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}
	out := make(map[string]string, len(keys))
	rows, err := s.db.Query(`SELECT key, val FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("get settings raw: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("get settings raw: %w", err)
		}
		if len(want) == 0 || want[k] {
			out[k] = v
		}
	}
	return out, rows.Err()
}

// SetSettings 原子写入多个设置项（upsert）。空 map 是合法 no-op
// —— 调用方只改一个字段时另一个字段就是空的，不该因此报错。
func (s *SkillStore) SetSettings(kv map[string]string) error {
	if len(kv) == 0 {
		return nil
	}
	for k := range kv {
		if strings.TrimSpace(k) == "" {
			return errors.New("set settings: 键不能为空")
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range kv {
		if _, err := tx.Exec(
			`INSERT INTO settings (key, val, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
			 ON CONFLICT(key) DO UPDATE SET val=excluded.val, updated_at=CURRENT_TIMESTAMP`,
			k, v); err != nil {
			return fmt.Errorf("set settings %q: %w", k, err)
		}
	}
	return tx.Commit()
}

// ResetSettings 删除指定键，让它们回落到默认值（管理端「恢复默认」用）。
func (s *SkillStore) ResetSettings(keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, k := range keys {
		if _, err := tx.Exec(`DELETE FROM settings WHERE key=?`, k); err != nil {
			return fmt.Errorf("reset settings %q: %w", k, err)
		}
	}
	return tx.Commit()
}
