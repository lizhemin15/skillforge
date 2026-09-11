package api

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lizhemin15/skillforge/internal/agent"
	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/store"
	"github.com/lizhemin15/skillforge/internal/tools"
)

// buildToolRegistry 按环境变量装配工具集。返回 nil 表示工具能力关闭。
//
//	SKILLFORGE_TOOLS=off            关掉整个工具循环（回退到纯文本对话）
//	SKILLFORGE_EXEC=off             只关掉代码执行（沙箱），保留 http_request
//	SKILLFORGE_DOCS=off             只关掉文档工具（模板填充 / 生成文档）
//	SKILLFORGE_TOOL_HTTP_ALLOW=...  内网白名单（逗号分隔主机名或 CIDR）
//	SKILLFORGE_TOOL_TIMEOUT=20s     单次工具超时
//
// 默认：工具开、代码执行开（沙箱兜底，用户明确要求公开可用）。
func buildToolRegistry(s *store.SkillStore) *tools.Registry {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("SKILLFORGE_TOOLS")), "off") {
		return nil
	}
	reg := tools.NewRegistry()

	httpCfg := tools.DefaultHTTPConfig()
	httpCfg.AllowHosts = splitList(os.Getenv("SKILLFORGE_TOOL_HTTP_ALLOW"))
	reg.Register(tools.NewHTTPRequestTool(httpCfg))

	if !strings.EqualFold(strings.TrimSpace(os.Getenv("SKILLFORGE_EXEC")), "off") {
		reg.Register(tools.NewRunPythonTool(tools.DefaultExecConfig()))
	}

	// 文档工具：模板发现/填充 + 从零生成。语义映射（哪个值填哪个字段）由
	// 循环里的模型负责，工具只做机械落盘——工具里再调一次 LLM 就成了双重
	// 调用，既慢又贵，而且模型看不到中间态没法纠错。
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("SKILLFORGE_DOCS")), "off") {
		src := newStoreTemplates(s)
		reg.Register(tools.NewListTemplatesTool(src))
		reg.Register(tools.NewFillTemplateTool(src))
		reg.Register(tools.NewGenDocumentTool())
	}
	return reg
}

// splitList 解析逗号分隔的环境变量。
func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// agentSystemPrompt 是工具循环的系统提示。
//
// 关键约束来自实战教训：
//   - 明确「联网取数」与「本地计算」分工，否则模型会写 requests 代码（沙箱内必失败）
//   - 禁止输出工具调用语法（前端不渲染）+ 禁止编造工具未返回的数据
func agentSystemPrompt() string {
	return `你是 SkillForge 的办公智能助手，可以用工具替用户把事情办完。

工具使用原则：
- 需要外部数据、调用接口/API → 用 http_request 取数；一次可以同时请求多个地址。
- 需要计算、统计、清洗数据、生成 CSV/文本 → 用 run_python。
  该沙箱**没有网络**，不要在里面写 requests/urllib 联网代码，取数一律交给 http_request。
  先把 http_request 返回的数据整理成代码里的字面量，再算。
- 要填用户的模板（合同 / 验收单 / 报价单这类现成表格）→ 必须先用 list_templates
  查出模板和字段名，再用 fill_template 填。**绝不要凭想象编字段名**：字段名错了
  填出来就是一片空白，而且不会报错。
- 用户要一份新文档、且没有现成模板 → 用 gen_document 生成 Word / Excel / PPT / PDF。
- 能在本地算出来的结论，不要靠猜；工具拿到的数据优先于你的记忆。
- 用户只是闲聊或问常识时，直接回答，不要调用工具。

办公文档要点：
- 数字要写成纯数字（"12000"），不要带千分位、货币符号或单位，否则 Excel 里
  算不出合计；单位放在相邻的「备注」或列标题里。
- 日期统一写成 2026年8月26日 或 2026-08-26。

输出要求：
- 全部用中文回答，简洁、结论先行。
- 表格用 Markdown 表格；不要输出工具调用语法（如 <tool_call>），也不要描述你正在调用工具的过程。
- 绝不要编造工具没有返回的数据；拿不到就如实说明。
- 如果产出了文件，告诉用户文件名和用途。

今天是 ` + time.Now().Format("2006年1月2日") + `。`
}

