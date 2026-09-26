package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lizhemin15/skillforge/internal/db"
	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/model"
	"github.com/lizhemin15/skillforge/internal/store"
)

// 这一组守的是「换模型到底生不生效」。
//
// 为什么必须有它：旧实现里 ensureLLM 只看 `e.llm == nil`，启动那一刻读一次库就
// 再也不看库了。于是用户在管理端把模型从 A 切到 B、界面明确显示「在用：B」，
// 每一轮问答却仍然打 A 的地址。线上事故（2026-09-26）就是这样：A 已欠费，
// 用户切到 B 之后继续收到 A 的 402，只能得出「这系统不支持热切换」。
//
// 三个判据都不许省：
//  1. 就地改同一条记录（换模型名/base_url/key）→ 必须换血。id 不变，
//     所以「只看 id 变没变」的实现会在这里漏掉——而这恰恰是最常见的改法。
//  2. 启用另一条 → 必须换血。
//  3. 调用方注入的客户端（测试替身/一次性实例，id 零值）→ 永远不许换。

// markerServer 是一个「会自报家门」的假模型：谁被请求到，谁的命中数就 +1，
// 并且正文里带着自己的标记。判断「请求打到哪」只能靠这个，不能靠内部字段——
// 内部字段说换了、请求其实还走旧地址，正是这次事故的形状。
func markerServer(t *testing.T, tag string, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"c1","object":"chat.completion","model":"m-%s","choices":[{"index":0,"message":{"role":"assistant","content":"{\"who\":\"%s\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, tag, tag)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newHotswapStore(t *testing.T) *store.SkillStore {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return store.NewSkillStore(d, dir)
}

// seedLLM 塞一条配置并回读——必须回读：引擎要 id>0 才认这个客户端「来自库」，
// 手搓一份 ID 对不上的配置会让测试在错误的语义下通过。
func seedLLM(t *testing.T, st *store.SkillStore, cfg model.LLMConfig) *model.LLMConfig {
	t.Helper()
	id, err := st.UpsertLLMConfig(&cfg)
	if err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	got, err := st.GetLLM(int(id))
	if err != nil {
		t.Fatalf("回读配置失败: %v", err)
	}
	return got
}

// 判据 1：就地编辑「已在用的那条」。id 不变，只有模型名与网关地址变了。
func TestHotSwapOnInPlaceEditOfActiveConfig(t *testing.T) {
	var hitsA, hitsB int32
	srvA := markerServer(t, "A", &hitsA)
	srvB := markerServer(t, "B", &hitsB)

	st := newHotswapStore(t)
	row := seedLLM(t, st, model.LLMConfig{
		Provider: "网关A", BaseURL: srvA.URL, APIKey: "k-a", Model: "m-A", IsActive: true,
	})
	eng := New(llm.New(row), st)

	if got := eng.LLMModel(); got != "m-A" {
		t.Fatalf("起步就该是 A，得到 %q", got)
	}
	if got := eng.LLMConfigID(); got != row.ID {
		t.Fatalf("起步的运行期配置 id 应为 %d，得到 %d", row.ID, got)
	}
	if _, err := eng.llm.FastJSON(context.Background(), "s", "u", 64); err != nil {
		t.Fatalf("首次请求应当打到 A: %v", err)
	}

	// 用户改的就是这一条：留空 key 沿用、只换地址和模型名（前端编辑态就是这个形状）。
	edited := *row
	edited.BaseURL = srvB.URL
	edited.Model = "m-B"
	if _, err := st.UpsertLLMConfig(&edited); err != nil {
		t.Fatalf("改配置失败: %v", err)
	}

	if err := eng.ensureLLM(); err != nil {
		t.Fatalf("ensureLLM 报错: %v", err)
	}
	if got := eng.LLMModel(); got != "m-B" {
		t.Fatalf("就地改配置后引擎仍握着旧模型 %q：换模型没生效", got)
	}
	// 关键行为判据：不是内部字段变了，而是**真请求**打到新地址。
	out, err := eng.llm.FastJSON(context.Background(), "s", "u", 64)
	if err != nil {
		t.Fatalf("换血后的请求失败: %v", err)
	}
	if !strings.Contains(out, `"who":"B"`) {
		t.Fatalf("换血后的请求应打到 B，实际回应 %q", out)
	}
	if got := atomic.LoadInt32(&hitsB); got != 1 {
		t.Fatalf("B 应收到 1 次请求，实际 %d", got)
	}
	if got := atomic.LoadInt32(&hitsA); got != 1 {
		t.Fatalf("A 不该再收到请求（应仍是起步那 1 次），实际 %d", got)
	}
}

// 判据 2：启用另一条（管理端「切换」按钮）。
func TestHotSwapOnSwitchToAnotherConfig(t *testing.T) {
	var hitsA, hitsB int32
	srvA := markerServer(t, "A", &hitsA)
	srvB := markerServer(t, "B", &hitsB)

	st := newHotswapStore(t)
	rowA := seedLLM(t, st, model.LLMConfig{Provider: "网关A", BaseURL: srvA.URL, APIKey: "k-a", Model: "m-A", IsActive: true})
	rowB := seedLLM(t, st, model.LLMConfig{Provider: "网关B", BaseURL: srvB.URL, APIKey: "k-b", Model: "m-B"})
	eng := New(llm.New(rowA), st)

	if err := st.SetActiveLLM(rowB.ID); err != nil {
		t.Fatalf("切启用失败: %v", err)
	}
	if err := eng.ensureLLM(); err != nil {
		t.Fatalf("ensureLLM 报错: %v", err)
	}
	if got := eng.LLMConfigID(); got != rowB.ID {
		t.Fatalf("启用 B 之后运行期应换成 id=%d，实际 %d", rowB.ID, got)
	}
	if got := eng.LLMEndpoint(); got != srvB.URL {
		t.Fatalf("运行期地址应为 B 的 %s，实际 %s", srvB.URL, got)
	}
}

// 判据 3：库变了，但手里的客户端不是库里来的 —— 一个字都不许动它。
// 否则引擎会把测试替身、以及调用方明确指定的模型悄悄换掉。
func TestInjectedClientIsNeverSwapped(t *testing.T) {
	var hitsStore, hitsStub int32
	srvStore := markerServer(t, "store", &hitsStore)
	srvStub := markerServer(t, "stub", &hitsStub)

	st := newHotswapStore(t)
	seedLLM(t, st, model.LLMConfig{Provider: "网关", BaseURL: srvStore.URL, APIKey: "k", Model: "m-store", IsActive: true})

	// 替身形状：ID 零值（库里没有这条）。
	stub := llm.New(&model.LLMConfig{BaseURL: srvStub.URL, APIKey: "k", Model: "m-stub"})
	eng := New(stub, st)
	if err := eng.ensureLLM(); err != nil {
		t.Fatalf("ensureLLM 报错: %v", err)
	}
	if got := eng.LLMModel(); got != "m-stub" {
		t.Fatalf("注入的客户端被换掉了：得到 %q", got)
	}
	out, err := eng.llm.FastJSON(context.Background(), "s", "u", 64)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if !strings.Contains(out, `"who":"stub"`) {
		t.Fatalf("请求应走注入的客户端，实际 %q", out)
	}
	if atomic.LoadInt32(&hitsStore) != 0 {
		t.Fatalf("库里的地址不该被碰到，实际收到 %d 次", hitsStore)
	}
}

// SetLLM 之后必须仍是「库配置」，否则管理端推一次就永久失去跟随库的能力：
// 换模型这件事只生效一次，之后又退回启动时那份的老毛病。
func TestSetLLMKeepsFollowingStore(t *testing.T) {
	var hitsA, hitsB int32
	srvA := markerServer(t, "A", &hitsA)
	srvB := markerServer(t, "B", &hitsB)

	st := newHotswapStore(t)
	rowA := seedLLM(t, st, model.LLMConfig{Provider: "A", BaseURL: srvA.URL, APIKey: "k-a", Model: "m-A", IsActive: true})
	rowB := seedLLM(t, st, model.LLMConfig{Provider: "B", BaseURL: srvB.URL, APIKey: "k-b", Model: "m-B"})
	eng := New(nil, st)

	// 管理端「启用」按现实现的动作：改库 + SetLLM。
	if err := st.SetActiveLLM(rowB.ID); err != nil {
		t.Fatalf("切启用失败: %v", err)
	}
	eng.SetLLM(llm.New(rowB))
	if got := eng.LLMConfigID(); got != rowB.ID {
		t.Fatalf("SetLLM 后运行期应为 %d，实际 %d", rowB.ID, got)
	}
	// 再切回 A：SetLLM 之后仍须能跟随库变化。
	if err := st.SetActiveLLM(rowA.ID); err != nil {
		t.Fatalf("切启用失败: %v", err)
	}
	if err := eng.ensureLLM(); err != nil {
		t.Fatalf("ensureLLM 报错: %v", err)
	}
	if got := eng.LLMConfigID(); got != rowA.ID {
		t.Fatalf("SetLLM 之后引擎不再跟随库（应回到 %d，实际 %d）", rowA.ID, got)
	}
}

// ReloadLLM 是「点完立刻生效」那条路：不经过 ensureLLM 也该换好。
func TestReloadLLMAppliesImmediately(t *testing.T) {
	var hitsA, hitsB int32
	srvA := markerServer(t, "A", &hitsA)
	srvB := markerServer(t, "B", &hitsB)

	st := newHotswapStore(t)
	rowA := seedLLM(t, st, model.LLMConfig{Provider: "A", BaseURL: srvA.URL, APIKey: "k-a", Model: "m-A", IsActive: true})
	rowB := seedLLM(t, st, model.LLMConfig{Provider: "B", BaseURL: srvB.URL, APIKey: "k-b", Model: "m-B"})
	eng := New(llm.New(rowA), st)

	if err := st.SetActiveLLM(rowB.ID); err != nil {
		t.Fatalf("切启用失败: %v", err)
	}
	if err := eng.ReloadLLM(); err != nil {
		t.Fatalf("ReloadLLM 报错: %v", err)
	}
	if got := eng.LLMModel(); got != "m-B" {
		t.Fatalf("ReloadLLM 后应立即是 m-B，实际 %q", got)
	}
}

// 库里一条都没有时：不许 panic，也不许把手里能用的客户端弄没。
func TestEnsureLLMWithoutConfig(t *testing.T) {
	// (a) 空库 + 无客户端 → 可读错误
	empty := newHotswapStore(t)
	if err := New(nil, empty).ensureLLM(); err == nil {
		t.Fatal("空库且无客户端时应报错")
	} else if !strings.Contains(err.Error(), "未配置 LLM") {
		t.Fatalf("错误文案应能给人看懂，实际 %q", err.Error())
	}

	var hits int32
	srv := markerServer(t, "only", &hits)
	// (b) 空库 + 有客户端（注入）→ 保留
	stub := llm.New(&model.LLMConfig{BaseURL: srv.URL, APIKey: "k", Model: "m-stub"})
	if err := New(stub, empty).ensureLLM(); err != nil {
		t.Fatalf("空库但有客户端时不该报错: %v", err)
	}

	// (c) store 为 nil（单测里常见的构造方式）→ 不许空指针崩
	if err := New(nil, nil).ensureLLM(); err == nil {
		t.Fatal("无 store 且无客户端时应报错而不是 panic")
	}
	if err := New(stub, nil).ensureLLM(); err != nil {
		t.Fatalf("无 store 但有客户端时不该报错: %v", err)
	}
}

// 库为空、手里这条是库配置（库被清空/实例被摘掉）时不许自毁：
// 手里的客户端还能用，把它置 nil 只会让在跑的会话立刻变成「未配置 LLM」。
func TestEnsureLLMKeepsWorkingClientWhenStoreEmptied(t *testing.T) {
	var hits int32
	srv := markerServer(t, "A", &hits)
	st := newHotswapStore(t)
	row := seedLLM(t, st, model.LLMConfig{Provider: "A", BaseURL: srv.URL, APIKey: "k", Model: "m-A", IsActive: true})
	eng := New(llm.New(row), st)

	if err := st.DeleteLLMConfig(row.ID); err != nil {
		t.Fatalf("删配置失败: %v", err)
	}
	if err := eng.ensureLLM(); err != nil {
		t.Fatalf("库里清空后不该报错: %v", err)
	}
	if !eng.HasLLM() {
		t.Fatal("手里的客户端不该被清掉")
	}
}
