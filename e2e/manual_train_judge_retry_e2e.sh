#!/bin/bash
# r503 训练线 E2E：裁判调用撞上瞬时 503 时，是不是「带退避重试、且重试不吃裁判轮次」。
#
# 【为什么这条线必须独立自证】
#   manual_train_judge_e2e.sh 跑的是「一路顺利」的裁判循环：它证明不了任何关于抖动的行为。
#   而线上事故恰恰发生在这里——裁判第 1 轮撞上一次 503（System is too busy now），
#   judgeLoop 当场 break，3 轮预算被一次网络抖动清空，技能照常落盘，fidelity.md 只剩
#   「⚠️ 裁判未跑完」。产物看上去完全正常，事故只在 fidelity 的一句话里。所以本脚本
#   用**假模型注入 503**（e2e/fake_llm_train.py 的 FAKE_LLM_JUDGE_503_TIMES）复现两种天候，
#   把断言钉在「用户能看见的结果」上：
#     场景 A（抖一下就自己好）：
#       ① trace 里出现「遇到瞬时故障，5s 后重试（第 1/3 次）」——重试是可见的，不是静默等待；
#       ② 训练照常走完并交付第 2 轮版本，system_prompt.md 含 SYS-V2-MARK；
#       ③ 关键：那一轮被顶替的请求和重试成功的请求**评分对象完全相同**（同一篇 TRIAL-R1
#          草稿、同一份 V1 提示词），且 fidelity.md 仍恰好 2 行轮次明细——重试没吃掉轮次；
#       ④ 没有「⚠️ 裁判未跑完」。
#     场景 B（抖动一直不停）：
#       ① 有限次放弃：裁判被判失败前恰好尝试 3 次（1+退避表长度），无一笔成功打分；
#       ② 不 hang：服务仍在跑、无 panic、SSE 里没有 error 帧（降级交付而不是把训练报错）；
#       ③ 如实告诉用户：fidelity.md 出现「⚠️ 裁判未跑完」、trace 出现放弃文案；
#       ④ 降级交付的是**原版** V1 提示词（不是空壳）。
#
# 【归属铁律】就绪断言必须同时命中「本次端口」+「本次数据目录」——服务自己打印的
#   `listening on <addr> (data: <dir>)` 是现成的归属证据。端口随机，避免残留实例占位。
#
# 退出码：0=通过  1=失败  2=跳过（缺二进制/缺假模型/fixture，显式标注，绝不假装通过）
set -uo pipefail

REPO=/root/skillforge
BIN=${BIN:-/tmp/sf-retry-e2e}
FAKE=$REPO/e2e/fake_llm_train.py
FIXTURE=$REPO/e2e/fixtures/manual_train_handbook.md
FAIL=0

echo "########## r503 裁判瞬时故障重试 E2E ##########"
echo "被测二进制：$BIN"
[ -x "$BIN" ] || {
  echo "SKIP: 缺可执行二进制 $BIN"
  echo "  先构建：export PATH=/usr/local/go/bin:\$PATH; CGO_ENABLED=0 go build -trimpath \\"
  echo "          -ldflags \"-s -w\" -o /tmp/sf-retry-e2e ./cmd/server"
  exit 2
}
stat -c '  编译时间 %y  %s bytes' "$BIN"
for f in "$FAKE" "$FIXTURE"; do
  [ -f "$f" ] || { echo "✗ 缺文件：$f"; exit 2; }
done

free_port() {
  python3 - <<'PY'
import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()
PY
}

