package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lizhemin15/skillforge/internal/model"
)

// 这一组守的是**片级**看门狗：上游字节一直在来（SSE 保活帧），但连续不出正文/思考片。
//
// 为什么必须单独守这条：字节看门狗（streamIdleLimit 20s 无字节）对它是瞎的。
// 线上现场（v7 第 1 轮）：
//
//	[write-plain] hop=346.1s ttft=-1.00s reason=2 out_pieces=0 out=0
//	err=LLM 返回空 content（流完整跑完但正文 0 字，只收到 1 片思考链…）
//
// 346 秒里用户一直在看计时器，最后一字未得。三个断言各自对应一种错法：
// ① 保活帧能骗过字节看门狗 → 片看门狗必须收手（否则退回 346s）；
// ② 第一片之前与之后要分开判 → 模型还在想（首片实测最长 104.71s）不能被杀；
// ③ 阈值不能紧到误杀健康流 → 正常吐字的流必须完整跑完。

// dribbleSrv：先按 pieces 脚本吐片，其余时间不断滴保活帧（有字节、无内容片）。
// pieces 里每一项是「第几毫秒吐一片」，片内容是 reasoning_content（模拟思考链）。
func dribbleSrv(t *testing.T, untilMS int, pieceAtMS []int) (*httptest.Server, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	var extra, hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		start := time.Now()
		deadline := start.Add(time.Duration(untilMS) * time.Millisecond)
		next := 0
		for time.Now().Before(deadline) {
			// 客户端收手（看门狗关了连接）就立刻退出，别让 httptest 的 Close 干等
			// 到本函数自己跑完 —— 那会让「看门狗生效」这件事在测试耗时上消失。
			select {
			case <-r.Context().Done():
				return
			default:
			}
			elapsed := int(time.Since(start).Milliseconds())
			wrote := false
			for next < len(pieceAtMS) && pieceAtMS[next] <= elapsed {
				_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"想 %d\"}}]}\n\n", next)
				next++
				extra.Add(1)
				wrote = true
			}
			if !wrote {
				// 保活帧：字节在来，但**不是内容片**（注释行，SSE 规范允许）。
				_, _ = w.Write([]byte(": keepalive\n\n"))
			}
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(50 * time.Millisecond)
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &extra, &hits
}

// ① 只有保活帧、一片不出 → 必须在「首片上限」处收手，而不是干等到整体超时。
// 负向自证：把 streamFirstPieceLimit() 调大（或不让 watchProgress 参与），
// 这条会一直等到 dribbleSrv 自己收工，耗时与「没被抓住」无法区分 → 断言耗时就会红。
func TestStreamWatchdogTripsWhenOnlyKeepAliveFrames(t *testing.T) {
	t.Setenv("SKILLFORGE_STREAM_FIRST_PIECE_SEC", "2")
	t.Setenv("SKILLFORGE_STREAM_PIECE_GAP_SEC", "30")
	t.Setenv("SKILLFORGE_STREAM_IDLE_SEC", "0") // 关掉字节看门狗：本测试只考「字节活着」这一种

	srv, _, hits := dribbleSrv(t, 30000, nil) // 30 秒只有保活帧
	c := New(&model.LLMConfig{BaseURL: srv.URL, APIKey: "k", Model: "test-model"})

	start := time.Now()
	_, err := c.StreamChat(context.Background(), "sys", "user", StreamOpts{DisableThinking: true})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("上游只滴保活帧、一片正文都没有，不能算成功")
	}
	if !errors.Is(err, ErrNoProgress) {
		t.Fatalf("必须是「字节活着但不出片」这条（ErrNoProgress），实际 %v", err)
	}
	if errors.Is(err, ErrStreamStalled) {
		t.Fatalf("字节一直在来，不该判成「不给数据」那条：%v", err)
	}
	// 首片上限 2s + 一次重试（IsStreamBroken 的 triedPartial）= 最坏约 4s 出头。
	if elapsed > 12*time.Second {
		t.Fatalf("首片上限 2s 时整轮该在十几秒内收手，实际 %s —— 说明没被片看门狗抓住", elapsed)
	}
	if !strings.Contains(err.Error(), "第一片") {
		t.Fatalf("错误里要说清是「连第一片都没等到」，实际：%v", err)
	}
	// 光收手不算完：这类错误必须被 IsStreamBroken 认成「当场重试一次」，
	// 否则用户是「不卡了、直接失败」。接线掉了这条就红。
	if n := hits.Load(); n < 2 {
		t.Fatalf("收到 ErrNoProgress 后必须当场重试一次，实际只打了 %d 发", n)
	}
}

// ② 出过片、然后不再出片 → 用「片间隔」那条收手（此时首片上限不该背锅）。
func TestStreamWatchdogTripsOnPieceGap(t *testing.T) {
	t.Setenv("SKILLFORGE_STREAM_FIRST_PIECE_SEC", "30")
	t.Setenv("SKILLFORGE_STREAM_PIECE_GAP_SEC", "2")
	t.Setenv("SKILLFORGE_STREAM_IDLE_SEC", "0")

	// 第 0ms 与第 500ms 各一片，之后 30 秒全是保活帧。
	srv, pieces, _ := dribbleSrv(t, 30000, []int{0, 500})
	c := New(&model.LLMConfig{BaseURL: srv.URL, APIKey: "k", Model: "test-model"})

	start := time.Now()
	_, err := c.StreamChat(context.Background(), "sys", "user", StreamOpts{DisableThinking: true})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrNoProgress) {
		t.Fatalf("必须报 ErrNoProgress，实际 %v", err)
	}
	if got := pieces.Load(); got < 1 {
		t.Fatalf("前提不成立：上游一片都没吐出来，考的不是「片间隔」这条路的")
	}
	if !strings.Contains(err.Error(), "没有新的正文/思考片段") {
		t.Fatalf("错误里要说清是「片间隔」这条，实际：%v", err)
	}
	if elapsed > 12*time.Second {
		t.Fatalf("片间隔 2s 时该很快收手，实际 %s", elapsed)
	}
}

