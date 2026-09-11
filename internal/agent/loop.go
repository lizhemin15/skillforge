package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/tools"
)

// Chatter 是循环对 LLM 的最小依赖。抽成接口是为了让循环能脱离真实 provider 做单测
// （用假客户端按剧本返回工具调用序列），否则核心逻辑没法在没有网络/额度的情况下验证。
type Chatter interface {
	ChatTools(ctx context.Context, msgs []llm.Msg, defs []llm.ToolDef) (llm.Msg, error)
}

// LoopEvent 是循环对外播报的进度事件（前端据此显性展示「调用了什么工具」）。
type LoopEvent struct {
	Round  int    `json:"round"`
	Tool   string `json:"tool"`
	Args   string `json:"args,omitempty"`
	Status string `json:"status"` // running | done | error
	Note   string `json:"note,omitempty"`
}

// LoopOutcome 是一次循环的最终结果。
type LoopOutcome struct {
	Text   string
	Files  []tools.File
	Rounds int
	Calls  []string
}

// Loop 是 Agent 工具循环：模型 → 调工具 → 看结果 → 再决定，直到不再需要工具。
//
// 这是「工具一组合威力无穷」的来源：单次调用只能算一个按钮，循环才是会用工具的人。
type Loop struct {
	LLM      Chatter
	Registry *tools.Registry
	MaxRound int // 工具调用轮数上限，防止死循环烧 token
	MaxFiles int
}

// NewLoop 构造循环。
func NewLoop(c Chatter, r *tools.Registry, maxRound int) *Loop {
	if maxRound <= 0 {
		maxRound = 6
	}
	return &Loop{LLM: c, Registry: r, MaxRound: maxRound, MaxFiles: 8}
}

// Run 执行循环。history 是历史对话（可选），user 是本轮输入。
// onEvent 可为 nil；它会在每次工具调用前后被同步调用，用于 SSE 播报。
func (l *Loop) Run(ctx context.Context, system string, history []llm.Msg, user string, onEvent func(LoopEvent)) (LoopOutcome, error) {
	var out LoopOutcome
	if l.LLM == nil {
		return out, fmt.Errorf("未配置模型")
	}
	msgs := make([]llm.Msg, 0, len(history)+2)
	if strings.TrimSpace(system) != "" {
		msgs = append(msgs, llm.Msg{Role: "system", Content: system})
	}
	msgs = append(msgs, history...)
	msgs = append(msgs, llm.Msg{Role: "user", Content: user})

	var defs []llm.ToolDef
	if l.Registry != nil {
		for _, t := range l.Registry.All() {
			defs = append(defs, llm.ToolDef{Name: t.Name(), Description: t.Description(), Parameters: t.Schema()})
		}
	}

	repeats := map[string]int{}
	for round := 1; round <= l.MaxRound; round++ {
		out.Rounds = round
		resp, err := l.LLM.ChatTools(ctx, msgs, defs)
		if err != nil {
			return out, err
		}
		if len(resp.ToolCalls) == 0 {
			out.Text = strings.TrimSpace(resp.Content)
			return out, nil
		}
		msgs = append(msgs, resp)

		for _, call := range resp.ToolCalls {
			out.Calls = append(out.Calls, call.Name)
			sig := call.Name + "|" + call.Args
			repeats[sig]++

			if onEvent != nil {
				onEvent(LoopEvent{Round: round, Tool: call.Name, Args: summarizeArgs(call.Args), Status: "running"})
			}

			// 同一调用重复三次 = 模型卡住了（常见于工具一直报错却不改参数），
			// 直接把情况告诉它并终止，而不是陪着它烧 token。
			if repeats[sig] > 3 {
				msg := "检测到重复调用同一工具且参数未变，已停止。" +
					"请基于已有信息直接给出最终回答，或换成别的工具/参数。"
				msgs = append(msgs, llm.Msg{Role: "tool", ToolCallID: call.ID, Content: msg})
				if onEvent != nil {
					onEvent(LoopEvent{Round: round, Tool: call.Name, Status: "error", Note: "重复调用，已打断"})
				}
				out.Text = "（工具重复调用已打断）" + l.wrapUp(ctx, msgs)
				return out, nil
			}

			resText, files, note, err := l.execOne(ctx, call)
			if err != nil {
				note = err.Error()
				resText = "工具执行失败：" + err.Error()
			}
			for _, f := range files {
				if len(out.Files) < l.MaxFiles {
					out.Files = append(out.Files, f)
				}
			}
			msgs = append(msgs, llm.Msg{Role: "tool", ToolCallID: call.ID, Content: resText})
			if onEvent != nil {
				st := "done"
				if err != nil {
					st = "error"
				}
				onEvent(LoopEvent{Round: round, Tool: call.Name, Status: st, Note: note})
			}
		}
	}

	// 到达轮数上限：不再给工具，逼模型收口。
	out.Text = l.wrapUp(ctx, msgs)
	return out, nil
}

// execOne 解析参数并执行一次工具调用。
func (l *Loop) execOne(ctx context.Context, call llm.ToolCall) (string, []tools.File, string, error) {
	if l.Registry == nil {
		return "", nil, "", fmt.Errorf("工具未启用")
	}
	args := map[string]any{}
	raw := strings.TrimSpace(call.Args)
	if raw != "" && raw != "null" {
		// UseNumber：数字保留 JSON 原文，不要落成 float64。
		// 落成 float64 后 fmt 出来是 "1e+06" 这种科学计数法，
		// 填进财务模板就是实打实的错值（"1000000" → "1e+06"）。
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&args); err != nil {
			// 参数不是合法 JSON：把原因回给模型让它自我修正，不中断循环
			return fmt.Sprintf("参数解析失败（必须返回合法 JSON 对象）：%v。请修正后重新调用。", err), nil, "参数非法", nil
		}
	}
	res, err := l.Registry.Execute(ctx, call.Name, args)
	if err != nil {
		return "工具执行失败：" + err.Error(), nil, "执行失败", err
	}
	note := res.Display
	if note == "" {
		note = "完成"
	}
	return res.Content, res.Files, note, nil
}

// wrapUp 让模型基于已有信息给出最终回答（不再提供工具）。
func (l *Loop) wrapUp(ctx context.Context, msgs []llm.Msg) string {
	final := append([]llm.Msg{}, msgs...)
	final = append(final, llm.Msg{
		Role:    "user",
		Content: "请停止调用工具，基于以上已有信息直接给出面向用户的最终回答（中文，简洁，不要输出工具调用语法）。",
	})
	resp, err := l.LLM.ChatTools(ctx, final, nil)
	if err != nil {
		return "工具调用已达上限，未能生成最终回答：" + err.Error()
	}
	return strings.TrimSpace(resp.Content)
}

// summarizeArgs 把参数压成一行短摘要用于 trace 展示（太长会撑爆前端）。
func summarizeArgs(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || s == "{}" || s == "null" {
		return ""
	}
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > 120 {
		r := []rune(s)
		return string(r[:120]) + "…"
	}
	return s
}
