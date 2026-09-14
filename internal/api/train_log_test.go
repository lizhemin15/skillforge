package api

import (
	"bytes"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lizhemin15/skillforge/internal/skillgen"
)

// captureLog 把标准 logger 的输出接进缓冲区，跑完 fn 再还原。
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	oldOut, oldFlags, oldPrefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(&buf)
	log.SetFlags(0)
	log.SetPrefix("")
	defer func() {
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
		log.SetPrefix(oldPrefix)
	}()
	fn()
	return buf.String()
}

// TestTrainOutcomeLandsInStderr：训练结论必须落到 stderr，不能只活在 SSE 里。
//
// 线上现场：训练挂在 SSE 上十几分钟。用户在浏览器里关页 / 刷新 / 代理断连之后，
// error 与 done 帧全部写进一个死连接（掉进虚空），运维事后查 journalctl 只能看到
// 「什么都没发生」：那份没过线的技能到底有没有落盘、卡在第几步，无从查证。
// v0.3.28 现场就是靠猜（一次掐线测试为什么没留产物，猜了三轮才排除「进程被杀」）。
// 这条断言钉三件事：失败要有 name+slug+原因、要带耗时；成功要有 slug 与降级标记。
func TestTrainOutcomeLandsInStderr(t *testing.T) {
	// 失败路径
	out := captureLog(t, func() {
		logTrainOutcome("线上掐线验收150000", "xianshang-qiaxian-150000", 92*time.Second,
			nil, errors.New("step3: 全部素材无法解析，本次训练已中止"))
	})
	for _, want := range []string{"[train]", "线上掐线验收150000", "xianshang-qiaxian-150000", "失败", "1m32s", "全部素材无法解析"} {
		if !strings.Contains(out, want) {
			t.Fatalf("失败结论缺 %q，实际日志：%q", want, out)
		}
	}

	// 成功路径（降级交付）：必须显性写出降级，别让「10/100 落盘」看起来像通过
	out = captureLog(t, func() {
		logTrainOutcome("线上混合素材验收150000", "hunhe-150000", 13*time.Minute,
			&skillgen.Result{PromptLen: 4210, ExampleN: 3, Degraded: true, DegradeReason: "8.5/9 裁判未过线"}, nil)
	})
	for _, want := range []string{"完成", "hunhe-150000", "13m0s", "prompt 4210 字", "示例 3 个", "降级交付=true", "8.5/9 裁判未过线"} {
		if !strings.Contains(out, want) {
			t.Fatalf("成功结论缺 %q，实际日志：%q", want, out)
		}
	}

	// 结果为空这条分支不能静默（既没 err 也没 res 时最容易什么都不打）
	out = captureLog(t, func() {
		logTrainOutcome("空结果验收", "kong-150000", time.Second, nil, nil)
	})
	if !strings.Contains(out, "结果为空") || !strings.Contains(out, "kong-150000") {
		t.Fatalf("结果为空分支没落日志：%q", out)
	}
}

// TestTrainHandlerLogsOutcome：guard 住「调用点」本身。
//
// 上面的单测只证明 logTrainOutcome 会写日志，删掉 handler 里的调用点它照样绿 ——
// 而线上真实故障正是「结论只在 SSE 里」这条线断了。这里直接查源码契约：
// Train handler 里 Generate 之后必须紧跟一次 logTrainOutcome 调用。
// 删掉那行调用，这条立刻变红（已注入自证：注释掉调用 → 红，还原 → 绿）。
func TestTrainHandlerLogsOutcome(t *testing.T) {
	src, err := os.ReadFile("admin.go")
	if err != nil {
		t.Fatalf("读不到 admin.go：%v", err)
	}
	text := string(src)
	idx := strings.Index(text, "a.gen.Generate(tctx, in, func(step string)")
	if idx < 0 {
		t.Fatal("admin.go 里找不到训练入口 a.gen.Generate(tctx, in, …) —— 调用点可能被搬走了，这条契约需要跟着改")
	}
	tail := text[idx:]
	if !strings.Contains(tail, "logTrainOutcome(name, in.Slug") {
		t.Fatal("训练入口之后没有 logTrainOutcome 调用：训练结论又会只活在 SSE 里，人一走就查不到")
	}
	if strings.Index(tail, "logTrainOutcome(name, in.Slug") > strings.Index(tail, `send("done"`) {
		t.Fatal("logTrainOutcome 在 done 帧之后才调用 —— 失败路径（提前 return）会漏掉日志")
	}
}
