#!/bin/bash
# 训练线 E2E：Step8.5 裁判循环是否真的被 Generate() 跑到、落盘的是不是「最优轮版本」。
#
# 【为什么这条线必须独立自证】
#   manual_pipeline_e2e.sh  只证明「素材抽出来了」（分类数/范文数/保真）；
#   manual_write_runtime_e2e.sh 只证明「运行时按手册那一类写」；
#   都没有碰过训练期的 **Step8.5 裁判试用与回炉**。而这段逻辑正是最容易静默降级的：
#   裁判一次没跑（或永远给满分）时，技能照样生成、照样注册上线，磁盘上躺着的
#   却是没验收过的第 1 版提示词——从产物上完全看不出差别。所以本脚本把断言钉在
#   三件可复核的事上：
#     ① trace 里出现完整的 8.5 开帧与交付帧（裁判真的在 Generate() 里跑了）；
#     ② 假模型日志证明「第 1 轮用 V1 提示词试用并判不合格 → 回炉 → 第 2 轮用 V2 试用并满分」；
#     ③ 落盘的 system_prompt.md 是 **V2**（= 最优那一轮），fidelity.md 恰好 2 轮明细 +
#        「- 交付轮次：第 2 轮」。
#
# 【假模型】e2e/fake_llm_train.py 顶替真模型：第 1 轮草稿含 TRIAL-R1（裁判给低分 → 回炉），
#   第 2 轮草稿含 TRIAL-R2（裁判给满分 → 交付）。判定靠草稿内容而不是调用次数，
#   所以重跑/乱序都不影响结论；每一笔请求与回复全文都落 llm.jsonl 供事后核对。
#
# 【归属铁律】就绪断言必须同时命中「本次端口」+「本次数据目录」——服务自己打印的
#   `listening on <addr> (data: <dir>)` 是现成的归属证据。端口随机，避免残留实例占位
#   导致「请求打到别人实例上」的假红/假绿。
#
# 退出码：0=通过  1=失败  2=跳过（缺二进制/缺假模型/缺 fixture，显式标注，绝不假装通过）
set -uo pipefail

REPO=/root/skillforge
BIN=${BIN:-/tmp/sf-train-e2e}
FAKE=$REPO/e2e/fake_llm_train.py
FIXTURE=$REPO/e2e/fixtures/manual_train_handbook.md
DATADIR=${DATADIR:-/tmp/sf-train-e2e-$(date +%H%M%S)}
NAME=${NAME:-train-judge-e2e}

