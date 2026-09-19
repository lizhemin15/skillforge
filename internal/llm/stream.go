package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lizhemin15/skillforge/internal/tlsconf"
)

// StreamOpts 控制一次流式调用的可选行为。
//
// 存在的理由是线上这两件事同时成立：
//
//  1. 路由/抽取类调用（意图分类、分类路由、文档要素抽取）**不需要思考链**，
//     但线上活跃模型是 reasoning 模型，思考链不关就必然几十秒（实测
//     siliconflow/Qwen3.6-27B：带思考 63.0s / 关思考 4.8s，同一段提示词）。
//     go-openai 的 ChatCompletionRequest 表达不了 provider 私有开关
//     （enable_thinking / reasoning_effort），所以这条路必须自己拼 body。
//  2. 长文执笔**保留思考链质量更好**，但思考链期间正文一个字都没有，用户看到的
//     就是「一直卡着计时」。所以思考链片段要能**流出来**当中间材料给用户看。
type StreamOpts struct {
	// DisableThinking 请求 provider 关掉思考链。两族开关都带（互不通用，见 fastjson.go）：
	// enable_thinking 给 Qwen 系，reasoning_effort 给 astron 系。
	DisableThinking bool
	// JSONMode 要求 provider 直接吐 JSON 对象（结构化消费时必须开）。
	JSONMode bool
	// MaxTokens >0 时带上上限。
	MaxTokens int
	// OnReasoning 收思考链片段（中间材料）。可为 nil。
	OnReasoning func(string)
	// OnContent 收正文片段。可为 nil。
	OnContent func(string)
	// OnNote 收「调用方该知道的旁白」（重试、降级、放大预算…）。可为 nil。
	//
	// 单独一个回调而不是混进 OnReasoning：思考链是模型说的话，旁白是流水线自己
	// 说的话，前端按类别分开显示（skillgen 的 MaterialThink / MaterialNote）。
	// 混在一起的话，用户会把「系统正在重试」误读成模型想到了重试这件事。
	OnNote func(string)
}

// StreamChat 走流式 chat/completions，把思考链与正文片段分别交给回调，返回正文全文。
//
// 与 Complete 的分工：Complete 用 go-openai 且不带任何 provider 私有开关；
// StreamChat 自己拼 body，因此能带关思考链的开关，也知道怎么把 reasoning_content
// 与 content 分开。
//
// 2026-09-20 起**正文执笔也走这条路**（StreamChat + DisableThinking=false，即 knobNone，
// 请求体与 CompleteEx 逐字一致）：go-openai 那条隧道不认 SKILLFORGE_THINK_BUDGET，
// 也没有这里的空闲看门狗与断流重试，而执笔是整轮最长的一跳——上游一慢就是「一直卡着
// 计时」（线上实测首正文 347.8s、最大静默 297.7s）。Complete/CompleteEx 保留给需要
// go-openai 语义的老调用方。
func (c *Client) StreamChat(ctx context.Context, sys, user string, o StreamOpts) (string, error) {
	if err := c.usable(); err != nil {
		return "", err
	}
	if strings.TrimSpace(c.cfg.APIKey) == "" {
		return "", errors.New("未配置 LLM API Key（请在管理端配置）")
	}

	// 与 FastJSON 同构的三段重试：① 网关 400（严格校验未知字段）→ 摘掉非标准的
	// enable_thinking，留下真正管用的 reasoning_effort；② 网关 400 且开了 JSONMode
	// → 摘掉 response_format（部分 provider 的 json_object 与推理模型不兼容）；
	// ③ 200 但正文空（哨兵 ErrEmptyContent）→ 放大输出预算重试。
	//
	// 第 ③ 段是这次线上事故的正面回击：step2 元数据调用拿到空 content，
	// 调用方把空串喂给 json.Unmarshal，用户看到
	// 「模型输出不是合法json unexpected end of json input 原文=<<>>」。
	// fastjson 那条路早就有这段（errEmptyContent + 放大预算），流式这条没有——
	// 同一件事两套标准，于是修在缺的那一边。
	knob := knobBoth
	if !o.DisableThinking {
		knob = knobNone
	}
	opt := o
	triedBigger, droppedJSON, triedPartial := false, false, false
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		content, status, err := c.streamOnce(ctx, sys, user, opt, knob)
		if err == nil {
			return content, nil
		}
		lastErr = err
		switch {
		case status == http.StatusBadRequest && opt.JSONMode && !droppedJSON:
			// 有的网关/模型组合不吃 response_format=json_object，直接 400。
			// 提示词里本来就写着「只输出JSON」，且调用方用 extractJSON 兜底，
			// 所以摘掉它重试是安全的；不摘就只能把 400 原样丢给用户。
			droppedJSON = true
		case status == http.StatusBadRequest && knob == knobBoth:
			// 网关不认 enable_thinking（严格校验未知字段的 Azure / 部分自建）。
			knob = knobEffortOnly
		case IsStreamBroken(err) && !triedPartial && ctx.Err() == nil:
			// 流断在半路（上游把连接挂住 / 没给结束标记）。与 5xx 的区别很关键：
			// 5xx 是「请求没成」，交给上层决定重试；这里是「请求成了但回答不完整」，
			// 当场重试一次最省事——而且绝不能把半截正文漏给调用方（那会变成
			// 「模型输出不是合法json」这种指向错方向的诊断）。
			triedPartial = true
			if opt.OnNote != nil {
				opt.OnNote("上游流式中断（连接被挂住或没有结束标记），正在重试一次…")
			}
		case IsEmptyContent(err) && !triedBigger && ctx.Err() == nil:
			// 200 但一个字正文都没有 ⇒ 思考链把 completion 预算吃光了。
			// 放大预算再问一次：慢，但比「整个阶段失败」强。
			// 上限见 maxEmptyRetryTokens 的注释。
			triedBigger = true
			mt := opt.MaxTokens
			if mt <= 0 {
				// 没设过上限时 provider 用的是它自己的默认值（可能很小）。
				// 显式给一个下限，否则「放大」这件事无从谈起。
				mt = emptyRetryBaseTokens
			}
			if mt *= 4; mt > maxEmptyRetryTokens {
				mt = maxEmptyRetryTokens
			}
			opt.MaxTokens = mt
			if opt.OnNote != nil {
				// 用户这一轮白等了几十秒，得当场知道为什么，以及系统正在做什么。
				// 不说的话，材料流会莫名其妙地从头再念一遍思考链。
				opt.OnNote(fmt.Sprintf("这一轮模型只回了思考过程、没有正文，正在放大输出预算到 max_tokens=%d 重试一次…", mt))
			}
		default:
			return content, err
		}
	}
	return "", lastErr
}

