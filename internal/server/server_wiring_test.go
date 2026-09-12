package server

import (
	"testing"

	"github.com/lizhemin15/skillforge/internal/config"
	"github.com/lizhemin15/skillforge/internal/db"
	"github.com/lizhemin15/skillforge/internal/model"
	"github.com/lizhemin15/skillforge/internal/store"
	"path/filepath"
)

// 线上事故：server.go 里写的是 api.NewHandler(st, nil, ...)，指望管理端再点一次
// "启用模型"来热插拔。库里已经有激活配置的部署，重启后引擎手里是 nil ——
// 第一条走到模型调用的请求（比如手动指定「采购合同」这种模板类技能）就在
// llm.(*Client).Chat 上空指针 panic，net/http 掐断连接，前端只看到
// "连接失败：network error"，整件事看起来像网络/跨域问题，很难往"启动没装模型"上想。
//
// 所以这里断言的不是"函数返回值好看"，而是**装配结果**：启动装配完，引擎必须真的
// 拿到客户端。不然改了 server.go 的调用点、测试还全绿，等于没守。
func TestBuildHandlerLoadsActiveLLMIntoEngine(t *testing.T) {
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	defer sqlDB.Close()
	st := store.NewSkillStore(sqlDB, t.TempDir())

	// 库里点上一颗激活的模型（模拟"线上早就配好了"）
	if _, err := st.UpsertLLMConfig(&model.LLMConfig{
		Provider: "siliconflow", BaseURL: "https://example.invalid/v1",
		Model: "Qwen/Qwen3.6-27B", APIKey: "sk-test-not-a-real-key", IsActive: true,
	}); err != nil {
		t.Fatalf("写激活模型失败: %v", err)
	}

	h, err := buildHandler(st, &config.Config{JWTSecret: "test-secret"})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if h.Eng == nil {
		t.Fatal("引擎没装配上")
	}
	if !h.Eng.HasLLM() {
		t.Fatal("启动后引擎手里没有模型：请求会在 Chat 上空指针 panic，" +
			"前端只会显示「网络错误」。检查 buildHandler 是不是又传了 nil")
	}
}

// 库里确实一个激活模型都没有时，允许装配出"没有模型"的引擎（管理端可以后配），
// 但这条路径必须是**可读报错**而不是 panic —— 报错那一半由 llm.ErrNoLLM 的单测守。
func TestBuildHandlerWithoutActiveLLMIsStillBuildable(t *testing.T) {
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	defer sqlDB.Close()
	st := store.NewSkillStore(sqlDB, t.TempDir())

	h, err := buildHandler(st, &config.Config{JWTSecret: "test-secret"})
	if err != nil {
		t.Fatalf("没有激活模型时也应该能起来（管理端再配）: %v", err)
	}
	if h.Eng.HasLLM() {
		t.Fatal("库里没有激活模型，引擎却拿到了客户端，说明读的不是激活那条")
	}
}
