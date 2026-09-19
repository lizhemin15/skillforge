package agent

import (
	"encoding/json"
	"strings"

	"github.com/lizhemin15/skillforge/internal/model"
)

// Eval 的「模型手抖容忍层」。
//
// 为什么需要：Eval 是**每一轮对话的第一跳**，它解析失败不是「少一个字段」，
// 而是整轮分类被丢掉 → 重试（多一次模型调用）→ 用户对着计时器多等几秒到几十秒。
// 线上实测（2026-09-19，purchase 类任务）模型返回的其它字段全对（intent/action/
// skill_slug/steps 四段俱全），唯独 params 是**被双重编码的字符串**：
//
//	{"intent":"docgen","action":"gen", ... ,"params":"{\"doc_type\":\"通知\",...}"}
//	                                          ^^^^^^^^ 对象被序列化成了字符串
//
// Go 的解码是严格的：一个字段形状不对 → `cannot unmarshal string into Go struct
// field Eval.params of type map[string]interface {}` → **整个 Eval 全丢**。
// 这与 internal/model/param_flex.go 是同族问题（同源缺陷成对出现），那边管
// Param 的字段，这边管 Eval 自己的字段。
//
// 三条边界（照 param_flex.go 的规矩）：
//  1. 只宽容**形状**，不宽容**语法**：真·非法 JSON 照旧报错，别把模型的错藏起来。
//  2. 不许把数据丢掉：字符串里包着的对象要解开并保留（解不开才退化成原文，不是空）。
//  3. 列表里单个元素坏掉不许连坐整条列表 —— 丢掉那一项，保住其余。

// rawEval 与 Eval 字段一一对应，全部用 RawMessage 接住「形状不确定」的风险。
// 用独立结构体是为了不触发自身的 UnmarshalJSON 造成无限递归。
type rawEval struct {
	SkillSlug  json.RawMessage `json:"skill_slug"`
	Intent     json.RawMessage `json:"intent"`
	Reason     json.RawMessage `json:"reason"`
	Params     json.RawMessage `json:"params"`
	Needs      json.RawMessage `json:"needs"`
	Steps      json.RawMessage `json:"steps"`
	Action     json.RawMessage `json:"action"`
	NeedsTools json.RawMessage `json:"needs_tools"`
}

// UnmarshalJSON 宽容解析分类结果：字段形状不标准时降级继续，而不是整轮失败。
func (e *Eval) UnmarshalJSON(b []byte) error {
	var r rawEval
	if err := json.Unmarshal(b, &r); err != nil {
		// 语法错（真·非法 JSON）照旧抛出：宽容层不许掩护模型的语法错误。
		return err
	}
	e.SkillSlug = flexText(r.SkillSlug)
	e.Intent = flexText(r.Intent)
	e.Reason = flexText(r.Reason)
	e.Action = flexText(r.Action)
	e.Params = flexObj(r.Params)
	e.NeedsTools = flexTruthy(r.NeedsTools)
	e.Needs = flexParams(r.Needs)
	e.Steps = flexSteps(r.Steps)
	return nil
}

// flexText 把任意 JSON 值收敛成短文本字段（字符串原样；数字/布尔还原；其余走紧凑 JSON）。
func flexText(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	if sk, ok := v.(map[string]any); ok {
		// 只取可见文案键，取不到就把紧凑 JSON 原样留下（宁可留丑文本也不丢信息）。
		for _, k := range []string{"value", "text", "label", "name", "slug", "id"} {
			if x, ok := sk[k].(string); ok && strings.TrimSpace(x) != "" {
				return x
			}
		}
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
		return ""
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return ""
}

// flexObj 把「对象 / 被双重编码的字符串对象 / null」统一收敛成 map。
// 线上那句 `"params":"{\"doc_type\":\"通知\"}"` 就是这里被救回来的。
func flexObj(raw json.RawMessage) map[string]interface{} {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	// ① 正常形状：对象。
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err == nil {
		return m
	}
	// ② 双重编码：字符串里包着一段 JSON（线上实测形态）。最多再解两层，
	//    防 `"\"{\\\"a\\\":1}\""` 这种反复套娃把解析拖成无底洞。
	cur := raw
	for i := 0; i < 3; i++ {
		var str string
		if err := json.Unmarshal(cur, &str); err != nil {
			break
		}
		inner := strings.TrimSpace(str)
		if inner == "" || inner == "null" {
			return nil
		}
		if err := json.Unmarshal([]byte(inner), &m); err == nil {
			return m
		}
		cur = []byte(inner)
	}
	// ③ 是数组：包一层，别让整包参数消失（下游按 key 取值，键名用 items）。
	var arr []interface{}
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		return map[string]interface{}{"items": arr}
	}
	return nil
}

