package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lizhemin15/skillforge/internal/docgen"
	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/model"
	"github.com/lizhemin15/skillforge/internal/store"
)

// ---------------------------------------------------------------------------
// Engine is the self-contained, single-binary multi-agent chat engine.
// It owns the dialogue loop: intent recognition → skill retrieval → skill
// injection → streaming generation. No external framework dependency.
//
// Design notes (absorbing pi/agent frameworks without the weight):
//   - Session-scoped memory: per-session message history, resumable.
//   - Zero-vector-search retrieval: skills are few, so route intent directly
//     through the LLM against the full skill roster (lightweight & self-contained).
//   - Pure reasoning (intent/slot) uses non-streaming Chat; final generation
//     uses streaming Complete for a responsive UX.
// ---------------------------------------------------------------------------

// Message is one turn of a conversation.
type Message struct {
	Role    string    `json:"role"` // user | assistant
	Content string    `json:"content"`
	At      time.Time `json:"at"`
	// SkillSlug is set on assistant messages that were generated using a skill.
	SkillSlug string `json:"skill_slug,omitempty"`
	// Kind 标记消息的**角色之外的用途**：KindArtifact 表示「这条是上一轮的产物
	// （文章/文档/已填值）」，上下文注入时按原文保留、不参与压缩、不参与截断。
	// 用结构化字段而不是扫正文关键词，是因为关键词随文案一变就漏——漏一次就是
	// 用户又看到「它不管我上一轮生成的东西」。
	Kind string `json:"kind,omitempty"`
}

// Eval is the orchestrator's decision after reading a user turn.
type Eval struct {
	SkillSlug string                 `json:"skill_slug"`       // "" = no skill, plain chat
	Intent    string                 `json:"intent"`           // "write" = writing task, "chat" = casual chit-chat
	Reason    string                 `json:"reason"`           // short human note (skill hit / fallback)
	Params    map[string]interface{} `json:"params,omitempty"` // extracted params (may contain arrays, e.g. columns)
	Needs     []model.Param          `json:"needs,omitempty"`  // skill params the user hasn't filled
	Steps     []TraceStep            `json:"steps,omitempty"`  // per-stage reasoning trace surfaced to UI
	// Action is the concrete tool/routine the orchestrator intends to run for
	// this turn, resolved from intent judgment (NOT string/keyword matching):
	//   "fill"          → fill data into the matched template → FillDoc
	//   "template_only" → deliver the blank template file → attachment download
	//   "gen"           → generate an office file via a docgen skill → GenerateDoc
	//   "write"         → free-form writing via a write skill → Generate
	//   "answer"        → plain chat / QA, no file → Generate (general)
	Action string `json:"action"`
	// NeedsTools 表示这个任务必须先拿到外部实时数据或做真实计算才能完成。
	// 为 true 时走 Agent 工具循环（http_request / run_python），而不是让模型
	// 凭记忆编内容——编造 star 数、销量、汇率这类「看起来合理」的数字是最坏的结果。
	NeedsTools bool `json:"needs_tools"`
}

// TraceStep is one visible stage of the agent's decision pipeline.
// Phase ∈ {analyze, match, params, generate}; Status ∈ {pending,active,done}.
type TraceStep struct {
	Phase  string `json:"phase"`
	Label  string `json:"label"`  // stage heading e.g. 「① 意图分析」
	Detail string `json:"detail"` // one-line human note e.g. 命中：需要一份述职报告
	Status string `json:"status"`
	// Material 是模型侧正在流出来的**中间材料**（思考链片段 / 分析过程）。
	// 与 Detail 分开而不是拼进 Detail：Detail 是稳定的一句话（也是顶部状态条的
	// 文本源），Material 是每 400ms 滚动的长文本，混在一起会让状态条变成一个
	// 不断变长的墙。只做展示，不参与任何判定。
	Material string `json:"material,omitempty"`
}

// Engine orchestrates the dialogue.
type Engine struct {
	llm      *llm.Client
	store    *store.SkillStore
	mu       sync.Mutex
	sessions map[string][]Message // in-memory trimmed history (maxHist)
	full     map[string][]Message // full history as persisted to disk (untouched by maxHist)
	dataDir  string               // sessions persisted under <dataDir>/sessions/*.json
	// maxHist 内存里保留的消息条数上限。
	//
	// 原值 12（=6 轮）是「不截断、交给大模型压缩」那套设计落地前留下的：它比
	// 压缩层还先动手，于是分层根本没机会生效——超过 6 轮的产物**轮不到被压缩，
	// 直接整段消失**。用户实测的「多轮对话就忘了我之前问了什么」正是它：
	// 18 条历史被砍到 12 条，最早贴进的那份写作要求连影子都没有。
	//
	// 定 40（=20 轮）：够让压缩层（compactKeepRecent=6 + watermark 触发阈值
	// 4000 字）真正吃上长历史，而 40 条消息体量对现代模型的上下文窗口不值一提。
	// 真正决定注入体积的是 ContextBlock 的分层预算，不是这个裁剪条数。
	maxHist int // max assistant+user turns kept for in-memory context

	// summaries 存「旧对话被模型压缩的结果」及其 watermark（见 compact.go）。
	// 为什么不在每次请求里现场压：压缩是一次真实模型调用（秒级），每轮都压会
	// 把对话延迟翻倍；watermark 让同一段旧对话只压一次，之后多轮复用。
	summaries map[string]*compaction
	// summarize 是压缩函数，默认走真实 LLM；单测注入假实现（模型不可用也要能测）。
	summarize func(context.Context, string) (string, error)
}

// New builds an engine over the existing store and LLM client.
func New(l *llm.Client, s *store.SkillStore) *Engine {
	e := &Engine{
		llm:       l,
		store:     s,
		sessions:  make(map[string][]Message),
		full:      make(map[string][]Message),
		maxHist:   40,
		summaries: make(map[string]*compaction),
	}
	// sessions are persisted to <dataDir>/sessions/<id>.json so a service
	// restart or page refresh does not wipe multi-turn fill context.
	if s != nil {
		d := dataDirOf(s.SkillsDir())
		e.dataDir = d
		os.MkdirAll(filepath.Join(d, "sessions"), 0o755)
	}
	return e
}

// dataDirOf derives the data root dir from the skills dir (its parent).
func dataDirOf(skillsDir string) string {
	if len(skillsDir) > 7 && skillsDir[len(skillsDir)-7:] == "/skills" {
		return skillsDir[:len(skillsDir)-7]
	}
	if len(skillsDir) > 8 && skillsDir[len(skillsDir)-8:] == "/skills/" {
		return skillsDir[:len(skillsDir)-8]
	}
	return skillsDir
}

func (e *Engine) sessionPath(id string) string {
	return filepath.Join(e.dataDir, "sessions", sanitizeID(id)+".json")
}

// sanitizeID keeps only safe characters so a session id cannot escape the
// sessions directory via path traversal.
func sanitizeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if s == "" {
		s = "anon"
	}
	return s
}

// persistSession writes the full history for a session atomically to disk.
func (e *Engine) persistSession(id string, hist []Message) {
	if e.dataDir == "" {
		return
	}
	b, err := json.Marshal(hist)
	if err != nil {
		return
	}
	path := e.sessionPath(id)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	os.Rename(tmp, path)
}

// loadSessionFromDisk reads a session's full history from disk, if present.
func loadSessionFromDisk(path string) []Message {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var hist []Message
	if err := json.Unmarshal(b, &hist); err != nil {
		return nil
	}
	return hist
}

// SetLLM swaps the underlying client (provider hot-swap).
func (e *Engine) SetLLM(l *llm.Client) { e.mu.Lock(); e.llm = l; e.mu.Unlock() }

// HasLLM 报告引擎手里有没有模型客户端。
// 存在理由：线上事故是"启动时传了 nil"，而 nil 只有在第一个请求打到模型时才炸成
// 空指针（前端显示得像网络错误）。有个可查询的状态，启动装配才有可断言的抓手，
// 不用真的发一个请求去试探。
func (e *Engine) HasLLM() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.llm != nil
}

// FastJSON 是一次性的「硬延迟预算」JSON 调用：不给工具、不读也不写会话历史、
// 不做意图分类。专给「首页推荐行」这类 UI 糖用（api 层的 suggestHandler）。
//
// 为什么不复用 Chat：Chat 那条路带记忆、带路由、带工具循环，一次请求可能
// 跑几十秒、几十次模型调用。推荐行的价值全在「用户还没开始打字的那两秒」，
// 它必须便宜、可超时、失败无所谓——所以要在引擎上开一条最短的通道，
// 而不是把糖挂在重路径上。
//
// 实测（线上 Qwen3.6-27B）：走 Chat 这条路 6.24s（思考链），走 llm.FastJSON
// 关掉思考链后 1.38s。慢的那一版在 5s 预算下等于功能不存在——接口 200 空数组，
// 前端悄悄退回规则版，页面上什么都看不出来。所以这里不是「优化了一下」，
// 是这条功能能不能存在。
func (e *Engine) FastJSON(ctx context.Context, sys, user string) (string, error) {
	if err := e.ensureLLM(); err != nil {
		return "", err
	}
	e.mu.Lock()
	cli := e.llm
	e.mu.Unlock()
	return cli.FastJSON(ctx, sys, user, 600)
}

// ChatTools 是工具循环所需的模型调用入口（引擎持有 LLM 客户端，
// 让 api 层不必自己再持有一份，也避免两处配置漂移）。
func (e *Engine) ChatTools(ctx context.Context, msgs []llm.Msg, defs []llm.ToolDef) (llm.Msg, error) {
	if err := e.ensureLLM(); err != nil {
		return llm.Msg{}, err
	}
	e.mu.Lock()
	cli := e.llm
	e.mu.Unlock()
	return cli.ChatTools(ctx, msgs, defs)
}

// ensureLLM lazily builds a client from the store's active config if the
// engine holds a nil one (typical at startup when only env config exists).
// Returns nil if no LLM is configured yet — callers surface that as an error.
func (e *Engine) ensureLLM() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.llm != nil {
		return nil
	}
	cfg, err := e.store.GetActiveLLM()
	if err != nil {
		return errors.New("未配置 LLM 服务（请在管理端配置）")
	}
	e.llm = llm.New(cfg)
	return nil
}

// Session returns (and lazily seeds) the in-memory (trimmed) history for a
// session id. On a cold start it restores the session from disk so a service
// restart does not wipe multi-turn context.
func (e *Engine) Session(id string) []Message {
	e.mu.Lock()
	defer e.mu.Unlock()
	h, ok := e.sessions[id]
	if ok {
		return h
	}
	// cold start: restore from disk, then trim to maxHist for context.
	full := e.loadFullLocked(id)
	if full == nil {
		full = []Message{}
		e.full[id] = full
	} else {
		e.full[id] = full
	}
	if len(full) > e.maxHist {
		// 裁剪窗口，但把素材/产物消息钉住（见 isPinnedMsg）。
		//
		// 用户实测：「我多轮对话的时候，似乎就忘了我之前问了什么，以及你自己
		// 回答了什么。」——素材层和产物层都要靠这些消息才能逐字注入，被纯时间窗
		// 切走就等于用户贴的一万字要求和之前写好的长稿一起消失，而且是**连压缩
		// 的机会都没有**（压缩只看近轮之外的对话）。maxHist 调大只能拖延，钉住
		// 才是根治；窗口该收的是寒暄，不是证据。
		keep := full[len(full)-e.maxHist:]
		var pinned []Message
		for _, m := range full[:len(full)-e.maxHist] {
			if isPinnedMsg(m) {
				pinned = append(pinned, m)
			}
		}
		if len(pinned) > 0 {
			full = append(pinned, keep...)
		} else {
			full = keep
		}
	}
	e.sessions[id] = full
	return full
}

// FullSession returns the complete persisted history for a session (NOT trimmed
// to maxHist). Callers that recover prior fill values (continuation edits)
// must use this so long-running conversations do not lose the "已填值:" marker
// simply because the in-memory context window trimmed it.
func (e *Engine) FullSession(id string) []Message {
	e.mu.Lock()
	defer e.mu.Unlock()
	if h, ok := e.full[id]; ok {
		return h
	}
	full := e.loadFullLocked(id)
	if full == nil {
		full = []Message{}
	}
	e.full[id] = full
	return full
}

// loadFullLocked returns a session's full history, from the in-memory map if
// present, otherwise from disk. Caller must hold e.mu.
func (e *Engine) loadFullLocked(id string) []Message {
	if h, ok := e.full[id]; ok {
		return h
	}
	if e.dataDir == "" {
		return nil
	}
	return loadSessionFromDisk(e.sessionPath(id))
}

