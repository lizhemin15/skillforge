package skillgen

// ===== 内嵌附件提取：把「源文件里的附件」拆出来（0 次模型调用）=====
//
// 场景（用户原话）：「给 skillforge 一键生成查询 skill，并能够给用户发送拆分出来的
// word 模板，通常是源文件中的附件」。
// 也就是说：管理员上传一份公文 .docx，正文里写着「附件：1.报名表 2.汇总表」，而这两张
// 表**不是**单独上传的文件，是以 OLE 对象的形式内嵌在这份 docx 里（word/embeddings/
// oleObject1.bin）。用户命中技能时要能把它们作为文件直接下发。所以生成期必须把它们从
// 源容器里拆出来、落盘、登记。
//
// 为什么整条路都不调模型：拆附件是**纯结构**问题（zip 目录项 + CFB 流表），让模型"猜"
// 只会猜出一个不存在的文件名，或者更糟——把正文里那句「附件：报名表」当成文件名去编一份
// 假附件下发。宁可拆不出来（步骤日志如实报「未发现内嵌附件」），也不能伪造一个。
//
// 判定一律按 **magic**，不看扩展名：内网里「把 .xlsx 改名成 .docx」是常见操作，扩展名
// 说不了真话；而 zip / OLE / PDF 三种容器的魔数是无歧义的。与本包 looksBinary 的口径一致。

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"mime"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/lizhemin15/skillforge/internal/model"
	"github.com/lizhemin15/skillforge/internal/store"
	"github.com/richardlehane/mscfb"
)

const (
	// AttachDir 是拆出来的附件在技能目录下的落盘位置。
	//
	// 刻意**不复用** source/：source/ 是「管理员上传的原始留档」，语义是"证据"，
	// 附件是"要下发给用户的产品"。混在一起后，管理端文件树会把它们画成同级，
	// 用户分不清「这个文件是原始素材还是会被发给我的模板」，删除时更容易删错。
	AttachDir = "attachments"

	// attachMaxCount 是单份源文件最多拆几个附件。一份公文里挂 20 张表已经远超
	// 正常范围；超过基本意味着魔数误判（把某个压缩流当成容器一层层钻下去了）。
	attachMaxCount = 20
	// attachMaxBytes 是单个附件的体积上限。飞书 IM 附件上限 30MB，超过这个数
	// 就算拆出来也发不出去，只会让生成的技能带着一个必然失败的下载入口。
	attachMaxBytes = 30 << 20
	// attachMinBytes 是下限：几十字节的"附件"一定是误判（比如 CFB 里的空占位流）。
	attachMinBytes = 64
	// attachMaxDepth 是容器递归层数：docx → 内嵌 docx → 内嵌附件，两层到顶。
	attachMaxDepth = 2
)

// ExtractedAttachment 是从源文件里拆出来的一个附件。
type ExtractedAttachment struct {
	Name string `json:"name"` // 展示名（含扩展名），如 报名表.docx
	Rel  string `json:"rel"`  // 相对技能目录，如 attachments/报名表.docx
	MIME string `json:"mime,omitempty"`
	Size int    `json:"size"`
	// Origin 记的是「从哪份源文件里拆出来的」（上传文件名），用于在步骤日志里
	// 让管理员看清来源；How 记的是"走的哪条提取路径"。
	//
	// 两个都要暴露给日志，理由是同一条：**提取是启发式的**（尤其 Ole10Native 那段
	// 是猜出来的载荷边界）。把过程写出来，拆错时管理员能一眼看出错在哪一层，
	// 而不是只看到一句「附件不对」。
	Origin string `json:"origin"`
	How    string `json:"how"` // docx:embeddings | ole:Package | ole:Ole10Native | pdf:EmbeddedFile | embed:office
	Raw    []byte `json:"-"`
}

// attachmentCandidate 是提取过程中的一个中间产物：载荷 + 已知的名字/扩展名线索。
type attachmentCandidate struct {
	raw  []byte
	ext  string // 嗅探出的扩展名（不含点），如 docx
	orig string // 容器里自带的原始文件名（只有直嵌的 office 文件才有）
	how  string
}

