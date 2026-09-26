package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lizhemin15/skillforge/internal/agent"
	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/model"
	"github.com/lizhemin15/skillforge/internal/store"
)

// 这一组守的是管理端三个接口的**接线**：改完 llm_config 必须把改动推给运行期的引擎。
//
// 为什么单测引擎侧还不够：agent 包的用例证明了「库里变了、引擎会跟着变」，
// 但没人保证管理端的接口真的触发了那个动作。旧实现里三个接口都只改库：
// SetActiveLLM 改完 is_active 就返回 200，界面立刻显示「在用：B」，
// 而进程手里的客户端还是 A —— 用户切了模型，收到的仍是 A 的 402。
// 把 applyActiveLLM() 那一行删掉，全仓测试曾经照样全绿。
//
// 断言用引擎**运行期**的配置 id，而不是库里的 is_active：库是对的、运行期是旧的，
// 正是这次事故的形状，所以判据必须落在运行期那一侧。

// seedLLMRow 塞一条配置并回读（引擎只认 id>0 的「库配置」）。
func seedLLMRow(t *testing.T, st *store.SkillStore, cfg model.LLMConfig) *model.LLMConfig {
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

// adminWithEngine 造一个「引擎正握着 row 这条配置」的管理端。
func adminWithEngine(t *testing.T, st *store.SkillStore, row *model.LLMConfig) (*Admin, *agent.Engine) {
	t.Helper()
	eng := agent.New(llm.New(row), st)
	return &Admin{store: st, eng: eng}, eng
}

func doSwitchLLM(t *testing.T, adm *Admin, id int) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"id": id})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/llms/active", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	adm.SetActiveLLM(rec, req)
	return rec.Code, rec.Body.String()
}

func doDeleteLLM(t *testing.T, adm *Admin, id int) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/api/admin/llms/1", nil)
	req.SetPathValue("id", itoaTest(id))
	rec := httptest.NewRecorder()
	adm.DeleteLLM(rec, req)
	return rec.Code, rec.Body.String()
}

