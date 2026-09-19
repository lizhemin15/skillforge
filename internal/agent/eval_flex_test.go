package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// 线上实测原文（2026-09-19，通知类文档生成任务）。
// 注意 params 是**被双重编码的字符串**，其余字段全部正常 —— 这正是它危险的地方：
// 模型这一轮判断完全正确，严格解却把整包丢掉，于是走 retry，用户白等一次模型调用。
// 这段必须**逐字**保留（含中文与嵌套引号），改一个字就不再是那个真故障了。
const liveDoubleEncodedEval = `{"intent":"docgen","action":"gen","skill_slug":"办公文档管家","needs_tools":false,` +
	`"reason":"用户明确要求生成一份新的通知类Word文档，且提供了详细的背景素材，属于新建文档生成。",` +
	`"params":"{\"doc_type\":\"通知\",\"title\":\"关于开展数据治理专项行动的通知\",` +
	`\"content_summary\":\"自2026年起推进数据治理，建成数据中台，完成12类主数据梳理，需规范数据质量与安全责任，生成Word文件\"}",` +
	`"needs":[],"steps":[` +
	`{"phase":"analyze","label":"① 意图分析","detail":"识别出的意图与动作：docgen/gen（生成新Word文档）","status":"done"},` +
	`{"phase":"match","label":"② 工具匹配","detail":"决定调用工具。办公文档管家（通用文档生成能力）","status":"done"},` +
	`{"phase":"params","label":"③ 参数提取","detail":"已提取文件类型、标题、背景及核心素材，无需补充参数","status":"done"},` +
	`{"phase":"generate","label":"④ 执行中","detail":"将执行生成包含指定内容的Word文件并下载","status":"active"}]}`

// TestEvalLiveDoubleEncodedParamsIsTolerated 是这条修复的承重断言：
// 线上那一次真故障原文必须能解析，且**字段一个不少**。
// 只断言「没报错」是不够的 —— 解出来但 params 为空，下游照样拿不到文件类型与标题，
// 用户的观感与整包失败几乎一样（步骤板显示「无需补充参数」，实际什么都没提到）。
func TestEvalLiveDoubleEncodedParamsIsTolerated(t *testing.T) {
	var e Eval
	if err := json.Unmarshal([]byte(liveDoubleEncodedEval), &e); err != nil {
		t.Fatalf("线上原文应当能解析（双重编码的 params 不该连坐整包），却报：%v", err)
	}
	if e.Intent != "docgen" || e.Action != "gen" {
		t.Fatalf("意图/动作被解析丢了：intent=%q action=%q", e.Intent, e.Action)
	}
	if e.SkillSlug != "办公文档管家" {
		t.Fatalf("技能命中被解析丢了：skill_slug=%q", e.SkillSlug)
	}
	if len(e.Params) == 0 {
		t.Fatal("params 一个都没解出来 —— 等于没修（严格解也是这个观感）")
	}
	// 双重编码里包着的键必须原样取回，不能退化成整段 JSON 文本。
	if got := e.Params["doc_type"]; got != "通知" {
		t.Fatalf("doc_type 期望「通知」，实际 %#v", got)
	}
	if got, _ := e.Params["title"].(string); !strings.Contains(got, "数据治理专项行动") {
		t.Fatalf("title 没取回来：%#v", e.Params["title"])
	}
	if _, ok := e.Params["content_summary"].(string); !ok {
		t.Fatalf("content_summary 应当是字符串，实际 %#v", e.Params["content_summary"])
	}
	if e.NeedsTools {
		t.Fatal("needs_tools=false 被解析成了 true")
	}
	// 步骤板必须完整：少一段就是「用户又只看到一个跳秒的计时」。
	if len(e.Steps) != 4 {
		t.Fatalf("步骤板期望 4 段，实际 %d 段：%#v", len(e.Steps), e.Steps)
	}
	if e.Steps[3].Status != "active" || e.Steps[0].Phase != "analyze" {
		t.Fatalf("步骤板相位/状态不对：%#v", e.Steps)
	}
}

