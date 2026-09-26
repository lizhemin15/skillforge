package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lizhemin15/skillforge/internal/agent"
	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/model"
	"github.com/lizhemin15/skillforge/internal/skillgen"
	"github.com/lizhemin15/skillforge/internal/store"
	"github.com/lizhemin15/skillforge/internal/tools"
)

func atoi(s string) (int, error) { return strconv.Atoi(s) }

// trainMaxDuration 是训练流程的绝对时长上限。
//
// 训练要跑「生成 → 本地校验 → 最多 3 轮裁判试用与回炉」，慢的时候十几分钟，
// 而它是挂在 SSE 请求上的：浏览器一关、刷新、或代理掐掉连接，r.Context()
// 立刻被 cancel，cancel 会一路传到裁判层，fidelity.md 里留下
// 「回炉失败: context canceled」，随后一份没过线的技能照样落盘——用户看到的
// 就是「生成的技能和我给的素材完全没关系」而系统毫无提示。
//
// 所以训练 ctx 必须从请求生命周期里解绑（context.WithoutCancel：保留请求内的
// value，只丢掉 cancel），再用这个绝对上限兜底，避免真卡死的运行永远占着
// 训练锁（Admin.mu 是全局串行的）。
const trainMaxDuration = 30 * time.Minute

// trainingCtx 把训练从 HTTP 请求生命周期里解绑，只留绝对时长上限。
// 单独抽成函数是为了让「解绑」这件事能被单测直接盯住：请求 ctx 一 cancel，
// 返回的 ctx 必须还活着（改成 context.WithTimeout(reqCtx, …) 会立刻变红）。
func trainingCtx(reqCtx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(reqCtx), trainMaxDuration)
}

// logTrainOutcome 把训练结论落到 stderr（journalctl）。
//
// 为什么非落日志不可：训练是挂在 SSE 上的十几分钟长任务，用户十有八九不等在页面上
// （关页、刷新、断网）。以前失败只往 SSE 里写一行 error，连接一断这行错误就掉进虚空，
// 运维在 journalctl 里只能看到「什么都没发生」——一个没过线的技能为什么没落盘、
// 卡在哪一步，全靠猜。抽成函数是为了让「结论必须落 stderr」这条能被单测直接盯住。
func logTrainOutcome(name, slug string, el time.Duration, res *skillgen.Result, err error) {
	switch {
	case err != nil:
		log.Printf("[train] 技能 %q（slug=%s）失败，用时 %s：%v", name, slug, el.Round(time.Second), err)
	case res == nil:
		log.Printf("[train] 技能 %q（slug=%s）结束但结果为空，用时 %s", name, slug, el.Round(time.Second))
	default:
		log.Printf("[train] 技能 %q（slug=%s）完成，用时 %s，prompt %d 字，示例 %d 个，降级交付=%v（%s）",
			name, slug, el.Round(time.Second), res.PromptLen, res.ExampleN, res.Degraded, res.DegradeReason)
	}
}

// Admin serves authenticated endpoints: LLM config + skill training.
type Admin struct {
	store  *store.SkillStore
	gen    *skillgen.Generator
	eng    *agent.Engine // chat engine; kept in sync with the live LLM
	mu     sync.Mutex    // serialize training to one run (simple)
	ocrURL string        // scanned-PDF OCR microservice base URL (empty = disabled)
	// ocrTimeout 是上传解析的客户端超时；<=0 表示用 skillgen.DefaultOCRTimeout。
	ocrTimeout time.Duration
	// toolAllow 是拉取模型清单时的内网白名单，与 http_request 工具同源同策略
	// （同一个 SKILLFORGE_TOOL_HTTP_ALLOW）。两处各写一套规则迟早会不一致。
	toolAllow []string

	// mcp 是 MCP 服务管理器（管理员后台统一配置/开关的外部工具来源）。
	mcp *tools.MCPManager
	// mcpRefreshMu 保证同一时刻只有一次重连在跑：连点保存不该叠出多次握手。
	// 与 mcp 内部的锁分工不同——那把锁保护状态，这把锁保护「别重复干活」。
	mcpRefreshMu sync.Mutex
}

func NewAdmin(s *store.SkillStore, g *skillgen.Generator) *Admin {
	return &Admin{
		store:     s,
		gen:       g,
		ocrURL:    "http://127.0.0.1:8093",
		toolAllow: splitList(os.Getenv("SKILLFORGE_TOOL_HTTP_ALLOW")),
	}
}

