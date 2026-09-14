package agent

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/docgen"
	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/model"
)

// ===== 「把上一轮的长文整理成 Word」确定性直通兜底的测试 =====
//
// 守的是用户抱怨过的那件事：先让 AI 写一篇新闻稿，再说「把上面那篇整理成 Word」，
// 结果它不管上一轮写了什么。这里用**假模型**（httptest 起一个 OpenAI 兼容端点）
// 把「模型跑偏」变成可复现的输入，然后断言用户最终拿到的文件里逐字含上一轮正文。
//
// 断言为什么落在「文件字节解析出来的文本」而不是 DocResult 的字段上：字段只能证明
// 引擎自己认为搬运成功，用户拿到的是 .docx。所以这里把 docx 解开、从
// word/document.xml 里把 <w:t> 的文字按顺序取出，再跟上一轮正文比——中间隔着的
// OOXML 打包、转义、段落切分全都被覆盖到。

// docFakeLLM 是只走非流式 Chat 的假模型：GenerateDoc 全程非流式（要 JSON 模式）。
type docFakeLLM struct {
	reply string
	srv   *httptest.Server
}

func newDocFakeLLM(t *testing.T, reply string) *docFakeLLM {
	t.Helper()
	f := &docFakeLLM{reply: reply}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "fake",
			"object": "chat.completion",
			"choices": []any{map[string]any{
				"index":   0,
				"message": map[string]string{"role": "assistant", "content": f.answer()},
			}},
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// answer 返回本次作答。答复在测试里是常量，留一个读取点是为了以后要按请求内容
// 换答复时不必改结构。
func (f *docFakeLLM) answer() string { return f.reply }

// engine 组装一个只连着假模型的引擎。store 传 nil：GenerateDoc 不走技能库
// （技能内容由测试直接给），也就不需要落盘目录。
func (f *docFakeLLM) engine(t *testing.T) *Engine {
	t.Helper()
	return New(llm.New(&model.LLMConfig{APIKey: "test-key", BaseURL: f.srv.URL, Model: "fake"}), nil)
}

// ---------------------------------------------------------------------------
// 夹具：上一轮的长文（合成内容，去空白后 500 字左右，远超 200 字门槛）
// ---------------------------------------------------------------------------

const artTitle = "关于在全市推广数据治理专项培训的通知"

// artParags 是上一轮正文的段落。刻意写到 500 字上下：一是过「长文」门槛，
// 二是够长才能从中间取一段 240 字的跨段窗口做逐字断言。
var artParags = []string{
	"各县区数据管理部门、市直有关单位：为进一步提升全市数据治理能力，经研究决定，自九月二十日起在全市范围内开展数据治理专项培训，现将有关事项通知如下。",
	"一、培训对象为各县区数据管理部门业务骨干、市直单位信息化负责人，每单位选派两人参加；培训内容涵盖数据标准编制、质量核查、共享交换与安全合规四个模块，共二十四学时。",
	"二、培训采取线上授课与线下实操相结合的方式，线上部分通过市政务服务网学习平台观看，线下实操安排在市政务中心三层第一会议室，参训人员须携带本单位数据资产清单。",
	"三、请各单位于九月十八日前将参训人员名单报市数据局综合处，逾期未报视为不参加；名单须写明姓名、职务、联系电话，加盖单位公章后扫描发送至邮箱sjj@example.gov.cn。",
	"特此通知。联系人：李工，电话：010-88886666。",
}

func artText() string {
	return artTitle + "\n" + strings.Join(artParags, "\n\n")
}

// artWithSpec 是「上一轮那篇产物」落盘时的真实形态：正文 + 尾部的规格标记。
// 规格 JSON 不是文章内容，兜底时必须只搬正文（否则 Word 里会出现一段 JSON）。
func artWithSpec() string {
	return artText() + "\n\n已生成规格:{\"format\":\"word\",\"filename\":\"数据治理培训通知.docx\"," +
		"\"title\":\"" + artTitle + "\",\"parags\":[\"各县区数据管理部门\"]}"
}

func docHistory(artifact string) []Message {
	return []Message{
		{Role: "user", Content: "帮我写一篇关于数据治理专项培训的通知"},
		{Role: "assistant", Content: artifact, SkillSlug: "通知公告", Kind: KindArtifact},
	}
}

// skill 返回一个最小技能内容：GenerateDoc 只用到 Name / SystemPrompt。
func docSkill() *SkillContent {
	return &SkillContent{Slug: "通知公告", Name: "公文写作助手", SystemPrompt: "按 DOCJSON 契约输出文档。"}
}

// ---------------------------------------------------------------------------
// 工具：从 docx 字节里取出全部可见文本 + 去空白比较
// ---------------------------------------------------------------------------

// docxPlainText 解开 .docx，把 word/document.xml 里所有 <w:t> 的文字按顺序拼起来
// （段落之间补换行）。取不到 document.xml 直接算测试失败——那份文件对用户毫无用处。
func docxPlainText(t *testing.T, data []byte) string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("生成的 docx 不是合法 zip：%v", err)
	}
	var raw []byte
	for _, f := range zr.File {
		if f.Name != "word/document.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("打开 word/document.xml 失败：%v", err)
		}
		raw, err = io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("读取 word/document.xml 失败：%v", err)
		}
	}
	if len(raw) == 0 {
		t.Fatalf("docx 里没有 word/document.xml")
	}
	dec := xml.NewDecoder(bytes.NewReader(raw))
	var sb strings.Builder
	insideText := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("解析 document.xml 失败：%v", err)
		}
		switch e := tok.(type) {
		case xml.StartElement:
			switch e.Name.Local {
			case "t":
				insideText = true
			case "p":
				sb.WriteString("\n")
			}
		case xml.EndElement:
			if e.Name.Local == "t" {
				insideText = false
			}
		case xml.CharData:
			if insideText {
				sb.WriteString(string(e))
			}
		}
	}
	return sb.String()
}

