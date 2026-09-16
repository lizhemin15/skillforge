// Package tlsconf 处理离线内网部署里的 https 信任问题。
//
// 由来（客户现场必撞的一堵墙）：内网的模型网关 / 代理通常是自签证书或企业内网 CA 签的，
// 而 SkillForge 装在一台干净的离线机器上（系统里甚至可能连 ca-certificates 都没有）。
// 于是任何 https 调用都以 `x509: certificate signed by unknown authority` 收场，
// 客户看到的是一句英文 Go 错误 —— 既不知道是证书问题，更不知道往哪放证书。
//
// 这个包做三件事：
//
//  1. 让配置能生效：SKILLFORGE_CA_BUNDLE 指定的 CA（PEM/DER，文件或目录，可用
//     `:`/`,` 分隔多个）真正参与证书验证。之前这个变量在全仓零消费者 ——
//     客户按文档配了也白配，这是最坏的一种「文档说了、程序没做」。
//  2. 把失败翻译成人话 + 具体修复命令（与 internal/ocrsvc 同一套哲学：
//     中文结论 → 修复 → 自查命令 → 原始错误原文保留）。
//  3. 给 `-selftest` 一个可判定的「TLS 信任」检查项：信任库到底装好了没有、
//     指定地址的证书到底信不信得过，装完当场就能验，而不是等客户跑训练时才炸。
//
// 边界：不做任何联网动作（除被显式要求探测的那个地址），不修改系统信任库，
// 不打印任何密钥/凭据。
package tlsconf

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// EnvVarCA 指向自定义 CA：单个文件 / 目录 / 多个路径（`:` 或 `,` 分隔）。
	// 值与 SKILLFORGE_* 其它配置一样由 systemd 从 <prefix>/skillforge.env 注入。
	EnvVarCA = "SKILLFORGE_CA_BUNDLE"

	// EnvVarInsecure 显式关闭证书校验（写 1/true/yes）。仅当客户清楚风险时使用：
	// 关掉之后中间人无法被发现。故意不提供命令行开关——命令行开关太容易被顺手打开。
	EnvVarInsecure = "SKILLFORGE_INSECURE_SKIP_VERIFY"

	// EnvVarProbe 是可选的探测地址：给一个 https 地址，自检就真握手验一次证书。
	// 典型用法：填内网模型网关（https://gw.corp/v1），装完立刻知道证书认不认。
	EnvVarProbe = "SKILLFORGE_TLS_PROBE_URL"

	// probeTimeout 单次握手预算：局域网内足够，且自检不能被一个坏地址挂死。
	probeTimeout = 8 * time.Second
)

// FileStatus 报告一个 CA 来源文件的加载结果。Certs 为 0 表示「文件在，但里面没有
// 可用证书」——这通常是客户给错了文件（比如给了 DER 编码的服务端证书、或给了个空文件）。
type FileStatus struct {
	Path  string
	Certs int
	Err   string
}

// Status 是一次加载的完整结论，供自检/日志展示。
type Status struct {
	// BundlePath 是 EnvVarCA 的原始值（用于告诉客户「你配的是这个」）。不回显其它环境变量。
	BundlePath string
	Files      []FileStatus
	// BundleCerts 是从自定义 CA 里读到的证书总数。
	BundleCerts int
	// SystemCerts 是系统根证书数量。0 = 本机信任库为空，任何 https 都会失败。
	SystemCerts int
	// SystemOK 报告系统信任库是否可用。为什么不用 SystemCerts>0 判定：Go 的
	// Subjects() 在个别平台上取不到数量，把「数不出来」当成「没有证书」会给出
	// 一条假的失败项——客户于是去修一个本来没坏的东西。宁可标注未知，也不冤枉机器。
	SystemOK bool
	// SystemNote 说明系统根池的状况（人话，空 = 一切正常）。
	SystemNote string
	Insecure   bool
	// Err 非空 = 自定义 CA 配置无效（文件不存在/读不动/解析不出证书）。
	// 这是真故障：客户以为配好了，实际上一个证书都没装上。
	Err error
}

