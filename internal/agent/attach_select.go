package agent

import (
	"path/filepath"
	"strings"

	"github.com/lizhemin15/skillforge/internal/model"
)

// 本文件管「本轮到底该把哪几份附件发给用户」——纯函数，不碰网络、不碰磁盘，
// 就是为了能被测试钉住。
//
// 为什么需要一层独立的选件逻辑（而不是沿着老的 `sc.Attachment` 单字段继续跑）：
//
//	老的模型里一份技能**最多只有一份**交付文件（`attachment` 列 = 管理员上传的
//	那份源文件），所以运行时的写法是「有就发」，没有任何选择余地。新场景是从
//	一份通知/方案 docx 里拆出**多份**内嵌附件（"附件：1.报名表 2.汇总表"），
//	用户会点名要其中一份（"发我报名表"）。这时候「有就发」变成两个坑：
//	  ① 发错对象：用户要报名表，收到汇总表 —— 比不发更坏，用户会以为系统读错了文件；
//	  ② 全塞给用户：把没要的几份一起丢过去，聊天里一排文件卡片。
//
// 所以规则是「点名优先、单份兜底、多份只在笼统要模板时才全发」，
// 且**点名对不上时宁可不说发**（除清单只有一份，那种情况不可能发错对象）。

// DeliverMaxFiles 是一次回复里最多下发的附件份数。
//
// 阈值不是性能考虑，是**人眼**考虑：聊天窗口里连排 8 个文件卡片已经要滚屏了，
// 而提取上限是 20 份。超出部分由 chat 层出一句「清单里还有 N 份」让用户点名要，
// 而不是静默丢掉（静默丢 = 用户以为拆分漏了）。
const DeliverMaxFiles = 8

// MatchAttachments 把用户/分类器点名的文件（want）映射到技能已声明的附件（declared）。
//
// 返回值：picked 是命中的附件（按 want 的顺序、按 Rel 去重）；unknown 是**没对上**的原名，
// 调用方拿它来出声（"没找到附件「X」，现有：A、B"）—— 不出声就是用户以为系统没理他。
//
// 匹配按「严 → 松」五档逐轮尝试，越靠后越宽容：
//
//	① Rel 原样（分类器照抄 roster 里印的 rel 时走这条）
//	② Name 原样（最常见：模型照抄展示名"报名表.docx"）
//	③ filepath.Base(Rel)（Name 为空的老数据、只存了路径的那种）
//	④ 去扩展名后相等（模型把 .docx 省掉了："报名表"）
//	⑤ 唯一包含（用户说"附件2那份"，而文件叫"附件2-报名表.docx"）
//
// 第⑤档**必须唯一**才认：包含匹配天然会把"报名表"同时对上"报名表.docx"和
// "报名表(模板).docx"，歧义时挑一个就是掷骰子 —— 宁可不发并如实报「没找到确切的那份」，
// 也不许把用户没要的那份塞过去。
func MatchAttachments(declared []model.Attachment, want []string) (picked []model.Attachment, unknown []string) {
	if len(declared) == 0 || len(want) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, w := range want {
		key := normFileName(w)
		if key == "" {
			continue
		}
		hit, ok := matchOne(declared, key)
		if !ok {
			unknown = append(unknown, strings.TrimSpace(w))
			continue
		}
		if seen[hit.Rel] {
			continue
		}
		seen[hit.Rel] = true
		picked = append(picked, hit)
	}
	return picked, unknown
}

// matchOne 在 declared 里找 want 对应的那一份，五档依次尝试。
func matchOne(declared []model.Attachment, key string) (model.Attachment, bool) {
	// ①②③④：确定性相等口径。
	for _, pass := range []func(model.Attachment) string{
		func(a model.Attachment) string { return normFileName(a.Rel) },
		func(a model.Attachment) string { return normFileName(a.Name) },
		func(a model.Attachment) string { return normFileName(filepath.Base(filepath.FromSlash(a.Rel))) },
		func(a model.Attachment) string { return strings.TrimSuffix(normFileName(a.Name), normExt(a.Name)) },
	} {
		for _, a := range declared {
			if got := pass(a); got != "" && got == key {
				return a, true
			}
		}
	}
	// ⑤：唯一包含。key 去扩展名后再比，避免".docx"把毫不相干的文件也包含进来。
	stem := strings.TrimSuffix(key, normExt(key))
	if stem == "" {
		return model.Attachment{}, false
	}
	var cand model.Attachment
	n := 0
	for _, a := range declared {
		hay := normFileName(a.Name)
		if hay == "" {
			hay = normFileName(filepath.Base(filepath.FromSlash(a.Rel)))
		}
		if strings.Contains(hay, stem) {
			cand = a
			n++
		}
	}
	if n == 1 {
		return cand, true
	}
	return model.Attachment{}, false
}