// TestEvalNormalShapeUnchanged 是控制测试：正常形状必须逐字段照旧，
// 不能因为加了宽容层就把正常输入也「变形」（宽容层最容易做成「什么都吞」）。
func TestEvalNormalShapeUnchanged(t *testing.T) {
	src := `{"skill_slug":"述职报告","intent":"write","reason":"命中技能",` +
		`"params":{"columns":["姓名","部门","岗位","入职日期"],"qty":5},` +
		`"needs":[{"name":"期间","required":true,"options":[{"label":"不启用","value":"off"}],` +
		`"min":"1","max":"12"}],` +
		`"steps":[{"phase":"analyze","label":"① 意图分析","detail":"写作任务","status":"done"}],` +
		`"action":"write","needs_tools":false}`
	var e Eval
	if err := json.Unmarshal([]byte(src), &e); err != nil {
		t.Fatalf("正常形状解析失败：%v", err)
	}
	if e.SkillSlug != "述职报告" || e.Intent != "write" || e.Action != "write" {
		t.Fatalf("标量字段被改动：%#v", e)
	}
	// 数组值必须留在 params 里（陷阱 11 的老账，别在宽容层里丢掉）。
	if cols, ok := e.Params["columns"].([]interface{}); !ok || len(cols) != 4 {
		t.Fatalf("数组值 columns 丢了或变形：%#v", e.Params["columns"])
	}
	// 数字值必须还原成数字口径（陷阱 12），不能被悄悄转成字符串。
	if n, ok := e.Params["qty"].(float64); !ok || n != 5 {
		t.Fatalf("数字值 qty 变了：%#v", e.Params["qty"])
	}
	if len(e.Needs) != 1 || e.Needs[0].Name != "期间" || !e.Needs[0].Required {
		t.Fatalf("needs 解析不对：%#v", e.Needs)
	}
	if len(e.Needs[0].Options) == 0 {
		t.Fatal("needs[0].options 丢了（对象数组未被容错层收下）")
	}
	if e.NeedsTools {
		t.Fatal("needs_tools=false 被解析成了 true")
	}
	// 回写：不能因为过了一遍宽容层就丢字段（下游有人 re-marshal 给前端）。
	out, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("回写失败：%v", err)
	}
	for _, k := range []string{"skill_slug", "intent", "params", "needs", "steps", "action"} {
		if !strings.Contains(string(out), `"`+k+`"`) {
			t.Fatalf("回写丢了字段 %s：%s", k, out)
		}
	}
}

// TestEvalSyntaxErrorStillFails 明确一条边界：宽容层**只宽容形状，不宽容语法**。
// 真·非法 JSON 必须照旧报错，否则模型把 JSON 写坏了也静默放行，排障两眼一抹黑。
//
// ⚠️ 这条断言不参与注入自证：它的红来自 Go 顶层解码器（在进入 UnmarshalJSON 之前），
// 不依赖本包代码 —— 拿它当注入目标只会得到假自证。详见 regression-guard-quality。
func TestEvalSyntaxErrorStillFails(t *testing.T) {
	bad := `{"intent":"docgen",,,"params":{"a":1}}`
	var e Eval
	if err := json.Unmarshal([]byte(bad), &e); err == nil {
		t.Fatal("语法错的 JSON 被静默放行了 —— 宽容层越界了")
	}
}

// TestEvalOddShapesStillUsable 覆盖几种模型真实会吐的怪形状：
// params 是数组 / 是空串 / needs 里混进坏元素。任一形状都不许连坐整包。
func TestEvalOddShapesStillUsable(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantKey string // params 里应当存在的键（空串表示允许 params 为空）
	}{
		{"params 是数组", `{"intent":"chat","params":[{"a":1}],"action":"answer"}`, "items"},
		{"params 是空串", `{"intent":"chat","params":"","action":"answer"}`, ""},
		{"params 是 null", `{"intent":"chat","params":null,"action":"answer"}`, ""},
		{"params 是数字", `{"intent":"chat","params":5,"action":"answer"}`, ""},
		{"needs 是字符串数组", `{"intent":"write","needs":"[{\"name\":\"期间\"}]","params":{"k":"v"}}`, "k"},
		{"needs 混进坏元素", `{"intent":"write","needs":[{"name":"期间"},42],"params":{"k":"v"}}`, "k"},
		{"steps 被编码成字符串", `{"intent":"write","steps":"[{\"phase\":\"analyze\"}]","params":{"k":"v"}}`, "k"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var e Eval
			if err := json.Unmarshal([]byte(c.src), &e); err != nil {
				t.Fatalf("怪形状不该让整包失败：%v", err)
			}
			if strings.TrimSpace(e.Intent) == "" {
				t.Fatalf("intent 丢了：%#v", e)
			}
			if c.wantKey != "" {
				if _, ok := e.Params[c.wantKey]; !ok {
					t.Fatalf("params 期望含键 %q，实际 %#v", c.wantKey, e.Params)
				}
			}
		})
	}
}

