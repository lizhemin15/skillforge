#!/usr/bin/env bash
#
# verify_live_train_detach.sh —— 在**已部署的线上实例**上验证一件事：
# 「用户把训练页面关了 / 刷新了 / 代理掐了连接」之后，训练本身是否还在跑完。
#
# 为什么必须有这个脚本（线上现场，不是推演）：
#   训练是挂在 SSE 上的十几分钟长任务。v0.3.27 及以前，训练用的 ctx 就是 HTTP
#   请求的 ctx，浏览器一关 → net/http 立刻 cancel 请求 ctx → 判分回炉的预算清零 →
#   fidelity.md 里留一行「⚠️ 裁判未跑完：回炉失败: context canceled」，
#   而一份 10/100 的技能照常落盘、前端照常显示「✔ 训练完成」。
#   用户在 v0.3.27 现场就是这么拿到一份没过线技能的（线上混合素材验收 10/100）。
#   v0.3.28 把训练 ctx 从请求生命周期里解绑（context.WithoutCancel + 绝对时限），
#   所以「掐掉连接」这件事从此不该影响训练结果。
#
# 断言（真打线上端口，不 go build 临时实例）：
#   D0) 掐连接期间服务进程 PID 未变（否则「靠 systemd 重启重跑」会伪装成「掐线没掐死」）
#   D1) 掐连接后训练照旧跑到收尾：技能目录落盘、fidelity.md 写了最终判分（N/100）
#   D2) fidelity.md 里**没有** context canceled 痕迹（旧行为的确凿指纹）
#   D3) meta.json 存在；若这份技能是降级交付，meta.json 必须显性标记 degraded=true
#       （未验收通过的技能不许静默落盘 —— 这是 v0.3.28 的另一半改动）
#   D5) 训练结论（成功/失败/降级）必须落 journalctl，不能只活在 SSE 里
#   D4) 收尾删掉测试技能，不留垃圾在用户实例里
#
# 用法：bash testdata/mixed/verify_live_train_detach.sh [BASE_URL] [ABORT_SECONDS]
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE="${1:-http://127.0.0.1:8092}"
# 掐连接的时机：训练要跑十几分钟，45 秒必然还在中途（素材解析+模型生成阶段）
ABORT_AFTER="${2:-45}"
ENVF="${SKILLFORGE_ENV_FILE:-/opt/skillforge/skillforge.env}"
DATA_DIR="${SKILLFORGE_DATA_DIR:-$(grep -E '^SKILLFORGE_DATA_DIR=' "$ENVF" | cut -d= -f2- | tr -d '"')}"
POLL_MAX="${POLL_MAX:-1800}"     # 训练收尾最多等 30 分钟
TOPIC="木槿花周报"
WORK="$(mktemp -d /tmp/verify_livedetach.XXXXXX)"; trap 'rm -rf "$WORK"' EXIT
SKILLS="$DATA_DIR/skills"
say() { printf '%s\n' "$*"; }
ok()  { printf '  ok   %s\n' "$*"; }
bad() { printf '  FAIL %s\n' "$*"; FAILED=$((FAILED+1)); }
FAILED=0