// ExtractAttachments 从一份上传文件里拆出内嵌附件。
//
// mainText 是同一份文件解析出来的正文（用于读「附件：1.报名表」这类清单，把
// 无名内嵌对象配上有意义的文件名）。mainText 为空不影响提取，只是名字退化成「附件N」。
//
// 返回 nil 表示这份文件里没有内嵌附件——这是**正常情况**，不是错误：绝大多数
// 素材文档都不带附件。所以签名里没有 error：调用方要么拿到附件，要么拿到"没有"，
// 不存在"失败"这种需要分支处理的状态。
func ExtractAttachments(filename string, raw []byte, mainText string) []ExtractedAttachment {
	cands := attachCandidates(raw, 0)
	if len(cands) == 0 {
		return nil
	}
	if len(cands) > attachMaxCount {
		log.Printf("[attach] %s：拆出 %d 个候选超过上限 %d，只保留前 %d 个（超过基本是魔数误判）",
			filename, len(cands), attachMaxCount, attachMaxCount)
		cands = cands[:attachMaxCount]
	}

	// 正文清单只在**数量正好对上**时才用来命名。数量对不上就整体不用——半对半错的
	// 配对会把「报名表」写到「汇总表」的头上，而下发时用户看不出错（拿到的是真文件、
	// 只是名字错位），比直接用「附件N」更坏。
	names := parseAttachmentList(mainText)
	if len(names) != len(cands) {
		names = nil
	}

	used := map[string]int{}
	out := make([]ExtractedAttachment, 0, len(cands))
	for i, c := range cands {
		base := ""
		if i < len(names) {
			base = names[i]
		}
		if base == "" {
			base = c.orig
		}
		if base == "" {
			base = fmt.Sprintf("附件%d", i+1)
		}
		name := attachFileName(base, c.ext, used)
		out = append(out, ExtractedAttachment{
			Name:   name,
			Rel:    AttachDir + "/" + name,
			MIME:   attachMIME(name),
			Size:   len(c.raw),
			Origin: filename,
			How:    c.how,
			Raw:    c.raw,
		})
	}
	return out
}

// DedupeAttachments 把多份源文件各自拆出的附件合成一份清单，并处理**跨文件重名**。
//
// 为什么需要单独一趟：ExtractAttachments 的去重表是**单文件内**的（每次调用一张
// used），两份 docx 各带一个「报名表.docx」时，它们各自都是自己那一趟里的第一个，
// 最终名字一模一样。直接拼在一起落盘就是往同一个路径写两次——后写的悄悄覆盖先写的，
// 用户拿到手的不是他以为的那份，而整个过程没有任何报错（同名覆盖是文件系统最安静的
// 一种失败，日志里连一行 warning 都不会有）。
//
// 做法：换一张**跨文件共享**的 used 表，按最终展示名逐条重新定名（重名的追加 -2/-3）。
// 顺序敏感是刻意的：先出现的那份保持原名，后来的改名——清单里第一份通常就是主表，
// 让「报名表.docx」这个名字落在它头上比落在第二份上更符合直觉。
func DedupeAttachments(all []ExtractedAttachment) []ExtractedAttachment {
	if len(all) == 0 {
		return nil
	}
	used := map[string]int{}
	out := make([]ExtractedAttachment, 0, len(all))
	for _, a := range all {
		ext := strings.TrimPrefix(path.Ext(a.Name), ".")
		name := attachFileName(a.Name, ext, used)
		a.Name = name
		a.Rel = AttachDir + "/" + name
		// MIME 跟着最终名字重算：扩展名可能因为去重而变（`x.docx` → `x-2.docx` 不变，
		// 但 `x` → `x.doc` 这种补扩展名的情形会变），凭原来的值会算出与实际不符的
		// Content-Type，下发时浏览器按错误的类型存盘。
		a.MIME = attachMIME(name)
		out = append(out, a)
	}
	return out
}