// runAgentLoop 执行「工具循环」。返回 true 表示本轮已由循环处理完毕。
//
// 只在「没命中技能 + 不是闲聊」时启用：命中技能走原有快路径（模板填充/docgen），
// 闲聊走原有的流式纯文本，谁都不受改造影响。
func (h *chatHandler) runAgentLoop(
	ctx context.Context,
	write func(ev, data string),
	req chatReq,
	eval agent.Eval,
	history []agent.Message,
) bool {
	if h.tools == nil || len(h.tools.All()) == 0 {
		return false
	}
	if strings.TrimSpace(req.Message) == "" {
		return false
	}

	steps := make([]agent.TraceStep, len(eval.Steps))
	copy(steps, eval.Steps)
	// 开一个「工具编排」段落，前端会显示它正在干活
	steps = append(steps, agent.TraceStep{
		Phase:  "tools",
		Label:  "工具编排",
		Detail: "模型将自行决定调用哪些工具",
		Status: "active",
	})
	write(evTrace, jsonSafe(steps))

	// toolNo 是稳定的工具序号：不能用 len(steps) 推算——running 事件刚追加过步骤，
	// 用长度算会从 2 开始跳号（实测踩过）。
	toolNo := 0
	emit := func(ev agent.LoopEvent) {
		switch ev.Status {
		case "running":
			toolNo++
			d := "调用中"
			if ev.Args != "" {
				d += "：" + ev.Args
			}
			steps = append(steps, agent.TraceStep{
				Phase:  "tool",
				Label:  fmt.Sprintf("工具 %d · %s", toolNo, ev.Tool),
				Detail: d,
				Status: "active",
			})
			write(evTrace, jsonSafe(steps))
		case "done", "error":
			detail := ev.Note
			if detail == "" {
				detail = "完成"
			}
			if ev.Status == "error" {
				detail = "失败：" + detail
			}
			if len(steps) > 0 {
				steps[len(steps)-1].Detail = detail
				steps[len(steps)-1].Status = "done"
			}
			write(evTrace, jsonSafe(steps))
		}
	}

	loop := agent.NewLoop(h.eng, h.tools, h.maxRound)
	out, err := loop.Run(ctx, agentSystemPrompt(), toLLMMessages(history), req.Message, emit)
	if err != nil {
		// 循环失败：把错误交给前端，但不要静默变成空回答
		steps[len(steps)-1].Status = "done"
		steps[len(steps)-1].Detail = "失败：" + err.Error()
		write(evTrace, jsonSafe(steps))
		write(evError, jsonSafe(map[string]string{"error": "工具执行失败: " + err.Error()}))
		return true
	}

	for i := range steps {
		steps[i].Status = "done"
	}
	if len(steps) > 0 {
		if n := len(out.Calls); n > 0 {
			steps[len(steps)-1].Detail = fmt.Sprintf("共调用 %d 次工具", n)
		}
	}
	write(evTrace, jsonSafe(steps))

	text := strings.TrimSpace(out.Text)
	if text == "" {
		text = "（模型没有返回内容）"
	}
	write(evDelta, jsonSafe(map[string]string{"t": text}))

	// 交付工具产出的文件
	for _, f := range out.Files {
		tok := h.gen.put(f.Name, f.ContentType, f.Bytes)
		write(evFile, jsonSafe(map[string]string{
			"slug": "", "name": f.Name, "url": "/api/chat/gen/" + tok,
		}))
	}

	h.eng.Push(req.SessionID, agent.Message{
		Role: "assistant", Content: text, At: time.Now(),
	})
	write(evDone, jsonSafe(map[string]string{"skill": ""}))
	return true
}

// toLLMMessages 把会话历史翻译成工具循环用的消息（只取 user/assistant 文本）。
func toLLMMessages(hist []agent.Message) []llm.Msg {
	out := make([]llm.Msg, 0, len(hist))
	for _, m := range hist {
		if m.Content == "" {
			continue
		}
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		out = append(out, llm.Msg{Role: m.Role, Content: m.Content})
	}
	return out
}
