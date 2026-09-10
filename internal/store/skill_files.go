package store

import (
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lizhemin15/skillforge/internal/model"
)

// ===== Knowledge-base style file management =====
//
// A skill is a knowledge pack living in <data>/skills/<slug>/:
//   system_prompt.md   — the core writing instructions (editable)
//   template.md        — article skeleton (editable, optional)
//   requirement.md     — the training requirement (editable)
//   examples/*.md      — few-shot sample articles (CRUD)
//   source/*           — raw uploaded reference files (managed: add/edit/delete)
//
// All paths exposed to the API are RELATIVE to the skill dir. Every entry
// point enforces the path stays inside the skill dir (no traversal).

// fileKind classifies a relative path and whether admin may edit it.
func fileKind(rel string) (kind string, editable bool) {
	switch rel {
	case "system_prompt.md":
		return "prompt", true
	case "template.md":
		return "template", true
	case "requirement.md":
		return "requirement", true
	case "style_profile.md":
		// immutable style anchor — read-only, never editable/deletable.
		return "style", false
	}
	if strings.HasPrefix(rel, "examples/") && strings.HasSuffix(rel, ".md") {
		return "example", true
	}
	if strings.HasPrefix(rel, "source/") {
		// raw reference material — binary files are previewable but not text-editable.
		return "source", !isBinaryExt(rel)
	}
	return "other", false
}

// isBinaryExt reports whether a filename looks like a non-textual format.
// Text-ish formats (md/txt/csv/json/etc.) come back false so they stay editable.
func isBinaryExt(rel string) bool {
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".pdf", ".docx", ".xlsx", ".pptx", ".doc", ".xls", ".ppt", ".docm", ".xlsm", ".pptm",
		".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".bmp", ".ico", ".tif", ".tiff",
		".zip", ".rar", ".7z", ".gz", ".tar", ".mp3", ".mp4", ".avi", ".mov":
		return true
	}
	return false
}

// mimeFor returns the MIME type used for raw preview delivery.
func mimeFor(rel string) string {
	ext := strings.ToLower(filepath.Ext(rel))
	switch ext {
	case ".pdf":
		return "application/pdf"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".bmp":
		return "image/bmp"
	case ".ico":
		return "image/x-icon"
	case ".tif", ".tiff":
		return "image/tiff"
	}
	if m := mime.TypeByExtension(filepath.Ext(rel)); m != "" {
		return m
	}
	return "application/octet-stream"
}

// safeRel validates rel stays inside the skill dir and isn't reserved/system.
func (s *SkillStore) safeRel(slug, rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", fmt.Errorf("路径为空")
	}
	clean := filepath.Clean("/" + rel) // force rooted; prevents ".."
	clean = strings.TrimPrefix(clean, "/")
	if clean == "." || strings.Contains(clean, "..") || filepath.IsAbs(clean) {
		return "", fmt.Errorf("非法路径: %s", rel)
	}
	first := clean
	if i := strings.Index(clean, "/"); i >= 0 {
		first = clean[:i]
	}
	if first == "meta.json" {
		return "", fmt.Errorf("系统文件不可访问")
	}
	return clean, nil
}

func (s *SkillStore) absFile(slug, rel string) (string, error) {
	rel, err := s.safeRel(slug, rel)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.skillDir(slug), rel), nil
}

