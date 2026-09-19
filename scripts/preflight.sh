#!/usr/bin/env bash
#
# preflight.sh —— 提交前把 CI 的闸门在本地原样跑一遍
#
# 为什么需要它：CI 的 `gofmt check` / `go.mod tidy` / 前端回归是**分步独立**的，
# 任何一步红了整条 run 就是 failure，但发现时间在推送之后（还要等 runner 排队）。
# 2026-09-13 就因为只跑了 `go test` 没跑 `gofmt -l .`，CI 白红一次、
# 连带把已经打好的 Release 拖成半成品（只能取消重发 tag）。
#
# 设计要点：
#   - **顺序与 CI 一致**：tidy → vet → gofmt → build → go test → node 回归。
#     便宜的检查放前面，贵的放后面，早失败早停。
#   - **不静默跳过**：某个闸门所需的工具不存在（比如没装 node）时**报错退出**，
#     不许「工具缺失 → 当作通过」。这种静默恰好是最坏的情况：本地全绿、CI 全红。
#   - `go test ./...` 的缓存：默认沿用 Go 的构建缓存，不强制 -count=1，
#     否则每次 preflight 都要重跑全部测试，慢到没人愿意跑。
#
# 用法：
#   scripts/preflight.sh          # 全跑
#   scripts/preflight.sh --quick  # 跳过 Go 单测与前端回归，只跑静态检查（改注释/格式时用）
#
# 退出码：0 全绿；非 0 表示有闸门失败（失败项逐条列出）。

set -uo pipefail

QUICK=0
[ "${1:-}" = "--quick" ] && QUICK=1

cd "$(dirname "$0")/.." || exit 1

# 落地校验：dirname 不可用（或被 sh 环境剥掉）时 `cd "/.."` 会静默落到根目录，
# 于是所有闸门都在错的目录里跑 —— 全绿但什么都没检查。必须挡住这种假绿。
if [ ! -f go.mod ] || [ ! -d web/tests ]; then
  printf '\033[31m✗ 没落到仓库根目录（当前：%s）—— 别在错的地方跑闸门\033[0m\n' "$PWD" >&2
  exit 3
fi

FAILED=()
PASSED=0
SKIPPED=()

