package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lizhemin15/skillforge/internal/agent"
)

// 旁白行的**内容契约**：每一行都必须是本地已知真值，一条都不许是编的。
// 这里按「有素材 / 无素材」两种形态分别钉住，防止以后有人为了让屏幕好看
// 去写「正在分析文档结构…」这类我们当时并不知道的话。
func TestClassifyNarrationTellsLocalTruths(t *testing.T) {
	arts := []agent.Message{
		{Role: "user", Content: strings.Repeat("写作要求", 50), Kind: agent.KindMaterial},
		{Role: "assistant", Content: "好的"},
		{Role: "user", Content: "再写一份通知"},
		{Role: "assistant", Content: "好的"},
		{Role: "user", Content: "把它整理成 word"},
	}

	lines := classifyNarration("把它整理成 word", arts)
	got := strings.Join(lines, "\n")
	// 钉死三样真值：本轮字数、素材条数与总字数、上文轮数。
	for _, want := range []string{"本轮 10 字", "1 段素材", "200 字", "2 轮对话"} {
		if !strings.Contains(got, want) {
			t.Errorf("旁白缺了本地真值 %q：\n%s", want, got)
		}
	}

	// ⚠️ 行首不许自带「· 」：Narrate 会给每行补一个（chat_trace.go 的
	// `c.Thinking("· " + clean[i])`），自带的后果是界面上的双点「· · 收到…」。
	// 这条必须查**本函数的原始输出**，不能查帧里的 material —— 帧里的那一个点
	// 是 Narrate 给的，查帧等于查了个寂寞（原实现就这么错过一次，是自证脚本
	// 用「加双点」的注入抓出来的）。
	for _, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "·") {
			t.Errorf("旁白行自带「· 」会和 Narrate 补的点叠成双点：%q", ln)
		}
	}

	// 冷启动（第一句话、无素材）：**任何**关于素材/上文的字样都是编的。
	// 这里故意用宽判据（「素材」「轮对话」各查一次），而不是只查「段素材」——
	// 窄判据会放过「已带上你给的素材一起判定」这种同样在骗人的写法。
	got = strings.Join(classifyNarration("你好", nil), "\n")
	for _, bad := range []string{"素材", "轮对话"} {
		if strings.Contains(got, bad) {
			t.Errorf("无素材无历史时不该出现 %q（凭空编上下文）：\n%s", bad, got)
		}
	}
	if !strings.Contains(got, "第一句话") {
		t.Errorf("冷启动该说清没有上文要承接：\n%s", got)
	}
}

// 接线断言：分类这一跳**真的**挂了旁白。
//
// 这条尺子的前提是「假模型全程一个字都不吐」（newChatTestHandler 的假模型直接挂起
// 不写 body），所以采样窗口里出现的任何材料**只可能**来自本地旁白 —— 不是模型推的，
// 也不是测试自己塞的 DOM。这正是线上那 54s 静默的复刻：模型不说话时屏幕必须有东西在动。
//
// 只测「进跳瞬间是否已有材料」，不在这里测滚动节奏：滚动由 TestNarrateRollsLines
// 在 clock 层守着（那边能把 narrateEvery 注入成 450ms），两处分工不重复。
func TestClassifyHopNarratesWhenModelIsSilent(t *testing.T) {
	h := newChatTestHandler(t, t.TempDir())

	fw := newFrameWriter()
	req := httptest.NewRequest(http.MethodPost, "/api/chat",
		strings.NewReader(`{"session_id":"narr1","message":"帮我写一份数据治理通知","mode":"auto"}`))
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(fw, req) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("handler goroutine 2 秒内没退出（旁白的 stop 可能漏调，goroutine 泄漏）")
		}
	})

	// 采样 1.2 秒：够收下 t≈0 的骨架帧与紧随其后的第一行旁白，又不用等 3s 的滚动间隔。
	deadline := time.After(1200 * time.Millisecond)
	var sawMaterial string
	for sawMaterial == "" {
		select {
		case frame := <-fw.frames:
			var steps []struct {
				Label    string `json:"label"`
				Status   string `json:"status"`
				Material string `json:"material"`
			}
			if err := json.Unmarshal([]byte(frame), &steps); err != nil {
				continue // meta / 其它帧：只挑步骤帧
			}
			for _, s := range steps {
				// 材料只能挂在**进行中**那一步上：挂到 done 的步骤上，用户会读成
				// 「① 早就做完了、现在卡在别处」。这条在采样过程中就顺手判掉。
				if strings.TrimSpace(s.Material) != "" && s.Status != "active" {
					t.Errorf("步骤 %q（status=%s）挂上了材料，但材料只属于进行中的那一步",
						s.Label, s.Status)
				}
				if s.Status == "active" && strings.TrimSpace(s.Material) != "" {
					sawMaterial = s.Material
				}
			}
		case <-deadline:
			t.Fatalf("分类跳静默 1.2 秒仍无任何材料：假模型一个字都没吐时，屏幕又只剩跳秒的计时（用户原话「一直卡着计时」）")
		}
	}

	if !strings.Contains(sawMaterial, "收到你的需求") {
		t.Errorf("分类跳的材料不是本地旁白（可能是别处漏进来的模型残片）：%q", sawMaterial)
	}
	// 落到屏幕上必须是单点：这一条查的是 Narrate 补齐后的成品，和上面纯函数那条
	// （查原始行不许自带点）配对，两头都堵住。
	if !strings.Contains(sawMaterial, "· 收到你的需求") || strings.Contains(sawMaterial, "· · ") {
		t.Errorf("旁白落到屏幕上的点不对（应为 Narrate 补的单点）：%q", sawMaterial)
	}
}
