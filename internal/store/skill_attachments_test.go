package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/db"
	"github.com/lizhemin15/skillforge/internal/model"
)

// sf3 的尺子：skill 的「可下发附件」从单值字符串变成数组。
//
// 这里钉的三件事，都是**上线后会真出事**的地方，而不是「代码长这样」：
//  1. 顺序与来源字段要活下来 —— 用户看到的清单顺序得跟源正文一致（「附件：1.报名表
//     2.汇总表」），Origin 是拆错了以后唯一能追问「这份是从哪儿来的」的线索；
//  2. 线上已有的老行（只有 attachment 单值列）**不做数据迁移也要读对** —— 升级动作
//     只有一个「重启换二进制」，没有人工跑脚本这一步；
//  3. 老库缺 attachments 列时迁移必须加上 —— SQLite 的 CREATE TABLE IF NOT EXISTS
//     对已有表是空操作，漏了 ALTER 就是升级即炸（INSERT 报 no such column）。

func newStoreForAttachTest(t *testing.T) *SkillStore {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return NewSkillStore(d, dir)
}

// 存进去什么，读出来就得是什么：顺序、Origin、How 一个都不能丢。
// 顺序是产品语义（清单顺序 = 正文清单顺序），不是实现细节。
func TestSkillAttachments_RoundTripKeepsOrderAndOrigin(t *testing.T) {
	s := newStoreForAttachTest(t)
	want := []model.Attachment{
		{Name: "报名表.docx", Rel: "attachments/报名表.docx", Origin: "关于开展XX工作的通知.docx", Size: 20480, How: "docx:embeddings>ole:Package"},
		{Name: "汇总表.xlsx", Rel: "attachments/汇总表.xlsx", Origin: "关于开展XX工作的通知.docx", Size: 15360, How: "docx:embeddings>ole:Package"},
	}
	if err := s.Create(&model.Skill{
		Slug: "xx通知办理", Name: "XX通知办理", Category: "general", Version: 1, Enabled: true,
		SkillType: model.SkillTypeQuery, Attachments: want,
	}, nil); err != nil {
		t.Fatalf("注册技能失败: %v", err)
	}

	got, err := s.Get("xx通知办理")
	if err != nil {
		t.Fatalf("读回技能失败: %v", err)
	}
	if len(got.Attachments) != len(want) {
		t.Fatalf("附件条数 %d，期望 %d（%+v）", len(got.Attachments), len(want), got.Attachments)
	}
	for i := range want {
		if got.Attachments[i] != want[i] {
			t.Errorf("第%d条附件读回后变了: got %+v, want %+v", i+1, got.Attachments[i], want[i])
		}
	}
	if got.SkillType != model.SkillTypeQuery {
		t.Errorf("技能类型读回变成 %q，期望 %q", got.SkillType, model.SkillTypeQuery)
	}
}

// List 与 Get 是两条独立的 SELECT。漏一条就会「详情页有附件、列表里没有」，
// 而 roster/分类器走的是 List —— 那种漏法线上表现是「一键生成的技能根本没附件」。
func TestSkillAttachments_ListCarriesSameAttachmentsAsGet(t *testing.T) {
	s := newStoreForAttachTest(t)
	if err := s.Create(&model.Skill{
		Slug: "list附件", Name: "list附件", Category: "general", Version: 1, Enabled: true,
		SkillType:   model.SkillTypeQuery,
		Attachments: []model.Attachment{{Name: "模板.docx", Rel: "attachments/模板.docx", Origin: "源文件.docx"}},
	}, nil); err != nil {
		t.Fatalf("注册技能失败: %v", err)
	}

	all, err := s.List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	var found *model.Skill
	for i := range all {
		if all[i].Slug == "list附件" {
			found = &all[i]
		}
	}
	if found == nil {
		t.Fatal("List 里没有刚注册的技能")
	}
	if len(found.Attachments) != 1 || found.Attachments[0].Rel != "attachments/模板.docx" {
		t.Fatalf("List 带出的附件与 Get 不一致: %+v", found.Attachments)
	}
}

