package ocrsvc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"
)

// 本文件锁死本轮投诉的形态：目标机上解析服务没在运行时，主服务原样把
// `Post "http://127.0.0.1:8093/extract": dial tcp 127.0.0.1:8093: connect: connection refused`
// 抛给用户。用户看到的是「训练技能报错」，既判断不出这是环境问题（不是他文件的问题），
// 也不知道下一步敲什么命令 —— 于是只能来问。
//
// 所以这里测的不是「字符串好看」，而是三件有判别力的事：
//  1. 连不上 ⇒ 消息里必须出现**可执行**的修复命令 + 明确「不是文件问题」；
//  2. 原始错误必须仍可 errors.Is 到（refused）/ Unwrap 到，排障不能瞎；
//  3. 非连不上类错误（超时、解析失败）**原样透传**，不许乱加环境指引把人带偏。

// deadURL 返回一个「端口上确实没有监听者」的地址：先占一个空闲端口再关掉。
// 用它而不用写死 8093，是为了让测试在任何机器上都真能拿到 ECONNREFUSED。
func deadURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口失败：%v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "http://" + addr
}

// realRefusedErr 拿到真实内核给的 refused 错误（走真 http 客户端，不手捏错误对象）。
func realRefusedErr(t *testing.T, svcURL string) error {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequest(http.MethodPost, svcURL+"/extract", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("造请求失败：%v", err)
	}
	_, err = client.Do(req)
	if err == nil {
		t.Fatal("对没有监听者的端口发请求竟然成功了：测试前提不成立")
	}
	return err
}

// TestExplainRefusedGivesActionableChinese：refused ⇒ 人话 + 可执行命令。
// 注入会红的方式：把 Explain 的 return 改回 return err（事故原样）→ 后四条断言全红。
func TestExplainRefusedGivesActionableChinese(t *testing.T) {
	svcURL := deadURL(t)
	raw := realRefusedErr(t, svcURL)

	if !Unreachable(raw) {
		t.Fatalf("真实 refused 没被判定成 Unreachable：%v", raw)
	}

	got := Explain(raw, svcURL).Error()
	// 1) 必须给出「去重启服务」这条可执行动作（默认单元名）。
	if !strings.Contains(got, "systemctl restart "+ProbingService+DefaultUnitSuffix) {
		t.Errorf("修复命令缺失：%q", got)
	}
	// 2) 必须点明这是环境问题、不是他的文件问题 —— 这是用户最需要的那句话。
	if !strings.Contains(got, "不是你的文件有问题") {
		t.Errorf("没有区分「环境问题 / 文件问题」：%q", got)
	}
	// 3) 必须报出**这次真的连不上的地址**（端口要现取，不能写死 8093）。
	if !strings.Contains(got, Endpoint(svcURL)) {
		t.Errorf("消息里没有出现真实地址 %s：%q", Endpoint(svcURL), got)
	}
	// 4) 排障入口要给全：status / journalctl。
	for _, want := range []string{"systemctl status", "journalctl -u"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少排障入口 %q：%q", want, got)
		}
	}
	// 5) 底层噪音只允许出现在**带标签的附录**里，且必须排在修复指引之后：
	//    用户第一眼要看到「怎么修」，而不是 Go 的包装文本。原始错误不能整个丢
	//    （排障要靠它分清 refused / DNS / 路由），但也不许顶在开头。
	firstLineOf := got
	if i := strings.IndexByte(got, '\n'); i >= 0 {
		firstLineOf = got[:i]
	}
	if strings.Contains(firstLineOf, "dial tcp") {
		t.Errorf("第一行就是底层噪音，用户会以为是自己文件的问题：%q", firstLineOf)
	}
	if !strings.Contains(got, "原始错误") {
		t.Errorf("原始错误必须带标签保留（排障要用）：%q", got)
	}
	if i, j := strings.Index(got, "systemctl restart"), strings.Index(got, "dial tcp"); j >= 0 && (i < 0 || j < i) {
		t.Errorf("底层噪音排在修复指引之前：%q", got)
	}
	if !strings.Contains(firstLine(raw.Error()), "connection refused") {
		t.Fatalf("测试前提不成立：原始错误里没有 refused：%v", raw)
	}
}