// Configured 报告客户是否配了自定义 CA（用于区分「没配」和「配错了」）。
func (s Status) Configured() bool { return strings.TrimSpace(s.BundlePath) != "" }

// TrustSummary 一句话说明这次验证用的是哪些根：给自检和报错收尾用。
func (s Status) TrustSummary() string {
	var parts []string
	if s.BundleCerts > 0 {
		parts = append(parts, fmt.Sprintf("自定义 CA %d 份（%s）", s.BundleCerts, s.BundlePath))
	}
	if s.SystemCerts > 0 {
		parts = append(parts, fmt.Sprintf("系统根证书 %d 份", s.SystemCerts))
	} else if s.SystemOK {
		parts = append(parts, "系统根证书库可用（数量未取到）")
	}
	if len(parts) == 0 {
		return "没有任何可用的根证书"
	}
	return "信任来源：" + strings.Join(parts, " + ")
}

// 进程级缓存。**按「信任配置指纹」失效，不是「一次加载就永远用」**。
//
// 为什么不是 sync.Once：客户把证书文件**换掉**（重签、续期、换网关）时，
// 服务不会自动重启，Once 会让进程抱着旧证书池一辈子 —— 症状是
// 「证书明明是新的，还是报 unknown authority」，而客户会去怀疑证书本身。
// 指纹里带上文件大小与 mtime，于是「原地覆盖证书文件」也能被感知。
//
// 指纹只是一两个 os.Stat，比一次 HTTP 往返便宜几个数量级，放在请求路径上没有负担。
var (
	mu             sync.Mutex
	cachedKey      string
	cached         Status
	pool           *x509.CertPool
	builtTransport *http.Transport
)

// trustFingerprint 是「当前信任配置 + CA 文件现状」的指纹。
// env 变了、文件增删改了，指纹就变，缓存随之作废。
func trustFingerprint() string {
	var b strings.Builder
	b.WriteString(EnvVarCA + "=" + strings.TrimSpace(os.Getenv(EnvVarCA)) + "\n")
	b.WriteString(EnvVarInsecure + "=" + strings.TrimSpace(os.Getenv(EnvVarInsecure)) + "\n")
	for _, raw := range splitList(strings.TrimSpace(os.Getenv(EnvVarCA))) {
		files, err := expand(raw)
		if err != nil {
			// 路径不存在也算指纹的一部分：客户把文件放上去之后要能被感知。
			fmt.Fprintf(&b, "%s|err:%s\n", raw, firstLine(err.Error()))
			continue
		}
		for _, f := range files {
			fi, err := os.Stat(f)
			if err != nil {
				fmt.Fprintf(&b, "%s|err:%s\n", f, firstLine(err.Error()))
				continue
			}
			fmt.Fprintf(&b, "%s|%d|%d\n", f, fi.Size(), fi.ModTime().UnixNano())
		}
	}
	return b.String()
}

// state 返回「当前配置下的」结论与证书池，需要时重新加载。
func state() (Status, *x509.CertPool) {
	key := trustFingerprint()
	mu.Lock()
	defer mu.Unlock()
	if key != cachedKey {
		cached, pool = loadFresh()
		cachedKey = key
		// transport 是从证书池克隆出来的产物：池换了，transport 必须跟着换，
		// 否则会出现「Load() 说证书装上了，请求却还在用旧池」的自相矛盾。
		builtTransport = nil
	}
	return cached, pool
}

// resetForTests 丢弃缓存，供测试在改 env / 改文件后立刻重新加载。
// 生产路径不需要它——指纹会自动感知变化。
func resetForTests() {
	mu.Lock()
	defer mu.Unlock()
	cachedKey = ""
	cached = Status{}
	pool = nil
	builtTransport = nil
}