// SetEngine links the chat engine so LLM hot-swaps also reach it.
func (a *Admin) SetEngine(e *agent.Engine) { a.eng = e }

// SetOCR 注入文档解析服务地址（空串 = 禁用）。地址来源见 ocrServiceURL()。
func (a *Admin) SetOCR(url string) { a.ocrURL = url }

// SetOCRTimeout 注入上传解析的客户端超时（<=0 = 用 skillgen.DefaultOCRTimeout）。
func (a *Admin) SetOCRTimeout(d time.Duration) { a.ocrTimeout = d }

// SetMCP 注入 MCP 服务管理器（nil = 关闭 MCP 管理能力）。
func (a *Admin) SetMCP(m *tools.MCPManager) { a.mcp = m }

// ocrTimeoutOrDefault 返回生效的上传解析超时；与训练通道共用同一个默认值，
// 免得两条通道各写一个数字、改一条忘一条。
func (a *Admin) ocrTimeoutOrDefault() time.Duration {
	if a.ocrTimeout > 0 {
		return a.ocrTimeout
	}
	return skillgen.DefaultOCRTimeout
}

// maxDocBytes 是单个参考文档的体积上限。
//
// 取 64MB 的依据：50 页 300dpi 的扫描件约 35MB，而这里原来写的是 2MB——
// 上传大 PDF 时字节被静默截断，OCR 只认出前几页却当成整本手册用（比直接报错更危险）。
const maxDocBytes = 64 << 20

// maxFormBytes 是整个 multipart 表单的内存阈值（超出部分落临时文件，不是硬上限）。
const maxFormBytes = 128 << 20

// ===== Skill generation (训练 skill 造新技能) =====

// Train accepts a multipart form: name, category, description, requirement,
// and optional files[]; runs the generator and streams progress via SSE.
func (a *Admin) Train(w http.ResponseWriter, r *http.Request) {
	if !a.mu.TryLock() {
		writeErr(w, http.StatusConflict, "已有训练任务在运行，请稍候")
		return
	}
	defer a.mu.Unlock()

	if err := r.ParseMultipartForm(maxFormBytes); err != nil {
		writeErr(w, http.StatusBadRequest, "表单过大或无效")
		return
	}
	name := r.FormValue("name")
	cat := r.FormValue("category")
	desc := r.FormValue("description")
	req := r.FormValue("requirement")
	if name == "" || req == "" {
		writeErr(w, http.StatusBadRequest, "缺少 name 或 requirement")
		return
	}

	// Build a fresh LLM client from active config (hot-swap).
	lcfg, err := a.store.GetActiveLLM()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "未配置 LLM 服务（请在管理端配置）")
		return
	}
	a.gen.SetLLM(llm.New(lcfg))
	// 注入文档解析服务（ocrd）：创建技能时上传的 PDF/docx 靠它文本化。
	// 这条通道以前没接线——上传的扫描件字节被当文本直接喂给 LLM，必然乱码。
	a.gen.SetOCR(a.ocrURL)
	if a.eng != nil {
		a.eng.SetLLM(llm.New(lcfg))
	}

	in := &skillgen.Input{
		Name: name, Category: cat, Description: desc, Requirement: req,
	}
	// collect uploaded reference files
	for _, h := range r.MultipartForm.File["files"] {
		f, err := h.Open()
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(f, maxDocBytes))
		f.Close()
		if len(b) > 0 {
			in.Files = append(in.Files, &skillgen.UploadedFile{Filename: h.Filename, Content: string(b)})
		}
	}

	in.Slug = skillgen.Slugify(name)
	if _, err := a.store.Get(in.Slug); err == nil {
		writeErr(w, http.StatusConflict, fmt.Sprintf("技能 '%s' 已存在，请换一个名称", in.Slug))
		return
	}

	// SSE progress
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "流式输出不可用")
		return
	}
	// 训练全程用这个脱离请求的 ctx：客户端断连不该让 3 轮回炉预算凭空消失。
	tctx, cancelTrain := trainingCtx(r.Context())
	defer cancelTrain()
	// send 会被「训练主流程」和「OCR 心跳 goroutine」同时调用，必须串行化：
	// 两处并发写同一个 ResponseWriter 会交错出坏帧（更别说 data race）。
	var sendMu sync.Mutex
	send := func(t, data string) {
		b, _ := json.Marshal(map[string]string{"type": t, "data": data})
		sendMu.Lock()
		defer sendMu.Unlock()
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	// 中间材料：把模型流式吐出的思考链/正文片段攒批后转成 delta 帧。
	// 训练一跑二十分钟，流水线只在阶段边界发一条进度，阶段内部是几分钟级的静默
	// 模型调用——屏幕上只有一个计时器在动，用户分不清「在慢慢想」和「卡死了」。
	// 攒批参数（400ms / 240 字节）与前端「就地更新一个实况块」配套：不攒批的话
	// 一帧一片，每秒几百帧会把浏览器拖垮。
	relay := skillgen.NewMaterialRelay(400*time.Millisecond, 240, func(kind, text string) {
		b, _ := json.Marshal(map[string]string{"kind": kind, "text": text})
		send("delta", string(b))
	})
	tctx = skillgen.WithDelta(tctx, relay.Push)

	send("status", fmt.Sprintf("开始训练技能：%s", name))
	trainStart := time.Now()
	res, err := a.gen.Generate(tctx, in, func(step string) {
		// 阶段边界先把积压材料吐干净：不然尾巴会串到下一个阶段的材料里。
		relay.Flush()
		send("step", step)
	})
	relay.Flush()
	// 结论同时落 SSE 与 stderr：SSE 给当下还盯着的浏览器，stderr 给「人已经走了」的场景。
	// 结论同时落 SSE 与 stderr：SSE 给当下还盯着的浏览器，stderr 给「人已经走了」的场景。
	logTrainOutcome(name, in.Slug, time.Since(trainStart), res, err)
	if err != nil {
		send("error", err.Error())
		return
	}
	b, _ := json.Marshal(res)
	send("done", string(b))
}

