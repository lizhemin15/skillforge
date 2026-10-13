package agent

import (
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/model"
)

// 本文件钉住「本轮发哪几份附件」的规则。
//
// 为什么这些用例值得写死：这套规则是**用户只能从结果里反推**的东西。
// 发多了（用户问流程却收到一叠表格）他不会报错，只会觉得这个技能莫名其妙；
// 发错了（要报名表给汇总表）他更不会报错，直接拿错的表去填。两种失败都
// 不留痕、不进日志，只能在代码层用断言守住。所以这里的每个 case 都对应
// 一个「用户会怎么读这个结果」的真实场景。

func att(name, rel string) model.Attachment {
	return model.Attachment{Name: name, Rel: rel, Origin: "通知.docx"}
}

// threeAtts 是那个典型现场：一份通知里拆出三份附件。
func threeAtts() []model.Attachment {
	return []model.Attachment{
		att("附件1-申报指南.docx", "attachments/附件1-申报指南.docx"),
		att("附件2-报名表.docx", "attachments/附件2-报名表.docx"),
		att("附件3-汇总表.xlsx", "attachments/附件3-汇总表.xlsx"),
	}
}

func names(atts []model.Attachment) string { return AttachNames(atts) }

// TestResolveDeliverablesRuleTable 把四条规则一次钉死。
//
// 这里用「期望的展示名串」而不是比对切片下标：切片相等看不出「发的是哪一份」，
// 而这条链路上唯一的真问题就是「发的是哪一份」。
func TestResolveDeliverablesRuleTable(t *testing.T) {
	one := []model.Attachment{att("附件2-报名表.docx", "attachments/附件2-报名表.docx")}
	cases := []struct {
		name     string
		declared []model.Attachment
		files    []string
		action   string
		want     string // 期望下发的展示名串；空串=不下发
		why      string
	}{
		{
			name: "规则0：没声明附件→什么都不发", declared: nil, files: nil, want: "",
			why: "绝大多数写作技能走这条，不能凭空造文件卡片",
		},
		{
			name: "规则1：点名命中→只发点名那份", declared: threeAtts(),
			files: []string{"附件2-报名表.docx"}, want: "附件2-报名表.docx",
			why: "要报名表给汇总表比不发更坏——用户会拿错的表去填",
		},
		{
			name: "规则1：点名多份→按点名顺序发", declared: threeAtts(),
			files: []string{"附件3-汇总表.xlsx", "附件1-申报指南.docx"},
			want:  "附件3-汇总表.xlsx、附件1-申报指南.docx",
			why:   "顺序跟用户说的来，用户能一眼对上自己那句话",
		},
		{
			name:     "规则1：点名对不上（多份）→不发，由调用方出声报清单",
			declared: threeAtts(), files: []string{"体检表.xls"}, want: "",
			why: "多份时宁可什么都不发：发哪一份都是掷骰子",
		},
		{
			// 这一条是**有意的宽容**，不是漏网：用户说「报名表.xls」而库里只有
			// 附件2-报名表.docx 时，按扩展名判"没找到"会让用户以为拆分漏了这份表
			// （最贵的误判），而按主干命中只是拿到的格式不同——名字对得上他一看就懂。
			// 反之若主干对不上（上面的体检表），照样不发。
			name: "规则1：主干对得上但扩展名不同→仍命中", declared: threeAtts(),
			files: []string{"报名表.xls"}, want: "附件2-报名表.docx",
			why: "同一个东西的不同格式，比「没找到」更有用",
		},
		{
			name: "规则1：点名单份技能但名字对不上→兜底发那一份", declared: one,
			files: []string{"那个表"}, want: "附件2-报名表.docx",
			why: "清单只有一份时不可能发错对象，用户明显就是在要它",
		},
		{
			name: "规则2：没点名+只有一份→发（老技能行为不变）", declared: one,
			files: nil, want: "附件2-报名表.docx",
			why: "老技能那份就是管理员上传的源文件，历史上一直随回答下发，不能让它突然不发",
		},
		{
			name: "规则3：没点名+多份+在要模板→全发", declared: threeAtts(),
			files: nil, action: "template_only",
			want: "附件1-申报指南.docx、附件2-报名表.docx、附件3-汇总表.xlsx",
			why:  "「发我附件」这种笼统要法只能全给，用户再挑",
		},
		{
			name: "规则4：没点名+多份+普通提问→不发", declared: threeAtts(),
			files: nil, action: "answer", want: "",
			why: "用户问「怎么报名」时收到一叠表格，正是这次改造要消掉的噪音",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sk := &model.Skill{Slug: "tongzhi", Name: "通知", Attachments: c.declared}
			got := names(ResolveDeliverables(sk, c.files, c.action))
			if got != c.want {
				t.Fatalf("下发清单不对：期望 %q，实际 %q\n（%s）", c.want, got, c.why)
			}
		})
	}
}

