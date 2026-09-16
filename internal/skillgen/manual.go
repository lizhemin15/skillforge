package skillgen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/lizhemin15/skillforge/internal/ocrsvc"
	"github.com/lizhemin15/skillforge/internal/tlsconf"
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
	[]byte("\xD0\xCF\x11\xE0\xA1\xB1\x1A\xE1"),                                            // OLE2：老式 doc/xls/ppt
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

// needsDocParse 判断这次训练的素材里有没有「必须过解析服务」的二进制文档。
// 只有存在时才值得做依赖预检：纯文本素材的训练不该为探活白等一次 HTTP。
func needsDocParse(in *Input) bool {
	if in == nil {
		return false
	}
	for _, uf := range in.Files {
		if uf == nil {
			continue
		}
		if !isDocFile(filepath.Base(strings.TrimSpace(uf.Filename))) {
			continue
		}
		if looksBinary(uf.Content) {
			return true
		}
	}
	return false
}

// isDocFile 判断文件名是否需要走 OCR 解析。
func isDocFile(name string) bool {
	return docExts[strings.ToLower(filepath.Ext(name))]
}

// SetOCR 注入 ocrd 微服务地址（http://127.0.0.1:8093）。为空则跳过二进制抽取。
func (g *Generator) SetOCR(url string) { g.ocrURL = url }

// SetOCRTimeout 覆盖单次文档解析的客户端超时（<=0 表示回到 DefaultOCRTimeout）。
func (g *Generator) SetOCRTimeout(d time.Duration) { g.ocrTimeout = d }

// OCRTimeout 返回当前生效的解析超时（默认值也在此fold入）。
// 存在的意义：上层要能自检「环境变量到底吃进去没有」，而不是只能靠观察一次真实解析。
func (g *Generator) OCRTimeout() time.Duration { return g.ocrTimeoutOrDefault() }

// DefaultOCRTimeout 是单次文档解析的客户端超时默认值。
//
// 实测（4 核 / dpi=200）：13.6MB、50 页的扫描件 manual-scan-lite.pdf 需要 397.5s
// 才能出文本。旧实现硬编码 300s，比真实耗时还短——扫描件必然超时，训练随即
// 静默退回通用流程（8.5 裁判层根本没机会执行）。这里取实测值的 4 倍余量，
// 给页数更多 / dpi 更高的手册留空间；再多就用 SKILLFORGE_OCR_TIMEOUT 调。
const DefaultOCRTimeout = 30 * time.Minute

// ocrProgressInterval 是长解析期间回报进度的间隔。
// 变量而非常量：测试要把它压到毫秒级，避免真的等 30s。
var ocrProgressInterval = 30 * time.Second

// ocrClient 返回调用解析服务用的 HTTP 客户端。
// 超时是**整体**上限（含逐页推理），不是连接超时——别改小。
func (g *Generator) ocrClient() *http.Client {
	d := g.ocrTimeout
	if d <= 0 {
		d = DefaultOCRTimeout
	}
	// 解析服务通常在本机（http://127.0.0.1:8093），但也可以指到内网另一台机器；
	// 那种部署下多半是自签证书，所以照样走本机信任配置。
	return tlsconf.NewClient(d)
}

// ocrProgress 在解析期间周期回报「还在跑、已等待多久」，避免前端看起来像卡死。
//
// 返回的 stop 必须在解析返回后**立刻**调用：它停掉心跳并等待心跳 goroutine 退出。
// 少了这一步，goroutine 可能在 HTTP handler 返回之后还在往 ResponseWriter 写。
// stop 幂等（内部 sync.Once）：调用点写成 defer 再显式调用一次也不会炸。
func ocrProgress(steps func(string), fn string, start time.Time) func() {
	if steps == nil {
		return func() {}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(ocrProgressInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				steps(fmt.Sprintf("%s 解析中，已等待 %s…", fn, humanDuration(time.Since(start))))
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			wg.Wait()
		})
	}
}

// ocrTimeoutOrDefault 返回当前生效的解析超时上限（用于给人看的报错文案）。
func (g *Generator) ocrTimeoutOrDefault() time.Duration {
	if g.ocrTimeout > 0 {
		return g.ocrTimeout
	}
	return DefaultOCRTimeout
}

