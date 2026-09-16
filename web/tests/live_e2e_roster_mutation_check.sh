#!/usr/bin/env bash
# 行为自证：线上验收 leg 名单这张网，注入真实故障后真的会响吗？
#
# 为什么必须有这个脚本（而不是"roster 测试全绿就够了"）：
#   全绿只说明**现在**没坏，不说明断言抓得住坏。web/tests/live_e2e_roster.test.mjs
#   全是「读文本 + 正则」的断言，这类断言最常见的死法是**骑在空集上**：
#   枚举到 0 个文件时，for 循环一次都不进，整个测试"全部通过"。
#   而且它的两个断言对象（leg 声明 / runner 的 SKIP 判据）都是**字符串约定**，
#   写歪一个字符就会退化成永远绿。所以这里注入两类真故障：
#     A 类（结构）——磁盘上多一个没声明 leg 的脚本 / leg 里带脚本没读的键 /
#       runner 退回写死清单 → roster 测试必须红，且必须红在预期那一条。
#     B 类（行为）——伪造 SKIP、伪造"没断言小结"、伪造"一条都没跑"、空目录，
#       看 runner 是不是真判失败；以及伪造两条 leg 看是不是**真执行了两次**
#       （这是"枚举来自声明"这个核心主张的行为证据，纯读文本证明不了）。
#   B 类全程不需要模型、不需要服务：假 leg 脚本只 print，所以它是快的。
#
# 四重判据（缺一不可，与仓库里其它自证脚本同规矩）：
#   1. 注入点必须存在（锚点找不到 = 注入无效 = 等于没测，直接不合格）
#   2. 注入后**必须变红**。"红在崩溃上不算红"：rc!=0 但没有预期的报错文本，
#      说明注入把东西改坏了、而不是断言响了。
#   3. 红的必须**是预期那一条**（每条都钉了要出现的原话片段）
#   4. 还原后必须回绿（不回绿说明注入有残留，下次跑基线就已经脏了）
#   另有正对照（B2/B3）：注入方案不能是"永远判红"——那样 2、3 条也能过。
#
# 用法：bash web/tests/live_e2e_roster_mutation_check.sh
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

ROSTER='web/tests/live_e2e_roster.test.mjs'
RUNNER='scripts/acceptance-live.sh'
PROBE="$ROOT/web/tests/zzz_live_e2e_roster_probe_e2e.py"
TMP="$(mktemp -d)"
RUNNER_BAK="$TMP/acceptance-live.sh.bak"
cp "$RUNNER" "$RUNNER_BAK"