// WriteAttachments 把拆出来的附件落进 <dir>/attachments/，返回登记进技能元数据的清单。
//
// 名字在这里**不再重新定**（沿用 DedupeAttachments 定死的那份），因为同一份清单会
// 同时出现在两个地方：磁盘上的文件、以及写进提示词让模型转述给用户的清单。两处名字
// 一旦分叉，模型会一本正经地告诉用户「已发送 报名表-2.docx」，而磁盘上根本没有这个
// 文件——这是最难受的一种错：用户觉得技能坏了，日志里却全是成功的。
//
// 落盘失败**不吞**：附件是这个技能的主要交付物，少一个就意味着用户按提示去下载却下
// 不到。宁可让这次创建失败（用户重试一次），也不交付一份「说得出、拿不到」的技能。
func WriteAttachments(dir string, atts []ExtractedAttachment) ([]model.Attachment, error) {
	if len(atts) == 0 {
		return nil, nil
	}
	root := filepath.Join(dir, AttachDir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("建附件目录失败: %w", err)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	out := make([]model.Attachment, 0, len(atts))
	for _, a := range atts {
		target := filepath.Join(dir, filepath.FromSlash(a.Rel))
		absTarget, err := filepath.Abs(target)
		if err != nil {
			return nil, err
		}
		// Rel 已经过 SafeCatFileName 清洗（去掉了路径分隔符），这里再确认一次落地路径
		// 没跳出附件目录——防御性检查成本一行，代价却是「技能目录里凭空多出个文件」，
		// 不划算。与 store.AttachmentPath 的越界检查同一口径。
		if absTarget != absRoot && !strings.HasPrefix(absTarget, absRoot+string(os.PathSeparator)) {
			log.Printf("[attach] 跳过越界附件路径 %q（源 %s）", a.Rel, a.Origin)
			continue
		}
		if err := os.WriteFile(absTarget, a.Raw, 0o644); err != nil {
			return nil, fmt.Errorf("写附件 %s 失败: %w", a.Name, err)
		}
		log.Printf("[attach] 落盘 %s（%d 字节，来自 %s，%s）", a.Rel, a.Size, a.Origin, a.How)
		out = append(out, model.Attachment{
			Name:   a.Name,
			Rel:    a.Rel,
			Origin: a.Origin,
			// ExtractedAttachment.Size 是 int（拆的时候算的是内存里的字节数），
			// 而登记用的 model.Attachment.Size 是 int64（要落 DB 列）。显式转换，
			// 别让两边类型漂着——这里曾经因为隐式期待报过编译错。
			Size: int64(a.Size),
			How:  a.How,
		})
	}
	return out, nil
}

// extractFromFiles 从本次上传件里拆内嵌附件，供 land 统一调用（两个通道都经过它）。
//
// mp 已经拆过时**直接复用**那份结果：极简通道在取材阶段必须先知"有没有附件"才能
// 定类型、写提示词，所以它拆得比 land 早。同一份上传件拆两次的结果必然一样，但没
// 必要真跑两次——更要紧的是**保证是同一份**：两处各拆一次、其中一处判据稍有出入，
// 就会出现「提示词里说有 报名表.docx、磁盘上没有」这种最难受的错。
//
// 只处理"手里有原始字节"的上传件（见 rawBytesOf）：Content 被解析服务文本化之后
// 再拿去解 zip/CFB 只会白跑。
func extractFromFiles(files []*UploadedFile, mp *manualPack) []ExtractedAttachment {
	if mp != nil && mp.Extracted != nil {
		return mp.Extracted
	}
	var all []ExtractedAttachment
	for _, uf := range files {
		raw := rawBytesOf(uf)
		if len(raw) == 0 {
			continue
		}
		// 名字用 base：上传件可能带路径（浏览器整体上传目录时），而 Origin 会进
		// meta.json 给管理员看来源，带一串本地路径既没用又像泄露。
		all = append(all, ExtractAttachments(filepath.Base(strings.TrimSpace(uf.Filename)), raw, uf.Content)...)
	}
	return DedupeAttachments(all)
}

// rawBytesOf 取回上传件的原始字节，供附件提取使用。
//
// 优先 uf.Raw：解析成功后回填的就是**原件字节**。但 Raw 为空并不等于「没有原件」——
// 解析服务没配置/没起来时（或这份文档压根不需要走解析），Content 里躺着的就是上传的
// 原始字节（API 层用 string(b) 塞进来的）。
//
// 为什么不看这一段会出真事故：拆内嵌附件是纯 zip/CFB 解包，一步 OCR 都不需要，但
// 提取的入口挂在「解析成功」之后，于是解析服务一挂，**附件型技能就静默退化成查询技能**
// （拆不到附件 → 类型判成 query → 用户要的表格一份也拿不到，而创建过程全程无错）。
//
// 拿 Content 当原始字节之前先确认它"看起来还是二进制"：解析跑过之后 Content 已被替换
// 成正文文本，把几万字正文喂给 zip 解包器只会白跑一遍（还可能误判出「附件」）。
func rawBytesOf(uf *UploadedFile) []byte {
	if uf == nil {
		return nil
	}
	if len(uf.Raw) > 0 {
		return uf.Raw
	}
	if looksBinary(uf.Content) {
		return []byte(uf.Content)
	}
	return nil
}

// resolveAttachRel 把技能元数据里的 attachment 值归一到「技能目录下的相对路径」。
//
// 这个字段有两个来源，取值口径不同：detectType 给的是**裸文件名**（如 `报名表.docx`，
// 历史上落盘在 source/ 下），内嵌附件给的是**已带目录的 rel**（`attachments/报名表.docx`）。
// 不归一的话，`"source/" + x` 会把后者拼成 `source/attachments/报名表.docx`——一个不存在
// 的路径，而 meta.json 里看着挺正常，直到运行时下发才报「文件不存在」。
//
// 顺序很关键：**先查已有前缀，再查登记表，最后才兜底加 source/**。登记表查的是名字，
// 而名字是可能重名的（用户既单独传了个「报名表.docx」，源文件里又内嵌了一个同名的），
// 所以不能一上来就按名字改写——已带目录的值是明确的，优先尊重它。
//
// 兜底加 source/ 是**向后兼容**：老技能（本次改动之前建的）attachment 就是裸文件名，
// 文件也确实在 source/ 下，这批技能不能因为这次改动就下不到文件了。
func resolveAttachRel(a string, regs []model.Attachment) string {
	a = strings.TrimSpace(a)
	switch {
	case a == "":
		return ""
	case strings.HasPrefix(a, AttachDir+"/"), strings.HasPrefix(a, "source/"):
		return a
	}
	// 按名字命中内嵌附件 → 用它的 rel（这样 发我个报名表.docx 就能指向 attachments/ 下那份）
	for _, r := range regs {
		if r.Name == a {
			return r.Rel
		}
	}
	// 精确名字没命中，再试一次「去扩展名同名」：detectType 有可能给出不带扩展名的名字
	// （模型有时把 `报名表.docx` 写成 `报名表`）。只在唯一候选时认，多个候选宁可兜底，
	// 免得两份附件争一个裸名时指向随机那份。
	base := strings.TrimSuffix(a, path.Ext(a))
	var hit string
	n := 0
	for _, r := range regs {
		if strings.TrimSuffix(r.Name, path.Ext(r.Name)) == base {
			hit, n = r.Rel, n+1
		}
	}
	if n == 1 {
		return hit
	}
	return "source/" + a
}

// attachNames 把登记表压成一行 `a.docx(12KB)、b.xlsx(3KB)`，只用于日志。
//
// 大小刻意带上：拆错的时候（把某个压缩流当成了附件）体积会明显不合常理——一张报名表
// 正常几十 KB，日志里写着 8MB 一眼就知道钻错了层。只报名字的话得先 find 去量。
func attachNames(regs []model.Attachment) string {
	parts := make([]string, 0, len(regs))
	for _, r := range regs {
		parts = append(parts, fmt.Sprintf("%s(%s)", r.Rel, humanSize(int(r.Size))))
	}
	return strings.Join(parts, "、")
}

// humanSize 是 size 的粗粒度可读形式。日志用，不做精细换算。
func humanSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// attachCandidates 按容器类型派发提取。depth 是递归层数（见 attachMaxDepth）。
func attachCandidates(raw []byte, depth int) []attachmentCandidate {
	if len(raw) < attachMinBytes || depth > attachMaxDepth {
		return nil
	}
	switch {
	case hasZipMagic(raw):
		return zipAttachCandidates(raw, depth)
	case hasOLEMagic(raw):
		return oleAttachCandidates(raw)
	case hasPDFMagic(raw):
		return pdfAttachCandidates(raw)
	}
	return nil
}

// ===== 魔数 =====

func hasZipMagic(b []byte) bool {
	return len(b) >= 4 && b[0] == 'P' && b[1] == 'K' &&
		(b[2] == 0x03 || b[2] == 0x05 || b[2] == 0x07) && (b[3] == 0x04 || b[3] == 0x06 || b[3] == 0x08)
}

func hasOLEMagic(b []byte) bool {
	return len(b) >= 8 && bytes.Equal(b[:8], []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1})
}

