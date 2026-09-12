package api

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lizhemin15/skillforge/internal/agent"
)

type frameRec struct {
	mu    sync.Mutex
	evs   []string
	datas []string
}

func (f *frameRec) write(ev, data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evs = append(f.evs, ev)
	f.datas = append(f.datas, data)
}

// traces 只取 trace 事件，解析回步骤组。
func (f *frameRec) traces(t *testing.T) [][]agent.TraceStep {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]agent.TraceStep
	for i, ev := range f.evs {
		if ev != evTrace {
			continue
		}
		var st []agent.TraceStep
		if err := json.Unmarshal([]byte(f.datas[i]), &st); err != nil {
			t.Fatalf("trace 帧不是合法 JSON: %v (%s)", err, f.datas[i])
		}
		out = append(out, st)
	}
	return out
}

func (f *frameRec) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.evs)
}

func bootSteps() []agent.TraceStep {
	return []agent.TraceStep{{Phase: "analyze", Label: "① 意图分析", Detail: "正在理解你的问题…", Status: "active"}}
}

// t=0 就必须有帧：用户抱怨的第一现场是「几十秒屏幕上什么都没有」。
func TestTraceClockEmitsFirstFrameAtOnce(t *testing.T) {
	rec := &frameRec{}
	c := newTraceClock(rec.write, bootSteps())
	defer c.Freeze()

	got := rec.traces(t)
	if len(got) != 1 {
		t.Fatalf("首帧应当立刻下发，实际 %d 帧", len(got))
	}
	if got[0][0].Status != "active" {
		t.Fatalf("首帧的当前步骤应为 active，实际 %q", got[0][0].Status)
	}
	if !strings.Contains(got[0][0].Detail, "正在理解你的问题") {
		t.Fatalf("首帧文案不对: %q", got[0][0].Detail)
	}
}

// 心跳：阻塞期间必须持续有帧，且带上「已用 Ns」，否则用户无法判断是卡死还是慢。
func TestTraceClockHeartbeatsWithElapsed(t *testing.T) {
	rec := &frameRec{}
	c := newTraceClockBeat(rec.write, bootSteps(), 15*time.Millisecond)
	defer c.Freeze()

	time.Sleep(90 * time.Millisecond)
	got := rec.traces(t)
	if len(got) < 3 {
		t.Fatalf("90ms / 15ms 心跳至少应有 3 帧（含首帧），实际 %d", len(got))
	}
	last := got[len(got)-1][0].Detail
	if !strings.Contains(last, elapsedOpen) {
		t.Fatalf("心跳帧应带「已用 Ns」，实际 %q", last)
	}
	if n := strings.Count(last, elapsedOpen); n != 1 {
		t.Fatalf("「已用」出现 %d 次，说明秒数在叠加: %q", n, last)
	}
}

// 秒数只装饰发送副本，绝不能写回内部状态（否则文案会叠成「已用 3s（已用 6s）」）。
func TestTraceClockElapsedDoesNotPolluteState(t *testing.T) {
	rec := &frameRec{}
	c := newTraceClockBeat(rec.write, bootSteps(), 10*time.Millisecond)
	defer c.Freeze()
	time.Sleep(50 * time.Millisecond)

	st := c.Steps()
	if st[0].Detail != "正在理解你的问题…" {
		t.Fatalf("内部步骤文案被秒数污染: %q", st[0].Detail)
	}
	c.Set(st)
	frames := rec.traces(t)
	last := frames[len(frames)-1]
	if strings.Count(last[0].Detail, elapsedOpen) != 1 {
		t.Fatalf("Set 后应只有一处秒数: %q", last[0].Detail)
	}
}

// 等待用户补充时要停跳：等待态继续跳秒会让人以为后台还在跑。
func TestTraceClockQuietWhileAwaiting(t *testing.T) {
	rec := &frameRec{}
	c := newTraceClockBeat(rec.write, bootSteps(), 15*time.Millisecond)
	defer c.Freeze()

	c.Awaiting("等待补充：金额")
	time.Sleep(80 * time.Millisecond)

	frames := rec.traces(t)
	if len(frames) != 2 { // 首帧 + Awaiting 帧
		t.Fatalf("等待态不应再心跳，期望 2 帧实际 %d", len(frames))
	}
	last := frames[1]
	if last[0].Status != "waiting" {
		t.Fatalf("等待态最后一步应为 waiting，实际 %q", last[0].Status)
	}
	if !strings.Contains(last[0].Detail, "等待补充：金额") {
		t.Fatalf("等待态文案不对: %q", last[0].Detail)
	}
}

