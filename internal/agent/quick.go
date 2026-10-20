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
		// 措辞：早先是「你只需回应最新用户消息」—— 那句话把模型推向了「每条消息都是
		// 一个独立请求」，于是用户在自检追问后回一句「张老师」，它就把这三个字当成
		// 新题目重写（实测输出「张老师人物素描」构想）。用户原话：「我补充了，它似乎
		// 把我补充的话当成了第一句话，忘了我最开始说的以及它自己答的东西了。」
		//
		// 现在明确两件事：(1) 先读前情，弄清「这一篇」是什么；(2) 本轮消息多半是补细节，
		// 短不等于换题目。真正把「本轮是补充」钉死的判据不由这里负责（那是分类器与
		// 构思跳的事），这一句只负责不误导。
		combined = "【前情（本次会话已发生的内容）】\n" + histBlock + "\n\n" +
			"【请先看清前情】用户前面在做什么（例如在写哪一份通知/哪一篇稿），" +
			"本轮消息多半是在**给那件事补一个细节**（答一个人名、一个日期、一个地址）。\n" +
			"本轮消息很短不代表换了题目：除非用户明说要改题，否则请继续把前情里那一篇写完。\n\n" +
			"【本轮用户消息】\n" + user
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
3. 每条 ask 都必须是用户能直接回答的问题（例如「目标读者是谁」），不能是「写得更好一点」这类无法执行的空话。
4. **每条问题对应每条 ask，不许复用**：几条不足共用一个 ask（例如三条都写「请提供时间、地点」），
   用户会拿到一张对不上的条子。特别地——
   - 文风类问题（开场白、收尾语、分条、篇幅）**不是用户能补的信息**：这类条目的 ask 写
     「无需补充，下一版我会改正」，不要硬塞一个问题。
   - 只有「缺具体值」（人名/日期/数字/单位）的条目，ask 才去问那个值。

【关于【待补充：xxx】占位符 —— 这条最容易判错，按硬规则执行】
写作要求允许甚至要求用【待补充：xxx】标注用户没给的信息，所以：
- 成稿里**有**占位符 ⇒ **不算**「未完成」「不可直接使用」「缺少信息」，**不得**据此判不达标。
  占位符是按要求做的正确行为，不是缺陷。
- 成稿里**该有占位符的地方凭空编了具体值**（用户没给的人名、日期、数字、单位、金额）⇒
  这**是**缺陷，判不达标，ask 要问出那个值。
- 两种判法**不得在同一份成稿上同时使用**：不能说「占位符太多」，又不能说「这里没用占位符」。
  要么它按要求标了（通过），要么它没标而编了（不通过）。

【关于「用户没提供的信息」】
那是需求侧欠的账，不是成稿的缺陷。判的是：**用户给了的信息用对了吗？该占位的地方占位了吗？**
不是「用户给的信息全不全」。`

// QuickReview 阶段二：对照系统提示词自检成稿。
//
// 独立上下文（与 Review 同一取舍）：**不带起草时的模型自述**，避免起草者给自己的稿子
// 放行；但**必须带用户自己说过的话**（会话历史）——那是判据，不是自述。
//
// 为什么历史必须进（实测矛盾）：多轮时用户先要「员工培训通知」，再补一句「张老师」。
// 自检只收到 user=「张老师」，于是拿这三个字当全部需求去审一整篇通知，判出
// 「成稿自行编造了'员工技能培训'这一主题」——用户明明第一轮就说了。这类误判会让
// 自检在补充信息的那一轮**反而更不通过**，用户观感就是「我越补它越不满」。
//
// **返回 nil = 视为通过**（见文件头注释的危害不对称分析）：调用方拿不到
// 结论时静默结束，绝不能把「自检环节抖了」升级成用户的报错。
func (e *Engine) QuickReview(ctx context.Context, user, draft, prompt string) *QuickVerdict {
	return e.QuickReviewWithHistory(ctx, "", user, draft, prompt, nil)
}

// QuickReviewWithHistory 同 QuickReview，额外把会话历史作为「用户已提供的信息」交给审稿人。
func (e *Engine) QuickReviewWithHistory(ctx context.Context, sessionID, user, draft, prompt string, history []Message) *QuickVerdict {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var b strings.Builder
	fmt.Fprintf(&b, "## 写作要求\n\n%s\n\n", strings.TrimSpace(prompt))
	// 会话历史：只取**用户说过的部分**。助手自己的话不进判据（那是自述，会让起草者
	// 给自己的稿子放行），但用户之前提的要求必须进——它们是「用户已提供的信息」。
	if len(history) > 0 {
		var said []string
		for _, m := range history {
			if !strings.EqualFold(strings.TrimSpace(m.Role), "user") {
				continue
			}
			if t := strings.TrimSpace(m.Content); t != "" {
				said = append(said, t)
			}
		}
		if len(said) > 0 {
			fmt.Fprintf(&b, "## 用户在本次会话里先后提出的要求（按时间顺序，都是已提供的信息）\n\n")
			for i, t := range said {
				fmt.Fprintf(&b, "%d. %s\n", i+1, t)
			}
			b.WriteString("\n（注意：最后一条往往只是对前文的补充，如只答了一个人名/日期。判断「用户提供了什么」要看这一整段，不能只看最后一条。）\n\n")
		}
	}
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