// TestResolveDeliverablesNilSkillIsSafe 钉住 nil 技能不 panic。
// 假分类结果 / 技能被并发删除时确实会走到 nil，那时宁可静默不发也不能崩掉整轮。
func TestResolveDeliverablesNilSkillIsSafe(t *testing.T) {
	if got := ResolveDeliverables(nil, []string{"a.docx"}, "template_only"); len(got) != 0 {
		t.Fatalf("nil 技能应返回空清单，实际 %v", got)
	}
}

// TestMatchAttachmentsTolerantShapes 钉住「模型给的形态千奇百怪但都要认出来」。
//
// 为什么必须宽容：分类器是模型，契约里写 "files: []string" 也拦不住它吐
// 《报名表.docx》、attachments\附件2-报名表.docx、"报名表"（省扩展名）。
// 这些形态**解析失败就等于用户点了名却收到「没找到」**，而用户完全无从判断
// 是系统坏了还是自己叫错了名字。原则是：只宽容形状，不丢数据。
func TestMatchAttachmentsTolerantShapes(t *testing.T) {
	declared := threeAtts()
	cases := []struct {
		want string
		hit  string
		why  string
	}{
		{"附件2-报名表.docx", "附件2-报名表.docx", "标准形态：照抄展示名"},
		{"attachments/附件2-报名表.docx", "附件2-报名表.docx", "照抄 roster 里的 rel"},
		{"《附件2-报名表.docx》", "附件2-报名表.docx", "带书名号（模型爱加）"},
		{"  附件2-报名表.docx ", "附件2-报名表.docx", "带空白"},
		{"附件2-报名表", "附件2-报名表.docx", "省了扩展名"},
		{"附件2-报名表.DOCX", "附件2-报名表.docx", "大小写不同"},
		{"报名表", "附件2-报名表.docx", "口语说法：唯一包含匹配"},
		{`attachments\附件2-报名表.docx`, "附件2-报名表.docx", "反斜杠当分隔符（Windows 口径）"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			picked, unknown := MatchAttachments(declared, []string{c.want})
			if len(unknown) != 0 {
				t.Fatalf("不该有未命中项，实际 unknown=%v（%s）", unknown, c.why)
			}
			if len(picked) != 1 || picked[0].Name != c.hit {
				t.Fatalf("匹配结果不对：期望 %q，实际 %v（%s）", c.hit, picked, c.why)
			}
		})
	}
}

// TestMatchAttachmentsAmbiguousContainsRejected 钉住「唯一包含」这一档的**唯一性**要求，
// 以及它与「主干精确相等」的优先级。
//
// 包含匹配天生会把「报名表」同时对上「附件2-报名表.docx」和「附件2-报名表(模板).docx」。
// 歧义时挑一个就是掷骰子：用户要正式版却收到模板版，他只会觉得系统读错了文件。
// 所以歧义必须判为「没找到」，让调用方如实说清单。
//
// 但**主干精确相等**（正是「报名表.docx」）是确定性的，不算歧义，必须命中 ——
// 不能为了"防歧义"把最常见的正常点名叫法一起收窄掉。
func TestMatchAttachmentsAmbiguousContainsRejected(t *testing.T) {
	declared := []model.Attachment{
		att("附件2-报名表.docx", "attachments/附件2-报名表.docx"),
		att("附件2-报名表(模板).docx", "attachments/附件2-报名表(模板).docx"),
	}

	// ① 主干精确相等（去掉扩展名后一字不差）是**确定性**判据，优先于包含匹配：
	//    「附件2-报名表」只可能指那份 .docx，不该因为旁边有个 (模板) 就变歧义。
	picked, unknown := MatchAttachments(declared, []string{"附件2-报名表"})
	if len(picked) != 1 || picked[0].Name != "附件2-报名表.docx" {
		t.Fatalf("主干精确相等应确定命中 .docx，实际 %v", picked)
	}
	if len(unknown) != 0 {
		t.Fatalf("确定命中时不该记 unknown，实际 %v", unknown)
	}

	// ② 只有**包含**这一档才有歧义：「报名表」是两份文件名字的公共子串，
	//    挑一个就是掷骰子 → 判「没找到」，让调用方如实报清单。
	picked, unknown = MatchAttachments(declared, []string{"报名表"})
	if len(picked) != 0 {
		t.Fatalf("歧义包含不该命中任何一份，实际命中 %v", picked)
	}
	if len(unknown) != 1 {
		t.Fatalf("认不出来必须记进 unknown（调用方靠它出声），实际 %v", unknown)
	}

	// ③ 独一无二的包含才认（清单里只有一份带「模板」时，「模板」能对上它）。
	twoOnly := declared[:1]
	picked, _ = MatchAttachments(twoOnly, []string{"报名表"})
	if len(picked) != 1 || picked[0].Name != "附件2-报名表.docx" {
		t.Fatalf("唯一包含应命中，实际 %v", picked)
	}
}