// Load 加载 CA 配置并返回结论（配置未变时走缓存）。
func Load() Status {
	st, _ := state()
	return st
}

// Pool 返回自定义 CA 池；nil 表示「没有自定义 CA，按系统默认信任库验证」。
// 传 nil 给 tls.Config.RootCAs 正是 Go 的默认语义，所以不需要额外的分支。
func Pool() *x509.CertPool {
	_, pool := state()
	return pool
}

// InsecureSkipVerify 报告客户是否显式要求跳过证书校验。
func InsecureSkipVerify() bool { return isTruthy(os.Getenv(EnvVarInsecure)) }

// NewClient 造一个使用本机信任配置的 http.Client。
//
// timeout=0 表示不设整体超时（流式长连接要这样，靠 ctx 收尾）；>0 用于普通请求。
//
// 每次调用只包一层结构体：Transport 自身在 buildTransport 里缓存，
// 所以「每请求造一个 client」不会反复克隆 transport。
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: Transport()}
}

// Wrap 给已有的 client 换上本机信任配置（保留它的 Timeout / CheckRedirect 等）。
// 给 tools 里的抓取客户端用：那些地方有 SSRF 逐跳复检逻辑，不能整个换掉。
//
// 注意：必须在 client 第一次发请求之前调用——Transport 在首次请求后不可替换。
func Wrap(cli *http.Client) *http.Client {
	if cli == nil {
		cli = &http.Client{}
	}
	cli.Transport = Transport()
	return cli
}

// Transport 返回带本机信任配置的 transport。
//
// 基于 DefaultTransport 克隆：保留 HTTP/2、代理环境变量（HTTPS_PROXY）等默认行为，
// 只改 TLS 校验这一处。内网出口走正向代理的机器正依赖这些默认行为。
//
// ⚠️ 调用时机：不要在包级变量初始化（var x = tlsconf.Transport()）里调用。
// 包级变量在 main() 之前求值，那时实例的 env 文件还没读进来，会造出一个
// 「没装 CA」的 transport 并被缓存 —— 症状是「配了证书也不生效」，
// 而客户会以为证书本身有问题。放在请求路径上调用（延迟到 main 之后）才安全。
func Transport() *http.Transport {
	st, p := state() // 先确保缓存与当前配置一致（必要时会清掉旧 transport）
	mu.Lock()
	if t := builtTransport; t != nil {
		mu.Unlock()
		return t
	}
	mu.Unlock()

	// 构建过程**不持锁**：buildTransport 会去读配置，持锁调用会自锁。
	// 并发下可能重复建一次，但只有一个结果会被采用，重复建设没有副作用。
	t := buildTransport(st, p)
	mu.Lock()
	if builtTransport == nil {
		builtTransport = t
	}
	out := builtTransport
	mu.Unlock()
	return out
}

// buildTransport 造一份新的 transport。入参是**已加载**的结论与证书池，
// 不在这里回头去读缓存（读缓存会绕回 state()，在持锁路径上就是自锁）。
func buildTransport(st Status, p *x509.CertPool) *http.Transport {
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// 理论上不会发生；真发生了也不能返回一个不做校验的 transport。
		tr = &http.Transport{}
	}
	out := tr.Clone()
	if out.TLSClientConfig == nil {
		out.TLSClientConfig = &tls.Config{}
	} else {
		out.TLSClientConfig = out.TLSClientConfig.Clone()
	}
	if p != nil {
		out.TLSClientConfig.RootCAs = p
	}
	if st.Insecure {
		out.TLSClientConfig.InsecureSkipVerify = true
	}
	return out
}

