package model

import "time"

// Skill is the central unit: a curated "type-of-article writing" knowledge pack.
// Content files (system_prompt, template, examples) live in <data>/skills/<slug>/,
// metadata + runtime state live in SQLite.
type Skill struct {
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Category    string    `json:"category"`
	Version     int       `json:"version"`
	Enabled     bool      `json:"enabled"`
	InputParams []Param   `json:"input_params"`
	IsCore      bool      `json:"is_core,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// Metadata for display
	PromptLen    int    `json:"prompt_len,omitempty"`
	ExampleCount int    `json:"example_count,omitempty"`
	TrainedFrom  string `json:"trained_from,omitempty"`
	GeneratedBy  string `json:"generated_by,omitempty"`
	// SkillType classifies what the skill does at run time so the agent can
	// pick the right behavior: write | query | template (see constants below).
	// "write"    -> generate a document/article (default, legacy behavior)
	// "query"    -> return a办事流程/answers from the reference (no article)
	// "template" -> return the流程 + push the declared attachment file for download
	SkillType string `json:"skill_type,omitempty"`
	// Attachment is a file (relative to <data>/skills/<slug>/) that should be
	// delivered to the user when this skill is hit (e.g. "source/form.docx").
	// Empty means no downloadable attachment.
	Attachment string `json:"attachment,omitempty"`
}

// Skill type constants.
const (
	SkillTypeWrite    = "write"    // generate a document/article
	SkillTypeQuery    = "query"    // return a办事流程/answer from references
	SkillTypeTemplate = "template" // 流程 + downloadable template attachment
	SkillTypeDocGen   = "docgen"   // fill/generate an office file (word/excel/ppt/pdf) and deliver it
)

// Param defines a dynamic form field for a skill. Rendered by the frontend
// so each skill's UI doesn't need hand-written code.
type Param struct {
	Name        string   `json:"name"`
	Label       string   `json:"label"`
	Type        string   `json:"type"` // text | textarea | select | number
	Required    bool     `json:"required"`
	Placeholder string   `json:"placeholder,omitempty"`
	Help        string   `json:"help,omitempty"`
	Options     []string `json:"options,omitempty"`
	Default     string   `json:"default,omitempty"`
	Min         int      `json:"min,omitempty"`
	Max         int      `json:"max,omitempty"`
}

// LLMConfig is provider config for chat completions. Stored in SQLite,
// hot-swappable via admin UI (no restart).
type LLMConfig struct {
	ID        int       `json:"id"`
	Provider  string    `json:"provider"` // display name
	BaseURL   string    `json:"base_url"` // OpenAI-compatible base; empty = OpenAI default
	APIKey    string    `json:"api_key"`
	Model     string    `json:"model"`
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
}

// GenerateRequest is the public body to generate an article.
type GenerateRequest struct {
	Slug  string            `json:"slug"`
	Input map[string]string `json:"input"`
}

// StreamEvent is one SSE chunk pushed to the client during generation.
type StreamEvent struct {
	Type string   `json:"type"` // meta | delta | done | error
	Data string   `json:"data,omitempty"`
	Meta *Message `json:"meta,omitempty"`
}

type Message struct {
	ID      string `json:"id,omitempty"` // reference to store an article
	Title   string `json:"title,omitempty"`
	Content string `json:"content,omitempty"`
	Slug    string `json:"slug,omitempty"`
}

// SkillFile is a manageable content file inside a skill's knowledge pack.
// Path is relative to <data>/skills/<slug>/ (e.g. "system_prompt.md",
// "template.md", "requirement.md", "examples/example01.md", "source/original.docx").
type SkillFile struct {
	Path     string `json:"path"`              // relative path inside skill dir
	Kind     string `json:"kind"`              // prompt | template | templatefile | requirement | style | example | source
	Name     string `json:"name"`              // base filename
	Size     int    `json:"size"`              // bytes
	Content  string `json:"content,omitempty"` // populated on read only (text files)
	Editable bool   `json:"editable"`          // whether admin may edit/overwrite
	Mime     string `json:"mime,omitempty"`    // MIME type for preview dispatch (raw endpoint)
	Binary   bool   `json:"binary,omitempty"`  // true = not text; use GET /raw to preview
}

// TrainRequest is admin body to create a new skill via skill-generator.
type TrainRequest struct {
	Name        string `json:"name"` // a.k.a. new skill slug/title
	Category    string `json:"category"`
	Description string `json:"description"`
	Requirement string `json:"requirement"` // free-text requirement from admin
}

// AdminCredentials is the login body.
type AdminCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}
