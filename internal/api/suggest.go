package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lizhemin15/skillforge/internal/agent"
)

// 「推荐行」（chips）的服务端实现。
//
// 产品形态（用户拍板）：首页极简到只剩三处——推荐行 + 模式胶囊 + 发送按钮。
// 推荐行是唯一「主动说话」的地方，所以它的内容必须跟着对话变：
// 用户刚出完一份通知，推荐行该给的是「再精简一版 / 换成表格」，不是固定的
// 「写通知 / 写总结 / 写周报」。**规则版（前端 chipPlan）只能按关键词猜**，
// 它手里只有消息文本，没有「这个站到底能干什么」的语义。这里补的就是那一层。
//
// 契约（前端 web/js/chat.js refineChips 是唯一调用方）：
//
//	请求  {session_id, last_user, last_reply, used_skill, skills:[{slug,name}]}
//	响应  {chips:[{label, send}]}
//
// 三条硬约定：
//  1. **失败一律 200 + {"chips":[]}**，绝不 4xx/5xx。这是 UI 糖不是业务接口：
//     报错只会让浏览器控制台红一片，而用户什么也做不了。空数组 = 「这次没有
//     更好的建议」，前端保留规则版，功能不退化。
//  2. **不落库、不读会话历史、不写会话文件**。它拿到的上下文由前端显式传进来，
//     这样「推荐行看到的东西」和「用户屏幕上有的东西」是同一份，不会出现
//     「它推荐了三轮前的内容」这种莫名其妙。
//  3. **超时 + 并发上限**。公网无鉴权端点，模型调用是花钱的：默认 5s 超时
//     （前端 6s 就 abort 了，服务端再算就是白烧 token），并发满了直接回空数组。

// suggestChip 是推荐行上的一颗胶囊。
//
// label / send 分开的理由：胶囊要挤在一行里显示 4 颗，**标签必须短**；
// 但真正点下去发给模型的句子不能短，短了就丢掉了「刚才那版」「800 字」
// 这类指代，助手只能反问「哪一版？」。所以 label 给人看，send 给模型看，
// 悬停 title 显示 send（用户点之前就能知道会发生什么）。
type suggestChip struct {
	Label string `json:"label"`
	Send  string `json:"send"`
}

type suggestSkill struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type suggestReq struct {
	SessionID string         `json:"session_id"`
	LastUser  string         `json:"last_user"`
	LastReply string         `json:"last_reply"`
	UsedSkill string         `json:"used_skill"`
	Skills    []suggestSkill `json:"skills"`
}

type suggestResp struct {
	Chips []suggestChip `json:"chips"`
}

const (
	// suggestMaxChips：推荐行最多 4 颗——一行的容量。多出来的第 5 颗会换行，
	// 而换行意味着「极简」这件事已经失败了。
	suggestMaxChips = 4
	// suggestLabelRunes：label 的视觉上限（汉字按 1 计）。超了截断而不是丢弃：
	// 截断还是一条可点的建议，丢弃等于模型白答。
	suggestLabelRunes = 10
	// suggestReplyRunes：喂给模型的上一轮回复上限。推荐行是「便宜」的东西，
	// 长回复（尤其是带表格的交付说明）整段塞进去会让这个调用变贵十倍，
	// 而它只需要知道「刚才干了什么」——前 600 字足够，尾部反而多是客套话。
	suggestReplyRunes = 600
)

// suggestHandler 是 POST /api/chat/suggest 的处理器。
type suggestHandler struct {
	eng     *agent.Engine
	timeout time.Duration
	// sem 是并发闸门（非阻塞）：这是无鉴权的公网端点，一次请求一次模型调用，
	// 没有闸门时「按住 F5 刷新」就是免费烧钱器。排队是**错的**——推荐行排队
	// 到 30 秒后再出现，不如这次不出现。
	sem chan struct{}
}

func newSuggestHandler(eng *agent.Engine, timeout time.Duration) *suggestHandler {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &suggestHandler{eng: eng, timeout: timeout, sem: make(chan struct{}, 4)}
}

func (h *suggestHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 缓存一律关掉：这是「依据当前对话」生成的，被 CDN 或浏览器缓存下来
	// 就会出现「换个话题推荐行还是老的」这种幽灵 bug。
	w.Header().Set("Cache-Control", "no-store")

	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}

	var req suggestReq
	if r.Method == http.MethodPost {
		// 宽松解析（**不用 readBody**）：那边 DisallowUnknownFields，多一个字段
		// 就 400。前端以后加字段（比如带上附件名）是为了让建议更准，
		// 结果整个推荐行静默消失——这种合约不该用「宁可失败」的严格模式。
		dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
		_ = dec.Decode(&req) // 坏 body 当作空请求，下面照样回空数组
		defer r.Body.Close()
	} else {
		q := r.URL.Query()
		req.SessionID = q.Get("session_id")
		req.LastUser = q.Get("last_user")
		req.LastReply = q.Get("last_reply")
		req.UsedSkill = q.Get("used_skill")
	}

	h.serve(w, r.Context(), req)
}

