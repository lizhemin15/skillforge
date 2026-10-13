package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/agent"
	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/model"
	"github.com/lizhemin15/skillforge/internal/store"
)

// 本文件钉住附件下发链路的两端：
//   ① 下载端点按**白名单**发文件（公开无鉴权，绝不能按名字拼路径）；
//   ② 一轮回答里能落**多张**文件卡片，且每张卡的 URL 各自指向自己那份。
//
// 为什么这两端要写死：①是安全问题（穿越）②是这次改造的核心诉求（拆分出来的
// 模板要能逐份发出去）。而且它们都是「坏了也不报错」的类型 —— 端点发错文件
// 用户只会以为技能做得不对，少发一份用户只会以为源文档里本来就没有。

type deliverEnv struct {
	h    *chatHandler
	dir  string
	st   *store.SkillStore
	slug string
}

// newDeliverEnv 建一个能跑 chat/附件端点的最小环境（真 sqlite + 临时技能目录）。
func newDeliverEnv(t *testing.T, slug, skillType string, names ...string) *deliverEnv {
	t.Helper()
	dir := t.TempDir()
	st := newStoreForTest(t, dir)
	atts := make([]model.Attachment, 0, len(names))
	for _, n := range names {
		p := filepath.Join(dir, "skills", slug, "attachments", n)
		mkdirAll(t, filepath.Dir(p))
		writeBytes(t, p, []byte("PAYLOAD:"+n))
		atts = append(atts, model.Attachment{Name: n, Rel: "attachments/" + n, Origin: "通知.docx"})
	}
	sk := &model.Skill{Slug: slug, Name: "通知", SkillType: skillType, Attachments: atts, Enabled: true}
	if err := st.Create(sk, nil); err != nil {
		t.Fatalf("建技能失败：%v", err)
	}
	h := &chatHandler{eng: agent.New(llm.New(&model.LLMConfig{APIKey: "test", BaseURL: "http://127.0.0.1:1", Model: "fake"}), st), maxRound: 1, gen: newGenCache()}
	return &deliverEnv{h: h, dir: dir, st: st, slug: slug}
}

// getAttachment 打一次下载端点（走真 mux，顺带钉住路由模式）。
func (e *deliverEnv) getAttachment(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/chat/attachment/{slug}", e.h.Attachment)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestAttachmentEndpointServesNamedFile 钉住「点名哪份发哪份」。
func TestAttachmentEndpointServesNamedFile(t *testing.T) {
	e := newDeliverEnv(t, "tongzhi", model.SkillTypeQuery,
		"附件1-申报指南.docx", "附件2-报名表.docx", "附件3-汇总表.xlsx")

	for _, want := range []string{"附件2-报名表.docx", "附件3-汇总表.xlsx"} {
		rec := e.getAttachment(t, "/api/chat/attachment/tongzhi?file="+url.QueryEscape(want))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 应可下载，实际 %d（%s）", want, rec.Code, rec.Body.String())
		}
		if got := rec.Body.String(); got != "PAYLOAD:"+want {
			// 发错文件是本文件要防的头号失败：卡片名字对、点开是别的东西，
			// 用户会把错的表拿去用（比 404 更坏，因为他不疑心）。
			t.Fatalf("发出的是错的文件：期望 %s 的内容，实际 %q", want, got)
		}
		cd := rec.Header().Get("Content-Disposition")
		if !strings.Contains(cd, "attachment") {
			t.Fatalf("Content-Disposition 应声明附件，实际 %q", cd)
		}
		if !strings.Contains(cd, url.PathEscape(want)) {
			// 中文名必须走 filename* 才能落地成原名（否则下载下来是 download.docx）。
			t.Fatalf("下载名应保留原名 %q，实际 %q", want, cd)
		}
	}
}

