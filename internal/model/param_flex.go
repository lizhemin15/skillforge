package model

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// 大模型输出 JSON 的「手抖容忍层」。
//
// 为什么需要（线上实测 2026-09-17，训练 leg 抓到的真故障）：
//
//	技能 “线上流式leg09170307” 失败，用时 18s：
//	step2: 元数据：模型输出不是合法 JSON：
//	  json: cannot unmarshal object into Go struct field Param.input_params.options of type string
//
// 模型很自然地把下拉选项写成对象数组：
//
//	"options": [{"label": "不启用", "value": "off"}, {"label": "启用", "value": "on"}]
//
// 而 Param.Options 是 []string，Go 严格解会当场报错 → **整轮训练在第 2 步中断**。
// 用户看到的是「训练失败」，而实际只是某个字段的形状不标准 —— 一个字段的瑕疵
// 不该有把整轮训练连坐的杀伤力（这也是用户投诉「生成的技能跟我给的东西没关系」
// 的一个隐性来源：训练根本没走到写提示词那一步）。
//
// 设计原则：
//  1. 能救的救回来 —— 对象取可见文案键（label/text/title/name/value/id），
//     数字按整/浮点还原成字符串；
//  2. 救不回来就退化成空串/空数组，**绝不因为形状问题让整轮训练失败**；
//  3. 只宽容「形状」，不宽容「语法」—— 真·非法 JSON 依旧报错（jsonErrDetail 带原文），
//     否则等于把模型的错藏起来，排障时两眼一抹黑。
//
// 注意：字段类型保持原样（Options 仍是 []string），前端与模板一行都不用改。
func (p *Param) UnmarshalJSON(b []byte) error {
	// 全部先收成 RawMessage，再逐个按「宽容规则」落位。
	// 用独立结构体是为了不触发自身 UnmarshalJSON 造成无限递归。
	type rawParam struct {
		Name        json.RawMessage `json:"name"`
		Label       json.RawMessage `json:"label"`
		Type        json.RawMessage `json:"type"`
		Required    json.RawMessage `json:"required"`
		Placeholder json.RawMessage `json:"placeholder"`
		Help        json.RawMessage `json:"help"`
		Options     json.RawMessage `json:"options"`
		Default     json.RawMessage `json:"default"`
		Min         json.RawMessage `json:"min"`
		Max         json.RawMessage `json:"max"`
	}
	var r rawParam
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	p.Name = flexStr(r.Name)
	p.Label = flexStr(r.Label)
	p.Type = flexStr(r.Type)
	p.Required = flexBool(r.Required)
	p.Placeholder = flexStr(r.Placeholder)
	p.Help = flexStr(r.Help)
	p.Options = flexStrList(r.Options)
	p.Default = flexStr(r.Default)
	p.Min = flexInt(r.Min)
	p.Max = flexInt(r.Max)
	return nil
}

// flexStr 把任意 JSON 值收敛成字符串字段。
// 字符串原样；数字还原（3.0 → "3"）；布尔 → "true"/"false"；
// 数组用「、」连接；对象取常见文案键，取不到就退化成紧凑 JSON。
func flexStr(raw json.RawMessage) string {
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
	return anyToStr(v)
}

// flexStrList 把任意 JSON 值收敛成 []string（下拉选项）。
// 对象数组取 label/text/... ；对象映射取键（键才是给人看的文案，排序保证确定性）；
// 单个字符串按常见分隔符拆开（"甲、乙、丙" / "甲,乙" / 换行分隔）。
func flexStrList(raw json.RawMessage) []string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return cleanList(list)
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return splitLoose(one)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if x := strings.TrimSpace(anyToStr(e)); x != "" {
				out = append(out, x)
			}
		}
		return cleanList(out)
	case map[string]any:
		if x := pickLabel(t); x != "" {
			return []string{x}
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			if strings.TrimSpace(k) != "" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		return cleanList(keys)
	}
	return nil
}

// flexBool 容忍 "true" / "1" / "是" 这类字符串布尔值。
func flexBool(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f != 0
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		switch strings.ToLower(strings.TrimSpace(str)) {
		case "true", "1", "yes", "y", "是", "需要", "必填":
			return true
		}
	}
	return false
}

// flexInt 容忍 "3" / "3.0" / 3 这类数字字段；解不出就 0（min/max 退化成「无约束」）。
func flexInt(raw json.RawMessage) int {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return int(f)
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		if n, err := strconv.ParseFloat(strings.TrimSpace(str), 64); err == nil {
			return int(n)
		}
	}
	return 0
}

// anyToStr 是宽容层的最后一跳：任何 JSON 值 → 人类看得懂的字符串。
func anyToStr(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			if s := strings.TrimSpace(anyToStr(e)); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "、")
	case map[string]any:
		if s := pickLabel(t); s != "" {
			return s
		}
		if out, err := json.Marshal(t); err == nil {
			return string(out)
		}
		return ""
	}
	return fmt.Sprint(v)
}

// pickLabel 从对象里挑「给人看的那个值」。
// 顺序有讲究：label 优先，其次 text/title/name（都是文案），最后才是 value/id（多是机器码）。
func pickLabel(m map[string]any) string {
	for _, k := range []string{"label", "text", "title", "name", "value", "id"} {
		if v, ok := m[k]; ok {
			if s := strings.TrimSpace(anyToStr(v)); s != "" {
				return s
			}
		}
	}
	return ""
}

// splitLoose 把 "甲、乙、丙" 这类单字符串拆成多项。
// 只在真出现分隔符且拆完 ≥2 项时拆 —— 否则一个正常选项名被逗号拆成两半更糟。
func splitLoose(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	if !strings.ContainsAny(s, "、,，;；\n") {
		return []string{strings.TrimSpace(s)}
	}
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '、' || r == ',' || r == '，' || r == ';' || r == '；' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	if len(out) < 2 {
		return []string{strings.TrimSpace(s)}
	}
	return out
}

// cleanList 去掉空项；全空返回 nil（前端据此不渲染 select 的下拉内容）。
func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, x := range in {
		if t := strings.TrimSpace(x); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
