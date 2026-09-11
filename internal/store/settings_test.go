package store

import (
	"path/filepath"
	"testing"

	"github.com/lizhemin15/skillforge/internal/db"
)

// newStoreForTest 建一个落临时目录的真 sqlite 技能库（真库而非 mock：
// settings 的 upsert/删除语义全在 SQL 里，mock 掉等于什么都没测）。
func newStoreForTest(t *testing.T, dataDir string) *SkillStore {
	t.Helper()
	d, err := db.Open(filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatalf("建测试库失败：%v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return NewSkillStore(d, dataDir)
}

// settings 表（通用 key/value）的回归防线。
//
// 守三条契约：
//   - 全表为空时读出默认值（老库升级上来不能白屏）；
//   - SetSettings 是 upsert，重复写不报错、值以最后一次为准；
//   - ResetSettings 删键 → 读回来回落默认；并且 **GetSettingsRaw 能区分
//     「删了键」和「存了空串」**（is_custom 判据依赖它）。

func TestSettingsDefaultsOnEmptyTable(t *testing.T) {
	s := newStoreForTest(t, t.TempDir())
	got, err := s.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings 出错：%v", err)
	}
	if got[SettingSiteName] != DefaultSiteName {
		t.Fatalf("空表应回落默认站名 %q，实际 %q", DefaultSiteName, got[SettingSiteName])
	}
	if got[SettingSiteTagline] != DefaultSiteTagline {
		t.Fatalf("空表应回落默认副标题 %q，实际 %q", DefaultSiteTagline, got[SettingSiteTagline])
	}
	// 没登记默认值的键：显式点名去读，应得空串且不报错（不是「读不到」，是「读出来是空」）。
	unknown, err := s.GetSettings("no_such_key")
	if err != nil {
		t.Fatalf("读未登记的键不该报错：%v", err)
	}
	if v := unknown["no_such_key"]; v != "" {
		t.Fatalf("未登记默认值的键应为空串，实际 %q", v)
	}
	raw, err := s.GetSettingsRaw(SettingSiteName, SettingSiteTagline)
	if err != nil {
		t.Fatalf("GetSettingsRaw 出错：%v", err)
	}
	if len(raw) != 0 {
		t.Fatalf("空表时 raw 应为空 map（不铺默认值），实际 %v", raw)
	}
}

func TestSettingsUpsertAndReset(t *testing.T) {
	s := newStoreForTest(t, t.TempDir())
	if err := s.SetSettings(map[string]string{SettingSiteName: "甲"}); err != nil {
		t.Fatalf("首次写入失败：%v", err)
	}
	// 空 map 必须是合法 no-op（调用方只改一个字段时另一个就是空的）。
	if err := s.SetSettings(nil); err != nil {
		t.Fatalf("空 map 应合法 no-op，实际报错：%v", err)
	}
	if err := s.SetSettings(map[string]string{SettingSiteName: "乙"}); err != nil {
		t.Fatalf("覆盖写入失败：%v", err)
	}
	// 两个键一起读：只写 name 不能把 tagline 的默认值弄丢。
	got, err := s.GetSettings(SettingSiteName, SettingSiteTagline)
	if err != nil {
		t.Fatalf("GetSettings 出错：%v", err)
	}
	if got[SettingSiteName] != "乙" {
		t.Fatalf("upsert 后应为最后一次写入的 乙，实际 %q", got[SettingSiteName])
	}
	if got[SettingSiteTagline] != DefaultSiteTagline {
		t.Fatalf("未写入的 tagline 应仍是默认值，实际 %q", got[SettingSiteTagline])
	}

	if err := s.ResetSettings(SettingSiteName); err != nil {
		t.Fatalf("ResetSettings 出错：%v", err)
	}
	got, err = s.GetSettings(SettingSiteName)
	if err != nil {
		t.Fatalf("ResetSettings 后 GetSettings 出错：%v", err)
	}
	if got[SettingSiteName] != DefaultSiteName {
		t.Fatalf("删键后应回落默认值，实际 %q", got[SettingSiteName])
	}
	raw, _ := s.GetSettingsRaw(SettingSiteName)
	if _, exists := raw[SettingSiteName]; exists {
		t.Fatalf("删键后 raw 里不该还有这个键，实际 %v", raw)
	}
}

// TestSettingsRawDistinguishesEmptyFromMissing 是 is_custom 判据的地基：
// 「存了空串」和「没这个键」必须能分开 —— 否则「恢复默认」会被误报成「已自定义」。
func TestSettingsRawDistinguishesEmptyFromMissing(t *testing.T) {
	s := newStoreForTest(t, t.TempDir())
	if err := s.SetSettings(map[string]string{SettingSiteName: ""}); err != nil {
		t.Fatalf("写入空串失败：%v", err)
	}
	raw, err := s.GetSettingsRaw(SettingSiteName, SettingSiteTagline)
	if err != nil {
		t.Fatalf("GetSettingsRaw 出错：%v", err)
	}
	if v, ok := raw[SettingSiteName]; !ok || v != "" {
		t.Fatalf("存了空串时 raw 应带回该键且值为空，实际 ok=%v v=%q", ok, v)
	}
	if _, ok := raw[SettingSiteTagline]; ok {
		t.Fatalf("没写过的键不该出现在 raw 里，实际 %v", raw)
	}
}

func TestSettingsRejectsBlankKey(t *testing.T) {
	s := newStoreForTest(t, t.TempDir())
	if err := s.SetSettings(map[string]string{"   ": "x"}); err == nil {
		t.Fatalf("空键应被拒绝")
	}
}
