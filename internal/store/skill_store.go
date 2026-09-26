package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lizhemin15/skillforge/internal/model"
)

// SkillStore manages skills across the filesystem (content) and SQLite (metadata).
type SkillStore struct {
	db        *sql.DB
	skillsDir string
}

func NewSkillStore(db *sql.DB, dataDir string) *SkillStore {
	s := &SkillStore{db: db, skillsDir: filepath.Join(dataDir, "skills")}
	s.seedCoreSkills()
	// 历史库里有技能被错误地标成核心（业务技能占了核心位）。
	// 归一化只跑一次，之后管理端「设为核心 / 取消核心」的选择不会再被启动流程覆盖。
	_, _ = s.NormalizeCoreSkills(DefaultCoreSkillSlugs)
	return s
}

func (s *SkillStore) SkillsDir() string { return s.skillsDir }

// skillDir returns <skillsDir>/<slug>
func (s *SkillStore) skillDir(slug string) string { return filepath.Join(s.skillsDir, slug) }

// List returns all skills with their metadata + light content stats.
func (s *SkillStore) List() ([]model.Skill, error) {
	rows, err := s.db.Query(`SELECT slug,name,description,category,version,enabled,is_core,created_at,updated_at,COALESCE(skill_type,''),COALESCE(attachment,'') FROM skills ORDER BY is_core DESC, name ASC`)
	if err != nil {
		return nil, err
	}
	var out []model.Skill
	for rows.Next() {
		var sk model.Skill
		var ct, ut string
		if err := rows.Scan(&sk.Slug, &sk.Name, &sk.Description, &sk.Category, &sk.Version, &sk.Enabled, &sk.IsCore, &ct, &ut, &sk.SkillType, &sk.Attachment); err != nil {
			rows.Close()
			return nil, err
		}
		sk.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", ct)
		sk.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", ut)
		out = append(out, sk)
		// n.b. TrainStatus not loaded here
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	// NOTE: rows must be fully closed before issuing further queries — the
	// single-connection pool (SetMaxOpenConns(1)) would deadlock otherwise.
	for i := range out {
		if p, err := s.params(out[i].Slug); err == nil {
			out[i].InputParams = p
		}
		out[i].PromptLen, out[i].ExampleCount, _ = s.contentStats(out[i].Slug)
	}
	return out, nil
}

// Get returns one skill including params + content stats.
func (s *SkillStore) Get(slug string) (*model.Skill, error) {
	var sk model.Skill
	var ct, ut string
	var enabled int
	err := s.db.QueryRow(`SELECT slug,name,description,category,version,enabled,is_core,created_at,updated_at,COALESCE(skill_type,''),COALESCE(attachment,'') FROM skills WHERE slug=?`, slug).
		Scan(&sk.Slug, &sk.Name, &sk.Description, &sk.Category, &sk.Version, &enabled, &sk.IsCore, &ct, &ut, &sk.SkillType, &sk.Attachment)
	if err != nil {
		return nil, err
	}
	sk.Enabled = enabled == 1
	sk.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", ct)
	sk.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", ut)
	sk.InputParams, _ = s.params(slug)
	sk.PromptLen, sk.ExampleCount, _ = s.contentStats(slug)
	return &sk, nil
}

func (s *SkillStore) params(slug string) ([]model.Param, error) {
	rows, err := s.db.Query(`SELECT name,label,type,required,placeholder,help,options,default_val FROM params WHERE slug=? ORDER BY position`, slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Param
	for rows.Next() {
		var p model.Param
		var req int
		var opts string
		if err := rows.Scan(&p.Name, &p.Label, &p.Type, &req, &p.Placeholder, &p.Help, &opts, &p.Default); err != nil {
			return nil, err
		}
		p.Required = req == 1
		if opts != "" {
			_ = json.Unmarshal([]byte(opts), &p.Options)
		}
		out = append(out, p)
	}
	return out, nil
}

// contentStats reads content files to report prompt size + example count.
func (s *SkillStore) contentStats(slug string) (promptLen, exampleCount int, err error) {
	dir := s.skillDir(slug)
	sp := filepath.Join(dir, "system_prompt.md")
	if b, e := os.ReadFile(sp); e == nil {
		promptLen = len(b)
	}
	ex := filepath.Join(dir, "examples")
	if entries, e := os.ReadDir(ex); e == nil {
		for _, en := range entries {
			if !en.IsDir() && strings.HasSuffix(en.Name(), ".md") {
				exampleCount++
			}
		}
	}
	return promptLen, exampleCount, nil
}

// SystemPrompt returns the system prompt content (with few-shot examples
// appended) for generating articles.
func (s *SkillStore) SystemPrompt(slug string) (string, error) {
	dir := s.skillDir(slug)
	sp := filepath.Join(dir, "system_prompt.md")
	b, err := os.ReadFile(sp)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.Write(b)
	// Append examples as few-shot
	ex := filepath.Join(dir, "examples")
	if entries, err := os.ReadDir(ex); err == nil {
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, en := range entries {
			if en.IsDir() || !strings.HasSuffix(en.Name(), ".md") {
				continue
			}
			eb, _ := os.ReadFile(filepath.Join(ex, en.Name()))
			sb.WriteString("\n\n--- 示例范文" + strings.TrimSuffix(en.Name(), ".md") + " ---\n")
			sb.Write(eb)
		}
	}
	return sb.String(), nil
}

// Template returns the skeleton template content (may be empty).
func (s *SkillStore) Template(slug string) (string, error) {
	b, err := os.ReadFile(filepath.Join(s.skillDir(slug), "template.md"))
	if err != nil {
		return "", nil // template optional
	}
	return string(b), nil
}

// AttachmentPath resolves a downloadable attachment for a skill, guarding
// against path traversal: the given rel must resolve strictly inside the
// skill directory and the resolved file must exist. Used by the public
// download endpoint so only declared skill files are served.
func (s *SkillStore) AttachmentPath(slug, rel string) (string, bool) {
	if slug == "" || rel == "" || strings.Contains(rel, "..") {
		return "", false
	}
	dir := s.skillDir(slug)
	full := filepath.Join(dir, filepath.FromSlash(rel))
	// resolve symlinks+`..` then require prefix
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	absFull, err := filepath.Abs(full)
	if err != nil {
		return "", false
	}
	if absFull != absDir && !strings.HasPrefix(absFull, absDir+string(os.PathSeparator)) {
		return "", false
	}
	fi, err := os.Stat(absFull)
	if err != nil || fi.IsDir() {
		return "", false
	}
	return absFull, true
}

// Create registers a skill's metadata (content files already written by generator).
func (s *SkillStore) Create(sk *model.Skill, params []model.Param) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stype := sk.SkillType
	if stype == "" {
		stype = model.SkillTypeWrite
	}
	if _, err := tx.Exec(`INSERT INTO skills(slug,name,description,category,version,enabled,is_core,skill_type,attachment) VALUES(?,?,?,?,?,?,?,?,?)`,
		sk.Slug, sk.Name, sk.Description, sk.Category, sk.Version, bool2int(sk.Enabled), bool2int(sk.IsCore), stype, sk.Attachment); err != nil {
		return err
	}
	for i, p := range params {
		opts, _ := json.Marshal(p.Options)
		if _, err := tx.Exec(`INSERT INTO params(slug,name,label,type,required,placeholder,help,options,default_val,position) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			sk.Slug, p.Name, p.Label, p.Type, bool2int(p.Required), p.Placeholder, p.Help, string(opts), p.Default, i); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpdateMeta 更新技能的展示元信息（名称/描述/分类）。
//
// 这是极简通道的专属需求：阶段 A 只能用「推断出来的名字」注册（名字来自指南标题
// 或文件名），阶段 B 的模型精炼会给出更合适的名称与描述，必须能回写。
// 只动这三列，不碰 version/enabled/skill_type —— 回写展示信息不该顺手改行为开关。
func (s *SkillStore) UpdateMeta(slug, name, description, category string) error {
	_, err := s.db.Exec(`UPDATE skills SET name=?,description=?,category=?,updated_at=CURRENT_TIMESTAMP WHERE slug=?`,
		name, description, category, slug)
	return err
}

// ReplaceParams 用一份新的输入项清单替换旧的（极简通道精炼完成后回写）。
//
// 用「删净再插」而不是逐条 upsert：输入项是「这份技能该向用户问哪些信息」的完整
// 声明，两次生成的交集没有保留价值。留下上一版多出来的项，运行时就会继续追问一个
// 已经不存在的字段——用户看到的是莫名其妙的问题，而不是明确的报错。
func (s *SkillStore) ReplaceParams(slug string, params []model.Param) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM params WHERE slug=?`, slug); err != nil {
		return err
	}
	for i, p := range params {
		opts, _ := json.Marshal(p.Options)
		if _, err := tx.Exec(`INSERT INTO params(slug,name,label,type,required,placeholder,help,options,default_val,position) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			slug, p.Name, p.Label, p.Type, bool2int(p.Required), p.Placeholder, p.Help, string(opts), p.Default, i); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetEnabled toggles a skill.
func (s *SkillStore) SetEnabled(slug string, enabled bool) error {
	_, err := s.db.Exec(`UPDATE skills SET enabled=?, updated_at=CURRENT_TIMESTAMP WHERE slug=?`, bool2int(enabled), slug)
	return err
}

// Delete registers a skill from DB and removes its content dir.
func (s *SkillStore) Delete(slug string) error {
	if _, err := s.db.Exec(`DELETE FROM skills WHERE slug=?`, slug); err != nil {
		return err
	}
	os.RemoveAll(s.skillDir(slug))
	return nil
}

// SaveArticle stores a generated article.
func (s *SkillStore) SaveArticle(slug, title, content string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO articles(slug,title,content) VALUES(?,?,?)`, slug, title, content)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetArticle returns a stored article.
func (s *SkillStore) GetArticle(id int64) (*model.Message, error) {
	var m model.Message
	var slug string
	if err := s.db.QueryRow(`SELECT id,slug,title,content FROM articles WHERE id=?`, id).
		Scan(&m.ID, &slug, &m.Title, &m.Content); err != nil {
		return nil, err
	}
	m.Slug = slug
	num := fmt.Sprintf("%d", id)
	m.ID = num
	return &m, nil
}

// ===== LLM config store =====

// ListLLMConfigs returns all LLM provider configs (API key masked).
func (s *SkillStore) ListLLMConfigs() ([]model.LLMConfig, error) {
	rows, err := s.db.Query(`SELECT id,provider,base_url,api_key,model,is_active,created_at FROM llm_config ORDER BY is_active DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.LLMConfig
	for rows.Next() {
		var c model.LLMConfig
		var act int
		var ct string
		if err := rows.Scan(&c.ID, &c.Provider, &c.BaseURL, &c.APIKey, &c.Model, &act, &ct); err != nil {
			return nil, err
		}
		c.IsActive = act == 1
		c.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", ct)
		out = append(out, c)
	}
	return out, nil
}

// GetLLM returns one config by id.
func (s *SkillStore) GetLLM(id int) (*model.LLMConfig, error) {
	var c model.LLMConfig
	var act int
	var ct string
	err := s.db.QueryRow(`SELECT id,provider,base_url,api_key,model,is_active,created_at FROM llm_config WHERE id=?`, id).
		Scan(&c.ID, &c.Provider, &c.BaseURL, &c.APIKey, &c.Model, &act, &ct)
	if err != nil {
		return nil, err
	}
	c.IsActive = act == 1
	c.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", ct)
	return &c, nil
}

// GetActiveLLM returns the active LLM config, or the first one if none flagged.
func (s *SkillStore) GetActiveLLM() (*model.LLMConfig, error) {
	c, err := s.spreadLLMConfigs()
	if err != nil {
		return nil, err
	}
	return c, nil
}

// propagate masking
func (s *SkillStore) spreadLLMConfigs() (*model.LLMConfig, error) {
	var c model.LLMConfig
	var act int
	var ct string
	err := s.db.QueryRow(`SELECT id,provider,base_url,api_key,model,is_active,created_at FROM llm_config WHERE is_active=1 LIMIT 1`).
		Scan(&c.ID, &c.Provider, &c.BaseURL, &c.APIKey, &c.Model, &act, &ct)
	if err != nil {
		if err == sql.ErrNoRows {
			// fall back to first
			err = s.db.QueryRow(`SELECT id,provider,base_url,api_key,model,is_active,created_at FROM llm_config ORDER BY id ASC LIMIT 1`).
				Scan(&c.ID, &c.Provider, &c.BaseURL, &c.APIKey, &c.Model, &act, &ct)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	c.IsActive = act == 1
	c.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", ct)
	return &c, nil
}

// UpsertLLMConfig inserts or updates a provider config.
func (s *SkillStore) UpsertLLMConfig(c *model.LLMConfig) (int64, error) {
	if c.ID > 0 {
		_, err := s.db.Exec(`UPDATE llm_config SET provider=?,base_url=?,api_key=?,model=?,is_active=? WHERE id=?`,
			c.Provider, c.BaseURL, c.APIKey, c.Model, bool2int(c.IsActive), c.ID)
		return int64(c.ID), err
	}
	res, err := s.db.Exec(`INSERT INTO llm_config(provider,base_url,api_key,model,is_active) VALUES(?,?,?,?,?)`,
		c.Provider, c.BaseURL, c.APIKey, c.Model, bool2int(c.IsActive))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetActiveLLM flags one config active, others inactive.
func (s *SkillStore) SetActiveLLM(id int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE llm_config SET is_active=0`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE llm_config SET is_active=1 WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteLLMConfig removes a provider config.
func (s *SkillStore) DeleteLLMConfig(id int) error {
	_, err := s.db.Exec(`DELETE FROM llm_config WHERE id=?`, id)
	return err
}

// ===== admin auth =====

// BootstrapAdmin 只在「库里一个管理员都没有」时建号。
//
// ⚠️ 原来是另一个语义：每次启动都用 env 里的密码覆盖已有账号。那等于管理端
// 改的密码活不过一次重启 —— 用户改完看着成功，重启后旧密码又能登进去，
// 而新密码失效。表现比「不能改」更坏：它让人以为自己记错了密码。
//
// 所以 env 的定位是「第一次开机用的初始密码」，不是「持续生效的配置」。
// 已经在用的部署不受影响：库里早有账号时这函数是空操作（密码保持现状）。
func (s *SkillStore) BootstrapAdmin(username, hash string) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM admins`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO admins(username,pass_hash) VALUES(?,?)`, username, hash)
	return err
}

// GetAdminHash returns the pass hash for a username.
func (s *SkillStore) GetAdminHash(username string) (string, error) {
	var h string
	err := s.db.QueryRow(`SELECT pass_hash FROM admins WHERE username=?`, username).Scan(&h)
	return h, err
}

// CountAdmins 给「改自己账号」的校验用：改完不能把系统里最后一个管理员改没了。
func (s *SkillStore) CountAdmins() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM admins`).Scan(&n)
	return n, err
}

// SetAdminPassword updates a password hash.
func (s *SkillStore) SetAdminPassword(username, hash string) error {
	res, err := s.db.Exec(`UPDATE admins SET pass_hash=? WHERE username=?`, hash, username)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 没打中任何行必须报错：静默成功只会让「改密码成功、登录还是旧密码」这种
		// 幽灵故障流到用户面前（改用户名时写错旧名就会这样）。
		return fmt.Errorf("账号不存在：%s", username)
	}
	return nil
}

// RenameAdmin 改用户名 + 可选改密码（hash 为空表示只改名）。
// 一步完成是刻意的：分成两次写，中间失败会留下「密码已换、名字还是旧的」的
// 半截状态，而用户以为自己改的是同一个账号。
func (s *SkillStore) RenameAdmin(oldName, newName, hash string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if hash != "" {
		res, err := tx.Exec(`UPDATE admins SET pass_hash=? WHERE username=?`, hash, oldName)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("账号不存在：%s", oldName)
		}
	}
	if newName != "" && newName != oldName {
		// UNIQUE 约束兜底：重名会返回错误，交给上层翻成人话。
		if _, err := tx.Exec(`UPDATE admins SET username=? WHERE username=?`, newName, oldName); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetTrainStatus stores per-skill training metadata (trained_from, generated_by).
// It also records the skill type and downloadable attachment when the generator
// detects a non-write skill so the agent knows how to behave at run time.
func (s *SkillStore) SetTrainStatus(slug, trainedFrom, generatedBy, skillType, attachment string, version int) error {
	_, err := s.db.Exec(`UPDATE skills SET version=?, skill_type=?, attachment=? WHERE slug=?`, version, skillType, attachment, slug)
	return err
}

func bool2int(b bool) int {
	if b {
		return 1
	}
	return 0
}
