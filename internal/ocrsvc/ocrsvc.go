// Package ocrsvc 是「文档解析服务」（ocrd，默认 127.0.0.1:8093）这一**外部依赖**的
// 健康判定 + 错误翻译层。
//
// 为什么单独一层：ocrd 是独立 systemd 单元，主服务只是它的 HTTP 客户端。它没在跑时，
// Go 的 http 客户端只会吐一行底层错误：
//
//	Post "http://127.0.0.1:8093/extract": dial tcp 127.0.0.1:8093: connect: connection refused
//
// 用户看到的却是「训练技能报错」—— 既看不出这是**环境问题而不是他的文件有问题**，
// 也不知道下一步该敲什么命令。这类「底层错误直接穿透到界面」正是本轮投诉的形态。
// 本包把这些错误翻成「照着敲就能修」的中文说明，并把原始错误降级成附带信息。
//
// 设计约束（改动前先读）：
//   - 只翻译**连不上**（dial 级）错误。超时、解析失败、413 这些是文件/内容问题的信号，
//     硬塞一句「去重启服务」只会把人带偏。
//   - 原始错误必须仍然可达（errors.Is/As / Unwrap），否则排障时就没法判断到底是
//     refused 还是 DNS —— 我们只是不让它占据用户视野，不是把它丢掉。
//   - 消息里出现的地址/端口一律从入参 url 现取，不写死 8093：install.sh 支持改 OCR_PORT。
package ocrsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

const (
	// DefaultURL 是解析服务默认地址（与 systemd 单元、install.sh 的 OCR_PORT 默认值一致）。
	DefaultURL = "http://127.0.0.1:8093"
	// DefaultUnitSuffix 拼在服务名后面得到解析服务单元名：skillforge → skillforge-ocr。
	DefaultUnitSuffix = "-ocr"
	// ProbingService 是用于给出修复命令的默认服务名（install.sh --service 可改名）。
	ProbingService = "skillforge"
	// ProbeTimeout 是探活上限。3s 是刻意取的：真连不上时 refused 是毫秒级返回，
	// 真正要防的是「对端黑洞」——那种情况下用户宁可早点看到人话，也不要等默认 30 分钟的解析超时。
	ProbeTimeout = 3 * time.Second
)

// serviceError 把「给用户看的人话」和「给排障看的原始错误」分开：
// Error() 只吐人话，Unwrap() 保留底层错误供 errors.Is/As 判定。
type serviceError struct {
	msg   string
	cause error
}

func (e *serviceError) Error() string { return e.msg }
func (e *serviceError) Unwrap() error { return e.cause }

