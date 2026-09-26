#!/usr/bin/env bash
#
# suite-lock.sh —— 整仓测试套件的互斥锁（可 source，也可当探针执行）
#
# 为什么必须有它（2026-09-26 实测钉死，不是猜的）：
#   scripts/preflight.sh 与 web/tests/live_e2e_roster_mutation_check.sh 都会
#   **就地改写仓库里的真文件**：
#     · scripts/acceptance-live.sh（runner 本体：A3 注入写死清单）
#     · web/tests/admin_train_progress_e2e.py、web/tests/chat_doubts_e2e.py（两条真 leg）
#     · web/tests/zzz_live_e2e_roster_probe_e2e.py（探针**必须**落在 web/tests/ 下 ——
#       roster 测试的职责就是枚举 web/tests/*_e2e.py，挪到临时目录那条断言就假了）
#   除此之外它们还断言「树必须干净」，并会为缓存版本号自证**提交一次 mutation 再 reset**。
#   两个实例并发跑必然互踩：一个实例注入期间，另一个的基线/收尾断言看到的是被改过的世界。
#
#   实测形态（同代码、同机器、同秒起跑，两实例背靠背）：
#     A: 19/21 ok  RC=1 （FAIL B3 两条 leg 的假脚本没跑过：rc=2；FAIL B5 期望 rc=1 实得 rc=0）
#     B: 21/21 ok  RC=0
#   一红一绿 → 这种红**不可归因**。它比「不跑」更毒：会让人去怀疑一棵好树、去改好代码。
#   同一形态也解释过一批「单跑全绿、全量跑红」的假红（含 reflog 里两次
#   `mutation: style.css 内容变了，?v= 没 bump` 提交）。
#
# 用法：
#   source scripts/suite-lock.sh && sf_lock "$PWD"      # 调用方进程生命周期内持锁
#   bash scripts/suite-lock.sh [ROOT] [锁文件]           # 可执行探针（自证用）
#
# 退出码：拿到锁 0；已有实例在跑 1（并说明是谁在跑）。
# 锁绑在 fd 上、随进程退出自动释放 —— kill -9 也不会留下死锁。
set -uo pipefail

# 锁文件放 .git/ 里：既不弄脏工作树，又天然随仓库走（同一仓的多个入口共用一把）。
sf_lock_path() {
  local root="$1"
  if [ -d "$root/.git" ]; then
    printf '%s' "$root/.git/skillforge-suite.lock"
  else
    printf '%s' "/tmp/skillforge-suite-$(printf '%s' "$root" | md5sum | cut -c1-8).lock"
  fi
}

sf_lock() {
  local root="${1:-$PWD}"
  local lock="${2:-}"
  [ -n "$lock" ] || lock="$(sf_lock_path "$root")"

  # 子步骤（preflight 会 fork 出一堆自证脚本）继承的是**同一个实例**的锁：
  # 标记在就放行 —— 否则子步骤会自杀式地拒绝自己。
  # ★ 自证要测「第二个实例必须拒绝」时必须 `env -u SKILLFORGE_SUITE_LOCK_HELD`，
  #   否则测的是继承来的标记 → 永远放行 → 假绿
  #   （见 web/tests/suite_lock_mutation_check.sh 第 4 条，那条就是钉这个陷阱的）。
  if [ -n "${SKILLFORGE_SUITE_LOCK_HELD:-}" ]; then
    return 0
  fi

  # 用 >> 打开：不能把持锁者写的「谁在跑」截断掉，否则报错里就丢了归因线索。
  exec 8>>"$lock" || { printf '\033[31m✗ 锁文件打不开：%s\033[0m\n' "$lock" >&2; exit 1; }
  if ! flock -n 8; then
    local holder
    holder="$(head -1 "$lock" 2>/dev/null || true)"
    printf '\033[31m✗ 已有另一个 skillforge 测试在跑（%s）—— 已拒绝启动本实例\033[0m\n' \
      "${holder:-读不到持有者}" >&2
    printf '  本套件会就地改写真源文件、并断言「树必须干净」，并发跑必然互踩、产出不可归因的红。\n' >&2
    printf '  等它结束再跑；或先确认没有别的 preflight / roster 自证在跑。\n' >&2
    exit 1
  fi
  printf 'pid=%s 起于=%s root=%s\n' "$$" "$(date '+%F %T')" "$root" > "$lock"
  export SKILLFORGE_SUITE_LOCK_HELD="$$"
  return 0
}

# 可执行探针模式（自证用；锁随进程退出释放）。source 时 BASH_SOURCE[0] != $0，不会误触发。
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  sf_lock "${1:-$PWD}" "${2:-}"
  echo "LOCKED"
fi
