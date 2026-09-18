package api

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lizhemin15/skillforge/internal/agent"
)

// assemblyFacts 是「这一轮往上下文里装了什么」的**本地事实**。
//
// 为什么要有这个结构：用户的原话是「速度过于慢了，中间可以流式输出思考的一些中间
// 材料，现在一直卡着计时」。线上实测那一次 400 字新闻稿的账本是：
//
//	[  1.4s] 首片材料（分类器的思考链）
//	[  6.8s] 分类结束，下发步骤拆解
//	[ 61.5s] 第一个正文字才到  ← 中间 54.7 秒只有心跳在跳秒
//
// 这 54.7 秒里 backend 什么都没发生吗？不是——我们把技能提示词、要素、会话上文、
// 手册范文装成了一个几万字的 prompt 送出去了。真正的问题是**这件事没有任何地方
// 报出来**，所以屏幕上只有计时器在动。
//
// 指望模型侧给材料是不可靠的：provider 不推 reasoning 时（实测 reasoning 片数=0），
// reasoningSink 一片都收不到，material 恒空。所以这里改用我们手里**本来就有**的
// 确定性事实来当材料——技能名、要素项数、上文条数与字数、手册分类与范文篇数。
// 用户看到的是「它拿什么在写」，而不是「它花了多久」。
type assemblyFacts struct {
	Skill     string            // 技能名，空 = 未命中技能
	Args      map[string]string // 要素提炼结果
	HistMsgs  int               // 会话上文条数
	HistChars int               // 会话上文字数（rune）
	Category  string            // 手册模式命中的写作类别，可空
	Examples  int               // 该类范文篇数
	HasReview bool              // 手册里有没有审稿清单
}

// maxNoteArgs 是材料里最多列几个要素名。
// 材料留尾 160 字（materialCap），列满十几个键会把真正该看见的「上文多少字」挤掉。
const maxNoteArgs = 6

// note 把事实拼成一句人话。**必须**非空：它是「分类回来到首字之间」唯一的可见物，
// 返回空串等于把那几十秒又还给计时器。
func (f assemblyFacts) note() string { return f.noteDoing("正在起草…") }

// noteDoing 同 note，但把结尾那半句换成这条链路**实际在做的事**。
//
// 为什么不留一个固定尾巴：docgen 那一跳是在生成 .docx 文件，屏幕上却写「正在起草…」，
// 用户会去找一篇不存在的正文；反过来写文书时说「正在生成文档…」也一样是骗人。
// 尾巴是唯一随链路变化的部分，所以只让尾巴可换。
func (f assemblyFacts) noteDoing(tail string) string {
	if strings.TrimSpace(tail) == "" {
		tail = "正在起草…" // 空尾巴 = 又只剩计时器，退回默认
	}
	parts := make([]string, 0, 5)
	if strings.TrimSpace(f.Skill) != "" {
		parts = append(parts, fmt.Sprintf("技能《%s》", strings.TrimSpace(f.Skill)))
	} else {
		// 没命中技能也要说清楚，否则用户以为「它没管我的要求」。
		parts = append(parts, "未命中技能手册，走通用写作")
	}
	if c := strings.TrimSpace(f.Category); c != "" {
		parts = append(parts, fmt.Sprintf("类别《%s》", c))
		if f.Examples > 0 {
			parts = append(parts, fmt.Sprintf("范文 %d 篇", f.Examples))
		}
		parts = append(parts, fmt.Sprintf("审稿清单 %s", yesNo(f.HasReview)))
	}
	if s := argsBrief(f.Args); s != "" {
		parts = append(parts, s)
	}
	if f.HistMsgs > 0 {
		parts = append(parts, fmt.Sprintf("会话上文 %d 条 %d 字", f.HistMsgs, f.HistChars))
	}
	return "已装配：" + strings.Join(parts, "｜") + "｜" + tail
}

// argsBrief 列出要素名（不是值——值可能很长，而且用户自己刚说过，没必要复述）。
func argsBrief(args map[string]string) string {
	if len(args) == 0 {
		return ""
	}
	keys := make([]string, 0, len(args))
	for k, v := range args {
		// 空值要素对模型是噪音，对用户也是：列出来只会让人以为它拿到了东西。
		if strings.TrimSpace(v) == "" {
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys) // map 遍历顺序随机，排序才能让同一轮的表象可复现（也才能测）
	total := len(keys)
	more := ""
	if total > maxNoteArgs {
		more = fmt.Sprintf(" 等 %d 项", total)
		keys = keys[:maxNoteArgs]
	}
	return fmt.Sprintf("要素 %d 项（%s%s）", total, strings.Join(keys, "、"), more)
}

// noteFacts 把调用点手里的事实收成一个 assemblyFacts。history 用调用点现成的那份
// （会话窗口）就行：条数/字数差一两条不影响用户对「它拿什么在写」的判断。
// sc 可为 nil（没命中技能）；手册模式下 Category / 范文 / 审稿清单由调用点补上。
func noteFacts(sc *agent.SkillContent, args map[string]string, history []agent.Message) assemblyFacts {
	f := assemblyFacts{Args: args, HistMsgs: len(history)}
	for _, m := range history {
		f.HistChars += runeLen(m.Content)
	}
	if sc != nil {
		f.Skill = sc.Name
		if strings.TrimSpace(f.Skill) == "" {
			f.Skill = sc.Slug
		}
	}
	return f
}

func yesNo(b bool) string {
	if b {
		return "有"
	}
	return "无"
}