// TestMatchAttachmentsDedupesAndSkipsEmpty 钉住重复点名只发一次、空串不产生假命中。
// 模型经常把同一份文件在 files 里写两遍（"报名表.docx","附件2-报名表.docx"），
// 不去重用户就会看到两张一模一样的卡片。
func TestMatchAttachmentsDedupesAndSkipsEmpty(t *testing.T) {
	declared := threeAtts()
	picked, unknown := MatchAttachments(declared,
		[]string{"附件2-报名表.docx", "附件2-报名表", "", "   "})
	if len(picked) != 1 {
		t.Fatalf("同一份文件被点名两次应只发一份，实际 %v", picked)
	}
	if len(unknown) != 0 {
		t.Fatalf("空串不算「没找到」，实际 unknown=%v", unknown)
	}
}

// TestTruncateDeliverablesReportsDropped 钉住截断必须**报出被截了几份**。
// 静默截断 = 用户以为拆分漏了，他会回头怀疑整个提取功能（这是最贵的误判）。
func TestTruncateDeliverablesReportsDropped(t *testing.T) {
	many := make([]model.Attachment, 0, DeliverMaxFiles+3)
	for i := 0; i < DeliverMaxFiles+3; i++ {
		n := "附件" + string(rune('A'+i)) + ".docx"
		many = append(many, att(n, "attachments/"+n))
	}
	kept, dropped := TruncateDeliverables(many)
	if len(kept) != DeliverMaxFiles || dropped != 3 {
		t.Fatalf("截断口径不对：kept=%d dropped=%d（期望 %d/3）", len(kept), dropped, DeliverMaxFiles)
	}
	// 刚好等于上限时不能误报截断（否则用户会被告知"还有 0 份"）。
	if _, d := TruncateDeliverables(many[:DeliverMaxFiles]); d != 0 {
		t.Fatalf("刚好到上限不该报截断，实际 dropped=%d", d)
	}
}

// TestUnknownFileNoteListsWhatCanBeSent 钉住「没找到」时要把清单列给用户。
// 只回一句「没找到」是死路：用户不知道是自己叫错名字还是系统拆漏了。
func TestUnknownFileNoteListsWhatCanBeSent(t *testing.T) {
	note := UnknownFileNote([]string{"报名表.xls"}, threeAtts())
	for _, want := range []string{"报名表.xls", "附件1-申报指南.docx", "附件3-汇总表.xlsx"} {
		if !strings.Contains(note, want) {
			t.Fatalf("提示里应列出 %q，实际：%s", want, note)
		}
	}
	if UnknownFileNote(nil, threeAtts()) != "" {
		t.Fatal("没有未命中项时不该出声")
	}
	// 没有清单可报时也不能吐半句「能发的是：」这种断头话。
	if n := UnknownFileNote([]string{"x.docx"}, nil); strings.Contains(n, "能发的是") {
		t.Fatalf("没有清单时不该说「能发的是」，实际：%s", n)
	}
}

// TestAttachDisplayNameFallsBackToRel 钉住老数据（Name 为空）也能取到名字。
//
// 这一步取不到名字的后果不只是卡片标题空：下发帧会带着空 name 拼出
// ?file= 空值，下载端点按白名单核对必然 404 —— 卡片点不动。
func TestAttachDisplayNameFallsBackToRel(t *testing.T) {
	if got := AttachDisplayName(model.Attachment{Rel: "attachments/报名表.docx"}); got != "报名表.docx" {
		t.Fatalf("Name 为空时应退到 Rel 的 basename，实际 %q", got)
	}
	if got := AttachDisplayName(att("报名表.docx", "attachments/瞎写的.docx")); got != "报名表.docx" {
		t.Fatalf("Name 非空时以 Name 为准（它才是 roster 印给模型的名字），实际 %q", got)
	}
}
