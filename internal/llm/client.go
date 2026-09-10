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

// Complete streams a chat completion, writing text deltas to onDelta.
func (c *Client) Complete(ctx context.Context, system, user string, onDelta func(string)) (string, error) {
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
			onDelta(delta)
		}
	}
	return sb.String(), nil
}

// Chat runs a chat completion. Pass jsonMode=true to request the provider emit
// a strict JSON object (response_format), which is required for the intent
// classifier and docgen parser that unmarshal the reply.
func (c *Client) Chat(ctx context.Context, sys, user string, jsonMode ...bool) (string, error) {
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

// Config returns the underlying LLM config (for display).
func (c *Client) Config() *model.LLMConfig { return c.cfg }
