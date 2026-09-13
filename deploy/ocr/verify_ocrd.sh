#!/usr/bin/env bash
# 在**干净的最小镜像**里验证 ocrd 自包含（构建流程里的一道门，不是可选步骤）。
#
# 判据（缺一不可）：
#   1. 进程能起来（打包漏了系统库 → 这里就崩了）
#   2. /health 返回 ok（ONNX 模型惰性加载完成）
#   3. 对**纯图像 PDF**抽取出的文本里含预期关键词 —— 证明 ONNX 推理真的跑了
#      （带文本层的 PDF 会走「零 OCR 快路径」，测不到引擎）
#
# 「红在崩溃上不算红」：每步失败都打印 FAIL 行并非零退出，避免「没验证」被当成「验证通过」。
# 期望关键词可用 $EXPECT 覆盖 —— 这是本脚本的自证开关：故意传一个不存在的词，必须见 FAIL。
set -uo pipefail

PORT="${PORT:-8093}"
EXPECT="${EXPECT:-离线部署}"
LOG=/tmp/ocrd-verify.log
FAILED=0

fail() { echo "FAIL: $*"; FAILED=1; }
ok()   { echo "  ok: $*"; }

echo "=== 1) 启动 ocrd（干净容器，无 GUI 库）==="
ocrd --port "$PORT" >"$LOG" 2>&1 &
PID=$!

echo "=== 2) 等 /health（模型惰性加载，首次较慢）==="
READY=0
for _ in $(seq 1 40); do
  if ! kill -0 "$PID" 2>/dev/null; then
    fail "ocrd 进程已退出（自包含性被破坏：多半缺某个系统库）"
    sed -n '1,25p' "$LOG"
    exit 1
  fi
  if curl -fsS "http://127.0.0.1:$PORT/health" >/tmp/health.json 2>/dev/null; then
    READY=1; break
  fi
  sleep 2
done
if [ "$READY" -eq 1 ]; then
  ok "health: $(head -c 200 /tmp/health.json)"
else
  fail "80 秒内 /health 未就绪"
  sed -n '1,25p' "$LOG"
fi

echo "=== 3) 真 OCR 一轮（纯图像 PDF → 必须读回关键词 '$EXPECT'）==="
if [ -f /fixture/scan.pdf ]; then
  RESP=$(curl -fsS -F "file=@/fixture/scan.pdf" "http://127.0.0.1:$PORT/extract" 2>/dev/null || echo '{"ok":false}')
  echo "  resp: $(printf '%s' "$RESP" | head -c 400)"
  # 结构判定先去空白：服务端 JSON 是 `"ok": true`（冒号后有空格），
  # 早先这里写死 `*'"ok":true'*` → 明明 ok=true 却报「ok 不是 true」，是**假红**
  # （断言本身错，不是产物错）。这类「靠字面量比对」的断言最容易被格式变化骗到。
  FLAT=$(printf '%s' "$RESP" | tr -d '[:space:]')
  case "$FLAT" in
    *'"ok":true'*) ok "ok=true" ;;
    *) fail "ok 不是 true" ;;
  esac
  # 关键词判定留在原始响应上（关键词本身可能含空格，不能先去空白）
  case "$RESP" in
    *"$EXPECT"*) ok "抽到预期关键词 '$EXPECT'" ;;
    *) fail "抽取结果里没有 '$EXPECT'（OCR 引擎没真跑起来）" ;;
  esac
else
  fail "容器里没有 /fixture/scan.pdf，无法验证 OCR 路径"
fi

kill "$PID" 2>/dev/null || true
wait "$PID" 2>/dev/null || true

if [ "$FAILED" -ne 0 ]; then
  echo "=== 自检未通过 ==="
  exit 1
fi
echo "=== 自检通过：ocrd 在无 GUI 库的裸镜像里可独立运行，且 OCR 推理可用 ==="
