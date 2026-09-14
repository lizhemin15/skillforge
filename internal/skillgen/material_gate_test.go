package skillgen

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 本文件锁死一条线上事故：用户在内网环境上传了一份**混合型 PDF**（前几页是扫描的，
// 后面几页是可选的），训练照跑、进度全绿，最后交付的技能「和我给的内容完全没有关系」。
//
// 根因不是某一步崩了，而是整条流水线对「素材到底有没有读到」一无所知：
//   - 逐页解析只要文本层非空就直取 → 真实扫描页一页都没送 OCR；
//   - 只要字符数非 0，上层就以为素材没问题 → 那 1129 个字符可能全是「第 N 页 + 水印」；
//   - 任何一步失败都只是 continue（打一行 ⚠️），没有门禁拦下这次训练。
//
// 所以这里测的不是「函数能跑」，而是「素材没读到正文时，训练必须中止」。

// ---------------------------------------------------------------------------
// 1. 结构化判据
// ---------------------------------------------------------------------------

// TestJudgeExtraction 是这批改动的判据核心：区分「有文本」和「有正文」。
// 注入会红的方式：把判据退回 chars > 0（事故原样）→ 第 2、3 组立刻失败。
func TestJudgeExtraction(t *testing.T) {
	cases := []struct {
		name   string
		stats  map[string]any
		chars  int
		want   bool // 是否判定为「等于没读到正文」
		pages  int
		reason string
	}{
		{
			name:   "整本页页空白",
			stats:  map[string]any{"pages": float64(10), "text_pages": float64(0), "ocr_pages": float64(0), "empty_pages": float64(10)},
			chars:  0,
			want:   true,
			pages:  10,
			reason: "所有页都没识别出文字，等于没有素材",
		},
		{
			name:  "混合PDF只拿到页码水印",
			stats: map[string]any{"pages": float64(50), "text_pages": float64(50), "ocr_pages": float64(0), "empty_pages": float64(0)},
			chars: 1129,
			want:  true,
			pages: 50,
			reason: "线上事故原样：50 页 / 1129 字符（平均每页 22 字），全是水印。字符数非 0 是最有欺骗性的一种。" +
				"注：这组数字把最初 20 字/页的阈值照红了（22 > 20），阈值因此定在 50",
		},
		{
			name:   "正常文本PDF",
			stats:  map[string]any{"pages": float64(50), "text_pages": float64(50), "ocr_pages": float64(0), "empty_pages": float64(0)},
			chars:  40000,
			want:   false,
			pages:  50,
			reason: "平均每页 800 字，是真正文",
		},
		{
			name:   "扫描件OCR成功",
			stats:  map[string]any{"pages": float64(20), "text_pages": float64(0), "ocr_pages": float64(20), "empty_pages": float64(0)},
			chars:  12000,
			want:   false,
			pages:  20,
			reason: "文本层为 0 但 OCR 出了正文，不能误杀",
		},
		{
			name:   "无逐页统计（docx等非分页格式）",
			stats:  nil,
			chars:  300,
			want:   false,
			pages:  0,
			reason: "拿不到事实就不猜——docx/txt 只要解析出文本就算可用",
		},
	}
	for _, c := range cases {
		got := judgeExtraction(c.stats, c.chars)
		if got.suspect != c.want {
			t.Fatalf("%s：suspect=%v，期望 %v（%s）", c.name, got.suspect, c.want, c.reason)
		}
		if got.pages != c.pages {
			t.Fatalf("%s：pages=%d，期望 %d", c.name, got.pages, c.pages)
		}
	}
}

// TestStatsIntAcceptsJSONNumbers：stats 经 JSON 解码是 float64，但单测/未来上游
// 可能直接塞 int 或 json.Number。收口函数对三种形态都要认得，否则判据会静默失效
// （读不到 pages → 页数算 0 → 可疑判定整体跳过 → 门禁形同没有）。
func TestStatsIntAcceptsJSONNumbers(t *testing.T) {
	cases := []struct {
		in   any
		want int
	}{
		{float64(50), 50},
		{int(7), 7},
		{nil, 0},
		{"50", 0}, // 字符串不做隐式转换：错的形态宁可算 0 也不要猜出个数字
	}
	for _, c := range cases {
		if got := statsInt(map[string]any{"pages": c.in}, "pages"); got != c.want {
			t.Fatalf("statsInt(%v) = %d，期望 %d", c.in, got, c.want)
		}
	}
	if got := statsInt(nil, "pages"); got != 0 {
		t.Fatalf("stats 为 nil 时 = %d，期望 0", got)
	}
}

// ---------------------------------------------------------------------------
// 2. 门禁本身
// ---------------------------------------------------------------------------

