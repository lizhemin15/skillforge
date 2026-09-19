package api

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/lizhemin15/skillforge/internal/agent"
)

// classifyNarration 生成「① 意图分析」这一跳的**本地旁白**。
//
// 为什么单独给这一跳配旁白：它是用户按下发送之后挡在第一位的那一跳，而它的材料
// 来源有两个，且**都可能一片都没有**：
//   - 思考链：这一跳明确设了 enable_thinking=false（正常 4.8s 就回来），
//     provider 守规矩时 reasoning 片数 = 0；
//   - 流式 JSON 正文（contentSink 抽 reason 字段）：provider 若整段缓冲，
//     在返回之前一片都不推。
//
// 线上实测（2026-09-19，docgen leg）：这一跳跑了 54.0s 而**屏幕上一片材料都没有**，
// 用户看到的只有不断 +1 的「已用 Ns」。用户原话就是「现在速度过于慢了，中间可以流式
// 输出思考的一些中间材料，现在一直卡着计时」—— 那段 54s 静默就发生在这里。
//
// 修法不是造假流：这里的每一行都是**我们手里已经有的真值**（本轮消息字数、会话里
// 有几条用户素材、多少轮对话）。模型一开口，Narrate 内部的让路判断就不插话了。
//
// 输出顺序有讲究：第一行必须是最稳、最不会变的那句（它会在进跳瞬间就下发，是
// 用户在整个分类阶段看到的第一行材料）。
//
// ⚠️ 行首**不许**自带「· 」：Narrate 自己会给每一行补上（chat_trace.go 的
// `c.Thinking("· " + clean[i])`）。带了就是「· · 收到你的需求」，界面上双点。
// 这条被自证脚本钉住（注入了双点的坏法必须变红）。
func classifyNarration(msg string, history []agent.Message) []string {
	out := []string{"收到你的需求（本轮 " + itoa(utf8.RuneCountInString(strings.TrimSpace(msg))) + " 字）"}

	mats, matRunes, turns := 0, 0, 0
	for _, m := range history {
		switch {
		case m.Kind == agent.KindMaterial:
			mats++
			matRunes += utf8.RuneCountInString(m.Content)
		case strings.TrimSpace(m.Role) == "user":
			turns++
		}
	}
	if mats > 0 {
		out = append(out, fmt.Sprintf("已带上你给的 %d 段素材（共 %d 字）一起判定", mats, matRunes))
	}
	if turns > 0 {
		out = append(out, fmt.Sprintf("还带着之前 %d 轮对话（承接上文的改法要看得见才对）", turns))
	}
	if mats == 0 && turns == 0 {
		out = append(out, "这是本轮第一句话，没有上文要承接")
	}
	// 最后一行说清「这一步在决定什么」：用户能理解为什么它值得等。
	out = append(out, "正在定意图与要调的能力，它决定后面走哪条链路")
	return out
}

// itoa 是本文件唯一需要的小整数格式化：避免为一行文案引入 strconv 与 fmt 两种写法。
func itoa(n int) string { return fmt.Sprintf("%d", n) }
