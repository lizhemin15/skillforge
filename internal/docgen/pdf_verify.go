package docgen

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// pdf_verify.go 实现「PDF 渲染 → 回读文本」的自证工具，不依赖任何第三方库
// （pymupdf / poppler 这些在离线客户机上不一定有）。
//
// 为什么必须回读：gopdf 找不到字形时的默认行为是**静默替换成空格**
// （TtfOption.OnGlyphNotFoundSubstitute='\u0020'）。于是字体不全时，PDF 照样能生成、
// 照样是合法 %PDF 文件、照样能打开——只是数字和拉丁字母全部消失（Bug G）。
// 所以「PDF 生成成功」不是证据，「能从生成的 PDF 字节里回读出数字和中文」才是证据。
//
// 实现原理（只针对 gopdf 的输出形态，够用且可解释）：
//  1. 从 PDF 里取出所有 stream（优先信 /Length，退化到找 endstream），zlib 解压；
//  2. 含 beginbfchar/beginbfrange 的流就是字体子集的 ToUnicode CMap，
//     解析成 glyphID(code) → Unicode 的映射；
//  3. 含 `[<hex>] TJ` / `<hex> Tj` 的流是内容流，按 codespacerange 的字节宽度
//     把 hex 切成一个个 code，过 CMap 得到可见文本。

// BuildSelfTestPDF 生成一份「数字 + 拉丁 + 中文」都齐的最小 PDF，供自检回读用。
func BuildSelfTestPDF() ([]byte, error) {
	return Generate(Doc{
		Format: "pdf",
		Title:  "自检样例",
		Cols:   []string{"产品", "数量", "单价"},
		Rows:   [][]string{{"云服务器", "5", "12000"}},
		Parags: []string{"0123456789 ABCdef 产品报价单"},
	})
}

// ExtractPDFText 从（gopdf 生成的）PDF 字节流里回读可见文本。
// 每个文本绘制片段之间用换行分隔；无法解析时返回错误而不是空串，
// 避免「空结果」被当成「没有缺字」而蒙混过关。
func ExtractPDFText(pdf []byte) (string, error) {
	streams := pdfStreams(pdf)
	if len(streams) == 0 {
		return "", fmt.Errorf("PDF 里没找到任何 stream（%d 字节）", len(pdf))
	}

	var cmap map[uint32]rune
	var codeWidth int
	for _, s := range streams {
		if bytes.Contains(s, []byte("beginbfchar")) || bytes.Contains(s, []byte("beginbfrange")) {
			cmap, codeWidth = parseToUnicodeCMap(s)
			break
		}
	}
	if len(cmap) == 0 {
		return "", fmt.Errorf("PDF 里找不到可解析的 ToUnicode CMap（%d 个 stream）", len(streams))
	}

	var out []string
	for _, s := range streams {
		if !bytes.Contains(s, []byte("TJ")) && !bytes.Contains(s, []byte("Tj")) {
			continue
		}
		out = append(out, decodeContentStream(s, cmap, codeWidth)...)
	}
	if len(out) == 0 {
		return "", fmt.Errorf("PDF 内容流里没找到任何文本绘制指令")
	}
	return strings.Join(out, "\n"), nil
}

