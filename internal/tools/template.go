package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lizhemin15/skillforge/internal/docgen"
)

// ---------------------------------------------------------------------------
// 模板来源（接口反转）
// ---------------------------------------------------------------------------

// TemplateRef 是一个可填模板的引用，由上层（api 层）从技能库提供。
type TemplateRef struct {
	Slug   string
	Rel    string // 相对技能目录的路径
	Name   string // 文件名
	Format string // docx | xlsx
	Size   int64
}

// TemplateSource 提供模板的只读访问。
//
// 这里必须用接口而不是直接依赖 store：agent 包已经 import 了 tools（loop.go），
// 若 tools 再 import store/agent 就成环。上层把 store 适配成这个接口注入进来。
type TemplateSource interface {
	Templates() []TemplateRef
	ReadTemplate(slug, rel string) ([]byte, error)
}

// templateID 是对外暴露的稳定标识，模型在 fill_template 里原样回传即可。
func (r TemplateRef) templateID() string { return r.Slug + "/" + r.Rel }

// ---------------------------------------------------------------------------
// list_templates
// ---------------------------------------------------------------------------

// ListTemplatesTool 让模型发现「有哪些模板可填、每个模板要填哪些字段」。
//
// 为什么需要它：模板是用户上传的文件，模型不可能凭记忆知道字段名。
// 没有这个工具，模型只能瞎猜 key，填出来全是空白。
type ListTemplatesTool struct {
	src TemplateSource
}

// NewListTemplatesTool 构造模板清单工具。
func NewListTemplatesTool(src TemplateSource) *ListTemplatesTool {
	return &ListTemplatesTool{src: src}
}

// Name 工具名。
func (t *ListTemplatesTool) Name() string { return "list_templates" }

// Description 工具说明。
func (t *ListTemplatesTool) Description() string {
	return "列出当前可用的文档模板（Word/Excel）以及每个模板需要填写的字段名。" +
		"准备用 fill_template 填模板前先调用它拿到准确的模板 id 和字段名，不要猜。"
}

// Schema 参数定义。
func (t *ListTemplatesTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"filter": map[string]any{"type": "string", "description": "可选关键词，按模板名/技能名过滤，例如 \"验收\""},
		},
	}
}

// Run 列出模板。
func (t *ListTemplatesTool) Run(ctx context.Context, args map[string]any) (Result, error) {
	if t.src == nil {
		return Result{Content: "模板功能未启用。", Display: "模板未启用"}, nil
	}
	refs := t.src.Templates()
	filter := strings.TrimSpace(Str(args, "filter"))
	if filter != "" {
		kept := refs[:0:0]
		for _, r := range refs {
			if strings.Contains(r.templateID(), filter) || strings.Contains(r.Name, filter) {
				kept = append(kept, r)
			}
		}
		refs = kept
	}
	if len(refs) == 0 {
		msg := "当前没有可填模板。"
		if filter != "" {
			msg = fmt.Sprintf("没有匹配「%s」的模板。", filter)
		}
		return Result{Content: msg + "如果用户需要填空表，可以用 gen_document 从零生成。", Display: "0 个模板"}, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "共 %d 个模板。用 fill_template 填充时，template 参数填下面的 id：\n", len(refs))
	for i, r := range refs {
		keys, err := t.fieldsOf(r)
		fmt.Fprintf(&b, "\n%d) id=%s | 技能=%s | 格式=%s", i+1, r.templateID(), r.Slug, r.Format)
		switch {
		case err != nil:
			fmt.Fprintf(&b, "\n   字段：读取失败（%v）", err)
		case len(keys) == 0:
			b.WriteString("\n   字段：模板里没有 {{占位符}}，只能用 ops 做结构编辑")
		default:
			b.WriteString("\n   字段：" + strings.Join(keys, "、"))
		}
	}
	return Result{Content: b.String(), Display: fmt.Sprintf("发现 %d 个模板", len(refs))}, nil
}

// maxKeysPerTemplate 限制单模板列出的字段数，避免占位符上千时把上下文撑爆。
const maxKeysPerTemplate = 80

// fieldsOf 读取模板里的 {{key}} 字段。
func (t *ListTemplatesTool) fieldsOf(r TemplateRef) ([]string, error) {
	data, err := t.src.ReadTemplate(r.Slug, r.Rel)
	if err != nil {
		return nil, err
	}
	keys, err := extractFields(data, r.Format)
	if err != nil {
		return nil, err
	}
	if len(keys) > maxKeysPerTemplate {
		keys = append(keys[:maxKeysPerTemplate:maxKeysPerTemplate],
			fmt.Sprintf("…（共 %d 个，省略其余）", len(keys)))
	}
	return keys, nil
}

// extractFields 按格式分派字段提取。
func extractFields(data []byte, format string) ([]string, error) {
	if format == "xlsx" {
		return docgen.ExtractFieldsXLSX(data)
	}
	return docgen.ExtractFields(data)
}

// ---------------------------------------------------------------------------
// fill_template
// ---------------------------------------------------------------------------

