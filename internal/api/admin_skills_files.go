package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/lizhemin15/skillforge/internal/model"
	"github.com/lizhemin15/skillforge/internal/store"
)

// ===== Knowledge-base style skill file management =====
// Admin can treat a skill's knowledge pack like a managed knowledge base:
// list/read/edit/delete its content files, upload new source material,
// add examples, edit metadata, and create skills manually.

// ListSkillFiles returns the full file tree of a skill's knowledge pack.
func (a *Admin) ListSkillFiles(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	files, err := a.store.ListFiles(slug)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	// manual_mode = 这个技能是不是「手册模式」（目录里有 categories/）。
	// 管理端左树靠它决定要不要摆「+ 新增分类」：非手册技能上摆这个按钮，
	// 点下去后端只能回「该技能不是手册模式」——等于承诺一个只会报错的操作。
	// 信号由后端给而不是前端去数「有没有 category 行」：空分类目录（分类被删光
	// 但目录还在）在前端看起来就像非手册，那就是**该给入口却不给**、
	// 用户以为功能没了。HasCategories 看的是目录，不是行数。
	writeJSON(w, http.StatusOK, map[string]any{
		"files":       files,
		"manual_mode": a.store.HasCategories(slug),
	})
}

// ReadSkillFile returns the content of one managed file.
func (a *Admin) ReadSkillFile(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	rel := r.URL.Query().Get("path")
	// download mode: stream raw bytes with attachment headers
	if r.URL.Query().Get("download") == "1" {
		content, err := a.store.ReadFileBytes(slug, rel)
		if err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		// 下载的是真文件（模板 .docx/.xlsx、素材 .pdf…），Content-Type 必须按扩展名给：
		// 一律 text/plain 会让浏览器/解压工具认错格式（.xlsx 被当成文本打开）。
		w.Header().Set("Content-Type", mimeTypeFor(rel))
		name := rel
		if idx := lastSlash(rel); idx >= 0 {
			name = rel[idx+1:]
		}
		w.Header().Set("Content-Disposition", contentDisposition(name))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(content))
		return
	}
	// preview-extract mode: for binary extractable docs (docx/xlsx/pptx/pdf),
	// read raw bytes and return extracted text synchronously for inline preview.
	if r.URL.Query().Get("preview") == "1" {
		f, err := a.store.ReadFile(slug, rel)
		if err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		if !f.Binary || !extractableExt(rel) {
			writeJSON(w, http.StatusOK, map[string]any{"previewable": false})
			return
		}
		raw, err := a.store.ReadFileBytes(slug, rel)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		text, xerr := a.extractDoc(rel, raw)
		if xerr != nil {
			writeJSON(w, http.StatusOK, map[string]any{"previewable": true, "error": xerr.Error(), "text": ""})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"previewable": true, "text": text, "mime": f.Mime})
		return
	}
	f, err := a.store.ReadFile(slug, rel)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// contentDisposition 生成下载头。
//
// 只写 `filename=` 时，HTTP 头按 latin-1 传，中文名到浏览器就是乱码或直接丢失
// （用户下载到「____.docx」）。所以 ASCII 兜底名 + RFC 5987 的 filename* 一起给：
// 老浏览器读前者，现代浏览器读后者拿到正确中文名。
func contentDisposition(name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 128 && r != '"' && r != '\\' {
			return r
		}
		return '_'
	}, name)
	return fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s", ascii, url.PathEscape(name))
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

