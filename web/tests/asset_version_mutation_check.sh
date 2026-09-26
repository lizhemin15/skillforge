#!/usr/bin/env bash
# 缓存版本号守卫（web/tests/asset_version.test.mjs）的「断言自证」脚本。
#
# 为什么专门给这一条写自证：这个守卫的前三道断言（HTML↔manifest↔磁盘）长期
# 看起来在守「改了文件忘了 bump ?v=」，其实守不住——gen-asset-manifest.mjs
# 会把当前哈希照抄进 manifest、v 也照抄 HTML，所以只要按文档重新生成一次，
# 前三道全绿，?v= 一个字没动也全绿。第四道（git 比对）就是补这个洞的，
# 而「补洞的断言自己是不是真的会红」必须现场验一次，不能靠看着像。
#
# 用法：bash web/tests/asset_version_mutation_check.sh
set -u
cd "$(dirname "$0")/../.."
TEST=web/tests/asset_version.test.mjs
GEN=web/tests/gen-asset-manifest.mjs
HTMLS=(web/index.html web/admin.html)

BAK=$(mktemp -d)
cp "$TEST" "$BAK/$(basename $TEST)"
cp "$GEN" "$BAK/$(basename $GEN)"
cp web/assets.manifest.json "$BAK/manifest.json"
for h in "${HTMLS[@]}"; do cp "$h" "$BAK/$(basename $h)"; done
cp web/css/style.css "$BAK/style.css"

# 第 1 条注入要造出「这次提交里资源内容变了、?v= 没动」这个状态，而它只能靠
# git 比对才看得见 —— 所以注入会临时提交一次（见下面第 1 条）。这里记住原始
# 提交，还原时回滚。
#
# ⚠️ 回滚用 `reset --mixed` + 只 checkout 本脚本碰过的两个文件，**不能用
# `reset --hard`**：hard 会把工作区里所有未提交改动一起抹掉 —— 包括此刻正在
# 改这个脚本的人（写完脚本还没提交就来跑一次自证，是很自然的动作）。
# 实测踩过：hard 版把本脚本自己那版未提交的修改直接吃掉了，表现是「改完跑一次
# 自证，改动消失」，人只会以为是自己没保存。
ORIG_HEAD_SHA="$(git rev-parse HEAD)"
TOUCHED_PATHS=(web/css/style.css web/assets.manifest.json)

restore() {
  # HEAD 先回到原始提交（临时提交只含上面两个文件，按 mixed 撤回不会动工作区）。
  if [ "$(git rev-parse HEAD 2>/dev/null)" != "$ORIG_HEAD_SHA" ]; then
    git reset --mixed "$ORIG_HEAD_SHA" >/dev/null 2>&1 || true
  fi
  cp "$BAK/$(basename $TEST)" "$TEST"
  cp "$BAK/$(basename $GEN)" "$GEN"
  cp "$BAK/manifest.json" web/assets.manifest.json
  for h in "${HTMLS[@]}"; do cp "$BAK/$(basename $h)" "$h"; done
  cp "$BAK/style.css" web/css/style.css
  # ⚠️ 这里**不能**再补一句 `git checkout -- $p`：它是按索引/HEAD 覆盖工作区，
  # 会把「跑自证之前工作区里本来就有的未提交改动」一并抹掉，而这些改动跟本次注入
  # 毫无关系。实测踩过：先在台上改了 js/site.js 之类未提交改动 + 重新生成 manifest，
  # 再跑一次自证 → manifest 被 HEAD 版覆盖，守卫当场报 3 条红
  # （HTML=20260914D manifest=20260913D、哈希漂移、git 比对），看起来像真故障，
  # 其实是自证脚本吃掉了自己的工作成果 —— 又一次假红。
  # BAK 快照 = 真正的「运行前状态」，恢复它就够了。
  for p in "${TOUCHED_PATHS[@]}"; do
    [ -f "$BAK/$(basename "$p")" ] && cp "$BAK/$(basename "$p")" "$p"
  done
}
trap restore EXIT

run_test() { node "$TEST" 2>&1; }

fails=0
reds=0
greens=0

check_expect_red() { # $1 描述  $2 期望出现的失败子串
  local out; out="$(run_test)"
  local rc=$?
  if grep -qF "$2" <<<"$out"; then
    echo "✓ [$1] → 「$2」变红"
    reds=$((reds+1))
  elif [ $rc -eq 0 ]; then
    echo "✗ [$1] 注入后仍然全绿 —— 断言是假的"
    fails=$((fails+1))
  else
    echo "✗ [$1] 红了，但红的不是预期那条（期望含「$2」）"
    grep -m3 'FAIL' <<<"$out" | sed 's/^/      /'
    fails=$((fails+1))
  fi
  restore
}

