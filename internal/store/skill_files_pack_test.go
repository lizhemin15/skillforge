package store

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/lizhemin15/skillforge/internal/model"
)

// 「技能文件类型」三兄弟（category / reviewer / fidelity）+ 范文子目录的回归防线。
//
// 真实 bug（用户视角）：技能编辑界面左侧每个分组显示什么，取决于技能目录里
// 恰巧存在哪些文件；而且 examples/ 只扫一级目录，examples/经营业绩/01.md 这种
// 按类别分文件夹的范文一个都列不出来。用户看到的「左侧分类各不一样、也不可编辑」
// 正是这两件事叠在一起。新增「分类要求」这种文件类型，就是要把左侧那棵树从
// 「看出什么文件」改成「有一套稳定的、可编辑的分组」。
//
// 这层断言守六件事：
//  1. categories/ 下的 *.md（含 _index.md）必须是 kind=category、Editable=true；
//  2. category 段内按路径有序（不能因为 os.ReadDir 顺序而抖动）；
//  3. 段与段的先后：category 紧跟 requirement、整体在 style 之前；reviewer 在
//     source 之后；fidelity 排最后 —— 全部用**两两下标比较**，不写「下标 == N」
//     （后者多一个文件就假红）。
//  4. examples/<类别名>/*.md 必须被列出来（这是本次新增的能力，老代码必然失败）；
//  5. 向后兼容：老的扁平 examples/example01.md 仍要能列出来；
//  6. reviewer 可编辑、fidelity 只读；**不存在的可选文件不许凭空造条目**
//     （防「os.Stat 失败也照样 append」的实现）。

// packBuildFull 造一个「什么都有」的真实技能目录，覆盖全部 kind + 两种范文布局。
// 用真目录真文件而不是 mock：ListFiles 的行为全在 os.ReadDir/os.Stat 上，
// mock 掉文件系统等于什么都没测。
func packBuildFull(t *testing.T) (*SkillStore, string) {
	t.Helper()
	s := newStoreForTest(t, t.TempDir())
	const slug = "政务信息"
	dir := filepath.Join(s.SkillsDir(), slug)

	mustWrite(t, filepath.Join(dir, "system_prompt.md"), "你是政务信息写作助手")
	mustWrite(t, filepath.Join(dir, "template.md"), "## 标题\n")
	mustWrite(t, filepath.Join(dir, "requirement.md"), "训练需求：写政务信息")
	mustWrite(t, filepath.Join(dir, "style_profile.md"), "文风锚点（只读）")
	mustWrite(t, filepath.Join(dir, "reviewer.md"), "审稿标准：数据来源必须可核")
	mustWrite(t, filepath.Join(dir, "fidelity.md"), "保真度：0.92（机器产物）")
	mustWrite(t, filepath.Join(dir, "categories", "_index.md"), "分类总纲")
	mustWrite(t, filepath.Join(dir, "categories", "01-经营业绩.md"), "经营业绩写作要求")
	// 两种范文布局并存：老的扁平 + 新的按类别分目录
	mustWrite(t, filepath.Join(dir, "examples", "example01.md"), "老扁平范文")
	mustWrite(t, filepath.Join(dir, "examples", "经营业绩", "01.md"), "子目录范文")
	mustWrite(t, filepath.Join(dir, "source", "参考件.pdf"), "%PDF-1.4")
	return s, slug
}

