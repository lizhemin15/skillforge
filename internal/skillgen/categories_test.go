package skillgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// masterText 是本文件 SplitByAnchors 测试共用的"原文"。
// 用带中文标点的真实句式，且每段首尾锚点都是原文中唯一出现的片段。
const masterText = "手册总则。\n" +
	"A1样例一开头，这里是第一类范文的正文。B1样例一结尾。\n" +
	"A2样例二开头，这里是第二类范文的正文。B2样例二结尾。\n" +
	"A3样例三开头，这里是第三类范文的正文。B3样例三结尾。\n"

func anchorsOf(specs ...[2]string) []CatAnchor {
	out := make([]CatAnchor, 0, len(specs))
	for _, s := range specs {
		out = append(out, CatAnchor{Start: s[0], End: s[1]})
	}
	return out
}

// TestSplitByAnchors 是 SplitByAnchors 的重头戏：不但要 err==nil，还要断
// 「片段的首尾锚点」「片段之间的真实下标关系」——因为静默切错位置比直接报错
// 危险得多，只有下标断言才能抓住"切到了别的段落"这种隐性 bug。
func TestSplitByAnchors(t *testing.T) {
	a1 := [2]string{"A1样例一开头，这里是第一类范文的正文。", "B1样例一结尾。"}
	a2 := [2]string{"A2样例二开头，这里是第二类范文的正文。", "B2样例二结尾。"}
	a3 := [2]string{"A3样例三开头，这里是第三类范文的正文。", "B3样例三结尾。"}

	t.Run("正常_两段", func(t *testing.T) {
		anchors := anchorsOf(a1, a2)
		segs, err := SplitByAnchors(masterText, anchors)
		if err != nil {
			t.Fatalf("期望无错，实际: %v", err)
		}
		if len(segs) != 2 {
			t.Fatalf("期望切出 2 段，实际 %d 段", len(segs))
		}
		for i, seg := range segs {
			if !strings.HasPrefix(seg, anchors[i].Start) {
				t.Errorf("第 %d 段未以 Start 开头:\n  段=%q\n  Start=%q", i+1, anchorPreview(seg), anchors[i].Start)
			}
			if !strings.HasSuffix(seg, anchors[i].End) {
				t.Errorf("第 %d 段未以 End 结尾:\n  段=%q\n  End=%q", i+1, anchorPreview(seg), anchors[i].End)
			}
		}
		// 逐字节等于原文的对应子串（原样、不 trim、不重写）。
		for i, a := range anchors {
			si := strings.Index(masterText, a.Start)
			ei := strings.Index(masterText, a.End)
			want := masterText[si : ei+len(a.End)]
			if segs[i] != want {
				t.Errorf("第 %d 段与原文子串不一致:\n  got =%q\n  want=%q", i+1, segs[i], want)
			}
			if got, wantLen := len(segs[i]), ei+len(a.End)-si; got != wantLen {
				t.Errorf("第 %d 段长度 = %d，期望 %d", i+1, got, wantLen)
			}
		}
		// 片段2 的 Start 下标必须 > 片段1 的 End 下标（真的按顺序、不重叠）。
		s2 := strings.Index(masterText, anchors[1].Start)
		e1 := strings.Index(masterText, anchors[0].End)
		if s2 <= e1 {
			t.Errorf("片段2 的 Start 下标(%d) 应 > 片段1 的 End 下标(%d)", s2, e1)
		}
	})

	t.Run("正常_三段", func(t *testing.T) {
		anchors := anchorsOf(a1, a2, a3)
		segs, err := SplitByAnchors(masterText, anchors)
		if err != nil {
			t.Fatalf("期望无错，实际: %v", err)
		}
		if len(segs) != 3 {
			t.Fatalf("期望切出 3 段，实际 %d 段", len(segs))
		}
		// 相邻两段：后一段 Start 下标 > 前一段 End 下标。
		for i := 1; i < len(segs); i++ {
			prevEnd := strings.Index(masterText, anchors[i-1].End)
			curStart := strings.Index(masterText, anchors[i].Start)
			if curStart <= prevEnd {
				t.Errorf("第 %d 段 Start 下标(%d) 应 > 第 %d 段 End 下标(%d)", i+1, curStart, i, prevEnd)
			}
			if !strings.HasPrefix(segs[i], anchors[i].Start) {
				t.Errorf("第 %d 段未以 Start 开头", i+1)
			}
			if !strings.HasSuffix(segs[i], anchors[i].End) {
				t.Errorf("第 %d 段未以 End 结尾", i+1)
			}
		}
		if !strings.HasSuffix(segs[2], a3[1]) {
			t.Errorf("第 3 段未以末段 End 结尾: %q", segs[2])
		}
	})

	t.Run("锚点找不到", func(t *testing.T) {
		anchors := anchorsOf(
			[2]string{"这段文字原文里根本没有出现XYZ", "B1样例一结尾。"},
		)
		_, err := SplitByAnchors(masterText, anchors)
		if err == nil {
			t.Fatal("期望报错（锚点找不到），实际无错")
		}
		// 错误信息里必须带上那个锚点片段的前若干字符，否则线上排查无从下手。
		if !strings.Contains(err.Error(), "这段文字原文里根本没有出现XYZ") {
			t.Errorf("错误信息未带上锚点片段: %v", err)
		}
		if !strings.Contains(err.Error(), "找不到") {
			t.Errorf("错误信息语义不对: %v", err)
		}
	})

	t.Run("锚点重复_不唯一", func(t *testing.T) {
		// 构造一段"同一句出现两次"的原文——比如 OCR 重复识别了页眉。
		dupText := "重复出现的句子。中间正文。重复出现的句子。后面正文。"
		anchors := anchorsOf([2]string{"重复出现的句子。", "后面正文。"})
		_, err := SplitByAnchors(dupText, anchors)
		if err == nil {
			t.Fatal("期望报错（锚点不唯一），实际无错——静默切错位置是最危险的")
		}
		if !strings.Contains(err.Error(), "重复出现的句子。") {
			t.Errorf("错误信息未带上重复锚点片段: %v", err)
		}
		if !strings.Contains(err.Error(), "2 次") {
			t.Errorf("错误信息应说明出现次数: %v", err)
		}
	})

	t.Run("顺序颠倒", func(t *testing.T) {
		// a2 排在 a1 前面：后一个锚点的 Start 出现在前一段 Start 之前。
		anchors := anchorsOf(a2, a1)
		_, err := SplitByAnchors(masterText, anchors)
		if err == nil {
			t.Fatal("期望报错（顺序颠倒），实际无错")
		}
		if !strings.Contains(err.Error(), "顺序颠倒") {
			t.Errorf("错误信息语义不对: %v", err)
		}
	})

	t.Run("相邻两段重叠", func(t *testing.T) {
		ovText := "AAAA开头。中间。BBBB结束。"
		anchors := anchorsOf(
			[2]string{"AAAA开头。", "BBBB结束。"},
			[2]string{"中间。", "BBBB结束。"}, // Start 落在前一段区间内
		)
		_, err := SplitByAnchors(ovText, anchors)
		if err == nil {
			t.Fatal("期望报错（两段重叠），实际无错")
		}
		if !strings.Contains(err.Error(), "重叠") {
			t.Errorf("错误信息语义不对: %v", err)
		}
	})

	t.Run("空锚点列表", func(t *testing.T) {
		if _, err := SplitByAnchors(masterText, nil); err == nil {
			t.Fatal("期望报错（anchors 为空），实际无错")
		}
	})

	t.Run("空Start", func(t *testing.T) {
		anchors := anchorsOf([2]string{"", "B1样例一结尾。"})
		if _, err := SplitByAnchors(masterText, anchors); err == nil {
			t.Fatal("期望报错（Start 为空串），实际无错")
		}
	})

	t.Run("空End", func(t *testing.T) {
		anchors := anchorsOf([2]string{"A1样例一开头，这里是第一类范文的正文。", ""})
		if _, err := SplitByAnchors(masterText, anchors); err == nil {
			t.Fatal("期望报错（End 为空串），实际无错")
		}
	})
}