# run_case <场景名> <503 次数> <数据目录>
run_case() {
  local CASE=$1 N503=$2 DATADIR=$3
  local APP_PORT LLM_PORT ADDR LLM_PID SRV ready
  APP_PORT=$(free_port); LLM_PORT=$(free_port)
  ADDR="127.0.0.1:${APP_PORT}"
  rm -rf "$DATADIR"; mkdir -p "$DATADIR"

  echo
  echo "=========================================================="
  echo "场景 $CASE：FAKE_LLM_JUDGE_503_TIMES=$N503"
  echo "  端口 $ADDR（随机） / 假模型 127.0.0.1:$LLM_PORT"
  echo "  数据目录 $DATADIR"
  echo "=========================================================="

  export FAKE_LLM_PORT="$LLM_PORT"
  export FAKE_LLM_LOG="$DATADIR/llm.jsonl"
  export FAKE_LLM_JUDGE_503_TIMES="$N503"
  python3 "$FAKE" > "$DATADIR/llm.log" 2>&1 &
  LLM_PID=$!
  ready=0
  for i in $(seq 1 30); do
    grep -q 'FAKE_LLM_READY' "$DATADIR/llm.log" 2>/dev/null && { ready=1; break; }
    kill -0 "$LLM_PID" 2>/dev/null || { echo "✗ 假模型退出"; tail -20 "$DATADIR/llm.log"; return 1; }
    sleep 0.5
  done
  [ "$ready" = 1 ] || { echo "✗ 假模型未就绪"; tail -20 "$DATADIR/llm.log"; return 1; }

  # 被测服务：绝不 source 线上 env（本次要的是假模型，不是真密钥）。
  export SKILLFORGE_ADDR="$ADDR"
  export SKILLFORGE_DATA_DIR="$DATADIR"
  export SKILLFORGE_DB="$DATADIR/skillforge.db"
  export SKILLFORGE_ADMIN_USER="e2e-admin"
  export SKILLFORGE_ADMIN_PASS="e2e-pass-123"
  # 真密钥一律不写进脚本；这两个只是在**启动时生成**的本地占位串。
  # 为什么不在源码里写死：一是写死了会被密钥脱敏过滤器改写（值变成 e2e-lo...cret
  # 这种半截串，读脚本的人分不清它是真密钥还是占位），二是占位串本来也没必要固定。
  export SKILLFORGE_LLM_PROVIDER="openai"
  export SKILLFORGE_LLM_BASE_URL="http://127.0.0.1:${LLM_PORT}"
  local RAND="e2e-local-$$-$RANDOM"
  export SKILLFORGE_JWT_SECRET="$RAND"
  export SKILLFORGE_LLM_API_KEY="$RAND"
  export SKILLFORGE_LLM_MODEL="fake-model"
  export SKILLFORGE_BASE_URL="http://$ADDR"

  local T0 T1
  T0=$(date +%s)
  "$BIN" > "$DATADIR/server.log" 2>&1 &
  SRV=$!
  ready=0
  for i in $(seq 1 40); do
    if grep -qF "listening on ${ADDR} (data: ${DATADIR})" "$DATADIR/server.log" 2>/dev/null; then
      ready=1; break
    fi
    if ! kill -0 "$SRV" 2>/dev/null; then
      echo "✗ 服务进程已退出（pid=$SRV）——不接受「打到别人实例上」的结果"
      tail -20 "$DATADIR/server.log"; kill "$LLM_PID" 2>/dev/null; return 1
    fi
    sleep 1
  done
  [ "$ready" = 1 ] || { echo "✗ 40s 内未确认服务归属"; tail -20 "$DATADIR/server.log"; kill "$LLM_PID" 2>/dev/null; return 1; }

  # 投放手册 + 触发训练（SSE 落盘）。退出码由「有没有 error 帧」决定：流结束 ≠ 成功。
  python3 - "$ADDR" "$FIXTURE" "$DATADIR/train.sse" "$CASE" <<'PY'
import json, os, sys, urllib.request, uuid

addr, fixture, out, case = sys.argv[1:5]
BASE = "http://" + addr
USER = os.environ.get("SKILLFORGE_ADMIN_USER", "e2e-admin")
PASS = os.environ.get("SKILLFORGE_ADMIN_PASS", "")

body = json.dumps({"username": USER, "password": PASS}).encode()
r = urllib.request.Request(BASE + "/api/login", data=body, method="POST")
r.add_header("Content-Type", "application/json")
with urllib.request.urlopen(r, timeout=30) as resp:
    tok = json.load(resp).get("token", "")
if not tok:
    raise SystemExit("✗ 登录失败：拿不到 token")
print("  管理端登录 ✓")

raw = open(fixture, "rb").read()
bnd = "----sf" + uuid.uuid4().hex
parts = []


def field(k, v):
    parts.append(f'--{bnd}\r\nContent-Disposition: form-data; name="{k}"\r\n\r\n{v}\r\n'.encode())


field("name", f"retry-e2e-{case}")
field("category", "写作")
field("description", "依据上传的写作手册覆盖全部类别")
field("requirement", "依据上传手册覆盖全部类别；每类给出可核对的写作要求，并引用手册原文作为范文。")
parts.append(
    f'--{bnd}\r\nContent-Disposition: form-data; name="files"; '
    f'filename="{os.path.basename(fixture)}"\r\nContent-Type: text/markdown\r\n\r\n'.encode()
)
parts.append(raw)
parts.append(f"\r\n--{bnd}--\r\n".encode())
req = urllib.request.Request(BASE + "/api/admin/train", data=b"".join(parts), method="POST")
req.add_header("Authorization", "Bearer " + tok)
req.add_header("Content-Type", "multipart/form-data; boundary=" + bnd)
with urllib.request.urlopen(req, timeout=900) as resp, open(out, "wb") as f:
    while True:
        chunk = resp.read(256)
        if not chunk:
            break
        f.write(chunk)
        f.flush()

txt = open(out, encoding="utf-8", errors="replace").read()
errs = [l for l in txt.splitlines() if '"type":"error"' in l]
if errs:
    # 场景 B 期望的是「降级交付」，所以这里只记录，判定交给断言段（避免脚本中途硬退）。
    print("  ⚠ SSE 出现 error 帧：" + errs[0][:300])
print("  训练流结束（SSE %s）" % out)
PY
  local rc=$?
  T1=$(date +%s)
  echo "  本轮墙钟耗时 $((T1 - T0))s"
  [ $rc -eq 0 ] || { echo "✗ 投放素材/触发训练失败"; kill "$SRV" "$LLM_PID" 2>/dev/null; return 1; }

  local SLUGDIR=""
  for d in "$DATADIR"/skills/*/; do
    [ -f "${d}fidelity.md" ] && SLUGDIR="${d%/}"
  done
  if [ -z "$SLUGDIR" ]; then
    echo "✗ 找不到落盘技能目录（$DATADIR/skills/*/fidelity.md 一个都没有）"
    kill "$SRV" "$LLM_PID" 2>/dev/null; return 1
  fi
  echo "  产出技能目录：$SLUGDIR"

  echo "  --- 场景 $CASE 断言 ---"
  if grep -qiE 'panic' "$DATADIR/server.log"; then
    echo "  ✗ 服务端日志有 panic"; grep -i -m3 panic "$DATADIR/server.log"; FAIL=1
  else
    echo "  ✓ 服务端日志无 panic"
  fi
  if kill -0 "$SRV" 2>/dev/null; then
    echo "  ✓ 服务进程仍存活（有限次重试，没有挂死/崩溃）"
  else
    echo "  ✗ 服务进程已退出（跑到一半挂了）"; FAIL=1
  fi

  python3 - "$DATADIR" "$SLUGDIR" "$CASE" "$N503" "$((T1 - T0))" <<'PY'
import json, os, re, sys

datadir, slugdir, case, n503, elapsed = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4]), int(sys.argv[5])
F = []


