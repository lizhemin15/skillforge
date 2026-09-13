package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// 左树分类结构操作入口的**数据契约**测试。
//
// 背景（用户视角的报障）：「左侧分类各不一样，用户也不能编辑」——分类文件内容
// 能编辑，但结构（新增 / 改名 / 删除）在界面上**没有任何入口**，用户只能 SSH 去
// 改磁盘。要给出入口，前端必须先知道两件事，而这两件事后端不说、前端就只能猜：
//
//	① 这个技能是不是「手册模式」（有 categories/ 目录）。
//	   猜的代价是实打实的：非手册技能上摆一个「+ 新增分类」，点下去后端只能回
//	   「该技能不是手册模式」——等于承诺一个只会报错的操作（仓库里
//	   delSkillFile 的注释已经为同一类问题吃过一次教训）。
//	② 哪几行是真分类文件。
//	   categories/_index.md 是路由表的**人读版本**，不是分类：它没有名字、没有
//	   范文目录，改名/删除给它都是无意义操作（后端 findCategory 也不认它）。
//	   靠前端按路径前缀猜（`categories/` 且不以 `_` 开头）能凑合，但那是把
//	   store.categoryFileOf 的规则抄了第二份，两处一旦漂移就是「界面上能点、
//	   后端 400」。所以身份由后端给：category_name 有值 = 这一行是个真分类。
//
// 分类名必须是**解析出来的展示名**（H1 优先），不能是「文件名去前缀」这种
// 猜法：手册抽出来的分类文件是 `03-领导讲话.md`、H1 是「领导讲话稿」，
// 用户看到和点改名的应该是「领导讲话稿」。
//
// 断言全部锚在「真分类有名字 / 非分类没有名字」这种**会因实现写错而变红**的
// 差异上：只断 `category_name != ""` 这类形状断言，改坏了照样绿。

// listFilesForTest 走真实 handler（含 PathValue），返回原始 JSON。
func listFilesForTest(t *testing.T, adm *Admin, slug string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/skills/"+slug+"/files", nil)
	req.SetPathValue("slug", slug)
	rec := httptest.NewRecorder()
	adm.ListSkillFiles(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("列文件应 200，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v（body=%s）", err, rec.Body.String())
	}
	return out
}

// fileByName 在 files 数组里按相对路径取一条。
func fileByName(t *testing.T, resp map[string]any, path string) map[string]any {
	t.Helper()
	arr, _ := resp["files"].([]any)
	for _, it := range arr {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if m["path"] == path {
			return m
		}
	}
	t.Fatalf("响应里没有 %s 这一条（files=%v）", path, resp["files"])
	return nil
}

// TestSkillFilesExposesCategoryStructureContract 手册模式技能：manual_mode=true，
// 真分类带 category_name，路由表 _index.md 不带。
func TestSkillFilesExposesCategoryStructureContract(t *testing.T) {
	adm, dir, slug := catAPIFixture(t)
	// 追加一个「文件名段 ≠ H1」的分类：区分「解析出展示名」与「文件名去前缀」。
	writeAPIFile(t, filepath.Join(dir, "skills", slug, "categories", "03-领导讲话.md"),
		"# 领导讲话稿\n\n## 触发场景\n\n大会讲话、致辞。\n")

	resp := listFilesForTest(t, adm, slug)

	if mm, ok := resp["manual_mode"].(bool); !ok || !mm {
		t.Errorf("手册模式技能要回 manual_mode=true，实际 %v —— 前端没有这个信号就只能"+
			"给非手册技能也摆上「新增分类」按钮，点下去必报错", resp["manual_mode"])
	}

	// 真分类：category_name = H1（不是文件名段）
	cases := map[string]string{
		"categories/01-新闻通稿.md":   "新闻通稿",
		"categories/02-公司新闻通稿.md": "公司新闻通稿",
		"categories/03-领导讲话.md":   "领导讲话稿", // H1 与文件名段不同，必须是 H1
	}
	for path, want := range cases {
		f := fileByName(t, resp, path)
		got, _ := f["category_name"].(string)
		if got != want {
			t.Errorf("%s 的 category_name 应为 %q，实际 %q（要 H1 优先的展示名，不是文件名去前缀）",
				path, want, got)
		}
	}

	// 反例：路由表不是分类，不许给名字（给了前端就会给它摆改名/删除）
	idx := fileByName(t, resp, "categories/_index.md")
	if got, _ := idx["category_name"].(string); got != "" {
		t.Errorf("categories/_index.md 不该带 category_name，实际 %q —— 它是路由表的人读版本，"+
			"改名/删除对它是无意义操作，后端也会拒", got)
	}

	// 反例：非分类文件一律不带
	for _, path := range []string{"system_prompt.md", "examples/新闻通稿/01.md", "meta.json"} {
		if f := fileByNameOrNil(resp, path); f != nil {
			if got, _ := f["category_name"].(string); got != "" {
				t.Errorf("%s 不该带 category_name，实际 %q", path, got)
			}
		}
	}
}