// 写数组时必须把旧单值列清空。否则库里同时存在两份「事实」，回滚到旧二进制时
// 会按旧列下发一份陈旧文件 —— 而旧列这时候已经没人维护了。
func TestSkillAttachments_ArrayWriteClearsLegacyColumn(t *testing.T) {
	s := newStoreForAttachTest(t)
	if err := s.Create(&model.Skill{
		Slug: "清旧列", Name: "清旧列", Category: "general", Version: 1, Enabled: true,
		SkillType:   model.SkillTypeQuery,
		Attachment:  "source/旧文件.docx", // 调用方同时给了旧字段
		Attachments: []model.Attachment{{Name: "新表.docx", Rel: "attachments/新表.docx"}},
	}, nil); err != nil {
		t.Fatalf("注册技能失败: %v", err)
	}

	var legacy string
	var atts string
	if err := s.db.QueryRow(`SELECT attachment, attachments FROM skills WHERE slug=?`, "清旧列").
		Scan(&legacy, &atts); err != nil {
		t.Fatalf("直读 DB 失败: %v", err)
	}
	if legacy != "" {
		t.Errorf("数组列有内容时旧列仍留着 %q —— 库里出现了两份事实来源", legacy)
	}
	if !strings.Contains(atts, "attachments/新表.docx") {
		t.Errorf("数组列没写进去: %q", atts)
	}
}

// 只给旧字段（seed 与历史调用方就是这么干的）时，旧列照旧生效，读回来折成数组一项。
func TestSkillAttachments_LegacyOnlyWriteStillWorks(t *testing.T) {
	s := newStoreForAttachTest(t)
	if err := s.Create(&model.Skill{
		Slug: "只给旧字段", Name: "只给旧字段", Category: "general", Version: 1, Enabled: true,
		SkillType: model.SkillTypeTemplate, Attachment: "source/form.docx",
	}, nil); err != nil {
		t.Fatalf("注册技能失败: %v", err)
	}
	got, err := s.Get("只给旧字段")
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if len(got.Attachments) != 1 {
		t.Fatalf("旧字段没折成数组一项: %+v", got.Attachments)
	}
	if got.Attachments[0].Rel != "source/form.docx" || got.Attachments[0].Name != "form.docx" {
		t.Errorf("折叠结果不对: %+v", got.Attachments[0])
	}
}

// 线上已有行是老形态（只有 attachment 列的值）。升级只是「重启换二进制」，
// 没有人工跑迁移脚本这一步 —— 所以读路径必须自己折叠。
func TestSkillAttachments_ExistingLegacyRowReadsWithoutDataMigration(t *testing.T) {
	s := newStoreForAttachTest(t)
	// 模拟老二进制写下的行：只填 attachment，attachments 是默认空串。
	if _, err := s.db.Exec(`INSERT INTO skills(slug,name,description,category,version,enabled,is_core,skill_type,attachment)
		VALUES(?,?,?,?,?,?,?,?,?)`, "老行", "老行", "", "general", 3, 1, 0, model.SkillTypeTemplate, "source/报名表.docx"); err != nil {
		t.Fatalf("造老行失败: %v", err)
	}

	got, err := s.Get("老行")
	if err != nil {
		t.Fatalf("读回老行失败: %v", err)
	}
	if len(got.Attachments) != 1 || got.Attachments[0].Rel != "source/报名表.docx" {
		t.Fatalf("老行没被折成数组一项: %+v", got.Attachments)
	}
	if got.Attachments[0].Name != "报名表.docx" {
		t.Errorf("折叠出来的展示名不对: %q", got.Attachments[0].Name)
	}
	// 老行没被改写：折叠发生在读路径，不落库。
	var atts string
	if err := s.db.QueryRow(`SELECT attachments FROM skills WHERE slug=?`, "老行").Scan(&atts); err != nil {
		t.Fatalf("直读 DB 失败: %v", err)
	}
	if atts != "" {
		t.Errorf("读操作把老行改了（attachments=%q）—— 读路径不该有副作用", atts)
	}
}

