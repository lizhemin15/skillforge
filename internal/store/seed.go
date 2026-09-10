package store

import (
	"os"
	"path/filepath"
)

// seedCoreSkills guarantees the built-in skills exist on every start, so a
// fresh deploy always ships them. It is idempotent: existing rows/files are
// left untouched so admin edits to the built-in skills survive restarts.
func (s *SkillStore) seedCoreSkills() {
	s.ensureCoreSkill(
		"办公文档管家",
		"办公文档管家",
		`根据用户要求生成或填写常见办公文档：Word 文字文档、Excel 表格、PDF 正式文档、PPT 演示文稿。支持下发空白模板，也支持用户给出材料/数据后填充生成成品文档。触发场景：生成/填写/导出/下发 Word/Excel/PDF/PPT、表格、表单、报销单、请假条、报告、合同、名单、演示等。`,
		"docgen",
		"",
		coreDocGenSystemPrompt,
	)
}

// ensureCoreSkill creates (if missing) a single built-in skill: its metadata
// row in SQLite plus its system_prompt.md on disk under the skills dir.
// No required params are registered so docgen-style skills never block on the
// `needs` path — they always generate (blank template or filled doc).
func (s *SkillStore) ensureCoreSkill(slug, name, description, skillType, attachment, systemPrompt string) {
	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM skills WHERE slug=?`, slug).Scan(&exists); err != nil {
		return
	}
	if exists == 0 {
		_, err := s.db.Exec(
			`INSERT INTO skills(slug,name,description,category,version,enabled,is_core,skill_type,attachment)
			 VALUES(?,?,?,?,1,1,1,?,?)`,
			slug, name, description, "通用", skillType, attachment,
		)
		_ = err
	}
	// ensure the system prompt file exists on disk
	dir := filepath.Join(s.skillsDir, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	sp := filepath.Join(dir, "system_prompt.md")
	if _, err := os.Stat(sp); os.IsNotExist(err) {
		_ = os.WriteFile(sp, []byte(systemPrompt), 0o644)
	}
}

// coreDocGenSystemPrompt is the built-in 办公文档管家 contract. It instructs
// the generator LLM to emit a strict DOCJSON describing the document, which
// the backend parses and renders into an office file.
const coreDocGenSystemPrompt = `你是办公文档管家，负责把用户的需求转成一份可下载的办公文档（Word/Excel/PDF/PPT）。

你可以处理两类请求：
- 用户只说"发个空白模板 / 给个空白表 / 下发模板 / 下载"等 → 生成一个含标题和表头的空白模板（缺少的数据行留空，或给一个示例行供参考）。
- 用户给出具体信息（人员名单、报销明细、请假事由、汇报要点等）→ 把这些信息填进文档生成成品。

支持的文档类型（format 字段）：
- word  文字文档：标题 + 段落，或用表格呈现结构化内容。适合：报告、通知、请假条、合同条款、信函、说明。
- excel 表格工作簿：标题 + 表头列 + 数据行。适合：名单、报销清单、考勤表、信息登记表、任何"多行多列"的数据。
- pdf   排版固定的正式文档：标题 + 段落或表格。适合：需要打印/签署的正式文件。
- ppt   幻灯片演示：标题 + 分点要点，每行 parags 是一页的要点。适合：汇报、培训、方案介绍。

【输出契约】
只输出一个 JSON 对象，不要用 markdown 代码块包裹，不要输出任何其他文字。字段：
{
  "format": "word | excel | pdf | ppt",
  "filename": "建议的下载文件名（含扩展名，如 员工信息表.xlsx）",
  "title": "文档标题（中文，清晰体现用途）",
  "cols": ["表头1","表头2",...],          // 表格类文档填表头；纯文字/PPT 可为空数组
  "rows": [["单元格值","单元格值",...],...], // 数据行，每行列数与 cols 一致；空白模板可为空数组
  "parags": ["段落或要点1","段落或要点2",...] // 文字/PPT 用；excel 可为空数组
}

【规则】
1. 用户没说格式时，按内容决定最合适的：多行多列数据 → excel；正式文字 → word；汇报/培训/方案 → ppt；需要打印签署 → pdf。
2. 标题和文件名用中文，简短清楚。
3. "空白模板"类请求：构造合理的表头结构，数据行留空。
4. excel 里，每行 rows 的列数必须与 cols 一致。
5. 用户信息不完整时，基于现有信息生成一份合理的文档（缺的列就省略，不要卡住反复索要信息；只有完全无法开始时才需要向用户询问）。
6. 不要有任何开场白、解释或总结，只输出 JSON。
`