// ③ 反向对照（防「太紧误杀」）：正常吐字的流必须完整跑完，一片不少。
// 没有这条，把阈值设成 1ms 也能让上面两条绿 —— 那是拿误杀换速度。
func TestStreamWatchdogDoesNotKillHealthyStream(t *testing.T) {
	t.Setenv("SKILLFORGE_STREAM_FIRST_PIECE_SEC", "5")
	t.Setenv("SKILLFORGE_STREAM_PIECE_GAP_SEC", "3")
	t.Setenv("SKILLFORGE_STREAM_IDLE_SEC", "5")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for i := 0; i < 12; i++ {
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"第%d片\"}}]}\n\n", i)
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(700 * time.Millisecond) // 片间隔 0.7s < 上限 3s，属于健康范围
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	c := New(&model.LLMConfig{BaseURL: srv.URL, APIKey: "k", Model: "test-model"})
	got, err := c.StreamChat(context.Background(), "sys", "user", StreamOpts{DisableThinking: true})
	if err != nil {
		t.Fatalf("健康流（片间隔 0.7s）不该被看门狗杀掉：%v", err)
	}
	for i := 0; i < 12; i++ {
		if !strings.Contains(got, fmt.Sprintf("第%d片", i)) {
			t.Fatalf("正文不该缺片：缺了第 %d 片，实际 %q", i, got)
		}
	}
}

// ④ 首片很慢但仍在预算内 → 不能杀。线上健康样本首片最长 104.71s（默认上限 150s），
// 这条用短阈值复刻同一形状：首片 3s 到，上限 5s，必须成功。
func TestStreamWatchdogAllowsSlowFirstPiece(t *testing.T) {
	t.Setenv("SKILLFORGE_STREAM_FIRST_PIECE_SEC", "5")
	t.Setenv("SKILLFORGE_STREAM_PIECE_GAP_SEC", "5")
	t.Setenv("SKILLFORGE_STREAM_IDLE_SEC", "0")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		// 3 秒内只滴保活帧（模型在「想」）
		for i := 0; i < 30; i++ {
			_, _ = w.Write([]byte(": keepalive\n\n"))
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(100 * time.Millisecond)
		}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"慢热但出来了\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	c := New(&model.LLMConfig{BaseURL: srv.URL, APIKey: "k", Model: "test-model"})
	got, err := c.StreamChat(context.Background(), "sys", "user", StreamOpts{DisableThinking: true})
	if err != nil {
		t.Fatalf("首片 3s（上限 5s）属于健康范围，不该被杀：%v", err)
	}
	if !strings.Contains(got, "慢热但出来了") {
		t.Fatalf("正文没收到：%q", got)
	}
}

// ⑤ 阈值旋钮：0 = 关掉那一段；非法值回落默认。与别的旋钮同一套约定。
func TestStreamPieceKnobs(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", 150 * time.Second},
		{"0", 0},
		{"-3", 150 * time.Second},
		{"7", 7 * time.Second},
		{"abc", 150 * time.Second},
	}
	for _, c := range cases {
		t.Setenv("SKILLFORGE_STREAM_FIRST_PIECE_SEC", c.env)
		if got := streamFirstPieceLimit(); got != c.want {
			t.Errorf("SKILLFORGE_STREAM_FIRST_PIECE_SEC=%q → %s，期望 %s", c.env, got, c.want)
		}
	}
	for _, c := range cases {
		want := c.want
		if want == 150*time.Second {
			want = 60 * time.Second // 同一个表，默认值不同
		}
		t.Setenv("SKILLFORGE_STREAM_PIECE_GAP_SEC", c.env)
		if got := streamPieceGapLimit(); got != want {
			t.Errorf("SKILLFORGE_STREAM_PIECE_GAP_SEC=%q → %s，期望 %s", c.env, got, want)
		}
	}
}

// ⑥ 两段阈值必须都够宽：首片上限要盖得住实测最慢的健康首片（104.71s），
// 片间隔上限要盖得住健康流里的最长片间隔余量。太紧 = 拿误杀换速度。
func TestStreamPieceDefaultsHaveHeadroom(t *testing.T) {
	t.Setenv("SKILLFORGE_STREAM_FIRST_PIECE_SEC", "")
	t.Setenv("SKILLFORGE_STREAM_PIECE_GAP_SEC", "")
	const slowestHealthyFirstPiece = 104710 * time.Millisecond // 线上 hop=114.6s ttft=104.71s
	if got := streamFirstPieceLimit(); got <= slowestHealthyFirstPiece {
		t.Fatalf("首片上限 %s 会杀掉实测最慢的健康首片 %s（那是误杀，不是保护）", got, slowestHealthyFirstPiece)
	}
	if got := streamPieceGapLimit(); got < 30*time.Second {
		t.Fatalf("片间隔上限 %s 太紧：长文执笔里思考链片偶尔断档几十秒是正常的", got)
	}
}
