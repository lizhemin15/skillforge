package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 内置业务技能「数据治理任务开发」的两条回归防线：
//
//  1. 提示词自洽 —— 里面写的 gov API 必须与 DataToolbox 官方名单一致。
//     这份提示词的全部价值就在于「教会模型只用手上真有的 API」；一旦里面
//     混进一个杜撰的 gov.xxx（或者官方 async 方法漏了 await），模型会照着
//     抄，用户拿到的脚本在治理任务里当场报错。人是看不住的，尺子看。
//  2. 播种语义 —— 安装即有、归「业务场景」、默认关闭、幂等、不覆盖管理员改动。

// govOfficialAPIList 是 DataToolbox 官方 API 面（23 项）的快照。
//
// 来源：DataToolbox 仓库 gov-shared.js 的 GOV_API_SECTIONS —— 它自己的
// 「代码工坊」就是拿这份名单驳回杜撰 API 的，跟运行时是同一份真身。
// DataToolbox 增删 API 时这份名单要同步：不同步的后果是**本测试先红**，
// 而不是让模型在现场瞎猜（这正是我们要的失败方向）。
var govOfficialAPIList = []string{
	"log", "showTable", "getDbType", "getDatabases",
	"readExcel", "readCSV", "readWord", "parseWordStructure", "readWordTables",
	"writeExcel", "writeCSV", "writeText", "writeJSON",
	"word", "buildWordTables", "fillWordTemplate", "getDefaultFont", "fillExcelTemplate",
	"querySQL", "executeSQL", "querySQLForDb", "executeSQLForDb",
	"callAI",
}

// govOfficialAwaitAPIs 是官方签名里带 await 的那批（返回 Promise）。
// 它们在示例代码里必须写成 await gov.xxx(...)，否则用户复制走就是拿到 Promise。
var govOfficialAwaitAPIs = []string{
	"readExcel", "readCSV", "readWord", "parseWordStructure", "readWordTables",
	"fillWordTemplate", "fillExcelTemplate",
	"querySQL", "executeSQL", "querySQLForDb", "executeSQLForDb",
	"callAI",
}

// govBannedInExamples 是提示词明令禁用、也绝不能出现在示例代码里的写法。
// 模型是照着示例学的：示例里出现 fs/require/fetch，它下一轮就会用。
var govBannedInExamples = []string{"require(", "import ", "child_process", "fetch(", "window.", "document.", "fs."}

var (
	reGovAPIItem  = regexp.MustCompile("- `(?:await )?gov\\.([A-Za-z]+)\\(")
	reGovCodeBlk  = regexp.MustCompile("(?s)```javascript\\n(.*?)```")
	reGovCallName = regexp.MustCompile("gov\\.([A-Za-z]+)\\(")
)

func TestGovTaskDevPromptAPISurfaceMatchesOfficial(t *testing.T) {
	documented := map[string]bool{}
	for _, m := range reGovAPIItem.FindAllStringSubmatch(govTaskDevSystemPrompt, -1) {
		documented[m[1]] = true
	}
	for _, name := range govOfficialAPIList {
		if !documented[name] {
			t.Errorf("官方 API gov.%s 没写进「可用 API」清单：模型不知道有这个能力", name)
		}
	}
	for name := range documented {
		if !containsStr(govOfficialAPIList, name) {
			t.Errorf("清单里的 gov.%s 不在官方名单上 —— 提示词开始教人用不存在的 API 了", name)
		}
	}
}

