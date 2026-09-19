package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// genCache holds freshly generated office files so the SSE/chat handler can
// hand them to the client via a download endpoint.
//
// 线上证据（这个缺陷的起因，别把内存 map 改回唯一真相）：交付物只存在进程内存里时，
// 一次部署重启后，刚发给用户的链接立刻 404 —— 实测对几秒钟前才发出去的 token 发
// GET /api/chat/gen/02a891a33508ecf6ec7e0b9bb943ed56，返回 HTTP 404。
// 用户什么时候点「下载」我们控制不了（翻聊天记录再点、手机放一会儿再点），
// 而重启（部署、崩溃拉起、systemd restart）却是常态，所以内存只能是快路径。
//
// 结构 = 内存快路径 + 磁盘持久化：
//   - 内存命中 → 直接返回（同一次会话里的重复点击 / 浏览器刷新走这条）；
//   - 内存未命中（典型场景就是重启之后，内存必然是空的）→ 回落读盘，
//     读到且未过期就返回并回填内存；
//   - 两边都没有 / 已过期 → 404。
type genEntry struct {
	name string
	ct   string
	data []byte
	exp  time.Time
}

// genMeta 是落盘的元信息，与字节文件分开存：判断是否过期只需要读这个小的，
// 不必把几百 KB 的 docx 读进内存。
type genMeta struct {
	Name string    `json:"name"`
	CT   string    `json:"ct"`
	Exp  time.Time `json:"exp"`
}

// genTTL 是交付物的存活期。从原来的 15 分钟放宽到 24 小时，理由：
//  1. 用户生成后可能隔一段时间才点下载（翻回聊天记录、手机上过一会儿再点），
//     15 分钟太短，失效的表现就是「明明刚生成却说文件不存在」；
//  2. 部署重启不该让已经发出去的链接失效（见类型注释里的 404 证据）——重启后
//     条目只剩磁盘那一份，TTL 比一次部署的间隔长才有意义；
//  3. token 是 16 字节随机数（randomToken(16) → 32 位 hex），不可枚举，
//     窗口内重复读不构成泄露；代价只是文件在磁盘上多留一会儿，而每次 put 都会
//     清扫过期条目（sweepLocked），不会无限堆积。
const genTTL = 24 * time.Hour

const (
	// genTokenBytes 是 token 的随机字节数；genTokenHexLen 是它的 hex 长度。
	// get 用这个形状做校验，它同时也是防路径穿越的闸门（token 会被直接拼成文件名）。
	genTokenBytes  = 16
	genTokenHexLen = genTokenBytes * 2
	genDataSuffix  = ".bin"
	genMetaSuffix  = ".json"
)

type genCache struct {
	mu  sync.Mutex
	m   map[string]genEntry
	dir string // 落盘目录；空串 = 纯内存模式（目录不可用时也会退化成这个）
	now func() time.Time
}

// newGenCache 建纯内存缓存（不落盘）。
// 保留这个签名：多处测试按它构造，且「不想落盘」的场景仍然合法。
// 带磁盘持久化的版本见 newGenCacheWithDir。
func newGenCache() *genCache {
	return &genCache{m: make(map[string]genEntry), now: time.Now}
}

// newGenCacheWithDir 建「内存 + 磁盘」缓存，条目落在 dir 下，每条两个文件：
// <token>.bin（字节）+ <token>.json（元信息）。
//
// dir 为空串、或目录建不出来时必须优雅退化为纯内存模式：交付物能不能下载是
// 业务路径，不该因为磁盘不可写（只读挂载、配额满、权限不对）把服务或请求拖垮，
// 更不能 panic。退化后行为与 newGenCache() 完全一致，只差重启会丢。
func newGenCacheWithDir(dir string) *genCache {
	g := &genCache{m: make(map[string]genEntry), now: time.Now}
	if dir == "" {
		return g
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "[gen] 交付物落盘目录 %s 不可用，退化为纯内存保存（重启后这些链接会失效）：%v\n", dir, err)
		return g
	}
	g.dir = dir
	return g
}

