package skillgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 合成手册：两章、各一篇范文。刻意不依赖 testdata/ 下那本真手册——
// 回归测试骑在真实素材上，换台机器（或素材清理后）就红。
const fidelityDoc = `第一章 经营业绩通稿

经营业绩类稿件要突出数据支撑，用同比、环比数字说话。

范文：某公司年度报告显示，营业收入同比增长百分之十二。

第二章 产品发布通稿

产品发布类稿件要交代产品定位、核心卖点与上市时间。

范文：某品牌发布新一代旗舰产品，主打轻量化与长续航。
`

// fidelityAnchors 返回两个分类的锚点。corruptSecond=true 时把第二类的 End
// 换成模型「改写引用」的走样版本：在原文「主打轻量化与长续航」里塞进原文
// 没有的「极致」。
//
// 造故障的教训（实测踩过）：最初用「截断 + 省略号」造故障，测试没红——
// 省略号是标点，被 levelNoPunct 归一化掉了，故障反而被分级定位救回来。
// 分级定位只处理**表层差异**（空白、标点、换行），改词属内容差异，
// 它救不回来，也正因此才是真故障。
func fidelityAnchors(corruptSecond bool) *Structure {
	second := CatAnchor{Start: "产品发布类稿件要交代", End: "主打轻量化与长续航。"}
	if corruptSecond {
		second.End = "主打极致轻量化与长续航。"
	}
	return &Structure{Categories: []Category{
		{Name: "经营业绩", Anchor: []CatAnchor{{Start: "经营业绩类稿件要突出", End: "同比增长百分之十二。"}}},
		{Name: "产品发布", Anchor: []CatAnchor{second}},
	}}
}

// writePack 走**真实切分路径**（SplitByAnchors）落盘，返回 fidelity.md 正文。
// 刻意不手写 Examples：手写会绕过「范文必须是原文连续子串」这个前提，
// 让保真核对的断言变成自说自话。
func writePack(t *testing.T, st *Structure) string {
	t.Helper()
	mp := &manualPack{
		Structure: st,
		Examples:  map[string][]string{},
		Paths:     map[string][]string{},
		Source:    fidelityDoc,
	}
	for _, c := range st.Categories {
		segs, err := SplitByAnchors(fidelityDoc, c.Anchor)
		if err != nil {
			mp.Warnings = append(mp.Warnings, "分类「"+c.Name+"」范文未切出："+err.Error())
			continue
		}
		mp.Examples[c.Name] = segs
	}
	dir := t.TempDir()
	if _, err := mp.WriteTo(dir); err != nil {
		t.Fatalf("WriteTo 失败: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "fidelity.md"))
	if err != nil {
		t.Fatalf("fidelity.md 未落盘: %v", err)
	}
	return string(b)
}

// 红侧：锚点走样 → 报告必须点名，且不得宣称无事。
func TestFidelityReportsSplitFailure(t *testing.T) {
	got := writePack(t, fidelityAnchors(true))
	for _, want := range []string{"范文覆盖：1/2 类", "⚠️ 待处理：1 类范文未切出", "产品发布"} {
		if !strings.Contains(got, want) {
			t.Errorf("fidelity.md 缺少 %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, "无待处理项") {
		t.Errorf("有失败分类却宣称无待处理项——这正是静默降级\n---\n%s", got)
	}
}

// 绿侧：全部命中 → 干净结论，且不得出现告警字样。
func TestFidelityCleanRun(t *testing.T) {
	got := writePack(t, fidelityAnchors(false))
	for _, want := range []string{"范文覆盖：2/2 类", "无待处理项", "保真核对：2/2 篇"} {
		if !strings.Contains(got, want) {
			t.Errorf("fidelity.md 缺少 %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, "⚠️") {
		t.Errorf("无失败分类却出现告警标记\n---\n%s", got)
	}
}

// 保真语义：能逐字在原文找到的才算过。
func TestFidelitySegmentsAreVerbatim(t *testing.T) {
	mp := &manualPack{
		Examples: map[string][]string{"a": {"第一章 经营业绩通稿", "这段不在原文里"}},
		Paths:    map[string][]string{},
		Source:   fidelityDoc,
	}
	found, total := mp.countFidelity()
	if total != 2 || found != 1 {
		t.Fatalf("countFidelity = (%d,%d)，期望 (1,2)", found, total)
	}
}

// Source 为空必须报「没核对过」(0,0)，不能谎报全过——后者会让报告失去可信度。
func TestFidelityWithoutSourceDoesNotClaimPass(t *testing.T) {
	mp := &manualPack{Examples: map[string][]string{"a": {"x"}}, Paths: map[string][]string{}}
	if found, total := mp.countFidelity(); found != 0 || total != 0 {
		t.Fatalf("无 Source 时 countFidelity = (%d,%d)，期望 (0,0)", found, total)
	}
}

// 且：Source 为空时报告里不应出现「保真核对」这一行（否则读者会以为核对过）。
func TestFidelityReportOmitsCheckWhenNoSource(t *testing.T) {
	mp := &manualPack{
		Structure: &Structure{Categories: []Category{{Name: "A"}}},
		Examples:  map[string][]string{"A": {"x"}},
		Paths:     map[string][]string{},
	}
	dir := t.TempDir()
	if _, err := mp.WriteTo(dir); err != nil {
		t.Fatalf("WriteTo 失败: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "fidelity.md"))
	if strings.Contains(string(b), "保真核对") {
		t.Errorf("无原文却报告保真核对\n---\n%s", b)
	}
}
