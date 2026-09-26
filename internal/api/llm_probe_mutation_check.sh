#!/usr/bin/env bash
# 「LLM 连通性测试按钮」的断言自证脚本（2026-09-26）。
#
# 为什么必须有：internal/llm/probe_test.go 与 internal/api/llm_probe_test.go 全绿，
# 只证明「今天它测得出通不通」，证明不了「哪天有人把地址归一化的剥尾巴删了 / 把空正文
# 判成失败 / 把 10404 的归因合并掉 / 把掩码回退摘了，会被抓住」。
#
# 这个按钮的价值全在**结论的可信度**上，而它翻车的样子是用户看得见的：
#   · 报的地址和实际打出去的地址不一致 → 「测试通过、聊天打不通」，用户从此不再信它
#   · 把「200 但正文空」判成不通（内网慢模型的常见长相）→ 整排假红 → 真坏的配置被淹掉
#   · 把 400+10404 合并进泛化 400 → 用户不知道该去控制台换路由名，只能反复试
#   · 掩码不回退 → 列表点「测试」永远 401，看着像服务坏了，其实是界面把掩码当 key 发了
#
# 五条注入分别打在这五种坏法上，每条都必须红在**指定**的断言上。
#
# 用法：bash internal/api/llm_probe_mutation_check.sh
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

FASTJSON=internal/llm/fastjson.go
PROBE=internal/llm/probe.go
APIPROBE=internal/api/llm_probe.go
BAK_DIR="$(mktemp -d)"
cp "$FASTJSON" "$BAK_DIR/fastjson.go"
cp "$PROBE" "$BAK_DIR/probe.go"
cp "$APIPROBE" "$BAK_DIR/api_probe.go"

# restore 只还原、**不删备份**（备份删了第二次还原就是空操作，注入态会一路带到下一条），
# 也**不用 git checkout** —— 出货文件上可能压着未提交的人工改动，git 还原会把它一起抹掉。
restore() {
  cp "$BAK_DIR/fastjson.go" "$FASTJSON"
  cp "$BAK_DIR/probe.go" "$PROBE"
  cp "$BAK_DIR/api_probe.go" "$APIPROBE"
}
cleanup() { restore; rm -rf "$BAK_DIR"; }
trap cleanup EXIT

fails=0
CASE_RE='TestNormalizeBaseURL|TestChatEndpoint|TestProbeEndpoint|TestProbeEmptyContent|TestProbeUpstreamErrors|TestProbeLocalPrecheck|TestProbeTimeout|TestProbeNeverLeaksKey|TestProbeLLM'
run_test() {
  "$GO_BIN" test ./internal/llm/ ./internal/api/ -run "$CASE_RE" -count=1 2>&1
}

# ---------- 基线：不注入时必须全绿 ----------
if ! out="$(run_test)"; then
  echo "基线就是红的，先修好再来做注入自证："
  echo "$out" | tail -20
  exit 1
fi
echo "基线：全绿 ✓"

# 注入：$1=故障说明 $2=被改的文件 $3=锚点 $4=替换文本 $5=应变红的断言（文案片段）
inject_case() {
  local desc="$1" file="$2" old="$3" new="$4" expect="$5"
  python3 - "$file" "$old" "$new" <<'PY'
import sys
path, old, new = sys.argv[1], sys.argv[2], sys.argv[3]
s = open(path, encoding='utf-8').read()
n = s.count(old)
if n != 1:
    sys.exit(f"注入失败：锚点在 {path} 里命中 {n} 次（应为 1 次）—— 出货文件改了，"
             f"请同步更新本脚本的锚点，别让自证脚本变成永远绿的摆设")
open(path, 'w', encoding='utf-8').write(s.replace(old, new))
PY
  if [ $? -ne 0 ]; then echo "✗ [$desc] 注入失败"; fails=$((fails + 1)); return; fi

  local out rc
  out="$(run_test)"; rc=$?
  if [ $rc -eq 0 ]; then
    echo "✗ [$desc] 注入后测试仍然全绿 —— 断言是假的（抓不住这个故障）"
    fails=$((fails + 1))
  elif grep -q 'build failed\|\[build failed\]\|cannot use\|undefined:' <<<"$out"; then
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
echo "注入自证（每条都必须变红）"

# 注入 1 = 地址归一化退回旧实现（不剥「完整端点」的尾巴）。
# 这是修掉的真 bug：控制台给的整条 https://…/v2/chat/completions 会被拼成
# …/chat/completions/v1/chat/completions，运行期真在打这个畸形地址。
# 抓它的判据同时也是「按钮不说谎」的守卫：httptest 收到的路径必须正好一层。
inject_case '地址归一化不剥尾巴（完整端点被拼出双份 /chat/completions）' \
  "$FASTJSON" \
  '	for _, tail := range []string{"/chat/completions", "/completions"} {
		if strings.HasSuffix(base, tail) {
			base = strings.TrimRight(strings.TrimSuffix(base, tail), "/")
			break
		}
	}' \
  '	// （注入：不剥尾巴）' \
  '实际请求路径'