// put stores a file and returns a random unguessable token.
//
// 先落盘再进内存：反过来的话，磁盘写失败的条目会先在内存里短暂可见，
// 重启后表现成「有时能下、有时 404」的薛定谔状态，最难排查。
// 落盘失败不阻断本次对话：这一份仍走内存，只打一行告警。
func (g *genCache) put(name, ct string, data []byte) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweepLocked()
	tok := randomToken(genTokenBytes)
	e := genEntry{name: name, ct: ct, data: data, exp: g.now().Add(genTTL)}
	if g.dir != "" {
		if err := g.storeLocked(tok, e); err != nil {
			fmt.Fprintf(os.Stderr, "[gen] 交付物 %s 落盘失败，仅内存可见（重启后该链接会失效）：%v\n", name, err)
		}
	}
	g.m[tok] = e
	return tok
}

// get fetches a file by token. The entry stays readable until it expires:
// a browser refresh, a second click, or a gateway prefetch + user click must
// all be able to fetch the same bytes. The token carries 16 random bytes, so
// repeated reads are not a meaningful leak, and TTL still bounds memory.
//
// 内存未命中时回落读盘 —— 重启后内存必然是空的，已经发出去的链接能不能用，
// 全看这一条路径。
func (g *genCache) get(tok string) (genEntry, bool) {
	if !validToken(tok) {
		// 形状不对（含 "/"、"."、"%"、绝对路径、长度不对……）一律当作不存在：
		// token 会被拼进文件路径，这里是唯一的防穿越闸门。
		return genEntry{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if e, ok := g.m[tok]; ok {
		if !g.now().After(e.exp) {
			return e, true
		}
		delete(g.m, tok) // 内存里的过期条目立刻失效
	}
	e, ok := g.loadLocked(tok)
	if !ok {
		return genEntry{}, false
	}
	if g.now().After(e.exp) {
		// 过期了顺手把文件删掉：否则要等下一次 put 的清扫才扫得到，
		// 期间磁盘上留着一份永远不会再被读到的死数据。
		g.removeFilesLocked(tok)
		return genEntry{}, false
	}
	g.m[tok] = e // 回填内存：同一次会话里的重复下载走快路径
	return e, true
}

// sweepLocked removes expired entries from memory and disk. Caller must hold mu.
// 清扫时机保持原样（每次 put）：读路径不该为了删文件去遍历整个目录。
func (g *genCache) sweepLocked() {
	now := g.now()
	for k, e := range g.m {
		if now.After(e.exp) {
			delete(g.m, k)
			g.removeFilesLocked(k)
		}
	}
	g.sweepDiskLocked(now)
}

// sweepDiskLocked 删掉落盘目录里已经过期的 <token>.bin/.json。
// 只认「文件名就是一个合法 token + 已知后缀」的文件，其它文件一律不碰：
// 同一个目录里将来可能放别的东西，清扫绝不能误删（这个目录是按 DataDir/gen 约定的，
// 不是我们独占的沙箱）。
func (g *genCache) sweepDiskLocked(now time.Time) {
	if g.dir == "" {
		return
	}
	ents, err := os.ReadDir(g.dir)
	if err != nil {
		// 目录读不了就算了，下次 put 再试：清扫失败不该影响生成本身。
		return
	}
	for _, ent := range ents {
		tok, ok := tokenFromFilename(ent.Name())
		if !ok {
			continue
		}
		meta, ok := g.loadMetaLocked(tok)
		if !ok || now.After(meta.Exp) {
			// meta 读不到/解析不了（半截文件、手动改坏）也当过期清掉：
			// 读不出 name/ct/exp 的字节没有任何再用价值。
			g.removeFilesLocked(tok)
		}
	}
}

// storeLocked 把一条交付物写盘：先 <token>.bin 再 <token>.json（顺序不能反，
// 否则崩在中间会留下「有 meta 没字节」的条目，下载出来是空文件）。
// Caller must hold mu.
func (g *genCache) storeLocked(tok string, e genEntry) error {
	meta, err := json.Marshal(genMeta{Name: e.name, CT: e.ct, Exp: e.exp})
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(g.dir, tok+genDataSuffix), e.data); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(g.dir, tok+genMetaSuffix), meta)
}

