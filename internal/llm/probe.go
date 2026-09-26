package llm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/lizhemin15/skillforge/internal/tlsconf"
)

// ProbeResult 是「测试连通性」按钮的结构化回执。
//
// 为什么要单独一个按钮：一条 LLM 配置有四个会各自出错的地方（地址、模型名、key、额度），
// 而**四种错的用户观感完全一样** —— 对话里没反应 / 转两下报错。用户没有任何办法自查。
// 实测到的三种原始错误（三次都发生在同一天）：
//
//	· siliconflow 欠费          → HTTP 402
//	· 讯飞maas 模型名不是路由名  → HTTP 400 + 上游码 10404 "no category route found"
//	· 讯飞maas 账号无授权额度    → HTTP 403 "no valid authorization"
//
// 三种在聊天界面上长得一模一样，但该做的事完全不同（充值 / 换模型名 / 开授权）。
// 所以回执里必须同时给：结论、归因、上游原话、实际打到的地址、耗时。
type ProbeResult struct {
	OK        bool   `json:"ok"`
	Endpoint  string `json:"endpoint"` // 运行期真正会打出去的地址（与 New 共用归一化）
	Model     string `json:"model"`
	Status    int    `json:"status"`             // HTTP 状态码；0 = 没走到 HTTP 层（DNS/连接/TLS/超时）
	APICode   string `json:"api_code,omitempty"` // 上游业务码，如 10404
	Message   string `json:"message"`            // 给人看的一句话结论（带归因与下一步）
	Reply     string `json:"reply,omitempty"`    // 模型真回的内容（证明不是空跑）
	LatencyMS int64  `json:"latency_ms"`
}

// probeMaxTokens：连通性判定不需要长回答。内网 token 慢，按一次按钮不该等一整篇；
// 但也不能小到被网关直接拒（个别平台要求 >= 16），故取 24。
const probeMaxTokens = 24

// probePrompt 要一句能一眼看出「模型真的干活了」的短回答。
const probePrompt = "只回复两个字：正常"

// ChatEndpoint 返回运行期**真正**会打的地址。
//
// 拼法与 go-openai 的 fullURL 一致（BaseURL 去尾斜杠 + "/chat/completions"），
// 归一化与 New() 共用同一处。这是整个测试按钮的可信度所在：
// 如果测试自己另写一套拼法，就会出现「测试通过、聊天打不通」（或反过来）——
// 按钮就从「省时间」变成「骗人」，用户下次不会再用它。
// TestProbeEndpointMatchesRuntime 专门断言这一条：httptest 收到的路径必须与这里相符。
func (c *Client) ChatEndpoint() string {
	if c == nil || c.cfg == nil {
		return ""
	}
	return normalizeBaseURL(c.cfg.BaseURL) + "/chat/completions"
}

// Probe 是 probe 的对外壳子，只做一件事：把回执里可能夹带的 key 擦掉。
// 之所以包一层而不是在每个 return 前手动擦 —— 以后往 probe 里加分支的人一定会忘，
// 而这里忘了是把 key 写进管理端页面，代价不对等。
//
// 判定原则（重要，别改成「有正文才算通」）：
//
//	HTTP 200 = 地址对、key 有效、模型名被认 —— 就是通。
//	正文可能是空的（思考型模型把 max_tokens 全喂给了思考链），或内容是乱码，
//	那是回答质量/预算问题，不是连通性问题。若把空正文当失败，内网慢模型会整排假红，
//	用户会把「配置明明是好的但按钮说坏了」当成按钮坏了，然后不再信它。
func (c *Client) Probe(ctx context.Context) ProbeResult {
	res := c.probe(ctx)
	if c != nil && c.cfg != nil {
		res.Message = scrubSecret(res.Message, c.cfg.APIKey)
		res.Reply = scrubSecret(res.Reply, c.cfg.APIKey)
	}
	return res
}

// probe 用一次**最小的真实调用**回答「这条配置到底通不通」。
func (c *Client) probe(ctx context.Context) ProbeResult {
	res := ProbeResult{Endpoint: c.ChatEndpoint()}
	if err := c.usable(); err != nil {
		res.Message = err.Error()
		return res
	}
	res.Model = strings.TrimSpace(c.cfg.Model)
	if strings.TrimSpace(c.cfg.APIKey) == "" {
		res.Message = "未配置 API Key：先在管理端填好并保存"
		return res
	}
	if res.Model == "" {
		res.Message = "未配置模型名：可点「获取模型」拉取该服务的清单后选择"
		return res
	}

	req := openai.ChatCompletionRequest{
		Model:     res.Model,
		Messages:  []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: probePrompt}},
		MaxTokens: probeMaxTokens,
	}
	start := time.Now()
	resp, err := c.cli.CreateChatCompletion(ctx, req)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		return c.probeFailed(res, err)
	}

	res.OK = true
	res.Status = http.StatusOK
	if len(resp.Choices) > 0 {
		res.Reply = strings.TrimSpace(resp.Choices[0].Message.Content)
	}
	if res.Reply == "" {
		res.Message = fmt.Sprintf("连通正常（%dms）：上游 200，但正文是空的 —— 多半是思考链吃光了 max_tokens，连通性没问题",
			res.LatencyMS)
	} else {
		res.Message = fmt.Sprintf("连通正常（%dms）：模型回了「%s」", res.LatencyMS, clipRunes(res.Reply, 40))
	}
	return res
}

