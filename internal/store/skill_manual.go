package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ===== 手册模式的运行时读取 =====
//
// 训练期把手册落成三样东西：categories/*.md（每类的触发场景 + 写作要求 + 范文链接）、
// examples/<类别>/*.md（原文切出的范文）、reviewer.md（审稿清单）。运行时要把它们
// 读回来注入 prompt，这个文件负责「读 + 解析」这一层。
//
// 解析的是 Markdown 而不是另存一份 JSON，是有意的：categories/*.md 在管理端左树里
// 是可编辑文件，管理员改完必须立刻影响运行时，否则「能编辑却不生效」就是 bug。
// 代价是解析要容错——小节标题是渲染时约定的，但人手改过之后可能缺项、改名，
// 所以这里一律「能读到多少算多少」，不因为缺一个小节就整类失败。

// CategoryDoc 是一个分类文件（categories/NN-<名称>.md）的解析结果。
type CategoryDoc struct {
	File        string   `json:"file"`        // 相对路径，如 categories/01-新闻通稿.md
	Name        string   `json:"name"`        // 分类名（首个 H1；缺失时用文件名去序号）
	Trigger     string   `json:"trigger"`     // 触发场景：什么需求该落到这一类（路由依据）
	Requirement string   `json:"requirement"` // 写作要求：注入 prompt 的硬约束
	Refs        []string `json:"refs"`        // 「参考范文」里列出的相对路径
}

// CategoryExample 是一篇范文。
type CategoryExample struct {
	Path    string `json:"path"`    // 相对路径，如 examples/新闻通稿/01.md
	Content string `json:"content"` // 原文（训练期由锚点从原文切出，未经模型改写）
}

// HasCategories 报告技能是否存在 categories/ 目录。
//
// 这是「手册模式」双开关的第二关（第一关是 skill_type=write）。约定优于配置：
// 是否按手册处理不看管理员勾选，只看目录在不在——抽不出结构时训练期压根不建这个
// 目录，运行时于是自动退回通用流程，两边判断口径天然一致。
func (s *SkillStore) HasCategories(slug string) bool {
	fi, err := os.Stat(filepath.Join(s.skillDir(slug), "categories"))
	return err == nil && fi.IsDir()
}

// categoryFileOf 判断一个 categories/ 下的文件名是否是分类文件。
// _index.md 是给人看的索引（路由表的人读版本），不参与解析：它的内容与各类
// 的 trigger 重复，读进来只会让模型看到两份可能不一致的路由表。
func categoryFileOf(name string) bool {
	return strings.HasSuffix(name, ".md") && !strings.HasPrefix(name, "_") && name != "README.md"
}

// catDirName 从分类文件名反推范文目录名。
// 训练期两侧用的是同一套清洗规则：文件叫 NN-<清洗名>.md，目录叫 examples/<清洗名>/，
// 所以这里剥掉「第一个 '-' 之前」就得到目录名。改用 strings.TrimPrefix 逐字符剥
// 序号是不行的——序号到 100 就是三位数，而分类名本身可能含 '-'（如「A-B 类」）。
func catDirName(catFile string) string {
	base := strings.TrimSuffix(filepath.Base(catFile), ".md")
	if i := strings.Index(base, "-"); i >= 0 {
		return strings.TrimSpace(base[i+1:])
	}
	return strings.TrimSpace(base)
}