// loadFresh 真正干活的加载逻辑（Load 缓存它）。
func loadFresh() (Status, *x509.CertPool) {
	st := Status{BundlePath: strings.TrimSpace(os.Getenv(EnvVarCA)), Insecure: InsecureSkipVerify()}

	// 系统根池：先读一次，把结论留下来。读不到不是「没配」——是这台机器装得太素，
	// 客户需要知道这一点（否则他会以为是内网证书有问题，白折腾 CA）。
	if sys, err := x509.SystemCertPool(); err != nil {
		st.SystemNote = fmt.Sprintf("读系统根证书库失败（%s）：这台机器可能没装 ca-certificates，"+
			"所有 https 都会无法验证。", firstLine(err.Error()))
	} else if sys != nil {
		st.SystemCerts = countCerts(sys)
		st.SystemOK = st.SystemCerts > 0
		if !st.SystemOK {
			// 数量取不到时用「系统 CA 文件在不在」兜底判断，别把未知说成空。
			if f, size := systemRootsEvidence(); f != "" {
				st.SystemOK = true
				st.SystemNote = fmt.Sprintf("系统根证书数量没数出来，但 %s 在（%d 字节），按可用处理", f, size)
			}
		}
	}

	if st.BundlePath == "" {
		return st, nil
	}

	p := x509.NewCertPool()
	for _, raw := range splitList(st.BundlePath) {
		files, err := expand(raw)
		if err != nil {
			st.Err = fmt.Errorf("自定义 CA 路径 %s 读不了：%s", raw, firstLine(err.Error()))
			return st, nil
		}
		for _, f := range files {
			n, err := appendFile(p, f)
			fs := FileStatus{Path: f, Certs: n}
			if err != nil {
				fs.Err = firstLine(err.Error())
			}
			st.Files = append(st.Files, fs)
			st.BundleCerts += n
		}
	}
	if st.BundleCerts == 0 {
		// 最费客户时间的一种失败：env 配了、文件也在，但证书没读进去。
		st.Err = fmt.Errorf("%s 里没有读到任何可用证书：路径 %s（看了 %d 个文件）。"+
			"常见原因：给的是服务端证书（自签也行但要给签发它的 CA）、DER 二进制格式、或空文件",
			EnvVarCA, st.BundlePath, len(st.Files))
		return st, nil
	}
	return st, p
}

// expand 把一个 CA 路径展开成文件列表：目录 → 目录下的普通文件；文件 → 它自己。
func expand(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	ents, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		out = append(out, filepath.Join(path, e.Name()))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("目录里没有文件")
	}
	return out, nil
}

// appendFile 把一个文件里的证书追加进池子，返回实际加进去的份数。
//
// 同时吃 PEM 和 DER：客户从 Windows 导出的 .cer 常是 DER 二进制，
// 只认 PEM 的话他会得到「文件在但一个证书都没有」，然后开始怀疑人生。
func appendFile(p *x509.CertPool, path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	n := 0
	rest := raw
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if !strings.Contains(blk.Type, "CERTIFICATE") {
			continue
		}
		if cert, err := x509.ParseCertificate(blk.Bytes); err == nil {
			p.AddCert(cert)
			n++
		}
	}
	if n > 0 {
		return n, nil
	}
	// 退回 DER：整份文件就是一个证书。
	if cert, err := x509.ParseCertificate(raw); err == nil {
		p.AddCert(cert)
		return 1, nil
	}
	return 0, errors.New("没有 PEM 证书块，也不是 DER 证书")
}

// countCerts 数一下根池里的证书（x509.CertPool 只在 Go 1.22+ 暴露 Subjects()，
// 这里用它；拿不到就当 0，不影响判定）。
func countCerts(p *x509.CertPool) int { return len(p.Subjects()) }

// systemRootsEvidence 检查「这台机器上常见的系统 CA 文件」有没有、非不非空。
// 只在数量数不出来时兜底用：用它把「未知」和「空」分开。
func systemRootsEvidence() (string, int64) {
	for _, p := range []string{
		"/etc/ssl/certs/ca-certificates.crt", // Debian/Ubuntu
		"/etc/pki/tls/certs/ca-bundle.crt",   // RHEL/CentOS
		"/etc/ssl/ca-bundle.pem",             // SUSE
		"/etc/ssl/cert.pem",                  // Alpine/其它
	} {
		if fi, err := os.Stat(p); err == nil && fi.Size() > 0 {
			return p, fi.Size()
		}
	}
	return "", 0
}

