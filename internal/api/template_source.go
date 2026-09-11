package api

import (
	"errors"

	"github.com/lizhemin15/skillforge/internal/store"
	"github.com/lizhemin15/skillforge/internal/tools"
)

// errNoStore 在技能库未接入时报错（只可能出现在测试里）。
var errNoStore = errors.New("技能库未接入")

// storeTemplates 把技能库适配成 tools.TemplateSource。
//
// 为什么要这层适配：tools 包不能 import store（agent 包已经 import 了 tools，
// 而 agent 也依赖 store 的类型，直接互相 import 会成环）。所以 tools 侧定义
// 一个只有两个方法的窄接口，由 api 层在这里做一次翻译。
//
// 每次调用都重新扫盘，不做缓存：模板是用户随时上传/删除的，缓存会让模型
// 拿到过期的模板清单（「有个模板填不了」这种 bug 最难查）。
type storeTemplates struct{ s *store.SkillStore }

func newStoreTemplates(s *store.SkillStore) storeTemplates {
	return storeTemplates{s: s}
}

// Templates 返回当前技能库里所有可填模板。
// 扫盘失败按空处理：工具报错会让模型卡死，而「没有模板」是模型能理解的正常状态。
func (a storeTemplates) Templates() []tools.TemplateRef {
	if a.s == nil {
		return nil
	}
	files, err := a.s.ListTemplates()
	if err != nil {
		return nil
	}
	out := make([]tools.TemplateRef, 0, len(files))
	for _, f := range files {
		out = append(out, tools.TemplateRef{
			Slug:   f.Slug,
			Rel:    f.Rel,
			Name:   f.Name,
			Format: f.Format,
			Size:   f.Size,
		})
	}
	return out
}

// ReadTemplate 按 slug + 相对路径读出模板字节。
func (a storeTemplates) ReadTemplate(slug, rel string) ([]byte, error) {
	if a.s == nil {
		return nil, errNoStore
	}
	return a.s.ReadTemplate(slug, rel)
}