// TestMaterialGateBlocksUnreadableDocs：传了文档、一份都没解析出来 → 必须中止。
func TestMaterialGateBlocksUnreadableDocs(t *testing.T) {
	g := &Generator{}
	err := g.enforceMaterialGate(&materialReport{
		DocFiles: 2, OKFiles: 0,
		Failures: []string{"a.pdf: 解析服务超时", "b.pdf: 解析结果为空"},
	})
	if err == nil {
		t.Fatal("两份文档全部解析失败，门禁却放行了：训练会生成与素材无关的技能")
	}
	for _, want := range []string{"2 份", "解析服务超时", "解析结果为空"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("报错没提到 %q，用户无从排查：%v", want, err)
		}
	}
}

// TestMaterialGateBlocksWatermarkOnlyMaterial：本次用户事故的正主。
// 注入会红的方式：门禁退回「只看 OKFiles / Chars」→ 这里 Chars=1129 远高于 200 下限，
// 立刻放行，测试失败。
func TestMaterialGateBlocksWatermarkOnlyMaterial(t *testing.T) {
	g := &Generator{}
	err := g.enforceMaterialGate(&materialReport{
		DocFiles: 1, OKFiles: 1, SuspectFiles: 1, TotalPages: 50, Chars: 1129,
		Warnings: []string{"手册.pdf: 共 50 页只解析出 1129 字（平均每页 23 字），疑似只拿到了页码/水印而正文缺失"},
	})
	if err == nil {
		t.Fatal("整本只读出页码水印（50 页/1129 字），门禁却放行了：这正是「技能与素材无关」的成因")
	}
	// 报错必须自带「共几页 / 多少字 / 该怎么办」，否则用户只会重传同一个文件。
	for _, want := range []string{"50 页", "1129", "可搜索的文本 PDF"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("报错缺 %q，用户在界面上一头雾水：%v", want, err)
		}
	}
}

// TestMaterialGateOnlyFailsWhenAllDocsSuspect：只要有一份文档读出了正文，
// 就不能因为另一份可疑而中止——半份好素材也能训，宁可训得一般也不要误杀。
func TestMaterialGateOnlyFailsWhenAllDocsSuspect(t *testing.T) {
	g := &Generator{}
	err := g.enforceMaterialGate(&materialReport{
		DocFiles: 2, OKFiles: 2, SuspectFiles: 1, TotalPages: 60, Chars: 21000,
		Warnings: []string{"b.pdf: 有 10/10 页未识别出任何文字"},
	})
	if err != nil {
		t.Fatalf("两份里有一份可用，不该中止：%v", err)
	}
}

// TestMaterialGateBlocksTooLittleText：实在没料（文本素材加起来几句话）也要停。
func TestMaterialGateBlocksTooLittleText(t *testing.T) {
	g := &Generator{}
	if err := g.enforceMaterialGate(&materialReport{DocFiles: 1, OKFiles: 1, Chars: 30}); err == nil {
		t.Fatal("素材只有 30 字符，门禁却放行了")
	}
}

// TestMaterialGateAllowsPurePromptTraining：不传文档、纯靠需求描述训练技能是正当用法，
// 门禁不能把这条路一起堵死（否则「不传文件」这个正常入口会直接报错）。
func TestMaterialGateAllowsPurePromptTraining(t *testing.T) {
	g := &Generator{}
	if err := g.enforceMaterialGate(&materialReport{Chars: 0}); err != nil {
		t.Fatalf("没传文档时不该中止：%v", err)
	}
	if err := g.enforceMaterialGate(nil); err != nil {
		t.Fatalf("报告为 nil 时不该中止：%v", err)
	}
}

// TestMinMaterialCharsEnvOverride：阈值必须可调（不同客户素材形态差别很大），
// 写死数值迟早有人要改代码。非法值回退默认，不允许变成 0 把门禁关掉。
func TestMinMaterialCharsEnvOverride(t *testing.T) {
	t.Setenv("SKILLFORGE_MIN_MATERIAL_CHARS", "5000")
	g := &Generator{}
	if got := g.minMaterialChars(); got != 5000 {
		t.Fatalf("env 覆盖未生效：%d", got)
	}
	if err := g.enforceMaterialGate(&materialReport{DocFiles: 1, OKFiles: 1, Chars: 3000}); err == nil {
		t.Fatal("阈值被抬到 5000 后，3000 字符的素材应当被拦下")
	}
	t.Setenv("SKILLFORGE_MIN_MATERIAL_CHARS", "abc")
	if got := g.minMaterialChars(); got != 200 {
		t.Fatalf("非法值应回退默认 200，实际 %d", got)
	}
}

// ---------------------------------------------------------------------------
// 3. 端到端（假 ocrd）
// ---------------------------------------------------------------------------

