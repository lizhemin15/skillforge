package skillgen

// ===== attach.go 的单测 =====
//
// 这份测试的两条原则：
//
//  1. **手造 fixture，不依赖真实样本文件**。原因不是洁癖，是 CI：往仓库里塞一个
//     真的 .doc（几 MB 二进制 blob）时，没人 review 得动它，也没人知道它里面到底
//     内嵌了什么；而手造的那个 CFB 每个字节的含义都写在 buildCFB 里，改坏了能看出
//     改坏了哪儿。
//  2. 先给 fixture 本身写测试（TestBuildCFB_FixtureIsRealContainer）。否则 fixture
//     一旦构造错了，提取函数「正确地返回 nil」会让一堆断言统统变绿——测试全绿而
//     功能全坏，这是最坏的一种绿。
//
// fixture 的一个已知偏差：CFB 里的流都 ≥ 4096 字节。mscfb 对 <4096 的流走
// ministream（另一套扇区表），这里没造 ministream——那条路径是 mscfb 自己的职责，
// 有它自己的测试；本文件的重点是「流里的载荷怎么被认出来」，与走哪张扇区表无关。

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/richardlehane/mscfb"
)

// ===== fixture：手造容器 =====

type cfbStream struct {
	name string
	data []byte
}

// buildCFB 手造一个最小可用的复合文件（CFB/OLE2）容器。
//
// 布局（均为 512 字节扇区，major version 3）：
//
//	扇区 0        FAT
//	扇区 1..D     目录（每扇区 4 个 128 字节目录项，项 0 是 Root Entry）
//	扇区 D+1..    各流的载荷，按顺序紧排
//
// 只支持 regular sector（见文件头注释：不造 ministream）。
func buildCFB(streams []cfbStream) []byte {
	const (
		sec      = 512
		freeSect = 0xFFFFFFFF
		endChain = 0xFFFFFFFE
		fatSect  = 0xFFFFFFFD
		noStream = 0xFFFFFFFF
	)
	dirSectors := (len(streams) + 1 + 3) / 4

	type place struct{ start, n int }
	places := make([]place, len(streams))
	next := 1 + dirSectors
	for i, s := range streams {
		n := (len(s.data) + sec - 1) / sec
		if n == 0 {
			n = 1
		}
		places[i] = place{next, n}
		next += n
	}
	total := next

	fat := make([]uint32, sec/4)
	for i := range fat {
		fat[i] = freeSect
	}
	fat[0] = fatSect
	for i := 0; i < dirSectors; i++ {
		sn := uint32(1 + i)
		if i == dirSectors-1 {
			fat[sn] = endChain
		} else {
			fat[sn] = sn + 1
		}
	}
	for i := range streams {
		for j := 0; j < places[i].n; j++ {
			sn := uint32(places[i].start + j)
			if j == places[i].n-1 {
				fat[sn] = endChain
			} else {
				fat[sn] = sn + 1
			}
		}
	}

	// 注意这个 +1：CFB 的扇区号是**相对头部**编号的（mscfb 的 fileOffset 就是
	// (sn+1)*sectorSize），扇区 0 落在文件偏移 512 处。所以 total 个扇区要 total+1 段
	// 512 字节——头部自己占掉第一段。少留这一段的话，最后一个扇区会落到文件末尾之外，
	// 读它时 mscfb 报 "underlying reader fail (EOF)"。
	buf := make([]byte, (total+1)*sec)

	// 头（512 字节），字段偏移见 mscfb 的 headerFields
	h := buf[:sec]
	binary.LittleEndian.PutUint64(h[0:], 0xE11AB1A1E011CFD0) // 签名 = 魔数 D0CF11E0A1B11AE1
	binary.LittleEndian.PutUint16(h[24:], 0x003E)            // minor
	binary.LittleEndian.PutUint16(h[26:], 0x0003)            // major=3 → 512 字节扇区
	binary.LittleEndian.PutUint16(h[28:], 0xFFFE)            // 小端
	binary.LittleEndian.PutUint16(h[30:], 0x0009)            // sectorShift=9 → 512
	binary.LittleEndian.PutUint16(h[32:], 0x0040)            // mini sector = 64
	binary.LittleEndian.PutUint32(h[40:], 0)                 // numDirectorySectors（v3 必须为 0）
	binary.LittleEndian.PutUint32(h[44:], 1)                 // numFatSectors
	binary.LittleEndian.PutUint32(h[48:], 1)                 // 目录起始扇区
	binary.LittleEndian.PutUint32(h[56:], 4096)              // mini stream cutoff
	binary.LittleEndian.PutUint32(h[60:], endChain)          // miniFatSectorLoc：无
	binary.LittleEndian.PutUint32(h[64:], 0)                 // numMiniFatSectors=0
	binary.LittleEndian.PutUint32(h[68:], endChain)          // difatSectorLoc（numDifat=0 时不用）
	binary.LittleEndian.PutUint32(h[72:], 0)                 // numDifatSectors=0
	for i := 0; i < 109; i++ {
		binary.LittleEndian.PutUint32(h[76+i*4:], freeSect)
	}
	binary.LittleEndian.PutUint32(h[76:], 0) // DIFAT[0] → FAT 在扇区 0

	// FAT 扇区（扇区 0 → 文件偏移 512）
	for i, v := range fat {
		binary.LittleEndian.PutUint32(buf[sec+i*4:], v)
	}

	// 目录扇区（扇区 1 → 文件偏移 1024）
	dirOff := sec * 2
	putEntry := func(idx int, name string, objType byte, child uint32, start uint32, size uint32) {
		e := buf[dirOff+idx*128:]
		ru := utf16.Encode([]rune(name))
		for i, r := range ru {
			binary.LittleEndian.PutUint16(e[i*2:], r)
		}
		binary.LittleEndian.PutUint16(e[64:], uint16((len(ru)+1)*2)) // 含结尾 NUL
		e[66] = objType
		e[67] = 1 // black
		binary.LittleEndian.PutUint32(e[68:], noStream)
		binary.LittleEndian.PutUint32(e[72:], noStream)
		binary.LittleEndian.PutUint32(e[76:], child)
		binary.LittleEndian.PutUint32(e[116:], start)
		binary.LittleEndian.PutUint32(e[120:], size)
	}
	putEntry(0, "Root Entry", 5, 1, endChain, 0) // root：child=1，无 ministream
	for i, s := range streams {
		putEntry(1+i, s.name, 2, noStream, uint32(places[i].start), uint32(len(s.data)))
	}

	// 载荷
	for i, s := range streams {
		copy(buf[(places[i].start+1)*sec:], s.data)
	}
	return buf
}

