package llm

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/lizhemin15/skillforge/internal/model"
)

// TestProbeReasoningFromActiveProvider 是「材料为什么一片都没滚」的定位探针：
// 走**真**的 llm.Client（真 SDK、真 HTTP、真 TLS 配置）打**真**的生效 provider，
// 数 onReasoning 收到多少片、第一片在第几秒。
//
// 为什么需要它：裸 HTTP 探针（scripts/probe_provider_db.py）已经证明 provider 从
// 0.6s 起就推 reasoning_content（1517 片），但线上用户在那几十秒里一片材料都没看到。
// 两句话只能有一句是真的，夹在中间的就是 Client/CompleteEx 这一层。本探针就是那把卡尺：
//   - 这里有片 → 故障在更上层（clock.Thinking 的丢弃规则 / ctx 里的 sink 丢了）
//   - 这里没片 → 故障就在这一层（SDK 解析、请求参数、超时）
//
// 默认跳过（要真打 provider、要花几十秒），显式开：
//
//	SKILLFORGE_PROBE_REASONING=1 go test ./internal/llm/ -run TestProbeReasoning -v -timeout 300s
//
// 配置从 DB 的 llm_config（is_active=1）读——env 里那套是 is_active=0 的旧配置，
// 拿 env 跑会得出「provider 地址 404」这种与线上无关的结论（踩过）。
func TestProbeReasoningFromActiveProvider(t *testing.T) {
	if os.Getenv("SKILLFORGE_PROBE_REASONING") != "1" {
		t.Skip("探针默认不跑：SKILLFORGE_PROBE_REASONING=1 才打真 provider")
	}
	dbPath := os.Getenv("SKILLFORGE_DB")
	if dbPath == "" {
		dbPath = "/opt/skillforge/data/skillforge.db"
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("打开 DB 失败：%v", err)
	}
	defer db.Close()

	var cfg model.LLMConfig
	row := db.QueryRow(`select provider, base_url, api_key, model from llm_config where is_active=1 limit 1`)
	if err := row.Scan(&cfg.Provider, &cfg.BaseURL, &cfg.APIKey, &cfg.Model); err != nil {
		t.Fatalf("读 llm_config 失败：%v", err)
	}
	if cfg.APIKey == "" {
		t.Fatal("生效配置没有凭据，探针无意义")
	}
	t.Logf("生效配置：provider=%s model=%s base=%s 凭据长度=%d",
		cfg.Provider, cfg.Model, cfg.BaseURL, len(cfg.APIKey))

	cli := New(&cfg)
	start := time.Now()
	var (
		rPieces, rChars int
		rFirst          time.Duration
		cPieces, cChars int
		cFirst          time.Duration
	)
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	out, err := cli.CompleteEx(ctx,
		"你是资深记者。严格按要求写作。",
		"写一篇 300 字左右的新闻稿，主题：某市地铁 5 号线今日开通。直接输出正文，不要标题以外的解释。",
		func(d string) {
			if cPieces == 0 {
				cFirst = time.Since(start)
			}
			cPieces++
			cChars += len([]rune(d))
		},
		func(r string) {
			if rPieces == 0 {
				rFirst = time.Since(start)
			}
			rPieces++
			rChars += len([]rune(r))
		})
	total := time.Since(start)
	if err != nil {
		t.Logf("调用报错：%v", err)
	}
	t.Logf("思考链：%d 片 / %d 字 / 首片 %.1fs", rPieces, rChars, rFirst.Seconds())
	t.Logf("正文  ：%d 片 / %d 字 / 首片 %.1fs", cPieces, cChars, cFirst.Seconds())
	t.Logf("总耗时：%.1fs，正文长度 %d 字", total.Seconds(), len([]rune(out)))

	if rPieces == 0 {
		t.Errorf("这一层就一片思考链都收不到 → 故障在 llm.Client/CompleteEx（SDK 或请求参数），不是上层展示")
	} else {
		t.Logf("这一层能收到思考链 → 故障在更上层：clock.Thinking 的丢弃规则或 ctx 里的 sink 没了")
	}
}
