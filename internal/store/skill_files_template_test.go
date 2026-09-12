package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lizhemin15/skillforge/internal/model"
)

// 顶层模板文件（kind=templatefile）的回归防线。
//
// 真实 bug（用户视角）：技能「采购合同」目录里明明躺着 `采购合同模板.docx`，
// Agent 也确实在用它填表（fill_template），但管理端文件树上一个模板都看不到 ——
// 因为 ListFiles 只列白名单 md + examples/ + source/，顶层 .docx 落进 fileKind 的
// "other" 分支，既不显示也不可下载。结果：用户换模板只能 SSH 进服务器，
// 「技能文件」这套管理界面看起来压根没有模板这个概念。
//
// 这层断言要守住四件事：
//  1. 顶层模板**必须在**清单里，且 kind=templatefile / editable=false（二进制不可文本编辑）；
//  2. 不属于技能知识包的内部文件（meta.json、versions/ 历史快照）**必须不在**清单里 ——
//     否则就是把「扫一遍顶层目录」写成了「泄露一切」；
//  3. 清单里被标成模板的文件，必须正好是 fill_template 能填的那些（两套视图不能各活一半）；
//  4. 模板要能下载（ReadFile 走二进制分支）、能替换（同名重传覆盖）、能删（回退到无模板）。

const fakeDocx = "PK\x03\x04 fake docx bytes" // 扩展名判定够用，不需要真 OOXML 头

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录失败 %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件失败 %s: %v", path, err)
	}
}

func kindsOf(files []string) map[string]bool {
	m := map[string]bool{}
	for _, f := range files {
		m[f] = true
	}
	return m
}

func TestListFilesIncludesTopLevelTemplates(t *testing.T) {
	s := newStoreForTest(t, t.TempDir())
	const slug = "采购合同"
	dir := filepath.Join(s.SkillsDir(), slug)

	mustWrite(t, filepath.Join(dir, "system_prompt.md"), "你是合同助手")
	mustWrite(t, filepath.Join(dir, "template.md"), "## 甲方\n")
	mustWrite(t, filepath.Join(dir, "采购合同模板.docx"), fakeDocx)
	mustWrite(t, filepath.Join(dir, "source", "参考件.docx"), fakeDocx)
	// 不该出现在清单里的：历史快照（versions/）与内部元数据（meta.json）
	mustWrite(t, filepath.Join(dir, "versions", "v1", "采购合同模板.docx"), fakeDocx)
	mustWrite(t, filepath.Join(dir, "meta.json"), "{}")
	// 顶层但 fill_template 填不了的格式：.pdf 不是「可填模板」，不该冒充
	mustWrite(t, filepath.Join(dir, "参考样例.pdf"), "%PDF-1.4")

	files, err := s.ListFiles(slug)
	if err != nil {
		t.Fatalf("ListFiles 出错：%v", err)
	}
	byPath := map[string]string{} // path -> kind
	editable := map[string]bool{}
	for _, f := range files {
		byPath[f.Path] = f.Kind
		editable[f.Path] = f.Editable
	}

	// 1. 顶层模板必须在，kind/editable 正确
	if k, ok := byPath["采购合同模板.docx"]; !ok {
		t.Fatalf("顶层模板没进文件清单：%v（这正是用户报的 bug）", byPath)
	} else if k != "templatefile" {
		t.Errorf("顶层模板的 kind 应为 templatefile，实际 %q", k)
	}
	if editable["采购合同模板.docx"] {
		t.Error("模板是二进制文件，editable 必须为 false（否则前端会给一个改了也存不进去的编辑框）")
	}

	// 2. 内部文件绝不能泄露进清单
	for _, banned := range []string{"meta.json", "versions/v1/采购合同模板.docx", "参考样例.pdf"} {
		if _, ok := byPath[banned]; ok {
			t.Errorf("不该出现在技能文件清单里的文件出现了：%s（清单=%v）", banned, byPath)
		}
	}

	// 3. 被标成模板的，必须正好是 fill_template 能填的（两套视图一致）
	tpls, err := s.ListTemplates()
	if err != nil {
		t.Fatalf("ListTemplates 出错：%v", err)
	}
	fillable := map[string]bool{}
	for _, tp := range tpls {
		if tp.Slug == slug {
			fillable[tp.Rel] = true
		}
	}
	for path, kind := range byPath {
		if kind != "templatefile" {
			continue
		}
		if !fillable[path] {
			t.Errorf("文件清单说 %s 是模板，但 fill_template 扫不到它（模型填不了、界面却有）", path)
		}
	}
	if !fillable["采购合同模板.docx"] {
		t.Errorf("fill_template 应该能填顶层模板，实际扫到：%v", fillable)
	}

	// 4. 能下载：ReadFile 走二进制分支（不给 content，给 mime）
	sf, err := s.ReadFile(slug, "采购合同模板.docx")
	if err != nil {
		t.Fatalf("读模板失败（下载按钮会 500）：%v", err)
	}
	if !sf.Binary || sf.Content != "" {
		t.Errorf("模板应按二进制返回：binary=%v content=%q", sf.Binary, sf.Content)
	}
	if sf.Mime == "" {
		t.Error("模板缺 mime，前端预览/下载会拿不到类型")
	}
}