// buildZip 打一个 zip 包（parts 按顺序写入）。docx/xlsx 就是 zip，够用了。
func buildZip(parts [][2]string) []byte {
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, p := range parts {
		w, err := zw.Create(p[0])
		if err != nil {
			panic(err)
		}
		if _, err := w.Write([]byte(p[1])); err != nil {
			panic(err)
		}
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// buildDocx 造一份「像 docx」的 zip：有 word/document.xml，体积靠正文撑到 4096 以上
// （绕开 ministream，见文件头注释）。
func buildDocx(body string) []byte {
	return buildZip([][2]string{
		{"[Content_Types].xml", `<?xml version="1.0"?><Types/>`},
		{"word/document.xml", `<?xml version="1.0"?><w:document><w:body><w:p>` + body + `</w:p></w:body></w:document>`},
	})
}

func buildXlsx(body string) []byte {
	return buildZip([][2]string{
		{"[Content_Types].xml", `<?xml version="1.0"?><Types/>`},
		{"xl/workbook.xml", `<?xml version="1.0"?><workbook><sheets><sheet name="` + body + `"/></sheets></workbook>`},
	})
}

// bigBody 造一段足够长的正文，让容器超过 mini stream cutoff（4096）。
// 填充必须是**不可压缩**的：这层正文最后要经过 zip 的 deflate，
// strings.Repeat("正文填充", 800) 会被压成几十字节，整个 docx 只有 458 字节，
// 塞进 CFB 后走 ministream 分支——那不是本 fixture 想测的路径。
func bigBody(seed string) string {
	return seed + padText(12000)
}

// padText 造 n 个字符的确定性伪随机填充（LCG）。选它有两个原因：
// 体积撑得住（36 个符号的伪随机串 deflate 只能压到六成左右，12000 字符 → 约 7800 字节），
// 且每次跑出来的字节完全一致——真随机会让失败无法复现。
func padText(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	s := uint32(20261013)
	for i := range b {
		s = s*1664525 + 1013904223 // Numerical Recipes LCG
		b[i] = alphabet[(s>>16)%uint32(len(alphabet))]
	}
	return string(b)
}

// buildOle10Native 手造 Ole10Native 流的两个锚点：NUL 结尾的原始文件名（含临时路径）、
// 以及紧贴载荷的 4 字节长度字段（payload 就是流的尾部）。
func buildOle10Native(tmpPath string, payload []byte) []byte {
	var b bytes.Buffer
	b.Write([]byte{0x02, 0x00, 0x00, 0x00}) // 开头几个标志/长度字段
	b.WriteString("C:\\Users\\jamily\\AppData\\Local\\Temp\\")
	b.WriteString(tmpPath)
	b.WriteByte(0)
	b.Write([]byte{0x00, 0x00, 0x00, 0x00, 0x01, 0x00})
	var lb [4]byte
	binary.LittleEndian.PutUint32(lb[:], uint32(len(payload)))
	b.Write(lb[:])
	b.Write(payload)
	return b.Bytes()
}

// utf16beHex 把字符串转成 PDF 十六进制名字符串（FEFF 前缀 = UTF-16BE）。
func utf16beHex(s string) string {
	var sb strings.Builder
	sb.WriteString("FEFF")
	for _, r := range s {
		fmt.Fprintf(&sb, "%04X", r)
	}
	return sb.String()
}

// buildPDFWithEmbedded 手造一份带 /EmbeddedFile 的 PDF（公文附件包的最小形态）。
// hexName=true 时名字走 /UF <FEFF...>（中文名常见形态），否则走 /F (...)。
// filter=false 时流不压缩，覆盖「明文流」分支。
func buildPDFWithEmbedded(name string, hexName bool, payload []byte, filter bool) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.7\n")
	b.WriteString("1 0 obj\n<< /Type /Filespec /F ")
	if hexName {
		b.WriteString("<" + utf16beHex(name) + ">")
	} else {
		b.WriteString("(" + name + ")")
	}
	b.WriteString(" /EF << /F 2 0 R >> >>\nendobj\n")

	data := payload
	dict := ""
	if filter {
		var z bytes.Buffer
		zw := zlib.NewWriter(&z)
		if _, err := zw.Write(payload); err != nil {
			panic(err)
		}
		if err := zw.Close(); err != nil {
			panic(err)
		}
		data = z.Bytes()
		dict = " /Filter /FlateDecode"
	}
	b.WriteString("2 0 obj\n<< /Type /EmbeddedFile /Length " + strconv.Itoa(len(data)) + dict + " >>\nstream\n")
	b.Write(data)
	b.WriteString("\nendstream\nendobj\n")
	b.WriteString("trailer\n<< /Root 1 0 R >>\n%%EOF\n")
	return b.Bytes()
}

// ===== 0. fixture 自检 =====

func TestBuildCFB_FixtureIsRealContainer(t *testing.T) {
	payload := bytes.Repeat([]byte("ABCDEFGH"), 1000) // 8000 字节 → regular sector
	raw := buildCFB([]cfbStream{{name: "Package", data: payload}})

	if !hasOLEMagic(raw) {
		t.Fatalf("手造的容器前 8 字节不是 OLE2 魔数：%x", raw[:8])
	}
	r, err := mscfb.New(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("mscfb 打不开手造的容器：%v", err)
	}
	f, err := r.Next()
	if err != nil {
		t.Fatalf("读第一个目录项失败：%v", err)
	}
	if f.Name != "Package" {
		t.Errorf("流名 = %q，want %q", f.Name, "Package")
	}
	if int(f.Size) != len(payload) {
		t.Errorf("流大小 = %d，want %d", f.Size, len(payload))
	}
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("读流内容失败：%v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("流内容与写入不一致（读到 %d 字节，want %d）", len(got), len(payload))
	}
	if len(got) < 4096 {
		t.Fatalf("fixture 的流小于 4096，会走 ministream 分支（本 fixture 没造 ministream）")
	}
}

// ===== 1. docx 里内嵌 OLE 附件（用户的主力场景）=====

func TestExtractAttachments_DocxOLEEmbedding(t *testing.T) {
	want := buildDocx(bigBody("报名表模板"))
	if len(want) <= 4096 {
		t.Fatalf("内嵌载荷只有 %d 字节，撑不到 regular sector", len(want))
	}
	ole := buildCFB([]cfbStream{{name: "Package", data: want}})

	outer := buildZip([][2]string{
		{"[Content_Types].xml", `<?xml version="1.0"?><Types/>`},
		{"word/document.xml", `<?xml version="1.0"?><w:document><w:body><w:p>附件：1.报名表</w:p></w:body></w:document>`},
		{"word/embeddings/oleObject1.bin", string(ole)},
	})

	got := ExtractAttachments("关于开展XX工作的通知.docx", outer, "正文……\n附件：1.报名表\n")
	if len(got) != 1 {
		t.Fatalf("拆出 %d 个附件，want 1：%+v", len(got), got)
	}
	a := got[0]
	if a.Name != "报名表.docx" {
		t.Errorf("Name = %q，want %q（名字应来自正文清单，不是 oleObject1.bin）", a.Name, "报名表.docx")
	}
	if a.Rel != "attachments/报名表.docx" {
		t.Errorf("Rel = %q", a.Rel)
	}
	if a.Origin != "关于开展XX工作的通知.docx" {
		t.Errorf("Origin = %q", a.Origin)
	}
	if !strings.HasPrefix(a.How, "docx:embeddings>ole:Package") {
		t.Errorf("How = %q，want 以 docx:embeddings>ole:Package 开头（要能看出穿过了哪几层容器）", a.How)
	}
	if a.Size != len(want) {
		t.Errorf("Size = %d，want %d", a.Size, len(want))
	}
	if !bytes.Equal(a.Raw, want) {
		t.Errorf("Raw 与内嵌载荷不一致（%d vs %d 字节）", len(a.Raw), len(want))
	}
	if a.MIME == "" {
		t.Errorf("MIME 为空（下发给用户时要带 content-type）")
	}
}

// ===== 2. docx 里直嵌的 OOXML（WPS 常见）=====

func TestExtractAttachments_DocxDirectXlsxEmbedding(t *testing.T) {
	want := buildXlsx(bigBody("汇总表"))
	outer := buildZip([][2]string{
		{"word/document.xml", `<?xml version="1.0"?><w:document/>`},
		{"word/embeddings/embed1.xlsx", string(want)},
	})

	got := ExtractAttachments("通知.docx", outer, "")
	if len(got) != 1 {
		t.Fatalf("拆出 %d 个附件，want 1：%+v", len(got), got)
	}
	if got[0].Name != "embed1.xlsx" {
		t.Errorf("Name = %q，want embed1.xlsx（正文明细为空时退回容器内的原名）", got[0].Name)
	}
	if !bytes.Equal(got[0].Raw, want) {
		t.Errorf("Raw 不一致")
	}
	if got[0].How != "docx:embeddings>embed:office" {
		t.Errorf("How = %q", got[0].How)
	}
}

// ===== 3. 「扩展名说假话」：.bin 里其实装着 xlsx =====

func TestExtractAttachments_ExtensionLies(t *testing.T) {
	want := buildXlsx(bigBody("明细"))
	outer := buildZip([][2]string{
		{"word/document.xml", `<?xml version="1.0"?><w:document/>`},
		{"word/embeddings/oleObject2.bin", string(want)}, // 名叫 .bin，内容是 xlsx
	})

	got := ExtractAttachments("通知.docx", outer, "")
	if len(got) != 1 {
		t.Fatalf("拆出 %d 个附件，want 1", len(got))
	}
	if !strings.HasSuffix(got[0].Name, ".xlsx") {
		t.Errorf("Name = %q，want 以 .xlsx 结尾（判据是 magic，不是扩展名）", got[0].Name)
	}
	if got[0].Name == "oleObject2.bin" {
		t.Errorf("把容器占位名当成了附件名：%q", got[0].Name)
	}
}

// ===== 4. 老式 OLE：正文本体流不是附件 =====

func TestExtractAttachments_SkipsDocumentBodyStreams(t *testing.T) {
	body := bytes.Repeat([]byte("W\x00O\x00R\x00D\x00"), 2000)
	raw := buildCFB([]cfbStream{
		{name: "WordDocument", data: body},
		{name: "1Table", data: body},
	})
	if got := ExtractAttachments("老公文.doc", raw, ""); len(got) != 0 {
		t.Errorf("把文档本体当成了附件：%+v", got)
	}
}

// ===== 5. 老式 OLE：Ole10Native（拖进来的对象，流里自带原名）=====

func TestExtractAttachments_Ole10Native(t *testing.T) {
	payload := buildDocx(bigBody("模板"))

	cases := []struct {
		name    string
		tmpName string
		want    string
	}{
		{"ASCII 原名", "baomingbiao.docx", "baomingbiao.docx"},
		{"中文原名", "报名表.docx", "报名表.docx"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream := buildOle10Native(tc.tmpName, payload)
			raw := buildCFB([]cfbStream{{name: "\x01Ole10Native", data: stream}})

			got := ExtractAttachments("拖进来的对象.docx", raw, "")
			if len(got) != 1 {
				t.Fatalf("拆出 %d 个附件，want 1：%+v", len(got), got)
			}
			if got[0].Name != tc.want {
				t.Errorf("Name = %q，want %q", got[0].Name, tc.want)
			}
			if !bytes.Equal(got[0].Raw, payload) {
				t.Errorf("Raw 不是 Ole10Native 里的载荷（%d vs %d 字节）", len(got[0].Raw), len(payload))
			}
			if got[0].How != "ole:Ole10Native" {
				t.Errorf("How = %q", got[0].How)
			}
		})
	}
}

