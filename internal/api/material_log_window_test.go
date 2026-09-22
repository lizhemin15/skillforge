package api

import (
	"strings"
	"testing"
)

// 这组测试问的是**尺子**对不对，不是产品对不对。
//
// 背景：线上尺子 scripts/sse_material_log_gate.py 的 L2 判据原是「同一跳的相邻两帧，
// 后一帧的前缀必须覆盖前一帧的后缀（重叠 ≥ min(len)-300）」。它假设 materialLog 是一个
// **只在尾部增长**、且**尾部即末尾**的字符串。2026-09-22 的线上 A/B 对照里，这条判据对
// **修复后**的线上二进制变红了（重叠 0，需要 882）。
//
// 两个假设都不成立，这是本条测试串行化下来的结论：
//   (b) 窗口尾部挂着旁白时，新来的一片会先**摘掉旧旁白**再追加 —— 于是「前一帧的后缀」
//       根本不在后一帧里，后缀/前缀关系不成立；
//   (c) 窗口顶到上限后前部按句读前移，会加一个前导「…」标记。前一帧的尾巴是正文、
//       后一帧的头是「…」，逐字比前缀**永远归零** —— 这条最狠：连纯材料序列都会假红。
//
// 线上一次跑要 40~50s，还受模型路径（有无思考链）影响，靠它分辨「产品换了文字」和
// 「尺子模型错」太贵。这里用真函数（appendMaterialWindow / dropTrailingNarration）造
// 序列，直接问：**合法操作会不会造出「前帧尾巴在后帧里找不到」的相邻帧？**
//
// 会 → 尺子模型错，尺子必须按 body()（摘前导「…」+ 摘尾部旁白）再判。
// 本文件锁的就是这个语义：修复后的判据在全部合法序列上绿，且**夹具真的走到了**那些分支
// （见各用例的前置断言 —— 夹具没走到分支的绿灯是空跑绿）。

// overlapRaw 是尺子**修复前**的判据，逐字同构地留在这里，只用于 t.Log 记录「旧模型在
// 这条序列上会怎么判」。它不再作为断言：断言旧代码错，测的是历史而不是现在的行为。
func overlapRaw(prev, cur string) int {
	n := len([]rune(prev))
	cr := []rune(cur)
	pr := []rune(prev)
	if len(cr) < n {
		n = len(cr)
	}
	for l := n; l > 0; l-- {
		if string(pr[len(pr)-l:]) == string(cr[:l]) {
			return l
		}
	}
	return 0
}

// isNarration 必须与尺子（Python 侧 is_narration）**同一份**判断：判据一致才叫同一把尺子。
// 旁白形态来自后端源码：material_filter.go 的「模型思考中…已产出…」、api 侧以 "· " 开头的
// 步骤旁白（含「· 已装配：…」）、classify 的「收到你的需求（本轮 N 字）」。
func isNarration(line string) bool {
	s := strings.TrimSpace(line)
	for _, h := range []string{"· ", "模型思考中…", "已装配：", "收到你的需求"} {
		if strings.HasPrefix(s, h) {
			return true
		}
	}
	return false
}