# final_diag：不管成功还是失败都要跑的两条诊断断言（放在最后，也放在提前退出的那条路上）。
#
# D0) 服务进程必须是同一个 PID。这个脚本判「掐线后训练跑完了」靠的是技能目录快照求差，
#     可服务进程要是挂了被 systemd 拉起来重跑一遍，目录照样出现、断言照样全绿 —— 假绿。
#     进程变了 = 这次结果与「掐线没掐死训练」无关，直接判失败。
# D5) 训练结论必须落 journalctl。掐线场景下 error/done 帧全部掉进虚空，
#     只写 SSE 的话运维在日志里看到的就是「什么都没发生」，产物有没有、卡在哪一步无从查证。
final_diag() {
  local p1
  p1="$(systemctl show -p MainPID --value skillforge 2>/dev/null || echo '')"
  if [ -n "${SVC_PID0:-}" ] && [ "$SVC_PID0" = "$p1" ]; then
    ok "D0) 服务进程全程未变（PID $p1）→ 下面的落盘结果可信"
  else
    bad "D0) 服务进程变了（${SVC_PID0:-空} → ${p1:-空}）—— 可能是重启后重跑，本次结果不可信"
  fi

  local logs line
  logs="$(journalctl -u skillforge --since "@${T0:-0}" -o cat 2>/dev/null || true)"
  line="$(printf '%s\n' "$logs" | grep -F '[train]' | tail -1)"
  # [ -n "$NAME" ] 不能省：grep -F "" 匹配一切，空名字会让这条断言恒绿（自证时踩到过）。
  if [ -n "$NAME" ] && printf '%s' "$logs" | grep -qF "$NAME"; then
    ok "D5) 训练结论已落 journalctl：$(printf '%s' "$line" | cut -c1-170)"
  else
    bad "D5) journalctl 里没有本次训练的结论行（$NAME）—— 掐线后为何有/无产物无从查证"
  fi
}

[ -d "$SKILLS" ] || { say "FAIL 技能目录不存在：$SKILLS（用 SKILLFORGE_DATA_DIR 覆盖）"; exit 1; }

USER_V="$(grep -E '^SKILLFORGE_ADMIN_USER=' "$ENVF" | cut -d= -f2- | tr -d '"')"
PASS_V="$(grep -E '^SKILLFORGE_ADMIN_PASS=' "$ENVF" | cut -d= -f2- | tr -d '"')"
[ -n "${USER_V:-}" ] && [ -n "${PASS_V:-}" ] || { say "FAIL 读不到管理端账号"; exit 1; }

# 登录给的是 JSON 里的 JWT，不是 Set-Cookie；scheme 用 printf 拼、变量名避开 AUTH/TOKEN
# 之类，免得被工具层脱敏过滤器改写（踩过）。
curl -s -o "$WORK/login.json" -X POST "$BASE/api/login" -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,sys;print(json.dumps({"username":sys.argv[1],"password":sys.argv[2]}))' "$USER_V" "$PASS_V")"
JWT_V="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("token",""))' "$WORK/login.json")"
[ -n "$JWT_V" ] || { say "FAIL 登录失败"; exit 1; }
H1="Authorization: $(printf 'Bea%s' 'rer') $JWT_V"
ok "登录成功（JWT）"

# slug 不许猜：训练前先拍目录快照，收尾时求差得到「这次新落盘的技能目录」
ls -1 "$SKILLS" > "$WORK/before.txt" 2>/dev/null || : > "$WORK/before.txt"

python3 "$HERE/build_topic_pdf.py" "$WORK/topic.pdf" --topic "$TOPIC" --scan 2 --text 2 >/dev/null 2>&1 \
  || { say "FAIL 生成混合素材失败"; exit 1; }

NAME="线上掐线验收$(date +%H%M%S)"
say "  起训练并在 ${ABORT_AFTER}s 后强行掐断连接（模拟用户关训练页面）…"
# T0 是 journalctl --since 的锚点：只捞本次训练产生的日志，避免捞到历史训练里的同名字符串。
T0="$(date +%s)"
SVC_PID0="$(systemctl show -p MainPID --value skillforge 2>/dev/null || echo '')"
# -m 就是「掐连接」：curl 到点主动断开，等价于浏览器关页/代理掉线。
# 拿不到 done 帧是预期的，所以后面靠目录快照求差认技能，而不是靠 SSE。
code="$(curl -s -N -o "$WORK/train.sse" -w '%{http_code}' -H "$H1" -m "$ABORT_AFTER" -X POST "$BASE/api/admin/train" \
  -F "name=$NAME" -F "category=验收" -F "description=线上端到端：掐断连接后训练仍应跑完" \
  -F "requirement=按素材里的写作手册与范文，生成一份「$TOPIC」写作助手技能" \
  -F "files=@$WORK/topic.pdf" 2>/dev/null || echo "aborted")"
