package docgen

import "testing"

// TestSafeFilenameAsForcesRealExtension 锁死「违规格式请求」边界：
// 交付的文件名绝不能与实际字节格式不符（要 .exe 不能真给个 .exe 名字），
// 也不能靠路径穿越逃出下载目录。
func TestSafeFilenameAsForcesRealExtension(t *testing.T) {
	cases := []struct {
		name, format, want string
	}{
		{"采购清单.xlsx", "excel", "采购清单.xlsx"},
		{"采购清单", "excel", "采购清单.xlsx"},
		{"报告.exe", "word", "报告.docx"}, // 违规扩展名 → 换成真实格式
		{"视频.mp4", "pdf", "视频.pdf"},   // 同上
		{"图纸.dwg", "ppt", "图纸.pptx"},  // 同上
		{"报表.xlsx", "pdf", "报表.pdf"},  // 扩展名与格式矛盾 → 以字节为准
		{"../../etc/passwd", "excel", "etc_passwd.xlsx"},
		{"..\\..\\win.ini", "word", "win.docx"},
		{"", "excel", "document.xlsx"},
		{"", "pdf", "document.pdf"},
		{"", "", "document.pdf"},
	}
	for _, c := range cases {
		if got := SafeFilenameAs(c.name, c.format); got != c.want {
			t.Errorf("SafeFilenameAs(%q,%q) = %q, want %q", c.name, c.format, got, c.want)
		}
	}
}

// TestSafeFilenameBackCompat 老调用点（不知道 format）行为不退化。
func TestSafeFilenameBackCompat(t *testing.T) {
	if got := SafeFilename("采购合同.docx"); got != "采购合同.docx" {
		t.Errorf("正常名不该被改: %q", got)
	}
	if got := SafeFilename("a/b/../../c"); got != "a_b_c" {
		t.Errorf("分隔符与穿越点都应被替换: %q", got)
	}
	if got := SafeFilename(""); got != "document.pdf" {
		t.Errorf("空名兜底: %q", got)
	}
}
