package tlsconf

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// reset 清掉加载缓存，让下一次 Load() 重新读环境变量。
//
// 这是测试专用：生产中缓存按「信任配置指纹」自动失效，改 env 或换证书文件都会被
// 立刻感知；测试里要的是「无视指纹、强制重来」。
func reset() {
	// 委托给生产代码里的重置，而不是在这里手抄一份缓存列表。
	// 手抄过一次的后果：只清了证书池、没清 transport，于是测试用回上一个用例
	// 建的 transport（出现「配了 CA 不生效」的假红），而生产路径上的同款失效
	// 是「换了证书文件却不生效」——只有共用一份重置逻辑才不会再漏。
	resetForTests()
}

// ── 测试用证书 ───────────────────────────────────────────────────────────

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func genCA(t *testing.T) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成 CA 私钥失败: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "SkillForge 测试根 CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发 CA 失败: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("解析 CA 失败: %v", err)
	}
	return testCA{
		cert: cert,
		key:  key,
		pem:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

// issue 签一张服务端证书：ips/dns 决定它对哪些地址有效（名字不匹配的用例靠这里造）。
func (ca testCA) issue(t *testing.T, cn string, ips []net.IP, dns []string, nb, na time.Time) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成服务端私钥失败: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    nb,
		NotAfter:     na,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  ips,
		DNSNames:     dns,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("签发服务端证书失败: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// serveTLS 起一个用指定证书的 https 服务（只回 "ok"）。
func serveTLS(t *testing.T, cert tls.Certificate) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// loopbackLeaf 签一张对 127.0.0.1 有效的证书（测试服务都跑在回环上）。
func loopbackLeaf(t *testing.T, ca testCA) tls.Certificate {
	t.Helper()
	return ca.issue(t, "skillforge-test",
		[]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, []string{"localhost"},
		time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
}

// withEnv 设环境变量跑一段逻辑，结束后恢复（测试之间不能互相污染）。
func withEnv(t *testing.T, kv map[string]string, fn func()) {
	t.Helper()
	type old struct {
		v  string
		ok bool
	}
	saved := map[string]old{}
	for k, v := range kv {
		o, ok := os.LookupEnv(k)
		saved[k] = old{o, ok}
		if v == "" {
			_ = os.Unsetenv(k)
		} else {
			_ = os.Setenv(k, v)
		}
	}
	reset()
	defer func() {
		for k, o := range saved {
			if o.ok {
				_ = os.Setenv(k, o.v)
			} else {
				_ = os.Unsetenv(k)
			}
		}
		reset()
	}()
	fn()
}

// writeCA 把 CA 证书落盘（模拟客户把 ca.pem 拷到机器上）。
func writeCA(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("写 CA 文件失败: %v", err)
	}
	return p
}

// ── 核心行为：配了 CA 就能连上，没配就给出人话 ──────────────────────────

// TestCABundleTurnsX509FailureIntoSilence 是这个包存在的理由：
// 同一个自签 https 服务，不配 CA 时请求失败（客户现场原样），配上 CA 之后必须成功。
// 这一对断言同时证明「配置真的生效了」和「失败真的是证书引起的」。
func TestCABundleTurnsX509FailureIntoSilence(t *testing.T) {
	ca := genCA(t)
	srv := serveTLS(t, loopbackLeaf(t, ca))

	// 1) 没配 CA：请求失败，且翻译后是人话 + 修法。
	withEnv(t, map[string]string{EnvVarCA: "", EnvVarInsecure: ""}, func() {
		cli := NewClient(5 * time.Second)
		resp, err := cli.Get(srv.URL)
		if err == nil {
			resp.Body.Close()
			t.Fatalf("没配 CA 却连上了自签 https 服务 —— 说明证书校验没在起作用，这个包的核心前提不成立")
		}
		if !LooksLikeTLS(err) {
			t.Fatalf("没配 CA 的失败不是 TLS 类错误（%v），后面的翻译断言会变成空断言", err)
		}
		msg := Explain(err, srv.URL).Error()
		for _, want := range []string{"TLS 证书不被信任", EnvVarCA, "systemctl restart", "原始错误"} {
			if !strings.Contains(msg, want) {
				t.Errorf("人话里缺少 %q；实际：\n%s", want, msg)
			}
		}
		// 翻译不能吃掉错误身份：上层还要靠 errors.As 做判定。
		var ua x509.UnknownAuthorityError
		if !errors.As(Explain(err, srv.URL), &ua) {
			t.Errorf("翻译后丢了 errors.As(x509.UnknownAuthorityError)：上层判定会失效")
		}
	})

	// 2) 配上同一个 CA（PEM 文件）：请求必须成功。
	p := writeCA(t, ca.pem)
	withEnv(t, map[string]string{EnvVarCA: p, EnvVarInsecure: ""}, func() {
		st := Load()
		if st.Err != nil {
			t.Fatalf("加载 CA 失败: %v", st.Err)
		}
		if st.BundleCerts != 1 {
			t.Fatalf("CA 证书数 %d，期望 1", st.BundleCerts)
		}
		resp, err := NewClient(5 * time.Second).Get(srv.URL)
		if err != nil {
			t.Fatalf("配了 CA 仍连不上：%v —— 「配了不生效」比不配更坏，客户会以为程序坏了", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("状态码 %d，期望 200", resp.StatusCode)
		}
	})
}

// TestDERBundleAccepted：客户从 Windows 导出的 .cer 常是 DER 二进制。
// 只认 PEM 的话他会得到「文件在但一个证书都没有」，然后开始怀疑自己。
func TestDERBundleAccepted(t *testing.T) {
	ca := genCA(t)
	srv := serveTLS(t, loopbackLeaf(t, ca))
	blk, _ := pem.Decode(ca.pem)
	if blk == nil {
		t.Fatal("CA PEM 解析失败")
	}
	p := writeCA(t, blk.Bytes) // 裸 DER，没有 PEM 头尾
	withEnv(t, map[string]string{EnvVarCA: p}, func() {
		if err := Load().Err; err != nil {
			t.Fatalf("DER 格式的 CA 没装上：%v", err)
		}
		resp, err := NewClient(5 * time.Second).Get(srv.URL)
		if err != nil {
			t.Fatalf("DER 格式的 CA 装上但连不上：%v", err)
		}
		resp.Body.Close()
	})
}

// TestCADirectoryAccepted：有些运维给的是一个目录（系统 CA 目录的习惯）。
func TestCADirectoryAccepted(t *testing.T) {
	ca := genCA(t)
	srv := serveTLS(t, loopbackLeaf(t, ca))
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "corp-ca.crt"), ca.pem, 0o644); err != nil {
		t.Fatal(err)
	}
	withEnv(t, map[string]string{EnvVarCA: dir}, func() {
		if st := Load(); st.Err != nil || st.BundleCerts != 1 {
			t.Fatalf("目录形式的 CA 没装上：err=%v certs=%d", st.Err, st.BundleCerts)
		}
		resp, err := NewClient(5 * time.Second).Get(srv.URL)
		if err != nil {
			t.Fatalf("目录形式装上却连不上：%v", err)
		}
		resp.Body.Close()
	})
}

// TestMultipleCAPathsAccepted：多个 CA（企业里常见：网关一个 CA、代理另一个）。
func TestMultipleCAPathsAccepted(t *testing.T) {
	ca1, ca2 := genCA(t), genCA(t)
	srv := serveTLS(t, loopbackLeaf(t, ca1))
	p1, p2 := writeCA(t, ca1.pem), writeCA(t, ca2.pem)
	withEnv(t, map[string]string{EnvVarCA: p1 + ":" + p2}, func() {
		st := Load()
		if st.BundleCerts != 2 {
			t.Fatalf("两份 CA 只装上了 %d 份（配置 %q）", st.BundleCerts, st.BundlePath)
		}
		resp, err := NewClient(5 * time.Second).Get(srv.URL)
		if err != nil {
			t.Fatalf("连不上：%v", err)
		}
		resp.Body.Close()
	})
}

// TestReplacingCABundleTakesEffectWithoutRestart：证书**原地换掉**必须立刻生效。
//
// 客户现场的真实动作：网关证书续期后，运维把新 CA 覆盖到同一个路径上，然后
// 期望服务自己跟上（没人会因为换证书文件去重启业务）。如果实现是「首次加载后
// 一辈子用缓存」，表现就是「新证书装上了还报 unknown authority」——
// 客户于是去怀疑证书或我们，而不是去怀疑缓存。这条测试锁住这个行为。
func TestReplacingCABundleTakesEffectWithoutRestart(t *testing.T) {
	caOld, caNew := genCA(t), genCA(t)
	srvOld := serveTLS(t, loopbackLeaf(t, caOld))
	srvNew := serveTLS(t, loopbackLeaf(t, caNew))

	// 同一个路径，先放旧 CA。
	p := writeCA(t, caOld.pem)
	withEnv(t, map[string]string{EnvVarCA: p, EnvVarInsecure: ""}, func() {
		get := func(u, what string) {
			t.Helper()
			resp, err := NewClient(5 * time.Second).Get(u)
			if err != nil {
				t.Fatalf("%s：%v", what, err)
			}
			resp.Body.Close()
		}
		get(srvOld.URL, "旧 CA 在时连旧服务失败")

		// 原地覆盖成新 CA（不 reset、不重启进程 —— 客户的真实情形）。
		if err := os.WriteFile(p, caNew.pem, 0o644); err != nil {
			t.Fatalf("覆盖 CA 文件失败: %v", err)
		}
		// 显式把 mtime 推后：同一次测试里两次写入可能落在同一时间戳上，
		// 那样「靠 mtime 感知变化」的实现会被误判成失效（假红）。
		future := time.Now().Add(2 * time.Second)
		if err := os.Chtimes(p, future, future); err != nil {
			t.Fatalf("改 CA 文件时间戳失败: %v", err)
		}

		if st := Load(); st.BundleCerts != 1 {
			t.Fatalf("换证书后重新加载应仍是 1 份 CA，实际 %d（err=%v）", st.BundleCerts, st.Err)
		}
		get(srvNew.URL, "证书已换成新的却仍连不上 —— 「换证书要重启服务」是客户不可接受的")
	})
}

// ── 配置错误必须报出来，不能静默失效 ────────────────────────────────────

func TestMissingBundleFileIsReported(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.pem")
	withEnv(t, map[string]string{EnvVarCA: missing}, func() {
		st := Load()
		if st.Err == nil {
			t.Fatal("CA 文件不存在却没报错 —— 客户会以为配好了，直到运行期才炸")
		}
		if !strings.Contains(st.Err.Error(), missing) {
			t.Errorf("报错里没点出是哪个路径：%v", st.Err)
		}
		if Pool() != nil {
			t.Error("加载失败时不该给出证书池（半装的池比不装更难查）")
		}
	})
}

func TestBundleWithoutCertsIsReported(t *testing.T) {
	p := writeCA(t, []byte("这不是证书，是运维随手写的一段说明\n"))
	withEnv(t, map[string]string{EnvVarCA: p}, func() {
		st := Load()
		if st.Err == nil {
			t.Fatal("文件里没有证书却没报错")
		}
		if !strings.Contains(st.Err.Error(), "没有读到任何可用证书") {
			t.Errorf("报错没说明「读到了文件但没有证书」：%v", st.Err)
		}
	})
}

// ── 翻译的覆盖面 ─────────────────────────────────────────────────────────

func TestHostnameMismatchTranslated(t *testing.T) {
	ca := genCA(t)
	// 证书只对 gw.corp 有效，访问的是 127.0.0.1 → HostnameError。
	leaf := ca.issue(t, "gw.corp", nil, []string{"gw.corp"},
		time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	srv := serveTLS(t, leaf)
	p := writeCA(t, ca.pem)
	withEnv(t, map[string]string{EnvVarCA: p}, func() {
		_, err := NewClient(5 * time.Second).Get(srv.URL)
		if err == nil {
			t.Fatal("证书名字不匹配却连上了")
		}
		msg := Explain(err, srv.URL).Error()
		if !strings.Contains(msg, "名字不匹配") {
			t.Errorf("没翻译成「名字不匹配」：\n%s", msg)
		}
		if !strings.Contains(msg, "Subject Alternative Name") {
			t.Errorf("没给自查命令：\n%s", msg)
		}
	})
}

func TestExpiredCertTranslated(t *testing.T) {
	ca := genCA(t)
	leaf := ca.issue(t, "skillforge-test",
		[]net.IP{net.ParseIP("127.0.0.1")}, []string{"localhost"},
		time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour)) // 昨天就过期了
	srv := serveTLS(t, leaf)
	p := writeCA(t, ca.pem)
	withEnv(t, map[string]string{EnvVarCA: p}, func() {
		_, err := NewClient(5 * time.Second).Get(srv.URL)
		if err == nil {
			t.Fatal("过期证书却连上了")
		}
		msg := Explain(err, srv.URL).Error()
		if !strings.Contains(msg, "过期") {
			t.Errorf("没翻译成「过期」：\n%s", msg)
		}
		if !strings.Contains(msg, "定时") && !strings.Contains(msg, "时钟") {
			t.Errorf("没提「也可能是本机时钟跑偏」这种最常见误判：\n%s", msg)
		}
	})
}

// TestNonTLSErrorUntouched：不是证书问题就别套证书的帽子 ——
// 那会把客户引到错误的排查方向（去翻证书，其实是端口没开）。
func TestNonTLSErrorUntouched(t *testing.T) {
	plain := errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	if got := Explain(plain, "https://gw.corp/v1"); got != plain {
		t.Errorf("非 TLS 错误被改写了：%v", got)
	}
	if LooksLikeTLS(plain) {
		t.Error("connection refused 被判成 TLS 类错误")
	}
}

// TestSchemeMismatchTranslated：地址 scheme 写反（`https://` 打到只说明文的端口）
// 是内网部署的高频手误。Go 报 `server gave HTTP response to HTTPS client`，
// 客户看不出问题在哪，第一反应是去折腾证书 —— 必须给「协议写反了」的人话。
func TestSchemeMismatchTranslated(t *testing.T) {
	// 用真链路的真错误开路，不自己编错误字符串：编出来的文案可能和 Go 实际给的不一样，
	// 那样这条测试就只证明了「我认识我自己写的字符串」。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	raw := strings.Replace(srv.URL, "http://", "https://", 1)
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(raw)
	if err == nil {
		resp.Body.Close()
		t.Fatal("明文服务收到 https 请求却没报错 —— 测试前提不成立，后面的断言是空跑")
	}
	if !LooksLikeTLS(err) {
		t.Fatalf("地址协议写反的错误没被认出：%v", err)
	}
	if !isSchemeMismatch(err) {
		t.Fatalf("没判成协议不匹配：%v", err)
	}
	msg := Explain(err, raw).Error()
	if !strings.Contains(msg, "协议和端口对不上") {
		t.Errorf("没翻译成「协议对不上」：\n%s", msg)
	}
	if !strings.Contains(msg, "这不是证书问题") {
		t.Errorf("没明确排除证书方向 —— 客户会继续折腾证书：\n%s", msg)
	}
	if strings.Contains(msg, EnvVarCA+"=") {
		t.Errorf("协议写反还说让人配 CA，属于误导：\n%s", msg)
	}
}

// TestRecordHeaderErrorTranslated：另一种同样含义的报错形态（对端是真 TLS，
// 我方用明文打过去时在服务端炸出来/noise 到客户端）。这条保证兜底文案不会漏。
func TestRecordHeaderErrorTranslated(t *testing.T) {
	e := tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}
	if !LooksLikeTLS(e) {
		t.Fatal("RecordHeaderError 没被认出")
	}
	if !isSchemeMismatch(e) {
		t.Fatal("RecordHeaderError 没判成协议不匹配")
	}
	if got := Explain(e, "https://gw.corp:8080/v1").Error(); !strings.Contains(got, "协议和端口对不上") {
		t.Errorf("没给协议人话：\n%s", got)
	}
}