// 长度字段不可信时的兜底：从第一个魔数截到流尾。
func TestExtractAttachments_Ole10NativeNoLengthField(t *testing.T) {
	payload := buildDocx(bigBody("兜底"))
	var b bytes.Buffer
	b.Write([]byte{0x02, 0x00, 0x00, 0x00})
	b.WriteString("C:\\Temp\\aa.docx\x00")
	b.Write(payload) // 后面没有长度字段
	raw := buildCFB([]cfbStream{{name: "\x01Ole10Native", data: b.Bytes()}})

	got := ExtractAttachments("x.docx", raw, "")
	if len(got) != 1 {
		t.Fatalf("拆出 %d 个附件，want 1（长度字段不可信时应按魔数兜底）", len(got))
	}
	if !bytes.Equal(got[0].Raw, payload) {
		t.Errorf("兜底截取的内容不对（%d vs %d 字节）", len(got[0].Raw), len(payload))
	}
}

// ===== 6. PDF 内嵌文件 =====

func TestExtractAttachments_PDFEmbeddedFile(t *testing.T) {
	payload := buildDocx(bigBody("申请表"))

	cases := []struct {
		name     string
		fileName string
		hexName  bool
		filter   bool
	}{
		{"/F 明文名 + FlateDecode", "baomingbiao.docx", false, true},
		{"/UF 十六进制中文名 + FlateDecode", "报名表.docx", true, true},
		{"不压缩的明文流", "baomingbiao.docx", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := buildPDFWithEmbedded(tc.fileName, tc.hexName, payload, tc.filter)
			got := ExtractAttachments("附件包.pdf", raw, "")
			if len(got) != 1 {
				t.Fatalf("拆出 %d 个附件，want 1：%+v", len(got), got)
			}
			if got[0].Name != tc.fileName {
				t.Errorf("Name = %q，want %q", got[0].Name, tc.fileName)
			}
			if !bytes.Equal(got[0].Raw, payload) {
				t.Errorf("Raw 解出来不对（%d vs %d 字节）", len(got[0].Raw), len(payload))
			}
			if got[0].How != "pdf:EmbeddedFile" {
				t.Errorf("How = %q", got[0].How)
			}
		})
	}
}

