package skillgen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lizhemin15/skillforge/internal/llm"
)

// 本文件实现「写作手册 → 分类 skill」的抽取与落盘，是旧流水线的关键修正：
//
//	旧做法：buildExamples 直接让 LLM「生成一篇 400-900 字示例范文」——
//	         范文是编出来的，读者拿到的"参考范文"其实手册里根本没有。
//	新做法：LLM 只负责「摘录 + 定位」（ExtractStructure），真正的切分由
//	         SplitByAnchors 按锚点字符串在原文里逐字切出来。原文片段不经过
//	         任何模型改写，所以保真度可以被机械验证（切出来的片段一定能在
//	         source 原文里 strings.Index 到）。
//
// 这样分工的原因：LLM 定位/摘录是它的强项（理解语义、找章节边界），
// 但让它"复述"一段原文时几乎必然会顺手润色几个字——而范文的每一个字都
// 必须是手册原文，否则就等于给用户喂了一份伪手册。

// CatAnchor 是范文在素材原文中的首尾锚点（原文片段，不是页码）。
//
// 为什么不用页码：OCR 出来的页码常常错位/识别不全，而「范文首句」「范文末句」
// 是原文里真实存在、可以 strings.Index 定位的字符串，天然抗漂移。
type CatAnchor struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// Category 是手册里的一个写作分类。
type Category struct {
	Name        string      `json:"name"`        // 分类名（原文标题）
	Trigger     string      `json:"trigger"`     // 触发特征：什么需求该落到这一类（用于路由）
	Requirement string      `json:"requirement"` // 该类写作要求的原文摘录（不重写）
	Anchor      []CatAnchor `json:"anchors"`     // 该类范文的起止锚点（可多篇）
}

// Structure 是整本手册抽出来的结构：总则 + 若干分类。
type Structure struct {
	General    string     `json:"general"`
	Categories []Category `json:"categories"`
}

