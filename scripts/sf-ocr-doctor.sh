#!/usr/bin/env bash
#
# sf-ocr-doctor.sh —— 在「装了 SkillForge 的那台机器」上跑，一条命令定位
#   「训练技能 / 自动提取技能 报 127.0.0.1:8093 connection refused」
# 的原因，并给出可直接粘贴的修复命令。
#
# 用法：
#   sudo bash sf-ocr-doctor.sh
#   sudo SF_OCR_UNIT=skillforge-ocr bash sf-ocr-doctor.sh   # 指定单元名（多实例/改名时用）
#
# 退出码：
#   0 = 解析服务健康（8093 在听且 /health 报 ok）
#   1 = 有问题：问题清单 + 修复命令已打印
#   2 = 缺前置（不是 systemd 装的 / 没有 curl）
#
# 为什么要有它（2026-09-16 现场）：
#   这个报错的原话是 Go 的 `dial tcp 127.0.0.1:8093: connect: connection refused`。
#   它只说明「没人听 8093」，不说明是「单元压根没装」「崩溃后没重启（旧包没有
#   Restart=always，一次死亡即永久死亡）」还是「端口被别的进程占了」。
#   三种情形的修法完全不同，用户拿到的信息量等于零。
#   本脚本把三种情形分开，并且**只报告看到的**，不猜。

set -uo pipefail

PORT="${SF_OCR_PORT:-8093}"
UNIT="${SF_OCR_UNIT:-}"

C_OK=$'\033[32m'; C_WARN=$'\033[33m'; C_ERR=$'\033[31m'; C_DIM=$'\033[2m'; C_OFF=$'\033[0m'
[ -t 1 ] || { C_OK=""; C_WARN=""; C_ERR=""; C_DIM=""; C_OFF=""; }

PROBLEMS=()
FIXES=()
add_problem() { PROBLEMS+=("$1"); }
add_fix() { FIXES+=("$1"); }

hr() { printf '%s\n' "────────────────────────────────────────────────────────────"; }

echo "SkillForge 文档解析服务（8093）体检"
hr

# ── 0. 前置 ──────────────────────────────────────────────────────────
command -v systemctl >/dev/null 2>&1 || {
	echo "${C_ERR}这台机器没有 systemctl —— SkillForge 的解析服务是 systemd 单元，${C_OFF}"
	echo "请贴回：uname -a 与你的安装方式（是否用了 offline 包的 install.sh）。"
	exit 2
}
command -v curl >/dev/null 2>&1 || { echo "${C_ERR}缺 curl，无法探测 8093。装一个 curl 再跑。${C_OFF}"; exit 2; }

# ── 1. 找到单元 ──────────────────────────────────────────────────────
if [ -z "$UNIT" ]; then
	# 先认标准名；认不到就按 skillforge*ocr 扫（安装时 --service-name 改过名的情况）
	if systemctl cat skillforge-ocr.service >/dev/null 2>&1; then
		UNIT="skillforge-ocr"
	else
		UNIT="$(systemctl list-unit-files --type=service --no-legend 2>/dev/null \
			| awk '{print $1}' | grep -i 'skillforge.*ocr' | head -1 | sed 's/\.service$//')"
	fi
fi

if [ -z "$UNIT" ] || ! systemctl cat "$UNIT.service" >/dev/null 2>&1; then
	echo "${C_ERR}① 没找到文档解析服务单元${C_OFF}（既没有 skillforge-ocr.service，也没有名字含 skillforge…ocr 的单元）"
	echo
	echo "这说明安装时**没有启用解析服务**，最常见的原因是装的时候带了 ${C_WARN}--no-ocr${C_OFF}，"
	echo "或者包本身是不含 ocrd 的那份（arm64 离线包按设计不带解析服务）。"
	echo
	echo "确认一下："
	echo "  ls -l /opt/skillforge/bin/ocrd        # 解析服务二进制在不在"
	echo "  systemctl list-unit-files | grep -i skillforge"
	hr
	echo "修复：用**自带 ocrd 的 amd64 离线包**重装并启用解析服务（不要加 --no-ocr）。"
	echo "  sudo ./install.sh                    # 默认就装并启用解析服务"
	exit 1
fi

echo "单元：${C_OK}$UNIT${C_OFF}    端口：$PORT"
PREFIX="$(systemctl show "$UNIT" -p ExecStart --value 2>/dev/null | grep -o '/[^ ]*/bin/ocrd' | head -1 | sed 's|/bin/ocrd$||')"
[ -n "$PREFIX" ] || PREFIX="/opt/skillforge"
echo "前缀：$PREFIX"
hr

# ── 2. 单元在不在跑 ──────────────────────────────────────────────────
ACTIVE="$(systemctl is-active "$UNIT" 2>/dev/null || true)"
ENABLED="$(systemctl is-enabled "$UNIT" 2>/dev/null || true)"
NFAIL="$(systemctl show "$UNIT" -p NRestarts --value 2>/dev/null || echo '?')"
RESTART_POLICY="$(systemctl show "$UNIT" -p Restart --value 2>/dev/null || echo '?')"
TMPDIR_SET="$(systemctl show "$UNIT" -p Environment --value 2>/dev/null | tr ' ' '\n' | grep '^TMPDIR=' || true)"

