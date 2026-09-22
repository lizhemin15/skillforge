package agent

import (
	"strings"

	"github.com/lizhemin15/skillforge/internal/model"
)

// SplitNeeds 把分类器报上来的 needs 分成两拨：真该拦下这一轮的（必填缺参）
// 和**不该拦**的（可选字段没填）。
//
// 为什么需要这道过滤（2026-09-22 线上实测逮到）：
//
//	needs 是分类器照技能清单回填的，它会把用户没填的**可选**字段也塞进来。线上
//	「贴完一万字素材、再提写作需求」的第 2 轮就这么被拦掉：技能「公司新闻通稿」的
//	可选字段「标题亮点（可选，未提供则生成）」被判成缺参 → 整轮 4.5 秒结束，只回一句
//	「我需要你补充以下信息：标题亮点（可选，未提供则生成）」，正文一个字都没有。
//	用户读到的是「像没看到我给他的信息」「还在问我要信息」—— 而技能自己的标签写着
//	「未提供则生成」，产品在这儿是自相矛盾的。
//
// 判据取**技能声明的 input_params**（地面真值），模型自报的 Required 只当兜底：
//
//	声明说可选、模型说必填 → 不拦（白拦一轮是用户唯一能感知的伤害）；
//	声明说必填、模型说可选 → 拦（宁可拦，也不能拿空字段去套模板）；
//	声明里没这个名字（清单漂移）→ 按 need.Required 判。
//
// 为什么不干脆「docgen 一律不拦」：技能工厂新建的技能可以自己声明必填参数，
// 一刀切会让那些技能拿空值去套模板，产出一份看着像样、字段全空的文档 ——
// 那是另一种更隐蔽的「生成的 skill 和我给的内容完全没关系」。
func SplitNeeds(declared, needs []model.Param) (blocking, optional []model.Param) {
	if len(needs) == 0 {
		return nil, nil
	}
	// 声明表按 name 索引；同名重复以第一条为准（ORDER BY position，即表单顺序）。
	req := make(map[string]bool, len(declared))
	for _, p := range declared {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			continue
		}
		if _, dup := req[name]; dup {
			continue
		}
		req[name] = p.Required
	}
	blocking = make([]model.Param, 0, len(needs))
	optional = make([]model.Param, 0, len(needs))
	for _, n := range needs {
		must := n.Required
		if dv, known := req[strings.TrimSpace(n.Name)]; known {
			must = dv
		}
		if must {
			blocking = append(blocking, n)
			continue
		}
		optional = append(optional, n)
	}
	return blocking, optional
}

// NeedsSummary 取一组 need 的名字，逗号分隔（给中间材料/等待提示用）。
// 只用 label 里最靠前的短名，避免把「标题亮点（可选，未提供则生成）」整段贴进
// 步骤条 —— 那一格宽度只够几个字。
func NeedsSummary(needs []model.Param) string {
	names := make([]string, 0, len(needs))
	for _, n := range needs {
		s := strings.TrimSpace(n.Label)
		if s == "" {
			s = strings.TrimSpace(n.Name)
		}
		if i := strings.IndexAny(s, "（(，,"); i > 0 {
			s = strings.TrimSpace(s[:i])
		}
		if s != "" {
			names = append(names, s)
		}
	}
	return strings.Join(names, "、")
}