cleanup() {
  rm -f "$PROBE"
  cp "$RUNNER_BAK" "$RUNNER" 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT

checks=0
fails=0
ok() { checks=$((checks + 1)); echo "ok   $1"; }
bad() { checks=$((checks + 1)); fails=$((fails + 1)); echo "FAIL $1"; }

# —— 跑 roster 测试 ——
roster_run() { ROSTER_OUT="$(node "$ROSTER" 2>&1)"; ROSTER_RC=$?; }

# 期望：本轮必须变红，且红的理由里必须出现 $2 这段原话
expect_roster_red() {
  if [ "$ROSTER_RC" -eq 0 ]; then
    bad "$1 —— 注入后 roster 测试仍然全绿：这条断言抓不住该故障"
  elif ! printf '%s' "$ROSTER_OUT" | grep -qF -- "$2"; then
    bad "$1 —— 红了，但不是预期那条（输出里找不到「$2」），红在别处不算红"
  else
    ok "$1（红在预期那条）"
  fi
}

# 注入：把 $2 精确替换成 $3（要求锚点恰好命中 1 处，否则算注入无效）
inject() {
  python3 - "$1" "$2" "$3" <<'PY'
import sys
path, old, new = sys.argv[1], sys.argv[2], sys.argv[3]
s = open(path, encoding='utf-8').read()
n = s.count(old)
if n != 1:
    print(f'锚点失效（期望恰好 1 处，实得 {n}）：{old[:60]}')
    sys.exit(3)
open(path, 'w', encoding='utf-8').write(s.replace(old, new))
PY
}

# 造假 leg 脚本：$1 目录 $2 文件名，内容从 stdin 读
mkfake() { mkdir -p "$1"; cat > "$1/$2"; }
# 清空一个假目录
fresh() { rm -rf "$1"; mkdir -p "$1"; }

# —— runner 行为探针 ——
runner_run() { # $1 = LIVE_E2E_DIR, 其余 env 由调用方自己前置
  RUNNER_OUT="$(LIVE_E2E_DIR="$1" bash "$RUNNER" 2>&1)"
  RUNNER_RC=$?
}
expect_runner() { # $1 desc $2 期望 rc $3 必须出现的片段
  if [ "$RUNNER_RC" -ne "$2" ]; then
    bad "$1 —— 期望 rc=$2，实得 rc=$RUNNER_RC"
  elif ! printf '%s' "$RUNNER_OUT" | grep -qF -- "$3"; then
    bad "$1 —— rc 对了但输出里找不到「$3」"
  else
    ok "$1（rc=$2 且报出了该报的原话）"
  fi
}

echo "==== 基线：现在必须全绿（否则下面的红不能归因于注入）===="
roster_run
if [ "$ROSTER_RC" -eq 0 ]; then ok "基线 roster 测试全绿"; else
  bad "基线就是红的，先修它再谈自证（最后 8 行）：$(printf '%s' "$ROSTER_OUT" | tail -8 | tr '\n' ' ')"
fi

echo
echo "==== A 类：结构注入（roster 测试必须红在预期那条）===="

# A1 磁盘上多一个没声明 leg 的 e2e 脚本
# → 「枚举目录」这个设计的核心断言：新脚本没接线必须被发现，而不是静默不跑。
mkfake "$(dirname "$PROBE")" "$(basename "$PROBE")" <<'EOF'
#!/usr/bin/env python3
print('ok   假装验了点什么')
print('--- 1/1 ok ---')
EOF
if [ -f "$PROBE" ]; then
  roster_run
  expect_roster_red "A1 新增 e2e 脚本但没声明 leg" "没有声明任何 leg"
else
  bad "A1 注入点不存在：假脚本没写进 web/tests/"
fi
rm -f "$PROBE"

# A2 leg 里带一个脚本根本没读的键
# → 这正是"两条 leg 其实跑的是同一件事、报告上却看着覆盖了两种路径"的形态。
mkfake "$(dirname "$PROBE")" "$(basename "$PROBE")" <<'EOF'
# LIVE-LEGS: default GHOST_KNOB=1
import os
print('ok   x')
print('--- 1/1 ok ---')
EOF
roster_run
expect_roster_red "A2 leg 带脚本没读的死键" "但脚本里根本没读"
rm -f "$PROBE"

# A3 runner 退回写死清单
if inject "$RUNNER" 'files=("$E2E_DIR"/*_e2e.py)' 'files=(web/tests/chat_layer_e2e.py)'; then
  roster_run
  expect_roster_red "A3 runner 写死脚本清单" "执行行里出现了写死的脚本名"
  cp "$RUNNER_BAK" "$RUNNER"
else
  bad "A3 注入点不存在：runner 的枚举行写法变了，锚点要跟着改"
fi

# A4 还原后必须回绿
roster_run
if [ "$ROSTER_RC" -eq 0 ]; then ok "A4 结构注入全部还原后 roster 测试回绿"; else
  bad "A4 还原后没回绿（注入有残留）：$(printf '%s' "$ROSTER_OUT" | tail -6 | tr '\n' ' ')"
fi

echo
echo "==== B 类：runner 行为注入（不需要服务/模型，假 leg 只 print）===="

# B1 假 SKIP（playwright 不可用那种）→ 必须判失败，不许"跳过了就当没事"
D="$TMP/skip"
fresh "$D"
mkfake "$D" skip_e2e.py <<'EOF'
# LIVE-LEGS: default
print('SKIP playwright 不可用：假的')
EOF
runner_run "$D"
expect_runner "B1 假 SKIP 必须判失败（SKIP≠PASS）" 1 "SKIP≠PASS"

# B1b 放行 SKIP 也不能变成"通过"
RUNNER_OUT="$(LIVE_E2E_DIR="$D" ALLOW_SKIP=1 bash "$RUNNER" 2>&1)"
RUNNER_RC=$?
if [ "$RUNNER_RC" -ne 0 ]; then
  ok "B1b 全 SKIP 时即使 ALLOW_SKIP=1 也判失败（一个真结论都没有）"
elif printf '%s' "$RUNNER_OUT" | grep -qF "一个真结论都没有"; then
  ok "B1b 全 SKIP 时即使 ALLOW_SKIP=1 也判失败"
else
  bad "B1b 全 SKIP + ALLOW_SKIP=1 竟然报通过 —— SKIP 被洗成了绿"
fi

# B2 正对照：真绿的假 leg 必须通过
# 没有这条，"B1/B5 永远判红"的坏 runner 也能骗过上面几条。
D="$TMP/green"
fresh "$D"
mkfake "$D" green_e2e.py <<'EOF'
# LIVE-LEGS: default
print('ok   假断言')
print('--- 1/1 ok ---')
EOF
runner_run "$D"
expect_runner "B2 正对照：真绿的假 leg 必须通过" 0 "全部真实通过"

# B3 声明驱动：一个脚本声明两条 leg，两条都必须真跑，且 leg 的 env 必须真送到进程
# 这是「枚举来自脚本自己的声明」这个核心主张的行为证据 —— 纯读文本证明不了。
D="$TMP/legs"
fresh "$D"
mkfake "$D" legs_e2e.py <<'EOF'
# LIVE-LEGS: alpha | beta KNOB=1
import os
print(f"LEGRUN KNOB={os.environ.get('KNOB', '<未设置>')}")
print('ok   x')
print('--- 1/1 ok ---')
EOF
runner_run "$D"
if [ "$RUNNER_RC" -ne 0 ]; then
  bad "B3 两条 leg 的假脚本没跑过：rc=$RUNNER_RC"
elif ! printf '%s' "$RUNNER_OUT" | grep -qF -- ':: alpha'; then
  bad "B3 第一条 leg（alpha）没被执行"
elif ! printf '%s' "$RUNNER_OUT" | grep -qF -- ':: beta'; then
  bad "B3 第二条 leg（beta）没被执行 —— 声明里的第 2 条腿被吞了"
elif ! printf '%s' "$RUNNER_OUT" | grep -qF 'LEGRUN KNOB=1'; then
  bad "B3 leg 的 KEY=VAL 没有真送到进程（进程里 KNOB 不是 1）"
elif ! printf '%s' "$RUNNER_OUT" | grep -qF 'LEGRUN KNOB=<未设置>'; then
  bad "B3 alpha 那条不该有 KNOB，但进程里也读到了值 —— 说明 env 漏到了别的 leg"
else
  ok "B3 两条 leg 都真跑了，且只有声明了 KNOB 的那条拿到 KNOB=1"
fi

# B4 一条都没跑（被 ONLY 全过滤）→ 必须失败，空跑不能算通过
D="$TMP/legs2"
fresh "$D"
mkfake "$D" legs_e2e.py <<'EOF'
# LIVE-LEGS: alpha
print('ok   x')
print('--- 1/1 ok ---')
EOF
RUNNER_OUT="$(LIVE_E2E_DIR="$D" ONLY=zzz_no_such_leg bash "$RUNNER" 2>&1)"
RUNNER_RC=$?
expect_runner "B4 一条 leg 都没跑（ONLY 全过滤）必须失败" 1 "一条 leg 都没跑"

# B5 exit 0 但没有断言小结 → 必须失败（"没有任何断言的绿"是最常见的假绿）
D="$TMP/nosummary"
fresh "$D"
mkfake "$D" nosummary_e2e.py <<'EOF'
# LIVE-LEGS: default
print('好像跑完了')
EOF
runner_run "$D"
expect_runner "B5 没打断言小结的假绿必须判失败" 1 "绿得没有断言"

# B6 空目录 → 必须失败（枚举到 0 个脚本时最危险：跑 0 个 leg 然后报绿）
D="$TMP/empty"
fresh "$D"
runner_run "$D"
expect_runner "B6 目录里没有 *_e2e.py 时必须报错" 1 "没找到任何 *_e2e.py"

# B7 没有 leg 声明的脚本 → 必须失败（不是静默跳过）
D="$TMP/nodecl"
fresh "$D"
mkfake "$D" nodecl_e2e.py <<'EOF'
print('ok   x')
print('--- 1/1 ok ---')
EOF
runner_run "$D"
expect_runner "B7 脚本没声明 leg 时 runner 必须报错而非静默跳过" 1 "没有 # LIVE-LEGS: 声明"

# B8 逐腿超时必须真生效：leg 声明 TIMEOUT_S=1，脚本睡 5 秒 —— 必须在 1 秒上被砍。
# 这条守的是「长腿自己报数」这个机制：420s 的默认值砍 12 分钟的训练腿等于永远红，
# 而如果 TIMEOUT_S 被无视（还是用默认值），腿上那些「太慢了」的判断就全是假的。
D="$TMP/legtimeout"
fresh "$D"
mkfake "$D" slow_e2e.py <<'EOF'
# LIVE-LEGS: default TIMEOUT_S=1
import time
time.sleep(5)
print('SHOULD_NOT_FINISH 睡满了 5 秒 —— 逐腿超时没生效')
print('--- 1/1 ok ---')
EOF
runner_run "$D"
if [ "$RUNNER_RC" -eq 0 ]; then
  bad "B8 TIMEOUT_S=1 竟然放过了睡 5 秒的腿 —— 逐腿超时被无视了"
elif ! printf '%s' "$RUNNER_OUT" | grep -qF -- 'rc=124'; then
  bad "B8 腿被砍了，但报告里没有 rc=124（超时的原话）—— 分不清「超时」和「断言失败」"
elif printf '%s' "$RUNNER_OUT" | grep -qF 'SHOULD_NOT_FINISH'; then
  bad "B8 TIMEOUT_S=1 的腿居然睡满了 5 秒并打了自己的小结"
else
  ok "B8 逐腿超时真生效：TIMEOUT_S=1 的腿在 1 秒上被砍（rc=124），脚本没跑完"
fi

# B9 超时值写错（不是秒数）必须当场报错，不许**静默退回**默认值继续跑 ——
# 静默退回的后果是：以为自己在跑一个 35 分钟的长腿，实际被 420s 砍掉，报的是「超时」。
D="$TMP/badtimeout"
fresh "$D"
mkfake "$D" badto_e2e.py <<'EOF'
# LIVE-LEGS: default TIMEOUT_S=35min
print('ok   x')
print('--- 1/1 ok ---')
EOF
runner_run "$D"
expect_runner "B9 非秒数的 TIMEOUT_S 必须当场报错（不许静默退回默认值）" 1 "不是秒数"

echo
echo "==== 收尾：全部还原后必须回绿 ===="
roster_run
if [ "$ROSTER_RC" -eq 0 ]; then ok "收尾 roster 测试回绿"; else
  bad "收尾没回绿：$(printf '%s' "$ROSTER_OUT" | tail -6 | tr '\n' ' ')"
fi
D="$TMP/finalgreen"
fresh "$D"
mkfake "$D" green_e2e.py <<'EOF'
# LIVE-LEGS: default
print('ok   x')
print('--- 1/1 ok ---')
EOF
runner_run "$D"
expect_runner "收尾 runner 对真绿的假 leg 仍判通过" 0 "全部真实通过"

echo
echo "--- $((checks - fails))/$checks ok ---"
if [ "$fails" -gt 0 ]; then
  echo "FAILED: 有 $fails 条自证不合格"
  exit 1
fi
echo "自证通过：结构注入 4 条 + 行为注入 10 条，红的都是预期那条，还原后全绿。"
