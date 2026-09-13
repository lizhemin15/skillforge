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

restore() {
  cp "$BAK/$(basename $TEST)" "$TEST"
  cp "$BAK/$(basename $GEN)" "$GEN"
  cp "$BAK/manifest.json" web/assets.manifest.json
  for h in "${HTMLS[@]}"; do cp "$BAK/$(basename $h)" "$h"; done
  cp "$BAK/style.css" web/css/style.css
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
sed -i 's|style.css?v=20260913B|style.css?v=20260913A|g' "${HTMLS[@]}"
node "$GEN" >/dev/null
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