// anchorPreview 截取锚点片段的前若干字符用于报错。
// 用 rune 切而不是 byte 切，避免把一个汉字劈成两半、在日志里显示成乱码。
func anchorPreview(s string) string {
	const n = 30
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// SplitByAnchors 在原文中按锚点顺序切出各段范文，原样返回
// （不 trim 正文内部、不重写任何一个字）。
//
// 严格语义：任何"切不准"的情况都必须报错，绝不静默降级——
// 因为静默降级切错位置，产出的是「看起来像范文、其实是别段」的脏数据，
// 比直接失败危险得多（错的数据会被当成手册原文喂给模型）。报错情形：
//
//	① 某锚点片段在原文中找不到；
//	② 某锚点片段在原文中出现多次（不唯一）——拿重复片段当锚点必然切错；
//	③ 锚点顺序颠倒（后一个 Start 出现在前一段 Start 之前）；
//	④ 相邻两段重叠（后一个 Start 落在前一段区间内）；
//	⑤ anchors 为空；
//	⑥ Start 或 End 为空串。
//
// 返回的每个片段以 Start 开头、以 End 结尾（含锚点本身），片段之间不含原文中
// 的额外空白——是 text 的连续子串，可直接与原文做子串比对。
func SplitByAnchors(text string, anchors []CatAnchor) ([]string, error) {
	if len(anchors) == 0 {
		return nil, errors.New("SplitByAnchors: 锚点列表为空，无法切分")
	}

	segs := make([]string, 0, len(anchors))
	// prevStart / prevEnd 是上一段的真实下标。注意必须先量出下标再比大小：
	// 裸子串比较（如 strings.Contains）判断不出"谁在前"和"是否重叠"，
	// 这正是最容易埋 bug 的地方。
	prevStart, prevEnd := -1, -1

	for i, a := range anchors {
		if strings.TrimSpace(a.Start) == "" || strings.TrimSpace(a.End) == "" {
			return nil, fmt.Errorf("SplitByAnchors: 第 %d 个锚点的 Start/End 为空串", i+1)
		}

		// 唯一性：用 strings.Count 而非 Contains。出现 0 次 = 找不到；
		// 出现 ≥2 次 = 有歧义，Index 只会给第一个，切出来可能是错的段落。
		if n := strings.Count(text, a.Start); n != 1 {
			if n == 0 {
				return nil, fmt.Errorf("SplitByAnchors: 第 %d 个锚点的 Start %q 在原文中找不到", i+1, anchorPreview(a.Start))
			}
			return nil, fmt.Errorf("SplitByAnchors: 第 %d 个锚点的 Start %q 在原文中出现 %d 次（必须唯一，否则会切错位置）", i+1, anchorPreview(a.Start), n)
		}
		if n := strings.Count(text, a.End); n != 1 {
			if n == 0 {
				return nil, fmt.Errorf("SplitByAnchors: 第 %d 个锚点的 End %q 在原文中找不到", i+1, anchorPreview(a.End))
			}
			return nil, fmt.Errorf("SplitByAnchors: 第 %d 个锚点的 End %q 在原文中出现 %d 次（必须唯一，否则会切错位置）", i+1, anchorPreview(a.End), n)
		}

		si := strings.Index(text, a.Start)
		ei := strings.Index(text, a.End)
		// End 在自己这段的 Start 之前 → 锚点本身写反了。
		if ei < si {
			return nil, fmt.Errorf("SplitByAnchors: 第 %d 个锚点的 End %q 出现在其 Start %q 之前", i+1, anchorPreview(a.End), anchorPreview(a.Start))
		}
		end := ei + len(a.End) // 结束位置（不含）——下标比较统一用半开区间

		if prevEnd >= 0 {
			// 先判"顺序颠倒"再判"重叠"：si < prevStart 时两者都成立，
			// 但语义上是"顺序反了"更准确（后一段整个跑到前一段之前）。
			if si < prevStart {
				return nil, fmt.Errorf("SplitByAnchors: 第 %d 个锚点的 Start %q 出现在第 %d 段 Start %q 之前，锚点顺序颠倒",
					i+1, anchorPreview(a.Start), i, anchorPreview(anchors[i-1].Start))
			}
			if si < prevEnd {
				return nil, fmt.Errorf("SplitByAnchors: 第 %d 个锚点的 Start %q 落在第 %d 段区间内，两段重叠",
					i+1, anchorPreview(a.Start), i)
			}
		}

		// 切片即「原样」：不 TrimSpace、不折叠空白，逐字节等于原文子串。
		segs = append(segs, text[si:end])
		prevStart, prevEnd = si, end
	}
	return segs, nil
}

// parseStructure 解析 LLM 返回的结构 JSON，容忍常见输出噪声。
//
// 复用同包的 extractJSON（generator.go）剥掉 ```json 代码围栏与前后解释性
// 文字——它取第一个 "{" 到最后一个 "}" 之间的内容，本项目其它 JSON 调用
// （detectType / synthesizeMetadata）都走这条路，保持一致便于排查。
//
// 空串判为错误（而非返回空结构）：空回复意味着上游模型调用失败或被打断，
// 这里若静默返回空 Structure，调用方会以为"手册里没有分类"，把一次故障
// 伪装成一次成功的抽取——宁可直接报错，让上层重试或降级。
func parseStructure(raw string) (*Structure, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("parseStructure: LLM 返回为空")
	}
	body := extractJSON(raw)
	var st Structure
	if err := json.Unmarshal(body, &st); err != nil {
		return nil, fmt.Errorf("parseStructure: JSON 解析失败: %w", err)
	}
	return &st, nil
}

