package api

import (
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/agent"
	"github.com/lizhemin15/skillforge/internal/store"
)

// 这里的断言只压两条不变的规矩，不压实现细节（格子怎么生成、什么顺序）：
//   1. append-only：一步落格之后，**它再也不会被后面的步骤改掉**。
//   2. 结论必须留在时间线上：跑完一整套流程后，屏幕上还找得到「命中哪一类」和
//      「审稿过没过」。
// 第 1 条是这次要修的病本身（旧实现是固定四格、每步整组覆盖），所以它必须在。
// 用合成数据，任何一条都不依赖本机的手册素材。

// streamSteps 模拟一次典型的手册写作流程：判类 → 对齐素材 → 起草 → 审稿两轮 → 通过。
//
// 每一步**落格当场**留一份快照（快照必须在那一刻取：事后再取 return 的 board，
// 拿到的全是最终状态，那样比出来的「没被改过」是恒真的假绿——这个坑踩过）。
func streamSteps(t *testing.T) (*traceBoard, []snapRec) {
	t.Helper()
	b := newTraceBoard()
	var snaps []snapRec
	snap := func() { snaps = append(snaps, snapRec{n: b.Len(), d: digest(b.Steps())}) }

	b.Carry([]agent.TraceStep{
		{Phase: "analyze", Label: "① 指定技能", Detail: "已选定《公文写作》，跳过意图分析", Status: "done"},
	})
	snap()

	routeIdx := b.Active("match", "② 手册分类", "正在按手册类目的适用场景判断…")
	b.Close(routeIdx, hitDetail("会议纪要", "high", "用户要一份会议纪要"))
	snap()

	b.Done("params", "③ 对齐要求与范文", alignDetail(&agent.WriteCategory{
		CategoryDoc: store.CategoryDoc{File: "07-会议纪要.md", Name: "会议纪要", Trigger: "记录会议结论"},
		Examples: []store.CategoryExample{
			{Path: "examples/会议纪要/01.md", Content: "范文正文一"},
			{Path: "examples/会议纪要/02.md", Content: "范文正文二"},
		},
	}))
	snap()

	draftIdx := b.Active("generate", "④ 起草初稿", "按《会议纪要》的写作要求与本类范文起草…")
	b.Close(draftIdx, "初稿 480 字，转入审稿")
	snap()

	// 审稿第 1 轮：有问题 → 修订
	rv1 := b.Active("generate", "审稿 · 第 1 轮", "按 reviewer.md 的检查项逐条核对…")
	b.Close(rv1, "发现 2 条问题，需要修改")
	snap()
	fx1 := b.Active("generate", "修订 · 第 1 轮", "按审稿意见改…")
	b.Close(fx1, "已改，505 字")
	snap()

	// 审稿第 2 轮：通过
	rv2 := b.Active("generate", "审稿 · 第 2 轮", "按 reviewer.md 的检查项逐条核对…")
	b.Close(rv2, "通过")
	snap()

	b.Active("generate", "交付", "交付终稿 505 字")
	snap()

	return b, snaps
}

// snapRec 是「第 n 格落定之后，前 n 格长什么样」的一条记录。
type snapRec struct {
	n int
	d string
}

func firstN(s []agent.TraceStep, n int) []agent.TraceStep {
	if n > len(s) {
		n = len(s)
	}
	return s[:n]
}

// digest 把前 n 格压成一个字符串，用来比较「第 i 步时的前 i 格」有没有被后面改过。
func digest(s []agent.TraceStep) string {
	var sb strings.Builder
	for _, st := range s {
		sb.WriteString(st.Phase + "|" + st.Label + "|" + st.Detail + "|" + st.Status + "\n")
	}
	return sb.String()
}

