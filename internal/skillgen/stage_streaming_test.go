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

// allowedSilent 是唯一允许直接摸 g.llm 的位置，理由要写在旁边：
// chatWithMaterial 自己——它就是那个「拿不到流式能力时优雅退回」的兜底，
// 以及它内部流式失败后的重试。
var allowedSilent = map[string]string{
	"chatWithMaterial": "兜底分支：接收器没挂 / provider 不支持流式 / 流式失败后重试",
}

// g.llm 的「无害用法」白名单。守卫只认这几种形态，其余一律判红。
//
// 为什么必须收这么紧：漏接流式有**两种**形态，第一种是 `g.llm.Chat(`（直接在
// 阻塞接口上调用），第二种更隐蔽——把裸客户端当参数递给自由函数
// （`ExtractStructure(ctx, g.llm, src)`），阻塞调用发生在被调函数内部，看着不像
// 问题。线上 353.3 秒零帧就是第二种：Step 5 手册结构抽取走的正是这条。按「出现
// g.llm 就查用途」来判，两种形态用同一条规则就能同时拦住。
var gllmHarmless = []func(string) bool{
	func(l string) bool { return strings.Contains(l, "g.llm == nil") }, // 未配置模型的守卫
	func(l string) bool { return strings.Contains(l, "g.llm != nil") },
	func(l string) bool { return strings.Contains(l, "g.llm = ") }, // 注入/热切换
}

// gllmUseIsHarmless 是上面白名单的唯一入口（抽成函数是为了能被表驱动直接钉住）。
func gllmUseIsHarmless(line string) bool {
	for _, ok := range gllmHarmless {
		if ok(line) {
			return true
		}
	}
	return false
}

// TestGllmUseClassifierPinsBothLeakShapes 把分类器本身钉住：两种漏接形态都必须
// 被判「有害」。这是守卫的守卫 —— 分类器一松，源码守卫就退化成空跑。
func TestGllmUseClassifierPinsBothLeakShapes(t *testing.T) {
	cases := []struct {
		line     string
		harmless bool
		why      string
	}{
		{`return g.llm.Chat(ctx, sys, user, jsonMode...)`, false, "形态一：直接阻塞调用"},
		{`st, stErr := ExtractStructure(ctx, g.llm, src)`, false, "形态二：裸客户端递给自由函数"},
		{`sc, ok := g.llm.(streamChatClient)`, false, "断言本身也得待在白名单函数里"},
		{`if g.llm == nil {`, true, "未配置模型的守卫"},
		{`g.llm = l`, true, "注入/热切换"},
	}
	for _, c := range cases {
		if got := gllmUseIsHarmless(c.line); got != c.harmless {
			t.Errorf("分类器判错（%s）：line=%q 期望 harmless=%v 实际 %v", c.why, c.line, c.harmless, got)
		}
	}
}

// TestNoSilentModelCallOutsideChatWithMaterial 是源码守卫。
//
// 为什么不能只靠行为用例：漏接流式是「新增一处调用点忘了用包装函数」，行为用例
// 只覆盖已知的那几处，新加的调用点它看不见。这里的规则与漏接的形态同构：
// 凡是在非白名单函数里碰 g.llm，就是把材料转发绕过去了。
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
			// 注释行不可能发起调用：允许它们在正文里说明「别把 g.llm 传出去」，
			// 否则守卫会逼着人把最该写的那句解释删掉。
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if !strings.Contains(line, "g.llm") {
				continue
			}
			scanned++
			if _, ok := allowedSilent[cur]; ok {
				allowedHits++
				continue
			}
			harmless := false
			for _, ok := range gllmHarmless {
				if ok(line) {
					harmless = true
					break
				}
			}
			if harmless {
				allowedHits++
				continue
			}
			offenders = append(offenders, path+":"+strconv.Itoa(i+1)+"（在 "+cur+" 里）："+strings.TrimSpace(line))
		}
	}

	// 下限断言：规则必须真的扫到了那些「该被允许」的行。否则启发式一旦退化
	// （比如改回只看 `g.llm.Chat(` 这一种写法），本用例会永远绿 —— 空跑的尺子。
	//
	// 6 这个数怎么来的：g.llm 的合法出现至少有 11 处（chatWithMaterial 里 3 处阻塞
	// 兜底 + 1 处能力断言、2 处注入赋值、5 处 nil 守卫）。退化到只看 `.Chat(` 时
	// 只会剩 3 处，直接跌破下限。**别调小**：调小等于允许守卫退化成只看一种写法。
	if scanned < 6 || allowedHits < 6 {
		t.Fatalf("守卫自身失效：扫到含 g.llm 的行 %d 处、其中被允许的 %d 处，低于应有下限 6。"+
			"说明匹配规则已经跟不上代码写法（或退化成只看某一种写法），必须修守卫而不是放行。"+
			"合法用法至少 11 处：chatWithMaterial 里 3 处兜底 + 1 处能力断言、2 处注入赋值、5 处 nil 守卫",
			scanned, allowedHits)
	}
	if len(offenders) > 0 {
		t.Errorf("训练链路里有 %d 处绕过了材料转发的模型调用（用户在界面上只会看到计时器在跳）：\n  %s\n"+
			"修法：直接调用换 g.chatWithMaterial(ctx, ...)（签名一致，不多发请求）；"+
			"要把能力交给自由函数就传 g.streamingChat()（它内部就是同一个包装）；"+
			"确属极短调用必须保留阻塞的话，把函数名加进 allowedSilent 并写明理由",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// TestBuildManualStreamsMaterial 守 Step 5「检测素材是否为写作手册」这一段。
//
// 线上实测：这一段从 `+287.6s [step] 5/9 检测素材是否为写作手册…` 到
// `+640.9s [step] 5/9 未按手册处理…` 之间 **353.3 秒零帧**，独占整轮 11.9 分钟的
// 近六分钟。根因是把裸客户端递给了 ExtractStructure，阻塞调用藏在被调方内部。
// 本用例钉在「这一段是否真的把材料吐出来了」上。
func TestBuildManualStreamsMaterial(t *testing.T) {
	// 模型回一份「分类数不足」的结构：buildManual 会走完 ExtractStructure 然后
	// 因分类不足降级返回 error。我们要证的正是**降级路径上也流过材料**——
	// 事故当场就是这条降级路径。
	f := &fakeStreamer{streamReturn: `{"categories":[]}`}
	g := &Generator{llm: f}
	relay, got := collectAll()
	ctx := WithDelta(context.Background(), relay.Push)

	in := &Input{Files: []*UploadedFile{{Filename: "写作手册.txt", Content: "第一章 总则\n本手册规定通稿的格式要求。"}}}
	if _, err := g.buildManual(ctx, in); err == nil {
		t.Fatalf("分类数不足时应降级返回 error（这是设计上的降级路径，不是故障）")
	}
	relay.Flush()
	if f.streamCalls == 0 {
		t.Fatalf("手册结构抽取必须走流式：streamCalls=0 chatCalls=%d —— 用户会又只看见计时器", f.chatCalls)
	}
	if jt := strings.Join(*got, "|"); !strings.Contains(jt, MaterialThink+":先看") {
		t.Errorf("结构抽取的推理链没转发到接收器，实收=%q", jt)
	}
}
