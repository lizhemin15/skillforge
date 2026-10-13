package skillgen

// 本文件钉住「查询 / 模板型技能」这条新链路的判据与边界。
//
// 起因（用户原话）：「给 skillforge 一键生成查询 skill，并能够给用户发送拆分出来的
// word 模板，通常是源文件中的附件。现在都是写作，不能满足我的需求。」
//
// 于是这里要守住四件事，每一件失败都会让用户拿到一个「看起来创建成功、实际拿不到
// 他要的东西」的技能——这种静默失效比报错难查得多：
//
//	1. 类型判定：素材里拆出附件且不是写作素材 → template（不是 write）
//	2. 原始字节的来源：解析服务没跑时也要能拆附件（否则模板型静默退化成查询型）
//	3. 路径归一：attachments/ 前缀与老技能的裸文件名都指向真实存在的文件
//	4. 重名去重：两份源文件各带同名附件时，落盘不能互相覆盖

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/model"
)

// liteNoticeDocx 造一份用户描述的主力素材：「制度文件正文 + 内嵌一份要发给别人的表格」。
// 内嵌件是真 docx（zip），走 word/embeddings/ 直嵌这一路（WPS 导出常见）。
func liteNoticeDocx(embedName, mainText string) []byte {
	payload := buildDocx(bigBody("报名表模板"))
	return buildZip([][2]string{
		{"[Content_Types].xml", `<?xml version="1.0"?><Types/>`},
		{"word/document.xml", `<?xml version="1.0"?><w:document><w:body><w:p>` + mainText + `</w:p></w:body></w:document>`},
		{"word/embeddings/" + embedName, string(payload)},
	})
}

// ===== 1. 类型判定矩阵 =====

func TestLiteDecideType_Matrix(t *testing.T) {
	longGuide := strings.Repeat("写清楚时间地点和联系人。", 10)
	longBody := strings.Repeat("这份材料说明办事要求。", 10) // > liteMinGuideChars

	cases := []struct {
		name     string
		attachN  int
		guide    string
		exN      int
		exChars  int
		bodyChar int
		want     string
	}{
		{"拆出附件且没有写作指南范文 → 模板型", 2, "", 0, 0, 0, model.SkillTypeTemplate},
		{"拆出附件但素材本身就是写作素材 → 仍是写作型", 1, longGuide, 2, 500, 500, model.SkillTypeWrite},
		{"没有附件、只有正文 → 查询型", 0, "", 0, 0, len([]rune(longBody)), model.SkillTypeQuery},
		{"有指南有范文 → 写作型", 0, longGuide, 2, 500, 500, model.SkillTypeWrite},
		{"什么都没有（纯兜底） → 写作型", 0, "", 0, 0, 0, model.SkillTypeWrite},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, why := liteDecideType(c.attachN, c.guide, c.exN, c.exChars, c.bodyChar)
			if got != c.want {
				t.Fatalf("类型 = %q，want %q（理由：%s）", got, c.want, why)
			}
			if strings.TrimSpace(why) == "" {
				t.Errorf("判定理由为空——用户看不到「为什么判成这个类型」，就没法判断该不该重训")
			}
			if c.attachN > 0 && !strings.Contains(why, "附件") {
				t.Errorf("理由 %q 没提附件；拆出附件是这次判定的关键事实，必须写在理由里", why)
			}
		})
	}
}

// 附件比「正文够长」优先：一份带附件的材料即使正文也很长，也不该被判成纯查询型
// （那会让用户拿不到文件）。这是本次需求的核心断言。
func TestLiteDecideType_AttachmentOutranksBodyLength(t *testing.T) {
	typ, _ := liteDecideType(1, "", 0, 0, 5000)
	if typ != model.SkillTypeTemplate {
		t.Fatalf("类型 = %q，want template：有附件时不能被更长的正文抢走类型", typ)
	}
}

// ===== 2. 面向用户的文案不能对非写作技能说谎 =====

func TestLiteTypeSurfaces_NonWritingDoesNotSayWrite(t *testing.T) {
	for _, typ := range []string{model.SkillTypeQuery, model.SkillTypeTemplate} {
		if got := liteTypeLabel(typ); got == "" || got == typ {
			t.Errorf("liteTypeLabel(%q) = %q，要有中文人话", typ, got)
		}
		trig := liteCategoryTrigger(typ, "公文办理")
		if strings.Contains(trig, "撰写") {
			t.Errorf("触发语 %q 含「撰写」：会把「这个手续怎么办」导向一个不会写文章的技能，"+
				"用户拿到答非所问的回复，而产物列表上只看得出多了一行字", trig)
		}
	}
	// 模板型的硬约束必须写明「文件怎么给」：管理员打开技能时看到的应是它凭什么答、
	// 给什么文件，而不是一片空白。
	req := liteRequirement(&liteMaterial{Type: model.SkillTypeTemplate, CatName: "公文办理"})
	if !strings.Contains(req, "文件") && !strings.Contains(req, "附件") {
		t.Errorf("模板型硬约束 %q 没提文件下发口径", req)
	}
	if !strings.Contains(req, "没写") && !strings.Contains(req, "不得编造") {
		t.Errorf("硬约束 %q 没写「材料没有就直说」——这条是答问类最要紧的约束，"+
			"写作型提示词里根本不存在", req)
	}
}