func hasPDFMagic(b []byte) bool {
	return len(b) >= 5 && bytes.HasPrefix(b, []byte("%PDF-"))
}

// ===== OOXML 容器（docx / xlsx / pptx）=====

// embedDirs 是 OOXML 里内嵌对象的存放目录。只认 embeddings：
// word/media/ 是正文插图，不是"附件"，把它拆出来下发属于误解用户意图。
var embedDirs = []struct{ prefix, kind string }{
	{"word/embeddings/", "docx:embeddings"},
	{"xl/embeddings/", "xlsx:embeddings"},
	{"ppt/embeddings/", "pptx:embeddings"},
}

func zipAttachCandidates(raw []byte, depth int) []attachmentCandidate {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil
	}
	var out []attachmentCandidate
	for _, f := range zr.File {
		lower := strings.ToLower(f.Name)
		how := ""
		for _, d := range embedDirs {
			if strings.HasPrefix(lower, d.prefix) {
				how = d.kind
				break
			}
		}
		if how == "" || f.FileInfo().IsDir() {
			continue
		}
		b, err := readZipEntry(f)
		if err != nil || len(b) < attachMinBytes {
			continue
		}
		out = append(out, payloadCandidates(b, f.Name, how, depth)...)
	}
	return out
}

// payloadCandidates 处理「一个内嵌载荷」：它可能自己又是容器（OLE 包 / 直嵌的
// OOXML），那就往里再钻一层；否则就是最终附件。
//
// 为什么这里不用递归到底而是只钻一层：docx 里嵌 docx 再嵌 zip 的形态不存在；
// 无限递归在魔数误判时会变成把一份文件拆成几千个碎片。
func payloadCandidates(b []byte, entryName, how string, depth int) []attachmentCandidate {
	// 内层是 OLE 包：真正的文件字节在 Package/Ole10Native 流里。
	if hasOLEMagic(b) {
		if inner := oleAttachCandidates(b); len(inner) > 0 {
			for i := range inner {
				inner[i].how = how + ">" + inner[i].how
			}
			return inner
		}
		// OLE 里没有可下发的载荷流（例如只存了 WordDocument 本体）→ 放弃，
		// 不把整个 .bin 当附件下发：用户拿到一个打不开的 oleObject1.bin 更糟。
		return nil
	}
	// 内层还是 zip：直嵌的 office 文件（WPS 常见），entry 名往往就是原名。
	if hasZipMagic(b) && depth < attachMaxDepth {
		sub := zipAttachCandidates(b, depth+1)
		if len(sub) > 0 {
			return sub
		}
		return []attachmentCandidate{{raw: b, ext: zipOfficeExt(b), orig: baseName(entryName), how: how + ">embed:office"}}
	}
	ext := sniffOfficeExt(b)
	if ext == "" {
		return nil // 认不出来的二进制不下发：拿了也打不开
	}
	orig := baseName(entryName)
	// .bin / .dat 这类容器占位名不含信息，丢掉让上层用正文清单或「附件N」兜底。
	if s := strings.ToLower(orig); strings.HasSuffix(s, ".bin") || strings.HasSuffix(s, ".dat") {
		orig = ""
	}
	return []attachmentCandidate{{raw: b, ext: ext, orig: orig, how: how}}
}

