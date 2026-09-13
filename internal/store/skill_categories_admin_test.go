package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 「分类结构可增删改」的回归防线（管理端新增 / 改名 / 删除分类）。
//
// 这一层要守的不是「函数跑通」，而是**一个分类名同时是 6 个落点的 key** 这件事：
//
//	① categories/NN-<名字>.md 文件名段   ② 文件里的 H1（运行时显示名）
//	③ categories/_index.md 路由表（人读） ④ system_prompt.md 路由表（模型读）
//	⑤ examples/<名字>/ 范文目录          ⑥ meta.json 的 manual.categories
//
// 只改一处 = 运行时路由到一个不存在的类**而且不报错**（ReadCategoryDocs 读文件、
// ReadCategoryExamples 找不到目录返回空，模型照样把文章编出来）——本项目最忌的
// 「静默错到底」。所以断言的是「6 处一起变」+「运行时的读回结果一致」。
//
// 另一半是**禁裸子串**：分类名里互相含前缀是常态（「新闻通稿」⊂「公司新闻通稿」），
// 裸 ReplaceAll 会把「公司新闻通稿」改成「公司时政要闻」，一次改名毁掉另一类。
// 下面的 TestRewriteAnchoredRefs_NoPrefixBleed / TestRenameCategory_ 系列都钉这一点。

const catAdminSlug = "公司通稿技能"

// catSystemPrompt 有 4 种「新闻通稿」出现形态，正好覆盖 4 条锚定规则：
// 表格路由行（命中）、正文散文（不动）、散文里的分类名（不动）、散文引用路径（动）。
func catSystemPrompt() string {
	return "# 公司通稿技能\n\n" +
		"你是公司通稿写作助手。\n\n" +
		"## 分类路由\n\n" +
		"先判断需求落到哪一类，再按该类的写作要求与参考范文写作。\n\n" +
		"| 分类 | 触发场景 |\n" +
		"| --- | --- |\n" +
		"| 新闻通稿 | 会议、活动类短稿 |\n" +
		"| 公司新闻通稿 | 公司经营、业绩、财报类稿件 |\n\n" +
		"## 其它注意事项\n\n" +
		"正文里不要出现「新闻通稿」这四个字。\n"
}

func catIndex() string {
	return "# 分类索引\n\n" +
		"| 分类 | 触发场景 |\n" +
		"| --- | --- |\n" +
		"| 新闻通稿 | 会议、活动类短稿 |\n" +
		"| 公司新闻通稿 | 公司经营、业绩、财报类稿件 |\n"
}

func catFile1() string {
	return "# 新闻通稿\n\n" +
		"## 触发场景\n\n" +
		"会议、活动类短稿。\n\n" +
		"## 写作要求\n\n" +
		"- 标题不超过 20 字。\n" +
		"- 不要与「公司新闻通稿」混用——那是另一类，业绩稿不适用本类要求。\n" +
		"- 本类范文在（examples/新闻通稿）。\n\n" +
		"## 参考范文\n\n" +
		"- examples/新闻通稿/01.md\n" +
		"- examples/新闻通稿/02.md\n"
}

func catFile2() string {
	return "# 公司新闻通稿\n\n" +
		"## 触发场景\n\n" +
		"公司经营、业绩类稿件。\n\n" +
		"## 参考范文\n\n" +
		"- examples/公司新闻通稿/01.md\n" +
		"- 另见 categories/02-公司新闻通稿.md\n"
}

func catReviewer() string {
	return "# 审稿清单\n\n" +
		"### 新闻通稿\n\n" +
		"- 标题不超过 20 字。\n\n" +
		"### 公司新闻通稿\n\n" +
		"- 业绩数据必须可核。\n"
}

// 散文里同时出现两个分类名：要求「机器只改锚定结构，不动人写的散文」，
// 这条断言就是钉这个的（改了反而是越权，用户看到自己的需求说明被机器改字会炸）。
func catRequirement() string {
	return "# 训练需求\n\n" +
		"要能写「新闻通稿」与公司新闻通稿两类稿件。\n"
}

const catMetaJSON = `{
  "slug": "cat-admin-test",
  "manual": {
    "categories": ["新闻通稿", "公司新闻通稿"],
    "source_bytes": 12345678901234567890
  },
  "note": "新闻通稿是手册第一章"
}`