# 注入 2 = 把「200 但正文空」判成不通。内网思考型模型的常见长相，
# 判红就会整排假红 —— 用户把「配置是好的、按钮说它坏了」当成按钮坏了。
inject_case '空正文被判成不通（内网慢模型整排假红）' \
  "$PROBE" \
  '	if res.Reply == "" {
		res.Message = fmt.Sprintf("连通正常（%dms）：上游 200' \
  '	if res.Reply == "" {
		res.OK = false
		res.Message = fmt.Sprintf("连通正常（%dms）：上游 200' \
  'HTTP 200 就该判连通'

# 注入 3 = 归因表把「模型名不是路由名」合并进泛化 400。
# 用户拿到的会是一句没用的「请求不合法」，而不是「去控制台复制路由名」。
inject_case '10404 归因被合并（用户不知道要换路由名）' \
  "$PROBE" \
  '	case res.Status == http.StatusBadRequest && isRouteMissingErr(res.APICode, upstream):' \
  '	case false && isRouteMissingErr(res.APICode, upstream):' \
  '归因里应出现'

# 注入 4 = 密钥擦洗被摘。回执要显示在管理端、还会被复制去问客服。
inject_case '回执不擦洗 key（网关抄请求头时漏出去）' \
  "$PROBE" \
  '	if c != nil && c.cfg != nil {
		res.Message = scrubSecret(res.Message, c.cfg.APIKey)
		res.Reply = scrubSecret(res.Reply, c.cfg.APIKey)
	}' \
  '	// （注入：不擦洗）' \
  '回执里出现了 key 原文'

# 注入 5 = 掩码回退被摘。「列表点测试」走的正是这条路：表单里是掩码，
# 不回退就必然 401 —— 用户看到的是「服务坏了」，其实是界面把掩码当 key 发了。
inject_case '掩码不回退用库里的 key（列表点测试永远 401）' \
  "$APIPROBE" \
  '			if isMaskedKey(cfg.APIKey) {
				cfg.APIKey = stored.APIKey
			}' \
  '			// （注入：掩码不回退）' \
  '掩码必须回退成库里的真 key'

echo
if [ "$fails" -eq 0 ]; then
  echo "自证通过：5/5 条注入都被预期断言抓住，且还原后回绿。"
else
  echo "自证失败：$fails 条注入没被抓住（这些断言现在没有判别力）。"
fi

# 还原必须逐字节回到原样：否则脚本本身会把注入态留在出货文件里（比不跑更糟）。
before="$(md5sum "$BAK_DIR/fastjson.go" "$BAK_DIR/probe.go" "$BAK_DIR/api_probe.go" | awk '{print $1}' | tr '\n' ' ')"
after="$(md5sum "$FASTJSON" "$PROBE" "$APIPROBE" | awk '{print $1}' | tr '\n' ' ')"
if [ "$before" != "$after" ]; then
  echo "✗ 还原后 md5 不一致：$before vs $after —— 出货文件被留在改动状态！"
  exit 1
fi
echo "还原校验：逐字节一致 ✓"
[ "$fails" -eq 0 ] || exit 1