// loadLocked 从磁盘读回一条交付物。meta 或字节缺一个都算没有 ——
// 半个条目读出来只会得到一个坏文件。Caller must hold mu.
func (g *genCache) loadLocked(tok string) (genEntry, bool) {
	if g.dir == "" {
		return genEntry{}, false
	}
	meta, ok := g.loadMetaLocked(tok)
	if !ok {
		return genEntry{}, false
	}
	data, err := os.ReadFile(filepath.Join(g.dir, tok+genDataSuffix))
	if err != nil {
		return genEntry{}, false
	}
	return genEntry{name: meta.Name, ct: meta.CT, data: data, exp: meta.Exp}, true
}

// loadMetaLocked 只读元信息。Caller must hold mu.
func (g *genCache) loadMetaLocked(tok string) (genMeta, bool) {
	raw, err := os.ReadFile(filepath.Join(g.dir, tok+genMetaSuffix))
	if err != nil {
		return genMeta{}, false
	}
	var meta genMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return genMeta{}, false
	}
	return meta, true
}

// removeFilesLocked 删掉一个条目的两个文件（缺了就当已删）。Caller must hold mu.
func (g *genCache) removeFilesLocked(tok string) {
	if g.dir == "" || !validToken(tok) {
		return
	}
	_ = os.Remove(filepath.Join(g.dir, tok+genDataSuffix))
	_ = os.Remove(filepath.Join(g.dir, tok+genMetaSuffix))
}

// tokenFromFilename 从 <token>.bin / <token>.json 里取出 token；
// 不是这两种形状（临时文件、别人放进来的东西）就返回 false。
func tokenFromFilename(name string) (string, bool) {
	for _, suf := range []string{genDataSuffix, genMetaSuffix} {
		if strings.HasSuffix(name, suf) {
			tok := strings.TrimSuffix(name, suf)
			if validToken(tok) {
				return tok, true
			}
		}
	}
	return "", false
}

// validToken 校验 token 形状：只接受 randomToken(16) 产出的 32 位小写 hex。
// token 来自 URL 路径且会被直接拼进文件名，所以必须在这里挡住 "../"、"..%2f"、
// 绝对路径等一切非法形状。校验放在 get 入口，任何读路径都绕不过去；
// 非法 token 与「不存在」返回同一个结果（404），不给探测者任何区别。
func validToken(tok string) bool {
	if len(tok) != genTokenHexLen {
		return false
	}
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

// writeFileAtomic 先写同目录下的临时文件再 rename 过去：同目录才保证 rename 是原子的。
// 为什么要原子：部署重启/进程被杀正好落在写入中途时，绝不能让目录里出现半截文件 ——
// 半截 docx 比 404 更难查（用户拿到的是打不开的文件，我们这边看不到任何错误）。
func writeFileAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// randomToken returns a URL-safe random hex string of n bytes.
func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// extremely unlikely; fall back to a time-based token
		return hex.EncodeToString([]byte(time.Now().Format("150405.000000000")))
	}
	return hex.EncodeToString(b)
}

// GenFile serves a generated office file by its one-time token.
func (h *Handler) GenFile(g *genCache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := r.PathValue("token")
		if tok == "" {
			http.Error(w, "missing token", http.StatusBadRequest)
			return
		}
		// 重启后这里靠磁盘命中（见 genCache 的类型注释）：以前只查内存，
		// 一次部署就让所有刚发出去的下载链接变成 404。
		e, ok := g.get(tok)
		if !ok {
			http.Error(w, "generated file expired or not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", e.ct)
		w.Header().Set("Content-Disposition", "attachment; filename="+quoteEscaped(e.name))
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(e.data)
	}
}
