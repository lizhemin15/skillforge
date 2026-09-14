package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lizhemin15/skillforge/internal/llm"
)

// 对话上下文管理：**不截断，交给大模型滚动压缩**。
//
// 老实现（compactHistory）对每条消息一律「只留头 400 字」。这种截断有三个硬伤，
// 都是用户真实踩到的：
//  1. 「先写新闻稿，再让它整理成 word」——新闻稿正文在注入前就被砍到 400 字，
//     模型看不到稿子，只能另起炉灶写一篇新的。用户的原话是「通常没有管之前
//     的生成内容」；
//  2. 只保头不保尾。文档的结论、备注、签字栏往往在末尾，砍掉尾部等于把结论
//     删了；
//  3. 砍多砍少全凭一个常量，跟内容重要性无关：一段寒暄和一份正式函件在它眼里
//     一样值 400 字。
//
// 所以这里改成三层结构，**按「信息价值」而不是「消息条数」来分配上下文额度**：
//
//	A. 产物层（原文，不压缩）：模型之前生成的文章/文档/已填字段是后续所有
//	   「改成…/整理成…/导出成…」的指代对象，必须逐字保留——压缩成摘要就等于
//	   让模型拿着二手描述去重构原文，必然走形。这层的额度按「字符预算」控制，
//	   超预算时**丢最旧的产物、保最新的**（用户几乎总是指代最近那几轮的结果）。
//	B. 前情层（大模型压缩，一次压缩多轮复用）：较早的对话交给模型压成要点，
//	   只留事实（目标/已确认的参数与数字/待办/偏好），丢掉寒暄与重复。
//	   压过的不再压：靠 watermark 缓存，避免每轮多一次调用。
//	        这里刻意**不压产物层**——摘要里的数字/名目一旦被模型改写（把 8000
//	   写成「约八千」），填表时就会写进正式文档。
//	C. 近轮层（原文+头尾截断）：最近几轮原样注入，只对超长单条做头尾截断，
//	   因为指代词（「上面那个」「照这个改」）都指向最近几轮。
//
// 压缩调用失败时退回「头尾截断」而不是报错：上下文退化也必须能继续对话，
// 但退化要留痕（stderr + 注入块里的一句声明），别让模型和用户都不知道发生过降级。

const (
	// KindArtifact 标记「这条消息是上一轮的产物（文章/文档/已填值）」。
	// 单独建字段而不是靠扫正文关键词，是因为关键词会随文案变化而漏——
	// 判断依据必须是结构化事实（详见下方 isArtifactMsg 的历史兼容分支）。
	KindArtifact = "artifact"

	// compactKeepRecent 最近 N 条对话逐字保留（指代词的落点都在这里）。
	compactKeepRecent = 6
	// compactTriggerRunes 旧对话超过这个量级才触发压缩调用。
	// 小会话（几句闲聊）根本没多少内容，为它多打一次模型是纯浪费。
	compactTriggerRunes = 4000
	// compactSlack 旧对话又攒够这么多条才重新压缩一次。
	// 每轮都压等于把调用量翻倍，而旧对话多两条对摘要几乎没有增量。
	compactSlack = 4
	// compactArtifactRunes 产物层总预算（字符）。超了丢最旧的产物。
	// 定在 20000 是因为一份正式文档（公文/合同/长文）大概 3000~8000 字，
	// 够容纳最近两三份产物原文。
	compactArtifactRunes = 20000
	// compactMsgRunes 单条消息在「近轮层」的额度（超过则头尾截断）。
	compactMsgRunes = 600
	// compactTimeout 压缩调用的超时。压不动就走截断兜底，绝不让用户等在这里。
	compactTimeout = 30 * time.Second
)

// compaction 是一次压缩的结果 + 覆盖范围（watermark）。
type compaction struct {
	Upto int       `json:"upto"` // 已压缩到 older 的下标（不含）
	Text string    `json:"text"` // 压缩后的要点
	At   time.Time `json:"at"`
}