// 升级真实存在的老库：表里根本没有 attachments 列。
// 这条尺子同时测两件事：迁移加上了列，且加完列之后老数据还能读。
func TestSkillAttachments_MigrationAddsColumnToLegacyDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")

	// ---- fixture：手造一个「老版本 schema」的库 ----
	// 刻意不 import internal/db，直接建表，保证测的是真·旧结构。
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("建老库失败: %v", err)
	}
	if _, err := legacy.Exec(`CREATE TABLE skills (
		slug TEXT PRIMARY KEY, name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
		category TEXT NOT NULL DEFAULT 'general', version INTEGER NOT NULL DEFAULT 1,
		enabled INTEGER NOT NULL DEFAULT 1, is_core INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		skill_type TEXT NOT NULL DEFAULT 'write', attachment TEXT NOT NULL DEFAULT '')`); err != nil {
		legacy.Close()
		t.Fatalf("建老表失败: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO skills(slug,name,skill_type,attachment) VALUES(?,?,?,?)`,
		"升级前技能", "升级前技能", model.SkillTypeTemplate, "source/旧模板.docx"); err != nil {
		legacy.Close()
		t.Fatalf("造老数据失败: %v", err)
	}
	legacy.Close()

	// fixture 自检：列必须真的不存在，否则这条尺子测的是「本来就有的列」，
	// 迁移漏写 ALTER 也照样绿（sf2 学到的教训：fixture 自己也得有断言）。
	if hasAttachColumn(t, path) {
		t.Fatal("fixture 不成立：手造的老库里已经有 attachments 列，测不出迁移")
	}

	// ---- 升级：只做「打开库」这一个动作 ----
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("打开（升级）老库失败: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if !hasAttachColumn(t, path) {
		t.Fatal("迁移没加上 attachments 列 —— 线上老库升级后 INSERT/UPDATE 会直接报 no such column")
	}
	s := NewSkillStore(d, dir)

	// 老数据读得出来，且折成了数组一项。
	old, err := s.Get("升级前技能")
	if err != nil {
		t.Fatalf("升级后读老技能失败: %v", err)
	}
	if len(old.Attachments) != 1 || old.Attachments[0].Rel != "source/旧模板.docx" {
		t.Fatalf("升级后老技能的附件不对: %+v", old.Attachments)
	}

	// 升级后的库能正常写数组。
	if err := s.Create(&model.Skill{
		Slug: "升级后新技能", Name: "升级后新技能", Category: "general", Version: 1, Enabled: true,
		SkillType:   model.SkillTypeQuery,
		Attachments: []model.Attachment{{Name: "新模板.docx", Rel: "attachments/新模板.docx"}},
	}, nil); err != nil {
		t.Fatalf("升级后写数组失败: %v", err)
	}
	nu, err := s.Get("升级后新技能")
	if err != nil {
		t.Fatalf("升级后读新技能失败: %v", err)
	}
	if len(nu.Attachments) != 1 || nu.Attachments[0].Name != "新模板.docx" {
		t.Fatalf("升级后新技能的附件不对: %+v", nu.Attachments)
	}
}

func hasAttachColumn(t *testing.T, dbPath string) bool {
	t.Helper()
	d, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("探测列失败: %v", err)
	}
	defer d.Close()
	rows, err := d.Query(`PRAGMA table_info(skills)`)
	if err != nil {
		t.Fatalf("PRAGMA 失败: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("扫 PRAGMA 行失败: %v", err)
		}
		if name == "attachments" {
			return true
		}
	}
	return false
}