// TestExplainKeepsCauseReachable：翻译归翻译，原始错误必须还能 errors.Is 到。
// 否则线上排障就只能看人话，分不清 refused / DNS / 路由 —— 这比不改还糟。
// 注入会红的方式：Explain 里用 fmt.Errorf("%s", ...) 丢掉 cause（或 serviceError 去掉 Unwrap）。
func TestExplainKeepsCauseReachable(t *testing.T) {
	svcURL := deadURL(t)
	raw := realRefusedErr(t, svcURL)

	got := Explain(raw, svcURL)
	if !errors.Is(got, syscall.ECONNREFUSED) {
		t.Errorf("Explain 之后 errors.Is(err, ECONNREFUSED) 不成立：原始错误被丢了\n%v", got)
	}
	var opErr *net.OpError
	if !errors.As(got, &opErr) {
		t.Errorf("Explain 之后 errors.As 拿不到 *net.OpError：排障信息被丢")
	}
	if got.Error() == raw.Error() {
		t.Error("Explain 没有替换用户可见文案（等于没翻译）")
	}
}

// TestExplainLeavesContentErrorsAlone：超时/解析失败不是环境问题，不许加环境指引。
// 注入会红的方式：把 Unreachable 的判定放宽成「err != nil 就算连不上」→ 本测试立刻红。
func TestExplainLeavesContentErrorsAlone(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"超时", context.DeadlineExceeded},
		{"解析业务错", errors.New("解析服务: 这份 PDF 加密了")},
		{"响应不可识别", fmt.Errorf("解析服务返回无法识别: %w", errors.New("bad json"))},
	}
	for _, c := range cases {
		got := Explain(c.err, "http://127.0.0.1:8093")
		if got.Error() != c.err.Error() {
			t.Errorf("%s：不该被翻译，却被改写成 %q", c.name, got.Error())
		}
		if strings.Contains(got.Error(), "systemctl restart") {
			t.Errorf("%s：非环境错误却塞了环境指引，会把用户带偏：%q", c.name, got.Error())
		}
	}
	if Explain(nil, "http://x") != nil {
		t.Error("nil 错误必须原样返回 nil")
	}
}

// TestUnreachablePositiveAndNegative：判定面必须两边都挡住。
func TestUnreachablePositiveAndNegative(t *testing.T) {
	yes := []error{
		syscall.ECONNREFUSED,
		syscall.EHOSTUNREACH,
		fmt.Errorf("wrap: %w", syscall.ECONNREFUSED),
		&net.DNSError{Err: "no such host", Name: "ocrd"},
		errors.New("dial tcp 127.0.0.1:8093: connect: connection refused"),
	}
	for _, e := range yes {
		if !Unreachable(e) {
			t.Errorf("%v 应判为连不上", e)
		}
	}
	no := []error{
		nil,
		context.DeadlineExceeded,
		context.Canceled,
		errors.New("解析服务: too_large"),
		errors.New("解析服务返回无法识别: unexpected end of JSON input"),
	}
	for _, e := range no {
		if Unreachable(e) {
			t.Errorf("%v 不该判为连不上", e)
		}
	}
}