func (h *suggestHandler) serve(w http.ResponseWriter, ctx context.Context, req suggestReq) {
	// 两条短路**必须分开写**，不要合成一个 `||`：
	//   - 空输入＝「这次不该问模型」，和有没有模型无关；
	//   - 没配模型＝「想问也问不了」，和用户说没说话无关。
	// 合成一条的代价是有实测证据的：把这两个条件用 `||` 连在一起后，
	// 往空输入那一半注入「不过滤」，连带把没模型那条用例也掀翻（走了 nil store 的
	// ensureLLM → 空指针 panic），自证脚本从此分不清是哪一半坏了。
	if strings.TrimSpace(req.LastUser) == "" {
		// 首页刚打开、用户还没发过消息：此时前端要显示的是规则版的「开场白」，
		// 不是模型猜的。这一条也是纯省钱——每次刷新首页都是一次模型调用。
		writeJSON(w, http.StatusOK, suggestResp{Chips: []suggestChip{}})
		return
	}
	if h.eng == nil || !h.eng.HasLLM() {
		// 管理端没填 key（线上常见）或引擎还没起来：照样 200 + 空数组，
		// 前端保留规则版推荐行，功能不退化。
		writeJSON(w, http.StatusOK, suggestResp{Chips: []suggestChip{}})
		return
	}

	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	default:
		// 闸门满了：这次不推荐，不排队。
		writeJSON(w, http.StatusOK, suggestResp{Chips: []suggestChip{}})
		return
	}

	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	sys, user := suggestPrompt(req)
	raw, err := h.eng.FastJSON(ctx, sys, user)
	if err != nil {
		// 超时/未配模型/上游抖动都走这里。**不写 error 字段**：前端只看 chips，
		// 多一个字都是噪音（而且会把「模型没配好」的运维信息暴露给公网用户）。
		writeJSON(w, http.StatusOK, suggestResp{Chips: []suggestChip{}})
		return
	}
	chips := parseSuggestChips(raw, suggestMaxChips)
	writeJSON(w, http.StatusOK, suggestResp{Chips: chips})
}

