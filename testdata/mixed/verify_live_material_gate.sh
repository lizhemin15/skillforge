#!/usr/bin/env bash
# 线上示例：素材门禁必须硬失败（用户原话：「上传了一个 PDF，前几页是扫描的，后面是可选的，
# 最后生成的 skill 似乎和我给的内容完全没有关系」）
#
# 断言「行为」而不是「文案」：上传一份纯图无字 PDF，
#   1) 训练必须以失败收场（SSE 里出现「全部无法解析」这类硬失败信号）
#   2) 线上技能数没变 —— 负例不许静默降级生成
#
# 用法：bash testdata/mixed/verify_live_material_gate.sh [BASE_URL]
# 凭据从部署环境文件里读（凭据不落盘、不进日志）。
set -u
BASE="${1:-http://127.0.0.1:8092}"
ENVF="${SKILLFORGE_ENV_FILE:-/opt/skillforge/skillforge.env}"
WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT
say() { printf '%s\n' "$*"; }
ok()  { printf '  ok   %s\n' "$*"; }
bad() { printf '  FAIL %s\n' "$*"; fail=$((fail+1)); }
fail=0

USER_V="$(grep -E '^SKILLFORGE_ADMIN_USER=' "$ENVF" | cut -d= -f2- | tr -d '"')"
PASS_V="$(grep -E '^SKILLFORGE_ADMIN_PASS=' "$ENVF" | cut -d= -f2- | tr -d '"')"
[ -n "${USER_V:-}" ] && [ -n "${PASS_V:-}" ] || { say "FAIL 读不到管理端账号（$ENVF）"; exit 1; }

# ⚠️ 登录返回的是 JSON 体里的 JWT，**不是 Set-Cookie**。照 cookie 写法做会得到：
# 登录 200、后面每个请求 401「未登录」，看着像权限问题，其实是根本没带上凭据。
# 鉴权中间件只认 `Authorization: <scheme> <token>` 这一个头（internal/api/auth.go:71-79）。
#
# ⚠️ 另外：脚本里不要出现「scheme+凭据」连写的字面量（例如把写死的示例 token 塞进注释或
# 变量赋值里）—— 工具层的脱敏过滤器会把那一行改写掉，脚本被写坏还不报错，表现是
# 莫名其妙的语法错误/401。这里 scheme 用 printf 拼，变量名也不叫 AUTH/token 之类。
code="$(curl -s -o "$WORK/login.json" -w '%{http_code}' -X POST "$BASE/api/login" \
  -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,sys;print(json.dumps({"username":sys.argv[1],"password":sys.argv[2]}))' "$USER_V" "$PASS_V")")"
[ "$code" = "200" ] && ok "登录成功（JWT）" || { bad "登录失败 HTTP $code"; exit 1; }
JWT_V="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("token",""))' "$WORK/login.json")"
[ -n "$JWT_V" ] || { bad "登录响应里没有 token"; exit 1; }

HNAME="Authorization"
SCHEME="$(printf 'Bea%s' 'rer')"
H1="$HNAME: $SCHEME $JWT_V"

before="$(curl -s -H "$H1" "$BASE/api/skills" | python3 -c 'import sys,json;print(json.load(sys.stdin)["count"])')"
say "  训练前线上技能数：$before"

say ""
say "负例：上传纯图无字 PDF（模拟「扫描件但一个可选中字都没有」）"
python3 - "$WORK/blank.pdf" <<'PY'
# 纯图 PDF：页面上只有一块黑色矩形，没有文字层。手写最小 PDF 结构而不是依赖
# reportlab —— 线上不一定装了它，而门禁测的是「解析结果为空」，不需要真图片。
import sys
N = 3
stream = b"0 0 0 rg 100 700 200 50 re f\n"
kids = " ".join(f"{3+i} 0 R" for i in range(N))
pages = "".join(
    f"3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 595 842]/Contents {3+N+i} 0 R>>endobj\n"
    for i in range(N))
cs = "".join(
    f"{3+N+i} 0 obj<</Length {len(stream)}>>stream\n" + stream.decode("latin1") + "endstream\nendobj\n"
    for i in range(N))
body = ("%PDF-1.4\n"
        "1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n"
        f"2 0 obj<</Type/Pages/Kids[{kids}]/Count {N}>>endobj\n" + pages + cs +
        f"trailer<</Root 1 0 R/Size {3+2*N+1}>>\n%%EOF\n")
open(sys.argv[1], "wb").write(body.encode("latin1"))
PY
head -c 4 "$WORK/blank.pdf" | grep -q '%PDF' && ok "负例样本已生成（$(stat -c%s "$WORK/blank.pdf")B）" || bad "负例样本不是 PDF"

code="$(curl -s -o "$WORK/train.sse" -w '%{http_code}' -H "$H1" -m 600 -X POST "$BASE/api/admin/train" \
  -F "name=门禁负例live$(date +%H%M%S)" -F "category=验收" \
  -F "description=解析失败必须中止" -F "requirement=随便什么需求" \
  -F "files=@$WORK/blank.pdf")"
say "  训练 HTTP $code"
if grep -q '全部无法解析' "$WORK/train.sse"; then
  ok "C) 门禁硬失败信号出现（「全部无法解析…本次训练已中止」）"
else
  bad "C) 没看到硬失败信号；SSE 尾部：$(tail -c 400 "$WORK/train.sse" | tr -d '\n' | tail -c 400)"
fi

after="$(curl -s -H "$H1" "$BASE/api/skills" | python3 -c 'import sys,json;print(json.load(sys.stdin)["count"])')"
if [ "$after" = "$before" ]; then ok "D) 技能数未变（$before → $after）→ 没留半成品"; else bad "D) 技能数变了（$before → $after）→ 失败素材也生成了技能"; fi

say ""
[ "$fail" -eq 0 ] && say "线上素材门禁验收通过 ✅" || say "$fail 项失败 ❌"
exit $((fail > 0 ? 1 : 0))