// Reset clears a session's history (memory + disk).
func (e *Engine) Reset(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sessions[id] = nil
	e.full[id] = nil
	if e.dataDir != "" {
		os.Remove(e.sessionPath(id))
	}
}

// EvalTurn asks the orchestrator to decide what to do with the latest user turn,
// given the existing history. It routes through the LLM against the skill roster.
func (e *Engine) EvalTurn(ctx context.Context, id, user string, history []Message) (*Eval, error) {
	if err := e.ensureLLM(); err != nil {
		return nil, err
	}
	// Build a compact roster for intent routing.
	rosterStr, _, err := e.buildRoster()
	if err != nil {
		return nil, err
	}

	sys := `你是一个多智能体管线的调度器。每个用户请求都要先做「意图分析」，再根据意图**决定调用哪个工具/动作**，最后进入执行。这是一个「先意图判断 → 再决定工具调用 → 再执行」的逐步链路，你的唯一职责是前两步：判定意图并按意图解析出要执行的 action（工具选择），执行由后续阶段负责。

意图类别（intent，和 action 一一映射）：
- 用户要求把数据/内容**填进某个已存在的表单/模板/文档**（哪怕用户没给具体数据、只是让AI自己编/随便填/造数据）→ intent="docgen"，action="fill"。判断依据是"用户指向一个既有模板/表单并要求往里面填内容"——典型说法：用XX模板填、帮我填、随便编点数据填进去、把数据填进验收单、起草一份XX(合同/验收单/申请表/报销单)。
- 用户只要**空白模板/空表**、要求下发模板文件本身（"发我个模板 / 空白表 / 下载模板 / 给我模板文件"）→ intent="docgen"，action="template_only"。
- 用户要**生成一份新的办公文档**（并给出内容，不是填既有模板）→ intent="docgen"，action="gen"。
- 用户要写文章/稿子/介绍/邮件/文案/报道等自由文本 → intent="write"，action="write"。
- **用户明说只要正文/不要文件**（"直接输出正文""直接给我文字""不要生成文件""不用做 Word""只要文字就行"）→ intent="write"，action="write"。即使题材像公文（通知/通报/报告/函），只要用户明说只要正文，就不许判成 docgen：**明说的交付形态压过题材判断**。
- 用户要办理事务/查询流程规则，或闲聊问答 → intent="query"/"chat"，action="answer"。

技能匹配（在意图判断之后进行，作为工具候选清单）：
技能清单里带"专用模板附件"的 template 技能（如 采购合同、采购验收单）用于填充 → 当用户指向该模板并要求填写/起草时，action 必须是 "fill" 并命中该技能。
当匹配到带模板的技能但用户只要空白模板时，action="template_only" 并命中该技能。
当命中 docgen 技能且用户给出内容要生成新文档时，action="gen"。
当命中 write 技能时 action="write"。
只有当没有专门技能匹配时才落到通用能力（action="write"/"answer"）。

优先级：用户要求"填/起草/生成"某个明确表单或文档，且清单里有带专用模板的 template 技能精确匹配（如用户说"采购验收单"）时，必须优先选它且 action="fill"，绝不能退而选通用 docgen 技能。只有当没有专门 template 匹配时，才选通用 docgen/write 技能。

需求要点：
1. 若命中某个技能，返回它的 slug，并在 params 里提取用户已提及的关键参数（键名用技能参数名）。needs 的判定**必须严格**：只把技能参数中标记为 [必填]、且用户这次消息**确实没有提供**的参数列进 needs（name 用技能参数名，label 用中文提示）。判定"有没有提供"时**必须把上下文块【用户提供的要求与素材】整段算进去**：那里写明的信息就是用户已经给过的，一律不得列进 needs，也不得在 reason/steps 里说"待用户补充"。只有素材层和最近对话里都找不到的参数，才算没提供。[可选] 参数一律不追问——用户没给就自行推断合理占位或省略，直接进入生成。action="fill" 时绝不需要 params/needs（填充阶段会解析字段并让后续环节补全/编造值），needs 留空。docgen 类技能（含 fill）没有任何必填参数：用户给了详情就填进文档，用户只要模板/没给详情就编造合理示例数据填充或下发模板。
2. 用户可能在延续话题（如"再写一遍但改短点""把手机号改成139…"）——结合历史判断是否沿用之前的技能并继续 action（延续填充用 action="fill"）；延续时写 reason 说明。
3. 无论什么情况，都在 steps 里输出 4 阶段拆解执行路径，让用户看到多智能体怎么处理。每项**只给两个字段**：
   {"phase":"analyze|match|params|generate","detail":"≤10字"}
   - **不要写 label / status**：那是固定文案，服务端按 phase 补齐（写它只是白等——这一跳的输出速度就是线上吐字速度）。
   - analyze 写识别出的 intent/action（如 "write/write"）；match 写命中的技能名，未命中写 "通用能力"；params 写已提取/还需追问的参数名，未命中写 "提炼用户内容"；generate 写将执行的动作（如 "通用写作"、"填充模板"）。
   - detail 一句话、面向用户，别用内部术语。

只输出一个 JSON 对象，不要任何其他文字。照下面这个长度写，全篇 ≤220 字：
{"intent":"docgen","action":"fill","skill_slug":"采购合同","needs_tools":false,"reason":"命中模板，开始填充","params":{"party":"星禾科技"},"needs":[],"steps":[{"phase":"analyze","detail":"docgen/fill"},{"phase":"match","detail":"命中 采购合同"},{"phase":"params","detail":"已提取 1 项"},{"phase":"generate","detail":"填充模板"}]}
（字段说明：intent=write|docgen|query|chat，action=fill|template_only|gen|write|answer，skill_slug=<slug 或空>，reason ≤12 字；params 是对象、needs 是数组，没有内容就给 {} / []，不要写占位话术。）

needs_tools 判断（很重要，判错会导致答案里的数字是编的）：
- true：任务必须先拿到**外部实时数据**（查接口/API、抓网页、查实时行情、查仓库/订单/库存等外部系统数据），或需要对数据做**真实计算/统计**（求和、占比、同比、排序汇总）才能给出正确答案。
  典型：「查一下某仓库的 star 数」「把某接口的数据拉下来统计」「各区域销量算占比」「今天的汇率是多少」。
- false：用户要生成/填写**办公文档**、写文章、按模板出文件，或闲聊问答——这类不需要外部数据。
  典型：「写一份述职报告」「用验收单模板填数据」「介绍下你们的产品」。
- 判断分界线：**这个任务里有没有「事实类数字/数据」是模型不知道、必须去外部拿的**。有 → true。
- needs_tools=true 时，skill_slug 可以为空、action 照常输出（后续阶段会改用工具完成，而不是让模型凭记忆编内容）。

action 必须根据上面「意图→动作」映射严格输出，不要省略。params/needs/steps 可以为空，但 intent、action、steps、needs_tools 必须输出。

技能清单：
` + rosterStr

	// Ask the classifier; if the first attempt comes back unusable (malformed
	// JSON or empty intent), retry once with a stricter one-line-JSON nudge so a
	// single flaky completion doesn't silently degrade the route.
	// 整个意图识别阶段共享一个总预算：首发 + JSON 重试 + 兜底重试全算在里面。
	// 只罩在 classify 这几发上，后面的执笔/渲染照旧用整轮 ctx（它们本来就该跑很久）。
	pctx, pcancel := context.WithTimeout(ctx, classifyPhaseLimit())
	defer pcancel()

	eval, retry, ctxRunes, ctxBlk := e.classify(pctx, id, sys, history, user)
	if retry {
		eval, _, _, _ = e.classify(pctx, id, sys, history, user)
	}
	if eval == nil {
		// 超时 / 上游报错走的是 retry=false 那条路，过去**直接静默降级**：意图丢空、
		// skill 为空，整轮当成通用写作。线上 2026-09-20 第 2 轮「把上面那篇整理成 Word」
		// 就是这么变成 0 交付物的——用户看到的是「它不管我上文」，真因是分类跳
		// `hop=60.0s ttft=-1.00 … context deadline exceeded`。
		//
		// 兜底重试，但**只在这一发真的能变小时才打**：失败那一发的 in=2311 token，
		// 涨上去的原因是 ContextBlock 把上一轮产物逐字注入（docgen 需要，不能砍），
		// 而意图识别只需要知道用户在指代什么。同类请求在 352 token 量级只要 2.3s。
		// 如果本来就没有历史可裁（裁剪后没显著变小），重试只是把等待翻倍——
		// 那正是 TestEvalTurnBoundsClassifierHop 守着的事，所以这里必须带这个判据。
		clipped := clipForClassify(history)
		clipRunes := len([]rune(clipped))
		if ctxRunes > 0 && clipRunes*2 < ctxRunes {
			eval, _, _, _ = e.classifyWith(pctx, sys, user, func(context.Context) string { return clipped },
				classifyRetryLimit())
			if eval != nil {
				ReportProgress(ctx, "· 意图识别首次超时，已用精简上下文重试成功")
			} else {
				fmt.Fprintf(os.Stderr, "[classify-retry] clipped hop 仍失败（原 ctx=%d 字 → 裁剪后 %d 字）\n",
					ctxRunes, clipRunes)
			}
		} else {
			// 没有可裁的历史：整条消息就是用户这一句，ctx 本来就小（线上有一发
			// ctx=15 字 / user=29294 字节），裁剪一分钱也省不下来。旧实现在这个
			// 分支里**一次重试都不打**，直接降级——那 60 秒白等，路由也丢了。
			//
			// 但挂死是**瞬时**的：同一形态输入直连上游实测 1.7/1.8/2.0s，而同一分钟
			// 里也能 60s 不给一个字节。所以同输入原样快问一发（不能重跑 buildBlock：
			// 它可能有副作用，重发已构造好的那份字符串）；总等待由
			// classifyPhaseLimit() 兜着，默认 25+15=40s，仍短于旧默认的单发 60s。
			eval, _, _, _ = e.classifyWith(pctx, sys, user, func(context.Context) string { return ctxBlk },
				classifyRetryLimit())
			if eval != nil {
				ReportProgress(ctx, "· 意图识别首次超时，已原样重试成功")
			} else {
				fmt.Fprintf(os.Stderr, "[classify-retry] 同输入快问仍失败（ctx=%d 字）\n", ctxRunes)
			}
		}
	}
	if eval == nil {
		// 两次都失败：先按**零成本本地规则**救一次，救不回来才降级。
		// 为什么值得救：分类跳是网络调用，上游一抖就 60s 超时（实测一分钟内
		// 同一上游「40s 超时 / 2.3s 成功 / 0.6s 成功」三种结果）。而「把上面那篇
		// 整理成 Word」这类承接请求，判它要不要出文件根本不需要模型——词面 + 历史里
		// 有没有长文产物，两条本地事实就够。降级成通用写作等于把这一轮彻底丢掉。
		if ev := e.localRescueRoute(user, history); ev != nil {
			ReportProgress(ctx, "· 意图识别超时/失败：已按本地规则判定这是承接上一轮稿子并要求出文件，直接走文档生成")
			return ev, nil
		}
		// 降级可以，但**必须出声**。用户看到的材料会写明「本轮没按你的上文
		// 路由」，而不是产出一份看起来正常、其实走错链的东西。
		ReportProgress(ctx, "· 意图识别超时/失败：本轮按通用写作处理（没有按你的上文路由到对应能力）")
		return &Eval{SkillSlug: "", Reason: "意图识别异常，按普通对话处理"}, nil
	}
	eval.SkillSlug = strings.TrimSpace(eval.SkillSlug)
	return eval, nil
}

// classifyHopLimit 是意图识别这一跳的独立时间预算。
//
// 为什么单独给：它挡在用户第一句话后面，拿的是整轮 ctx，一旦上游挂住就会把
// 整轮一起拖下去（线上 616.5s 那一轮，600s 全耗在这一跳）。超时按识别失败兜底。
//
// 默认值从 60s 收到 25s（2026-09-20 实测重定）：
//   - 健康样本 45 发，耗时 4.1~14.3s（最慢 14.3s），25s 仍有约 1.7 倍余量；
//   - 同一形态输入（1 万字素材原文 + 关思考 + json_object）直连上游实测
//     1.7/1.8/2.0s —— 输入规模**不是**慢的原因，裁输入省不出时间；
//   - 而同一分钟里上游能 60s 不给一个字节：24h 内 56 发里 11 发撞满 60s，
//     20% 的失败率，每一发都把用户按在计时器前 60 秒，救回来的概率又极低。
// 所以这里选：**宁可早收手换一发重试，也不陪上游静坐**。
func classifyHopLimit() time.Duration {
	const def = 25 * time.Second
	v := strings.TrimSpace(os.Getenv("SKILLFORGE_CLASSIFY_TIMEOUT_SEC"))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return time.Duration(n) * time.Second
}

