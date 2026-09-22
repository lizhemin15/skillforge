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
	// OnReset：本轮作废、即将重试时回调（调用方据此清掉已经流出去的
	// 半截/复读正文——不清的话，重试成功后新正文会追加在一屏垃圾后面）。
	// 可为 nil。
	OnReset func()
	// FrequencyPenalty：>0 时随请求发送。复读重试时由 StreamChat 自动带上
	// （frequency_penalty 是破 repetition loop 的标准旋钮）。
	FrequencyPenalty float64
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
	triedBigger, droppedJSON, triedPartial, triedLoopFix := false, false, false, false
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
		case IsLoopDetected(err) && !triedLoopFix && ctx.Err() == nil:
			// 模型复读（线上实锤：5 字问题流了 47KB 同一句话）。收手后不是干重试：
			// frequency_penalty 是破 repetition loop 的标准旋钮，带上再问一次。
			// 已经流到前端的复读正文由 OnReset 通知清掉，别让新答案追加在一屏垃圾后面。
			triedLoopFix = true
			opt.FrequencyPenalty = 0.4
			if opt.OnReset != nil {
				opt.OnReset()
			}
			if opt.OnNote != nil {
				opt.OnNote("检测到模型在复读同一内容，已提前收手；正在调整采样参数重试…")
			}
		case IsStreamBroken(err) && !triedPartial && ctx.Err() == nil:
			// 流断在半路（上游把连接挂住 / 没给结束标记）。与 5xx 的区别很关键：
			// 5xx 是「请求没成」，交给上层决定重试；这里是「请求成了但回答不完整」，
			// 当场重试一次最省事——而且绝不能把半截正文漏给调用方（那会变成
			// 「模型输出不是合法json」这种指向错方向的诊断）。
			triedPartial = true
			if opt.OnReset != nil {
				opt.OnReset() // 半截正文同样作废：清掉，重试后流干净的新正文
			}
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
			// 终态前若这次已经流过正文又作废（复读二次/断流二次），同样先清屏再报错——
			// 否则半屏复读正文和 ⚠ 错误提示缝在同一屏，用户还得手动滚过那屏垃圾。
			if opt.OnReset != nil && (IsLoopDetected(err) || IsStreamBroken(err)) {
				opt.OnReset()
			}
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
	// ErrNoProgress：字节一直在来（保活帧），但连续没有一片正文/思考。
	// 与 ErrStreamStalled 并列而不是合并：两者的现场完全不同（一个 Read 阻塞、
	// 一个 Read 很活跃），排障时要能一眼分开；对上层处理方式则一致——都算
	// 「请求成了但回答不完整」，当场重试一次。
	ErrNoProgress = errors.New("上游流式响应卡住：连接活着、保活帧在滴，但一直不出正文/思考片段")
	// ErrLoopDetected：片很活跃、看门狗全绿，但吐出来的是同一段话的第 N 遍。
	// 2026-09-22 线上实锤（10:53 那轮）：5 字问题（「现在在调用的是什么模型」）
	// 流了 10748 片 / 47KB 正文、5 分半不给结束标记，idle/progress/片级三个
	// 看门狗全绿（每片都 mark），最后靠总预算 327.5s 硬 cancel、整段丢弃——
	// 用户盯着一屏复读刷屏白等 5 分钟。这是模型层 repetition loop，应用层
	// 必须自己收手。
	ErrLoopDetected = errors.New("上游模型陷入复读循环")
)

// IsLoopDetected 判断错误是不是「复读循环」这一类。与 IsStreamBroken 分开：
// 复读重试要带 frequency_penalty（破复读的标准旋钮），不是干重试。
func IsLoopDetected(err error) bool { return errors.Is(err, ErrLoopDetected) }