// pdfStreams 取出并解压 PDF 里所有 stream 的内容。
func pdfStreams(pdf []byte) [][]byte {
	var out [][]byte
	rest := pdf
	for {
		i := bytes.Index(rest, []byte("stream"))
		if i < 0 {
			break
		}
		// "endstream" 里也含 "stream"，必须挡住；同时要求 stream 前后是分隔符。
		if i >= 3 && bytes.Equal(rest[i-3:i], []byte("end")) {
			rest = rest[i+6:]
			continue
		}
		start := i + len("stream")
		if start < len(rest) && rest[start] == '\r' {
			start++
		}
		if start < len(rest) && rest[start] == '\n' {
			start++
		}

		// 优先按字典里的 /Length 切；取不到就退回找 endstream。
		end := -1
		if n, ok := streamLength(rest[:i]); ok && start+n <= len(rest) {
			end = start + n
			// 校验一下，不信错 Length
			tail := rest[end:min(end+20, len(rest))]
			if !bytes.Contains(tail, []byte("endstream")) {
				end = -1
			}
		}
		if end < 0 {
			e := bytes.Index(rest[start:], []byte("endstream"))
			if e < 0 {
				break
			}
			end = start + e
		}

		raw := rest[start:end]
		if d, err := inflate(raw); err == nil {
			out = append(out, d)
		} else {
			out = append(out, raw)
		}

		j := bytes.Index(rest[end:], []byte("endstream"))
		if j < 0 {
			break
		}
		rest = rest[end+j+len("endstream"):]
	}
	return out
}

// streamLength 从 stream 关键字前面的字典里读出 /Length 数值。
func streamLength(dict []byte) (int, bool) {
	if len(dict) > 800 {
		dict = dict[len(dict)-800:]
	}
	// 可能写成 /Length 123 或 /Length 5 0 R，只认直接给数字的。
	re := regexp.MustCompile(`/Length\s+(\d+)`)
	ms := re.FindAllSubmatch(dict, -1)
	if len(ms) == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(string(ms[len(ms)-1][1]))
	if err != nil || n <= 0 || n > 64<<20 {
		return 0, false
	}
	return n, true
}