// suggestPrompt 拼这次调用的两段提示。抽成纯函数是为了能直接测：
// 提示词里最容易悄悄退化的两件事是「忘了带上下文」和「忘了给技能清单」，
// 而它们的表现是「推荐行给出的建议越来越空」，肉眼看界面上看不出来。
func suggestPrompt(req suggestReq) (sys, user string) {
	sys = "你是「SkillForge 智能写作工坊」首页输入框上方推荐行的生成器。\n" +
		"用户在这个站里能做任何文字工作：写公文/材料、改稿子、要一份表格、把长文压成简报、问这个站怎么用。\n" +
		"你的任务：读下面的对话状态，给出 2-4 条**用户此刻最可能想点的下一步**，它们会显示成输入框上方的小胶囊。\n\n" +
		"硬约束：\n" +
		"1. label 不超过 10 个汉字，以动词开头（如「再精简一版」「换成表格」「补一段结论」），结尾不加标点，不加编号。\n" +
		"2. send 是点下去**直接发给助手**的完整句子：要能独立看懂，把具体对象带进去（如「把刚才那版通知再精简到 300 字」）。\n" +
		"3. 必须扣住对话里出现过的具体内容（文种、主题、单位、数字、格式），禁止「帮我写点东西」这类空话。\n" +
		"4. 候选之间要**拉开方向**：一条顺着往下做、一条换形态（表格/PPT/简报）、一条换角度（更正式/更口语/补数据）。\n" +
		"5. 如果上一轮助手的回复在反问用户要材料，**第一条必须是「回答它」**（例如「材料我发你」→ send 写成「我把原始材料发给你」）。\n" +
		"6. 只输出 JSON，不要解释、不要 Markdown 围栏：{\"chips\":[{\"label\":\"…\",\"send\":\"…\"}]}"

	var b strings.Builder
	if len(req.Skills) > 0 {
		b.WriteString("这个站已有的能力（仅帮你理解范围，**不要**把它们当推荐语直接抛给用户）：\n")
		for i, s := range req.Skills {
			if i >= 12 {
				break
			}
			name := strings.TrimSpace(s.Name)
			if name == "" {
				continue
			}
			b.WriteString("- " + name + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("对话状态：\n")
	b.WriteString("- 用户刚说：" + clampRunes(strings.TrimSpace(req.LastUser), 200) + "\n")
	if strings.TrimSpace(req.LastReply) == "" {
		b.WriteString("- 助手还没回复（这是对话的第一轮）\n")
	} else {
		b.WriteString("- 助手刚答：" + clampRunes(strings.TrimSpace(req.LastReply), suggestReplyRunes) + "\n")
	}
	if strings.TrimSpace(req.UsedSkill) != "" {
		b.WriteString("- 上一轮走的是技能：" + strings.TrimSpace(req.UsedSkill) + "\n")
	}
	b.WriteString("\n按硬约束给出 2-4 条建议，只回 JSON。")
	return sys, b.String()
}

// parseSuggestChips 把模型回的一坨文本变成能用的胶囊清单。
//
// 为什么必须宽容到这种程度（这里每一行都对应线上会遇到的形态）：
//   - 模型把 JSON 包在 ```json 围栏里、或前面加一句「好的，这是建议：」；
//   - 该回的 {"chips":[...]} 回成了裸数组 [...]；
//   - label 里带引号/编号/末尾句号，或者干脆是字符串 "undefined"（前端 filter
//     只能滤空值，滤不掉这个词，于是界面上真的出现过一颗写着 undefined 的胶囊）；
//   - 同一个建议重复两次（模型自己数不清）。
//
// 语义：**能救则救，救不回来就少给一条**。一颗坏胶囊上屏比少一颗胶囊糟得多——
// 用户点了一颗写着 undefined 的按钮，得到的是助手的一句「请问您想做什么？」
func parseSuggestChips(raw string, max int) []suggestChip {
	if max <= 0 {
		max = suggestMaxChips
	}
	body := extractJSONBody(raw)
	if body == "" {
		return nil
	}
	var obj struct {
		Chips []suggestChip `json:"chips"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil || len(obj.Chips) == 0 {
		// 裸数组形态：模型偶尔直接回 [{"label":…}]。
		var arr []suggestChip
		if err2 := json.Unmarshal([]byte(body), &arr); err2 != nil {
			return nil
		}
		obj.Chips = arr
	}

	out := make([]suggestChip, 0, max)
	seen := make(map[string]bool, max)
	for _, c := range obj.Chips {
		label := cleanChipLabel(c.Label)
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		send := strings.TrimSpace(c.Send)
		if send == "" {
			send = label
		}
		out = append(out, suggestChip{Label: label, Send: clampRunes(send, 120)})
		if len(out) >= max {
			break
		}
	}
	if len(out) == 0 {
		// 返回 nil 而不是空 slice：JSON 里就是 []（前端两种都当「没有建议」）。
		return nil
	}
	return out
}

// cleanChipLabel 把一条 label 洗成能上屏的样子。返回空串表示「这条不要」。
func cleanChipLabel(s string) string {
	t := strings.TrimSpace(s)
	// 引号/书名号是模型爱加的包装；编号与 markdown 强调是它爱加的装饰。
	t = strings.Trim(t, "\"'“”‘’《》")
	t = strings.TrimPrefix(t, "- ")
	t = strings.TrimSpace(t)
	// 去掉 "1. " / "2、" 这类序号。**必须按字符比**（中文标点 3 字节，按字节比会溢出）；
	// 而且要求序号后面不是数字——否则「1.5 倍」会被砍成「5 倍」，改错了意思。
	if r := []rune(t); len(r) > 2 && r[0] >= '1' && r[0] <= '9' &&
		(r[1] == '.' || r[1] == '、' || r[1] == ')' || r[1] == '）') &&
		!(r[2] >= '0' && r[2] <= '9') {
		t = strings.TrimSpace(string(r[2:]))
	}
	t = strings.Trim(t, "*`_")
	t = strings.TrimSpace(t)
	t = strings.TrimRight(t, "。.！!？?，,")
	t = strings.TrimSpace(t)

	// 空值/占位符：模型在「没什么好建议」时会老实写出这些东西，
	// 它们是**字符串**，前端只能靠这里挡掉。
	switch strings.ToLower(t) {
	case "", "undefined", "null", "nil", "n/a", "none", "无":
		return ""
	}
	// 模型把 schema 本身回了出来（label 字段里带 JSON 语法）→ 整条丢弃。
	if strings.ContainsAny(t, "{}[]") || strings.Contains(t, "label") && strings.Contains(t, "send") {
		return ""
	}
	return clampRunes(t, suggestLabelRunes)
}

// extractJSONBody 从模型的原始回复里挖出 JSON 主体。
// 顺序：先整体试（jsonMode 下最常见），再去 Markdown 围栏，最后按首尾括号切片。
func extractJSONBody(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if json.Valid([]byte(s)) {
		return s
	}
	// ```json … ``` / ``` … ```
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		rest = strings.TrimPrefix(rest, "json")
		rest = strings.TrimPrefix(rest, "JSON")
		if j := strings.Index(rest, "```"); j >= 0 {
			rest = rest[:j]
		}
		rest = strings.TrimSpace(rest)
		if json.Valid([]byte(rest)) {
			return rest
		}
		s = rest
	}
	// 兜底：从第一个 { 或 [ 切到最后一个 } 或 ]。
	// 用「最后一个」而不是「第一个匹配的右括号」——嵌套 JSON 里第一个 } 是内层的，
	// 切早了会得到半截 JSON，解析失败后整批建议全丢。
	start := strings.IndexAny(s, "{[")
	if start < 0 {
		return ""
	}
	end := strings.LastIndexAny(s, "}]")
	if end <= start {
		return ""
	}
	body := strings.TrimSpace(s[start : end+1])
	if !json.Valid([]byte(body)) {
		return ""
	}
	return body
}

// clampRunes 按字符（不是字节）截断——中文一个字 3 字节，用字节截会切出乱码。
func clampRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