// classifyPhaseLimit 是整个意图识别阶段（首发 + JSON 重试 + 兜底重试）的**总**预算。
//
// 为什么必须有总预算：单跳预算管不住总和。三发各 25s 就是 75s，比旧默认的**单发**
// 60s 还差——「每一跳都调小了」不等于「用户等得更短」。用户体感只认「从点发送到看见
// 路由」这一段，所以这里给一个硬顶：默认 25+15=40s，短于旧默认的 60s，且无论走哪条
// 兜底分支都不会超（阶段 deadline 一到期，后续那发直接失败，不再新起等待）。
func classifyPhaseLimit() time.Duration {
	return classifyHopLimit() + classifyRetryLimit()
}

// classify runs one classifier completion. Returns (eval, retry) where retry
// is true when the result was unusable (bad JSON or empty intent).
//
// 这一跳是**关思考链 + 流式**的（曾经是普通的阻塞 Chat）：
//   - 关思考链：线上活跃模型是 reasoning 模型，同一段提示词实测带思考 63s、
//     关思考 4.8s。分类是「按给定规则选一个格子」的活，思考链纯属浪费，
//     而它挡在用户第一句话后面，是「点了发送几十秒没反应」的最大一笔账。
//   - 流式：万一 provider 忽略开关照旧产思考链（astron 就会静默忽略
//     enable_thinking），思考片段能当中间材料流出去，用户至少看得见它在干活。
func (e *Engine) classify(ctx context.Context, id, sys string, history []Message, user string) (*Eval, bool, int, string) {
	return e.classifyWith(ctx, sys, user, func(hctx context.Context) string {
		return e.ContextBlock(hctx, id, history)
	}, classifyHopLimit())
}

// classifyWith 是 classify 的本体：上下文块的构造方式与这一跳的时间预算由调用方给。
//
// 拆出这一层是为了让兜底重试能换上下文（且换一个更短的预算），同时**保持原有行为
// 不变**：ContextBlock 仍在同一个 hop 预算内构造（它内部可能触发一次压缩调用，
// 那笔账本来就记在这一跳上）。第三个返回值是上下文块的字符数——调用方靠它判断
// 「换裁剪上下文到底能不能让输入显著变小」，不然重试只是把等待翻倍。
// 第四个返回值是**构造好的上下文块本身**，供「同输入快问一次」原样重发：那条路不能
// 重跑 buildBlock——它可能有副作用（压缩会真的花掉一次模型调用），重发一份字符串才是
// 「同样的请求再来一发」。
func (e *Engine) classifyWith(ctx context.Context, sys, user string, buildBlock func(context.Context) string, limit time.Duration) (*Eval, bool, int, string) {
	// hop 级预算：这一跳挡在用户**第一句话**后面，正常 6s 就流完（线上实测），
	// 所以绝不能拿整轮级别的 ctx 陪着等——传进来的 ctx 覆盖整个请求（几十秒到
	// 几分钟）。线上曾有一整轮 616.5s，就是这一跳挂住 600s 造成的（见 R1）。
	// 超时后按「识别失败」走兜底：宁可退化成普通对话，也不让用户对着计时器干等。
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	// 意图识别也吃上下文：用户说「整理成 word」时，识别「这是承接上一轮新闻稿」
	// 才能路由到正确的技能，而不是当成一句没头没尾的新指令。
	ctxBlk := buildBlock(ctx)
	prompt := "对话历史（供参考，重点回应最新消息）：\n" + ctxBlk + "\n\n用户最新消息：\n" + user
	// 这一跳的耗时观测：ttft（预填/排队）与总时长（吐字）拆开，输出字数记下来。
	// 线上实测（2026-09-19）生效的是 siliconflow/Qwen3.6-27B，吐字 ~50 tok/s，
	// 所以「让它少写固定文案」直接把这一跳从 10.3s 压到 2.9s。没有这行日志，
	// 「意图分析为什么慢」只能靠猜 provider。
	t0 := time.Now()
	var firstTok time.Time
	opts := llm.StreamOpts{
		DisableThinking: true,
		JSONMode:        true,
		MaxTokens:       classifyMaxTokens(),
		OnReasoning:     reasoningSink(ctx),
	}
	if sink := contentSink(ctx); sink != nil {
		opts.OnContent = func(s string) {
			if firstTok.IsZero() {
				firstTok = time.Now()
			}
			sink(s)
		}
	}
	out, err := e.llm.StreamChat(ctx, sys, prompt, opts)
	hop := time.Since(t0)
	ttft := -1.0
	if !firstTok.IsZero() {
		ttft = firstTok.Sub(t0).Seconds()
	}
	logHop := func(ev *Eval, retry bool, why string) {
		intent, action, skill, needN, paramN, stepN := "", "", "", 0, 0, 0
		if ev != nil {
			intent, action, skill = ev.Intent, ev.Action, ev.SkillSlug
			needN, paramN, stepN = len(ev.Needs), len(ev.Params), len(ev.Steps)
		}
		fmt.Fprintf(os.Stderr, "[classify] hop=%.1fs ttft=%.2fs in=%d字/%dB(sys=%d字 ctx=%d字 user=%d字) out=%d intent=%s action=%s skill=%s needs=%d params=%d steps=%d retry=%v %s\n",
			hop.Seconds(), ttft, len([]rune(prompt)), len(prompt), len([]rune(sys)), len([]rune(ctxBlk)), len([]rune(user)), len(out),
			intent, action, skillshort(skill), needN, paramN, stepN, retry, why)
	}
	if err != nil {
		logHop(nil, false, "err="+err.Error())
		return nil, false, len([]rune(ctxBlk)), ctxBlk
	}
	eval := &Eval{}
	if err := json.Unmarshal([]byte(extractJSON(out)), eval); err != nil {
		fmt.Fprintf(os.Stderr, "[classify-retry] unmarshal err=%v raw_out=%q\n", err, out)
		logHop(nil, true, "unmarshal")
		return nil, true, len([]rune(ctxBlk)), ctxBlk
	}
	if strings.TrimSpace(eval.Intent) == "" && strings.TrimSpace(eval.SkillSlug) == "" {
		// empty intent AND no skill — the model dodged. Only retryable when it
		// truly produced nothing usable.
		fmt.Fprintf(os.Stderr, "[classify-retry] empty intent raw_out=%q\n", out)
		logHop(eval, true, "empty-intent")
		return nil, true, len([]rune(ctxBlk)), ctxBlk
	}
	logHop(eval, false, "")
	return eval, false, len([]rune(ctxBlk)), ctxBlk
}

// classifyRetryLimit 是**兜底重试**那一发的预算：输入已被裁到几百 token，实测那一量级
// 2~3 秒就回来（线上 352 token 的那发 8.1s，含吐字），15s 是五倍余量。
//
// 为什么不能沿用 classifyHopLimit（60s）：超时后再等一个 60s 就是「等待翻倍」，
// 用户体感比直接降级更差。上限仍夹在 classifyHopLimit() 之内，保证「兜底比首发更短」
// 这条性质恒成立（有人把 SKILLFORGE_CLASSIFY_TIMEOUT_SEC 设成 5s 时，兜底也是 5s）。
func classifyRetryLimit() time.Duration {
	base := classifyHopLimit()
	v := strings.TrimSpace(os.Getenv("SKILLFORGE_CLASSIFY_RETRY_SEC"))
	if v == "" {
		if base < 15*time.Second {
			return base
		}
		return 15 * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 15 * time.Second
	}
	d := time.Duration(n) * time.Second
	if d > base {
		return base
	}
	return d
}

// classifyMaxTokens 第一跳的输出上限，0 = 不限（默认，保持原行为）。
// 留这个旋钮是为了能在不重新编译的前提下做线上 A/B：这一跳的耗时 ≈ 输出字数 ÷
// provider 吐字速度，掐上限是唯一能立刻验证「是不是输出太长」的手段。
func classifyMaxTokens() int {
	v := strings.TrimSpace(os.Getenv("SKILLFORGE_CLASSIFY_MAXTOK"))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// skillshort 日志里只留技能名的前 24 字，避免把长 slug 灌进日志。
func skillshort(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "-"
	}
	r := []rune(s)
	if len(r) > 24 {
		return string(r[:24]) + "…"
	}
	return s
}

// SkillContent bundles everything needed to generate with a skill.
type SkillContent struct {
	Slug         string
	Name         string
	SkillType    string // write | query | template
	Attachment   string // relative path for template skills (may be empty)
	SystemPrompt string
	Template     string
}

// HasDocJSONContract 判断一份技能提示词是否带文档生成契约（能让 parseDocJSON 解析成功的
// 那段 JSON 规格说明）。
//
// 出文件这条路**只能**由带契约的技能走：GenerateDoc 最后要 parseDocJSON，拿写作技能的
// 提示词去发文只会解析失败 —— 所以纠偏时不能把任意技能凑上去。
//
// ⚠️ 锚点用**结构**，不许认某个词：初版写的是 Contains(prompt, "DOCJSON")，而真实的
// 内置「办公文档管家」提示词里从来没有 "DOCJSON" 这个字面量（它只出现在 seed.go 的
// 注释里）。于是本函数对唯一的 docgen 技能恒返回 false，PickDocGenSkill 恒返回 nil ——
// 闸门在生产里**永远不开**，纠偏形同虚设（写这把尺子时被当场逮到：红线打印的是
// 「技能库里没有可用的文档生成技能」，而不是「没纠偏」）。
// 认字段结构既贴合真实契约（【输出契约】里就是 format/filename/cols/rows/parags），
// 也不会因为换个说法（"文档规格 JSON" 之类）就失效。
func HasDocJSONContract(prompt string) bool {
	p := strings.ToLower(prompt)
	// 老词保留：技能工厂产出的技能可能自己带 DOCJSON 这名字。
	hasSpec := strings.Contains(p, "docjson") ||
		(strings.Contains(p, `"format"`) && strings.Contains(p, `"filename"`))
	hasBody := strings.Contains(p, `"parags"`) || strings.Contains(p, `"rows"`) ||
		strings.Contains(p, `"cols"`)
	return hasSpec && hasBody
}

// ExplicitTextOnly 判断用户这一轮**明说**只要正文、不要文件。
//
// 为什么需要这么一把确定性闸门（2026-09-17 线上实测，真浏览器，非推断）：
// 用户输入「写一份关于开展数据治理专项工作的通知。背景与核心素材：…。正文不少于
// 600 字，直接输出正文」——分类器按题材把 intent 判成 docgen（标题像公文），
// 于是 chat.go 的出文件纠偏闸门当场把这一轮抢去生成 .docx：屏幕上只有一个文件卡片，
// 正文是「已为您生成《…docx》，点击下方文件即可下载」94 字。
// **用户明说了「直接输出正文」，交付物却是个文件** —— 这是真故障，不是审美问题；
// 用户那句「它总是不能很好地理解」有一类就是这么来的。
//
// 为什么不直接信分类器：这类误判来自题材词（通知/报告/函），靠调提示词只能降低概率，
// 压不到零；而「明说的交付形态」是用户亲手写下的判据，不该由模型投票决定。
// 分类器提示词那侧也补了同一条规则（双保险），这里是**确定性兜底**。
//
// 判据刻意收窄：只认显式祈使式要求（直接输出正文 / 不要生成文件 / 只要文字…），
// 绝不认「通知」「报告」这类题材词 —— 题材词正是误判根源，拿它当判据等于把闸门
// 整个关掉，要 Word 的用户又拿不到文件（反向故障）。
// 【续】否定式「不要文件」用正则而不是固定短语表：线上真语料就有「不用做 Word」，
// 只列「不用word / 不要word / 不要生成文件」会漏掉中间夹动词的写法（本文件的表测
// 第一次就是被它咬红的 —— 漏判方向是**真故障方向**：闸门照样把这一轮抢去出文件）。
// 允许否定词与交付形态之间夹 0~3 个字（做/生成/出/给/再…），但停在句读处，
// 免得跨小句把「不要改标题，生成一份 Word」判成只看正文。
var textOnlyNegated = regexp.MustCompile(
	`(?:不要|不用|别|无需|不需要|别给我)[^。；;，,、！!？?]{0,3}` +
		`(?:文件|文档|附件|下载|word|docx|pdf|excel|ppt|xlsx)`,
)