// FillTemplateTool 把数据填进用户上传的模板。
//
// 分工是刻意的：**语义映射（哪份数据对应哪个字段）由模型做，机械填充由这里做**。
// 不要在这里再调一次 LLM（老路径 FillDoc 那种做法），否则循环里套 LLM：
// 慢一倍、贵一倍，还会让模型看不到自己填了什么。
type FillTemplateTool struct {
	src TemplateSource
}

// NewFillTemplateTool 构造模板填充工具。
func NewFillTemplateTool(src TemplateSource) *FillTemplateTool {
	return &FillTemplateTool{src: src}
}

// Name 工具名。
func (t *FillTemplateTool) Name() string { return "fill_template" }

// Description 工具说明。
func (t *FillTemplateTool) Description() string {
	return "把数据填进用户已有模板，产出填好的 Word/Excel 文件并交付给用户下载。" +
		"values 的键必须与 list_templates 返回的字段名完全一致（区分大小写）。" +
		"需要增删表格行/段落时用 ops。填完会告诉你哪些字段还是空的。"
}

// Schema 参数定义。
func (t *FillTemplateTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"template": map[string]any{"type": "string", "description": "模板 id（list_templates 返回的 id，形如 采购验收单/采购验收单模板.xlsx）或模板文件名"},
			"values": map[string]any{
				"type":        "object",
				"description": "字段名→值。键用模板里的 {{字段名}}，值一律给字符串或数字。例如 {\"供应商\":\"华云科技\",\"合计金额\":12000}",
			},
			"ops": map[string]any{
				"type":        "array",
				"description": "可选（仅 docx）：在填充前对模板做结构编辑。每项形如 {\"action\":\"add_row\",\"table\":0,\"row\":-1,\"values\":[\"甲\",\"乙\"]}；action 支持 add_row/del_row/set_cell/add_para/set_para/del_para",
				"items":       map[string]any{"type": "object"},
			},
			"filename": map[string]any{"type": "string", "description": "可选：产出文件的下载名（含扩展名）。不给则按模板名自动命名"},
		},
		"required": []string{"template", "values"},
	}
}

// Run 执行填充。
func (t *FillTemplateTool) Run(ctx context.Context, args map[string]any) (Result, error) {
	if t.src == nil {
		return Result{Content: "模板功能未启用。", Display: "模板未启用"}, nil
	}
	want := strings.TrimSpace(Str(args, "template"))
	if want == "" {
		return Result{}, errors.New("template 参数为空")
	}
	ref, err := t.resolve(want)
	if err != nil {
		return Result{}, err
	}
	raw, err := t.src.ReadTemplate(ref.Slug, ref.Rel)
	if err != nil {
		return Result{}, fmt.Errorf("读取模板失败: %w", err)
	}

	vals, err := valueMap(args["values"])
	if err != nil {
		return Result{}, err
	}
	if len(vals) == 0 {
		return Result{}, errors.New("values 为空：请给出至少一个字段值")
	}

	// 模板声明的字段（用于漏填/错 key 自检）。
	keys, kerr := extractFields(raw, ref.Format)

	// 结构编辑先行：先按 ops 增删行，再填占位符，这样新加的行也能被本次 values 覆盖。
	tpl := raw
	if ops, oerr := parseOps(args["ops"]); oerr != nil {
		return Result{}, oerr
	} else if len(ops) > 0 {
		if ref.Format != "docx" {
			return Result{Content: "ops 结构编辑目前只支持 docx 模板，Excel 模板请直接把值放进 values。", Display: "ops 不支持 xlsx"}, nil
		}
		edited, eerr := docgen.RenderIR(docgen.IR{Mode: "edit", Ops: ops}, tpl)
		if eerr != nil {
			return Result{}, fmt.Errorf("应用 ops 失败: %w", eerr)
		}
		tpl = edited
	}

	var out []byte
	switch ref.Format {
	case "xlsx":
		out, err = docgen.FillXLSX(tpl, vals)
	default:
		out, err = docgen.Fill(tpl, vals)
	}
	if err != nil {
		return Result{}, fmt.Errorf("填充失败: %w", err)
	}

	name := strings.TrimSpace(Str(args, "filename"))
	if name == "" {
		name = filledName(ref)
	}

	// 自检：漏填的字段在产出里是空白（docgen.Fill 对缺值写空串，不留占位符），
	// 所以必须显式回报，否则模型会以为填完了，用户拿到一份空表才发现。
	var missing, unknown []string
	if kerr == nil {
		known := map[string]bool{}
		for _, k := range keys {
			known[k] = true
			if strings.TrimSpace(vals[k]) == "" {
				missing = append(missing, k)
			}
		}
		for k := range vals {
			if !known[k] {
				unknown = append(unknown, k)
			}
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "已生成 %s（%d 字节，来源模板：%s）。", name, len(out), ref.templateID())
	if len(missing) > 0 {
		fmt.Fprintf(&b, "\n⚠️ 以下 %d 个字段没有提供值，产出中这些位置是空白：%s。"+
			"\n如果需要填它们，请补上值后再调用一次 fill_template（其余字段的值请一并原样再传一次，工具是无状态的）。",
			len(missing), strings.Join(missing, "、"))
	}
	if len(unknown) > 0 {
		fmt.Fprintf(&b, "\n⚠️ 以下键在模板里并不存在，已忽略：%s。请核对 list_templates 返回的字段名。",
			strings.Join(unknown, "、"))
	}
	if len(missing) == 0 && len(unknown) == 0 && kerr == nil && len(keys) > 0 {
		fmt.Fprintf(&b, "全部 %d 个字段已填。", len(keys))
	}
	return Result{
		Content: b.String(),
		Files:   []File{{Name: name, ContentType: contentTypeOf(name), Bytes: out}},
		Display: fmt.Sprintf("填 %s", name),
	}, nil
}

// resolve 把模型给的 template 参数解析成具体模板。
// 宽容匹配：模型可能给完整 id、文件名、或去掉扩展名的名字。
func (t *FillTemplateTool) resolve(want string) (TemplateRef, error) {
	refs := t.src.Templates()
	if len(refs) == 0 {
		return TemplateRef{}, errors.New("当前没有可填模板")
	}
	norm := func(s string) string {
		return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(s, "\\", "/")))
	}
	w := norm(want)
	base := norm(filepath.Base(w))
	stem := strings.TrimSuffix(base, filepath.Ext(base))

	var fuzzy []TemplateRef
	for _, r := range refs {
		id := norm(r.templateID())
		nb := norm(r.Name)
		ns := strings.TrimSuffix(nb, filepath.Ext(nb))
		switch {
		case id == w || nb == base || ns == stem:
			return r, nil
		case strings.Contains(id, w) || strings.Contains(nb, base) || strings.Contains(ns, stem):
			fuzzy = append(fuzzy, r)
		}
	}
	switch len(fuzzy) {
	case 0:
		names := make([]string, 0, len(refs))
		for _, r := range refs {
			names = append(names, r.templateID())
		}
		return TemplateRef{}, fmt.Errorf("找不到模板「%s」。可用模板：%s（先用 list_templates 确认）", want, strings.Join(names, "、"))
	case 1:
		return fuzzy[0], nil
	default:
		names := make([]string, 0, len(fuzzy))
		for _, r := range fuzzy {
			names = append(names, r.templateID())
		}
		return TemplateRef{}, fmt.Errorf("「%s」匹配到多个模板，请用完整 id 指定：%s", want, strings.Join(names, "、"))
	}
}

