package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ===== 分类结构管理：新增 / 改名（级联） / 删除 =====
//
// 背景：categories/ 是训练期从写作手册里抽出来的**结构**。管理端此前能编辑分类
// 文件的内容，却动不了结构本身——想补一类、改一个写错的分类名、删掉不需要的类，
// 都没有入口。缺口的代价在「改名」上最刺眼：一个分类名同时是 6 个落点的 key：
//
//	1. categories/NN-<名字>.md        文件名段（catDirName 靠它反推类别名）
//	2. 该文件里的 H1                  运行时的显示名
//	3. categories/_index.md 的路由表   人读版路由
//	4. system_prompt.md 的路由表       模型读版路由
//	5. examples/<名字>/ 目录           范文归属
//	6. meta.json 的 manual.categories  排查用的清单
//
// 只改一处 = 运行时路由到一个不存在的类，而**不会报错**：ReadCategoryDocs 读的是
// 文件、ReadCategoryExamples 找不到目录就返回空，模型照样能编出一篇文章来。
// 这类「静默错到底」的失败正是本项目最忌讳的，所以改名必须级联，一次改全。
//
// 铁律（踩过 4 次）：分类名之间天然互为子串——「新闻通稿」⊂「公司新闻通稿」、
// 「采购合同」⊂「采购合同附件」。任何 strings.ReplaceAll 式的裸子串替换都会
// 顺手改坏邻类，且改完看不出来。本文件所有文本替换一律走**整段锚定**：
//
//	规则 1  整行只放一个名字（`# 新闻通稿` / `- 新闻通稿` / `### 新闻通稿`）
//	规则 2  表格行的第一格（路由表就是这种形状）
//	规则 3  路径段：examples/新闻通稿/01.md —— 段名必须整段相等
//	规则 4  路径段：categories/03-新闻通稿.md —— 序号前缀必须是数字
//
// 散落在正文里的「新闻通稿」四个字（用户写的说明、范文原文）**故意不动**：
// 范文原文必须逐字保真（见 constraints：范文原样切分、禁止 LLM 重写），
// 而管理员的散文里提到分类名是正常表达，改它属于越权。

// ErrCategoryBadInput 表示「用户改一下输入就能成功」的一类失败：名字非法、重名、
// 目标已存在、分类文件找不到。
//
// 为什么要专门区分：API 层要据此回 400 而不是 500 —— 400 的语义是「你的输入有问题，
// 你可以自己修」，前端直接把消息显示给用户；500 是「服务端出事了」，不该让用户
// 盯着自己的输入反复试。靠匹配错误文本判类型是会烂的（文案一改就失效），所以用错误链。
var ErrCategoryBadInput = errors.New("分类输入不合法")

// badInputError 是一条面向用户的错误消息，同时在错误链里挂着 ErrCategoryBadInput，
// 所以它既能原样显示给用户，又能被 errors.Is 认出来。
type badInputError struct{ msg string }

func (e badInputError) Error() string { return e.msg }
func (e badInputError) Unwrap() error { return ErrCategoryBadInput }

func catBadInput(format string, a ...any) error {
	return badInputError{msg: fmt.Sprintf(format, a...)}
}

// ErrCategoryInUse 表示该分类下还有范文，删除需要 force 明确确认。
var ErrCategoryInUse = errors.New("该分类下还有范文")

// ErrNotManualSkill 表示该技能不是手册模式（没有 categories/），加分类属于
// 「把技能切成另一条运行时路线」，必须由训练流程而不是随手一点来完成。
var ErrNotManualSkill = errors.New("该技能不是手册模式（没有 categories/ 目录）")

// CategoryChange 是一次分类结构变更的落点清单，直接回给前端展示
// （让用户看到「我改个名到底动了哪些文件」，否则级联就是黑盒）。
type CategoryChange struct {
	Action        string   `json:"action"` // create | rename | delete
	Name          string   `json:"name"`   // 变更后的分类名（delete 为被删的名字）
	OldName       string   `json:"old_name,omitempty"`
	CategoryFile  string   `json:"category_file"` // categories/NN-<名字>.md
	FileMovedFrom string   `json:"file_moved_from,omitempty"`
	ExampleDir    string   `json:"example_dir,omitempty"` // examples/<名字>/
	ExampleCount  int      `json:"example_count"`
	FilesTouched  []string `json:"files_touched"` // 内容被改写的文件
	FilesDeleted  []string `json:"files_deleted"`
	Warnings      []string `json:"warnings,omitempty"` // 「本该在却没在」的情况，不静默
}

// categoryEntry 是 categories/ 下一个分类文件的三件事：
// 路径、序号（手册章序）、名字（文件名段 + H1，两者可能被人工改得不一致）。
type categoryEntry struct {
	File string // categories/03-新闻通稿.md
	Seq  int    // 3（文件名没有数字前缀时为 0）
	Dir  string // 文件名段：03-新闻通稿.md -> 新闻通稿
	H1   string // 文件里的 H1（缺 H1 时等于 Dir）
}

// displayName 是给用户看的名字，优先 H1。
func (e categoryEntry) displayName() string {
	if s := strings.TrimSpace(e.H1); s != "" {
		return s
	}
	return e.Dir
}

