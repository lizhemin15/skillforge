package agent

import (
	"strings"
	"unicode/utf8"
)

// docTextKeys 是流式 JSON 里**值得当中间材料给用户看**的字段名。
//
// 这份白名单是踩过「看着像能流、其实一片都没有」之后才落下的。背景（2026-09-15
// 线上实测）：docgen 那一跳关掉思考链后要裸跑 16.8 秒（同一提示词整轮 23.8s），
// 而这 16.8 秒里屏幕上只有「正在生成…（已用 6s/9s/12s…）」在跳。三条 JSON 调用
// （意图识别 / docgen 规格 / 字段映射）都挂了 OnReasoning，但 astron 上
// reasoning_effort=none 是**真管用**的（实测 reasoning 片数 = 0），所以思考链一片
// 都没有 —— 材料挂接没错，是根本没料可挂。
//
// 而同一跳的 content 是**按片段流式**的（实测 440 片 / 2350 字节、首片 382ms）。
// 这些字段正是用户最终要的那份文档的文字，边生成边露出来就是「内容在动」。
//
// 分三类：
//   - 文档正文：parags/title/rows/content/… —— docgen 与字段映射两条路要的字；
//   - 填值内容：fields/values —— 字段映射把值填进占位符，值本身是文档内容；
//   - 判定理由：reason/note —— 意图识别那一跳同样是 6~7 秒静默，reason 是模型
//     给出的中文判定依据，露出来比一个空跳的计时有用。
//
// 刻意**不**收 format/filename/cols/action 这类结构性字段：它们是机器消费的
// 枚举值与文件名，出现在「正在写」区域只会让人以为出错了。
var docTextKeys = map[string]bool{
	"title": true, "parags": true, "paragraphs": true, "rows": true,
	"content": true, "text": true, "body": true, "lines": true,
	"items": true, "summary": true,
	"fields": true, "values": true,
	"reason": true, "note": true,
}

// jsonPreview 是**增量式**的「流式 JSON 片段 → 人话正文」抽取器。
//
// 为什么不能等 JSON 收完再解析：这一跳的意义就在于「生成期间让用户看见内容」，
// 攒到最后一次性解析等于什么都没解决。所以它按字节进、按字节出，状态跨调用保留。
//
// 为什么不用 encoding/json：输入是**残缺**的 JSON（每片可能只有几个字节、字符串
// 随时断在半路），标准库只会报错。这里是一台只认「结构字符 + 字符串字面量」的
// 极简状态机，畸形输入一律丢弃而不是崩。
//
// 抽取规则：
//   - 字符串字面量区分 key / value：在对象里、且上一个有效字节是 '{' 或 ',' 的
//     字符串即 key；
//   - value 的归属字段 = 在对象里取最近完成的 key，在数组里取该数组的 key
//     （parags/rows/values 都是数组）；只要**自己或任一外层容器**的归属字段在白
//     名单里就放行（外层放行是为了覆盖 `"fields":{"item1":"…"}` 这种值带自己
//     key 的映射）；
//   - key 与结构性字符一律不进预览。
type jsonPreview struct {
	inStr bool // 正在字符串内部
	esc   bool // 上一字节是反斜杠
	isKey bool // 当前这个字符串是 key

	cap  bool            // 当前字符串是否放行
	buf  strings.Builder // 当前 key 的原文（value 不缓冲，省内存）
	cur  string          // 当前对象里最近完成的 key
	kind []byte          // 容器栈：'{' / '['
	own  []string        // 每个容器「由哪个字段引入」，与 kind 同长
	prev byte            // 最近一个非空白、且不在字符串内的字节

	uCnt int  // \u 转义还剩几个十六进制位
	uVal rune // \u 已累积的值
	hi   rune // 待配对的高位代理（emoji 这类 4 字节字符）

	out []byte // 本次 Feed 产出的文字（按字节，返回时拷贝成 string）
}

// Feed 吃一片 JSON 原文，返回这片里新抽出来的**人话正文**（可能为空）。
func (p *jsonPreview) Feed(s string) string {
	p.out = p.out[:0]
	for i := 0; i < len(s); i++ {
		p.step(s[i])
	}
	// string(...) 会拷贝：Builder/切片直接暴露底层数组的话，下一片 Feed 会
	// 就地改写上一片已经交出去的字符串（调用方拿到的内容会被悄悄改掉）。
	return string(p.out)
}

func (p *jsonPreview) step(b byte) {
	if p.inStr {
		p.stepInString(b)
		return
	}
	switch b {
	case '"':
		p.openString()
	case '{', '[':
		// 归属必须在**压栈之前**算：attr() 看的是栈顶容器，压完再算就是在问
		// 「新容器归谁」，答案永远是它自己。这个顺序写反过一次，症状是
		// `"rows":[["a","b"]]` 一个值都露不出来，以及根级 `[` 直接越界 panic。
		owner := p.attr()
		p.kind = append(p.kind, b)
		p.own = append(p.own, owner)
		p.cur = ""
		p.prev = b
	case '}', ']':
		if n := len(p.kind); n > 0 {
			p.kind = p.kind[:n-1]
			p.own = p.own[:n-1]
		}
		p.cur = ""
		p.prev = b
	case ',', ':':
		p.prev = b
	case ' ', '\t', '\n', '\r':
		// 空白不进 prev：格式化过的 JSON 里 '{' 与 key 之间隔着换行缩进。
	default:
		p.prev = b
	}
}

