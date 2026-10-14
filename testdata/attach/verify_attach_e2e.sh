#!/bin/bash
# SkillForge 内嵌附件下发 E2E —— 真 API 全流程（不起 mock，不碰生产实例）
#
# 用法：
#   WORK=/tmp/sf-attach-e2e BASE=http://localhost:18123 bash testdata/attach/verify_attach_e2e.sh
#
# 前置（这个脚本只管「验」，不管「起」）：
#   1) 有 18123 上跑着的**本次构建**的实例，data 是生产 data 的副本：
#        cp -a /opt/skillforge/data $WORK/data-dev
#        set -a; . /opt/skillforge/skillforge.env; set +a
#        SKILLFORGE_DATA_DIR=$WORK/data-dev SKILLFORGE_DB=$WORK/data-dev/skillforge.db \
#          SKILLFORGE_ADDR=:18123 exec ./skillforge   # 用 exec，别套 shell 包装
#      端口先探空再起（本机端口很挤，占用会让新实例**静默退出**）：
#        ss -ltn | grep :18123 && echo "换个端口"
#   2) $WORK/token.txt 里是从该实例登录取回的 token
#
# 为什么绕这么一圈：附件下发的真路径是「上传带内嵌附件的文档 → 训练 → 用户点名 → 落盘字节下发」，
# 任何一段用 mock 替掉都会漏掉真正会坏的地方（字节、文件名、白名单、404 边界）。
set -u
WORK=${WORK:-/tmp/sf-attach-e2e}
BASE=${BASE:-http://localhost:18123}
SLUG=e2e-train-notice
cd "$WORK"
PASS=0; FAIL=0
ok()   { echo "  ok   $1"; PASS=$((PASS+1)); }
bad()  { echo "  FAIL $1"; FAIL=$((FAIL+1)); }
chk()  { if [ "$2" = "$3" ]; then ok "$1 ($2)"; else bad "$1 — 期望 $3，实际 $2"; fi; }

# 令牌绝不落在命令行上（命令行进日志/历史，且本机的脱敏过滤器会改写显示值）。
# 让 python 从文件拼一个 curl 能读的 header 文件，shell 里始终只有路径。
# 路径用环境变量传进去 —— 写成 <<'PY' 的 heredoc 不做变量展开，直接写 $WORK 会变成字面量。
SKILLFORGE_E2E_WORK="$WORK" python3 - <<'PY'
import os
work = os.environ['SKILLFORGE_E2E_WORK']
tok = open(os.path.join(work, 'token.txt')).read().strip()
hdr = 'Authoriz' + 'ation: Be' + 'arer ' + tok
open(os.path.join(work, 'hdr.txt'), 'w').write(hdr + '\n')
print('  header 文件已生成，token 长度', len(tok))
PY

echo "== 1. 管理端文件树：拆出来的附件必须可见（kind=attach）=="
curl -s -H @$WORK/hdr.txt "$BASE/api/admin/skills/$SLUG/files" -o files.json -w '' 
SKILLFORGE_E2E_WORK="$WORK" python3 - <<'PY' | tee "$WORK/sec1.out"
import json, sys, os
W = os.environ['SKILLFORGE_E2E_WORK']
d = json.load(open(os.path.join(W, 'files.json')))
files = d.get('files') or d if isinstance(d, list) else d.get('files', [])
assert isinstance(files, list), f'意外的响应形状: {str(d)[:200]}'
paths = {f['path']: f for f in files}
import sys
def out(cond, msg):
    print(('  ok   ' if cond else '  FAIL ') + msg)
    return cond
res = []
res.append(out(any(p.startswith('attachments/') for p in paths),
               '文件树里出现 attachments/ 下的附件：' + str([p for p in paths if p.startswith('attachments/')])))
for f in files:
    if f['path'].startswith('attachments/'):
        res.append(out(f.get('kind') == 'attach', f"{f['path']} kind=attach（实际 {f.get('kind')}）"))
        res.append(out(f.get('editable') is False, f"{f['path']} 不可文本编辑（docx 不该给编辑入口）"))
kinds = [f.get('kind') for f in files]
i_tpl = kinds.index('templatefile') if 'templatefile' in kinds else -1
i_att = kinds.index('attach') if 'attach' in kinds else -1
i_req = kinds.index('requirement') if 'requirement' in kinds else -1
# 只在参照物**真实存在**时才下顺序结论：本技能没有 templatefile，
# 拿 -1 去比「-1 < 1」永远为真 —— 那条断言看着过了，其实什么都没验。
# （Go 单测里用带全部参照物的固定素材把顺序钉死了，这里就诚实标 n/a。）
if i_tpl >= 0 and i_req >= 0:
    res.append(out(i_tpl < i_att < i_req, f'顺序 templatefile@{i_tpl} < attach@{i_att} < requirement@{i_req}'))
else:
    print(f'  n/a  顺序断言跳过（本技能 templatefile@{i_tpl}，参照物不全；顺序由 store 单测覆盖）')
    res.append(True)
print(f'  —— 文件树 {len(files)} 项：' + ', '.join(f"{f['path']}({f.get('kind')})" for f in files))
sys.exit(0 if all(res) else 1)
PY
# 计数交给末尾的 tally()（它还会抓「零断言=没跑起来」），这里不再自己加。

echo "== 2. 磁盘上的附件字节 = 源文档里内嵌的原始字节（md5 逐字节比对）=="
INNER1=$(md5sum inner1.docx | cut -d' ' -f1)
INNER2=$(md5sum inner2.docx | cut -d' ' -f1)
python3 - <<PY | tee "$WORK/sec2.out"
import hashlib, glob, os
exp = {'$INNER1': 'inner1.docx', '$INNER2': 'inner2.docx'}
got = {}
for p in glob.glob('$WORK/data-dev/skills/$SLUG/attachments/*'):
    got[hashlib.md5(open(p,'rb').read()).hexdigest()] = os.path.basename(p)
hit = [exp[h] for h in exp if h in got]
print('  落盘附件：', got)
print(('  ok   ' if len(hit) == 2 else '  FAIL ') + f'两份内嵌附件字节与源文件内嵌完全一致（命中 {hit}）')
PY

echo "== 3. 下载端点：白名单 + 越界防护 =="
# 从 roster 里取一个真实附件名（不猜名字）
NAME=$(python3 -c "
import json
d=json.load(open('$WORK/files.json'))
print([f['name'] for f in d['files'] if f['path'].startswith('attachments/')][0])")
echo "  用真实附件名：$NAME"
curl -s -o dl.bin -w '%{http_code}' "$BASE/api/chat/attachment/$SLUG?file=$(python3 -c "import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))" "$NAME")" > code.txt
chk "下载真实附件返回 200" "$(cat code.txt)" "200"
DLMD5=$(md5sum dl.bin | cut -d' ' -f1)
if [ "$DLMD5" = "$INNER1" ] || [ "$DLMD5" = "$INNER2" ]; then ok "下载到的字节 = 源文档内嵌字节 ($DLMD5)"; else bad "下载字节不匹配：$DLMD5"; fi
# 越界：目录穿越
curl -s -o /dev/null -w '%{http_code}' "$BASE/api/chat/attachment/$SLUG?file=..%2F..%2Fskillforge.db" > code2.txt
if [ "$(cat code2.txt)" != "200" ]; then ok "目录穿越被拒（HTTP $(cat code2.txt)）"; else bad "目录穿越竟然 200 —— 严重安全问题"; fi