// ── 显式跳过校验（客户知情时的逃生门）──────────────────────────────────

func TestInsecureFlagAllowsSelfSigned(t *testing.T) {
	srv := serveTLS(t, loopbackLeaf(t, genCA(t)))
	withEnv(t, map[string]string{EnvVarCA: "", EnvVarInsecure: "1"}, func() {
		if !InsecureSkipVerify() {
			t.Fatal("SKILLFORGE_INSECURE_SKIP_VERIFY=1 没被识别")
		}
		resp, err := NewClient(5 * time.Second).Get(srv.URL)
		if err != nil {
			t.Fatalf("显式跳过校验后仍连不上：%v", err)
		}
		resp.Body.Close()
	})
	for _, v := range []string{"0", "false", "no", ""} {
		withEnv(t, map[string]string{EnvVarInsecure: v}, func() {
			if InsecureSkipVerify() {
				t.Errorf("%q 被当成了「跳过校验」——这种值只能是显式的 1/true/yes", v)
			}
		})
	}
}

// ── Probe（自检的真握手）────────────────────────────────────────────────

func TestProbeSkipsNonHTTPS(t *testing.T) {
	withEnv(t, map[string]string{EnvVarCA: ""}, func() {
		for _, u := range []string{"http://127.0.0.1:8092", "127.0.0.1:8092", "::::"} {
			if r := Probe(context.Background(), u); !r.Skipped {
				t.Errorf("地址 %q 不该真探（结果 %+v）", u, r)
			}
		}
	})
}

