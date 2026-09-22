package skillgen

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件锁死「解析期屏幕上滚的必须是材料本身」这条体验契约。
//
// 起因是用户原话：「现在速度过于慢了，中间可以流式输出思考的一些中间材料，现在一直
// 卡着计时，用户体验不佳」。旧实现只有一个 30s 一跳的裸计时心跳（"已等待 30s…"），
// 20~30 分钟的解析期屏幕上什么真材料都没有 —— 读起来就是卡死。
//
// 契约（对端 deploy/ocr/ocrd.py 的 v6 逐页流）：
//   每页一行：{"page":N,"pages":M,"src":"text|ocr|empty","chars":C,"text":"<本页原文>"}
//   末行一行：{"done":true,"ok":true, ...与阻塞式完全相同的字段}
// 兼容兜底：Content-Type 不含 ndjson（老版 ocrd / 提前失败 / 非 PDF）→ 走原单 JSON 路径。
//
// 这些测试的共同要求是「断言可自证」：注入坏实现必须真能变红，而不是恒真。
// 每条测试上方都写了「注入什么会让它红」。

// pageStreamStub 起一个逐页流式假服务：把 pages 逐行写出去，再写末行。
// tailBroken=true 时故意不写末行就断（模拟服务被杀 / 中间设备掐连接）。
type pageStreamStubOpts struct {
	pages      []string
	srcs       []string
	brokenLine bool // 中途插一行坏 JSON
	hugePage   bool // 某页正文塞到 200KB（>64KB）
	tailBroken bool // 不写末行
	tailOK     bool // 末行 ok 字段
	tailErr    string
	selfHeal   bool
	contentTT  string // 非空则覆盖 Content-Type（用来测老版 ocrd 的单 JSON 兜底）
	rawBody    string // 非空则直接回这段 body（配合 contentTT）
}

func pageStreamStub(t *testing.T, o pageStreamStubOpts) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 先验调用方的请求头：不接受这个头的客户端不该拿到流式回包。
		if !strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/x-ndjson") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true,"text":"调用方没有索取逐页流"}`)
			return
		}
		if o.contentTT != "" || o.rawBody != "" {
			tt := o.contentTT
			if tt == "" {
				tt = "application/json"
			}
			w.Header().Set("Content-Type", tt)
			fmt.Fprint(w, o.rawBody)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Connection", "close")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		for i, p := range o.pages {
			src := "text"
			if i < len(o.srcs) {
				src = o.srcs[i]
			}
			if o.hugePage && i == 1 {
				p = strings.Repeat("填充", 100000) // 200KB，单行远超 Scanner 的 64KB 上限
			}
			line, _ := json.Marshal(map[string]any{
				"page": i + 1, "pages": len(o.pages), "src": src, "chars": len(p), "text": p,
			})
			fmt.Fprintf(w, "%s\n", line)
			fl.Flush()
		}
		if o.brokenLine {
			fmt.Fprint(w, "{这不是 JSON\n")
			fl.Flush()
		}
		if o.tailBroken {
			return // 直接断：没有末行
		}
		tail := map[string]any{
			"done": true, "ok": o.tailOK, "fmt": "pdf",
			"text": strings.Join(o.pages, "\n"), "chars": 0,
		}
		if o.tailErr != "" {
			tail["error"] = o.tailErr
		}
		if o.selfHeal {
			tail["self_healing"] = true
			tail["runtime_ok"] = false
		}
		b, _ := json.Marshal(tail)
		fmt.Fprintf(w, "%s\n", b)
		fl.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestOCRPageStreamEmitsEveryPage：逐页流必须**逐页**回调，且把每页真文字交出去。