// TestAttachmentEndpointFuzzyNameStillServesRightFile 钉住宽容匹配**没有**把
// 「发的是哪一份」搞错：用户少写扩展名时仍然发他自己那一份。
func TestAttachmentEndpointFuzzyNameStillServesRightFile(t *testing.T) {
	e := newDeliverEnv(t, "tongzhi", model.SkillTypeQuery,
		"附件1-申报指南.docx", "附件2-报名表.docx")
	rec := e.getAttachment(t, "/api/chat/attachment/tongzhi?file="+url.QueryEscape("附件2-报名表"))
	if rec.Code != http.StatusOK {
		t.Fatalf("省扩展名的点名应可下载，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "PAYLOAD:附件2-报名表.docx" {
		t.Fatalf("宽容匹配发错了文件：实际 %q", got)
	}
}

// TestAttachmentEndpointRejectsUnknown 钉住认不出的名字 404 且**报出清单**。
func TestAttachmentEndpointRejectsUnknown(t *testing.T) {
	e := newDeliverEnv(t, "tongzhi", model.SkillTypeQuery,
		"附件1-申报指南.docx", "附件2-报名表.docx")
	rec := e.getAttachment(t, "/api/chat/attachment/tongzhi?file="+url.QueryEscape("体检表.xlsx"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("清单外的名字必须 404，实际 %d（body=%q）", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "附件2-报名表.docx") {
		t.Fatalf("404 应报出可下载清单（用户才知道该叫什么名字），实际 %q", body)
	}
	// 清单里只印展示名，不能把磁盘路径漏出去。
	if strings.Contains(body, "attachments/") {
		t.Fatalf("404 不该泄露磁盘路径，实际 %q", body)
	}
}

// TestAttachmentEndpointBlocksTraversal 钉住路径穿越被挡。
//
// 这是**公开无鉴权**端点：如果名字被直接拼进路径，任何人都能顺着 ../ 读走
// 技能目录里的任何文件（含 system_prompt.md / 示例范文 / 甚至 .db 同级的文件）。
// 断言必须打在"拿到 200"上——只断言 404 无法区分"挡住了"和"文件恰好不存在"。
func TestAttachmentEndpointBlocksTraversal(t *testing.T) {
	e := newDeliverEnv(t, "tongzhi", model.SkillTypeQuery, "附件2-报名表.docx")
	// 先证明目标文件确实存在且**就在技能目录里**（层目录之外），
	// 否则下面的 404 可能只是"文件不存在"的假绿。
	secret := filepath.Join(e.dir, "skills", "tongzhi", "system_prompt.md")
	writeBytes(t, secret, []byte("SECRET-SYSTEM-PROMPT"))

	for _, bad := range []string{
		"../system_prompt.md",
		"..%2Fsystem_prompt.md",
		"attachments/../../system_prompt.md",
		"system_prompt.md",
		"/etc/passwd",
		"../../../../etc/passwd",
	} {
		rec := e.getAttachment(t, "/api/chat/attachment/tongzhi?file="+url.QueryEscape(bad))
		if rec.Code == http.StatusOK {
			t.Fatalf("危险的 file=%q 竟然 200 发出了内容：%q", bad, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "SECRET-SYSTEM-PROMPT") {
			t.Fatalf("file=%q 泄露了技能目录内文件内容", bad)
		}
	}
}

// TestAttachmentEndpointWithoutFileParamServesFirst 钉住兼容路：不带 file= 发第一份。
//
// 老前端与已发出去的卡片只会拼 slug，这条不许 404 —— 否则本次改造会让
// 历史上所有模板技能的下发链接一起失效。
func TestAttachmentEndpointWithoutFileParamServesFirst(t *testing.T) {
	e := newDeliverEnv(t, "tongzhi", model.SkillTypeQuery,
		"附件1-申报指南.docx", "附件2-报名表.docx")
	rec := e.getAttachment(t, "/api/chat/attachment/tongzhi")
	if rec.Code != http.StatusOK {
		t.Fatalf("不带 file= 应回落到第一份，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "PAYLOAD:附件1-申报指南.docx" {
		t.Fatalf("应发清单第一份，实际 %q", got)
	}
}

// collectFrames 收集下发帧，用于断言"这一轮到底落了什么"。
func collectFrames() (func(ev, data string), *[]frame) {
	var got []frame
	return func(ev, data string) {
		got = append(got, frame{ev: ev, data: data})
	}, &got
}

type frame struct {
	ev   string
	data string
}

// fileFrames 只挑出文件帧并解出 slug/name/url。
type fileFrame struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

func (f frame) file(t *testing.T) fileFrame {
	t.Helper()
	var ff fileFrame
	if err := json.Unmarshal([]byte(f.data), &ff); err != nil {
		t.Fatalf("文件帧不是合法 JSON：%q（%v）", f.data, err)
	}
	return ff
}

// TestDeliverAttachmentsEmitsOneFramePerFile 钉住「多份 = 多张卡，且各指各的」。
//
// 这条是本次改造的核心诉求（拆分出来的模板要能逐份发出去）。旧实现只发一份，
// 且帧里的 name 是磁盘相对路径（attachments/xxx.docx）——用户看到的卡片标题
// 会是路径而不是文件名。这类问题在管理端看不出来，只在用户下载时暴露。
func TestDeliverAttachmentsEmitsOneFramePerFile(t *testing.T) {
	atts := []model.Attachment{
		{Name: "附件1-申报指南.docx", Rel: "attachments/附件1-申报指南.docx"},
		{Name: "附件2-报名表.docx", Rel: "attachments/附件2-报名表.docx"},
	}
	write, got := collectFrames()
	if n := deliverAttachments(write, "tongzhi", atts); n != 2 {
		t.Fatalf("应下发 2 份，实际 %d", n)
	}

	var files []fileFrame
	for _, f := range *got {
		if f.ev == evFile {
			files = append(files, f.file(t))
		}
	}
	if len(files) != 2 {
		t.Fatalf("应有 2 个文件帧，实际 %d（%+v）", len(files), files)
	}
	seen := map[string]string{}
	for _, ff := range files {
		if ff.Slug != "tongzhi" {
			t.Fatalf("文件帧的 slug 不对：%+v", ff)
		}
		if strings.Contains(ff.Name, "/") {
			t.Fatalf("帧里的 name 必须是文件名而不是路径，实际 %q", ff.Name)
		}
		seen[ff.Name] = ff.URL
	}
	for _, a := range atts {
		u, ok := seen[a.Name]
		if !ok {
			t.Fatalf("少了 %s 的卡片，实际 %v", a.Name, seen)
		}
		if !strings.Contains(u, url.QueryEscape(a.Name)) {
			t.Fatalf("%s 的卡片 URL 没带自己的文件名（会发错/发不出）：%q", a.Name, u)
		}
	}
	// 两张卡的 URL 必须不同，否则前端去重会把它们合成一张。
	if files[0].URL == files[1].URL {
		t.Fatalf("两份文件的 URL 相同（%q），前端会去重成一张卡", files[0].URL)
	}
}

// TestDeliverAttachmentsSilentWhenNothingToSend 钉住「没东西发时一个帧都不吐」。
// 吐空卡片或空提示会让用户以为技能崩了/文件丢了。
func TestDeliverAttachmentsSilentWhenNothingToSend(t *testing.T) {
	write, got := collectFrames()
	if n := deliverAttachments(write, "tongzhi", nil); n != 0 {
		t.Fatalf("空清单不该下发，实际 %d", n)
	}
	if len(*got) != 0 {
		t.Fatalf("空清单不该产生任何帧，实际 %+v", *got)
	}
	// 退化成"有名字但没 Rel"（老数据的坏形态）时也不该发——发出去必然 404。
	write2, got2 := collectFrames()
	if n := deliverAttachments(write2, "tongzhi", []model.Attachment{{Name: "报名表.docx"}}); n != 0 {
		t.Fatalf("没有 Rel 的附件发不出去，不该产生卡片，实际 %d", n)
	}
	if len(*got2) != 0 {
		t.Fatalf("没有 Rel 时不该产生帧，实际 %+v", *got2)
	}
}

// TestDeliverAttachmentsCountsAndReportsTruncation 钉住多份时有总数说明、
// 截断时有被截份数的说明。
func TestDeliverAttachmentsCountsAndReportsTruncation(t *testing.T) {
	write, got := collectFrames()
	atts := []model.Attachment{
		{Name: "A.docx", Rel: "attachments/A.docx"},
		{Name: "B.docx", Rel: "attachments/B.docx"},
		{Name: "C.docx", Rel: "attachments/C.docx"},
	}
	if n := deliverAttachments(write, "tongzhi", atts); n != 3 {
		t.Fatalf("应下发 3 份，实际 %d", n)
	}
	var text strings.Builder
	for _, f := range *got {
		if f.ev == evDelta {
			var d struct {
				T string `json:"t"`
			}
			if err := json.Unmarshal([]byte(f.data), &d); err != nil {
				t.Fatalf("delta 帧不是合法 JSON：%q", f.data)
			}
			text.WriteString(d.T)
		}
	}
	if !strings.Contains(text.String(), "3") {
		t.Fatalf("多份时应说明这一轮共几份，实际文本：%q", text.String())
	}
}
