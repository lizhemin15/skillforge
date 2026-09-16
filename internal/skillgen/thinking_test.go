package skillgen

// 事故锚（用户投诉原话）：「现在速度过于慢了，中间可以流式输出思考的一些中间材料，
// 现在一直卡着计时，用户体验不佳。」
//
// 上一轮修的是「看得见」（阶段内部的材料流式转发），这一轮修的是**慢本身**：线上实测
// 一轮训练 860.5s，吐出的字符里 **94% 是思考链**（约 5.6 万字思考 / 3.7 千字正文）。
// 最刺眼的是 5/9：430.4s、27287 字推演，产出只是一段 JSON 摘录，结论还是「不是手册，
// 降级通用流程」。修法：结构化/机械阶段关掉思考链，成品文字阶段保留。
//
// 本文件守四件事，缺一不可：
//   1. 行为面：`withoutThinking(ctx)` 真的落到了 provider 上（StreamOpts.DisableThinking）；
//   2. 结构面：训练链路里**每个**模型调用点都必须为思考链表态 —— 新增调用点忘了表态就红。
//      这次漏的 9 处就是「新代码忘了关」，行为用例只看得见写用例时已知的那几处；
//   3. 反向：该保留思考的 5 处（撰写系统提示词 / 返修 / 优化收紧 / 范文兜底 / 裁判试写）
//      不许被人顺手关掉。一刀切关全部 = 用所有技能的质量换速度，那不叫提速，叫降级；
//   4. 守卫自身不许退化：扫描器要能被合成料钉住（否则规则一松，上面三条全成空跑）。

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ---- 行为面：开关真的传到了 provider ----

// TestChatWithMaterialCarriesThinkingFlagToProvider 守 `withoutThinking` 的落点。
//
// 断言钉在「传出去的 StreamOpts 上」，而不是「耗时可观测地变短了」——后者要真打
// provider，慢且会因网关行为波动，测不出「开关接线断了」这种确定性的坏。
func TestChatWithMaterialCarriesThinkingFlagToProvider(t *testing.T) {
	cases := []struct {
		name string
		mark bool // 调用点有没有表态「关思考链」
		want bool
	}{
		{"结构化阶段（标了 withoutThinking）关思考链", true, true},
		{"成品文字阶段（没标）保留思考链", false, false},
	}
	for _, c := range cases {
		f := &fakeStreamer{streamReturn: "{}"}
		g := &Generator{llm: f}
		ctx, _ := collectStream()
		want := ctx
		if c.mark {
			want = withoutThinking(ctx)
		}
		if _, err := g.chatWithMaterial(want, "sys", "user"); err != nil {
			t.Fatalf("%s：不该报错 %v", c.name, err)
		}
		if f.streamCalls != 1 {
			t.Fatalf("%s：没走流式（streamCalls=%d），行为用例骑在空集上", c.name, f.streamCalls)
		}
		if got := f.lastOpts.DisableThinking; got != c.want {
			t.Errorf("%s：DisableThinking=%v 期望 %v —— 开关没接到 provider 上，"+
				"结构化阶段的思考链又回来了（5/9 那一步就是 430 秒 / 2.7 万字推演）",
				c.name, got, c.want)
		}
	}
}

// TestThinkingOffDefaultsToKeep 守默认值方向。
//
// 为什么单独钉一条：如果哪天 `thinkingOff` 写反（默认关），后果不是「慢」而是**静默降级** ——
// 所有技能的成品文字都少了思考，而界面、单测、验收全绿。默认必须是「保留」。
func TestThinkingOffDefaultsToKeep(t *testing.T) {
	if thinkingOff(context.Background()) {
		t.Fatal("空 ctx 被判成「关思考链」—— 默认反了：成品文字阶段会静默降级成无思考，没人会红")
	}
	if !thinkingOff(withoutThinking(context.Background())) {
		t.Fatal("标了 withoutThinking 却没被识别 —— 开关形同虚设")
	}
}

// ---- 结构面：每个模型调用点必须表态 ----

// modelCallRe 命中「这一行发起了一次模型调用」，含两种形态：
// 直接调包装函数（chatWithMaterial / extractStructure*），或把包装后的客户端递出去
// （streamingChat()）。后一种是线上真踩过的坑：阻塞调用发生在被调函数内部，看着不像问题。
var modelCallRe = regexp.MustCompile(`(chatWithMaterial\(|streamingChat\(\)|ExtractStructure\(|extractStructureByChapters\()`)

// funcHeadRe（「这一行是哪个函数的开头」）与 stage_streaming_test.go **共用同一份**，
// 不在本文件重抄一遍：抄的那份永远不会跟着改，两个守卫就会各信各的。