// TestParseStructure 覆盖 LLM 常见输出噪声：代码围栏、前后废话、纯 JSON、空串。
func TestParseStructure(t *testing.T) {
	const body = `{"general":"总则内容","categories":[{"name":"经营业绩","trigger":"报道经营成果","requirement":"要求原文","anchors":[{"start":"范文首句","end":"范文末句"}]}]}`

	t.Run("合法JSON", func(t *testing.T) {
		st, err := parseStructure(body)
		if err != nil {
			t.Fatalf("期望无错，实际: %v", err)
		}
		if st.General != "总则内容" {
			t.Errorf("General = %q", st.General)
		}
		if len(st.Categories) != 1 || st.Categories[0].Name != "经营业绩" {
			t.Fatalf("categories 解析错误: %+v", st.Categories)
		}
		if len(st.Categories[0].Anchor) != 1 || st.Categories[0].Anchor[0].Start != "范文首句" {
			t.Errorf("anchor 解析错误: %+v", st.Categories[0].Anchor)
		}
	})

	t.Run("带json围栏", func(t *testing.T) {
		raw := "```json\n" + body + "\n```"
		st, err := parseStructure(raw)
		if err != nil {
			t.Fatalf("期望无错（应剥掉代码围栏），实际: %v", err)
		}
		if st.General != "总则内容" || len(st.Categories) != 1 {
			t.Fatalf("围栏解析错误: %+v", st)
		}
	})

	t.Run("带前后废话", func(t *testing.T) {
		raw := "好的，以下是抽取结果：\n" + body + "\n希望对你有帮助。"
		st, err := parseStructure(raw)
		if err != nil {
			t.Fatalf("期望无错（应剥掉前后解释文字），实际: %v", err)
		}
		if st.General != "总则内容" || len(st.Categories) != 1 {
			t.Fatalf("废话包裹解析错误: %+v", st)
		}
	})

	t.Run("空串", func(t *testing.T) {
		// 空回复 = 上游调用失败/被打断，若静默返回空结构，上层会把一次故障
		// 当成"手册没有分类"的成功抽取。因此这里判为错误。
		if _, err := parseStructure(""); err == nil {
			t.Fatal("期望报错（空串），实际无错")
		}
		if _, err := parseStructure("   \n\t "); err == nil {
			t.Fatal("期望报错（全空白），实际无错")
		}
	})

	t.Run("非法JSON", func(t *testing.T) {
		if _, err := parseStructure("这不是JSON，也没有花括号"); err == nil {
			t.Fatal("期望报错（非法 JSON），实际无错")
		}
	})
}