// loopState：在线复读检测器。判据——尾部 loopTailBytes 字节在前文出现 ≥2 次
// （含尾部自身共 3 遍）即判复读。
//
// 误报分析：192 字节的连续精确重复，在自然语言/表格/JSON 正文里几乎不可能合法
// 出现 3 遍（表格行内容各不相同；段落级重复的正常文档会在写完后正常给 stop，
// 但检测是流式进行的，不能依赖它——所以阈值取 3 遍而不是 2 遍，给合法重复留了
// 一遍余量）。抓不住的：周期 > 全文 1/3 的大循环，那种由总预算兜底。
//
// 成本：每 loopCheckEvery 字节检测一次。检测要扫完尾巴在前文里的**全部**出现
// （取最小间距，见 hit 内注释），所以单次是 O(前文长度)；47KB 正文最坏也不过
// 十几次 × 几十 KB 的原生 strings.Index 扫描，毫秒级。这个代价换来的是
// 错误信息里那个「循环体约 N 字节」在任何形态下都不说谎。
type loopState struct {
	checked int // 上次检测时的累计字节数
}

const (
	loopTailBytes  = 192 // 复读判据用的尾巴长度
	loopCheckEvery = 2048
	loopMinContent = 3 * loopTailBytes // 尾巴×3 遍是判复读的最小正文量
)

// hit 检测一次。返回（循环体长度, 是否复读）。
func (ls *loopState) hit(sb *strings.Builder) (period int, ok bool) {
	n := sb.Len()
	if n < ls.checked+loopCheckEvery || n < loopMinContent {
		return 0, false
	}
	ls.checked = n
	full := sb.String()
	tail := full[n-loopTailBytes:]
	prev := full[:n-loopTailBytes]
	// 从前到后把尾巴在前文里的每次出现都找出来，取**最小相邻间距**当循环体长度。
	// 不取「头两次出现的间距」：尾巴若在正文里零散出现过（合法文档引用同一长句、
	// 模板头尾重复），头两次的间距是个假数（实测把 456 的循环体报成 798），
	// 而最小间距在真循环里恰好就是循环体本身。
	prevAt, cnt, period := -1, 0, 0
	for i := 0; ; {
		j := strings.Index(prev[i:], tail)
		if j < 0 {
			break
		}
		i += j
		if prevAt >= 0 {
			if gap := i - prevAt; period == 0 || gap < period {
				period = gap
			}
		}
		prevAt, cnt = i, cnt+1
		// 重叠搜索（i++ 而非跳 192 字节）：循环体 < 尾巴长时（线上实锤 45 字节/帧），
		// 下一处出现就在 i+unit——跳过尾巴会量出一个 192+ε 的假周期，错误信息里
		// 「循环体约 N 字节」就失真了。代价是落空时多扫一遍 prev，MB 级微不足道。
		i++
	}
	if cnt >= 2 {
		// 尾巴自身也算一次出现：它与前文最后一处的间距，就是「手上正在吐的这一遍」
		// 的长度。少了这一项，循环体报的是前两处之间的老间距（实测 798），
		// 而手上真实的那一遍是 456。
		if gap := (n - loopTailBytes) - prevAt; gap < period {
			period = gap
		}
		return period, true
	}
	return 0, false
}