func inflate(b []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

var hexTokenRe = regexp.MustCompile(`<([0-9A-Fa-f\s]*)>`)

// parseToUnicodeCMap 解析 ToUnicode CMap，返回 code→rune 映射与 code 的字节宽度。
func parseToUnicodeCMap(cmapData []byte) (map[uint32]rune, int) {
	out := make(map[uint32]rune)
	width := 2 // gopdf 默认 Identity + 2 字节 code

	text := string(cmapData)
	if m := regexp.MustCompile(`begincodespacerange\s*<([0-9A-Fa-f]+)>`).FindStringSubmatch(text); m != nil {
		if w := len(m[1]) / 2; w > 0 {
			width = w
		}
	}

	parseHex := func(s string) (uint32, bool) {
		s = strings.TrimSpace(s)
		if s == "" {
			return 0, false
		}
		v, err := strconv.ParseUint(s, 16, 32)
		if err != nil {
			return 0, false
		}
		return uint32(v), true
	}

	for _, block := range splitBlocks(text, "beginbfchar", "endbfchar") {
		toks := hexTokenRe.FindAllStringSubmatch(block, -1)
		for i := 0; i+1 < len(toks); i += 2 {
			src, ok1 := parseHex(toks[i][1])
			dstRaw := strings.TrimSpace(toks[i+1][1])
			if !ok1 || dstRaw == "" {
				continue
			}
			if r := utf16BeToRunes(dstRaw); len(r) > 0 {
				out[src] = r[0]
			}
		}
	}

	for _, block := range splitBlocks(text, "beginbfrange", "endbfrange") {
		for _, line := range strings.Split(block, "\n") {
			toks := hexTokenRe.FindAllStringSubmatch(line, -1)
			if len(toks) < 2 {
				continue
			}
			lo, ok1 := parseHex(toks[0][1])
			hi, ok2 := parseHex(toks[1][1])
			if !ok1 || !ok2 || hi < lo || hi-lo > 65535 {
				continue
			}
			if len(toks) >= 3 { // <lo> <hi> <dst> 连续映射
				base, ok := parseHex(toks[2][1])
				if !ok {
					continue
				}
				for c := lo; c <= hi; c++ {
					out[c] = rune(base + (c - lo))
				}
				continue
			}
			// <lo> <hi> [<d1> <d2> ...] 数组映射
			if m := regexp.MustCompile(`\[([^\]]*)\]`).FindStringSubmatch(line); m != nil {
				arr := hexTokenRe.FindAllStringSubmatch(m[1], -1)
				for i, t := range arr {
					if v, ok := parseHex(t[1]); ok {
						out[lo+uint32(i)] = rune(v)
					}
				}
			}
		}
	}
	return out, width
}

// splitBlocks 取出 begin...end 之间的内容（不含标记行）。
func splitBlocks(text, begin, end string) []string {
	var out []string
	rest := text
	for {
		i := strings.Index(rest, begin)
		if i < 0 {
			return out
		}
		rest = rest[i+len(begin):]
		j := strings.Index(rest, end)
		if j < 0 {
			return out
		}
		out = append(out, rest[:j])
		rest = rest[j+len(end):]
	}
}

// utf16BeToRunes 把 CMap 目标端（UTF-16BE hex）解成 rune 序列。
func utf16BeToRunes(hexStr string) []rune {
	hexStr = strings.Join(strings.Fields(hexStr), "")
	if len(hexStr)%4 != 0 {
		if len(hexStr)%2 != 0 {
			return nil
		}
		// 2 字节一组，退化成单字节（罕见，但别 panic）
		var out []rune
		for i := 0; i+2 <= len(hexStr); i += 2 {
			if v, err := strconv.ParseUint(hexStr[i:i+2], 16, 16); err == nil {
				out = append(out, rune(v))
			}
		}
		return out
	}
	var out []rune
	for i := 0; i+4 <= len(hexStr); i += 4 {
		v, err := strconv.ParseUint(hexStr[i:i+4], 16, 32)
		if err != nil {
			continue
		}
		out = append(out, rune(v))
	}
	return out
}

// decodeContentStream 从内容流里抽出所有文本绘制指令并过 CMap 还原。
// 每个 TJ/Tj 片段产出一个字符串元素。
func decodeContentStream(content []byte, cmap map[uint32]rune, codeWidth int) []string {
	var out []string
	rest := content
	for {
		// 找到下一个文本绘制指令
		ti := bytes.Index(rest, []byte("] TJ"))
		si := bytes.Index(rest, []byte("> Tj"))
		if ti < 0 && si < 0 {
			return out
		}
		useTj := si >= 0 && (ti < 0 || si < ti)

		var start int
		if useTj {
			start = bytes.LastIndex(rest[:si], []byte("<"))
		} else {
			start = bytes.LastIndex(rest[:ti], []byte("["))
		}
		if start < 0 {
			rest = rest[max(ti, si)+1:]
			continue
		}
		seg := rest[start:max(ti, si)]
		rest = rest[max(ti, si)+3:]

		var sb strings.Builder
		for _, m := range hexTokenRe.FindAllStringSubmatch(string(seg), -1) {
			sb.WriteString(codesToText(m[1], cmap, codeWidth))
		}
		out = append(out, sb.String())
	}
}

// codesToText 把一段 glyphID hex 按 codeWidth 切分并过 CMap 还原成文本。
// 未映射到的 code 会被跳过（对应「字形缺失被替换成空格」的情况——这正是要暴露的现象）。
func codesToText(hexStr string, cmap map[uint32]rune, codeWidth int) string {
	hexStr = strings.Join(strings.Fields(hexStr), "")
	if codeWidth <= 0 {
		codeWidth = 2
	}
	step := codeWidth * 2
	var sb strings.Builder
	for i := 0; i+step <= len(hexStr); i += step {
		v, err := strconv.ParseUint(hexStr[i:i+step], 16, 32)
		if err != nil {
			continue
		}
		if r, ok := cmap[uint32(v)]; ok {
			sb.WriteRune(r)
		} else {
			// gopdf 缺字形时生成的子集里没有该 code（或被替换成空格），
			// 用空串占位，让「数字消失」直接反映到回读文本上。
			continue
		}
	}
	return sb.String()
}
