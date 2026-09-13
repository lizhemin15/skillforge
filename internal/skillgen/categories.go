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
	"unicode"
	"unicode/utf8"

	"github.com/lizhemin15/skillforge/internal/store"
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
	segs, err := SplitByAnchorsDetailed(text, anchors)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		out = append(out, s.Text)
	}
	return out, nil
}

// Segment 是一段按锚点切出的范文：Text 是原文的连续子串（原样，未改写一个字），
// Start/End 是它在原文中的字节区间（半开），Level 是定位它所用的匹配级别。
//
// 暴露 Level 是为了让调用方把「这段是靠放宽匹配才切到的」显性报给管理员——
// 静默放宽会让管理员失去判断依据，也可能掩盖手册本身的问题。
type Segment struct {
	Text  string
	Start int
	End   int
	Level normLevel
}

// normLevel 是锚点定位的归一化级别，定位时从严到宽依次尝试。
//
// 为什么需要放宽：手册 PDF 经解析服务抽出的文本保留了原文的物理换行
// （实测一本 6 万字的 50 页手册里有 1600+ 个换行），而模型「摘录」一句
// 跨行的话时会把换行吞掉、连成一行——两边内容逐字相同，只在换行/空格上
// 有别。若只认逐字相等，就会把「模型摘对了、只是没保留换行」误判成失败，
// 整类范文被丢弃（实测 12 类里 6 类全灭）。
//
// 放宽的边界很硬：只放宽「怎么找到位置」，绝不放宽「切片从哪来」——
// 片段永远是原文的一段连续子串，一个字符都不是模型补的，保真度不受影响。
type normLevel int

const (
	levelExact   normLevel = iota // 逐字相等（旧行为，优先）
	levelNoSpace                  // 忽略所有空白（换行、空格、全角空格）
	levelNoPunct                  // 再忽略中英文标点
)

var normLevels = []normLevel{levelExact, levelNoSpace, levelNoPunct}

func (l normLevel) String() string {
	switch l {
	case levelExact:
		return "逐字"
	case levelNoSpace:
		return "忽略空白"
	case levelNoPunct:
		return "忽略空白与标点"
	}
	return "未知"
}

// punctCutset 是归一化时丢弃的标点白名单。
//
// 刻意用白名单而不是 unicode.IsPunct：书名号《》、方括号【】这类符号在各
// Unicode 类别里归属不一，白名单可读、可审计。也刻意不含 %、+、#、字母与
// 数字——这些是稿件里要保真的内容，丢了会让"数字保真核对"失效。
const punctCutset = "，。、；：？！“”‘’（）()《》〈〉【】[]{}—–…·「」『』,.!?;:\"'`　"

// cutByLevel 判断某字符在该级别下是否应从归一化文本中丢弃。
func cutByLevel(r rune, lv normLevel) bool {
	if unicode.IsSpace(r) {
		return lv >= levelNoSpace
	}
	return lv >= levelNoPunct && strings.ContainsRune(punctCutset, r)
}

// normIndex 是「归一化文本 + 反向位置映射」。
//
// s[i] 是保留下来的第 i 个字符；beg[i]/end[i] 是它在原文中的字节区间（半开）。
// 被丢弃的字符不占位置，因此任意一段归一化区间都能映射回原文里一段连续的
// 原始字节——把归一化坐标换回原文坐标再切片，拿到的仍是原文子串。
type normIndex struct {
	s   []rune
	beg []int
	end []int
}

func buildNormIndex(text string, lv normLevel) *normIndex {
	ni := &normIndex{}
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if cutByLevel(r, lv) {
			i += size
			continue
		}
		ni.s = append(ni.s, r)
		ni.beg = append(ni.beg, i)
		ni.end = append(ni.end, i+size)
		i += size
	}
	return ni
}