// compactSumSys 压缩用的系统提示。
//
// 写成「只留事实」而不是「总结一下」：后者会产出读后感式的文字（「用户希望
// 写一篇新闻稿，AI 完成了它」），既占字数又丢事实。要的是能直接当参数用的
// 句子（「单位=XX公司；日期=2026-09-14；篇幅=800字」）。
const compactSumSys = `你是对话压缩器。把较早的对话压成要点，供后续轮次继续工作使用。

只输出要点列表（每行一条，以 "- " 开头），不要标题、不要解释、不要建议、不要寒暄。

必须保留（有则写，没有不提，**不许编造**）：
- 用户的目标与已确认的写作/文档要求（主题、篇幅、语气、格式、分类）
- 已确认的具体事实与参数：单位/人名/日期/金额/数量/编号/型号等**原样照抄**
- 已经产出过什么（文章标题、文档名、已填字段名）
- 用户明确表达的偏好与禁忌（如「不要用古文风格」「要黑白灰简约」）
- 尚未完成的待办与用户最后提出的问题

必须丢掉：礼貌用语、重复表述、模型的自我评价与建议、与后续工作无关的闲聊。`

// ---------------------------------------------------------------------------
// 对外入口
// ---------------------------------------------------------------------------

// ContextBlock 把一次对话的历史渲染成可注入 prompt 的上下文块，
// 承担「产物保原文 + 旧对话模型压缩 + 近轮保真」三件事。
//
// id 为会话 id（压缩结果按会话缓存）；id 为空时退化为「不压缩」（压了也无处缓存，
// 每轮重压纯属浪费），此时仅做本文档内的分层与头尾截断。
func (e *Engine) ContextBlock(ctx context.Context, id string, history []Message) string {
	if len(history) == 0 {
		return "（无历史）"
	}

	artifacts, dialogue := splitArtifacts(history)

	// 近轮层：对话尾部逐字保留；其余进「旧对话」，可能被压。
	recentStart := len(dialogue) - compactKeepRecent
	if recentStart < 0 {
		recentStart = 0
	}
	older, recent := dialogue[:recentStart], dialogue[recentStart:]

	var b strings.Builder

	// A. 产物层（原文）
	if block := renderArtifacts(artifacts); block != "" {
		b.WriteString("【已有产物（原文，逐字保留）】\n")
		b.WriteString("下面是你之前已经生成并交付给用户的内容。用户说「改成…/整理成…/导出成…/继续写」时，")
		b.WriteString("**指的就是这些内容**——必须以此为基础修改，不要另起炉灶重写，也不要凭记忆复述。\n\n")
		b.WriteString(block)
		b.WriteString("\n")
	}

	// B. 前情层（压缩或截断兜底）
	if len(older) > 0 {
		b.WriteString("【前情摘要（较早对话，已压缩，仅供理解背景）】\n")
		b.WriteString(e.compressedOlder(ctx, id, older))
		b.WriteString("\n\n")
	}

	// C. 近轮层（原文，超长头尾截断）
	if len(recent) > 0 {
		b.WriteString("【最近对话（原文）】\n")
		for _, m := range recent {
			b.WriteString(renderMsg(m, compactMsgRunes))
		}
	}

	out := strings.TrimRight(b.String(), "\n")
	if out == "" {
		return "（无历史）"
	}
	return out
}

// ---------------------------------------------------------------------------
// A. 产物层
// ---------------------------------------------------------------------------

// splitArtifacts 把历史切成「产物」和「对话」两路，保持各自原有顺序。
//
// 产物不参与压缩、也不参与近轮截断：它是**证据**，不是叙述。
func splitArtifacts(history []Message) (artifacts, dialogue []Message) {
	for _, m := range history {
		if isArtifactMsg(m) {
			artifacts = append(artifacts, m)
			continue
		}
		dialogue = append(dialogue, m)
	}
	return artifacts, dialogue
}