// ===== 7. 没有附件时要老老实实返回空 =====

func TestExtractAttachments_NothingToExtract(t *testing.T) {
	t.Run("普通 docx（含插图）", func(t *testing.T) {
		raw := buildZip([][2]string{
			{"word/document.xml", `<?xml version="1.0"?><w:document/>`},
			{"word/media/image1.png", strings.Repeat("PNG", 100)},
		})
		if got := ExtractAttachments("通知.docx", raw, "附件：1.报名表"); len(got) != 0 {
			t.Errorf("word/media 是正文插图，不是附件：%+v", got)
		}
	})
	t.Run("纯文本", func(t *testing.T) {
		if got := ExtractAttachments("通知.txt", []byte("附件：1.报名表"), "附件：1.报名表"); len(got) != 0 {
			t.Errorf("非容器文件不该拆出附件：%+v", got)
		}
	})
	t.Run("认不出的二进制载荷不下发", func(t *testing.T) {
		junk := bytes.Repeat([]byte{0x11, 0x22, 0x33, 0x44}, 2000)
		outer := buildZip([][2]string{
			{"word/document.xml", `<?xml version="1.0"?><w:document/>`},
			{"word/embeddings/oleObject1.bin", string(junk)},
		})
		if got := ExtractAttachments("通知.docx", outer, ""); len(got) != 0 {
			t.Errorf("认不出来就该放弃，不能让用户收到打不开的文件：%+v", got)
		}
	})
	t.Run("PDF 里没有 /EmbeddedFile", func(t *testing.T) {
		raw := []byte("%PDF-1.7\n1 0 obj\n<< /Type /Page >>\nendobj\n%%EOF\n")
		if got := ExtractAttachments("正文.pdf", raw, ""); len(got) != 0 {
			t.Errorf("拆出了不存在的附件：%+v", got)
		}
	})
}