// 注入会红的方式：忽略 Content-Type、把整个 body 当单 JSON 解析（老实现）→
// json.Unmarshal 在 NDJSON 上必然失败 → 解析报错，本测试红。
func TestOCRPageStreamEmitsEveryPage(t *testing.T) {
	srv := pageStreamStub(t, pageStreamStubOpts{
		pages:  []string{"第一页：这是手册的要求正文", "第二页：示例一", "第三页：示例二"},
		srcs:   []string{"text", "ocr", "text"},
		tailOK: true,
	})

	type ev struct {
		pno, total int
		src, text  string
	}
	var mu sync.Mutex
	var got []ev
	res, err := ocrExtractPages(context.Background(), srv.URL, "手册.pdf", []byte("x"),
		(&Generator{}).ocrClient(), func(pno, total int, src, text string) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, ev{pno, total, src, text})
		})
	if err != nil {
		t.Fatalf("逐页流解析应当成功：%v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应当逐页回调 3 次，实际 %d 次：%+v", len(got), got)
	}
	for i, e := range got {
		if e.pno != i+1 || e.total != 3 {
			t.Errorf("第 %d 次回调页码错：page=%d pages=%d", i, e.pno, e.total)
		}
		if e.text == "" {
			t.Errorf("第 %d 次回调没带本页文字：屏幕就只能滚空气", i)
		}
	}
	if got[1].src != "ocr" {
		t.Errorf("扫描页 src 应为 ocr，实际 %q（屏幕上就分不清直取和 OCR）", got[1].src)
	}
	// 末行才是最终结果：文本/统计必须完整落回 ocrResult。
	if !strings.Contains(res.Text, "示例二") {
		t.Fatalf("末行汇总文本没被采纳：%q", res.Text)
	}
}

// TestOCRPageStreamFallsBackToSingleJSON：老版 ocrd（Content-Type: application/json）
// 必须走原路径 —— 应用服务与解析服务要能各自独立升级。
// 注入会红的方式：只要发了 Accept 头就强制按 NDJSON 解析（丢兼容分支）→ 单 JSON 报错。
func TestOCRPageStreamFallsBackToSingleJSON(t *testing.T) {
	srv := pageStreamStub(t, pageStreamStubOpts{
		contentTT: "application/json",
		rawBody:   `{"ok":true,"text":"老服务返回的整份文本","warning":"有 1/9 页没识别出文字","stats":{"pages":9,"text_pages":8,"ocr_pages":1}}`,
	})
	called := 0
	res, err := ocrExtractPages(context.Background(), srv.URL, "手册.pdf", []byte("x"),
		(&Generator{}).ocrClient(), func(int, int, string, string) { called++ })
	if err != nil {
		t.Fatalf("老版（单 JSON）回包应当成功：%v", err)
	}
	if called != 0 {
		t.Fatalf("单 JSON 路径不该有逐页回调，实际 %d 次", called)
	}
	if res.Text != "老服务返回的整份文本" {
		t.Fatalf("文本不对：%q", res.Text)
	}
	if res.Warning == "" || res.Stats == nil {
		t.Fatalf("warning/stats 丢了：warning=%q stats=%v", res.Warning, res.Stats)
	}
}

// TestOCRPageStreamTruncatedIsError：没收到末行就断流 → **必须报错**。
// 这是最危险的一条：半截素材不会报错，只会安静地训出一个错技能。
// 注入会红的方式：EOF 时把已收到的文本当成功返回。
func TestOCRPageStreamTruncatedIsError(t *testing.T) {
	srv := pageStreamStub(t, pageStreamStubOpts{
		pages:      []string{"第一页正文", "第二页正文"},
		tailBroken: true,
	})
	res, err := ocrExtractPages(context.Background(), srv.URL, "手册.pdf", []byte("x"),
		(&Generator{}).ocrClient(), nil)
	if err == nil {
		t.Fatalf("断流（无末行）竟然成功了，还会把半截文本当素材：%q", res.Text)
	}
	if !strings.Contains(err.Error(), "不完整") {
		t.Fatalf("错误文案没说清「结果不完整」：%v", err)
	}
}