// textOnlyPhrases 是「只要正文」这一族（肯定式）。
// 刻意不认题材词（通知/报告/函）—— 题材词正是分类器误判的根源，
// 把它当判据等于把闸门整个关掉，要 Word 的用户又拿不到文件（反向故障）。
var textOnlyPhrases = []string{
	"直接输出正文", "直接给我正文", "直接输出文字", "直接给我文字",
	"只输出正文", "只要正文", "仅要正文", "只要文字", "仅要文字",
	"输出正文即可", "正文即可", "文字就行", "文字即可",
	"nofile",
}

func ExplicitTextOnly(msg string) bool {
	// 去空白 + 小写：容忍「直接 输出 正文」「不用做 Word」这种断词，
	// 英文按整词缩写匹配（"no file" → "nofile"）。
	m := strings.ToLower(msg)
	m = strings.NewReplacer(" ", "", "	", "", "\n", "", "　", "").Replace(m)
	if textOnlyNegated.MatchString(m) {
		return true
	}
	for _, k := range textOnlyPhrases {
		if strings.Contains(m, k) {
			return true
		}
	}
	return false
}

// PickDocGenSkill 挑一个能出文件的技能（skill_type=docgen 且提示词带 DOCJSON 契约）。
// 用途：分类器给出「intent=docgen（要出文件）却命中非 docgen 型技能」这种自相矛盾的
// 组合时，由调用方纠偏到这里，而不是静默降级成写正文。
//
// hint 是本轮用户原话（可为空）：多个 docgen 技能时按名称/描述的二字片段重合度排序，
// 让「合同生成器」去接合同请求、「通知生成器」去接通知请求。分数相同按 slug 稳定排序，
// 保证同一输入每次选同一个（不然路由会飘，排障时对不上账）。
func (e *Engine) PickDocGenSkill(hint string) *SkillContent {
	skills, err := e.store.List()
	if err != nil {
		return nil
	}
	best, bestScore := "", -1
	for _, sk := range skills {
		if !sk.Enabled || sk.SkillType != model.SkillTypeDocGen {
			continue
		}
		sc, lerr := e.LoadSkill(sk.Slug)
		if lerr != nil || sc == nil || !HasDocJSONContract(sc.SystemPrompt) {
			continue // 没契约的发文技能等于不能用，宁可继续找下一个
		}
		score := shingleOverlap(sk.Name+" "+sk.Description, hint)
		if score > bestScore || (score == bestScore && (best == "" || sk.Slug < best)) {
			best, bestScore = sk.Slug, score
		}
	}
	if best == "" {
		return nil
	}
	sc, err := e.LoadSkill(best)
	if err != nil {
		return nil
	}
	return sc
}

// shingleOverlap 数 a 的相邻二字片段有多少出现在 b 里。此处只用来给候选技能排序，
// 不追求语义精度：能区分「合同」和「通知」就够了，分不出来时全部同分、退回稳定排序。
func shingleOverlap(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) < 2 || len(rb) < 2 {
		return 0
	}
	bset := make(map[string]bool, len(rb))
	for i := 0; i+1 < len(rb); i++ {
		bset[string(rb[i:i+2])] = true
	}
	n := 0
	for i := 0; i+1 < len(ra); i++ {
		if bset[string(ra[i:i+2])] {
			n++
		}
	}
	return n
}

// LoadSkill fetches a skill's anchor files (system_prompt + template) plus its
// type/attachment so the caller can route generation and file delivery.
func (e *Engine) LoadSkill(slug string) (*SkillContent, error) {
	sk, err := e.store.Get(slug)
	if err != nil {
		return nil, err
	}
	if sk == nil {
		return nil, errors.New("技能不存在: " + slug)
	}
	sys, err := e.store.SystemPrompt(slug)
	if err != nil {
		return nil, fmt.Errorf("读取技能提示词: %w", err)
	}
	tpl, _ := e.store.Template(slug) // template optional
	stype := sk.SkillType
	if stype == "" {
		stype = model.SkillTypeWrite
	}
	return &SkillContent{
		Slug: slug, Name: sk.Name, SkillType: stype, Attachment: sk.Attachment,
		SystemPrompt: sys, Template: tpl,
	}, nil
}

// Skill returns the raw skill record (for the public attachment endpoint).
func (e *Engine) Skill(slug string) (*model.Skill, error) {
	return e.store.Get(slug)
}

// AttachmentPath resolves a skill's declared attachment to an absolute path,
// guarding against traversal. Returns ok=false if unavailable.
func (e *Engine) AttachmentPath(slug, rel string) (string, bool) {
	return e.store.AttachmentPath(slug, rel)
}

// buildRoster renders the enabled-skill manifest (slug/name/desc/params) for
// injection into any prompt that needs skill awareness — intent routing AND
// plain chat, so the assistant can answer "what skills do you have".
func (e *Engine) buildRoster() (string, []model.Skill, error) {
	skills, err := e.store.List()
	if err != nil {
		return "", nil, err
	}
	var roster strings.Builder
	for _, sk := range skills {
		if !sk.Enabled {
			continue
		}
		fmt.Fprintf(&roster, "- slug=%s | 名称=%s | 类型=%s | 描述=%s\n", sk.Slug, sk.Name, skillTypeLabel(sk.SkillType), sk.Description)
		if len(sk.InputParams) > 0 {
			fmt.Fprintf(&roster, "    参数: ")
			names := make([]string, 0, len(sk.InputParams))
			for _, p := range sk.InputParams {
				tag := "可选"
				if p.Required {
					tag = "必填"
				}
				names = append(names, fmt.Sprintf("%s(%s)[%s]", p.Name, p.Label, tag))
			}
			fmt.Fprintf(&roster, "%s\n", strings.Join(names, ", "))
		}
	}
	if roster.Len() == 0 {
		roster.WriteString("（当前没有可用的写作技能）")
	}
	return roster.String(), skills, nil
}

// Generate streams a response using the loaded skill. Args are the merged
// param values (from eval params + any follow-up filled by the user). Behavior
// is typed: write skills produce an article; query/template skills produce an
// actionable flow (and reference the downloadable attachment if present).
func (e *Engine) Generate(ctx context.Context, sc *SkillContent, args map[string]string, onDelta func(string)) (string, error) {
	return e.generateWithExtra(ctx, sc, args, "", onDelta)
}

// generateWithExtra 是 Generate 的完整实现。extra 非空时被追加到 system prompt
// **末尾**：手册模式用它注入「本类写作要求 + 本类范文」。放末尾而不是插在中间，
// 是因为 system prompt 里越靠后的内容离用户这句话越近，越不容易被中间的大段
// 技能说明冲淡——手册要求是硬约束，不能被当成背景介绍。
// extra 留空即普通生成，与原实现逐字等价。
func (e *Engine) generateWithExtra(ctx context.Context, sc *SkillContent, args map[string]string, extra string, onDelta func(string)) (string, error) {
	return e.generateWithExtraPlan(ctx, sc, args, extra, "", onDelta)
}

// hopStat 给「执笔/构思」这类长跳记账：总时长、首字时刻、思考片数、正文片数。
//
// 为什么需要它（2026-09-20 线上账本）：整轮 353.0s、**首正文 347.8s**、最大静默
// 297.7s，而同一时间窗口里 stderr 只有一条 [classify] ——「慢在哪一跳」全靠人肉推理。
// 执笔是整轮最长的一跳，没有这行日志就只能靠猜 provider（前几次就是这么猜错的）。
// 它也是判断「中间材料到底流没流」的唯一硬证据：reason=0 就是一片都没拿到，
// 跟「前端没显示」是两件事。
type hopStat struct {
	t0       time.Time
	firstTok time.Time
	nReason  int
	nContent int
}

// content 把正文片段记一笔并原样转发给下游（onDelta 可为 nil）。
func (s *hopStat) content(onDelta func(string)) func(string) {
	return func(d string) {
		if s.firstTok.IsZero() {
			s.firstTok = time.Now()
		}
		s.nContent++
		if onDelta != nil {
			onDelta(d)
		}
	}
}

// reasoning 把思考链片段记一笔并转发给材料面板；没挂接收器就返回 nil，
// 让 LLM 层省掉每片一次的函数调用（与 reasoningSink 的分工一致）。
func (s *hopStat) reasoning(sink func(string)) func(string) {
	if sink == nil {
		return nil
	}
	return func(r string) {
		s.nReason++
		sink(r)
	}
}

// log 输出一行跳级账本。ttft=-1 表示整跳一个 token 都没收到——那是「上游挂住」
// 的确诊信号，跟「模型写得多」必须分得开。
func (s *hopStat) log(kind, out string, err error) {
	ttft := -1.0
	if !s.firstTok.IsZero() {
		ttft = s.firstTok.Sub(s.t0).Seconds()
	}
	why := ""
	if err != nil {
		why = " err=" + err.Error()
	}
	fmt.Fprintf(os.Stderr, "[%s] hop=%.1fs ttft=%.2fs reason=%d out_pieces=%d out=%d%s\n",
		kind, time.Since(s.t0).Seconds(), ttft, s.nReason, s.nContent, len([]rune(out)), why)
}

