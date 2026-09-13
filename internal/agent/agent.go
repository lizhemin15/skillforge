package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
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
}

// Engine orchestrates the dialogue.
type Engine struct {
	llm      *llm.Client
	store    *store.SkillStore
	mu       sync.Mutex
	sessions map[string][]Message // in-memory trimmed history (maxHist)
	full     map[string][]Message // full history as persisted to disk (untouched by maxHist)
	dataDir  string               // sessions persisted under <dataDir>/sessions/*.json
	maxHist  int                  // max assistant+user turns kept for in-memory context
}

// New builds an engine over the existing store and LLM client.
func New(l *llm.Client, s *store.SkillStore) *Engine {
	e := &Engine{
		llm:      l,
		store:    s,
		sessions: make(map[string][]Message),
		full:     make(map[string][]Message),
		maxHist:  12,
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
		full = full[len(full)-e.maxHist:]
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
- 用户要办理事务/查询流程规则，或闲聊问答 → intent="query"/"chat"，action="answer"。

技能匹配（在意图判断之后进行，作为工具候选清单）：
技能清单里带"专用模板附件"的 template 技能（如 采购合同、采购验收单）用于填充 → 当用户指向该模板并要求填写/起草时，action 必须是 "fill" 并命中该技能。
当匹配到带模板的技能但用户只要空白模板时，action="template_only" 并命中该技能。
当命中 docgen 技能且用户给出内容要生成新文档时，action="gen"。
当命中 write 技能时 action="write"。
只有当没有专门技能匹配时才落到通用能力（action="write"/"answer"）。

优先级：用户要求"填/起草/生成"某个明确表单或文档，且清单里有带专用模板的 template 技能精确匹配（如用户说"采购验收单"）时，必须优先选它且 action="fill"，绝不能退而选通用 docgen 技能。只有当没有专门 template 匹配时，才选通用 docgen/write 技能。

需求要点：
1. 若命中某个技能，返回它的 slug，并在 params 里提取用户已提及的关键参数（键名用技能参数名）。needs 的判定**必须严格**：只把技能参数中标记为 [必填]、且用户这次消息**确实没有提供**的参数列进 needs（name 用技能参数名，label 用中文提示）。[可选] 参数一律不追问——用户没给就自行推断合理占位或省略，直接进入生成。action="fill" 时绝不需要 params/needs（填充阶段会解析字段并让后续环节补全/编造值），needs 留空。docgen 类技能（含 fill）没有任何必填参数：用户给了详情就填进文档，用户只要模板/没给详情就编造合理示例数据填充或下发模板。
2. 用户可能在延续话题（如"再写一遍但改短点""把手机号改成139…"）——结合历史判断是否沿用之前的技能并继续 action（延续填充用 action="fill"）；延续时写 reason 说明。
3. 无论什么情况，都在 steps 里输出 4 阶段拆解执行路径，让用户看到多智能体怎么处理：
   - {phase:"analyze", label:"① 意图分析", detail:"识别出的意图与 action：<intent>/<action>（如 docgen/fill、write/write、query/answer）", status:"done"}
   - {phase:"match",   label:"② 工具匹配", detail:"决定调用工具。<命中技能名>（为什么） 或 未命中技能（write/query 用通用能力，chat 直接回答）", status:"done"}
   - {phase:"params",  label:"③ 参数提取", detail:"<命中时列出已提取/还需追问参数；未命中 write/query 说明将提炼用户内容；chat 省略>", status:"done"}
   - {phase:"generate",label:"④ 执行中",  detail:"<说明将执行的动作：填充模板/生成新文档/下发空白模板/通用写作/直接回答>", status:"active"}
   detail 用一句话，面向用户，别用内部术语。

只输出一个 JSON 对象，不要任何其他文字：
{"intent":"write|docgen|query|chat","action":"fill|template_only|gen|write|answer","skill_slug":"<slug 或空>","needs_tools":true|false,"reason":"<一句话说明这次要不要技能、用什么动作>","params":{...},"needs":[],"steps":[...]}

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
	eval, retry := e.classify(ctx, sys, history, user)
	if retry {
		eval, _ = e.classify(ctx, sys, history, user)
	}
	if eval == nil {
		return &Eval{SkillSlug: "", Reason: "意图识别异常，按普通对话处理"}, nil
	}
	eval.SkillSlug = strings.TrimSpace(eval.SkillSlug)
	return eval, nil
}

// classify runs one classifier completion. Returns (eval, retry) where retry
// is true when the result was unusable (bad JSON or empty intent).
func (e *Engine) classify(ctx context.Context, sys string, history []Message, user string) (*Eval, bool) {
	out, err := e.llm.Chat(ctx, sys, "对话历史（供参考，重点回应最新消息）：\n"+compactHistory(history)+"\n\n用户最新消息：\n"+user, true)
	if err != nil {
		return nil, false
	}
	eval := &Eval{}
	if err := json.Unmarshal([]byte(extractJSON(out)), eval); err != nil {
		fmt.Fprintf(os.Stderr, "[classify-retry] unmarshal err=%v raw_out=%q\n", err, out)
		return nil, true
	}
	if strings.TrimSpace(eval.Intent) == "" && strings.TrimSpace(eval.SkillSlug) == "" {
		// empty intent AND no skill — the model dodged. Only retryable when it
		// truly produced nothing usable.
		fmt.Fprintf(os.Stderr, "[classify-retry] empty intent raw_out=%q\n", out)
		return nil, true
	}
	return eval, false
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
	sys := e.generateSys(sc)
	if strings.TrimSpace(extra) != "" {
		sys += "\n\n" + extra
	}
	var done string
	if sc.SkillType == model.SkillTypeWrite {
		done = "请据此直接写出完整文章。"
	} else {
		done = "请据此给出结构化、可直接照做的办事流程/答案。"
	}
	return e.llm.Complete(ctx, sys, argBlockOf(sc, args)+"\n"+done, onDelta)
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
func (e *Engine) GenerateDoc(ctx context.Context, sc *SkillContent, args map[string]string, userMsg string, history []Message) (*DocResult, error) {
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

	out, err := e.llm.Chat(ctx, sys, argBlock.String(), true)
	if err != nil {
		return nil, err
	}
	doc, err := parseDocJSON(out)
	if err != nil {
		return nil, fmt.Errorf("解析文档规格失败: %w", err)
	}
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
	return &DocResult{
		Filename:    docgen.SafeFilenameAs(doc.Filename, doc.Format),
		Bytes:       data,
		ContentType: docgen.ContentType(doc.Format),
		Summary:     "已为您生成《" + doc.Filename + "》，点击下方文件即可下载。",
		Spec:        string(spec),
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
func (e *Engine) FillDoc(ctx context.Context, sc *SkillContent, userMsg string, history []Message) (*FillResult, error) {
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
	if hist := compactHistory(history); hist != "" && hist != "（无历史）" {
		prompt = "对话历史（供参考，重点回应最新消息）：\n" + hist + "\n\n" + prompt
	}
	out, err := e.llm.Chat(ctx, sys, prompt, true)
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
			return &FillResult{Clarify: &FillClarify{Questions: []string{
				"您想往这张表/文档里填哪些内容？请告诉我具体的关键信息（比如品名、数量、金额、往来单位、日期等），我按您给的填。",
			}}}, nil
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
	rosterStr, _, err := e.buildRoster()
	if err != nil {
		return "", err
	}
	sys := `你是 SkillForge 的智能写作助手。你的职责是帮助用户写作。
当用户提出的写作需求与某个技能匹配时，你会自动调用对应技能；当前这条消息你判断不需要技能，所以请你用一般性写作助手的方式自然回应。
可以帮用户：闲聊、回答一般问题、或者在用户还没明确用哪个技能时引导ta描述写作需求（文章主题/篇幅/语气等）。
如果用户问"有哪些技能/能做什么"，请根据下方"技能清单"如实列出当前可用的技能名称和用途；如果技能清单为空，就如实说明当前没有配置技能。
回答保持简洁、专业、口语化，用中文。

技能清单：
` + rosterStr

	histBlock := compactHistory(history)
	combined := user
	if len(history) > 0 {
		combined = "对话历史（供参考，你只需回应最新用户消息）：\n" + histBlock + "\n\n最新用户消息：\n" + user
	}
	return e.llm.Complete(ctx, sys, combined, onDelta)
}

// ---------------------------------------------------------------------------
// helpers

func compactHistory(history []Message) string {
	if len(history) == 0 {
		return "（无历史）"
	}
	var b strings.Builder
	for _, m := range history {
		b.WriteString(m.Role + ": ")
		c := strings.TrimSpace(m.Content)
		if len(c) > 400 {
			c = c[:400] + "…"
		}
		b.WriteString(c)
		b.WriteString("\n")
	}
	return b.String()
}

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
