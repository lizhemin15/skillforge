package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 落盘缓存（内存快路径 + 磁盘持久化）的测试。
//
// 为什么要有这个文件：线上证据——一次部署重启后，对几秒钟前才发给用户的 token 发
// GET /api/chat/gen/02a891a33508ecf6ec7e0b9bb943ed56，返回 HTTP 404。
// 交付物只存内存时，重启（部署/崩溃拉起/systemd restart）就等于作废所有下载链接。
// 下面的测试把「重启后还能下载」「过期真的失效」「token 不能穿越目录」「目录不可用
// 要优雅退化」「过期文件要清扫」五条钉死。

const genDiskCT = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

// genDiskDir 返回一个还没建出来的落盘目录（等价于 cfg.DataDir/gen，
// 用 t.TempDir() 是因为测试不该往真实 DataDir 里写东西）。
func genDiskDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "gen")
}

// plantGenDiskEntry 绕过 put 直接往盘上写一个条目：用来模拟「上一个进程留下的文件」
// 以及「本来就不该被读到的文件」。token 与文件名的拼接规则和 storeLocked 一致。
func plantGenDiskEntry(t *testing.T, dir, tok, name, ct string, data []byte, exp time.Time) {
	t.Helper()
	mkdirAll(t, dir)
	meta, err := json.Marshal(genMeta{Name: name, CT: ct, Exp: exp})
	if err != nil {
		t.Fatalf("序列化 meta 失败：%v", err)
	}
	writeBytes(t, filepath.Join(dir, tok+genDataSuffix), data)
	writeBytes(t, filepath.Join(dir, tok+genMetaSuffix), meta)
}

// mustBeGone 断言文件不存在（缺失之外的任何 err 也算失败，避免把权限问题读成「已删」）。
func mustBeGone(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s：文件 %s 应当已被删除，实际 err=%v", why, path, err)
	}
}

// TestGenCacheSurvivesRestart 本缺陷的主测试：进程重启（内存缓存全新一个）之后，
// 之前发出去的 token 必须还能读出同样的 name / ct / 字节。
func TestGenCacheSurvivesRestart(t *testing.T) {
	dir := genDiskDir(t)
	data := []byte("PK\x03\x04 采购清单 docx 的字节")

	before := newGenCacheWithDir(dir)
	tok := before.put("采购清单.docx", genDiskCT, data)

	// 模拟部署重启：内存全丢，只剩目录里的文件
	after := newGenCacheWithDir(dir)
	e, ok := after.get(tok)
	if !ok {
		t.Fatalf("重启后 token %s 读不回来 —— 这正是线上那个 404", tok)
	}
	if e.name != "采购清单.docx" {
		t.Fatalf("重启后文件名不一致：got %q want %q", e.name, "采购清单.docx")
	}
	if e.ct != genDiskCT {
		t.Fatalf("重启后 Content-Type 不一致：got %q want %q", e.ct, genDiskCT)
	}
	if string(e.data) != string(data) {
		t.Fatalf("重启后字节不一致：got %q want %q", e.data, data)
	}
}

// TestGenCacheDiskExpiredEntryIs404 落盘条目过期后必须 404，且最终从盘上清掉
// （磁盘不是只增不减的垃圾场）。
func TestGenCacheDiskExpiredEntryIs404(t *testing.T) {
	dir := genDiskDir(t)
	g := newGenCacheWithDir(dir)
	base := time.Now()
	g.now = func() time.Time { return base }

	// 形状必须和 randomToken(16) 产出的完全同形（32 位小写 hex）：否则会被
	// validToken 挡在门外，这条测试就变成「因为 token 非法而 404」的空转绿灯。
	tok := strings.Repeat("ab", genTokenHexLen/2)
	plantGenDiskEntry(t, dir, tok, "旧文档.docx", genDiskCT, []byte("OLD"), base.Add(-time.Hour))

	if _, ok := g.get(tok); ok {
		t.Fatalf("磁盘上已过期的条目仍被读出：token=%s", tok)
	}

	// 「最终被清掉」：不论这次 get 顺手删了还是下一次清扫删了，结束时两个文件都必须消失
	g.sweepLocked()
	mustBeGone(t, filepath.Join(dir, tok+genDataSuffix), "过期条目清扫")
	mustBeGone(t, filepath.Join(dir, tok+genMetaSuffix), "过期条目清扫")
}

