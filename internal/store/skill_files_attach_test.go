package store

import (
	"os"
	"path/filepath"
	"testing"
)

// attachments/ 目录在管理端文件树里的可见性回归防线。
//
// 真实缺口（本轮发现）：从源文档里拆出来的内嵌附件落在 attachments/ 下，
// 技能运行时也确实能把它下发给用户 —— 但 ListFiles 压根不扫这个目录，
// 于是**管理端树上一个附件都看不到**。后果跟当年顶层模板那个 bug 一样：
// 管理员没法核对「到底拆出了什么」、没法自己下载看一眼，用户报
// 「说好有模板却打不开」时只能 SSH 进服务器翻目录。
// 拆是拆了、也确实能发，但产品上看不见 —— 等于没做。
//
// 这层断言守四件事：
//  1. attachments/* 必须在清单里，kind=attach；二进制（.docx/.xlsx）editable=false，
//     文本附件（.txt）editable=true —— 扩展名判据跟 source/ 同一把尺子；
//  2. **排序**：attach 紧跟 templatefile（「要填的模板」与「要发给用户的成品」挨着），
//     且在 requirement 之前 —— 这是插队改 orderOf 最容易错的地方（漏改/重排会静默变序）；
//  3. 只扫一层：attachments/sub/ 下的文件不列（跟 source/ 同口径，深扫会把杂文件当附件）；
//  4. **不可删**：删文件不会同步清 DB 里的附件声明，声明还在文件没了 = 用户拿到
//     点开就 404 的卡（见 api 侧 validDeliverables）—— 所以 DeleteFile 必须拒绝。

func TestListFilesIncludesExtractedAttachments(t *testing.T) {
	s := newStoreForTest(t, t.TempDir())
	const slug = "培训通知"
	dir := filepath.Join(s.SkillsDir(), slug)

	mustWrite(t, filepath.Join(dir, "system_prompt.md"), "你是通知助手")
	mustWrite(t, filepath.Join(dir, "采购合同模板.docx"), fakeDocx)
	// 排序断言的**参照物**：没有这几个文件，「attach 排在谁前面」根本无从判起。
	// （第一版就漏了它们，于是把 attach 排到整个清单末尾的注入破坏照样绿灯 —— 假防线。）
	mustWrite(t, filepath.Join(dir, "requirement.md"), "训练需求")
	mustWrite(t, filepath.Join(dir, "source", "原稿.docx"), fakeDocx)
	mustWrite(t, filepath.Join(dir, "attachments", "附件2-报名表.docx"), fakeDocx)
	mustWrite(t, filepath.Join(dir, "attachments", "附件3-汇总表.xlsx"), fakeDocx)
	mustWrite(t, filepath.Join(dir, "attachments", "附件1-说明.txt"), "纯文本附件")
	// 只扫一层：子目录里的不该列出来
	mustWrite(t, filepath.Join(dir, "attachments", "backup", "旧版报名表.docx"), fakeDocx)

	files, err := s.ListFiles(slug)
	if err != nil {
		t.Fatalf("ListFiles 出错：%v", err)
	}
	kind := map[string]string{}
	editable := map[string]bool{}
	for _, f := range files {
		kind[f.Path] = f.Kind
		editable[f.Path] = f.Editable
	}

	// 1. 在清单里，kind 正确
	if k, ok := kind["attachments/附件2-报名表.docx"]; !ok {
		t.Fatalf("拆出来的附件没进文件清单 %v（管理员界面上看不到 = 这功能等于没做）", kind)
	} else if k != "attach" {
		t.Fatalf("附件 kind = %q，want attach（会落到别的分组里，管理员在「下发附件」组看不到它）", k)
	}
	if editable["attachments/附件2-报名表.docx"] {
		t.Fatalf("docx 附件被标成可编辑 —— 二进制在文本编辑器里改只会存坏文件")
	}
	if !editable["attachments/附件1-说明.txt"] {
		t.Fatalf("文本附件应为可编辑（.txt 跟 md 一样是纯文本，判据来自 isBinaryExt）")
	}

	// 2. 排序：templatefile → attach → requirement → source 逐段递进。
	// 用「相邻参照物」而不是只判两个点：只写 templatefile<attach 的话，
	// 把 attach 排到清单末尾（fidelity 后面）也照样过 —— 那条断言等于没写。
	idx := map[string]int{}
	for i, f := range files {
		idx[f.Path] = i
	}
	// 用「存在性」判参照物，而不是判下标为 0：
	// 下标 0 是合法位置（requirement.md 排第一时 iReq==0），拿 0 当「没找到」
	// 会在某个未来的排序调整后突然误报，把人引到错的方向。
	lookup := func(path string) int {
		i, ok := idx[path]
		if !ok {
			t.Fatalf("参照物 %q 没进清单，排序断言会退化成假绿（清单：%v）", path, idx)
		}
		return i
	}
	iTplFile := lookup("采购合同模板.docx")
	iAttach := lookup("attachments/附件2-报名表.docx")
	iReq := lookup("requirement.md")
	iSrc := lookup("source/原稿.docx")
	if !(iTplFile < iAttach && iAttach < iReq && iReq < iSrc) {
		t.Fatalf("附件排序不对：期望 templatefile(%d) < attach(%d) < requirement(%d) < source(%d)",
			iTplFile, iAttach, iReq, iSrc)
	}

	// 3. 只扫一层
	if _, ok := kind["attachments/backup/旧版报名表.docx"]; ok {
		t.Fatalf("attachments/ 被深扫了，子目录里的文件混进了附件清单")
	}

	// 4. 不可删（且文件必须还在）
	if err := s.DeleteFile(slug, "attachments/附件2-报名表.docx"); err == nil {
		t.Fatalf("attachments/ 下的附件居然能删 —— 文件删了 DB 里的声明还在，用户会拿到点开就 404 的卡")
	}
	if _, err := os.Stat(filepath.Join(dir, "attachments", "附件2-报名表.docx")); err != nil {
		t.Fatalf("拒绝删除后文件却没了：%v", err)
	}
}

// 没有附件的技能（绝大多数手册）不能被这条改动弄出噪音或报错。
func TestListFilesWithoutAttachmentsIsQuiet(t *testing.T) {
	s := newStoreForTest(t, t.TempDir())
	const slug = "普通技能"
	mustWrite(t, filepath.Join(s.SkillsDir(), slug, "system_prompt.md"), "你是助手")

	files, err := s.ListFiles(slug)
	if err != nil {
		t.Fatalf("ListFiles 出错：%v", err)
	}
	for _, f := range files {
		if f.Kind == "attach" {
			t.Fatalf("没有 attachments/ 目录却列出了附件 %q", f.Path)
		}
	}
}
