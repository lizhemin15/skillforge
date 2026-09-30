package agent

// 极速写作模式（首页「快速开始」档）。
//
// 与自动调度/指定技能两档的关系：那两档是「多智能体调度」——意图分类、技能
// 路由、工具调用一整套。极速档把这些全部砍掉，只剩两跳：
//
//	阶段一 QuickDraft  —— 拿管理端配置的系统提示词当写作要求，直接起草。
//	阶段二 QuickReview —— 拿**同一份提示词**当评分标准，判断成稿是否达标；
//	                     达标 → 结束；不达标 → 告诉用户「目前差在哪 + 要补什么」。
//
// 阶段二的评审对像是成稿而不是用户：用户拿到的永远是完整的稿子 + 一段
// 「哪里还欠着」的清单，绝不会出现「审稿没过所以稿子不给你看」。
//
// 设计取舍（为什么自检失败一律当通过）：
//
//	自检是增值项不是闸门。模型抖动 / JSON 解析失败 / 超时时把整轮判死，
//	用户拿到的是一句报错而不是稿子——比少一段「需补充」提示糟得多。
//	危害不对称，所以 err → nil（当 pass 处理），静默结束。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/store"
)

// QuickGap 一条不足：现在差在哪 + 要用户补什么。
type QuickGap struct {
	Problem string `json:"problem"` // 成稿目前的不足（一句话）
	Ask     string `json:"ask"`     // 需要用户补充的信息（一句话，具体可答）
}

// QuickVerdict 自检结论。Pass=true 表示成稿满足系统提示词要求。
type QuickVerdict struct {
	Pass  bool       `json:"pass"`
	Items []QuickGap `json:"items"`
}

// QuickPrompt 读管理端配置的极速写作系统提示词。
//
// **每次现读**而不是启动时读一份：管理端保存后下一轮生成立即生效，
// 这是「改提示词」这个动作用户可感知的唯一方式（不重启、不刷新）。
// 读库失败回落默认值——设置表抖一下不该让整轮生成失败。
func (e *Engine) QuickPrompt() string {
	if e.store == nil {
		return store.DefaultQuickPrompt
	}
	kv, err := e.store.GetSettings(store.SettingQuickPrompt)
	if err != nil {
		return store.DefaultQuickPrompt
	}
	if v := strings.TrimSpace(kv[store.SettingQuickPrompt]); v != "" {
		return v
	}
	return store.DefaultQuickPrompt
}

// quickDraftSys 把管理端提示词包成起草用的 system prompt。
// 管理员配的是「写作要求」，不是完整的对话系统提示——补一句角色定位，
// 避免模型把要求清单原样复述给用户（那等于什么都没写）。
func quickDraftSys(prompt string) string {
	return prompt + "\n\n（以上是站点为你设定的写作要求。你的任务：按最新用户消息直接产出成稿，满足上述全部要求；不要复述要求本身，不要输出与成稿无关的说明。）"
}

// QuickDraft 阶段一：按系统提示词直接起草。
//
// 关思考链（DisableThinking 走 llm.WriteThinkingOff，与技能链路执笔跳同一开关：
// 线上实测 35B-A3B 开思考首正文 195s、关思考 12.5s——极速档的意义就是这段）。
// 多轮续写：历史走 ContextBlock（与 plainChatWithPlan 同一格式），用户补充
// 信息后下一轮能接着改，而不是从零再来。
func (e *Engine) QuickDraft(ctx context.Context, id, user string, history []Message, prompt string, onDelta func(string)) (string, error) {
	sys := quickDraftSys(prompt)
	histBlock := e.ContextBlock(ctx, id, history)
	combined := user
	if len(history) > 0 {
		combined = "对话历史（供参考，你只需回应最新用户消息）：\n" + histBlock + "\n\n最新用户消息：\n" + user
	}
	st := &hopStat{t0: time.Now()}
	out, err := e.llm.StreamChat(ctx, sys, combined, llm.StreamOpts{
		DisableThinking: llm.WriteThinkingOff(),
		OnContent:       st.content(onDelta),
		OnReasoning:     st.reasoning(reasoningSink(ctx)),
		OnNote:          func(s string) { ReportProgress(ctx, "· "+s) },
		OnReset:         ResetOf(ctx),
	})
	st.log("quick-draft", out, err)
	return out, err
}