// ListFiles returns every manageable file in the skill's knowledge pack.
func (s *SkillStore) ListFiles(slug string) ([]model.SkillFile, error) {
	dir := s.skillDir(slug)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("技能不存在")
	}
	var out []model.SkillFile
	add := func(path, rel, kind string) {
		fi, err := os.Stat(path)
		if err != nil || fi.IsDir() {
			return
		}
		_, editable := fileKind(rel)
		out = append(out, model.SkillFile{
			Path:     rel,
			Kind:     kind,
			Name:     fi.Name(),
			Size:     int(fi.Size()),
			Editable: editable,
		})
	}
	// recompute kind/editable for top-level known files
	knownKinds := map[string]string{
		"system_prompt.md": "prompt",
		"template.md":      "template",
		"requirement.md":   "requirement",
		"style_profile.md": "style",
	}
	// top-level known files
	for _, base := range []string{"system_prompt.md", "template.md", "requirement.md", "style_profile.md"} {
		add(filepath.Join(dir, base), base, knownKinds[base])
	}
	// examples/*.md
	if entries, err := os.ReadDir(filepath.Join(dir, "examples")); err == nil {
		for _, en := range entries {
			if en.IsDir() || !strings.HasSuffix(en.Name(), ".md") {
				continue
			}
			add(filepath.Join(dir, "examples", en.Name()), "examples/"+en.Name(), "example")
		}
	}
	// source/*
	if entries, err := os.ReadDir(filepath.Join(dir, "source")); err == nil {
		for _, en := range entries {
			if en.IsDir() {
				continue
			}
			add(filepath.Join(dir, "source", en.Name()), "source/"+en.Name(), "source")
		}
	}
	sort.Slice(out, func(i, j int) bool {
		oi, oj := orderOf(out[i].Kind), orderOf(out[j].Kind)
		if oi != oj {
			return oi < oj
		}
		return out[i].Path < out[j].Path
	})
	// sync Editable flag from fileKind
	for i := range out {
		_, out[i].Editable = fileKind(out[i].Path)
	}
	return out, nil
}

func orderOf(kind string) int {
	switch kind {
	case "prompt":
		return 1
	case "template":
		return 2
	case "requirement":
		return 3
	case "style":
		return 4
	case "example":
		return 5
	case "source":
		return 6
	}
	return 9
}

// ReadFile returns content of one managed file.
func (s *SkillStore) ReadFile(slug, rel string) (*model.SkillFile, error) {
	rel, err := s.safeRel(slug, rel)
	if err != nil {
		return nil, err
	}
	kind, _ := fileKind(rel)
	if kind == "" {
		return nil, fmt.Errorf("该文件不在技能知识库内: %s", rel)
	}
	abs, err := s.absFile(slug, rel)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("文件不存在: %s", rel)
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("是目录不是文件")
	}
	if fi.Size() > 4<<20 { // 4MB cap
		return nil, fmt.Errorf("文件过大(%d B)，不支持在线预览", fi.Size())
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	_, editable := fileKind(rel)
	binary := isBinaryExt(rel)
	sf := &model.SkillFile{
		Path: rel, Kind: kind, Name: fi.Name(), Size: int(fi.Size()),
		Editable: editable,
		Mime:     mimeFor(rel),
		Binary:   binary,
	}
	if !binary {
		sf.Content = string(b)
	}
	return sf, nil
}

// WriteFile creates/overwrites an editable file in the skill's knowledge pack.
func (s *SkillStore) WriteFile(slug, rel, content string) error {
	kind, editable := fileKind(rel)
	if !editable {
		return fmt.Errorf("该文件只读，不可编辑: %s", rel)
	}
	if kind == "" {
		return fmt.Errorf("不允许新建/覆盖该路径: %s", rel)
	}
	return s.writeAllow(slug, rel, []byte(content), kind)
}

