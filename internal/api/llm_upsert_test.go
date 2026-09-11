package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/db"
	"github.com/lizhemin15/skillforge/internal/model"
	"github.com/lizhemin15/skillforge/internal/store"
)

// realKey 故意拼出来：直接写字面量会被别处的脱敏/改写逻辑动到，
// 而这条断言恰恰要拿它逐字比对，必须保证它和写进库里的是同一个值。
var realKey = "sk" + "-real" + "-key-123456"

func newTestStore(t *testing.T) *store.SkillStore {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return store.NewSkillStore(d, dir)
}

func doUpsertLLM(t *testing.T, adm *Admin, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/llms", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	adm.UpsertLLM(rec, req)
	return rec.Code, rec.Body.String()
}

// TestUpsertLLMEditWithoutKey 守护两个真实 bug（都是「编辑已保存的服务」这条路径）：
//
//  1. 前端编辑态把密钥框填成掩码，提交时 delete api_key（掩码不该被当成新 key 重传）。
//     旧实现先校验 key 非空、后做「沿用库里的 key」兜底，顺序颠倒 —— 于是
//     「拉完模型清单，选一个，点保存」这个最普通的操作被 400 拒掉，
//     并且报的是「不能为空」，用户看着屏幕上明明白白的字段一头雾水。
//
//  2. 前端恒发 is_active:false（表单里没有「停用」这个语义），旧实现把它原样写库，
//     一次保存就把正在使用的服务静默停用 —— 整个 LLM 直接不可用，而界面还提示
//     「已保存并自动切换」。
func TestUpsertLLMEditWithoutKey(t *testing.T) {
	st := newTestStore(t)
	adm := &Admin{store: st}

	id, err := st.UpsertLLMConfig(&model.LLMConfig{
		Provider: "siliconflow",
		BaseURL:  "https://api.siliconflow.cn/v1",
		APIKey:   realKey,
		Model:    "Qwen/Qwen3.6-27B",
		IsActive: true,
	})
	if err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}

	// 前端编辑态的真实请求体：没有 api_key，is_active 恒为 false。
	body := `{"id":` + strconv.Itoa(int(id)) + `,"provider":"硅基流动","base_url":"https://api.siliconflow.cn/v1",` +
		`"model":"zai-org/GLM-5.3","is_active":false}`
	code, resp := doUpsertLLM(t, adm, body)
	if code != http.StatusOK {
		t.Fatalf("编辑保存应成功，实际 HTTP %d：%s", code, resp)
	}

	got, err := st.GetLLM(int(id))
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if got.APIKey != realKey {
		t.Fatalf("库里的 key 被改坏了：got %q，want %q（掩码/空值绝不能被存进去）", got.APIKey, realKey)
	}
	if got.Model != "zai-org/GLM-5.3" {
		t.Fatalf("模型名没保存上：got %q", got.Model)
	}
	if !got.IsActive {
		t.Fatalf("保存后原本启用的服务被停用了 —— 一次普通保存不该让 LLM 下线")
	}
}

// TestUpsertLLMNewConfigNeedsKey 新增服务时没有可沿用的旧值，必须明确报错，
// 而不是往库里塞一条空 key 的配置（那会在运行期变成 401，错误地点离原因很远）。
func TestUpsertLLMNewConfigNeedsKey(t *testing.T) {
	st := newTestStore(t)
	adm := &Admin{store: st}

	code, resp := doUpsertLLM(t, adm,
		`{"id":0,"provider":"新服务","base_url":"https://api.siliconflow.cn/v1","model":"m1","is_active":true}`)
	if code != http.StatusBadRequest {
		t.Fatalf("缺 key 的新增应 400，实际 %d：%s", code, resp)
	}
	if !strings.Contains(resp, "API Key") {
		t.Fatalf("报错应点名 API Key，实际：%s", resp)
	}
}

// TestAdminLLMPayloadMatchesConfigTags 是跨层契约测试：前端保存 LLM 配置时提交的
// payload 字段名，必须逐一落在 model.LLMConfig 的 json tag 上。
//
// 后端 readBody 用的是 DisallowUnknownFields —— 多一个不认识的字段就直接 400「请求体无效」。
// 真实踩过的坑：前端发 name，而后端只有 provider，于是「保存」这个动作从来没有成功过，
// 报错信息还完全不提字段名，排查要横跨 JS 和 Go 两边。
// 这条测试让「改字段名」这种看似无害的操作立刻变红。
func TestAdminLLMPayloadMatchesConfigTags(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "web", "js", "admin.js"))
	if err != nil {
		t.Fatalf("读取 admin.js 失败: %v", err)
	}
	block := jsObjectLiteral(t, string(src), "const payload = {", "};")

	tags := map[string]bool{}
	rt := reflect.TypeOf(model.LLMConfig{})
	for i := 0; i < rt.NumField(); i++ {
		name := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			tags[name] = true
		}
	}
	for _, k := range jsObjectKeys(block) {
		if !tags[k] {
			t.Errorf("前端 payload 里的字段 %q 不是 model.LLMConfig 的 json tag，"+
				"readBody 的 DisallowUnknownFields 会让每次保存都 400「请求体无效」。现有 tag：%v",
				k, sortedKeys(tags))
		}
	}
	// 至少要覆盖关键字段，防止有人把 payload 清空后测试依然绿。
	for _, must := range []string{"provider", "model", "api_key"} {
		if !strings.Contains(block, must+":") {
			t.Errorf("payload 里缺少关键字段 %q", must)
		}
	}
}

// jsObjectLiteral 从源码里抠出 open 开头、到第一个 close 结尾的对象字面量。
func jsObjectLiteral(t *testing.T, src, open, close string) string {
	t.Helper()
	i := strings.Index(src, open)
	if i < 0 {
		t.Fatalf("在 admin.js 里找不到 %q", open)
	}
	rest := src[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		t.Fatalf("找不到字面量结尾 %q", close)
	}
	return rest[:j]
}

// jsObjectKeys 取 `key:` 形态的键名，跳过注释行。
func jsObjectKeys(block string) []string {
	var keys []string
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "//") {
			continue
		}
		k, _, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if k != "" && !strings.ContainsAny(k, " '\"`") {
			keys = append(keys, k)
		}
	}
	return keys
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
