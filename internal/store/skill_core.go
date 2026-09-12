package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// 核心技能（core skill）= **通用能力**，与具体业务无关，换一个用户/换一家公司
// 也照样需要它：
//   - 办公文档管家：生成/修改 Word、Excel、PDF、PPT
//   - 技能工厂：按用户需求产出一份可落地的技能定义
//
// 反面是「业务技能」：采购合同、采购验收单、公积金办事、公司新闻通稿……
// 它们只属于某个具体场景，不该占据核心位。
//
// 核心标记有两个实际作用：
//  1. 管理端里核心技能不可删除（防手滑删掉通用能力，产品直接残废）
//  2. 技能列表 is_core DESC 排序 —— 核心排前面，前台/管理端都先看到通用能力
//
// 注意「核心」与「启用」是两回事：核心技能同样可以被停用。
var DefaultCoreSkillSlugs = []string{"办公文档管家", "技能工厂"}

// ErrSkillNotFound 让上层能把「技能不存在」和「参数不合法」分开报：
// 前者是 404，后者是 400。混成一个状态码，前端就没法区分"你点错了"和"技能被删了"。
var ErrSkillNotFound = errors.New("技能不存在")

// settingCoreSkillsMigrated 是一次性迁移闸门。改成新值即可再跑一次归一化
// （比如以后新增了一个内置核心技能，想让老库也认它）。
const settingCoreSkillsMigrated = "migr_core_skills_v1"

// SetCore 把某个技能标记/取消「核心」。
func (s *SkillStore) SetCore(slug string, core bool) error {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return errors.New("需要 slug")
	}
	flag := 0
	if core {
		flag = 1
	}
	res, err := s.db.Exec(`UPDATE skills SET is_core=?, updated_at=CURRENT_TIMESTAMP WHERE slug=?`, flag, slug)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 可能本来就等于目标值（UPDATE 命中 0 行）——先确认技能是否存在。
		var exists int
		if err := s.db.QueryRow(`SELECT COUNT(1) FROM skills WHERE slug=?`, slug).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return fmt.Errorf("%w: %s", ErrSkillNotFound, slug)
		}
	}
	return nil
}

// NormalizeCoreSkills 做**一次性**归一：名单内 → is_core=1，名单外 → is_core=0。
//
// 为什么要一次性：这是历史数据的修正（老库里「公司新闻通稿」被标成了核心），
// 但不能每启动一次就重放——那会把管理端后来手动设的核心技能抹掉。
// 迁移跑过就落一个 settings 标记，之后启动直接跳过。
//
// 返回本次改动行数，便于测试断言。
func (s *SkillStore) NormalizeCoreSkills(slugs []string) (int, error) {
	var marker string
	err := s.db.QueryRow(`SELECT val FROM settings WHERE key=?`, settingCoreSkillsMigrated).Scan(&marker)
	if err == nil {
		return 0, nil // 已迁移过，尊重管理端的后续选择
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	changed := 0
	for _, slug := range slugs {
		res, err := tx.Exec(`UPDATE skills SET is_core=1, updated_at=CURRENT_TIMESTAMP WHERE slug=? AND is_core=0`, slug)
		if err != nil {
			return 0, fmt.Errorf("normalize core %q: %w", slug, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			changed += int(n)
		}
	}
	// 核心必须与「通用能力」严格一一对应：名单外一律归零。
	// 这条 UPDATE 是有意为之的"兜底清零"，别改成只处理已知 slug 的白名单累加。
	res, err := tx.Exec(
		`UPDATE skills SET is_core=0, updated_at=CURRENT_TIMESTAMP
		 WHERE is_core=1 AND slug NOT IN (`+placeholders(len(slugs))+`)`,
		toAnySlice(slugs)...)
	if err != nil {
		return 0, fmt.Errorf("normalize core reset: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		changed += int(n)
	}

	if _, err := tx.Exec(
		`INSERT INTO settings (key, val, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(key) DO UPDATE SET val=excluded.val, updated_at=CURRENT_TIMESTAMP`,
		settingCoreSkillsMigrated, "1"); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return changed, nil
}

// placeholders renders "?,?,?" for an IN clause.
func placeholders(n int) string {
	if n <= 0 {
		return "NULL" // NOT IN (NULL) 恒为 false → 一条都不清，好过语法错误
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func toAnySlice(ss []string) []any {
	out := make([]any, 0, len(ss))
	for _, s := range ss {
		out = append(out, s)
	}
	return out
}