step() { printf '\n\033[1m=== %s ===\033[0m\n' "$1"; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$1"; PASSED=$((PASSED + 1)); }
bad()  { printf '  \033[31m✗\033[0m %s\n' "$1"; FAILED+=("$1"); }

need() {
  command -v "$1" >/dev/null 2>&1 || {
    printf '\033[31m✗ 缺少工具 %s —— 不许当作通过\033[0m\n' "$1" >&2
    exit 2
  }
}

need go
need node

# ---- 工具链校验：PATH 里的 go 必须真的能编译本仓库 ----
# 为什么必须做：本机 /usr/bin/go 是发行版自带的 go1.18，而 /usr/local/go/bin/go 才是
# go1.25。preflight 原来直接用 PATH 里的 `go`，于是「闸门是绿是红」取决于调用者的 PATH
# 里有没有 /usr/local/go/bin：后台/干净 env 下跑就解析到 1.18，go.mod 里 `go 1.25.0`
# 被它判成格式错，一口气报出 vet / build / go test / 自证 4 项红 —— 看着像代码坏了，
# 其实是工具链老了。这种假红最坑：会把人带到完全错误的方向去翻源码。
# 规则：不写死绝对路径（写死绝对路径已在 CI runner 上翻车两次），改为「挑一个够新的 go」：
#   1) PATH 里的 go 够新 → 用它
#   2) 否则 /usr/local/go/bin/go 够新 → 用它，并把它前置进 PATH（子脚本/自证也一起受益）
#   3) 都不够新 → 明确报错退出。宁可说「工具链太老」，也绝不放出一片假红。
GOMIN="$(sed -n 's/^go \([0-9][0-9.]*\)$/\1/p' go.mod | head -n 1)"
GOMIN="${GOMIN:-1.21}"

# 用 `go version` 而不是 `go env GOVERSION`：后者会去读 go.mod，老版本 go 读不了
# 新格式的 go.mod 就直接失败，等于用「工具链太老」去证明「工具链太老」——能work但报错信息丢失。
go_ver() {
  "$1" version 2>/dev/null | awk '{print $3}' | sed 's/^go//'
}
go_ok() {
  [ -x "$1" ] || return 1
  local v
  v="$(go_ver "$1")"
  [ -n "$v" ] || return 1
  # 用 sort -V 比版本，别自己拆三段数字（1.25.0 与 1.25.10 这类形态手拆必错）
  [ "$(printf '%s\n%s\n' "$GOMIN" "$v" | sort -V | head -n 1)" = "$GOMIN" ]
}

if go_ok "$(command -v go)"; then
  :
elif go_ok /usr/local/go/bin/go; then
  PATH="/usr/local/go/bin:$PATH"; export PATH
  printf '  \033[33m·\033[0m PATH 里的 go 太老，已自动切到 /usr/local/go/bin/go（%s）\n' "$(go_ver /usr/local/go/bin/go)"
else
  printf '\033[31m✗ 本机 go 工具链太老，跑不了这个仓库（go.mod 要求 >= %s）\033[0m\n' "$GOMIN" >&2
  printf '  PATH 里的 go ： %s → %s\n' "$(command -v go || echo 无)" "$(go_ver "$(command -v go)" || echo 不可用)" >&2
  printf '  /usr/local/go ： %s\n' "$([ -x /usr/local/go/bin/go ] && go_ver /usr/local/go/bin/go || echo 不存在)" >&2
  printf '  修法：export PATH=/usr/local/go/bin:$PATH（或装 go >= %s）\n' "$GOMIN" >&2
  printf '  注意：这是环境问题，不是代码问题 —— 别去翻源码。\n' >&2
  exit 2
fi

# ---- node 版本校验：同一个坑，先堵住 ----
# 本机 /usr/bin/node 是发行版自带的 v12，而前端测试用了 node:test（要 18+）和 ESM 顶层
# await（要 14.8+）。PATH 里没前置 nvm 时，7 个前端测试会报
#   SyntaxError: Unexpected reserved word / No such built-in module: node:test
# —— 看着像测试代码写坏了，其实只是 node 太老。
# 这里只校验、不自动切换：node 的安装位置因人而异（nvm / n / 发行版），猜路径比直接报错更糟。
NODEMIN=18
NODE_V="$(node --version 2>/dev/null | sed 's/^v//')"
NODE_MAJ="${NODE_V%%.*}"
case "$NODE_MAJ" in
  ''|*[!0-9]*)
    printf '\033[31m✗ 取不到 node 版本（node --version 输出异常：%s）\033[0m\n' "$NODE_V" >&2
    exit 2
    ;;
esac
if [ "$NODE_MAJ" -lt "$NODEMIN" ]; then
  # 在 PATH 之外还有哪些 node 里挑一个真正够新的来建议（别推荐 /bin/node 这种
  # 和 /usr/bin/node 同体异名的老货，那种建议等于没说）
  BEST_NODE=""
  for c in $(which -a node 2>/dev/null); do
    cv="$("$c" --version 2>/dev/null | sed 's/^v//')"
    cmaj="${cv%%.*}"
    case "$cmaj" in ''|*[!0-9]*) continue ;; esac
    if [ "$cmaj" -ge "$NODEMIN" ]; then BEST_NODE="$c"; break; fi
  done
  printf '\033[31m✗ node 太老：v%s（前端测试要 >= %s）\033[0m\n' "$NODE_V" "$NODEMIN" >&2
  printf '  PATH 里的 node   ： %s → v%s\n' "$(command -v node)" "$NODE_V" >&2
  if [ -n "$BEST_NODE" ]; then
    printf '  本机可用的新 node： %s → %s\n' "$BEST_NODE" "$("$BEST_NODE" --version 2>/dev/null)" >&2
    printf '  修法：export PATH="%s:$PATH"\n' "$(dirname "$BEST_NODE")" >&2
  else
    printf '  修法：本机没找到 >= %s 的 node，装一个（nvm / 官方 tarball / 发行版都行）\n' "$NODEMIN" >&2
  fi
  printf '  注意：这是环境问题，不是测试代码坏了 —— 别去翻 web/tests 里的文件。\n' >&2
  exit 2
fi

