package skillgen

// 二进制判定（looksBinary）的回归防线。
//
// 守的是一次真实线上事故：手册流水线对 17MB 的 vector PDF **静默降级**了——
// 素材被判成「文本」→ 跳过 OCR → 原始 PDF 字节当原文灌进 LLM → provider 报
// 26 万 token 超限 → 抽取失败 → 却仍发 done 帧报成功。
//
// 根因是旧判据「只统计前 8192 字节的控制字符占比」，而 PDF 的头部和尾部都是
// 纯 ASCII（%PDF- 头 + xref 表）。实测该文件前 8192 字节的控制字符占比是
// **0.0000%**，于是 17MB 二进制被判成文本。
//
// 本文件同时守住两个方向，只测一边等于把 bug 从左边挪到右边：
//   - 正向：二进制素材必须判 true（漏判 = 静默污染 LLM）；
//   - 反向：OCR 产出的中文文本必须判 false（误判 = 素材被丢弃，报「原文为空」）。
//
// 防止「改断言就绿」：两个方向都做过故障注入验证能变红——
// 采样退回「只看头部」+ 短路魔数层（即复现修复前行为），vector 用例变红；
// binaryMinHits 改成 0（即退回「只看占比」），零星 NUL 用例变红。

import (
	"bytes"
	"compress/flate"
	"math/rand"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestLookBinaryRealFiles 用仓库里的真实素材做双向断言，并把逐段统计打进日志。
//
// 为什么要打逐段统计：它记录了「必须采中段」这个结论的原始证据。实测
// manual-vector.pdf 的头部与尾部控制字符都是 0.00%，只有 1/3、2/3 两段
// 是压缩流（控制字符 40%+）。也就是说采样若只取「头部 + 尾部」（很常见的做法）
// 依然漏判——这个数字是量出来的，不是推出来的，留在测试日志里供后人复核。
//
// 注意：真实 PDF 有 54MB，不进版本库。文件在就顺手跑一遍全尺寸验证，
// 不在就跳过——**不让断言骑在本机环境上**，否则 CI 上会假绿。
func TestLookBinaryRealFiles(t *testing.T) {
	cases := []struct {
		path string
		want bool
		note string
	}{
		{"../../testdata/manual/manual-vector.pdf", true, "vector PDF：事故元凶，头部/尾部是 ASCII 但中段是压缩流"},
		{"../../testdata/manual/manual-scan.pdf", true, "扫描版 PDF：字节分布随机，旧判据恰好挡住的那个"},
		{"../../testdata/manual/manual-ocr.txt", false, "OCR 产出的中文手册文本：含零星 NUL，必须仍判文本"},
		{"manual.go", false, "本包源码：纯文本"},
	}

	for _, c := range cases {
		b, err := os.ReadFile(c.path)
		if err != nil {
			if strings.HasSuffix(c.path, ".pdf") {
				t.Logf("⏭  跳过 %s（%s）：真实 PDF 不入库，见 TestLookBinarySyntheticContainer", c.path, c.note)
				continue
			}
			t.Fatalf("读 %s 失败: %v", c.path, err)
		}
		got := looksBinary(string(b))
		if got != c.want {
			t.Errorf("%s\n  期望 looksBinary=%v 实际=%v\n  说明：%s", c.path, c.want, got, c.note)
		}
		mark := "✅"
		if got != c.want {
			mark = "❌"
		}
		t.Logf("%s %s\n  字节=%d 期望=%v 实际=%v", mark, c.note, len(b), c.want, got)

		if c.want {
			for i, seg := range sniffSegments(string(b)) {
				total, ctrl, nul, invalid := segStats(seg)
				if total == 0 {
					continue
				}
				pct := func(n int) float64 { return float64(n) * 100 / float64(total) }
				t.Logf("    采样段%d rune=%d 非法UTF8=%.2f%% 控制=%.2f%% NUL=%.2f%% 判二进制=%v",
					i, total, pct(invalid), pct(ctrl), pct(nul), segBinary(seg))
			}
		}
	}
}

// syntheticVectorPDF 造一个「vector PDF 式」字节串：干净的 ASCII 文件头 +
// 真实 deflate 压缩流 + 干净的 ASCII 尾部，也就是事故文件的结构骨架。
//
// 中段为什么用 compress/flate 真压一遍，而不是手搓「一串高位字节」：
// 手搓的字节序列很容易凑成合法的 UTF-8 序列，占不着「非法 UTF-8」这条判据，
// 于是样本根本复现不了事故，测试变成假绿。真压缩流里字节是高熵的，
// 与 PDF 里 deflate 流的形态完全一致——这是最不容易自欺的造法。
func syntheticVectorPDF(t *testing.T) string {
	t.Helper()

	// 高熵中文正文：随机汉字不可压缩，压完仍是实体积的高位字节流
	rng := rand.New(rand.NewSource(42)) // 固定种子，样本与判定结果都可复现
	var src strings.Builder
	for i := 0; i < 4000; i++ {
		src.WriteRune(rune(0x4E00 + rng.Intn(0x9FA5-0x4E00)))
	}

	var comp bytes.Buffer
	w, err := flate.NewWriter(&comp, flate.BestCompression)
	if err != nil {
		t.Fatalf("flate writer: %v", err)
	}
	if _, err := w.Write([]byte(src.String())); err != nil {
		t.Fatalf("flate write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("flate close: %v", err)
	}

	var b bytes.Buffer
	b.WriteString("%PDF-1.7\n") // ① 纯 ASCII 头：旧判据看到的全部内容
	// 头部要撑到 4KB 采样段之外，否则压缩流会溢进第一个采样段，
	// 那样「头部干净、中段才脏」这个事故结构就没复现出来（实测踩过）
	b.WriteString(strings.Repeat("1 0 obj << /Type /Catalog /Pages 2 0 R >> endobj\n", 300))
	b.WriteString("stream\n")
	b.Write(comp.Bytes()) // ② 中段：真实压缩流
	b.WriteString("\nendstream\nendobj\n")
	b.WriteString(strings.Repeat("0 0 0 0 0 0 0 0 n\n", 200))
	b.WriteString("startxref\n12345\n%%EOF\n") // ③ 纯 ASCII 尾：xref 表，也是干净的
	return b.String()
}

// TestLookBinarySyntheticContainer 是不骑环境的核心断言：事故的复现不依赖
// 那 54MB 真实素材，任何机器上都能跑。
//
// 断言锁的是行为——「头部和尾部都是干净 ASCII 的二进制流，仍须判为二进制」。
func TestLookBinarySyntheticContainer(t *testing.T) {
	raw := syntheticVectorPDF(t)
	if len(raw) < 3*sniffSegLen {
		t.Fatalf("合成样本太小(%d)，采样覆盖不到中段，测试失去意义", len(raw))
	}

	// 先自证样本真的复现了旧判据的盲区，否则整条测试是无意义的绿灯
	head := raw
	if len(head) > 8192 {
		head = head[:8192]
	}
	headTotal := len([]rune(head))
	_, headCtrl, headNul, headInvalid := segStats(head)
	if headTotal == 0 {
		t.Fatal("合成样本头部为空")
	}
	headBad := float64(headCtrl+headNul) * 100 / float64(headTotal)
	if headBad > 10 {
		t.Fatalf("合成样本头部控制字符占比 %.2f%%（>10%%），没能复现「头部干净」这个前提，测试无效", headBad)
	}
	t.Logf("合成样本：字节=%d 头部 rune=%d 控制=%d NUL=%d 非法UTF8=%d",
		len(raw), headTotal, headCtrl, headNul, headInvalid)
	t.Logf("  ↳ 旧判据只看头部 8192 字节：控制字符占比 %.4f%% → 会判「文本」（事故复现前提成立）", headBad)

	// 中段必须真的携带高熵字节，否则这条测试只是在验证一个空集
	segs := sniffSegments(raw)
	midBad := 0
	for i, seg := range segs {
		total, ctrl, nul, invalid := segStats(seg)
		if total == 0 {
			continue
		}
		pct := func(n int) float64 { return float64(n) * 100 / float64(total) }
		t.Logf("    采样段%d rune=%d 非法UTF8=%.2f%% 控制=%.2f%% NUL=%.2f%% 判二进制=%v",
			i, total, pct(invalid), pct(ctrl), pct(nul), segBinary(seg))
		if invalid >= binaryMinHits && pct(invalid) > 2 {
			midBad++
		}
	}
	if midBad == 0 {
		t.Fatal("合成样本没有任何采样段达到「非法 UTF-8」判据，测试骑在空集上，无效")
	}

	if !looksBinary(raw) {
		t.Errorf("头部与尾部都是干净 ASCII 的二进制流被误判为文本——这正是事故的复现，" +
			"17MB 原始字节会因此灌进 LLM")
	}

	// 这条断言锁的是「多段采样」这一层的**独立**能力，不是魔数层。
	// 剥掉开头 6KB（含 %PDF- 魔数与整段 ASCII 头）后再判：此时魔数层已失效，
	// 判定只能靠采样段覆盖到中段的压缩流。如果实现退化成「只看头部」，
	// 这里必然判成文本——这正是不依赖魔数层兜底才算真修好的证据。
	const stripped = 6144
	if len(raw) <= stripped {
		t.Fatalf("合成样本仅 %d 字节，剥掉头部后无法覆盖采样段", len(raw))
	}
	if !looksBinary(raw[stripped:]) {
		t.Errorf("剥掉开头 %d 字节（含魔数）后判成文本：说明判定只靠魔数层兜底，"+
			"采样没覆盖到中段——旧事故换了个触发条件还会复现", stripped)
	}

	// 反向：把干净尾部单独拿出来必须判文本（中段之外的内容不该被牵连）
	tail := raw[len(raw)-3000:]
	if looksBinary(tail) {
		t.Errorf("纯 ASCII 尾部被判成二进制，说明采样把整份文件一刀切了")
	}
}

// TestLookBinaryEdgeCases 覆盖短输入与边界，防止采样切片越界 panic，
// 并锁住两个容易互相矛盾的行为：
//   - 含零星 NUL 的中文必须仍是文本（OCR 产物真实存在这种字节）；
//   - 以文档魔数开头的短样本必须是二进制（PDF/zip 字节流永远是二进制）。
func TestLookBinaryEdgeCases(t *testing.T) {
	cases := []struct {
		in   string
		want bool
		note string
	}{
		{"", false, "空内容"},
		{"# 标题\n\n正文内容，中文。\n", false, "普通 markdown"},
		{"二\x00二六年三月", false, "含零星 NUL 的中文（真实 OCR 产物片段）——占比判据的绝对量门槛就为它设的"},
		{"ok", false, "极短文本"},
		{"%PDF-1.7\n", true, "PDF 魔数：字节流永远是二进制，无论样本多短"},
		{"PK\x03\x04rest", true, "docx/zip 魔数"},
		{"\xD0\xCF\x11\xE0\xA1\xB1\x1A\xE1rest", true, "老式 doc 的 OLE2 魔数"},
	}
	for _, c := range cases {
		if got := looksBinary(c.in); got != c.want {
			t.Errorf("%s\n  输入 %q 期望 %v 实际 %v", c.note, c.in, c.want, got)
		}
	}
}

// segStats 返回采样段的 rune 总数与控制字符/非法 UTF-8 计数。
// 与 segBinary 的计数口径保持一致：NUL 单独计、不重复计入控制字符。
func segStats(seg string) (total, ctrl, nul, invalid int) {
	for i := 0; i < len(seg); {
		r, size := utf8.DecodeRuneInString(seg[i:])
		i += size
		total++
		if r == utf8.RuneError && size == 1 {
			invalid++
			continue
		}
		if r == 0 {
			nul++
			continue
		}
		if r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			ctrl++
		}
	}
	return
}