// extractionSystemPrompt 是结构抽取的 system prompt。
// 这里把「禁止创作」写死成硬约束，是因为一旦模型顺手润色了一个字，
// 后面 SplitByAnchors 的锚点就会在原文里找不到（Count==0）从而整段失败——
// 与其走到报错，不如在 prompt 层就把红线划清楚。
const extractionSystemPrompt = `你是写作手册的结构抽取器。你的唯一职责是「摘录 + 定位」，绝不是创作。
硬约束（违反即视为失败）：
1. 只允许从给定原文中原样摘录片段，禁止生成、改写、润色任何句子；摘录的每个字都必须在原文中出现过。
2. requirement 字段是该类写作要求的原文摘录（可跨段选取多句，但每一句都必须在原文中逐字出现过）。
3. anchors 里的 start/end 必须是原文中「唯一出现一次」的片段，长度建议 15-40 字；
   严禁使用页眉/页脚——OCR 会把同一行页眉重复识别很多次，拿它当锚点必然不唯一、必然切错。
4. 只抽取原文里真实存在的分类；不存在的分类不要编造，宁缺毋滥。
5. 只输出一个 JSON 对象，不要输出任何解释性文字，不要用 Markdown 代码围栏包裹。
输出 JSON 结构：
{
  "general": "总则/通用写作要求的原文摘录",
  "categories": [
    {
      "name": "分类名（原文里的章节标题）",
      "trigger": "什么样的写作需求该落到这一类（用于运行时意图路由，可以是你的概括）",
      "requirement": "该类写作要求的原文摘录",
      "anchors": [
        {"start": "范文首句的原文片段", "end": "范文末句的原文片段"}
      ]
    }
  ]
}`

// ExtractStructure 让 LLM 只做摘录与定位（prompt 里必须强约束「禁止创作新句子」）。
// 这是薄封装：真正的职责边界是「模型给锚点 → 纯代码 SplitByAnchors 切原文」，
// 所以这里不解析、不重排、不动原文，只把模型输出交给 parseStructure。
func ExtractStructure(ctx context.Context, c *llm.Client, sourceText string) (*Structure, error) {
	if strings.TrimSpace(sourceText) == "" {
		return nil, errors.New("ExtractStructure: 原文为空，无法抽取结构")
	}
	// jsonMode=true：让 provider 以 response_format 强约束输出 JSON 对象，
	// 省掉大半"模型在 JSON 外面裹一段寒暄"的解析麻烦。
	user := "以下是写作手册的完整原文（OCR 文本）。请只做摘录与定位，按约定输出 JSON：\n\n" + sourceText
	out, err := c.Chat(ctx, extractionSystemPrompt, user, true)
	if err != nil {
		return nil, fmt.Errorf("ExtractStructure: 调用模型失败: %w", err)
	}
	return parseStructure(out)
}