// plain 去空白。独立实现一份（不用生产代码里的 stripAllWS）：
// 断言与被测实现共用同一个工具函数时，工具本身写错会让测试一起绿。
func plain(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r', '\u3000', '\u00a0':
			return -1
		}
		return r
	}, s)
}

// mustCarryVerbatim 断言 got（用户拿到的文件文本）里**逐字连续**含有 want 的
// 一段 n 字窗口，窗口从 want 的中点往前取，因此必然横跨段落边界。
//
// 为什么是「连续窗口」而不是「关键词都在」：关键词都在只能证明两篇东西在讲同一
// 件事——那正是模型重写后的样子。连续 200+ 字一字不差地出现，才等价于「原文被
// 搬过来了」。
func mustCarryVerbatim(t *testing.T, got, want string, n int) {
	t.Helper()
	gp, wp := plain(got), plain(want)
	wr := []rune(wp)
	if len(wr) < n {
		t.Fatalf("上一轮正文太短（%d 字），无法截取 %d 字窗口", len(wr), n)
	}
	mid := len(wr) / 2
	start := mid - n/2
	if start < 0 {
		start = 0
	}
	seg := string(wr[start : start+n])
	if !strings.Contains(gp, seg) {
		t.Fatalf("文件正文里没有逐字出现上一轮的 %d 字连续片段。\n片段=%q\n文件正文(前 400 字)=%q",
			n, seg, firstRunes(gp, 400))
	}
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ---------------------------------------------------------------------------
// 一、模型跑偏 → 必须走直通，用户拿到的文件里是上一轮原文
// ---------------------------------------------------------------------------

func TestDocPassthroughKeepsPreviousArticleVerbatim(t *testing.T) {
	// 假模型给出的是一份**完全无关**的东西：把「整理成 Word」理解成「做一张要点表」，
	// 正文一个字都没搬。这就是线上真实的失效形态（用户抱怨的那种）。
	drift := `{"format":"excel","filename":"数据治理要点.xlsx","title":"数据治理工作要点",` +
		`"cols":["要点","说明"],` +
		`"rows":[["培训时间","九月二十日起"],["参训对象","各单位业务骨干"],["报名时限","九月十八日前"]],` +
		`"parags":["以上为本次培训的要点摘要。"]}`
	f := newDocFakeLLM(t, drift)
	e := f.engine(t)

	res, err := e.GenerateDoc(context.Background(), "s-pass", docSkill(), nil,
		"把上面那篇整理成 word", docHistory(artWithSpec()))
	if err != nil {
		t.Fatalf("GenerateDoc 失败：%v", err)
	}

	// 1) 走了直通路径
	if !res.Passthrough {
		t.Fatalf("模型返回的规格与上一轮正文无关（覆盖率应远低于 60%%），必须走确定性直通，但 Passthrough=false（Coverage=%.2f）", res.Coverage)
	}
	if res.Coverage >= passthroughCoverageMin {
		t.Fatalf("覆盖率应低于阈值 %.2f，实际 %.2f", passthroughCoverageMin, res.Coverage)
	}

	// 2) 文件里逐字含上一轮正文的 240 字连续片段（跨段落）
	body := docxPlainText(t, res.Bytes)
	mustCarryVerbatim(t, body, artText(), 240)

	// 3) 头尾都在：末段的落款与电话不能被砍掉（历史 bug 是只保头 400 字）
	if !strings.Contains(plain(body), "010-88886666") {
		t.Fatalf("文件正文末尾的联系方式丢失（尾部被截断）:\n%q", firstRunes(plain(body), 400))
	}

	// 4) 模型给的具体内容被丢弃，不是「混着来」
	for _, driftMark := range []string{"以上为本次培训的要点摘要", "培训时间", "参训对象", "报名时限"} {
		if strings.Contains(plain(body), driftMark) {
			t.Fatalf("模型跑偏的内容 %q 仍然出现在文件里：%q", driftMark, firstRunes(plain(body), 400))
		}
	}

	// 5) 格式被强制成 word（模型说的是 excel）
	if res.ContentType != "application/vnd.openxmlformats-officedocument.wordprocessingml.document" {
		t.Fatalf("直通必须用 Word 渲染，实际 ContentType=%q", res.ContentType)
	}
	if !strings.HasSuffix(res.Filename, ".docx") {
		t.Fatalf("直通文件名应以 .docx 结尾，实际 %q", res.Filename)
	}

	// 6) 用户看得见：说明进了既有 Summary 通道（SSE 里已经下发的那段文本）
	if !strings.Contains(res.Summary, "确定性直通渲染") || !strings.Contains(res.Summary, "覆盖率") {
		t.Fatalf("兜底说明没有出现在用户可见文案里：%q", res.Summary)
	}

	// 7) 产物消息尾部的规格 JSON 不能被当成正文排版
	if strings.Contains(body, "已生成规格") || strings.Contains(body, `"parags"`) {
		t.Fatalf("规格 JSON 被当成正文写进了文件：%q", firstRunes(plain(body), 400))
	}
}

// ---------------------------------------------------------------------------
// 二、模型正确搬运 → 必须用模型版本，不走直通（正常路径不受影响）
// ---------------------------------------------------------------------------

func TestDocPassthroughNotTriggeredWhenModelCarriesCorrectly(t *testing.T) {
	// 忠实搬运：把上一轮正文原样放进 parags，只把文件名标成「-word 版」。
	spec := map[string]any{
		"format":   "word",
		"filename": "数据治理培训通知-word.docx",
		"title":    artTitle,
		"parags":   artParags,
	}
	b, _ := json.Marshal(spec)
	f := newDocFakeLLM(t, string(b))
	e := f.engine(t)

	res, err := e.GenerateDoc(context.Background(), "s-ok", docSkill(), nil,
		"把上面那篇整理成 word", docHistory(artWithSpec()))
	if err != nil {
		t.Fatalf("GenerateDoc 失败：%v", err)
	}

	if res.Passthrough {
		t.Fatalf("模型已经原样搬运（Coverage=%.2f），不应夺走它的输出", res.Coverage)
	}
	if res.Coverage < 0.9 {
		t.Fatalf("忠实搬运的覆盖率应 >= 0.9，实际 %.2f（覆盖率算错会把正常路径也兜底掉）", res.Coverage)
	}
	if strings.Contains(res.Summary, "直通") {
		t.Fatalf("正常路径不该出现兜底说明：%q", res.Summary)
	}
	// 用模型版本的可判据：文件名是模型给的那个（直通会把它改写成别的名字/别的后缀）
	if res.Filename != "数据治理培训通知-word.docx" {
		t.Fatalf("应使用模型给出的文件名，实际 %q", res.Filename)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(res.Spec), &got); err != nil {
		t.Fatalf("Spec 不是合法 JSON：%v", err)
	}
	if got["filename"] != "数据治理培训通知-word.docx" {
		t.Fatalf("Spec 应保留模型版本，实际 filename=%v", got["filename"])
	}
	// 模型版本同样带着正文（这里只是确认文件可用，不重复验证搬运语义）
	mustCarryVerbatim(t, docxPlainText(t, res.Bytes), artText(), 240)
}

// ---------------------------------------------------------------------------
// 三、上一轮只是短文本 → 不触发直通
// ---------------------------------------------------------------------------

func TestDocPassthroughNotTriggeredByShortArtifact(t *testing.T) {
	short := "好的，收到，今天下午给你答复。" // 去空白后 15 字，远低于 200 字门槛
	t.Logf("上一轮产物去空白后 %d 字", len([]rune(plain(short))))
	drift := `{"format":"word","filename":"回复草稿.docx","title":"回复草稿",` +
		`"parags":["今天下午给对方一个书面答复。"]}`
	f := newDocFakeLLM(t, drift)
	e := f.engine(t)

	res, err := e.GenerateDoc(context.Background(), "s-short", docSkill(), nil,
		"把上面那段整理成 word", docHistory(short))
	if err != nil {
		t.Fatalf("GenerateDoc 失败：%v", err)
	}
	if res.Passthrough {
		t.Fatalf("上一轮产物只有 %d 字（< %d），不该触发直通（Coverage=%.2f）",
			len([]rune(plain(short))), passthroughMinArtifactRunes, res.Coverage)
	}
	if res.Coverage != 0 {
		t.Fatalf("短产物没有可比对象，覆盖率应记 0，实际 %.2f", res.Coverage)
	}
	if res.Filename != "回复草稿.docx" {
		t.Fatalf("应使用模型版本，实际 %q", res.Filename)
	}
	if !strings.Contains(docxPlainText(t, res.Bytes), "今天下午给对方一个书面答复") {
		t.Fatalf("短产物的场景下模型内容应被采用")
	}
}

// ---------------------------------------------------------------------------
// 四、长文但在开新话题 → 不触发（覆盖率低也不行）
// ---------------------------------------------------------------------------

func TestDocPassthroughNotTriggeredOnNewTopic(t *testing.T) {
	drift := `{"format":"excel","filename":"员工信息表.xlsx","title":"员工信息表",` +
		`"cols":["姓名","部门"],"rows":[["张三","综合处"]]}`
	f := newDocFakeLLM(t, drift)
	e := f.engine(t)

	res, err := e.GenerateDoc(context.Background(), "s-newtopic", docSkill(), nil,
		"帮我做一份员工信息表 excel，要有姓名和部门两列", docHistory(artWithSpec()))
	if err != nil {
		t.Fatalf("GenerateDoc 失败：%v", err)
	}
	if res.Passthrough {
		t.Fatalf("本轮是新话题（不是搬运意图），即使覆盖率低也不能兜底（Coverage=%.2f）", res.Coverage)
	}
	if res.Coverage >= passthroughCoverageMin {
		t.Fatalf("本例就是低覆盖率的场景（%v），用它来证明是「意图」这道闸门挡住兜底", res.Coverage)
	}
	if res.Filename != "员工信息表.xlsx" {
		t.Fatalf("新话题必须原样采用模型版本，实际 %q", res.Filename)
	}
}

// ---------------------------------------------------------------------------
// 五、覆盖率指标本身：抗短串巧合，且对真搬运给满分
// ---------------------------------------------------------------------------

func TestDocCoverageMetric(t *testing.T) {
	art := plain(artText())

	// 同一份内容、空白不同 → 满分（排版差异不该被当成内容改动）
	reflowed := strings.ReplaceAll(artText(), "\n\n", "\n \u3000\n")
	if got := docCoverage(artText(), reflowed); got != 1 {
		t.Fatalf("换行/空格变化不影响内容，覆盖率应为 1，实际 %.2f", got)
	}

	// 直通渲染出来的 doc → 满分（自洽：兜底产物绝不会再被判成跑偏）
	pt := buildPassthroughDoc(artText(), passthroughFormatProbe{Format: "excel", Filename: "数据治理培训通知.xlsx"}.doc())
	if got := docCoverage(artText(), docTextOf(pt)); got != 1 {
		t.Fatalf("直通渲染的文本应 100%% 覆盖上一轮正文，实际 %.2f", got)
	}
	// 顺带钉住「格式强制 word」：模型说的是 excel，直通后必须是 word。
	if pt.Format != "word" {
		t.Fatalf("直通必须强制 word 格式，实际 %q", pt.Format)
	}

	// 同主题的「摘要版」：单字重合度很高，但没有 32 字连续片段
	summary := "为进一步提升全市数据治理能力，我市将于九月二十日起开展专项培训，内容包括数据标准、质量核查、共享交换与安全合规四个模块，" +
		"采取线上线下相结合的方式，请各单位按时报送参训名单，未尽事宜另行通知。"
	if got := docCoverage(artText(), summary); got >= passthroughCoverageMin {
		t.Fatalf("同主题但换过措辞的摘要版不应被判成搬运，实际覆盖率 %.2f", got)
	}

	// **指标选型的自证**：拿一份「同样这些字、只把顺序打乱」的文本——逐字符集合
	// 覆盖率必然是满分，若指标用的是单字命中率，它就会被误判成「搬运」。
	reordered := scrambleChunks(art, 18)
	charOverlap := charCoverage(art, reordered)
	if charOverlap < 0.99 {
		t.Fatalf("用例前提不成立：重排稿的单字重合度只有 %.2f，换不出「逐字符指标会误判」的对比", charOverlap)
	}
	if got := docCoverage(artText(), reordered); got != 0 {
		t.Fatalf("同字重排稿没有任何 32 字连续片段，覆盖率应为 0，实际 %.2f", got)
	}
	t.Logf("自证：逐字符命中率 %.2f（>= 0.6，会被误判成搬运），定长切片覆盖率 %.2f（=0，判为跑偏）",
		charOverlap, docCoverage(artText(), reordered))

	// 完全无关 → 0
	if got := docCoverage(artText(), "员工信息表：姓名、部门、入职日期。"); got != 0 {
		t.Fatalf("无关文档覆盖率应为 0，实际 %.2f", got)
	}
	// 空上一轮 → 满分（没有可比对象，不算跑偏）
	if got := docCoverage("", "任意内容"); got != 1 {
		t.Fatalf("空上一轮产物应返回 1，实际 %.2f", got)
	}
}

// charCoverage 是本测试自用的「朴素逐字符集合覆盖率」，只用来证明
// docCoverage 选的连续片段口径确实更强（不是恒真断言的一部分）。
func charCoverage(prev, other string) float64 {
	o := map[rune]bool{}
	for _, r := range other {
		o[r] = true
	}
	pr := []rune(prev)
	if len(pr) == 0 {
		return 1
	}
	hit := 0
	for _, r := range pr {
		if o[r] {
			hit++
		}
	}
	return float64(hit) / float64(len(pr))
}

// scrambleChunks 把文本按 size 字一块切开，再把块按「奇数块在前、偶数块在后」的
// 顺序重新拼接：用的还是原来那些字（逐字符覆盖率 1.0），但**没有任何一对相邻块**
// 保持原来的前后关系，因此不存在 32 字连续片段——这是「重写」在字符层面的极端
// 形态，用来证明指标不是单字命中率（单字命中率会给它 100 分）。
//
// 只用「块序旋转」是不够的：旋转只破坏一处衔接，其余相邻块原样保留，覆盖率照样
// 能到 0.9——第一版就踩了这个坑。
func scrambleChunks(s string, size int) string {
	r := []rune(s)
	var chunks []string
	for i := 0; i < len(r); i += size {
		end := i + size
		if end > len(r) {
			end = len(r)
		}
		chunks = append(chunks, string(r[i:end]))
	}
	if len(chunks) <= 2 {
		return s
	}
	out := make([]string, 0, len(chunks))
	for i := 1; i < len(chunks); i += 2 {
		out = append(out, chunks[i])
	}
	for i := 0; i < len(chunks); i += 2 {
		out = append(out, chunks[i])
	}
	return strings.Join(out, "")
}

// passthroughFormatProbe 是「模型说要 excel」的探针，用来验证直通会把格式强制成 word。
type passthroughFormatProbe struct {
	Format   string
	Filename string
}

func (p passthroughFormatProbe) doc() docgen.Doc {
	return docgen.Doc{Format: p.Format, Filename: p.Filename, Title: "别的标题", Cols: []string{"a"}}
}

// ---------------------------------------------------------------------------
// 六、意图判定与段落切分
// ---------------------------------------------------------------------------

func TestWantsCarryOver(t *testing.T) {
	yes := []string{
		"把上面那篇整理成 word",
		"把这篇文章整理成 Word 文档",
		"导出成 pdf",
		"转成word",
		"帮我把它变成 word 文档",
		"保持原样，导出一份 word",
		"不要重写，直接给我 word 版",
		"把上面的稿子另存为 docx",
	}
	no := []string{
		"帮我写一篇关于数据治理的新闻稿",
		"帮我做一份员工信息表 excel，要有姓名和部门两列",
		"把单价改成 8000",
		"给这篇稿子润色一下，扩写到八百字",
		"",
	}
	for _, s := range yes {
		if !wantsCarryOver(s) {
			t.Errorf("应判为搬运意图：%q", s)
		}
	}
	for _, s := range no {
		if wantsCarryOver(s) {
			t.Errorf("不应判为搬运意图（这会夺走模型的正常输出）：%q", s)
		}
	}
}

func TestSplitArtifactLines(t *testing.T) {
	title, paras := splitArtifactLines(artText())
	if title != artTitle {
		t.Fatalf("标题应取首个短行，实际 %q", title)
	}
	if len(paras) != len(artParags) {
		t.Fatalf("正文段落数应为 %d，实际 %d", len(artParags), len(paras))
	}
	for i, p := range artParags {
		if paras[i] != p {
			t.Fatalf("第 %d 段被改动：got %q want %q", i, paras[i], p)
		}
	}
	// 没有短行可用时不许硬造标题（否则整段正文会变成居中大标题）
	long := strings.Repeat("这是一段很长的正文，", 5)
	title, paras = splitArtifactLines(long)
	if title != "" || len(paras) != 1 || paras[0] != long {
		t.Fatalf("全为长行时不应造标题：title=%q paras=%d", title, len(paras))
	}
}

// ---------------------------------------------------------------------------
// 七、产物消息尾部的标记必须被切掉
// ---------------------------------------------------------------------------

func TestArtifactBodyTextDropsMachineMarkers(t *testing.T) {
	if got := artifactBodyText(artWithSpec()); got != artText() {
		t.Fatalf("应只取正文，实际尾部残留：%q", firstRunes(got, 120))
	}
	if got := artifactBodyText("已按您提供的信息填充《表格.xlsx》。\n已填值:{a:1}"); strings.Contains(got, "已填值") {
		t.Fatalf("已填值标记应被切掉：%q", got)
	}
	if got := artifactBodyText("就是一段普通正文。"); got != "就是一段普通正文。" {
		t.Fatalf("无标记时不应改动正文：%q", got)
	}
}