// TestSkillFilesNonManualSkillHasNoCategoryRows 非手册技能：manual_mode=false。
// 这是「不摆只会报错的按钮」的依据。
func TestSkillFilesNonManualSkillHasNoCategoryRows(t *testing.T) {
	dir := t.TempDir()
	st := newStoreForTest(t, dir)
	adm := &Admin{store: st}
	const slug = "单段写作"
	skillDir := filepath.Join(dir, "skills", slug)
	writeAPIFile(t, filepath.Join(skillDir, "system_prompt.md"), "# 提示词\n")
	writeAPIFile(t, filepath.Join(skillDir, "examples", "01.md"), "范文")

	resp := listFilesForTest(t, adm, slug)
	if mm, ok := resp["manual_mode"].(bool); !ok || mm {
		t.Errorf("非手册技能要回 manual_mode=false，实际 %v", resp["manual_mode"])
	}
	arr, _ := resp["files"].([]any)
	for _, it := range arr {
		if m, ok := it.(map[string]any); ok {
			if got, _ := m["category_name"].(string); got != "" {
				t.Errorf("非手册技能不该出现带 category_name 的行：%v", m)
			}
		}
	}
}

// TestDeleteCategoryInUseCarriesMachineReadableFlag 有范文时删除必须显式 force，
// 且响应要带**机器可读**的 need_force / example_count。
//
// 为什么不能靠前端匹配错误文案：文案是给人看的，会改；前端一旦匹配不上，
// 用户点「删除」得到的就是一句「删除失败」，永远走不到「确认再删」那一步。
func TestDeleteCategoryInUseCarriesMachineReadableFlag(t *testing.T) {
	adm, _, slug := catAPIFixture(t)

	// 01-新闻通稿 下面挂着 1 篇范文
	code, body := postCategoryJSON(t, adm, http.MethodDelete,
		"/api/admin/skills/"+slug+"/categories?file=categories/01-新闻通稿.md", slug, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("有范文且未 force 应 400，实际 %d（body=%s）", code, body)
	}
	var out struct {
		Error        string `json:"error"`
		NeedForce    bool   `json:"need_force"`
		ExampleCount int    `json:"example_count"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("响应不是预期结构: %v（body=%s）", err, body)
	}
	if !out.NeedForce {
		t.Errorf("响应缺少 need_force:true（body=%s）—— 前端只能靠它决定弹第二次确认", body)
	}
	if out.ExampleCount != 1 {
		t.Errorf("example_count 应为 1（该分类下 1 篇范文），实际 %d（body=%s）", out.ExampleCount, body)
	}
	if out.Error == "" {
		t.Errorf("need_force 之外仍要有人话错误消息（body=%s）", body)
	}
}

func fileByNameOrNil(resp map[string]any, path string) map[string]any {
	arr, _ := resp["files"].([]any)
	for _, it := range arr {
		if m, ok := it.(map[string]any); ok && m["path"] == path {
			return m
		}
	}
	return nil
}