def ok(cond, msg):
    print(("  ✓ " if cond else "  ✗ ") + msg)
    if not cond:
        F.append(msg)


sse = open(os.path.join(datadir, "train.sse"), encoding="utf-8", errors="replace").read()
logs = [json.loads(l) for l in open(os.path.join(datadir, "llm.jsonl"), encoding="utf-8") if l.strip()]
kinds = [x.get("kind") for x in logs]
fid = open(os.path.join(slugdir, "fidelity.md"), encoding="utf-8").read()
spp = os.path.join(slugdir, "system_prompt.md")
sp = open(spp, encoding="utf-8").read() if os.path.exists(spp) else ""

# ---------- 共用：抖动确实打在了裁判上，且次数正好是「预算内该打的次数」 ----------
# 一轮裁判最多尝试 1 + len(退避表) = 3 次；注入次数超过它就是「抖动一直不停」。
BUDGET = 3
ok(kinds.count("judge_503") == min(n503, BUDGET),
   f"假模型按注入次数回了 503（n503={n503}，期望 {min(n503, BUDGET)}，实得 {kinds.count('judge_503')}）")

RE_NOTE = re.compile(r"8\.5/9 第 (\d+) 轮裁判调用遇到瞬时故障，(\S+) 后重试（第 (\d+)/(\d+) 次）")