// TestGenCacheRejectsPathTraversalToken token 来自 URL 路径且会被直接拼成文件名，
// 非法的必须一律当作不存在。
//
// 为了让这条测试不是「空转绿灯」，在缓存目录的**上一级**放一个真有内容的条目：
// 如果 token 形状校验被绕过，get("../secret") 就会把它当成命中读出来
// —— 断言失败即证明闸门真的在挡，而不是碰巧文件不存在。
func TestGenCacheRejectsPathTraversalToken(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "gen")
	mkdirAll(t, dir)
	plantGenDiskEntry(t, base, "secret", "不该被读到的文件.txt", "text/plain",
		[]byte("TOP SECRET"), time.Now().Add(time.Hour))

	g := newGenCacheWithDir(dir)
	bad := []string{
		"../../etc/passwd",
		"..%2f..",
		"../secret", // 命中上一级那个真文件：没有校验就会被读出来
		"..",
		"a/b",
		"",
		strings.Repeat("a", genTokenHexLen-1), // 长度不对
		strings.Repeat("a", genTokenHexLen+1),
		strings.Repeat("A", genTokenHexLen), // 大写 hex 也不接受（randomToken 只产小写）
		strings.Repeat("g", genTokenHexLen), // 非 hex 字符
		strings.Repeat("f", genTokenHexLen), // 形状合法但不存在：同样 404
	}
	for _, tok := range bad {
		if _, ok := g.get(tok); ok {
			t.Fatalf("非法/不存在的 token %q 被读出来了，路径穿越闸门失效", tok)
		}
	}
}

// TestGenCacheFallsBackToMemory 落盘不可用时必须优雅退化为纯内存模式：能 put/get、
// 不 panic、不报错阻断。两种「不可用」都要覆盖——dir 为空串（显式不落盘）
// 和目录根本建不出来（MkdirAll 失败，比如路径上一级是个普通文件）。
func TestGenCacheFallsBackToMemory(t *testing.T) {
	cases := []struct {
		name string
		dir  string
	}{
		{"dir 为空串", ""},
		{"目录建不出来", filepath.Join(blockerFile(t), "gen")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGenCacheWithDir(tc.dir)
			if g.dir != "" {
				t.Fatalf("落盘不可用时应当退化为纯内存（dir 为空），实际 dir=%q", g.dir)
			}
			tok := g.put("退化.docx", genDiskCT, []byte("MEM"))
			e, ok := g.get(tok)
			if !ok {
				t.Fatal("退化为纯内存后，put 进去的条目读不回来")
			}
			if e.name != "退化.docx" || string(e.data) != "MEM" {
				t.Fatalf("纯内存路径内容被破坏：name=%q data=%q", e.name, e.data)
			}
		})
	}
}

// blockerFile 返回一个「普通文件」的路径：拿它当目录用，MkdirAll 必然失败（ENOTDIR）。
func blockerFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "blocker")
	writeBytes(t, p, []byte("我是一个文件，不是目录"))
	return p
}

// TestGenCacheSweepDropsExpiredDiskFiles put 触发的清扫必须同时清内存和磁盘上的过期
// 条目，且不能误删未过期的。
//
// 过期条目用 plantGenDiskEntry 造（它等价于「上一个进程留下的文件」），
// 这样这条测试锚的就是清扫本身，而不是「put 一定会写盘」——写盘那条由重启测试锚。
func TestGenCacheSweepDropsExpiredDiskFiles(t *testing.T) {
	dir := genDiskDir(t)
	g := newGenCacheWithDir(dir)
	base := time.Now()
	g.now = func() time.Time { return base }

	oldTok := strings.Repeat("a", genTokenHexLen)
	liveTok := strings.Repeat("b", genTokenHexLen)
	plantGenDiskEntry(t, dir, oldTok, "旧文档.docx", genDiskCT, []byte("OLD"), base.Add(-time.Minute))
	plantGenDiskEntry(t, dir, liveTok, "在用的.docx", genDiskCT, []byte("LIVE"), base.Add(time.Hour))

	// put 是清扫的触发点（时机与原实现一致：每次 put 扫一次）
	g.put("刚生成的.docx", genDiskCT, []byte("PUT"))

	mustBeGone(t, filepath.Join(dir, oldTok+genDataSuffix), "put 清扫过期条目")
	mustBeGone(t, filepath.Join(dir, oldTok+genMetaSuffix), "put 清扫过期条目")
	if _, err := os.Stat(filepath.Join(dir, liveTok+genDataSuffix)); err != nil {
		t.Fatalf("未过期的条目被清扫误删：%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, liveTok+genMetaSuffix)); err != nil {
		t.Fatalf("未过期的条目（meta）被清扫误删：%v", err)
	}
}
