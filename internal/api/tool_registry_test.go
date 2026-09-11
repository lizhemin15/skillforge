package api

import (
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/tools"
)

// TestBuildToolRegistryRegistersDocTools 守住「工具注册」这一步。
//
// 为什么值得一条测试：工具没注册**不会报任何错**——模型只是看不到它，
// 于是退回去凭空编内容，用户看到的是「答案不对」而不是「功能没开」。
// 这类静默失效最难查，所以注册表本身要有断言。
func TestBuildToolRegistryRegistersDocTools(t *testing.T) {
	t.Setenv("SKILLFORGE_TOOLS", "")
	t.Setenv("SKILLFORGE_EXEC", "")
	t.Setenv("SKILLFORGE_DOCS", "")

	reg := buildToolRegistry(nil)
	if reg == nil {
		t.Fatal("默认配置下工具注册表不该是 nil（除非显式 SKILLFORGE_TOOLS=off）")
	}

	for _, name := range []string{
		"http_request", "run_python",
		"list_templates", "fill_template", "gen_document",
	} {
		tool, ok := reg.Get(name)
		if !ok {
			t.Errorf("工具 %q 没注册，模型永远看不到它（且不会报错）", name)
			continue
		}
		// 描述为空模型就不知道何时该用，等于没注册。
		if strings.TrimSpace(tool.Description()) == "" {
			t.Errorf("工具 %q 没有描述，模型不会用对", name)
		}
		// required 必须是数组：写成字符串会被 LLM 接口直接拒掉。
		if req, ok := tool.Schema()["required"]; ok {
			if _, isArr := req.([]string); !isArr {
				t.Errorf("工具 %q 的 schema.required 不是 []string（实际 %T），接口会报错", name, req)
			}
		}
	}
}

// TestDocToolsCanBeDisabledIndependently 保证开关是「外科手术式」的：
// 关掉文档工具不能顺手把取数能力也关了（那样用户会以为整个助手坏了）。
func TestDocToolsCanBeDisabledIndependently(t *testing.T) {
	t.Setenv("SKILLFORGE_TOOLS", "")
	t.Setenv("SKILLFORGE_DOCS", "off")

	reg := buildToolRegistry(nil)
	if reg == nil {
		t.Fatal("只关文档工具不该让整个注册表变成 nil")
	}
	if _, ok := reg.Get("fill_template"); ok {
		t.Error("SKILLFORGE_DOCS=off 之后 fill_template 仍然注册着")
	}
	if _, ok := reg.Get("gen_document"); ok {
		t.Error("SKILLFORGE_DOCS=off 之后 gen_document 仍然注册着")
	}
	if _, ok := reg.Get("http_request"); !ok {
		t.Error("关文档工具把 http_request 也带走了")
	}

	t.Setenv("SKILLFORGE_TOOLS", "off")
	if reg := buildToolRegistry(nil); reg != nil {
		t.Error("SKILLFORGE_TOOLS=off 应该返回 nil（回退纯文本对话）")
	}
}

// TestStoreTemplatesAdapter 验证 api 层的适配器真的能把技能库里的模板
// 翻译成 tools.TemplateSource 认识的引用（路径/格式翻译错 = 工具填不了模板）。
func TestStoreTemplatesAdapter(t *testing.T) {
	dir := t.TempDir()
	writeFakeSkillTemplate(t, dir, "采购验收单", "采购验收单模板.xlsx")

	s := newStoreForTest(t, dir)
	src := newStoreTemplates(s)

	refs := src.Templates()
	if len(refs) != 1 {
		t.Fatalf("应扫出 1 个模板，实际 %d 个：%+v", len(refs), refs)
	}
	r := refs[0]
	if r.Slug != "采购验收单" || r.Name != "采购验收单模板.xlsx" {
		t.Errorf("模板引用翻译错了：%+v", r)
	}
	if r.Format != "xlsx" {
		t.Errorf("格式应为 xlsx，实际 %q", r.Format)
	}
	if r.Size == 0 {
		t.Error("模板大小不该是 0（说明 stat 没走到）")
	}

	// 关键一步：模型拿到引用后要能原样读回来。
	b, err := src.ReadTemplate(r.Slug, r.Rel)
	if err != nil {
		t.Fatalf("按引用读模板失败（模型会因此填不出文件）：%v", err)
	}
	if len(b) == 0 {
		t.Fatal("读回来的模板是空的")
	}

	// 读一个不存在的模板必须报错而不是返回空字节，
	// 否则工具会把「一个空文件」当成正常结果交付给用户。
	if _, err := src.ReadTemplate(r.Slug, "不存在的.docx"); err == nil {
		t.Error("读不存在的模板竟然没报错")
	}
}

// TestFillTemplateToolThroughRegistry 走一遍完整链路：
// 注册表取工具 → 模型式参数 → 真填出文件。防的是「工具在但跑不通」。
func TestFillTemplateToolThroughRegistry(t *testing.T) {
	t.Setenv("SKILLFORGE_TOOLS", "")
	dir := t.TempDir()
	writeFakeSkillTemplate(t, dir, "采购验收单", "采购验收单模板.xlsx")
	s := newStoreForTest(t, dir)

	reg := buildToolRegistry(s)
	tool, ok := reg.Get("fill_template")
	if !ok {
		t.Fatal("fill_template 没注册")
	}

	listTool, ok := reg.Get("list_templates")
	if !ok {
		t.Fatal("list_templates 没注册")
	}
	listRes, err := listTool.Run(t.Context(), map[string]any{})
	if err != nil {
		t.Fatalf("list_templates 执行失败：%v", err)
	}
	if !strings.Contains(listRes.Content, "采购验收单") {
		t.Errorf("list_templates 的输出里没有模板名，模型会以为没有模板：%s", listRes.Content)
	}

	res, err := tool.Run(t.Context(), map[string]any{
		"template": "采购验收单/采购验收单模板.xlsx",
		"values":   map[string]any{"amount": "12000", "name": "云服务器"},
	})
	if err != nil {
		t.Fatalf("fill_template 执行失败：%v", err)
	}
	if len(res.Files) == 0 {
		t.Fatal("fill_template 没有交付文件")
	}
	if len(res.Files[0].Bytes) == 0 {
		t.Fatal("交付的文件是空的")
	}
	var _ tools.Result = res
}
