# SkillForge

> 以 **Skill（技能）** 为核心的 AI 办公文档工作台：把「一份公司内部的模板 + 一份参考资料 + 一套行文习惯」沉淀成可复用的技能，之后用自然语言一句话就能产出合规的 Word / Excel / PPT / PDF。

单个 Go 静态二进制，纯 Go SQLite，前端嵌入，**完全离线可部署**（不依赖任何外部 CDN）。

[![CI](https://github.com/lizhemin15/skillforge/actions/workflows/ci.yml/badge.svg)](https://github.com/lizhemin15/skillforge/actions/workflows/ci.yml)

---

## 这个项目解决什么问题

企业内部文档的痛点从来不是「写不出来」，而是**格式不合规、口径不一致、每次都要重新解释一遍要求**。通用大模型能写字，但不懂你们公司的模板长什么样、上一版合同是怎么措辞的、报价单的合计怎么算。

SkillForge 的做法是：**把规范预先固化成技能**，运行时只做「检索技能 → 抽取参数 → 按规范生成」。

| 通用对话式 AI | SkillForge |
| --- | --- |
| 每次都要在提示词里重新描述模板 | 技能里存着模板，命中即用 |
| 输出格式靠模型自觉 | 真的产出 .docx / .xlsx / .pptx / .pdf 文件 |
| 表格合计、大小写金额容易算错 | 由 Go 代码确定性计算，不经模型手 |
| 要联网、依赖外部服务 | 单二进制离线运行 |

---

## 特性

- **技能即插件**：一个技能 = 一个目录（`meta.json` + `system_prompt.md` + 模板 + 参考资料），支持热更新、版本回滚、启用/停用，不需要重新编译。
- **三种技能形态**：`write`（生成文章）、`query`（回答办事流程）、`template`（返回流程 + 推送附件下载），由 `skill_type` 声明。
- **女娲式技能生成**：上传参考资料 + 写一句需求，`skill-generator` 流水线自动产出并校验一个新技能。
- **办公文档引擎（docgen）**：纯 Go 离线生成 Word / Excel / PPT / PDF，支持**在已填好的文档上继续改**（改值 / 加行 / 删行），并**自动重算合计与小写金额**。
- **多智能体对话引擎**：意图识别 → 技能检索 → 参数抽取 → 执行，全过程通过 SSE 的 `trace` 事件**显性回放**给前端，用户能看见每一步在想什么、用了哪个技能。
- **LLM 可热切换**：管理端网页上配置 provider / base_url / model / api_key，立即生效，无需重启。
- **单文件部署**：`go build` 出来就是一个二进制，前端 `go:embed` 在里面。

---

## 架构

```
                    ┌──────────────────────────────────────────┐
  浏览器  ──SSE──▶  │  internal/api        路由 / 鉴权 / 静态资源 │
                    └────────────────┬─────────────────────────┘
                                     │
                    ┌────────────────▼─────────────────────────┐
                    │  internal/agent      多智能体对话引擎       │
                    │  ① 意图分析 → ② 工具匹配 → ③ 参数抽取 → ④ 执行 │
                    └───┬──────────────┬───────────────┬───────┘
                        │              │               │
             ┌──────────▼───┐  ┌───────▼──────┐  ┌─────▼──────────┐
             │ skillgen     │  │ store        │  │ docgen         │
             │ 女娲技能生成  │  │ 技能/SQLite  │  │ 文档引擎       │
             └──────────────┘  └──────────────┘  └────────────────┘
                        │              │               │
                        └──────────────┼───────────────┘
                                ┌──────▼──────┐
                                │ llm         │  OpenAI 兼容协议
                                └─────────────┘
```

| 包 | 职责 |
| --- | --- |
| `internal/api` | HTTP 路由、JWT 鉴权、SSE 流式响应、管理端接口 |
| `internal/agent` | 对话引擎：意图识别、技能检索、参数抽取、文档动作分发 |
| `internal/skillgen` | 「女娲」技能生成流水线：参考文件 → 技能 |
| `internal/docgen` | 离线文档引擎：docx / xlsx / pptx / pdf 生成与续改 |
| `internal/store` | 纯 Go SQLite（无 cgo）持久化：技能、LLM 配置、管理员 |
| `internal/llm` | OpenAI 兼容客户端，支持 provider 覆盖 |
| `internal/config` | 环境变量配置 |
| `web/` | 原生 JS 前端（无构建步骤），`go:embed` 进二进制 |

---

## 快速开始

### 从源码运行

```bash
git clone https://github.com/lizhemin15/skillforge.git
cd skillforge
go build -o skillforge ./cmd/server
mkdir -p /opt/skillforge/data
SKILLFORGE_DATA_DIR=/opt/skillforge/data ./skillforge
```

打开 <http://localhost:8092>。首次启动会自动建库并创建管理员（默认 `admin` / `skillforge123`，**请立刻改掉**）。

### 配置 LLM

两种方式，任选其一：

1. **环境变量**（见 `deploy.env.example`）
2. **管理端网页**：登录 `/admin` → LLM 配置 → 填 provider / base_url / model / api_key。网页配置优先级更高，可随时热切换。

> 任何 OpenAI 兼容接口都能用（OpenAI、DeepSeek、通义、vLLM、Ollama…），只要 `base_url` 指向 `/v1`。

---

## 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `SKILLFORGE_ADDR` | `:8092` | 监听地址 |
| `SKILLFORGE_DATA_DIR` | `/root/skillforge` | 技能内容与数据库目录 |
| `SKILLFORGE_DB` | `<DATA_DIR>/skillforge.db` | 单独指定数据库路径 |
| `SKILLFORGE_PUBLIC_URL` | `http://localhost:8092` | 对外地址，用于生成下载链接 |
| `SKILLFORGE_ADMIN_USER` | `admin` | 管理员账号 |
| `SKILLFORGE_ADMIN_PASS` | `skillforge123` | 管理员密码（环境变量值始终为准） |
| `SKILLFORGE_PDF_FONT_FILE` | 空 | 强制指定 PDF 渲染字体（`.ttf`），留空则自动探测 |
| `SKILLFORGE_JWT_SECRET` | `change-me-...` | JWT 签名密钥，**务必改成随机串** |
| `SKILLFORGE_LLM_PROVIDER` | 空 | LLM 供应商标识 |
| `SKILLFORGE_LLM_BASE_URL` | 空 | OpenAI 兼容 base_url |
| `SKILLFORGE_LLM_MODEL` | 空 | 模型名 |
| `SKILLFORGE_LLM_API_KEY` | 空 | API Key |

完整示例见 [`deploy.env.example`](deploy.env.example)。

---

## 部署

### 方式一：离线一键安装包（推荐，目标机不需要联网、不需要装 Go）

从 [Releases](https://github.com/lizhemin15/skillforge/releases) 下载 `skillforge-offline-<版本>-linux-<架构>.tar.gz`，
拷到目标机任意位置（U 盘、内网跳板都行），然后：

```bash
tar -xzf skillforge-offline-*.tar.gz
cd skillforge-offline-*/
sudo ./install.sh                      # 默认装到 /opt/skillforge，端口 8092
```

装完直接开浏览器访问 `http://<机器IP>:8092`。卸载用同目录的 `uninstall.sh`。

包内自带静态二进制（含全部依赖）、中文字体及字体许可证，安装过程**不执行任何 curl / wget / apt / pip**，
所以完全断网的机器也能装。前置要求只有一条：目标机有 systemd
（代码执行沙箱建立在 `systemd-run` 降权机制上，它是本服务对外公开时唯一的防线，缺了它脚本拒绝安装）。

常用参数：

```bash
sudo ./install.sh --port 9000                  # 换端口
sudo ./install.sh --prefix /srv/sf             # 换安装前缀
sudo ./install.sh --service skillforge-test    # 换服务名（同机装第二份实例）
sudo ./uninstall.sh --purge                    # 卸载并连数据一起删
```

> **同机多实例必须换 `--service` 名字。** 服务名同时决定 systemd 单元名；如果单元已存在但指向
> 另一个安装前缀，`install.sh` / `uninstall.sh` 会**拒绝执行**而不是默默覆盖——因为覆盖单元文件时
> 已经在跑的进程不会报错，但那个服务下次重启就会静默切到本次的目录、数据和配置上，是最难排查的
> 一类线上事故。确认要顶掉旧实例时显式加 `--force`。

详细说明见 [`deploy/offline/README.md`](deploy/offline/README.md)。

### 方式二：手动 systemd 部署

```bash
sudo mkdir -p /opt/skillforge && cd /opt/skillforge
sudo cp skillforge .            # 从 release 下载的二进制
cp deploy.env.example skillforge.env && chmod 600 skillforge.env
$EDITOR skillforge.env          # 填真实值
sudo systemctl daemon-reload && sudo systemctl enable --now skillforge
```

`/etc/systemd/system/skillforge.service`：

```ini
[Unit]
Description=SkillForge
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/skillforge
EnvironmentFile=/opt/skillforge/skillforge.env
ExecStart=/opt/skillforge/skillforge
Restart=always
RestartSec=3
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
```

---

## API 一览

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/api/skills` | 技能列表 |
| `GET` | `/api/skills/{slug}` | 技能详情 |
| `POST` | `/api/generate` | 按技能生成 |
| `POST` | `/api/chat` | 对话（SSE 流式，主入口） |
| `GET` | `/api/chat/gen/{token}` | 下载生成的文件 |
| `POST` | `/api/login` | 管理端登录 |
| `GET/POST/DELETE` | `/api/admin/llms*` | LLM 配置管理 |
| `GET/PUT/POST/DELETE` | `/api/admin/skills*` | 技能与文件的增删改查 |
| `POST` | `/api/admin/train` | 训练（生成）新技能 |

### SSE 事件

| 事件 | 含义 |
| --- | --- |
| `meta` | 本轮意图、命中的技能、动作 |
| `trace` | 多阶段执行步骤（前端「推理面板」的数据源） |
| `skill` | 最终选中的技能与理由 |
| `delta` | 增量文本 |
| `file` | 生成的产物（名称 + 下载地址） |
| `error` / `done` | 异常 / 结束 |

---

## 技能目录结构

```
skills/<slug>/
├── meta.json          # slug / name / description / skill_type / input_params
├── system_prompt.md   # 人设与输出规范
├── template.md        # 文档模板
├── requirement.md     # 触发条件（什么需求该命中这个技能）
├── style_profile.md   # 行文风格画像
├── source/            # 参考资料（上传的 pdf / docx / xlsx / txt）
└── versions/          # 历史版本，支持回滚
```

---

## 办公文档引擎（docgen）

`internal/docgen` 是纯 Go 的离线文档引擎，无需 LibreOffice、无需外部服务：

- **生成**：给定结构化 `Doc` 规格 → 返回 `.docx` / `.xlsx` / `.pptx` / `.pdf` 原始字节
- **续改**：在同一份已填好的 docx 上继续改值、加行、删行，保留原有格式
- **合计重算**：按明细行「数量 × 单价」重算合计，同时刷新大写金额与旧值清理

> **PDF 字体注意**：PDF 渲染需要一个同时覆盖中文与 ASCII（数字、字母）的 TrueType 字体。程序会在运行时按「环境变量指定 → 内置候选 → 扫描系统字体目录」的顺序**实测字符覆盖率**，只有真正覆盖数字的字体才会被采用（未覆盖的字符会被 gopdf 静默渲染成空白，历史上正是这个原因让 PDF 里的金额全部消失）。
>
> ```bash
> # 部署机器上装一个全覆盖的 .ttf 字体（Debian/Ubuntu）
> apt-get install -y fonts-arphic-gbsn00lp fonts-arphic-gkai00mp
> # 字体装在非常规路径时，直接指定：
> SKILLFORGE_PDF_FONT_FILE=/path/to/your.ttf
> ```
>
> ⚠️ gopdf **不能加载** `.ttc`（字体集合）与 `.otf`（CFF 轮廓）——若系统里只有 `NotoSansCJK-Regular.ttc`，PDF 生成仍然会失败并给出明确报错，请改用 `.ttf` 字体。

---

## 测试

```bash
go test ./...                       # 单元测试
go vet ./...                        # 静态检查
python3 e2e/docgen_regression.py    # 端到端：文档生成 / 续改 / 合计重算（需服务在 8092）
```

端到端脚本走真实 HTTP API，下载真实产物字节并解包校验，不做 mock。

---

## 构建与发布

CI 由 GitHub Actions 负责，无需本地交叉编译：

- **`.github/workflows/ci.yml`** — 每次 push / PR：`go vet` + `go test` + 构建
- **`.github/workflows/release.yml`** — 打 tag（`v*`）时：交叉编译 linux / darwin / windows × amd64 / arm64，自动创建 Release 并附上产物

```bash
git tag v0.1.0 && git push origin v0.1.0   # 触发 Release
```

---

## License

[MIT](LICENSE)
