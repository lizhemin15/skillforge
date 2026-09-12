package llm

import (
	"context"
	"errors"
	"testing"
)

// nil 客户端绝不能 panic。
//
// 线上事故原样复现：启动时引擎拿到 nil client，用户手动指定「采购合同」发一条，
// 请求在 (*Client).Chat 上 panic → net/http 掐断连接 → 前端弹出"连接失败：network error"。
// 排查时第一反应是网络/跨域/CF，实际是启动没装模型。修完启动装配之后，这层守卫
// 负责"就算还有别的路径漏了，也要给一句人话 + 请求正常收尾"。
func TestNilClientReturnsErrNoLLMInsteadOfPanicking(t *testing.T) {
	var c *Client

	t.Run("Chat", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("nil client 的 Chat 居然 panic 了：%v", r)
			}
		}()
		out, err := c.Chat(context.Background(), "sys", "user")
		if !errors.Is(err, ErrNoLLM) {
			t.Fatalf("想要 ErrNoLLM，拿到 %v（输出 %q）", err, out)
		}
		if out != "" {
			t.Fatalf("报错时不该有输出，拿到 %q", out)
		}
	})

	t.Run("Complete", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("nil client 的 Complete 居然 panic 了：%v", r)
			}
		}()
		called := false
		_, err := c.Complete(context.Background(), "sys", "user", func(string) { called = true })
		if !errors.Is(err, ErrNoLLM) {
			t.Fatalf("想要 ErrNoLLM，拿到 %v", err)
		}
		if called {
			t.Fatal("没模型还往里写增量")
		}
	})

	t.Run("Config 展示路径也不能炸", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("nil client 的 Config 居然 panic 了：%v", r)
			}
		}()
		if cfg := c.Config(); cfg != nil {
			t.Fatalf("想要 nil，拿到 %+v", cfg)
		}
	})

	t.Run("空配置的非 nil 客户端同样报 ErrNoLLM", func(t *testing.T) {
		// cfg 字段为 nil 但接收者非 nil（比如有人手搓 &Client{}）也不能炸。
		var zero Client
		if _, err := zero.Chat(context.Background(), "s", "u"); !errors.Is(err, ErrNoLLM) {
			t.Fatalf("想要 ErrNoLLM，拿到 %v", err)
		}
	})
}