// nameAliases 返回该分类在文本里可能出现的所有「展示名」写法。
func (e categoryEntry) nameAliases() []string {
	out := []string{}
	for _, s := range []string{e.Dir, e.H1} {
		if s = strings.TrimSpace(s); s != "" && !containsStr(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// dirAliases 返回该分类在**路径段**里可能出现的写法（清洗成文件名片段后的形态）。
// 范文路径用的是 SafeCatFileName(name)，与展示名可能不同（含 '/'、空格时）。
func (e categoryEntry) dirAliases() []string {
	out := []string{}
	for _, s := range e.nameAliases() {
		if safe := SafeCatFileName(s); safe != "" && !containsStr(out, safe) {
			out = append(out, safe)
		}
	}
	return out
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ----- 目录扫描与查找（身份一律用「分类文件相对路径」，整段相等） -----

func (s *SkillStore) listCategories(slug string) ([]categoryEntry, error) {
	dir := filepath.Join(s.skillDir(slug), "categories")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []categoryEntry
	for _, en := range entries {
		if en.IsDir() || !categoryFileOf(en.Name()) {
			continue
		}
		e := categoryEntry{File: "categories/" + en.Name(), Dir: catDirName(en.Name()), H1: ""}
		if b, err := os.ReadFile(filepath.Join(dir, en.Name())); err == nil {
			e.H1 = ParseCategoryMD(e.File, string(b)).Name
			if e.H1 == e.Dir {
				e.H1 = "" // 解析回退成文件名时不要当成第二个别名
			}
		}
		if i := strings.Index(en.Name(), "-"); i > 0 {
			if n, err := strconv.Atoi(en.Name()[:i]); err == nil {
				e.Seq = n
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out, nil
}

// findCategory 按**分类文件相对路径**精确定位一个分类。
// 不用名字模糊匹配：分类名互为子串（新闻通稿 / 公司新闻通稿），
// 模糊匹配会把 A 的改名落到 B 头上。路径整段相等是唯一不会误命中的身份。
func (s *SkillStore) findCategory(slug, file string) (categoryEntry, error) {
	file = filepath.ToSlash(strings.TrimSpace(file))
	if !strings.HasPrefix(file, "categories/") || strings.Contains(file, "..") {
		return categoryEntry{}, catBadInput("分类标识必须是 categories/ 下的文件路径，收到 %q", file)
	}
	list, err := s.listCategories(slug)
	if err != nil {
		return categoryEntry{}, err
	}
	for _, e := range list {
		if e.File == file {
			return e, nil
		}
	}
	names := make([]string, 0, len(list))
	for _, e := range list {
		names = append(names, e.File)
	}
	return categoryEntry{}, catBadInput("未找到分类文件 %s（可用：%s）", file, strings.Join(names, "、"))
}

// ----- 纯函数：整段锚定的文本改写（全部可用黄金值测试钉住） -----

// catMarkerLineRe 匹配「行首标记 + 正文」：`# 标题`、`- 列表项`、`1. 列表项`。
// 只吃行首标记，正文里出现什么都无所谓。
var catMarkerLineRe = regexp.MustCompile(`^([ \t]*(?:[#>*+\-]+|[0-9]+[.)])[ \t]*)(.*)$`)

func catNameHit(s string, names []string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	for _, n := range names {
		if n != "" && s == n {
			return true
		}
	}
	return false
}

// tableFirstCell 取出 markdown 表格行的第一格（不含竖线），非表格行返回 false。
func tableFirstCell(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "|") {
		return "", false
	}
	rest := t[1:]
	j := strings.Index(rest, "|")
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// rewriteTableFirstCell 把表格行第一格里的分类名换掉，保留原有缩进与格内空白。
func rewriteTableFirstCell(line string, names []string, newName string) (string, bool) {
	cell, ok := tableFirstCell(line)
	if !ok || !catNameHit(cell, names) {
		return line, false
	}
	lead := line[:strings.Index(line, "|")]
	rest := line[len(lead):]
	j := strings.Index(rest[1:], "|")
	old := rest[1 : 1+j]
	pre := old[:len(old)-len(strings.TrimLeft(old, " \t"))]
	post := old[len(strings.TrimRight(old, " \t")):]
	return lead + "|" + pre + newName + post + rest[1+j:], true
}

// isPathSegRune 判断某个字符能不能出现在路径段里（段＝`examples/` 之后的目录名）。
//
// 这里用**白名单**而不是「收尾字符黑名单」：路径会被写在中文正文里，
// 收尾符号是无穷的（`）`、`。`、`、`、`，`、`「`、`」`、`｜`…），黑名单列不全；
// 漏一个就把整段读成「新闻通稿）」→ 与分类名不相等 → 改名时静默漏改，
// 留下一根指向旧目录的死路径。白名单只认「名字里真的会有的字符」，
// 其余任何字符（含全角标点、空格、引号）都当段结束。
func isPathSegRune(r rune) bool {
	if r == '/' || r == '-' || r == '_' || r == '.' {
		return true
	}
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// rewritePathRefs 只改**整段相等**的路径段。
//
// 这是防「新闻通稿」改坏「公司新闻通稿」的关键一处：
// `examples/公司新闻通稿/01.md` 的段名是「公司新闻通稿」，与「新闻通稿」不相等，
// 因此原样保留；只有 `examples/新闻通稿/…` 这种整段相等的才会被换。
func rewritePathRefs(line string, dirs []string, newDir string) (string, int) {
	var b strings.Builder
	i, hits := 0, 0
	for i < len(line) {
		je := strings.Index(line[i:], "examples/")
		jc := strings.Index(line[i:], "categories/")
		pick, plen := -1, 0
		if je >= 0 && (jc < 0 || je < jc) {
			pick, plen = i+je, len("examples/")
		} else if jc >= 0 {
			pick, plen = i+jc, len("categories/")
		}
		if pick < 0 {
			b.WriteString(line[i:])
			break
		}
		segStart := pick + plen
		segEnd := segStart
		for segEnd < len(line) {
			r, sz := utf8.DecodeRuneInString(line[segEnd:])
			if !isPathSegRune(r) {
				break
			}
			segEnd += sz
		}
		b.WriteString(line[i:segStart])
		seg := line[segStart:segEnd]
		replaced := ""
		if plen == len("examples/") {
			for _, d := range dirs {
				if d == "" {
					continue
				}
				if seg == d || strings.HasPrefix(seg, d+"/") {
					replaced = newDir + strings.TrimPrefix(seg, d)
					break
				}
			}
		} else {
			replaced = rewriteCatFileSeg(seg, dirs, newDir)
		}
		if replaced != "" && replaced != seg {
			b.WriteString(replaced)
			hits++
		} else {
			b.WriteString(seg)
		}
		i = segEnd
	}
	return b.String(), hits
}

// rewriteCatFileSeg 改 `03-新闻通稿.md` 里的名字段。
// 锚定：序号前缀必须全是数字、段名必须整段相等、扩展名必须是 .md。
// 于是 `03-公司新闻通稿.md` 不会被「新闻通稿」命中。
func rewriteCatFileSeg(seg string, dirs []string, newDir string) string {
	if !strings.HasSuffix(seg, ".md") {
		return ""
	}
	stem := strings.TrimSuffix(seg, ".md")
	i := strings.Index(stem, "-")
	if i <= 0 {
		return ""
	}
	prefix := stem[:i]
	if _, err := strconv.Atoi(prefix); err != nil {
		return ""
	}
	name := stem[i+1:]
	for _, d := range dirs {
		if d != "" && name == d {
			return prefix + "-" + newDir + ".md"
		}
	}
	return ""
}

// rewriteAnchoredRefs 对一份 Markdown 做一次分类改名级联，返回（新内容, 命中数）。
// 命中数 0 是**正常结果**（该文件压根没提这个分类），不是错误。
func rewriteAnchoredRefs(content string, names, dirs []string, newName, newDir string) (string, int) {
	lines := strings.Split(content, "\n")
	hits := 0
	for i, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		crlf := len(raw) != len(line)
		out := line
		n := 0
		if m := catMarkerLineRe.FindStringSubmatch(line); m != nil {
			if catNameHit(m[2], names) {
				out = m[1] + newName
				n++
			}
		} else if catNameHit(line, names) {
			// 整行就是一个光名字（独立成行的小标题）
			out = newName
			n++
		}
		if o, ok := rewriteTableFirstCell(out, names, newName); ok {
			out, n = o, n+1
		}
		if o, k := rewritePathRefs(out, dirs, newDir); k > 0 {
			out, n = o, n+k
		}
		if n > 0 {
			if crlf {
				out += "\r"
			}
			lines[i] = out
			hits += n
		}
	}
	return strings.Join(lines, "\n"), hits
}

// headingOf 解析 markdown 标题行，返回层级与标题文本。
func headingOf(line string) (int, string, bool) {
	t := strings.TrimLeft(line, " \t")
	n := 0
	for n < len(t) && t[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return 0, "", false
	}
	rest := strings.TrimSpace(t[n:])
	if rest == "" {
		return 0, "", false
	}
	return n, rest, true
}

// routeTableRange 定位「表头第一格是『分类』」的那张表，返回表头行与表体末行下标。
// 为什么钉表头：system_prompt 里除了路由表还可能有人写的其它表格，
// 无脑往文件最后一张表里追加 = 把分类路由行插进不相干的表。
func routeTableRange(lines []string) (start, end int) {
	start, end = -1, -1
	for i, line := range lines {
		c, ok := tableFirstCell(line)
		if ok && strings.TrimSpace(c) == "分类" {
			start = i
			break
		}
	}
	if start < 0 {
		return -1, -1
	}
	end = start
	for i := start + 1; i < len(lines); i++ {
		if _, ok := tableFirstCell(lines[i]); ok {
			end = i
			continue
		}
		break
	}
	return start, end
}

// appendRouteRow 往分类路由表末尾追加一行；找不到路由表返回 false（调用方须给 warning）。
func appendRouteRow(content, row string) (string, bool) {
	lines := strings.Split(content, "\n")
	_, end := routeTableRange(lines)
	if end < 0 {
		return content, false
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:end+1]...)
	out = append(out, row)
	out = append(out, lines[end+1:]...)
	return strings.Join(out, "\n"), true
}

// dropRouteRow 从分类路由表里删掉分类名命中的行；表外同名的表格行不动。
func dropRouteRow(content string, names []string) (string, int) {
	lines := strings.Split(content, "\n")
	start, end := routeTableRange(lines)
	if start < 0 {
		return content, 0
	}
	out := make([]string, 0, len(lines))
	hits := 0
	for i, raw := range lines {
		if i >= start && i <= end {
			if c, ok := tableFirstCell(raw); ok && catNameHit(c, names) {
				hits++
				continue
			}
		}
		out = append(out, raw)
	}
	return strings.Join(out, "\n"), hits
}

// dropSection 删掉「标题恰好等于分类名」的整段（到下一个同级或更高级标题之前）。
// 用在 reviewer.md：审稿清单是按分类分小节的，分类删了还留着旧检查项，
// 运行时审稿人会拿已经不存在的分类要求去挑刺。
func dropSection(content string, names []string, minLevel int) (string, int) {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	skip, hits := 0, 0
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		if lvl, title, ok := headingOf(line); ok {
			if skip > 0 && lvl <= skip {
				skip = 0
			}
			if skip == 0 && lvl >= minLevel && catNameHit(title, names) {
				skip = lvl
				hits++
				continue
			}
		}
		if skip > 0 {
			continue
		}
		out = append(out, raw)
	}
	return strings.Join(out, "\n"), hits
}

// upsertRefLine 把一篇范文的相对路径写进分类文件的「参考范文」清单。
// 为什么必须同时改清单：运行时优先按清单取范文（ReadCategoryExamples 先读 Refs，
// 读不到才回退扫目录）。只落文件不改清单 = 运行时靠回退才碰巧看到，
// 而用户看到的是「清单里没有、我是不是没传上去」。
func upsertRefLine(content, rel string) (string, bool) {
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if rel == "" {
		return content, false
	}
	line := "- " + rel
	lines := strings.Split(content, "\n")
	head := -1
	for i, raw := range lines {
		if lvl, title, ok := headingOf(raw); ok && lvl >= 2 && strings.Contains(title, "范文") {
			head = i
			break
		}
	}
	if head < 0 {
		// 没有「参考范文」小节：补一个（分类文件是文档，末尾加小节不改写既有内容）
		body := strings.TrimRight(content, "\n")
		if body != "" {
			body += "\n\n"
		}
		return body + "## 参考范文\n" + line + "\n", true
	}
	end := len(lines)
	for i := head + 1; i < len(lines); i++ {
		if _, _, ok := headingOf(lines[i]); ok {
			end = i
			break
		}
	}
	for i := head + 1; i < end; i++ {
		if strings.TrimSpace(lines[i]) == rel || strings.TrimSpace(lines[i]) == line {
			return content, false // 已在清单里
		}
	}
	insertAt := head + 1
	for i := head + 1; i < end; i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "-") || strings.HasPrefix(t, "*") {
			insertAt = i + 1
			continue
		}
		if strings.HasPrefix(t, "（") && strings.Contains(t, "人工补充") {
			// 占位行「（暂无，需人工补充）」——第一条范文落地时把它换掉
			out := append([]string{}, lines[:i]...)
			out = append(out, line)
			out = append(out, lines[i+1:]...)
			return strings.Join(out, "\n"), true
		}
	}
	out := append([]string{}, lines[:insertAt]...)
	out = append(out, line)
	out = append(out, lines[insertAt:]...)
	return strings.Join(out, "\n"), true
}

// buildCategorySkeleton 生成新分类文件，形状与训练期 WriteCategories 完全一致
// （同样的 H1 / 触发场景 / 写作要求 / 参考范文 四段），这样运行时的
// ParseCategoryMD 能原样解析出它，不会因为是管理端手工建的类而认不出来。
func buildCategorySkeleton(name, trigger, requirement string) string {
	var b strings.Builder
	b.WriteString("# " + strings.TrimSpace(name) + "\n\n")
	b.WriteString("## 触发场景\n")
	b.WriteString(firstNonEmptyStr(strings.TrimSpace(trigger), "（待补充：写明什么样的需求应当落到这一类）") + "\n\n")
	b.WriteString("## 写作要求\n")
	b.WriteString(firstNonEmptyStr(strings.TrimSpace(requirement), "（待补充：逐条写明硬约束，运行时会被当成不可取舍的要求）") + "\n\n")
	b.WriteString("## 参考范文\n（暂无，需人工补充）\n")
	return b.String()
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// MdCellText 就是 skillgen.mdCell 的实现本体（skillgen 已改为直接调它，不再各留一份）。
// 它把文本压成能安全放进表格单元格的形式：
func MdCellText(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

// safeCatFileName 同 skillgen.safeCatFileName：清洗成安全的文件名片段（保留中文）。
// 保持逐字一致很重要——两处规则一旦分叉，管理端建出来的文件运行时反推不出类别名。
func SafeCatFileName(name string) string {
	name = strings.TrimSpace(name)
	repl := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_", "?", "_",
		"\"", "_", "<", "_", ">", "_", "|", "_", "\n", "_", "\r", "_", "\t", "_",
	)
	name = repl.Replace(name)
	name = strings.Trim(name, " .")
	if name == "" {
		name = "未命名"
	}
	return name
}

// validateCategoryName 校验管理员输入的分类名。
// 挡的是「会破坏落盘契约」的字符，不是审美：路径分隔符会被 safeCatFileName
// 换掉导致文件名与用户输入不一致（用户会以为没生效），下划线开头会撞 _index.md 的保留前缀。
func validateCategoryName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return catBadInput("分类名不能为空")
	}
	if utf8.RuneCountInString(name) > 40 {
		return catBadInput("分类名过长（最多 40 字，当前 %d 字）", utf8.RuneCountInString(name))
	}
	if strings.HasPrefix(name, "_") {
		return catBadInput(`分类名不能以下划线开头（"_index" 是索引文件的保留前缀）`)
	}
	if strings.ContainsAny(name, "/\\:*?\"<>|") {
		return catBadInput(`分类名不能包含 / \ : * ? " < > | 等字符`)
	}
	for _, r := range name {
		if r < 0x20 {
			return catBadInput("分类名不能包含换行或控制字符")
		}
	}
	return nil
}

// ----- store 层：读-改-写 与级联 -----

// cascadeFile 对一个技能内文件做「读-改-写」。文件不存在不算错（很多落点是可选的）。
// metaPath 返回技能的 meta.json 绝对路径。
//
// 不能走 s.absFile：safeRel 出于「HTTP 层不许碰系统文件」把 meta.json 挡在外面
// （接口传给它的 rel 来自请求体）。但这里是服务端自己的固定文件名、不含用户输入，
// 没有穿越风险，必须直连；否则改名的清单同步会静默失败——用户在界面上看到
// 「已改名」，meta.json 里却还留着旧名，下次重生成时又冒出旧分类。
func (s *SkillStore) metaPath(slug string) string {
	return filepath.Join(s.skillDir(slug), "meta.json")
}

func (s *SkillStore) cascadeFile(slug, rel string, fn func(string) (string, int)) (bool, int, error) {
	abs, err := s.absFile(slug, rel)
	if err != nil {
		return false, 0, err
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, 0, nil
		}
		return false, 0, err
	}
	out, hits := fn(string(b))
	if hits == 0 || out == string(b) {
		return false, hits, nil
	}
	if err := os.WriteFile(abs, []byte(out), 0o644); err != nil {
		return false, hits, fmt.Errorf("写入 %s 失败: %w", rel, err)
	}
	return true, hits, nil
}

// cascadeRenameMeta 改 meta.json 里的 manual.categories 清单。
// 用 JSON 解析而不是文本替换：清单项与字段名可能同名，文本替换会改坏字段。
func (s *SkillStore) cascadeRenameMeta(slug string, names []string, newName string) (bool, error) {
	b, err := os.ReadFile(s.metaPath(slug))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	// UseNumber：meta.json 里的数字（产物大小、字数、耗时毫秒）不能被 float64 往返改写，
	// 例如 12345678901234567890 会变成 1.2345678901234567e+19。
	// 代价是重新序列化后键序按字典序排列（Go map 语义），内容不变。
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return false, fmt.Errorf("meta.json 不是合法 JSON，跳过清单同步: %w", err)
	}
	man, _ := m["manual"].(map[string]any)
	if man == nil {
		return false, nil
	}
	arr, _ := man["categories"].([]any)
	changed := false
	for i, v := range arr {
		if sv, ok := v.(string); ok && catNameHit(sv, names) {
			arr[i] = newName
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return false, err
	}
	return true, os.WriteFile(s.metaPath(slug), out, 0o644)
}

// countExampleFiles 数 examples/<分类>/ 下的 .md 范文。
func countExampleFiles(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, en := range entries {
		if !en.IsDir() && strings.HasSuffix(en.Name(), ".md") {
			n++
		}
	}
	return n
}

// routeRow 造一行路由表。
func routeRow(name, trigger string) string {
	return "| " + MdCellText(name) + " | " +
		MdCellText(firstNonEmptyStr(strings.TrimSpace(trigger), "（待补充：写明什么样的需求落到这一类）")) + " |"
}

// addRouteRows 往 _index.md 与 system_prompt.md 两张路由表各追加一行。
//
// 两张表缺一不可：_index.md 给人看（用户在左树里点开就能核对路由），
// system_prompt.md 给模型看（技能被拷走独立运行时，只有 prompt 里的表还在）。
func (s *SkillStore) addRouteRows(slug, name, trigger string, ch *CategoryChange) {
	row := routeRow(name, trigger)

	// categories/_index.md：这是我们自己生成的索引，缺表就补表头
	touched, _, err := s.cascadeFile(slug, "categories/_index.md", func(c string) (string, int) {
		if out, ok := appendRouteRow(c, row); ok {
			return out, 1
		}
		body := strings.TrimRight(c, "\n")
		if body == "" {
			body = "# 分类索引\n\n| 分类 | 触发场景 |\n| --- | --- |"
		} else {
			body += "\n\n| 分类 | 触发场景 |\n| --- | --- |"
		}
		return body + "\n" + row + "\n", 1
	})
	if err != nil {
		ch.Warnings = append(ch.Warnings, "categories/_index.md 未更新: "+err.Error())
	} else if touched {
		ch.FilesTouched = append(ch.FilesTouched, "categories/_index.md")
	}

	// system_prompt.md：路由表是训练期追加的「写作手册模式」小节里的，找不到就明说
	touched, _, err = s.cascadeFile(slug, "system_prompt.md", func(c string) (string, int) {
		if out, ok := appendRouteRow(c, row); ok {
			return out, 1
		}
		return c, 0
	})
	if err != nil {
		ch.Warnings = append(ch.Warnings, "system_prompt.md 未更新: "+err.Error())
	} else if touched {
		ch.FilesTouched = append(ch.FilesTouched, "system_prompt.md")
	} else {
		ch.Warnings = append(ch.Warnings,
			"system_prompt.md 里没有找到分类路由表（表头应为「| 分类 | 触发场景 |」），未追加分类行——"+
				"运行时模型可能判不出这一类，请手工补一行")
	}
}

// dropRouteRows 从两张路由表里删掉该分类的行。
func (s *SkillStore) dropRouteRows(slug string, names []string, ch *CategoryChange) {
	for _, rel := range []string{"categories/_index.md", "system_prompt.md"} {
		touched, hits, err := s.cascadeFile(slug, rel, func(c string) (string, int) {
			return dropRouteRow(c, names)
		})
		if err != nil {
			ch.Warnings = append(ch.Warnings, rel+" 未更新: "+err.Error())
			continue
		}
		if touched && hits > 0 {
			ch.FilesTouched = append(ch.FilesTouched, rel)
		}
	}
}

// renameCascadeTargets 是改名时要扫一遍的文件（**不含 examples/ 与 source/**）。
//
// 排除 examples/：范文原文必须逐字保真（用户定的硬约束），
// 哪怕它正好有一行等于分类名也不许动——那是手册原文。
// 排除 source/：原始素材同理。
// fidelity.md 不入列：它是训练期一次运行的机器报告（含时间戳），属于历史证据，
// 留着旧名字反而是「当时跑的是哪一版」的正确记录。
func renameCascadeTargets(catFile string) []string {
	return []string{
		catFile,
		"categories/_index.md",
		"system_prompt.md",
		"requirement.md",
		"template.md",
		"reviewer.md",
	}
}

// ----- 对外：新增 / 改名 / 删除 / 加范文 -----

// CreateCategory 在手册模式技能下新增一个分类（文件 + 范文目录 + 两张路由表）。
func (s *SkillStore) CreateCategory(slug, name, trigger, requirement string) (*CategoryChange, error) {
	if err := validateCategoryName(name); err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if !s.HasCategories(slug) {
		return nil, fmt.Errorf("%w；分类结构由训练期从写作手册抽取生成，"+
			"技能要按分类写作请先用写作手册训练", ErrNotManualSkill)
	}
	list, err := s.listCategories(slug)
	if err != nil {
		return nil, err
	}
	safe := SafeCatFileName(name)
	for _, e := range list {
		if e.Dir == safe || strings.TrimSpace(e.H1) == name {
			return nil, catBadInput("已存在同名分类：%s", e.File)
		}
	}
	seq := 1
	for _, e := range list {
		if e.Seq >= seq {
			seq = e.Seq + 1
		}
	}
	fname := fmt.Sprintf("%02d-%s.md", seq, safe)
	rel := "categories/" + fname
	abs := filepath.Join(s.skillDir(slug), "categories", fname)
	if _, err := os.Stat(abs); err == nil {
		return nil, catBadInput("文件已存在：%s", rel)
	}
	if err := os.WriteFile(abs, []byte(buildCategorySkeleton(name, trigger, requirement)), 0o644); err != nil {
		return nil, fmt.Errorf("写入分类文件失败: %w", err)
	}
	// 范文目录预先建好：不建的话用户传范文时才知道没地方放（errors 在下一步才冒出来）
	exampleRel := "examples/" + safe + "/"
	if err := os.MkdirAll(filepath.Join(s.skillDir(slug), "examples", safe), 0o755); err != nil {
		return nil, fmt.Errorf("创建范文目录失败: %w", err)
	}
	ch := &CategoryChange{
		Action: "create", Name: name, CategoryFile: rel, ExampleDir: exampleRel,
		FilesTouched: []string{rel},
	}
	s.addRouteRows(slug, name, trigger, ch)
	return ch, nil
}

// RenameCategory 改名并级联到 6 个落点（详见文件头注释）。
func (s *SkillStore) RenameCategory(slug, file, newName string) (*CategoryChange, error) {
	if err := validateCategoryName(newName); err != nil {
		return nil, err
	}
	newName = strings.TrimSpace(newName)
	cur, err := s.findCategory(slug, file)
	if err != nil {
		return nil, err
	}
	list, err := s.listCategories(slug)
	if err != nil {
		return nil, err
	}
	newSafe := SafeCatFileName(newName)
	for _, e := range list {
		if e.File == cur.File {
			continue
		}
		if e.Dir == newSafe || strings.TrimSpace(e.H1) == newName {
			return nil, catBadInput("已存在同名分类：%s", e.File)
		}
	}
	// 保留原序号：序号是手册的章序（01 经营业绩 → 02 会议纪要），
	// 改名不该把章节顺序重排，否则左树里的顺序会跳。
	base := filepath.Base(cur.File)
	prefix := ""
	if i := strings.Index(base, "-"); i > 0 {
		prefix = base[:i+1]
	}
	newRel := "categories/" + prefix + newSafe + ".md"
	if newRel != cur.File {
		if _, err := os.Stat(filepath.Join(s.skillDir(slug), prefix+newSafe+".md")); err == nil {
			return nil, catBadInput("目标文件已存在：%s", newRel)
		}
	}

	ch := &CategoryChange{
		Action: "rename", Name: newName, OldName: cur.displayName(),
		CategoryFile: newRel, FileMovedFrom: cur.File,
		ExampleDir: "examples/" + newSafe + "/",
	}

	// 1) 范文目录改名（先改目录：之后写分类文件里的范文路径就指向新目录）
	oldDirAbs := filepath.Join(s.skillDir(slug), "examples", cur.Dir)
	newDirAbs := filepath.Join(s.skillDir(slug), "examples", newSafe)
	if fi, statErr := os.Stat(oldDirAbs); statErr == nil && fi.IsDir() {
		if cur.Dir == newSafe {
			// 只改显示名（H1 与登记名不一致时纠偏），目录名没变 → 直接复用，
			// 否则下面的 os.Rename 会「自己覆盖自己」而误报「目录已存在」。
			ch.ExampleDir = "examples/" + cur.Dir + "/"
			ch.ExampleCount = countExampleFiles(oldDirAbs)
		} else {
			if _, err := os.Stat(newDirAbs); err == nil {
				return nil, catBadInput("范文目录已存在，改名会覆盖：examples/%s/", newSafe)
			}
			if err := os.Rename(oldDirAbs, newDirAbs); err != nil {
				return nil, fmt.Errorf("范文目录改名失败: %w", err)
			}
			ch.ExampleDir = "examples/" + newSafe + "/"
			ch.ExampleCount = countExampleFiles(newDirAbs)
		}
	} else if cur.Dir != newSafe {
		ch.Warnings = append(ch.Warnings,
			fmt.Sprintf("未找到范文目录 examples/%s/（该分类暂无范文）", cur.Dir))
	}

	// 2) 文本级联（整段锚定）
	names, dirs := cur.nameAliases(), cur.dirAliases()
	for _, rel := range renameCascadeTargets(cur.File) {
		if rel == cur.File {
			// 分类文件本体：内容改完落到新路径，老路径删掉（否则会同时存在两个同类）。
			// 这里的写入**不看命中数**——文件本身换了名字，命中 0 也要搬。
			body, rerr := os.ReadFile(filepath.Join(s.skillDir(slug), cur.File))
			if rerr != nil {
				return nil, fmt.Errorf("读取分类文件失败: %w", rerr)
			}
			out, _ := rewriteAnchoredRefs(string(body), names, dirs, newName, newSafe)
			if werr := os.WriteFile(filepath.Join(s.skillDir(slug), newRel), []byte(out), 0o644); werr != nil {
				return nil, fmt.Errorf("写入新分类文件失败: %w", werr)
			}
			ch.FilesTouched = append(ch.FilesTouched, newRel)
			if newRel != cur.File {
				if rmerr := os.Remove(filepath.Join(s.skillDir(slug), cur.File)); rmerr != nil {
					ch.Warnings = append(ch.Warnings, "旧分类文件删除失败: "+rmerr.Error())
				} else {
					ch.FilesDeleted = append(ch.FilesDeleted, cur.File)
				}
			}
			continue
		}
		touched, hits, err := s.cascadeFile(slug, rel, func(c string) (string, int) {
			return rewriteAnchoredRefs(c, names, dirs, newName, newSafe)
		})
		if err != nil {
			ch.Warnings = append(ch.Warnings, rel+" 未更新: "+err.Error())
			continue
		}
		if touched && hits > 0 {
			ch.FilesTouched = append(ch.FilesTouched, rel)
		}
	}

	// 3) meta.json 的分类清单
	if touched, err := s.cascadeRenameMeta(slug, names, newName); err != nil {
		ch.Warnings = append(ch.Warnings, "meta.json 未更新: "+err.Error())
	} else if touched {
		ch.FilesTouched = append(ch.FilesTouched, "meta.json")
	}
	return ch, nil
}

// DeleteCategory 删除分类；分类下还有范文时必须 force（前端先弹二次确认）。
func (s *SkillStore) DeleteCategory(slug, file string, force bool) (*CategoryChange, error) {
	cur, err := s.findCategory(slug, file)
	if err != nil {
		return nil, err
	}
	// names 用于路由表与 reviewer 小节；这里**不做路径清理**是有意的：
	// 引用范文路径的地方只有分类文件自己（已删）和索引表（已清行），
	// 剩下的只可能是管理员在 requirement.md 里手写的散文，机器不该改它。
	names := cur.nameAliases()
	exDirAbs := filepath.Join(s.skillDir(slug), "examples", cur.Dir)
	count := countExampleFiles(exDirAbs)

	ch := &CategoryChange{
		Action: "delete", Name: cur.displayName(), CategoryFile: cur.File,
		ExampleDir: "examples/" + cur.Dir + "/", ExampleCount: count,
		FilesDeleted: []string{cur.File},
	}
	if count > 0 && !force {
		return ch, fmt.Errorf("%w（%d 篇），删除会一并删掉这些范文", ErrCategoryInUse, count)
	}

	// 1) 范文目录
	if fi, statErr := os.Stat(exDirAbs); statErr == nil && fi.IsDir() {
		if err := os.RemoveAll(exDirAbs); err != nil {
			return nil, fmt.Errorf("删除范文目录失败: %w", err)
		}
		ch.FilesDeleted = append(ch.FilesDeleted, ch.ExampleDir)
	}
	// 2) 分类文件
	if err := os.Remove(filepath.Join(s.skillDir(slug), file)); err != nil {
		return nil, fmt.Errorf("删除分类文件失败: %w", err)
	}
	// 3) 两张路由表
	s.dropRouteRows(slug, names, ch)
	// 4) reviewer.md 里该分类的小节（整段）
	if touched, hits, err := s.cascadeFile(slug, "reviewer.md", func(c string) (string, int) {
		return dropSection(c, names, 3)
	}); err != nil {
		ch.Warnings = append(ch.Warnings, "reviewer.md 未更新: "+err.Error())
	} else if touched && hits > 0 {
		ch.FilesTouched = append(ch.FilesTouched, "reviewer.md")
	}
	// 5) meta.json 清单
	if touched, err := s.cascadeDropMeta(slug, names); err != nil {
		ch.Warnings = append(ch.Warnings, "meta.json 未更新: "+err.Error())
	} else if touched {
		ch.FilesTouched = append(ch.FilesTouched, "meta.json")
	}
	return ch, nil
}

// cascadeDropMeta 从 meta.json 的 manual.categories 里移除该分类。
func (s *SkillStore) cascadeDropMeta(slug string, names []string) (bool, error) {
	b, err := os.ReadFile(s.metaPath(slug))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	// UseNumber：meta.json 里的数字（产物大小、字数、耗时毫秒）不能被 float64 往返改写，
	// 例如 12345678901234567890 会变成 1.2345678901234567e+19。
	// 代价是重新序列化后键序按字典序排列（Go map 语义），内容不变。
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return false, fmt.Errorf("meta.json 不是合法 JSON，跳过清单同步: %w", err)
	}
	man, _ := m["manual"].(map[string]any)
	if man == nil {
		return false, nil
	}
	arr, _ := man["categories"].([]any)
	if len(arr) == 0 {
		return false, nil
	}
	kept := make([]any, 0, len(arr))
	dropped := false
	for _, v := range arr {
		if sv, ok := v.(string); ok && catNameHit(sv, names) {
			dropped = true
			continue
		}
		kept = append(kept, v)
	}
	if !dropped {
		return false, nil
	}
	man["categories"] = kept
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return false, err
	}
	return true, os.WriteFile(s.metaPath(slug), out, 0o644)
}

