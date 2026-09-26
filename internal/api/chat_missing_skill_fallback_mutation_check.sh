#!/usr/bin/env bash
# 「技能加载失败不许整轮报错」三层修复的断言自证脚本（2026-09-26）。
#
# 为什么必须有：三条单测（store / agent / api 各一条）全绿，只证明「今天这三层都拦得住」，
# 证明不了「哪天有人把 ErrSkillNotFound 映射删了 / 把 L2 守卫短路了 / 把停用过滤去掉 /
# 把降级说明改成静默 / 把兜底退回 write(evError, …) 会被抓住」。
#
# 而它翻车的样子是用户直接看得见的：屏幕上只剩一行
#   ⚠ sql: no rows in result set
# 整轮零正文，连一句人话都没有（2026-09-26 线上原样）。
#
# 五条注入分别打在这五种坏法上，每条都必须红在**指定**的那条断言上。
#
# 用法：bash internal/api/chat_missing_skill_fallback_mutation_check.sh
set -uo pipefail
cd "$(dirname "$0")/../.."

# go 走候选解析，不写死本机工具链：写死 /usr/local/go/bin/go 在开发机对（1.25），
# 在 CI 上（go 由 setup-go 提供）会变成一堆莫名其妙的编译错 —— 环境红冒充断言红。
# 但候选**必须按 go.mod 的版本要求筛**：本机 /usr/bin/go 是发行版自带的 1.18，
# 拿它跑 `go test` 只会报 "invalid go version '1.25.0'"（同样是与判据无关的环境红）。
NEED_GO="$(sed -n 's/^go \([0-9]*\)\.\([0-9]*\).*/\12/p' go.mod | head -1)"
go_ok() {
  local bin="$1" v
  [ -n "$bin" ] || return 1
  if [ ! -x "$bin" ] && ! command -v "$bin" >/dev/null 2>&1; then return 1; fi
  v="$("$bin" env GOVERSION 2>/dev/null | sed -n 's/^go\([0-9]*\)\.\([0-9]*\).*/\12/p')"
  [ -n "$v" ] && [ "$v" -ge "${NEED_GO:-0}" ]
}
GO_BIN_ENV="${GO_BIN:-}"
GO_BIN=""
for c in "$GO_BIN_ENV" "$(command -v go || true)" /usr/local/go1.25/bin/go /usr/local/go/bin/go /usr/bin/go /opt/go/bin/go; do
  if go_ok "$c"; then GO_BIN="$c"; break; fi
done
if [ -z "$GO_BIN" ]; then
  echo "环境红：挑不到 go >= $(sed -n 's/^go \(.*\)/\1/p' go.mod | head -1)（不是断言红，先修环境：export GO_BIN=... 或把新版 go 放进 PATH）" >&2
  exit 2
fi

STORE=internal/store/skill_store.go
AGENT=internal/agent/agent.go
CHAT=internal/api/chat.go
BAK_DIR="$(mktemp -d)"
cp "$STORE" "$BAK_DIR/S.go"; cp "$AGENT" "$BAK_DIR/A.go"; cp "$CHAT" "$BAK_DIR/C.go"

# 出货文件的 md5，用来最后证明还原是逐字节的（不是「看起来像」）。
BEFORE="$(md5sum "$STORE" "$AGENT" "$CHAT" | awk '{print $1}' | tr '\n' ' ')"

# restore 只还原、**不删备份**（备份删了第二次还原就是空操作，注入态会一路带到下一条），
# 也**不用 git checkout** —— 出货文件上可能压着未提交的人工改动，git 还原会把它一起抹掉。
restore() {
  cp "$BAK_DIR/S.go" "$STORE"
  cp "$BAK_DIR/A.go" "$AGENT"
  cp "$BAK_DIR/C.go" "$CHAT"
}
cleanup() { restore; rm -rf "$BAK_DIR"; }
trap cleanup EXIT

