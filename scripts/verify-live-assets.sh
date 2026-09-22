#!/usr/bin/env bash
# 线上静态资源「同一批」复验：二进制换了，前端资源有没有跟着换。
#
# 为什么写这个：部署流水线只替换二进制，前端 JS/CSS 由二进制内嵌（go:embed）出货。
# 一旦改的是 web/ 却漏了 embed/版本号，会出现「后端新、前端旧」——功能静默不存在，
# 而 HTTP 仍 200，肉眼看不出来。
#
# 关键纪律（踩过一次假红）：
#   资源路径**必须从服务端返回的 index.html 里现取**，不许写死。
#   首版写死了 /js/chat.js（真实路径是 /assets/js/chat.js），拿到 19 字节的
#   404 页面，于是报「静态资源没换」——一次纯粹的假红，差点当成真故障去修。
#
# 用法：
#   scripts/verify-live-assets.sh ctk-mat ch-scroll
#   BASE=http://127.0.0.1:8092 scripts/verify-live-assets.sh '某某特征串'
# 退出码：0 全绿 / 1 有 FAIL / 2 前提不成立（首页拿不到、没解析出任何资源）
set -uo pipefail

BASE="${BASE:-http://127.0.0.1:8092}"
ASSET_RE='(src|href)="[^"]+\.(js|css)[^"]*"'

HTML="$(curl -s -m 10 "$BASE/" || true)"
if [ -z "$HTML" ] || ! printf '%s' "$HTML" | grep -qE "$ASSET_RE"; then
  echo "PREMISE_MISS: 首页拿不到或没有任何 js/css 引用（BASE=$BASE）"
  exit 2
fi

mapfile -t ASSETS < <(printf '%s' "$HTML" | grep -oE "$ASSET_RE" \
  | sed -E 's/^(src|href)="//; s/"$//' | sort -u)

if [ "${#ASSETS[@]}" -eq 0 ]; then
  echo "PREMISE_MISS: 解析出的静态资源数为 0"
  exit 2
fi

FAIL=0
declare -A BODY
for a in "${ASSETS[@]}"; do
  code="$(curl -s -o /tmp/.vla_body -w '%{http_code}' -m 20 "$BASE$a")"
  sz="$(wc -c < /tmp/.vla_body | tr -d ' ')"
  md="$(md5sum /tmp/.vla_body | cut -c1-8)"
  printf '  %-42s HTTP=%s bytes=%-7s md5=%s\n' "$a" "$code" "$sz" "$md"
  if [ "$code" != "200" ] || [ "$sz" -lt 1000 ]; then
    echo "FAIL: 资源取不到或不完整：$a（HTTP=$code bytes=$sz）"
    FAIL=1
  fi
  BODY["$a"]="$(cat /tmp/.vla_body)"
done

# 特征串检查：每个待查串必须命中**至少一个**资源（默认查 chat.js 那条链路）
for want in "$@"; do
  hit=0
  for a in "${ASSETS[@]}"; do
    case "$a" in *chat.js*) ;; *) continue ;; esac
    printf '%s' "${BODY[$a]}" | grep -qF -- "$want" && hit=1
  done
  if [ "$hit" = "1" ]; then
    echo "  ✓ 前端特征串命中：$want"
  else
    echo "FAIL: 线上前端资源缺特征串「$want」—— 二进制换了但前端资源没换（或改动没进 embed）"
    FAIL=1
  fi
done

if [ "$FAIL" != "0" ]; then
  echo "ASSETS_RED"
  exit 1
fi
echo "ASSETS_OK 资源数=${#ASSETS[@]} 特征串=$*"
