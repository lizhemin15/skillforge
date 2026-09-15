package llm

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lizhemin15/skillforge/internal/model"
)

// TestProbeJSONStreaming 是**实测探针**，不是回归断言：默认跳过，只有显式
// SKILLFORGE_LLM_PROBE=1 时才真打线上 provider（要 SKILLFORGE_LLM_* 环境变量）。
//
// 它回答一个决定修法可行性的问题：**JSON 模式下 provider 是否按片段流式吐 content**。
// 有的网关会把 response_format=json_object 的响应攒到最后一次性下发 —— 那样就
// 没有「中间材料」可流，任何基于 OnContent 的进度展示都是零帧，只是在最后
// 23.7s 那一瞬间补齐。不先量这个就动手，等于把「屏幕上有内容在动」押在猜测上。
//
// 输出只看时序与长度，不回显任何凭据。
func TestProbeJSONStreaming(t *testing.T) {
	if os.Getenv("SKILLFORGE_LLM_PROBE") != "1" {
		t.Skip("探针：设 SKILLFORGE_LLM_PROBE=1 才真打 provider")
	}
	cfg := &model.LLMConfig{
		Provider: os.Getenv("SKILLFORGE_LLM_PROVIDER"),
		BaseURL:  os.Getenv("SKILLFORGE_LLM_BASE_URL"),
		APIKey:   os.Getenv("SKILLFORGE_LLM_API_KEY"),
		Model:    os.Getenv("SKILLFORGE_LLM_MODEL"),
	}
	if strings.TrimSpace(cfg.APIKey) == "" || strings.TrimSpace(cfg.Model) == "" {
		t.Skip("缺 SKILLFORGE_LLM_API_KEY / SKILLFORGE_LLM_MODEL，跳过探针")
	}
	t.Logf("provider=%s model=%s base=%s（key 长度 %d，不回显）",
		cfg.Provider, cfg.Model, cfg.BaseURL, len(cfg.APIKey))

	// 与 docgen 那一跳同构：JSON 模式 + 关思考链（astron 上 reasoning_effort=none 有效）。
	sys := "你是办公文档生成器。只输出一个 JSON 对象，字段：" +
		`{"format":"word","filename":"x.docx","title":"…","parags":["…","…"]}。` +
		"parags 是正文段落数组，每段 60~120 字，共 12 段。不要输出任何其他文字。"
	user := "写一份关于开展数据治理专项工作的通知"

	start := time.Now()
	var cChunks, rChunks, cBytes int
	var firstC, firstR time.Duration
	var gapMax time.Duration
	var lastC time.Time
	var tail string

	_, err := New(cfg).StreamChat(context.Background(), sys, user, StreamOpts{
		DisableThinking: true,
		JSONMode:        true,
		OnContent: func(s string) {
			now := time.Now()
			cChunks++
			cBytes += len(s)
			if firstC == 0 {
				firstC = now.Sub(start)
			}
			if !lastC.IsZero() {
				if g := now.Sub(lastC); g > gapMax {
					gapMax = g
				}
			}
			lastC = now
			tail = tail + s
			if len(tail) > 120 {
				tail = tail[len(tail)-120:]
			}
		},
		OnReasoning: func(s string) {
			if firstR == 0 {
				firstR = time.Since(start)
			}
			rChunks++
		},
	})
	total := time.Since(start)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	t.Logf("整轮        : %v", total)
	t.Logf("content 片数: %d 片 / %d 字节", cChunks, cBytes)
	t.Logf("reasoning 片数: %d（首个 %v）", rChunks, firstR)
	if cChunks > 0 {
		t.Logf("首个 content 片: %v（占整轮 %.1f%%）", firstC, float64(firstC)/float64(total)*100)
		t.Logf("content 最大片间隔: %v", gapMax)
		t.Logf("尾部片段     : %.120s", tail)
	}
	if cChunks <= 1 {
		t.Logf("结论: ❌ JSON 模式被整块下发（%d 片），OnContent 做进度展示行不通", cChunks)
	} else if firstC < total/4 {
		t.Logf("结论: ✅ JSON 内容按片段流式（%d 片，首片在 %.1f%% 处），OnContent 可做中间材料", cChunks, float64(firstC)/float64(total)*100)
	} else {
		t.Logf("结论: ⚠️ 有分片但首片太晚（%.1f%%），进度价值有限", float64(firstC)/float64(total)*100)
	}
}
