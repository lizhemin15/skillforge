# SkillForge Agent 工具系统改造方案

> 目标（用户原话）：「整个 ai 系统我期望你做的像 hermes 一样，内置很多常用工具，且能写一些代码，然后这些工具一组合，威力无穷」

## 进度

- ✅ **P1 已完成（2026-09-11）**：工具注册表 + Agent 循环 + `http_request` + `run_python`（systemd 沙箱）
- ✅ 分类器新增 `needs_tools` 判定：需要外部实时数据/真实计算的任务优先走工具循环
- ⏳ P2：把 `fill_template` / `gen_document` 包装成循环可用工具（这样「取实时数据 → 填进指定模板」也能一站完成）
- ⏳ P3：`search_skills` / `read_skill`（让模型自己翻技能库）、`query_db`

### P1 验收证据（实测）

服务以 root 运行、公网可达，故沙箱是唯一防线，逐项实测：

| 项 | 结果 |
|---|---|
| 降权 | 沙箱内 `uid=65534`（nobody），绝不以 root 执行 |
| 断网 | `socket` 连接失败（PrivateNetwork 命名空间） |
| 只读根 | 写 `/opt/skillforge`、`/etc`、`/root` 全部拒绝；仅工作区可写 |
| 密钥防护 | 读不到 `skillforge.env` / DB / 会话记录（并已把 data 目录从 755 收到 700、DB 与会话文件收到 600） |
| 资源限额 | MemoryMax 256M 拦住 800MB 分配；TasksMax 32 拦住 fork 炸弹；CPUQuota 50% |
| 超时 | 死循环 3s 被杀，且**不留孤儿进程**（Go 只杀 systemd-run 不够，必须 `systemctl kill` 掉 unit） |
| SSRF | 环回/私网/云元数据地址默认拦截；302 跳转到内网同样拦；内网接口可由 `SKILLFORGE_TOOL_HTTP_ALLOW` 开白 |

端到端（真实模型，两轮）：
1. 「查 golang/go 的 star/fork 并算比值，出 CSV」→ `http_request`(GitHub API) → `run_python` → 交付 CSV，
   文件内容 `138403,19353,7.1515` 与 GitHub 官方 API **逐位一致**（工具取数，不是编的）。
2. 「取 golang/go 与 rust-lang/rust 的 star，算倍差，出 CSV」→ 2×http_request + run_python → CSV，
   `138403` / `118379` 与官方一致。

对比改造前：同一个问题会命中「办公文档管家」技能、由模型**凭空编造** star 数并生成一份看着很合理的 xlsx。
这是本次改造要解决的核心弊病。

### 环境坑（本机实测）

- `DynamicUser=yes` 起不来（本机未配子 uid 范围，exit 200），改用 `User=nobody`
- `PrivateTmp=yes` 起不来（容器环境，exit 200），已剔除
- `unshare -n` 在降权**之后**执行会失败（非 root 无权限）；顺序必须是先建命名空间再降权
- 每次沙箱执行开销 ~30ms，可接受
- 本机 PATH 里的 `go` 是 1.18 古董，必须用 `/usr/local/go/bin/go`（1.25.10）

## 0. 结论先行：威力来自「循环」，不是「工具多」

当前架构是**一次性管线**：

```
EvalTurn（LLM 选技能/动作）→ Go 里 if 技能类型==docgen → 调一次 → 结束
```

工具是散落在 `internal/api/chat.go` 里的 `if/else` 分支，模型**只有一次发言机会**，
它看不到工具返回的结果，因此**无法组合**。堆再多分支也不会有「威力」。

要变成 Hermes 那样，缺的是三样东西：

1. **工具注册表**：工具是数据（名字 + 描述 + JSON Schema + 执行函数），不是 if 分支
2. **Agent 循环**：模型调工具 → 拿结果 → 再决定下一步，直到自己说「完事」
3. **可执行代码**：能现场写 Python 处理数据（这是「工具一组合」的粘合剂）

## 1. 前提验证（已实测，20260911）

对本机配置的真实模型（`astron-code-latest`）做了 function calling 探针：

| 能力 | 结果 |
|---|---|
| 非流式原生 tools | ✅ `finish_reason=tool_calls`，参数 JSON 正确 |
| 流式 tools 解析 | ✅ 54 个 SSE 分片里能提取出 tool_call |
| **自主串联**（多轮） | ✅ 4 次调用：`http_request`×2 → `run_python` → `gen_document` → 汇总 |
| **并行调用**（同轮多工具） | ✅ 第 1 轮同时发起 2 个 `http_request` |
| 对 JSON Schema 的严格度 | ⚠️ `required` 写成字符串直接 400 —— 参数 schema 必须严谨 |

**结论：可以直接用原生 function calling，不必退化成 prompt-JSON 土办法。**

## 2. 架构