// Unreachable 判断错误是否为 dial 级失败（对端没在监听 / 路由不通 / 域名解析不了）。
// 这类错误与上传的文件内容无关，整机环境问题，因此值得给出「去修环境」的指引。
func Unreachable(err error) bool {
	if err == nil {
		return false
	}
	for _, target := range []error{
		syscall.ECONNREFUSED, syscall.EHOSTUNREACH, syscall.ENETUNREACH, syscall.ECONNRESET,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	// urllib 层面拿不到完整 URL 时的兜底：Go 的 *url.Error 里包着 *net.OpError。
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "dial" {
		return true
	}
	// 最后一道字符串兜底：不同内核/容器/内核参数的文案不完全一致，
	// 而这里判错方向的代价（少给人一条修复指引）远小于漏判（用户对着 refused 干瞪眼）。
	s := strings.ToLower(err.Error())
	for _, k := range []string{
		"connection refused", "no route to host", "network is unreachable",
		"no such host", "connection reset by peer", "connect: connection refused",
	} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// Explain 把 ocrd 调用错误翻成可执行的中文说明。
// 非 dial 级错误（超时、解析失败…）原样返回：那些不是「服务没运行」，加环境指引会误导。
func Explain(err error, svcURL string) error {
	if err == nil {
		return nil
	}
	if !Unreachable(err) {
		return err
	}
	where := Endpoint(svcURL)
	unit := UnitName()
	return &serviceError{
		msg: fmt.Sprintf(
			"文档解析服务连不上（%s）：本机解析服务没有在运行 —— 这是环境问题，不是你的文件有问题。\n"+
				"修复：在目标机执行 `systemctl restart %s`；若提示 unit not found，说明装的时候用了 --no-ocr"+
				"（没装解析服务），扫描件/Office 文档要它才能读出文字，需重装并启用。\n"+
				"排查：`systemctl status %s` / `journalctl -u %s -n 50`。\n"+
				"原始错误：%s",
			where, unit, unit, unit, firstLine(err.Error())),
		cause: err,
	}
}

// Endpoint 从解析服务地址里取出「主机:端口」，用于给用户指出到底是哪个地址连不上。
func Endpoint(svcURL string) string {
	s := strings.TrimSpace(svcURL)
	if s == "" {
		return ""
	}
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return u.Host
	}
	return strings.TrimRight(s, "/")
}

// UnitName 返回解析服务的 systemd 单元名（默认 skillforge-ocr）。
// 支持 SKILLFORGE_SERVICE_NAME 覆盖，这样换过 --service 的实例给出的命令也是对的。
func UnitName() string {
	if v := strings.TrimSpace(envServiceName()); v != "" {
		return v + DefaultUnitSuffix
	}
	return ProbingService + DefaultUnitSuffix
}

// Health 是一次探活的结果。
type Health struct {
	URL       string
	Running   bool // 服务是否在监听（HTTP 层可达）
	RuntimeOK bool // 服务自报的运行时是否健康（ocrd /health 的 runtime_ok）
	Version   string
	Err       error // Running=false 时的底层错误
}

// Detail 把探活结果翻成一行人话，供「上传解析」场景直接展示。
// 健康时返回空串（调用方无需展示任何东西）。
func (h Health) Detail() string {
	switch {
	case !h.Running:
		// 排查入口必须和 Explain 一样给全（status / journalctl）：线上取证时发现
		// 只给一条 restart 命令，用户遇到「restart 也不行」（比如 unit not found、
		// 起来又挂）就没下一步了。
		return fmt.Sprintf("⚠️ 文档解析服务没有在运行（%s 连不上）：在目标机执行 `systemctl restart %s` 即可修复；"+
			"若提示 unit not found，说明安装时未启用解析服务，需重装并启用（去掉 --no-ocr）。"+
			"排查：`systemctl status %s` / `journalctl -u %s -n 50`。",
			Endpoint(h.URL), UnitName(), UnitName(), UnitName())
	case !h.RuntimeOK:
		return fmt.Sprintf("⚠️ 文档解析服务在运行但运行时已损坏（%s）：它正在自动重启，约 10 秒后重试即可；"+
			"若持续如此，执行 `systemctl restart %s` 并在目标机看 `journalctl -u %s -n 50`。",
			Endpoint(h.URL), UnitName(), UnitName())
	}
	return ""
}

// Healthy 是「这份探活结果能不能支撑一次解析」的判定。
func (h Health) Healthy() bool { return h.Running && h.RuntimeOK }

// Check 探一次 /health。它永不返回错误——调用方要的是「能不能干活」这个结论 +
// 一句人话，探活本身的网络错误已折进 Health.Err / Health.Detail()。
//
// 注意两个不同的坏态（这是本轮修复的分水岭）：
//   - Running=false：进程没起来（refused）→ 依赖根本没装/没起；
//   - Running=true & RuntimeOK=false：进程活着但引擎坏了（PyInstaller 解包目录被清等）
//     → ocrd 侧守卫会自己退出让 systemd 拉起。
//
// 两者给用户的动作不同，所以不能只报一个 bool。
func Check(ctx context.Context, svcURL string) Health {
	h := Health{URL: svcURL}
	if strings.TrimSpace(svcURL) == "" {
		return h
	}
	reqCtx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, strings.TrimRight(svcURL, "/")+"/health", nil)
	if err != nil {
		h.Err = err
		return h
	}
	client := &http.Client{Timeout: ProbeTimeout}
	resp, err := client.Do(req)
	if err != nil {
		h.Err = err
		return h
	}
	defer resp.Body.Close()
	h.Running = true
	var body struct {
		OK        bool   `json:"ok"`
		RuntimeOK *bool  `json:"runtime_ok"`
		Version   string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		// 端口上有个不是 ocrd 的东西在回话：既不是「没运行」也不是「健康」，
		// 报 Running 但运行时未知 = 不可用，让调用方照「异常」处理。
		h.RuntimeOK = false
		return h
	}
	h.Version = body.Version
	if body.RuntimeOK != nil {
		h.RuntimeOK = *body.RuntimeOK
	} else {
		// 老版本 ocrd 没有 runtime_ok 字段：以 ok 为准。
		h.RuntimeOK = body.OK
	}
	return h
}

// SelfHealing 判断 /extract 的响应体是不是「服务自报运行时损坏、正在自动重启」。
// ocrd 这种回包长这样（200 + ok:false）：
//
//	{"ok":false,"error":"<RUNTIME_LOST_MSG>","runtime_ok":false,"self_healing":true}
//
// 它与「这份文件解析不了」是两码事：文件没问题，重试就会成功。
// 不识别它的话，用户看到的是「解析服务: <内部错误文案>」，然后再也不试了。
func SelfHealing(body []byte) bool {
	var probe struct {
		SelfHealing bool `json:"self_healing"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return probe.SelfHealing
}

// SelfHealingMessage 是「服务自愈中」时给用户的一句话。
func SelfHealingMessage(svcURL, raw string) string {
	return fmt.Sprintf("文档解析服务刚重启过（运行时被破坏，已自愈）：%s。约 10 秒后重试即可，"+
		"文件本身没有问题。原始信息：%s", Endpoint(svcURL), firstLine(raw))
}

// firstLine 只取第一行：底层错误常带多行 context，铺进界面会把关键信息顶掉。
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// envServiceName 读「本实例的服务名」，供修复命令拼出正确的单元名。
// install.sh 会把它写进实例 env（多实例机器上默认名可能是错的）。
func envServiceName() string { return os.Getenv("SKILLFORGE_SERVICE_NAME") }