// TestOCRPageStreamSkipsBrokenLine：单行坏掉不该把整轮解析判死（末行才是判定依据）。
// 注入会红的方式：遇到坏行直接 return err（一份材料的第 7 页若是编码异常，整份就废了）。
func TestOCRPageStreamSkipsBrokenLine(t *testing.T) {
	srv := pageStreamStub(t, pageStreamStubOpts{
		pages:      []string{"甲", "乙"},
		brokenLine: true,
		tailOK:     true,
	})
	got := 0
	res, err := ocrExtractPages(context.Background(), srv.URL, "手册.pdf", []byte("x"),
		(&Generator{}).ocrClient(), func(int, int, string, string) { got++ })
	if err != nil {
		t.Fatalf("中间有坏行不该判死整轮解析：%v", err)
	}
	if got != 2 {
		t.Fatalf("坏行前后的页都该收到，实际 %d 页", got)
	}
	if res.Text == "" {
		t.Fatal("末行文本丢了")
	}
}

// TestOCRPageStreamHugePageLine：单页正文超过 64KB 不许炸。
// 注入会红的方式：改用 bufio.Scanner（默认 64KB 单行上限）→ "token too long"，整份判死。
// 大字号扫描页一页正文 200KB 完全正常，这不是边界情况。
func TestOCRPageStreamHugePageLine(t *testing.T) {
	srv := pageStreamStub(t, pageStreamStubOpts{
		pages:    []string{"甲", "乙"},
		hugePage: true,
		tailOK:   true,
	})
	var big int
	_, err := ocrExtractPages(context.Background(), srv.URL, "手册.pdf", []byte("x"),
		(&Generator{}).ocrClient(), func(pno, total int, src, text string) {
			if len(text) > big {
				big = len(text)
			}
		})
	if err != nil {
		t.Fatalf("单页 200KB 不该判死解析：%v", err)
	}
	if big < 64*1024 {
		t.Fatalf("大页没被完整回调（只拿到 %d 字节）：Scanner 64KB 上限回归了", big)
	}
}

// TestOCRPageStreamSelfHealingKept：末行自报「正在自动重启」时，错误文案必须仍走
// 自愈指引（这条逻辑单 JSON 和流式共用一份实现，测的是「共用」这件事真的成立）。
// 注入会红的方式：流式路径自己写一遍判定、漏掉 SelfHealing。
func TestOCRPageStreamSelfHealingKept(t *testing.T) {
	srv := pageStreamStub(t, pageStreamStubOpts{
		pages:    []string{"甲"},
		tailOK:   false,
		tailErr:  "runtime directory lost",
		selfHeal: true,
	})
	_, err := ocrExtractPages(context.Background(), srv.URL, "手册.pdf", []byte("x"),
		(&Generator{}).ocrClient(), nil)
	if err == nil {
		t.Fatal("末行 ok=false 却成功了")
	}
	if !strings.Contains(err.Error(), "自愈") {
		t.Fatalf("自愈回包被当成了普通解析失败（用户会以为文件有问题）：%v", err)
	}
}

// TestOCRPageStreamPlainError：末行普通失败（文件真的解析不了）→ 报服务原始信息。
func TestOCRPageStreamPlainError(t *testing.T) {
	srv := pageStreamStub(t, pageStreamStubOpts{
		pages:   []string{},
		tailOK:  false,
		tailErr: "unsupported format",
	})
	_, err := ocrExtractPages(context.Background(), srv.URL, "手册.pdf", []byte("x"),
		(&Generator{}).ocrClient(), nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported format") {
		t.Fatalf("普通失败要带上服务原始信息，实际：%v", err)
	}
}