// IsStreamBroken 判断错误是不是「流断在半路」这一类。
func IsStreamBroken(err error) bool {
	return err != nil && (errors.Is(err, ErrStreamStalled) || errors.Is(err, ErrStreamTruncated) ||
		errors.Is(err, ErrNoProgress))
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

// streamPieceGapLimit / streamFirstPieceLimit 是**片级**看门狗的两段阈值。
//
// 为什么字节看门狗不够：线上有一轮
//
//	[write-plain] hop=346.1s ttft=-1.00s reason=2 out_pieces=0 out=0
//	[桩] …上游流式响应卡住（20s 没有新字节）—— 那一轮压根没触发，因为字节一直在来
//
// 上游一直在滴 SSE 保活帧：字节没断（20s 的字节看门狗看不见），却整整 346 秒没吐出
// 一片正文，用户那边就是「一直卡着计时」+ 最终 0 字。**字节活着 ≠ 它在干活。**
//
// 两段分开是因为「还没开始吐字」与「吐到一半不动了」是两种东西：
//   - 第一片之前：模型还在想、还在预填。健康样本首片最长 104.71s（hop=114.6s
//     那一轮，正文照常出来了），所以给 150s（1.4 倍余量）；
//   - 第一片之后：一旦开始吐字就该持续。健康样本里 5978 片跑了 163s（平均 27ms
//     一片），60s 没一片已是病态 —— 上面那 346 秒的轮子在这里被抓住。
//
// 触发后返回 ErrNoProgress（算「流断在半路」），由 StreamChat 既有的
// triedPartial 分支当场重试一次；0 表示关掉那一段。
func streamFirstPieceLimit() time.Duration {
	return streamSecKnob("SKILLFORGE_STREAM_FIRST_PIECE_SEC", 150)
}

func streamPieceGapLimit() time.Duration {
	return streamSecKnob("SKILLFORGE_STREAM_PIECE_GAP_SEC", 60)
}

// streamSecKnob 是这几个「秒数旋钮」的统一解析：空/非法 → 默认值，0 → 关掉。
func streamSecKnob(name string, def int) time.Duration {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return time.Duration(def) * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return time.Duration(def) * time.Second
	}
	return time.Duration(n) * time.Second
}

// progressClock 记住「最近一片正文/思考」的时刻（0 = 一片都还没有）。
// 与 idleReader 的区别：它只认**内容片**，不认保活字节。
type progressClock struct {
	last  atomic.Int64 // 最近一片的 UnixNano
	count atomic.Int64
}

func (p *progressClock) mark() {
	p.last.Store(time.Now().UnixNano())
	p.count.Add(1)
}

// sinceLast 返回「距上一片多久」；第二值为 false 表示还没出过任何片。
func (p *progressClock) sinceLast() (time.Duration, bool) {
	n := p.last.Load()
	if n == 0 {
		return 0, false
	}
	return time.Since(time.Unix(0, n)), true
}