const (
	// emptyRetryBaseTokens：调用方没指定 max_tokens 时的放大起点（×4 = 4096）。
	emptyRetryBaseTokens = 1024
	// maxEmptyRetryTokens 是空正文重试的预算上限。比 fastjson 的 4096 宽：
	// 这条路上跑的是长文执笔与结构化产出，4096 可能把稿子截断；而且只有
	// 「第一轮一个字都没吐出来」才会走到这里，不存在常态多花钱的问题。
	maxEmptyRetryTokens = 8192
)

// 流「断在半路」的两枚哨兵。
//
// 为什么单独一组：这类故障里 HTTP 是 200、请求也成功了，坏的是「这次回答不完整」。
// 它们与 5xx/429 那种「请求没成」必须分开处理——
//
//   - 对调用方：绝不能把已经收到的半截正文当结果返回。分类跳拿半截 JSON 去
//     json.Unmarshal，报出来的是「模型输出不是合法json」，一句把用户和运维
//     都带向错误方向的诊断（线上 raw_out="{\"" 就是这么来的）。
//   - 对重试策略：请求成了但回答没成，当场重试一次最省事；5xx 那种仍交给上层。
var (
	ErrStreamStalled   = errors.New("上游流式响应卡住：连接没关但一直不给数据")
	ErrStreamTruncated = errors.New("上游流式响应不完整：没有结束标记")
)

// IsStreamBroken 判断错误是不是「流断在半路」这一类。
func IsStreamBroken(err error) bool {
	return err != nil && (errors.Is(err, ErrStreamStalled) || errors.Is(err, ErrStreamTruncated))
}

// streamIdleLimit 是「读流期间连续多久没有任何字节」就判上游卡死。
// 为什么必须有这把尺子：streamHTTPClient 的整体超时是 10 分钟（流式问答要长连接，
// 不能像普通请求那样 60s）。但上游有一种真实故障形态——把答案流完之后**既不关
// 连接、也不再给字节**。此时 Read 会一直阻塞，客户端只能干等到 10 分钟：线上实测
// 一整轮 616.5s（600s 超时 + 16.5s 重试），用户看到的就是「一直卡着计时」。
//
// 20s 的由来：线上正常流的最长帧间隔实测 1.2s（思考链连续片），关思考链的分类跳
// 首片 0.6s；20s 是十几倍余量，正常流不可能触发。0 表示关掉看门狗。
func streamIdleLimit() time.Duration {
	v := strings.TrimSpace(os.Getenv("SKILLFORGE_STREAM_IDLE_SEC"))
	if v == "" {
		return 20 * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 20 * time.Second
	}
	return time.Duration(n) * time.Second
}

