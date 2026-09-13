package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

// 「哪些错误值得重试」这一层必须钉死，因为它的两个方向都真金白银：
//
//   - 漏判（把瞬时当永久）：线上已踩过一次——Step 8.5 第一轮撞上供应商瞬时
//     503（System is too busy now），整个裁判层当场放弃，3 轮预算归零。
//   - 误判（把永久当瞬时）：钥匙错了却退避重试三次，用户等 20 秒才被告知
//     配置不对；更糟的是「手册格式错了」这种能被立刻修的问题被拖成超时。
//
// 断言全部对着**真实字符串**（testLive503 就是线上 fidelity.md 里那一行原文，
// 只差外层 skillgen 的 %w 包装），不对着我臆想的错误格式。

// testLive503 复刻线上真值：judgeDraft 用 %w 包住 go-openai 的 RequestError。
const testLive503Body = `{"code":50508,"message":"System is too busy now. Please try again later.","data":null}`

func liveRequestErr503() error {
	return &openai.RequestError{
		HTTPStatusCode: 503,
		HTTPStatus:     "503 Service Unavailable",
		Err:            errors.New("openai: no choices"),
		Body:           []byte(testLive503Body),
	}
}

// liveJudgeErr 就是线上 fidelity.md 里那行：judgeDraft: 裁判调用失败: <RequestError>
func liveJudgeErr() error {
	return fmt.Errorf("judgeDraft: 裁判调用失败: %w", liveRequestErr503())
}

func TestIsTransientLive503IsTransient(t *testing.T) {
	err := liveJudgeErr()
	if !IsTransient(err) {
		t.Fatalf("线上那次的 503 必须判为可重试，否则裁判层还会被一次抖动清空：%v", err)
	}
	// 结构化路径要能穿透 %w 包装（错误分类靠它，不靠字符串碰运气）。
	var req *openai.RequestError
	if !errors.As(err, &req) {
		t.Fatal("errors.As 应能穿过 %w 拿到 *openai.RequestError —— 拿不到说明包装链断了")
	}
	if req.HTTPStatusCode != 503 {
		t.Fatalf("状态码解析错：%d", req.HTTPStatusCode)
	}
}

func TestIsTransientTable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"线上真实503(RequestError穿透%w)", liveJudgeErr(), true},
		{"限流429", &openai.RequestError{HTTPStatusCode: 429, Body: []byte(`{"code":"rate_limit_exceeded"}`)}, true},
		{"请求超时408", &openai.RequestError{HTTPStatusCode: 408}, true},
		{"上游502网关抖动", &openai.RequestError{HTTPStatusCode: 502}, true},
		{"APIError 500", &openai.APIError{HTTPStatusCode: 500, Message: "internal"}, true},
		{"body code=server_error(HTTP200)", &openai.APIError{HTTPStatusCode: 200, Code: "server_error"}, true},
		{"APIError 裸文本5xx(无状态码结构)", errors.New("upstream boom"), false},

		// 文案兜底：网关把 5xx 包成普通 error，状态码只在文本里。
		{"文案含status code: 503", errors.New("error, status code: 503, status: 503 Service Unavailable"), true},
		{"文案含too busy", errors.New("System is too busy now. Please try again later."), true},
		{"文案含connection reset", errors.New("read tcp: connection reset by peer"), true},
		{"文案含deadline exceeded", errors.New("Post \"https://x\": context deadline exceeded"), true},

		// 不该重试：配置/请求本身坏了，重试只是拖时间。
		{"钥匙不对401", &openai.RequestError{HTTPStatusCode: 401, Body: []byte(`{"message":"invalid api key"}`)}, false},
		{"无权限403", &openai.RequestError{HTTPStatusCode: 403}, false},
		{"请求非法400", &openai.RequestError{HTTPStatusCode: 400, Body: []byte(`{"message":"model not found"}`)}, false},
		{"文案含401", errors.New("error, status code: 401, status: 401 Unauthorized, message: invalid api key"), false},
		{"模型不存在404", errors.New("error, status code: 404, message: model not found"), false},
		{"JSON解析失败", errors.New("judgeDraft: 裁判输出不是合法 JSON"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsTransient(c.err); got != c.want {
				t.Fatalf("IsTransient=%v，期望 %v；错误：%v", got, c.want, c.err)
			}
		})
	}
}

// 取消是上层主动放弃，重试只会拖住取消 —— 用户点了取消却还要等 20 秒才停，很糟。
func TestIsTransientCanceledIsNotRetried(t *testing.T) {
	if IsTransient(context.Canceled) {
		t.Fatal("context.Canceled 不该重试")
	}
	if IsTransient(fmt.Errorf("judgeDraft: %w", context.Canceled)) {
		t.Fatal("包在 %w 里的 context.Canceled 也不该重试")
	}
	if !IsTransient(context.DeadlineExceeded) {
		t.Fatal("本端 deadline 可能是上游太慢，值得重试一次")
	}
	// 网络层超时是典型瞬时故障。
	var ne net.Error = &net.DNSError{IsTimeout: true}
	if !IsTransient(ne) {
		t.Fatal("网络超时应判为瞬时")
	}
	if !IsTransient(io.ErrUnexpectedEOF) {
		t.Fatal("流式响应被截断应判为瞬时")
	}
}

// NormalizeErr 只加身份、不改文案：错误信息进 fidelity.md 和界面，多一层前缀
// 就是往用户看板里塞噪音，也会打断既有的错误断言。
func TestNormalizeErrKeepsMessage(t *testing.T) {
	raw := liveJudgeErr()
	got := NormalizeErr(raw)
	if got == nil {
		t.Fatal("不该把错误变成 nil")
	}
	if got.Error() != raw.Error() {
		t.Fatalf("文案被改了：\n原 %q\n新 %q", raw.Error(), got.Error())
	}
	if !IsTransient(got) {
		t.Fatal("归一化后应保持可重试身份")
	}
	var te *TransientError
	if !errors.As(got, &te) {
		t.Fatal("应包成 *TransientError")
	}
	// 幂等：重复归一化不该套娃。
	if again := NormalizeErr(got); again.Error() != got.Error() || !IsTransient(again) {
		t.Fatalf("NormalizeErr 不幂等：%v", again)
	}
	// 非瞬时错误原样返回（同一对象），调用方可以照旧做 errors.Is/As 判定。
	plain := errors.New("judgeDraft: 裁判输出不是合法 JSON")
	if out := NormalizeErr(plain); out != plain {
		t.Fatal("非瞬时错误应原样返回，不该被包装")
	}
	if NormalizeErr(nil) != nil {
		t.Fatal("nil 应原样返回 nil")
	}
	if !strings.Contains(te.Error(), "503") {
		t.Fatalf("包装后仍应看得到 503：%q", te.Error())
	}
}