// catAdminFixture 造一个「手册模式」技能：2 个分类（其中一个名字是另一个的前缀）、
// 3 篇范文、两张路由表、reviewer 小节、meta 清单。
func catAdminFixture(t *testing.T) (*SkillStore, string) {
	t.Helper()
	s := newStoreForTest(t, t.TempDir())
	dir := filepath.Join(s.SkillsDir(), catAdminSlug)
	mustWrite(t, filepath.Join(dir, "system_prompt.md"), catSystemPrompt())
	mustWrite(t, filepath.Join(dir, "requirement.md"), catRequirement())
	mustWrite(t, filepath.Join(dir, "reviewer.md"), catReviewer())
	mustWrite(t, filepath.Join(dir, "meta.json"), catMetaJSON)
	mustWrite(t, filepath.Join(dir, "categories", "_index.md"), catIndex())
	mustWrite(t, filepath.Join(dir, "categories", "01-新闻通稿.md"), catFile1())
	mustWrite(t, filepath.Join(dir, "categories", "02-公司新闻通稿.md"), catFile2())
	mustWrite(t, filepath.Join(dir, "examples", "新闻通稿", "01.md"), "第一篇会议通稿原文")
	mustWrite(t, filepath.Join(dir, "examples", "新闻通稿", "02.md"), "第二篇会议通稿原文")
	mustWrite(t, filepath.Join(dir, "examples", "公司新闻通稿", "01.md"), "公司业绩通稿原文")
	return s, catAdminSlug
}

func readSkillFile(t *testing.T, s *SkillStore, slug, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.SkillsDir(), slug, rel))
	if err != nil {
		t.Fatalf("读 %s 失败：%v", rel, err)
	}
	return string(b)
}

func existsSkillPath(t *testing.T, s *SkillStore, slug, rel string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(s.SkillsDir(), slug, rel))
	return err == nil
}

func mustNotContain(t *testing.T, what, hay, needle string) {
	t.Helper()
	if strings.Contains(hay, needle) {
		t.Fatalf("%s 不该出现 %q，实际内容：\n%s", what, needle, hay)
	}
}

func mustContain(t *testing.T, what, hay, needle string) {
	t.Helper()
	if !strings.Contains(hay, needle) {
		t.Fatalf("%s 缺少 %q，实际内容：\n%s", what, needle, hay)
	}
}

// ----- 纯函数层 -----

// 前缀误命中：改名「新闻通稿」绝不能碰「公司新闻通稿」。
// 这是整个 B 路线最容易出的错，先单测钉死再谈落盘。
func TestRewriteAnchoredRefs_NoPrefixBleed(t *testing.T) {
	in := "# 新闻通稿\n\n" +
		"## 参考范文\n\n" +
		"- examples/新闻通稿/01.md\n\n" +
		"# 公司新闻通稿\n\n" +
		"| 分类 | 触发场景 |\n" +
		"| --- | --- |\n" +
		"| 新闻通稿 | 会议 |\n" +
		"| 公司新闻通稿 | 业绩 |\n" +
		"- examples/公司新闻通稿/01.md\n" +
		"- 另见 categories/02-公司新闻通稿.md\n" +
		"正文里不要出现「新闻通稿」这四个字。\n"

	out, hits := rewriteAnchoredRefs(in, []string{"新闻通稿"}, []string{"新闻通稿"}, "时政要闻", "时政要闻")

	if hits != 3 {
		t.Fatalf("命中数应为 3（H1 + examples/新闻通稿/01.md + 表格行「| 新闻通稿 | 会议 |」），实际 %d：\n%s", hits, out)
	}
	// 该改的：3 处
	mustContain(t, "输出", out, "# 时政要闻\n")
	mustContain(t, "输出", out, "- examples/时政要闻/01.md")
	mustContain(t, "输出", out, "| 时政要闻 | 会议 |")
	// 不该改的：公司新闻通稿的 H1 / 路径 / 表格行 / categories 路径
	mustContain(t, "输出", out, "# 公司新闻通稿\n")
	mustContain(t, "输出", out, "- examples/公司新闻通稿/01.md")
	mustContain(t, "输出", out, "| 公司新闻通稿 | 业绩 |")
	mustContain(t, "输出", out, "- 另见 categories/02-公司新闻通稿.md")
	// 散文不动
	mustContain(t, "输出", out, "正文里不要出现「新闻通稿」这四个字。")
	// 反向自证：任何「公司时政要闻」都是裸子串替换的产物
	mustNotContain(t, "输出", out, "公司时政要闻")
}