// 核心回归：跑完整套流程后，每一步落下的结论都得还在。
// 旧实现（固定四格 + 整组覆盖）在这条上必红：它跑完只剩第 ④ 格的最后一句
// 「正在核对…」，「命中《会议纪要》」和「通过」两处都被覆盖没了。
func TestManualWriteTraceKeepsEveryConclusion(t *testing.T) {
	b, _ := streamSteps(t)
	all := digest(b.Steps())

	// 双条件：「类别名」+「命中」缺一不可——只断类别名会被需求原文里的
	// 「帮我写一份会议纪要」这种字样假绿。
	if !strings.Contains(all, "命中「会议纪要」") {
		t.Fatalf("时间线里没有留下分类命中结论（旧 bug：被后续步骤覆盖）。实际内容：\n%s", all)
	}
	if !strings.Contains(all, "通过") {
		t.Fatalf("时间线里没有留下审稿通过的结论（旧实现只在 stderr 写了「通过」，屏幕上没有）。实际内容：\n%s", all)
	}
	// 审稿轮次要能看出跑了几轮：第 1 轮 2 条问题、第 2 轮通过，两句都得在。
	if !strings.Contains(all, "发现 2 条问题") {
		t.Fatalf("时间线里没有留下第 1 轮审稿的问题数。实际内容：\n%s", all)
	}
	if !strings.Contains(all, "审稿 · 第 2 轮") {
		t.Fatalf("时间线里看不出审稿跑到了第几轮。实际内容：\n%s", all)
	}
	// 对齐素材那一格要摊开范文文件名，出事时用户才能顺着文件名回手册核对。
	for _, want := range []string{"01.md", "02.md", "会议纪要"} {
		if !strings.Contains(all, want) {
			t.Fatalf("对齐要求与范文那一格缺少 %q。实际内容：\n%s", want, all)
		}
	}
}

// append-only 不变量：任何一步落格之后，它和它之前的所有格都不会再被后续步骤改写。
// 比较的是「那一格落定当场看到的前 n 格」与「跑完后的前 n 格」——两者必须逐字相同。
// 旧实现（固定四格、每步整组覆盖）在这条上必红。
func TestManualWriteTraceIsAppendOnly(t *testing.T) {
	b, snaps := streamSteps(t)
	final := b.Steps()
	for _, sn := range snaps {
		got := digest(firstN(final, sn.n))
		if got != sn.d {
			t.Fatalf("第 %d 格落定之后，前面的格子被后来的步骤改写了——append-only 被破坏：\n当时看到的：\n%s\n跑完再看：\n%s", sn.n, sn.d, got)
		}
	}
}

// 旧实现在这条上也必红，而且是更狠的一种红：它连「换个格子」都做不到，
// 因为压根没有追加能力——步骤数恒等于 4。
func TestManualWriteTraceGrowsWithReviewRounds(t *testing.T) {
	b, _ := streamSteps(t)
	if b.Len() < 8 {
		t.Fatalf("两轮审稿 + 修订 + 交付之后步骤数只有 %d 格，说明仍是固定骨架、每步覆盖（审稿过程不可见）", b.Len())
	}
	// 至少要有 4 个挂在 generate 段上的格子（初稿 / 审稿1 / 修订1 / 审稿2 / 交付），
	// 它们都是「内容执笔」这一跳做的事，必须落在同一个 phase 上：
	// 前端拿 phase 查角色标签表，取了别的值角色那一栏就是空的（踩过的坑）。
	gen := 0
	for _, st := range b.Steps() {
		switch st.Phase {
		case "analyze", "match", "params", "generate":
		default:
			t.Fatalf("格子 %q 的 phase=%q 不在前端认识的四个值里，角色那一栏会空掉", st.Label, st.Phase)
		}
		if st.Phase == "generate" {
			gen++
		}
	}
	if gen < 4 {
		t.Fatalf("挂在 generate 段上的格子只有 %d 个（初稿/审稿/修订/交付都该在这段里）", gen)
	}
}

// Carry 只接首格：手动骨架的后两格说的是单段流程，手册模式下会被
// 「② 手册分类 / ③ 对齐要求与范文」取代，接过来同一件事会出现两次。
func TestTraceBoardCarryTakesOnlyFirstStep(t *testing.T) {
	b := newTraceBoard()
	b.Carry([]agent.TraceStep{
		{Phase: "analyze", Label: "① 指定技能", Detail: "已选定《公文写作》，跳过意图分析", Status: "done"},
		{Phase: "params", Label: "② 载入能力", Detail: "系统提示词已注入", Status: "done"},
		{Phase: "generate", Label: "③ 执行", Detail: "按约定处理这条输入", Status: "active"},
	})
	if b.Len() != 1 {
		t.Fatalf("Carry 接了 %d 格，应该只接首格", b.Len())
	}
	got := b.Steps()[0]
	if got.Label != "① 指定技能" || !strings.Contains(got.Detail, "跳过意图分析") {
		t.Fatalf("首格被改写了，界面文案会自相矛盾：%+v", got)
	}
	if got.Status != "active" {
		t.Fatalf("承载的首格应是进行中（否则前端会以为已经跑完），实际 status=%q", got.Status)
	}
}

