package agent

import (
	"strconv"
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

// noMaterialChars 是「**材料真空**」的上限（字）。
//
// 它和 noAskMaterialChars(800) 一起把这一轮分成三段，判据都取本地数得准的事实：
//
//	< 80        → 真空：技能声明的必填项一个都没落实就必须停问（本文件的
//	              MissingRequired / ZeroMaterialAsk 就是给这一段写的）
//	80 ~ 799    → 灰区：照旧只看模型自报的 needs（不新增行为，不动既有判据）
//	>= 800      → 材料在手：不因为必填缺失拦人，改为带假设直写（MaterialNoAsk）
//
// 为什么是 80：一句话需求（「帮我写篇新闻稿」线上实测 4~40 字）够不到；而只要
// 用户把简报事实写进了消息（实测最短的一份 90 来字带齐了时间/主体/数据），
// 80 这条线就过了，不会再被问一遍 —— 「信息就在原文里还被问」是用户投诉的原话，
// 那条线绝不能往回退。
//
// 它是一个**判据**不是一个**参数**：调它就是改产品行为（要不要为了必填项停一轮），
// 所以写成常量 + 这里写清依据，不给环境变量留口子（同 noAskMaterialChars）。
const noMaterialChars = 80

// MissingRequired 取「技能声明必填、但这一轮**没拿到值**」的参数（本地地面真值）。
//
// 与 SplitNeeds 的分工：SplitNeeds 判的是「模型自报的这条 need 该不该拦」，
// 它**补不出模型没报的缺口** —— 而线上逮到的正是这个缺口：零材料 + 一句话指令，
// 分类器自报 needs 为空，于是 blocking 为空，写作跳一路直通，产出 645 字带假日期、
// 假参会人的会议纪要。声明表是地面真值，缺哪项本地就能算出来，不该等模型自觉。
//
// 名字归一（TrimSpace）跟 SplitNeeds 一致：模型回填常带空格。同名重复以第一条为准
// （params 表 ORDER BY position）。值表里**空串算没给**（前端表单空着提交就是 ""）。
// 没有任何必填项缺 → 回 nil（不是空切片），调用方按 len()==0 判不拦。
func MissingRequired(declared []model.Param, given map[string]string) []model.Param {
	if len(declared) == 0 {
		return nil
	}
	have := make(map[string]bool, len(given))
	for k, v := range given {
		if strings.TrimSpace(v) != "" {
			have[strings.TrimSpace(k)] = true
		}
	}
	var out []model.Param
	seen := make(map[string]bool, len(declared))
	for _, p := range declared {
		name := strings.TrimSpace(p.Name)
		if name == "" || !p.Required || seen[name] || have[name] {
			continue
		}
		seen[name] = true
		out = append(out, p)
	}
	return out
}

// GrantedFreeRein 判用户是否已经**明确授权**「别问，照你的理解写」。
//
// 它是零材料强制门的唯一出口，作用是**防止问成死循环**：停问文案让用户回一句
// 「就按你的」，那一轮的消息只有 4 个字 → 材料依然是真空、必填项依然没落实 →
// 没有这条判定就会再问一遍，用户永远走不到写作。授权是用户亲手写的判据，
// 不该由模型投票决定（同 ExplicitTextOnly 的设计）。
//
// 关键词只收**不会跟别的语境撞车**的那几个：实测反例是「直接写进模板」「直接输出正文」
// 这类指令里的「直接写」—— 收进来会把「用户其实没授权」的轮次放行，而放行的代价是
// 编事实（本门存在的理由）。所以宁可少收，用户的出口就锚在文案里那句「就按你的」。
func GrantedFreeRein(msg string) bool {
	h := strings.ToLower(strings.TrimSpace(msg))
	if h == "" {
		return false
	}
	for _, kw := range []string{
		"就按你的", "就按你说的", "按你的理解", "按你理解", "照你的理解", "照你理解", "依着你的理解",
		"你看着办", "你自己决定", "你来决定", "你决定就好", "你定就行",
		"自由发挥", "随便写", "随意写", "别问了", "不用问",
	} {
		if strings.Contains(h, kw) {
			return true
		}
	}
	return false
}

// ZeroMaterialAsk 是**零材料强制门**：材料真空 + 技能声明的必填项没落实 → 必须停问。
//
// 补的是 SplitNeeds 补不出的那个缺口（见 MissingRequired 的文件头注释）：
// 模型自报 needs 为空时，blocking 就是空的，闸门形同不存在。这里用声明表算一次。
//
// 只开「材料确实是空的」这一格 —— 不动「明明给了材料还在问」那条路径（那由
// MaterialNoAsk + 疑点回执负责），两条判据的区间不重叠（80 vs 800），
// 所以不存在「一个说要拦、一个说不拦」的打架。
//
// blankTemplate：本轮 action=template_only，交付物就是那个空白附件本身，跟
// 「必填项有没有落实」无关 —— 用户要的是文件，停问会把「发我个空白模板」拦成一句反问。
// 这是**缩打击面**：模板交付分支在闸门之后，不显式放过就会被误伤。
//
// 不增加模型调用：判据全在本地（声明表 + 参数值表 + 字数 + 关键词），t≈0 就能发。
// 内网模型 token 慢，这一格宁可本地算准，也不外包给疑点跳。
func ZeroMaterialAsk(declared []model.Param, given map[string]string, materialChars int,
	msg string, blankTemplate bool) []model.Param {
	// 三条豁免**分开写**（不并成一个 ||）：每一条都是一类「不该拦」的现场，
	// 自证脚本按行注入时也才打得准（见 internal/api/zero_material_gate_mutation_check.sh）。
	if blankTemplate {
		return nil
	}
	if materialChars >= noMaterialChars {
		return nil
	}
	if GrantedFreeRein(msg) {
		return nil
	}
	return MissingRequired(declared, given)
}

// ZeroMaterialMessage 拼零材料停问文案。
//
// 两条契约（都有守卫钉着，见 internal/api/chat_needs_gate_wiring_test.go）：
//   - **逐字引用用户这次的原话**：被判「没看见我给的信息」的那次投诉，根因是旧版
//     照技能清单拼通用要素清单（「我需要你补充以下信息：标题亮点」），跟用户说了什么
//     完全无关。零材料的这一跳尤其要引用 —— 用户唯一的输入就是那句话；
//   - **给一句话出口**（「就按你的」）：用户不想逐条答也能开工，且这正是
//     GrantedFreeRein 认的那个词，不能改文案却不改判词。
func ZeroMaterialMessage(msg string, missing []model.Param) string {
	quoted := strings.Join(strings.Fields(msg), " ")
	// 截断时省略号扔到**引号外面**：引号里那截必须逐字来自用户原话。
	// （线上尺子 web/tests/chat_doubts_e2e.py 的 Q4b 会拿文案里每个「」回查原话，
	//   展示一个假引用比不展示更糟 —— 用户会以为那是自己说过的话。）
	tail := ""
	if r := []rune(quoted); len(r) > 40 {
		quoted, tail = string(r[:40]), "…"
	}
	var b strings.Builder
	b.WriteString("你这次说的是「" + quoted + "」" + tail + "，但这条消息里没有可用的成篇材料（我这边只收到 ")
	b.WriteString(strconv.Itoa(len([]rune(msg))))
	b.WriteString(" 字指令），直接动笔就只能替你编事实 —— 日期、参会人、数字这类具体信息我不想凭空造。\n\n")
	b.WriteString("要动手还缺：**" + NeedsSummary(missing) + "**。\n\n")
	b.WriteString("把这些信息贴给我（越全越好，粘一整篇进来最好），或者回一句「就按你的」，我就按通用写法先起一稿、把拿不准的具体信息留成可替换的占位。")
	return b.String()
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
