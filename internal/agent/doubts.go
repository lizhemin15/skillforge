package agent

// 疑点回执：needs 闸门的「停问」分支从「甩通用要素清单」换成「对着用户原文挑真疑点」。
//
// 背景（用户原话）：「有的时候似乎像是没看到我的信息一样的，还在问我要信息，
// 要的时候也不是根据我目前提供的信息的基础上来进一步补充，而是直接通用的补充。」
// 旧版停问文案 needsMessage 是照技能参数表拼的——清单跟用户说了什么完全无关，
// 所以「信息其实就在原文里」也会被问一遍；真缺信息时问的又是「请提供标题、时间、
// 地点」这种脱离语境的泛问。
//
// 新做法（三段式）：
//  1. RaiseDoubts：一次便宜调用（关思考链 + JSON 模式），让模型**逐字引用用户原文**
//     挑 0~3 条真疑点，每条带默认理解（用户回「就按你的」即可继续）；
//  2. ValidateDoubts：本地校验——引用必须能在用户原文（含最近几条用户消息）里
//     逐字找到，找不到的疑点一律丢弃（防模型编造引用，宁可直写也不show假引用）；
//  3. 全部疑点都被丢弃 / 模型说没有疑点 / 这一跳超时报错 → 不拦，带假设直写
//     （危害不对称：拦错一轮，用户白等且以为材料丢了；放过一轮，假设摆在明面上可纠正）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/model"
)

const (
	// maxDoubts 真疑点上限。用户要的是「交互着改」，不是填表——问太多就变回旧版。
	maxDoubts = 3
	// minQuoteRunes 引用片段的最短长度（rune 数）。太短的引用（「的」「写一篇」）
	// 锚不住任何具体语义，等于没锚。
	minQuoteRunes = 8
	// minInferRunes 默认理解的最短长度。没有具体值/写法的 inference 等于
	// 「请提供更多信息」换了个说法——那正是要废掉的泛问。
	minInferRunes = 6
)

// Doubt 一条疑点。Kind: ambiguity=我可能理解歪了（必须带原文引用）；
// missing=原文确实没写（quote 必须为空，inference 给出我打算采用的默认）。
type Doubt struct {
	Kind      string `json:"kind"`
	Quote     string `json:"quote"`
	Inference string `json:"inference"`
	Impact    string `json:"impact"`
}

// DoubtReport RaiseDoubts 的输出。没有疑点时 Doubts 为空——「无疑点」是合法且
// 应有的结果，不是失败（禁止硬凑）。
type DoubtReport struct {
	Doubts []Doubt `json:"doubts"`
}

const doubtsSys = `你是「对齐疑点」裁判。用户给了一段材料/需求，一个写作技能即将动笔。你的任务不是列出缺哪些字段，而是读用户原文，把「我可能理解歪了」或「原文确实没写、但随便猜会明显影响成稿」的点挑出来，每条给出你打算采用的默认理解，供用户一句话确认或纠正。

硬规则（违反的条目会被程序直接丢弃）：
1. quote 必须从用户原文里**逐字**复制（≥8 字）：不许改写、不许跨句拼接、不许补标点。原文长这样，quote 就得长这样。
2. 原文已经写明白的，绝不许列成疑点。
3. 最多 3 条。能自己合理推断的一律不列——推断结果直接当默认理解用。
4. 没有疑点就输出 {"doubts":[]}。禁止硬凑；禁止把「请提供标题/时间/地点」这类泛问列进来。
5. 某项原文确实没提时，kind 用 "missing"、quote 留空字符串，inference 写你打算怎么默认。绝不允许给 missing 编造引用。
6. 每条 inference ≥6 字、具体到值或写法（「按 9 月 22 日办」「语气按内部通知」），impact 写清影响成稿的哪一点。

只输出一个 JSON 对象，全篇 ≤300 字：
{"doubts":[{"kind":"ambiguity","quote":"用户原文片段","inference":"我按……理解","impact":"……"},{"kind":"missing","quote":"","inference":"……","impact":"……"}]}`

