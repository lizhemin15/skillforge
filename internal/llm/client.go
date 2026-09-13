package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	openai "github.com/sashabaranov/go-openai"

	"github.com/lizhemin15/skillforge/internal/model"
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
	if cfg.BaseURL != "" {
		base := strings.TrimRight(cfg.BaseURL, "/")
		if !strings.HasSuffix(base, "/v1") && !strings.Contains(base, "/v1/") {
			base += "/v1"
		}
		conf.BaseURL = base
	}
	return &Client{cfg: cfg, cli: openai.NewClientWithConfig(conf)}
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
		return "", err
	}
	defer stream.Close()

	var sb strings.Builder
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return sb.String(), err
		}
		if len(resp.Choices) == 0 {
			continue
		}
		delta := resp.Choices[0].Delta.Content
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
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("LLM returned no choices")
	}
	return resp.Choices[0].Message.Content, nil
}

// Config returns the underlying LLM config (for display). nil 接收者返回 nil，
// 免得展示路径（管理端读当前模型）也跟着 panic。
func (c *Client) Config() *model.LLMConfig {
	if c == nil {
		return nil
	}
	return c.cfg
}
