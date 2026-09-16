package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// 断言自证：见 internal/model/param_flex_mutation_check.sh
// 一句话：往出货文件 param_flex.go 注入「退回严格解」的真实故障，本测试必须变红。
//
// 本文件只锁一件事：**一个字段的形状不标准，不能让整轮训练失败**。
// 线上原始故障（2026-09-17）：options 是对象数组 → 整轮训练第 2 步 18 秒中断。

// mustParse 解不出来直接 Fatalf —— 「解不出来」本身就是被测的坏结果。
func mustParse(t *testing.T, raw string) Param {
	t.Helper()
	var s struct {
		InputParams []Param `json:"input_params"`
	}
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("整轮训练会在这里中断（这正是线上故障的形态）：%v\n原文=%s", err, raw)
	}
	if len(s.InputParams) != 1 {
		t.Fatalf("字段数=%d，期望 1\n原文=%s", len(s.InputParams), raw)
	}
	return s.InputParams[0]
}

func eqList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestParamOptionsObjectArrayIsTolerated 就是线上那一次真故障的复刻。
// 注入「退回严格解」后，这条必须先红。
func TestParamOptionsObjectArrayIsTolerated(t *testing.T) {
	p := mustParse(t, `{"input_params":[{
		"name":"enable","label":"是否启用","type":"select","required":true,
		"options":[{"label":"不启用","value":"off"},{"label":"启用","value":"on"}]}]}`)
	if !eqList(p.Options, []string{"不启用", "启用"}) {
		t.Fatalf("options=%q，期望取 label 文案 [不启用 启用]", p.Options)
	}
	if p.Label != "是否启用" || p.Name != "enable" || p.Type != "select" || !p.Required {
		t.Fatalf("同一条记录的其它字段被连坐：%+v", p)
	}
}

// 对象数组里没有 label：退到 text/title/name/value，不许解不出来。
func TestParamOptionsObjectKeysFallback(t *testing.T) {
	p := mustParse(t, `{"input_params":[{"options":[
		{"text":"甲"},{"title":"乙"},{"name":"丙"},{"value":"丁"},{"id":"戊"}]}]}`)
	if !eqList(p.Options, []string{"甲", "乙", "丙", "丁", "戊"}) {
		t.Fatalf("options=%q，期望 [甲 乙 丙 丁 戊]", p.Options)
	}
}

// 单字符串选项：真带分隔符才拆。
func TestParamOptionsSingleStringSplit(t *testing.T) {
	p := mustParse(t, `{"input_params":[{"options":"甲、乙、丙"}]}`)
	if !eqList(p.Options, []string{"甲", "乙", "丙"}) {
		t.Fatalf("options=%q，期望拆成三项", p.Options)
	}
	// 不带分隔符的单个选项名不许被拆
	p = mustParse(t, `{"input_params":[{"options":"只此一项"}]}`)
	if !eqList(p.Options, []string{"只此一项"}) {
		t.Fatalf("options=%q，期望原样一项", p.Options)
	}
}

// 对象映射选项：键是给人看的文案，排序保证两次训练结果一致。
func TestParamOptionsMapUsesSortedKeys(t *testing.T) {
	p := mustParse(t, `{"input_params":[{"options":{"是":"yes","否":"no"}}]}`)
	if p.Options == nil {
		t.Fatal("options 是对象映射时被整条丢掉 —— 下拉会渲染成空")
	}
	if p.Options[0] > p.Options[1] {
		t.Fatalf("options=%q 未排序，两次训练的字段顺序会漂", p.Options)
	}
	if len(p.Options) != 2 {
		t.Fatalf("options=%q，期望 2 项", p.Options)
	}
}