func itoaTest(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// 点「切换」必须当场生效，不许等下一轮问答。
func TestSwitchLLMAppliesToLiveEngine(t *testing.T) {
	st := newTestStore(t)
	rowA := seedLLMRow(t, st, model.LLMConfig{Provider: "网关A", BaseURL: "https://a.example/v1", APIKey: "k-a", Model: "m-A", IsActive: true})
	rowB := seedLLMRow(t, st, model.LLMConfig{Provider: "网关B", BaseURL: "https://b.example/v1", APIKey: "k-b", Model: "m-B"})
	adm, eng := adminWithEngine(t, st, rowA)

	code, body := doSwitchLLM(t, adm, rowB.ID)
	if code != http.StatusOK {
		t.Fatalf("切换应返回 200，实际 %d %s", code, body)
	}
	// 不调用 ensureLLM：要的就是「接口自己把运行期换了」。
	if got := eng.LLMConfigID(); got != rowB.ID {
		t.Fatalf("点启用后运行期应立刻是 id=%d，实际 %d（仍是旧模型）", rowB.ID, got)
	}
	if got := eng.LLMModel(); got != "m-B" {
		t.Fatalf("运行期模型应为 m-B，实际 %q", got)
	}
}

// 「编辑已保存的服务」这条路径前端恒发 is_active:false（沿用库里的启用状态）。
// 改的恰恰可能是正在用的那条 —— 它必须也能当场生效，不能被 IsActive 判断挡在门外。
func TestEditActiveLLMAppliesToLiveEngine(t *testing.T) {
	st := newTestStore(t)
	rowA := seedLLMRow(t, st, model.LLMConfig{Provider: "网关A", BaseURL: "https://a.example/v1", APIKey: "k-a", Model: "m-A", IsActive: true})
	adm, eng := adminWithEngine(t, st, rowA)

	body := `{"id":` + itoaTest(rowA.ID) + `,"provider":"网关A","base_url":"https://a2.example/v1","model":"m-A2","is_active":false}`
	code, resp := doUpsertLLM(t, adm, body)
	if code != http.StatusOK {
		t.Fatalf("保存应返回 200，实际 %d %s", code, resp)
	}
	if got := eng.LLMModel(); got != "m-A2" {
		t.Fatalf("就地改模型名后运行期应变成 m-A2，实际 %q", got)
	}
	if got := eng.LLMEndpoint(); got != "https://a2.example/v1" {
		t.Fatalf("运行期地址应跟着变，实际 %q", got)
	}
}

// 删掉正在用的那条：运行期必须退到剩下的那条去。
// 不退的话，界面「运行中」那一行指向一条已被删除的记录，排查时越看越乱。
func TestDeleteActiveLLMStepsBackToRemaining(t *testing.T) {
	st := newTestStore(t)
	rowA := seedLLMRow(t, st, model.LLMConfig{Provider: "网关A", BaseURL: "https://a.example/v1", APIKey: "k-a", Model: "m-A"})
	rowB := seedLLMRow(t, st, model.LLMConfig{Provider: "网关B", BaseURL: "https://b.example/v1", APIKey: "k-b", Model: "m-B", IsActive: true})
	adm, eng := adminWithEngine(t, st, rowB)

	if got := eng.LLMConfigID(); got != rowB.ID {
		t.Fatalf("前置：运行期应为 B（id=%d），实际 %d", rowB.ID, got)
	}
	code, body := doDeleteLLM(t, adm, rowB.ID)
	if code != http.StatusOK {
		t.Fatalf("删除应返回 200，实际 %d %s", code, body)
	}
	if got := eng.LLMConfigID(); got != rowA.ID {
		t.Fatalf("删掉在用的那条后运行期应退回 A（id=%d），实际 %d", rowA.ID, got)
	}
}

// 列表接口要如实报告「进程真正在用的那条」：界面靠它把「已启用」和「运行中」
// 分开显示。两者不一致时用户看到的才是真相，而不是一个乐观的「在用」标签。
func TestListLLMReportsRuntimeID(t *testing.T) {
	st := newTestStore(t)
	rowA := seedLLMRow(t, st, model.LLMConfig{Provider: "网关A", BaseURL: "https://a.example/v1", APIKey: "k-a", Model: "m-A", IsActive: true})
	rowB := seedLLMRow(t, st, model.LLMConfig{Provider: "网关B", BaseURL: "https://b.example/v1", APIKey: "k-b", Model: "m-B"})
	adm, _ := adminWithEngine(t, st, rowA)

	read := func() float64 {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/admin/llms", nil)
		rec := httptest.NewRecorder()
		adm.ListLLM(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("列表应返回 200，实际 %d", rec.Code)
		}
		var got struct {
			RuntimeID float64 `json:"runtime_id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("响应不是合法 JSON: %v", err)
		}
		return got.RuntimeID
	}

	if got := int(read()); got != rowA.ID {
		t.Fatalf("运行期 id 应为 %d，实际 %d", rowA.ID, got)
	}
	// 库切到 B 但运行期还是 A：接口必须照实报 A（这才是「没生效」的可见证据）。
	if err := st.SetActiveLLM(rowB.ID); err != nil {
		t.Fatalf("切启用失败: %v", err)
	}
	if got := int(read()); got != rowA.ID {
		t.Fatalf("库改了但运行期没动时，应照实报旧的 %d，实际 %d（把库当运行期报等于掩盖问题）", rowA.ID, got)
	}
	// 走一遍接口之后就该跟上了。
	if code, body := doSwitchLLM(t, adm, rowB.ID); code != http.StatusOK {
		t.Fatalf("切换失败: %d %s", code, body)
	}
	if got := int(read()); got != rowB.ID {
		t.Fatalf("切换后运行期 id 应为 %d，实际 %d", rowB.ID, got)
	}
}