// AddCategoryExample 往指定分类里加一篇范文，并把路径写进该分类的「参考范文」清单。
// 返回新范文的相对路径。
func (s *SkillStore) AddCategoryExample(slug, file, content string) (string, error) {
	cur, err := s.findCategory(slug, file)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(content) == "" {
		return "", errors.New("范文内容不能为空")
	}
	dirRel := "examples/" + cur.Dir
	dirAbs := filepath.Join(s.skillDir(slug), "examples", cur.Dir)
	if err := os.MkdirAll(dirAbs, 0o755); err != nil {
		return "", fmt.Errorf("创建范文目录失败: %w", err)
	}
	next := 1
	if entries, err := os.ReadDir(dirAbs); err == nil {
		for _, en := range entries {
			if en.IsDir() || !strings.HasSuffix(en.Name(), ".md") {
				continue
			}
			stem := strings.TrimSuffix(en.Name(), ".md")
			if n, err := strconv.Atoi(stem); err == nil && n >= next {
				next = n + 1
			}
		}
	}
	rel := fmt.Sprintf("%s/%02d.md", dirRel, next)
	if _, err := os.Stat(filepath.Join(dirAbs, fmt.Sprintf("%02d.md", next))); err == nil {
		return "", catBadInput("范文文件已存在：%s", rel)
	}
	if err := os.WriteFile(filepath.Join(dirAbs, fmt.Sprintf("%02d.md", next)), []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("写入范文失败: %w", err)
	}
	touched, _, err := s.cascadeFile(slug, file, func(c string) (string, int) {
		out, ok := upsertRefLine(c, rel)
		if !ok {
			return c, 0
		}
		return out, 1
	})
	if err != nil {
		return rel, fmt.Errorf("范文已保存（%s），但更新「参考范文」清单失败: %w", rel, err)
	}
	_ = touched // 未改动＝该路径已在清单里，属正常
	return rel, nil
}