// ===== 3. 原始字节的来源：解析服务不可用时也要能拆附件 =====

func TestRawBytesOf_ContentFallback(t *testing.T) {
	docx := liteNoticeDocx("embed1.docx", "附件：1.报名表")

	t.Run("Raw 优先", func(t *testing.T) {
		uf := &UploadedFile{Filename: "a.docx", Content: "已被解析成文本", Raw: docx}
		if got := rawBytesOf(uf); len(got) != len(docx) {
			t.Errorf("取到 %d 字节，want %d（应优先用解析时留档的原件）", len(got), len(docx))
		}
	})
	t.Run("Raw 为空但 Content 还是二进制 → 当原始字节用", func(t *testing.T) {
		uf := &UploadedFile{Filename: "a.docx", Content: string(docx)}
		if got := rawBytesOf(uf); len(got) != len(docx) {
			t.Errorf("取到 %d 字节，want %d。解析服务没跑时取不到原件，附件拆不出来，"+
				"模板型技能会静默退化成查询型", len(got), len(docx))
		}
	})
	t.Run("Content 已是正文文本 → nil（别拿几万字正文喂 zip 解包器）", func(t *testing.T) {
		uf := &UploadedFile{Filename: "a.txt", Content: strings.Repeat("正文。", 5000)}
		if got := rawBytesOf(uf); len(got) != 0 {
			t.Errorf("取到 %d 字节，want 0", len(got))
		}
	})
	t.Run("nil 安全", func(t *testing.T) {
		if got := rawBytesOf(nil); len(got) != 0 {
			t.Errorf("nil 上传件取到 %d 字节", len(got))
		}
	})
}

// 端到端（到提取为止）：只有 Content、没有 Raw 的上传件，也要拆出内嵌附件。
// 这条覆盖的是「解析服务没配置/挂掉」的现实场景——那条路径上 Raw 永远是空的。
func TestExtractFromFiles_ContentOnlyFindsAttachment(t *testing.T) {
	docx := liteNoticeDocx("embed1.docx", "附件：1.报名表")
	// 场景 A：解析服务没配置/挂掉（Raw 永远为空，Content 里还是原件字节）。
	// 这条路径上附件必须照样能拆出来，否则用户要的模板静默消失。
	t.Run("解析服务不可用：只有 Content 也要拆出附件", func(t *testing.T) {
		got := extractFromFiles([]*UploadedFile{{Filename: "关于开展XX工作的通知.docx", Content: string(docx)}}, &manualPack{})
		if len(got) != 1 {
			t.Fatalf("拆出 %d 个附件，want 1：%+v", len(got), got)
		}
		if !strings.Contains(got[0].How, "docx:embeddings") {
			t.Errorf("How = %q，要能看出走过哪条提取路径（拆错时要靠它定位层）", got[0].How)
		}
		if len(got[0].Raw) == 0 {
			t.Errorf("Raw 为空：落盘会写出一份 0 字节的「模板」")
		}
		if !strings.HasPrefix(got[0].Rel, AttachDir+"/") {
			t.Errorf("Rel = %q，want 以 %s/ 开头", got[0].Rel, AttachDir)
		}
		// 没有正文可读（正文清单在 document.xml 里，而 Content 还是压缩包），
		// 名字退回容器内的原名。这是这条路径的已知代价：**文件照样发得出去**，
		// 只是名字不如「报名表.docx」好看。要更好的名字就得有解析出的正文。
		if got[0].Name == "" {
			t.Errorf("名字为空（下发给用户时会是一个没有文件名的附件）")
		}
	})

	// 场景 B：解析服务正常（Raw 是留档原件，Content 已是正文文本）→ 名字取自正文清单。
	t.Run("解析正常：名字取自正文里的附件清单", func(t *testing.T) {
		got := extractFromFiles([]*UploadedFile{{
			Filename:  "关于开展XX工作的通知.docx",
			Content:   "关于开展XX工作的通知\n附件：1.报名表\n",
			Raw:       docx,
			Extracted: true,
		}}, &manualPack{})
		if len(got) != 1 {
			t.Fatalf("拆出 %d 个附件，want 1：%+v", len(got), got)
		}
		if got[0].Name != "报名表.docx" {
			t.Errorf("Name = %q，want 报名表.docx（名字来自正文清单，而不是 embed1.docx）", got[0].Name)
		}
		if got[0].Rel != AttachDir+"/报名表.docx" {
			t.Errorf("Rel = %q", got[0].Rel)
		}
	})

	// 纯文本上传件不该拆出任何东西（也不能把正文当容器解）。
	txt := &UploadedFile{Filename: "说明.txt", Content: strings.Repeat("正文。", 5000)}
	if atts := extractFromFiles([]*UploadedFile{txt}, &manualPack{}); len(atts) != 0 {
		t.Errorf("纯文本拆出 %d 个附件，want 0：%+v", len(atts), atts)
	}
}

