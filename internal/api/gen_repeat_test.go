package api

import (
	"testing"
	"time"
)

// TestGenCacheRepeatReads 锁死「下载链接可刷新」：同一 token 多次下载都必须成功，
// 过期后才 404。旧实现是一次即焚，浏览器刷新 / 网关预取后再点都会 404。
func TestGenCacheRepeatReads(t *testing.T) {
	g := newGenCache()
	base := time.Now()
	g.now = func() time.Time { return base }

	tok := g.put("采购清单.xlsx", "application/vnd.ms-excel", []byte("PK\x03\x04data"))
	for i := 1; i <= 3; i++ {
		e, ok := g.get(tok)
		if !ok {
			t.Fatalf("第 %d 次下载应当成功（可重复读），实际 404", i)
		}
		if string(e.data) != "PK\x03\x04data" || e.name != "采购清单.xlsx" {
			t.Fatalf("第 %d 次内容被破坏: %+v", i, e)
		}
	}

	// 过期后必须失效（内存有界）
	g.now = func() time.Time { return base.Add(genTTL + time.Second) }
	if _, ok := g.get(tok); ok {
		t.Fatal("过期后仍可下载，TTL 失效")
	}
	if _, ok := g.get("不存在的token"); ok {
		t.Fatal("未知 token 不该命中")
	}
}