// headRunes 取前 n 个 rune 用于日志留痕。
// 为什么不用 s[:n]：中文按下标切会切碎 UTF-8，日志里会出现乱码，读起来像另一个 bug。
func headRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// PlanEssay 先把「这一篇怎么写」的构思**当正文要出来**，而不是等模型藏在思考链里。
//
// 为什么需要这一跳（线上账本，不是推测）：技能链路执笔跳保留思考链时，首字实测等到
// 72.8s，其中 63.2s 屏幕上零可见变化——账本按帧时间戳算出来的最大静默就是它。而
// internal/llm/stream.go 自己记着同一台模型的对照：带思考 63.0s / 关思考 4.8s。
// 也就是说这 63 秒就是思考链，而 provider 在长文执笔这段**一片 reasoning 都不推**
// （reasoningSink 接了、收不到），所以屏幕上只剩计时器在跳——用户原话「一直卡着计时」。
//
// 这一跳把构思变成 content：关思考链（与分类/抽取同类，几秒级），OnContent 走
// contentSink → ProgressOf → 步骤面板，材料因此在秒级就开始滚，而且滚的是真内容。
// 它**不碰执笔那一跳**：执笔仍保留思考链（质量优先，见 progress_wiring_test.go 的守卫）。
// 失败不致命：拿不到要点就照旧直接落笔（返回空串，由调用方决定怎么显示）。
func (e *Engine) PlanEssay(ctx context.Context, sc *SkillContent, args map[string]string, userMsg string) (string, error) {
	if e == nil || e.llm == nil {
		return "", nil
	}
	// 两条道都要能用：命中技能时按技能提示词构思；没命中技能（线上第 1 轮就是这条）
	// 时没有技能可依，就用一个轻量写作身份 + 用户原话起头。
	sys := "你是一名中文写作助手，先帮用户把这一篇的写法想清楚。"
	lead := ""
	if sc != nil {
		sys = e.generateSys(sc)
		lead = argBlockOf(sc, args)
	} else {
		lead = "用户本轮需求：\n" + strings.TrimSpace(userMsg)
	}
	ask := lead + "\n\n# 本轮任务\n先只写这一篇的构思要点，不要写正文：\n" +
		"1) 3~6 条，每条一行，以「· 」开头；\n" +
		"2) 每条写清：写什么、按什么结构写、必须带哪些要素、要避开什么；\n" +
		"3) 不要开场白、不要解释、不要 markdown 标题；\n" +
		"4) 全中文，总量控制在 300 字以内。"
	st := &hopStat{t0: time.Now()}
	// 构思跳必须自带**硬上限**。它关思考链（provider 实测 reason=0），自己一片材料都不推，
	// 所以它整段耗时都是屏幕上的静默。线上实测（2026-09-20，siliconflow）这一跳跑出过
	// hop=50.0s / ttft=45.5s，整轮「最长无变化 49.2s」就是它——而它存在的唯一目的恰恰是
	// 让屏幕有东西在动（见 chat.go 的注释）。让一个「为了展示」的跳把展示冻住 50 秒是
	// 本末倒置，所以给上限：超时按「没拿到要点」继续落笔。执笔那跳自己会流思考链
	// （实测 thinking_budget=1024 时 0.8s 出首片思考、23.4s 出首正文），材料立刻就有。
	if d := planDeadline(); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	out, err := e.llm.StreamChat(ctx, sys, ask, llm.StreamOpts{
		DisableThinking: true,
		OnReasoning:     st.reasoning(reasoningSink(ctx)),
		OnContent:       st.content(rawSink(ctx)),
	})
	st.log("plan", out, err)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// GenerateWithPlan 同 Generate，额外把本轮构思要点并进 system prompt 末尾。
func (e *Engine) GenerateWithPlan(ctx context.Context, sc *SkillContent, args map[string]string, plan string, onDelta func(string)) (string, error) {
	return e.generateWithExtraPlan(ctx, sc, args, "", plan, onDelta)
}

// writeThinkingOn 执笔跳是否保留思考链。**默认保留**（质量优先）。
// 只有显式设 SKILLFORGE_WRITE_THINKING=0 才关：那是明示的取舍（首字 63s → 4.8s，
// 换来的是执笔质量的自行承担），不是悄悄降质的默认值。
func writeThinkingOn() bool {
	return strings.TrimSpace(os.Getenv("SKILLFORGE_WRITE_THINKING")) != "0"
}

// planDeadline 是构思跳（PlanEssay）的硬上限，默认 20s，SKILLFORGE_PLAN_DEADLINE_MS 可调，
// 显式设 0 = 不限制（给「这一轮我就要它慢慢想」留出口）。
//
// 定这条的账（线上实测，2026-09-20，siliconflow Qwen3.6-27B）：
//
//	plan 跳     hop=50.0s ttft=45.5s reason=0   ← 整轮「最长无变化 49.2s」就是它
//	write 跳    hop=25.8s ttft=19.5s reason=1024
//
// 构思跳关思考链，所以它**一片材料都不推**（reason=0）：这 50 秒里屏幕上除了计时器
// 什么都没有，而这一跳存在的唯一目的恰恰是让屏幕有东西在动（见 api/chat.go 的注释）——
// 让一个「为了展示」的跳把展示冻住 50 秒是本末倒置。正常 provider 下这一跳全量返回
// 约 7.7s，20s 只砍异常、不碰正常路径；真被砍了也不致命：拿不到要点就照旧直接落笔，
// 而执笔跳自带思考链流（实测 0.8s 出首片思考），材料立刻接上。
func planDeadline() time.Duration {
	const def = 20 * time.Second
	v := strings.TrimSpace(os.Getenv("SKILLFORGE_PLAN_DEADLINE_MS"))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	if n <= 0 {
		return 0 // 显式不限
	}
	return time.Duration(n) * time.Millisecond
}

func (e *Engine) generateWithExtraPlan(ctx context.Context, sc *SkillContent, args map[string]string, extra, plan string, onDelta func(string)) (string, error) {
	sys := e.generateSys(sc)
	if strings.TrimSpace(extra) != "" {
		sys += "\n\n" + extra
	}
	if strings.TrimSpace(plan) != "" {
		sys += "\n\n==== 本轮构思要点（按它落笔，不要再写成两篇）====\n" + plan
	}
	var done string
	if sc.SkillType == model.SkillTypeWrite {
		done = "请据此直接写出完整文章。"
	} else {
		done = "请据此给出结构化、可直接照做的办事流程/答案。"
	}
	// 执笔这一跳**默认保留思考链**（质量优先），但把思考片段当中间材料流出去：
	// 思考链期间正文一个字都没有，不流的话用户看到的就是「一直卡着计时」。
	//
	// ★ 2026-09-20 改：原来「保留思考」这条分支走 CompleteEx（go-openai SDK），而 SDK
	// 那条隧道不认 SKILLFORGE_THINK_BUDGET —— 于是「保留思考 + 掐思考预算」这条中间路
	// 在**执笔跳上是死的**，只有审稿跳（writing.go 的 Revise）吃得到预算，而最慢、
	// 最需要它的一跳恰恰是执笔。等价性论证见 plainChatWithPlan 里的同款注释。
	//
	// ★ 2026-09-19 修：`DisableThinking` 必须**逐字**跟着 writeThinkingOn() 走。它从
	// b1a431a 引入时漏在那一行上，于是 SKILLFORGE_WRITE_THINKING=0 只是把调用从
	// CompleteEx 换成 StreamChat，思考链照旧开着（DisableThinking=false 走 knobNone
	// = 不带关思考的开关 = provider 默认开），文档里写的「首字 63s → 4.8s」从来没兑现过：
	// 线上 A/B 实测两臂 186s vs 138s，那 48s 全是模型方差，因为「关掉」那一臂根本没关。
	// 守着这两行的是 TestWriteHopKnobActuallyReachesProvider（默认臂必须**不带**开关，
	// =0 臂必须**真带** enable_thinking=false + reasoning_effort=none）。
	st := &hopStat{t0: time.Now()}
	out, err := e.llm.StreamChat(ctx, sys, argBlockOf(sc, args)+"\n"+done, llm.StreamOpts{
		DisableThinking: !writeThinkingOn(),
		OnContent:       st.content(onDelta),
		OnReasoning:     st.reasoning(reasoningSink(ctx)),
	})
	st.log("write-skill", out, err)
	return out, err
}

// generateSys 拼出技能的 system prompt（身份 + 技能提示词 + 骨架模板 + 附件提示）。
func (e *Engine) generateSys(sc *SkillContent) string {
	sys := "你是" + sc.Name + "的执行者。严格遵循下面的技能提示词响应。\n\n==== 技能提示词 ====\n" + sc.SystemPrompt
	if strings.TrimSpace(sc.Template) != "" {
		sys += "\n\n==== 回答骨架模板 ====\n" + sc.Template
	}
	// For typed flow skills, remind the model to point at the downloadable file.
	if sc.SkillType != model.SkillTypeWrite && strings.TrimSpace(sc.Attachment) != "" {
		sys += "\n\n注意：有一个模板文件《" + sc.Attachment + "》会随本次回答下发，请在回答中明确指引用户下载使用。"
	}
	return sys
}

// argBlockOf 把本次的要素整理成 user 侧的一段。写作类读作「具体要求」，
// 办事类读作「要办理的事项」——同一份数据，两种技能对它的心理定位不同。
func argBlockOf(sc *SkillContent, args map[string]string) string {
	var argBlock strings.Builder
	if sc.SkillType == model.SkillTypeWrite {
		argBlock.WriteString("# 本次写作的具体要求/内容\n\n")
	} else {
		argBlock.WriteString("# 本次要办理/查询的具体事项及已知信息\n\n")
	}
	if len(args) == 0 {
		argBlock.WriteString("（用户本次对话中提供的细节）\n")
		return argBlock.String()
	}
	for k, v := range args {
		if strings.TrimSpace(v) == "" {
			continue
		}
		fmt.Fprintf(&argBlock, "- %s: %s\n", k, v)
	}
	return argBlock.String()
}

// FlattenParams converts raw extracted params (which may hold arrays / nested
// values, e.g. docgen columns) into string values by compact-JSON-encoding any
// non-scalar so downstream string-only consumers stay intact.
func (e *Engine) FlattenParams(raw map[string]interface{}) map[string]string {
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		switch t := v.(type) {
		case string:
			out[k] = t
		case nil:
			out[k] = ""
		default:
			b, err := json.Marshal(t)
			if err != nil {
				out[k] = fmt.Sprintf("%v", v)
			} else {
				out[k] = string(b)
			}
		}
	}
	return out
}

