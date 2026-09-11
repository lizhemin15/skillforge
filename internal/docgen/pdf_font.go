package docgen

import (
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/signintech/gopdf"
)

// pdfFontCandidates 是 PDF 渲染可用字体的候选列表，按「覆盖能力 + 观感」排序。
//
// ⚠️ 历史教训（Bug G）：本列表曾经只有一个常量路径
// /usr/share/fonts/truetype/droid/DroidSansFallbackFull.ttf。该文件是 Debian 的
// **纯 CJK fallback** 字体，cmap 里**不含 ASCII 数字 0-9、拉丁字母和半角标点**
// （实测缺 76 种字符）。而 gopdf 的默认行为是把找不到字形的字符**静默替换成空格**
// （TtfOption.OnGlyphNotFoundSubstitute 默认为 '\u0020'），于是所有 PDF 里的
// 数字（数量/单价/金额/日期）全部变成不可见的空格——数据静默丢失，且打开 PDF
// 看不出来是坏了。所以字体必须在运行时**实测覆盖率**，绝不能假设路径存在就可用。
//
// gopdf 不能加载 .ttc 集合格式（报 "Unrecognized file (font) format"），
// 因此候选里 .ttc 类字体（wqy-zenhei / Noto CJK / uming）暂时无解，仅列作未来兼容。
var pdfFontCandidates = []string{
	// 思源黑体 / Noto CJK（若运维另行安装，观感最佳，优先使用）
	"/usr/share/fonts/opentype/noto/NotoSansCJKsc-Regular.otf",
	"/usr/share/fonts/truetype/noto/NotoSansCJKsc-Regular.otf",
	"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
	// 文鼎宋体：覆盖中文 + 数字 + 拉丁，公文/报价单观感合适
	"/usr/share/fonts/truetype/arphic-gbsn00lp/gbsn00lp.ttf",
	// 文鼎楷体：同上，作次选
	"/usr/share/fonts/truetype/arphic-gkai00mp/gkai00mp.ttf",
	// GNU Unifont：覆盖最全但点阵观感差，兜底层
	"/usr/share/fonts/truetype/unifont/unifont.ttf",
	// ⚠️ 最后兜底：缺数字/拉丁，只在系统里没有任何更好字体时使用（会被 WARN 记录）
	"/usr/share/fonts/truetype/droid/DroidSansFallbackFull.ttf",
}

// pdfFontScanDirs 是固定候选全部未「全覆盖」时，做一次目录扫描的搜索根。
//
// 为什么需要扫描：固定候选只覆盖 Debian/Ubuntu 的路径，换个发行版（Alpine / Arch /
// 自编译环境）或运维把字体装在别处，7 个常量路径就会全部 miss，PDF 生成直接失败。
// 扫到之后仍然**逐个实测覆盖率**，不假设「文件名带 CJK 就一定能用」。
var pdfFontScanDirs = []string{
	"/usr/share/fonts",
	"/usr/local/share/fonts",
	"/usr/share/X11/fonts",
	"/Library/Fonts",
	"/System/Library/Fonts",
	"/opt/homebrew/share/fonts",
}

// pdfRequiredSample 是字体必须覆盖的「门槛字符集」。任何 PDF 都可能出现数字、
// 拉丁字母和半角标点，所以这些是硬要求；中文部分取公文/合同/报价单高频字。
const pdfRequiredSample = "0123456789" +
	"ABCDEFGHIJKLMNOPQRSTUVWXYZ" +
	"abcdefghijklmnopqrstuvwxyz" +
	".,:;%()[]-+/@#&*_=<>!?'\" " +
	"￥$" +
	"产品报价单数量单价小计合计金额元万仟佰拾壹贰叁肆伍陆柒捌玖零年月日" +
	"云服务器技术支持人民币大写备注：，。！？、（）【】《》；“”‘’—…" +
	"合同甲方乙方签署日期编号部门姓名地址电话邮箱项目名称规格型号单位总计"

// pdfFontFileEnv 允许运维强制指定字体文件，跳过全部探测（用于字体装在非常规路径）。
const pdfFontFileEnv = "SKILLFORGE_PDF_FONT_FILE"