// readUpload 读一个上传件（带上限，防止大文件把内存吃光）。
func readUpload(h *multipart.FileHeader) ([]byte, bool) {
	f, err := h.Open()
	if err != nil {
		return nil, false
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, maxDocBytes))
	return b, len(b) > 0
}

// TrainLite 是「极简创建」通道：用户只给一份写作指南 + 若干篇范文。
//
// multipart 字段（前四项都是普通表单值，后两项是文件）：
// name/slug（可空，空则从材料推断）、guide（粘贴的指南正文）、
// examples（粘贴的范文，多篇用一行 --- 分隔）、guide_doc（指南文件，单份）、
// example_files（范文文件，多份，前端同名多选）。
// 范文的文本与文件用**不同字段名**：同名虽然 multipart 能分开存，但下一个看代码的
// 人一定会以为 examples 是文件列表，改错一处就是「范文读不出来」这种哑故障。
//
// 为什么单开一个端点而不是给 Train 加个 mode 参数：
//  1. 必填规则正好相反——Train 缺 name/requirement 直接 400，而极简通道这两项都由
//     材料推断（用户手上只有指南和范文，逼他先想技能名是本末倒置）。塞进同一个
//     handler，两套必填规则会在同一个 if 里互相打架。
//  2. 失败语义不同——极简通道允许「AI 精炼失败但技能已经可用」，那是 done 帧不是
//     error 帧；混在一起最容易在某个分支上把「可用」报成「失败」。
func (a *Admin) TrainLite(w http.ResponseWriter, r *http.Request) {
	if !a.mu.TryLock() {
		writeErr(w, http.StatusConflict, "已有训练任务在运行，请稍候")
		return
	}
	defer a.mu.Unlock()

	if err := r.ParseMultipartForm(maxFormBytes); err != nil {
		writeErr(w, http.StatusBadRequest, "表单过大或无效")
		return
	}
	// 指南只取第一份：两份指南的硬约束常常互相矛盾（一份说 800 字、一份说 1500 字），
	// 静默挑一份合并比直接报错危险得多。
	guideFileName := ""
	var files []*skillgen.UploadedFile
	for _, h := range r.MultipartForm.File["guide_doc"] {
		b, ok := readUpload(h)
		if !ok {
			continue
		}
		guideFileName = h.Filename
		files = append(files, &skillgen.UploadedFile{Filename: h.Filename, Content: string(b)})
		break
	}
	for _, h := range r.MultipartForm.File["example_files"] {
		b, ok := readUpload(h)
		if !ok {
			continue
		}
		files = append(files, &skillgen.UploadedFile{Filename: h.Filename, Content: string(b)})
	}

	// slug 只在用户真填了的时候才预处理：对空串调 Slugify 会命中它内部的时间戳兜底
	// （`skill-1790401421` 这种），于是 GenerateLite 以为「用户指定了 slug」而放弃
	// 从指南标题推断名字——技能名和目录名就双双退化成时间戳。别在传参层替它做决定。
	slugRaw := strings.TrimSpace(r.FormValue("slug"))
	in := &skillgen.LiteInput{
		Name:      strings.TrimSpace(r.FormValue("name")),
		Guide:     strings.TrimSpace(r.FormValue("guide")),
		Examples:  skillgen.SplitLiteExamples(r.FormValue("examples")),
		Files:     files,
		GuideFile: guideFileName,
	}
	if slugRaw != "" {
		in.Slug = skillgen.Slugify(slugRaw)
	}
	// 预检只看「材料给没给」，不看「够不够长」：长度是素材门禁的判据，它要在 SSE 里
	// 报出来（用户需要看到是「指南太短」还是「范文读不出来」），而不是一个 400 就没了。
	exFileN := len(files)
	if guideFileName != "" {
		exFileN--
	}
	if in.Guide == "" && guideFileName == "" {
		writeErr(w, http.StatusBadRequest, "缺写作指南：请粘贴指南正文，或上传指南文件")
		return
	}
	if len(in.Examples) == 0 && exFileN <= 0 {
		writeErr(w, http.StatusBadRequest, "缺范文：请粘贴至少一篇范文（多篇用一行 --- 分隔），或上传范文文件")
		return
	}

	// LLM 配不上**不算失败**：极简通道的阶段 A 是纯本地装盘，没有模型也能生成一份
	// 按素材直装的可用技能（内网常见的「模型还没接好」场景不该被卡在门口）。
	// 只是要在流里说清楚，别让用户以为 AI 已经精炼过。
	noLLM := false
	if lcfg, err := a.store.GetActiveLLM(); err != nil {
		noLLM = true
	} else {
		a.gen.SetLLM(llm.New(lcfg))
		a.gen.SetOCR(a.ocrURL)
		if a.eng != nil {
			a.eng.SetLLM(llm.New(lcfg))
		}
	}

	// SSE progress
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "流式输出不可用")
		return
	}
	tctx, cancelTrain := trainingCtx(r.Context())
	defer cancelTrain()
	var sendMu sync.Mutex
	send := func(t, data string) {
		b, _ := json.Marshal(map[string]string{"type": t, "data": data})
		sendMu.Lock()
		defer sendMu.Unlock()
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	relay := skillgen.NewMaterialRelay(400*time.Millisecond, 240, func(kind, text string) {
		b, _ := json.Marshal(map[string]string{"kind": kind, "text": text})
		send("delta", string(b))
	})
	tctx = skillgen.WithDelta(tctx, relay.Push)

	send("status", "极简创建：读素材 → 按素材装盘（这一步不花模型时间）→ AI 精炼一次")
	if noLLM {
		send("step", "⚠️ 当前没有可用的 LLM 配置：将只按素材装盘生成可用技能，跳过 AI 精炼")
	}
	trainStart := time.Now()
	res, err := a.gen.GenerateLite(tctx, in, func(step string) {
		relay.Flush()
		send("step", step)
	})
	relay.Flush()
	// 失败时 res 可能是 nil，日志里的 slug 退回推断值：训练日志是事后排查的唯一线索，
	// 不能在「已经失败」的路径上再因为取字段而 panic。
	slugForLog := in.Slug
	if res != nil && res.Slug != "" {
		slugForLog = res.Slug
	}
	logTrainOutcome(in.Name, slugForLog, time.Since(trainStart), res, err)
	if err != nil {
		send("error", err.Error())
		return
	}
	b, _ := json.Marshal(res)
	send("done", string(b))
}