echo "状态：active=${ACTIVE:-?}  enabled=${ENABLED:-?}  已重启次数=${NFAIL}  重启策略=${RESTART_POLICY}"
echo "TMPDIR：${TMPDIR_SET:-（单元里没设 → 会用默认 /tmp）}"

if [ "$ACTIVE" != "active" ]; then
	add_problem "① 单元没在运行（active=$ACTIVE）"
	add_fix "systemctl status $UNIT --no-pager -l | tail -30"
	add_fix "journalctl -u $UNIT -n 80 --no-pager"
fi

# 旧包（≤ v260916.0147）的单元没有 Restart=always：
# 解析服务崩一次就永久 failed，之后每个请求都是 connection refused，且永不恢复。
case "$RESTART_POLICY" in
	always) : ;;
	*)
		add_problem "② 单元没有 Restart=always（当前 $RESTART_POLICY）→ 崩一次就永久 refused，不会自愈"
		add_fix "sudo mkdir -p /etc/systemd/system/$UNIT.service.d"
		add_fix "printf '[Unit]\\nStartLimitIntervalSec=0\\n[Service]\\nRestart=always\\nRestartSec=3\\n' | sudo tee /etc/systemd/system/$UNIT.service.d/restart.conf"
		;;
esac

# ── 3. 端口有没有人听 ────────────────────────────────────────────────
LISTEN_PID=""
if command -v ss >/dev/null 2>&1; then
	LISTEN_PID="$(ss -ltnp 2>/dev/null | awk -v p=":$PORT" '$4 ~ p {print; exit}')"
elif command -v netstat >/dev/null 2>&1; then
	LISTEN_PID="$(netstat -ltnp 2>/dev/null | awk -v p=":$PORT" '$4 ~ p {print; exit}')"
fi

if [ -z "$LISTEN_PID" ]; then
	add_problem "③ 没有进程在听 $PORT"
else
	echo "监听：$LISTEN_PID"
	if [ "$ACTIVE" != "active" ]; then
		add_problem "④ $PORT 被**不是** $UNIT 的进程占着（单元是 $ACTIVE）"
		add_fix "...安装时换端口：sudo ./install.sh --ocr-port 8094"
	fi
fi

# ── 4. 冒烟请求 ──────────────────────────────────────────────────────
if [ -n "$LISTEN_PID" ]; then
	H="$(curl -s -m 10 "http://127.0.0.1:$PORT/health" 2>/dev/null || true)"
	if [ -z "$H" ]; then
		add_problem "⑤ $PORT 在听但 /health 无响应（服务可能正卡在解包/加载模型）"
	elif ! printf '%s' "$H" | grep -q '"ok": *true'; then
		add_problem "⑤ /health 未报 ok：$(printf '%s' "$H" | head -c 200)"
	elif ! printf '%s' "$H" | grep -q '"runtime_ok": *true'; then
		add_problem "⑥ runtime_ok=false —— 运行时目录坏了，重启即修（见下）"
		add_fix "sudo systemctl restart $UNIT"
	else
		echo "${C_OK}/health：ok${C_OFF}"
	fi
else
	add_problem "⑤ 解析服务没在跑，无法冒烟 /health"
fi

# ── 5. 死因取证：OOM / 解包目录被清 ──────────────────────────────────
echo
echo "最近日志（找死因：OOM / 缺 config.yaml / Exec format error）："
journalctl -u "$UNIT" -n 200 --no-pager 2>/dev/null \
	| grep -iE "oom|killed process|signal|No such file or directory|Exec format|_MEI|memory" \
	| tail -8 | sed "s/^/${C_DIM}  /;s/$/${C_OFF}/" \
	|| echo "  ${C_DIM}（没有命中关键字的行）${C_OFF}"

if journalctl -u "$UNIT" -n 400 --no-pager 2>/dev/null | grep -qiE "oom-kill|Killed process|out of memory"; then
	add_problem "⑦ 被 cgroup OOM 杀过（大页数扫描件内存峰值高，单元 MemoryMax=2G）"
	add_fix "...如反复 OOM：装新版（解析服务按 20 页回收引擎）+ 增加内存，或 sudo ./install.sh --ocr-port 8094 单机分服务"
fi

# ── 6. 结论 ──────────────────────────────────────────────────────────
hr
if [ "${#PROBLEMS[@]}" -eq 0 ]; then
	echo "${C_OK}解析服务健康，8093 正常。${C_OFF} 若主服务仍报 refused，说明它连的不是这个端口/单元："
	echo "  grep -i ocr /opt/skillforge/skillforge.env"
	exit 0
fi

echo "${C_ERR}发现 ${#PROBLEMS[@]} 个问题：${C_OFF}"
for p in "${PROBLEMS[@]}"; do echo "  $p"; done

echo
echo "建议按顺序执行（最省事的自愈路径）："
echo "  sudo systemctl reset-failed $UNIT 2>/dev/null; sudo systemctl restart $UNIT"
for f in "${FIXES[@]}"; do echo "  $f"; done
echo "  sudo systemctl daemon-reload && sudo systemctl reset-failed $UNIT && sudo systemctl restart $UNIT"
echo "  sleep 3; curl -s -m 10 http://127.0.0.1:$PORT/health"
echo
echo "根治：换 v260916.1905 之后的离线包（单元自带 Restart=always，崩了 3 秒自愈）。"
exit 1
