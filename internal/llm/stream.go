package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lizhemin15/skillforge/internal/tlsconf"
)

// StreamOpts 控制一次流式调用的可选行为。
//
// 存在的理由是线上这两件事同时成立：
//
//  1. 路由/抽取类调用（意图分类、分类路由、文档要素抽取）**不需要思考链**，
//     但线上活跃模型是 reasoning 模型，思考链不关就必然几十秒（实测
//     siliconflow/Qwen3.6-27B：带思考 63.0s / 关思考 4.8s，同一段提示词）。
//     go-openai 的 ChatCompletionRequest 表达不了 provider 私有开关
//     （enable_thinking / reasoning_effort），所以这条路必须自己拼 body。
//  2. 长文执笔**保留思考链质量更好**，但思考链期间正文一个字都没有，用户看到的
//     就是「一直卡着计时」。所以思考链片段要能**流出来**当中间材料给用户看。
type StreamOpts struct {
	// DisableThinking 请求 provider 关掉思考链。两族开关都带（互不通用，见 fastjson.go）：
	// enable_thinking 给 Qwen 系，reasoning_effort 给 astron 系。
	DisableThinking bool
	// JSONMode 要求 provider 直接吐 JSON 对象（结构化消费时必须开）。
	JSONMode bool
	// MaxTokens >0 时带上上限。
	MaxTokens int
	// OnReasoning 收思考链片段（中间材料）。可为 nil。
	OnReasoning func(string)
	// OnContent 收正文片段。可为 nil。
	OnContent func(string)
}

// StreamChat 走流式 chat/completions，把思考链与正文片段分别交给回调，返回正文全文。
//
// 与 Complete 的分工：Complete 用 go-openai 且不带任何 provider 私有开关（正文执笔
// 用它，行为与历史逐字一致）；StreamChat 自己拼 body，因此能带关思考链的开关，
// 也知道怎么把 reasoning_content 与 content 分开。
func (c *Client) StreamChat(ctx context.Context, sys, user string, o StreamOpts) (string, error) {
	if err := c.usable(); err != nil {
		return "", err
	}
	if strings.TrimSpace(c.cfg.APIKey) == "" {
		return "", errors.New("未配置 LLM API Key（请在管理端配置）")
	}

	// 与 FastJSON 同构的两段重试：① 两个开关都带 ② 网关 400（严格校验未知字段）
	// → 摘掉非标准的 enable_thinking，留下真正管用的 reasoning_effort。
	// 关思考链是「快 10 倍」这件事的全部来源，能不能摘、摘到什么程度，只能靠这一次重试问出来。
	knob := knobBoth
	if !o.DisableThinking {
		knob = knobNone
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		content, status, err := c.streamOnce(ctx, sys, user, o, knob)
		if err == nil {
			return content, nil
		}
		lastErr = err
		if status == http.StatusBadRequest && knob == knobBoth {
			knob = knobEffortOnly
			continue
		}
		return content, err
	}
	return "", lastErr
}

// knobNone 表示不带任何关思考链的开关（正文执笔走这条）。
const knobNone thinkKnob = -1

// streamOnce 发一次流式请求并解析到最后一片。
func (c *Client) streamOnce(ctx context.Context, system, user string, o StreamOpts, knob thinkKnob) (string, int, error) {
	messages := make([]map[string]string, 0, 2)
	if strings.TrimSpace(system) != "" {
		messages = append(messages, map[string]string{"role": "system", "content": system})
	}
	messages = append(messages, map[string]string{"role": "user", "content": user})
	body := map[string]any{
		"model":    c.cfg.Model,
		"messages": messages,
		"stream":   true,
	}
	if o.JSONMode {
		body["response_format"] = map[string]string{"type": "json_object"}
	}
	if o.MaxTokens > 0 {
		body["max_tokens"] = o.MaxTokens
	}
	switch knob {
	case knobBoth:
		// 两族开关互不通用，谁也不指望对方管用：enable_thinking 给 Qwen 系，
		// reasoning_effort 给 astron 系（详见 fastjson.go 文件头那张实测表）。
		body["enable_thinking"] = false
		body["reasoning_effort"] = "none"
	case knobEffortOnly:
		body["reasoning_effort"] = "none"
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
	req.Header.Set("Accept", "text/event-stream")

	resp, err := streamHTTPClient().Do(req)
	if err != nil {
		return "", 0, c.wrapErr(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 只读 8KB：错误体是给人看的，不必把整段网关 HTML 吞进内存。
		tail, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		e := fmt.Errorf("LLM HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(tail)))
		if isTransientStatus(resp.StatusCode) {
			return "", resp.StatusCode, &TransientError{Err: e}
		}
		return "", resp.StatusCode, e
	}

	var sb strings.Builder
	sc := bufio.NewScanner(resp.Body)
	// SSE 的 data 行里带整段 delta JSON；默认 64KB 上限对长思考片段偏紧，放宽到 1MB。
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// 单帧坏掉不该让整轮作废：provider 的 usage 尾帧/空心跳帧都不是标准 delta。
			continue
		}
		for _, ch := range chunk.Choices {
			if r := ch.Delta.ReasoningContent; r != "" && o.OnReasoning != nil {
				o.OnReasoning(r)
			}
			if d := ch.Delta.Content; d != "" {
				sb.WriteString(d)
				if o.OnContent != nil {
					o.OnContent(d)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		// 有内容就别把整轮判死：网络在末尾抖一下，正文往往是完整的。
		if sb.Len() > 0 {
			return sb.String(), resp.StatusCode, nil
		}
		return "", resp.StatusCode, c.wrapErr(err)
	}
	return sb.String(), resp.StatusCode, nil
}

// streamHTTPClient：整体超时远大于普通请求。关思考链之后这些调用都在十几秒内，
// 但 provider 偶发抖动时不该被 60s 卡死；真正生效的截止时间是 ctx。
//
// 写成函数而不是包级变量：包级变量在 main() 之前求值，那时实例 env
// （SKILLFORGE_CA_BUNDLE）还没读进来，会造出一个「没有 CA」的 transport 并被缓存
// ——症状是「证书放好了也不生效」，客户会转头怀疑证书本身。
// 放在请求路径上求值才安全；transport 在 tlsconf 内部已缓存，这里只包一层结构体。
func streamHTTPClient() *http.Client { return tlsconf.NewClient(10 * time.Minute) }