// flexTruthy 容忍 "true" / "是" / 1 这类字符串布尔值。
func flexTruthy(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		switch strings.ToLower(strings.TrimSpace(str)) {
		case "true", "1", "yes", "y", "是", "需要", "on":
			return true
		}
		return false
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n != 0
	}
	return false
}

// flexList 把「数组 / 被编码成字符串的数组 / 单个对象」统一成元素切片。
func flexList(raw json.RawMessage) []json.RawMessage {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err == nil {
		return list
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		inner := strings.TrimSpace(str)
		if inner == "" || inner == "null" {
			return nil
		}
		if err := json.Unmarshal([]byte(inner), &list); err == nil {
			return list
		}
		return nil
	}
	// 单个对象当成只含一项的列表：模型有时省略方括号。
	var one json.RawMessage
	if err := json.Unmarshal(raw, &one); err == nil && len(one) > 0 {
		return []json.RawMessage{one}
	}
	return nil
}

// flexParams 宽容解析 needs：单个元素坏掉只丢那一项，不连坐整条列表。
func flexParams(raw json.RawMessage) []model.Param {
	items := flexList(raw)
	if len(items) == 0 {
		return nil
	}
	out := make([]model.Param, 0, len(items))
	for _, it := range items {
		var p model.Param
		if err := json.Unmarshal(it, &p); err != nil {
			// 坏的那一项丢掉，其余照收（用户的必填项少一项，也强过整轮重来）。
			continue
		}
		out = append(out, p)
	}
	return out
}

// phaseLabel 步骤板四个阶段的固定文案。
//
// 为什么 label 由服务端补而不是让模型写：label 是**纯固定文案**，模型每轮把它抄一遍
// 要花掉这一跳最贵的东西——输出 tokens。线上生效那家 provider（siliconflow/
// Qwen3.6-27B）实测吐字 ~50 tok/s：契约里让模型写 label+status 时全篇输出 811 字、
// 这一跳 10.3s；改成只写 phase+detail 后输出 290 字、2.9s，路由结论逐条一致。
// 所以「固定文案」一律留在服务端，模型只写它真正独有的信息（detail）。
//
// 与 internal/api 的 fallbackSteps 是同一套文案（那边按意图再细分，这里是通用版）。
var phaseLabel = map[string]string{
	"analyze":  "① 意图分析",
	"match":    "② 工具匹配",
	"params":   "③ 参数提取",
	"generate": "④ 执行中",
}

// flexSteps 宽容解析步骤板：phase/status 缺失或为空时补默认值，否则前端会因为
// 认不出阶段而整块不渲染 —— 用户就又看到「只有一个跳秒的计时」。
func flexSteps(raw json.RawMessage) []TraceStep {
	items := flexList(raw)
	if len(items) == 0 {
		return nil
	}
	out := make([]TraceStep, 0, len(items))
	for _, it := range items {
		var s TraceStep
		if err := json.Unmarshal(it, &s); err != nil {
			continue
		}
		s.Phase = strings.TrimSpace(s.Phase)
		s.Label = strings.TrimSpace(s.Label)
		s.Detail = strings.TrimSpace(s.Detail)
		s.Status = strings.ToLower(strings.TrimSpace(s.Status))
		if s.Label == "" {
			// 模型按瘦身契约只给 phase+detail，label 在这里补齐（认不出 phase 就留空，
			// 由前端按 phase 兜底，绝不因为「label 缺失」把整条步骤丢掉）。
			s.Label = phaseLabel[s.Phase]
		}
		if s.Phase == "" && s.Label == "" && s.Detail == "" {
			continue
		}
		switch s.Status {
		case "pending", "active", "done":
		default:
			s.Status = "done"
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