func TestProbeReportsCertAndTrust(t *testing.T) {
	ca := genCA(t)
	srv := serveTLS(t, loopbackLeaf(t, ca))
	p := writeCA(t, ca.pem)

	// 没配 CA：探测必须失败，且原因可翻译（这就是客户装完立刻能看到的那条）。
	withEnv(t, map[string]string{EnvVarCA: "", EnvVarInsecure: ""}, func() {
		r := Probe(context.Background(), srv.URL)
		if r.Skipped || r.Err == nil {
			t.Fatalf("没配 CA 时探测应当失败：%+v", r)
		}
		if !LooksLikeTLS(r.Err) {
			t.Fatalf("探测失败不是 TLS 类：%v", r.Err)
		}
	})

	// 配上 CA：成功并报出证书主体/有效期（运维据此判断「快过期了」）。
	withEnv(t, map[string]string{EnvVarCA: p}, func() {
		r := Probe(context.Background(), srv.URL)
		if r.Err != nil {
			t.Fatalf("配上 CA 探测失败：%v（%s）", r.Err, Explain(r.Err, srv.URL))
		}
		if r.Subject == "" || r.NotAfter.IsZero() {
			t.Errorf("没报出证书信息：%+v", r)
		}
		if !strings.Contains(r.TrustedBy, "自定义 CA") {
			t.Errorf("没说清信任来源：%q", r.TrustedBy)
		}
	})
}

