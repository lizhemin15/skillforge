package llm

// 复读看门狗测试。守的是线上实锤过的故障形态（2026-09-22：5 字问题流了 47KB
// 同一句话，用户屏幕上正文疯狂重复刷屏）：
//
//	A1. 检测器命中——真复读要被判出来，且报出的循环体长度是准的
//	    （错误信息里「循环体约 N 字节」是排障第一眼看的数，不能说谎）；
//	A2. 检测器零误报——自然长文跨多个检测窗口都不许判；
//	A3. 阈值确在 3 遍——同一段落 2 遍放行、3 遍判中，隔离出「遍数」这一个变量；
//	B.  收手在客户端侧——真的只吞了 2KB 级内容就掐断（服务器准备的是 2.2MB）；
//	C.  StreamChat 重试——第二次请求带 frequency_penalty=0.4，OnReset 清屏
//	    恰好一次，OnNote 有「复读」说明，最终拿到干净正文；
//	D.  复读二次（带 penalty 也拦不住）——终态报错前也要 OnReset，
//	    调用方拿到 ErrLoopDetected，总共恰好 2 次请求，不会无限重试。
//
// 为什么不用现有 newStreamSrv：它一次性 Write 整个 SSE 体，既不能逐帧 Flush，
// 也造不出「服务器还在写、客户端已经跑了」的时序。这里自定义 handler 逐帧写。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lizhemin15/skillforge/internal/model"
)

// ---------------------------------------------------------------------------
// A. 检测器纯函数
// ---------------------------------------------------------------------------

// 自然文本生成器。from 是起始行号——必须由调用方保证各批次行号不相交，
// 否则「同一批填充行反复追加」本身就是复读，测出来的是这个 bug 而不是检测器。
func naturalLines(from, n int) string {
	var b strings.Builder
	for i := from; i < from+n; i++ {
		fmt.Fprintf(&b, "第%04d条：这一行内容各不相同，编号%04d，用来充当自然正文的填充素材长度足够长。\n", i, i)
	}
	return b.String()
}

func TestLoopDetectorFiresOnRepetition(t *testing.T) {
	// 单元必须自身非周期（用行号唯一的自然行拼），否则最小周期是单元内部的那个
	// 小周期——比如 strings.Repeat("循环体句子。", 20) 的真周期是 18 而不是 360。
	unit := naturalLines(100, 8) // ≈1KB，内部无 192 字节级重复
	var sb strings.Builder
	sb.WriteString(naturalLines(0, 8)) // 前导自然文本：模拟「正常开头然后开始复读」
	repeat := strings.Repeat(unit, 60)
	sb.WriteString(repeat)

	ls := &loopState{}
	period, ok := 0, false
	for !ok {
		period, ok = ls.hit(&sb)
		if !ok {
			// hit 每 loopCheckEvery 字节才检一次；模拟流式继续吐字
			sb.WriteString(repeat)
			if sb.Len() > 1<<20 {
				t.Fatal("加到 1MB 还没判复读")
			}
		}
	}
	if period != len(unit) {
		t.Fatalf("循环体长度 = %d，want %d（unit 真实字节数）", period, len(unit))
	}
}

func TestLoopDetectorNoFalsePositiveOnNaturalText(t *testing.T) {
	// 大段自然文本（句句不同，行号全局唯一），跨多个检测窗口，绝不能误判。
	// 误报的代价是把正常长文砍断，比漏报更糟。
	sb := &strings.Builder{}
	sb.WriteString(naturalLines(0, 900))
	ls := &loopState{}
	if _, ok := ls.hit(sb); ok {
		t.Fatal("自然文本误判复读")
	}
	sb.WriteString(naturalLines(900, 400)) // 行号接续，与上一批不相交
	if _, ok := ls.hit(sb); ok {
		t.Fatal("第二个检测窗口仍误判")
	}
}

func TestLoopDetectorNeedsThreeOccurrences(t *testing.T) {
	// 同一个变量的两个取值：段落出现 2 遍（放行）vs 3 遍（判中）。
	// 合法文档里「同一段引用两遍」很常见，3 遍阈值就是给它的余量。
	// 单元取行号唯一的自然行（自身非周期），否则最小周期是单元内部的小周期。
	unit := naturalLines(500, 4) // ≈456 字节 > loopTailBytes

	// 骨架 = 自然文本 + unit + 自然文本 + unit（末尾就是第二遍）。
	// 两个用例只差「末尾再紧接一遍」，把「遍数」隔离成唯一变量。
	base := naturalLines(0, 10) + unit + naturalLines(10, 3) + unit

	// 前提自证：不够长就根本没触发检测，那样「不判复读」是空跑绿。
	if len(base) < loopCheckEvery {
		t.Fatalf("骨架 %d 字节 < 检测门 %d，前提不成立", len(base), loopCheckEvery)
	}
	// 前提自证：尾巴必须真的是「unit 的第二遍」，且前文恰好只有 1 处同款。
	// 若尾巴落在唯一填充行上（前文 0 处），这个用例就测不到遍数阈值——变异测试
	// （把阈值 3 遍改成 2 遍）跑出来是绿的，等于这把尺子是假的。
	tail := base[len(base)-loopTailBytes:]
	if got := strings.Count(base[:len(base)-loopTailBytes], tail); got != 1 {
		t.Fatalf("2 遍用例的前提不成立：尾巴在前文出现 %d 次，want 1", got)
	}
	var two strings.Builder
	two.WriteString(base)
	lsTwo := &loopState{}
	if _, ok := lsTwo.hit(&two); ok {
		t.Fatalf("2 遍出现不应判复读；sb.Len()=%d", two.Len())
	}

	// 3 遍：末尾再紧接一遍（与上面只差这一遍，其余同构）
	if !strings.HasSuffix(base, unit) {
		t.Fatal("前提不成立：骨架末尾不是一整遍 unit")
	}
	var three strings.Builder
	three.WriteString(base + unit)
	lsThree := &loopState{}
	period, ok := lsThree.hit(&three)
	if !ok {
		t.Fatalf("3 遍出现应判复读；sb.Len()=%d", three.Len())
	}
	if period != len(unit) {
		t.Fatalf("循环体长度 = %d，want %d", period, len(unit))
	}
}

