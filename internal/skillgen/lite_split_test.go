package skillgen

import (
	"strings"
	"testing"
)

// 极简通道的两个真实线上坑，各自钉一条回归：
//
//  1. 浏览器 textarea 的 value 带 CRLF 时，分隔符正则 `(?m)^[ \t]*-{3,}[ \t]*$`
//     匹配不到 `---\r`（行尾是 \r，不是 \n），用户粘 3 篇范文只得到 1 篇，
//     界面上写的是「范文 1 篇」，他却明明分了篇 —— 这是静默的数据损失，必须钉住。
//  2. `【周报写作指南】` 这种带壳标题不脱壳，尾缀正则锚不到行尾的「指南」，
//     分类名/目录名会歪成「周报写作指南」，与 AI 精炼出的「周报写作」自相矛盾。
func TestSplitLiteExamples_CRLF(t *testing.T) {
	crlf := "第一篇正文第一段。\r\n第二段。\r\n---\r\n第二篇正文。\r\n---\r\n第三篇正文。"
	got := SplitLiteExamples(crlf)
	if len(got) != 3 {
		t.Fatalf("CRLF 分篇失败：想要 3 篇，得到 %d 篇 %#v", len(got), got)
	}
	for i, e := range got {
		if strings.ContainsAny(e, "\r") {
			t.Errorf("第 %d 篇仍残留 \\r：%q", i+1, e)
		}
	}
	if got[0] != "第一篇正文第一段。\n第二段。" {
		t.Errorf("第 1 篇内容被改动：%q", got[0])
	}
}

func TestSplitLiteExamples_LFAndEdgeCases(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"LF 分隔", "A\n---\nB", 2},
		{"长横线也算分隔", "A\n-----\nB", 2},
		{"分隔线两侧有空格", "A\n   ---   \nB", 2},
		{"单篇不分", "只有一篇，里面有段落。\n\n还有一段。", 1},
		{"三连横线在行内不算分隔", "版本号 a---b 不是分隔线", 1},
		{"空段被丢弃", "A\n---\n\n---\nB", 2},
		{"孤立 CR 也能分", "A\r---\rB", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SplitLiteExamples(c.in); len(got) != c.want {
				t.Fatalf("想要 %d 篇，得到 %d 篇 %#v", c.want, len(got), got)
			}
		})
	}
}

func TestCleanLiteTypeName_ShelledTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"【周报写作指南】", "周报"},
		{"周报写作指南", "周报"},
		{"《公司新闻通稿写作指南》", "公司新闻通稿"},
		{"会议纪要写作规范", "会议纪要"},
		{"关于公文写作的说明", "公文写作"}, // 分类名随后还会被削成「公文」
		{"周报（2026版）写作要点", "周报"},
	}
	for _, c := range cases {
		if got := cleanLiteTypeName(c.in); got != c.want {
			t.Errorf("cleanLiteTypeName(%q) = %q，想要 %q", c.in, got, c.want)
		}
	}
	// 削到只剩 1 个字时必须退回脱壳后的原名，不能给出「周」这种会误命中的类型名。
	if got := cleanLiteTypeName("【写作指南】"); len([]rune(got)) < 2 {
		t.Errorf("类型名被削得过短：%q", got)
	}
}

// 钉住转义：liteTrimRe 若被写成双反斜杠（raw string 里 \\s 是「字面反斜杠 + s」而非空白类），
// 字符类会提前闭合，标点/空白其实一个都没剔掉 —— 表面看代码「在清理」，实际没清。
func TestLiteTrimRe_ActuallyStrips(t *testing.T) {
	if got := cleanLiteTypeName("周报、写作 指南"); strings.ContainsAny(got, "、 ") {
		t.Errorf("标点/空白未被剔除，说明 liteTrimRe 字符类写坏了：%q", got)
	}
	if !liteSuffixRe.MatchString("周报写作指南") {
		t.Error("liteSuffixRe 匹配不上「周报写作指南」，说明 \\s* 被写成了字面反斜杠")
	}
}

func TestLiteNormalizeNewlines(t *testing.T) {
	got := liteNormalizeNewlines("a\r\nb\rc\nd")
	if got != "a\nb\nc\nd" {
		t.Fatalf("换行归一化错误：%q", got)
	}
	if got2 := liteNormalizeNewlines(got); got2 != got {
		t.Fatalf("不幂等：%q -> %q", got, got2)
	}
}
