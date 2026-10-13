package api

import (
	"fmt"
	"log"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/lizhemin15/skillforge/internal/agent"
	"github.com/lizhemin15/skillforge/internal/model"
)

// 本文件是**附件下发的唯一出口**（帧怎么拼、URL 怎么拼、清单怎么截断）。
//
// 为什么收敛成一个函数：下发点有三处——技能链尾、手册写作链尾、template_only 短路。
// 历史上这三处各写各的 `write(evFile, ...)`，于是同一个多附件技能在不同链路上
// 发的文件不一样（模板短路只发主附件、写作链路发另一个），用户看到的是一套
// 说不清的行为。加规则只改这里。

// declaredOf 取一份技能**声明的**可下发附件清单（下载端点的白名单来源）。
//
// store 在 Get/List 时已经把旧单字段折成数组一项，所以正常情况直接返回数组；
// 这里再兜一次旧列是为了防御"直接构造 Skill 的调用方/老二进制写下的行"，
// 清单少了会让一个本来能下的文件 404，多兜一次的成本近乎零。
func declaredOf(sk *model.Skill) []model.Attachment {
	if sk == nil {
		return nil
	}
	if len(sk.Attachments) > 0 {
		return sk.Attachments
	}
	legacy := strings.TrimSpace(sk.Attachment)
	if legacy == "" {
		return nil
	}
	return []model.Attachment{{
		Name: filepath.Base(filepath.FromSlash(legacy)),
		Rel:  legacy,
	}}
}

// deliverAttachments 把本轮定出的附件逐份写成 evFile 帧，返回实际下发的份数。
//
// 逐份独立成帧（而不是塞进一张卡片里的多行）：前端每个 file 帧 = 一条独立的
// assistant 消息，各有下载按钮、单独可点；合并渲染需要前端配合，而下发链路
// 与前端是两个发布节奏，独立帧是最小耦合面。
//
// 份数超上限（agent.DeliverMaxFiles）时**出声**报「还有 N 份」：静默少发等于
// 告诉用户「这文件没拆出来」，而真相是拆出来了、只是没一次全列。
func deliverAttachments(write func(ev, data string), slug string, atts []model.Attachment) int {
	kept, dropped := agent.TruncateDeliverables(atts)

	// 没有落盘路径（Rel 空）的条目**发不出**：端点要按名字回查清单再解析文件，
	// Rel 空必然解析失败 → 用户拿到一张点开就 404 的卡。这种卡片比不发更坏
	// （用户以为文件坏了、反复点）。所以在这里滤掉，并留一行服务端日志：
	// 「少发一份」在用户侧是看不出来的，只有这行日志能回答管理员那句
	// 「为什么我的技能少发了一份」。
	if kept = validDeliverables(kept, slug); len(kept) == 0 {
		return 0
	}
	if len(kept) > 1 {
		// 先报总数再出卡片：用户看到"共 N 份"就知道后面还会滚出几张卡，
		// 不会以为第一张就是全部、把剩下的当成重复。
		write(evDelta, jsonSafe(map[string]string{
			"t": fmt.Sprintf("另外有 %d 份文件可下载：\n", len(kept)),
		}))
	}
	for _, a := range kept {
		name := agent.AttachDisplayName(a)
		write(evFile, jsonSafe(map[string]string{
			"slug": slug,
			"name": name,
			// 带 file= 是因为多附件技能必须能区分"发哪一份"；
			// 只带 slug 的地址仍然可用（端点不带 file= 时发第一份），
			// 这样旧前端/旧卡片不会 404。
			"url":  attachURL(slug, name),
			"kind": "attach",
		}))
	}
	if dropped > 0 {
		write(evDelta, jsonSafe(map[string]string{
			"t": fmt.Sprintf("（附件清单里还有 %d 份没列出，回复文件名即可单独下载。）", dropped),
		}))
	}
	return len(kept)
}

// validDeliverables 滤掉「登记不完整、实际发不出去」的条目（Rel 为空）。
//
// 单独成函数而不是内联，是为了让「什么算可下发」只有一个定义：端点的白名单、
// 帧的生成、截断计数都该用同一把尺子量，否则会出现"卡片发了但端点 404"
// 这种两侧不一致的错——它在单侧测试里永远绿。
func validDeliverables(atts []model.Attachment, slug string) []model.Attachment {
	out := make([]model.Attachment, 0, len(atts))
	var broken []string
	for _, a := range atts {
		if strings.TrimSpace(a.Rel) == "" {
			broken = append(broken, agent.AttachDisplayName(a))
			continue
		}
		out = append(out, a)
	}
	if len(broken) > 0 {
		log.Printf("附件下发：slug=%s 有 %d 份附件缺落盘路径，已跳过（用户侧不可见）：%v",
			slug, len(broken), broken)
	}
	return out
}

// attachURL 拼公开附件端点的地址。
//
// slug 与文件名都过一遍转义：文件名绝大多数是中文（"附件2-报名表.docx"），
// 直接拼进 query 会因未转义的字符被截断（空格、#、& 出现即炸），
// 表现是「卡片在、点下载 404」——最难查的那种错。
func attachURL(slug, name string) string {
	base := "/api/chat/attachment/" + url.PathEscape(slug)
	name = strings.TrimSpace(name)
	if name == "" {
		return base
	}
	return base + "?file=" + url.QueryEscape(name)
}
