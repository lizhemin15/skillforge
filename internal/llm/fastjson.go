package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// FastJSON：一次「必须在 1~2 秒内回来」的 JSON 调用。给首页推荐行（/api/chat/suggest）用。
//
// 为什么不用 go-openai 的 Chat（其余所有调用都在用它）：
// 线上活跃模型是 **reasoning 模型**，思考链不关掉，这一次调用就必然超过推荐行的
// 超时预算，功能在线上 **等于不存在**（接口 200 + 空数组，前端悄悄退回规则版，
// 页面上什么都看不出来）。而 go-openai 的 ChatCompletionRequest 表达不了
// provider 的私有开关，所以这一条自己拼 body。
//
// 关思考链的开关有**两族，而且互不通用** —— 这是踩过的坑，2026-09-14 实测：
//
//	provider / 模型                         enable_thinking=false   reasoning_effort=none
//	siliconflow / Qwen3.6-27B（旧线上）      1.38s 有效 ✅            6.47s 无效 ❌
//	astron / astron-code-latest（现线上）    **被接受但彻底忽略** ❌    0.94s 有效 ✅
//	                                        HTTP 200，思考链照产，
//	                                        reasoning_tokens 吃满 max_tokens
//
// 第二行是本文件最贵的一课：astron 对 enable_thinking **不报 400**，只是默默无视。
// 于是「摘掉字段重试」那条路永远不会触发，而 content 回来是空的（预算全被
// 思考链吃掉），接口照旧 200 + 空数组 —— 全链路没有任何一处报错。
// 所以这里**两个开关都带**（谁也不指望对方管用），并且额外加一层「空 content
// 就放大预算再问一次」的兜底，专门给「两个开关都被忽略」的 provider。
//
// 与 Chat 刻意保持的差异：
//   - 不流式（推荐行要的是最后一整块 JSON，流式反而要多拼一遍）
//   - 固定 response_format=json_object（源头少一层围栏/客套话）
//   - 带 max_tokens 上限（推荐行不是长文；也是防「思考链吃满预算」的第二道闸）
//   - 最多三次请求：① 两个开关都带 ② 400 → 摘掉非标准的 enable_thinking 重试
//     ③ 空 content → 预算×4 再问一次。严格校验未知字段的网关（Azure/部分自建）
//     也能用，代价只是那一家慢一点。
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

	// 最多三次请求。用循环而不是三段 if：这两条重试路径会**叠加**
	// （网关先 400 摘字段，再发现 200 但 content 空），写成平铺的三段 if
	// 时第二条永远进不去 —— 因为第一条的 err 已经被覆盖了。
	// 迭代 1：两个开关都带。迭代 2/3 取决于上一步错在哪。
	knob := knobBoth
	mt := maxTokens
	triedBigger := false
	var lastErr error
	for i := 0; i < 3; i++ {
		content, status, err := c.fastOnce(ctx, system, user, mt, knob)
		if err == nil {
			return content, nil
		}
		lastErr = err
		switch {
		case status == http.StatusBadRequest && knob == knobBoth:
			// 网关不认 enable_thinking（严格校验未知字段的 Azure / 部分自建）。
			// 摘掉它重试 —— 注意 reasoning_effort 要留着，它才是真正管用的那个。
			knob = knobEffortOnly
		case errors.Is(err, errEmptyContent) && !triedBigger && ctx.Err() == nil && mt < 4096:
			// 200 但 content 空 ⇒ 思考链把 completion 预算吃光了（provider 把两个
			// 开关都忽略了）。放大预算再问一次：慢，但比「推荐行永远只有规则版」强。
			// 上限 4096，避免在这种 provider 上烧掉一笔意外的账。knob 保持不动 ——
			// 上面刚因为 400 摘掉 enable_thinking 的话，这里加回去等于白跑一趟。
			triedBigger = true
			if mt *= 4; mt > 4096 {
				mt = 4096
			}
		default:
			return "", lastErr
		}
	}
	return "", lastErr
}

// thinkKnob 选这次请求带哪些「关思考链」的开关。
type thinkKnob int

const (
	// knobBoth：两族开关都带。默认值 —— 两族互不通用，谁也不知道对面是哪一家。
	knobBoth thinkKnob = iota
	// knobEffortOnly：只带 reasoning_effort（400 重试用：对面不认 enable_thinking）。
	knobEffortOnly
)

// errEmptyContent 是「200 但 content 空」的哨兵错误。调用方要能把它和
// 「超时」「连不上」「格式坏」区分开：前者的修法是放大预算，后者不是。
var errEmptyContent = errors.New("LLM 返回空 content")

// fastOnce 发一次请求。knob 决定带哪些关思考链的开关。
func (c *Client) fastOnce(ctx context.Context, system, user string, maxTokens int, knob thinkKnob) (string, int, error) {
	body := map[string]any{
		"model": c.cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"response_format": map[string]string{"type": "json_object"},
		"max_tokens":      maxTokens,
	}
	// reasoning_effort=none：OpenAI 官方字段，astron 认。旧 provider 会忽略它。
	body["reasoning_effort"] = "none"
	if knob == knobBoth {
		// enable_thinking=false：Qwen3 系认。astron **不报错也不管用**（见文件头）。
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
			CompletionTokens  int `json:"completion_tokens"`
			CompletionDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
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
		// 这条错误必须说清「空」而不是「格式坏」——两者的修法完全不同，而且
		// 调用方（FastJSON 的第三段）要靠 errEmptyContent 这个哨兵来决定放大预算重试。
		// reasoning_tokens 一起报出来：它 ≈ completion_tokens 就是「开关被无视、
		// 模型把预算全花在想」的指纹，运维一眼能定位到是 provider 侧的问题。
		return "", resp.StatusCode, fmt.Errorf("%w（completion_tokens=%d，reasoning_tokens=%d，多半是思考链吃掉了 max_tokens）",
			errEmptyContent, out.Usage.CompletionTokens, out.Usage.CompletionDetails.ReasoningTokens)
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
