#!/usr/bin/env bash
#
# suite_lock_mutation_check.sh —— 套件互斥锁的自证（证明锁**有牙**，而不是「恰好没人并发」）
#
# 守的是什么：scripts/suite-lock.sh 的**拒绝路径**。
#
# 它失灵的形态全是**静默**的：锁文件被挪走、flock 条件写反、探针继承了「已持锁」标记 ——
# 这些都不会有任何红，只是「并发跑出不可归因的红」重新回到这个世界里，而那正是引入锁的原因。
# 所以这里必须逐条钉，而不是「有文件、能跑」就算验过。
#
# 为什么负向注入用**临时锁文件**而不是真锁文件：
#   在 preflight 里跑本自证时，真锁**正被 preflight 自己持有** —— 探针会被正当地拒绝，
#   于是断言「恰好」通过；而单独跑本脚本时没人持锁，同一条断言又必红。
#   那就是一条随调用方式变色的假尺子。所以负向注入用 $TMP 下的锁文件：走的是
#   **同一个 sf_lock 代码路径**（只有文件名不同）；真锁的路径与「谁在用它」由第 1、5 条钉住。
#
# 用法：bash web/tests/suite_lock_mutation_check.sh
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"
LIB='scripts/suite-lock.sh'
PROBE="$ROOT/$LIB"
WANT='已有另一个 skillforge 测试在跑'
TMP="$(mktemp -d)"
SCRATCH="$TMP/probe.lock"
trap 'rm -rf "$TMP"' EXIT

checks=0
fails=0
ok() { checks=$((checks + 1)); echo "ok   $1"; }
bad() { checks=$((checks + 1)); fails=$((fails + 1)); echo "FAIL $1"; }

# 没人持锁时跑探针（清掉继承标记后跑，才是在测「fresh 实例」）
probe_free() {
  env -u SKILLFORGE_SUITE_LOCK_HELD bash "$PROBE" "$ROOT" "$SCRATCH" 2>&1
}

# 有人持锁时跑探针：flock 先拿住 $SCRATCH 再执行探针子进程。
# $1=1 表示**故意不清**继承标记（模拟后人把探针「简化」成不清 env -u）。
# 探针的输出 + rc 一起回传（rc 不能用管道/命令替换的 $? 直接取，要塞进输出里带回来）。
probe_under_lock() {
  local keep="${1:-0}" pre='env -u SKILLFORGE_SUITE_LOCK_HELD'
  [ "$keep" = "1" ] && pre='SKILLFORGE_SUITE_LOCK_HELD=99999'
  flock -n "$SCRATCH" bash -c \
    "$pre bash '$PROBE' '$ROOT' '$SCRATCH' 2>&1; printf '__RC=%s' \$?"
}

# 1. 锁必须落在 .git/ 下 —— 否则锁文件本身就把工作树弄脏，
#    而一条条判据都要求「树必须干净」，会反过来制造假红。
got="$(bash -c "source '$LIB'; sf_lock_path '$ROOT'")"
want="$ROOT/.git/skillforge-suite.lock"
if [ "$got" = "$want" ]; then
  ok "1 默认锁文件落在 .git/ 下（不弄脏工作树）"
else
  bad "1 默认锁文件位置不对：期望 $want，实得 ${got:-空} —— 锁会把自己变成脏树、制造假红"
fi

# 2. 正对照：没人持锁时必须放行。
#    没有这条，「永远拒绝」也会通过第 3 条 —— 那种锁同样没有分辨力，且会天天假红。
out="$(probe_free)"; rc=$?
if [ "$rc" -eq 0 ]; then
  ok "2 正对照：没人持锁时放行（rc=0）"
else
  bad "2 正对照失败：没人持锁也拒绝了（rc=$rc）—— 锁成了「永远拒绝」，它没有分辨力：$out"
fi

# 3. 负向：有人持锁时第二个实例必须拒绝，且必须报出该报的原话
r="$(probe_under_lock 0)"
rc="${r##*__RC=}"
body="${r%__RC=*}"
if [ "$rc" -ne 0 ] && printf '%s' "$body" | grep -qF -- "$WANT"; then
  ok "3 负向：有人持锁时第二实例必须拒绝（rc=$rc 且报出了该报的原话）"
else
  bad "3 负向失败：持锁时第二实例竟然放行了（rc=$rc）—— 锁没有牙，并发互踩会静默回来：$body"
fi

# 4. 陷阱钉桩：不清继承标记时，探针会**假绿放行**。
#    这不是多余的 —— 它就是第 3 条必须写 `env -u` 的理由。把陷阱钉成判据，
#    后人「简化」掉 env -u 时会撞到这条，而不是把第 3 条悄悄变成永远绿。
r="$(probe_under_lock 1)"
rc="${r##*__RC=}"
if [ "$rc" -eq 0 ]; then
  ok "4 陷阱钉桩：不清继承标记时探针假绿放行（rc=0）—— 所以负向探针必须 env -u"
else
  bad "4 陷阱钉桩不符：不清标记竟然也拒绝了（rc=$rc）—— 与「子步骤继承放行」的设计矛盾，先对齐设计再谈别的"
fi

# 5. 接线：两个入口都必须真的挂了这把锁。
#    「锁在、但没人用」= 什么都没变 —— 那才是最可能的退化（摘掉一行就回到互踩）。
for f in scripts/preflight.sh web/tests/live_e2e_roster_mutation_check.sh; do
  if grep -qF 'suite-lock.sh' "$f" && grep -qE '^[^#]*sf_lock ' "$f"; then
    ok "5 $f 已挂锁（source + 真调用 sf_lock）"
  else
    bad "5 $f 没挂锁：引用 suite-lock.sh $(grep -cF 'suite-lock.sh' "$f") 处、非注释调用 sf_lock $(grep -cE '^[^#]*sf_lock ' "$f") 处 —— 有人把锁摘了，并发互踩会回来"
  fi
done

# 6. 还原后必须回绿（不回绿说明负向注入有残留，下一轮起就已经是脏的）
out="$(probe_free)"; rc=$?
if [ "$rc" -eq 0 ]; then
  ok "6 还原后探针回绿（rc=0）"
else
  bad "6 还原后仍然拒绝（rc=$rc）—— 上一轮注入有残留：$out"
fi

echo
echo "--- $((checks - fails))/$checks ok ---"
if [ "$fails" -gt 0 ]; then
  echo "FAILED: 有 $fails 条自证不合格"
  exit 1
fi
echo "自证通过：锁的位置、拒绝路径、继承标记陷阱、两个入口的接线，逐条钉住。"
