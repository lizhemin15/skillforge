package skillgen

// 事故锚（用户投诉原话）：「现在速度过于慢了，中间可以流式输出思考的一些中间材料，
// 现在一直卡着计时，用户体验不佳」。
//
// 流水线只在**阶段边界**发一条进度；阶段内部是分钟级的模型调用。8.5/9 裁判每轮
// 「试用写稿 + 独立评分」是全流程最长的一段，`buildReviewer` 要通读手册原文拟审稿
// 清单，`enforcePromptHygiene` 要逐字修订整份提示词 —— 这几处原先走的是静默阻塞
// `g.llm.Chat`，用户在界面上只看到一个跳秒的计时器，分不清「在慢慢想」还是「卡死了」。
//
// 本文件守两件事，缺一不可：
//   1. 行为面：这三个长阶段**真的**走了流式，且推理链/正文片段确实被转发到接收器；
//   2. 结构面：训练链路里不允许再冒出「没接流式的阻塞调用」——新增的长调用忘了接，
//      正是这次事故的成因，靠人眼 review 拦不住，交给源码守卫。

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// collectStream 造一个收集材料片段的 ctx；返回的 kinds 里每项形如 "think:先看"。
func collectStream() (context.Context, *[]string) {
	var mu sync.Mutex
	got := &[]string{}
	ctx := WithDelta(context.Background(), func(kind, text string) {
		mu.Lock()
		defer mu.Unlock()
		*got = append(*got, kind+":"+text)
	})
	return ctx, got
}

func joined(got *[]string) string {
	return strings.Join(*got, "|")
}

// TestTrialDraftStreamsMaterial 守 8.5/9 裁判的「试用写稿」这一段。
//
// 断言钉在「走哪条路 + 材料有没有真的到接收器」上，而不是「模型今天写了什么」。
// 替身 fakeStreamer 会真回调 OnReasoning/OnContent（不回调就等于把被测行为整个
// 跳过，测试会假绿）。
func TestTrialDraftStreamsMaterial(t *testing.T) {
	f := &fakeStreamer{streamReturn: "  关于压降库存的专题会会议纪要  "}
	g := &Generator{llm: f}
	ctx, got := collectStream()

	out, err := g.trialDraft(ctx, "技能自己的提示词", synthPack(), Category{Name: "会议纪要"}, "素材正文")
	if err != nil {
		t.Fatalf("trialDraft 不该报错：%v", err)
	}
	if f.streamCalls != 1 || f.chatCalls != 0 {
		t.Fatalf("试用写稿必须走流式：streamCalls=%d chatCalls=%d（chatCalls>0 说明退回静默阻塞调用，"+
			"用户会又只看见计时器）", f.streamCalls, f.chatCalls)
	}
	jt := joined(got)
	if !strings.Contains(jt, MaterialThink+":先看") {
		t.Errorf("思考链片段没转发到接收器，实收=%q", jt)
	}
	if !strings.Contains(jt, MaterialText+":正文") {
		t.Errorf("正文片段没转发到接收器，实收=%q", jt)
	}
	// 流式返回的正文不能因为「接了流式」而丢：这里同时守住返回值语义没变。
	if strings.TrimSpace(out) != "关于压降库存的专题会会议纪要" {
		t.Errorf("试用稿正文被改动：%q", out)
	}
}

// TestJudgeDraftStreamsMaterial 守 8.5/9 的「独立裁判评分」这一段（逐维推理，分钟级）。
//
// 这里**故意**不评裁判输出能不能解析：解析失败也不影响「走了流式且材料被转发」这一
// 结论（材料回调发生在流式解码过程中）。解析与打分另有 judge_test.go 覆盖，本用例
// 只管「界面上看不看得见」。
func TestJudgeDraftStreamsMaterial(t *testing.T) {
	f := &fakeStreamer{streamReturn: `{"dims":[],"findings":[]}`}
	g := &Generator{llm: f}
	ctx, got := collectStream()

	_, _ = g.judgeDraft(ctx, synthPack(), Category{Name: "会议纪要"}, "素材正文", "待评草稿")
	if f.streamCalls != 1 || f.chatCalls != 0 {
		t.Fatalf("裁判评分必须走流式：streamCalls=%d chatCalls=%d", f.streamCalls, f.chatCalls)
	}
	if jt := joined(got); !strings.Contains(jt, MaterialThink+":先看") {
		t.Errorf("裁判的推理链没转发到接收器，实收=%q", jt)
	}
}