// ── 错误翻译 ─────────────────────────────────────────────────────────────

// tlsError 包一层人话，Error() 返回翻译后的完整文案（含原始错误），Unwrap 保留原始错误。
// 与 ocrsvc.serviceError 同一个模式：上层拿到的还是原始错误的身份，可继续 errors.Is/As，
// 但给人的文案已经是中文的修复指引。
type tlsError struct {
	msg   string
	cause error
}

func (e *tlsError) Error() string { return e.msg }
func (e *tlsError) Unwrap() error { return e.cause }

// LooksLikeTLS 报告这个错误是不是「证书/加密握手」类问题。
func LooksLikeTLS(err error) bool {
	if err == nil {
		return false
	}
	var (
		ua x509.UnknownAuthorityError
		sr x509.SystemRootsError
		hn x509.HostnameError
		ci x509.CertificateInvalidError
		cv *tls.CertificateVerificationError
		rh tls.RecordHeaderError
	)
	if errors.As(err, &ua) || errors.As(err, &sr) || errors.As(err, &hn) ||
		errors.As(err, &ci) || errors.As(err, &cv) || errors.As(err, &rh) {
		return true
	}
	// 文案兜底：有些网关/代理把 TLS 失败包成普通文本错误，结构化信息进不了类型里。
	// 这一路宁可宽一点：多给一段人话没有代价，漏给代价是客户在英文报错前发呆。
	s := err.Error()
	for _, k := range append([]string{"x509:", "tls:", "certificate", "certificate signed by unknown authority"},
		schemeMismatchMarkers...) {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// schemeMismatchMarkers：「地址 scheme 和端口对不上」这类错误的文案特征。
//
// 这是内网部署的高频手误：把网关写成 http://（其实它只开 https），或者反过来。
// Go 的报错是 `http: server gave HTTP response to HTTPS client` /
// `first record does not look like a TLS handshake` —— 两句都看不出问题出在哪，
// 客户的第一反应是「证书坏了」，然后开始折腾证书。
var schemeMismatchMarkers = []string{
	"server gave HTTP response to HTTPS client",
	"first record does not look like a TLS handshake",
	"HTTP response to HTTPS",
}

func isSchemeMismatch(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, k := range schemeMismatchMarkers {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// Explain 把 TLS 类错误翻译成「人话结论 + 修复命令 + 自查命令 + 原始错误」。
// 不是 TLS 类错误时原样返回（不要给不相关的失败套上证书的帽子）。
func Explain(err error, endpoint string) error {
	if err == nil || !LooksLikeTLS(err) {
		return err
	}
	st := Load()
	where := endpoint
	if where == "" {
		where = "目标地址"
	}
	raw := firstLine(err.Error())
	if isSchemeMismatch(err) {
		return &tlsError{cause: err, msg: fmt.Sprintf(
			"地址协议和端口对不上（%s）：配置里用了 https，但对端那个端口说的是明文 http（或者反过来）。"+
				"这不是证书问题——证书改多少遍都不会好。\n"+
				"修复：按服务端实际协议改地址——只开 http 就写 `http://主机:端口`，"+
				"只开 https 就写 `https://主机:端口`（端口别抄错，80/443 与 8080/8443 常被混）。\n"+
				"自查：`curl -v http://主机:端口` 与 `curl -k https://主机:端口` 各跑一次，"+
				"哪条通就说明对端是哪个协议。\n"+
				"原始错误：%s", where, raw)}
	}
	var (
		ua x509.UnknownAuthorityError
		sr x509.SystemRootsError
		hn x509.HostnameError
		ci x509.CertificateInvalidError
	)
	switch {
	case errors.As(err, &sr):
		return &tlsError{cause: err, msg: fmt.Sprintf(
			"TLS 握手失败（%s）：本机**没有可用的系统根证书库**，所以任何 https 都验不过。\n"+
				"修复：装系统证书包（`yum install ca-certificates` / `apt install ca-certificates`，"+
				"离线机器从系统光盘装），或直接指定一个 CA 文件：在 <实例目录>/skillforge.env 里加\n"+
				"      %s=/path/to/ca-bundle.pem\n"+
				"      改完 `systemctl restart %s`。\n"+
				"自查：`skillforge -selftest` 的「TLS 信任」一项；`ls /etc/ssl/certs/ca-certificates.crt`。\n"+
				"原始错误：%s", where, EnvVarCA, serviceName(), raw)}

	case errors.As(err, &ua):
		more := fmt.Sprintf("当前 %s 未设置。", EnvVarCA)
		if st.Configured() {
			if st.Err != nil {
				more = fmt.Sprintf("当前 %s=%s，但它没能装上：%s", EnvVarCA, st.BundlePath, st.Err.Error())
			} else {
				more = fmt.Sprintf("当前 %s=%s 已加载 %d 份证书，但里面没有签这个地址的 CA —— "+
					"检查是不是给错了 CA（要签发**这个地址证书**的那个 CA，不是别的系统的）。",
					EnvVarCA, st.BundlePath, st.BundleCerts)
			}
		}
		return &tlsError{cause: err, msg: fmt.Sprintf(
			"TLS 证书不被信任（%s）：对端用的是自签证书或企业内网 CA 签的证书，本机信任库不认它。"+
				"这是环境问题，不是你的模型配置错了。\n"+
				"修复：把签发该证书的 CA 放到这台机器上，然后在 <实例目录>/skillforge.env 里加一行\n"+
				"      %s=/path/to/ca.pem\n"+
				"      改完 `systemctl restart %s`。（%s）\n"+
				"自查：`openssl s_client -connect %s -showcerts </dev/null` 看服务端链，"+
				"`openssl x509 -in ca.pem -noout -subject` 确认给的是 CA 证书；"+
				"`curl -v %s` 应报同样的错误。%s\n"+
				"原始错误：%s", where, EnvVarCA, serviceName(), more, hostPort(where), where, st.TrustSummary(), raw)}

	case errors.As(err, &hn):
		return &tlsError{cause: err, msg: fmt.Sprintf(
			"TLS 证书名字不匹配（%s）：证书是有效的，但证书上的域名/地址和你访问的不一样。\n"+
				"修复：把配置里的地址改成证书上的那个名字（例如 https://网关域名/v1 而不是 https://IP/v1），"+
				"或让运维给证书补上这个 IP/域名的 SAN。\n"+
				"自查：`openssl x509 -in server.crt -noout -text | grep -A2 'Subject Alternative Name'`。\n"+
				"原始错误：%s", where, raw)}

	case errors.As(err, &ci):
		return &tlsError{cause: err, msg: fmt.Sprintf(
			"TLS 证书无效（%s）：%s\n"+
				"修复：联系证书签发方换证书（过期就续签；时间不对就校准本机时钟"+
				"`timedatectl set-ntp true` 或手动 `date -s`）。\n"+
				"自查：`openssl x509 -in server.crt -noout -dates -text`；`date`（本机时间）。\n"+
				"原始错误：%s", where, certInvalidReason(ci), raw)}
	}

	// 兜底：命中 LooksLikeTLS 但没对上具体类型（代理转手过的自定义文案最常见）。
	return &tlsError{cause: err, msg: fmt.Sprintf(
		"TLS 握手失败（%s）。%s\n"+
			"排查：`curl -v %s` 复现同一错误；若是自签/内网 CA，把签发它的 CA 配到 %s 再 "+
			"`systemctl restart %s`；`skillforge -selftest` 的「TLS 信任」一项给出具体结论。\n"+
			"原始错误：%s", where, st.TrustSummary(), where, EnvVarCA, serviceName(), raw)}
}

// certInvalidReason 把 x509 的 Reason 码翻译成客户能懂的话。
func certInvalidReason(e x509.CertificateInvalidError) string {
	switch e.Reason {
	case x509.Expired:
		return "证书已过期或还没到生效时间（也可能是本机时钟跑偏了）。"
	case x509.CANotAuthorizedForThisName:
		return "签发这张证书的 CA 不被允许签这个名字。"
	case x509.TooManyIntermediates:
		return "证书链太深，超过校验上限。"
	case x509.IncompatibleUsage:
		return "证书的用途不对（例如拿服务器证书去验证客户端）。"
	case x509.NameMismatch, x509.NameConstraintsWithoutSANs, x509.UnconstrainedName:
		return "证书名字与访问地址不符。"
	default:
		return "证书链校验没过（格式、用途或签发关系有问题）。"
	}
}

// ── 真握手探测（自检用）──────────────────────────────────────────────────

// ProbeResult 是一次握手探测的结论。
type ProbeResult struct {
	URL      string
	Host     string
	Skipped  bool
	SkipWhy  string
	Err      error
	Subject  string
	Issuer   string
	NotAfter time.Time
	// TrustedBy 说明这次验证用了什么根（人话）。
	TrustedBy string
	// Expiring 报告证书是否将在 14 天内过期：现在能用，但很快要换——提前说是运维最喜欢的信息。
	Expiring bool
}

// Probe 真握手验一次证书。只对 https 地址有效；非 https 返回 Skipped。
//
// 为什么用 tls.Dialer 而不是 http.Get：这里验的是「证书认不认」，
// 不该因为对端 /v1 路径不存在或返回 404 而报错。
func Probe(ctx context.Context, rawURL string) ProbeResult {
	res := ProbeResult{URL: rawURL, TrustedBy: Load().TrustSummary()}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		res.Skipped = true
		res.SkipWhy = "地址语法不对，没探"
		return res
	}
	if u.Scheme != "https" {
		res.Skipped = true
		res.SkipWhy = fmt.Sprintf("地址是 %s://，不是 https：这条链路不涉及证书，无需验证", u.Scheme)
		return res
	}
	res.Host = u.Host
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "443"
	}
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: probeTimeout}, Config: &tls.Config{ServerName: host}}
	if p := Pool(); p != nil {
		d.Config.RootCAs = p
	}
	if InsecureSkipVerify() {
		d.Config.InsecureSkipVerify = true
	}
	conn, err := d.DialContext(cctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		res.Err = err
		return res
	}
	defer conn.Close()
	if tc, ok := conn.(*tls.Conn); ok {
		if st := tc.ConnectionState(); len(st.PeerCertificates) > 0 {
			leaf := st.PeerCertificates[0]
			res.Subject = leaf.Subject.String()
			res.Issuer = leaf.Issuer.String()
			res.NotAfter = leaf.NotAfter
			res.Expiring = time.Until(leaf.NotAfter) < 14*24*time.Hour
		}
	}
	return res
}

// ── 小工具 ───────────────────────────────────────────────────────────────

func splitList(s string) []string {
	f := func(r rune) bool { return r == ':' || r == ',' || r == os.PathListSeparator }
	var out []string
	for _, p := range strings.FieldsFunc(s, f) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "y":
		return true
	}
	return false
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// hostPort 从地址里取 host:port（可能带路径），给 openssl s_client 用。
func hostPort(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return strings.TrimSpace(raw)
	}
	if u.Port() != "" {
		return u.Host
	}
	return u.Hostname() + ":443"
}

// serviceName 指向上层 systemd 单元名（install.sh 写进 env），
// 让修复命令能直接抄——多实例安装时尤其重要。取不到就退回服务名。
func serviceName() string {
	if v := strings.TrimSpace(os.Getenv("SKILLFORGE_SERVICE_NAME")); v != "" {
		return v
	}
	return "skillforge"
}