if case == "A":
    # ① 重试可见：trace 里有「5s 后重试（第 1/3 次）」，且是在**第 1 轮**里发生的
    m = RE_NOTE.search(sse)
    ok(m is not None, "trace 含重试文案「8.5/9 第 N 轮裁判调用遇到瞬时故障，X 后重试（第 a/b 次）」")
    if m:
        ok(m.group(1) == "1", f"重试发生在第 {m.group(1)} 轮（裁判第一次调用就抽到了 503）")
        ok(m.group(2) == "5s", f"退避时长 {m.group(2)}（产品退避表首项，不是被测试改过的毫秒值）")
        ok(m.group(4) == "3", f"文案里交代了总尝试数 {m.group(4)}（= 1 + 退避表长度）")
    # 文案只截 120 字（clipRunes），所以**不能**要求它含上游 body 全文：真值里 body 原文
    # 被切在 120 字之后。用户要能分辨的是「上游忙」还是「自己配错了」——那就钉状态码。
    note_line = next((l for l in sse.splitlines() if "遇到瞬时故障" in l), "")
    ok("503" in note_line and "Service Unavailable" in note_line,
       "重试文案带上了上游状态码（503 + Service Unavailable）——用户看得出是上游忙，不是自己配错了")
    # 「不静默等待」：重试提示必须排在交付帧之前，界面才不会静止十几秒像卡死。
    i_note, i_done = sse.find("遇到瞬时故障"), sse.find("8.5/9 交付第 2 轮版本（")
    ok(0 <= i_note < i_done, f"重试提示出现在交付帧之前（位置 {i_note} < {i_done}）——不是静默等待")

    # ② 训练照常走完，且交付的是经过评审的第 2 轮版本
    ok("8.5/9 交付第 2 轮版本（" in sse, "trace 含整段交付帧「8.5/9 交付第 2 轮版本（N 字）」")
    ok("SYS-V2-MARK" in sp and "SYS-V1-MARK" not in sp,
       "落盘 system_prompt.md 是 V2（回炉版）——一次抖动没把交付质量打回去")
    ok("- ⚠️ 裁判未跑完：" not in fid, "fidelity.md 没有「⚠️ 裁判未跑完」（裁判跑完了）")

    # ③ 关键：重试不吃轮次 —— 被顶替的那笔与重试成功的那笔评的是**同一篇**稿子
    j503 = [x for x in logs if x.get("kind") == "judge_503"]
    jok = [x for x in logs if x.get("kind") == "judge"]
    ok(len(j503) == 1, f"503 顶替了恰好 1 笔裁判调用（实得 {len(j503)}）")
    ok(len(jok) == 2, f"成功打分的裁判恰好 2 轮（实得 {len(jok)}）——重试没生成额外的评审结论")
    if j503 and jok:
        ok(j503[0]["user"] == jok[0]["user"],
           "重试的请求与被 503 顶替的请求**评的是同一篇稿子**（同 user 内容），不是换了一轮")
        ok("TRIAL-R1" in j503[0]["user"] and "TRIAL-R1" in jok[0]["user"],
           "两笔都在评第 1 轮草稿（TRIAL-R1）")
    trials = [x for x in logs if x.get("kind") == "trial"]
    ok(len(trials) == 2, f"试用写稿仍是 2 次（实得 {len(trials)}）——没因为裁判抖动多试一遍稿")

    # ④ fidelity 的轮次表仍是 2 行，交付第 2 轮 —— 「轮次」这个概念没被重试污染
    rows = [l for l in fid.splitlines() if re.match(r"^\|\s*第 \d+ 轮\s*\|", l)]
    ok(len(rows) == 2, f"fidelity.md 恰好 2 行轮次明细（实得 {len(rows)}）")
    ok("- 交付轮次：第 2 轮（同分取更早轮次，保证结果可复核）" in fid,
       "fidelity.md 交付行是「- 交付轮次：第 2 轮…」")
    ok(kinds.count("judge_503") == 1 and kinds.count("judge") == 2,
       "llm.jsonl 计数自洽：裁判 = 1 笔抖动 + 2 笔结论")