// ResolveDeliverables 定出本轮要下发的附件清单。files 是分类器给出的点名文件
// （可能为空），action 是本轮动作（template_only 表示用户就是在要模板文件本身）。
//
// 规则（顺序即优先级）：
//
//  0. 没声明附件 → 空（绝大多数写作技能走这条）
//  1. 用户点了名 → 命中的那几份；**一个都对不上时**：清单只有一份才兜底发它，
//     多份则什么也不发（要 A 给 B 是最坏结果，宁可如实说没找到）
//  2. 用户没点名且清单只有一份 → 发它（老行为：那份就是管理员上传的源文件，
//     历史上一直随回答下发，不能因为这次改造让老技能突然不发文件）
//  3. 用户没点名、清单多份、且用户是在要模板（template_only）→ 全发，
//     由调用方按 DeliverMaxFiles 截断并出声
//  4. 其余（多份、没点名、也不是在要模板）→ 空。多份附件技能不再「有就发」：
//     用户问「怎么报名」时收到一叠表格，是这次改造要消掉的噪音。
func ResolveDeliverables(sk *model.Skill, files []string, action string) []model.Attachment {
	if sk == nil {
		return nil
	}
	// store 的读路径已把旧的单字段 attachment 折进 Attachments（见
	// store.decodeAttachments），所以这里只认 Attachments 一处，不分叉。
	declared := sk.Attachments
	if len(declared) == 0 {
		return nil
	}
	want := trimAll(files)
	if len(want) > 0 {
		picked, _ := MatchAttachments(declared, want)
		if len(picked) > 0 {
			return picked
		}
		if len(declared) == 1 {
			return []model.Attachment{declared[0]}
		}
		return nil
	}
	if len(declared) == 1 {
		return []model.Attachment{declared[0]}
	}
	if strings.EqualFold(strings.TrimSpace(action), "template_only") {
		return declared
	}
	return nil
}

// TruncateDeliverables 按 DeliverMaxFiles 截断，并报出被截掉几份。
// 调用方必须用 dropped 出声：静默少发等于告诉用户「这文件没拆出来」。
func TruncateDeliverables(in []model.Attachment) (kept []model.Attachment, dropped int) {
	if len(in) <= DeliverMaxFiles {
		return in, 0
	}
	return in[:DeliverMaxFiles], len(in) - DeliverMaxFiles
}

// UnknownFileNote 拼一句「没找到附件」的用户提示，并列出**实际可下发的清单**。
// 只说「没找到」是死路：用户不知道是自己叫错名字还是系统拆漏了；
// 列出现有清单，他能立刻改口再要一次。
func UnknownFileNote(unknown []string, declared []model.Attachment) string {
	if len(unknown) == 0 {
		return ""
	}
	have := make([]string, 0, len(declared))
	for _, a := range declared {
		have = append(have, displayName(a))
	}
	if len(have) == 0 {
		return "· 没找到附件「" + strings.Join(unknown, "」「") + "」"
	}
	return "· 没找到附件「" + strings.Join(unknown, "」「") + "」，这份技能能发的是：" +
		strings.Join(have, "、")
}

// displayName 取附件的展示名（Name 为空的老数据退到 Rel 的 basename）。
func displayName(a model.Attachment) string {
	if s := strings.TrimSpace(a.Name); s != "" {
		return s
	}
	return filepath.Base(filepath.FromSlash(a.Rel))
}

// AttachDisplayName 是 displayName 的导出壳，给 api 层用。
//
// 为什么必须走这个壳而不是让调用方自己取 a.Name：下发帧里的 name 会被前端
// 直接渲染成卡片标题、也拼进 ?file= 参数回查白名单，两处都必须和 roster 里
// 印给模型/用户的名字**逐字一致**；老数据 Name 可能为空（只有 Rel），
// 各自实现必然漂移出「卡片标题空、点下载 404」。
func AttachDisplayName(a model.Attachment) string { return displayName(a) }

// AttachNames 把清单压成「报名表.docx、汇总表.xlsx」这种可读串，
// 用于步骤日志、会话历史与用户回执（用户要看得见"我到底拿到了哪几份"）。
func AttachNames(atts []model.Attachment) string {
	return strings.Join(attachDisplayNames(atts), "、")
}

// attachDisplayNames 取清单里每一项的展示名（跳过取不出名字的坏项）。
// 单独抽出来是因为「拼成一句」和「逐项列进 prompt」两种用法都需要，
// 但拼接方式不同（一个用、一个用《》），别让调用方各自拆字符串。
func attachDisplayNames(atts []model.Attachment) []string {
	out := make([]string, 0, len(atts))
	for _, a := range atts {
		if n := displayName(a); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// trimAll 去掉空白项（模型偶尔吐 ["", "报名表.docx"] 这种带空串的数组）。
func trimAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// normFileName 归一化文件名做比较：去空白、去《》引号、统一小写、把反斜杠当路径分隔符、
// 只留最后一段路径。**不**去扩展名 —— 去扩展名是单独一档，混在一起会让
// "a.docx" 和 "a.xlsx" 互相冒充。
func normFileName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "《》\"'`")
	s = strings.ReplaceAll(s, "\\", "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// normExt 返回小写扩展名（含点）；无扩展名返回空串。
func normExt(name string) string {
	return strings.ToLower(filepath.Ext(strings.TrimSpace(name)))
}