// ===== LLM config management =====
func (a *Admin) ListLLM(w http.ResponseWriter, r *http.Request) {
	cfgs, err := a.store.ListLLMConfigs()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// mask keys
	for i := range cfgs {
		if cfgs[i].APIKey != "" {
			k := cfgs[i].APIKey
			if len(k) > 8 {
				cfgs[i].APIKey = k[:4] + "…" + k[len(k)-4:]
			} else {
				cfgs[i].APIKey = "••••"
			}
		}
	}
	// runtime_id 是「进程真正在用的那条库记录」。界面把「已启用」和「运行中」
	// 都摆出来：这次线上事故的表象就是两者不一致，而界面只显示了前者，
	// 用户只能看到「明明切了却没生效」。
	//
	// 不能拿模型名去判断是否一致：内网多套网关常挂同名模型（线上就是
	// 讯飞maas 与 siliconflow 都叫 Qwen3.6-35B-A3B），名字相同、地址与 key 都不同，
	// 靠名字对照会得出「一致」这个错误结论。所以只在服务端按 id 判。
	runtimeID := 0
	if a.eng != nil {
		runtimeID = a.eng.LLMConfigID()
	}
	writeJSON(w, http.StatusOK, map[string]any{"configs": cfgs, "runtime_id": runtimeID})
}