```
internal/tools/            ← 新包：工具注册表
  tool.go                  Tool 接口 + Registry + JSON Schema → go-openai 转换
  fs.go                    文件读写（会话工作区）
  exec.go                  run_python（沙箱执行）
  net.go                   http_request / web_fetch
  doc.go                   包一层现有 docgen / 模板填充 / 技能检索
internal/agent/loop.go     ← 新：Agent 循环（多轮 tool calling + trace 事件）
internal/llm/client.go     ← 扩展：ChatWithTools（支持 messages 数组 + tools）
```

### Tool 接口

```go
type Tool interface {
    Name() string
    Description() string          // 给模型看的能力说明，决定它会不会用对
    Schema() map[string]any       // JSON Schema，严格：required 必须是数组
    Run(ctx context.Context, args map[string]any) (Result, error)
}
type Result struct {
    Content string        // 回给模型的文本
    Files   []FileRef     // 要交付给用户的文件（文档/图片）
    Display string        // UI 面板上显示的一行摘要（显性展示工具链）
}
```

### 循环

```
messages = [system(技能提示 + 工具使用准则)] + 历史 + 用户输入
for round := 1; round <= maxRounds(默认8); round++ {
    resp = LLM(messages, tools=registry.All())
    if resp 无 tool_calls { 输出文本，结束 }
    for each call in resp.tool_calls {            // 支持并行
        emit trace{tool, args摘要, "运行中"}      // 用户看得见
        result = registry.Run(call)
        emit trace{..., 结果摘要, done}
        messages += {role:"tool", tool_call_id, content:result.Content}
    }
}
```

**向后兼容**：现有「技能命中 → 模板填充 / docgen」快路径**保留不动**（延迟低、行为稳定）。
新循环作为**第二条路径**：当模型判断需要外部数据/计算/多步时进入。
判定权交给 `EvalTurn`（它已经会判断 intent），新增一个 `action="agent"`。

## 3. 工具清单（P1 核心 5 个，每个都必须挣到自己的位置）

| 工具 | 解决什么 | 依赖 |
|---|---|---|
| `http_request` | **用户核心需求**：调外部数据中台 API 取数 | 无（纯 Go） |
| `run_python` | 现场写代码算数据/转格式（粘合剂） | 沙箱 + python3 |
| `gen_document` | 出 Word/Excel/PPT/PDF | 现有 docgen，包装 |
| `fill_template` | 往技能模板填数据出文件 | 现有 FillDoc，包装 |
| `search_skills` / `read_skill` | 让模型自己翻技能库找资料/模板 | 现有 SkillStore |

P3 候选（先不做，避免「无必要增实体」）：`query_db`、`file_read/write`、`web_fetch`、`ask_user`。

**组合示例（这才是要交付的演示）**：
「把各区域销售数据拉过来算占比，出个 Excel」
→ `http_request`（调数据中台）→ `run_python`（算）→ `gen_document`（出文件）→ 文本汇总

## 4. 安全（必须和工具同时上线，不是后补）

**现状问题：`systemctl show skillforge -p User` → 空，即服务以 root 运行。**
skillhub 挂在 Cloudflare 公网。**以 root 给公网开代码执行 = 送服务器。**

姿态（默认值，可用 env 覆盖）：

- `SKILLFORGE_EXEC=off | admin | public`，**默认 `admin`**（未登录访客用不了 `run_python`）
- `SKILLFORGE_TOOLS=off | on`：整体工具开关
- `run_python` 执行约束：
  - **降权**：`setpriv --reuid=65534 --regid=65534 --clear-groups`（nobody），绝不以 root 跑
  - **断网**：`unshare -n`（要联网就走 `http_request`，那条路径可审计）
  - **隔离目录**：每会话临时工作区，跑完清理
  - **限时**：`timeout 30s`；**限内存**：`ulimit -v`；**限输出**：截断到 64KB
- 可选强化（若本机有 python 镜像）：docker `--network=none --memory=256m --cpus=1 --pids-limit=64`
  —— 但**离线部署**铁律下不能依赖拉镜像，docker 不可用就回落 setpriv 方案

## 5. 分阶段

| 阶段 | 内容 | 验收 |
|---|---|---|
| **P1 内核** | `internal/tools` 注册表 + `agent/loop.go` + `llm.ChatWithTools` + 2 个工具（`http_request`、`run_python`） | 单测：循环能跑通多轮、能并行、达上限能退出；真实模型能自主串起两个工具 |
| **P2 工具集** | 包装 docgen / 模板填充 / 技能检索为工具 | 端到端：一句话 → 取数 → 算 → 出报表（3 工具串联） |
| **P3 显性展示** | 工具链实时上 UI（复用现有 trace 面板） | 面板逐条显示「工具名 + 参数 + 结果摘要」 |
| **P4 安全加固** | 降权/断网/限额 + env 开关 + README | 以 nobody 身份执行，网络不可达，超时被砍 |

**流程**：每阶段 → git push → GitHub Action 编译 → 下载 release → 部署 → 自测 → 才报「完成」。
