package skillgen

import (
	"context"
	"strconv"
	"strings"
)

// 交付物卫生：生成的 system_prompt 里**不得要求成稿附带核对清单 / 自证附录**。
//
// 为什么要有这道机械校验（而不是只写在母模板里靠模型自觉）：
// 实测 v0.3.16 交付的提示词第 44 行写着「成稿正文结束后，换行附加【逐条核对清单】
// 与【手册原文范文引用】，用于合规性自证」。那不是模型在跑试用时自作主张，而是
// **提示词自己要求的** —— 手册模式下的生成器屡次自发把「合规性自证」写成交付要求，
// 线上 6 份手册类技能的提示词里都有（2~10 处），非手册类为 0。后果是成稿尾部跟着
// 交出一份核对清单，裁判扣「自查清单擅自引入手册未载明的标准」。
//
// 光靠母模板禁令不够：模型可以无视（实测在提示词层面出现自相矛盾的指令时，
// 后置指令胜出，成稿仍 3/5 带清单）。所以这里做成可判定的检查，命中就修，
// 修不掉就机械剔除并留下告警 —— 绝不静默放行。

// hygieneViolationPatterns 是「要求把合规性自证输出到成稿」的措辞。
// 刻意只收指向**交付物**的说法，不收「逐条核对」这种内部动作（那份是合法的自检要求）。
var hygieneViolationPatterns = []string{
	"核对清单",
	"校验清单",
	"合规性自证",
	"输出附录",
	"附录机制",
	"范文引用",
	"自证附录",
	"核对结果",
}

// hygieneNegations 标记「这句本身就是禁令」，含其一的句子一律不算违规。
// 少了这一条，我们自己写进母模板的「不得要求成稿附带核对清单」会被自己判违规 ——
// 这不是假想：第一版检查就踩了这个坑，母模板的禁令行自己被判成违规。
var hygieneNegations = []string{"不得", "禁止", "不要", "严禁", "无需", "勿", "不作", "避免"}

// promptHygieneViolations 返回违规行（原样，便于日志与人工复核）。
func promptHygieneViolations(prompt string) []string {
	var bad []string
	for _, line := range strings.Split(prompt, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if containsAny(t, hygieneNegations) {
			continue
		}
		if containsAny(t, hygieneViolationPatterns) {
			bad = append(bad, t)
		}
	}
	return bad
}

// stripHygieneViolations 是兜底动作：整行删掉违规行。
// 只在「清理调用也没清干净」时用 —— 保守（删）比放任（交付带清单的稿子）好，
// 但调用方必须把删除条数写进 trace，不能让用户以为一切正常。
func stripHygieneViolations(prompt string) (string, int) {
	var keep []string
	n := 0
	for _, line := range strings.Split(prompt, "\n") {
		t := strings.TrimSpace(line)
		if t != "" && !containsAny(t, hygieneNegations) && containsAny(t, hygieneViolationPatterns) {
			n++
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n"), n
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// enforcePromptHygiene 是生成流程的交付物卫生硬门：命中 → 一次外科式清理调用 →
// 复查 → 仍未清干净就机械剔除并告警。返回的提示词可以直接落盘。
func (g *Generator) enforcePromptHygiene(ctx context.Context, prompt string, steps func(string)) (string, error) {
	bad := promptHygieneViolations(prompt)
	if len(bad) == 0 {
		return prompt, nil
	}
	steps("5.5/9 ⚠️ 提示词含 " + strconv.Itoa(len(bad)) + " 处「要求成稿附核对清单/自证附录」——清理中")
	if g.llm != nil {
		sys := `你是提示词修订器：删掉不该有的要求，其余一字不改。
输入是一份写作技能的 system_prompt。它可能夹着「成稿后附加核对清单 / 自证附录 / 范文引用附录」这类要求。
规则:
1. 只删除这类要求所在的句子或列表项(含仅为它服务的子项),其余内容**逐字保留**,包括标题、编号、顺序、措辞、标点。
2. 不得新增任何要求,不得改写其他句子,不要重排结构。
3. 若输入里没有这类要求,原样输出。
4. 直接输出修订后的完整提示词正文,不要解释、不要代码块包裹。`
		if out, err := g.llm.Chat(ctx, sys, prompt); err == nil {
			rev := cleanCodeFence(out)
			if strings.TrimSpace(rev) != "" && len(promptHygieneViolations(rev)) < len(bad) {
				prompt = rev
				bad = promptHygieneViolations(prompt)
			}
		}
	}
	if len(bad) > 0 {
		p2, n := stripHygieneViolations(prompt)
		steps("5.5/9 ⚠️ 清理后仍有 " + strconv.Itoa(len(bad)) + " 处，机械剔除 " + strconv.Itoa(n) + " 行")
		prompt = p2
	}
	if left := promptHygieneViolations(prompt); len(left) > 0 {
		// 走到这里说明剔除逻辑本身失效了。宁可报错，也不交付一份会带清单的提示词。
		return "", errHygieneUnfixable(left)
	}
	steps("5.5/9 交付物卫生已达标（成稿不含核对清单/自证附录）")
	return prompt, nil
}

type hygieneErr struct{ left []string }

func (e hygieneErr) Error() string {
	return "交付物卫生校验未通过，剩余违规 " + strconv.Itoa(len(e.left)) + " 处：" + truncateLine(e.left[0], 120)
}

func errHygieneUnfixable(left []string) error { return hygieneErr{left: left} }

func truncateLine(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
