#!/usr/bin/env bash
# 「写作链路步骤板」渲染契约断言（chat_board_contract_test.go）的自证脚本。
#
# 为什么必须有它：那份测试全绿只证明「现在没坏」，证明不了「坏了会被抓住」。步骤板这
# 块地有三类断言最容易写成摆设，而且每一类都在真机上见过：
#   - 「板上有这格」——格子确实有，但它永远停在 active（转不完的圈），用户读到的是卡住；
#   - 「phase 随便写」——后端写个前端不认识的 phase，前端[chat.js:1362] 取不到映射就
#     把 phase 原样印在编号栏上，那格里出现一个英文单词 "plan"、角色徽标空白；
#   - 「对着抄来的表量」——尺子自己抄了一份 phase 表，于是后端发明新 phase 时它永远绿。
# 做法：往**出货文件**（chat.go / web/js/chat.js）注入真实故障，要求对应断言变红；
# 还原后回绿且逐字节一致。
#
# 用法：bash internal/api/chat_board_contract_mutation_check.sh
set -uo pipefail
cd "$(dirname "$0")/../.."
export PATH=/usr/local/go/bin:$PATH

CHAT=internal/api/chat.go
JS=web/js/chat.js
FILES=("$CHAT" "$JS")
BAK_DIR="$(mktemp -d)"
for f in "${FILES[@]}"; do
  cp "$f" "$BAK_DIR/$(basename "$f")"
done
sums() { md5sum "${FILES[@]}" | awk '{print $1}'; }
BEFORE="$(sums)"

# restore 只还原、**不删备份**：每条注入后都要复原一次，备份删了第二次还原就是空操作，
# 注入状态会一路带到下一条（自证结果全乱）。也因此**不用 git checkout** —— 出货文件上
# 可能压着未提交的人工改动，git 还原会把人的劳动一起抹掉。
restore() {
  for f in "${FILES[@]}"; do
    cp "$BAK_DIR/$(basename "$f")" "$f"
  done
}
cleanup() { restore; rm -rf "$BAK_DIR"; }
trap cleanup EXIT

fails=0
run_test() {
  go test ./internal/api/ -run 'TestWriteSkillBoardShowsPlanThenDraft|TestPlainPathBoardShowsPlanThenDraft' -count=1 2>&1
}

# ---------- 基线：不注入时必须全绿 ----------
if ! out="$(run_test)"; then
  echo "基线就是红的，先修好再来做注入自证："
  echo "$out" | tail -20
  exit 1
fi
echo "基线：全绿 ✓"

# 注入：$1=故障说明 $2=目标文件 $3=锚点 $4=替换文本 $5=应变红的断言 $6=锚点应命中次数（默认 1）
inject_case() {
  local desc="$1" file="$2" old="$3" new="$4" expect="$5" want="${6:-1}"
  python3 - "$file" "$old" "$new" "$want" <<'PY'
import sys
path, old, new, want = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
s = open(path, encoding='utf-8').read()
n = s.count(old)
if n != want:
    sys.exit(f"注入失败：锚点在 {path} 里命中 {n} 次（应为 {want} 次）—— 出货文件改了，"
             f"请同步更新本脚本的锚点，别让自证脚本变成永远绿的摆设")
open(path, 'w', encoding='utf-8').write(s.replace(old, new))
PY
  if [ $? -ne 0 ]; then echo "✗ [$desc] 注入失败"; fails=$((fails + 1)); return; fi

  local out rc
  out="$(run_test)"; rc=$?
  if [ $rc -eq 0 ]; then
    echo "✗ [$desc] 注入后测试仍然全绿 —— 断言是假的（抓不住这个故障）"
    fails=$((fails + 1))
  elif grep -q 'build failed\|\[build failed\]\|cannot use\|undefined:' <<<"$out"; then
    # 「红在编译上不算红」：编译不过说明注入本身是坏的，不能算断言有效。
    echo "✗ [$desc] 注入把代码改到编译不过 —— 这次红不算数"
    echo "$out" | grep -m3 '\.go:' | sed 's/^/      /'
    fails=$((fails + 1))
  elif ! grep -qF "$expect" <<<"$out"; then
    echo "✗ [$desc] 测试红了，但红的不是预期那条（期望含「$expect」）"
    echo "$out" | grep '^--- FAIL\|^    --- FAIL' | sed 's/^/      /'
    fails=$((fails + 1))
  else
    echo "✓ [$desc] → 「$expect」变红"
  fi
  restore
}

echo
echo "注入自证（每条都必须变红）"

# 注入 1 = 修复前的原样：Carry 接过 t≈0 的首格之后，又用 Done 追加一格同名步骤。
# 两个调用点（write 型技能那条 / 通用写作那条）都注入：两条路都必须自己红。
inject_case 'Carry 之后又追加同名步骤（板上出现两个 ①，其中一个永远在转）' \
  "$CHAT" 'tb.CloseCarried(analyzeCarriedDetail(manualSkill))' \
  'tb.Done("analyze", "① 意图分析", "已判断出本轮要做什么")' \
  '格同时「进行中」' 2

# 注入 2a = write 型技能那条路退回自造 phase "plan"。
inject_case 'write 技能路自造 phase "plan"（前端认不得，编号栏印出英文单词）' \
  "$CHAT" 'tb.Active("generate", "④ 构思要点"' 'tb.Active("plan", "④ 构思要点"' \
  '在前端 AGENTS_MANUAL 表里没有映射'

# 注入 2b = 通用写作那条路退回自造 phase "plan"。
inject_case '通用写作路自造 phase "plan"（前端认不得，编号栏印出英文单词）' \
  "$CHAT" 'tb.Active("generate", "③ 构思要点"' 'tb.Active("plan", "③ 构思要点"' \
  '在前端 AGENTS 表里没有映射'

# 注入 3 = 前端表里真的少一个 phase（后端还用着它）。
# 这条查的是尺子的方向：它读的必须是**出货的那个文件**，而不是抄来的一份表。
inject_case '前端 AGENTS 表里少掉 generate（尺子若对着抄来的表量，这条抓不住）' \
  "$JS" "    generate: { n: '4', role: '内容执笔', act: '起草生成内容' }," \
  "    generateX: { n: '4', role: '内容执笔', act: '起草生成内容' }," \
  '在前端 AGENTS 表里没有映射'

# 注入 4 = 把表名改掉：尺子必须**响**，而不是解析出空表然后恒绿。
inject_case '前端表名被改（解析器必须报「找不到表」，不许静默变成空尺子）' \
  "$JS" 'const AGENTS = {' 'const AGENTS_RENAMED = {' \
  '没有 AGENTS 表'

echo
if ! out="$(run_test)"; then
  echo "✗ 还原后测试还是红的 —— 注入没被干净还原，出货文件可能已被改坏"
  echo "$out" | tail -20
  fails=$((fails + 1))
elif [ "$(sums)" != "$BEFORE" ]; then
  # 文件内容必须**逐字节**回到注入前（含未提交的人工改动），否则这个脚本本身在改坏仓库。
  echo "✗ 还原后文件内容与注入前不一致 —— 出货文件被这个脚本改动了"
  md5sum "${FILES[@]}" | sed 's/^/      /'
  fails=$((fails + 1))
else
  echo "还原：全绿且逐字节一致 ✓"
fi

echo
[ "$fails" -eq 0 ] && echo "全部注入都被抓住，断言可信" || echo "$fails 条注入没被抓住 —— 断言需要收紧"
exit $((fails > 0))