// ===== 老式 OLE（.doc/.xls/.ppt 本体，或 OOXML 里的 oleObject*.bin）=====

// oleAttachCandidates 遍历 CFB 的所有流，挑出「可下发的载荷流」。
//
// 三条命名口径来自 OLE 的现实：
//   - Package / CONTENTS：Packager 外壳对象（Word「插入对象→由文件创建」不分链接），
//     流里就是那个文件的原始字节；流名不含原名，所以名字交给正文清单兜底。
//   - Ole10Native：从资源管理器拖/复制进来的对象，流首部**自带原始文件名**，
//     顺带把名字捡回来（这是最省事的一路，可惜布局是私有的，只能靠启发式定位）。
//   - 其余（WordDocument / 1Table / Workbook / PowerPoint Document）：那不是附件，
//     是这份文档自己的本体，跳过。
func oleAttachCandidates(raw []byte) []attachmentCandidate {
	r, err := mscfb.New(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	var out []attachmentCandidate
	for f, err := r.Next(); err == nil; f, err = r.Next() {
		if f.FileInfo().IsDir() || f.Size < attachMinBytes {
			continue
		}
		if f.Size > attachMaxBytes {
			log.Printf("[attach] 跳过超大内嵌流 %s（%d 字节，超过 %d 上限）", f.Name, f.Size, attachMaxBytes)
			continue
		}
		name := strings.TrimPrefix(f.Name, "\x01")
		switch {
		case name == "Package" || name == "CONTENTS":
			b, err := readStream(f)
			if err != nil {
				continue
			}
			out = append(out, payloadCandidates(b, "", "ole:"+name, 0)...)
		case strings.Contains(name, "Ole10Native"):
			b, err := readStream(f)
			if err != nil {
				continue
			}
			orig, payload, ok := parseOle10Native(b)
			if !ok {
				continue
			}
			ext := sniffOfficeExt(payload)
			if ext == "" {
				continue
			}
			out = append(out, attachmentCandidate{raw: payload, ext: ext, orig: orig, how: "ole:Ole10Native"})
		}
	}
	return out
}

func readStream(f *mscfb.File) ([]byte, error) {
	b, err := io.ReadAll(f)
	if err != nil {
		log.Printf("[attach] 读取 CFB 流 %s 失败，跳过：%v", f.Name, err)
		return nil, err
	}
	if len(b) < attachMinBytes {
		return nil, io.ErrUnexpectedEOF
	}
	return b, nil
}

// parseOle10Native 从 Ole10Native 流里抠出「原始文件名 + 文件字节」。
//
// 这个流是私有布局（没有公开规范），各区段之间没有可靠的偏移表，唯一稳的锚点是
// 「以一个 NUL 结尾的可打印字符串」和「一个等于剩余长度的 4 字节长度字段」。
// 于是走两条独立的启发式，任一命中即可：
//  1. 名字：扫前 2KB 里所有 NUL 结尾的 ASCII 串，取最后一个以办公扩展名结尾的
//     （前面那些是临时路径，如 C:\Users\x\AppData\Local\Temp\...，最后一个才是原名）；
//  2. 载荷：扫每个偏移，若 u32 == 剩余字节数且紧随其后的内容能嗅探出容器类型 → 命中。
//     取**最后**一个命中：外层还有一个 totalSize 字段也满足长度关系，但它后面跟的是
//     元数据而不是文件头，嗅探会把它挡掉，真载荷在更后面。
//
// 两条启发式都失败就返回 ok=false，绝不把整段流当文件下发。
func parseOle10Native(b []byte) (string, []byte, bool) {
	if len(b) < 16 {
		return "", nil, false
	}
	lim := len(b)
	if lim > 2048 {
		lim = 2048
	}
	// 名字：扫前 2KB 里所有 NUL 结尾的串，取**最后**一个以办公扩展名结尾的
	// （前面那些是临时路径，如 C:\Users\x\AppData\Local\Temp\...，最后一个才是原名）。
	//
	// 「可打印」不能按 ASCII 判定：中文名的字节 ≥ 0x80，一到「报」就断串，于是
	// 「报名表.docx」这种最常见的原名反而取不到。改成把 ≥0x80 的字节也收进串，
	// 但收完要求整串是**合法 UTF-8**——Ole10Native 里用 ANSI/GBK 存的名字解出来
	// 是乱码字节，宁可不要（退回正文清单），也不能把乱码落成文件名下发给用户。
	var orig string
	start := -1
	for i := 0; i < lim; i++ {
		c := b[i]
		if c >= 0x20 && c != 0x7F {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 && c == 0 {
			if s := string(b[start:i]); utf8.ValidString(s) {
				if n := baseName(s); isOfficeName(n) {
					orig = n
				}
			}
		}
		start = -1
	}

	var payload []byte
	for o := 0; o+4 <= len(b); o++ {
		n := int(binary.LittleEndian.Uint32(b[o:]))
		if n < attachMinBytes || o+4+n != len(b) {
			continue
		}
		if ext := sniffOfficeExt(b[o+4:]); ext != "" {
			payload = b[o+4:]
		}
	}
	if payload == nil {
		// 长度字段不可信的兜底：从第一个容器魔数起截到流尾。
		if o := firstMagicOffset(b); o >= 0 && len(b)-o >= attachMinBytes {
			payload = b[o:]
		}
	}
	if payload == nil {
		return "", nil, false
	}
	return orig, payload, true
}

func firstMagicOffset(b []byte) int {
	for i := 0; i+8 <= len(b); i++ {
		if b[i] == 'P' && b[i+1] == 'K' && (b[i+2] == 0x03 || b[i+2] == 0x05) {
			return i
		}
		if i+8 <= len(b) && hasOLEMagic(b[i:]) {
			return i
		}
		if i+5 <= len(b) && bytes.HasPrefix(b[i:], []byte("%PDF-")) {
			return i
		}
	}
	return -1
}

// ===== PDF 内嵌文件 =====

// pdfAttachCandidates 裸扫 PDF 的 /EmbeddedFile 流（附件集 / 公文模板包）。
//
// 不引第三方 PDF 库：需要的只是「找 stream..endstream + 必要时 inflate」，
// 而 PDF 的 xref 表对提取没用（对象在文件里就是明文顺序排列的）。
//
// 已知边界（写在这里防止后人当成 bug 报）：
//   - ObjectStream（/Type /ObjStm，PDF 1.5+ 压缩对象）里的 /EmbeddedFile 扫不到；
//   - 不含 /Filter 的明文流、以及 FlateDecode 两种最常见形态支持，其余 Filter 跳过。
//
// 两种情况都只是"这条 PDF 拆不出附件"，不影响主流程——技能照常生成，只是没有下发件。
func pdfAttachCandidates(raw []byte) []attachmentCandidate {
	var out []attachmentCandidate
	seen := map[string]bool{}
	for off := 0; ; {
		i := bytes.Index(raw[off:], []byte("/EmbeddedFile"))
		if i < 0 {
			break
		}
		i += off
		off = i + len("/EmbeddedFile")

		head := raw[i:]
		if len(head) > 4096 {
			head = head[:4096]
		}
		s := rePDFStream.FindIndex(head)
		if s == nil {
			continue
		}
		dataStart := i + s[1]
		end := bytes.Index(raw[dataStart:], []byte("endstream"))
		if end < 0 {
			continue
		}
		payload := bytes.TrimRight(raw[dataStart:dataStart+end], "\r\n")
		if len(payload) < attachMinBytes {
			continue
		}
		if bytes.Contains(head[:s[0]], []byte("FlateDecode")) {
			if zr, err := zlib.NewReader(bytes.NewReader(payload)); err == nil {
				if inf, err := io.ReadAll(io.LimitReader(zr, int64(attachMaxBytes)+1)); err == nil {
					payload = inf
				}
				zr.Close()
			}
		}
		ext := sniffOfficeExt(payload)
		if ext == "" {
			continue
		}
		key := fmt.Sprintf("%d:%d", len(payload), crc32Key(payload))
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, attachmentCandidate{
			raw:  payload,
			ext:  ext,
			orig: pdfEmbeddedName(raw, i),
			how:  "pdf:EmbeddedFile",
		})
	}
	return out
}

var rePDFStream = regexp.MustCompile(`(?s)\bstream\r?\n`)

// pdfEmbeddedName 在 /EmbeddedFile 之前 2KB 里找 /F (...) 或 /UF <...> 作为文件名。
// 找不到就返回空串（上层退化用正文清单或「附件N」）——不编名字。
func pdfEmbeddedName(raw []byte, pos int) string {
	lo := pos - 2048
	if lo < 0 {
		lo = 0
	}
	head := raw[lo:pos]
	if m := rePDFNameParen.FindSubmatch(head); m != nil {
		return cleanPDFName(string(m[1]))
	}
	if m := rePDFNameHex.FindSubmatch(head); m != nil {
		return cleanPDFName(decodePDFHexName(string(m[1])))
	}
	return ""
}

var (
	rePDFNameParen = regexp.MustCompile(`/(?:UF|F)\s*\(([^)]*)\)`)
	rePDFNameHex   = regexp.MustCompile(`/(?:UF|F)\s*<([0-9A-Fa-f\s]+)>`)
)

// decodePDFHexName 解 PDF 的十六进制名：以 FEFF 开头的是 UTF-16BE（中文名几乎都走这条），
// 否则按单字节处理。
func decodePDFHexName(h string) string {
	h = strings.Join(strings.Fields(h), "")
	if len(h)%2 != 0 {
		h += "0"
	}
	b := make([]byte, 0, len(h)/2)
	for i := 0; i+1 < len(h); i += 2 {
		var v byte
		_, err := fmt.Sscanf(h[i:i+2], "%02x", &v)
		if err != nil {
			return ""
		}
		b = append(b, v)
	}
	if len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF {
		var sb strings.Builder
		for i := 2; i+1 < len(b); i += 2 {
			sb.WriteRune(rune(binary.BigEndian.Uint16(b[i:])))
		}
		return sb.String()
	}
	return string(b)
}

func cleanPDFName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "/")
	if strings.ContainsAny(s, "/\\") {
		s = baseName(s)
	}
	return s
}

