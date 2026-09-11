package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/lizhemin15/skillforge/internal/docgen"
)

// GenDocumentTool 从零生成办公文档（Word / Excel / PPT / PDF）。
//
// 和 fill_template 的分工：
//   - fill_template：用户已经有模板，把数据填进去（保留模板排版）。
//   - gen_document：没有模板，按标题/表格/段落从零拼一份。
//
// 引擎全部是纯 Go 离线实现（无外部服务、无 cgo），符合单二进制离线部署约束。
type GenDocumentTool struct{}

// NewGenDocumentTool 构造文档生成工具。
func NewGenDocumentTool() *GenDocumentTool { return &GenDocumentTool{} }

// Name 工具名。
func (t *GenDocumentTool) Name() string { return "gen_document" }

// Description 工具说明。
func (t *GenDocumentTool) Description() string {
	return "从零生成一份 Word/Excel/PDF/PPT 文档并交付给用户下载。" +
		"适合用户没有模板、直接要「给我一份 xx 表/报告」的场景。" +
		"表格数据放 rows（二维数组），行文放 paragraphs。生成 Excel 时数字会被写成数值，可直接求和。"
}

// Schema 参数定义。
func (t *GenDocumentTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"format": map[string]any{
				"type":        "string",
				"enum":        []string{"word", "excel", "pdf", "ppt"},
				"description": "文档类型：word(.docx) / excel(.xlsx) / pdf / ppt(.pptx)",
			},
			"title":      map[string]any{"type": "string", "description": "文档标题；Excel 里同时作为工作表名"},
			"filename":   map[string]any{"type": "string", "description": "可选：下载文件名（含扩展名）。不给则按标题自动生成"},
			"columns":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "表格表头，例如 [\"产品\",\"数量\",\"单价\"]"},
			"rows":       map[string]any{"type": "array", "items": map[string]any{"type": "array", "items": map[string]any{}}, "description": "表格数据，二维数组，每行长度与 columns 对齐，例如 [[\"云服务器\",5,12000]]"},
			"paragraphs": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "正文段落（Word/PDF/PPT 用），每项一段"},
		},
		"required": []string{"format", "title"},
	}
}

// Run 生成文档。
func (t *GenDocumentTool) Run(ctx context.Context, args map[string]any) (Result, error) {
	format := strings.ToLower(strings.TrimSpace(Str(args, "format")))
	switch format {
	case "word", "docx", "doc":
		format = "word"
	case "excel", "xlsx", "xls":
		format = "excel"
	case "pdf":
		format = "pdf"
	case "ppt", "pptx":
		format = "ppt"
	default:
		return Result{}, fmt.Errorf("不支持的格式 %q（可选 word/excel/pdf/ppt）", Str(args, "format"))
	}

	title := strings.TrimSpace(Str(args, "title"))
	if title == "" {
		return Result{}, errors.New("title 不能为空")
	}
	cols := strList(args["columns"])
	rows := strMatrix(args["rows"])
	parags := strList(args["paragraphs"])
	if len(cols) == 0 && len(rows) == 0 && len(parags) == 0 {
		return Result{}, errors.New("columns / rows / paragraphs 至少要给一个，否则文档是空的")
	}

	name := strings.TrimSpace(Str(args, "filename"))
	if name == "" {
		name = docgen.SafeFilenameAs(title, format)
	}

	out, err := docgen.Generate(docgen.Doc{
		Format:   format,
		Filename: name,
		Title:    title,
		Cols:     cols,
		Rows:     rows,
		Parags:   parags,
	})
	if err != nil {
		return Result{}, fmt.Errorf("生成失败: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "已生成 %s（%d 字节）。", name, len(out))
	if len(cols) > 0 {
		fmt.Fprintf(&b, "\n表格：%d 列 × %d 行数据。", len(cols), len(rows))
	}
	if len(parags) > 0 {
		fmt.Fprintf(&b, "\n正文：%d 段。", len(parags))
	}
	return Result{
		Content: b.String(),
		Files:   []File{{Name: name, ContentType: contentTypeOf(name), Bytes: out}},
		Display: fmt.Sprintf("生成 %s", name),
	}, nil
}

// strList 把 JSON 数组转成字符串切片（标量元素被规范化，数字不丢精度）。
func strList(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		if ss, ok := v.([]string); ok {
			return ss
		}
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, it := range arr {
		out = append(out, scalarText(it))
	}
	return out
}

// strMatrix 把 JSON 二维数组转成字符串矩阵（表格数据）。
func strMatrix(v any) [][]string {
	arr, ok := v.([]any)
	if !ok {
		if mm, ok := v.([][]string); ok {
			return mm
		}
		return nil
	}
	out := make([][]string, 0, len(arr))
	for _, row := range arr {
		r := strList(row)
		if r == nil {
			// 模型偶尔给一维数组或标量当行，包一层而不是丢掉
			r = []string{scalarText(row)}
		}
		out = append(out, r)
	}
	return out
}