// doubtsPhaseLimit 疑点跳的独立时间预算。它加在「本该停下问用户」的分支上，
// 那个分支旧版是 0 秒本地出话术；新版的预算必须小（默认 12s），否则
// 「为了问得更好反而让用户干等」就成了新的投诉点。超时按「无疑点」处理。
func doubtsPhaseLimit() time.Duration {
	if v := strings.TrimSpace(os.Getenv("SKILLFORGE_DOUBTS_SEC")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 12 * time.Second
}

// RaiseDoubts 对着用户原文挑真疑点。skillName 用于让模型知道成稿形态；
// missing 是技能声明必填、但分类器没从原文里提取到的参数（只当「重点核对区」，
// 提示模型优先确认这几项是不是其实已经写在原文里了，不是让它照抄着问一遍）。
func (e *Engine) RaiseDoubts(ctx context.Context, skillName string, missing []model.Param, userMsg string, history []Message) (*DoubtReport, error) {
	if strings.TrimSpace(userMsg) == "" {
		return nil, fmt.Errorf("RaiseDoubts: 用户消息为空")
	}
	dctx, dcancel := context.WithTimeout(ctx, doubtsPhaseLimit())
	defer dcancel()

	var b strings.Builder
	b.WriteString("## 即将动笔的技能\n\n")
	b.WriteString(strings.TrimSpace(skillName))
	if labels := needsLabels(missing); labels != "" {
		b.WriteString("\n\n## 技能声明必填、但没提取到的项（优先核对：这些是不是其实已经写在原文里）\n\n")
		b.WriteString(labels)
	}
	b.WriteString("\n\n## 用户这次给的原话\n\n")
	b.WriteString(strings.TrimSpace(userMsg))
	if recent := recentRawUser(history, 3); recent != "" {
		b.WriteString("\n\n## 本对话中用户先前说过的话（供理解指代，不作为本次原文）\n\n")
		b.WriteString(recent)
	}

	out, err := e.llm.StreamChat(dctx, doubtsSys, b.String(), llm.StreamOpts{
		DisableThinking: true,
		JSONMode:        true,
		OnReasoning:     reasoningSink(ctx),
		OnContent:       contentSink(ctx),
	})
	if err != nil {
		return nil, fmt.Errorf("疑点跳调用失败: %w", err)
	}
	rep := &DoubtReport{}
	if err := json.Unmarshal([]byte(extractJSON(out)), rep); err != nil {
		fmt.Fprintf(os.Stderr, "[doubts] 输出解析失败: %v（原文: %s）\n", err, runeClip(out, 200))
		return nil, fmt.Errorf("疑点跳输出解析失败: %w", err)
	}
	return rep, nil
}

// ValidateDoubts 本地校验疑点，返回可展示的疑点（可能为空=无疑点，直接写）。
// 规则见文件头。haystack 是引用的合法查找域（本轮消息 + 最近几条用户消息原文）。
func ValidateDoubts(rep *DoubtReport, haystack string) []Doubt {
	if rep == nil || len(rep.Doubts) == 0 {
		return nil
	}
	hay := normForQuote(haystack)
	if hay == "" {
		return nil
	}
	seen := map[string]bool{}
	out := make([]Doubt, 0, maxDoubts)
	for _, d := range rep.Doubts {
		if len(out) >= maxDoubts {
			break
		}
		d.Quote = strings.TrimSpace(d.Quote)
		d.Inference = strings.TrimSpace(d.Inference)
		d.Impact = strings.TrimSpace(d.Impact)
		d.Kind = strings.ToLower(strings.TrimSpace(d.Kind))

		q := normForQuote(d.Quote)
		if q == "" {
			// 没引用 = 缺失型。允许，但必须明说 kind=missing（防模型把「编不出引用」
			// 的 ambiguity 偷偷降级成 missing 留下来——没有引用的 ambiguity 一律丢）。
			if d.Kind != "missing" {
				continue
			}
		} else {
			// 有引用 = 歧义型。引用必须逐字命中且够长，否则整条丢弃：
			// 展示一条对不上原文的引用比不展示更糟（用户会去原文里找，找不到）。
			if len([]rune(q)) < minQuoteRunes || !strings.Contains(hay, q) {
				continue
			}
			d.Kind = "ambiguity"
		}
		// inference 是疑点的价值所在：没有具体默认理解的「疑点」就是旧版泛问换皮。
		if len([]rune(normForQuote(d.Inference))) < minInferRunes {
			continue
		}
		if d.Impact == "" {
			d.Impact = "成稿的口径"
		}
		key := d.Kind + "\x00" + q + "\x00" + normForQuote(d.Inference)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, d)
	}
	return out
}

// DoubtsMessage 停问时给用户看的文案。每条必须落在用户能一句话回复的形态上。
func DoubtsMessage(doubts []Doubt) string {
	if len(doubts) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "我把你给的原话过了一遍，有 %d 处想跟你对齐（其余我都按原文推断，不再追问）：\n", len(doubts))
	for i, d := range doubts {
		b.WriteString("\n")
		if d.Kind == "ambiguity" {
			fmt.Fprintf(&b, "%d. 你说「%s」——我理解成：%s（影响到：%s）\n", i+1, d.Quote, d.Inference, d.Impact)
		} else {
			fmt.Fprintf(&b, "%d. 原文没提%s——我打算按「%s」写\n", i+1, d.Impact, d.Inference)
		}
	}
	b.WriteString("\n回「就按你的」我就照上面的理解直接写；要改哪条，直接说第几条、怎么改。")
	return b.String()
}

// DoubtsShort 给计时器旁白用的一句话。
func DoubtsShort(doubts []Doubt) string {
	if len(doubts) == 0 {
		return "无疑点"
	}
	if len(doubts) == 1 {
		return "1 处待确认"
	}
	return strconv.Itoa(len(doubts)) + " 处待确认"
}

// DoubtHaystack 引用的合法查找域：本轮消息 + 最近 n 条用户消息的**原文**
// （不经 mdCellInline 转义、不截断——疑点可以锚在用户上一条贴的材料里，
// 比如先贴素材再说「照这个写」的两段式）。
func DoubtHaystack(userMsg string, history []Message) string {
	parts := []string{userMsg}
	for i := len(history) - 1; i >= 0 && len(parts) <= 3; i-- {
		if history[i].Role != "user" {
			continue
		}
		if t := strings.TrimSpace(history[i].Content); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

// recentRawUser 最近 n 条用户消息原文（供模型理解指代用）。
func recentRawUser(history []Message, n int) string {
	var lines []string
	for i := len(history) - 1; i >= 0 && len(lines) < n; i-- {
		if history[i].Role != "user" {
			continue
		}
		if t := strings.TrimSpace(history[i].Content); t != "" {
			lines = append(lines, t)
		}
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return strings.Join(lines, "\n---\n")
}

// normForQuote 引用比对的归一化：剥掉所有空白（含全角空格/换行）+ 小写。
// 用户原文里的换行、空格不该成为「逐字命中」的障碍，但语义字符一个都不能少。
func normForQuote(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// needsLabels 把参数列表拼成「名（标签）」一行，给疑点跳当重点核对区。
func needsLabels(needs []model.Param) string {
	labels := make([]string, 0, len(needs))
	for _, n := range needs {
		l := n.Label
		if l == "" {
			l = n.Name
		}
		if l != "" {
			labels = append(labels, l)
		}
	}
	return strings.Join(labels, "、")
}
