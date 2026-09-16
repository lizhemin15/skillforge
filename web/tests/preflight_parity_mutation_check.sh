#!/usr/bin/env bash
# 行为自证：把关「每条尺子都被真调用」的这把元守卫，自己抓不抓得住坏？
#
# 为什么必须有这个脚本（而不是「parity 测试全绿就够了」）：
#   web/tests/preflight_parity.test.mjs 是整个仓库里唯一拦「零引用的尺子」的东西 ——
#   它读 ci.yml / scripts/preflight.sh 的文本，断言里面出现了每一条自证脚本的路径。
#   于是它自己也长着同一副牙口：**纯文本断言 + 按目录枚举**，最经典的死法就是
#   骑在空集或写歪一点就永远绿。更要命的是它一旦失效，失效形态是「一切正常」：
#   所有尺子照旧、CI 照旧全绿，只是有天有人发现某项从来没跑过。
#   所以这里对它注入真故障，看它是不是真的会响、响在预期那一条：
#     A 类（接线）—— 从 ci.yml / preflight.sh 里拿掉一条真调用 → 必须红，且点名是哪个文件少哪条。
#     B 类（注入模式）—— 拿掉 INJECT=2、给注入行加 `|| true` → 必须红。
#        「看着有、其实没传」和「注入了但把红吞掉」是负向自证最常见两种假绿，
#        它们的共同点是本地绿、CI 也绿。
#     C 类（枚举活性）—— 往 deploy/offline/tests/ 丢一个游离的真跑型 .py，
#        没人接线 → 必须红并点名这个新文件。这条是「按目录收」这个设计的
#        核心主张的行为证据：往那儿放尺子的人不需要记得改守卫文件，
#        但守卫必须能看见他。纯读文本证明不了这一点。
#
# 四重判据（缺一不可，与仓库里其它自证脚本同规矩）：
#   1. 注入点必须存在（锚点找不到 = 注入无效 = 等于没测，直接不合格）
#   2. 注入后**必须变红**。rc!=0 但没有预期的报错文本不算红（那多半是把文件改坏了，
#      不是断言响了）
#   3. 红的必须**是预期那一条**（每条都钉了要出现的原话片段）
#   4. 还原后必须回绿（不残留，否则下次跑基线就已经脏了）
#
# 用法：bash web/tests/preflight_parity_mutation_check.sh
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

GUARD='web/tests/preflight_parity.test.mjs'
CI_FILE='.github/workflows/ci.yml'
PF_FILE='scripts/preflight.sh'
NEWLEDGER='deploy/offline/tests/selftest_parse_live_check.py'

TMP="$(mktemp -d)"
CI_BAK="$TMP/ci.yml.bak"
PF_BAK="$TMP/preflight.sh.bak"
GUARD_BAK="$TMP/preflight_parity.test.mjs.bak"
cp "$CI_FILE" "$CI_BAK"
cp "$PF_FILE" "$PF_BAK"
cp "$GUARD" "$GUARD_BAK"
STRAY="$ROOT/deploy/offline/tests/zzz_stray_probe_check.py"