// watchProgress 起一个片级看门狗：连续没有新的正文/思考片段就**关掉连接**，让阻塞住
// 的 Read 当场返回。返回的 stop 必须在读完流之后调用（可重复调用），它会给出
// 「是不是看门狗动的手」以及原因——事后给用户/日志一个准确的说法，而不是把
// 「我们自己关的连接」说成网络故障。
func watchProgress(p *progressClock, rc io.Closer, first, gap time.Duration) func() (bool, string) {
	if first <= 0 && gap <= 0 {
		return func() (bool, string) { return false, "" }
	}
	var aborted atomic.Bool
	var why atomic.Value
	start := time.Now()
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-t.C:
				if d, ok := p.sinceLast(); ok {
					if gap > 0 && d >= gap {
						why.Store(fmt.Sprintf("连续 %s 没有新的正文/思考片段（上限 %s；此前已收到 %d 片）",
							d.Round(time.Second), gap, p.count.Load()))
						aborted.Store(true)
						_ = rc.Close() // 关连接 => 阻塞的 Read 立即返回
						return
					}
					continue
				}
				if first > 0 && now.Sub(start) >= first {
					why.Store(fmt.Sprintf("连第一片都没等到（等了 %s，上限 %s）",
						now.Sub(start).Round(time.Second), first))
					aborted.Store(true)
					_ = rc.Close()
					return
				}
			}
		}
	}()
	stopped := false
	return func() (bool, string) {
		if !stopped {
			stopped = true
			close(done)
		}
		if !aborted.Load() {
			return false, ""
		}
		if s, ok := why.Load().(string); ok {
			return true, s
		}
		return true, "上游无进展"
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
	if o.FrequencyPenalty > 0 {
		body["frequency_penalty"] = o.FrequencyPenalty
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

	// 片级看门狗：字节活着但不出片，同样要收手（见 streamFirstPieceLimit 的注释：
	// 线上那一轮字节没断、346 秒没吐一片正文，字节看门狗根本看不见）。
	pc := &progressClock{}
	stopProgress := watchProgress(pc, rc, streamFirstPieceLimit(), streamPieceGapLimit())
	defer func() { stopProgress() }()

	var sb strings.Builder
	// 空正文诊断用：思考链片数与 provider 报的 token 明细。片数 > 0 而正文 0 字，
	// 就是「关了思考链的开关被无视、预算全花在想」的指纹。
	reasonChunks, completionTokens, reasoningTokens := 0, 0, 0
	// 终止信号：正常收尾**必给其中一个**（线上 provider 实测 3/3 都给
	// finish_reason=stop + [DONE]）。两者都缺 ⇒ 这次回答是被截断的。
	sawDone, sawStop := false, false
	// 复读看门狗：片级看门狗只看「来不来」，这台看「来的是什么」。
	ls := &loopState{}
	looped, loopPeriod := false, 0
	sc := bufio.NewScanner(rc)
	// SSE 的 data 行里带整段 delta JSON；默认 64KB 上限对长思考片段偏紧，放宽到 1MB。
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	// scan 标签是必须的：收手要跳出**扫描循环**，不是只跳出内层 choices 循环。
	// 只 break 内层的话，scanner 已经缓冲在手的帧（实测 33KB ≈ 743 帧）会被逐帧
	// 继续写进 sb 并逐帧 OnContent 推给前端——用户气泡里照样刷满一屏复读，
	// 而且错误信息里「已收 N 字节」会被这个下溢过程污染成 33456（真值 2KB 级）。
scan:
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
				pc.mark() // 思考片也是「它在干活」的凭据
				if o.OnReasoning != nil {
					o.OnReasoning(r)
				}
			}
			if d := ch.Delta.Content; d != "" {
				sb.WriteString(d)
				pc.mark()
				if o.OnContent != nil {
					o.OnContent(d)
				}
				// 复读检测。触发就断流收手——继续等只会把同一句话
				// 刷到天荒地老（2026-09-22 线上那轮：47KB / 5 分半）。
				if period, ok := ls.hit(&sb); ok {
					looped, loopPeriod = true, period
					rc.Close() // 让阻塞中的 sc.Scan() 当场返回
					break scan // 必须跳出扫描循环：缓冲在手的帧也不能再吐给前端
				}
			}
		}
	}
	if looped {
		// 复读收手。终止信号没来，正文按不完整丢弃（半截复读更不能交付）；
		// 循环体长度与收到字节数都进错误信息，排障时一眼看出循环有多大。
		return "", resp.StatusCode, &TransientError{Err: fmt.Errorf(
			"%w（循环体约 %d 字节，已收 %d 字节时收手），按不完整丢弃",
			ErrLoopDetected, loopPeriod, sb.Len())}
	}
	stalled := stopWatch()
	noProgress, progWhy := stopProgress()
	if stalled || noProgress {
		// 收手了：两种情形分开判，判据是**有没有终止信号**：
		//  ① 已经收到终止信号（provider 说了 stop / 给了 [DONE]）⇒ 回答本身是完整的，
		//     只是它没关连接。这是线上 616.5s 那一轮的形态：按成功返回最贴合事实，
		//     也省掉一次「重跑一遍、再挂一遍」的无谓重试。
		//  ② 没有终止信号 ⇒ 半截回答，按不完整丢弃，绝不交给调用方
		//     （半截 JSON 会让分类跳报「模型输出不是合法json」，把人带向错的病）。
		if (sawDone || sawStop) && sb.Len() > 0 {
			return sb.String(), resp.StatusCode, nil
		}
		if noProgress {
			// 字节在来、片不来 —— 说清是这条，别让排障的人去查网络。
			return "", resp.StatusCode, &TransientError{Err: fmt.Errorf(
				"%w（%s）：已收到 %d 字节，按不完整丢弃", ErrNoProgress, progWhy, sb.Len())}
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
