package store

import (
	"testing"
)

// clearCoreMarker 抹掉迁移闸门，回到"新代码第一次启动老库"的状态。
//
// 必须显式抹：newStoreForTest → NewSkillStore 的构造函数本身就会跑一次归一化
// 并落闸门，不抹的话测试里再来一次会直接短路返回 0，断言就成了空转。
func clearCoreMarker(t *testing.T, s *SkillStore) {
	t.Helper()
	if _, err := s.db.Exec(`DELETE FROM settings WHERE key=?`, settingCoreSkillsMigrated); err != nil {
		t.Fatalf("抹迁移闸门失败：%v", err)
	}
}

// seedBizSkill 造一个业务技能（非核心）用于测试。
func seedBizSkill(t *testing.T, s *SkillStore, slug, name string, core bool) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO skills (slug, name, description, category, skill_type, is_core) VALUES (?,?,?,?,?,?)`,
		slug, name, "d", "业务", "write", boolToInt(core)); err != nil {
		t.Fatalf("插技能 %s 失败：%v", slug, err)
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func coreOf(t *testing.T, s *SkillStore, slug string) int {
	t.Helper()
	var v int
	if err := s.db.QueryRow(`SELECT is_core FROM skills WHERE slug=?`, slug).Scan(&v); err != nil {
		t.Fatalf("读 %s 的 is_core 失败：%v", slug, err)
	}
	return v
}

// 用户的口径：「核心 = 通用能力」，其余都不是核心。
// 老库里「公司新闻通稿」这种业务技能被标成了核心（历史遗留），
// 归一化必须把它摘下来，同时把通用能力顶上去。
func TestNormalizeCoreSkillsReshapesLegacyCore(t *testing.T) {
	s := newStoreForTest(t, t.TempDir())
	clearCoreMarker(t, s)
	// 内置核心技能由 seedCoreSkills 建好；再造两个"被误标成核心"的业务技能。
	seedBizSkill(t, s, "公司新闻通稿", "公司新闻通稿", true)
	seedBizSkill(t, s, "采购合同", "采购合同", false)

	changed, err := s.NormalizeCoreSkills([]string{"办公文档管家", "技能工厂"})
	if err != nil {
		t.Fatalf("归一化失败：%v", err)
	}
	if changed == 0 {
		t.Fatal("归一化应该至少改动「公司新闻通稿」一行，实际 0 行")
	}
	if got := coreOf(t, s, "公司新闻通稿"); got != 0 {
		t.Fatalf("业务技能不该是核心，got is_core=%d", got)
	}
	if got := coreOf(t, s, "采购合同"); got != 0 {
		t.Fatalf("业务技能不该是核心，got is_core=%d", got)
	}
	for _, slug := range []string{"办公文档管家", "技能工厂"} {
		if got := coreOf(t, s, slug); got != 1 {
			t.Fatalf("内置通用能力 %s 必须是核心，got is_core=%d", slug, got)
		}
	}
}

// 归一化只跑一次：否则管理端手动设的核心技能（比如把"公司新闻通稿"设回核心）
// 会在下次重启时被悄悄抹掉 —— 用户看不见的修改比不做更糟。
func TestNormalizeCoreSkillsRunsOnlyOnce(t *testing.T) {
	s := newStoreForTest(t, t.TempDir())
	clearCoreMarker(t, s)
	seedBizSkill(t, s, "公司新闻通稿", "公司新闻通稿", true)
	if _, err := s.NormalizeCoreSkills([]string{"办公文档管家", "技能工厂"}); err != nil {
		t.Fatalf("首次归一化失败：%v", err)
	}

	// 模拟管理端的显式选择
	if err := s.SetCore("公司新闻通稿", true); err != nil {
		t.Fatalf("SetCore 失败：%v", err)
	}
	// 再过一次启动流程
	if _, err := s.NormalizeCoreSkills([]string{"办公文档管家", "技能工厂"}); err != nil {
		t.Fatalf("二次归一化失败：%v", err)
	}
	if got := coreOf(t, s, "公司新闻通稿"); got != 1 {
		t.Fatalf("迁移不该覆盖管理端的选择，got is_core=%d", got)
	}
}

// 迁移闸门必须落库：不能靠"内存里跑过一次"判断，
// 重启进程就会重放，等于没有一次性。
func TestNormalizeCoreSkillsWritesMarker(t *testing.T) {
	dir := t.TempDir()
	s := newStoreForTest(t, dir)
	clearCoreMarker(t, s)
	if _, err := s.NormalizeCoreSkills([]string{"办公文档管家"}); err != nil {
		t.Fatalf("归一化失败：%v", err)
	}
	var v string
	if err := s.db.QueryRow(`SELECT val FROM settings WHERE key=?`, settingCoreSkillsMigrated).Scan(&v); err != nil {
		t.Fatalf("迁移闸门没落库：%v", err)
	}
	if v == "" {
		t.Fatal("迁移闸门不该是空串（空串和「没写」在读侧无法区分）")
	}
}

// SetCore 的正常路径与异常路径。
func TestSetCore(t *testing.T) {
	s := newStoreForTest(t, t.TempDir())
	seedBizSkill(t, s, "公司新闻通稿", "公司新闻通稿", false)

	if err := s.SetCore("公司新闻通稿", true); err != nil {
		t.Fatalf("设为核心失败：%v", err)
	}
	if got := coreOf(t, s, "公司新闻通稿"); got != 1 {
		t.Fatalf("设为核心没生效，is_core=%d", got)
	}
	// 幂等：本来就是核心，再设一次不该报错（管理端双击/重放很常见）
	if err := s.SetCore("公司新闻通稿", true); err != nil {
		t.Fatalf("重复设为核心不该报错：%v", err)
	}
	if err := s.SetCore("公司新闻通稿", false); err != nil {
		t.Fatalf("取消核心失败：%v", err)
	}
	if got := coreOf(t, s, "公司新闻通稿"); got != 0 {
		t.Fatalf("取消核心没生效，is_core=%d", got)
	}
	// 不存在的技能要报错，不能静默成功（否则管理端会显示成"已设为核心"）
	if err := s.SetCore("不存在的技能", true); err == nil {
		t.Fatal("对不存在的技能设核心应当报错")
	}
	if err := s.SetCore("   ", true); err == nil {
		t.Fatal("空 slug 应当报错")
	}
}

// placeholders 的边界：0 个参数必须退化成"一条都不清"，
// 而不是拼出 `NOT IN ()` 这种语法错误把启动流程炸掉。
func TestPlaceholdersEdge(t *testing.T) {
	if got := placeholders(0); got != "NULL" {
		t.Fatalf("placeholders(0) = %q, want NULL", got)
	}
	if got := placeholders(3); got != "?,?,?" {
		t.Fatalf("placeholders(3) = %q", got)
	}
}

// 名单为空时归一化不能误伤：一条都不该清（NOT IN (NULL) 恒 false）。
func TestNormalizeCoreSkillsEmptyListIsNoop(t *testing.T) {
	s := newStoreForTest(t, t.TempDir())
	clearCoreMarker(t, s)
	seedBizSkill(t, s, "公司新闻通稿", "公司新闻通稿", true)
	if _, err := s.NormalizeCoreSkills(nil); err != nil {
		t.Fatalf("空名单不该报错：%v", err)
	}
	if got := coreOf(t, s, "公司新闻通稿"); got != 1 {
		t.Fatalf("空名单不该清任何核心标记，got is_core=%d", got)
	}
}