// isTimeoutErr 区分「超时」和「其他失败」：只有超时才需要在流里解释降级后果。
// 注意 context.Canceled（用户关页面）不算超时，不该弹那句提示。
func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// humanDuration 把耗时说成人话（中文流水线里给用户看的）。
func humanDuration(d time.Duration) string {
	sec := int(d.Seconds() + 0.5)
	if sec < 60 {
		return fmt.Sprintf("%d 秒", sec)
	}
	return fmt.Sprintf("%d 分 %d 秒", sec/60, sec%60)
}

// ocrResult ocrd /extract 的解析结果。
//
// Warning/Stats 是「素材可信度」诊断，为什么必须一路传到上层：
// 线上事故是混合型 PDF（前几页扫描 + 后几页可选）只解析出 1129 字符的水印，
// 字符数非 0 ⇒ 全链路绿灯 ⇒ 生成了一份和素材毫无关系的技能。**字符数分辨不出
// 「正文」和「水印」**，只有服务端的逐页统计能分辨，所以不能被丢在这一层。
type ocrResult struct {
	Text    string
	Warning string         // 「有 N/M 页没识别出文字」这类可据以决策的提示
	Stats   map[string]any // 逐页统计（pages/text_pages/ocr_pages/empty_pages），非 PDF 为空
}