cleanup() {
  rm -f "$STRAY"
  cp "$CI_BAK" "$CI_FILE" 2>/dev/null || true
  cp "$PF_BAK" "$PF_FILE" 2>/dev/null || true
  cp "$GUARD_BAK" "$GUARD" 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT

checks=0
fails=0
ok() { checks=$((checks + 1)); echo "ok   $1"; }
bad() { checks=$((checks + 1)); fails=$((fails + 1)); echo "FAIL $1"; }

guard_run() { GUARD_OUT="$(node "$GUARD" 2>&1)"; GUARD_RC=$?; }

# 期望：本轮必须变红，且红的理由里必须出现 $2 这段原话
expect_red() {
  if [ "$GUARD_RC" -eq 0 ]; then
    bad "$1 —— 注入后守卫仍然全绿：这条断言抓不住该故障（这正是最危险的形态）"
  elif ! printf '%s' "$GUARD_OUT" | grep -qF -- "$2"; then
    bad "$1 —— 红了，但不是预期那条（输出里找不到「$2」），红在别处不算红"
  else
    ok "$1（红在预期那条）"
  fi
}

# 注入：把文件 $1 里 $2 精确替换成 $3（锚点必须恰好命中 1 处，否则算注入无效）
inject() {
  python3 - "$1" "$2" "$3" <<'PY'
import sys
path, old, new = sys.argv[1], sys.argv[2], sys.argv[3]
s = open(path, encoding='utf-8').read()
n = s.count(old)
if n != 1:
    print(f'锚点失效（期望恰好 1 处，实得 {n}）：{old[:70]}')
    sys.exit(3)
open(path, 'w', encoding='utf-8').write(s.replace(old, new))
PY
}

say_inject_fail() { bad "$1 注入点不存在：$2（说明守卫/被守的文件的写法变了，锚点要跟着改）"; }

echo "==== 基线：现在必须全绿（否则下面的红不能归因于注入）===="
guard_run
if [ "$GUARD_RC" -eq 0 ]; then ok "基线 preflight_parity 全绿"; else
  bad "基线就是红的，先修它再谈自证（最后 8 行）：$(printf '%s' "$GUARD_OUT" | tail -8 | tr '\n' ' ')"
fi

echo
echo "==== A 类：接线注入（拿掉一条真调用，守卫必须点名）===="

# A1 ci.yml 少调一条 → 红，且必须点名「ci.yml 里没有调用 …」
if inject "$CI_FILE" "        run: python3 $NEWLEDGER" "        # mutation：临时删掉这一行"; then
  guard_run
  expect_red "A1 ci.yml 少调 deploy/offline/tests 的真跑 .py" "ci.yml 里没有调用 $NEWLEDGER"
  cp "$CI_BAK" "$CI_FILE"
else
  say_inject_fail "A1" "ci.yml 里没有「run: python3 $NEWLEDGER」这一行"
fi

# A2 preflight.sh 少调一条 → 红，且必须点名「scripts/preflight.sh 里没有调用 …」
if inject "$PF_FILE" "selfcheck '自检 / 解析服务栏真跑 + 注入自证' python3 $NEWLEDGER" "  # mutation：临时删掉这一行"; then
  guard_run
  expect_red "A2 preflight.sh 少调同一条 → 本地闸门比 CI 少一道" "scripts/preflight.sh 里没有调用 $NEWLEDGER"
  cp "$PF_BAK" "$PF_FILE"
else
  say_inject_fail "A2" "preflight.sh 里没有那条 selfcheck"
fi

# A3 把守卫自己的枚举规则打瘸（让 deploy/offline/tests 那一组一个文件都收不到）
#    → 必须红在**按组**那条断言上（点名是哪一组塌了），而不是拿一句「总数不够」糊过去。
#    这条打的是本文件最怕的形态：**骑在空集上全绿**。老写法（几组拼成一个大数组 +
#    一句全局下限）对这种「单组塌陷」0 报错 —— 本自证脚本第一次跑就抓到了这个真缺陷。
#    （注：「两边同时拿掉」不单独断言 —— node 的 assert 在同一个 test 里第一个失败就抛出，
#      要求两条点名同时出现是在要求工具做不到的事；preflight 那半边由 A2 单独覆盖。）
if inject "$GUARD" "    .filter((f) => f.endsWith('.sh') || (f.endsWith('.py') && !PY_HELPERS.has(f)))" "    .filter(() => false)"; then
  guard_run
  expect_red "A3 守卫自己的枚举被打瘸（deploy/offline/tests 整组塌成 0）" "枚举规则失效：deploy/offline/tests"
  cp "$GUARD_BAK" "$GUARD"
else
  say_inject_fail "A3" "守卫里的 deploy/offline/tests 枚举规则写法变了"
fi

echo
echo "==== B 类：注入模式（看着有、其实没传 / 注入了但把红吞掉）===="

# B1 拿掉 ci.yml 的 INJECT=2 行 → 红，必须报「没有以「INJECT=2」真跑」
if inject "$CI_FILE" "          INJECT=2 bash deploy/offline/tests/install_probe_test.sh
" ""; then
  guard_run
  expect_red "B1 ci.yml 少了 INJECT=2 那一行" "没有以「INJECT=2」真跑"
  cp "$CI_BAK" "$CI_FILE"
else
  say_inject_fail "B1" "ci.yml 里没有单独一行 INJECT=2 bash deploy/offline/tests/install_probe_test.sh"
fi

# B2 给注入行加 || true → 红，必须报「吞错写法」
if inject "$CI_FILE" "          INJECT=1 bash deploy/offline/tests/install_probe_test.sh" \
                    "          INJECT=1 bash deploy/offline/tests/install_probe_test.sh || true"; then
  guard_run
  expect_red "B2 注入行后面挂 || true → 承认「没红也算过」" "吞错写法"
  cp "$CI_BAK" "$CI_FILE"
else
  say_inject_fail "B2" "ci.yml 里没有 INJECT=1 那一行"
fi

echo
echo "==== C 类：枚举活性（往黑洞目录丢一个没接线的真跑尺子）===="

# C1 新增游离 .py（真跑型名字）→ 必须红并点名它。
#    这条不能用「往已有文件里塞内容」来代替：要证的正是「新文件被自动看见」。
cat > "$STRAY" <<'EOF'
#!/usr/bin/env python3
# mutation 探针：一条没人接线的真跑尺子
print('--- 1/1 ok ---')
EOF
if [ -f "$STRAY" ]; then
  guard_run
  expect_red "C1 黑洞目录新增没接线的 .py" "里没有调用 deploy/offline/tests/zzz_stray_probe_check.py"
else
  bad "C1 注入点不存在：探针文件没写进 deploy/offline/tests/"
fi
rm -f "$STRAY"

echo
echo "==== 收尾：全部还原后必须回绿 ===="
guard_run
if [ "$GUARD_RC" -eq 0 ]; then ok "收尾 preflight_parity 回绿（注入无残留）"; else
  bad "收尾没回绿：$(printf '%s' "$GUARD_OUT" | tail -6 | tr '\n' ' ')"
fi

# 还原是真还原（不是「碰巧绿」）：拿 git 比对两个被注入过的文件
if command -v git >/dev/null 2>&1 && git -C "$ROOT" rev-parse --git-dir >/dev/null 2>&1; then
  DIRTY="$(git -C "$ROOT" diff --name-only -- "$CI_FILE" "$PF_FILE")"
  if [ -z "$DIRTY" ]; then
    ok "收尾 ci.yml / preflight.sh 与 HEAD 逐字节一致（还原干净）"
  else
    # 允许「本轮本来就改了这两个文件」的情况：那时 diff 非空是正常的，
    # 换成与脚本开跑时的备份比对，才是真的还原判据。
    if diff -q "$CI_BAK" "$CI_FILE" >/dev/null && diff -q "$PF_BAK" "$PF_FILE" >/dev/null; then
      ok "收尾两个文件与开跑时的备份逐字节一致（本轮改动未被自证污染）"
    else
      bad "收尾还原不干净：$DIRTY 与开跑时的备份不一致"
    fi
  fi
fi

echo
echo "--- $((checks - fails))/$checks ok ---"
if [ "$fails" -gt 0 ]; then
  echo "FAILED: 有 $fails 条自证不合格"
  exit 1
fi
echo "自证通过：接线注入 2 条 + 按组枚举守卫 1 条 + 注入模式 2 条 + 枚举活性 1 条 + 收尾 2 条，红的都是预期那条，还原后全绿。"