const (
	// disabledThinkingBudget：要求关思考链时顺手带的思考预算上限（第三道闸，
	// 见 streamOnce 的 knobBoth 分支）。关思考链本来应该是 0 片，512 只是给
	// 「开关失灵」兜个底，对正常路径没有任何影响。
	disabledThinkingBudget = 512
)

// writingThinkBudget 是起草/审稿这类**要**思考链的调用可以配的上限。
//
// 默认 1024，**不是**不限。这条默认值是线上实测拍出来的，不是拍的脑袋：
// 线上生效配置 provider=siliconflow model=Qwen/Qwen3.6-27B，同一段「1 万字素材 +
// 写 1500 字新闻稿」提示词，只改 thinking_budget：
//
//	不限    → 首片正文 226.7s｜正文 1216 字
//	1024    → 首片正文  22.76s｜正文  855 字   （探针档）
//	1024    → 首片正文  26.1s ｜正文 1974 字   （复测档，比「不限」还长）
//	2048    → 总 1909.8s｜正文 256583 字       （退化：模型刹不住车，会撑爆客户端超时）
//
// 所以「掐预算 = 掉质量」这个假设在线上**不成立**：1024 又快又长，瓶颈是首片
// 正文前那 200 秒空转；而 2048 反而是灾难档。用户原话「现在速度过于慢了…一直卡着
// 计时，用户体验不佳」——默认不设上限就是让每一位新部署的人踩同一个 226 秒。
//
// 0（或负数）= **显式**要「不限」，给「这一轮我就要它使劲想」的场合留出口；
// 非法值一律回落到默认，不静默变成不限。
func writingThinkBudget() int {
	const def = 1024
	v := strings.TrimSpace(os.Getenv("SKILLFORGE_THINK_BUDGET"))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	if n <= 0 {
		return 0 // 显式不限
	}
	return n
}

// idleReader 记住「最后一次读到字节」的时刻，供看门狗判定上游是不是挂了。
type idleReader struct {
	rc   io.ReadCloser
	last atomic.Int64
}

func newIdleReader(rc io.ReadCloser) *idleReader {
	r := &idleReader{rc: rc}
	r.last.Store(time.Now().UnixNano())
	return r
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if n > 0 {
		r.last.Store(time.Now().UnixNano())
	}
	return n, err
}

func (r *idleReader) Close() error { return r.rc.Close() }

func (r *idleReader) idleFor() time.Duration {
	return time.Since(time.Unix(0, r.last.Load()))
}

// watchIdle 起一个看门狗：连续 idle 没有新字节就**关掉连接**，让阻塞住的 Read 当场
// 返回。返回的 stop 必须在读完流之后调用一次，并且它会给出「是不是看门狗动的手」
// ——事后判断故障性质要靠它，而不是靠 sc.Err()（被我们主动关掉时，那个错误是
// 「read on closed response body」，会掩盖真正的原因）。
func watchIdle(r *idleReader, idle time.Duration) func() bool {
	var aborted atomic.Bool
	if idle <= 0 {
		return func() bool { return false }
	}
	done := make(chan struct{})
	iv := idle / 4
	if iv < 100*time.Millisecond {
		iv = 100 * time.Millisecond
	}
	go func() {
		t := time.NewTicker(iv)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if r.idleFor() >= idle {
					aborted.Store(true)
					_ = r.Close() // 关连接 => 阻塞的 Read 立即返回
					return
				}
			}
		}
	}()
	stopped := false
	return func() bool {
		if !stopped {
			stopped = true
			close(done)
		}
		return aborted.Load()
	}
}

// knobNone 表示不带任何关思考链的开关（正文执笔走这条）。
const knobNone thinkKnob = -1