// scrubSecret 把回执里可能夹带的 key 换成 ***。
// 个别网关会把请求头原样抄进错误消息（"invalid key: Bearer xxx"），
// 而这条回执要显示在管理端、还会被用户复制去问客服 —— 不能顺手把 key 带出去。
// 擦洗放在 llm 层（真正知道 key 的地方），api 层再擦一次做纵深防御。
func scrubSecret(s, secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" || len(secret) < 6 {
		return s
	}
	return strings.ReplaceAll(s, secret, "***")
}

// probeFailed 把上游的错翻成「用户下一步该改什么」。
// 顺序要求：先给结论（哪一项配置不对），再附上游原话 —— 只给结论的话，
// 用户没法验证我们猜得对不对，猜错时会被带偏。
func (c *Client) probeFailed(res ProbeResult, err error) ProbeResult {
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		res.Status = apiErr.HTTPStatusCode
		res.APICode = apiCodeString(apiErr.Code)
		res.Message = probeHint(res, apiErr.Message)
		return res
	}
	// 网关回的不是 OpenAI 形状的 error 对象（纯文本 / 裸数组）时 go-openai 给的是这个，
	// 状态码和原始正文仍拿得到 —— 归因不能因为「正文形状不对」就丢掉状态码。
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) {
		res.Status = reqErr.HTTPStatusCode
		res.Message = probeHint(res, string(reqErr.Body))
		return res
	}

	// 没走到 HTTP 层：DNS / 连接 / TLS / 超时。证书类交给项目统一翻译（含自查命令），
	// 地址也一并带上 —— 内网常有多套网关，只说「证书不可信」用户不知道该修哪一台。
	msg := tlsconf.Explain(err, res.Endpoint).Error()
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		msg = fmt.Sprintf("按 %dms 的超时没等到响应：地址不通、或该网关响应太慢。%s", res.LatencyMS, msg)
	}
	res.Message = msg
	return res
}

// probeHint 把 HTTP 状态 + 上游正文翻译成「该动哪一项」。
// 每个分支都要带上游原话：用户可能拿着它去问平台客服。
func probeHint(res ProbeResult, upstream string) string {
	up := clipRunes(strings.Join(strings.Fields(upstream), " "), 200)
	if up == "" {
		up = "(上游没给错误正文)"
	}
	head := fmt.Sprintf("打到 %s（HTTP %d", res.Endpoint, res.Status)
	if res.APICode != "" {
		head += "，上游码 " + res.APICode
	}
	head += "）："

	switch {
	case res.Status == http.StatusUnauthorized: // 401
		return head + "API Key 无效或已过期 —— 确认这把 key 属于该平台，且前后没有多余空格。" + up
	case res.Status == http.StatusPaymentRequired: // 402
		return head + "账号余额/额度不足 —— 到平台充值，或换一条可用配置。" + up
	case res.Status == http.StatusForbidden: // 403
		return head + "账号没有有效授权/额度：key 认得出，但没开通这个模型或用完了配额（讯飞maas 实测就是这个，不是 key 写错）。" + up
	case res.Status == http.StatusNotFound: // 404
		return head + "地址不对：这个地址下没有 chat/completions —— 检查 Base URL 是否多写/少写了路径段（如 /v1、/v2、/compatible-mode/v1）。" + up
	case res.Status == http.StatusTooManyRequests: // 429
		return head + "被限流：稍后重试；持续如此说明该 key 的并发/频率配额已满。" + up
	case res.Status == http.StatusBadRequest && isRouteMissingErr(res.APICode, upstream):
		// 讯飞maas 的原话是 no category route found —— 它要「路由名」而不是模型本名。
		return head + "模型名不是该平台认的路由名 —— 有些平台（如讯飞maas）要求填控制台下发的路由名，" +
			"不是模型本名（实测 Qwen3.6-35B-A3B 报此错，换成控制台里的路由名才过）。去平台控制台复制正确的名字。" + up
	case res.Status == http.StatusBadRequest: // 400
		return head + "上游认为这次请求不合法：最可能是模型名不对，其次是不支持非流式调用或 max_tokens 受限。" + up
	case res.Status == http.StatusUnprocessableEntity: // 422
		return head + "参数不合法（此处通常就是模型名不对）。" + up
	case res.Status >= 500 && res.Status <= 599:
		return head + "上游故障或网关抖动：配置本身可能没问题，稍后重试。" + up
	}
	return head + "上游拒绝了这次请求。" + up
}

// isRouteMissingErr 认「模型名不是路由名」这一类错误。
// 不能只看状态码：同样是 400，模型名写错、参数写错、额度用尽都可能回 400，
// 而只有这一类该提示「去控制台复制路由名」。
func isRouteMissingErr(apiCode, upstream string) bool {
	if strings.TrimSpace(apiCode) == "10404" {
		return true
	}
	l := strings.ToLower(upstream)
	for _, s := range []string{"no category route", "category route", "route not found", "no route"} {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

// apiCodeString：上游码可能是数字也可能是字符串（go-openai 的 Code 是 any），统一成字符串。
func apiCodeString(code any) string {
	switch v := code.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.Itoa(int(v))
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

// clipRunes 按字符（而非字节）截断，避免把中文切半个字。
func clipRunes(s string, max int) string {
	rs := []rune(strings.TrimSpace(s))
	if len(rs) <= max {
		return string(rs)
	}
	return string(rs[:max]) + "…"
}