func TestGovTaskDevPromptExamplesUseOnlyDocumentedAPIs(t *testing.T) {
	blocks := reGovCodeBlk.FindAllStringSubmatch(govTaskDevSystemPrompt, -1)
	// 前提自证：没抽到代码块时下面的循环全是空转，等于没测。
	if len(blocks) < 4 {
		t.Fatalf("只从提示词里抽到 %d 个 javascript 代码块，示例/配方丢了？", len(blocks))
	}
	documented := map[string]bool{}
	for _, m := range reGovAPIItem.FindAllStringSubmatch(govTaskDevSystemPrompt, -1) {
		documented[m[1]] = true
	}

	total := 0
	for _, b := range blocks {
		// 注释里也会出现 gov.xxx(...)（示例里就有「// 关键 API：gov.readWordTables() 提取样式」），
		// 那是讲解不是调用，漏 await 也不该报错 —— 所以先剥注释再扫。
		code := stripJSComments(b[1])
		for _, loc := range reGovCallName.FindAllStringSubmatchIndex(code, -1) {
			name := code[loc[2]:loc[3]]
			total++
			if !documented[name] {
				t.Errorf("示例代码里用了清单外的 gov.%s：\n%s", name, lineAround(code, loc[0]))
			}
			if containsStr(govOfficialAwaitAPIs, name) {
				prefix := strings.TrimRight(code[:loc[0]], " \t\r\n")
				if !strings.HasSuffix(prefix, "await") {
					t.Errorf("gov.%s 官方签名是 async，示例里漏了 await：\n%s", name, lineAround(code, loc[0]))
				}
			}
		}
		for _, bad := range govBannedInExamples {
			if strings.Contains(code, bad) {
				t.Errorf("示例代码里出现被明令禁用的 %q（模型会照抄）：\n%s", bad, code)
				break
			}
		}
	}
	// 前提自证：真扫到了调用点，避免正则写错导致「全绿但什么都没查」。
	if total < 20 {
		t.Fatalf("示例代码里只扫到 %d 处 gov.* 调用，明显偏少，正则或示例可能已失效", total)
	}
}

// 契约要素：缺了任何一条，用户拿到的就不是「能直接粘贴运行的完整脚本」。
func TestGovTaskDevPromptKeepsDeliveryContract(t *testing.T) {
	must := []string{
		"只输出一个", "```javascript", // 输出形态：一段可粘贴的完整脚本
		"INPUT_FILE", "INPUT_TEXT", "INPUT_FILES", "currentGovTask", // 全局变量
		"await",                                                     // 异步语义
		"gov.callAI", "gov.executeSQLForDb", "gov.fillWordTemplate", // 三条最容易被写错的 API 至少要有说明
	}
	for _, s := range must {
		if !strings.Contains(govTaskDevSystemPrompt, s) {
			t.Errorf("系统提示词缺少契约要素 %q", s)
		}
	}
	if len([]rune(govTaskDevSystemPrompt)) < 5000 {
		t.Fatalf("系统提示词只有 %d 字：聊天时只注入这份提示词，太短就不可能自包含 gov 契约与示例",
			len([]rune(govTaskDevSystemPrompt)))
	}
}

// 播种后的默认形态：安装即有、归业务场景、默认关闭、非核心。
func TestSeedGovTaskDevSkillDefaults(t *testing.T) {
	dir := t.TempDir()
	s := newStoreForTest(t, dir)

	sk, err := s.Get(GovTaskDevSlug)
	if err != nil {
		t.Fatalf("内置业务技能没被播种出来：%v", err)
	}
	// 这里必须钉**字面量**，不能写 `sk.Category != govTaskDevCategory`：
	// 那样是拿常量跟自己比，注入把常量改成「通用」后左右一起变、恒真。
	// 负向自证（scripts/seed_gov_skill_inject.py 第 5 路）实测就是这么漏掉的：
	// 分类被挪出「业务场景」而这条断言全绿。用户要的是「归在业务场景下」，
	// 那就把这个词写死在断言里。
	if sk.Category != "业务场景" {
		t.Errorf("分类应为「业务场景」（业务专用能力不许混进通用分类），实际 %q", sk.Category)
	}
	if sk.Enabled {
		t.Errorf("内置业务技能必须默认关闭：新装实例会平白多一个用不上的技能，且它只在 DataToolbox 场景下才有意义")
	}
	if sk.IsCore {
		t.Errorf("业务技能不能占核心位（核心=通用能力，管理端不可删除）")
	}
	if sk.SkillType != "query" {
		t.Errorf("技能类型应为 query（非写作技能，不走文章管线），实际 %q", sk.SkillType)
	}
	if !strings.Contains(sk.Description, "触发场景") {
		t.Errorf("描述里必须有触发场景：模型选技能只看这一行，缺了它这个技能等于选了也不会被唤起：%q", sk.Description)
	}

	spPath := filepath.Join(s.SkillsDir(), GovTaskDevSlug, "system_prompt.md")
	b, err := os.ReadFile(spPath)
	if err != nil {
		t.Fatalf("磁盘上没有系统提示词文件（管理员要能就地改）：%v", err)
	}
	if string(b) != govTaskDevSystemPrompt {
		t.Errorf("磁盘上的提示词与内置内容不一致：播种被改成了改写模式？")
	}
	if len(b) < 8000 {
		t.Errorf("提示词文件只有 %d 字节，装不下 23 项 API 契约 + 完整示例", len(b))
	}
}

