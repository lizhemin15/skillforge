package skillgen

import (
	"context"
	"strings"
	"sync"
	"time"
)

// 中间材料的两种类别：思考链（模型自己推演的过程）与正文（正在写出来的成品文字）。
// 前端按类别分开显示——用户说的是「流式输出思考的一些中间材料」，思考和正文混成
// 一锅就分不出「它在想」和「它在写」。
const (
	MaterialThink = "think"
	MaterialText  = "text"
	// MaterialNote 是流水线自己补的旁白（如「流式中断，回退阻塞调用」）。
	MaterialNote = "note"
)

type deltaKey struct{}

type deltaFunc func(kind, text string)

// WithDelta 把中间材料接收器挂到 ctx 上。
//
// 为什么不给 Generator 加字段：Generator 是常驻单例，多个训练可能并发跑，
// 在结构体上挂「本次运行的接收器」就是数据竞争。也不改 Generate 的签名——
// 那会逼着所有调用点、fake 替身一起改，代价远大于收益。挂在 ctx 上，只有
// 显式注入的那条链路（训练页）能看到材料，其他调用方行为一字不变。
func WithDelta(ctx context.Context, fn deltaFunc) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, deltaKey{}, fn)
}

// deltaOf 取出接收器；没注入过就返回 nil（表示这条链路不要流式）。
func deltaOf(ctx context.Context) deltaFunc {
	if fn, ok := ctx.Value(deltaKey{}).(deltaFunc); ok {
		return fn
	}
	return nil
}

// MaterialRelay 把高频小片段攒成批再往外吐。
//
// 为什么必须攒批：流式回调一次往往只有几个字符，一片一帧就是每秒几百个 SSE 帧，
// 浏览器要重排几百次——「中间材料」这件事最常见的做坏方式不是没有流，是被自己刷死。
// 攒批规则：距上次吐出 ≥ min，或缓冲 ≥ max 字节，满足其一即吐。
type MaterialRelay struct {
	mu   sync.Mutex
	min  time.Duration
	max  int
	last map[string]time.Time
	buf  map[string]*strings.Builder
	emit func(kind, text string)
}

// NewMaterialRelay 造一个攒批器；min<=0 用 400ms，max<=0 用 240 字节。
func NewMaterialRelay(min time.Duration, max int, emit func(kind, text string)) *MaterialRelay {
	if min <= 0 {
		min = 400 * time.Millisecond
	}
	if max <= 0 {
		max = 240
	}
	return &MaterialRelay{
		min:  min,
		max:  max,
		last: map[string]time.Time{},
		buf:  map[string]*strings.Builder{},
		emit: emit,
	}
}

// Push 收下一个片段；按阈值决定是攒着还是立刻吐。
func (r *MaterialRelay) Push(kind, text string) {
	if r == nil || r.emit == nil || text == "" {
		return
	}
	r.mu.Lock()
	b := r.buf[kind]
	if b == nil {
		b = &strings.Builder{}
		r.buf[kind] = b
	}
	b.WriteString(text)
	// 首次一定立刻吐（last 是零值，Since 极大）——否则第一段材料要等 400ms 才出现，
	// 而「屏幕上多久出现第一个字」正是用户体感的全部。
	due := time.Since(r.last[kind]) >= r.min || b.Len() >= r.max
	out := ""
	if due {
		out = b.String()
		b.Reset()
		r.last[kind] = time.Now()
	}
	r.mu.Unlock()
	if due && out != "" {
		r.emit(kind, out)
	}
}

// Flush 立刻吐掉所有积压片段。
//
// 阶段边界必须调：否则一个阶段结束时剩下的最后几十个字符要一直压到下一个阶段的
// 第一次 Push 才出现，材料会「串台」到下一阶段。
func (r *MaterialRelay) Flush() {
	if r == nil {
		return
	}
	r.mu.Lock()
	pending := map[string]string{}
	for k, b := range r.buf {
		if b.Len() > 0 {
			pending[k] = b.String()
			b.Reset()
		}
	}
	r.mu.Unlock()
	for k, v := range pending {
		if r.emit != nil {
			r.emit(k, v)
		}
	}
}
