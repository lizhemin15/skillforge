package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/db"
)

func newStoreForNotFoundTest(t *testing.T) *SkillStore {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return NewSkillStore(d, dir)
}

// 2026-09-26 线上形态：分类器往 skill_slug 里写了一个不在库里的名字（占位词
// 「通用能力」），LoadSkill → store.Get 撞到 QueryRow().Scan() 的 sql.ErrNoRows，
// 而那一格当时是原样上抛 —— 用户屏幕上只有一行「⚠ sql: no rows in result set」，
// 整轮零正文。
//
// 尺子钉在**错误契约**上，不是钉在措辞上：Get 找不到技能时必须返回
// ErrSkillNotFound（可被上层 errors.Is 认出来、当可降级情况处理），并且
// **不许把 sql 层的实现细节漏出去**。上层（chat.go 的 L3 兜底）就是照这个契约
// 写的降级分支，契约一改，那边就悄悄退回「整轮报错」。
func TestGetMissingSkillReturnsErrSkillNotFoundNotSQLNoRows(t *testing.T) {
	s := newStoreForNotFoundTest(t)

	// 前提：库本身是好的（拿一个真技能证明不是「全都查不到」）。
	if _, err := s.Get("技能工厂"); err != nil {
		t.Fatalf("前提不成立：内置技能「技能工厂」都读不出来（%v）—— "+
			"这样这条尺子会对着一个坏库判绿/判红，结论没有意义", err)
	}

	sk, err := s.Get("通用能力")
	if err == nil {
		t.Fatalf("不存在的技能竟然查出来了：%+v", sk)
	}
	if !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("找不到技能时返回的错误不是 ErrSkillNotFound，而是 %v —— "+
			"上层没法用 errors.Is 把「技能不存在」和「系统故障」分开，只能整轮报错", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("sql.ErrNoRows 直接漏出来了：%v —— 这是存储实现细节，"+
			"2026-09-26 线上就是它出现在用户屏幕上", err)
	}
	// 兜底再按文本查一次：sql 包的措辞就是「sql: no rows in result set」。
	if msg := err.Error(); strings.Contains(msg, "no rows") {
		t.Fatalf("错误文本里带着 sql 实现细节（%q）—— 这一句会被整段播给用户", msg)
	}
}