// TestWriteCategories 落盘测试：断言文件真的写出来了、_index.md 含全部类别与
// 触发场景、分类文件真的列出了传入的范文相对路径、序号是 01/02。
func TestWriteCategories(t *testing.T) {
	dir := t.TempDir()
	st := &Structure{
		General: "总则：写新闻稿要真实、及时。",
		Categories: []Category{
			{Name: "经营业绩", Trigger: "报道公司经营成果与业绩", Requirement: "要求甲原文"},
			{Name: "会议纪要", Trigger: "报道会议内容与决议", Requirement: "要求乙原文"},
		},
	}
	examples := map[string][]string{
		"经营业绩": {"examples/经营业绩/01.md", "examples/经营业绩/02.md"},
	}

	paths, err := WriteCategories(dir, st, examples)
	if err != nil {
		t.Fatalf("WriteCategories 报错: %v", err)
	}
	wantPaths := []string{"categories/_index.md", "categories/01-经营业绩.md", "categories/02-会议纪要.md"}
	if len(paths) != len(wantPaths) {
		t.Fatalf("返回路径数 = %d，期望 %d：%v", len(paths), len(wantPaths), paths)
	}
	for i, w := range wantPaths {
		if paths[i] != w {
			t.Errorf("返回路径[%d] = %q，期望 %q", i, paths[i], w)
		}
	}

	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("文件未写出 %s: %v", rel, err)
		}
		return string(b)
	}

	idx := read("categories/_index.md")
	for _, want := range []string{"| 分类 | 触发场景 |", "经营业绩", "报道公司经营成果与业绩", "会议纪要", "报道会议内容与决议"} {
		if !strings.Contains(idx, want) {
			t.Errorf("_index.md 缺少 %q\n---\n%s", want, idx)
		}
	}

	c1 := read("categories/01-经营业绩.md")
	for _, want := range []string{"## 触发场景", "## 写作要求", "## 参考范文", "要求甲原文",
		"- examples/经营业绩/01.md", "- examples/经营业绩/02.md"} {
		if !strings.Contains(c1, want) {
			t.Errorf("01-经营业绩.md 缺少 %q\n---\n%s", want, c1)
		}
	}
	// NN 序号必须落在文件名上（01、02），不能只是列表顺序。
	if base := filepath.Base(paths[1]); base != "01-经营业绩.md" {
		t.Errorf("序号文件名不对: %q", base)
	}
	if base := filepath.Base(paths[2]); base != "02-会议纪要.md" {
		t.Errorf("序号文件名不对: %q", base)
	}

	c2 := read("categories/02-会议纪要.md")
	if !strings.Contains(c2, "（暂无，需人工补充）") {
		t.Errorf("无范文的分类应写占位提示\n---\n%s", c2)
	}

	// 落盘内容必须是纯 Markdown：不能出现 JSON / 生成器痕迹（会被注入 prompt）。
	for _, rel := range wantPaths {
		body := read(rel)
		for _, banned := range []string{"JSON", "AI 生成", "AI生成"} {
			if strings.Contains(body, banned) {
				t.Errorf("%s 不应出现 %q\n---\n%s", rel, banned, body)
			}
		}
	}

	t.Run("空结构报错", func(t *testing.T) {
		if _, err := WriteCategories(t.TempDir(), nil, nil); err == nil {
			t.Fatal("期望报错（结构为空），实际无错")
		}
	})

	t.Run("目录不存在时自动创建", func(t *testing.T) {
		nested := filepath.Join(t.TempDir(), "a", "b")
		got, err := WriteCategories(nested, st, nil)
		if err != nil {
			t.Fatalf("应自动 MkdirAll: %v", err)
		}
		if _, err := os.Stat(filepath.Join(nested, "categories", "_index.md")); err != nil {
			t.Fatalf("嵌套目录未写出: %v (%v)", err, got)
		}
	})
}