# 技能不存在：必须是 404。这条是补线上的洞 —— 曾经这里返 500，
# 因为 store 把 sql.ErrNoRows 转成了 ErrSkillNotFound，处理器却拿 == sql.ErrNoRows 去比。
# 500 会被运营/用户读成「服务挂了」，实际只是 slug 不存在；而且 500 不会触发前端
# 的「没这个文件」提示。线上复现过，所以钉在这里。
code_ns=$(curl -s -o body_ns.txt -w '%{http_code}' "$BASE/api/chat/attachment/meiyouzhege?file=x.docx")
chk "不存在的技能是 404（不是 500）" "$code_ns" "404"
# 越界：清单里没有的名字
curl -s -o /dev/null -w '%{http_code}' "$BASE/api/chat/attachment/$SLUG?file=no-such-file.docx" > code3.txt
if [ "$(cat code3.txt)" != "200" ]; then ok "清单外文件名被拒（HTTP $(cat code3.txt)）"; else bad "清单外文件名返回 200 —— 白名单失效"; fi

echo "== 4. 老技能回归：历史裸文件名附件仍能下发 =="
SKILLFORGE_E2E_WORK="$WORK" python3 - <<'PY' > old.txt
import json, os, sqlite3
db = sqlite3.connect(os.path.join(os.environ['SKILLFORGE_E2E_WORK'], 'data-dev', 'skillforge.db'))
db.row_factory = sqlite3.Row
try:
    rows = db.execute("select slug, attachments from skills where attachments is not null and attachments != '' and slug != ?", ('$SLUG',)).fetchall()
except Exception as e:
    rows = []
for r in rows:
    print(r['slug'], '|', r['attachments'][:200])
PY
if [ -s old.txt ]; then echo "  库里带附件的其它技能："; head -5 old.txt | sed 's/^/    /'; else echo "  （库里没有其它带附件的技能，老技能兼容由单测覆盖）"; fi

echo "== 5. 运行时下发：用户点名的模板必须真发出去（这就是用户要的场景）=="
chat() {
  curl -sS -N -X POST "$BASE/api/chat" -H 'Content-Type: application/json' \
    --data "$(python3 -c "import json,sys;print(json.dumps({'session_id':sys.argv[2],'message':sys.argv[1]},ensure_ascii=False))" "$1" "$2")" \
    --max-time 240 > "$2.log" 2>&1
}
chat "把报名表发给我" chat_name
chat "培训时间是几号？" chat_noname
python3 $WORK/parse_frames.py chat_name.log name | tee sec5.out
python3 $WORK/parse_frames.py chat_noname.log noname | tee sec6.out

# 反假绿闸门：本节一条断言都没输出 = 脚本崩了/接口异常，
# 必须记失败。早先的版本用「grep -c '^  ok'」计数，python 崩掉后
# 计数为 0 却打印「全部通过」—— 报告好看，结论是假的。
tally() {
  okn=$(grep -c '^  ok' "$1" 2>/dev/null); okn=${okn:-0}
  badn=$(grep -c '^  FAIL' "$1" 2>/dev/null); badn=${badn:-0}
  PASS=$((PASS + okn)); FAIL=$((FAIL + badn))
  if [ $((okn + badn)) -eq 0 ]; then
    FAIL=$((FAIL + 1)); echo "  FAIL $2：本节零断言输出（没跑起来，不算通过）"; fi
}
tally sec5.out "运行时下发"
tally sec6.out "不预判反向用例"
tally sec1.out "文件树可见性"
tally sec2.out "落盘字节比对"

echo
echo "PASS=$PASS FAIL=$FAIL"
[ "$FAIL" = "0" ] && echo "全部通过" || echo "有失败项，别当通过"
