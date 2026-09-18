package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	openai "github.com/sashabaranov/go-openai"

	"github.com/lizhemin15/skillforge/internal/model"
	"github.com/lizhemin15/skillforge/internal/tlsconf"
)

// Client wraps an OpenAI-compatible client with provider overrides.
type Client struct {
	cfg *model.LLMConfig
	cli *openai.Client
}

// New builds a client from an LLMConfig. DefaultBaseURL behavior: empty →
// OpenAI. If BaseURL is set but lacks /v1, append it (many providers expose
// a base like https://api.deepseek.com/v1).
func New(cfg *model.LLMConfig) *Client {
	conf := openai.DefaultConfig(cfg.APIKey)
	// 归一化逻辑与 FastJSON 共用一处（fastjson.go 的 normalizeBaseURL）：
	// 两条路各写一份的话，某天只改一条，同一个 provider 在两条路上会打到不同地址。
	conf.BaseURL = normalizeBaseURL(cfg.BaseURL)
	// 走本机信任配置（SKILLFORGE_CA_BUNDLE）：内网网关用自签证书时，
	// 不这样接就是把证书放好了也照样「certificate signed by unknown authority」。
	// 这里不设整体超时——流式问答要长连接，截止时间由 ctx 负责。
	conf.HTTPClient = tlsconf.NewClient(0)
	return &Client{cfg: cfg, cli: openai.NewClientWithConfig(conf)}
}

// endpointForErr：报错里要能看出「打到哪个地址」。内网常有多套网关（灰度/生产各一套），
// 只说「证书不可信」客户不知道该修哪一个。
func (c *Client) endpointForErr() string {
	if c == nil || c.cfg == nil {
		return ""
	}
	return normalizeBaseURL(c.cfg.BaseURL)
}

// wrapErr 是 LLM 出错的统一出口（两条路 streamOnce/fastOnce + Chat 都走它）：
// 先把证书类错误翻成人话（含修复与自查命令），再打瞬时故障标记。
// 顺序不能反：NormalizeErr 只会给瞬时错误套一层壳，套完仍保留原文，
// 所以先翻译不会被吞掉；反过来则可能出现「TransientError 包着证书错误」，
// 调用方看到「网络抖动，稍后重试」，而真相是证书没配——重试一万次也没用。
func (c *Client) wrapErr(err error) error {
	if err == nil {
		return nil
	}
	return NormalizeErr(tlsconf.Explain(err, c.endpointForErr()))
}

// ErrNoLLM 是"引擎手里根本没装模型"时的统一错误。
// 背景（线上事故）：启动时 NewHandler 收到的是 nil client，第一条走到模型调用的
// 请求在 (*Client).Chat 上空指针 panic —— net/http 直接掐断连接，前端只显示
// "连接失败：network error"，排查时会误以为是网络/跨域问题。
// 有了这个守卫，同样的配置错误会变成一句人话，而且请求能正常收尾。
var ErrNoLLM = errors.New("未配置 LLM：请在管理端「模型」里选择并启用一个模型")

// usable 挡住 nil 接收者。放在每个公开方法的最前面，比在每个调用点加 if 更不容易漏。
func (c *Client) usable() error {
	if c == nil || c.cfg == nil {
		return ErrNoLLM
	}
	return nil
}

// Complete streams a chat completion, writing text deltas to onDelta.
func (c *Client) Complete(ctx context.Context, system, user string, onDelta func(string)) (string, error) {
	return c.CompleteEx(ctx, system, user, onDelta, nil)
}