// TestOCRProgressSilentWhilePagesFlow：页在回来时**不许**插话。
// 这是「一直卡着计时」的核心：有材料在滚，再叠一句裸计时既是噪音又把材料挤走。
// 注入会红的方式：回到固定 30s 报「已等待 N秒…」（旧实现）。
func TestOCRProgressSilentWhilePagesFlow(t *testing.T) {
	oldInterval, oldStall := ocrProgressInterval, ocrStallInterval
	ocrProgressInterval, ocrStallInterval = 30*time.Millisecond, 200*time.Millisecond
	defer func() { ocrProgressInterval, ocrStallInterval = oldInterval, oldStall }()

	act := &ocrActivity{}
	var mu sync.Mutex
	var msgs []string
	steps := func(s string) { mu.Lock(); msgs = append(msgs, s); mu.Unlock() }

	stop := ocrProgress(steps, "手册.pdf", time.Now(), act)
	// 每 20ms 来一页，持续 300ms：全程「在动」，一拍都不该说话。
	for i := 0; i < 15; i++ {
		act.page(i+1, 60)
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	if len(msgs) != 0 {
		t.Fatalf("页一直在回来还插了 %d 句话（屏幕被裸计时占住）：%v", len(msgs), msgs)
	}

	// 反过来：真的连续静默超过阈值，必须说话，而且要带上页码 ——
	// 用户能分清「在慢慢啃扫描件」和「进程死了」。
	act2 := &ocrActivity{}
	act2.page(3, 60)
	msgs = nil
	stop2 := ocrProgress(steps, "手册.pdf", time.Now(), act2)
	time.Sleep(3 * ocrStallInterval)
	stop2()
	if len(msgs) == 0 {
		t.Fatal("连续静默超过阈值却一句话不说：又变回「卡死」观感")
	}
	if !strings.Contains(msgs[0], "第 3/60 页") {
		t.Fatalf("停滞提示必须带页码，实际：%q", msgs[0])
	}
}

// TestOCRProgressNoPagesFallsBackToWait：拿不到页（老服务 / 非 PDF）时退回报等待时长。
// 那是没有逐页可播报的场景，秒数就是唯一能给的活信息。
func TestOCRProgressNoPagesFallsBackToWait(t *testing.T) {
	oldInterval, oldStall := ocrProgressInterval, ocrStallInterval
	ocrProgressInterval, ocrStallInterval = 30*time.Millisecond, 30*time.Millisecond
	defer func() { ocrProgressInterval, ocrStallInterval = oldInterval, oldStall }()

	var mu sync.Mutex
	var msgs []string
	stop := ocrProgress(func(s string) { mu.Lock(); msgs = append(msgs, s); mu.Unlock() },
		"手册.pdf", time.Now(), nil)
	time.Sleep(150 * time.Millisecond)
	stop()
	if len(msgs) < 2 {
		t.Fatalf("没有逐页流时必须报「已等待」，实际 %d 条：%v", len(msgs), msgs)
	}
	if !strings.Contains(msgs[0], "已等待") {
		t.Fatalf("文案不带等待时长：%q", msgs[0])
	}
}

// TestPageSnippet：给屏幕看的这一页摘要。
func TestPageSnippet(t *testing.T) {
	if got := pageSnippet("", 60); got != "（本页无文字）" {
		t.Errorf("空页文案：%q（不能让用户以为有内容被吞了）", got)
	}
	// 取的是**本页开头的第一个非空行**（多行原文压平成一行，别把屏幕撑爆）。
	if got := pageSnippet("  \n\n  正文在这 \n 第二行", 60); got != "正文在这" {
		t.Errorf("没取到首个非空行：%q", got)
	}
	long := strings.Repeat("字", 100)
	got := pageSnippet(long, 60)
	if !strings.HasSuffix(got, "…") || len([]rune(got)) != 61 {
		t.Errorf("长行没按 60 字截断：%q（长度 %d）", got, len([]rune(got)))
	}
	// 截断必须按**字符**不是字节：中文按字节切会切出半个字，屏幕上就是乱码。
	if strings.Contains(got, "\ufffd") {
		t.Errorf("截断切坏了多字节字符：%q", got)
	}
}

// TestOCRPageStepWording：页级旁白要写成人能读的话，且带上本页真材料。
// 注入会红的方式：src 直译（屏幕上出现 "text: xxx" 这种内部黑话）。
func TestOCRPageStepWording(t *testing.T) {
	cases := []struct{ src, want string }{
		{"text", "文本层直取"},
		{"ocr", "扫描页 OCR"},
		{"empty", "无文字"},
	}
	for _, c := range cases {
		got := ocrPageStep("手册.pdf", 7, 60, c.src, "本页正文")
		if !strings.Contains(got, c.want) {
			t.Errorf("src=%s 的话术里没有 %q：%q", c.src, c.want, got)
		}
		for _, must := range []string{"第 7/60 页", "本页正文"} {
			if !strings.Contains(got, must) {
				t.Errorf("页级旁白缺 %q：%q", must, got)
			}
		}
	}
}
