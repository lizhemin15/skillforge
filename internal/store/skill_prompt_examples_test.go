package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// SystemPrompt 必须把**子目录里的范文**也拼进来。
//
// 线上症状（用户报「幻觉严重，表格没提取出来」）：流水线建的技能一律把范文放在
// examples/<分类>/NN.md，而 SystemPrompt 只扫 examples/ 顶层的 .md —— 于是一份范文
// 都拼不进来。上下文材料（报销标准表、时限表、审批节点）恰恰只存在范文里，
// system_prompt 里只有「照着表格答」这句指令却没有表。模型于是拿常识填数字：
// 实测把「经理住宿 600」答成 500、「总监 800」答成 600，还编出原文没有的
// 「火车软卧」。**抽取层把表格修好之后这些断言才现形** —— 表格进不了模型，
// 再怎么修解析都白搭。
//
// 守四件事：
//  1. examples/<分类>/*.md 必须进提示词，且内容逐字节在（表格的竖线/数字不能被丢）
//  2. 老形态 examples/*.md 仍然进，且分隔线文字与顺序不变（不能把已有技能的
//     prompt 文本整体挪位 —— 那会让所有老技能的输出风格漂移）
//  3. 两种形态混存时，顶层在前、子目录在后，各自按名字排序（输出稳定，可 diff）
//  4. 子目录里非 .md 文件、以及**更深一层**的目录不扫（避免把用户塞的杂文件带进来）

func TestSystemPromptIncludesCategorizedExamples(t *testing.T) {
	s := newStoreForTest(t, t.TempDir())
	dir := filepath.Join(s.SkillsDir(), "差旅报销")
	mustWrite(t, filepath.Join(dir, "system_prompt.md"), "你是差旅报销咨询助手")
	table := "| 人员级别 | 住宿标准 |\n| --- | --- |\n| 经理 | 600 |"
	mustWrite(t, filepath.Join(dir, "examples", "biaozhun", "01.md"), table)

	got, err := s.SystemPrompt("差旅报销")
	if err != nil {
		t.Fatalf("读提示词失败：%v", err)
	}
	if !strings.Contains(got, "你是差旅报销咨询助手") {
		t.Fatalf("system_prompt.md 本体丢了：%q", got)
	}
	if !strings.Contains(got, "| 经理 | 600 |") {
		t.Fatalf("子目录范文没拼进提示词 —— 表格数据进不了模型，只能靠编：\n%s", got)
	}
	if !strings.Contains(got, "biaozhun/01") {
		t.Errorf("分隔线应标出分类，便于排障时定位是哪份范文：\n%s", got)
	}
}

func TestSystemPromptKeepsFlatExamplesUnchanged(t *testing.T) {
	s := newStoreForTest(t, t.TempDir())
	dir := filepath.Join(s.SkillsDir(), "老技能")
	mustWrite(t, filepath.Join(dir, "system_prompt.md"), "PROMPT")
	mustWrite(t, filepath.Join(dir, "examples", "01.md"), "范文一")
	mustWrite(t, filepath.Join(dir, "examples", "02.md"), "范文二")

	got, err := s.SystemPrompt("老技能")
	if err != nil {
		t.Fatalf("读提示词失败：%v", err)
	}
	// 与改动前的输出逐字节一致（老技能不能被这次修复改了口径）
	want := "PROMPT" +
		"\n\n--- 示例范文01 ---\n范文一" +
		"\n\n--- 示例范文02 ---\n范文二"
	if got != want {
		t.Fatalf("老形态技能的提示词发生了变化（会导致所有老技能输出风格漂移）\n got=%q\nwant=%q", got, want)
	}
}

func TestSystemPromptOrdersFlatThenCategorized(t *testing.T) {
	s := newStoreForTest(t, t.TempDir())
	dir := filepath.Join(s.SkillsDir(), "混存")
	mustWrite(t, filepath.Join(dir, "system_prompt.md"), "P")
	mustWrite(t, filepath.Join(dir, "examples", "09-flat.md"), "FLAT9")
	mustWrite(t, filepath.Join(dir, "examples", "01-flat.md"), "FLAT1")
	mustWrite(t, filepath.Join(dir, "examples", "zeta", "02.md"), "Z2")
	mustWrite(t, filepath.Join(dir, "examples", "alpha", "01.md"), "A1")
	// 噪声：非 md、以及更深一层（都不该被扫进来）
	mustWrite(t, filepath.Join(dir, "examples", "alpha", "notes.txt"), "NOISE_TXT")
	mustWrite(t, filepath.Join(dir, "examples", "alpha", "deep", "x.md"), "NOISE_DEEP")

	got, err := s.SystemPrompt("混存")
	if err != nil {
		t.Fatalf("读提示词失败：%v", err)
	}
	iFlat1 := strings.Index(got, "FLAT1")
	iFlat9 := strings.Index(got, "FLAT9")
	iAlpha := strings.Index(got, "A1")
	iZeta := strings.Index(got, "Z2")
	if iFlat1 < 0 || iFlat9 < 0 || iAlpha < 0 || iZeta < 0 {
		t.Fatalf("有范文没被拼进来：\n%s", got)
	}
	if !(iFlat1 < iFlat9) {
		t.Errorf("顶层范文应按名字排序：FLAT1 在 FLAT9 之前")
	}
	if !(iFlat9 < iAlpha && iAlpha < iZeta) {
		t.Errorf("顺序应为 顶层(async) → 子目录(按名字)：flat9=%d alpha=%d zeta=%d", iFlat9, iAlpha, iZeta)
	}
	if strings.Contains(got, "NOISE_TXT") {
		t.Errorf("子目录里的非 .md 文件不该被拼进提示词")
	}
	if strings.Contains(got, "NOISE_DEEP") {
		t.Errorf("更深一层的目录不该被扫（只扫 examples/<分类> 这一层）")
	}
}

// 没有 examples/ 目录（手建技能很常见）时不能报错，只返回 system_prompt 本体。
func TestSystemPromptWithoutExamplesDir(t *testing.T) {
	s := newStoreForTest(t, t.TempDir())
	dir := filepath.Join(s.SkillsDir(), "无范文")
	mustWrite(t, filepath.Join(dir, "system_prompt.md"), "只有提示词")
	got, err := s.SystemPrompt("无范文")
	if err != nil {
		t.Fatalf("没有 examples/ 不该报错：%v", err)
	}
	if got != "只有提示词" {
		t.Fatalf("got=%q", got)
	}
}
