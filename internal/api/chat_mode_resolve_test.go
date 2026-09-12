package api

import (
	"errors"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/agent"
)

// 技能失效降级：锁定技能后技能被删/被停用，不能静默换技能。
// 用户界面上表现为：手动档选的技能已经不在了，如果一声不吭，
// 用户会以为"我锁定的技能在干活"，实际跑的是另一个技能 —— 结果全错还找不到原因。
func TestResolveModeDegradeWhenSkillMissing(t *testing.T) {
	boom := errors.New("skill not found")
	cases := []struct {
		name      string
		mode      string
		slug      string
		load      func(string) (*agent.SkillContent, error)
		wantSkill bool
		wantNote  string // 期望 note 包含的关键信息（"" 表示必须没有 note）
	}{
		{"手动+技能在 → 按锁定技能跑", "manual", "采购合同",
			func(s string) (*agent.SkillContent, error) { return &agent.SkillContent{Slug: s}, nil }, true, ""},
		{"手动+技能不存在 → 降级且说明原因", "manual", "已删除的技能",
			func(string) (*agent.SkillContent, error) { return nil, boom }, false, "已删除的技能"},
		{"手动+技能不存在 → 说明里要讲清已改用自动", "manual", "x",
			func(string) (*agent.SkillContent, error) { return nil, boom }, false, "自动调度"},
		{"手动+loader 返回 nil → 也当不可用", "manual", "x",
			func(string) (*agent.SkillContent, error) { return nil, nil }, false, "自动调度"},
		{"手动+没给 slug → 自动，不用提示", "manual", "",
			func(string) (*agent.SkillContent, error) { return nil, boom }, false, ""},
		{"自动档 → 不带技能、不打扰用户", "auto", "采购合同",
			func(s string) (*agent.SkillContent, error) { return &agent.SkillContent{Slug: s}, nil }, false, ""},
		{"大小写/空格脏输入也能用", "  MANUAL ", " 采购合同 ",
			func(s string) (*agent.SkillContent, error) { return &agent.SkillContent{Slug: s}, nil }, true, ""},
	}
	for _, c := range cases {
		sc, mode, note := resolveMode(c.mode, c.slug, c.load)
		if (sc != nil) != c.wantSkill {
			t.Fatalf("%s: 技能解析错了: got %v", c.name, sc)
		}
		if c.wantSkill && mode != "manual" {
			t.Fatalf("%s: 技能可用时模式应为 manual，got %q", c.name, mode)
		}
		if !c.wantSkill && mode != "auto" {
			t.Fatalf("%s: 降级后模式必须是 auto，got %q", c.name, mode)
		}
		if c.wantNote == "" {
			if note != "" {
				t.Fatalf("%s: 不该有说明，got %q", c.name, note)
			}
			continue
		}
		if !strings.Contains(note, c.wantNote) {
			t.Fatalf("%s: 说明里没提到 %q，got %q", c.name, c.wantNote, note)
		}
	}
}

// 降级说明必须点名是哪个技能 —— 用户可能锁了好几个技能，
// 只说"技能不可用"会让人对着技能列表挨个猜。
func TestResolveModeNoteNamesTheSkill(t *testing.T) {
	_, _, note := resolveMode("manual", "公司新闻通稿", func(string) (*agent.SkillContent, error) {
		return nil, errors.New("gone")
	})
	if !strings.Contains(note, "公司新闻通稿") {
		t.Fatalf("说明没点名技能: %q", note)
	}
}

// load 为 nil 不能 panic（比如引擎还没初始化就来了一个手动请求）。
func TestResolveModeNilLoaderDoesNotPanic(t *testing.T) {
	sc, mode, note := resolveMode("manual", "采购合同", nil)
	if sc != nil || mode != "auto" || note != "" {
		t.Fatalf("nil loader 处理错了: sc=%v mode=%q note=%q", sc, mode, note)
	}
}
