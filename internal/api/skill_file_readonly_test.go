package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/model"
)

// styleAnchorBody 是训练流程固化出来的风格锚点内容（generator 写盘时的样子）。
const styleAnchorBody = "# 风格锚点\n\n- 读者：公司内部员工\n- 语气：正式\n"

// promptBody 是可编辑文件，用来证明「只读」不是一刀切。
const promptBody = "# 系统提示词\n\n你是公文写作助手。\n"

// TestStyleProfileIsReadOnly 钉住「风格锚点不可编辑」这条前后端契约。
//
// 真实 bug（用户视角）：管理端技能知识库里点开 style_profile.md，
// 界面给出一个可编辑框 + 「保存」按钮 —— 因为前端完全无视文件列表接口
// 已经返回的 editable:false。用户认真改完点保存，后端 store.WriteFile
// 直接 400「该文件只读，不可编辑」，改动全丢，界面看着像坏了。
//
// 后端这一侧本来就是对的，所以本测试抓不住「前端回归」那半边；它守的是另一半，
// 且同样重要：别哪天有人把 style_profile.md 从只读白名单里放出去
// （或反过来把 system_prompt.md 一起锁死）——那才是这类 bug 的源头。
func TestStyleProfileIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	st := newStoreForTest(t, dir)
	adm := &Admin{store: st}

	const slug = "风格锚点技能"
	skillDir := filepath.Join(dir, "skills", slug)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("建技能目录失败: %v", err)
	}
	stylePath := filepath.Join(skillDir, "style_profile.md")
	promptPath := filepath.Join(skillDir, "system_prompt.md")
	if err := os.WriteFile(stylePath, []byte(styleAnchorBody), 0o644); err != nil {
		t.Fatalf("写 style_profile.md 失败: %v", err)
	}
	if err := os.WriteFile(promptPath, []byte(promptBody), 0o644); err != nil {
		t.Fatalf("写 system_prompt.md 失败: %v", err)
	}

	// ---- 1. 读接口必须带出 editable=false + kind=style ----
	style := readSkillFile(t, adm, slug, "style_profile.md")
	if style.Editable {
		t.Errorf("style_profile.md 的 editable 应为 false（前端据此渲染只读视图），实际 true")
	}
	if style.Kind != "style" {
		t.Errorf("style_profile.md 的 kind 应为 style，实际 %q", style.Kind)
	}
	if style.Content != styleAnchorBody {
		t.Errorf("读回的 style_profile.md 内容与写盘内容不一致:\n got %q\nwant %q", style.Content, styleAnchorBody)
	}

	// ---- 2. 反向：可编辑文件不能被一起锁掉 ----
	prompt := readSkillFile(t, adm, slug, "system_prompt.md")
	if !prompt.Editable {
		t.Errorf("system_prompt.md 必须可编辑（否则前端整个编辑器都没保存入口），实际 editable=false")
	}
	if prompt.Kind != "prompt" {
		t.Errorf("system_prompt.md 的 kind 应为 prompt，实际 %q", prompt.Kind)
	}

	// ---- 3. 写接口必须 400 拒绝，且磁盘内容一字不动 ----
	code, body := putSkillFile(t, adm, slug, "style_profile.md", "被篡改的风格锚点\n")
	if code != http.StatusBadRequest {
		t.Errorf("PUT style_profile.md 应被 400 拒绝，实际 %d（body=%s）", code, body)
	}
	if !strings.Contains(body, "只读") {
		t.Errorf("拒绝理由应说明「只读」，实际 body=%s", body)
	}
	onDisk, err := os.ReadFile(stylePath)
	if err != nil {
		t.Fatalf("回读 style_profile.md 失败: %v", err)
	}
	if string(onDisk) != styleAnchorBody {
		t.Errorf("被拒的写入竟然落了盘:\n got %q\nwant %q", string(onDisk), styleAnchorBody)
	}

	// ---- 4. 反向：可编辑文件的写入链路没被误伤 ----
	const newPrompt = "# 系统提示词 v2\n\n只输出最终稿。\n"
	code, body = putSkillFile(t, adm, slug, "system_prompt.md", newPrompt)
	if code != http.StatusOK {
		t.Errorf("PUT system_prompt.md 应成功，实际 %d（body=%s）", code, body)
	}
	onDisk, err = os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("回读 system_prompt.md 失败: %v", err)
	}
	if string(onDisk) != newPrompt {
		t.Errorf("system_prompt.md 未按新内容落盘:\n got %q\nwant %q", string(onDisk), newPrompt)
	}
}

func readSkillFile(t *testing.T, adm *Admin, slug, rel string) model.SkillFile {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		"/api/admin/skills/"+slug+"/file?path="+rel, nil)
	req.SetPathValue("slug", slug)
	rec := httptest.NewRecorder()
	adm.ReadSkillFile(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("读 %s 失败: %d %s", rel, rec.Code, rec.Body.String())
	}
	var out model.SkillFile
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析 %s 的响应失败: %v（body=%s）", rel, err, rec.Body.String())
	}
	return out
}

func putSkillFile(t *testing.T, adm *Admin, slug, rel, content string) (int, string) {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"path": rel, "content": content})
	if err != nil {
		t.Fatalf("构造请求体失败: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut,
		"/api/admin/skills/"+slug+"/file", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("slug", slug)
	rec := httptest.NewRecorder()
	adm.WriteSkillFile(rec, req)
	return rec.Code, rec.Body.String()
}