// keepThinkingSites 是「保留思考链」的白名单。键是「文件|函数」，值是**这个函数里
// 该有几个保留思考的调用点** —— 数字不是装饰：只按函数名放行的话，往
// `buildSystemPrompt` 里新加第二个调用点会被这份白名单顺带放行，等于开了个后门。
//
// 每条都得写清为什么不能关：这里的产出是给人读的成品文字，思考链直接影响质量。
var keepThinkingSites = map[string]struct {
	Want int
	Why  string
}{
	"generator.go|buildSystemPrompt": {
		Want: 1,
		Why:  "4/9 撰写系统提示词本身 —— 一份技能的提示词决定它后续所有产出的水平",
	},
	"generator.go|reviseSystemPrompt": {
		Want: 1,
		Why:  "优化指令收紧，改的是要给人读的提示词正文",
	},
	"generator.go|Review": {
		Want: 1,
		Why:  "返修重写：把裁判意见落成新的提示词正文",
	},
	"generator.go|buildExamples": {
		Want: 1,
		Why:  "范文兜底：技能没带示例时现写一份给人看的范文",
	},
	"judge.go|trialDraft": {
		Want: 1,
		Why:  "8.5/9 裁判试写：先按技能写一篇，再用它当评审依据 —— 写的是成品文字",
	},
}

// scanModelCallSites 把「文件路径 → 源码」扫成「每个调用点分别怎么表态」。
//
// 抽成纯函数是为了能被合成料钉住（见 TestModelCallScannerPinsClassification）：
// 真正的风险不是今天漏了哪一处，而是**规则本身退化**（比如只认 `chatWithMaterial(`、
// 或者把 `func` 定义行也算成调用）—— 那样的守卫永远是绿的。
func scanModelCallSites(files map[string]string, allow map[string]struct {
	Want int
	Why  string
}) (off []string, keepCount map[string]int, offenders []string) {
	keepCount = map[string]int{}
	for path, src := range files {
		cur := ""
		for i, line := range strings.Split(src, "\n") {
			if m := funcHeadRe.FindStringSubmatch(line); m != nil {
				cur = m[1]
			}
			trimmed := strings.TrimSpace(line)
			// 注释行不发调用：允许正文里说明「这处为什么保留思考链」，
			// 否则守卫会逼着人把最该写的那句解释删掉（同 stage_streaming_test.go）。
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			// `func ... streamingChat() { return chatFunc(g.chatWithMaterial) }` 这类
			// **定义/取方法值**的行不是调用点，别算进来（否则它自己就成了永久违规）。
			if strings.HasPrefix(trimmed, "func ") || strings.HasPrefix(line, "func ") {
				continue
			}
			if !modelCallRe.MatchString(line) {
				continue
			}
			where := path + ":" + strconv.Itoa(i+1) + "（在 " + cur + " 里）"
			if strings.Contains(line, "withoutThinking(") {
				off = append(off, where)
				continue
			}
			key := path + "|" + cur
			if _, ok := allow[key]; !ok {
				offenders = append(offenders, where+"："+trimmed)
				continue
			}
			keepCount[key]++
		}
	}
	sort.Strings(off)
	sort.Strings(offenders)
	return off, keepCount, offenders
}

// readPackageSources 读本包所有**出货**源码（跳过 _test.go 与本能被合成料替代的东西）。
func readPackageSources(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("列目录失败：%v", err)
	}
	out := map[string]string{}
	for _, p := range files {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("读 %s 失败：%v", p, err)
		}
		out[p] = string(b)
	}
	return out
}

// offThinkingFloor 是「已表态关思考链」的调用点数下限。
//
// 为什么要有下限：offenders 为空这个条件，在**扫描器什么都扫不到**时也成立 ——
// 正则写歪、枚举漏了目录、文件改名，守卫都会安安静静变绿。9 这个数是本轮逐一
// 核实过的结构化/机械调用点数（1/9 特征、3/9 类型、2/9 元数据、6/9 骨架、5.5 机械
// 删句、裁判评分、5/9 全文抽取、5/9 按章兜底、审稿清单）。**别调小**：调小等于
// 允许守卫退化到「只盯一部分调用点」。
const offThinkingFloor = 9

// TestModelCallSitesDeclareThinkingMode 是本轮的核心守卫。
//
// 为什么不能只靠行为用例：漏表态的形态是「新加一处调用点」，行为用例只覆盖写用例
// 时已知的那几处，新调用点它看不见。规则与漏的形态同构：凡发起模型调用，要么显式
// `withoutThinking(...)`，要么登记进 keepThinkingSites（附理由）。
func TestModelCallSitesDeclareThinkingMode(t *testing.T) {
	off, _, offenders := scanModelCallSites(readPackageSources(t), keepThinkingSites)

	if len(offenders) > 0 {
		t.Errorf("有模型调用点没为思考链表态（%d 处）：\n  %s\n"+
			"要么包一层 withoutThinking(ctx)（结构化/机械阶段，产出是 JSON 或逐字改写），\n"+
			"要么登记进 keepThinkingSites 并写清为什么这处不能关（产出是给人读的成品文字）。",
			len(offenders), strings.Join(offenders, "\n  "))
	}
	if len(off) < offThinkingFloor {
		t.Errorf("只扫到 %d 处「关思考链」的调用点，下限是 %d —— 扫描器退化了（正则/枚举漏项），"+
			"这条守卫已经不可以信。修扫描器，别调小下限。", len(off), offThinkingFloor)
	}
}

