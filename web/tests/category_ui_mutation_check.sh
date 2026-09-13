#!/usr/bin/env bash
# 分类结构管理 UI 的「断言自证」脚本。
#
# 为什么必须有这个脚本：tests/skill_category_admin_ui.test.mjs 全绿只能证明
# 「现在没坏」，证明不了「坏了会被抓住」。断言写松了（比如正则永远匹配得上、
# 或拿后端返回的字段自己喂自己）会恒真，事故照样漏过去。
# 做法：往**出货文件** js/admin.js 里逐个注入真实故障，要求对应的那条断言变红；
# 还原后必须重新变绿。哪条注入还是绿的，就说明那条断言是假的，脚本直接退出 1。
#
# 用法：bash web/tests/category_ui_mutation_check.sh
set -uo pipefail
cd "$(dirname "$0")/.."

ADMIN=js/admin.js
TEST=tests/skill_category_admin_ui.test.mjs
BAK="$(mktemp)"
cp "$ADMIN" "$BAK"
# 注意：restore 只负责还原，**不能**在这里删备份 —— 每条注入后都要还原一次，
# 备份删了第二次还原就成了空操作，注入状态会一路带到下一条（自证结果全乱）。
restore() { cp "$BAK" "$ADMIN"; }
trap 'restore; rm -f "$BAK"' EXIT

fails=0

# ---------- 基线：不注入时必须全绿 ----------
if ! out="$(node "$TEST" 2>&1)"; then
  echo "基线就是红的，先修好再来做注入自证："
  echo "$out" | tail -20
  exit 1
fi
echo "基线：全绿 ✓"

# 注入：$1=故障说明 $2=锚点 $3=替换文本 $4=应变红的那条断言关键字
inject_case() {
  local desc="$1" old="$2" new="$3" expect="$4"
  python3 - "$ADMIN" "$old" "$new" <<'PY'
import sys
path, old, new = sys.argv[1], sys.argv[2], sys.argv[3]
s = open(path, encoding='utf-8').read()
n = s.count(old)
if n != 1:
    sys.exit(f"注入失败：锚点在 {path} 里命中 {n} 次（应为 1 次）—— 出货文件改了，"
             f"请同步更新本脚本的锚点，别让自证脚本变成永远绿的摆设")
open(path, 'w', encoding='utf-8').write(s.replace(old, new))
PY
  if [ $? -ne 0 ]; then echo "✗ [$desc] 注入失败"; fails=$((fails + 1)); return; fi

  local out rc
  out="$(node "$TEST" 2>&1)"; rc=$?
  if [ $rc -eq 0 ]; then
    echo "✗ [$desc] 注入后测试仍然全绿 —— 断言是假的（抓不住这个故障）"
    fails=$((fails + 1))
  elif ! grep -qF "$expect" <<<"$out"; then
    echo "✗ [$desc] 测试红了，但红的不是预期那条（期望含「$expect」）"
    echo "$out" | grep '^  FAIL' | sed 's/^/      /'
    fails=$((fails + 1))
  else
    echo "✓ [$desc] → 「$expect」变红"
  fi
  restore
}

echo
echo "注入自证（每条都必须变红）"

inject_case '前端又把新增入口门禁回去（非手册技能看不到按钮）' \
  "const titleOps = (k === 'category')" \
  "const titleOps = (k === 'category' && false)" \
  '非手册技能也有「+ 新增分类」入口'

inject_case '前端靠分类行数猜手册模式（删空后入口消失）' \
  "const titleOps = (k === 'category')" \
  "const titleOps = (k === 'category' && items.length > 0)" \
  '分类被删空后「+ 新增分类」仍在'

inject_case '空的分类组被整组跳过（入口无处可挂）' \
  "category: '暂无分类。分类由训练从手册抽出，各技能不一样；要加手册里没有的，点上面的「+ 新增分类」（categories/_index.md 是分类路由表）'," \
  "category: ''," \
  '空分类组仍然渲染'

inject_case '路由表 _index.md 也发改名/删除入口' \
  "const isCategory = k === 'category' && !!f.category_name;" \
  "const isCategory = k === 'category';" \
  'categories/_index.md（路由表）没有改名/删除入口'

inject_case '改名按钮不带分类名（按钮与行错位）' \
  "window.renameCategoryView('\${escapeJs(f.path)}','\${escapeJs(catName)}')" \
  "window.renameCategoryView('\${escapeJs(f.path)}','')" \
  '改名/删除按钮都带**自己那一行**的分类名'

inject_case '删除忽略 need_force 标记（有范文也只问一次）' \
  "if (!r.ok && j && j.need_force) {" \
  "if (false) {" \
  '有范文时问第二次'

inject_case '第二段确认点了取消仍然带 force 重试' \
  "确定连同这 ' + n + ' 篇一起删除？')) return;" \
  "确定连同这 ' + n + ' 篇一起删除？'));" \
  '第二段确认取消 → 不带 force 重试'

inject_case 'warnings 混进普通回执（不用红字单列）' \
  '<div class="msg err" style="margin-top:10px">${ch.warnings.map(esc).join('"'"'<br>'"'"')}</div>' \
  '<div class="dim">${ch.warnings.map(esc).join('"'"'<br>'"'"')}</div>' \
  'warnings（本该在却没在）用红字单列'

inject_case '回执不列被改写的文件（只说一句已保存）' \
  '<div style="margin-top:10px"><b style="font-size:13px">已改写</b>${list(ch.files_touched)}</div>' \
  '' \
  '回执列出被改写的文件'

echo
if ! out="$(node "$TEST" 2>&1)"; then
  echo "✗ 还原后测试还是红的 —— 注入没被干净还原，admin.js 可能已被改坏"
  echo "$out" | tail -20
  fails=$((fails + 1))
else
  echo "还原：全绿 ✓"
fi

echo
[ "$fails" -eq 0 ] && echo "全部注入都被抓住，断言可信" || echo "$fails 条注入没被抓住 —— 断言需要收紧"
exit $((fails > 0))
