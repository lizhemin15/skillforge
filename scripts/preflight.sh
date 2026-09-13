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
  step '7/7 断言自证（5 条）'
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
  selfcheck '后端 / 推荐行自证'         bash internal/api/suggest_mutation_check.sh
  selfcheck '后端 / 分类结构管理自证'   python3 scripts/category_guard_inject.py
  selfcheck '后端 / 思考开关矩阵自证'   python3 scripts/fastjson_knob_inject.py
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