func TestTemplateFileUploadReplaceDelete(t *testing.T) {
	s := newStoreForTest(t, t.TempDir())
	const slug = "采购验收单"
	dir := filepath.Join(s.SkillsDir(), slug)
	mustWrite(t, filepath.Join(dir, "system_prompt.md"), "你是验收单助手")

	// 上传（target=template 走的就是 WriteFileRaw 落到顶层）
	if err := s.WriteFileRaw(slug, "采购验收单模板.xlsx", []byte("PK\x03\x04 v1")); err != nil {
		t.Fatalf("上传顶层模板失败：%v", err)
	}
	files, _ := s.ListFiles(slug)
	if !kindsOf(pathsOf(files))["采购验收单模板.xlsx"] {
		t.Fatalf("上传后清单里还是没有模板：%v", pathsOf(files))
	}

	// 同名替换必须覆盖（用户换模板的真实动作就是同名重传）
	if err := s.WriteFileRaw(slug, "采购验收单模板.xlsx", []byte("PK\x03\x04 v2-longer")); err != nil {
		t.Fatalf("替换模板失败：%v", err)
	}
	got, err := s.ReadFileBytes(slug, "采购验收单模板.xlsx")
	if err != nil {
		t.Fatalf("读回模板失败：%v", err)
	}
	if string(got) != "PK\x03\x04 v2-longer" {
		t.Errorf("替换没生效，读回 %q", string(got))
	}
	if n := countKind(files, "templatefile"); n != 1 {
		t.Errorf("替换后应仍只有 1 个模板，实际 %d 个（重传变成了新增两个模板，模型会随机挑）", n)
	}

	// 删除 → 回到「无模板」
	if err := s.DeleteFile(slug, "采购验收单模板.xlsx"); err != nil {
		t.Fatalf("删模板失败（界面上的删除按钮会报错）：%v", err)
	}
	files, _ = s.ListFiles(slug)
	if kindsOf(pathsOf(files))["采购验收单模板.xlsx"] {
		t.Error("删完还在清单里")
	}
}

// 这条守「写入口不能被当成任意文件写通道」：
// WriteFileRaw 是给二进制上传用的批量入口，早先只允许 source/。
// 现在放行顶层模板，就必须把 meta.json / system_prompt.md / versions/ 全部挡住。
func TestWriteFileRawRejectsNonTemplateTopLevel(t *testing.T) {
	s := newStoreForTest(t, t.TempDir())
	const slug = "公积金办事"
	mustWrite(t, filepath.Join(s.SkillsDir(), slug, "system_prompt.md"), "原文")

	for _, bad := range []string{
		"meta.json",
		"system_prompt.md",
		"versions/v1/x.docx",
		"参考样例.pdf", // 顶层但不是可填格式
	} {
		if err := s.WriteFileRaw(slug, bad, []byte("hacked")); err == nil {
			t.Errorf("WriteFileRaw 应拒绝写 %s，但它写了", bad)
		}
	}
	// 没被写坏
	body, _ := os.ReadFile(filepath.Join(s.SkillsDir(), slug, "system_prompt.md"))
	if string(body) != "原文" {
		t.Errorf("system_prompt.md 被覆盖了：%q", string(body))
	}
}

// 核心 md 仍然只能编辑、不能删：模板放开了删除，别把白名单一起放开。
func TestDeleteTemplateFileKeepsCoreProtected(t *testing.T) {
	s := newStoreForTest(t, t.TempDir())
	const slug = "采购合同"
	dir := filepath.Join(s.SkillsDir(), slug)
	mustWrite(t, filepath.Join(dir, "system_prompt.md"), "x")
	mustWrite(t, filepath.Join(dir, "公积金申请表.docx"), fakeDocx)

	if err := s.DeleteFile(slug, "system_prompt.md"); err == nil {
		t.Error("system_prompt.md 不可删除，但删成功了")
	}
	if err := s.DeleteFile(slug, "公积金申请表.docx"); err != nil {
		t.Errorf("模板应可删除，实际报错：%v", err)
	}
}

// 生成式技能（办公文档管家）没有模板文件：它的输出是 JSON 规格 → docgen 从零生成，
// 不是填空式模板。这条把「本来就没有」和「有但没显示」区分开，免得下次又当成同一个 bug。
func TestSeededDocGenSkillHasNoTemplateFile(t *testing.T) {
	s := newStoreForTest(t, t.TempDir())
	files, err := s.ListFiles("办公文档管家")
	if err != nil {
		t.Fatalf("ListFiles 出错：%v", err)
	}
	if n := countKind(files, "templatefile"); n != 0 {
		t.Errorf("办公文档管家不该有模板源文件，实际 %d 个", n)
	}
	if len(files) == 0 {
		t.Fatal("连 system_prompt.md 都没列出来，说明是扫描坏了而不是真的没模板")
	}
}

func pathsOf(files []model.SkillFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

func countKind(files []model.SkillFile, kind string) int {
	n := 0
	for _, f := range files {
		if f.Kind == kind {
			n++
		}
	}
	return n
}
