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

	"github.com/lizhemin15/skillforge/internal/store"
)

// 分类结构增删改的 HTTP 层契约。
//
// 这一层要守的**不是**「级联改得对不对」（那是 store 的用例，靠
// scripts/category_guard_inject.py 的 8 条注入自证），而是三件只在 HTTP 层才成立的事：
//
//	① 状态码要对：用户把分类名填错了，回 400 让他改；不是 500 让他去翻日志。
//	② 响应要把「动了哪些文件」带出来 —— 级联改 6 个地方，不告诉用户就是黑盒。
//	③ 非手册模式技能必须拦住：随手给普通技能加个分类，等于把它切成另一条
//	   运行时路线（写文章三段式），而它的范文/模板都是按单段做的，行为会不可复现。

const catAPISlug = "通稿写作"

// catAPIFixture 造一个「手册模式」技能：两张路由表 + 两个分类（名字前缀重叠，
// 「新闻通稿」是「公司新闻通稿」的前缀——这是整段锚定要防的经典形状）。
func catAPIFixture(t *testing.T) (*Admin, string, string) {
	t.Helper()
	dir := t.TempDir()
	st := newStoreForTest(t, dir)
	adm := &Admin{store: st}
	skillDir := filepath.Join(dir, "skills", catAPISlug)
	writeAPIFile(t, filepath.Join(skillDir, "system_prompt.md"),
		"# 分类路由\n\n| 分类 | 触发场景 |\n| --- | --- |\n"+
			"| 新闻通稿 | 会议、活动类短稿 |\n| 公司新闻通稿 | 公司经营、业绩类稿件 |\n")
	writeAPIFile(t, filepath.Join(skillDir, "categories", "_index.md"),
		"# 分类索引\n\n| 分类 | 触发场景 |\n| --- | --- |\n"+
			"| 新闻通稿 | 会议、活动类短稿 |\n| 公司新闻通稿 | 公司经营、业绩类稿件 |\n")
	writeAPIFile(t, filepath.Join(skillDir, "categories", "01-新闻通稿.md"), catAPIFile1())
	writeAPIFile(t, filepath.Join(skillDir, "categories", "02-公司新闻通稿.md"),
		"# 公司新闻通稿\n\n## 参考范文\n\n- examples/公司新闻通稿/01.md\n")
	writeAPIFile(t, filepath.Join(skillDir, "examples", "新闻通稿", "01.md"), "会议通稿原文")
	writeAPIFile(t, filepath.Join(skillDir, "examples", "公司新闻通稿", "01.md"), "业绩通稿原文")
	writeAPIFile(t, filepath.Join(skillDir, "meta.json"),
		`{"name":"通稿写作","manual":{"categories":["新闻通稿","公司新闻通稿"],"note":"新闻通稿是手册第一章"}}`)
	return adm, dir, catAPISlug
}

func catAPIFile1() string {
	return "# 新闻通稿\n\n## 触发场景\n\n会议、活动类短稿。\n\n" +
		"## 写作要求\n\n- 不要与「公司新闻通稿」混用——那是另一类。\n\n" +
		"## 参考范文\n\n- examples/新闻通稿/01.md\n"
}

func writeAPIFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录失败 %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件失败 %s: %v", path, err)
	}
}

func readAPIFile(t *testing.T, dir, slug, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "skills", slug, rel))
	if err != nil {
		t.Fatalf("读 %s 失败: %v", rel, err)
	}
	return string(b)
}

// postCategoryJSON 走真实的 handler + PathValue（不用 mux，保持用例轻量；
// 路由注册有 router 的静态检查兜底）。
func postCategoryJSON(t *testing.T, adm *Admin, method, path, slug string, body any) (int, string) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("构造请求体失败: %v", err)
		}
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("slug", slug)
	rec := httptest.NewRecorder()
	switch {
	case strings.HasSuffix(path, "/categories/rename"):
		adm.RenameSkillCategory(rec, req)
	case method == http.MethodDelete || path[strings.LastIndex(path, "/"):] == "/categories":
		if method == http.MethodDelete {
			adm.DeleteSkillCategory(rec, req)
		} else {
			adm.CreateSkillCategory(rec, req)
		}
	default:
		t.Fatalf("用例没覆盖的路径: %s %s", method, path)
	}
	return rec.Code, rec.Body.String()
}