// 空骨架不能凭空造出一格来。
func TestTraceBoardCarryEmptySeed(t *testing.T) {
	b := newTraceBoard()
	b.Carry(nil)
	if b.Len() != 0 {
		t.Fatalf("空骨架应保持零格，实际 %d 格", b.Len())
	}
}

// Close 越界/空 detail 不能改坏已有内容：步骤流被各种失败分支切得很碎，
// 任何一个分支忘了 Close 都只是留一格 active，不该 panic 也不该改动别人。
func TestTraceBoardCloseIsSafe(t *testing.T) {
	b := newTraceBoard()
	i := b.Active("match", "② 手册分类", "正在判断…")
	b.Close(i, hitDetail("会议纪要", "high", "用户要一份会议纪要"))
	before := digest(b.Steps())

	b.Close(i+7, "越界")     // 越界：静默忽略
	b.Close(i, "   ")      // 空白 detail：不能把已有结论擦成空白
	b.Close(-1, "负数下标")   // 负数：静默忽略

	if after := digest(b.Steps()); after != before {
		t.Fatalf("越界/空 detail 的 Close 改动了已有内容：\n改前：\n%s\n改后：\n%s", before, after)
	}
}

// 命中那一格必须能自证：光有类别名会与用户需求原文里同样的字混淆。
func TestHitDetailMarksHitExplicitly(t *testing.T) {
	plain := hitDetail("会议纪要", "high", "")
	if !strings.Contains(plain, "命中「会议纪要」") {
		t.Fatalf("没有显性的「命中」标记，屏幕上看不出是系统判定的：%q", plain)
	}
	low := hitDetail("会议纪要", "low", "标题像纪要但没有结论段")
	if !strings.Contains(low, "命中「会议纪要」") {
		t.Fatalf("低把握时仍要带上类别名：%q", low)
	}
	if !strings.Contains(low, "把握不大") {
		t.Fatalf("低把握时要提醒用户核对类别，否则用户不会去看：%q", low)
	}
	if !strings.Contains(low, "标题像纪要") {
		t.Fatalf("判断理由要带上，用户才能知道它凭什么这么判：%q", low)
	}
	// 理由里的换行会把一格撑成两行，破坏时间线的排版。
	multi := hitDetail("会议纪要", "high", "第一行理由\n第二行不该出现")
	if strings.Contains(multi, "第二行") {
		t.Fatalf("判断理由没取成单行：%q", multi)
	}
}

// 对齐素材那一格列文件名，但不列范文正文——正文是给模型看的，
// 刷到对话里既污染屏幕，也可能让用户误以为稿子是从范文里抄的。
func TestAlignDetailListsFilesNotContent(t *testing.T) {
	cat := &agent.WriteCategory{
		CategoryDoc: store.CategoryDoc{Name: "会议纪要"},
		Examples: []store.CategoryExample{
			{Path: "examples/会议纪要/01.md", Content: "独一无二的范文正文标记"},
		},
	}
	got := alignDetail(cat)
	if !strings.Contains(got, "会议纪要") || !strings.Contains(got, "01.md") {
		t.Fatalf("该摊开的没摊开（类别名 + 范文文件名）：%q", got)
	}
	if strings.Contains(got, "独一无二的范文正文标记") {
		t.Fatalf("把范文正文刷进了时间线：%q", got)
	}
	if strings.Contains(got, "examples/") {
		t.Fatalf("范文路径该只留末段文件名，全路径会把这一格撑成一整行：%q", got)
	}
	if n := alignDetail(nil); !strings.Contains(n, "未载入") {
		t.Fatalf("没有分类素材时要如实说，不能空着让人以为是漏了：%q", n)
	}
	// 本类在手册里没有范文是合法情况（不是所有类目都配了范文），要说明而不是留白。
	bare := alignDetail(&agent.WriteCategory{CategoryDoc: store.CategoryDoc{Name: "会议纪要"}})
	if !strings.Contains(bare, "会议纪要") || !strings.Contains(bare, "没有范文") {
		t.Fatalf("没有范文时要说明只按要求起草：%q", bare)
	}
}

// 字数按字符数报：用字节数会把中文稿件的字数报成实际的 3 倍。
func TestRuneLenCountsCharacters(t *testing.T) {
	if got := runeLen("会议纪要"); got != 4 {
		t.Fatalf("runeLen(\"会议纪要\") = %d，应为 4（按字节是 12）", got)
	}
	if got := runeLen("ab会议"); got != 4 {
		t.Fatalf("runeLen 混排算错：%d", got)
	}
}
