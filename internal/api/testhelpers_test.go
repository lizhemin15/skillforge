package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xuri/excelize/v2"

	"github.com/lizhemin15/skillforge/internal/db"
	"github.com/lizhemin15/skillforge/internal/store"
)

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录 %s 失败：%v", dir, err)
	}
}

func writeBytes(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("写文件 %s 失败：%v", path, err)
	}
}

// newStoreForTest 建一个落临时目录的完整技能库（真 sqlite，因为 NewSkillStore
// 会 seed 核心技能，nil db 会 panic）。
func newStoreForTest(t *testing.T, dataDir string) *store.SkillStore {
	t.Helper()
	d, err := db.Open(filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatalf("建测试库失败：%v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return store.NewSkillStore(d, dataDir)
}

// writeFakeSkillTemplate 在技能目录里放一个 .xlsx 模板，占位符形如 {{字段名}}。
// 用 excelize 造的原因：真实用户在 Excel 里另存出来的 .xlsx 就是这个形态
// （共享字符串表），能顺带测到共享表那条解析路径。
func writeFakeSkillTemplate(t *testing.T, dataDir, slug, name string) string {
	t.Helper()
	f := excelize.NewFile()
	defer f.Close()
	sh := f.GetSheetName(0)
	if err := f.SetCellValue(sh, "A1", "{{name}}"); err != nil {
		t.Fatalf("写模板失败：%v", err)
	}
	if err := f.SetCellValue(sh, "B1", "{{amount}}"); err != nil {
		t.Fatalf("写模板失败：%v", err)
	}
	buf, err := f.WriteToBuffer()
	if err != nil {
		t.Fatalf("导出模板失败：%v", err)
	}
	dir := filepath.Join(dataDir, "skills", slug)
	mkdirAll(t, dir)
	path := filepath.Join(dir, name)
	writeBytes(t, path, buf.Bytes())
	return path
}