var (
	pdfFontOnce    sync.Once
	pdfFontPath    string
	pdfFontMissing []rune
)

// probeFontCoverage 用 gopdf 自己加载字体并逐个字符查表，返回该字体缺字形的字符。
// 通过把 OnGlyphNotFoundSubstitute 置空来关闭「缺失→空格」的默认替换，让缺失现出原形。
func probeFontCoverage(path, sample string) ([]rune, error) {
	var mu sync.Mutex
	var missing []rune

	pdf := &gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: *gopdf.PageSizeA4})
	opt := gopdf.TtfOption{
		OnGlyphNotFound:           func(r rune) { mu.Lock(); missing = append(missing, r); mu.Unlock() },
		OnGlyphNotFoundSubstitute: nil,
	}
	if err := pdf.AddTTFFontWithOption("probe", path, opt); err != nil {
		return nil, err
	}
	if err := pdf.SetFont("probe", "", 12); err != nil {
		return nil, err
	}
	pdf.AddPage()
	pdf.SetXY(20, 20)
	// Cell 内部会走 AddChars → 逐字 CharCodeToGlyphIndex，缺失即触发回调。
	_ = pdf.Cell(nil, sample)

	mu.Lock()
	defer mu.Unlock()
	return dedupRunes(missing), nil
}

func dedupRunes(in []rune) []rune {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[rune]bool, len(in))
	out := make([]rune, 0, len(in))
	for _, r := range in {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// scanTTFFonts 在给定根目录下递归收集 .ttf 文件。
//
// 跳过 .ttc（字体集合）与 .otf（CFF 轮廓）：gopdf 加载这两种会报
// "Unrecognized file (font) format"，收进来只会白跑一趟探测。
// 按「根」逐个扫、逐个排：**roots 的顺序就是优先级**。
//
// ⚠️ 历史教训：这里曾经把所有候选收进一个切片后做一次全局 sort.Strings，再截断到
// maxScannedFonts。后果是「自带字体目录」的优先级被文件名字典序抹平——系统字体目录
// 里只要字体够多（/usr/share/fonts 很容易上百个），排序后自带字体就被挤到截断线之外，
// 于是明明包里带了字体，运行期却挑到系统里那份（或挑不到）。性能封顶不能靠丢优先级实现。
func scanTTFFonts(roots []string) []string {
	var out []string
	for _, root := range roots {
		if root == "" {
			continue
		}
		if _, err := os.Stat(root); err != nil {
			continue
		}
		var inRoot []string
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if strings.EqualFold(filepath.Ext(path), ".ttf") {
				inRoot = append(inRoot, path)
			}
			return nil
		})
		sort.Strings(inRoot) // 根内排序：同一台机器上选出的字体可复现
		out = append(out, inRoot...)
		if len(out) >= maxScannedFonts {
			break // 已经够了就不再扫后面的根，优先级高的先拿到配额
		}
	}
	if len(out) > maxScannedFonts {
		out = out[:maxScannedFonts]
	}
	return out
}

// maxScannedFonts 给目录扫描的探测次数封顶，避免字体极多的机器上首次调用过慢。
const maxScannedFonts = 120

// bundledFontScanRoots 返回目录扫描的搜索根，顺序即优先级：
// 离线包自带的字体目录（安装脚本按实例写成 /usr/local/share/fonts/skillforge-<服务名>）
// 排在最前，然后是通用系统目录。
//
// 为什么把「包内」排最前：客户机上可能已经有别的中文字体，那些字体能通过覆盖率门槛，
// 但观感/口径与装包自检时用的那份不同——排前面能保证「自检说用哪个，运行时就用哪个」。
// 注：SKILLFORGE_PDF_FONT_FILE 优先级更高，安装脚本会显式指定，这里只兜底没配 env 的场景。
func bundledFontScanRoots() []string {
	bundled, _ := filepath.Glob("/usr/local/share/fonts/skillforge*")
	sort.Strings(bundled)
	return append(append([]string{}, bundled...), pdfFontScanDirs...)
}

