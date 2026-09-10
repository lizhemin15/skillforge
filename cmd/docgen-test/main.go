// Test program for the docgen package. Produces sample files in /tmp/docgen-test.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/lizhemin15/skillforge/internal/docgen"
)

func main() {
	dir := "/tmp/docgen-test"
	_ = os.MkdirAll(dir, 0o755)

	doc := docgen.Doc{
		Title:  "员工月度绩效表",
		Format: "word",
		Cols:   []string{"姓名", "部门", "KPI得分", "评级"},
		Rows: [][]string{
			{"张三", "研发部", "92", "优秀"},
			{"李四", "市场部", "85", "良好"},
			{"王五", "运营部", "78", "合格"},
		},
		Parags: []string{
			"本表依据《绩效管理制度》生成，数据为月度汇总。",
			"评级标准：95分以上优秀，85-94良好，70-84合格，70以下待改进。",
		},
	}

	formats := []string{"word", "excel", "pdf", "ppt"}
	exts := map[string]string{"word": "docx", "excel": "xlsx", "pdf": "pdf", "ppt": "pptx"}
	for _, f := range formats {
		d := doc
		d.Format = f
		data, err := docgen.Generate(d)
		if err != nil {
			fmt.Printf("[%s] ERROR: %v\n", f, err)
			continue
		}
		if len(data) == 0 {
			fmt.Printf("[%s] ERROR: empty output\n", f)
			continue
		}
		path := filepath.Join(dir, "sample."+exts[f])
		if err := os.WriteFile(path, data, 0o644); err != nil {
			fmt.Printf("[%s] WRITE ERROR: %v\n", f, err)
			continue
		}
		fmt.Printf("[%s] OK %d bytes -> %s\n", f, len(data), path)
	}
	fmt.Println("done")
}