echo "########## 训练线 Step8.5 裁判循环 E2E ##########"
echo "被测二进制：$BIN"
[ -x "$BIN" ] || {
  echo "SKIP: 缺可执行二进制 $BIN"
  echo "  先构建：export PATH=/usr/local/go/bin:\$PATH; CGO_ENABLED=0 go build -trimpath \\"
  echo "          -ldflags \"-s -w\" -o /tmp/sf-train-e2e ./cmd/server"
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

APP_PORT=$(free_port)
LLM_PORT=$(free_port)
ADDR="127.0.0.1:${APP_PORT}"
export FAKE_LLM_PORT="$LLM_PORT"
export FAKE_LLM_LOG="$DATADIR/llm.jsonl"

rm -rf "$DATADIR"; mkdir -p "$DATADIR"
echo "本次端口：$ADDR（随机） / 假模型：127.0.0.1:$LLM_PORT"
echo "本次数据目录：$DATADIR"
echo

# --- 假模型先起：它不开，被测服务的第一条模型请求就会失败并掩盖真实断言 ---
python3 "$FAKE" > "$DATADIR/llm.log" 2>&1 &
LLM_PID=$!
for i in $(seq 1 30); do
  grep -q 'FAKE_LLM_READY' "$DATADIR/llm.log" 2>/dev/null && break
  kill -0 "$LLM_PID" 2>/dev/null || { echo "✗ 假模型退出"; tail -20 "$DATADIR/llm.log"; exit 1; }
  sleep 0.5
done
grep -q 'FAKE_LLM_READY' "$DATADIR/llm.log" || { echo "✗ 假模型未就绪"; tail -20 "$DATADIR/llm.log"; exit 1; }
echo "假模型就绪 ✓（请求日志 $DATADIR/llm.jsonl）"

# --- 被测服务：绝不 source 线上 env（本次要的是假模型，不是真密钥） ---
export SKILLFORGE_ADDR="$ADDR"
export SKILLFORGE_DATA_DIR="$DATADIR"
export SKILLFORGE_DB="$DATADIR/skillforge.db"
export SKILLFORGE_ADMIN_USER="e2e-admin"
export SKILLFORGE_ADMIN_PASS="e2e-pass-123"
# 真密钥一律不写进脚本：这些只是本地 E2E 占位串。
export SKILLFORGE_JWT_SECRET="e2e-local-jwt-secret"
export SKILLFORGE_LLM_PROVIDER="openai"
export SKILLFORGE_LLM_BASE_URL="http://127.0.0.1:${LLM_PORT}"
export SKILLFORGE_LLM_API_KEY="e2e-local-dev-only"
export SKILLFORGE_LLM_MODEL="fake-model"
export SKILLFORGE_BASE_URL="http://$ADDR"

"$BIN" > "$DATADIR/server.log" 2>&1 &
SRV=$!
echo "本次实例 pid=$SRV"

cleanup() { kill "$SRV" "$LLM_PID" 2>/dev/null; wait "$SRV" 2>/dev/null; }
trap cleanup EXIT

ready=0
for i in $(seq 1 40); do
  if grep -qF "listening on ${ADDR} (data: ${DATADIR})" "$DATADIR/server.log" 2>/dev/null; then
    echo "就绪 ✓ 已确认本实例 data=${DATADIR}"; ready=1; break
  fi
  if ! kill -0 "$SRV" 2>/dev/null; then
    echo "✗ 服务进程已退出（pid=$SRV）——不接受「打到别人实例上」的结果"
    tail -20 "$DATADIR/server.log"; exit 1
  fi
  sleep 1
done
[ "$ready" = 1 ] || { echo "✗ 40s 内未确认服务归属"; tail -20 "$DATADIR/server.log"; exit 1; }
sleep 0.5

echo
echo "=== ① 登录 + 投放手册素材（.md 纯文本，3 类）+ 触发训练（SSE）==="
# 上传走 /api/admin/train（multipart）：登录 → 投 fixture → 把 SSE 流原样落盘。
# 注意：流结束 ≠ 训练成功（SSE 把失败表达成 error 帧），所以退出码由「有没有 error 帧」决定。
python3 - "$ADDR" "$FIXTURE" "$DATADIR/train.sse" <<'PY'
import json, os, sys, urllib.request, uuid

addr, fixture, out = sys.argv[1], sys.argv[2], sys.argv[3]
BASE = "http://" + addr
USER = os.environ.get("SKILLFORGE_ADMIN_USER", "e2e-admin")
PASS = os.environ.get("SKILLFORGE_ADMIN_PASS", "")


def login():
    body = json.dumps({"username": USER, "password": PASS}).encode()
    r = urllib.request.Request(BASE + "/api/login", data=body, method="POST")
    r.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(r, timeout=30) as resp:
        tok = json.load(resp).get("token", "")
    if not tok:
        raise SystemExit("✗ 登录失败：拿不到 token")
    return tok


tok = login()
print("  管理端登录 ✓")

raw = open(fixture, "rb").read()
bnd = "----sf" + uuid.uuid4().hex
name = os.environ.get("NAME", "train-judge-e2e")
parts = []


def field(k, v):
    parts.append(f'--{bnd}\r\nContent-Disposition: form-data; name="{k}"\r\n\r\n{v}\r\n'.encode())


field("name", name)
field("category", "写作")
field("description", "依据上传的写作手册覆盖全部类别")
field("requirement", "依据上传手册覆盖全部类别；每类给出可核对的写作要求，并引用手册原文作为范文。")
parts.append(
    f'--{bnd}\r\nContent-Disposition: form-data; name="files"; '
    f'filename="{os.path.basename(fixture)}"\r\nContent-Type: text/markdown\r\n\r\n'.encode()
)
parts.append(raw)
parts.append(f"\r\n--{bnd}--\r\n".encode())
body = b"".join(parts)

req = urllib.request.Request(BASE + "/api/admin/train", data=body, method="POST")
req.add_header("Authorization", "Bearer " + tok)
req.add_header("Content-Type", "multipart/form-data; boundary=" + bnd)
with urllib.request.urlopen(req, timeout=600) as resp, open(out, "wb") as f:
    while True:
        chunk = resp.read(256)
        if not chunk:
            break
        f.write(chunk)
        f.flush()

txt = open(out, encoding="utf-8", errors="replace").read()
errs = [l for l in txt.splitlines() if '"type":"error"' in l]
if errs:
    print("  ✗ 训练报错：" + errs[0][:400])
    raise SystemExit(1)
print("  训练流结束 ✓（SSE 日志 %s）" % out)
PY
[ $? -eq 0 ] || { echo "✗ 投放素材/触发训练失败"; exit 1; }

# 训练产物的技能目录：靠 fidelity.md 定位（内置技能没有这份报告），并要求唯一。
SLUGDIR=""
for d in "$DATADIR"/skills/*/; do
  [ -f "${d}fidelity.md" ] && SLUGDIR="${d%/}"
done
if [ -z "$SLUGDIR" ]; then
  echo "✗ 找不到落盘技能目录（$DATADIR/skills/*/fidelity.md 一个都没有）"
  exit 1
fi
echo "产出技能目录：$SLUGDIR"
echo

FAIL=0
report() { if [ "$1" = 1 ]; then echo "  ✓ $2"; else echo "  ✗ $2"; FAIL=1; fi; }

echo "########## 结果断言 ##########"

if grep -qiE 'panic' "$DATADIR/server.log"; then
  report 0 "服务端日志有 panic"; grep -i -m3 panic "$DATADIR/server.log"
else
  report 1 "服务端日志无 panic"
fi
kill -0 "$SRV" 2>/dev/null || report 0 "服务进程已退出（跑到一半挂了）"

python3 - "$DATADIR" "$SLUGDIR" <<'PY'
import json, os, re, sys

datadir, slugdir = sys.argv[1], sys.argv[2]
FAIL = []


def ok(cond, msg):
    print(("  ✓ " if cond else "  ✗ ") + msg)
    if not cond:
        FAIL.append(msg)


sse = open(os.path.join(datadir, "train.sse"), encoding="utf-8", errors="replace").read()
logs = [json.loads(l) for l in open(os.path.join(datadir, "llm.jsonl"), encoding="utf-8")
        if l.strip()]
kinds = [x.get("kind") for x in logs]


def of(k):
    return [x for x in logs if x.get("kind") == k]


# ---------- ① trace：Step8.5 真的被 Generate() 跑到了（整段锚定，不用裸「第 2 轮」） ----------
ok("8.5/9 裁判独立试用评分（上限 3 轮，不过线按扣分项回炉）" in sse,
   "trace 含整段开帧「8.5/9 裁判独立试用评分（上限 3 轮，不过线按扣分项回炉）…」")
ok("8.5/9 交付第 2 轮版本（" in sse,
   "trace 含整段交付帧「8.5/9 交付第 2 轮版本（N 字）」")
ok("8.5/9 回炉版本未过本地校验，保留原版" not in sse,
   "没有出现旁路帧「回炉版本未过本地校验，保留原版」（交付的确实是回炉版）")

# ---------- ② 假模型日志：V1 试用→不合格→回炉→V2 试用→满分 ----------
trials, judges, revs = of("trial"), of("judge"), of("revise_prompt")
ok(len(trials) == 2, f"试用调用恰好 2 轮（实得 {len(trials)}）——裁判循环真的转起来了")
ok(len(judges) == 2, f"裁判打分恰好 2 轮（实得 {len(judges)}）")
ok(len(revs) == 1, f"回炉重写提示词恰好 1 次（实得 {len(revs)}）")
ok(kinds.count("fallback") == 0,
   f"没有提示词走兜底分支（实得 {kinds.count('fallback')} 次）——分发覆盖了训练全流程")
ok(kinds.count("sysprompt_v1") == 1 and kinds.count("structure") >= 1,
   "Step4 生成提示词、Step5 结构抽取都被调用过")

if len(trials) == 2:
    ok("SYS-V1-MARK" in trials[0]["system"] and "SYS-V2-MARK" not in trials[0]["system"],
       "第 1 轮试用用的是 Step4 产出的 V1 提示词")
    ok("SYS-V2-MARK" in trials[1]["system"],
       "第 2 轮试用用的是**回炉后**的 V2 提示词（回炉结果真的接回了试用）")
    ok(trials[0]["reply"].startswith("TRIAL-R1") and trials[1]["reply"].startswith("TRIAL-R2"),
       "两轮草稿各自带轮次标记（第 1 轮 TRIAL-R1 / 第 2 轮 TRIAL-R2）")
    ok(trials[0]["user"] != trials[1]["user"],
       "两轮试用请求不同（第 2 轮换了提示词，不是把同一份原样再试一遍）")

if len(judges) == 2:
    def total(j):
        try:
            dims = json.loads(j["reply"])["dims"]
        except Exception:
            return None
        return sum(d["score"] for d in dims)

    t1, t2 = total(judges[0]), total(judges[1])
    ok(t1 is not None and t1 < 80, f"第 1 轮裁判总分 {t1} < 80（不过线 → 触发回炉）")
    ok(t2 == 100, f"第 2 轮裁判总分 {t2} == 100（通过 → 停止回炉）")
    ok(all("REQ-" in j["user"] for j in judges),
       "裁判 prompt 注入了手册该类写作要求的原文摘录（手上有标尺）")
    ok(not any("SYS-V1-MARK" in j["system"] or "SYS-V2-MARK" in j["system"] for j in judges),
       "裁判 system 里没有待评技能的自述（独立评审，不自己给自己判卷）")

if revs:
    ok("SYS-V1-MARK" in revs[0]["user"], "回炉请求带上了上一版（V1）原文，不是凭空重写")
    ok(len(revs[0]["reply"].strip()) >= 300, "回炉产出的新版提示词 ≥300 字符（能过本地硬门）")

# ---------- ③ 落盘：交付的是「最优轮版本」V2 ----------
sp_path = os.path.join(slugdir, "system_prompt.md")
sp = open(sp_path, encoding="utf-8").read() if os.path.exists(sp_path) else ""
ok("SYS-V2-MARK" in sp, f"落盘的 {os.path.basename(slugdir)}/system_prompt.md 含 SYS-V2-MARK（第 2 轮版本）")
ok("SYS-V1-MARK" not in sp, "落盘的 system_prompt.md **不含** SYS-V1-MARK（不是没验收的原版）")
ok(len(sp.strip()) >= 300, f"落盘 system_prompt.md 长度 {len(sp.strip())} ≥ 300")

fid_path = os.path.join(slugdir, "fidelity.md")
fid = open(fid_path, encoding="utf-8").read() if os.path.exists(fid_path) else ""
ok("| 轮次 | 试用分类 | 总分 | 结论 | 扣分项 |" in fid, "fidelity.md 含裁判评分表表头")
rows = [l for l in fid.splitlines() if re.match(r"^\|\s*第 \d+ 轮\s*\|", l)]
ok(len(rows) == 2, f"fidelity.md 恰好 2 行轮次明细（实得 {len(rows)}）")
ok("- 交付轮次：第 2 轮（同分取更早轮次，保证结果可复核）" in fid,
   "fidelity.md 交付行是「- 交付轮次：第 2 轮…」")
ok("- 提前止损：" not in fid,
   "fidelity.md 没有「- 提前止损：」（2 轮内通过，不是带薄弱项兜底交付）")
ok("- ⚠️ 裁判未跑完：" not in fid,
   "fidelity.md 没有「- ⚠️ 裁判未跑完：」（裁判确实跑完了）")

print()
sys.exit(1 if FAIL else 0)
PY
[ $? -eq 0 ] || FAIL=1

echo
if [ "$FAIL" = 0 ]; then
  echo "########## E2E 通过 ✓ ##########"
  echo "产物：$SLUGDIR"
  echo "证据：$DATADIR/train.sse（trace） / $DATADIR/llm.jsonl（每笔请求与回复）"
  exit 0
else
  echo "########## E2E 失败 ✗ ##########"
  echo "产物：$DATADIR"
  echo "  看 trace：$DATADIR/train.sse"
  echo "  看模型调用：$DATADIR/llm.jsonl（kind=trial/judge/revise_prompt/fallback）"
  exit 1
fi