// mdCell 把一段文本压成能安全放进 markdown 表格单元格的形式：
// 换行会破坏表格行结构，竖线会被当成列分隔符，必须先转义/折叠。
func mdCell(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

// safeCatFileName 把分类名清洗成安全的文件名片段（保留中文）。
// 只挡路径分隔符与少数文件系统保留字符，不做拼音化——手册分类名就是中文，
// 转成拼音反而让人认不出来。
func safeCatFileName(name string) string {
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

// WriteCategories 落盘 categories/_index.md + categories/NN-<name>.md，
// 返回写入的相对路径列表（POSIX 分隔符，便于直接塞进 prompt / API 响应）。
//
// examples 参数是「类别名 -> 该类范文的相对路径列表」
// （如 经营业绩 -> [examples/经营业绩/01.md]）。
//
// 落盘内容刻意保持纯 Markdown、不含任何 JSON / 生成器字样：这些文件会被
// 运行时直接注入模型 prompt，也会给管理员人工编辑，越朴素越好。
func WriteCategories(dir string, st *Structure, examples map[string][]string) ([]string, error) {
	if st == nil {
		return nil, errors.New("WriteCategories: 结构为空")
	}
	catDir := filepath.Join(dir, "categories")
	if err := os.MkdirAll(catDir, 0o755); err != nil {
		return nil, fmt.Errorf("WriteCategories: 创建目录失败: %w", err)
	}

	var written []string

	// ---- categories/_index.md：运行时的意图路由表 ----
	var idx strings.Builder
	idx.WriteString("# 分类索引\n\n")
	idx.WriteString("本目录是写作手册的分类体系与范文索引。运行时先按「触发场景」把需求归到某一类，\n")
	idx.WriteString("再读该类的要求与范文起草。下表可供人工编辑维护。\n\n")
	idx.WriteString("| 分类 | 触发场景 |\n")
	idx.WriteString("| --- | --- |\n")
	for _, c := range st.Categories {
		idx.WriteString("| " + mdCell(c.Name) + " | " + mdCell(c.Trigger) + " |\n")
	}
	idxPath := filepath.Join(catDir, "_index.md")
	if err := os.WriteFile(idxPath, []byte(idx.String()), 0o644); err != nil {
		return nil, fmt.Errorf("WriteCategories: 写入 _index.md 失败: %w", err)
	}
	written = append(written, filepath.ToSlash(filepath.Join("categories", "_index.md")))

	// ---- categories/NN-<name>.md：每个分类一份 ----
	for i, c := range st.Categories {
		name := safeCatFileName(c.Name)
		fname := fmt.Sprintf("%02d-%s.md", i+1, name)

		var b strings.Builder
		b.WriteString("# " + strings.TrimSpace(firstNonEmpty(c.Name, name)) + "\n\n")

		b.WriteString("## 触发场景\n")
		b.WriteString(firstNonEmpty(strings.TrimSpace(c.Trigger), "（手册未摘录到，需人工补充）") + "\n\n")

		b.WriteString("## 写作要求\n")
		b.WriteString(firstNonEmpty(strings.TrimSpace(c.Requirement), "（手册未摘录到，需人工补充）") + "\n\n")

		b.WriteString("## 参考范文\n")
		// examples 的 key 用原始类别名（不是清洗后的文件名），调用方按 name 取。
		refs := examples[c.Name]
		if len(refs) == 0 {
			b.WriteString("（暂无，需人工补充）\n")
		} else {
			for _, r := range refs {
				b.WriteString("- " + filepath.ToSlash(strings.TrimSpace(r)) + "\n")
			}
		}

		if err := os.WriteFile(filepath.Join(catDir, fname), []byte(b.String()), 0o644); err != nil {
			return nil, fmt.Errorf("WriteCategories: 写入 %s 失败: %w", fname, err)
		}
		written = append(written, filepath.ToSlash(filepath.Join("categories", fname)))
	}

	return written, nil
}

// binaryExts 是 SourceText 要跳过的二进制扩展名。
// source/ 目录里往往同时躺着 OCR 的 .txt 和原始 .pdf/.jpg，如果把 PDF 当文本
// 读进来拼进原文，锚点定位会被满屏乱码带偏，所以按扩展名挡掉。
var binaryExts = map[string]bool{
	".pdf": true, ".jpg": true, ".jpeg": true, ".png": true, ".gif": true,
	".bmp": true, ".tif": true, ".tiff": true, ".webp": true, ".docx": true,
	".doc": true, ".xlsx": true, ".xls": true, ".pptx": true, ".zip": true,
	".gz": true, ".bin": true, ".exe": true,
}

// SourceText 把 source/ 下的文本素材拼成一份原文（供锚点定位），按文件名排序拼接。
//
// dir 可以是 skill 根目录（此时读 dir/source/），也可以直接是素材目录本身——
// 两种都支持，方便测试与调用方灵活传入。若 dir/source 存在则优先用它。
//
// 排序后拼接的原因：OCR 产物常常是 p001.txt、p002.txt… 多个分页文件，
// 文件名顺序就是阅读顺序；锚点定位依赖"原文的自然前后关系"，乱序拼接会让
// SplitByAnchors 的顺序检查误报。
func SourceText(dir string) (string, error) {
	srcDir := dir
	if fi, err := os.Stat(filepath.Join(dir, "source")); err == nil && fi.IsDir() {
		srcDir = filepath.Join(dir, "source")
	}

	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return "", fmt.Errorf("SourceText: 读取素材目录失败: %w", err)
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") { // 隐藏文件（如 .DS_Store）不是素材
			continue
		}
		if binaryExts[strings.ToLower(filepath.Ext(name))] {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return "", fmt.Errorf("SourceText: 在 %s 下未找到可用的文本素材", srcDir)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(srcDir, n))
		if err != nil {
			return "", fmt.Errorf("SourceText: 读取 %s 失败: %w", n, err)
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.Write(data)
	}
	return b.String(), nil
}
