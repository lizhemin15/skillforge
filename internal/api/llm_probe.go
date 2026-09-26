package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/model"
)

// probeTimeout：一次最大 24 token 的非流式调用等 30 秒。
// 这不是「怕慢」，而是用来区分故障类型的：内网网关即使慢，小请求也就几秒；
// 30 秒都回不来基本就是地址不通 / 网关挂了，早点把这条结论给用户，
// 比让他盯着圈等 3 分钟再看到超时有用。真正的回答质量上限由对话那条路的预算管。
const probeTimeout = 30 * time.Second

// ProbeLLM 测一条 LLM 配置能不能真打通，供管理端「测试」按钮。
//
// 请求体：{id, base_url, model, api_key}
//   - base_url / model / api_key 给了就用给的 —— 这样**改完可以先测再保存**，
//     不必为了试一个模型名先把坏配置存进库里（存了等于把线上对话改坏一次）。
//   - 缺的项（含 key 是掩码时，编辑已保存服务时表单里显示的就是掩码）回退用 id 那条的。
//
// 回执：HTTP 200 + body 里的 ok 字段表示「测试跑完了，结论是通/不通」。
// 上游失败不用 5xx 表达 —— 这里被测端点的失败恰恰是**预期结果**之一，
// 用 5xx 会让「测出来不通」和「测试接口自己坏了」在前端长得一样。
func (a *Admin) ProbeLLM(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID      int    `json:"id"`
		BaseURL string `json:"base_url"`
		Model   string `json:"model"`
		APIKey  string `json:"api_key"`
	}
	if err := readBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体无效")
		return
	}

	cfg := &model.LLMConfig{BaseURL: req.BaseURL, Model: req.Model, APIKey: req.APIKey}
	if req.ID > 0 && a.store != nil {
		if stored, err := a.store.GetLLM(req.ID); err == nil && stored != nil {
			if strings.TrimSpace(cfg.BaseURL) == "" {
				cfg.BaseURL = stored.BaseURL
			}
			if strings.TrimSpace(cfg.Model) == "" {
				cfg.Model = stored.Model
			}
			// 掩码不是真 key，拿去请求必然 401，必须识别出来回退用库里的值。
			if isMaskedKey(cfg.APIKey) {
				cfg.APIKey = stored.APIKey
			}
		}
	}

	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		writeErr(w, http.StatusBadRequest, "请先填写 API Base URL（或选一条已保存的服务）")
		return
	}
	// 少写 http:// 时 go-openai 会拿 "api.deepseek.com/v1/chat/completions" 去 Parse，
	// 报出来的是一句与用户操作无关的 url 解析错。这里先挡下来说人话。
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		writeErr(w, http.StatusBadRequest, "API Base URL 要以 http:// 或 https:// 开头（当前："+base+"）")
		return
	}
	if strings.TrimSpace(cfg.Model) == "" {
		writeErr(w, http.StatusBadRequest, "请先填写模型名（可点「获取模型」拉取清单后选择）")
		return
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		writeErr(w, http.StatusBadRequest, "请先填写 API Key（编辑已保存的服务时可留空，会自动复用已存的 key）")
		return
	}

	// 这里**故意不做 SSRF 内网拦截**（与 /llms/models 的 tools.GuardedGet 不同）：
	// 运行期的对话路径本来就不拦（内网部署的网关地址就在内网，拦了等于禁用），
	// 测试按钮的职责是如实报告「这条配置拿去做对话会怎样」。
	// 在这里拦一下，会把「能正常聊天的地址」判成不通 —— 那就成了说谎的按钮。
	// 暴露面与对话路径完全一致，认证面与其它 /api/admin/* 一致。
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	res := llm.New(cfg).Probe(ctx)
	// 回执里绝不含 key：地址与模型名必须回显（用户就是靠它核对），
	// 但正文里可能夹带（个别网关会把请求头抄进错误消息）。
	res.Message = redactSecret(res.Message, cfg.APIKey)
	res.Reply = redactSecret(res.Reply, cfg.APIKey)

	writeJSON(w, http.StatusOK, res)
}
