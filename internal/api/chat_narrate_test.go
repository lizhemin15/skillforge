package api

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lizhemin15/skillforge/internal/agent"
)

// capWriter 收集下发的帧（同包其它测试的做法：套一层锁，goroutine 在写）。
type capWriter struct {
	mu   sync.Mutex
	data []string
}

func (c *capWriter) write(ev, d string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = append(c.data, ev+"|"+d)
}

func (c *capWriter) drain() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.data
	c.data = nil
	return out
}

func (c *capWriter) materials() []string {
	var out []string
	for _, s := range c.drain() {
		if !strings.HasPrefix(s, evTrace+"|") {
			continue
		}
		i := strings.Index(s, `"material":"`)
		if i < 0 {
			continue
		}
		rest := s[i+len(`"material":"`):]
		if j := strings.Index(rest, `"`); j >= 0 {
			out = append(out, rest[:j])
		}
	}
	return out
}

// 旁白必须真的换行：只发一次就等于「卡住不动」，正是用户抱怨的那种屏幕。
// 节奏必须 > materialThrottle(400ms)，否则测的是节流而不是旁白本身；
// 线上是 3s 一拍，比节流大一截，所以每行都会真的到屏幕上。
func TestNarrateRollsLines(t *testing.T) {
	w := &capWriter{}
	steps := []agent.TraceStep{{Phase: "generate", Label: "④ 按要点执笔", Status: "active"}}
	c := newTraceClockTuned(w.write, steps, time.Hour, 450*time.Millisecond) // 心跳调很远，只测旁白
	defer c.stop()

	stop := c.Narrate([]string{"点一", "点二", "点三"})
	time.Sleep(1600 * time.Millisecond)
	stop()

	mats := w.materials()
	if len(mats) < 3 {
		t.Fatalf("旁白只滚了 %d 次（%v），静默里屏幕还是静止的", len(mats), mats)
	}
	all := strings.Join(mats, "")
	for _, want := range []string{"点一", "点二", "点三"} {
		if !strings.Contains(all, want) {
			t.Fatalf("旁白没滚到 %q，实际 %v", want, mats)
		}
	}
	if !strings.Contains(all, "要点回顾") {
		t.Fatalf("一轮走完必须标「要点回顾」，否则用户把重复当成新进展：%v", mats)
	}
}

// stop 之后必须彻底安静：漏停的 goroutine 会往下一轮对话的流里写字（跨会话脏数据）。
func TestNarrateStopsQuietly(t *testing.T) {
	w := &capWriter{}
	steps := []agent.TraceStep{{Phase: "generate", Label: "④ 按要点执笔", Status: "active"}}
	c := newTraceClockTuned(w.write, steps, time.Hour, 450*time.Millisecond)
	defer c.stop()

	stop := c.Narrate([]string{"甲", "乙"})
	time.Sleep(700 * time.Millisecond) // 先让它真在滚，再 stop
	stop()
	w.drain()
	// 观察窗口要 > 旁白节奏(450ms) 且 > 节流(400ms)：太短的话 stop 坏掉也照样绿。
	time.Sleep(1300 * time.Millisecond)
	if got := w.materials(); len(got) != 0 {
		t.Fatalf("stop() 之后还在下发旁白：%v", got)
	}
}

// 没有旁白行时不能空转：没内容也要保证不 panic、不影响主流程。
func TestNarrateNoLinesIsNoop(t *testing.T) {
	w := &capWriter{}
	c := newTraceClockTuned(w.write, []agent.TraceStep{{Phase: "generate", Label: "x", Status: "active"}}, time.Hour, 20*time.Millisecond)
	defer c.stop()
	stop := c.Narrate([]string{"  ", ""})
	stop()
	if got := w.materials(); len(got) != 0 {
		t.Fatalf("空输入不该产生旁白：%v", got)
	}
}

func TestNarrateLinesExtraction(t *testing.T) {
	plan := "- 开头用一句话点明会议时间地点\n- 第二段引一处领导讲话\n1. 结尾落到产业协同\n这一段很长的说明" +
		strings.Repeat("啰嗦", 40) + "\n"
	got := narrateLines(plan, "")
	if len(got) != 3 {
		t.Fatalf("应只取 3 条列表行，实际 %d 条：%v", len(got), got)
	}
	if got[0] != "开头用一句话点明会议时间地点" {
		t.Fatalf("列表标记没剥干净：%q", got[0])
	}
	for _, s := range got {
		if len([]rune(s)) > narrateLineCap {
			t.Fatalf("旁白行超长会把别的行挤出材料窗口：%q", s)
		}
	}

	// 要点是一段话（没有列表行）时按句号切。
	para := narrateLines("先写导语，交代时间地点。再补一句领导讲话。结尾落到协同推进。", "装配：技能 A")
	if len(para) != 3 {
		t.Fatalf("长段要点应按句号切成 3 行，实际 %v", para)
	}

	// 要点整个为空时退回装配事实，并按分隔符拆行。
	fb := narrateLines("", "技能：公司新闻通稿｜范文 3 篇｜要素 4 项")
	if len(fb) != 3 {
		t.Fatalf("装配事实该按「｜」拆成 3 行，实际 %v", fb)
	}
}

// 一轮最多 8 行：面板不是阅读器，行太多等于这一轮久到看不出变化。
func TestNarrateLinesCapped(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20; i++ {
		b.WriteString("- 要点\n")
	}
	if got := narrateLines(b.String(), ""); len(got) > 8 {
		t.Fatalf("行数没封顶：%d", len(got))
	}
}