// TestProbeUnreachableGivesPlainResult：探测地址连不上时是普通网络错误，
// 不该被说成证书问题（否则客户会去翻证书，其实是防火墙）。
func TestProbeUnreachableGivesPlainResult(t *testing.T) {
	withEnv(t, map[string]string{EnvVarCA: ""}, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		r := Probe(ctx, "https://127.0.0.1:1/v1")
		if r.Err == nil {
			t.Fatal("连不上的地址却探测成功了")
		}
		if LooksLikeTLS(r.Err) {
			t.Errorf("连接被拒被判成 TLS 问题：%v", r.Err)
		}
	})
}

func TestTrustSummaryListsSources(t *testing.T) {
	ca := genCA(t)
	p := writeCA(t, ca.pem)
	withEnv(t, map[string]string{EnvVarCA: p}, func() {
		s := Load().TrustSummary()
		if !strings.Contains(s, "自定义 CA 1 份") {
			t.Errorf("信任来源没说清自定义 CA：%q", s)
		}
	})
}

// TestTransportKeepsProxySupport：内网出口常走 HTTPS_PROXY。
// 换 Transport 时把代理支持丢了的话，症状是「装完能连、走代理的机器全连不上」。
func TestTransportKeepsProxySupport(t *testing.T) {
	withEnv(t, map[string]string{EnvVarCA: "", "HTTPS_PROXY": "http://proxy.corp:3128"}, func() {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if tr.Proxy == nil {
			t.Skip("本机默认 transport 不支持代理读取，跳过")
		}
		if got := Transport().Proxy; got == nil {
			t.Error("Transport() 丢了代理支持")
		}
	})
}
