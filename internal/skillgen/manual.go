package skillgen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// 本文件是「写作手册 → 分类化 skill」流水线的接线层，负责把 categories.go 里的
// 纯函数（ExtractStructure / SplitByAnchors / WriteCategories）接到 generator 的
// 8 步主干上，并补上两个旧流水线缺失的环节：
//
//	① 素材入库（ingest）：创建技能时上传的 PDF/docx 以前是**原始字节直接塞进
//	   Content**，等于把二进制当文本喂给 LLM（乱码）。ocrd 只在「给已有技能传文件」
//	   这条通道上被调用过。这里把两条通道对齐：凡二进制文档一律先过 ocrd 抽取文本。
//	② 手册识别（buildManual）：素材能不能当手册处理，靠「抽出的分类数」判定，
//	   而不是靠管理员勾选项——约定优于配置。抽不出结构就自动退回旧的通用流程，
//	   这样既有 6 个技能的行为一个都不会变。
//
// 判定为手册后，范文不再由 LLM 编造（旧 buildExamples 的 `生成一篇 400-900 字
// 示例范文`），而是按锚点在原文里逐字切出来，保真度可机械验证。

// manualMinCategories 是「素材算不算手册」的门槛。
// 取 2 而不是 1：只抽出一个分类说明模型很可能把整份材料当成了单一文体，
// 此时走通用流程更诚实；而 2 类以上基本可以确认素材内部有分类体系。
const manualMinCategories = 2

// docExts 是允许送去 ocrd 抽取的文档扩展名（与 api 层 extractableExt 保持一致）。
// ocrd 支持 pdf|docx|xlsx|pptx|txt|md|csv|html，这里只列真正需要"解析"的；
// .txt/.md/.csv 本身就是纯文本，没必要绕一圈微服务。
var docExts = map[string]bool{
	".pdf": true, ".docx": true, ".doc": true, ".xlsx": true, ".xls": true,
	".pptx": true, ".ppt": true, ".docm": true, ".xlsm": true, ".pptm": true,
	".htm": true, ".html": true,
}

// sniffSegLen 是二进制判定里单个采样段的字节数。
// 取 4KB：四段合计 16KB，相对动辄 17MB 的原始素材可以忽略，但足够让压缩流现形。
const sniffSegLen = 4096

// binaryMinHits 是统计判据的**绝对量门槛**：计数不到这个数就不看占比。
// 存在的理由是占比在短样本上会骗人——「二\x00二六年三月」9 个 rune 里 1 个 NUL
// 就已经 11%。而真实二进制样本的非法字节是成片的，4KB 压缩流能到 3000+。
// 取 8：比任何 OCR 文本的噪声水平都高，又远低于真实二进制的量级。
const binaryMinHits = 8

// binaryMagic 是「一眼二进制」的文件魔数，判据的第一层：命中即二进制，零歧义。
//
// 为什么必须补这一层（线上事故）：旧判据只看「控制字符占比」，且只采前 8192 字节。
// 而 PDF/OOXML 这类容器格式的**头部是纯 ASCII**（%PDF-1.7 + 未压缩的对象表），
// 实测 17MB 的 manual-vector.pdf 前 8192 字节控制字符占比 0.0000% → 被判成文本
// → 跳过 OCR → 17MB 原始字节当「原文」灌进 LLM（provider 报 26 万 token 超限，
// 整条手册流水线静默降级）。扫描版因为字节分布随机恰好能被旧判据挡住，于是
// 症状表现为「扫描件能跑通、vector 版炸掉」，极难定位。魔数是唯一的确定信号。
var binaryMagic = [][]byte{
	[]byte("%PDF-"),
	[]byte("PK\x03\x04"), []byte("PK\x05\x06"), []byte("PK\x07\x08"), // zip / docx / xlsx / pptx
	[]byte("\xD0\xCF\x11\xE0\xA1\xB1\x1A\xE1"), // OLE2：老式 doc/xls/ppt
	[]byte("\x1F\x8B"), []byte("BZh"), []byte("\xFD7zXZ\x00"), []byte("\x28\xB5\x2F\xFD"), // gzip/bz2/xz/zstd
	[]byte("\x89PNG\r\n\x1a\n"), []byte("\xFF\xD8\xFF"), []byte("GIF87a"), []byte("GIF89a"),
	[]byte("II*\x00"), []byte("MM\x00*"), []byte("RIFF"), []byte("OggS"),
	[]byte("\x7FELF"), []byte("SQLite format 3\x00"),
}

