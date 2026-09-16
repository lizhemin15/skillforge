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

	// 内嵌 IANA 时区库（约 450KB）。理由：离线机器（最小化安装的 CentOS/AlmaLinux、
	// 精简容器）常常没有 /usr/share/zoneinfo，此时 time.LoadLocation("Asia/Shanghai")
	// 直接报错 —— 表现是日志时间、页面上的时间、定时任务全按 UTC 走，差 8 小时，
	// 而且没有任何报错，客户只会觉得「时间不对」却查不到原因。
	// 内嵌后即使系统一个时区文件都没有，TZ=Asia/Shanghai 也能正确解析。
	_ "time/tzdata"

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
