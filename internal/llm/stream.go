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
	// OnNote 收「调用方该知道的旁白」（重试、降级、放大预算…）。可为 nil。
	//
	// 单独一个回调而不是混进 OnReasoning：思考链是模型说的话，旁白是流水线自己
	// 说的话，前端按类别分开显示（skillgen 的 MaterialThink / MaterialNote）。
	// 混在一起的话，用户会把「系统正在重试」误读成模型想到了重试这件事。
	OnNote func(string)
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

	// 与 FastJSON 同构的三段重试：① 网关 400（严格校验未知字段）→ 摘掉非标准的
	// enable_thinking，留下真正管用的 reasoning_effort；② 网关 400 且开了 JSONMode
	// → 摘掉 response_format（部分 provider 的 json_object 与推理模型不兼容）；
	// ③ 200 但正文空（哨兵 ErrEmptyContent）→ 放大输出预算重试。
	//
	// 第 ③ 段是这次线上事故的正面回击：step2 元数据调用拿到空 content，
	// 调用方把空串喂给 json.Unmarshal，用户看到
	// 「模型输出不是合法json unexpected end of json input 原文=<<>>」。
	// fastjson 那条路早就有这段（errEmptyContent + 放大预算），流式这条没有——
	// 同一件事两套标准，于是修在缺的那一边。
	knob := knobBoth
	if !o.DisableThinking {
		knob = knobNone
	}
	opt := o
	triedBigger, droppedJSON := false, false
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		content, status, err := c.streamOnce(ctx, sys, user, opt, knob)
		if err == nil {
			return content, nil
		}
		lastErr = err
		switch {
		case status == http.StatusBadRequest && opt.JSONMode && !droppedJSON:
			// 有的网关/模型组合不吃 response_format=json_object，直接 400。
			// 提示词里本来就写着「只输出JSON」，且调用方用 extractJSON 兜底，
			// 所以摘掉它重试是安全的；不摘就只能把 400 原样丢给用户。
			droppedJSON = true
		case status == http.StatusBadRequest && knob == knobBoth:
			// 网关不认 enable_thinking（严格校验未知字段的 Azure / 部分自建）。
			knob = knobEffortOnly
		case IsEmptyContent(err) && !triedBigger && ctx.Err() == nil:
			// 200 但一个字正文都没有 ⇒ 思考链把 completion 预算吃光了。
			// 放大预算再问一次：慢，但比「整个阶段失败」强。
			// 上限见 maxEmptyRetryTokens 的注释。
			triedBigger = true
			mt := opt.MaxTokens
			if mt <= 0 {
				// 没设过上限时 provider 用的是它自己的默认值（可能很小）。
				// 显式给一个下限，否则「放大」这件事无从谈起。
				mt = emptyRetryBaseTokens
			}
			if mt *= 4; mt > maxEmptyRetryTokens {
				mt = maxEmptyRetryTokens
			}
			opt.MaxTokens = mt
			if opt.OnNote != nil {
				// 用户这一轮白等了几十秒，得当场知道为什么，以及系统正在做什么。
				// 不说的话，材料流会莫名其妙地从头再念一遍思考链。
				opt.OnNote(fmt.Sprintf("这一轮模型只回了思考过程、没有正文，正在放大输出预算到 max_tokens=%d 重试一次…", mt))
			}
		default:
			return content, err
		}
	}
	return "", lastErr
}

const (
	// emptyRetryBaseTokens：调用方没指定 max_tokens 时的放大起点（×4 = 4096）。
	emptyRetryBaseTokens = 1024
	// maxEmptyRetryTokens 是空正文重试的预算上限。比 fastjson 的 4096 宽：
	// 这条路上跑的是长文执笔与结构化产出，4096 可能把稿子截断；而且只有
	// 「第一轮一个字都没吐出来」才会走到这里，不存在常态多花钱的问题。
	maxEmptyRetryTokens = 8192
)

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
	// 空正文诊断用：思考链片数与 provider 报的 token 明细。片数 > 0 而正文 0 字，
	// 就是「关了思考链的开关被无视、预算全花在想」的指纹。
	reasonChunks, completionTokens, reasoningTokens := 0, 0, 0
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
			Usage *struct {
				CompletionTokens        int `json:"completion_tokens"`
				CompletionTokensDetails *struct {
					ReasoningTokens int `json:"reasoning_tokens"`
				} `json:"completion_tokens_details"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// 单帧坏掉不该让整轮作废：provider 的 usage 尾帧/空心跳帧都不是标准 delta。
			continue
		}
		if u := chunk.Usage; u != nil {
			completionTokens = u.CompletionTokens
			if u.CompletionTokensDetails != nil {
				reasoningTokens = u.CompletionTokensDetails.ReasoningTokens
			}
		}
		for _, ch := range chunk.Choices {
			if r := ch.Delta.ReasoningContent; r != "" {
				reasonChunks++
				if o.OnReasoning != nil {
					o.OnReasoning(r)
				}
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
	content := sb.String()
	if strings.TrimSpace(content) == "" {
		// 流跑完、HTTP 200，但一个字的正文都没有 —— 这不是「格式坏」，是「模型没干活」。
		// 与 fastjson.fastOnce 同一判据、同一个哨兵，上层才有一致的修法（放大预算重试）；
		// 空串直通调用方的话，json.Unmarshal 会报出那句正确的废话
		// 「unexpected end of json input 原文=<<>>」。
		return "", resp.StatusCode, fmt.Errorf("%w（流式：%s）", ErrEmptyContent, emptyStreamDetail(reasonChunks, completionTokens, reasoningTokens, o))
	}
	return content, resp.StatusCode, nil
}

// emptyStreamDetail 把人话拼给用户/运维：片数与 token 明细决定了修法完全不同。
func emptyStreamDetail(reasonChunks, completionTokens, reasoningTokens int, o StreamOpts) string {
	if reasonChunks > 0 {
		s := fmt.Sprintf("流完整跑完但正文 0 字，只收到 %d 片思考链；completion_tokens=%d、reasoning_tokens=%d",
			reasonChunks, completionTokens, reasoningTokens)
		if o.MaxTokens > 0 {
			s += fmt.Sprintf("（本次 max_tokens=%d，思考链多半把它吃光了）", o.MaxTokens)
		} else {
			s += "（本次没指定 max_tokens，用的是 provider 默认上限，思考链多半把它吃光了）"
		}
		return s
	}
	return "流完整跑完但正文 0 字，连思考链都没有（provider 返回了空响应）"
}

// streamHTTPClient：整体超时远大于普通请求。关思考链之后这些调用都在十几秒内，
// 但 provider 偶发抖动时不该被 60s 卡死；真正生效的截止时间是 ctx。
//
// 写成函数而不是包级变量：包级变量在 main() 之前求值，那时实例 env
// （SKILLFORGE_CA_BUNDLE）还没读进来，会造出一个「没有 CA」的 transport 并被缓存
// ——症状是「证书放好了也不生效」，客户会转头怀疑证书本身。
// 放在请求路径上求值才安全；transport 在 tlsconf 内部已缓存，这里只包一层结构体。
func streamHTTPClient() *http.Client { return tlsconf.NewClient(10 * time.Minute) }