// TestKeepThinkingAllowlistHasNoDrift 守**反向**：该留着思考的 5 处不许被一刀切关掉。
//
// 为什么这条必须单独红：把 5 处全关掉，整轮训练会更快、所有单测仍全绿、界面也照常 ——
// 唯一的变化是技能质量下降。没有这条断言，「提速」随时会漂成「降级」而无人察觉。
// 同时它挡住另一个后门：往白名单函数里塞第二个调用点，靠 allow 顺带放行。
func TestKeepThinkingAllowlistHasNoDrift(t *testing.T) {
	_, keepCount, _ := scanModelCallSites(readPackageSources(t), keepThinkingSites)

	for key, ent := range keepThinkingSites {
		got := keepCount[key]
		switch {
		case got == 0:
			t.Errorf("%s：白名单说这里有 %d 处保留思考的调用点，实际一处没看到 —— "+
				"要么这处被关掉了（%s），要么调用点搬了家而白名单没跟着改。两者都得人工确认。",
				key, ent.Want, ent.Why)
		case got > ent.Want:
			t.Errorf("%s：白名单只放行 %d 处，实际看到 %d 处 —— 白名单函数里新加的调用点"+
				"被顺带放行了。每个调用点都要单独表态。", key, ent.Want, got)
		}
	}
}

// TestModelCallScannerPinsClassification 是守卫的守卫：用合成料把扫描器钉住。
//
// 三种分类各给一条：显式关、白名单保留、没表态（必须进 offenders）。
// 再加两条「不该被算成调用点」的形态（函数定义行、注释行）—— 它们算进来就会造成
// 永久假红，逼着后来的人把守卫删掉。
func TestModelCallScannerPinsClassification(t *testing.T) {
	src := strings.Join([]string{
		"package skillgen",
		"",
		"// 注释里写 g.chatWithMaterial(ctx, sys, user) 不算调用",
		"func (g *Generator) streamingChat() chatClient { return chatFunc(g.chatWithMaterial) }",
		"",
		"func a(ctx context.Context) {",
		"\tout, _ := g.chatWithMaterial(withoutThinking(ctx), sys, user, true)",
		"}",
		"",
		"func buildSystemPrompt(ctx context.Context) {",
		"\tout, _ := g.chatWithMaterial(ctx, sys, user)",
		"}",
		"",
		"func newStage(ctx context.Context) {",
		"\tst, _ := ExtractStructure(ctx, g.streamingChat(), src)",
		"}",
	}, "\n")

	// 合成料的文件名要用真名（`generator.go`）：白名单键是「文件|函数」，
	// 随便造个 `synthetic.go` 会让白名单查不到，测的是我编错的文件名而不是扫描器。
	off, keepCount, offenders := scanModelCallSites(map[string]string{"generator.go": src}, keepThinkingSites)

	if len(off) != 1 || !strings.Contains(off[0], "（在 a 里）") {
		t.Errorf("显式关思考链那处没被认出来：off=%v", off)
	}
	if keepCount["generator.go|buildSystemPrompt"] != 1 {
		t.Errorf("白名单函数里的调用点没被计入保留数：keepCount=%v", keepCount)
	}
	if len(offenders) != 1 || !strings.Contains(offenders[0], "（在 newStage 里）") {
		t.Errorf("没表态的调用点必须进 offenders，实际 %v", offenders)
	}
	// 「不该算成调用点」的两行：`streamingChat` 的定义/取方法值行、正文里的注释行。
	// 它们一旦被算进来就是永久假红（谁也没法「修」一条定义行），最后逼着人删守卫。
	// 上面两条计数断言已经隐含了这点，这里把归因说清楚，免得将来有人误判成扫描器漏项。
	for _, entry := range append(append([]string{}, off...), offenders...) {
		if strings.Contains(entry, "（在 streamingChat 里）") || strings.Contains(entry, ":4（") {
			t.Errorf("定义行/注释行被当成调用点了（会造成永久假红）：%s", entry)
		}
	}
	if n := len(off) + len(offenders); n != 2 {
		t.Errorf("合成料里只该有 2 个调用点（1 关 + 1 没表态），扫到 %d 个 —— "+
			"多出来的多半是定义行/注释行被算了进来：off=%v offenders=%v", n, off, offenders)
	}
}