// resolvePDFFont 挑选第一个「门槛字符集全覆盖」的字体；若无，则取缺字最少的那个
// 并 WARN 记录缺了哪些字符。结果被缓存，只在进程内探测一次。
//
// 挑选顺序：
//  1. SKILLFORGE_PDF_FONT_FILE 指定的字体（运维强制，不满足则报错而不是静默降级）
//  2. pdfFontCandidates 固定候选（按观感排序）
//  3. 扫描 pdfFontScanDirs 找到的 .ttf（覆盖非 Debian 路径 / 自定义安装）
//
// 返回 (字体路径, 该字体相对门槛字符集的缺失字符)。路径为空表示系统里找不到任何可用字体。
func resolvePDFFont() (string, []rune) {
	pdfFontOnce.Do(func() {
		// 1) 运维显式指定：尊重它，但也要实测覆盖率（配错了必须说出来，不能装作没事）
		if forced := os.Getenv(pdfFontFileEnv); forced != "" {
			miss, err := probeFontCoverage(forced, pdfRequiredSample)
			if err != nil {
				log.Printf("[docgen/pdf] ERROR %s=%s 无法加载: %v", pdfFontFileEnv, forced, err)
			} else {
				pdfFontPath, pdfFontMissing = forced, miss
				if len(miss) == 0 {
					log.Printf("[docgen/pdf] font resolved by %s: %s", pdfFontFileEnv, forced)
				} else {
					log.Printf("[docgen/pdf] WARN %s=%s 缺 %d 个字符: %q",
						pdfFontFileEnv, forced, len(miss), string(miss))
				}
				return
			}
		}

		bestMissCount := -1

		// 尝试一个候选：全通则立即定案；否则只记录「目前缺得最少」的那个。
		tryOne := func(cand string) (done bool) {
			miss, err := probeFontCoverage(cand, pdfRequiredSample)
			if err != nil {
				// .ttc / .otf 等不可加载的情况走这里，属预期，降为噪音
				log.Printf("[docgen/pdf] font candidate unusable: %s: %v", cand, err)
				return false
			}
			if len(miss) == 0 {
				pdfFontPath, pdfFontMissing = cand, nil
				log.Printf("[docgen/pdf] font resolved: %s (full coverage)", cand)
				return true
			}
			if bestMissCount < 0 || len(miss) < bestMissCount {
				bestMissCount, pdfFontPath, pdfFontMissing = len(miss), cand, miss
			}
			return false
		}

		// 2) 固定候选
		for _, cand := range pdfFontCandidates {
			if _, err := os.Stat(cand); err != nil {
				continue
			}
			if tryOne(cand) {
				return
			}
		}

		// 2.5) 离线包自带的字体目录优先（见 bundledFontScanRoots）：
		// 固定候选里没有、只能靠扫描时，「包里带的」必须排在「系统里碰巧有的」前面。
		scanRoots := bundledFontScanRoots()

		// 3) 扫描字体目录，找一个全覆盖的（固定候选全都只能部分覆盖时才走到这里）
		for _, cand := range scanTTFFonts(scanRoots) {
			if tryOne(cand) {
				return
			}
		}

		if pdfFontPath != "" {
			log.Printf("[docgen/pdf] WARN no font covers all required glyphs; "+
				"falling back to %s, %d missing rune(s): %q — these will render as blank",
				pdfFontPath, len(pdfFontMissing), string(pdfFontMissing))
		} else {
			log.Printf("[docgen/pdf] ERROR no usable CJK font found; "+
				"install one (e.g. apt-get install fonts-arphic-gbsn00lp) "+
				"or point %s to a .ttf file", pdfFontFileEnv)
		}
	})
	return pdfFontPath, pdfFontMissing
}

// PDFFontPath 返回实际使用的 PDF 字体路径（供测试与运维自检使用）。
func PDFFontPath() string {
	p, _ := resolvePDFFont()
	return p
}

// PDFFontMissingRunes 返回实际字体相对门槛字符集缺失的字符（正常应为空）。
func PDFFontMissingRunes() []rune {
	_, miss := resolvePDFFont()
	return miss
}
