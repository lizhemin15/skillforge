package llm

import (
	"context"
	"errors"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

// 这一文件的由来（线上事故）：训练期的 Step 8.5 裁判层第一轮撞上供应商一次
// 瞬时 503（「System is too busy now」），循环立刻放弃整个裁判层，技能照常落盘，
// 但 fidelity.md 里只留下一句「裁判未跑完」。3 轮预算被一次抖动清空——
// 这不是供应商的问题，是调用方没把「等一下再试就好」和「配置错了，再试也没用」分开。
//
// 分工：这里只负责**判定**错误是不是瞬时的（不负责重试）；
// 重试策略（几次、隔多久、帧里怎么说）留在调用方（skillgen 的裁判循环），
// 因为「重试算不算一轮」这类语义只有循环自己知道。

// TransientError 包一层「等会儿重试可能就成功」的错误。
// Error() 原样透传底层文案，不打自己的前缀——错误信息是给人看的，
// 不该因为多包了一层就多一段噪音。
type TransientError struct{ Err error }

func (e *TransientError) Error() string { return e.Err.Error() }
func (e *TransientError) Unwrap() error { return e.Err }

// IsTransient 报告 err 是否属于「重试有意义」的故障。
//
// 判定分两路，任一路命中即可：
//
//  1. 结构化：供应商 SDK 返回的错误带状态码（*openai.RequestError / *openai.APIError），
//     以及网络层错误（超时、连接被重置）。
//  2. 文案兜底：有些 OpenAI 兼容网关把 5xx 包成普通 error，状态码不进结构体，
//     只留在文本里。此时按关键词与 `status code: NNN` 文本判定。
//
// 兜底为什么必要：漏判（把瞬时当永久）的代价是功能静默降级——就是这次事故；
// 误判（把永久当瞬时）的代价只是多花一次退避时间，然后照样报错。
// 两者代价不对称，所以兜底宁可宽一点。
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	var te *TransientError
	if errors.As(err, &te) {
		return true
	}
	// 上下文取消是用户/上层主动放弃，重试只会拖住取消。
	if errors.Is(err, context.Canceled) {
		return false
	}
	// 超时：可能是本端 deadline，也可能是上游太慢。都值得重试一次。
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}

	// 网络层：超时、连接被重置/中断。ECONNREFUSED 也算——上游重启的窗口里
	// 就是这个错误，而「地址填错了」会在重试耗尽后仍然报同一个错，不会更糟。
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	if msg := err.Error(); transientByText(msg) {
		return true
	}

	var req *openai.RequestError
	if errors.As(err, &req) && isTransientStatus(req.HTTPStatusCode) {
		return true
	}
	var api *openai.APIError
	if errors.As(err, &api) && (isTransientStatus(api.HTTPStatusCode) || isTransientByAPICode(api)) {
		return true
	}
	return false
}

// isTransientStatus：429（限流）、408（请求超时）、5xx（上游过载/网关抖动）。
// 4xx 其余一律不重试：400 是请求本身坏了，401/403 是钥匙不对，重试纯浪费。
func isTransientStatus(code int) bool {
	return code == 429 || code == 408 || (code >= 500 && code <= 599)
}

// isTransientByAPICode 认供应商用 JSON body 里的 code 表达过载的情况
// （例如某些网关返回 HTTP 200 + body 里 code=server_error）。
// APIError.Code 是 any：不同网关给字符串或数字，都取字符串形态比一句。
func isTransientByAPICode(e *openai.APIError) bool {
	if e == nil || e.Code == nil {
		return false
	}
	var code string
	switch v := e.Code.(type) {
	case string:
		code = v
	case float64:
		return isTransientStatus(int(v))
	case int:
		return isTransientStatus(v)
	default:
		return false
	}
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "server_error", "overloaded_error", "rate_limit_exceeded", "rate_limit_error",
		"service_unavailable", "timeout", "internal_error":
		return true
	}
	return false
}

var statusCodeRe = regexp.MustCompile(`status code:\s*(\d{3})`)

func transientByText(msg string) bool {
	if m := statusCodeRe.FindStringSubmatch(msg); len(m) == 2 {
		if code, err := strconv.Atoi(m[1]); err == nil && isTransientStatus(code) {
			return true
		}
	}
	l := strings.ToLower(msg)
	for _, s := range []string{
		"too busy",              // 硅基流动/部分国产网关的过载文案
		"rate limit",            // 限流
		"temporarily unavailable",
		"service unavailable",
		"bad gateway",
		"gateway timeout",
		"overloaded",
		"try again later",
		"connection reset",
		"connection refused",
		"broken pipe",
		"unexpected eof",
		"i/o timeout",
		"timed out",
		"deadline exceeded",
	} {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

// NormalizeErr 给调用方统一出口：瞬时故障包成 *TransientError，其余原样返回。
// 错误文案不变，所以既有的错误断言不受影响。
func NormalizeErr(err error) error {
	if err == nil {
		return nil
	}
	if IsTransient(err) {
		var te *TransientError
		if errors.As(err, &te) {
			return err
		}
		return &TransientError{Err: err}
	}
	return err
}