# ---- 1) go.mod / go.sum 是 tidy 状态（CI: Verify go.mod is tidy）----
step '1/7 go.mod tidy'
if [ -z "$(command -v go)" ]; then bad 'go 不可用'; else
  cp go.mod /tmp/.pf-go.mod.bak; cp go.sum /tmp/.pf-go.sum.bak
  go mod tidy >/tmp/.pf-tidy.log 2>&1
  if git diff --quiet -- go.mod go.sum; then
    ok 'go.mod / go.sum 已是 tidy'
  else
    bad 'go.mod / go.sum 不是 tidy 状态 —— 跑 go mod tidy 后提交'
  fi
  # 还原，避免 preflight 顺手改工作区
  cp /tmp/.pf-go.mod.bak go.mod; cp /tmp/.pf-go.sum.bak go.sum
fi

# ---- 2) go vet（CI: go vet ./...）----
step '2/7 go vet'
if go vet ./... >/tmp/.pf-vet.log 2>&1; then
  ok 'go vet 无告警'
else
  bad 'go vet 有告警（细节：/tmp/.pf-vet.log）'
  sed -n '1,15p' /tmp/.pf-vet.log | sed 's/^/      /'
fi

# ---- 3) gofmt（CI: gofmt check）★ 最容易被漏、最不该漏 ----
step '3/7 gofmt'
fmt=$(gofmt -l . 2>/dev/null)
if [ -z "$fmt" ]; then
  ok '全部文件已格式化'
else
  bad "以下文件未格式化（跑 gofmt -w .）：$(echo "$fmt" | tr '\n' ' ')"
fi

# ---- 4) 编译（CI: build 步骤）----
step '4/7 go build'
if go build ./... >/tmp/.pf-build.log 2>&1; then
  ok '编译通过'
else
  bad '编译失败（细节：/tmp/.pf-build.log）'
  sed -n '1,15p' /tmp/.pf-build.log | sed 's/^/      /'
fi

if [ "$QUICK" = 1 ]; then
  SKIPPED+=('go test ./...' '前端回归（node）')
fi

# ---- 5) Go 单测（CI: Unit tests）----
if [ "$QUICK" = 0 ]; then
  step '5/7 go test -race -count=1 ./...'
  # **必须与 CI 逐字一致**：CI 跑的是 `go test -race -count=1 ./...`，
  # 本地跑裸 `go test ./...` 会漏掉两类真缺陷：
  #   - 数据竞争（只有 -race 能看见，2026-09-13 白红一次）
  #   - 测试间互相污染（只有 -count=1 关缓存才暴露）
  # 这条命令的「一致性」由 web/tests/preflight_parity.test.mjs 跨文件守着，
  # 改一边不改另一边会红。
  if go test -race -count=1 ./... >/tmp/.pf-gotest.log 2>&1; then
    ok 'Go 单测全过（含 -race）'
  else
    bad 'Go 单测有失败（细节：/tmp/.pf-gotest.log）'
    grep -E 'WARNING: DATA RACE|^(--- )?FAIL|race detected' /tmp/.pf-gotest.log | head -10 | sed 's/^/      /'
  fi
fi

