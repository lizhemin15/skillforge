package skillgen

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件锁死一条线上事故：文档解析客户端超时曾被硬编码成 300s，
// 而真实扫描件（13.6MB / 50 页 / dpi=200 / 4 核）实测需要 397.5s 才出文本。
// 结果不是「报错」，而是解析必然失败 → 训练静默退回通用流程 → 8.5 裁判层
// 根本没机会执行。所以这里测的不是「超时能配」，而是「配了真生效 + 默认值
// 必须盖得住真实耗时 + 长解析中途必须有心跳」。

// slowOCRStub 起一个「思考 delay 才回答」的 /extract 假服务，返回 text。
func slowOCRStub(t *testing.T, delay time.Duration, text string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"text":%q}`, text)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestOCRClientHonorsConfiguredTimeout：超时值必须真的作用于请求。
// 注入会红的方式：把客户端超时写死成 300s（事故原样）→ 400ms 那组会「成功」，
// 断言 err != nil 立刻失败。
func TestOCRClientHonorsConfiguredTimeout(t *testing.T) {
	srv := slowOCRStub(t, 1200*time.Millisecond, "手册正文")
	g := &Generator{}

	// 上限小于服务耗时 → 必须失败。
	g.SetOCRTimeout(400 * time.Millisecond)
	if _, err := ocrExtract(context.Background(), srv.URL, "m.pdf", []byte("x"), g.ocrClient()); err == nil {
		t.Fatalf("超时上限 400ms、服务要 1200ms，竟然成功了：超时没有被应用到请求上")
	}

	// 上限大于服务耗时 → 必须成功，且拿到文本。
	g.SetOCRTimeout(3 * time.Second)
	got, err := ocrExtract(context.Background(), srv.URL, "m.pdf", []byte("x"), g.ocrClient())
	if err != nil {
		t.Fatalf("超时上限 3s、服务要 1200ms，却失败了：%v", err)
	}
	if got.Text != "手册正文" {
		t.Fatalf("解析文本不对：%q", got.Text)
	}
}

// TestOCRDefaultTimeoutCoversRealHandbook：默认上限必须盖得住实测的 397.5s。
// 这是防「有人又把它改回 300s」的硬钉子：300s < 397.5s → 必红。
func TestOCRDefaultTimeoutCoversRealHandbook(t *testing.T) {
	measured := 397*time.Second + 500*time.Millisecond // manual-scan-lite.pdf 线上实测（见 DefaultOCRTimeout 注释）
	if DefaultOCRTimeout <= measured {
		t.Fatalf("默认解析超时 %s 不大于实测耗时 %s：扫描件会必然超时并静默降级",
			DefaultOCRTimeout, measured)
	}
	// 留够余量：页数翻倍 / dpi 提高都要还能过。
	if DefaultOCRTimeout < 2*measured {
		t.Fatalf("默认解析超时 %s 余量不足（实测 %s，至少要 2 倍）", DefaultOCRTimeout, measured)
	}
	// 零值 Generator（没调过 SetOCRTimeout）也必须拿到同一个默认值。
	if got := (&Generator{}).OCRTimeout(); got != DefaultOCRTimeout {
		t.Fatalf("零值 Generator 的生效超时是 %s，期望 %s", got, DefaultOCRTimeout)
	}
}

// TestOCRProgressTicks：长解析期间要有心跳，且返回后必须彻底安静。
// 后半段是并发正确性：心跳 goroutine 若在 handler 返回后还往 ResponseWriter 写，
// 就是「往已经返回的响应写」——线上表现为偶发坏帧 / panic 日志。
// 注入会红的方式：stop 不 close(done)、不 Wait（泄漏心跳）→ 返回后仍在追加消息 + goroutine 数不回落。
func TestOCRProgressTicks(t *testing.T) {
	old := ocrProgressInterval
	ocrProgressInterval = 120 * time.Millisecond
	defer func() { ocrProgressInterval = old }()

	srv := slowOCRStub(t, 700*time.Millisecond, "手册正文")

	var mu sync.Mutex
	var steps []string
	collect := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		steps = append(steps, s)
	}
	snapshot := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), steps...)
	}

	start := time.Now()
	stop := ocrProgress(collect, "手册.pdf", start, nil)
	if _, err := ocrExtract(context.Background(), srv.URL, "手册.pdf", []byte("x"), (&Generator{}).ocrClient()); err != nil {
		t.Fatalf("解析应当成功：%v", err)
	}
	stop()

	ticks := 0
	for _, s := range snapshot() {
		if strings.Contains(s, "解析中，已等待") {
			ticks++
		}
	}
	if ticks < 2 {
		t.Fatalf("解析 700ms、心跳间隔 120ms，只收到 %d 条心跳：%v", ticks, snapshot())
	}

	// 返回后必须安静：再等 10 个心跳周期，消息数不许再涨。
	//
	// 这里刻意**不**用 runtime.NumGoroutine() 去抓泄漏：实测那只会数到
	// httptest 的 goServe / Transport.dialConn 这些测试桩自己的 goroutine，
	// 跟被测代码无关——属于骑同机环境的过强断言，无害重构也会假红。
	// 「返回后不再写流」才是产品真正要保证的性质，且它对注入有判别力。
	n := len(snapshot())
	time.Sleep(10 * ocrProgressInterval)
	if after := len(snapshot()); after != n {
		t.Fatalf("stop() 之后仍在写流：%d → %d 条（心跳 goroutine 没被 join）", n, after)
	}
}

// TestOCRProgressNilSteps：没有 steps 回调时不许 panic（也不许阻塞）。
func TestOCRProgressNilSteps(t *testing.T) {
	stop := ocrProgress(nil, "手册.pdf", time.Now(), nil)
	stop()
	stop() // 幂等：重复调用不能 panic（close 已关闭的 chan 会炸，这里必须靠实现保证）
}

// TestHumanDuration：给用户看的耗时文案。
func TestHumanDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{9 * time.Second, "9 秒"},
		{59 * time.Second, "59 秒"},
		{60 * time.Second, "1 分 0 秒"},
		{397 * time.Second, "6 分 37 秒"},
		{30 * time.Minute, "30 分 0 秒"},
	}
	for _, c := range cases {
		if got := humanDuration(c.in); got != c.want {
			t.Fatalf("humanDuration(%s) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestIsTimeoutErr：只有超时才该弹「降级」提示，用户关页面（Canceled）不该弹。
func TestIsTimeoutErr(t *testing.T) {
	if !isTimeoutErr(context.DeadlineExceeded) {
		t.Fatal("context.DeadlineExceeded 应被判定为超时")
	}
	if isTimeoutErr(fmt.Errorf("解析服务: %s", "boom")) {
		t.Fatal("普通业务错误不该被判定为超时")
	}
	if isTimeoutErr(context.Canceled) {
		t.Fatal("context.Canceled（用户关页面）不该被判定为超时")
	}
	if isTimeoutErr(nil) {
		t.Fatal("nil 不是超时")
	}
}
