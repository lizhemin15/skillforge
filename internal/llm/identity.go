package llm

import (
	"crypto/sha256"
	"fmt"

	"github.com/lizhemin15/skillforge/internal/model"
)

// 这个文件解决的是「手里这个客户端还是不是库里那条配置」这一件事。
//
// 为什么需要它（线上事故 2026-09-26）：用户在管理端把模型从 A 切到 B，界面显示
// 「在用」的是 B，但进程里常驻的客户端是启动时按 A 建的，于是每一轮问答都还在
// 打 A 的地址（A 已欠费，用户看到的是持续 402，看起来像「切换没生效」）。
// 判断依据不能只看 id：同一条记录被就地修改（换模型名、换 key、换网关地址）时
// id 不变，而客户端必须换 —— 那恰恰是最常见的改法。

// （Config() 已在 client.go 里给展示路径用了，这里不再重复声明。）

// Model 返回这个客户端实际会请求的模型名（空客户端返回 ""）。
// 给日志和界面用：报错里连模型名都没有，用户没法判断打的是哪一条配置。
func (c *Client) Model() string {
	if c == nil || c.cfg == nil {
		return ""
	}
	return c.cfg.Model
}

// Provider 返回这个客户端对应的服务商显示名（空客户端返回 ""）。
func (c *Client) Provider() string {
	if c == nil || c.cfg == nil {
		return ""
	}
	return c.cfg.Provider
}

// ConfigID 返回建这个客户端所依据的库记录 id（非库配置返回 0）。
// 界面靠它在「已启用的那条」和「进程真正在用的那条」之间做对照。
func (c *Client) ConfigID() int {
	if c == nil || c.cfg == nil {
		return 0
	}
	return c.cfg.ID
}

// FromStore 报告这个客户端的配置是否来自库里的一条真实记录（id > 0）。
//
// 为什么用 id 判「来自库」：库表自增 id 从 1 开始，而测试替身一律用
// `&model.LLMConfig{APIKey: "test", ...}` 构造（ID 零值）。引擎的自动换血
// 必须能区分这两种来源 —— 否则它会把调用方注入的假模型换掉，测试就测不到
// 自己注入的那条路了（假绿：断言过的其实是另一条配置）。
func (c *Client) FromStore() bool {
	return c != nil && c.cfg != nil && c.cfg.ID > 0
}

// Fingerprint 是「配置身份」的判定值：provider / base_url / model / key
// 任何一项变了都会变。空配置返回 ""（= 无法判定身份）。
//
// key 只进短哈希、不进明文：既要对「换 key」敏感，又不把密钥复制到日志或
// 内存快照这类第二个地方去。
func Fingerprint(cfg *model.LLMConfig) string {
	if cfg == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(cfg.APIKey))
	return fmt.Sprintf("%d|%s|%s|%s|%x",
		cfg.ID, cfg.Provider, normalizeBaseURL(cfg.BaseURL), cfg.Model, sum[:6])
}