// TestCreateCategoryAPI 新增分类：落盘 + 两张路由表都补行 + 回传落点清单。
func TestCreateCategoryAPI(t *testing.T) {
	adm, dir, slug := catAPIFixture(t)

	code, body := postCategoryJSON(t, adm, http.MethodPost,
		"/api/admin/skills/"+slug+"/categories", slug, map[string]string{
			"name":        "时政要闻",
			"trigger":     "领导活动、会议报道",
			"requirement": "- 不超过 800 字。",
		})
	if code != http.StatusOK {
		t.Fatalf("新增分类应 200，实际 %d（body=%s）", code, body)
	}

	// 分类文件落盘（带序号前缀），内容含管理员填的触发场景与要求
	catFile := findAPICatFile(t, dir, slug, "时政要闻")
	c := readAPIFile(t, dir, slug, catFile)
	for _, want := range []string{"# 时政要闻", "领导活动、会议报道", "- 不超过 800 字。"} {
		if !strings.Contains(c, want) {
			t.Errorf("分类文件缺少 %q：\n%s", want, c)
		}
	}

	// 两张路由表都要有它 —— 少一张就是「界面上有、模型看不见」
	for _, rel := range []string{"categories/_index.md", "system_prompt.md"} {
		doc := readAPIFile(t, dir, slug, rel)
		if !strings.Contains(doc, "时政要闻") {
			t.Errorf("%s 没补上「时政要闻」这一行（模型那侧会找不到这个类）：\n%s", rel, doc)
		}
	}

	// 响应要告诉用户动了哪些文件（级联是黑盒就没法排查）
	var out struct {
		OK     bool                 `json:"ok"`
		Change store.CategoryChange `json:"change"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("响应不是预期结构: %v（body=%s）", err, body)
	}
	if !out.OK || out.Change.Action != "create" || out.Change.Name != "时政要闻" {
		t.Errorf("change 回传不对: %+v", out.Change)
	}
	if len(out.Change.FilesTouched) == 0 {
		t.Errorf("FilesTouched 为空 —— 用户看不到改名/新增改了哪些文件: %+v", out.Change)
	}

	// 已有分类不许被碰
	if got := readAPIFile(t, dir, slug, "categories/02-公司新闻通稿.md"); !strings.Contains(got, "# 公司新闻通稿") {
		t.Errorf("新增分类把另一个分类改坏了:\n%s", got)
	}
}

// TestRenameCategoryAPI_CascadesAndReports 改名：级联到两张路由表 + 前缀重叠的另一类不动。
func TestRenameCategoryAPI_CascadesAndReports(t *testing.T) {
	adm, dir, slug := catAPIFixture(t)

	code, body := postCategoryJSON(t, adm, http.MethodPost,
		"/api/admin/skills/"+slug+"/categories/rename", slug, map[string]string{
			"file":     "categories/01-新闻通稿.md",
			"new_name": "时政要闻",
		})
	if code != http.StatusOK {
		t.Fatalf("改名应 200，实际 %d（body=%s）", code, body)
	}

	for _, rel := range []string{"categories/_index.md", "system_prompt.md"} {
		doc := readAPIFile(t, dir, slug, rel)
		if !strings.Contains(doc, "| 时政要闻 |") {
			t.Errorf("%s 里旧分类名没换成新名：\n%s", rel, doc)
		}
		if strings.Contains(doc, "| 新闻通稿 |") {
			t.Errorf("%s 里还留着旧分类名的路由行：\n%s", rel, doc)
		}
		// 整段锚定：前缀重叠的「公司新闻通稿」一个字都不许动
		if !strings.Contains(doc, "| 公司新闻通稿 |") {
			t.Errorf("%s 里「公司新闻通稿」被误改（前缀误命中）：\n%s", rel, doc)
		}
		if strings.Contains(doc, "公司时政要闻") {
			t.Errorf("%s 出现「公司时政要闻」——裸子串替换的典型症状：\n%s", rel, doc)
		}
	}

	// 分类文件本体：散文行提到另一类，不许被动（这是手写内容，改了就是越权）
	var moved string
	for _, f := range listAPICatFiles(t, dir, slug) {
		if strings.Contains(f, "时政要闻") {
			moved = f
		}
	}
	if moved == "" {
		t.Fatal("改名后找不到新分类文件")
	}
	c := readAPIFile(t, dir, slug, moved)
	if !strings.Contains(c, "不要与「公司新闻通稿」混用") {
		t.Errorf("散文行被机器改写了（越权）：\n%s", c)
	}
	if strings.Contains(c, "examples/新闻通稿/") {
		t.Errorf("分类文件里的范文路径没跟着改名：\n%s", c)
	}
	// 范文原文（手册原文）逐字保真
	if got := readAPIFile(t, dir, slug, "examples/时政要闻/01.md"); got != "会议通稿原文" {
		t.Errorf("范文原文被改写了：%q", got)
	}
	if got := readAPIFile(t, dir, slug, "examples/公司新闻通稿/01.md"); got != "业绩通稿原文" {
		t.Errorf("另一类的范文原文被改写了：%q", got)
	}
}

// TestCategoryAPI_BadInputIs400Not500 把「用户能自己修」和「服务端故障」分开。
//
// 真实症状（用户视角）：分类名填了个 `/`，界面弹「服务器错误，请稍后重试」，
// 用户以为是系统坏了，反复重试；实际只是名字非法。所以这里逐个钉住状态码。
func TestCategoryAPI_BadInputIs400Not500(t *testing.T) {
	adm, _, slug := catAPIFixture(t)

	cases := []struct {
		name   string
		path   string
		method string
		body   any
		want   string // 响应里必须出现的、能指导用户改的字样
	}{
		{
			name: "新增：名字带斜杠", path: "/api/admin/skills/" + slug + "/categories", method: http.MethodPost,
			body: map[string]string{"name": "新闻/通稿"}, want: "不能包含",
		},
		{
			name: "新增：名字为空", path: "/api/admin/skills/" + slug + "/categories", method: http.MethodPost,
			body: map[string]string{"name": "  "}, want: "不能为空",
		},
		{
			name: "新增：与已有分类重名", path: "/api/admin/skills/" + slug + "/categories", method: http.MethodPost,
			body: map[string]string{"name": "公司新闻通稿"}, want: "已存在同名分类",
		},
		{
			name: "改名：改成已有分类名", path: "/api/admin/skills/" + slug + "/categories/rename", method: http.MethodPost,
			body: map[string]string{"file": "categories/01-新闻通稿.md", "new_name": "公司新闻通稿"},
			want: "已存在同名分类",
		},
		{
			name: "改名：分类文件不存在", path: "/api/admin/skills/" + slug + "/categories/rename", method: http.MethodPost,
			body: map[string]string{"file": "categories/99-没有这个.md", "new_name": "新名字"},
			want: "未找到分类文件",
		},
		{
			name: "改名：越界的 file（想改到技能目录外）", path: "/api/admin/skills/" + slug + "/categories/rename", method: http.MethodPost,
			body: map[string]string{"file": "system_prompt.md", "new_name": "新名字"},
			want: "categories/",
		},
		{
			name: "删除：不是 categories/ 下的路径", path: "/api/admin/skills/" + slug + "/categories?file=meta.json",
			method: http.MethodDelete, want: "categories/",
		},
		{
			name: "删除：有范文但没给 force", path: "/api/admin/skills/" + slug + "/categories?file=categories/01-新闻通稿.md",
			method: http.MethodDelete, want: "还有范文",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := postCategoryJSON(t, adm, c.method, c.path, slug, c.body)
			if code != http.StatusBadRequest {
				t.Errorf("应回 400（用户能自己改），实际 %d（body=%s）", code, body)
			}
			if !strings.Contains(body, c.want) {
				t.Errorf("响应里应出现 %q 指导用户怎么改，实际 body=%s", c.want, body)
			}
		})
	}
}

// TestDeleteCategoryAPI_Cascade 删除：force 才准删，且两张路由表 + meta 清单一起清。
func TestDeleteCategoryAPI_Cascade(t *testing.T) {
	adm, dir, slug := catAPIFixture(t)

	code, body := postCategoryJSON(t, adm, http.MethodDelete,
		"/api/admin/skills/"+slug+"/categories?file=categories/01-新闻通稿.md&force=1", slug, nil)
	if code != http.StatusOK {
		t.Fatalf("带 force 删除应 200，实际 %d（body=%s）", code, body)
	}

	if _, err := os.Stat(filepath.Join(dir, "skills", slug, "categories", "01-新闻通稿.md")); !os.IsNotExist(err) {
		t.Errorf("分类文件没被删掉（err=%v）", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "skills", slug, "examples", "新闻通稿")); !os.IsNotExist(err) {
		t.Errorf("范文目录没被删掉（err=%v）", err)
	}

	// 路由表：旧分类的行必须清干净，另一个分类的行必须留着
	for _, rel := range []string{"categories/_index.md", "system_prompt.md"} {
		doc := readAPIFile(t, dir, slug, rel)
		if strings.Contains(doc, "| 新闻通稿 |") {
			t.Errorf("%s 里还留着已删分类的路由行（死链）：\n%s", rel, doc)
		}
		if !strings.Contains(doc, "| 公司新闻通稿 |") {
			t.Errorf("%s 里「公司新闻通稿」被误删（前缀误命中）：\n%s", rel, doc)
		}
	}

	// meta.json 清单（运行时会读它做「有哪些类」的判断）
	meta := readAPIFile(t, dir, slug, "meta.json")
	if strings.Contains(meta, `"新闻通稿"`) {
		t.Errorf("meta.json 清单里还留着已删分类：\n%s", meta)
	}
	if !strings.Contains(meta, "公司新闻通稿") {
		t.Errorf("meta.json 把另一个分类也删了：\n%s", meta)
	}

	// 另一个分类的文件与范文必须完好
	if got := readAPIFile(t, dir, slug, "categories/02-公司新闻通稿.md"); !strings.Contains(got, "# 公司新闻通稿") {
		t.Errorf("另一个分类文件被删坏了:\n%s", got)
	}
	if got := readAPIFile(t, dir, slug, "examples/公司新闻通稿/01.md"); got != "业绩通稿原文" {
		t.Errorf("另一个分类的范文被删坏了: %q", got)
	}
}

// TestDeleteCategoryAPI_EmptyNeedsNoForce 空分类（没范文）不用 force：
// 让管理员为了删个空壳还得点两次二次确认，是把安全做成麻烦。
func TestDeleteCategoryAPI_EmptyNeedsNoForce(t *testing.T) {
	adm, dir, slug := catAPIFixture(t)

	// 造一个没有范文的分类
	code, body := postCategoryJSON(t, adm, http.MethodPost,
		"/api/admin/skills/"+slug+"/categories", slug, map[string]string{"name": "临时分类"})
	if code != http.StatusOK {
		t.Fatalf("造空分类失败：%d %s", code, body)
	}
	rel := findAPICatFile(t, dir, slug, "临时分类")

	code, body = postCategoryJSON(t, adm, http.MethodDelete,
		"/api/admin/skills/"+slug+"/categories?file="+rel, slug, nil)
	if code != http.StatusOK {
		t.Fatalf("删空分类不该要求 force，实际 %d（body=%s）", code, body)
	}
	if _, err := os.Stat(filepath.Join(dir, "skills", slug, rel)); !os.IsNotExist(err) {
		t.Errorf("空分类文件没被删掉（err=%v）", err)
	}
}

// TestCategoryAPI_NonManualSkillRejected 普通技能（没有 categories/）不许加分类。
//
// 加一个分类会把技能推进「写文章三段式」路线，而它的范文/模板都是按单段准备的，
// 行为不可复现。要变赛道请走训练流程（重新拿手册训练），不是随手一点。
func TestCategoryAPI_NonManualSkillRejected(t *testing.T) {
	dir := t.TempDir()
	st := newStoreForTest(t, dir)
	adm := &Admin{store: st}
	const slug = "普通技能"
	writeAPIFile(t, filepath.Join(dir, "skills", slug, "system_prompt.md"), "# 普通\n")

	code, body := postCategoryJSON(t, adm, http.MethodPost,
		"/api/admin/skills/"+slug+"/categories", slug, map[string]string{"name": "新分类"})
	if code != http.StatusBadRequest {
		t.Fatalf("非手册技能加分类应 400，实际 %d（body=%s）", code, body)
	}
	if !strings.Contains(body, "手册") {
		t.Errorf("拒绝理由应说明「不是手册模式」，实际 body=%s", body)
	}
	if _, err := os.Stat(filepath.Join(dir, "skills", slug, "categories")); !os.IsNotExist(err) {
		t.Errorf("被拒的新增竟然建了 categories/ 目录（err=%v）", err)
	}
}

// findAPICatFile 在 categories/ 下按显示名找分类文件（序号前缀由后端分配，
// 用例不该假设序号一定等于 3）。
func findAPICatFile(t *testing.T, dir, slug, name string) string {
	t.Helper()
	for _, f := range listAPICatFiles(t, dir, slug) {
		if strings.Contains(f, name) {
			return f
		}
	}
	t.Fatalf("categories/ 下找不到分类「%s」，现有：%v", name, listAPICatFiles(t, dir, slug))
	return ""
}

func listAPICatFiles(t *testing.T, dir, slug string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "skills", slug, "categories"))
	if err != nil {
		t.Fatalf("读 categories/ 失败: %v", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		out = append(out, "categories/"+e.Name())
	}
	return out
}