# ---- 6) 前端回归（CI 里是一堆独立 step；这里统一枚举 web/tests/*.test.mjs）----
#    为什么不像 CI 那样一个个写死：新加测试文件时容易漏加 step，
#    而「漏加 = 这个测试在 CI 里从来不跑」。枚举目录才不会漏。
if [ "$QUICK" = 0 ]; then
  step '6/7 前端回归（web/tests/*.test.mjs）'
  n=0
  for t in web/tests/*.test.mjs; do
    [ -f "$t" ] || continue
    n=$((n + 1))
    if out=$(node "$t" 2>&1); then
      ok "$(basename "$t")"
    else
      bad "$(basename "$t") 失败"
      echo "$out" | grep -E 'FAIL|AssertionError|Error' | head -4 | sed 's/^/      /'
    fi
  done
  [ "$n" = 0 ] && bad 'web/tests/ 下没有找到任何 *.test.mjs —— 检查是不是选错了目录'
fi

# ---- 汇总 ----
if [ "$QUICK" = 0 ]; then
  # ---- 7/7 断言自证脚本（CI 里分三个 step 跑，这里一次跑完）----
  # 这些脚本往**出货文件**里注入真实故障、要求对应断言变红，再显式还原。
  # 它们不进任何闸门就会静态腐烂：实测 scripts/category_guard_inject.py 的一条锚点
  # 早就跟实现脱钩（categoryErrStatus 里 ErrNotManualSkill 被有意删掉），脚本每次
  # 都在打印「锚点失效 ✗」，但因为它既不在 CI 也没人手动跑，谁也没看见。
  # 「永远绿的自证脚本」比没有更糟：它让人以为这块有人看着。
  step '7/7 断言自证'
  selfcheck() {   # $1=标签，其余=命令
    local label="$1"; shift
    local out rc
    out=$("$@" 2>&1); rc=$?
    if [ "$rc" -eq 0 ]; then
      ok "$label"
    else
      bad "$label —— rc=$rc（末尾 12 行见下）"
      printf '%s\n' "$out" | tail -12 | sed 's/^/      /'
    fi
  }
  selfcheck '前端 / 分类结构管理自证'   bash web/tests/category_ui_mutation_check.sh
  selfcheck '前端 / 缓存版本号自证'     bash web/tests/asset_version_mutation_check.sh
  selfcheck '前端 / 勾选层与点外关闭自证' python3 web/tests/chat_ui_mutation_check.py
  selfcheck '前端 / 输入区布局与贴底滚动自证' python3 web/tests/chat_composer_mutation_check.py
  selfcheck '前端 / 思考材料渲染自证'   python3 web/tests/chat_trace_mutation_check.py
  selfcheck '前端 / 线上验收 leg 接线自证' bash web/tests/live_e2e_roster_mutation_check.sh
  # 大模型输出 JSON 的「手抖容忍层」自证。线上真故障（2026-09-17）：模型把
  # input_params.options 写成对象数组 [{"label":"启用","value":"on"}]，Go 严格解 []string
  # 当场报错 → 整轮训练在第 2 步 18 秒中断（用户看到「训练失败」，且这正是
  # 「生成的技能跟我给的东西没关系」的一个隐性来源）。
  selfcheck '后端 / 模型输出手抖容忍自证' bash internal/model/param_flex_mutation_check.sh
  # 「模型链路偷偷不走流式」的守卫自证（线上 353 秒零帧的原发现场）。
  # 它此前是一把**游离尺子**：仓库里有、本机能跑，但没人调用；而且仓库根写死成
  # /root/skillforge，CI 里想接也接不上（已改成从脚本自身位置推导）。
  selfcheck '前端 / 漏接流式守卫自证'   bash web/tests/stage_streaming_guard_mutation_check.sh
  # A1~A6 判定器（judge_stage_streaming_stats.py）自身的三态尺子。
  # 为什么必须有：那份判定逻辑此前只被 judge_stage_streaming_e2e.py 执行，而那个 e2e 要真跑
  # 一轮 20 分钟训练、只在 acceptance 里被调 —— 等于**CI 里没有任何东西碰过它**。于是刚发生过：
  #   ok is None（被证明过的 N/A）→ 打印成 `"PASS" if ok else "FAIL"` → None 假值 → 印 FAIL
  #   → 且计入 bad → 一个「本轮按设计不该跑」的阶段把整条尺子判红（假红）。
  # 现在用合成料把这三种状态钉住，秒级、不需要真跑训练。
  selfcheck '前端 / A1~A6 判定器三态'   python3 web/tests/judge_stats_three_state_check.py
  # 上面那条只钉住「三种状态各自印对」；这条钉住**判据本身松紧两个方向**：
  #   注入1 删掉一条登记理由 → S5(query 缺席) 该红必须红（尺子松了）
  #   注入2 摘掉反向否决 → S6(write 缺席) 必须红（否则 write 也印 N/A = 假绿）
  # 为什么非要有：三态判定里「N/A」是**唯一**能让缺席阶段不算红的口子。这个口子
  # 一旦宽到把 write 也放进来，症状是「一切正常」——只有双向注入才照得出来。
  # 文件名叫 *_mutation_check.py 是刻意的：preflight_parity.test.mjs 按后缀枚举，
  # 漏挂 CI/preflight 会直接红，往后不会腐烂成「0 引用的尺子」。
  selfcheck '前端 / A5 判据双向注入自证' python3 web/tests/judge_a5_double_injection_mutation_check.py
  # 「屏幕上到底有没有东西在动」这把尺子（chat_material_e2e.py 里的 silent_gaps）的离线自证。
  # 为什么必须有：用户的投诉原话就是「一直卡着计时，用户体验不佳」，而这条判据最初按**材料长度**
  # 判变化 —— 材料是截尾滚动窗口、长度顶死在 163 字，于是「正在滚」被误报成 **39.8s 静默**，
  # 差点带着人去修一个没坏的起草跳。改成按材料尾部原文判之后，必须有尺子钉住两个方向：
  #   ① 滚动窗口不得报静默（防假红，否则又去修不存在的问题）；② 真静止必须报出来（防假绿，
  #   否则流真挂住时 M7 会安静地绿）。case ③ 还反过来证明「只看长度的老尺子 = 假绿」。
  selfcheck '前端 / 静默段判据自证'     python3 web/tests/silent_gaps_mutation_check.py
  # 解析服务（ocrd）的单测。它此前又是一把**游离尺子**：本机跑得动、0 引用，
  # 于是钉死的版本串 `ocrd-v5-quality` 一直没跟着真值改名而烂掉（2/15 红）。
  # 已改成「与部署门禁 scripts/deploy_ocrd.sh 对账」——不再抄字面量。
  # 这条对应用户抱怨②：可选中页该直取文本层、不该无脑走 OCR。
  selfcheck 'OCR / 质量判据单测'        python3 deploy/ocr/test_ocrd_quality.py
  # 退出前等在飞请求走完（有上限），且不许把自愈路径堵死 —— 也是 0 引用的游离尺子。
  selfcheck 'OCR / 退出等待与自愈单测'  python3 deploy/ocr/test_guard_wait.py
  # Bug N「停用即从界面蒸发」的自证：跨前端契约 + 后端契约 + Go 行为三层。
  selfcheck '前端 / 停用技能列表自证'   python3 web/tests/skill_disabled_admin_ui_mutation_check.py
  # Bug O「误报目标机没有 python3」的自证：候选名单漂移 / 写死单路径 / 不探活。
  selfcheck '前端 / 解释器探测自证'     python3 web/tests/python_resolve_mutation_check.py
  # 内置「数据治理任务开发」技能：提示词无编译期约束，5 路真故障注入（杜撰 API /
  # 漏 await / 清单少项 / 默认打开 / 分类漂移）必须逐条红在对应断言上。
  # 第 5 路是它抓出来的真洞：分类断言原来写成 `sk.Category != govTaskDevCategory`，
  # 拿常量跟自己比，注入把常量改掉后左右一起变、恒真 —— 尺子假绿。
  selfcheck '后端 / 内置业务技能自证'   python3 scripts/seed_gov_skill_inject.py
  # 上面那条是**文本**断言（提示词里有没有五条硬约束）；下面这条是**真跑**：
  # 拿真 gov-runner + 假 AI 桩，把出货提示词里「## 六、完整示例」的两个示例脚本
  # 抠出来执行，验产出文件内容（示例1 的四行抽取结果逐字、示例2 的模板样式指纹
  # 表头底色 C6D9F1 / 双线 double sz=8 / 列宽 2400 真传进产出）。
  # 为什么必须真跑：示例脚本是**教模型怎么写脚本的范本**，它一旦跟 runner 的 API
  # 脱钩（改名 / 改返回形状），模型照着写出来的脚本在客户现场就是运行期报错 ——
  # 而提示词本身、示例文本、Go 单测全都不会红。文本断言抓不到这一层。
  # 三态：真机（本机有 runner）必须真跑 → GOV_RUNNER_REQUIRED=1 把「找不到 runner」
  # 从 SKIP 升级成 FAIL，不给「永远 SKIP 的尺子」留活路；CI 上没有 runner，会以
  # 醒目的 SKIP 横幅说明「本项不算 PASS、这块无人看守」，不装作绿。
  selfcheck '后端 / 内置技能示例脚本真跑' env GOV_RUNNER_REQUIRED=1 python3 web/tests/gov_examples_mutation_check.py
  selfcheck '后端 / 内置技能示例脚本·注入1（AI 返废话）' env GOV_RUNNER_REQUIRED=1 INJECT=1 python3 web/tests/gov_examples_mutation_check.py
  selfcheck '后端 / 内置技能示例脚本·注入2（素材缺规格）' env GOV_RUNNER_REQUIRED=1 INJECT=2 python3 web/tests/gov_examples_mutation_check.py
  # 上面那条是文本断言；这条真跑：遮蔽候选路径后看探测段认不认 $PATH 上的解释器
  # （用户报的现场就是「python3 在 PATH 上、却被说没有」）。带自证。
  selfcheck '安装脚本 / 解释器探测真跑' python3 web/tests/install_python_probe_live_check.py
  selfcheck '安装脚本 / 解释器探测真跑自证' python3 web/tests/install_python_probe_live_check.py --mutation-selfcheck
  selfcheck '后端 / 推荐行自证'         bash internal/api/suggest_mutation_check.sh
  # 「要文件却被路由到写作技能」的纠偏闸门：失败形态是**静默降级成写正文** ——
  # 用户点「生成 Word」拿到一段文字，页面上没有任何报错。
  selfcheck '后端 / 文件意图纠偏自证'   bash internal/api/chat_route_file_intent_mutation_check.sh
  # 写作链路的步骤板（① 意图分析 → 构思要点 → 按要点执笔）是用户盯着看的唯一东西：
  # 「卡着计时」有一半是板上那格永远停在 active（Carry 接过首格后又追加同名步骤），
  # 另一半是 phase 写了前端认不得的值（编号栏印出英文单词 "plan"、角色徽标空白）。
  # 两条都在线上真帧里取过证，所以断言挂到**出货的前端文件**上（尺子不许对着抄来的表量）。
  selfcheck '后端 / 步骤板渲染契约自证' bash internal/api/chat_board_contract_mutation_check.sh
  selfcheck '后端 / 分类结构管理自证'   python3 scripts/category_guard_inject.py
  selfcheck '后端 / 思考开关矩阵自证'   python3 scripts/fastjson_knob_inject.py
  selfcheck '后端 / 提速与中间材料自证' python3 scripts/thinking_knob_inject.py
  # MCP 接入（后台统一配置 + 开关）：28 路真故障注入 —— 传输层（握手头 / SSE / 翻页 /
  # 传参 / 会话 / 超时）、适配层（工具名 sanitize、schema、isError、僵尸工具、提示词清单、
  # 一台坏不拖垮全部）、HTTP 契约（明文掩码、掩码回存覆盖真 key、开关/删除落库、探测用真
  # key、刷新真重连）、跨层（前端 payload 字段名 ↔ 后端 json tag）。
  # 客户内网里这些退化的共同形态是「后台显示一切正常，对话里却调不到工具」——必须让尺子会红。
  selfcheck '后端 / MCP 接入自证'       python3 scripts/mcp_inject.py
  # 后端 / 沙箱限额：客户现场真阻碍（agents 写个解析脚本一碰大文件就被 cgroup OOM 秒杀，
  # 报成没有信息量的「退出码 -1」；而 MemoryMax=256M 硬编码，单机离线客户无从调）。
  # 正向（四旋钮可覆盖 / 非法值退回默认 / -1 分类成人话）由上面 `go test ./...` 覆盖；
  # 这条补**负向**：注入真故障必须红在预期那条测试、还原后回绿 —— 否则「可覆盖」
  # 三个字可能只是一段没人验过的注释。
  selfcheck '后端 / 沙箱限额自证'        python3 internal/tools/exec_limits_mutation_check.py
  # 离线包 install.sh 的装后服务探测（三段：正向场景 / 两路注入自证）。
  # 注入模式的退出码是反的 —— rc=0 表示「确实按预期转红了」，所以这里不能吞错：
  # selfcheck 把非 0 当失败，正好是我们要的语义。
  selfcheck '安装包 / 装后探测（正向场景）'   bash deploy/offline/tests/install_probe_test.sh
  selfcheck '安装包 / 装后探测自证（注入 1）' env INJECT=1 bash deploy/offline/tests/install_probe_test.sh
  selfcheck '安装包 / 装后探测自证（注入 2）' env INJECT=2 bash deploy/offline/tests/install_probe_test.sh
  # install.sh 其余几把尺子（同样从出货文件里抠真代码跑）。
  # 内存那两条：正向 6 档 + 5 路突变自证（含着「检查整段被删」这类假绿的兜底）。
  selfcheck '安装包 / 装前内存检查'          bash deploy/offline/tests/test_install_memory.sh
  selfcheck '安装包 / 装前内存检查突变自证'  bash deploy/offline/tests/test_install_memory_mutation.sh
  # 装前体检（prediag_gate）：把「这份包在这台机器上能不能跑」在动文件之前说清。
  # 正向 = 从出货 install.sh 里抠出真函数跑五种场景；负向 = 四种真实故障注入必须精确转红。
  selfcheck '安装包 / 装前体检'              bash deploy/offline/tests/test_install_prediag.sh
  selfcheck '安装包 / 装前体检负向自证'      bash deploy/offline/tests/test_install_prediag_mutation.sh
  # 基座体检（-diag / -selftest 沙箱三态）的**双向**真跑：用 systemd 219 的真实帮助文本
  # 造一个假 systemd-run 当「老基座」，跑同一份出货二进制 —— 老基座上必须给出结论与修法，
  # 正常机器上不许乱报同一条。单向的那种（只验老基座）写成「恒报不可用」也能过。
  selfcheck '安装包 / 基座体检双向对照'      python3 deploy/offline/tests/baseline_diag_mutation_check.py
  selfcheck '安装包 / 对外地址默认值'        bash deploy/offline/tests/test_install_public_url.sh
  selfcheck '安装包 / 时区处理'              bash deploy/offline/tests/test_install_timezone.sh
  selfcheck '安装包 / TLS 信任预检'          bash deploy/offline/tests/test_install_trust_precheck.sh
  # 安装期三把尺子（信任预检 / 对外地址 / 时区）的**注入自证**：
  # 正向场景只证明这些字样在当前实现下会出现，不证明实现退化后它们会消失。
  # 4 路真故障各红在预期那条：坏路径被静默忽略 / 无根证书时沉默 /
  # 客户显式 --public-url 被覆盖 / 探测到时区却不写进 env。
  selfcheck '安装包 / 安装期预检突变自证'    bash deploy/offline/tests/test_install_precheck_mutation.sh
  # 需要 root + unshare 造「没有时区库的机器」；条件不满足时脚本自己 SKIP 并说清怎么补。
  selfcheck '自检 / 时区栏离线双向对照'      bash deploy/offline/tests/test_selftest_tz_offline.sh
  # 同目录唯一一条 .py 真跑尺子（`-selftest` 的「文档解析服务」栏 + 4 路注入自证）。
  # 它曾整条免疫：目录里只有它不带 .sh、也不在 web/tests 下，于是写死的检查项数量
  # 在 F3/F5 加了两栏后失配、7 个场景全红，却没有任何闸门调用它 —— 报的还是一句
  # 误导人的「输出格式变了」。接进来这条由 preflight_parity.test.mjs 守着（按目录收，
  # 连扩展名一起收），别再让它掉出去。
  selfcheck '自检 / 解析服务栏真跑 + 注入自证' python3 deploy/offline/tests/selftest_parse_live_check.py
  # 元守卫（preflight_parity.test.mjs）自己的行为自证：它就是拦「零引用的尺子」的那把，
  # 失效形态同样是「一切正常」—— 所以它也得有人拿真故障去戳它。
  selfcheck '接线守卫自证（拿掉接线 / 吞错 / 游离尺子）' bash web/tests/preflight_parity_mutation_check.sh
fi

printf '\n\033[1m========== preflight 汇总 ==========\033[0m\n'
printf '通过 %d 项' "$PASSED"
if [ ${#SKIPPED[@]} -gt 0 ]; then printf '，跳过 %d 项（--quick）' "${#SKIPPED[@]}"; fi
if [ ${#FAILED[@]} -gt 0 ]; then
  printf '，\033[31m失败 %d 项\033[0m\n' "${#FAILED[@]}"
  for f in "${FAILED[@]}"; do printf '  ✗ %s\n' "$f"; done
  printf '\n\033[31m别推 —— CI 会以同样的理由红。\033[0m\n'
  exit 1
fi
printf '，\033[32m0 失败\033[0m\n'
printf '\033[32m可以推了。\033[0m\n'