// openString 判断这个字符串是 key 还是 value，并算好是否放行。
func (p *jsonPreview) openString() {
	p.inStr = true
	// 只有「在对象里」且「紧跟 '{' 或 ','」的字符串才是 key。
	p.isKey = len(p.kind) > 0 && p.kind[len(p.kind)-1] == '{' && (p.prev == '{' || p.prev == ',')
	p.buf.Reset()
	p.cap = false
	if !p.isKey {
		p.cap = p.allowed()
	}
}

// allowed 判断当前字符串该不该露给用户：自己或任一外层容器的归属字段在白名单里。
func (p *jsonPreview) allowed() bool {
	if docTextKeys[p.cur] {
		return true
	}
	for i := len(p.own) - 1; i >= 0; i-- {
		if docTextKeys[p.own[i]] {
			return true
		}
	}
	return false
}

// attr 返回「即将进入的容器由哪个字段引入」。
//
// 数组套数组（`"rows":[["a","b"]]`）必须**继承**外层数组的字段名，否则内层数组
// 的归属会变成空串，整张表的值一个都露不出来。
func (p *jsonPreview) attr() string {
	if len(p.kind) == 0 {
		return p.cur
	}
	if p.kind[len(p.kind)-1] == '{' {
		return p.cur
	}
	return p.own[len(p.own)-1]
}

func (p *jsonPreview) stepInString(b byte) {
	if p.uCnt > 0 {
		d := hexVal(b)
		if d < 0 {
			// 畸形 \u 转义：丢掉这一个转义，别把后面的正文一起吞了。
			p.uCnt, p.uVal, p.hi = 0, 0, 0
			return
		}
		p.uVal = p.uVal<<4 | rune(d)
		if p.uCnt--; p.uCnt == 0 {
			p.flushUnicode()
		}
		return
	}
	if p.esc {
		p.esc = false
		switch b {
		case 'n':
			p.putRune('\n')
		case 't':
			p.putRune('\t')
		case 'r':
			p.putRune('\r')
		case 'b', 'f':
			// 退格/换页在正文里当空格处理：原样传出去是控制字符，前端会显示成方块。
			p.putRune(' ')
		case 'u':
			p.uCnt = 4
			p.uVal = 0
		case '"':
			p.putRune('"')
		case '\\':
			p.putRune('\\')
		case '/':
			p.putRune('/')
		default:
			p.putRune(rune(b))
		}
		return
	}
	switch b {
	case '\\':
		p.esc = true
	case '"':
		// 字符串收口：key 记下来给后面的 value 认领，value 用完即弃。
		if p.isKey {
			p.cur = p.buf.String()
		} else {
			p.cur = ""
		}
		p.inStr, p.cap = false, false
	default:
		p.putByte(b)
	}
}

// flushUnicode 处理 \uXXXX 收满四位后的收尾，含 emoji 这类代理对。
func (p *jsonPreview) flushUnicode() {
	r := p.uVal
	p.uVal = 0
	switch {
	case r >= 0xD800 && r <= 0xDBFF:
		// 高位代理：先存着，等下一段 \uDC00-\uDFFF 配对。
		p.hi = r
	case r >= 0xDC00 && r <= 0xDFFF && p.hi != 0:
		p.putRune(0x10000 + (p.hi-0xD800)<<10 + (r - 0xDC00))
		p.hi = 0
	default:
		// 落单的高位代理（后面跟的不是低位代理）转成 U+FFFD，别丢字。
		if p.hi != 0 {
			p.hi = 0
			p.putRune(utf8.RuneError)
		}
		p.putRune(r)
	}
}

func (p *jsonPreview) putByte(b byte) {
	if p.isKey {
		p.buf.WriteByte(b)
	}
	if p.cap {
		p.out = append(p.out, b)
	}
}

func (p *jsonPreview) putRune(r rune) {
	var tmp [utf8.UTFMax]byte
	n := utf8.EncodeRune(tmp[:], r)
	if p.isKey {
		p.buf.Write(tmp[:n])
	}
	if p.cap {
		p.out = append(p.out, tmp[:n]...)
	}
}

func hexVal(b byte) int {
	switch {
	case b >= '0' && b <= '9':
		return int(b - '0')
	case b >= 'a' && b <= 'f':
		return int(b-'a') + 10
	case b >= 'A' && b <= 'F':
		return int(b-'A') + 10
	}
	return -1
}