// Finish 必须：全部 done + 停跳（终帧之后不能再冒帧，否则前端会闪回「进行中」）。
func TestTraceClockFinishMarksDoneAndStops(t *testing.T) {
	rec := &frameRec{}
	c := newTraceClockBeat(rec.write, bootSteps(), 15*time.Millisecond)
	defer c.Freeze()

	c.Finish()
	after := rec.count()
	time.Sleep(80 * time.Millisecond)
	if rec.count() != after {
		t.Fatalf("Finish 之后仍在心跳：%d -> %d 帧", after, rec.count())
	}
	frames := rec.traces(t)
	last := frames[len(frames)-1]
	for i, s := range last {
		if s.Status != "done" {
			t.Fatalf("终帧第 %d 步应为 done，实际 %q", i, s.Status)
		}
	}
}

func TestNormalizeStepsEnsuresActiveStep(t *testing.T) {
	allDone := []agent.TraceStep{
		{Phase: "analyze", Label: "a", Detail: "d1", Status: "done"},
		{Phase: "generate", Label: "b", Detail: "d2", Status: "done"},
	}
	got := normalizeSteps(allDone)
	if got[1].Status != "active" {
		t.Fatalf("全 done 时最后一步应被标 active，实际 %q", got[1].Status)
	}
	if allDone[1].Status != "done" {
		t.Fatal("normalizeSteps 不得改动入参")
	}
	empties := []agent.TraceStep{{Phase: "analyze", Label: "a"}}
	got2 := normalizeSteps(empties)
	if got2[0].Status != "active" {
		t.Fatalf("缺状态应兜成 active（末步），实际 %q", got2[0].Status)
	}
}

// phase 取值必须落在前端 AGENTS 映射表的键里，否则时间线会缺角色标签。
func TestFallbackStepsPhasesMatchFrontend(t *testing.T) {
	allowed := map[string]bool{"analyze": true, "match": true, "params": true, "generate": true}
	for _, intent := range []string{"chat", "query", "docgen", "write", ""} {
		got := fallbackSteps(agent.Eval{Intent: intent})
		if len(got) == 0 {
			t.Fatalf("intent=%q 兜底步骤为空 —— 那就回到「一直转圈」了", intent)
		}
		actives := 0
		for _, s := range got {
			if !allowed[s.Phase] {
				t.Fatalf("intent=%q phase=%q 不在前端映射表里", intent, s.Phase)
			}
			if s.Status == "active" {
				actives++
			}
		}
		if actives != 1 {
			t.Fatalf("intent=%q 应恰有一个 active 步骤，实际 %d", intent, actives)
		}
	}
}

// 接线守卫：trace 只能有一个下发口。绕过 clock 直写 evTrace 会与心跳并发切碎 SSE 帧，
// 且会丢掉「已用 Ns」心跳——正是本次修的问题，必须防回退。
func TestTraceOnlyEmittedThroughClock(t *testing.T) {
	for _, f := range []string{"chat.go", "agent_loop.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", f, err)
		}
		if strings.Contains(string(src), "write(evTrace") {
			t.Fatalf("%s 里有绕过 clock 的 write(evTrace…) —— 会造成并发写帧", f)
		}
	}
	src, err := os.ReadFile("chat.go")
	if err != nil {
		t.Fatalf("读 chat.go 失败: %v", err)
	}
	if !strings.Contains(string(src), "defer clock.Freeze()") {
		t.Fatal("chat.go 缺少 defer clock.Freeze()，error 早退时心跳会漏停")
	}
	if !strings.Contains(string(src), "newTraceClock(write, []agent.TraceStep{{") {
		t.Fatal("chat.go 未在请求入口下发步骤骨架 —— 首帧缺失即回到「空白加载」")
	}
}

// 回归守卫：心跳节拍必须在起 goroutine 之前定死。
// 曾经的写法是构造完再给节拍字段赋值，于是 loop() 读、测试写，构成数据竞争：
// 本地不带 -race 全绿、CI 上 `go test -race` 红。（正解是 newTraceClockBeat 参数注入。）
func TestTraceBeatIsNotMutatedAfterConstruction(t *testing.T) {
	// 病毒串拆开拼：否则本文件里的注释/字面量自己就命中自己（这坑踩了两遍）。
	needle := "." + "beat" + " " + "="
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("读 %s 失败: %v", e.Name(), err)
		}
		if strings.Contains(string(b), needle) {
			t.Fatalf("%s 里出现构造后改写心跳节拍 —— 与心跳 goroutine 构成数据竞争，请用 newTraceClockBeat 注入", e.Name())
		}
	}
}
