package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/model"
	"github.com/lizhemin15/skillforge/internal/skillgen"
)

// 本文件锁死另一条通道：上传解析（Admin.extractDoc）与训练解析（Generator）
// 必须**共用同一个**超时来源（SKILLFORGE_OCR_TIMEOUT）。曾经的病是两处各写一个
// 硬编码 300s，改一条忘一条；这里用行为断言把两条都钉住，光看字段不算数。

func stubOCR(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"text":"手册正文"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	h, err := NewHandler(newStoreForTest(t, t.TempDir()), llm.New(&model.LLMConfig{}), "test-secret")
	if err != nil {
		t.Fatalf("建 handler 失败：%v", err)
	}
	return h
}

// TestOCRTimeoutEnvReachesBothChannels：env 定值 → 两条通道的生效超时都是它，
// 而且上传解析**真的**按这个上限去放弃/等待（不是只改了个字段）。
func TestOCRTimeoutEnvReachesBothChannels(t *testing.T) {
	srv := stubOCR(t, 700*time.Millisecond)

	t.Setenv("SKILLFORGE_OCR_TIMEOUT", "400ms")
	h := newTestHandler(t)
	h.Admin.ocrURL = srv.URL

	if got := h.Admin.ocrTimeoutOrDefault(); got != 400*time.Millisecond {
		t.Fatalf("上传解析通道生效超时 %s，期望 400ms（env 没进去）", got)
	}
	if got := h.Admin.gen.OCRTimeout(); got != 400*time.Millisecond {
		t.Fatalf("训练解析通道生效超时 %s，期望 400ms（两条通道不同源）", got)
	}
	// 上限 400ms < 服务 700ms → 上传解析必须失败，否则说明超时根本没被用上。
	if _, err := h.Admin.extractDoc("m.pdf", []byte("x")); err == nil {
		t.Fatal("上限 400ms、桩服务要 700ms，上传解析却成功了：超时没被应用到请求上")
	}
}

// TestOCRTimeoutEnvParsesForms：30m / 90s / 纯秒数三种写法都要认；
// 零值/非法值一律回退默认且不许让服务起不来（解析慢是性能问题，不该升级成启动失败）。
func TestOCRTimeoutEnvParsesForms(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", skillgen.DefaultOCRTimeout},
		{"30m", 30 * time.Minute},
		{"90s", 90 * time.Second},
		{"600", 600 * time.Second},
		{" 20m ", 20 * time.Minute},
		{"abc", skillgen.DefaultOCRTimeout},
		{"-5s", skillgen.DefaultOCRTimeout},
		{"0", skillgen.DefaultOCRTimeout},
	}
	for _, c := range cases {
		t.Setenv("SKILLFORGE_OCR_TIMEOUT", c.env)
		h := newTestHandler(t)
		if got := h.Admin.ocrTimeoutOrDefault(); got != c.want {
			t.Fatalf("env=%q：上传解析超时 %s，期望 %s", c.env, got, c.want)
		}
		if got := h.Admin.gen.OCRTimeout(); got != c.want {
			t.Fatalf("env=%q：训练解析超时 %s，期望 %s", c.env, got, c.want)
		}
	}
}

// TestOCRDefaultTimeoutIsNotTheOldHardcoded300：把旧值写回去必红。
// 单独一条显式钉住，免得将来有人「顺手」把默认调回 5 分钟。
func TestOCRDefaultTimeoutIsNotTheOldHardcoded300(t *testing.T) {
	const oldHardcoded = 300 * time.Second
	if skillgen.DefaultOCRTimeout <= oldHardcoded {
		t.Fatalf("默认解析超时 %s 退回到了旧的 300s 档位：真实扫描件 397.5s 必然超时", skillgen.DefaultOCRTimeout)
	}
	t.Setenv("SKILLFORGE_OCR_TIMEOUT", "")
	h := newTestHandler(t)
	if got := h.Admin.ocrTimeoutOrDefault(); got <= oldHardcoded {
		t.Fatalf("未设 env 时生效超时是 %s，仍会卡死在旧硬编码档位", got)
	}
}