// WriteSkillFile creates/overwrites an editable file in the skill pack.
func (a *Admin) WriteSkillFile(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	var body struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := readBody(r, &body); err != nil || body.Path == "" {
		writeErr(w, http.StatusBadRequest, "需要 path 与 content")
		return
	}
	if err := a.store.WriteFile(slug, body.Path, body.Content); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// AddSkillExample appends a new example article to the skill.
func (a *Admin) AddSkillExample(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	var body struct {
		Content string `json:"content"`
	}
	if err := readBody(r, &body); err != nil || body.Content == "" {
		writeErr(w, http.StatusBadRequest, "需要 content")
		return
	}
	rel, err := a.store.AddExample(slug, body.Content)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": rel})
}

// DeleteSkillFile removes an example file from the skill pack.
func (a *Admin) DeleteSkillFile(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	rel := r.URL.Query().Get("path")
	if rel == "" {
		writeErr(w, http.StatusBadRequest, "需要 path")
		return
	}
	if err := a.store.DeleteFile(slug, rel); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// UpdateSkillMeta edits name/description/category of an existing skill.
func (a *Admin) UpdateSkillMeta(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Category    string `json:"category"`
	}
	if err := readBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体无效")
		return
	}
	if err := a.store.UpdateSkillMeta(slug, body.Name, body.Description, body.Category); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// UploadSkillFile adds raw material to a skill (~ multipart).
//
// 落点由表单字段 target 决定：
//   - 缺省 / "source" → source/<文件名>，原始素材（可抽取文字的会顺带抽一份 txt）
//   - "template"      → <文件名>（技能目录顶层），可填模板，同名即替换
//
// 模板必须能**原地替换**：用户换模板的真实动作就是「同名重传」，
// 所以这里不能另起文件名（那会变成同一个技能挂两个模板，模型随机挑一个，行为不可复现）。
func (a *Admin) UploadSkillFile(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if err := r.ParseMultipartForm(maxFormBytes); err != nil {
		writeErr(w, http.StatusBadRequest, "multipart 解析失败: "+err.Error())
		return
	}
	// 必须在 ParseMultipartForm 之后读：先读字段会把 multipart 提前按默认上限解析掉，
	// 后面这次 ParseMultipartForm(20<<20) 就形同虚设（体积上限被悄悄换成默认值）。
	target := strings.TrimSpace(r.FormValue("target"))
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "需要 file 字段")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxDocBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取文件失败")
		return
	}
	name := sanitizeFilename(hdr.Filename)
	isTemplate := target == "template"
	rel := "source/" + name
	if isTemplate {
		// 顶层模板：扩展名必须与 fill_template 认的格式一致，
		// 判据用 store 那一份真值（FillableTemplateFormat），不在这里另抄扩展名。
		if store.FillableTemplateFormat(name) == "" {
			writeErr(w, http.StatusBadRequest,
				"模板只支持 .docx/.xlsx（fill_template 能填的格式）: "+name)
			return
		}
		rel = name
	}
	if err := a.store.WriteFileRaw(slug, rel, data); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// If it's a document format we can extract text from, parse it asynchronously
	// and land the text as source/<base>.txt next to the raw file. Non-blocking:
	// extraction failure never fails the upload — the raw file stays for download.
	// 模板（target=template）跳过抽取：模板是给 fill_template 填的骨架，
	// 把它的样例文字抽成 source/<base>.txt 只会污染技能知识库（模型会当成范文）。
	if !isTemplate && extractableExt(hdr.Filename) {
		go a.extractAndLand(slug, hdr.Filename, data)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": rel})
}

// extractableExt reports whether a filename is a document we can pull text from.
func extractableExt(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".pdf", ".docx", ".xlsx", ".pptx", ".txt", ".md", ".csv":
		return true
	case ".doc", ".xls", ".ppt", ".docm", ".xlsm", ".pptm":
		return true // legacy Office sent to ocrd for best-effort handling
	}
	return false
}

func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return "file.txt"
	}
	return strings.TrimSpace(name)
}

// extractAndLand sends a document (PDF/docx/xlsx/etc.) to the extraction
// microservice and writes the extracted text into source/<base>.txt (next to
// the raw file). Best-effort: any failure is logged but never fails the caller.
// Runs in its own goroutine.
func (a *Admin) extractAndLand(slug, filename string, data []byte) {
	if a.ocrURL == "" {
		return
	}
	start := time.Now()
	base := strings.TrimSuffix(path.Base(filename), path.Ext(filename))
	if base == "" {
		base = "extracted"
	}
	text, err := a.extractDoc(filename, data)
	if err != nil {
		log.Printf("[extract] %s/%s 解析失败: %v (%.1fs)", slug, filename, err, time.Since(start).Seconds())
		return
	}
	land := "source/" + base + ".txt"
	if err := a.store.WriteFile(slug, land, text); err != nil {
		log.Printf("[extract] %s/%s 落地文本失败: %v", slug, land, err)
		return
	}
	log.Printf("[extract] %s/%s -> %s (%d chars, %.1fs)", slug, filename, land, len(text), time.Since(start).Seconds())
}

