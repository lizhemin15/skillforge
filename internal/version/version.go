// Package version 暴露构建期注入的版本信息。
//
// 这些变量由构建时通过 -ldflags 注入（见 .github/workflows/release.yml）：
//
//	go build -ldflags "-X github.com/lizhemin15/skillforge/internal/version.Version=v1.0.0 \
//	                   -X github.com/lizhemin15/skillforge/internal/version.Commit=abc1234 \
//	                   -X github.com/lizhemin15/skillforge/internal/version.Date=2026-09-11T00:00:00Z"
//
// 未注入时保持 "dev" 等默认值，便于区分手工构建。
package version

// Version 是语义化版本号，如 v0.1.0。
var Version = "dev"

// Commit 是构建所用的 git commit 短哈希。
var Commit = "none"

// Date 是构建时间（RFC3339）。
var Date = "unknown"

// String 返回单行可读的版本描述，供 -version 与日志使用。
func String() string {
	return Version + " (commit " + Commit + ", built " + Date + ")"
}
