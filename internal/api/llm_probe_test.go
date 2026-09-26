package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/model"
)

type probeResp struct {
	OK        bool   `json:"ok"`
	Endpoint  string `json:"endpoint"`
	Model     string `json:"model"`
	Status    int    `json:"status"`
	APICode   string `json:"api_code"`
	Message   string `json:"message"`
	Reply     string `json:"reply"`
	LatencyMS int64  `json:"latency_ms"`
}

func doProbeLLM(t *testing.T, adm *Admin, body string) (int, probeResp, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/llms/test", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	adm.ProbeLLM(rec, req)
	var got probeResp
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	return rec.Code, got, rec.Body.String()
}

// 上游报错时，handler 仍然要回 200（结论在 body 的 ok 里），并且把
// 「HTTP 状态 + 上游码 + 归因 + 实际地址 + 耗时」都交出去。
// 只有这样才能让前端把「测出来不通」和「测试接口自己坏了」区分开。
func TestProbeLLMReportsUpstreamVerdict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"no category route found","code":10404}}`))
	}))
	defer srv.Close()

	adm := &Admin{}
	code, got, raw := doProbeLLM(t, adm, `{"base_url":"`+srv.URL+`/v2/chat/completions","model":"Qwen3.6-35B-A3B","api_key":"probe-key-123456"}`)

	if code != http.StatusOK {
		t.Fatalf("上游不通不该让测试接口报错，实际 HTTP %d: %s", code, raw)
	}
	if got.OK {
		t.Fatalf("应判不通，实际 ok=true: %s", raw)
	}
	if got.Status != 400 || got.APICode != "10404" {
		t.Errorf("应透出上游状态与业务码，实际 status=%d api_code=%q", got.Status, got.APICode)
	}
	if !strings.Contains(got.Message, "路由名") {
		t.Errorf("归因应指向「模型名不是路由名」，实际：%s", got.Message)
	}
	if got.Endpoint != srv.URL+"/v2/chat/completions" {
		t.Errorf("endpoint = %q，期望 %q（用户靠它核对地址有没有写歪）", got.Endpoint, srv.URL+"/v2/chat/completions")
	}
}

// 编辑已保存的服务时表单里是掩码 —— 「列表点测试」走的就是这条路。
// 掩码被当成 key 发出去必然 401，那就变成了假红。
// 同时断言：表单里改过的 base_url / model 要生效，不能被库里的旧值覆盖 ——
// 否则用户「改完先测」测的是旧配置，看着通过、存下去还是坏的。
func TestProbeLLMUsesStoredKeyAndFormValues(t *testing.T) {
	var seenPath, seenAuth, seenModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		seenModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"正常"}}]}`))
	}))
	defer srv.Close()

	st := newTestStore(t)
	adm := &Admin{store: st}
	id, err := st.UpsertLLMConfig(&model.LLMConfig{
		Provider: "xf-maas",
		BaseURL:  "https://stored.invalid/v1",
		Model:    "stored-model",
		APIKey:   realKey,
	})
	if err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}

	_, got, raw := doProbeLLM(t, adm, `{"id":`+strconv.FormatInt(id, 10)+`,"base_url":"`+srv.URL+`/v1","model":"form-model","api_key":"••••••••"}`)

	if !got.OK {
		t.Fatalf("应判连通，实际：%s", raw)
	}
	if seenAuth != "Bearer "+realKey {
		t.Errorf("鉴权头 = %q —— 掩码必须回退成库里的真 key，否则「列表点测试」永远假红", seenAuth)
	}
	if seenPath != "/v1/chat/completions" {
		t.Errorf("实际请求路径 = %q，期望表单里填的地址", seenPath)
	}
	if seenModel != "form-model" {
		t.Errorf("上游收到的 model = %q —— 表单里改过的值必须优先于库里的旧值", seenModel)
	}
}

// 回执会显示在管理端、还会被用户复制去问客服：里面绝不能出现 key 原文。
// 这里让假上游把鉴权头原样抄回错误正文（真实网关会这么干），再断言 key 没漏出去。
func TestProbeLLMNeverEchoesKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key: ` + r.Header.Get("Authorization") + `","code":401}}`))
	}))
	defer srv.Close()

	adm := &Admin{}
	code, got, raw := doProbeLLM(t, adm, `{"base_url":"`+srv.URL+`/v1","model":"m","api_key":"`+realKey+`"}`)

	if code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", code, raw)
	}
	if got.OK {
		t.Fatal("401 不该判连通")
	}
	if strings.Contains(raw, realKey) {
		t.Fatalf("回执里漏出了 key 原文：%s", raw)
	}
	if !strings.Contains(got.Message, "API Key 无效") {
		t.Errorf("归因不对：%s", got.Message)
	}
}

// 缺项要在本地就说清是缺什么、下一步点哪里 —— 别打出去换一个看不懂的上游错回来。
func TestProbeLLMRejectsIncompleteInput(t *testing.T) {
	adm := &Admin{}
	cases := []struct {
		name, body, wantWord string
	}{
		{"没填地址", `{"base_url":"","model":"m","api_key":"k-123456"}`, "Base URL"},
		{"地址没写协议", `{"base_url":"api.deepseek.com/v1","model":"m","api_key":"k-123456"}`, "http://"},
		{"没填模型名", `{"base_url":"https://api.deepseek.com/v1","api_key":"k-123456"}`, "模型名"},
		{"没填 key", `{"base_url":"https://api.deepseek.com/v1","model":"m"}`, "API Key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, raw := doProbeLLM(t, adm, tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("应 400，实际 %d: %s", code, raw)
			}
			if !strings.Contains(raw, tc.wantWord) {
				t.Errorf("提示里应说明缺的是 %q，实际：%s", tc.wantWord, raw)
			}
		})
	}
}
