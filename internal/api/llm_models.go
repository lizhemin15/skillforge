package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lizhemin15/skillforge/internal/tools"
)

// ListProviderModels 拉取 OpenAI 兼容端点的模型清单，供管理端「自动获取 + 下拉选择」。
//
// 为什么要这个接口：模型名靠人手打，打错一个字符就是 400，而且 provider 上新模型后
// 用户根本不知道该填什么。这里直接向 provider 的模型列表接口问一遍。
//
// 请求体：{base_url, api_key, id}
//   - base_url 必填，写不写 /v1 都行（见 modelEndpointCandidates）
//   - api_key 为空或是掩码（编辑已保存服务时表单里显示的就是掩码）→ 回退用 id 对应配置在库里的 key
//
// 安全：地址由用户任意填写，等同 http_request 工具的暴露面，故复用 tools.GuardedGet（含逐跳重定向复检）。
// 响应里绝不回显 api_key（provider 的错误正文也会过一遍替换）。
func (a *Admin) ListProviderModels(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
		ID      int    `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体无效")
		return
	}
	candidates := modelEndpointCandidates(req.BaseURL)
	if len(candidates) == 0 {
		writeErr(w, http.StatusBadRequest, "请先填写 API Base URL")
		return
	}

	// key 解析顺序：手填的真 key > 库里的 key。掩码视为「没填」。
	key := strings.TrimSpace(req.APIKey)
	if isMaskedKey(key) {
		key = ""
	}
	if key == "" && req.ID > 0 {
		if c, err := a.store.GetLLM(req.ID); err == nil && c != nil {
			key = c.APIKey
		}
	}
	if key == "" {
		writeErr(w, http.StatusBadRequest, "请先填写 API Key（编辑已保存的服务时可留空，会自动复用已存的 key）")
		return
	}

	hdr := map[string]string{"Authorization": "Bearer " + key, "Accept": "application/json"}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	var lastErr string
	for _, ep := range candidates {
		status, body, err := tools.GuardedGet(ctx, ep, hdr, a.toolAllow, 15*time.Second, 1<<20)
		if err != nil {
			// 地址被 SSRF 防护拦下 / 连不上，换个候选也没意义，直接回。
			if len(candidates) == 1 || strings.Contains(err.Error(), "内网") || strings.Contains(err.Error(), "只支持 http") {
				writeErr(w, http.StatusBadGateway, redactSecret(err.Error(), key))
				return
			}
			lastErr = fmt.Sprintf("%s：%s", ep, redactSecret(err.Error(), key))
			continue
		}
		if status != http.StatusOK {
			lastErr = fmt.Sprintf("%s 返回 HTTP %d：%s", ep, status, redactSecret(clipBody(body), key))
			continue
		}
		models := modelNamesFromJSON(body)
		if len(models) == 0 {
			lastErr = fmt.Sprintf("%s 返回 200，但没能从中解析出模型名：%s", ep, clipBody(body))
			continue
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"models":   models,
			"count":    len(models),
			"endpoint": ep,
		})
		return
	}
	if lastErr == "" {
		lastErr = "未能从该地址获取模型列表"
	}
	writeErr(w, http.StatusBadGateway, lastErr)
}

// modelEndpointCandidates 给出可能的模型列表地址。
//
// base_url 写法在现实里不统一：有人填 https://api.deepseek.com，有人填 https://api.siliconflow.cn/v1。
// 两种都试、命中即用，省掉「到底该不该带 /v1」这种毫无意义的踩坑。
func modelEndpointCandidates(base string) []string {
	b := strings.TrimRight(strings.TrimSpace(base), "/")
	if b == "" {
		return nil
	}
	if strings.HasSuffix(b, "/models") { // 用户直接给了完整地址
		return []string{b}
	}
	if strings.HasSuffix(b, "/v1") || strings.HasSuffix(b, "/v1beta") || strings.HasSuffix(b, "/compatible-mode/v1") {
		return []string{b + "/models"}
	}
	return []string{b + "/v1/models", b + "/models"}
}

// isMaskedKey 判断是不是管理端列表里显示的掩码（"sk-a…wxyz" / "••••"）。
// 掩码不是真 key，拿去请求必然 401，必须识别出来回退用库里的值。
func isMaskedKey(k string) bool {
	// 只敲了空格等同于没填：放任它当 key 发出去只会换来 provider 的 401。
	k = strings.TrimSpace(k)
	if k == "" {
		return true
	}
	return strings.Contains(k, "…") || strings.Contains(k, "••••") ||
		strings.Trim(k, "*") == ""
}

// modelNamesFromJSON 从 provider 返回的 JSON 里宽容地抽出模型名。
//
// 各家实现不统一：OpenAI 是 {"object":"list","data":[{"id":"gpt-4"}]}，
// 有的给 {"models":[{"name":"qwen"}]}，也有的就是一个字符串数组。
// 故按「外部输入必须宽容」的原则遍历常见键名，而不是强类型解码 —— 否则换个 provider 就全空。
func modelNamesFromJSON(body []byte) []string {
	var top any
	if err := json.Unmarshal(body, &top); err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}

	var collect func(v any)
	collect = func(v any) {
		switch t := v.(type) {
		case string:
			add(t)
		case []any:
			for _, e := range t {
				collect(e)
			}
		case map[string]any:
			// 先按「容器」处理：只要列表型键的值是数组/对象就往下钻。
			// 必须排在「单对象」之前 —— 否则 {"id":"list-1","data":[...]} 会被误当成一个模型。
			container := false
			for _, k := range []string{"data", "models", "list", "items", "result", "model"} {
				if sub, ok := t[k]; ok {
					switch sub.(type) {
					case []any, map[string]any:
						collect(sub)
						container = true
					}
				}
			}
			if container {
				return
			}
			// 再按「单个模型对象」处理。
			for _, k := range []string{"id", "name", "model"} {
				if s, ok := t[k].(string); ok {
					add(s)
					return
				}
			}
		}
	}
	collect(top)
	return out
}

// clipBody 把 provider 的错误正文压成一行短摘要，便于直接显示给用户判断原因。
func clipBody(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	rs := []rune(s)
	if len(rs) > 300 {
		return string(rs[:300]) + "…"
	}
	if s == "" {
		return "(空响应)"
	}
	return s
}

// redactSecret 把凭据从任何将要回显的文本里抹掉。
// provider 的错误正文偶尔会回显请求头，多这一层不至于把用户的 key 泄到前端。
func redactSecret(s, secret string) string {
	if secret == "" || len(secret) < 8 {
		return s
	}
	return strings.ReplaceAll(s, secret, "***")
}