elif case == "B":
    # ① 有限次放弃：一轮里恰好尝试 3 次（1 + 2 次重试），无一笔拿到分数
    notes = RE_NOTE.findall(sse)
    ok(len(notes) == 2, f"trace 里恰好 2 条重试文案（实得 {len(notes)}）——退避表长度 = 2 次重试")
    ok([n[2] for n in notes] == ["1", "2"] if len(notes) == 2 else False,
       "两次重试的序号是 1、2（依次退避，不是原地连打）")
    ok(kinds.count("judge_503") == 3, f"裁判被判失败前共尝试 3 次（实得 {kinds.count('judge_503')}）")
    ok(kinds.count("judge") == 0, "没有任何一笔裁判拿到过分数（抖动确实持续了一整轮）")

    # ② 不 hang：不靠墙钟猜，靠「有限次数」自证；顺便报一下实际耗时给人看
    ok(elapsed < 120, f"整轮墙钟 {elapsed}s < 120s（两次退避 5s+15s 量级的有限等待，不是无限挂起）")

    # ③ 如实告知：用户看得见「裁判没跑完」，而不是拿到一个假装验收过的技能
    ok("- ⚠️ 裁判未跑完：" in fid, "fidelity.md 出现「- ⚠️ 裁判未跑完：」（如实交代，不假装验收过）")
    # 判别性断言：注入「不重试」缺陷时这句会消失——所以它证明的是「这一次真的重试过」，
    # 而不是像「错误里含『裁判调用』」那样在修复前后都能绿的空话。
    ok("已按退避重试 2 次" in fid,
       "fidelity.md 交代了「已按退避重试 2 次…仍失败」——用户/维护者据此区分「上游抖动，稍后再跑」与「配置错了，去改」")
    ok("累计等待 20s" in fid, "fidelity.md 写明了累计等待 20s（5s+15s，与产品退避表一致，不是被压缩过的值）")

    # ④ 降级交付的是原版 V1（不是空壳、也不是半成品）
    ok("SYS-V1-MARK" in sp, "落盘 system_prompt.md 是原版 V1（降级交付，不是空壳）")
    ok(len(sp.strip()) >= 300, f"落盘 system_prompt.md 长度 {len(sp.strip())} ≥ 300（过了本地硬门）")

print()
sys.exit(1 if F else 0)
PY
  [ $? -eq 0 ] || FAIL=1

  kill "$SRV" "$LLM_PID" 2>/dev/null
  wait "$SRV" 2>/dev/null
  return 0
}

run_case A 1 "/tmp/sf-retry-A-$(date +%H%M%S)"
run_case B 99 "/tmp/sf-retry-B-$(date +%H%M%S)"

echo
if [ "$FAIL" = 0 ]; then
  echo "########## r503 E2E 通过 ✓ ##########"
  echo "证据：/tmp/sf-retry-A-*/train.sse | llm.jsonl（1 笔 judge_503 + 2 笔 judge）"
  echo "      /tmp/sf-retry-B-*/train.sse | llm.jsonl（3 笔 judge_503 + 0 笔 judge）"
  exit 0
else
  echo "########## r503 E2E 失败 ✗ ##########"
  echo "  A 场景：/tmp/sf-retry-A-*/   B 场景：/tmp/sf-retry-B-*/"
  exit 1
fi