// quickReviewSys 自检跳的 system prompt。
//
// 关键约束写在提示里而不是靠模型自觉：
//   - 「用户没给的信息」不算不达标——起草要求本来就规定用【待补充：xxx】标注，
//     那是**用户侧**的欠账，不是**稿子**的缺陷。混在一起会把每篇稿都判不达标。
//   - pass=false 必须给 items 且每条都要能落到「让用户补什么」，否则等于
//     给用户一张没法执行的条子。
const quickReviewSys = `你是严格的写作审稿人。下面给你「写作要求」「用户需求」和「成稿」，判断成稿是否满足写作要求。

输出 JSON（不要输出其他任何内容）：
{"pass": true 或 false, "items": [{"problem": "成稿目前的不足，一句话", "ask": "需要用户补充的信息，一句话且具体可答"}]}

判定规则：
1. 逐条核对写作要求。成稿完全达标：{"pass": true, "items": []}。
2. 不达标时 pass=false，items 列 1~3 条最重要的不足，按重要度排序，不要凑数。
3. 「用户没提供的信息」不算成稿的不足——那是需求侧欠的账（成稿应按要求用【待补充：xxx】标注），只判成稿本身是否违反写作要求。
4. 每条 ask 都必须是用户能直接回答的问题（例如「目标读者是谁」），不能是「写得更好一点」这类无法执行的空话。`

// QuickReview 阶段二：对照系统提示词自检成稿。
//
// 独立上下文（与 Review 同一取舍）：不带起草历史，只给「要求 / 需求 / 成稿」，
// 避免起草者给自己的稿子放行。
//
// **返回 nil = 视为通过**（见文件头注释的危害不对称分析）：调用方拿不到
// 结论时静默结束，绝不能把「自检环节抖了」升级成用户的报错。
func (e *Engine) QuickReview(ctx context.Context, user, draft, prompt string) *QuickVerdict {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var b strings.Builder
	fmt.Fprintf(&b, "## 写作要求\n\n%s\n\n", strings.TrimSpace(prompt))
	fmt.Fprintf(&b, "## 用户这次的写作需求\n\n%s\n\n", strings.TrimSpace(user))
	fmt.Fprintf(&b, "## 成稿\n\n%s\n", strings.TrimSpace(draft))

	var out strings.Builder
	_, err := e.llm.StreamChat(cctx, quickReviewSys, b.String(), llm.StreamOpts{
		DisableThinking: true, // 结构化短跳：极速档这一跳不该再等思考链
		JSONMode:        true,
		MaxTokens:       800,
		OnContent:       func(s string) { out.WriteString(s) },
	})
	if err != nil {
		return nil
	}
	v := parseQuickVerdict(out.String())
	if v == nil {
		return nil
	}
	// 说 pass=false 却一条都给不出：审稿人自己都指不出问题，按通过处理。
	if !v.Pass && len(v.Items) == 0 {
		v.Pass = true
	}
	return v
}

// parseQuickVerdict 解析自检跳的 JSON 输出；任何一步失败都返回 nil（=当通过）。
func parseQuickVerdict(out string) *QuickVerdict {
	v := &QuickVerdict{}
	if err := json.Unmarshal([]byte(extractJSON(out)), v); err != nil {
		return nil
	}
	// 清洗：截掉空 problem/ask 的条目，不让「:」式空弹出现在用户气泡里。
	items := v.Items[:0]
	for _, it := range v.Items {
		it.Problem = strings.TrimSpace(it.Problem)
		it.Ask = strings.TrimSpace(it.Ask)
		if it.Problem == "" && it.Ask == "" {
			continue
		}
		items = append(items, it)
	}
	v.Items = items
	return v
}

// QuickReviewNote 把「不达标」渲染成追加在成稿后面的提示段。
// markdown：前端气泡按 markdown 渲染，编号列表自动成形。
func QuickReviewNote(v *QuickVerdict) string {
	if v == nil || v.Pass || len(v.Items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n---\n\n**自检未通过**：这一版还没完全满足站点的写作要求，差这 " +
		fmt.Sprint(len(v.Items)) + " 处：\n\n")
	for i, it := range v.Items {
		fmt.Fprintf(&b, "%d. ", i+1)
		if it.Problem != "" && it.Ask != "" {
			fmt.Fprintf(&b, "%s → **需要你补充**：%s\n", it.Problem, it.Ask)
		} else if it.Problem != "" {
			fmt.Fprintf(&b, "%s\n", it.Problem)
		} else {
			fmt.Fprintf(&b, "**需要你补充**：%s\n", it.Ask)
		}
	}
	b.WriteString("\n把这些信息回复给我，下一版直接落实。")
	return b.String()
}