// extractDoc calls the ocrd microservice (POST /extract) and returns the text.
func (a *Admin) extractDoc(filename string, data []byte) (string, error) {
	// 超时必须可配：写死 300s 而真实扫描件要 397.5s，只会让上传解析永远失败。
	client := &http.Client{Timeout: a.ocrTimeoutOrDefault()}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	name := path.Base(filename)
	if name == "" {
		name = "scan"
	}
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(data); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, a.ocrURL+"/extract", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		OK   bool   `json:"ok"`
		Text string `json:"text"`
		Err  string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if !out.OK {
		return "", fmt.Errorf("解析服务: %s", out.Err)
	}
	return strings.TrimSpace(out.Text), nil
}

// RawSkillFile streams a managed file's raw bytes with the correct Content-Type,
// so previews (<iframe> for PDF, <img> for images) render without JSON wrapping.
func (a *Admin) RawSkillFile(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	rel := r.URL.Query().Get("path")
	content, err := a.store.ReadFileBytes(slug, rel)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	name := rel
	if idx := lastSlash(rel); idx >= 0 {
		name = rel[idx+1:]
	}
	w.Header().Set("Content-Type", mimeTypeFor(rel))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "inline; filename="+name)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

// 统一委托给 store：下载、预览两处的 Content-Type 必须是同一份真值，
// 各写一张扩展名表迟早不对称（本轮就踩过：下载被硬编码成 text/plain，
// 模板 .docx/.xlsx 全被当文本）。
func mimeTypeFor(rel string) string { return store.MimeFor(rel) }

// CreateSkill manually creates a brand-new skill (non-LLM).
func (a *Admin) CreateSkill(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Slug         string        `json:"slug"`
		Name         string        `json:"name"`
		Description  string        `json:"description"`
		Category     string        `json:"category"`
		Params       []model.Param `json:"input_params"`
		SystemPrompt string        `json:"system_prompt"`
	}
	if err := readBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体无效")
		return
	}
	if body.Slug == "" || body.Name == "" {
		writeErr(w, http.StatusBadRequest, "需要 slug 与 name")
		return
	}
	if body.Params == nil {
		body.Params = []model.Param{}
	}
	sk := &model.Skill{
		Slug:        body.Slug,
		Name:        body.Name,
		Description: body.Description,
		Category:    firstNonEmptyS(body.Category, "general"),
		Enabled:     true,
	}
	if err := a.store.CreateSkill(sk, body.Params, body.SystemPrompt); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "slug": body.Slug})
}

// SetSkillCore marks/unmarks a skill as a "core" (generic) skill.
//
// 核心 = 通用能力（办公文档管家、技能工厂），与具体业务无关；业务技能不该占核心位。
// 前台与技能列表靠 is_core DESC 把通用能力排在前面，核心技能同时受删除保护。
func (a *Admin) SetSkillCore(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Slug   string `json:"slug"`
		IsCore *bool  `json:"is_core"`
	}
	if err := readBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体无效")
		return
	}
	if body.Slug == "" || body.IsCore == nil {
		writeErr(w, http.StatusBadRequest, "需要 slug 与 is_core")
		return
	}
	if err := a.store.SetCore(body.Slug, *body.IsCore); err != nil {
		// 技能不存在必须是 404：返回 200 会让管理端显示成"已设为核心"，
		// 而库里什么都没有 —— 用户以为改成功了，其实点了个已删除的技能。
		if errors.Is(err, store.ErrSkillNotFound) {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "slug": body.Slug, "is_core": *body.IsCore})
}

func firstNonEmptyS(vals ...string) string {
	for _, v := range vals {
		if len(v) > 0 {
			return v
		}
	}
	return ""
}
