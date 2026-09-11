// SkillForge — 以 Skill（技能）为核心的 AI 办公文档工作台。
//
// 单一静态二进制：Go 后端 + 纯 Go SQLite + 前端嵌入。
//   - 前台：对话即出文档（Word / Excel / PPT / PDF），SSE 流式 + 推理步骤可视化
//   - 管理端：登录 → 配置 LLM（可热切换）→ 训练新技能（女娲 skill-generator）
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/lizhemin15/skillforge/internal/server"
	"github.com/lizhemin15/skillforge/internal/version"
)

func main() {
	// allow overriding data dir & addr via flags even when env absent
	dataDir := flag.String("data", os.Getenv("SKILLFORGE_DATA_DIR"), "data directory (env SKILLFORGE_DATA_DIR)")
	addr := flag.String("addr", os.Getenv("SKILLFORGE_ADDR"), "listen address (env SKILLFORGE_ADDR)")
	showVersion := flag.Bool("version", false, "print version and exit")
	selfTest := flag.Bool("selftest", false, "run offline self-test (version / PDF font / sandbox) and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("skillforge " + version.String())
		return
	}

	if *selfTest {
		os.Exit(runSelfTest())
	}

	log.Printf("skillforge %s starting", version.String())

	if *dataDir != "" {
		os.Setenv("SKILLFORGE_DATA_DIR", *dataDir)
	}
	if *addr != "" {
		os.Setenv("SKILLFORGE_ADDR", *addr)
	}

	if err := server.Run(); err != nil {
		log.Fatal(err)
	}
}