// 取材阶段已拆过时必须复用同一批字节：拆两次的结果理论上一样，但两处判据一旦有出入，
// 就会出现「提示词里说有 报名表.docx、磁盘上没有」这种最难受的错。
func TestExtractFromFiles_ReusesEarlyExtraction(t *testing.T) {
	early := []ExtractedAttachment{{Name: "报名表.docx", Rel: AttachDir + "/报名表.docx", Size: 10, Raw: []byte("0123456789")}}
	mp := &manualPack{Extracted: early}
	got := extractFromFiles([]*UploadedFile{{Filename: "通知.docx", Content: string(liteNoticeDocx("embed1.docx", "附件：1.报名表"))}}, mp)
	if len(got) != 1 || got[0].Name != "报名表.docx" {
		t.Fatalf("没有复用取材阶段的清单：%+v", got)
	}
	if &got[0] != &early[0] {
		t.Errorf("返回的是新切片副本；land 落盘必须与提示词里承诺的清单是同一份（保持引用同一底层数组）")
	}
}

// ===== 4. 路径归一：新老两种取值口径 =====

func TestResolveAttachRel_Normalize(t *testing.T) {
	regs := []model.Attachment{{Name: "报名表.docx", Rel: AttachDir + "/报名表.docx", Size: 12}}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"已带 attachments/ 前缀 → 原样", "attachments/报名表.docx", "attachments/报名表.docx"},
		{"裸名字 + 登记表里有 → 补 attachments/", "报名表.docx", "attachments/报名表.docx"},
		{"老技能的裸名字 + 登记表里没有 → 兜底 source/（向后兼容）", "老模板.docx", "source/老模板.docx"},
		{"老技能显式 source/ 前缀 → 原样", "source/老模板.docx", "source/老模板.docx"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveAttachRel(c.in, regs); got != c.want {
				t.Fatalf("resolveAttachRel(%q) = %q，want %q", c.in, got, c.want)
			}
		})
	}
}

// ===== 5. 重名去重 + 落盘登记 =====

func TestDedupeAttachments_CrossFileSameName(t *testing.T) {
	mk := func() ExtractedAttachment {
		return ExtractedAttachment{Name: "报名表.docx", Rel: AttachDir + "/报名表.docx", Origin: "通知.docx", Size: 100, Raw: []byte("x")}
	}
	got := DedupeAttachments([]ExtractedAttachment{mk(), mk()})
	if len(got) != 2 {
		t.Fatalf("去重后剩 %d 个，want 2（同名不是重复，是两份不同的文件）", len(got))
	}
	seen := map[string]bool{}
	for _, a := range got {
		if seen[a.Rel] {
			t.Fatalf("落盘路径重复：%q —— 后一份会静默覆盖前一份", a.Rel)
		}
		seen[a.Rel] = true
		if !strings.HasPrefix(a.Rel, AttachDir+"/") {
			t.Errorf("Rel = %q，want 以 %s/ 开头", a.Rel, AttachDir)
		}
	}
	if got[1].Name != "报名表-2.docx" {
		t.Errorf("第二份名字 = %q，want 报名表-2.docx", got[1].Name)
	}
}

func TestWriteAttachments_LandsRegistersAndBlocksEscape(t *testing.T) {
	dir := t.TempDir()
	atts := []ExtractedAttachment{
		{Name: "报名表.docx", Rel: AttachDir + "/报名表.docx", Origin: "通知.docx", Size: 4, How: "docx:embeddings>embed:office", Raw: []byte("docx")},
		{Name: "越界.docx", Rel: "../越界.docx", Origin: "通知.docx", Size: 3, Raw: []byte("bad")},
	}
	regs, err := WriteAttachments(dir, atts)
	if err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	if len(regs) != 1 {
		t.Fatalf("登记 %d 份，want 1（越界路径必须被跳过）：%+v", len(regs), regs)
	}
	if regs[0].Rel != AttachDir+"/报名表.docx" {
		t.Errorf("登记 Rel = %q", regs[0].Rel)
	}
	if regs[0].Size != 4 {
		t.Errorf("登记 Size = %d，want 4（运行时要靠它报给用户）", regs[0].Size)
	}
	onDisk := filepath.Join(dir, AttachDir, "报名表.docx")
	b, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("磁盘上没有附件：%v —— 就是「说得出发不出」的技能", err)
	}
	if string(b) != "docx" {
		t.Errorf("落盘内容 = %q，want 原始字节", string(b))
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "越界.docx")); err == nil {
		t.Errorf("越界文件被写到了技能目录之外")
	}
}