# 单点替换：锚点必须**恰命中 1 次**。命中 0 次说明出货文件改了、脚本已失效；
# 命中多次说明锚点不够特异，替换会打到没打算改的地方 —— 两种都必须当场报错，
# 不许「替换 0 处照样往下跑」（那会变成永远绿的自证）。
edit_pair() { # $1=file $2=old $3=new
  python3 - "$1" "$2" "$3" <<'PY'
import sys
path, old, new = sys.argv[1], sys.argv[2], sys.argv[3]
s = open(path, encoding='utf-8').read()
n = s.count(old)
if n != 1:
    sys.exit(f"注入失败：锚点在 {path} 里命中 {n} 次（应为 1 次）—— 出货文件改了，"
             f"请同步更新本脚本的锚点，别让自证脚本变成永远绿的摆设")
open(path, 'w', encoding='utf-8').write(s.replace(old, new, 1))
PY
}

run_store() { "$GO_BIN" test ./internal/store/ -run 'TestGetMissingSkillReturnsErrSkillNotFoundNotSQLNoRows' -count=1 2>&1; }
run_agent() { "$GO_BIN" test ./internal/agent/ -run 'TestEvalTurnNeutralizesSkillSlugNotInRoster|TestEvalTurnKeepsSkillSlugPresentInRoster|TestEvalTurnNeutralizesDisabledSkillSlug' -count=1 2>&1; }
run_api()   { "$GO_BIN" test ./internal/api/ -run 'TestChatMissingSkillContentDegradesToPlainWriteNotError' -count=1 2>&1; }

# ---------- 基线：不注入时必须三条全绿 ----------
for pair in "store:run_store" "agent:run_agent" "api:run_api"; do
  name="${pair%%:*}" fn="${pair##*:}"
  if ! out="$("$fn")"; then
    echo "基线就是红的（$name），先修好再来做注入自证："
    echo "$out" | tail -20
    exit 1
  fi
done
echo "基线：store / agent / api 三条全绿 ✓"

fails=0
# 判定一条注入结果：$1=说明 $2=runner $3=期望变红的断言文案片段
check_one() {
  local desc="$1" runner="$2" expect="$3" out rc
  out="$("$runner")"; rc=$?
  if [ $rc -eq 0 ]; then
    echo "✗ [$desc] 注入后测试仍然全绿 —— 断言是假的（抓不住这个故障）"
    fails=$((fails + 1))
  elif grep -q 'build failed\|\[build failed\]\|cannot use\|undefined:\|declared and not used\|defined and not used\|imported and not used' <<<"$out"; then
    # 「红在编译上不算红」：编译不过说明注入本身是坏的，不能算断言有效。
    echo "✗ [$desc] 注入把代码改到编译不过 —— 这次红不算数"
    echo "$out" | grep -m3 '\.go:' | sed 's/^/      /'
    fails=$((fails + 1))
  elif ! grep -qF -- "$expect" <<<"$out"; then
    echo "✗ [$desc] 测试红了，但红的不是预期那条（期望含「$expect」）"
    echo "$out" | grep '^--- FAIL\|^    --- FAIL' | sed 's/^/      /'
    fails=$((fails + 1))
  else
    echo "✓ [$desc] → 「$expect」变红"
  fi
  restore
}

echo
echo "注入自证（每条都必须变红，且红在指定断言上）"

# 注入 1 = 存储层不映射 ErrSkillNotFound，退回原样上抛 sql.ErrNoRows。
# 这是故障链的源头：上层只能用 errors.Is 分辨「技能不存在」，否则「没这个技能」
# 和「数据库炸了」长得一模一样，整条降级路就没法写。
# 注意注入体**保留 `errors.Is` 的引用**（只是把包装换成原样上抛）：直接删掉这段会连带
# 让 "errors" 变成未使用 import → 编译红，那不算断言红（第一次跑就是这么假红的）。
edit_pair "$STORE" \
  $'\tif errors.Is(err, sql.ErrNoRows) {\n\t\treturn nil, fmt.Errorf("%w: %s", ErrSkillNotFound, slug)\n\t}\n' \
  $'\tif errors.Is(err, sql.ErrNoRows) {\n\t\treturn nil, err // （注入：不映射 ErrSkillNotFound，原样上抛 sql.ErrNoRows）\n\t}\n'
