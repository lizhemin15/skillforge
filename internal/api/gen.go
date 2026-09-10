package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

// genCache holds freshly generated office files so the SSE/chat handler can
// hand them to the client via a short-lived download endpoint, without
// touching the filesystem. Entries expire after genTTL to bound memory.
type genEntry struct {
	name string
	ct   string
	data []byte
	exp  time.Time
}

const genTTL = 15 * time.Minute

type genCache struct {
	mu  sync.Mutex
	m   map[string]genEntry
	now func() time.Time
}

func newGenCache() *genCache {
	return &genCache{m: make(map[string]genEntry), now: time.Now}
}

// put stores a file and returns a random unguessable token.
func (g *genCache) put(name, ct string, data []byte) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweepLocked()
	tok := randomToken(16)
	g.m[tok] = genEntry{name: name, ct: ct, data: data, exp: g.now().Add(genTTL)}
	return tok
}

// get fetches a file by token. The entry stays readable until it expires:
// a browser refresh, a second click, or a gateway prefetch + user click must
// all be able to fetch the same bytes. The token carries 16 random bytes, so
// repeated reads are not a meaningful leak, and TTL still bounds memory.
func (g *genCache) get(tok string) (genEntry, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.m[tok]
	if !ok {
		return genEntry{}, false
	}
	if g.now().After(e.exp) {
		delete(g.m, tok)
		return genEntry{}, false
	}
	return e, true
}

// sweepLocked removes expired entries. Caller must hold mu.
func (g *genCache) sweepLocked() {
	now := g.now()
	for k, e := range g.m {
		if now.After(e.exp) {
			delete(g.m, k)
		}
	}
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