// parseOps 把模型给的 ops 参数转成 docgen.Op 列表。
func parseOps(v any) ([]docgen.Op, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("ops 无法解析: %w", err)
	}
	var ops []docgen.Op
	if err := json.Unmarshal(b, &ops); err != nil {
		return nil, fmt.Errorf("ops 格式错误（应为对象数组）: %w", err)
	}
	return ops, nil
}

// valueMap 把 values 参数规整成 map[string]string。
//
// 不用通用的 StrMap：它对数字直接 fmt.Sprint(float64)，会把 1000000 写成
// "1e+06"、把 12000.50 写成 "12000.5" —— 填进财务模板就是错值。
// 这里对 json.Number 保留原文，并把 .0 结尾的整数值收敛成整数写法。
func valueMap(v any) (map[string]string, error) {
	if v == nil {
		return nil, nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		if ms, ok := v.(map[string]string); ok {
			out := make(map[string]string, len(ms))
			for k, vv := range ms {
				out[strings.TrimSpace(k)] = vv
			}
			return out, nil
		}
		return nil, errors.New("values 必须是对象（字段名→值）")
	}
	out := make(map[string]string, len(obj))
	for k, vv := range obj {
		out[strings.TrimSpace(k)] = scalarText(vv)
	}
	return out, nil
}

// scalarText 把 JSON 标量转成给人看的文本，数字不丢精度。
func scalarText(v any) string {
	switch n := v.(type) {
	case nil:
		return ""
	case string:
		return n
	case json.Number:
		return normalizeNumber(n.String())
	case float64:
		// 未启用 UseNumber 时会走到这里（例如单测直接构造 map）
		return normalizeNumber(fmt.Sprintf("%v", n))
	case bool:
		if n {
			return "是"
		}
		return "否"
	default:
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
		return fmt.Sprint(v)
	}
}

// normalizeNumber 把 "12000.0" → "12000"，"1e+06" → "1000000"，其余原样。
func normalizeNumber(s string) string {
	if !strings.ContainsAny(s, ".eE") {
		return s
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return s
	}
	if f == float64(int64(f)) && f < 1e15 && f > -1e15 {
		return fmt.Sprintf("%d", int64(f))
	}
	return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}

// filledName 按模板名生成产出文件名（去掉"模板"二字 + 加日期）。
func filledName(r TemplateRef) string {
	ext := filepath.Ext(r.Name)
	base := strings.TrimSuffix(r.Name, ext)
	base = strings.TrimSpace(strings.ReplaceAll(base, "模板", ""))
	if base == "" {
		base = "文档"
	}
	return fmt.Sprintf("%s_%s%s", base, time.Now().Format("20060102"), ext)
}
