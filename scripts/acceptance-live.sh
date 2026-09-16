#!/usr/bin/env bash
# 线上验收 runner：把 web/tests/*_e2e.py 的每一条 leg 真跑一遍。
#
# 为什么要有这个文件（而不是"需要时我手动敲命令"）：
#   web/tests/*_e2e.py 是**真浏览器 + 真服务 + 真模型**的终验 —— CI 里跑不了
#   （runner 上没有模型、没有起着的服务），所以它们天然进不了 CI，
#   于是天然会腐烂：脚本里写歪的锚点、改名后没人跟上的环境变量、
#   被删掉的一段断言，都不会有人发现。2026-09-15 复查时它们的真实处境就是
#   **零引用**：ci.yml 里没有、preflight.sh 里没有、docs 里没有，只有我脑子和
#   /tmp 里几个日志。这不是"防线"，这是"临时手段"。
#   所以：枚举与命名统一收到本文件 + 每个脚本自己的 `# LIVE-LEGS:` 声明里，
#   而「枚举不许写死、声明不许跟文件脱钩」由
#   web/tests/live_e2e_roster.test.mjs（进 CI/preflight）+ live_e2e_roster_mutation_check.sh
#   （行为自证）守着。
#
# 关键约定（每一条都是踩过的坑，别改）：
#   * 枚举用通配 web/tests/*_e2e.py，**禁止写死文件名** ——
#     写死的清单漏掉一个 = 那条 leg 永远不存在，而且没有任何提示。
#   * 跑哪几条 leg 由每个脚本自己的 `# LIVE-LEGS:` 行声明，本文件只做解析。
#     leg 里只能带**无空格**的 KEY=VAL（提示词这类长文本必须放进脚本内的
#     预设表，见 chat_material_e2e.py 的 PROMPT_PRESETS）。
#   * **SKIP != PASS**：脚本打 SKIP 并 exit 0（playwright 不可用 / 页面打不开）时
#     本文件默认判 FAIL。要放行必须显式 ALLOW_SKIP=1，且放行后它仍然只是
#     "未验证"，小结里单独计数、不许混进"通过"。
#   * 绿必须有断言条数：只 exit 0 但不打 `--- N/M ok ---` 的 leg 判 FAIL——
#     "没有任何断言的绿"是最常见的一种假绿。
#
# 用法：
#   bash scripts/acceptance-live.sh                    # 打 127.0.0.1:8092
#   BASE=http://127.0.0.1:9999 bash scripts/acceptance-live.sh
#   ONLY=docgen bash scripts/acceptance-live.sh        # 只跑名字含 docgen 的 leg
#   ALLOW_SKIP=1 bash scripts/acceptance-live.sh       # 放行 SKIP（仍是"未验证"）
#   LIVE_E2E_DIR=/tmp/fake bash scripts/acceptance-live.sh   # 自证用：换目录
#
# 退出码：0 = 所有 leg 都真通过；1 = 有失败 / 有 SKIP / 一条都没跑。
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

BASE="${BASE:-http://127.0.0.1:8092}"
E2E_DIR="${LIVE_E2E_DIR:-web/tests}"
LOGDIR="${LIVE_LOGDIR:-/tmp/acceptance-live}"
LEG_TIMEOUT="${LIVE_TIMEOUT:-420}"   # 单条 leg 上限（秒）；单轮真模型约 20~70s
ALLOW_SKIP="${ALLOW_SKIP:-0}"
ONLY="${ONLY:-}"

mkdir -p "$LOGDIR"

shopt -s nullglob
files=("$E2E_DIR"/*_e2e.py)
if [ "${#files[@]}" -eq 0 ]; then
  echo "::error::$E2E_DIR 下没找到任何 *_e2e.py —— 目录选错或验收脚本被删光，拒绝静默通过" >&2
  exit 1
fi

run=0
npass=0
fail=0
nskip=0
fail_list=()
rows=()

legs_of() {
  # 取第一条 `# LIVE-LEGS:` 声明的值（声明只有一行，多行即为错，由 roster 测试守着）
  awk '/^#[[:space:]]*LIVE-LEGS:/{sub(/^#[[:space:]]*LIVE-LEGS:[[:space:]]*/,""); print; exit}' "$1"
}

echo "线上验收：BASE=$BASE  目录=$E2E_DIR  共 ${#files[@]} 个脚本  单腿上限 ${LEG_TIMEOUT}s"