// 路径段必须整段相等：`新闻通稿2/`、`新闻通稿-old/` 这类不该被当成「新闻通稿」。
// 段后收尾字符（引号/括号/顿号/行尾）都要能识别，否则改名后路径变半个词。
func TestRewritePathRefs_ExactSegmentOnly(t *testing.T) {
	cases := []struct {
		in   string
		want string
		hits int
	}{
		{"- `examples/新闻通稿/01.md`", "- `examples/时政要闻/01.md`", 1},
		{"见 examples/新闻通稿/01.md。", "见 examples/时政要闻/01.md。", 1},
		{"（examples/新闻通稿）", "（examples/时政要闻）", 1},
		{"| examples/新闻通稿/01.md |", "| examples/时政要闻/01.md |", 1},
		{"examples/新闻通稿", "examples/时政要闻", 1},
		// 段名不相等：一律不动
		{"- examples/新闻通稿2/01.md", "- examples/新闻通稿2/01.md", 0},
		{"- examples/公司新闻通稿/01.md", "- examples/公司新闻通稿/01.md", 0},
		{"- examples/经营新闻通稿/01.md", "- examples/经营新闻通稿/01.md", 0},
	}
	for _, c := range cases {
		got, hits := rewritePathRefs(c.in, []string{"新闻通稿"}, "时政要闻")
		if got != c.want || hits != c.hits {
			t.Fatalf("rewritePathRefs(%q) = (%q, %d)，期望 (%q, %d)", c.in, got, hits, c.want, c.hits)
		}
	}
}

// 只有「表头第一格是分类」的那张表算路由表：文件里还有别的表格时，
// 同名单元格不能被删/被追加分类行。
func TestRouteTable_OnlyTheRouteTable(t *testing.T) {
	doc := "# 技能\n\n" +
		"| 分类 | 触发场景 |\n" +
		"| --- | --- |\n" +
		"| 新闻通稿 | 会议 |\n" +
		"| 公司新闻通稿 | 业绩 |\n\n" +
		"## 历史记录\n\n" +
		"| 分类 | 时间 |\n" +
		"| --- | --- |\n" +
		"| 新闻通稿 | 2025-01-01 |\n"

	out, hits := dropRouteRow(doc, []string{"新闻通稿"})
	if hits != 1 {
		t.Fatalf("dropRouteRow 命中数应为 1，实际 %d：\n%s", hits, out)
	}
	mustNotContain(t, "路由表已删行", out, "| 新闻通稿 | 会议 |")
	mustContain(t, "该留的行", out, "| 新闻通稿 | 2025-01-01 |")
	mustContain(t, "该留的行", out, "| 公司新闻通稿 | 业绩 |")

	appended, ok := appendRouteRow(doc, "| 时政要闻 | 会议 |")
	if !ok {
		t.Fatal("appendRouteRow 应能定位到路由表")
	}
	// 新行必须落在路由表里（公司新闻通稿之后），不能跑到「历史记录」表里
	idxNew := strings.Index(appended, "| 时政要闻 | 会议 |")
	idxLast := strings.Index(appended, "| 公司新闻通稿 | 业绩 |")
	idxHist := strings.Index(appended, "## 历史记录")
	if !(idxLast < idxNew && idxNew < idxHist) {
		t.Fatalf("新分类行位置不对（last=%d new=%d hist=%d）：\n%s", idxLast, idxNew, idxHist, appended)
	}
}

// 删分类时 reviewer.md 里该分类的整段检查项要一起走：
// 留着旧要求会让审稿人拿已经不存在的分类去挑刺。
func TestDropSection_WholeSectionOnly(t *testing.T) {
	out, hits := dropSection(catReviewer(), []string{"新闻通稿"}, 3)
	if hits != 1 {
		t.Fatalf("dropSection 命中数应为 1，实际 %d：\n%s", hits, out)
	}
	mustNotContain(t, "reviewer", out, "### 新闻通稿")
	mustNotContain(t, "reviewer", out, "标题不超过 20 字")
	mustContain(t, "reviewer", out, "### 公司新闻通稿")
	mustContain(t, "reviewer", out, "- 业绩数据必须可核。")
}

// 加范文必须同时改「参考范文」清单：只落文件不改清单，运行时靠回退目录才碰巧看到，
// 而用户看到的是「清单里没有、我是不是没传上去」。
func TestUpsertRefLine(t *testing.T) {
	// 占位行「（暂无，需人工补充）」要被第一篇范文替换掉，而不是并存
	skel := buildCategorySkeleton("新类", "触发", "要求")
	if !strings.Contains(skel, "（暂无，需人工补充）") {
		t.Fatalf("新分类骨架应带占位行：\n%s", skel)
	}
	out, ok := upsertRefLine(skel, "examples/新类/01.md")
	if !ok {
		t.Fatal("应写入清单")
	}
	mustContain(t, "清单", out, "- examples/新类/01.md")
	mustNotContain(t, "清单", out, "（暂无，需人工补充）")

	// 重复写入不产生第二行
	out2, ok := upsertRefLine(out, "examples/新类/01.md")
	if ok {
		t.Fatalf("重复路径不该再写：\n%s", out2)
	}

	// 没有「参考范文」小节时补小节，且不改写既有正文
	bare := "# 类\n\n## 写作要求\n\n- 别啰嗦。\n"
	out3, ok := upsertRefLine(bare, "examples/类/01.md")
	if !ok {
		t.Fatal("缺小节时应补小节")
	}
	mustContain(t, "补小节", out3, "- 别啰嗦。")
	mustContain(t, "补小节", out3, "## 参考范文")

	// 多个已有条目：新条目要接在最后一条列表项之后，不能插到小节标题与首条之间
	multi := "# 类\n\n## 参考范文\n\n- examples/类/01.md\n- examples/类/02.md\n\n## 其它\n\n文字\n"
	out4, _ := upsertRefLine(multi, "examples/类/03.md")
	i1 := strings.Index(out4, "examples/类/01.md")
	i2 := strings.Index(out4, "examples/类/02.md")
	i3 := strings.Index(out4, "examples/类/03.md")
	iOther := strings.Index(out4, "## 其它")
	if !(i1 < i2 && i2 < i3 && i3 < iOther) {
		t.Fatalf("新条目位置不对：\n%s", out4)
	}
}