// TestCheckDistinguishesNotRunningFromRuntimeLost：两种坏态必须分开报。
//
// 它们给用户的动作完全不同：进程没起来 → 去 restart/重装；进程活着但引擎坏了
// → 它自己会重启，等 10 秒重试。只报一个 bool 就必然把其中一种说错。
// 注入会红的方式：Check 里不解析 runtime_ok（一律按 healthy 处理）→ 第二组红。
func TestCheckDistinguishesNotRunningFromRuntimeLost(t *testing.T) {
	// ① 没在监听
	dead := deadURL(t)
	if h := Check(context.Background(), dead); h.Running || h.Healthy() || h.Detail() == "" {
		t.Fatalf("空端口应判为没在运行且有话可说，得到 %+v", h)
	} else {
		// 排查入口必须给全：线上取证（ocr_dependency_ux_live_check.py 的 leg A）
		// 抓出过一条真缺口 —— 预检只给 restart、没给 status/journalctl，
		// 用户遇到「restart 也不行」就没有下一步。这条断言把它钉住。
		for _, want := range []string{"systemctl restart", "systemctl status", "journalctl"} {
			if !strings.Contains(h.Detail(), want) {
				t.Errorf("预检文案缺少 %q：%q", want, h.Detail())
			}
		}
	}

	// ② 在监听 + runtime_ok=true
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true,"service":"ocrd","version":"v5-runtime-guard","runtime_ok":true}`)
	}))
	defer okSrv.Close()
	h := Check(context.Background(), okSrv.URL)
	if !h.Healthy() || h.Detail() != "" {
		t.Fatalf("健康服务不该有告警文案，得到 %+v", h)
	}
	if h.Version != "v5-runtime-guard" {
		t.Errorf("没读到 version：%+v", h)
	}

	// ③ 在监听 + runtime_ok=false（引擎坏了，正在自愈）
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":false,"service":"ocrd","runtime_ok":false}`)
	}))
	defer badSrv.Close()
	h = Check(context.Background(), badSrv.URL)
	if !h.Running {
		t.Error("端口有响应就该算 Running（否则会把「引擎坏了」误报成「没装」）")
	}
	if h.Healthy() {
		t.Error("runtime_ok=false 不该判为健康")
	}
	if !strings.Contains(h.Detail(), "运行时已损坏") {
		t.Errorf("引擎坏掉的文案不对：%q", h.Detail())
	}

	// ④ 老版本 ocrd 没有 runtime_ok 字段 → 以 ok 为准
	oldSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true,"service":"ocrd","version":"v1"}`)
	}))
	defer oldSrv.Close()
	if h = Check(context.Background(), oldSrv.URL); !h.Healthy() {
		t.Errorf("无 runtime_ok 字段的老版本应以 ok 为准：%+v", h)
	}

	// ⑤ 空地址：不探活，也不算健康（ocrURL="" 代表显式禁用）
	if h = Check(context.Background(), ""); h.Running || h.Healthy() {
		t.Errorf("空地址不该判为健康：%+v", h)
	}
}

// TestCheckNeverBlocksLongerThanProbeTimeout：探活不许把训练卡在黑洞上。
// 这条针对的是「用户等了几分钟页面没反应」那类体感问题：对端丢包时，
// 探活必须自己认输，而不是把默认 30 分钟的解析超时耗完。
func TestCheckNeverBlocksLongerThanProbeTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // 永不回包
	}))
	defer func() { close(block); srv.Close() }()

	start := time.Now()
	h := Check(context.Background(), srv.URL)
	elapsed := time.Since(start)
	if elapsed > ProbeTimeout*3 {
		t.Fatalf("探活耗时 %s，远超上限 %s", elapsed, ProbeTimeout)
	}
	if h.Healthy() {
		t.Error("黑洞服务不该判为健康")
	}
	if h.Detail() == "" {
		t.Error("探活失败必须给出人话")
	}
}

// TestSelfHealingRecognition：识别 ocrd 的「运行时损坏、正在自愈」回包。
// 注入会红的方式：SelfHealing 恒 false → 本测试红（线上表现就是用户看到内部文案后不再重试）。
func TestSelfHealingRecognition(t *testing.T) {
	body := []byte(`{"ok":false,"error":"运行时目录已丢失","runtime_ok":false,"self_healing":true}`)
	if !SelfHealing(body) {
		t.Fatal("self_healing=true 没被识别")
	}
	if SelfHealing([]byte(`{"ok":false,"error":"这份 PDF 加密了"}`)) {
		t.Error("普通解析失败被误判为自愈中")
	}
	if SelfHealing([]byte("not json")) {
		t.Error("非 JSON 不该被当成自愈中")
	}
	msg := SelfHealingMessage("http://127.0.0.1:8093", "运行时目录已丢失")
	if !strings.Contains(msg, "重试") || !strings.Contains(msg, "文件本身没有问题") {
		t.Errorf("自愈文案没告诉用户「重试即可」：%q", msg)
	}
}

// TestUnitNameHonorsInstanceOverride：多实例机器上单元名可能是 myforge-ocr，
// 给出的命令必须是那个名字，否则用户敲了 systemctl restart skillforge-ocr 会得到
// unit not found —— 又是一次「照做还是不行」。
func TestUnitNameHonorsInstanceOverride(t *testing.T) {
	if got := UnitName(); got != "skillforge-ocr" {
		t.Errorf("默认单元名 %q，期望 skillforge-ocr", got)
	}
	t.Setenv("SKILLFORGE_SERVICE_NAME", "myforge")
	if got := UnitName(); got != "myforge-ocr" {
		t.Errorf("覆盖后单元名 %q，期望 myforge-ocr", got)
	}
}

// TestEndpointShapes：地址解析要容错（配错成裸 host:port 也要能报出地址）。
func TestEndpointShapes(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:8093":     "127.0.0.1:8093",
		"http://127.0.0.1:8093/":    "127.0.0.1:8093",
		"127.0.0.1:8094":            "127.0.0.1:8094",
		"http://ocr.internal:9000/": "ocr.internal:9000",
	}
	for in, want := range cases {
		if got := Endpoint(in); got != want {
			t.Errorf("Endpoint(%q) = %q，期望 %q", in, got, want)
		}
	}
	if got := Endpoint(""); got != "" {
		t.Errorf("空地址应返回空串，得到 %q", got)
	}
}