// CompleteEx 是 Complete 的超集：除了正文片段，还额外把**思考链片段**交给
// onReasoning（可为 nil）。
//
// 为什么需要它：长文执笔保留思考链（质量更好），但思考链期间正文一个字都没有，
// 用户看到的就是「一直卡着计时」。reasoning_content 是 provider 在同一个流里推的
// 非标准字段，只有接出来当「中间材料」显示，等待才是可见的——不额外多花一分钱、
// 也不改模型的输出。onReasoning 拿到的文本**不进正文**，只用于过程展示。
func (c *Client) CompleteEx(ctx context.Context, system, user string, onDelta func(string), onReasoning func(string)) (string, error) {
	if err := c.usable(); err != nil {
		return "", err
	}
	if strings.TrimSpace(c.cfg.APIKey) == "" {
		return "", errors.New("未配置 LLM API Key（请在管理端配置）")
	}
	messages := make([]openai.ChatCompletionMessage, 0, 2)
	if strings.TrimSpace(system) != "" {
		messages = append(messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: system})
	}
	messages = append(messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: user})
	req := openai.ChatCompletionRequest{
		Model:    c.cfg.Model,
		Messages: messages,
		Stream:   true,
	}
	stream, err := c.cli.CreateChatCompletionStream(ctx, req)
	if err != nil {
		return "", c.wrapErr(err)
	}
	defer stream.Close()

	var sb strings.Builder
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return sb.String(), c.wrapErr(err)
		}
		if len(resp.Choices) == 0 {
			continue
		}
		delta := resp.Choices[0].Delta.Content
		// 思考链片段先交出去：它不属于正文，只是「还在干活」的证据。
		if r := resp.Choices[0].Delta.ReasoningContent; r != "" && onReasoning != nil {
			onReasoning(r)
		}
		if delta != "" {
			sb.WriteString(delta)
			// onDelta 允许为 nil：调用方只关心最终文本（例如批量生成、审稿走的是
			// 非流式 Chat）时不必为了凑一个空回调写闭包。对 nil 直接调用会整进程崩，
			// 而这是调用方完全合理的用法。
			if onDelta != nil {
				onDelta(delta)
			}
		}
	}
	return sb.String(), nil
}

// Chat runs a chat completion. Pass jsonMode=true to request the provider emit
// a strict JSON object (response_format), which is required for the intent
// classifier and docgen parser that unmarshal the reply.
func (c *Client) Chat(ctx context.Context, sys, user string, jsonMode ...bool) (string, error) {
	if err := c.usable(); err != nil {
		return "", err
	}
	if strings.TrimSpace(c.cfg.APIKey) == "" {
		return "", errors.New("未配置 LLM API Key")
	}
	req := openai.ChatCompletionRequest{
		Model: c.cfg.Model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: sys},
			{Role: openai.ChatMessageRoleUser, Content: user},
		},
	}
	if len(jsonMode) > 0 && jsonMode[0] {
		req.ResponseFormat = &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONObject,
		}
	}
	resp, err := c.cli.CreateChatCompletion(ctx, req)
	if err != nil {
		// 归一化：瞬时故障（429/5xx/网关抖动/超时）打上标记，让调用方能决定重试；
		// 文案不变，只是多带一个可判定的身份。
		return "", c.wrapErr(err)
	}
	if len(resp.Choices) == 0 {
		// 空 choices 在实践中同样出现在上游过载时，按瞬时处理。
		return "", &TransientError{Err: fmt.Errorf("LLM returned no choices")}
	}
	content := resp.Choices[0].Message.Content
	if strings.TrimSpace(content) == "" {
		// 「200 但没有正文」必须是一个能判定的身份（ErrEmptyContent），不能是空串：
		// 调用方把它当正常结果往下走，就会在 json.Unmarshal 那里炸成
		// 「unexpected end of json input 原文=<<>>」，把真因（模型没干活）说成了格式问题。
		// reasoning_tokens 一起报出来：它 ≈ completion_tokens 就是「思考链吃光预算」的指纹。
		return "", fmt.Errorf("%w（非流式：completion_tokens=%d，reasoning_tokens=%d，多半是思考链吃掉了 max_tokens）",
			ErrEmptyContent, resp.Usage.CompletionTokens, reasoningTokensOf(resp.Usage))
	}
	return content, nil
}

// reasoningTokensOf：provider 不一定报 completion_tokens_details，指针要做空值保护。
func reasoningTokensOf(u openai.Usage) int {
	if u.CompletionTokensDetails == nil {
		return 0
	}
	return u.CompletionTokensDetails.ReasoningTokens
}

// Config returns the underlying LLM config (for display). nil 接收者返回 nil，
// 免得展示路径（管理端读当前模型）也跟着 panic。
func (c *Client) Config() *model.LLMConfig {
	if c == nil {
		return nil
	}
	return c.cfg
}