// ParseCategoryMD 解析一个分类文件的 Markdown。
//
// 纯函数（不碰文件系统），便于用黄金值测试钉住行为。容错策略：
//   - 名称取首个 H1；没有 H1 就用文件名去序号（人手删了标题也不至于整类没名字）
//   - 小节按标题关键字认领：含「触发」→ Trigger，含「要求」→ Requirement，
//     含「范文」→ Refs。标题被改名成「写作要点」这类则认不到，退化为空。
//   - 未识别的小节内容一律丢弃：注入 prompt 的是「硬约束」，混进人写的备注
//     会让模型把备注也当成规则遵守。
func ParseCategoryMD(catFile, content string) CategoryDoc {
	doc := CategoryDoc{File: filepath.ToSlash(catFile), Name: catDirName(catFile)}
	var (
		section strings.Builder // 当前小节正文
		mode    string          // "" | trigger | requirement | refs
	)
	flush := func() {
		body := strings.TrimSpace(section.String())
		section.Reset()
		switch mode {
		case "trigger":
			if doc.Trigger == "" {
				doc.Trigger = body
			}
		case "requirement":
			if doc.Requirement == "" {
				doc.Requirement = body
			}
		case "refs":
			for _, line := range strings.Split(body, "\n") {
				line = strings.TrimSpace(line)
				// 只认列表项；「（暂无，需人工补充）」这类占位行天然被跳过。
				if !strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "*") {
					continue
				}
				p := strings.TrimSpace(strings.TrimLeft(line, "-*"))
				p = strings.Trim(p, "`")
				if p != "" {
					doc.Refs = append(doc.Refs, filepath.ToSlash(p))
				}
			}
		}
		mode = ""
	}
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimRight(raw, "\r")
		switch {
		case strings.HasPrefix(line, "# "):
			flush()
			if doc.Name == "" || doc.Name == catDirName(catFile) {
				doc.Name = strings.TrimSpace(strings.TrimPrefix(line, "# "))
			}
		case strings.HasPrefix(line, "## "):
			flush()
			h := strings.TrimSpace(strings.TrimPrefix(line, "## "))
			switch {
			case strings.Contains(h, "触发"):
				mode = "trigger"
			case strings.Contains(h, "要求"):
				mode = "requirement"
			case strings.Contains(h, "范文"):
				mode = "refs"
			default:
				mode = "other"
			}
		default:
			if mode != "" {
				section.WriteString(line + "\n")
			}
		}
	}
	flush()
	doc.Name = strings.TrimSpace(doc.Name)
	return doc
}

// ReadCategoryDocs 读一个技能的 categories/*.md，按文件名排序（= 手册章序）。
// categories/ 不存在时返回 (nil, nil)：调用方据此判定「不是手册模式」，不是错误。
func (s *SkillStore) ReadCategoryDocs(slug string) ([]CategoryDoc, error) {
	dir := filepath.Join(s.skillDir(slug), "categories")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, en := range entries {
		if en.IsDir() || !categoryFileOf(en.Name()) {
			continue
		}
		names = append(names, en.Name())
	}
	sort.Strings(names) // 文件名前缀是 NN- 序号，字符串序即手册章序

	out := make([]CategoryDoc, 0, len(names))
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, fmt.Errorf("读取分类文件 %s 失败: %w", n, err)
		}
		out = append(out, ParseCategoryMD(filepath.ToSlash(filepath.Join("categories", n)), string(b)))
	}
	return out, nil
}

// ReadCategoryExamples 读某一类的范文。
//
// 两条来源，优先「## 参考范文」里列的路径——那是训练期写下的权威清单，而且管理员
// 可以在左树里手改指向。清单为空（或列出的路径全都读不到）时才回退去扫
// examples/<类别>/：范文被人工挪过位置时仍能用上，而不是静默变成零篇。
//
// 所有来自文件内容的路径都过 absFile（内含 safeRel），手改出一条 ../../../etc/passwd
// 也只能落回技能目录内、读不到就跳过。
func (s *SkillStore) ReadCategoryExamples(slug string, cat CategoryDoc) ([]CategoryExample, error) {
	var out []CategoryExample
	seen := map[string]bool{}

	add := func(rel string) {
		if rel == "" || seen[rel] {
			return
		}
		if !strings.HasSuffix(strings.ToLower(rel), ".md") {
			return
		}
		abs, err := s.absFile(slug, rel)
		if err != nil {
			return // 非法路径：跳过，不让一条手改的坏路径毁掉整类范文
		}
		b, err := os.ReadFile(abs)
		if err != nil {
			return
		}
		seen[rel] = true
		out = append(out, CategoryExample{Path: filepath.ToSlash(rel), Content: string(b)})
	}

	for _, ref := range cat.Refs {
		add(ref)
	}
	if len(out) > 0 {
		return out, nil
	}

	sub := catDirName(cat.File)
	if sub == "" {
		return nil, nil
	}
	dir := filepath.Join(s.skillDir(slug), "examples", sub)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, en := range entries {
		if en.IsDir() || !strings.HasSuffix(en.Name(), ".md") {
			continue
		}
		names = append(names, en.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		add(filepath.ToSlash(filepath.Join("examples", sub, n)))
	}
	return out, nil
}

// ReadReviewer 读 reviewer.md 的审稿清单（不存在时返回空串，不是错误）。
func (s *SkillStore) ReadReviewer(slug string) (string, error) {
	b, err := os.ReadFile(filepath.Join(s.skillDir(slug), "reviewer.md"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(b), nil
}