// WriteFileRaw writes raw bytes into a skill's knowledge pack, bypassing the
// text-editability check. Only intended for source/ uploads (binary files such
// as .docx/.pdf/images are stored via this path). Kind is always "source".
func (s *SkillStore) WriteFileRaw(slug, rel string, data []byte) error {
	if !strings.HasPrefix(rel, "source/") {
		return fmt.Errorf("只能写入 source/ 目录: %s", rel)
	}
	abs, err := s.absFile(slug, rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	return os.WriteFile(abs, data, 0o644)
}

func (s *SkillStore) writeAllow(slug, rel string, content []byte, kind string) error {
	if kind == "" {
		return fmt.Errorf("不允许新建/覆盖该路径: %s", rel)
	}
	abs, err := s.absFile(slug, rel)
	if err != nil {
		return err
	}
	if kind == "example" {
		_ = os.MkdirAll(filepath.Join(s.skillDir(slug), "examples"), 0o755)
	}
	return os.WriteFile(abs, content, 0o644)
}

// ReadFileBytes returns raw bytes of a managed file (for download).
func (s *SkillStore) ReadFileBytes(slug, rel string) ([]byte, error) {
	if _, err := s.safeRel(slug, rel); err != nil {
		return nil, err
	}
	abs, err := s.absFile(slug, rel)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(abs)
}

// AddExample creates a new example file with an auto-incremented name.
func (s *SkillStore) AddExample(slug, content string) (string, error) {
	dir := filepath.Join(s.skillDir(slug), "examples")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	entries, _ := os.ReadDir(dir)
	n := len(entries) + 1
	for {
		name := fmt.Sprintf("example%02d.md", n+1)
		abs := filepath.Join(dir, name)
		if _, err := os.Stat(abs); os.IsNotExist(err) {
			if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
				return "", err
			}
			return "examples/" + name, nil
		}
		n++
	}
}

// DeleteFile removes an editable example file.
func (s *SkillStore) DeleteFile(slug, rel string) error {
	rel, err := s.safeRel(slug, rel)
	if err != nil {
		return err
	}
	kind, editable := fileKind(rel)
	if !editable {
		return fmt.Errorf("该文件不可删除: %s", rel)
	}
	if kind != "example" && kind != "source" {
		return fmt.Errorf("核心文件(system_prompt/template/requirement)不可删除，只能编辑")
	}
	abs, err := s.absFile(slug, rel)
	if err != nil {
		return err
	}
	if err := os.Remove(abs); err != nil {
		return fmt.Errorf("删除失败: %w", err)
	}
	return nil
}

// UpdateSkillMeta edits name/description/category of an existing skill.
func (s *SkillStore) UpdateSkillMeta(slug, name, desc, category string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("名称不能为空")
	}
	_, err := s.db.Exec(`UPDATE skills SET name=?, description=?, category=?, updated_at=CURRENT_TIMESTAMP WHERE slug=?`,
		name, desc, firstNonEmpty(category, "general"), slug)
	return err
}

