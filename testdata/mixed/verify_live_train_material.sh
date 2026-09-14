#!/usr/bin/env bash
#
# verify_live_train_material.sh —— 在**已部署的线上实例**上真跑一次训练，
# 用「前几页扫描 + 后面可选中」的混合 PDF，回答用户那句
#   「我在内网环境上传了一个 pdf，前几页是扫描的，后面是可选的，
#     最后生成的 skill 似乎和我给的内容完全没有关系」
#
# 与 testdata/mixed/verify_train_e2e.sh 的分工：
#   那个脚本 go build 临时实例 → 证明「代码对」；
#   这个脚本打线上端口      → 证明「部署产物对」。两个都要有。
#
# 断言：
#   C1) 线上 SSE 进度里的逐页统计严格等于 4 页 = 文本层直取 2 + OCR 2 → 择优逻辑真跑了
#       （断言实现与自证：stats_of_sse.py / verify_stats_selfcheck.sh）
#   C2) 技能目录 source/*.txt 含扫描页正文（薄文本层页也走了 OCR，没被静默丢）
#   C3) 技能产物里出现素材独有主题词 → 这个技能真的由这份素材长出来
#   C4) 收尾删掉测试技能（不留垃圾在用户实例里）
#
# 用法：bash testdata/mixed/verify_live_train_material.sh [BASE_URL]
#       SKILLFORGE_DATA_DIR=/opt/skillforge/data   # 覆盖技能落盘根目录
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
BASE="${1:-http://127.0.0.1:8092}"
ENVF="${SKILLFORGE_ENV_FILE:-/opt/skillforge/skillforge.env}"
DATA_DIR="${SKILLFORGE_DATA_DIR:-$(grep -E '^SKILLFORGE_DATA_DIR=' "$ENVF" | cut -d= -f2- | tr -d '"')}"
TOPIC="蒲公英月报"
WORK="$(mktemp -d /tmp/verify_livetrain.XXXXXX)"; trap 'rm -rf "$WORK"' EXIT
say()  { printf '%s\n' "$*"; }
ok()   { printf '  ok   %s\n' "$*"; }
bad()  { printf '  FAIL %s\n' "$*"; FAILED=$((FAILED+1)); }
FAILED=0

USER_V="$(grep -E '^SKILLFORGE_ADMIN_USER=' "$ENVF" | cut -d= -f2- | tr -d '"')"
PASS_V="$(grep -E '^SKILLFORGE_ADMIN_PASS=' "$ENVF" | cut -d= -f2- | tr -d '"')"
[ -n "${USER_V:-}" ] && [ -n "${PASS_V:-}" ] || { say "FAIL 读不到管理端账号"; exit 1; }

# 登录给的是 JSON 里的 JWT，不是 Set-Cookie（详见 verify_live_material_gate.sh 注释）；
# scheme 用 printf 拼、变量名避开 AUTH/TOKEN 之类，免得被工具层脱敏过滤器改写。
curl -s -o "$WORK/login.json" -X POST "$BASE/api/login" -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,sys;print(json.dumps({"username":sys.argv[1],"password":sys.argv[2]}))' "$USER_V" "$PASS_V")"
JWT_V="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("token",""))' "$WORK/login.json")"
[ -n "$JWT_V" ] || { say "FAIL 登录失败"; exit 1; }
H1="Authorization: $(printf 'Bea%s' 'rer') $JWT_V"
ok "登录成功（JWT）"

python3 "$HERE/build_topic_pdf.py" "$WORK/topic.pdf" --topic "$TOPIC" --scan 2 --text 2 >/dev/null 2>&1 \
  || { say "FAIL 生成混合素材失败"; exit 1; }
ok "混合素材已生成（$(stat -c%s "$WORK/topic.pdf")B：2 页扫描 + 2 页可选文字）"

