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
#    ⚠️ 必须自己造出「这次提交里内容变了」这个 git 事实（临时提交），不能只改工作区：
#    第四道断言是拿 HEAD~1 和**工作区**比，而 CI 上 HEAD~1 往往已经包含上一次
#    那波资源改动 —— 有一版 CI 里这条注入直接变绿（HEAD~1 的 style.css 已经是新
#    内容，比较结果「没变」，断言当然不红）。同一份注入换个历史位置就失效，
#    那就不叫自证。临时提交后 HEAD~1 = 刚推上去的提交，注入才成为真实的「相对
#    上一提交的变化」。
printf '\n/* mutation: 内容变了但 ?v= 没动 */\n' >> web/css/style.css
node "$GEN" >/dev/null                       # 真实作者也会照文档重新生成一次
# 只 stage 这两个文件：临时提交越小，回滚越不可能误伤别的改动。
git add "${TOUCHED_PATHS[@]}"
git -c user.email=mutation@local -c user.name=mutation \
  commit -qm "mutation: style.css 内容变了，?v= 没 bump"
check_expect_red '内容变了却没 bump ?v=（git 第四道）' '内容与上一提交不同，但 ?v= 还是'

# 2) 只 bump 了一个 HTML 里的 v（另一处漏了）。
python3 - <<'PY'
p='web/index.html'; s=open(p).read()
s=s.replace('style.css?v=20260913B','style.css?v=20260913Z')
open(p,'w').write(s)
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