// CreateSkill creates a brand-new skill manually (non-LLM): makes the
// directory + a minimal system_prompt, then registers metadata.
func (s *SkillStore) CreateSkill(sk *model.Skill, params []model.Param, systemPrompt string) error {
	dir := s.skillDir(sk.Slug)
	if err := os.MkdirAll(filepath.Join(dir, "examples"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "source"), 0o755); err != nil {
		return err
	}
	if strings.TrimSpace(systemPrompt) == "" {
		systemPrompt = "# 写作技能\n\n（尚未编写 system_prompt。请在管理端编辑此技能，撰写完整的写作提示词。）"
	}
	if err := os.WriteFile(filepath.Join(dir, "system_prompt.md"), []byte(systemPrompt), 0o644); err != nil {
		return err
	}
	if err := s.Create(sk, params); err != nil {
		return err
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ===== version snapshot / rollback (optimize safety) =====
//
// Every review/optimize snapshots the current system_prompt.md + template.md
// into <slug>/versions/v{N+1}/ before overwriting, so an admin can roll back a
// bad optimization (达尔文棘轮: keep improvements, undo regressions).

// versionsDir returns the versions root for a skill.
func (s *SkillStore) versionsDir(slug string) string {
	return filepath.Join(s.skillDir(slug), "versions")
}

// SnapshotVersion copies the current prompt+template into versions/v{next}/.
// Returns the next version number (current Version + 1).
func (s *SkillStore) SnapshotVersion(slug string, note string) (int, error) {
	sk, err := s.Get(slug)
	if err != nil {
		return 0, err
	}
	next := sk.Version + 1
	dir := filepath.Join(s.versionsDir(slug), fmt.Sprintf("v%d", next))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	abs := func(rel string) string { return filepath.Join(s.skillDir(slug), rel) }
	if b, e := os.ReadFile(abs("system_prompt.md")); e == nil {
		_ = os.WriteFile(filepath.Join(dir, "system_prompt.md"), b, 0o644)
	}
	if b, e := os.ReadFile(abs("template.md")); e == nil {
		_ = os.WriteFile(filepath.Join(dir, "template.md"), b, 0o644)
	}
	if strings.TrimSpace(note) != "" {
		_ = os.WriteFile(filepath.Join(dir, "note.txt"), []byte(note), 0o644)
	}
	return next, nil
}

// ListVersions returns the sorted snapshot versions for a skill, each with
// note + whether it contains a prompt / template.
type SkillVersion struct {
	Number      int    `json:"number"`
	Note        string `json:"note"`
	HasPrompt   bool   `json:"has_prompt"`
	HasTemplate bool   `json:"has_template"`
	IsCurrent   bool   `json:"is_current"`
}

func (s *SkillStore) ListVersions(slug string) ([]SkillVersion, error) {
	verDir := s.versionsDir(slug)
	entries, err := os.ReadDir(verDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no versions yet
		}
		return nil, err
	}
	cur, _ := s.Get(slug)
	var out []SkillVersion
	for _, en := range entries {
		if !en.IsDir() || !strings.HasPrefix(en.Name(), "v") {
			continue
		}
		var num int
		if _, err := fmt.Sscanf(en.Name(), "v%d", &num); err != nil {
			continue
		}
		dir := filepath.Join(verDir, en.Name())
		note := ""
		if b, e := os.ReadFile(filepath.Join(dir, "note.txt")); e == nil {
			note = strings.TrimSpace(string(b))
		}
		_, hpErr := os.Stat(filepath.Join(dir, "system_prompt.md"))
		_, htErr := os.Stat(filepath.Join(dir, "template.md"))
		out = append(out, SkillVersion{
			Number: num, Note: note,
			HasPrompt:   hpErr == nil,
			HasTemplate: htErr == nil,
			IsCurrent:   cur != nil && num == cur.Version,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number > out[j].Number })
	return out, nil
}

// RollbackVersion restores the prompt+template of a snapshot version into the
// live skill dir and sets the DB version back. Returns the restored version.
func (s *SkillStore) RollbackVersion(slug string, version int) (int, error) {
	dir := filepath.Join(s.versionsDir(slug), fmt.Sprintf("v%d", version))
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return 0, fmt.Errorf("版本 v%d 不存在", version)
	}
	// copy prompt + template back to live top-level
	if b, e := os.ReadFile(filepath.Join(dir, "system_prompt.md")); e == nil {
		if err := os.WriteFile(filepath.Join(s.skillDir(slug), "system_prompt.md"), b, 0o644); err != nil {
			return 0, err
		}
	}
	if b, e := os.ReadFile(filepath.Join(dir, "template.md")); e == nil {
		_ = os.WriteFile(filepath.Join(s.skillDir(slug), "template.md"), b, 0o644)
	}
	if err := s.SetVersion(slug, version); err != nil {
		return 0, err
	}
	return version, nil
}

// SetVersion sets a skill's version column (current live version).
func (s *SkillStore) SetVersion(slug string, version int) error {
	if _, err := s.db.Exec(`UPDATE skills SET version=?, updated_at=CURRENT_TIMESTAMP WHERE slug=?`, version, slug); err != nil {
		return err
	}
	return nil
}

// ReadStyleProfile returns the immutable style anchor content ("" if absent).
func (s *SkillStore) ReadStyleProfile(slug string) (string, error) {
	b, err := os.ReadFile(filepath.Join(s.skillDir(slug), "style_profile.md"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(b), nil
}

// ReadExamples returns the ordered examples/*.md contents for a skill.
func (s *SkillStore) ReadExamples(slug string) ([]string, error) {
	dir := filepath.Join(s.skillDir(slug), "examples")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var out []string
	for _, en := range entries {
		if en.IsDir() || !strings.HasSuffix(en.Name(), ".md") {
			continue
		}
		if b, e := os.ReadFile(filepath.Join(dir, en.Name())); e == nil {
			out = append(out, string(b))
		}
	}
	return out, nil
}