check_one '存储层不映射 ErrSkillNotFound（sql 细节原样上抛）' run_store '不是 ErrSkillNotFound'

# 注入 2 = L2 守卫被短路（模型编的名字直接放行进执行链）。
# 线上那一轮就是走了这条：分类器吐 skill_slug=通用能力 → 下游拿它查库。
edit_pair "$AGENT" \
  $'\tif eval.SkillSlug != "" && !rosterHasSlug(skills, eval.SkillSlug) {\n' \
  $'\tif false && eval.SkillSlug != "" && !rosterHasSlug(skills, eval.SkillSlug) {\n'
check_one 'L2 守卫被短路（模型编的技能名放行进执行链）' run_agent '被放行进了执行链'

# 注入 3 = rosterHasSlug 不筛 Enabled（停用技能照样算命中）。
# 清单里印不出停用技能，模型从历史上下文里翻出来的名字不该被放行 ——
# 否则用户以为已经关掉的能力会被静默启用一回。
edit_pair "$AGENT" \
  $'\t\tif sk.Enabled && sk.Slug == slug {\n' \
  $'\t\tif sk.Slug == slug {\n'
check_one 'rosterHasSlug 不筛 Enabled（停用技能被放行）' run_agent '已停用的技能'

# 注入 4 = 降级改成静默（slug 照样中和掉，但一声不吭）。
# 静默换路由比报错还难查：用户看到的是「正常写完了但在瞎写」，不知道技能没被用上。
edit_pair "$AGENT" \
  $'\t\tReportProgress(ctx, fmt.Sprintf("· 没找到叫「%s」的可用技能，本轮按通用写作处理", eval.SkillSlug))\n' \
  $'\t\t// （注入：故意静默降级，不出声）\n'
check_one '降级静默（换路由不告诉用户）' run_agent '降级是静默的'

# 注入 5 = L3 兜底整体退回旧实现（用户屏幕上只剩错误帧）。
# 两处一起改回旧形状：① 兜底改为播错误帧并结束整轮；② 标签随那个唯一的 goto 一起去掉
# （只删 goto 不删标签 = 「label defined and not used」编译红，不算断言红）。
# 注：log.Printf 刻意留着（保留它以避开 import 变未使用带来的编译红），
# 所以注入态只复刻用户可见行为：错误帧 + 零正文，不追求日志文案一致。
edit_pair "$CHAT" \
  $'\t\t\twrite(evMeta, jsonSafe(map[string]string{\n\t\t\t\t"reason": "技能「" + eval.SkillSlug + "」暂时读不出来，本轮按通用写作处理",\n\t\t\t\t"skill":  "", "intent": eval.Intent, "mode": mode,\n\t\t\t\t"note": "技能「" + eval.SkillSlug + "」暂时读不出来（可能刚被删除或文件缺失），这一轮已按通用写作处理。",\n\t\t\t}))\n\t\t\teval.SkillSlug = ""\n\t\t\tgoto plainPath\n' \
  $'\t\t\twrite(evError, lerr.Error())\n\t\t\treturn\n'
edit_pair "$CHAT" $'plainPath:\n' $'// （注入：标签随 goto 一起去掉）\n'
check_one 'L3 兜底退回整轮报错（用户屏幕只剩 ⚠ sql: no rows）' run_api '整轮报了错'

# ---------- 还原必须逐字节一致 ----------
AFTER="$(md5sum "$STORE" "$AGENT" "$CHAT" | awk '{print $1}' | tr '\n' ' ')"
if [ "$BEFORE" != "$AFTER" ]; then
  echo "✗ 还原不彻底：注入前后的 md5 不一致"
  echo "  注入前: $BEFORE"
  echo "  注入后: $AFTER"
  fails=$((fails + 1))
else
  echo "✓ 还原逐字节一致（md5 相同）"
fi

echo
if [ "$fails" -ne 0 ]; then
  echo "注入自证失败：$fails 条 —— 对应断言是假的，先修断言再推代码"
  exit 1
fi
echo "注入自证通过：5/5 全部红在指定断言上 ✓"