// looksBinary 判断一段内容是不是二进制。
//
// 判据按可靠性从高到低三层，任一层命中即判二进制：
//  1. 文件魔数——容器格式头部是 ASCII，旧判据在这里必然漏判；
//  2. 多段采样——只采头部不够，压缩流集中在中后段；
//  3. 段内统计——非法 UTF-8 / 控制字符 / NUL 三项占比（见 segBinary）。
//
// 不用扩展名判断：OCR 成功后 Content 里装的已是文本、但 Filename 还叫 xxx.pdf，
// 按扩展名会把刚抽好的文本又当二进制丢掉。判内容才是唯一可靠的口径。
func looksBinary(s string) bool {
	if s == "" {
		return false
	}
	for _, m := range binaryMagic {
		if strings.HasPrefix(s, string(m)) {
			return true
		}
	}
	for _, seg := range sniffSegments(s) {
		if segBinary(seg) {
			return true
		}
	}
	return false
}

// sniffSegments 取「头部 / 1/3 / 2/3 / 尾部」四段定长采样，去重后返回。
//
// 固定四段而不是随机采样：判定结果必须可复现——同一份素材两次训练给出不同结论
// 会让「为什么这次没按手册处理」变成玄学。四段覆盖足以命中容器格式的压缩流。
func sniffSegments(s string) []string {
	n := len(s)
	if n <= sniffSegLen {
		return []string{s}
	}
	offs := []int{0, n/3 - sniffSegLen/2, 2*n/3 - sniffSegLen/2, n - sniffSegLen}
	var out []string
	seen := make(map[int]bool, len(offs))
	for _, off := range offs {
		if off < 0 {
			off = 0
		}
		if off+sniffSegLen > n {
			off = n - sniffSegLen
		}
		if seen[off] {
			continue
		}
		seen[off] = true
		out = append(out, s[off:off+sniffSegLen])
	}
	return out
}

// segBinary 对单个采样段做统计判定。
//
// 三个判据的阈值都定得很松，因为这里的**误判代价不对称**：
//   - 把文本误判成二进制 → 素材被丢弃、后续报「原文为空」，是响亮的失败；
//   - 把二进制误判成文本 → 17MB 乱码进 LLM，是静默的污染（正是本次事故）。
//
// 所以宁可多判二进制。但有两处必须克制，否则会「修一个 bug 造一个 bug」：
//
//  1. NUL 不能用「出现一个就判」——OCR 产物会夹带零星 NUL（实测手册原文里就有
//     「二\x00二六年三月」），一扫就判会把自己的 OCR 结果当二进制挡掉。
//  2. 三项判据都必须**绝对量 + 占比双条件**——占比在短样本上不可信：
//     「二\x00二六年三月」只有 9 个 rune，一个 NUL 就把占比推到 11%，
//     单看占比必然误判（这条是实测被测试抓出来的，不是推演）。
//     真实二进制样本里非法字节是成片的（4KB 压缩流能到 3000+），
//     所以「绝对量 ≥ binaryMinHits」既挡得住噪声，又拦得住真二进制。
func segBinary(seg string) bool {
	if seg == "" {
		return false
	}
	var total, ctrl, nul, invalid int
	for i := 0; i < len(seg); {
		r, size := utf8.DecodeRuneInString(seg[i:])
		i += size
		total++
		if r == utf8.RuneError && size == 1 {
			invalid++ // 非法 UTF-8 序列：二进制最稳的信号
			continue
		}
		if r == 0 {
			nul++
			continue
		}
		if r == '	' || r == '\n' || r == '\r' {
			continue // 允许的空白
		}
		if r < 0x20 || r == 0x7f {
			ctrl++
		}
	}
	if total == 0 {
		return false
	}
	ratio := func(n int) float64 { return float64(n) / float64(total) }
	switch {
	case invalid >= binaryMinHits && ratio(invalid) > 0.02:
		return true
	case ctrl >= binaryMinHits && ratio(ctrl) > 0.10:
		return true
	case nul >= binaryMinHits && ratio(nul) > 0.005:
		return true
	}
	return false
}