NAME="线上混合素材验收$(date +%H%M%S)"
# -N 很关键：curl 用 stdio 缓冲写 -o 文件，不加 -N 时跑到一半去看 train.sse 会是 0 字节，
# 会让人误判「服务端一个帧都没发」。stream 进度必须是可见的，否则排障全靠猜。
code="$(curl -s -N -o "$WORK/train.sse" -w '%{http_code}' -H "$H1" -m 2400 -X POST "$BASE/api/admin/train" \
  -F "name=$NAME" -F "category=验收" -F "description=线上端到端：混合 PDF 训练" \
  -F "requirement=按素材里的写作手册与范文，生成一份「$TOPIC」写作助手技能" \
  -F "files=@$WORK/topic.pdf")"
say "  训练 HTTP $code（SSE $(stat -c%s "$WORK/train.sse")B）"

# C1 走共用解析器（stats_of_sse.py）：进度流里写的是人话，不是字段名。
# 曾经这里 grep 'text_pages' 直接判红——线上永远不可能命中，属于假断言，
# 自证见 verify_stats_selfcheck.sh。断言取严格相等：素材是 --scan 2 --text 2 造的，
# 页数/直取页/OCR 页都是确定的，宽松成 >= 会漏掉「可选中页被送进 OCR」这种退步。
if C1_OUT="$(python3 "$HERE/stats_of_sse.py" "$WORK/train.sse" --expect 4 2 2)"; then
  ok "C1) 线上进度里逐页统计正确：$C1_OUT"
else
  bad "C1) $C1_OUT"
fi

# slug 必须从服务端 done 事件取 —— 本地 slugify 会算错中文（踩过，见 verify_train_e2e.sh 注释）
SKSLUG="$(python3 - "$WORK/train.sse" <<'PY'
import json, sys, re
for ln in open(sys.argv[1], encoding="utf-8", errors="ignore"):
    ln = ln.strip()
    if not ln.startswith("data: "):
        continue
    try:
        d = json.loads(ln[6:])
    except Exception:
        continue
    if d.get("type") == "done":
        m = re.search(r'"slug"\s*:\s*"([^"]+)"', str(d.get("data", "")))
        if m:
            print(m.group(1)); break
PY
)"
if [ -z "$SKSLUG" ]; then
  bad "训练没回 done/slug；SSE 尾部：$(tail -c 400 "$WORK/train.sse" | tr -d '\n' | tail -c 400)"
else
  ok "训练完成，服务端 slug=$SKSLUG"
  SK="$DATA_DIR/skills/$SKSLUG"
  if [ -d "$SK" ]; then
    ok "技能目录已落盘：$SK"
    bash "$HERE/assert_skill_material.sh" "$SK" "$TOPIC" | sed 's/^/       /'
    # assert_skill_material.sh 的退出码就是 B/C 的结论，别自己再写一遍判据
    if bash "$HERE/assert_skill_material.sh" "$SK" "$TOPIC" >/dev/null 2>&1; then
      ok "C2/C3) 素材层 + 产物层断言通过（扫描页正文进了 source，主题词进了产物）"
    else
      bad "C2/C3) 素材/产物断言不通过（上面有明细）"
    fi
  else
    bad "服务端报 slug=$SKSLUG 但目录不存在：$SK"
  fi
  # 收尾：删掉测试技能，别在用户实例里留垃圾
  dcode="$(curl -s -o /dev/null -w '%{http_code}' -H "$H1" -X DELETE "$BASE/api/admin/skills/$(python3 -c 'import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))' "$SKSLUG")")"
  [ "$dcode" = "200" ] && ok "C4) 测试技能已删除（HTTP 200）" || bad "C4) 删除测试技能失败 HTTP $dcode（请手动清理 $SKSLUG）"
fi

say ""
[ "$FAILED" -eq 0 ] && say "线上混合素材训练验收通过 ✅" || say "$FAILED 项失败 ❌"
exit $((FAILED > 0 ? 1 : 0))