// ===== 嗅探 / 命名 =====

// sniffOfficeExt 嗅容器魔数并给出扩展名；认不出来返回空串。
//
// 刻意不返回 "bin" 这种兜底值：调用方用空串表达「不要下发」，
// 而 "bin" 会让用户收到一个双击打不开的文件。
func sniffOfficeExt(b []byte) string {
	switch {
	case hasZipMagic(b):
		return zipOfficeExt(b)
	case hasPDFMagic(b):
		return "pdf"
	case hasOLEMagic(b):
		// 老式 OLE：靠内部流名区分 doc/xls/ppt。区分不出时给 ".doc"——
		// 内网素材以公文（.doc）为绝大多数，且 Word 能打开 xls 的 OLE 时会给出提示，
		// 不会静默损坏。
		return oleOfficeExt(b)
	case bytes.HasPrefix(b, []byte("{\\rtf")):
		return "rtf"
	}
	return ""
}

func oleOfficeExt(b []byte) string {
	r, err := mscfb.New(bytes.NewReader(b))
	if err != nil {
		return "doc"
	}
	for f, err := r.Next(); err == nil; f, err = r.Next() {
		switch strings.TrimPrefix(f.Name, "\x01") {
		case "WordDocument":
			return "doc"
		case "Workbook", "Book":
			return "xls"
		case "PowerPoint Document":
			return "ppt"
		}
	}
	return "doc"
}