// find 返回 needle 在归一化文本中出现的全部起始位置（下标为归一化下标）。
// 返回全部而非首个，是因为「出现多次」必须被当成错误而不是随便取一个。
func (ni *normIndex) find(needle []rune) []int {
	if len(needle) == 0 || len(needle) > len(ni.s) {
		return nil
	}
	var hits []int
	for i := 0; i+len(needle) <= len(ni.s); i++ {
		match := true
		for j := range needle {
			if ni.s[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			hits = append(hits, i)
		}
	}
	return hits
}

// leadingCut 返回 frag 开头连续「在该级别下会被丢弃」的字符数。
func leadingCut(frag string, lv normLevel) int {
	n := 0
	for _, r := range frag {
		if !cutByLevel(r, lv) {
			break
		}
		n++
	}
	return n
}

// trailingCut 返回 frag 末尾连续「在该级别下会被丢弃」的字符数。
func trailingCut(frag string, lv normLevel) int {
	rs := []rune(frag)
	n := 0
	for i := len(rs) - 1; i >= 0; i-- {
		if !cutByLevel(rs[i], lv) {
			break
		}
		n++
	}
	return n
}

// absorbHead 从 off 向前最多吸收 want 个「该级别下本就要丢弃」的字符，返回新起点。
//
// 用途：定位骨架只包含锚点的**保留字符**，但锚点首尾被归一化丢掉的字符
// （如开头的【、结尾的。）在原文里往往真实存在。不补回来，切片就会缺头少尾
// ——实测「忽略空白与标点」级别下，末位句号必然被吃掉。
//
// 安全边界：只吃该级别本就要丢弃的字符，且数量严格等于 want（不多吃），
// 所以绝不可能把正文内容吞进切片。
func absorbHead(text string, off, want int, lv normLevel) int {
	for n := 0; n < want && off > 0; n++ {
		r, size := utf8.DecodeLastRuneInString(text[:off])
		if !cutByLevel(r, lv) {
			break
		}
		off -= size
	}
	return off
}

// absorbTail 从 off 向后最多吸收 want 个「该级别下本就要丢弃」的字符，返回新终点。
// 与 absorbHead 对称，边界约束相同。
func absorbTail(text string, off, want int, lv normLevel) int {
	for n := 0; n < want && off < len(text); n++ {
		r, size := utf8.DecodeRuneInString(text[off:])
		if !cutByLevel(r, lv) {
			break
		}
		off += size
	}
	return off
}

// locateAnchor 定位片段在原文中的字节区间（半开），返回终点下标与命中级别。
//
// 三级依次尝试：逐字 → 忽略空白 → 忽略空白与标点。任一级出现「不唯一」立即
// 报错（放宽只会让匹配更不唯一，继续放宽不会变好）；三级都找不到才报找不到，
// 并在错误里写明三级都试过了，避免排查时误以为只试了逐字。
func locateAnchor(text, frag string) (int, int, normLevel, error) {
	var tried normLevel
	for _, lv := range normLevels {
		tried = lv
		ni := buildNormIndex(text, lv)
		needle := make([]rune, 0, len(frag))
		for _, r := range frag {
			if cutByLevel(r, lv) {
				continue
			}
			needle = append(needle, r)
		}
		if len(needle) == 0 {
			return 0, 0, lv, fmt.Errorf("锚点 %q 在该级别下归一化后为空（整段都是空白/标点）", anchorPreview(frag))
		}
		hits := ni.find(needle)
		switch len(hits) {
		case 1:
			k := hits[0]
			// 骨架区间 = 锚点首个保留字符的起点 → 末个保留字符的终点。
			// 再按锚点首尾被归一化丢掉的字符数，向原文两侧「对齐吸收」补回来：
			// 走「忽略空白与标点」级别时，锚点的【 和 。 都不参与定位，
			// 不补回切片就会缺头少尾（末位句号必被吃掉）。
			beg := absorbHead(text, ni.beg[k], leadingCut(frag, lv), lv)
			end := absorbTail(text, ni.end[k+len(needle)-1], trailingCut(frag, lv), lv)
			return beg, end, lv, nil
		case 0:
			continue
		default:
			return 0, 0, lv, fmt.Errorf("锚点 %q 在原文中出现 %d 次（%s 匹配下不唯一，拿它当锚点会切错位置）",
				anchorPreview(frag), len(hits), lv)
		}
	}
	return 0, 0, tried, fmt.Errorf("锚点 %q 在原文中找不到（已依次尝试逐字 / 忽略空白 / 忽略空白与标点三种匹配）", anchorPreview(frag))
}

// SplitByAnchorsDetailed 是 SplitByAnchors 的完整版：除切片外还返回每段的
// 定位区间与所用匹配级别，供调用方记日志、做保真核对。
//
// 严格语义（与旧版一致）：任何"切不准"的情况都必须报错，绝不静默降级——
// 静默降级切错位置，产出的是「看起来像范文、其实是别段」的脏数据，比直接
// 失败危险得多（错的数据会被当成手册原文喂给模型）。报错情形：
//
//	① 某锚点片段在原文中找不到（三级匹配都试过）；
//	② 某锚点片段在原文中出现多次（不唯一）——拿重复片段当锚点必然切错；
//	③ 锚点顺序颠倒（后一个 Start 出现在前一段 Start 之前）；
//	④ 相邻两段重叠（后一个 Start 落在前一段区间内）；
//	⑤ anchors 为空；
//	⑥ Start 或 End 为空串。
func SplitByAnchorsDetailed(text string, anchors []CatAnchor) ([]Segment, error) {
	if len(anchors) == 0 {
		return nil, errors.New("SplitByAnchors: 锚点列表为空，无法切分")
	}

	segs := make([]Segment, 0, len(anchors))
	// prevStart / prevEnd 是上一段在原文中的真实下标。必须先量出下标再比大小：
	// 裸子串比较（如 strings.Contains）判断不出"谁在前"和"是否重叠"，
	// 这正是最容易埋 bug 的地方。
	prevStart, prevEnd := -1, -1

	for i, a := range anchors {
		if strings.TrimSpace(a.Start) == "" || strings.TrimSpace(a.End) == "" {
			return nil, fmt.Errorf("SplitByAnchors: 第 %d 个锚点的 Start/End 为空串", i+1)
		}

		si, _, lvS, err := locateAnchor(text, a.Start)
		if err != nil {
			return nil, fmt.Errorf("SplitByAnchors: 第 %d 个锚点的 Start 定位失败：%w", i+1, err)
		}
		_, ei, lvE, err := locateAnchor(text, a.End)
		if err != nil {
			return nil, fmt.Errorf("SplitByAnchors: 第 %d 个锚点的 End 定位失败：%w", i+1, err)
		}

		// End 在自己这段的 Start 之前（或紧贴其前）→ 锚点本身写反了。
		// ei 是半开区间的终点，故用 <= 而非 <：ei <= si 意味着这一段为空或反向。
		if ei <= si {
			return nil, fmt.Errorf("SplitByAnchors: 第 %d 个锚点的 End %q 出现在其 Start %q 之前", i+1, anchorPreview(a.End), anchorPreview(a.Start))
		}

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
		// 注意 si/ei 已由归一化坐标映射回原文坐标，所以即使定位走了「忽略
		// 空白」级别，切出的片段里依然保留着原文的换行与标点。
		lv := lvS
		if lvE > lv {
			lv = lvE
		}
		segs = append(segs, Segment{Text: text[si:ei], Start: si, End: ei, Level: lv})
		prevStart, prevEnd = si, ei
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
func ExtractStructure(ctx context.Context, c chatClient, sourceText string) (*Structure, error) {
	if strings.TrimSpace(sourceText) == "" {
		return nil, errors.New("ExtractStructure: 原文为空，无法抽取结构")
	}
	if c == nil {
		return nil, errors.New("ExtractStructure: 未配置模型客户端")
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
//
// 实现直接委托 store.MdCellText：管理端加分类时也要造同样的表格行，
// 两处各留一份实现迟早会分叉（分叉的代价是「训练生成的表」与「管理端加的表」
// 形状不同，运行时的解析可能只认一种）。
func mdCell(s string) string {
	return store.MdCellText(s)
}

// safeCatFileName 把分类名清洗成安全的文件名片段（保留中文）。
// 只挡路径分隔符与少数文件系统保留字符，不做拼音化——手册分类名就是中文，
// 转成拼音反而让人认不出来。
//
// 同样委托 store.SafeCatFileName：清洗规则必须与「管理端新增/改名分类」逐字一致，
// 否则训练期建的文件名与管理端建的文件名形态不同，catDirName 反推出的类别名会对不上。
func safeCatFileName(name string) string {
	return store.SafeCatFileName(name)
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
	// 开头统一用 store.CategoryIndexHeader：管理端手加分类时也要凭空造出这个文件
	// （非手册技能的第一次加类），两处各写一份迟早会分叉。
	var idx strings.Builder
	idx.WriteString(store.CategoryIndexHeader)
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
