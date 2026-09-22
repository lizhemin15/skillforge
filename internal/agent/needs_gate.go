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

// MaterialInHand 数「用户此刻已经交了多少成篇材料」（单位：字）。
//
// 判据只认字符数这种地面真值，不认模型的自觉。两个来源取大的那份：
//
//	本轮消息 —— 用户直接把素材贴在这一轮里；
//	注入上下文块 —— 素材/产物是更早轮次贴的，被 ContextBlock 注进来（「把上面那篇
//	整理成 Word」就属于这类：消息本身十来个字，材料全在上下文块里）。
//
// 小于号那种「两个都算上」的写法是错的：素材轮的消息本身就一万字，注入块里还带着
// 压缩后的同一份东西，加起来会把门槛推得虚高，判据就不稳了。
func MaterialInHand(user, ctxBlock string) int {
	u, c := len([]rune(user)), len([]rune(ctxBlock))
	if u > c {
		return u
	}
	return c
}

// noAskMaterialChars 是「材料在手，就别再拿必填项拦人」的字数门槛。
//
// 为什么是 800：
//
//	手打的一句话需求（「帮我写篇新闻稿」）量级 4~40 字，永远够不到；
//	线上素材轮实测 user=10340~10432 字、注入块 11235 字，两头都远超；
//	800 落在两者中间，且比任何「一句话请求」大一个量级以上。
//
// 它是一个**判据**不是一个**参数**：调它就是改产品行为（要拦还是要放），所以写成
// 常量 + 这里写清依据，不给环境变量留口子。
const noAskMaterialChars = 800

// MaterialNoAsk 判「材料在手，所以不因为必填项缺失拦下这一轮」。
//
// 这是把 needs 闸门从**模型自觉**改成**确定性**的那一刀。理由是线上实测（2026-09-22）：
// 同一份 10360 字素材 + 同一句写稿指令连发 6 次，模型自报的 needs 在 0/1/3/4/6 之间
// 抖动；报 3 的那次整轮 2.5 秒结束，只回一句「我需要你补充以下信息：…」，正文一字
// 没有 —— 用户读到的是「它像没看到我给的信息」「还在问我要信息」。判定权交给模型，
// 产品行为就是随机的；而「用户给了多少材料」本地数得准，就该用本地的事实下闸。
//
// 不拦不等于骗人：调用方会把「哪几项没在素材里写明、我按素材自行推断」当成中间材料
// 流式吐出去，假设摆在明面上，用户随时能纠正。对比一下两种代价：
//
//	拦下来 —— 用户白等一轮，且以为自己的材料丢了（不可感知、不可纠正）；
//	放过去 —— 字段靠推断，但用户看得见这个假设（可感知、可纠正）。
//
// 为什么不是「凡是长材料就一律放行」：门内只放**必填项缺失**这一个判定，其余校验
// 照旧（NeedsTools 那条工具链、写稿前的参数整理都不受影响）。声明说必填、素材里又
// 真没写，且用户压根没给材料（短消息）时，闸门照旧拦 —— 那种情况追问是对的。
func MaterialNoAsk(materialChars int) bool {
	return materialChars >= noAskMaterialChars
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