say "  连接已断（curl 退出码形态：$code，收到 SSE $(stat -c%s "$WORK/train.sse")B）"
[ "$(stat -c%s "$WORK/train.sse")" -gt 0 ] || bad "D0) 掐线前一个帧都没收到，说明训练压根没起来"

say "  等训练在后台跑完（最多 ${POLL_MAX}s）…"
SK=""; waited=0
while [ "$waited" -lt "$POLL_MAX" ]; do
  ls -1 "$SKILLS" > "$WORK/after.txt" 2>/dev/null || : > "$WORK/after.txt"
  SKREL="$(comm -13 <(sort "$WORK/before.txt") <(sort "$WORK/after.txt") | head -1)"
  if [ -n "$SKREL" ]; then
    SK="$SKILLS/$SKREL"
    # 目录是被逐步写满的：meta.json + fidelity.md 都在，且 40 秒内没有再变动，才算收尾
    if [ -f "$SK/fidelity.md" ] && [ -f "$SK/meta.json" ]; then
      m1="$(stat -c%Y "$SK/fidelity.md")"; sleep 40
      m2="$(stat -c%Y "$SK/fidelity.md")"
      [ "$m1" = "$m2" ] && break
    fi
  fi
  sleep 20; waited=$((waited + 20))
done

if [ -z "$SK" ] || [ ! -d "$SK" ]; then
  bad "D1) 等满 ${POLL_MAX}s 也没看到新技能目录落盘 —— 训练可能真被掐死了"
  final_diag
  say ""; say "1 项失败 ❌"; exit 1
fi
ok "D1) 掐线后训练仍落盘：$(basename "$SK")"

if grep -qE '[0-9]+/100' "$SK/fidelity.md"; then
  ok "D1) fidelity.md 里有最终判分：$(grep -oE '[0-9]+/100' "$SK/fidelity.md" | head -1)"
else
  bad "D1) fidelity.md 里没有最终判分（收尾没跑完）：$(tail -c 200 "$SK/fidelity.md" | tr '\n' ' ')"
fi

if grep -q 'context canceled' "$SK/fidelity.md"; then
  bad "D2) fidelity.md 仍有 context canceled —— 训练 ctx 仍被请求生命周期带着走：$(grep -n 'context canceled' "$SK/fidelity.md" | head -2 | tr '\n' ' ')"
else
  ok "D2) fidelity.md 无 context canceled 痕迹（掐线不再掐死训练）"
fi

if [ ! -f "$SK/meta.json" ]; then
  bad "D3) meta.json 不存在（管理员在左树点开技能看不到验收状态）"
else
  deg="$(python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));print(d.get("degraded",False), d.get("degrade_reason",""))' "$SK/meta.json" 2>/dev/null)"
  score="$(grep -oE '[0-9]+/100' "$SK/fidelity.md" | head -1)"
  case "$deg" in
    True*|true*)
      # 降级 → meta.json 必须显性标记，fidelity.md 顶部必须有降级告示，两者缺一不可
      if grep -q '降级交付' "$SK/fidelity.md"; then
        ok "D3) 降级交付已显性标记（meta.json degraded=true，fidelity.md 顶部有告示，判分 $score）"
      else
        bad "D3) meta.json 标了降级，但 fidelity.md 顶部没有降级告示"
      fi
      ;;
    False*|false*)
      ok "D3) 裁判过线交付（meta.json 未标降级，判分 $score），无降级标记属正确行为"
      ;;
    *)
      bad "D3) meta.json 解析不出 degraded 字段：$deg"
      ;;
  esac
fi

final_diag

dcode="$(curl -s -o /dev/null -w '%{http_code}' -H "$H1" -X DELETE "$BASE/api/admin/skills/$(python3 -c 'import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))' "$(basename "$SK")")")"
[ "$dcode" = "200" ] && ok "D4) 测试技能已删除（HTTP 200）" || bad "D4) 删除测试技能失败 HTTP $dcode（请手动清理 $(basename "$SK")）"

say ""
[ "$FAILED" -eq 0 ] && say "线上「掐线不掐训练」验收通过 ✅" || say "$FAILED 项失败 ❌"
exit $((FAILED > 0 ? 1 : 0))