// 一行脏 JSON 不能把整个技能读成错误：附件是附加品，技能本体（提示词/示例、
// 分类器要用的名字）才是主体。崩在这里等于「手动改坏一个字段，技能从 roster 里消失」。
func TestSkillAttachments_BadJSONDegradesToNoAttachments(t *testing.T) {
	s := newStoreForAttachTest(t)
	if _, err := s.db.Exec(`INSERT INTO skills(slug,name,description,category,version,enabled,is_core,skill_type,attachment,attachments)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, "脏数据", "脏数据技能", "", "general", 1, 1, 0, model.SkillTypeQuery, "", `{"oops":`); err != nil {
		t.Fatalf("造脏数据失败: %v", err)
	}
	got, err := s.Get("脏数据")
	if err != nil {
		t.Fatalf("脏 JSON 把整个技能读崩了: %v", err)
	}
	if len(got.Attachments) != 0 {
		t.Errorf("坏 JSON 应该退化成无附件，却读出 %+v", got.Attachments)
	}
	if got.Name != "脏数据技能" {
		t.Errorf("技能本体信息没读出来: %q", got.Name)
	}
	// 反向守卫：同一次查询里另一个正常技能的附件不受影响。
	if err := s.Create(&model.Skill{
		Slug: "干净数据", Name: "干净数据", Category: "general", Version: 1, Enabled: true,
		SkillType:   model.SkillTypeQuery,
		Attachments: []model.Attachment{{Name: "ok.docx", Rel: "attachments/ok.docx"}},
	}, nil); err != nil {
		t.Fatalf("注册干净技能失败: %v", err)
	}
	if all, err := s.List(); err != nil {
		t.Fatalf("List 失败: %v", err)
	} else {
		for _, sk := range all {
			if sk.Slug == "干净数据" && len(sk.Attachments) != 1 {
				t.Errorf("脏行影响到了别的技能: %+v", sk.Attachments)
			}
		}
	}
}

// 没有 Rel 的条目下发出去必然是死链（点了 404/空文件）。宁可少一条，
// 也不给用户一个点开是空的名字 —— 那会让用户怀疑整套提取是坏的。
func TestSkillAttachments_EntryWithoutRelIsDropped(t *testing.T) {
	s := newStoreForAttachTest(t)
	raw := `[{"name":"只有名字没有路径.docx"},{"name":"好的.docx","rel":"attachments/好的.docx"}]`
	if _, err := s.db.Exec(`INSERT INTO skills(slug,name,description,category,version,enabled,is_core,skill_type,attachment,attachments)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, "半条", "半条", "", "general", 1, 1, 0, model.SkillTypeQuery, "", raw); err != nil {
		t.Fatalf("造数据失败: %v", err)
	}
	got, err := s.Get("半条")
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if len(got.Attachments) != 1 {
		t.Fatalf("应该只留下有 Rel 的那一条，实际 %+v", got.Attachments)
	}
	if got.Attachments[0].Rel != "attachments/好的.docx" {
		t.Errorf("留下的那条不对: %+v", got.Attachments[0])
	}
}

// Name 缺失时用 Rel 的文件名兜底：前端/消息里要显示的可读名字不能是空。
func TestSkillAttachments_NameFallsBackToRelBase(t *testing.T) {
	s := newStoreForAttachTest(t)
	if _, err := s.db.Exec(`INSERT INTO skills(slug,name,description,category,version,enabled,is_core,skill_type,attachment,attachments)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, "无名", "无名", "", "general", 1, 1, 0, model.SkillTypeQuery, "", `[{"rel":"attachments/报名表.docx"}]`); err != nil {
		t.Fatalf("造数据失败: %v", err)
	}
	got, err := s.Get("无名")
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if len(got.Attachments) != 1 || got.Attachments[0].Name != "报名表.docx" {
		t.Fatalf("展示名兜底失败: %+v", got.Attachments)
	}
}