// isDocFile 判断文件名是否需要走 OCR 解析。
func isDocFile(name string) bool {
	return docExts[strings.ToLower(filepath.Ext(name))]
}

// SetOCR 注入 ocrd 微服务地址（http://127.0.0.1:8093）。为空则跳过二进制抽取。
func (g *Generator) SetOCR(url string) { g.ocrURL = url }

// ocrExtract 调用 ocrd 的 /extract 接口把文档解析成纯文本。
// 字段名与 api 层 extractDoc 保持一致（form 字段 "file"），避免两个调用方
// 对同一个微服务用两套协议。
func ocrExtract(ctx context.Context, ocrURL, filename string, data []byte) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	name := filepath.Base(filename)
	if name == "" || name == "." {
		name = "scan"
	}
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(data); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(ocrURL, "/")+"/extract", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	// 50 页扫描件实测约 40-60s（RapidOCR 逐页推理），留足余量。
	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return "", err
	}
	var out struct {
		OK    bool   `json:"ok"`
		Text  string `json:"text"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("解析服务返回无法识别: %w", err)
	}
	if !out.OK {
		return "", fmt.Errorf("解析服务: %s", out.Error)
	}
	return strings.TrimSpace(out.Text), nil
}

// ingestFiles 把上传的参考文件规整成「文本化」的素材：二进制文档先过 ocrd，
// 抽出的文本替换进 Content，原始字节留在 Raw 里供 source/ 留档。
//
// 幂等：Content 已是文本（管理员直接传 .txt/.md，或二次运行时）则原样跳过，
// 不会重复消耗 OCR 算力。
func (g *Generator) ingestFiles(ctx context.Context, in *Input, steps func(string)) {
	if in == nil || len(in.Files) == 0 {
		return
	}
	for _, uf := range in.Files {
		if uf == nil {
			continue
		}
		fn := filepath.Base(strings.TrimSpace(uf.Filename))
		if !isDocFile(fn) {
			continue
		}
		if !looksBinary(uf.Content) {
			continue // 已经是文本（或上次已抽过），无需再解析
		}
		if g.ocrURL == "" {
			if steps != nil {
				steps(fmt.Sprintf("⚠️ %s 是二进制文档，但未配置解析服务，无法作为参考素材", fn))
			}
			continue
		}
		if steps != nil {
			steps("正在解析上传的文档（" + fn + "，扫描件逐页 OCR，可能需要数十秒）…")
		}
		start := time.Now()
		raw := []byte(uf.Content)
		text, err := ocrExtract(ctx, g.ocrURL, fn, raw)
		if err != nil {
			if steps != nil {
				steps(fmt.Sprintf("⚠️ %s 解析失败：%v", fn, err))
			}
			continue
		}
		if strings.TrimSpace(text) == "" {
			if steps != nil {
				steps(fmt.Sprintf("⚠️ %s 解析结果为空，已跳过", fn))
			}
			continue
		}
		uf.Raw = raw
		uf.Content = text
		uf.Extracted = true
		if steps != nil {
			steps(fmt.Sprintf("%s 解析完成：%d 字符（%.1fs）", fn, len([]rune(text)), time.Since(start).Seconds()))
		}
	}
}

// manualSourceText 把内存里的素材拼成一份原文，供锚点定位使用。
//
// 与 categories.go 的 SourceText(磁盘版) 规则保持一致（按文件名排序、跳过二进制、
// 跳过隐藏文件），差异只在数据来源：生成阶段 source/ 还没落盘，只能用内存里的
// Content。排序是必须的——SplitByAnchors 依赖"原文自然前后顺序"做顺序校验，
// map 遍历顺序会让它随机误报。
func manualSourceText(in *Input) string {
	if in == nil {
		return ""
	}
	type ent struct{ name, text string }
	var ents []ent
	for _, f := range in.Files {
		if f == nil {
			continue
		}
		name := filepath.Base(strings.TrimSpace(f.Filename))
		if name == "" || name == "." {
			name = "unnamed.txt"
		}
		if strings.HasPrefix(name, ".") {
			continue // 隐藏文件不是素材
		}
		if strings.TrimSpace(f.Content) == "" {
			continue
		}
		// 未成功抽取的二进制混进原文会污染锚点定位（满屏乱码），必须挡掉。
		if looksBinary(f.Content) {
			continue
		}
		ents = append(ents, ent{name, f.Content})
	}
	sort.SliceStable(ents, func(i, j int) bool { return ents[i].name < ents[j].name })
	var b strings.Builder
	for _, e := range ents {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(e.text)
	}
	return b.String()
}

// manualPack 是一次「手册识别」的全部产物，在内存里流转到 land 阶段再落盘。
//
// 为什么先攒在内存：识别失败（抽不出分类/锚点全丢）时不该在磁盘上留下半个
// 技能目录。先判定成功、再落盘，失败就是零副作用。
type manualPack struct {
	Structure *Structure
	// Examples 的 key 是**原始类别名**（不是清洗后的文件名），与 WriteCategories
	// 取值的口径对齐；value 是逐字切出的范文正文。
	Examples map[string][]string
	// Paths 是同上口径的范文落盘相对路径，供 categories/NN-x.md 里写「参考范文」链接。
	Paths    map[string][]string
	Reviewer string
	Warnings []string
	// Source 是切出范文用的原文（OCR 后的手册全文）。只为 fidelity.md 的保真
	// 核对服务——「这篇范文确实能在原文里逐字找到」这句结论需要原文在手。
	Source string
}

// ExampleCount 返回成功切出的范文总数。
// 带 nil 接收者检查：调用点（generator 四处统计）在「非手册模式」下 mp 就是 nil，
// 让调用方每次写 `if mp != nil` 只会漏、不会少，不如让零值语义天然正确。
func (mp *manualPack) ExampleCount() int {
	if mp == nil {
		return 0
	}
	n := 0
	for _, v := range mp.Examples {
		n += len(v)
	}
	return n
}

// CategoryNames 按手册顺序返回类别名，供 trace 展示。
func (mp *manualPack) CategoryNames() []string {
	if mp == nil || mp.Structure == nil {
		return nil
	}
	out := make([]string, 0, len(mp.Structure.Categories))
	for _, c := range mp.Structure.Categories {
		out = append(out, c.Name)
	}
	return out
}

// buildManual 尝试把素材当写作手册处理：抽结构 → 按锚点原样切范文。
//
// 返回 error 表示「这不是一份可用的手册素材」，调用方应退回通用流程——
// 这是设计上的降级路径，不是故障。所有 error 都会被 generator 记进 trace。
func (g *Generator) buildManual(ctx context.Context, in *Input) (*manualPack, error) {
	src := manualSourceText(in)
	if strings.TrimSpace(src) == "" {
		return nil, errors.New("没有可用的文本素材（二进制文档可能未成功解析）")
	}
	if g.llm == nil {
		return nil, errors.New("未配置 LLM，无法抽取手册结构")
	}
	st, err := ExtractStructure(ctx, g.llm, src)
	if err != nil {
		return nil, fmt.Errorf("结构抽取失败: %w", err)
	}
	if st == nil || len(st.Categories) < manualMinCategories {
		n := 0
		if st != nil {
			n = len(st.Categories)
		}
		return nil, fmt.Errorf("只抽出 %d 个分类（需要 ≥%d），按通用素材处理", n, manualMinCategories)
	}

	mp := &manualPack{
		Structure: st,
		Examples:  map[string][]string{},
		Paths:     map[string][]string{},
		Source:    src,
	}
	for _, c := range st.Categories {
		segs, err := SplitByAnchors(src, c.Anchor)
		if err != nil {
			// 单个分类切不准不算致命：其余分类仍然可用，问题记进 Warnings
			// 让人能在 UI 上看到「这一类的范文没切出来」而不是整体失败。
			mp.Warnings = append(mp.Warnings, fmt.Sprintf("分类「%s」范文未切出：%v", c.Name, err))
			continue
		}
		mp.Examples[c.Name] = segs
	}
	if mp.ExampleCount() == 0 {
		return nil, errors.New("所有分类的范文锚点都定位失败，按通用素材处理")
	}
	return mp, nil
}

// WriteTo 把手册产物落盘：examples/<类别名>/NN.md（原文片段）、categories/*.md、
// reviewer.md。
//
// 落盘顺序有意为之：先写范文，再让 WriteCategories 拿到范文相对路径——因为
// categories/NN-x.md 里要列出「参考范文」的链接，路径必须已经确定。
func (mp *manualPack) WriteTo(dir string) ([]string, error) {
	if mp == nil || mp.Structure == nil {
		return nil, errors.New("WriteTo: 手册产物为空")
	}
	var written []string
	for _, c := range mp.Structure.Categories {
		segs := mp.Examples[c.Name]
		if len(segs) == 0 {
			continue
		}
		// 目录名复用 safeCatFileName，保证与 categories/NN-<name>.md 用的是
		// 同一套清洗规则；否则「分类 A/B」这类名字会让两边指向不同目录。
		sub := safeCatFileName(c.Name)
		subDir := filepath.Join(dir, "examples", sub)
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			return written, fmt.Errorf("创建范文目录失败: %w", err)
		}
		for i, seg := range segs {
			fn := fmt.Sprintf("%02d.md", i+1)
			if err := os.WriteFile(filepath.Join(subDir, fn), []byte(seg), 0o644); err != nil {
				return written, fmt.Errorf("写入范文 %s/%s 失败: %w", sub, fn, err)
			}
			rel := filepath.ToSlash(filepath.Join("examples", sub, fn))
			mp.Paths[c.Name] = append(mp.Paths[c.Name], rel)
			written = append(written, rel)
		}
	}
	catFiles, err := WriteCategories(dir, mp.Structure, mp.Paths)
	if err != nil {
		return written, err
	}
	written = append(written, catFiles...)

	if strings.TrimSpace(mp.Reviewer) != "" {
		if err := os.WriteFile(filepath.Join(dir, "reviewer.md"), []byte(mp.Reviewer), 0o644); err != nil {
			return written, fmt.Errorf("写入 reviewer.md 失败: %w", err)
		}
		written = append(written, "reviewer.md")
	}
	// fidelity.md：机器产出的质量报告，左树挂在只读的「质量报告」分组下。
	//
	// 为什么必须落盘（契约 docs/writing-skill-pipeline-design.md 3.2 步骤 4）：
	// 切分失败此前只进 meta.json 的 warnings 字段 + 一句一闪而过的 SSE step。
	// meta.json 管理员在左树里根本看不见，于是「12 类里 7 类范文没切出来」变成
	// 静默降级——技能照建照注册，用户拿到的是个残缺技能却毫不知情（实测事故）。
	// 写成只读文件是把「降级」变成「看得见的证据」。
	if err := mp.writeFidelity(dir); err != nil {
		return written, err
	}
	written = append(written, "fidelity.md")
	return written, nil
}

// writeFidelity 写 fidelity.md：把范文覆盖情况与切分失败原因摊开成一份人可读的
// 质量报告。内容刻意用朴素 Markdown，因为管理员可能直接编辑别的文件、顺手看它。
func (mp *manualPack) writeFidelity(dir string) error {
	var b strings.Builder
	b.WriteString("# 范文保真报告（机器生成 · 只读）\n\n")
	fmt.Fprintf(&b, "- 生成时间：%s\n", time.Now().Format("2006-01-02 15:04:05"))

	total := len(mp.Structure.Categories)
	covered := 0
	for _, c := range mp.Structure.Categories {
		if len(mp.Examples[c.Name]) > 0 {
			covered++
		}
	}
	fmt.Fprintf(&b, "- 手册分类：%d 个\n", total)
	fmt.Fprintf(&b, "- 范文覆盖：%d/%d 类，共 %d 篇\n", covered, total, mp.ExampleCount())

	// 保真核对是这份报告的立身之本：范文必须是原文连续子串。这一条是机械可验的，
	// 所以直接报数字，而不是写「已确保保真」这种无法证伪的话。
	if found, checked := mp.countFidelity(); checked > 0 {
		fmt.Fprintf(&b, "- 保真核对：%d/%d 篇可在 source/ 原文中逐字找到（未经模型改写）\n", found, checked)
	}

	if len(mp.Warnings) == 0 {
		b.WriteString("\n## 结论\n\n全部类别的范文均已按锚点从原文切出，无待处理项。\n")
	} else {
		fmt.Fprintf(&b, "\n## ⚠️ 待处理：%d 类范文未切出\n\n", len(mp.Warnings))
		for _, w := range mp.Warnings {
			fmt.Fprintf(&b, "- %s\n", w)
		}
		b.WriteString("\n这类失败通常不是手册的问题，而是模型摘录锚点时改动了原文" +
			"（吞掉换行、替换标点、把长句截断后加「…」）。已切出的分类不受影响；" +
			"未切出的分类在运行时不会注入范文。可打开 categories/ 下对应文件核对该类要求。\n")
	}
	return os.WriteFile(filepath.Join(dir, "fidelity.md"), []byte(b.String()), 0o644)
}

// countFidelity 逐篇核对范文是否为 source 原文的连续子串，返回（命中数, 总数）。
// Source 为空时返回 (0,0)，让调用方知道「没核对过」而不是「核对全过」——
// 后者会让报告谎报保真。
func (mp *manualPack) countFidelity() (found, total int) {
	if mp.Source == "" {
		return 0, 0
	}
	for _, segs := range mp.Examples {
		for _, s := range segs {
			total++
			if strings.Contains(mp.Source, s) {
				found++
			}
		}
	}
	return found, total
}

// augmentPromptForManual 把「分类路由 + 工作协议」追加进 system_prompt。
//
// 为什么嵌进 system_prompt 而不是只靠运行时注入：技能要能独立运行（用户直接
// 把 skill 目录拷走、或在别的 agent 里用）。system_prompt 里放**轻量路由表**
// （分类名 + 触发场景），重的分类要求与范文留给运行时按需注入——全都塞进
// prompt 会让每次对话都背上整本手册的 token。
//
// 协议第 4 条「拿不准就问」是本设计的核心诉求：以前把不确定的点自行猜掉，
// 是这类生成技能最让人不信任的地方。
func augmentPromptForManual(sysPrompt string, st *Structure) string {
	if st == nil || len(st.Categories) == 0 {
		return sysPrompt
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(sysPrompt, "\n"))
	b.WriteString("\n\n---\n\n## 写作手册模式\n\n")
	b.WriteString("本技能由一份写作手册训练而成，手册把文章分为若干类，每类有各自的写作要求与真实范文。\n\n")
	b.WriteString("### 第一步：判定分类\n\n")
	b.WriteString("先判断本次需求属于下面哪一类。**判不准、或同时符合多类时，先问用户，不要猜。**\n\n")
	b.WriteString("| 分类 | 触发场景 |\n| --- | --- |\n")
	for _, c := range st.Categories {
		b.WriteString("| " + mdCell(c.Name) + " | " + mdCell(c.Trigger) + " |\n")
	}
	b.WriteString("\n### 第二步：取该类的要求与范文\n\n")
	b.WriteString("1. 打开 `categories/` 下该类对应的文件，其「写作要求」小节是**硬约束**，逐条遵守，不得取舍。\n")
	b.WriteString("2. 同类文件的「参考范文」小节列出了手册里的真实范文（位于 `examples/<分类>/`）。范文可作为结构、措辞的参照；\n")
	b.WriteString("   其中与本次主体无关的事实**不得搬用**，事实只能来自用户提供的素材。\n\n")
	b.WriteString("### 第三步：写完自检\n\n")
	b.WriteString("对照 `reviewer.md` 的检查项逐条核对，任一条不满足就改到达标再交付。\n\n")
	b.WriteString("### 第四步：拿不准就问（强制）\n\n")
	b.WriteString("出现下列任一情况，**必须先向用户提问、等确认后再写**，不要自行决定，也不要写一半交差：\n\n")
	b.WriteString("- 分类无法确定；\n")
	b.WriteString("- 素材缺少该类要求的必备要素（如标题所需的主体名称、正文所需的数据/时间）；\n")
	b.WriteString("- 用户的要求与手册要求冲突（例如要求 800 字、手册规定不超过 500 字）——指出冲突并请用户拍板；\n")
	b.WriteString("- 需要引用事实、但素材里没有依据。\n")
	return b.String()
}

// buildReviewer 从手册的总则与各类「写作要求」里提炼审稿清单。
//
// 与范文不同，审稿清单**允许由 LLM 改写**：它的价值在「可判定」而不在「原文照抄」，
// 手册里常见的「语言要生动」这种没法执行的话，恰恰需要被转写成「是否使用具体动词、
// 有无空洞形容词」才能用。代价是可能掺进手册里没有的要求，所以 prompt 里把
// 「不得新增手册没有的要求」写成硬约束，并要求每条标注来源。
func (g *Generator) buildReviewer(ctx context.Context, st *Structure) (string, error) {
	if st == nil || len(st.Categories) == 0 {
		return "", errors.New("结构为空")
	}
	var src strings.Builder
	if strings.TrimSpace(st.General) != "" {
		src.WriteString("## 总则（原文）\n" + st.General + "\n\n")
	}
	src.WriteString("## 各分类写作要求（原文）\n")
	for _, c := range st.Categories {
		src.WriteString("### " + c.Name + "\n" + strings.TrimSpace(c.Requirement) + "\n\n")
	}

	sys := `你是审稿标准的提炼器，不是写作老师。请从给定的写作手册原文中提取可执行的审稿检查项，输出 Markdown。

硬性要求：
1. 每一条必须是能用「是 / 否」判定的规则，例如「标题不超过 22 字」「首段出现主体名称与核心动作」。
   遇到「语言要生动」「内容要充实」这类无法判定的原话，要么改写成可判定的形式（如「正文使用具体动词，无空洞形容词堆砌」），要么直接舍弃。
2. 不得新增手册里没有的要求。每条规则都必须能在给定原文里找到依据。
3. 分两级组织：「## 一、通用检查（每篇必查）」与「## 二、分类专属检查」，后者用「### 分类名」小标题。
4. 每条以「- [ ] 」开头，行尾用「（依据：分类名）」或「（依据：总则）」标注来源。
5. 只输出 Markdown 正文，不要任何解释性开场白与结尾。`
	user := "手册原文：\n\n" + src.String()

	out, err := g.llm.Chat(ctx, sys, user)
	if err != nil {
		return "", err
	}
	return cleanCodeFence(out), nil
}