// applyActiveLLM 把库里「当前启用」的配置推给生成器与引擎，让改动当场生效。
//
// 必须在任何改动 llm_config 启停状态的接口之后调用。旧实现只改库：
// 进程里常驻的客户端还是启动时那一条，于是用户切了模型、界面显示「在用」，
// 每一轮问答却仍在打旧地址（线上事故：切到讯飞后持续收到旧服务商的 402）。
// 引擎侧的 ensureLLM 会在下一轮问答自愈，这里做的是「点完立刻就是新的」——
// 用户点完那一下没变，就会认定这系统不支持热切换。
func (a *Admin) applyActiveLLM() {
	lcfg, err := a.store.GetActiveLLM()
	if err != nil || lcfg == nil {
		return // 一条都没配：保持现状，界面自己会提示「还没有配置 LLM 服务」
	}
	if a.gen != nil {
		a.gen.SetLLM(llm.New(lcfg))
	}
	if a.eng != nil {
		_ = a.eng.ReloadLLM()
	}
}

// UpsertLLM creates/updates a provider config.
func (a *Admin) UpsertLLM(w http.ResponseWriter, r *http.Request) {
	var c model.LLMConfig
	if err := readBody(r, &c); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体无效")
		return
	}
	if c.Provider == "" || c.Model == "" {
		writeErr(w, http.StatusBadRequest, "provider / model 不能为空")
		return
	}
	// 编辑已保存的服务时，前端不回传 key（密钥框里是掩码，重传等于把掩码存进库）。
	// 这个兜底必须排在 key 校验之前 —— 否则「只改个模型名再保存」这个最普通的
	// 操作会被 400 拒掉，用户看到的是「不能为空」，实际字段就摆在眼前。
	// 同一条记录也沿用 is_active：表单里没有「停用」这个语义，
	// 让一次保存把正在使用的服务悄悄停掉，整个 LLM 会直接不可用。
	if c.ID > 0 {
		if old, err := a.store.GetLLM(c.ID); err == nil {
			if isMaskedKey(c.APIKey) {
				c.APIKey = old.APIKey
			}
			if c.BaseURL == "" {
				c.BaseURL = old.BaseURL
			}
			if !c.IsActive {
				c.IsActive = old.IsActive
			}
		}
	}
	if c.APIKey == "" {
		writeErr(w, http.StatusBadRequest, "请填写 API Key（编辑已保存的服务时留空即沿用原 key）")
		return
	}
	id, err := a.store.UpsertLLMConfig(&c)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if c.IsActive {
		if err := a.store.SetActiveLLM(int(id)); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	// 保存后必须刷新运行期客户端，且不能只在 c.IsActive 时刷：
	// 「编辑已保存的服务」这条路径前端恒发 is_active:false（沿用库里的启用状态），
	// 改的恰恰可能就是当前在用的那条的模型名/key —— 那是最需要立刻生效的改动。
	a.applyActiveLLM()
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "ok": true})
}