// watermarkOCRStub 起一个假 ocrd：对任何上传都回答「这本 PDF 只读出水印」
// （文本非空、stats 显示 50 页全直取），复刻事故发生时的服务端行为。
func watermarkOCRStub(t *testing.T, text string, stats map[string]any, warning string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		parts := []string{fmt.Sprintf(`"text":%q`, text), `"ok":true`}
		if stats != nil {
			var sb strings.Builder
			first := true
			for k, v := range stats {
				if !first {
					sb.WriteByte(',')
				}
				first = false
				fmt.Fprintf(&sb, "%q:%v", k, v)
			}
			parts = append(parts, fmt.Sprintf(`"stats":{%s}`, sb.String()))
		}
		if warning != "" {
			parts = append(parts, fmt.Sprintf(`"warning":%q`, warning))
		}
		fmt.Fprintf(w, "{%s}", strings.Join(parts, ","))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// binaryPDFBytes 造一段「看起来是二进制 PDF」的内容：ingestFiles 靠 magic +
// NUL 字节判定需要送解析，纯 ASCII 会被当成文本直通（那就测不到这条路径了）。
func binaryPDFBytes() string {
	b := []byte("%PDF-1.4\n")
	for i := 0; i < 256; i++ {
		b = append(b, byte(i))
	}
	b = append(b, []byte("\n%%EOF")...)
	return string(b)
}

// TestIngestThenGateEndToEndWatermark：把整条链路串起来跑一次（ingestFiles → 门禁）。
// 这是本次用户反馈的最小复现：内网环境传了一份混合型 PDF，前几页扫描、后几页可选。
func TestIngestThenGateEndToEndWatermark(t *testing.T) {
	watermark := strings.Repeat("扫描全能王 第 N 页\n", 40) // ≈1129 字符的水印层
	srv := watermarkOCRStub(t, watermark,
		map[string]any{"pages": 50, "text_pages": 50, "ocr_pages": 0, "empty_pages": 0},
		"共 50 页只解析出 1129 字（平均每页 23 字），疑似只拿到了页码/水印而正文缺失")

	g := &Generator{}
	g.SetOCR(srv.URL)

	var steps []string
	in := &Input{Name: "test", Files: []*UploadedFile{
		{Filename: "手册.pdf", Content: binaryPDFBytes()},
	}}

	rep := g.ingestFiles(context.Background(), in, func(s string) { steps = append(steps, s) })
	if rep.DocFiles != 1 || rep.OKFiles != 1 {
		t.Fatalf("素材入库计数不对：%+v", rep)
	}
	if rep.SuspectFiles != 1 {
		t.Fatalf("应当判定为「有文本无正文」，实际 %+v", rep)
	}
	if err := g.enforceMaterialGate(rep); err == nil {
		t.Fatal("端到端：混合型 PDF 只读出 1129 字水印，门禁却放行了，又会生成一份与素材无关的技能")
	}
	// 进度流里必须出现「共 50 页」这类逐页事实，而不是只有一个字符数——
	// 用户在上传后就能看出自己的 PDF 没被读出来。
	joined := strings.Join(steps, "\n")
	if !strings.Contains(joined, "共 50 页") {
		t.Fatalf("进度流没有逐页统计，用户看不出素材有问题：\n%s", joined)
	}
}

// TestIngestThenGateEndToEndGood：同样的链路，素材正常时必须放行，且 uf.Content
// 被替换成解析文本（后续步骤喂给模型的是文本而不是二进制乱码）。
func TestIngestThenGateEndToEndGood(t *testing.T) {
	// 素材形态要贴近真实手册（每页几百字）：故意不压着阈值边界写，
	// 否则这条测试守的是「阈值恰好等于 64」而不是「正常素材能通过」。
	good := strings.Repeat("第一条 本手册适用于所有分公司，请遵照执行。\n", 600)
	srv := watermarkOCRStub(t, good, map[string]any{"pages": 50, "text_pages": 50, "ocr_pages": 0, "empty_pages": 0}, "")

	g := &Generator{}
	g.SetOCR(srv.URL)
	uf := &UploadedFile{Filename: "手册.pdf", Content: binaryPDFBytes()}
	in := &Input{Name: "test", Files: []*UploadedFile{uf}}

	rep := g.ingestFiles(context.Background(), in, func(string) {})
	if err := g.enforceMaterialGate(rep); err != nil {
		t.Fatalf("正常素材被误杀：%v（%+v）", err, rep)
	}
	// ocrExtract 会对文本做 TrimSpace（首尾空白不该进入证据链），断言按同一口径比。
	if uf.Content != strings.TrimSpace(good) {
		t.Fatalf("解析文本没有替换进 Content：后续步骤会拿到二进制乱码（len=%d）", len(uf.Content))
	}
	if !uf.Extracted || len(uf.Raw) == 0 {
		t.Fatal("原始字节应当留档在 Raw 里，Extracted 应当为 true")
	}
}