// zipOfficeExt 靠 OOXML 的顶层目录名区分 docx/xlsx/pptx；
// 认不出但确实是 zip 的（普通压缩包）返回 "zip"——zip 是用户能打开的东西，可以下发。
func zipOfficeExt(b []byte) string {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return ""
	}
	for _, f := range zr.File {
		switch {
		case strings.HasPrefix(f.Name, "word/"):
			return "docx"
		case strings.HasPrefix(f.Name, "xl/"):
			return "xlsx"
		case strings.HasPrefix(f.Name, "ppt/"):
			return "pptx"
		}
	}
	return "zip"
}

func attachMIME(name string) string {
	if m := mime.TypeByExtension(path.Ext(name)); m != "" {
		return m
	}
	return "application/octet-stream"
}

// attachFileName 把「一个名字线索 + 嗅探出的扩展名」变成安全、唯一的落盘名。
//
// 三件事，顺序不能换：
//  1. 去掉目录部分（正文清单或内嵌名里可能带路径，如 Temp\报名表.docx）；
//  2. 清洗保留字（委托 store.SafeCatFileName，与分类名同一套规则，保留中文）；
//  3. 去重。同名不同内容是常态（正文写着「附件：报名表」，但内嵌了两张不同的报名表），
//     直接覆盖会让用户少收到一张；used 记录已用次数，追加 -2/-3。
func attachFileName(base, ext string, used map[string]int) string {
	if ext == "" {
		ext = "dat"
	}
	base = baseName(strings.TrimSpace(base))
	stem := strings.TrimSuffix(base, path.Ext(base))
	if stem == "" {
		stem = "附件"
	}
	// 名字线索自带的扩展名可能错（.docx 里其实装着 xlsx），以嗅探结果为准。
	stem = store.SafeCatFileName(stem)
	if utf8.RuneCountInString(stem) > 60 {
		stem = string([]rune(stem)[:60])
	}
	name := stem + "." + ext
	if n := used[name]; n > 0 {
		used[name] = n + 1
		name = fmt.Sprintf("%s-%d.%s", stem, n+1, ext)
	}
	used[name]++
	return name
}

func baseName(s string) string {
	s = strings.ReplaceAll(s, "\\", "/")
	s = path.Base(s)
	s = strings.TrimSpace(s)
	if s == "." || s == "/" {
		return ""
	}
	return s
}