// ocrExtract 调用 ocrd 的 /extract 接口把文档解析成纯文本。
// 字段名与 api 层 extractDoc 保持一致（form 字段 "file"），避免两个调用方
// 对同一个微服务用两套协议。
func ocrExtract(ctx context.Context, ocrURL, filename string, data []byte, client *http.Client) (ocrResult, error) {
	var res ocrResult
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	name := filepath.Base(filename)
	if name == "" || name == "." {
		name = "scan"
	}
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		return res, err
	}
	if _, err := fw.Write(data); err != nil {
		return res, err
	}
	if err := mw.Close(); err != nil {
		return res, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(ocrURL, "/")+"/extract", &buf)
	if err != nil {
		return res, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	// 超时由调用方注入（默认 30 分钟，见 DefaultOCRTimeout）。这里坚决不写死数值：
	// 旧实现写死 300s，而真实扫描件要 397.5s，导致解析必然失败却只在流里留一行 ⚠️。
	resp, err := client.Do(req)
	if err != nil {
		// 连不上 = 环境问题（服务没在运行），不是这份文件有问题。
		// 翻译成「照着敲就能修」的中文；原始错误仍由 Unwrap 保留，排障不受影响。
		return res, ocrsvc.Explain(err, ocrURL)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return res, err
	}
	var out struct {
		OK      bool           `json:"ok"`
		Text    string         `json:"text"`
		Error   string         `json:"error"`
		Warning string         `json:"warning"`
		Stats   map[string]any `json:"stats"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return res, fmt.Errorf("解析服务返回无法识别: %w", err)
	}
	// 服务自报「运行时损坏、正在自动重启」：与「这份文件解析不了」是两码事——
	// 文件没问题，约 10 秒后重试就成功。不识别它，用户看到的是内部错误文案，
	// 然后就再也不会重试了。
	if ocrsvc.SelfHealing(body) {
		return res, errors.New(ocrsvc.SelfHealingMessage(ocrURL, out.Error))
	}
	if !out.OK {
		return res, fmt.Errorf("解析服务: %s", out.Error)
	}
	res.Text = strings.TrimSpace(out.Text)
	res.Warning = strings.TrimSpace(out.Warning)
	res.Stats = out.Stats
	return res, nil
}

// statsLine 把逐页统计压成一行人能读的话，写进训练进度流。
// 老版本不看统计，用户只能看到「解析完成：1129 字符」——一个数字，看不出那是水印。
func statsLine(stats map[string]any) string {
	if len(stats) == 0 {
		return ""
	}
	num := func(k string) int { return statsInt(stats, k) }
	pages := num("pages")
	if pages == 0 {
		return ""
	}
	s := fmt.Sprintf("共 %d 页（文本层直取 %d / OCR %d", pages, num("text_pages"), num("ocr_pages"))
	if e := num("empty_pages"); e > 0 {
		s += fmt.Sprintf(" / 空白 %d", e)
	}
	return s + "）"
}

// extractionJudgement 对一次解析结果的可信度判定（结构化，不靠关键字）。
type extractionJudgement struct {
	pages   int  // 该文档页数（非 PDF / 无统计时为 0）
	suspect bool // 文本非空但等于没读到正文
}

// judgeExtraction 依据逐页统计判断「这份文档到底读出来没有」。
//
// 判据（任一成立即 suspect）：
//   - 有统计且页数 > 0，但所有页都是空白页（empty_pages == pages）；
//   - 平均每页字数低于 suspectAvgPageChars（页码 + 水印就是这种形态）。
//
// 没有 stats（docx/txt 等非分页格式）时不做可疑判定——拿不到事实就不要猜。
func judgeExtraction(stats map[string]any, chars int) extractionJudgement {
	pages := statsInt(stats, "pages")
	if pages <= 0 {
		return extractionJudgement{}
	}
	j := extractionJudgement{pages: pages}
	if statsInt(stats, "empty_pages") >= pages {
		j.suspect = true
		return j
	}
	if chars/pages < suspectAvgPageChars {
		j.suspect = true
	}
	return j
}

// statsInt 读 ocrd stats 里的整数字段（JSON 走 float64，这里做一次收口）。
func statsInt(stats map[string]any, key string) int {
	if len(stats) == 0 {
		return 0
	}
	switch v := stats[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	}
	return 0
}

// materialReport 汇总本轮素材的准备结果，作为「素材门禁」的判据。
//
// 为什么需要一个报告对象：老流程里每一步失败都只是 continue（打一行 ⚠️ 就往下走），
// 整条流水线对「素材到底有没有拿到」一无所知。结果就是上传了一份解析不出来的 PDF，
// 训练照跑，最后交付一份和素材毫无关系的技能——用户的原话是「生成的 skill 似乎和我
// 给的内容完全没有关系」。**静默降级比报错危险得多**，所以把事实收集起来交给硬门。
type materialReport struct {
	DocFiles int      // 上传的二进制文档数（需要解析的那些）
	OKFiles  int      // 成功文本化的文档数
	Chars    int      // 可用素材总字符数（含纯文本素材）
	Failures []string // 逐个失败原因：「文件名: 原因」
	Warnings []string // 解析成功但可信度存疑（如平均每页字数过低）

	// EnvHint 记录「这份失败是环境问题，不是文件问题」时**一次性**的修复指引。
	//
	// 为什么单独一个字段：解析服务没在运行时，上传 3 份文档就是 3 条一模一样的
	// 「连不上」。把长指引塞进每一条 Failures 里，门禁报错会把同一段话印 3 遍，
	// 用户反而找不到重点。逐文件那行只说「服务没在运行」，指引在这里存一份，
	// 由 enforceMaterialGate 附在最终中止信息末尾。
	EnvHint string

	// SuspectFiles 记录「解析返回了非空文本，但逐页统计显示等于没读到正文」的文档数
	// （整本页页空白，或平均每页字数低于 suspectAvgPageChars）。
	//
	// 为什么单靠 OKFiles / Chars 抓不住：混合型 PDF 只读出「扫描全能王 / 第 N 页」水印时，
	// 文本非空、字符数上千，两道判据全部放行，训练照跑，最后交付的技能与素材毫无关系。
	// 所以判据必须落在「每页读到多少字」这种结构化事实上，而不是看字符串里有没有
	// 「水印」二字——靠关键词猜形态必然漏（换一个扫描 App 水印文案就变了）。
	SuspectFiles int

	// TotalPages 所有文档的页数合计，用于把「共 50 页只读到 1129 字」说清楚。
	TotalPages int
}

// minMaterialChars 素材可用性下限（字符）。低于此值等于没有素材，继续跑只会喂给
// 模型一堆水印噪声。可用 SKILLFORGE_MIN_MATERIAL_CHARS 覆盖。
func (g *Generator) minMaterialChars() int {
	if v := strings.TrimSpace(os.Getenv("SKILLFORGE_MIN_MATERIAL_CHARS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 200
}

// suspectAvgPageChars 「等于没读到正文」的每页字数水位。
//
// 取 50 而不是更小的值，是拿线上事故的真实数字校准过的：
// 那份混合型 PDF 是 50 页 / 1129 字符 = 平均每页 22 字，全是「第 N 页 + 水印」。
// 最初按 ocrd 侧的告警水位（20 字/页）抄过来，结果 22 > 20 —— 事故样本一次都拦不住，
// 单测用真实数字直接把这个阈值照出来了。真正文页每页几百字，50 的余量足够大，
// 不会误杀正常素材；而「50 页只凑出 2000 多字」本来也训不出任何东西。
const suspectAvgPageChars = 50

// enforceMaterialGate 素材硬门：上传了文档却一份都没解析出来（或素材总量少得可怜、
// 或整本只读出页码水印）时直接失败，绝不退回通用流程。
//
// 判据只在 DocFiles > 0 时生效——没传文档、纯靠需求描述训练技能是正当用法，
// 不能连带误杀。
func (g *Generator) enforceMaterialGate(rep *materialReport) error {
	if rep == nil || rep.DocFiles == 0 {
		return nil
	}
	err := g.materialGateError(rep)
	if err == nil {
		return nil
	}
	// 环境类失败必须把修复指引附在**最终**中止信息里：否则用户只看到「全部无法解析」，
	// 会去反复改自己的文件——而真正该动的是目标机上的 systemd 服务。
	if rep.EnvHint != "" {
		return fmt.Errorf("%w\n%s", err, rep.EnvHint)
	}
	return err
}

// materialGateError 是纯判据部分（不含环境指引的拼接），与 enforceMaterialGate 分开
// 只为让「判据」和「怎么把结论说给用户听」各自可测。
func (g *Generator) materialGateError(rep *materialReport) error {
	if rep == nil || rep.DocFiles == 0 {
		return nil
	}
	if rep.OKFiles == 0 {
		return fmt.Errorf("上传的 %d 份文档全部无法解析，本次训练已中止（避免生成与素材无关的技能）: %s",
			rep.DocFiles, strings.Join(rep.Failures, "；"))
	}
	// 解析成功但等于没读到正文：上传的每一份都是这种，就没有任何可用素材。
	// 这一条才是用户那条反馈（「生成的 skill 似乎和我给的内容完全没有关系」）的正主：
	// 前几页扫描 + 后几页可选的混合 PDF，正是这样一套「看着有字、其实全是水印」的素材。
	if rep.SuspectFiles > 0 && rep.SuspectFiles == rep.DocFiles {
		return fmt.Errorf("上传的 %d 份文档共 %d 页只解析出 %d 字符（平均每页 %.0f 字），"+
			"内容疑似只有页码/水印而正文缺失，本次训练已中止（避免生成与素材无关的技能）：%s\n"+
			"建议：确认文件是可搜索的文本 PDF，或重新导出/改为图片扫描件后重试",
			rep.DocFiles, rep.TotalPages, rep.Chars, avgCharsPerPage(rep.Chars, rep.TotalPages),
			strings.Join(rep.Warnings, "；"))
	}
	if rep.Chars < g.minMaterialChars() {
		return fmt.Errorf("素材可用内容仅 %d 字符（低于 %d），本次训练已中止（避免生成与素材无关的技能）：%s",
			rep.Chars, g.minMaterialChars(), strings.Join(rep.Warnings, "；"))
	}
	return nil
}

// avgCharsPerPage 只用于给人看的报错文案，页数为 0 时返回 0。
func avgCharsPerPage(chars, pages int) float64 {
	if pages <= 0 {
		return 0
	}
	return float64(chars) / float64(pages)
}

// ingestFiles 把上传的参考文件规整成「文本化」的素材：二进制文档先过 ocrd，
// 抽出的文本替换进 Content，原始字节留在 Raw 里供 source/ 留档。
//
// 幂等：Content 已是文本（管理员直接传 .txt/.md，或二次运行时）则原样跳过，
// 不会重复消耗 OCR 算力。
//
// 返回素材报告（供 enforceMaterialGate 判定）；**不在这里报错**——失败原因要攒够
// 一次性说清，而不是第一次失败就抛（用户上传 3 份文档，报 3 次错的信息量远不如一次说全）。
func (g *Generator) ingestFiles(ctx context.Context, in *Input, steps func(string)) *materialReport {
	rep := &materialReport{}
	if in == nil || len(in.Files) == 0 {
		return rep
	}
	rep.Chars = materialChars(in)
	// 依赖预检：这次训练要用到解析服务时，先探一次活。
	//
	// 为什么值得多花一次 HTTP（正常时上限 3s，连不上时毫秒返回）：解析失败有两种，
	// 「服务没在运行」和「这份文件读不出字」的用户动作完全不同——前者去重启服务，
	// 后者去改文件。逐文件报错说得清失败，但说不清该往哪修；而且引擎坏掉时用户
	// 要等一次真解析（数十秒）才知道。这里抢在长解析之前把结论给出来。
	if g.ocrURL != "" && needsDocParse(in) {
		if h := ocrsvc.Check(ctx, g.ocrURL); !h.Healthy() {
			if msg := h.Detail(); msg != "" {
				rep.EnvHint = "文档解析环境异常：" + strings.TrimPrefix(msg, "⚠️ ")
				if steps != nil {
					steps(msg)
				}
			}
		}
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
		rep.DocFiles++
		if g.ocrURL == "" {
			rep.Failures = append(rep.Failures, fn+": 未配置解析服务")
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
		// 扫描件解析要几分钟，中途必须回报进度——否则前端看起来就是卡死。
		stopProgress := ocrProgress(steps, fn, start)
		res, err := ocrExtract(ctx, g.ocrURL, fn, raw, g.ocrClient())
		stopProgress()
		if err != nil {
			// 环境类失败（服务没在运行）：逐文件只留一行短句，完整修复指引走 EnvHint
			// 一次性给出——否则上传 3 份文档就是把同一段指引印 3 遍。
			if ocrsvc.Unreachable(err) {
				reason := "文档解析服务没有在运行（" + ocrsvc.Endpoint(g.ocrURL) + " 连不上）"
				rep.Failures = append(rep.Failures, fn+": "+reason)
				// 界面上只给用户人话（可执行、不含底层噪音），但服务端日志要留真话：
				// 排障时要能分清 refused / DNS / 路由。「用户看不到」不等于「可以不留证据」。
				log.Printf("[ocr] %s 解析失败（依赖不可用）：%v", fn, err)
				if rep.EnvHint == "" {
					rep.EnvHint = "文档解析环境异常：" + ocrsvc.Explain(err, g.ocrURL).Error()
				}
				if steps != nil {
					steps(fmt.Sprintf("⚠️ %s 解析失败：%s", fn, reason))
				}
				continue
			}
			rep.Failures = append(rep.Failures, fmt.Sprintf("%s: %v", fn, err))
			if steps != nil {
				msg := fmt.Sprintf("⚠️ %s 解析失败：%v", fn, err)
				if isTimeoutErr(err) {
					// 超时必须指名道姓地说清后果和出口，否则用户只看到一行 ⚠️，
					// 完全不知道训练已经悄悄降级成通用流程了。
					msg += fmt.Sprintf("（已等待 %s；解析超时会让本次训练退回通用流程，"+
						"确实需要更久可调大 SKILLFORGE_OCR_TIMEOUT，当前上限 %s）",
						humanDuration(time.Since(start)), humanDuration(g.ocrTimeoutOrDefault()))
				}
				steps(msg)
			}
			continue
		}
		text := res.Text
		if strings.TrimSpace(text) == "" {
			rep.Failures = append(rep.Failures, fn+": 解析结果为空")
			if steps != nil {
				steps(fmt.Sprintf("⚠️ %s 解析结果为空，已跳过", fn))
			}
			continue
		}
		uf.Raw = raw
		uf.Content = text
		uf.Extracted = true
		rep.OKFiles++
		// 「有文本」和「有正文」是两件事：把逐页事实记进报告，供素材门禁判定。
		rec := judgeExtraction(res.Stats, len([]rune(text)))
		rep.TotalPages += rec.pages
		if rec.suspect {
			rep.SuspectFiles++
		}
		if steps != nil {
			steps(fmt.Sprintf("%s 解析完成：%d 字符（%.1fs）%s",
				fn, len([]rune(text)), time.Since(start).Seconds(), statsLine(res.Stats)))
			// 服务端用逐页统计判断出的「素材可能不完整」，必须原样透给用户看：
			// 这是唯一能让用户在上传后、训练前就发现「PDF 只有水印被读出来」的信号。
			if res.Warning != "" {
				rep.Warnings = append(rep.Warnings, fn+": "+res.Warning)
				steps(fmt.Sprintf("⚠️ %s 解析可能不完整：%s", fn, res.Warning))
			}
		} else if res.Warning != "" {
			rep.Warnings = append(rep.Warnings, fn+": "+res.Warning)
		}
	}
	rep.Chars = materialChars(in)
	return rep
}

// materialChars 统计当前可用素材的总字符数（跳过隐藏文件与仍是二进制的文件）。
// 与 manualSourceText 同口径，避免「门禁按 A 口径算、实际用料按 B 口径算」的错位。
func materialChars(in *Input) int {
	if in == nil {
		return 0
	}
	n := 0
	for _, f := range in.Files {
		if f == nil {
			continue
		}
		name := filepath.Base(strings.TrimSpace(f.Filename))
		if name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		c := f.Content
		if strings.TrimSpace(c) == "" || looksBinary(c) {
			continue
		}
		n += len([]rune(c))
	}
	return n
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
	// Fallbacks 记录「锚点不可用、改用原文章节整章兜底」的分类。
	//
	// 与 Warnings 分开记是刻意的：Warnings 是「这一类没范文」，Fallbacks 是
	// 「这一类有范文，但不是模型精确锚出来的那一段」。前者是缺陷，后者是降级，
	// 混在一起会让管理员分不清「要不要管」。两者都必须在 fidelity.md 里显性出现——
	// 静默降级等于管理员失去判断依据（这正是本模块反复踩的坑）。
	Fallbacks []string
	// Source 是切出范文用的原文（OCR 后的手册全文）。只为 fidelity.md 的保真
	// 核对服务——「这篇范文确实能在原文里逐字找到」这句结论需要原文在手。
	Source string
	// Judge 是 Step 8.5 裁判循环的结论，nil 表示没跑裁判（通用流程）。
	// 挂在 manualPack 上而不是另开一个参数传给 land：两者是同一件事的两面
	// （手册给标尺、裁判按标尺判手册类技能），一份数据一条路径写盘，
	// 免得出现「传了裁判结果却漏传给 fidelity」这种缺失。
	Judge *JudgeReport
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
	// 章节区间只探一次：它是兜底切片与按章抽取共用的地基，纯代码零成本。
	// 探不到（手册没有「第X章」这种结构）时两条路各自放弃，不影响主流程。
	spans := pickChapterSpans(src)

	st, stErr := ExtractStructure(withoutThinking(ctx), g.streamingChat(), src)
	var packErr error
	if stErr == nil && st != nil && len(st.Categories) >= manualMinCategories {
		// 路径一：全文一次抽取。能用就用——一次调用最省时间，成功率也不低
		// （实测 6 万字里能抽出 6-12 个分类），不能因为有小概率失败就先花
		// 十几次调用按章抽。
		mp, err := g.packFromStructure(src, st, spans, nil)
		if err == nil {
			return mp, nil
		}
		// 「分类抽出来了、但一篇范文都切不出来」同样是抽取不可靠的证据，
		// 值得走按章重抽（锚点全带「…」的实测事故就是这个形态）。
		packErr = err
	}

	// 路径二：按章小 prompt 抽取（治本，见 manual_repair.go 顶部注释）。
	// 只有路径一不可靠时才走，所以「十几次调用」这个代价只在失败路径上付。
	if len(spans) >= manualMinCategories {
		st2, warns2, err2 := extractStructureByChapters(withoutThinking(ctx), g.streamingChat(), spans)
		if err2 == nil && st2 != nil && len(st2.Categories) >= manualMinCategories {
			notes := append([]string{chapterStructureNote}, warns2...)
			if mp, err := g.packFromStructure(src, st2, spans, notes); err == nil {
				return mp, nil
			}
		}
	}

	// 两条路都没走通：保持原有降级语义（返回 error → 调用方退回通用流程），
	// 但把最能说明问题的原因带回去——trace 里只剩一句「失败了」没法排查。
	switch {
	case stErr != nil:
		return nil, fmt.Errorf("结构抽取失败: %w", stErr)
	case st == nil || len(st.Categories) < manualMinCategories:
		n := 0
		if st != nil {
			n = len(st.Categories)
		}
		return nil, fmt.Errorf("只抽出 %d 个分类（需要 ≥%d），按通用素材处理", n, manualMinCategories)
	default:
		return nil, fmt.Errorf("分类抽出 %d 个但一篇范文都切不出来（%v），按章重抽也没救回来，按通用素材处理",
			len(st.Categories), packErr)
	}
}

// packFromStructure 按分类切范文并组装手册产物，是手册模式唯一的「切范文」入口。
//
// 每类依次尝试两条路，从严到宽：
//  1. 模型给的锚点（SplitByAnchors）——精确，是首选；
//  2. 锚点不可用 → 用原文自己的章节标题兜底切整章（manual_chapters.go）——
//     切法降级但内容仍是原文逐字，且让这一类**不至于没有范文**。
//
// 为什么兜底是必须的：锚点短、唯一、逐字，恰恰是 reasoning 模型在长输入下最先
// 牺牲的东西（实测：分类抽出来了，anchor 全带省略号）。没有兜底时，这一类一张
// 范文都没有，Step 8.5 的确定性硬校验会直接否决整份技能——模型给 100 分也救不回，
// 而手册本身其实完全可用。兜底把「永久否决」变成「可用但切法降级」，并且
// **显性记进 Fallbacks**（fidelity.md 单独一节），管理员看得见、可人工改进。
//
// notes 是调用方带来的整体说明（如「已改用按章抽取」），先于逐类记录写进 Warnings。
func (g *Generator) packFromStructure(src string, st *Structure, spans []chapterSpan, notes []string) (*manualPack, error) {
	mp := &manualPack{
		Structure: st,
		Examples:  map[string][]string{},
		Paths:     map[string][]string{},
		Source:    src,
		Warnings:  append([]string(nil), notes...),
	}
	for _, c := range st.Categories {
		segs, err := SplitByAnchors(src, c.Anchor)
		if err == nil && len(segs) > 0 {
			mp.Examples[c.Name] = segs
			continue
		}
		reason := "模型没给锚点"
		switch {
		case err != nil:
			reason = err.Error()
		case len(segs) == 0:
			reason = "锚点切出 0 段"
		}
		if text, why, ok := chapterFallbackText(spans, c.Name); ok {
			mp.Examples[c.Name] = []string{text}
			mp.Fallbacks = append(mp.Fallbacks, fmt.Sprintf("分类「%s」模型锚点不可用（%s），%s", c.Name, reason, why))
			continue
		}
		// 单个分类切不准不算致命：其余分类仍然可用，问题记进 Warnings
		// 让人能在 UI 上看到「这一类的范文没切出来」而不是整体失败。
		mp.Warnings = append(mp.Warnings, fmt.Sprintf("分类「%s」范文未切出：%s", c.Name, reason))
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
	// 降级告示放最顶上：这份技能是按「裁判未验收通过」的状态交付的。
	// 报告下面的分数、薄弱项都是细节，只有这句话能让人 3 秒内知道该不该信这份技能。
	if deg, reason := mp.Judge.Degraded(); deg {
		fmt.Fprintf(&b, "> ⚠️ **本次为降级交付**：%s\n> 技能已落盘可用，但**未经裁判验收通过**，请人工复核后再投入生产。\n\n", reason)
	}
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

	// 章节兜底必须单独占一行：它和「范文覆盖」是两个不同的量——覆盖数好看不代表
	// 每篇都是精确锚出来的。只报覆盖数会让「整章兜底」伪装成正常结果，
	// 管理员就没法判断该不该回去改锚点。
	if n := len(mp.Fallbacks); n > 0 {
		fmt.Fprintf(&b, "- 章节兜底：%d 类（模型锚点不可用，已改用原文整章作范文）\n", n)
	}

	// 保真核对是这份报告的立身之本：范文必须是原文连续子串。这一条是机械可验的，
	// 所以直接报数字，而不是写「已确保保真」这种无法证伪的话。
	if found, checked := mp.countFidelity(); checked > 0 {
		fmt.Fprintf(&b, "- 保真核对：%d/%d 篇可在 source/ 原文中逐字找到（未经模型改写）\n", found, checked)
	}

	// 裁判评分表（Generate 的 Step 8.5）。跟保真核对的落盘理由一样：
	// 把「这个技能到底有没有被独立验收过」变成看得见的证据。三种情况必须显性——
	// 裁判没跑成、提前止损、到上限带薄弱项交付；只写「通过」二字的话，
	// 没验收过的技能看起来就跟验过的一样，那正是静默降级的藏身处。
	b.WriteString("\n## 裁判评分（独立评审 · 上限 ")
	fmt.Fprintf(&b, "%d 轮）\n\n", judgeMaxRounds)
	if mp.Judge == nil {
		b.WriteString("（未启用裁判评分）\n")
	} else {
		b.WriteString(judgeRoundTable(mp.Judge.Rounds))
		if mp.Judge.BestRound > 0 {
			fmt.Fprintf(&b, "\n- 交付轮次：第 %d 轮（同分取更早轮次，保证结果可复核）\n", mp.Judge.BestRound)
		}
		if mp.Judge.EarlyStop != "" {
			fmt.Fprintf(&b, "- 提前止损：%s\n", mp.Judge.EarlyStop)
		}
		if mp.Judge.Err != "" {
			fmt.Fprintf(&b, "- ⚠️ 裁判未跑完：%s\n", mp.Judge.Err)
		}
		if w := mp.Judge.WeakDims(3); len(w) > 0 {
			fmt.Fprintf(&b, "- 薄弱维度：%s\n", strings.Join(w, " · "))
		}
	}

	// 兜底明细单列一节：结论区只写「有几类降级」不够，管理员需要知道是哪些类、
	// 为什么降级、兜底出来多少字，才能判断要不要人工补锚点。
	if len(mp.Fallbacks) > 0 {
		fmt.Fprintf(&b, "\n## ⚠️ 章节兜底：%d 类未按锚点切出\n\n", len(mp.Fallbacks))
		for _, f := range mp.Fallbacks {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		b.WriteString("\n兜底范文取自原文章节的正文，仍是逐字切片（保真核对照样通过），" +
			"但粒度是「整章」而非手册里那一篇范文，可能混入该章的写法说明。" +
			"运行时会注入这一章当作该类的参考范文，效果弱于精确范文；" +
			"若要改回精确切分，可核对 categories/ 下对应文件里的锚点。\n")
	}

	switch {
	case len(mp.Warnings) == 0 && len(mp.Fallbacks) == 0:
		b.WriteString("\n## 结论\n\n全部类别的范文均已按锚点从原文切出，无待处理项。\n")
	case len(mp.Warnings) == 0:
		// 全集都有范文，但「全部按锚点切出」是假话——兜底那几类不是按锚点切的。
		// 结论区是这个模块唯一会被人快速扫一眼的地方，不能在这里模糊降级。
		fmt.Fprintf(&b, "\n## 结论\n\n全部 %d 类都取到了范文，其中 %d 类为章节兜底（见上节），无缺失分类。\n",
			total, len(mp.Fallbacks))
	default:
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
	b.WriteString("对照 `reviewer.md` 的检查项逐条核对，任一条不满足就改到达标再交付。\n")
	b.WriteString("核对是**内部动作**：只输出稿件本身，不要把核对清单、逐条结论或自证附录写进交付内容。\n\n")
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

	// 审稿清单要通读手册原文再逐条提炼，属分钟级调用；接流式免得这一段也是纯计时。
	out, err := g.chatWithMaterial(withoutThinking(ctx), sys, user)
	if err != nil {
		return "", err
	}
	return cleanCodeFence(out), nil
}