// detectRequestedFormat scans the user's original message and extracted params
// for a concrete document format (word/excel/pdf/ppt or a file extension). It
// returns a canonical format name, or "" if the request doesn't pin one.
func detectRequestedFormat(userMsg string, args map[string]string) string {
	hay := strings.ToLower(userMsg + " " + strings.Join(mapValues(args), " "))
	for _, kw := range []string{"pdf", "ppt", "pptx", "powerpoint", "幻灯片"} {
		if strings.Contains(hay, kw) {
			if kw == "ppt" || kw == "pptx" || kw == "powerpoint" || kw == "幻灯片" {
				return "ppt"
			}
			return "pdf"
		}
	}
	// word / excel often appear alongside the keyword; check explicit tokens
	if strings.Contains(hay, "word") || strings.Contains(hay, "docx") {
		return "word"
	}
	if strings.Contains(hay, "excel") || strings.Contains(hay, "xlsx") {
		return "excel"
	}
	return ""
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func fileExt(f string) string {
	switch f {
	case "word":
		return "docx"
	case "excel":
		return "xlsx"
	case "ppt":
		return "pptx"
	case "pdf":
		return "pdf"
	}
	return f
}

// DocResult is the outcome of generating an office file via a docgen skill.
type DocResult struct {
	Filename    string // final download name, e.g. 员工信息表.xlsx
	Bytes       []byte
	ContentType string
	Summary     string // short human text shown alongside the download
	// Spec is the normalised DOCJSON actually used to render the file. It is
	// persisted into session history (see appendDocSpecSuffix) so a follow-up
	// turn ("再加一行") can rebuild the FULL document instead of asking the LLM
	// to recollect content it never saw.
	Spec string
	// Passthrough 为 true 表示本轮没有采用模型给的内容，而是用上一轮产物原文
	// 确定性渲染的（见 doc_passthrough.go）。给调用方/测试一个可判定的身份，
	// 不必去猜 Summary 里的文案。
	Passthrough bool
	// Coverage 是本轮 doc 对上一轮正文的覆盖率（0~1）。仅当上一轮产物是长文
	// 时才有意义，其余情况为 0。
	Coverage float64
}

// GenerateDoc streams nothing; it drives a docgen skill to produce an office
// file. The skill's system_prompt contracts with the LLM to emit a strict JSON
// block describing the doc. We capture the whole (non-streaming) response,
// parse it, render the file and return it for delivery via SSE file event.
//
// history carries the previous turns of this session: when an earlier turn of
// the same session already generated a document, its spec is replayed into the
// prompt as the base so "再加一行 / 把单价改成 8000" produces the full updated
// table instead of a brand-new table containing only the delta.
func (e *Engine) GenerateDoc(ctx context.Context, id string, sc *SkillContent, args map[string]string, userMsg string, history []Message) (*DocResult, error) {
	sys := "你是" + sc.Name + "的执行者。严格遵循下面的技能提示词。\n\n==== 技能提示词 ====\n" + sc.SystemPrompt

	var argBlock strings.Builder
	argBlock.WriteString("# 本次要生成的文档信息（来自用户或推断）\n\n")
	// Replay the previously generated spec (if any) BEFORE the raw request so
	// the model sees it as established state rather than something to invent.
	prevSpec := extractLastDocSpec(history)
	if prevSpec != "" {
		argBlock.WriteString("## 上一轮本会话已生成的文档规格（本轮的基础，JSON）：\n")
		argBlock.WriteString(prevSpec)
		argBlock.WriteString("\n\n## 续改铁律：\n")
		argBlock.WriteString("如果用户本轮是在上面这份文档上做增/删/改（例如「再加一行」「把单价改成 8000」「删掉第二行」「换个标题」），" +
			"你必须输出**完整的**更新后规格：保留上一轮的所有列（cols）、所有数据行（rows）与段落（parags），" +
			"只把用户这次的改动应用上去。禁止只输出新增或修改的那一部分，禁止改变原有列名/表头，禁止丢行。" +
			"格式（format）也保持与上一轮一致，除非用户明确要求换成别的格式。\n\n")
	}
	// 上一轮的**正文产物**必须原样进 prompt。
	// 老实现只回放上一轮的「文档规格」（extractLastDocSpec），对「用户先让技能
	// 写了一篇新闻稿、这一轮说『整理成 word』」这条链路完全是盲的：规格还没生成
	// 过 → 什么都不注入 → 模型凭空造一份新文档，用户看到的就是「它不管我上一轮
	// 写的东西」。产物正文走 ContextBlock 的产物层（逐字保留、不压缩）。
	if block := e.ContextBlock(ctx, id, history); block != "" && block != "（无历史）" {
		argBlock.WriteString("## 本会话前文（含用户已认可的产物原文）\n")
		argBlock.WriteString("用户说「整理成…/导出成…/改一下…」时，指的就是下面的产物；")
		argBlock.WriteString("必须把它的正文原样放进本轮文档，不要重写、不要压缩、不要另起炉灶。\n\n")
		argBlock.WriteString(block)
		argBlock.WriteString("\n\n")
	}
	if strings.TrimSpace(userMsg) != "" {
		argBlock.WriteString("## 用户的原始请求（格式与内容以此为准）：\n")
		argBlock.WriteString(userMsg)
		argBlock.WriteString("\n\n")
	}
	// If the user's request names a concrete format (word/excel/pdf/ppt or a
	// file-extension), surface it as a hard directive so the generator honors
	// it instead of defaulting to Excel.
	if f := detectRequestedFormat(userMsg, args); f != "" {
		argBlock.WriteString("## 格式硬性要求：用户明确要求生成「" + f + "」格式文档，format 字段和文件名必须对应（如 ." + fileExt(f) + "）。\n\n")
	}
	if len(args) == 0 {
		argBlock.WriteString("（用户提供的细节请自行整理进 JSON）\n")
	} else {
		for k, v := range args {
			if strings.TrimSpace(v) == "" {
				continue
			}
			fmt.Fprintf(&argBlock, "- %s: %s\n", k, v)
		}
	}
	argBlock.WriteString("\n只输出一个 JSON 对象（见技能提示词中的 DOCJSON 契约），不要输出任何其他文字，不要使用 markdown 代码块。")

	// 关思考链：这一步是「把用户说的话填进给定的 JSON 契约」，规则全在提示词里，
	// 思考链只是把几十秒的等待摊在用户面前；真要产思考链（provider 忽略开关）时，
	// 片段会被当成中间材料流出去。
	// docgen 这一跳过去**不留任何痕**：失败只在 SSE 里吐一个 error 帧，stderr 一个字没有。
	// 线上实测（2026-09-20 04:35 轮）这一跳跑了 159.9s 然后抛错，journalctl 里干干净净 ——
	// 结果「为什么失败」只能靠猜 provider。失败必须留得下证据，成功也必须有自己的耗时数字
	// （它跟 [plan]/[write-skill] 一个量级，是这一轮里最贵的一跳）。
	docT0 := time.Now()
	out, err := e.llm.StreamChat(ctx, sys, argBlock.String(), llm.StreamOpts{
		DisableThinking: true,
		JSONMode:        true,
		OnReasoning:     reasoningSink(ctx),
		OnContent:       contentSink(ctx),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[docgen] 失败 hop=%.1fs err=%v\n",
			time.Since(docT0).Seconds(), err)
		return nil, err
	}
	doc, err := parseDocJSON(out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[docgen] 规格解析失败 hop=%.1fs out=%d 字节 out_head=%q\n",
			time.Since(docT0).Seconds(), len([]rune(out)), headRunes(out, 200))
		return nil, fmt.Errorf("解析文档规格失败: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[docgen] ok hop=%.1fs out=%d 字节 format=%s\n",
		time.Since(docT0).Seconds(), len([]rune(out)), doc.Format)
	// Normalise format alias to canonical extension for the filename.
	if doc.Format != "" {
		switch strings.ToLower(doc.Format) {
		case "word", "docx", "doc":
			doc.Format = "word"
		case "excel", "xlsx", "xls":
			doc.Format = "excel"
		case "pdf":
			doc.Format = "pdf"
		case "ppt", "pptx":
			doc.Format = "ppt"
		}
	}
	if doc.Filename == "" {
		ext := ".docx"
		switch doc.Format {
		case "excel":
			ext = ".xlsx"
		case "pdf":
			ext = ".pdf"
		case "ppt":
			ext = ".pptx"
		}
		doc.Filename = "document" + ext
	}
	// 渲染前的最后一道闸门：用户说「把上一轮那篇整理成 Word」时，语义是**搬运**
	// ——内容不变、只换容器。这件事不能押在模型自觉上（prompt 已经给了原文，模型
	// 仍可能重写/压缩/另写一篇，这正是用户抱怨的「没有管之前生成的内容」）。所以
	// 用覆盖率做确定性判定：模型写的东西跟上一轮正文对不上，就丢掉它，用原文直通
	// 渲染。上一轮产物原文来自产物层（splitArtifacts），与注入 prompt 的是同一份。
	if prev := lastArtifactText(history); prev != "" {
		hit, cov, note := shouldPassthrough(prev, doc, userMsg)
		if hit {
			fmt.Fprintf(os.Stderr, "[doc-passthrough] session=%s 上一轮正文覆盖率=%.2f，改用原文直通渲染\n", id, cov)
			doc = buildPassthroughDoc(prev, doc)
			return e.renderDocResult(doc, note, cov, true)
		}
		if cov > 0 && cov < passthroughCoverageMin {
			// 覆盖率低却没兜底，只可能是本轮不是搬运意图（新话题）。同样要留痕：
			// 否则「模型又在重写上一轮内容」这件事没人看得见。
			fmt.Fprintf(os.Stderr, "[doc-passthrough] session=%s 上一轮正文覆盖率=%.2f（本轮非搬运意图，未启用直通）\n", id, cov)
		}
		// 覆盖率照常透出：单测与排查要能看见「模型确实搬运了」，而不是一个 0。
		return e.renderDocResult(doc, "", cov, false)
	}
	return e.renderDocResult(doc, "", 0, false)
}

// renderDocResult 把一份已经定稿的 doc 规格渲染成文件并组装 DocResult。
//
// extraNote 非空时追加在 Summary 末尾：Summary 是既有 SSE 里就已经下发给用户的
// 那段文本（「已为您生成《…》」），兜底说明走它，用户必然看得见，不需要新增事件
// 类型、也不动 SSE 协议。
func (e *Engine) renderDocResult(doc docgen.Doc, extraNote string, cov float64, passthrough bool) (*DocResult, error) {
	data, err := docgen.Generate(doc)
	if err != nil {
		return nil, err
	}
	spec, _ := json.Marshal(map[string]any{
		"format":   doc.Format,
		"filename": doc.Filename,
		"title":    doc.Title,
		"cols":     doc.Cols,
		"rows":     doc.Rows,
		"parags":   doc.Parags,
	})
	summary := "已为您生成《" + doc.Filename + "》，点击下方文件即可下载。"
	if extraNote != "" {
		summary += extraNote
	}
	return &DocResult{
		Filename:    docgen.SafeFilenameAs(doc.Filename, doc.Format),
		Bytes:       data,
		ContentType: docgen.ContentType(doc.Format),
		Summary:     summary,
		Spec:        string(spec),
		Passthrough: passthrough,
		Coverage:    cov,
	}, nil
}

// docSpecMarker tags the assistant history message that carries a generated
// document's spec, mirroring the "已填值:" tracer used by the template path.
const docSpecMarker = "已生成规格:"

// extractLastDocSpec returns the most recent generated document spec persisted
// in this session's history, or "" when no earlier turn generated one.
//
// Scans assistant messages only, and takes the LAST hit so a conversation that
// generated several documents continues from the newest one.
func extractLastDocSpec(history []Message) string {
	last := ""
	for _, m := range history {
		if m.Role != "assistant" {
			continue
		}
		idx := strings.LastIndex(m.Content, docSpecMarker)
		if idx < 0 {
			continue
		}
		rest := strings.TrimSpace(m.Content[idx+len(docSpecMarker):])
		// The JSON object is the tail of the message; trim any trailing prose
		// the summariser may have appended after the closing brace.
		if end := strings.LastIndex(rest, "}"); end >= 0 {
			rest = rest[:end+1]
		}
		if strings.HasPrefix(rest, "{") && strings.HasSuffix(rest, "}") {
			last = rest
		}
	}
	return last
}

// docSpecJSON is a tolerant mirror of docgen.Doc. Cells are kept as raw JSON so
// a numeric cell (2, 65000, 0.5) can be coerced instead of aborting the whole
// spec — see rawScalar.
type docSpecJSON struct {
	Format   string              `json:"format"`
	Filename string              `json:"filename"`
	Title    string              `json:"title"`
	Cols     []json.RawMessage   `json:"cols"`
	Rows     [][]json.RawMessage `json:"rows"`
	Parags   []json.RawMessage   `json:"parags"`
}

// parseDocJSON extracts the DOCJSON object from an LLM reply and binds it to a
// docgen.Doc. It is tolerant of surrounding prose but insists on the block.
func parseDocJSON(s string) (docgen.Doc, error) {
	raw := extractJSON(s)
	if raw == "" {
		return docgen.Doc{}, errors.New("未找到文档规格 JSON")
	}
	var sp docSpecJSON
	if err := json.Unmarshal([]byte(raw), &sp); err != nil {
		return docgen.Doc{}, err
	}
	doc := docgen.Doc{Format: sp.Format, Filename: sp.Filename, Title: sp.Title}
	for _, c := range sp.Cols {
		v, _ := rawScalar(c)
		doc.Cols = append(doc.Cols, v)
	}
	for _, row := range sp.Rows {
		vals := make([]string, 0, len(row))
		for _, c := range row {
			v, _ := rawScalar(c)
			vals = append(vals, v)
		}
		doc.Rows = append(doc.Rows, vals)
	}
	for _, p := range sp.Parags {
		v, _ := rawScalar(p)
		doc.Parags = append(doc.Parags, v)
	}
	if doc.Format == "" {
		return docgen.Doc{}, errors.New("缺少 format 字段")
	}
	return doc, nil
}

// FillResult is the outcome of smart-filling a template skill's attachment docx.
type FillResult struct {
	Filename    string
	Bytes       []byte
	ContentType string
	Summary     string
	// Vals holds the exact field→value map that was written into the filled
	// docx this turn. The chat handler persists it into session history so a
	// follow-up turn can continue editing the SAME values instead of wiping
	// untouched fields (multi-turn incremental fill).
	Vals map[string]string
	// Clarify is set when the model decides the request needs more information
	// before it can fill the template (user gave an edit/continue instruction
	// with key facts missing and did NOT authorize inventing data). When set,
	// Bytes/Filename stay empty and the caller should ask Clarify.Questions
	// instead of delivering a file.
	Clarify *FillClarify
}

// FillClarify carries the clarification questions the assistant should ask
// back to the user before proceeding with a template fill.
type FillClarify struct {
	Questions []string
}

// FillDoc implements smart template filling: it reads the template skill's docx
// attachment, extracts the {{field}} placeholders it declares, asks the LLM to
// map the user's request to concrete values per field, then in-place fills the
// placeholders and returns a filled docx. The template's own placeholders act
// as exact, run-safe anchors — the LLM only maps values, it never guesses where
// text should go.
//
// history is the prior conversation turns for this session. It lets a follow-up
// request ("把部门改成市场部") see what was already filled last turn and keep
// the untouched fields, instead of blanking them. Previous values are gen from
// the history (via prior assistant fill summaries) and passed into the mapping
// prompt as the current state.
func (e *Engine) FillDoc(ctx context.Context, id string, sc *SkillContent, userMsg string, history []Message) (*FillResult, error) {
	if sc.Attachment == "" {
		return nil, errors.New("该技能没有可填充的模板文件")
	}
	path, ok := e.AttachmentPath(sc.Slug, sc.Attachment)
	if !ok {
		return nil, errors.New("模板文件不可用: " + sc.Attachment)
	}
	tplBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Both Word and Excel templates are fillable (layout-preserving).
	lower := strings.ToLower(sc.Attachment)
	isXLSX := strings.HasSuffix(lower, ".xlsx")
	if !strings.HasSuffix(lower, ".docx") && !isXLSX {
		return nil, errors.New("暂仅支持填充 .docx / .xlsx 模板")
	}
	var keys []string
	if isXLSX {
		keys, err = docgen.ExtractFieldsXLSX(tplBytes)
	} else {
		keys, err = docgen.ExtractFields(tplBytes)
	}
	if err != nil {
		return nil, errors.New("解析模板失败: " + err.Error())
	}
	if len(keys) == 0 {
		return nil, errors.New("模板中没有可填充的字段（模板可能不是表单结构）")
	}

	// Recover the previous turn's filled values from session history so a
	// follow-up ("把部门改成市场部") edits in place instead of wiping the
	// fields the user didn't mention. Prior assistant fill summaries look like
	// "已按您提供的信息填充《X.docx》…已填值: name=张三, dept=技术部".
	prev := extractFilledVals(history)

	// Ask the LLM to map fields→values from the user's natural-language request,
	// merging with the previous turn's values for incremental continuation.
	var stateBlock strings.Builder
	if len(prev) > 0 {
		stateBlock.WriteString("\n当前已有值（来自上次已填的结果，未提及的字段请保留这些值，不要清空）：\n")
		for _, k := range keys {
			if v, ok := prev[k]; ok && strings.TrimSpace(v) != "" {
				fmt.Fprintf(&stateBlock, "- %s = %s\n", k, v)
			}
		}
	} else {
		stateBlock.WriteString("\n（这是首次填写：本次主动提及的字段填用户给的值，未提及的字段填空字符串。）\n")
	}

	// Deterministic structural-capacity hint for table-style templates
	// (e.g. 品目明细行). 模板里有多少个 itemN 占位行、上一轮已占了几个、剩几个，
	// 直接算出来写进提示，避免让 LLM 猜测“还能不能直接填空行”。
	// 一旦剩余槽位不足又要新增，就明确要求用 ops add_row 在明细表(索引1)追加。
	var capBlock strings.Builder
	filledRows := 0
	for _, k := range keys {
		if strings.HasPrefix(k, "item") && strings.Contains(k, "Name") {
			if v, ok := prev[k]; ok && strings.TrimSpace(v) != "" {
				filledRows++
			}
		}
	}
	if filledRows > 0 {
		slot := 0
		seen := map[int]bool{}
		for _, k := range keys {
			if strings.HasPrefix(k, "item") && strings.Contains(k, "Name") {
				var n int
				fmt.Sscanf(strings.TrimPrefix(strings.SplitN(k, "Name", 2)[0], "item"), "%d", &n)
				if n > 0 && !seen[n] {
					seen[n] = true
					slot++
				}
			}
		}
		free := slot - filledRows
		capBlock.WriteString(fmt.Sprintf(
			"\n模板品目明细行状态：共 %d 个占位行(item1~item%d)，上一轮已占 %d 行，还能直接填 %d 行。\n"+
				"- 若用户又要新增、且剩余空行还够：直接填 fields 里对应的 itemN 空行即可，不要 add_row。\n"+
				"- 若剩余空行为 0 且用户还要再加：**必须在 ops 里 add_row（table 索引 1，即装品目的那张明细表）**追加新行，相关数据放 ops 的 values 里，不要再往不存在的 itemN key 写 fields。\n",
			slot, slot, filledRows, free))
	}
	stateBlock.WriteString(capBlock.String())

	sys := "你负责把用户的自然语言请求，映射到一张表单/文档模板的可填字段上，并把值填进去。\n" +
		"模板可填字段（key 表示字段，label 是它旁边的中文提示）：\n" +
		toFieldLines(keys) +
		"\n规则：\n" +
		stateBlock.String() +
		"先判断「信息是否足够」再决定这次是**直接填**还是**反问用户**：\n" +
		"- **直接填（输出 fields 对象，包含所有 key 的最终值）** 当：\n" +
		"  a) 首次填写，且用户明确授权自由发挥（说\"随便编\"/\"数据你定\"/\"随便填\"/\"你看着办\"），或用户给的信息已经覆盖模板所需；或\n" +
		"  b) 延续编辑，用户提到的修改点完全明确（如\"把乙方代表改成赵敏\"），未提及字段保留已有值；或\n" +
		"  c) 字段缺失但缺失的是无关紧要/可用合理默认值兜底的业务字段（日期用今天、金额的零头、人名用示例等）。\n" +
		"  特别说明——当用户是**延续编辑**（上次已填过、这次在改/补），且修改涉及**表格结构本身**（要增加新行、删除某行、新增一个品目/条目、补充一列），此时除了 fields 之外**还要输出 ops**（见下方 ops 用法），让助手真正改写表格行数/列结构，而不只是替换已有占位符。\n" +
		"- **反问（只输出 clarify 字段，不输出 fields）** 当：用户明确给出了一项**具体的编辑/新增操作意图**（如\"多加四五种不同的服务器\"\"补上第四个品目\"），但**连要加什么品目都没说清、或涉及替换冲突**——例如只说\"多加几种设备\"却没说是哪些；或要求顶替某个既有条目但没说替成什么。此时**绝不能擅自编造**，必须反问。\n" +
		"  【例外，直接填】当用户已经说清要新增的具体品目名称（如\"路由器\"\"网线\"），区分追加/替换通常由用词决定：说\"再加/再补/多放/再采购\"等表追加，默认追加到已有条目之后的新行即可，**无需反问追加还是替换**；若数量/规格未给但用户说\"随便/你定/请自拟\"，视为授权自拟，直接填。\n" +
		"1. 直接填时：每个 key 都要给最终值。用户本次明确修改的字段用新值覆盖；未提及且无上一轮值的字段，用合理示例数据兜底（但**绝不占位符、绝不空字符串**）。\n" +
		"   例外：延续编辑（补几行/增删条目到已填过的表格型内容）时，**只改用户提到的部分**——新增条目按用户给的信息填（用户说\"随便\"可自拟型号/数量/单价），**未提到的既有条目与本该留空的新增条目保持原值或留空**，不要为了\"每个key都有值\"而把多余的空行全部编造填满。\n" +
		"2. 值就是直接填进表格单元格的文本，保留用户原意，不要加注释或解释。\n" +
		"3. 只输出一个 JSON 对象，不要任何其他文字或 markdown 代码块：\n" +
		"   - 直接填：{\"fields\": {字段key: 值}, \"ops\": [...]}（fields 含所有 key；ops 仅在需要**改表格/段落结构**时给出）\n" +
		"   - 反问：{\"clarify\": { \"need\": true, \"questions\": [\"问题1\", \"问题2\"] }}（questions 是面向用户的中文追问，具体、有针对性，一次问清楚所有缺口）\n" +
		"ops 是可选的**结构编辑操作列表**，每项形如：\n" +
		"  {\"action\":\"add_row\",\"table\":1,\"values\":[\"品目\",\"数量\",\"单价\"]}   —— 在指定 table 表末尾追加一行并填入 values（列数须匹配，values 平铺该行各单元格）\n" +
		"  {\"action\":\"del_row\",\"table\":1,\"row\":3}                             —— 删除该表第 3 行（0 基）\n" +
		"  {\"action\":\"set_para\",\"paragraph_text\":\"...\",\"text\":\"新段落文本\"}  —— 改写某段（paragraph_text 填该段现有开头几个字，用于定位）\n" +
		"  {\"action\":\"add_para\",\"text\":\"要追加的段落文本\"}                    —— 文末追加一段\n" +
		"表格索引从 0 开始，按文档从前到后编号。**若要新增的是品目/条目数据，表索引必须是装有该品目明细的那个表**（通常也就是含「品目/规格型/数量/单价」表头的表，在采购合同里是第 2 张表、索引 1；第 1 张表是甲乙方、索引 0）。\n" +
		"生成规则：\n" +
		"1. **先看模板里是否已有能直接填的空行**——品目明细表一般预留了多个空行（比如已有 item1~item8 八个占位行）。用户要\"增加品目\"时，**优先直接填空行（item3、item4...）放进 fields 即可，不要 add_row**；只有当预留空行全被占满、确实没有多余行可填时，才用 add_row 真正加新行。\n" +
		"2. 凡是\"增加新表格行/删除某行/追加段落\"这类结构操作才放进 ops；纯改某个已有占位符的值放 fields（不必用 ops）。\n" +
		"3. 不要重复：一个由 ops 新增的行，其数据就放 ops 的 values 里，不要再塞进 fields 对应 key。\n" +
		"4. **追加新行务必选对 table 索引**（装明细的那个表），千万别往甲乙方/签字区那张表里加品目。"
	prompt := "用户请求：\n" + userMsg
	if hist := e.ContextBlock(ctx, id, history); hist != "" && hist != "（无历史）" {
		prompt = "对话历史（供参考，重点回应最新消息）：\n" + hist + "\n\n" + prompt
	}
	// 同 720 那一跳：字段映射是结构化抽取，关思考链 + 流式（思考片段当中间材料）。
	out, err := e.llm.StreamChat(ctx, sys, prompt, llm.StreamOpts{
		DisableThinking: true,
		JSONMode:        true,
		OnReasoning:     reasoningSink(ctx),
		OnContent:       contentSink(ctx),
	})
	if err != nil {
		return nil, err
	}
	vals := map[string]string{}
	var ops []docgen.Op
	if raw := extractJSON(out); raw != "" {
		// Two accepted shapes:
		//   {"fields":{k:v...},"ops":[...]}  or  {"clarify":{...}}
		//   legacy flat {k:v...}
		// NOTE: values are decoded tolerantly (see decodeFieldVals) because the
		// LLM routinely emits real JSON numbers for 数量/单价/金额
		// (e.g. "item2Qty":5). Decoding straight into map[string]string used to
		// fail on those, which made a continuation turn silently keep the
		// PREVIOUS values — i.e. "我改了交换机它没改".
		var env struct {
			Fields  json.RawMessage `json:"fields"`
			Ops     []docgen.Op     `json:"ops"`
			Clarify struct {
				Need      bool     `json:"need"`
				Questions []string `json:"questions"`
			} `json:"clarify"`
		}
		envErr := json.Unmarshal([]byte(raw), &env)

		fv := map[string]string{}
		if envErr == nil && len(env.Fields) > 0 {
			var derr error
			if fv, derr = decodeFieldVals(env.Fields); derr != nil {
				fmt.Fprintf(os.Stderr, "[filldoc] fields decode err=%v raw=%q\n", derr, out)
				fv = map[string]string{}
			}
		}

		switch {
		case len(fv) > 0:
			vals = fv
			ops = env.Ops
		case envErr == nil && env.Clarify.Need:
			// Request clarification: no file, just questions back to the user.
			qs := make([]string, 0, len(env.Clarify.Questions))
			for _, q := range env.Clarify.Questions {
				if s := strings.TrimSpace(q); s != "" {
					qs = append(qs, s)
				}
			}
			if len(qs) > 0 {
				return &FillResult{Clarify: &FillClarify{Questions: qs}}, nil
			}
			// need=true but no questions — treat as an empty result below (guarded).
		default:
			// Legacy flat {key: value} shape (also reached when `fields` was empty).
			lv, lerr := decodeFieldVals([]byte(raw))
			if lerr != nil {
				fmt.Fprintf(os.Stderr, "[filldoc] unmarshal err=%v raw=%q\n", lerr, out)
			} else {
				vals = lv
			}
		}

		// Guard against an empty value map: filling with no values would produce a
		// doc that still carries {{placeholder}} text everywhere — which reads as a
		// plain-text/txt file and silently confuses the user. If we somehow got a
		// result with no usable values AND no structural ops, ask for clarification
		// instead of emitting a broken artifact. (纯结构编辑——只有 ops 没有 fields——
		// 是合法的，比如"在第3行下面追加一行预算求和"，此时不应反问。)
		if len(vals) == 0 && len(ops) == 0 {
			q := "您想往这张表/文档里填哪些内容？请告诉我具体的关键信息（比如品名、数量、金额、往来单位、日期等），我按您给的填。"
			if hasMaterial(history) {
				// 用户已经贴过素材，还回头问「请告诉我品名/数量/金额」等于让他把
				// 刚贴过的一万字再说一遍——这正是用户实测点名的「要的时候也不是
				// 根据我目前提供的信息的基础上来进一步补充，而是直接通用的补充」。
				// 有素材时改成指着他给的素材问，让他只需补一句对应关系。
				q = "您提供的素材我已经看到，但没能从里面解析出可直接填入模板的字段。" +
					"请指明素材里哪一段对应模板里的哪些项（例如「第 2 段的设备清单填到品目列」），我按您指的位置填。"
			}
			return &FillResult{Clarify: &FillClarify{Questions: []string{q}}}, nil
		}
	} else {
		fmt.Fprintf(os.Stderr, "[filldoc] no json in reply: %q\n", out)
	}

	// Placeholder-fill guard: a continuation turn that returns empty for fields the
	// LLM "didn't touch" would WIPE prior edits (e.g. 合计金额在第二轮被清空).
	// Back-fill any key not explicitly overwritten this round from the previous
	// turn's values, so "在已修改文件的基础上继续改"永不丢历史值。
	// 覆盖两种情况：LLM 本轮给了空串（v==""），或 LLM 压根没输出该 key（!ok）。
	// （例外：prev 没有该 key 或 prev 为空串时不动；用户本轮显式给非空值则用新值。）
	for k, pv := range prev {
		if pv == "" {
			continue
		}
		v, ok := vals[k]
		if !ok {
			// LLM 本轮完全没提到该 key → 强制保留上一轮值
			vals[k] = pv
		} else if strings.TrimSpace(v) == "" {
			vals[k] = pv
		}
	}

	var filled []byte
	if len(ops) > 0 {
		// 结构性编辑：用 gooxml 渲染引擎先按 ops 改写表格/段落结构，
		// 再对剩余 {{placeholders}} 做值填充。
		oj, _ := json.Marshal(ops)
		fmt.Fprintf(os.Stderr, "[fill] structural ops received=%s\n", oj)
		ir := docgen.IR{Mode: "edit", Ops: ops}
		mid, mErr := docgen.RenderIR(ir, tplBytes)
		if mErr != nil {
			// 结构引擎失败（如模板里没有可编辑的表格）→ 回退纯占位符填充
			fmt.Fprintf(os.Stderr, "[fill] ops engine failed=%v; fallback to placeholder fill\n", mErr)
			mid = tplBytes
		}
		if isXLSX {
			filled, err = docgen.FillXLSX(mid, vals)
		} else {
			filled, err = docgen.Fill(mid, vals)
		}
	} else if isXLSX {
		filled, err = docgen.FillXLSX(tplBytes, vals)
	} else {
		filled, err = docgen.Fill(tplBytes, vals)
	}
	if err != nil {
		return nil, err
	}
	ctype := "docx"
	if isXLSX {
		ctype = "xlsx"
	}
	// The delivered file is a FILLED artifact — name it distinctly so it isn't
	// confused with the blank template: strip any trailing "模板"/"template"
	// from the base name and append a date-based version, e.g.
	// "采购合同模板.docx" -> "采购合同_20260910.docx".
	version := time.Now().Format("20060102")
	fname := versionedFillName(sc.Attachment, version)
	return &FillResult{
		Filename:    fname,
		Bytes:       filled,
		ContentType: docgen.ContentType(ctype),
		Summary:     "已按您提供的信息填充《" + fname + "》，点击下方文件即可下载填写后的版本。",
		Vals:        vals,
	}, nil
}

// versionedFillName renames a template file so a filled artifact carries a
// distinct, versioned name instead of the generic template name.
func versionedFillName(attachment, version string) string {
	base := attachment
	ext := ""
	if i := strings.LastIndex(base, "."); i >= 0 {
		ext = base[i:]
		base = base[:i]
	}
	// strip a trailing common template marker
	for _, m := range []string{"模板", "template", "Template", "template_"} {
		if strings.HasSuffix(base, m) {
			base = strings.TrimSuffix(base, m)
			break
		}
	}
	base = strings.TrimRight(base, "_- ")
	if base == "" {
		base = "文档"
	}
	return base + "_" + version + ext
}

// rawScalar converts a JSON scalar (string / number / bool) into its string
// form. ok is false for JSON null and for nested objects/arrays, which have no
// scalar representation.
//
// This is the single tolerant coercion point for all LLM-authored JSON: models
// freely write 5, 7800, 0.5 where we asked for a string, and a strict unmarshal
// into []string / map[string]string would throw away the WHOLE payload over one
// such cell. Integral floats become "5" not "5.0".
func rawScalar(raw json.RawMessage) (string, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return "", false
	}
	switch s[0] {
	case '"':
		var sv string
		if err := json.Unmarshal(raw, &sv); err != nil {
			return "", false
		}
		return sv, true
	case '{', '[':
		return "", false // nested structure, not a scalar value
	default:
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			if f == math.Trunc(f) && math.Abs(f) < 1e15 {
				return strconv.FormatInt(int64(f), 10), true
			}
			return strconv.FormatFloat(f, 'f', -1, 64), true
		}
		return s, true // true / false / bare token
	}
}