// TestBuildReviewerStreamsMaterial 守「通读手册原文拟审稿清单」这一段。
func TestBuildReviewerStreamsMaterial(t *testing.T) {
	f := &fakeStreamer{streamReturn: "- [ ] 标题不超过 22 字（依据：总则）"}
	g := &Generator{llm: f}
	ctx, got := collectStream()

	out, err := g.buildReviewer(ctx, synthPack().Structure)
	if err != nil {
		t.Fatalf("buildReviewer 不该报错：%v", err)
	}
	if f.streamCalls != 1 || f.chatCalls != 0 {
		t.Fatalf("审稿清单提炼必须走流式：streamCalls=%d chatCalls=%d", f.streamCalls, f.chatCalls)
	}
	if jt := joined(got); !strings.Contains(jt, MaterialThink+":先看") {
		t.Errorf("思考链没转发到接收器，实收=%q", jt)
	}
	if !strings.Contains(out, "标题不超过") {
		t.Errorf("审稿清单正文被改动：%q", out)
	}
}

// ---- 结构面：训练链路里不允许存在「没接流式的阻塞调用」----

var funcHeadRe = regexp.MustCompile(`^func (?:\([^)]*\) )?([A-Za-z0-9_]+)\(`)

// allowedSilent 是唯一允许出现阻塞调用的位置，理由要写在旁边：
// chatWithMaterial 自己——它就是那个「拿不到流式能力时优雅退回」的兜底，
// 以及它内部流式失败后的重试。
var allowedSilent = map[string]string{
	"chatWithMaterial": "兜底分支：接收器没挂 / provider 不支持流式 / 流式失败后重试",
}

// TestNoSilentModelCallOutsideChatWithMaterial 是源码守卫。
//
// 为什么不能只靠行为用例：漏接流式是「新增一处调用点忘了用包装函数」，行为用例
// 只覆盖已知的那几处，新加的调用点它看不见。这里的规则与漏接的形态同构。
func TestNoSilentModelCallOutsideChatWithMaterial(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("列目录失败：%v", err)
	}
	var offenders []string
	allowedHits := 0
	scanned := 0

	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读 %s 失败：%v", path, err)
		}
		cur := ""
		for i, line := range strings.Split(string(b), "\n") {
			if m := funcHeadRe.FindStringSubmatch(line); m != nil {
				cur = m[1]
			}
			if !strings.Contains(line, "g.llm.Chat(") {
				continue
			}
			scanned++
			if _, ok := allowedSilent[cur]; ok {
				allowedHits++
				continue
			}
			offenders = append(offenders, path+":"+strconv.Itoa(i+1)+"（在 "+cur+" 里）")
		}
	}

	// 下限断言：规则必须真的扫到了那些被允许的行。否则正则一旦失效（函数签名变了、
	// 调用写法变了），本用例会永远绿 —— 那就是一把空跑的尺子。
	if scanned < 3 || allowedHits < 3 {
		t.Fatalf("守卫自身失效：扫到阻塞调用 %d 处、其中被允许的 %d 处，少于兜底分支应有的 3 处。"+
			"说明匹配规则已经跟不上代码写法，必须修守卫而不是放行", scanned, allowedHits)
	}
	if len(offenders) > 0 {
		t.Errorf("训练链路里有 %d 处没接流式的阻塞模型调用（用户在界面上只会看到计时器在跳）：\n  %s\n"+
			"修法：把 g.llm.Chat(ctx, ...) 换成 g.chatWithMaterial(ctx, ...)（签名一致，不多发请求）；"+
			"确属极短调用必须保留阻塞的话，把函数名加进 allowedSilent 并写明理由",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}