func TestValidateCategoryName(t *testing.T) {
	bad := []string{"", "   ", "_index", "a/b", `a\b`, "a:b", "a*b", `a"b`, "a<b", "a|b", "带\n换行"}
	for _, s := range bad {
		if err := validateCategoryName(s); err == nil {
			t.Fatalf("分类名 %q 应被拒绝", s)
		}
	}
	long := strings.Repeat("字", 41)
	if err := validateCategoryName(long); err == nil {
		t.Fatal("超过 40 字的分类名应被拒绝")
	}
	for _, s := range []string{"新闻通稿", "公司新闻通稿", "公文 写作", "A类-2026"} {
		if err := validateCategoryName(s); err != nil {
			t.Fatalf("分类名 %q 应被接受，实际报错：%v", s, err)
		}
	}
}

// SafeCatFileName 是 store 与 skillgen 共用的清洗规则（skillgen.mdCell/safeCatFileName
// 已改为委托这里）。两处各留一份实现迟早分叉，分叉的代价是训练期建的文件名与
// 管理端建的文件名形态不同，catDirName 反推出的类别名对不上。
func TestSafeCatFileName(t *testing.T) {
	cases := map[string]string{
		"新闻通稿":  "新闻通稿",
		"a/b":   "a_b",
		" a. ":  "a",
		"":      "未命名",
		"x\ty":  "x_y",
		"公文|写作": "公文_写作",
	}
	for in, want := range cases {
		if got := SafeCatFileName(in); got != want {
			t.Fatalf("SafeCatFileName(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// ----- store 层：6 个落点一起变 -----

func TestCreateCategory_FileDirAndBothRouteTables(t *testing.T) {
	s, slug := catAdminFixture(t)

	ch, err := s.CreateCategory(slug, "会议纪要", "部门例会的纪要稿", "- 时间、地点、参会人齐全。")
	if err != nil {
		t.Fatalf("新增分类失败：%v", err)
	}
	if ch.CategoryFile != "categories/03-会议纪要.md" {
		t.Fatalf("新分类文件路径应为 03 号（接在 02 之后），实际 %s", ch.CategoryFile)
	}
	if !existsSkillPath(t, s, slug, ch.CategoryFile) {
		t.Fatalf("分类文件没落盘：%s", ch.CategoryFile)
	}
	if !existsSkillPath(t, s, slug, "examples/会议纪要") {
		t.Fatal("范文目录应预先建好（否则用户传范文时才知道没地方放）")
	}
	body := readSkillFile(t, s, slug, ch.CategoryFile)
	mustContain(t, "新分类文件", body, "# 会议纪要")
	mustContain(t, "新分类文件", body, "## 触发场景")
	mustContain(t, "新分类文件", body, "## 写作要求")
	mustContain(t, "新分类文件", body, "## 参考范文")

	// 两张路由表都要有（缺 system_prompt 的那份＝技能被拷走独立运行时模型判不出这一类）
	for _, rel := range []string{"categories/_index.md", "system_prompt.md"} {
		doc := readSkillFile(t, s, slug, rel)
		mustContain(t, rel, doc, "| 会议纪要 | 部门例会的纪要稿 |")
		mustContain(t, rel, doc, "| 新闻通稿 | 会议、活动类短稿 |")
		mustContain(t, rel, doc, "| 公司新闻通稿 | 公司经营、业绩、财报类稿件 |")
	}
	if len(ch.Warnings) != 0 {
		t.Fatalf("正常新增不该有 warning：%v", ch.Warnings)
	}

	// 运行时要能读回这一类（管理端能建、运行时认不出＝没用）
	docs, err := s.ReadCategoryDocs(slug)
	if err != nil {
		t.Fatalf("读分类失败：%v", err)
	}
	found := false
	for _, d := range docs {
		if d.Name == "会议纪要" {
			found = true
			if strings.TrimSpace(d.Trigger) == "" || strings.TrimSpace(d.Requirement) == "" {
				t.Fatalf("运行时解析出的分类缺触发场景/写作要求：%+v", d)
			}
		}
	}
	if !found {
		t.Fatal("运行时读不到新分类")
	}

	// 重名必须拦（否则两个同类文件，路由表两行，运行时按名字取到谁不确定）
	if _, err := s.CreateCategory(slug, "新闻通稿", "x", "y"); err == nil {
		t.Fatal("已存在的分类名应被拒绝")
	}
	if _, err := s.CreateCategory(slug, "新闻通稿 ", "x", "y"); err == nil {
		t.Fatal("首尾空格不该绕过重名检查")
	}
}

func TestRenameCategory_CascadesToAllSixPlaces(t *testing.T) {
	s, slug := catAdminFixture(t)
	const oldFile = "categories/01-新闻通稿.md"

	ch, err := s.RenameCategory(slug, oldFile, "时政要闻")
	if err != nil {
		t.Fatalf("改名失败：%v", err)
	}
	if ch.CategoryFile != "categories/01-时政要闻.md" {
		t.Fatalf("应保留原序号 01，实际 %s", ch.CategoryFile)
	}
	if ch.FileMovedFrom != oldFile {
		t.Fatalf("FileMovedFrom 应为 %s，实际 %s", oldFile, ch.FileMovedFrom)
	}
	if ch.ExampleDir != "examples/时政要闻/" || ch.ExampleCount != 2 {
		t.Fatalf("范文目录信息不对：dir=%q count=%d", ch.ExampleDir, ch.ExampleCount)
	}

	// ① 分类文件：新路径在、旧路径不在，H1 与范文路径都换了
	if !existsSkillPath(t, s, slug, "categories/01-时政要闻.md") {
		t.Fatal("新分类文件没落盘")
	}
	if existsSkillPath(t, s, slug, oldFile) {
		t.Fatal("旧分类文件还在（会同时存在两个同类）")
	}
	c1 := readSkillFile(t, s, slug, "categories/01-时政要闻.md")
	mustContain(t, "分类文件", c1, "# 时政要闻\n")
	mustContain(t, "分类文件", c1, "- examples/时政要闻/01.md")
	mustContain(t, "分类文件", c1, "- examples/时政要闻/02.md")
	mustNotContain(t, "分类文件", c1, "examples/新闻通稿/")
	// 分类文件本体的散文行：提到另一类时**一个字都不许动**（裸子串替换会把
	// 「公司新闻通稿」改成「公司时政要闻」，一次改名毁掉另一类）
	mustContain(t, "分类文件", c1, "不要与「公司新闻通稿」混用")
	mustNotContain(t, "分类文件", c1, "公司时政要闻")
	// 全角括号里的路径引用（无尾斜杠）：收尾符是「）」，必须也认出来
	mustContain(t, "分类文件", c1, "（examples/时政要闻）")

	// ⑤ 范文目录整体改名，且**不改范文原文**（用户硬约束：范文逐字保真）
	if !existsSkillPath(t, s, slug, "examples/时政要闻/01.md") {
		t.Fatal("范文没跟着搬到新目录")
	}
	if existsSkillPath(t, s, slug, "examples/新闻通稿/01.md") {
		t.Fatal("旧范文目录还在")
	}
	if got := readSkillFile(t, s, slug, "examples/时政要闻/01.md"); got != "第一篇会议通稿原文" {
		t.Fatalf("范文原文被改写了：%q", got)
	}

	// ③④ 两张路由表
	for _, rel := range []string{"categories/_index.md", "system_prompt.md"} {
		doc := readSkillFile(t, s, slug, rel)
		mustContain(t, rel, doc, "| 时政要闻 | 会议、活动类短稿 |")
		mustNotContain(t, rel, doc, "| 新闻通稿 |")
		mustContain(t, rel, doc, "| 公司新闻通稿 | 公司经营、业绩、财报类稿件 |")
		mustNotContain(t, rel, doc, "公司时政要闻")
	}

	// ⑥ meta.json 清单
	meta := readSkillFile(t, s, slug, "meta.json")
	var m map[string]any
	if err := json.Unmarshal([]byte(meta), &m); err != nil {
		t.Fatalf("meta.json 坏了：%v\n%s", err, meta)
	}
	man, _ := m["manual"].(map[string]any)
	arr, _ := man["categories"].([]any)
	if len(arr) != 2 || arr[0] != "时政要闻" || arr[1] != "公司新闻通稿" {
		t.Fatalf("meta 清单不对：%v", arr)
	}
	// 大整数不能被 float64 往返改写（UseNumber 的意义）
	mustContain(t, "meta.json", meta, "12345678901234567890")
	mustContain(t, "meta.json", meta, "新闻通稿是手册第一章")

	// 前缀误命中：另一个同类（公司新闻通稿）的文件/目录/meta 项一字未动
	if !existsSkillPath(t, s, slug, "categories/02-公司新闻通稿.md") {
		t.Fatal("公司新闻通稿的分类文件被误改/误删")
	}
	if !existsSkillPath(t, s, slug, "examples/公司新闻通稿/01.md") {
		t.Fatal("公司新闻通稿的范文目录被误改")
	}
	c2 := readSkillFile(t, s, slug, "categories/02-公司新闻通稿.md")
	mustContain(t, "另一分类", c2, "# 公司新闻通稿\n")
	mustContain(t, "另一分类", c2, "- examples/公司新闻通稿/01.md")
	mustContain(t, "另一分类", c2, "- 另见 categories/02-公司新闻通稿.md")

	// 散文（requirement.md）不许被机器改字——那是人写的需求说明
	req := readSkillFile(t, s, slug, "requirement.md")
	mustContain(t, "requirement.md", req, "要能写「新闻通稿」与公司新闻通稿两类稿件。")

	// 运行时读回：显示名、触发场景、范文清单、范文内容必须都跟着走
	docs, err := s.ReadCategoryDocs(slug)
	if err != nil {
		t.Fatalf("读分类失败：%v", err)
	}
	var target *CategoryDoc
	for i := range docs {
		if docs[i].Name == "时政要闻" {
			target = &docs[i]
		}
		if docs[i].Name == "新闻通稿" {
			t.Fatalf("运行时仍能读到旧分类名：%+v", docs[i])
		}
	}
	if target == nil {
		t.Fatalf("运行时读不到改名后的分类，读到的是：%+v", docs)
	}
	if target.File != "categories/01-时政要闻.md" {
		t.Fatalf("运行时读到的分类文件路径不对：%s", target.File)
	}
	if len(target.Refs) != 2 || !strings.HasPrefix(target.Refs[0], "examples/时政要闻/") {
		t.Fatalf("运行时读到的范文清单没跟着改名：%v", target.Refs)
	}
	examples, err := s.ReadCategoryExamples(slug, *target)
	if err != nil {
		t.Fatalf("读范文失败：%v", err)
	}
	if len(examples) != 2 {
		t.Fatalf("改名后应仍能取到 2 篇范文，实际 %d：%+v", len(examples), examples)
	}
	if examples[0].Content != "第一篇会议通稿原文" {
		t.Fatalf("范文内容不对：%q", examples[0].Content)
	}

	// reviewer.md 的小节标题也要跟着改（否则审稿人拿旧分类名去找人在的类）
	rev := readSkillFile(t, s, slug, "reviewer.md")
	mustContain(t, "reviewer.md", rev, "### 时政要闻")
	mustContain(t, "reviewer.md", rev, "### 公司新闻通稿")

	// 改名成已有分类要拦
	if _, err := s.RenameCategory(slug, target.File, "公司新闻通稿"); err == nil {
		t.Fatal("改成已存在的分类名应被拒绝")
	}
	if _, err := s.RenameCategory(slug, "categories/99-不存在.md", "随便"); err == nil {
		t.Fatal("改不存在的分类应报错")
	}
}

// 只改 H1（文件名段清洗后不变）时：文件原地改，不该产生「新文件+旧文件都在」。
func TestRenameCategory_DisplayNameOnlyRename(t *testing.T) {
	s, slug := catAdminFixture(t)
	// 「新闻通稿 」清洗后与原名相同 → newRel == cur.File
	ch, err := s.RenameCategory(slug, "categories/01-新闻通稿.md", "新闻通稿")
	if err != nil {
		t.Fatalf("同名改名应被容忍（H1 可能本来就不一致）：%v", err)
	}
	if ch.CategoryFile != "categories/01-新闻通稿.md" {
		t.Fatalf("同安全名时应原地改，实际 %s", ch.CategoryFile)
	}
	if len(ch.FilesDeleted) != 0 {
		t.Fatalf("原地改不该删文件：%v", ch.FilesDeleted)
	}
}

func TestDeleteCategory_NeedsForceAndCascades(t *testing.T) {
	s, slug := catAdminFixture(t)
	const file = "categories/01-新闻通稿.md"

	// 分类下有范文：不 force 必须拒绝，并把「有几篇」告诉前端（二次确认要说清后果）
	ch, err := s.DeleteCategory(slug, file, false)
	if err == nil {
		t.Fatal("有范文时不 force 应被拒绝")
	}
	if !strings.Contains(err.Error(), ErrCategoryInUse.Error()) {
		t.Fatalf("错误应可被识别为「分类在用」：%v", err)
	}
	if ch == nil || ch.ExampleCount != 2 {
		t.Fatalf("拒绝时也要返回待删篇数供前端确认，实际：%+v", ch)
	}
	// 拒绝时不许动任何文件
	if !existsSkillPath(t, s, slug, file) || !existsSkillPath(t, s, slug, "examples/新闻通稿/01.md") {
		t.Fatal("被拒绝的删除不该动文件")
	}

	ch, err = s.DeleteCategory(slug, file, true)
	if err != nil {
		t.Fatalf("force 删除失败：%v", err)
	}
	if existsSkillPath(t, s, slug, file) {
		t.Fatal("分类文件还在")
	}
	if existsSkillPath(t, s, slug, "examples/新闻通稿") {
		t.Fatal("范文目录还在")
	}
	// 路由表：删掉本类行，留下另一类行
	for _, rel := range []string{"categories/_index.md", "system_prompt.md"} {
		doc := readSkillFile(t, s, slug, rel)
		mustNotContain(t, rel, doc, "| 新闻通稿 |")
		mustContain(t, rel, doc, "| 公司新闻通稿 | 公司经营、业绩、财报类稿件 |")
	}
	// reviewer 小节整段删除
	rev := readSkillFile(t, s, slug, "reviewer.md")
	mustNotContain(t, "reviewer.md", rev, "### 新闻通稿")
	mustContain(t, "reviewer.md", rev, "### 公司新闻通稿")
	// meta 清单
	meta := readSkillFile(t, s, slug, "meta.json")
	mustNotContain(t, "meta.json", meta, `"新闻通稿"`)
	mustContain(t, "meta.json", meta, `"公司新闻通稿"`)
	// 运行时读不到被删的类，但另一类完好
	docs, err := s.ReadCategoryDocs(slug)
	if err != nil {
		t.Fatalf("读分类失败：%v", err)
	}
	if len(docs) != 1 || docs[0].Name != "公司新闻通稿" {
		t.Fatalf("删除后应只剩公司新闻通稿，实际 %+v", docs)
	}
	_ = ch
}

// 没有范文的空分类：不 force 也能删（不能让用户为「空的」写一次二次确认）。
func TestDeleteCategory_EmptyNeedsNoForce(t *testing.T) {
	s, slug := catAdminFixture(t)
	if err := os.RemoveAll(filepath.Join(s.SkillsDir(), slug, "examples", "新闻通稿")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteCategory(slug, "categories/01-新闻通稿.md", false); err != nil {
		t.Fatalf("空分类应可直接删：%v", err)
	}
}

// 非手册模式技能（没有 categories/ 目录）：新增/改名/删除都要明确报错，
// 不能偷偷建出一个半套结构（只有 categories/ 没有 system_prompt 路由表＝运行时判不出类）。
func TestCategoryAdmin_NonManualSkillRejected(t *testing.T) {
	s := newStoreForTest(t, t.TempDir())
	const slug = "普通技能"
	mustWrite(t, filepath.Join(s.SkillsDir(), slug, "system_prompt.md"), "你是普通写作助手")
	if _, err := s.CreateCategory(slug, "新类", "t", "r"); err == nil {
		t.Fatal("非手册模式技能不该能新增分类")
	} else if !strings.Contains(err.Error(), ErrNotManualSkill.Error()) {
		t.Fatalf("错误应说明不是手册模式：%v", err)
	}
	if _, err := s.RenameCategory(slug, "categories/01-x.md", "y"); err == nil {
		t.Fatal("非手册模式技能不该能改名")
	}
	if _, err := s.DeleteCategory(slug, "categories/01-x.md", true); err == nil {
		t.Fatal("非手册模式技能不该能删分类")
	}
}

// 加范文：文件按序号递增落盘 + 清单同步；空内容拒绝。
func TestAddCategoryExample(t *testing.T) {
	s, slug := catAdminFixture(t)
	rel, err := s.AddCategoryExample(slug, "categories/01-新闻通稿.md", "第三篇原文")
	if err != nil {
		t.Fatalf("加范文失败：%v", err)
	}
	if rel != "examples/新闻通稿/03.md" {
		t.Fatalf("序号应接在 02 之后，实际 %s", rel)
	}
	if got := readSkillFile(t, s, slug, rel); got != "第三篇原文" {
		t.Fatalf("范文内容不对：%q", got)
	}
	doc := readSkillFile(t, s, slug, "categories/01-新闻通稿.md")
	mustContain(t, "清单", doc, "- examples/新闻通稿/03.md")

	docs, _ := s.ReadCategoryDocs(slug)
	for _, d := range docs {
		if d.Name == "新闻通稿" {
			ex, err := s.ReadCategoryExamples(slug, d)
			if err != nil {
				t.Fatalf("读范文失败：%v", err)
			}
			if len(ex) != 3 {
				t.Fatalf("运行时应取到 3 篇，实际 %d", len(ex))
			}
		}
	}

	if _, err := s.AddCategoryExample(slug, "categories/01-新闻通稿.md", "   "); err == nil {
		t.Fatal("空范文应被拒绝")
	}
}

// ===== 自证组：证明「锚定实现」确实比朴素写法强，而不是靠断言放宽换来的绿 =====
//
// 这三个用例把「朴素的错写法」写在测试里当场跑一遍，看它真的会坏。这样做的意思是：
// 我们不是只在注释里声称「裸子串会误伤」，而是让 CI 每次都亲眼看见它会误伤。
// 断言方向：朴素写法 → 必须坏；锚定实现 → 必须不坏。二者同时成立才算数。

// naiveReplaceAll 是错的写法（B 路线要防的就是它）：全量字符串替换，不看锚定。
func naiveReplaceAll(content string, names, dirs []string, newName, newDir string) string {
	out := content
	for _, n := range names {
		out = strings.ReplaceAll(out, n, newName)
	}
	for _, d := range dirs {
		out = strings.ReplaceAll(out, d, newDir)
	}
	return out
}

func TestGuard_NaiveSubstringReplaceAllWouldBleed(t *testing.T) {
	const in = "不要与「公司新闻通稿」混用——那是另一类。\n"

	// ① 朴素写法确实会误伤
	bleed := naiveReplaceAll(in, []string{"新闻通稿"}, []string{"新闻通稿"}, "时政要闻", "时政要闻")
	mustContain(t, "朴素写法", bleed, "公司时政要闻")
	if bleed == in {
		t.Fatal("朴素写法居然没坏 —— 说明这个自证用例本身写错了（喂给它的输入必须含前缀重叠）")
	}

	// ② 锚定实现不许碰散文
	anchored, hits := rewriteAnchoredRefs(in, []string{"新闻通稿"}, []string{"新闻通稿"}, "时政要闻", "时政要闻")
	if hits != 0 {
		t.Fatalf("散文行不该被改（命中数应为 0），实际 %d：\n%s", hits, anchored)
	}
	if anchored != in {
		t.Fatalf("散文行被改写了：\n%q", anchored)
	}
}

// naivePathSeg 是错的路径段扫描：用 ASCII 收尾符黑名单。
// 中文正文里的收尾符是全角的（）。、，），黑名单漏一个，整段就会连着标点一起读进来。
func naivePathSeg(line string, start int) string {
	end := start
	for end < len(line) && !strings.ContainsRune(" \t\"'`)|],;>", rune(line[end])) {
		end++
	}
	return line[start:end]
}

func TestGuard_NaiveASCIIBlacklistWouldMissFullWidthParen(t *testing.T) {
	const in = "本类范文在（examples/新闻通稿）。"
	i := strings.Index(in, "examples/") + len("examples/")
	if got := naivePathSeg(in, i); got == "新闻通稿" {
		t.Fatal("朴素扫描居然读对了 —— 自证用例失效（输入里的收尾符必须是非 ASCII 的全角括号）")
	} else if !strings.Contains(got, "）") && !strings.Contains(got, "。") {
		t.Fatalf("朴素扫描读出的段既不干净也没粘标点，用例写得不对：%q", got)
	}

	out, hits := rewritePathRefs(in, []string{"新闻通稿"}, "时政要闻")
	if hits != 1 {
		t.Fatalf("全角括号里的路径引用必须被认出来（命中应为 1），实际 %d：%q", hits, out)
	}
	if out != "本类范文在（examples/时政要闻）。" {
		t.Fatalf("全角括号应原样保留、只有目录名被换：%q", out)
	}
}

// TestGuard_NaiveWholeLineRewriteWouldTouchProse 守「锚定规则②：正文散文不动」——
// 朴素的「行里出现分类名就整行替换」会把散文改成机器腔，人写的需求说明就废了。
func TestGuard_NaiveWholeLineRewriteWouldTouchProse(t *testing.T) {
	const in = "- 不要与「公司新闻通稿」混用——那是另一类，业绩稿不适用本类要求。"

	if !strings.Contains(in, "新闻通稿") {
		t.Fatal("自证输入必须含分类名，否则用例没有证明力")
	}
	if !strings.Contains(in, "公司新闻通稿") {
		t.Fatal("自证输入必须含前缀重叠的另一个分类名")
	}
	out, hits := rewriteAnchoredRefs(in, []string{"新闻通稿"}, []string{"新闻通稿"}, "时政要闻", "时政要闻")
	if hits != 0 || out != in {
		t.Fatalf("散文行必须原样不动（hits=%d）：\n%q", hits, out)
	}
}
