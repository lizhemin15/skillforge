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
// 防止「改断言就绿」：这两个方向都用故障注入验证过能变红——
// 把采样退回「只看头部」+ 短路魔数层（即复现修复前行为），
// TestLookBinaryRealFiles 的 vector PDF 用例会红；把 binaryMinHits 改成 0
// （即退回「只看占比」），TestLookBinaryEdgeCases 的零星 NUL 用例会红。

import (
	"os"
	"testing"
	"unicode/utf8"
)

// TestLookBinaryRealFiles 用仓库里的真实素材做双向断言，并把逐段统计打进日志。
//
// 为什么要打逐段统计：它记录了「必须采中段」这个结论的原始证据。实测
// manual-vector.pdf 的头部与尾部控制字符都是 0.00%，只有 1/3、2/3 两段
// 是压缩流（控制字符 40%+）。也就是说采样若只取「头部 + 尾部」（很常见的做法）
// 依然漏判——这个数字是量出来的，不是推出来的，留在测试日志里供后人复核。
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
