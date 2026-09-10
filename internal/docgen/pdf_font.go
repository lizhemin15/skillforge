package docgen

import (
	"log"
	"os"
	"sort"
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

// resolvePDFFont 挑选第一个「门槛字符集全覆盖」的候选字体；若无，则取缺字最少的那个
// 并 WARN 记录缺了哪些字符。结果被缓存，只在进程内探测一次。
//
// 返回 (字体路径, 该字体相对门槛字符集的缺失字符)。路径为空表示系统里找不到任何可用字体。
func resolvePDFFont() (string, []rune) {
	pdfFontOnce.Do(func() {
		bestMissCount := -1
		for _, cand := range pdfFontCandidates {
			if _, err := os.Stat(cand); err != nil {
				continue
			}
			miss, err := probeFontCoverage(cand, pdfRequiredSample)
			if err != nil {
				// .ttc 集合格式等不可加载的情况走这里，属预期，降级为 debug 级噪音
				log.Printf("[docgen/pdf] font candidate unusable: %s: %v", cand, err)
				continue
			}
			if len(miss) == 0 {
				pdfFontPath, pdfFontMissing = cand, nil
				log.Printf("[docgen/pdf] font resolved: %s (full coverage)", cand)
				return
			}
			if bestMissCount < 0 || len(miss) < bestMissCount {
				bestMissCount, pdfFontPath, pdfFontMissing = len(miss), cand, miss
			}
		}
		if pdfFontPath != "" {
			log.Printf("[docgen/pdf] WARN no candidate covers all required glyphs; "+
				"falling back to %s, %d missing rune(s): %q — these will render as blank",
				pdfFontPath, len(pdfFontMissing), string(pdfFontMissing))
		} else {
			log.Printf("[docgen/pdf] ERROR no usable CJK font found among %d candidates", len(pdfFontCandidates))
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