// ---------------------------------------------------------------------------
// B/C/D. SSE 流级别
// ---------------------------------------------------------------------------

type loopServer struct {
	mu      sync.Mutex
	bodies  []map[string]any
	written atomic.Int64 // 第 1 次请求实际写进连接的字节数（写失败即停）
	calls   atomic.Int32
}

const loopFrameText = "关于开展数据治理专项行动的通知。" // 15 字 = 45 字节

func loopFrame() string {
	return "data: {\"choices\":[{\"delta\":{\"content\":\"" + loopFrameText + "\"}}]}\n\n"
}

// newLoopServer：第 1 次请求逐帧复读（每帧 Flush，写失败=客户端收手）；之后按 after 走。
func newLoopServer(t *testing.T, s *loopServer, after func(n int) (int, string)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		n := len(s.bodies)
		s.mu.Unlock()
		s.calls.Add(1)

		if n == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			fr := loopFrame()
			for i := 0; i < 20000; i++ { // 20000 帧 ≈ 2.2MB，正常永远轮不到写完
				if _, err := io.WriteString(w, fr); err != nil {
					return // 客户端掐断 → 写失败，这里就是「收手点」
				}
				s.written.Add(int64(len(fr)))
				fl.Flush()
			}
			return // 走到这里 = 客户端从头吞到尾没收手
		}
		code, out := after(n)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(out))
	}))
}

var (
	reLoopBytes   = regexp.MustCompile(`循环体约 (\d+) 字节`)
	reTakenBytes  = regexp.MustCompile(`已收 (\d+) 字节`)
	loopYesAnswer = "重试后的干净答案：关于开展数据治理专项行动的通知。"
)

// B: 客户端侧收手点的精确度量。服务器侧 written 数会被 TCP 内核缓冲污染
// （客户端 Close 后、RST 到达前，服务器还能往缓冲区里灌 ~60KB，实测 59784），
// 那是内核行为不是看门狗行为。真正要证明的是「客户端只吞了 2KB 级内容就跑了」，
// 所以直接调 streamOnce，从错误信息里的「已收 N 字节」取客户端实收数。
func TestStreamOnceAbortsLoopEarlyClientSide(t *testing.T) {
	s := &loopServer{}
	srv := newLoopServer(t, s, func(n int) (int, string) { return http.StatusOK, "data: [DONE]\n\n" })
	defer srv.Close()
	c := New(&model.LLMConfig{BaseURL: srv.URL, APIKey: "k", Model: "test-model"})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	content, status, err := c.streamOnce(ctx, "sys", "user", StreamOpts{}, knobNone)

	if !IsLoopDetected(err) {
		t.Fatalf("应收手于 ErrLoopDetected，err=%v", err)
	}
	if content != "" {
		t.Fatalf("复读正文不能交付给调用方（会变成半屏垃圾），content 长度=%d", len(content))
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200（这是上游的问题不是 HTTP 层的问题）", status)
	}
	m := reTakenBytes.FindStringSubmatch(err.Error())
	if m == nil {
		t.Fatalf("错误信息缺「已收 N 字节」，排障时看不到收手点：%v", err)
	}
	taken, _ := strconv.Atoi(m[1])
	// 上界 8KB 是 4 倍余量：检测门 2048 + 一次读缓冲。没收手时这里会是 900KB。
	if taken > 8*1024 {
		t.Fatalf("客户端收了 %d 字节才收手 —— 看门狗没在秒级掐断", taken)
	}
	if taken < loopMinContent {
		t.Fatalf("客户端只收了 %d 字节就收手（<最小正文量 %d）—— 判据漏了，收手理由不对",
			taken, loopMinContent)
	}
	// 循环体长度必须是真值 45 字节（重叠搜索的用处：跳尾巴会量成 192+ε）
	pm := reLoopBytes.FindStringSubmatch(err.Error())
	if pm == nil {
		t.Fatalf("错误信息缺「循环体约 N 字节」：%v", err)
	}
	if got, _ := strconv.Atoi(pm[1]); got != len(loopFrameText) {
		t.Fatalf("报出的循环体 = %d 字节，want %d（每帧同一句话）", got, len(loopFrameText))
	}
}