// ===== 8. 命名：清单数量对不上就不配对 =====

func TestExtractAttachments_NamePairing(t *testing.T) {
	a := buildXlsx(bigBody("甲"))
	b := buildXlsx(bigBody("乙"))
	outer := buildZip([][2]string{
		{"word/document.xml", `<?xml version="1.0"?><w:document/>`},
		{"word/embeddings/embed1.xlsx", string(a)},
		{"word/embeddings/embed2.xlsx", string(b)},
	})

	t.Run("数量一致按序配对", func(t *testing.T) {
		got := ExtractAttachments("通知.docx", outer, "附件：1.报名表 2.汇总表\n")
		if len(got) != 2 {
			t.Fatalf("拆出 %d 个附件，want 2", len(got))
		}
		if got[0].Name != "报名表.xlsx" || got[1].Name != "汇总表.xlsx" {
			t.Errorf("配对结果 = %q/%q", got[0].Name, got[1].Name)
		}
		if !bytes.Equal(got[0].Raw, a) || !bytes.Equal(got[1].Raw, b) {
			t.Errorf("配对把内容搞错了")
		}
	})

	t.Run("数量不一致整体不配对", func(t *testing.T) {
		got := ExtractAttachments("通知.docx", outer, "附件：1.报名表\n")
		if len(got) != 2 {
			t.Fatalf("拆出 %d 个附件，want 2", len(got))
		}
		for _, x := range got {
			if x.Name == "报名表.xlsx" {
				t.Errorf("只列了 1 个名字却配上 2 个附件，名字是错的：%+v", got)
			}
		}
		if got[0].Name != "embed1.xlsx" || got[1].Name != "embed2.xlsx" {
			t.Errorf("应退回容器内原名，实得 %q/%q", got[0].Name, got[1].Name)
		}
	})
}