// isArtifactMsg 判断一条消息是否为「上一轮的产物」。
//
// 判据分两路，缺一不可：
//   - 结构化标记 Kind == artifact（新写入的消息走这条）；
//   - 历史兼容：会话是从磁盘恢复的，早先落盘的消息没有 Kind 字段，但
//     「已生成规格:」「已填值:」这两个标记是当时的契约；再退一步，
//     带 SkillSlug 且正文达到长文量级的 assistant 消息也按产物对待——
//     它就是技能生成出来的那篇文章，用户下一轮指代的对象正是它。
//
// 不做关键词扫描「正文里有没有『新闻稿』」：文案一变就漏，漏了就是用户
// 又一次看到「它不管我上一轮写的东西」。
func isArtifactMsg(m Message) bool {
	if m.Kind == KindArtifact {
		return true
	}
	if !strings.EqualFold(strings.TrimSpace(m.Role), "assistant") {
		return false
	}
	c := m.Content
	if strings.Contains(c, "已生成规格:") || strings.Contains(c, "已填值:") {
		return true
	}
	// 技能产出的长文：带 slug 且正文够长（短回答只是闲聊，不占产物额度）。
	if strings.TrimSpace(m.SkillSlug) != "" && len([]rune(strings.TrimSpace(c))) >= 200 {
		return true
	}
	return false
}

// renderArtifacts 渲染产物层，**从最新往旧填额度**：超预算时丢最旧的。
// 用户续改时指代的几乎总是最近那份产物，旧产物被挤出去是正确取舍。
func renderArtifacts(artifacts []Message) string {
	if len(artifacts) == 0 {
		return ""
	}
	left := compactArtifactRunes
	var blocks []string
	for i := len(artifacts) - 1; i >= 0; i-- {
		m := artifacts[i]
		body := strings.TrimSpace(m.Content)
		if body == "" {
			continue
		}
		// 单份产物超预算时按头尾截断，而不是整份丢掉：宁可能看到大部分正文，
		// 也不要「上一轮产物完全消失」那种最坏体验。
		body = truncHeadTail(body, left)
		used := len([]rune(body))
		if used > left {
			used = left
		}
		left -= used
		blocks = append(blocks, "--- 产物（"+artifactLabel(m)+"）---\n"+body+"\n")
		if left <= 0 {
			break
		}
	}
	if len(blocks) == 0 {
		return ""
	}
	// 反转为「旧→新」，与时间顺序一致，避免模型把倒数第二份当成最新那份。
	for i, j := 0, len(blocks)-1; i < j; i, j = i+1, j-1 {
		blocks[i], blocks[j] = blocks[j], blocks[i]
	}
	return strings.Join(blocks, "\n")
}

// ArtifactKind 给「没有命中技能」的普通对话产出打标：够长的才算产物。
//
// 为什么普通对话也要认产物：「写一篇新闻稿」这类请求有时没匹配到任何写作技能，
// 就由普通对话直接产出正文；用户下一轮说「整理成 word」时，指代的就是它。
// 门槛定 200 字，是为了不把闲聊短答也算成产物去挤占产物层额度。
func ArtifactKind(content string) string {
	if len([]rune(strings.TrimSpace(content))) >= 200 {
		return KindArtifact
	}
	return ""
}

// artifactLabel 给产物打个能认出来的标签（技能名 > 类型），
// 让模型知道「这是哪份产物」，避免多份产物之间张冠李戴。
func artifactLabel(m Message) string {
	if s := strings.TrimSpace(m.SkillSlug); s != "" {
		return s
	}
	return "上一轮生成"
}

// ---------------------------------------------------------------------------
// B. 前情层：大模型压缩（带 watermark 缓存）
// ---------------------------------------------------------------------------

// compressedOlder 返回旧对话的压缩结果；未达触发阈值时直接原文（做头尾截断）。
func (e *Engine) compressedOlder(ctx context.Context, id string, older []Message) string {
	raw := renderDialogue(older, compactMsgRunes)
	if len([]rune(raw)) <= compactTriggerRunes {
		// 还没到需要压缩的量级：原文注入更保真，也省一次模型调用。
		return raw
	}

	// 缓存判定：压过且增量不够多就复用（见 compactSlack 注释）。
	if id != "" {
		if c := e.cachedCompaction(id); c != nil && c.Upto > 0 && c.Upto >= len(older)-compactSlack {
			return c.Text
		}
	}

	sum := e.summarizer()
	if sum != nil && id != "" {
		pctx, cancel := context.WithTimeout(ctx, compactTimeout)
		defer cancel()
		if text, err := sum(pctx, raw); err == nil {
			text = strings.TrimSpace(text)
			if text != "" {
				e.storeCompaction(id, &compaction{Upto: len(older), Text: text, At: time.Now()})
				return text
			}
		} else {
			// 退化必须留痕：日志里看得见「压缩失败、本轮按截断处理」。
			fmt.Fprintf(os.Stderr, "[compact] session=%s summarize failed: %v（本轮退回头尾截断）\n", id, err)
			return truncHeadTail(raw, compactTriggerRunes) +
				"\n（注：早期对话过多且压缩失败，此处只保留了首尾）"
		}
	}
	return truncHeadTail(raw, compactTriggerRunes) +
		"\n（注：早期对话过多，此处只保留了首尾）"
}

