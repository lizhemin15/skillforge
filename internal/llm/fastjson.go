package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// FastJSON：一次「必须在 1~2 秒内回来」的 JSON 调用。给首页推荐行（/api/chat/suggest）用。
//
// 为什么不用 go-openai 的 Chat（其余所有调用都在用它）：
// 线上的活跃模型是 Qwen3.6-27B —— **reasoning 模型**。同一句「只输出 JSON」的
// 请求实测（2026-09-13，siliconflow 线上配置）：
//
//	裸调（现状）                          6.24s   completion 304 tokens
//	+ chat_template_kwargs.enable_thinking=false   8.58s   吃满 max_tokens（无效）
//	+ reasoning_effort=none               6.47s   318 tokens（无效）
//	+ 顶层 enable_thinking=false          1.38s   28 tokens  ← 只有这个真管用
//
// 也就是说：思考链不关掉，这一次调用就必然超过推荐行的超时预算，功能在线上
// **等于不存在**（接口 200 + 空数组，前端悄悄退回规则版，页面上什么都看不出来）。
// 而 go-openai 的 ChatCompletionRequest 没有这个**顶层**字段，它的
// ChatTemplateKwargs 走的是 chat_template_kwargs（上面第二行，实测无效）。
// 冻结的 SDK 表达不了 provider 的私有开关，所以这一条自己拼 body。
//
// 与 Chat 刻意保持的差异：
//   - 不流式（推荐行要的是最后一整块 JSON，流式反而要多拼一遍）
//   - 固定 response_format=json_object（源头少一层围栏/客套话）
//   - 带 max_tokens 上限（推荐行不是长文；也是防「思考链吃满预算」的第二道闸）
//   - 单次重试：只有 400（多半是 provider 不认识 enable_thinking 这个字段）才重试，
//     且重试时把该字段摘掉。严格校验未知字段的网关（Azure/部分自建）也能用，
//     代价只是那一家慢一点。
func (c *Client) FastJSON(ctx context.Context, system, user string, maxTokens int) (string, error) {
	if err := c.usable(); err != nil {
		return "", err
	}
	if strings.TrimSpace(c.cfg.APIKey) == "" {
		return "", fmt.Errorf("未配置 LLM API Key")
	}
	if maxTokens <= 0 {
		maxTokens = 600
	}

	content, status, err := c.fastOnce(ctx, system, user, maxTokens, true)
	if err == nil {
		return content, nil
	}
	// 400 = 服务端拒绝了这个请求体（典型原因就是这个非标准字段）。
	// 摘掉它再试一次，而不是把整条推荐行让给「这家 provider 不认识」。
	if status == http.StatusBadRequest {
		if content, _, err2 := c.fastOnce(ctx, system, user, maxTokens, false); err2 == nil {
			return content, nil
		}
	}
	return "", err
}

// fastOnce 发一次请求。withThinkingKnob=false 时不带 enable_thinking（给严格网关的重试路径）。
func (c *Client) fastOnce(ctx context.Context, system, user string, maxTokens int, withThinkingKnob bool) (string, int, error) {
	body := map[string]any{
		"model": c.cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"response_format": map[string]string{"type": "json_object"},
		"max_tokens":      maxTokens,
	}
	if withThinkingKnob {
		// Qwen3 系：false = 不产出思考链。别的 provider 会忽略或 400（400 走上面的重试）。
		body["enable_thinking"] = false
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", 0, err
	}

	endpoint := normalizeBaseURL(c.cfg.BaseURL) + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := fastHTTPClient.Do(req)
	if err != nil {
		// 超时/连不上都打上瞬时标记，与 Chat 的行为保持一致（调用方按同一套判据决定重试）。
		return "", 0, NormalizeErr(err)
	}
	defer resp.Body.Close()
	// 只读 8KB：错误体是给人看的，不需要把整段 HTML 网关页吞进内存。
	tail, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 显式按状态码判瞬时（429/408/5xx），不指望 transientByText 从文案里
		// 正则捞状态码 —— 那条路依赖错误文案的写法，而这里的文案是我们自己拼的。
		err := fmt.Errorf("LLM HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(tail)))
		if isTransientStatus(resp.StatusCode) {
			return "", resp.StatusCode, &TransientError{Err: err}
		}
		return "", resp.StatusCode, err
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(tail, &out); err != nil {
		return "", resp.StatusCode, fmt.Errorf("LLM 响应不是 JSON: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", resp.StatusCode, NormalizeErr(fmt.Errorf("LLM returned no choices"))
	}
	content := out.Choices[0].Message.Content
	if strings.TrimSpace(content) == "" {
		// 空 content 的典型成因：思考链把 completion 预算吃光了（max_tokens 太小）。
		// 这条错误必须说清「空」而不是「格式坏」——两者的修法完全不同。
		return "", resp.StatusCode, fmt.Errorf("LLM 返回空 content（completion_tokens=%d，多半是思考链吃掉了 max_tokens）", out.Usage.CompletionTokens)
	}
	return content, resp.StatusCode, nil
}

// fastHTTPClient：整体超时远大于推荐行的 5s 预算。真正生效的截止时间是 ctx
// （流式那条路要长连接，所以这里单独一个 client，不去动全局默认）。
var fastHTTPClient = &http.Client{Timeout: 60 * time.Second}

// normalizeBaseURL：BaseURL 为空走 OpenAI 默认；填了但没带 /v1 的补上
// （很多 provider 给的是 https://api.deepseek.com 这种）。
func normalizeBaseURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return "https://api.openai.com/v1"
	}
	if !strings.HasSuffix(base, "/v1") && !strings.Contains(base, "/v1/") {
		base += "/v1"
	}
	return base
}