// isOfficeName 判断一个名字是不是"像办公文件"，用于从 Ole10Native 的临时路径堆里
// 挑出真文件名。用现成的 isOfficeFile（generator.go），避免两份扩展名表漂移。
func isOfficeName(name string) bool {
	return name != "" && isOfficeFile(name)
}

func readZipEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, int64(attachMaxBytes)+1))
}

func crc32Key(b []byte) uint64 {
	// 只用来在提取阶段去重同一份 PDF 里的重复附件，不是完整性校验。
	var h uint64 = 1469598103934665603
	for _, c := range b {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return h
}

// ===== 正文附件清单解析 =====

// reAttachHead 命中「附件：…」「附 件：…」「附件1：…」「附件 1. …」这类行首。
//
// 为什么要认这几种写法：公文里这四种都常见（尤其「附 件」这种字间空格，
// 是公文排版里为了对齐硬敲出来的），不认就等于把最有信息量的一行漏掉。
var reAttachHead = regexp.MustCompile(`(?m)^[ \t]*附[ \t]*件[ \t]*([0-9]{0,2})[ \t]*[:：.、]?[ \t]*(.*)$`)

// reAttachItem 命中清单里的条目号：1. / 1、 / (1) / （1）
var reAttachItem = regexp.MustCompile(`[(（]?[0-9]{1,2}[)）.、,，][ \t]*`)

// parseAttachmentList 从正文里读「附件清单」。
//
// 只做**可数**的解析，不做语义理解：把「附件：」后面的行拆成条目，清洗成候选名。
// 数量对不对由调用方核对（ExtractAttachments 里那个 len(names) != len(cands) 的判定），
// 这里只管切——切错了顶多让名字退化，不会让内容出错。
func parseAttachmentList(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		m := reAttachHead.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		inline := strings.TrimSpace(m[2])
		items := splitAttachItems(inline)
		if len(items) == 0 {
			// 「附件：」独占一行 → 往下吃连续的条目行，遇到空行或非条目行停。
			for j := i + 1; j < len(lines) && j <= i+12; j++ {
				s := strings.TrimSpace(lines[j])
				if s == "" {
					break
				}
				mm := reAttachItem.FindStringSubmatch(s)
				if mm == nil || mm[0] != s[:len(mm[0])] {
					break
				}
				items = append(items, strings.TrimSpace(s[len(mm[0]):]))
				i = j
			}
		}
		out = append(out, items...)
	}
	// 清洗 + 过滤：名字里带句号的整句（「附件：请于9月30日前报送。」）不是文件名，
	// 留着会生成一个「请于9月30日前报送.docx」——看着像真的，其实是用正文编出来的。
	clean := make([]string, 0, len(out))
	for _, s := range out {
		s = cleanAttachItem(s)
		if s == "" {
			continue
		}
		clean = append(clean, s)
	}
	return clean
}

func splitAttachItems(s string) []string {
	if s == "" {
		return nil
	}
	locs := reAttachItem.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return []string{s}
	}
	var out []string
	if locs[0][0] > 0 {
		out = append(out, s[:locs[0][0]])
	}
	for i, l := range locs {
		end := len(s)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		out = append(out, s[l[1]:end])
	}
	return out
}

// attachRefPhrases 是「引用语」——正文里指路的话，不是文件名。
//
// 判据故意写成「整条就是引用语」或「以少数几个引用语结尾」，而不是「含『见』就丢」：
// 「见证材料汇总表」是正经文件名，误丢就是漏发。两侧代价并不对称——漏发会被用户发现
// （清单写着 3 份、只收到 2 份），而凭空多出一个假名字更糟：用户会当成真附件点开，
// 拿到 404 或空文件，反过头怀疑整套提取是坏的。所以口径偏严，但只严在
// 「整条就是句指路话」这一种形态上。
var attachRefPhrases = map[string]bool{
	"见下": true, "见上": true, "见后": true, "见附件": true, "见正文": true,
	"见附表": true, "见下文": true, "详见附件": true, "详见下文": true, "详见正文": true,
	"附后": true, "后附": true, "同上": true, "同前": true,
	"略": true, "无": true, "-": true, "—": true,
}

func cleanAttachItem(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "　 	·•-—、,，:：;；'\"“”")
	// 掐掉"（一式两份）""〈样式见下〉"这类附注：它是说明，不是名字的一部分。
	for _, cut := range []string{"（", "("} {
		if i := strings.Index(s, cut); i > 0 {
			s = strings.TrimSpace(s[:i])
		}
	}
	if s == "" || utf8.RuneCountInString(s) > 40 {
		return ""
	}
	if strings.ContainsAny(s, "。；;！？!?") {
		return "" // 整句，不是名字
	}
	if attachRefPhrases[s] {
		return ""
	}
	if strings.HasSuffix(s, "见") || strings.HasSuffix(s, "如下") ||
		strings.HasSuffix(s, "附后") || strings.HasSuffix(s, "见附件") ||
		strings.HasSuffix(s, "详见附件") {
		return ""
	}
	return s
}
