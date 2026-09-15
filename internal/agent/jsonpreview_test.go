package agent

import (
	"strings"
	"testing"
)

// feedAll 按固定片长喂进去，返回拼起来的预览文字。
func feedAll(p *jsonPreview, s string, chunk int) string {
	if chunk <= 0 {
		chunk = len(s)
	}
	var b strings.Builder
	for i := 0; i < len(s); i += chunk {
		end := i + chunk
		if end > len(s) {
			end = len(s)
		}
		b.WriteString(p.Feed(s[i:end]))
	}
	return b.String()
}

const docgenJSON = `{"format":"word","filename":"关于开展数据治理专项工作的通知.docx",` +
	`"title":"关于开展数据治理专项工作的通知",` +
	`"parags":["为深入贯彻公司数据治理工作部署，现将有关事项通知如下。","请各部门于每月底前报送工作进展。"]}`

// 预览要的正是「用户最终拿到的那份文档的文字」，且不含文件名、格式这类结构性字段。
func TestJsonPreviewDocgenSpec(t *testing.T) {
	got := feedAll(&jsonPreview{}, docgenJSON, 0)
	want := "关于开展数据治理专项工作的通知" +
		"为深入贯彻公司数据治理工作部署，现将有关事项通知如下。" +
		"请各部门于每月底前报送工作进展。"
	if got != want {
		t.Fatalf("预览不符\n got=%q\nwant=%q", got, want)
	}
	for _, noise := range []string{"word", ".docx", "parags", "format", "filename", "{", "[", ":"} {
		if strings.Contains(got, noise) {
			t.Errorf("预览里混进了结构性内容 %q：%q", noise, got)
		}
	}
}

// 流式解析器的正确性本质：**怎么切都对**。按字节切、按 3/7/64 字节切、整块进，
// 结果必须逐字相同 —— 真实 SSE 的切点由 provider 决定（实测 440 片 / 2350 字节，
// 平均 5 字节一片），任何依赖「片边界恰好落在结构字符上」的实现都是赌运气。
func TestJsonPreviewChunkInvariant(t *testing.T) {
	cases := []string{
		docgenJSON,
		`{"intent":"write","reason":"用户要求写新闻稿，命中公文写作技能","needs":[]}`,
		`{"rows":[["品目","数量"],["笔记本","3"]],"cols":["a","b"]}`,
		`{"fields":{"item1":"联想笔记本","item2":"3"},"ops":[]}`,
		`{"ops":[{"action":"add_para","text":"补充说明：本通知自发布之日起执行。"}]}`,
		`{"title":"带\n换行与\"引号\"的标题","parags":["段落里的反斜杠 \\ 与制表符	。"]}`,
		`{"title":"转义中文\u4e2d\u6587与表情\ud83d\ude00结束","parags":[]}`,
	}
	for i, c := range cases {
		whole := feedAll(&jsonPreview{}, c, 0)
		if whole == "" {
			t.Errorf("用例 %d 一片都没抽出来：%s", i, c)
		}
		for _, chunk := range []int{1, 2, 3, 7, 64} {
			if got := feedAll(&jsonPreview{}, c, chunk); got != whole {
				t.Errorf("用例 %d 片长 %d 结果不同\n chunked=%q\n   whole=%q", i, chunk, got, whole)
			}
		}
	}
}

// 白名单要真的挡住噪声字段，同时放行 should-have 的内容。
func TestJsonPreviewAllowlist(t *testing.T) {
	allow := []struct{ raw, want string }{
		{`{"title":"通知","parags":["正文"]}`, "通知正文"},
		{`{"reason":"判定为公文类","note":"补充"}`, "判定为公文类补充"},
		{`{"fields":{"item1":"笔记本"}}`, "笔记本"},
		{`{"values":["品目","数量"]}`, "品目数量"},
	}
	for _, c := range allow {
		if got := feedAll(&jsonPreview{}, c.raw, 1); got != c.want {
			t.Errorf("应放行却不同: raw=%s\n got=%q\nwant=%q", c.raw, got, c.want)
		}
	}
	block := []string{
		`{"format":"word","filename":"a.docx","action":"fill","cols":["h1","h2"],"confidence":"high"}`,
		`{"skill_slug":"office-doc","needs_tools":true,"steps":["a","b"]}`,
	}
	for _, raw := range block {
		if got := feedAll(&jsonPreview{}, raw, 1); got != "" {
			t.Errorf("应被挡住却有输出: raw=%s got=%q", raw, got)
		}
	}
}

// 残缺/畸形输入不能崩、也不能把后续正文一起吞掉 —— 输入是网络流，断在半路是常态。
func TestJsonPreviewMalformed(t *testing.T) {
	cases := []string{
		`{"title":"没写完的标`,                        // 字符串断在半路
		`{"title":"结尾是反斜杠\`,                      // 转义断在半路
		`{"title":"坏转义\u12`,                      // \u 位数不够
		`{"title":"坏转义\uZZZZ后面还有正文"}`,            // \u 跟的不是十六进制
		`{"title":"落单高位代理\ud83d后面继续"}`,           // 高位代理后面不是低位
		`{"title":"` + strings.Repeat("长", 5000), // 超长未收口
		`{"title":"孤零零的引号\"没转义","parags":["正文"]}`,
		`{`, `[]`, `"`, `\`, ``,
	}
	for i, raw := range cases {
		got := feedAll(&jsonPreview{}, raw, 1)
		_ = got // 只要求不 panic、不 OOM
		if i == len(cases)-1 && got != "" {
			t.Errorf("空输入应无输出，得到 %q", got)
		}
	}
	// 畸形 \u 之后，同一段里的正文必须还在（不能整段丢弃）。
	got := feedAll(&jsonPreview{}, `{"title":"坏转义\uZZZZ尾巴"}`, 1)
	if !strings.Contains(got, "尾巴") {
		t.Errorf("畸形转义把后续正文一起吞了：%q", got)
	}
}

// 已交出去的字符串不能被下一片 Feed 就地改写。
//
// strings.Builder.String() / 切片直接暴露底层数组是踩过的坑：攒够一片返回后，
// 下一次 Write 会覆盖同一块内存，调用方手里的「材料」会**在显示途中悄悄变样**。
// 这条断言就是那把尺子 —— 不比对内容，直接比对早先那次调用的返回值。
func TestJsonPreviewReturnedStringNotAliased(t *testing.T) {
	p := &jsonPreview{}
	first := p.Feed(`{"parags":["第一段完整内容`)

	// 同一实例继续吃大量数据，逼它复用底层数组。
	for i := 0; i < 200; i++ {
		p.Feed(strings.Repeat("后续内容XYZ", 50))
	}
	if first != `第一段完整内容` {
		t.Fatalf("早先返回的字符串被改写了：%q", first)
	}
}
