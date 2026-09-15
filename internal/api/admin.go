package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
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

// ===== LLM config management =====

// ListLLM returns provider configs (API keys masked for display).
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
	writeJSON(w, http.StatusOK, map[string]any{"configs": cfgs})
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
