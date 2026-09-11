package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

// ToolCall 是模型请求执行的一次工具调用。
// Args 是模型生成的原始 JSON 字符串——**不保证合法**，执行前必须校验，
// 校验失败要把原因回给模型让它自我修正，而不是直接中断整个循环。
type ToolCall struct {
	ID   string
	Name string
	Args string
}

// Msg 是工具循环里的一条消息。用自家结构而不是直接暴露 openai 的类型，
// 是为了让 agent 循环可以被单测替换（假客户端），也让换 provider 时改动收敛在本文件。
type Msg struct {
	Role       string // system | user | assistant | tool
	Content    string
	ToolCalls  []ToolCall // 仅 assistant：本轮的调用请求
	ToolCallID string     // 仅 tool：回应对哪一次调用
}

// ToolDef 描述一个可被模型调用的工具。
type ToolDef struct {
	Name        string
	Description string
	Parameters  map[string]any // JSON Schema；required 必须是数组，写错字符串会被 provider 直接 400
}

// ChatTools 发起一次带工具定义的对话，返回完整的 assistant 消息（可能含 ToolCalls）。
//
// 用非流式：工具调用要求参数是完整 JSON，流式分片拼装容易拿到半截 JSON，
// 而循环里真正要给用户看的是「调用了什么、结果如何」，不是逐字吐字。
func (c *Client) ChatTools(ctx context.Context, msgs []Msg, defs []ToolDef) (Msg, error) {
	if strings.TrimSpace(c.cfg.APIKey) == "" {
		return Msg{}, errors.New("未配置 LLM API Key")
	}
	if len(msgs) == 0 {
		return Msg{}, errors.New("空消息列表")
	}
	req := openai.ChatCompletionRequest{
		Model:    c.cfg.Model,
		Messages: toOpenAIMessages(msgs),
	}
	if len(defs) > 0 {
		ts := make([]openai.Tool, 0, len(defs))
		for _, d := range defs {
			ts = append(ts, openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        d.Name,
					Description: d.Description,
					Parameters:  d.Parameters,
				},
			})
		}
		req.Tools = ts
	}
	resp, err := c.cli.CreateChatCompletion(ctx, req)
	if err != nil {
		return Msg{}, err
	}
	if len(resp.Choices) == 0 {
		return Msg{}, errors.New("LLM returned no choices")
	}
	m := resp.Choices[0].Message
	out := Msg{Role: "assistant", Content: m.Content}
	for _, tc := range m.ToolCalls {
		if tc.Function.Name == "" {
			continue
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:   tc.ID,
			Name: tc.Function.Name,
			Args: tc.Function.Arguments,
		})
	}
	return out, nil
}

// toOpenAIMessages 把通用消息翻译成 openai 结构。
func toOpenAIMessages(msgs []Msg) []openai.ChatCompletionMessage {
	out := make([]openai.ChatCompletionMessage, 0, len(msgs))
	for _, m := range msgs {
		om := openai.ChatCompletionMessage{Role: m.Role, Content: m.Content}
		if m.Role == "tool" {
			// tool 结果必须挂 tool_call_id，否则 provider 报「缺少对应的调用」
			om.ToolCallID = m.ToolCallID
		}
		for _, tc := range m.ToolCalls {
			om.ToolCalls = append(om.ToolCalls, openai.ToolCall{
				ID:   tc.ID,
				Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{
					Name:      tc.Name,
					Arguments: tc.Args,
				},
			})
		}
		out = append(out, om)
	}
	return out
}

// DescribeToolCalls 把一轮调用压成一行摘要，用于日志与 trace。
func DescribeToolCalls(cs []ToolCall) string {
	if len(cs) == 0 {
		return ""
	}
	names := make([]string, 0, len(cs))
	for _, c := range cs {
		names = append(names, c.Name)
	}
	return fmt.Sprintf("%d 个工具: %s", len(cs), strings.Join(names, ", "))
}