// 数字/布尔/对象落到字符串字段上：还原成人看得懂的字，不许崩。
func TestParamScalarCoercion(t *testing.T) {
	p := mustParse(t, `{"input_params":[{
		"name":"3","label":3.0,"type":true,"placeholder":{"text":"请输入"},
		"help":["甲","乙"],"default":2026,"min":"1","max":5,"required":"是"}]}`)
	if p.Name != "3" || p.Label != "3" || p.Type != "true" {
		t.Fatalf("标量还原不对：name=%q label=%q type=%q", p.Name, p.Label, p.Type)
	}
	if p.Placeholder != "请输入" {
		t.Fatalf("对象形状的 placeholder=%q，期望取 text", p.Placeholder)
	}
	if p.Help != "甲、乙" {
		t.Fatalf("数组形状的 help=%q，期望「甲、乙」", p.Help)
	}
	if p.Default != "2026" || p.Min != 1 || p.Max != 5 || !p.Required {
		t.Fatalf("数字/字符串布尔还原不对：%+v", p)
	}
}

// 语法错误必须照旧报错 —— 宽容层只宽容「形状」，不宽容「非法 JSON」。
// 否则模型把 JSON 写坏了也静默放行，排障时两眼一抹黑。
//
// **如实标注**：这条的红是 Go 的解码器兜的（对象没闭合时顶层 decode 先炸），
// 不依赖本包的代码，因此它**不参与注入自证** —— 拿它当自证目标只会得到一条
// 永远抓不住东西的假自证。它留在这里的作用是拦住「以后有人把宽容做成全吞」。

func TestParamStillRejectsBrokenJSON(t *testing.T) {
	var s struct {
		InputParams []Param `json:"input_params"`
	}
	err := json.Unmarshal([]byte(`{"input_params":[{"name":`), &s)
	if err == nil {
		t.Fatal("非法 JSON 被静默放行了 —— 宽容层越界到「藏错」")
	}
	if !strings.Contains(err.Error(), "unexpected end of JSON input") {
		t.Fatalf("报错内容变了，排障会认不出：%v", err)
	}
}

// 正常形状必须逐字节等于原语义（宽容层不许把好数据改坏）。
func TestParamNormalShapeUnchanged(t *testing.T) {
	p := mustParse(t, `{"input_params":[{
		"name":"title","label":"标题","type":"text","required":true,
		"placeholder":"请输入标题","help":"不少于5字","default":"通知","min":1,"max":200}]}`)
	want := Param{Name: "title", Label: "标题", Type: "text", Required: true,
		Placeholder: "请输入标题", Help: "不少于5字", Default: "通知", Min: 1, Max: 200}
	if p.Name != want.Name || p.Label != want.Label || p.Type != want.Type ||
		p.Required != want.Required || p.Placeholder != want.Placeholder ||
		p.Help != want.Help || p.Default != want.Default || p.Min != want.Min ||
		p.Max != want.Max || p.Options != nil {
		t.Fatalf("正常字段被改动：got=%+v want=%+v", p, want)
	}
	// 回写（训练产物要落库/落文件）不许丢字段
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("回写失败：%v", err)
	}
	for _, k := range []string{`"name":"title"`, `"label":"标题"`, `"required":true`} {
		if !strings.Contains(string(out), k) {
			t.Fatalf("回写丢了 %s：%s", k, out)
		}
	}
}

// 对象里没有 label/text/... 这些已知键时，退化成紧凑 JSON，**不许变成空串**。
// 变成空串 → cleanList 丢掉它 → 下拉少一项，用户根本不知道有这个选项。
// 这条是承重的（注入见自证脚本第 3 条）。
func TestParamObjectWithoutLabelKeepsData(t *testing.T) {
	p := mustParse(t, `{"input_params":[{"options":[{"档位":"A","备注":"x"}]}]}`)
	if len(p.Options) != 1 {
		t.Fatalf("options=%q，期望保留 1 项（丢数据=下拉少一项）", p.Options)
	}
	if !strings.Contains(p.Options[0], "档位") {
		t.Fatalf("options[0]=%q 里看不到原来的键，数据被吃掉了", p.Options[0])
	}
}