// decodeFieldVals tolerantly decodes a JSON object of key → value into a flat
// map[string]string.
//
// Why not map[string]string directly: the LLM emits real JSON scalars —
// "item2Qty":5, "item2Price":7800, "amount":193600, sometimes booleans. A
// direct json.Unmarshal into map[string]string fails on any non-string value
// and drops the WHOLE object, so a continuation turn ("把交换机改成5台")
// silently fell back to the previous turn's values and the file never changed.
//
// Scalars are coerced to their string form (integral floats become "5" not
// "5.0"); nested objects/arrays/null are skipped because they are not field
// values (e.g. the sibling "ops" array).
func decodeFieldVals(b []byte) (map[string]string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(m))
	for k, rawv := range m {
		if v, ok := rawScalar(rawv); ok {
			out[k] = v
		}
	}
	return out, nil
}

// extractFilledVals scans prior session messages for the assistant fill summary
// marker "已填值:" and returns the last set of already-filled field values, so
// a follow-up turn can continue editing them incrementally.
func extractFilledVals(history []Message) map[string]string {
	// find the assistant message containing the fill summary marker
	marker := "已填值:"
	last := ""
	for _, m := range history {
		if m.Role == "assistant" && strings.Contains(m.Content, marker) {
			last = m.Content
		}
	}
	if last == "" {
		return nil
	}
	// take everything after the marker
	idx := strings.Index(last, marker)
	rest := last[idx+len(marker):]
	rest = strings.TrimSpace(rest)
	vals := map[string]string{}
	for _, pair := range strings.Split(rest, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		eq := strings.Index(pair, "=")
		if eq <= 0 {
			continue
		}
		vals[strings.TrimSpace(pair[:eq])] = strings.TrimSpace(pair[eq+1:])
	}
	if len(vals) == 0 {
		return nil
	}
	return vals
}