// SetActiveLLM flags one config active.
func (a *Admin) SetActiveLLM(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID int `json:"id"`
	}
	if err := readBody(r, &body); err != nil || body.ID <= 0 {
		writeErr(w, http.StatusBadRequest, "需要有效的 id")
		return
	}
	if err := a.store.SetActiveLLM(body.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 「切换」按钮的语义就是立刻生效。这里不刷，用户切完发现还是旧模型在答，
	// 只会得到一个「热切换是坏的」结论。
	a.applyActiveLLM()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// DeleteLLM removes a provider config.
func (a *Admin) DeleteLLM(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := atoi(idStr)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "无效 id")
		return
	}
	if err := a.store.DeleteLLMConfig(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 删掉的如果正是在用的那条，运行期必须跟着退到剩下的那条去：
	// 否则手里的客户端指向一条已经不存在的配置，界面「运行中」那一行
	// 会指向幽灵记录，排查时越看越糊涂。
	a.applyActiveLLM()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ToggleSkill enables/disables a skill.
func (a *Admin) ToggleSkill(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Slug    string `json:"slug"`
		Enabled bool   `json:"enabled"`
	}
	if err := readBody(r, &body); err != nil || body.Slug == "" {
		writeErr(w, http.StatusBadRequest, "需要 slug")
		return
	}
	if err := a.store.SetEnabled(body.Slug, body.Enabled); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// DeleteSkill removes a skill and its content.
func (a *Admin) DeleteSkill(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "skillforge-core" {
		writeErr(w, http.StatusBadRequest, "核心技能不可删除")
		return
	}
	if err := a.store.Delete(slug); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// Article returns a stored article by id.
func (a *Admin) GetArticle(w http.ResponseWriter, r *http.Request) {
	idStr := r.FormValue("id")
	id, err := atoi(idStr)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "无效 id")
		return
	}
	msg, err := a.store.GetArticle(int64(id))
	if err != nil {
		writeErr(w, http.StatusNotFound, "文章不存在或已删除")
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

// ===== Skill optimize (review) + version snapshot / rollback =====

// ReviewSkill performs an incremental optimization of an existing skill's
// system_prompt, anchored on its immutable style_profile. It snapshots the
// current prompt+template to versions/v{N+1}, runs the incremental rewrite,
// lands the new prompt, and bumps the skill version. Old versions stay on
// disk so a regression can be rolled back (达尔文棘轮).
func (a *Admin) ReviewSkill(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" {
		writeErr(w, http.StatusBadRequest, "需要 slug")
		return
	}
	sk, err := a.store.Get(slug)
	if err != nil {
		writeErr(w, http.StatusNotFound, "技能不存在: "+slug)
		return
	}
	var body struct {
		Instruction string `json:"instruction"`
		Note        string `json:"note"`
	}
	if err := readBody(r, &body); err != nil || strings.TrimSpace(body.Instruction) == "" {
		writeErr(w, http.StatusBadRequest, "需要优化指令 instruction")
		return
	}

	lcfg, err := a.store.GetActiveLLM()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "未配置 LLM 服务（请在管理端配置）")
		return
	}
	a.gen.SetLLM(llm.New(lcfg))
	if a.eng != nil {
		a.eng.SetLLM(llm.New(lcfg))
	}

	// read current prompt + template + style anchor + examples
	cur := ""
	if f, e := a.store.ReadFile(slug, "system_prompt.md"); e == nil {
		cur = f.Content
	}
	tpl := ""
	if f, e := a.store.ReadFile(slug, "template.md"); e == nil {
		tpl = f.Content
	}
	style, _ := a.store.ReadStyleProfile(slug)
	examples, _ := a.store.ReadExamples(slug)

	// snapshot current state for rollback safety -> next version
	next, err := a.store.SnapshotVersion(slug, body.Note)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "无法创建版本快照: "+err.Error())
		return
	}

	// incremental rewrite (style-anchored, never a full rewrite)
	newPrompt, err := a.gen.Review(r.Context(), slug, body.Instruction, cur, style, tpl, examples)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "优化失败: "+err.Error())
		return
	}
	if err := a.store.WriteFile(slug, "system_prompt.md", newPrompt); err != nil {
		writeErr(w, http.StatusInternalServerError, "落盘失败: "+err.Error())
		return
	}
	if err := a.store.SetVersion(slug, next); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "version": next, "old_version": sk.Version,
		"prompt": newPrompt,
	})
}

// ListSkillVersions returns the snapshot versions of a skill (for rollback UI).
func (a *Admin) ListSkillVersions(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	vs, err := a.store.ListVersions(slug)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if vs == nil {
		vs = []store.SkillVersion{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": vs})
}

// RollbackSkill restores a snapshot version's prompt+template and sets the
// skill version back (undo a bad optimization).
func (a *Admin) RollbackSkill(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	var body struct {
		Version int `json:"version"`
	}
	if err := readBody(r, &body); err != nil || body.Version <= 0 {
		writeErr(w, http.StatusBadRequest, "需要有效的 version")
		return
	}
	if _, err := a.store.Get(slug); err != nil {
		writeErr(w, http.StatusNotFound, "技能不存在: "+slug)
		return
	}
	ver, err := a.store.RollbackVersion(slug, body.Version)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": ver})
}
