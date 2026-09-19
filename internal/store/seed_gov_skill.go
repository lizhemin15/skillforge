package store

import (
	_ "embed"
)

// govTaskDevSystemPrompt 是内置业务技能「数据治理任务开发」的系统提示词。
//
// 内容本体放在 prompts/gov_task_dev.md：里面有大量 markdown 围栏和 JS 代码，
// 用 Go 原始字符串字面量会跟反引号打架，独立成文件后 git diff 也看得清。
// 真正的「数据分离」发生在运行时——播种时这份内容会被写成
// data/skills/数据治理任务开发/system_prompt.md，管理端可以就地改。
//
//go:embed prompts/gov_task_dev.md
var govTaskDevSystemPrompt string

const (
	// GovTaskDevSlug 同时用作技能目录名，与「办公文档管家」「技能工厂」等内置技能保持一致的中文命名。
	// 导出是为了让 agent 包的 roster 测试能直接引用它（停用态不可见 / 启用后可见）。
	GovTaskDevSlug = "数据治理任务开发"
	govTaskDevName = "数据治理任务开发"
	// govTaskDevCategory 落在「业务场景」下：它服务 DataToolbox 这一类具体产品，
	// 不是换谁都要的通用能力，所以既不进核心（is_core=0），也默认停用。
	govTaskDevCategory = "业务场景"
	// govTaskDevDescription 是模型选技能时唯一能看到的信息，触发场景要写全。
	govTaskDevDescription = `编写/修改 DataToolbox 数据治理任务的 JavaScript 脚本（gov.* API）：读 Word/Excel/CSV、公文标题层级解析、调 AI 抽取字段、批量入库、套模板生成 Word/Excel、定时 SQL 统计、任务注册成 API。触发场景：写数据治理任务代码、写治理脚本、把公文/报表转成结构化数据或表格、Excel 批量入库、按模板生成 Word/Excel、改我那个治理任务的脚本。`
)

// seedGovTaskDevSkill 播种内置业务技能「数据治理任务开发」。
//
// 与核心技能的区别只有三处：分类是「业务场景」而不是「通用」、is_core=0、
// enabled=0（默认停用）。默认停用是产品决定：它是特定产品的专用能力，
// 新装的实例不一定用得上，先躺着；管理端「技能管理」里一行开关就能启用。
//
// 仍然走 ensureBuiltinSkill 的同一条幂等路径：只在缺失时插入，
// 管理员改过的分类/开关/提示词文件都不会被重启覆盖。
func (s *SkillStore) seedGovTaskDevSkill() {
	s.ensureBuiltinSkill(builtinSkillSpec{
		Slug:         GovTaskDevSlug,
		Name:         govTaskDevName,
		Description:  govTaskDevDescription,
		Category:     govTaskDevCategory,
		SkillType:    "query",
		Enabled:      false,
		IsCore:       false,
		SystemPrompt: govTaskDevSystemPrompt,
	})
}