for f in "${files[@]}"; do
  spec="$(legs_of "$f")"
  if [ -z "$spec" ]; then
    echo "::error::$f 没有 # LIVE-LEGS: 声明 —— 枚举到了它，却不知道要跑哪条 leg。" >&2
    fail=$((fail + 1))
    fail_list+=("$(basename "$f")(无 leg 声明)")
    rows+=("FAIL   $(basename "$f")  ::  <缺 # LIVE-LEGS: 声明>")
    continue
  fi

  IFS='|' read -r -a parts <<<"$spec"
  for p in "${parts[@]}"; do
    p="$(printf '%s' "$p" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
    [ -z "$p" ] && continue
    read -r -a toks <<<"$p"
    leg="${toks[0]}"
    # 单腿超时：默认 LEG_TIMEOUT（LIVE_TIMEOUT 覆盖），但长腿得能自己报数 ——
    # 训练页「长阶段流式」那条要真建一次技能，12~25 分钟；拿 420s 的默认值去砍它，
    # 结果是这条尺子**每次都红在超时上**，下场就是被人忽略（等于又回到没人看）。
    # 所以允许 leg 自己写 TIMEOUT_S=秒数：runner 用它做外层硬砍，同一个键也传进脚本，
    # 脚本拿它算自己的等待预算（roster 测试要求 leg 里的键必须被脚本真读到）。
    leg_timeout="$LEG_TIMEOUT"
    assigns=()
    for kv in ${toks[@]:1}; do
      case "$kv" in
        TIMEOUT_S=*) leg_timeout="${kv#TIMEOUT_S=}" ;;
        *) assigns+=("$kv") ;;
      esac
    done
    case "$leg_timeout" in
      ''|*[!0-9]*) echo "::error::$f 的 leg「$leg」TIMEOUT_S=$leg_timeout 不是秒数" >&2; exit 1 ;;
    esac

    if [ -n "$ONLY" ] && [[ "$leg" != *"$ONLY"* ]]; then
      continue
    fi

    run=$((run + 1))
    log="$LOGDIR/$(basename "$f" .py).$leg.log"
    printf '\n======================= %s :: %s   (env: %s / 超时 %ss)\n' \
      "$f" "$leg" "${assigns[*]:-无}" "$leg_timeout"

    start="$(date +%s)"
    # 注意：`${assigns[@]:-}` 在空数组时会**多吐一个空参数**（env 收到空字符串当命令），
    # 结果是把「不带 env 的 leg」全判红 —— 这正是本文件自证脚本 B3 抓出来的。
    # 空数组安全展开只有一种写法：`${arr[@]+"${arr[@]}"}`。
    env ${assigns[@]+"${assigns[@]}"} TIMEOUT_S="$leg_timeout" BASE="$BASE" \
      timeout "$leg_timeout" python3 "$f" >"$log" 2>&1
    rc=$?
    dur=$(( $(date +%s) - start ))
    cat "$log"

    summary="$(grep -oE -- '--- [0-9]+/[0-9]+ ok ---' "$log" | tail -1)"
    if [ "$rc" -ne 0 ]; then
      fail=$((fail + 1))
      fail_list+=("$leg(rc=$rc)")
      rows+=("FAIL   $(basename "$f")  ::  $leg  rc=$rc  ${dur}s")
    elif grep -qE '^SKIP' "$log"; then
      # SKIP != PASS：脚本自己打了个体面的"跳过"，但这件事**没有被验证**。
      if [ "$ALLOW_SKIP" = "1" ]; then
        nskip=$((nskip + 1))
        rows+=("SKIP   $(basename "$f")  ::  $leg  （未验证，ALLOW_SKIP=1 放行）  ${dur}s")
      else
        fail=$((fail + 1))
        fail_list+=("$leg(SKIP)")
        rows+=("FAIL   $(basename "$f")  ::  $leg  SKIP≠PASS（要放行就显式 ALLOW_SKIP=1）  ${dur}s")
      fi
    elif [ -z "$summary" ]; then
      fail=$((fail + 1))
      fail_list+=("$leg(无断言小结)")
      rows+=("FAIL   $(basename "$f")  ::  $leg  没打 '--- N/M ok ---'，绿得没有断言  ${dur}s")
    else
      nums="$(printf '%s' "$summary" | grep -oE '[0-9]+/[0-9]+')"
      a="${nums%%/*}"
      b="${nums##*/}"
      if [ "$a" = "$b" ] && [ "$b" -gt 0 ]; then
        npass=$((npass + 1))
        rows+=("PASS   $(basename "$f")  ::  $leg  断言 $nums  ${dur}s")
      else
        fail=$((fail + 1))
        fail_list+=("$leg($nums 不全绿)")
        rows+=("FAIL   $(basename "$f")  ::  $leg  断言 $nums 不全绿  ${dur}s")
      fi
    fi
  done
done

echo
echo "============================= 线上验收小结 ============================="
for r in "${rows[@]}"; do
  echo "  $r"
done
echo "  ------------------------------------------------------------------"
echo "  leg 共 $run 条：真通过 $npass / 失败 $fail / 未验证(SKIP) $nskip"
echo "  日志目录：$LOGDIR"

if [ "$run" -eq 0 ]; then
  echo "::error:: 一条 leg 都没跑（是被 ONLY=${ONLY} 全过滤掉了？）—— 空跑不能算通过" >&2
  exit 1
fi
if [ "$npass" -eq 0 ] && [ "$fail" -eq 0 ]; then
  echo "::error:: 所有 leg 都是 SKIP —— 一个真结论都没有，绝不能报通过" >&2
  exit 1
fi
if [ "$fail" -gt 0 ]; then
  echo "::error:: 失败的 leg：${fail_list[*]}" >&2
  exit 1
fi
if [ "$nskip" -gt 0 ]; then
  echo "::warning:: 有 $nskip 条 leg 是 SKIP（未验证，ALLOW_SKIP=1 放行的）——" \
    "这不是绿，只是没测。"
fi
echo "全部真实通过：$npass/$run（未验证 $nskip 条）"