// 幂等 + 不覆盖管理员的改动：重启不能把管理员的开关/分类/手改提示词打回原样。
func TestSeedGovTaskDevSkillKeepsAdminEdits(t *testing.T) {
	dir := t.TempDir()
	s := newStoreForTest(t, dir)

	if err := s.SetEnabled(GovTaskDevSlug, true); err != nil {
		t.Fatalf("启用失败：%v", err)
	}
	if _, err := s.db.Exec(`UPDATE skills SET category=? WHERE slug=?`, "我的场景", GovTaskDevSlug); err != nil {
		t.Fatalf("改分类失败：%v", err)
	}
	spPath := filepath.Join(s.SkillsDir(), GovTaskDevSlug, "system_prompt.md")
	if err := os.WriteFile(spPath, []byte("管理员手改过的提示词"), 0o644); err != nil {
		t.Fatalf("手改提示词失败：%v", err)
	}

	// 重启：同一个 dataDir（同一个库文件 + 同一个技能目录）再建一次 store。
	s2 := newStoreForTest(t, dir)
	sk, err := s2.Get(GovTaskDevSlug)
	if err != nil {
		t.Fatalf("重启后技能不见了：%v", err)
	}
	if !sk.Enabled {
		t.Errorf("重启把管理员打开的开关关回去了（默认关闭只该作用于首次播种）")
	}
	if sk.Category != "我的场景" {
		t.Errorf("重启把管理员改的分类覆盖成 %q 了", sk.Category)
	}
	b, err := os.ReadFile(spPath)
	if err != nil {
		t.Fatalf("重启后提示词文件不见了：%v", err)
	}
	if string(b) != "管理员手改过的提示词" {
		t.Errorf("重启把管理员手改的提示词覆盖回内置版本了")
	}

	var n int
	if err := s2.db.QueryRow(`SELECT COUNT(1) FROM skills WHERE slug=?`, GovTaskDevSlug).Scan(&n); err != nil {
		t.Fatalf("查行数失败：%v", err)
	}
	if n != 1 {
		t.Errorf("重复播种出了 %d 行同名技能", n)
	}
}

// stripJSComments 逐字符剥掉 // 行注释与 /* */ 块注释，字符串字面量与正则字面量里的
// 斜杠不受影响（示例里有 'http://' 这类内容，纯正则替换会把它们切坏）。
func stripJSComments(src string) string {
	var out strings.Builder
	out.Grow(len(src))
	var quote byte // 当前所在字符串的引号字符（' " `），0 = 不在字符串里
	inLine, inBlock := false, false
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
				out.WriteByte(c)
			}
			continue
		case inBlock:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlock = false
				i++
			}
			continue
		}
		if quote != 0 {
			out.WriteByte(c)
			if c == '\\' && quote != '`' && i+1 < len(src) {
				out.WriteByte(src[i+1])
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch {
		case c == '\'' || c == '"' || c == '`':
			quote = c
			out.WriteByte(c)
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			inLine = true
			i++
			out.WriteByte('\n') // 保留换行，失败信息里的行号才对得上
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			inBlock = true
			i++
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

// lineAround 取命中位置所在行，用于把失败指到具体那一行代码。
func lineAround(code string, at int) string {
	start := strings.LastIndex(code[:at], "\n") + 1
	end := strings.Index(code[at:], "\n")
	if end < 0 {
		end = len(code) - at
	}
	return strings.TrimSpace(code[start : at+end])
}