func TestAttachFileName(t *testing.T) {
	t.Run("同名去重", func(t *testing.T) {
		used := map[string]int{}
		if got := attachFileName("报名表.docx", "docx", used); got != "报名表.docx" {
			t.Errorf("第一次 = %q", got)
		}
		if got := attachFileName("报名表.docx", "docx", used); got != "报名表-2.docx" {
			t.Errorf("第二次 = %q，want 报名表-2.docx（同名不同内容不能互相覆盖）", got)
		}
	})
	t.Run("扩展名以嗅探结果为准", func(t *testing.T) {
		// 名字线索说 .xlsx，但内容嗅探出 docx
		got := attachFileName("C:\\Temp\\报名表.xlsx", "docx", map[string]int{})
		if got != "报名表.docx" {
			t.Errorf("= %q，want 报名表.docx", got)
		}
	})
	t.Run("保留字清洗", func(t *testing.T) {
		got := attachFileName("报名表:1*2?", "docx", map[string]int{})
		if strings.ContainsAny(got, `: *?"<>|\`) {
			t.Errorf("落盘名里还有保留字：%q", got)
		}
		if !strings.HasSuffix(got, ".docx") {
			t.Errorf("= %q", got)
		}
	})
	t.Run("空名字兜底", func(t *testing.T) {
		if got := attachFileName("   ", "docx", map[string]int{}); got != "附件.docx" {
			t.Errorf("= %q，want 附件.docx", got)
		}
	})
}

// ===== 9. 正文清单解析 =====

func TestParseAttachmentList(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"行内多项", "附件：1.报名表 2.汇总表", []string{"报名表", "汇总表"}},
		{"清单独占一行", "附件：\n1.报名表\n2.汇总表\n", []string{"报名表", "汇总表"}},
		{"字间空格 + 编号在附件后", "附 件 1：报名表", []string{"报名表"}},
		{"括号编号", "附件：\n(1)报名表\n(2)汇总表", []string{"报名表", "汇总表"}},
		{"掐掉附注", "附件：报名表（一式两份）", []string{"报名表"}},
		{"整句不是文件名", "附件：请于9月30日前报送。", nil},
		{"“见下”不是文件名", "附件：见下", nil},
		{"“详见附件”不是文件名", "附件：详见附件", nil},
		{"“附后”不是文件名", "附件：附后", nil},
		// 反向守卫：上面几条引用语过滤很容易写成「含『见』就丢」，
		// 那会把这条真名字一起吃掉。名单少一份附件，用户收到的是残缺的。
		{"“见证材料汇总表”是真名字，不能被引用语规则误伤", "附件：见证材料汇总表", []string{"见证材料汇总表"}},
		{"没有附件行", "各科室：\n请认真落实。", nil},
		{"空文本", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAttachmentList(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("= %q，want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("= %q，want %q", got, tc.want)
				}
			}
		})
	}
}