func packPaths(files []model.SkillFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

// packFind 按路径取条目；找不到直接 Fatal（失败文案里带上完整清单，便于定位）。
func packFind(t *testing.T, files []model.SkillFile, path string) model.SkillFile {
	t.Helper()
	for _, f := range files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("清单里找不到 %s（清单=%v）", path, packPaths(files))
	return model.SkillFile{}
}

// packKindRange 返回某个 kind 段的首、末下标。段不存在直接 Fatal。
func packKindRange(t *testing.T, files []model.SkillFile, kind string) (first, last int) {
	t.Helper()
	first, last = -1, -1
	for i, f := range files {
		if f.Kind == kind {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		t.Fatalf("清单里一个 kind=%s 的文件都没有，整段分组消失了（清单=%v）", kind, packPaths(files))
	}
	return first, last
}

// 断言 1 + 2：categories/*.md 登记为 category、可编辑，且段内按路径有序。
func TestListFilesRegistersCategoryFiles(t *testing.T) {
	s, slug := packBuildFull(t)
	files, err := s.ListFiles(slug)
	if err != nil {
		t.Fatalf("ListFiles 出错：%v", err)
	}

	// 期望：段内按路径字典序（'0' < '_'，所以 01- 在 _index.md 前面）
	want := []string{"categories/01-经营业绩.md", "categories/_index.md"}
	var got []string
	for _, f := range files {
		if f.Kind != "category" {
			continue
		}
		got = append(got, f.Path)
		if !f.Editable {
			t.Errorf("分类要求 %s 必须可编辑（用户点进左树就是要改它），实际 Editable=false", f.Path)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("kind=category 的条目数不对，想要 %v，实际 %v（清单=%v）", want, got, packPaths(files))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("category 段第 %d 条应为 %s，实际 %s（段内必须按路径有序）", i, want[i], got[i])
		}
	}
	if !sort.StringsAreSorted(got) {
		t.Errorf("category 段不是按路径有序的：%v", got)
	}
}

// 断言 3：整体顺序 —— category 在 requirement 之后 / style 之前；
// reviewer 在 source 之后；fidelity 排最后。全用两两下标比较。
func TestListFilesKindOrdering(t *testing.T) {
	s, slug := packBuildFull(t)
	files, err := s.ListFiles(slug)
	if err != nil {
		t.Fatalf("ListFiles 出错：%v", err)
	}

	_, reqLast := packKindRange(t, files, "requirement")
	catFirst, catLast := packKindRange(t, files, "category")
	styleFirst, _ := packKindRange(t, files, "style")
	_, srcLast := packKindRange(t, files, "source")
	revFirst, revLast := packKindRange(t, files, "reviewer")
	fidFirst, fidLast := packKindRange(t, files, "fidelity")

	if catFirst <= reqLast {
		t.Errorf("category 段应整体排在 requirement 段之后（先看训练需求再看分类要求），实际 category 首=%d requirement 末=%d（清单=%v）",
			catFirst, reqLast, packPaths(files))
	}
	if catLast >= styleFirst {
		t.Errorf("category 段应整体排在 style 段之前，实际 category 末=%d style 首=%d（清单=%v）",
			catLast, styleFirst, packPaths(files))
	}
	if revFirst <= srcLast {
		t.Errorf("reviewer 应排在 source 之后，实际 reviewer 首=%d source 末=%d（清单=%v）",
			revFirst, srcLast, packPaths(files))
	}
	if fidFirst <= revLast {
		t.Errorf("fidelity 应排在 reviewer 之后，实际 fidelity 首=%d reviewer 末=%d（清单=%v）",
			fidFirst, revLast, packPaths(files))
	}
	if fidLast != len(files)-1 {
		t.Errorf("fidelity 段应排在整张清单的最后（机器产物掉到最底下），实际末位下标=%d，清单长度=%d（清单=%v）",
			fidLast, len(files), packPaths(files))
	}
	if fidFirst != fidLast {
		t.Errorf("fidelity.md 不应出现多份，实际下标 %d..%d（清单=%v）", fidFirst, fidLast, packPaths(files))
	}
}

// 断言 4 + 5：examples/ 要能列出子目录里的范文，同时兼容老的扁平范文。
func TestListFilesRecursesIntoExampleSubdirs(t *testing.T) {
	s, slug := packBuildFull(t)
	files, err := s.ListFiles(slug)
	if err != nil {
		t.Fatalf("ListFiles 出错：%v", err)
	}

	// 4. 新能力：examples/<类别名>/01.md
	nested := packFind(t, files, "examples/经营业绩/01.md")
	if nested.Kind != "example" {
		t.Errorf("子目录范文 examples/经营业绩/01.md 的 kind 应为 example，实际 %q", nested.Kind)
	}
	if !nested.Editable {
		t.Error("子目录范文应可编辑")
	}
	if nested.Name != "01.md" {
		t.Errorf("子目录范文的 Name 应为基名 01.md，实际 %q", nested.Name)
	}

	// 5. 向后兼容：老的扁平范文仍要在
	flat := packFind(t, files, "examples/example01.md")
	if flat.Kind != "example" {
		t.Errorf("老扁平范文 examples/example01.md 的 kind 应为 example，实际 %q", flat.Kind)
	}

	// fileKind 也要认子目录范文（ReadFile/WriteFile 走同一条判定），
	// 否则会出现「列得出来但读不出来」的半截状态。
	sf, err := s.ReadFile(slug, "examples/经营业绩/01.md")
	if err != nil {
		t.Fatalf("子目录范文列出来了但读不出来（点了会 500）：%v", err)
	}
	if sf.Kind != "example" || !sf.Editable {
		t.Errorf("子目录范文读出来的 kind/editable 不对：kind=%q editable=%v", sf.Kind, sf.Editable)
	}
}

// 断言 6：不存在的可选文件不许凭空造条目；存在时才登记，且各就各位。
func TestListFilesDoesNotInventMissingOptionalFiles(t *testing.T) {
	s := newStoreForTest(t, t.TempDir())
	const slug = "只有提示词"
	dir := filepath.Join(s.SkillsDir(), slug)
	mustWrite(t, filepath.Join(dir, "system_prompt.md"), "x")
	// 故意不建 reviewer.md / fidelity.md / categories/

	files, err := s.ListFiles(slug)
	if err != nil {
		t.Fatalf("ListFiles 出错：%v", err)
	}
	for _, f := range files {
		switch f.Path {
		case "reviewer.md", "fidelity.md":
			t.Errorf("技能目录里根本没有 %s，清单却凭空多出一条（os.Stat 失败也 append 的实现会这样）", f.Path)
		}
	}
	for _, kind := range []string{"reviewer", "fidelity", "category"} {
		if n := countKind(files, kind); n != 0 {
			t.Errorf("没有对应文件时 kind=%s 的条目应为 0，实际 %d（清单=%v）", kind, n, packPaths(files))
		}
	}

	// 只补一个 fidelity.md：它必须出现，reviewer 仍然不许出现（区分「有但没显示」和「本来就没有」）
	mustWrite(t, filepath.Join(dir, "fidelity.md"), "保真度：0.80")
	files, err = s.ListFiles(slug)
	if err != nil {
		t.Fatalf("ListFiles 出错：%v", err)
	}
	fid := packFind(t, files, "fidelity.md")
	if fid.Kind != "fidelity" {
		t.Errorf("fidelity.md 的 kind 应为 fidelity，实际 %q", fid.Kind)
	}
	if fid.Editable {
		t.Error("fidelity.md 是机器产物，Editable 必须为 false（只读，免得界面给一个存不住的编辑框）")
	}
	for _, f := range files {
		if f.Path == "reviewer.md" {
			t.Error("只建了 fidelity.md，reviewer.md 却也被凭空造了出来")
		}
	}
}

// 断言 6 的另一半：reviewer.md 存在时登记为 reviewer 且可编辑。
func TestListFilesReviewerIsEditable(t *testing.T) {
	s, slug := packBuildFull(t)
	files, err := s.ListFiles(slug)
	if err != nil {
		t.Fatalf("ListFiles 出错：%v", err)
	}
	rev := packFind(t, files, "reviewer.md")
	if rev.Kind != "reviewer" {
		t.Errorf("reviewer.md 的 kind 应为 reviewer，实际 %q", rev.Kind)
	}
	if !rev.Editable {
		t.Error("审稿标准是人工维护的，reviewer.md 必须可编辑")
	}
}