func TestStreamChatStopsLoopEarlyAndRetriesWithPenalty(t *testing.T) {
	s := &loopServer{}
	after := func(n int) (int, string) { // 第 2 次起给一份正常答案
		return http.StatusOK, strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"重试后的干净答案：关于开展"}}]}`,
			`data: {"choices":[{"delta":{"content":"数据治理专项行动的通知。"}}]}`,
			`data: {"choices":[{"delta":{}}],"usage":{"total_tokens":9}}`,
			`data: [DONE]`,
			``,
		}, "\n\n")
	}
	srv := newLoopServer(t, s, after)
	defer srv.Close()
	c := New(&model.LLMConfig{BaseURL: srv.URL, APIKey: "k", Model: "test-model"})

	var resets, loopNotes atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := c.StreamChat(ctx, "sys", "user", StreamOpts{
		OnReset: func() { resets.Add(1) },
		OnNote: func(note string) {
			if strings.Contains(note, "复读") {
				loopNotes.Add(1)
			}
		},
	})

	// C1: 重试成功，调用方拿到干净正文
	if err != nil {
		t.Fatalf("复读重试应成功，err=%v", err)
	}
	if out != loopYesAnswer {
		t.Fatalf("out = %q", out)
	}
	// C2: 恰好 2 次请求（1 次复读 + 1 次 penalty 重试），不无限打
	if got := s.calls.Load(); got != 2 {
		t.Fatalf("请求次数 = %d, want 2", got)
	}
	// C3: 第 2 次请求带 frequency_penalty=0.4；第 1 次不带
	s.mu.Lock()
	b1, b2 := s.bodies[0], s.bodies[1]
	s.mu.Unlock()
	if _, ok := b1["frequency_penalty"]; ok {
		t.Fatal("首次请求不应带 frequency_penalty（会影响所有正常流）")
	}
	if fp, ok := b2["frequency_penalty"].(float64); !ok || fp != 0.4 {
		t.Fatalf("重试请求 frequency_penalty = %v, want 0.4", b2["frequency_penalty"])
	}
	// C4: 清屏恰好 1 次（重试前作废旧正文）
	if got := resets.Load(); got != 1 {
		t.Fatalf("OnReset 次数 = %d, want 1", got)
	}
	// C5: 复读说明恰好 1 次（用户得知道这一轮为什么慢了一下）
	if got := loopNotes.Load(); got != 1 {
		t.Fatalf("含「复读」的说明次数 = %d, want 1", got)
	}
	// 服务器侧旁证：准备 2.2MB，实际只写出去一点。上界取 192KB 是宽松的——
	// 内核 send buffer 常吞 ~64KB，而不收手时这里会一路涨到 2.2MB。
	if got := s.written.Load(); got > 192*1024 {
		t.Fatalf("服务器已写出 %d 字节 —— 看门狗没提前掐断", got)
	}
}

func TestStreamChatLoopTwiceIsTerminal(t *testing.T) {
	s := &loopServer{}
	after := func(n int) (int, string) {
		// 第 2 次仍复读（penalty 也压不住的病态模型）
		return http.StatusOK, strings.Repeat(loopFrame(), 400)
	}
	srv := newLoopServer(t, s, after)
	defer srv.Close()
	c := New(&model.LLMConfig{BaseURL: srv.URL, APIKey: "k", Model: "test-model"})

	var resets atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := c.StreamChat(ctx, "sys", "user", StreamOpts{
		OnReset: func() { resets.Add(1) },
	})

	// D1: 终态错误是 ErrLoopDetected（能穿透 TransientError 包装）
	if !IsLoopDetected(err) {
		t.Fatalf("二次复读应终态返回 ErrLoopDetected，err=%v", err)
	}
	if errors.Is(err, ErrStreamStalled) {
		t.Fatal("复读与断流哨兵混了：会把排障引向连接问题而非模型问题")
	}
	// D2: 恰好 2 次请求：1 次初试 + 1 次 penalty 重试，二次复读不再重试
	if got := s.calls.Load(); got != 2 {
		t.Fatalf("请求次数 = %d, want 2（不允许无限重试）", got)
	}
	// D3: 清屏 2 次——重试前 1 次 + 终态报错前 1 次（半屏复读不能留给用户去滚）
	if got := resets.Load(); got != 2 {
		t.Fatalf("OnReset 次数 = %d, want 2", got)
	}
	// D4: 第 2 次请求确实带了 penalty（重试路径真的换了参数）
	s.mu.Lock()
	b2 := s.bodies[1]
	s.mu.Unlock()
	if fp, ok := b2["frequency_penalty"].(float64); !ok || fp != 0.4 {
		t.Fatalf("重试请求 frequency_penalty = %v, want 0.4", b2["frequency_penalty"])
	}
}