// streamOnce 发一次流式请求并解析到最后一片。
func (c *Client) streamOnce(ctx context.Context, system, user string, o StreamOpts, knob thinkKnob) (string, int, error) {
	messages := make([]map[string]string, 0, 2)
	if strings.TrimSpace(system) != "" {
		messages = append(messages, map[string]string{"role": "system", "content": system})
	}
	messages = append(messages, map[string]string{"role": "user", "content": user})
	body := map[string]any{
		"model":    c.cfg.Model,
		"messages": messages,
		"stream":   true,
	}
	if o.JSONMode {
		body["response_format"] = map[string]string{"type": "json_object"}
	}
	if o.MaxTokens > 0 {
		body["max_tokens"] = o.MaxTokens
	}
	switch knob {
	case knobBoth:
		// 两族开关互不通用，谁也不指望对方管用：enable_thinking 给 Qwen 系，
		// reasoning_effort 给 astron 系（详见 fastjson.go 文件头那张实测表）。
		body["enable_thinking"] = false
		body["reasoning_effort"] = "none"
		// 第三道闸：有的 provider/网关对上面两个开关都免疫（关了也照想），此时
		// 思考链把预算和时间全吃掉——实测同一段提示词带思考 63.0s、关思考 4.8s。
		// thinking_budget 实测被认账（思考链片数 1050 → 512），所以留作兜底：
		// 开关管用就无所谓（0 片最好），开关失灵时给浪费封个顶。
		body["thinking_budget"] = disabledThinkingBudget
	case knobEffortOnly:
		// 网关不认 enable_thinking（严格校验未知字段）时走这里：只留最保守的一条，
		// 不再带 thinking_budget——免得又踩一个「未知字段 400」。
		body["reasoning_effort"] = "none"
	case knobNone:
		// 正文执笔这条：思考链**要留着**（长文质量靠它），只是可以配一个上限。
		if b := writingThinkBudget(); b > 0 {
			body["thinking_budget"] = b
		}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return "", 0, err
	}

	endpoint := normalizeBaseURL(c.cfg.BaseURL) + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := streamHTTPClient().Do(req)
	if err != nil {
		return "", 0, c.wrapErr(err)
	}
	// 读流全程经过 idleReader：它记住「最后一次读到字节」的时刻，看门狗据此判断
	// 上游是不是把连接挂住了（详见 streamIdleLimit / watchIdle 的注释）。
	rc := newIdleReader(resp.Body)
	defer rc.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 只读 8KB：错误体是给人看的，不必把整段网关 HTML 吞进内存。
		tail, _ := io.ReadAll(io.LimitReader(rc, 8<<10))
		e := fmt.Errorf("LLM HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(tail)))
		if isTransientStatus(resp.StatusCode) {
			return "", resp.StatusCode, &TransientError{Err: e}
		}
		return "", resp.StatusCode, e
	}

	// 看门狗：连续 streamIdleLimit() 没有任何字节就关掉连接，让阻塞住的 Read 当场
	// 返回。没有它的话，「上游流完却不关连接」会把客户端钉到 10 分钟整体超时
	// ——线上实测一整轮 616.5s，用户看到的就是「一直卡着计时」。
	stopWatch := watchIdle(rc, streamIdleLimit())
	defer func() { stopWatch() }()

	var sb strings.Builder
	// 空正文诊断用：思考链片数与 provider 报的 token 明细。片数 > 0 而正文 0 字，
	// 就是「关了思考链的开关被无视、预算全花在想」的指纹。
	reasonChunks, completionTokens, reasoningTokens := 0, 0, 0
	// 终止信号：正常收尾**必给其中一个**（线上 provider 实测 3/3 都给
	// finish_reason=stop + [DONE]）。两者都缺 ⇒ 这次回答是被截断的。
	sawDone, sawStop := false, false
	sc := bufio.NewScanner(rc)
	// SSE 的 data 行里带整段 delta JSON；默认 64KB 上限对长思考片段偏紧，放宽到 1MB。
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			sawDone = true
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				CompletionTokens        int `json:"completion_tokens"`
				CompletionTokensDetails *struct {
					ReasoningTokens int `json:"reasoning_tokens"`
				} `json:"completion_tokens_details"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// 单帧坏掉不该让整轮作废：provider 的 usage 尾帧/空心跳帧都不是标准 delta。
			continue
		}
		if u := chunk.Usage; u != nil {
			completionTokens = u.CompletionTokens
			if u.CompletionTokensDetails != nil {
				reasoningTokens = u.CompletionTokensDetails.ReasoningTokens
			}
		}
		for _, ch := range chunk.Choices {
			if fr := ch.FinishReason; fr != nil && *fr != "" {
				// provider 说自己讲完了（stop/length/tool_calls…）。这是除了
				// [DONE] 之外唯一可信的「这次回答是完整的」凭据。
				sawStop = true
			}
			if r := ch.Delta.ReasoningContent; r != "" {
				reasonChunks++
				if o.OnReasoning != nil {
					o.OnReasoning(r)
				}
			}
			if d := ch.Delta.Content; d != "" {
				sb.WriteString(d)
				if o.OnContent != nil {
					o.OnContent(d)
				}
			}
		}
	}
	stalled := stopWatch()
	if stalled {
		// 上游把连接挂住：没关、也不再给字节。两种情形分开判，判据是**有没有终止信号**：
		//  ① 已经收到终止信号（provider 说了 stop / 给了 [DONE]）⇒ 回答本身是完整的，
		//     只是它没关连接。这是线上 616.5s 那一轮的形态：按成功返回最贴合事实，
		//     也省掉一次「重跑一遍、再挂一遍」的无谓重试。
		//  ② 没有终止信号 ⇒ 半截回答，按不完整丢弃，绝不交给调用方
		//     （半截 JSON 会让分类跳报「模型输出不是合法json」，把人带向错的病）。
		if (sawDone || sawStop) && sb.Len() > 0 {
			return sb.String(), resp.StatusCode, nil
		}
		return "", resp.StatusCode, &TransientError{Err: fmt.Errorf(
			"%w（等了 %s 没有新字节）：已收到 %d 字节，按不完整丢弃",
			ErrStreamStalled, streamIdleLimit(), sb.Len())}
	}
	if err := sc.Err(); err != nil {
		// 读流中途出错（不是看门狗动的手，是网络自己断了）。判据同样是终止信号：
		// 已经说过 stop/[DONE] ⇒ 回答完整，网络只是收尾时抖了一下，正文照收；
		// 没说过 ⇒ 半截，按不完整丢弃。以前这里只按「有没有内容」判，等于把
		// 半截 JSON 放进了调用方的 json.Unmarshal。
		if (sawDone || sawStop) && sb.Len() > 0 {
			return sb.String(), resp.StatusCode, nil
		}
		if sb.Len() > 0 {
			return "", resp.StatusCode, &TransientError{Err: fmt.Errorf(
				"%w（读流中断：%v）：已收到 %d 字节，按不完整丢弃",
				ErrStreamTruncated, err, sb.Len())}
		}
		return "", resp.StatusCode, c.wrapErr(err)
	}
	if !sawDone && !sawStop {
		// EOF 到了却既没 [DONE] 也没 finish_reason ⇒ 这是一条被截断的流。
		// 线上 provider 正常收尾实测 3/3 都给（finish_reason=stop + [DONE]），
		// 所以「两者都缺」可以放心判不完整；半截结果一律不当正文返回。
		return "", resp.StatusCode, &TransientError{Err: fmt.Errorf(
			"%w（既无 [DONE] 也无 finish_reason）：已收到 %d 字节，按不完整丢弃",
			ErrStreamTruncated, sb.Len())}
	}
	content := sb.String()
	if strings.TrimSpace(content) == "" {
		// 流跑完、HTTP 200，但一个字的正文都没有 —— 这不是「格式坏」，是「模型没干活」。
		// 与 fastjson.fastOnce 同一判据、同一个哨兵，上层才有一致的修法（放大预算重试）；
		// 空串直通调用方的话，json.Unmarshal 会报出那句正确的废话
		// 「unexpected end of json input 原文=<<>>」。
		return "", resp.StatusCode, fmt.Errorf("%w（流式：%s）", ErrEmptyContent, emptyStreamDetail(reasonChunks, completionTokens, reasoningTokens, o))
	}
	return content, resp.StatusCode, nil
}

// emptyStreamDetail 把人话拼给用户/运维：片数与 token 明细决定了修法完全不同。
func emptyStreamDetail(reasonChunks, completionTokens, reasoningTokens int, o StreamOpts) string {
	if reasonChunks > 0 {
		s := fmt.Sprintf("流完整跑完但正文 0 字，只收到 %d 片思考链；completion_tokens=%d、reasoning_tokens=%d",
			reasonChunks, completionTokens, reasoningTokens)
		if o.MaxTokens > 0 {
			s += fmt.Sprintf("（本次 max_tokens=%d，思考链多半把它吃光了）", o.MaxTokens)
		} else {
			s += "（本次没指定 max_tokens，用的是 provider 默认上限，思考链多半把它吃光了）"
		}
		return s
	}
	return "流完整跑完但正文 0 字，连思考链都没有（provider 返回了空响应）"
}

// streamHTTPClient：整体超时远大于普通请求。关思考链之后这些调用都在十几秒内，
// 但 provider 偶发抖动时不该被 60s 卡死；真正生效的截止时间是 ctx。
//
// 写成函数而不是包级变量：包级变量在 main() 之前求值，那时实例 env
// （SKILLFORGE_CA_BUNDLE）还没读进来，会造出一个「没有 CA」的 transport 并被缓存
// ——症状是「证书放好了也不生效」，客户会转头怀疑证书本身。
// 放在请求路径上求值才安全；transport 在 tlsconf 内部已缓存，这里只包一层结构体。
func streamHTTPClient() *http.Client { return tlsconf.NewClient(10 * time.Minute) }