// toFieldLines renders the extractable template field keys as a compact,
// human-readable label list for the LLM.
func toFieldLines(keys []string) string {
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "- key=%s\n", k)
	}
	return b.String()
}

// WantsFill is a lightweight guard used by the chat handler to decide whether
// a user turn against a template skill is a fill request (vs. a plain flow
// question). It scans for explicit fill vocabulary so regular queries still get
// the normal attachment-download behaviour.
func WantsFill(msg string) bool {
	h := strings.ToLower(strings.TrimSpace(msg))
	for _, kw := range []string{"填写", "填上", "帮我填", "填一下", "填表", "填入", "写进模板", "在表里填", "填好", "把表填", "改成", "改为", "修改", "调整", "替换", "更新", "换一个", "补上", "补填", "填进去", "填份", "填个",
		// 让 AI 编造/生成数据填进模板
		"随便编", "编点数据", "编份", "造一份", "生成数据", "数据填", "填好数据", "填个数据", "造个", "编个",
		// explicit-template scenarios: 用户点名"用XX模板做/起草/起草一份/出一份…"
		// must route to template fill rather than a blank template download.
		"模板", "起草", "做一份", "出一份", "写一份", "生成一份", "来一份", "帮我做", "帮我起草", "帮我出", "帮我写",
		// document words that imply filling a declared form/template
		"合同", "申请表", "验收单", "申报表", "报销单", "请假单", "通知书", "委托书", "协议书"} {
		if strings.Contains(h, kw) {
			return true
		}
	}
	return false
}

// TemplateFillSkillInHistory returns the slug of the last template skill whose
// assistant turn persisted filled values (marker "已填值:") — i.e. the user was
// filling a template and this turn is likely a continuation/editing of it.
// Returns "" when no prior template fill exists in the given history.
// msg is the current user turn; it must look like an editing/continuation turn
// for the relay to kick in (so a brand-new unrelated question doesn't hijack).
func TemplateFillSkillInHistory(history []Message, msg string) string {
	editish := false
	for _, kw := range []string{"改成", "改为", "修改", "调整", "替换", "更新", "换", "补", "填", "加", "删", "去掉"} {
		if strings.Contains(strings.ToLower(strings.TrimSpace(msg)), kw) {
			editish = true
			break
		}
	}
	if !editish {
		return ""
	}
	// scan history (most recent first) for an assistant fill marker
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		if m.Role == "assistant" && strings.Contains(m.Content, "已填值:") && strings.TrimSpace(m.SkillSlug) != "" {
			return strings.TrimSpace(m.SkillSlug)
		}
	}
	return ""
}

// Push appends a message to a session's history, trimming oldest to maxHist.
// The FULL history (untouched by maxHist) is also persisted to disk so a
// restart / refresh does not wipe continuation context.
func (e *Engine) Push(id string, m Message) {
	// 素材打标：用户贴的长要求/范文在这里被标记成 KindMaterial，之后才会进
	// ContextBlock 的素材层逐字保留。不打这一步，它就会被近轮层砍成 600 字。
	m = markMaterial(m)
	e.mu.Lock()
	defer e.mu.Unlock()
	h := e.sessions[id]
	h = append(h, m)
	if len(h) > e.maxHist {
		h = h[len(h)-e.maxHist:]
	}
	e.sessions[id] = h

	full := e.full[id]
	if full == nil {
		full = e.loadFullLocked(id)
	}
	full = append(full, m)
	e.full[id] = full
	e.persistSession(id, full)
}

// PlainChat streams a conversational reply when no skill is used. It passes the
// recent history so the assistant can hold multi-turn context (greetings,
// small talk, general questions, or follow-ups on a prior skillless turn).
func (e *Engine) PlainChat(ctx context.Context, id, user string, history []Message, onDelta func(string)) (string, error) {
	return e.plainChatWithPlan(ctx, id, user, history, "", onDelta)
}

// PlainChatWithPlan 同 PlainChat，额外把本轮构思要点（见 PlanEssay）并进 system prompt，
// 让这一跳按已经想清楚的要点落笔，而不是从零开始。
func (e *Engine) PlainChatWithPlan(ctx context.Context, id, user string, history []Message, plan string, onDelta func(string)) (string, error) {
	return e.plainChatWithPlan(ctx, id, user, history, plan, onDelta)
}

func (e *Engine) plainChatWithPlan(ctx context.Context, id, user string, history []Message, plan string, onDelta func(string)) (string, error) {
	rosterStr, _, err := e.buildRoster()
	if err != nil {
		return "", err
	}
	sys := `你是 SkillForge 的智能写作助手。你的职责是帮助用户写作。
当用户提出的写作需求与某个技能匹配时，你会自动调用对应技能；当前这条消息你判断不需要技能，所以请你用一般性写作助手的方式自然回应。
可以帮用户：闲聊、回答一般问题、或者在用户还没明确用哪个技能时引导ta描述写作需求（文章主题/篇幅/语气等）。
**引导前先看上下文**：如果【用户提供的要求与素材】或最近对话里已经写了主题/篇幅/语气/格式，那就是用户已经提供过的信息 —— 直接按它动手，不要再问一遍；需要补充时，只问那些**确实还没出现**的点，并且问题要建立在已有素材上（例如「我已按你给的《XX》要求写，其中第 3 条你希望按 A 还是 B 处理？」），而不是抛一份通用清单让用户从头再说一遍。
如果用户问"有哪些技能/能做什么"，请根据下方"技能清单"如实列出当前可用的技能名称和用途；如果技能清单为空，就如实说明当前没有配置技能。
回答保持简洁、专业、口语化，用中文。

技能清单：
` + rosterStr

	histBlock := e.ContextBlock(ctx, id, history)
	combined := user
	if len(history) > 0 {
		combined = "对话历史（供参考，你只需回应最新用户消息）：\n" + histBlock + "\n\n最新用户消息：\n" + user
	}
	if strings.TrimSpace(plan) != "" {
		sys += "\n\n==== 本轮构思要点（按它落笔）====\n" + plan
	}
	// ★ 2026-09-20 改：这条分支原来走 CompleteEx（go-openai SDK）。SDK 那条隧道
	// 有两个洞，而**这是线上最常走的一条路**（用户直接提写作需求、没命中技能时：
	// 线上账本第 1 轮 `[classify] ... skill=-` 就是它）：
	//   ① 不认 SKILLFORGE_THINK_BUDGET —— 上游一慢就无限期干等；
	//   ② 没有 idle 看门狗（StreamChat 有：20s 无字节即断连接、触发空正文重试）。
	//   实测代价：整轮 353.0s、首正文 347.8s、最大静默 297.7s——那 297.7s 里屏幕上
	//   只有计时器在跳，正是用户说的「一直卡着计时」。
	// 换 StreamChat 的等价性：`DisableThinking=false` 走 streamOnce 的 knobNone 分支，
	// 请求体仍是 model/messages/stream:true（只有配了预算才多一个 thinking_budget），
	// 也就是默认配置下这次改造**行为零变化**，纯粹把预算与看门狗这两条路接通。
	st := &hopStat{t0: time.Now()}
	out, err := e.llm.StreamChat(ctx, sys, combined, llm.StreamOpts{
		DisableThinking: false,
		OnContent:       st.content(onDelta),
		OnReasoning:     st.reasoning(reasoningSink(ctx)),
	})
	st.log("write-plain", out, err)
	return out, err
}

// ---------------------------------------------------------------------------
// helpers

// skillTypeLabel renders a human-readable label for a skill's type column.
func skillTypeLabel(t string) string {
	switch t {
	case model.SkillTypeQuery:
		return "办事流程/查询"
	case model.SkillTypeTemplate:
		return "模板下发"
	case model.SkillTypeDocGen:
		return "文档生成"
	default:
		return "文章写作"
	}
}

func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	// strip surrounding code fence
	if strings.HasPrefix(s, "```") {
		lines := strings.Split(s, "\n")
		lines = lines[1:]
		if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), "```") {
			lines = lines[:len(lines)-1]
		}
		s = strings.TrimSpace(strings.Join(lines, "\n"))
	}
	// find first { ... last }
	si := strings.Index(s, "{")
	ei := strings.LastIndex(s, "}")
	if si >= 0 && ei > si {
		return s[si : ei+1]
	}
	return s
}