// TestSourceText 覆盖：优先读 dir/source、跳过二进制与隐藏文件、按文件名排序。
func TestSourceText(t *testing.T) {
	t.Run("读source子目录并排序跳过二进制", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "source")
		if err := os.MkdirAll(src, 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(src, "p002.txt"), "第二页内容")
		mustWrite(t, filepath.Join(src, "p001.txt"), "第一页内容")
		mustWrite(t, filepath.Join(src, "manual.pdf"), "%PDF-1.4 二进制乱码")
		mustWrite(t, filepath.Join(src, ".DS_Store"), "junk")

		got, err := SourceText(dir)
		if err != nil {
			t.Fatalf("SourceText 报错: %v", err)
		}
		want := "第一页内容\n第二页内容"
		if got != want {
			t.Errorf("SourceText = %q，期望 %q", got, want)
		}
	})

	t.Run("直接传素材目录", func(t *testing.T) {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "b.txt"), "BBB")
		mustWrite(t, filepath.Join(dir, "a.txt"), "AAA")
		got, err := SourceText(dir)
		if err != nil {
			t.Fatalf("SourceText 报错: %v", err)
		}
		if got != "AAA\nBBB" {
			t.Errorf("SourceText = %q，期望 %q", got, "AAA\nBBB")
		}
	})

	t.Run("无素材报错", func(t *testing.T) {
		if _, err := SourceText(t.TempDir()); err == nil {
			t.Fatal("期望报错（无素材），实际无错")
		}
	})
}

// 保证 SplitByAnchors 的产物能被 SourceText 拼出的原文定位——即"切出来的
// 片段必然是原文子串"这条保真断言，在真实素材通路上也成立。
func TestSplitByAnchorsFidelity(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(src, "p001.txt"), "总则。范文首句：公司上半年营收增长显著。范文末句：特此通报。结尾。")
	text, err := SourceText(dir)
	if err != nil {
		t.Fatal(err)
	}
	anchors := anchorsOf([2]string{"范文首句：公司上半年营收增长显著。", "范文末句：特此通报。"})
	segs, err := SplitByAnchors(text, anchors)
	if err != nil {
		t.Fatalf("SplitByAnchors 报错: %v", err)
	}
	if len(segs) != 1 {
		t.Fatalf("期望 1 段，实际 %d", len(segs))
	}
	if !strings.Contains(text, segs[0]) {
		t.Fatalf("切出的片段不是原文子串（保真断言失败）: %q", segs[0])
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入 %s 失败: %v", path, err)
	}
}
