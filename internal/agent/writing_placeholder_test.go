package agent

import (
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/store"
)

// 手册范文里的 {公司名称}/{日期} 这类占位符，必须被提示词显式禁止原样输出。
// 真实线上技能（公司新闻通稿）的范文就带这种占位符，实测模型会连占位符一起照抄，
// 即便本轮用户已经给了「发布单位星河科技、发布日期 2026 年 9 月 14 日」——
// 用户看到的观感就是「它没管我说的话」。
func TestPackBlockBansPlaceholders(t *testing.T) {
	pack := &WritePack{Categories: []WriteCategory{{
		CategoryDoc: store.CategoryDoc{
			File:        "categories/01-新闻通稿.md",
			Name:        "企业新闻通稿",
			Trigger:     "需要对外发布产品/技术消息时",
			Requirement: "正文分三段：发布背景、技术亮点、行业影响。",
		},
		Examples: []store.CategoryExample{{
			Path:    "examples/新闻通稿/01.md",
			Content: "{公司名称}于{日期}正式发布{产品名}。",
		}},
	}}}
	cat := pack.FindCategory("企业新闻通稿")
	if cat == nil {
		t.Fatal("FindCategory 没命中，用例自身不成立")
	}
	got := packBlock(pack, cat)

	for _, want := range []string{"企业新闻通稿", "发布背景", "{公司名称}于{日期}正式发布"} {
		if !strings.Contains(got, want) {
			t.Fatalf("packBlock 少了 %q（说明用例没读到真实注入块）", want)
		}
	}
	if !strings.Contains(got, "绝不允许把占位符原样写进正文") {
		t.Fatalf("packBlock 没禁止原样输出占位符 —— 这正是 {公司名称}/{日期} 漏到成稿的原因")
	}
	if !strings.Contains(got, "事实只能来自用户提供的素材") {
		t.Fatalf("原有的「范文事实不得搬用」约束被挤掉了")
	}
}