// body 把一帧 materialLog 归一成「材料正文」：
//  1. 摘掉前导「…」—— 它不是内容，是窗口前移时的可读性标记（详见 appendMaterialWindow）；
//  2. 摘掉**尾部**的旁白行（可能连着好几行，也可能整帧只有旁白 → 空串）。
//
// 尺子上的所有判据都必须在 body 上判；在原始值上判就会踩 (b)/(c)。
func body(v string) string {
	v = strings.TrimLeft(v, "…")
	parts := strings.Split(v, "\n")
	for len(parts) > 0 && isNarration(parts[len(parts)-1]) {
		parts = parts[:len(parts)-1]
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// kept 判断「前一帧正文的尾巴是否被后一帧**含住**」（含在哪里不管），返回 (是否含住, 需要多少字)。
//
// 为什么不是「后一帧前缀 == 前一帧后缀」：窗口前部会被丢掉、首字会前移、尾部旁白会被替换
// —— 三件事都会让**前缀**变样，但正文尾巴一直在。need==0 表示两帧太短，判不出，不计红。
func kept(prev, cur string) (bool, int) {
	p, c := body(prev), body(cur)
	if p == "" || c == "" {
		return true, 0
	}
	rp, rc := []rune(p), []rune(c)
	n := len(rp)
	if len(rc) < n {
		n = len(rc)
	}
	need := n - 300
	if need <= 0 {
		return true, 0
	}
	return strings.Contains(c, string(rp[len(rp)-need:])), need
}

// shift 模拟 traceClock.Thinking 的落窗规则：日志只挂进行中的那一步，所以材料
// 是**一片片**进来的；每片都过一遍 appendMaterialWindow(..., materialLogCap, true)。
func shift(chunks []string) []string {
	var out []string
	cur := ""
	for _, c := range chunks {
		cur = appendMaterialWindow(cur, c, materialLogCap, true)
		out = append(out, cur)
	}
	return out
}

// l2CheckFixed 是尺子**修复后**的判据，逐对跑一遍，返回第一个违规对。
func l2CheckFixed(frames []string) (string, string, int, bool) {
	prev := ""
	for _, cur := range frames {
		if cur == prev || cur == "" {
			prev = cur
			continue
		}
		if ok, need := kept(prev, cur); !ok {
			return prev, cur, need, true
		}
		prev = cur
	}
	return "", "", 0, false
}

// l2WorstRaw 返回旧判据下最坏的一对（重叠最小于需要的差），只用于 t.Log 留痕。
func l2WorstRaw(frames []string) (int, int, string, string) {
	prev := ""
	worstOv, worstNeed := -1, 0
	var wp, wc string
	for _, cur := range frames {
		if cur == prev || cur == "" {
			prev = cur
			continue
		}
		n := len([]rune(prev))
		if len([]rune(cur)) < n {
			n = len([]rune(cur))
		}
		need := n - 300
		if need < 0 {
			need = 0
		}
		ov := overlapRaw(prev, cur)
		if ov < need && (worstOv < 0 || ov-need < worstOv-worstNeed) {
			worstOv, worstNeed, wp, wc = ov, need, prev, cur
		}
		prev = cur
	}
	return worstOv, worstNeed, wp, wc
}

// mustBeFixed 是所有用例共用的出口：修复后的判据必须绿，并把旧判据的结果记下来。
func mustBeFixed(t *testing.T, name string, frames []string) {
	t.Helper()
	if p, c, need, bad := l2CheckFixed(frames); bad {
		t.Fatalf("%s：修复后的判据仍判违规（需要含住前帧正文尾 %d 字）\n前帧正文尾 80：%q\n后帧正文头 80：%q",
			name, need, tail(body(p), 80), head(body(c), 80))
	}
	ov, need, _, _ := l2WorstRaw(frames)
	if ov < 0 {
		t.Logf("%s ✓ 修复后判据绿；旧判据在这条序列上也没找到违规对（都是纯材料短帧）", name)
		return
	}
	t.Logf("%s ✓ 修复后判据绿；旧判据最坏一对 overlap=%d need=%d（旧判据在此%s）",
		name, ov, need, map[bool]string{true: "假红", false: "也绿"}[ov < need])
}

// S1：纯材料（无旁白）。这是「理想形态」，尺子必须绿 —— 先证明这套测试夹具本身
// 不会平白无故报红（否则下面的失败就分不出是夹具错还是尺子错）。
func TestMaterialLogWindow_PureMaterialOnlyGrows(t *testing.T) {
	var chunks []string
	for i := 0; i < 40; i++ {
		chunks = append(chunks, "数据要素市场化配置是本次会议的核心议题之一，各方围绕机制建设进行研讨并形成共识。")
	}
	frames := shift(chunks)
	if !hitCap(frames) {
		t.Fatalf("夹具没把窗口顶到上限 %d（最长 %d 字）—— 没走到前移分支，这条是空跑绿",
			materialLogCap, maxLen(frames))
	}
	if !anyHasAlignMarker(frames) {
		t.Fatalf("窗口顶到上限却没出现前导「…」（最长 %d 字）—— (c) 那条分支没被覆盖，判据白锁",
			maxLen(frames))
	}
	mustBeFixed(t, "S1 纯材料", frames)
}

// S2：材料中间插旁白。旁白带前导 \n（material_filter.go 的 narration 就是这么造的），
// 显示层靠这个 \n 让新旁白**替换**旧旁白。
//
// 这里要看的正是 (b)：写完一句材料后来一条旁白，再写材料 —— 帧之间的后缀/前缀关系
// 还成不成立？
func TestMaterialLogWindow_WithNarration(t *testing.T) {
	var chunks []string
	for i := 0; i < 30; i++ {
		chunks = append(chunks, "会议围绕数据要素市场化配置与产业协同机制建设进行研讨。")
		if i%3 == 2 { // 每三片材料挂一条旁白（形如 material_filter 的 narration）
			chunks = append(chunks, "\n模型思考中…已产出 239 字（已 2s）")
		}
	}
	frames := shift(chunks)
	if !anyTrailingNarration(frames) {
		t.Fatalf("夹具没有任何一帧以旁白结尾 —— (b) 那条分支没被覆盖，判据白锁")
	}
	mustBeFixed(t, "S2 材料+旁白", frames)
}

// S3：长序列把窗口顶到上限（1200）后继续走 —— 这时前部会被丢掉、首字按句读前移。
// 尺子的「允许 min(len)-300 错位」就是为这一段写的，这里验证 300 够不够。
func TestMaterialLogWindow_AtCapFrontShiftStillOverlaps(t *testing.T) {
	var chunks []string
	for i := 0; i < 120; i++ {
		chunks = append(chunks, "公司召开数据要素产业协同推进会发布3项合作成果，32家产业链上下游单位代表与会。")
	}
	frames := shift(chunks)
	if !hitCap(frames) {
		t.Fatalf("夹具没把窗口顶到上限 %d（最长 %d 字）—— 这条测的就是满窗口后的前移，前提不成立",
			materialLogCap, maxLen(frames))
	}
	mustBeFixed(t, "S3 满窗口前移", frames)
}

// S4：满窗口 + 旁白。线上 ⑤ 段实测长这样：日志顶到 1200，期间还夹着旁白更新。
// 这是最接近线上那次「重叠 0」的序列：旧判据在这里必假红。
func TestMaterialLogWindow_AtCapWithNarration(t *testing.T) {
	var chunks []string
	for i := 0; i < 120; i++ {
		chunks = append(chunks, "公司召开数据要素产业协同推进会发布3项合作成果，32家产业链上下游单位代表与会。")
		if i%5 == 4 {
			chunks = append(chunks, "\n模型思考中…已产出 812 字（已 9s）")
		}
	}
	frames := shift(chunks)
	if !hitCap(frames) {
		t.Fatalf("夹具没顶到上限 %d —— 与线上 ⑤ 段形态不符，前提不成立", materialLogCap)
	}
	mustBeFixed(t, "S4 满窗口+旁白", frames)
}

// S5：真材料里混进「· 已装配：…」这类**步骤旁白**（api 侧会以 "· " 开头送进来，
// 且不带前导 \n）。看它是否破坏后缀/前缀关系。
func TestMaterialLogWindow_StepNarrationWithoutNewline(t *testing.T) {
	var chunks []string
	for i := 0; i < 30; i++ {
		chunks = append(chunks, "推进会由副总经理李某某主持，各方围绕数据要素市场化配置展开研讨。")
		if i == 4 {
			chunks = append(chunks, "· 已装配：技能《公司新闻通稿》｜要素 5 项（company_name、core_event、date、org、achievements）")
		}
	}
	frames := shift(chunks)
	mustBeFixed(t, "S5 步骤旁白（无前导\\n）", frames)
}

func hitCap(frames []string) bool {
	for _, f := range frames {
		if len([]rune(f)) >= materialLogCap {
			return true
		}
	}
	return false
}

func anyHasAlignMarker(frames []string) bool {
	for _, f := range frames {
		if strings.HasPrefix(f, "…") {
			return true
		}
	}
	return false
}

func anyTrailingNarration(frames []string) bool {
	for _, f := range frames {
		lines := strings.Split(f, "\n")
		if len(lines) > 1 && isNarration(lines[len(lines)-1]) {
			return true
		}
	}
	return false
}

func head(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

func maxLen(ss []string) int {
	m := 0
	for _, s := range ss {
		if n := len([]rune(s)); n > m {
			m = n
		}
	}
	return m
}