echo "基线：$(run_test | tail -1)"
echo
echo "注入自证（每条都必须变红）"

# 1) 内容变了、?v= 一个字没动，且按文档重新生成过 manifest。
#    这正是前三道断言的盲区：全绿，老浏览器继续吃旧 CSS。
#
#    ⚠️ 必须自己造出「这次提交里内容变了、而 ?v= 没变」这个 git 事实，而且得要**两个**
#    提交才造得出来。第四道断言的判据是 (HEAD~1 的文件内容 ≠ 工作区) && (HEAD~1 的 v == 工作区 v)：
#      · 只改工作区提交一次（老写法）：HEAD~1 是上一个真实提交。若那个提交本身也动过
#        style.css 并且 bump 过 v（本轮就是：改 composer 布局 + bump 20260914B→F），
#        那么“v 一样”这条前提当场不成立 → 注入假绿，自证脚本自己变成了假绿标本。
#      · 先提交一个基线快照 A（内容与 v 都等于当前工作区），再注入并提交 B：
#        HEAD~1 = A → A 的 style.css == 注入前的当前内容、A 的 v == 当前 v，
#        于是「内容变了」成立、「v 没变」成立 → 真正命中第四道断言。
#      一句话：注入必须自己把 HEAD~1 归一成「和工作区同款 ?v 的旧内容」，
#      否则同一条注入换个历史位置就失效 —— 那不叫自证。
git add "${TOUCHED_PATHS[@]}"
git -c user.email=mutation@local -c user.name=mutation \
  commit -qm "mutation: 基线快照（注入前的 style.css + manifest）"
printf '\n/* mutation: 内容变了但 ?v= 没动 */\n' >> web/css/style.css
node "$GEN" >/dev/null                       # 真实作者也会照文档重新生成一次
# 只 stage 这两个文件：临时提交越小，回滚越不可能误伤别的改动。
git add "${TOUCHED_PATHS[@]}"
git -c user.email=mutation@local -c user.name=mutation \
  commit -qm "mutation: style.css 内容变了，?v= 没 bump"
check_expect_red '内容变了却没 bump ?v=（git 第四道）' '内容与上一提交不同，但 ?v= 还是'

# 2) 只 bump 了一个 HTML 里的 v（另一处漏了）。
#    ⚠️ 别把版本号写成字面量（原版写死 'style.css?v=20260913B'）：style.css 一被
#    正常 bump，replace 就命中 0 次 = 注入变成空操作，而下面 check_expect_red
#    会把「注入后仍然全绿」印成假红（2026-09-26 差点踩到）。从 HTML 里现读。
python3 - <<'PY'
import re, sys
p = 'web/index.html'
s = open(p, encoding='utf-8').read()
m = re.search(r'style\.css\?v=([0-9A-Za-z]+)', s)
if not m:
    sys.exit('注入失败：web/index.html 里找不到 style.css 的 ?v= —— 守卫的锚点变了，请同步本脚本')
old, new = m.group(0), 'style.css?v=' + m.group(1)[:-1] + 'Z'
assert old != new, f'注入失败：版本号末位已经是 Z（{old}），换一个改法'
open(p, 'w', encoding='utf-8').write(s.replace(old, new, 1))
PY
check_expect_red '只改了一个 HTML 的 ?v=' '与另一处一致'

# 3) 改了文件但没重新生成 manifest（哈希漂移）。
printf '\n/* mutation */\n' >> web/css/style.css
check_expect_red '改了文件没重新生成 manifest（哈希漂移）' '内容未被改动'

# 4) 新资源加进 HTML 却没登记 manifest。
python3 - <<'PY'
p='web/index.html'; s=open(p).read()
s=s.replace('<script src="/assets/js/chat.js', '<script src="/assets/js/brand-new.js?v=20260913A"></script>\n<script src="/assets/js/chat.js')
open(p,'w').write(s)
PY
check_expect_red '新资源没登记 manifest' '已登记进 manifest'

echo
if [ "$(run_test | tail -1)" = "全部通过" ]; then
  echo "还原：全绿 ✓"; greens=1
else
  echo "还原后仍红 —— 自证脚本自己没收拾干净"; fails=$((fails+1))
fi

echo
if [ $fails -eq 0 ] && [ $reds -ge 4 ] && [ $greens -eq 1 ]; then
  echo "全部注入都被抓住，断言可信"
  exit 0
fi
echo "$fails 条注入没被抓住 —— 断言需要收紧"
exit 1