// summarizer 返回压缩函数。默认走真实 LLM；测试可注入假实现（Engine.summarize）。
func (e *Engine) summarizer() func(context.Context, string) (string, error) {
	if e.summarize != nil {
		return e.summarize
	}
	if e.llm == nil {
		return nil
	}
	l := e.llm
	return func(ctx context.Context, raw string) (string, error) {
		return l.Chat(ctx, compactSumSys, "较早的对话：\n\n"+raw)
	}
}

func (e *Engine) cachedCompaction(id string) *compaction {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.summaries == nil {
		return nil
	}
	return e.summaries[id]
}

// storeCompaction 写缓存。**注意不能持锁调用模型**：压缩可能跑好几秒，
// 持锁会把整个会话读写全堵住（Push/Session 都抢这把锁）。
func (e *Engine) storeCompaction(id string, c *compaction) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.summaries == nil {
		e.summaries = make(map[string]*compaction)
	}
	e.summaries[id] = c
}

// ---------------------------------------------------------------------------
// C. 渲染与截断
// ---------------------------------------------------------------------------

// renderDialogue 逐条渲染对话（同一口径，供压缩输入与原文注入复用）。
func renderDialogue(msgs []Message, cap int) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(renderMsg(m, cap))
	}
	return b.String()
}

// renderMsg 渲染单条消息，超长做头尾截断。
func renderMsg(m Message, cap int) string {
	role := "用户"
	if strings.EqualFold(strings.TrimSpace(m.Role), "assistant") {
		role = "助手"
	}
	body := strings.TrimSpace(m.Content)
	if body == "" {
		body = "（空）"
	}
	return role + "：" + truncHeadTail(body, cap) + "\n"
}

// truncHeadTail 保留首尾、挖掉中间。
//
// **只保头是错的**：一份文档的结论、备注、落款都在尾部，砍掉尾部等于把结论删了，
// 而模型会拿被砍断的开头一路编下去。所以这里两头都保，并在中间明确标注省略了多少字——
// 标注本身就是给模型的信号：这里缺东西，别当作完整文本使用。
func truncHeadTail(s string, max int) string {
	r := []rune(s)
	if max <= 0 || len(r) <= max {
		return s
	}
	head := max * 2 / 3
	tail := max - head
	if head < 1 {
		head = 1
	}
	if tail < 1 {
		tail = 1
	}
	if head+tail >= len(r) {
		return s
	}
	omitted := len(r) - head - tail
	return string(r[:head]) +
		fmt.Sprintf("\n…（此处省略约 %d 字）…\n", omitted) +
		string(r[len(r)-tail:])
}

// compactHistory 历史遗留入口：老代码里那些「没有会话 id、也不该为此多打一次
// 模型」的地方（工具循环内的中间提示等）仍旧用它。
//
// 它保留分层与头尾截断，只是不做模型压缩——需要模型压缩的路径一律改走
// ContextBlock（见 chat.go / agent.go 的调用点）。
func compactHistory(history []Message) string {
	if len(history) == 0 {
		return "（无历史）"
	}
	artifacts, dialogue := splitArtifacts(history)
	var b strings.Builder
	if block := renderArtifacts(artifacts); block != "" {
		b.WriteString("【已有产物（原文，逐字保留）】\n" + block + "\n")
	}
	recentStart := len(dialogue) - compactKeepRecent*2
	if recentStart < 0 {
		recentStart = 0
	}
	b.WriteString(renderDialogue(dialogue[recentStart:], compactMsgRunes))
	return strings.TrimRight(b.String(), "\n")
}

var _ = llm.Client{} // 保持 import（summarizer 依赖 *llm.Client 的签名）