// TestEvalStepsDefaultStatus 步骤板相位/状态缺失时不能整段丢：
// 前端认不出 phase/status 就不渲染，用户看到的就是「只有一个跳秒的计时」。
// TestEvalLeanStepsGetLabels 守住「瘦身契约」的另一半：
//
// 线上生效那家 provider 吐字 ~50 tok/s，让模型每轮把 label/status 这些**固定文案**
// 抄一遍要花掉这一跳最贵的东西 —— 实测全篇 811 字/10.3s vs 290 字/2.9s（路由结论逐条一致）。
// 所以契约改成「模型只给 phase+detail，label/status 服务端补」。
//
// 这条断言的承重面：**补 label 这一半没做，步骤板就会变成空格子**（前端按 label 显示
// ①~④ 阶段名，空了就等于回到「只有一个跳秒的计时」）。所以这里钉死四个阶段的具体文案，
// 而不是只断言「label 非空」——文案漂了，用户看到的阶段名就漂了。
func TestEvalLeanStepsGetLabels(t *testing.T) {
	// 瘦身契约下模型的真实输出形态：只有 phase + detail，没有 label / status。
	const lean = `{"intent":"write","action":"write","skill_slug":"公司新闻通稿","needs_tools":false,` +
		`"reason":"通用写作","params":{},"needs":[],"steps":[` +
		`{"phase":"analyze","detail":"write/write"},` +
		`{"phase":"match","detail":"命中 公司新闻通稿"},` +
		`{"phase":"params","detail":"提炼用户内容"},` +
		`{"phase":"generate","detail":"通用写作"}]}`
	var e Eval
	if err := json.Unmarshal([]byte(lean), &e); err != nil {
		t.Fatalf("瘦身契约的输出应当能解析，却报：%v", err)
	}
	if len(e.Steps) != 4 {
		t.Fatalf("4 个阶段都该留下，实际 %d 个：%+v", len(e.Steps), e.Steps)
	}
	want := []string{"① 意图分析", "② 工具匹配", "③ 参数提取", "④ 执行中"}
	for i, s := range e.Steps {
		if s.Label != want[i] {
			t.Errorf("第 %d 步的 label 应由服务端按 phase 补齐成 %q，实际 %q", i+1, want[i], s.Label)
		}
		if s.Detail == "" {
			t.Errorf("第 %d 步 detail 是模型真正独有的信息，不该丢", i+1)
		}
	}
	// detail 必须原样保留：模型写的是「命中了哪个技能」，这才是用户要看的调度过程。
	if !strings.Contains(e.Steps[1].Detail, "公司新闻通稿") {
		t.Errorf("② 的 detail 应保留模型给的技能名，实际 %q", e.Steps[1].Detail)
	}
	// 认不出的 phase 不许连坐丢掉整条步骤（宁可 label 空着让前端按 phase 兜底）。
	var odd Eval
	if err := json.Unmarshal([]byte(`{"intent":"chat","steps":[{"phase":"unknown","detail":"x"}]}`), &odd); err != nil {
		t.Fatalf("未知 phase 不该让整包失败：%v", err)
	}
	if len(odd.Steps) != 1 {
		t.Fatalf("未知 phase 的步骤也该保留，实际 %d 条", len(odd.Steps))
	}
}

// TestEvalStepsDefaultStatus 步骤板 status 缺失时补 done，
// 否则前端会因为认不出状态而不给这一步画格子。
func TestEvalStepsDefaultStatus(t *testing.T) {
	var e Eval
	src := `{"intent":"write","action":"write","steps":[{"label":"① 意图分析"},{"phase":"match","status":"奇怪的词"}]}`
	if err := json.Unmarshal([]byte(src), &e); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(e.Steps) != 2 {
		t.Fatalf("步骤板期望 2 段，实际 %d", len(e.Steps))
	}
	for i, s := range e.Steps {
		if s.Status != "done" {
			t.Fatalf("第 %d 段状态期望兜底成 done，实际 %q", i+1, s.Status)
		}
	}
}
