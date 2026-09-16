package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/ocrsvc"
	"github.com/lizhemin15/skillforge/internal/tlsconf"
)

// selftest_test.go 守护「自检本身」。
//
// 自检是离线包交给客户后的唯一体检手段，它自己要是会误报绿灯，就等于没有自检：
// 客户看到「全部通过」却仍然拿不到能用的 PDF、或者沙箱其实没拦住，
// 这类问题排查成本极高。所以这里用**合成证据**逐条试探判定逻辑：
// 每一条真实的失败形态都必须被判失败。
//
// 更关键的是 Bug G 的复发形态：字体覆盖率 100%、PDF 也生成成功，
// 但回读出来没有数字——这条必须红，否则自检只是个安慰剂。

// TestJudgeVersion 版本号必须是构建期注入的，dev 一律判失败。
func TestJudgeVersion(t *testing.T) {
	cases := []struct {
		name   string
		v      string
		commit string
		wantOK bool
	}{
		{"CI 产物", "v0.4.0", "abc1234", true},
		{"本地 dev 构建", "dev", "none", false},
		{"空版本号", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := judgeVersion(tc.v, tc.commit)
			if got.ok != tc.wantOK {
				t.Fatalf("judgeVersion(%q) ok=%v，期望 %v；明细：%v", tc.v, got.ok, tc.wantOK, got.detail)
			}
			if len(got.detail) == 0 {
				t.Fatal("判定必须给出明细，客户要照着修")
			}
		})
	}
}

// TestJudgePDFFont 覆盖字体的全部失败形态，特别是 Bug G 的「覆盖率过了但回读无字」。
func TestJudgePDFFont(t *testing.T) {
	const healthy = "自检样例\n产品\n数量\n单价\n云服务器\n5\n12000\n" + selfTestDigits + " ABCdef " + selfTestChinese

	cases := []struct {
		name    string
		ev      fontEvidence
		wantOK  bool
		wantHit string // 明细里必须出现的关键词（帮客户定位）
	}{
		{
			name:    "健康的字体",
			ev:      fontEvidence{Path: "/usr/share/fonts/truetype/x.ttf", Text: healthy, Bytes: 21000},
			wantOK:  true,
			wantHit: "回读",
		},
		{
			name:    "一个字体都没找到",
			ev:      fontEvidence{Path: ""},
			wantOK:  false,
			wantHit: "找不到任何可加载",
		},
		{
			name:    "字体缺字形（Bug G 的根源）",
			ev:      fontEvidence{Path: "/usr/share/fonts/truetype/droid.ttf", Missing: []rune("0123456789")},
			wantOK:  false,
			wantHit: "缺 10 个必备字符",
		},
		{
			name: "Bug G 复发：覆盖率过了，但生成的 PDF 里没有数字",
			ev: fontEvidence{
				Path: "/usr/share/fonts/truetype/x.ttf",
				// 数字被绘制路径吞掉——正是当初报价单里「5 台 / 12000」全变空白的形态
				Text:  "自检样例\n产品\n数量\n单价\n云服务器\n A Cdef " + selfTestChinese,
				Bytes: 21172,
			},
			wantOK:  false,
			wantHit: "Bug G",
		},
		{
			name:    "回读不出中文",
			ev:      fontEvidence{Path: "/usr/share/fonts/truetype/x.ttf", Text: selfTestDigits + " ABCdef", Bytes: 900},
			wantOK:  false,
			wantHit: "回读不到中文",
		},
		{
			name:    "提取器空手而归（不能当通过）",
			ev:      fontEvidence{Path: "/usr/share/fonts/truetype/x.ttf", Text: "", Bytes: 900},
			wantOK:  false,
			wantHit: "回读不出任何文本",
		},
		{
			name:    "试渲染失败",
			ev:      fontEvidence{Path: "/usr/share/fonts/truetype/x.ttf", Err: errors.New("试渲染 PDF 失败：boom")},
			wantOK:  false,
			wantHit: "试渲染",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := judgePDFFont(tc.ev)
			if got.ok != tc.wantOK {
				t.Fatalf("ok=%v，期望 %v；明细：%v", got.ok, tc.wantOK, got.detail)
			}
			if !containsAny(got.detail, tc.wantHit) {
				t.Fatalf("明细里没有 %q，客户定位不到原因；实际明细：%v", tc.wantHit, got.detail)
			}
		})
	}
}

// TestJudgePDFFontBugGRelapseDetailIsActionable 确认 Bug G 复发时，
// 明细要同时给出「症状名」和「回读到的实际文本」——否则客户只会看到一句「失败」。
func TestJudgePDFFontBugGRelapseDetailIsActionable(t *testing.T) {
	got := judgePDFFont(fontEvidence{
		Path:  "/usr/share/fonts/truetype/x.ttf",
		Text:  "自检样例\n产品\n数量\n云服务器",
		Bytes: 12345,
	})
	if got.ok {
		t.Fatal("回读文本里没有任何数字，必须判失败")
	}
	joined := strings.Join(got.detail, "\n")
	for _, want := range []string{selfTestDigits, "回读到的文本", "自检样例"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Bug G 明细缺少 %q，客户看不到实际症状，实际：%s", want, joined)
		}
	}
}

// TestJudgeSandbox 沙箱判定的每一条「危险 / 无意义证据」都必须红。
func TestJudgeSandbox(t *testing.T) {
	healthy := map[string]string{
		"uid":                                 "65534",
		"network":                             "断(OK)",
		"write:work":                          "可以(OK)",
		"write:/etc":                          "拒绝(OK)",
		"read:/etc/shadow":                    "拒绝(OK)",
		"read:/opt/skillforge/skillforge.env": "拒绝(OK)",
	}

	cases := []struct {
		name    string
		diag    map[string]string
		err     error
		wantOK  bool
		wantHit string
	}{
		{"健康的沙箱", healthy, nil, true, "uid=65534"},
		{"探针采集失败", nil, errors.New("找不到任何真实存在的机密文件可作证据——拿不存在路径当证据是假绿灯"), false, "假绿灯"},
		{"探针自己不报错但没证据", nil, nil, false, "没有返回任何证据"},
		{"探针执行报错", map[string]string{"error": "systemd-run: exit 200"}, nil, false, "systemd-run"},
		{"没降权（root 跑代码）", map[string]string{"uid": "0", "network": "断(OK)", "write:work": "可以(OK)"}, nil, false, "没有降权"},
		{"网络没断", map[string]string{"uid": "65534", "network": "通(危险)", "write:work": "可以(OK)"}, nil, false, "网络"},
		{"工作区写不进去", map[string]string{"uid": "65534", "network": "断(OK)", "write:work": "失败(异常): PermissionError"}, nil, false, "写不了工作目录"},
		{
			"读得到机密文件",
			map[string]string{"uid": "65534", "network": "断(OK)", "write:work": "可以(OK)", "read:/opt/skillforge/skillforge.env": "可读(危险)"},
			nil, false, "没拦住",
		},
		{
			"能往 /etc 乱写",
			map[string]string{"uid": "65534", "network": "断(OK)", "write:work": "可以(OK)", "write:/etc": "可写(危险)"},
			nil, false, "没拦住",
		},
		{
			"拿不存在的文件当证据（曾经的假绿灯）",
			map[string]string{"uid": "65534", "network": "断(OK)", "write:work": "可以(OK)", "read:/opt/skillforge/skillforge.env": "文件不存在(证据无效)"},
			nil, false, "无意义证据",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := judgeSandbox(tc.diag, []string{"/opt/skillforge/skillforge.env"}, tc.err)
			if got.ok != tc.wantOK {
				t.Fatalf("ok=%v，期望 %v；明细：%v", got.ok, tc.wantOK, got.detail)
			}
			if !containsAny(got.detail, tc.wantHit) {
				t.Fatalf("明细里没有 %q；实际明细：%v", tc.wantHit, got.detail)
			}
		})
	}
}

// TestJudgeSandboxEmptyUIDIsNotBlamedOnPrivilegeDrop 锁住一条真实发生过的误诊。
//
// 现场：almalinux:8 最小安装默认不带 python3 → 沙箱探针的解释器起不来 → 探针输出里
// 压根没有 uid 这一项 → 旧逻辑掉进「uid != 65534」分支，打印「没有降权：uid=」。
// 客户看到的是「安全加固失效」，实际只是目标机缺个解释器。归因错了比不报还坏：
// 它把「环境缺件」伪装成「安全缺陷」，会把人带去查错方向。
//
// 后来又逮到第二类真因（Bug Y2）：AlmaLinux 8 的 systemd 239 不认 systemd-run 的
// --working-directory，探针一个字都不吐，而这句话当时一口咬定「缺 python3」——
// 客户装了几遍 python3 都修不好。所以现在这条明细**不许给出单一结论**：
// 只能说「证据缺失」，并把已知真因按可能性摊开。
func TestJudgeSandboxEmptyUIDIsNotBlamedOnPrivilegeDrop(t *testing.T) {
	got := judgeSandbox(map[string]string{
		"network":    "断(OK)",
		"write:work": "可以(OK)",
	}, []string{"/opt/skillforge/skillforge.env"}, nil)

	if got.ok {
		t.Fatalf("没拿到 uid 证据却判 ok=true（假绿灯）：%v", got.detail)
	}
	joined := strings.Join(got.detail, "\n")
	if strings.Contains(joined, "没有降权") {
		t.Fatalf("把「证据缺失」误诊成「没有降权」——这条误诊必须绝迹：%v", got.detail)
	}
	if !strings.Contains(joined, "证据缺失") {
		t.Fatalf("必须点明这是「证据缺失」：%v", got.detail)
	}
	// ⚠️ 断言必须锚在「区分性子句」上，不能锚在相邻句子里也有的词。
	// 踩过的坑：原来这里只要求出现 "systemd" 和 "原始回执"，结果把文案改回
	// 「一口咬定缺 python3」后测试**依然全绿**——因为紧挨着的上一句
	// 「请先看探针的原始回执（systemd-run 的 stdout/stderr）」把这两个词都送上了。
	// 子串断言会跨句泄漏：量到的不是被测属性，而是邻居的余光。
	for _, want := range []string{"两类真因", "python3", "systemd 版本较老"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("明细没摊开已知真因 %q（只给单一结论会把客户带去错方向）：%v", want, got.detail)
		}
	}
	if !strings.Contains(joined, "原始回执") {
		t.Fatalf("没告诉客户去哪儿看原始证据：%v", got.detail)
	}
	// 历史上那句错误的单一结论必须绝迹——它就是客户「装几遍 python3 也修不好」的来源。
	if strings.Contains(joined, "最常见原因是目标机缺 python3") {
		t.Fatalf("出现历史误诊原句（把两类真因收窄成 python3 一个）：%v", got.detail)
	}
}

// TestJudgeSandboxHealthyKeepsEveryEvidence 健康路径也要把每条证据摊开——
// 客户要能看见「探针真的逐项检查了」，而不是一句「OK」。
func TestJudgeSandboxHealthyKeepsEveryEvidence(t *testing.T) {
	got := judgeSandbox(map[string]string{
		"uid":              "65534",
		"network":          "断(OK)",
		"write:work":       "可以(OK)",
		"write:/etc":       "拒绝(OK)",
		"read:/etc/shadow": "拒绝(OK)",
	}, []string{"/etc/shadow"}, nil)
	if !got.ok {
		t.Fatalf("健康沙箱应通过，实际明细：%v", got.detail)
	}
	joined := strings.Join(got.detail, "\n")
	for _, want := range []string{"read:/etc/shadow", "write:/etc", "网络=断(OK)"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("明细缺少证据 %q：%s", want, joined)
		}
	}
}

// TestSecretEvidencePathsOnlyRealFiles 证据路径必须真实存在，且去重、含底裤探针。
// 这是「假证据」那个坑的回归防线：不存在的路径同样打不开，会伪装成「拒绝(OK)」。
func TestSecretEvidencePathsOnlyRealFiles(t *testing.T) {
	t.Setenv("SKILLFORGE_ENV_FILE", "/definitely/not/here/skillforge.env")
	t.Setenv("SKILLFORGE_DB", "/definitely/not/here/skillforge.db")
	t.Setenv("SKILLFORGE_DATA_DIR", "/definitely/not/here")

	got := secretEvidencePaths()
	for _, p := range got {
		if strings.HasPrefix(p, "/definitely/not/here") {
			t.Fatalf("不存在的路径被当成了证据：%s（它的「打不开」说明不了沙箱拦得住）", p)
		}
	}
	// /etc/shadow 在任何 Linux 上都存在，是最后一道真实证据。
	foundShadow := false
	for _, p := range got {
		if p == "/etc/shadow" {
			foundShadow = true
		}
	}
	if !foundShadow {
		t.Fatalf("应当始终包含 /etc/shadow 作为真实证据，实际：%v", got)
	}
	seen := map[string]bool{}
	for _, p := range got {
		if seen[p] {
			t.Fatalf("证据路径重复：%s（%v）", p, got)
		}
		seen[p] = true
	}
}

// TestFirstRunes 明细里的截断不能把 UTF-8 切坏（中文必须整字截断）。
func TestFirstRunes(t *testing.T) {
	if got := firstRunes("产品报价单", 2); got != "产品…" {
		t.Fatalf("firstRunes=%q，期望 %q", got, "产品…")
	}
	if got := firstRunes("  abc  ", 10); got != "abc" {
		t.Fatalf("firstRunes=%q，期望 %q", got, "abc")
	}
}

// TestJudgeParseService 解析服务判定的五种结论各钉一条。
//
// 为什么这组用例必须存在：这一项的难点全在「坏了」和「按设计没有」的边界上 ——
// 判严了，`--no-ocr` 的正常安装被报成「安装失败」，客户去折腾一个本来就不该跑的服务；
// 判松了，真坏掉的解析服务被静默放过，客户只看到「生成的技能跟我给的素材没关系」。
// 线上两种都发生过：前者是假红，后者是假绿。
func TestJudgeParseService(t *testing.T) {
	// healthy 是一个「自报 ok 且运行时正常」的响应。
	healthy := ocrsvc.Health{Running: true, RuntimeOK: true, Version: "ocrd-v5-runtime-guard"}
	// zombie 是线上真实形态：端口在听（HTTP 200），但运行时已坏（ok:false）。
	zombie := ocrsvc.Health{Running: true, RuntimeOK: false, Version: "ocrd-v5-runtime-guard"}
	// refused 是探不通的形态（连接被拒 / 超时）。
	refused := ocrsvc.Health{Err: errors.New("dial tcp 127.0.0.1:8093: connect: connection refused")}

	cases := []struct {
		name      string
		ev        parseEvidence
		wantOK    bool
		wantSkip  bool
		wantHits  []string // 明细里必须全部出现（每条都是客户要的「下一步」）
		wantAvoid []string // 明细里必须**不**出现（防止把预期行为说成事故）
	}{
		{
			name:     "显式禁用（--no-ocr 安装）",
			ev:       parseEvidence{RawEnv: "off", URL: ""},
			wantSkip: true,
			wantHits: []string{"显式禁用", "--no-ocr", "扫描件 PDF"},
			// 跳过不是失败：不能出现「失败 / 损坏 / 重装」这类吓人的话术。
			wantAvoid: []string{"损坏"},
		},
		{
			name:     "禁用值的另一种写法（none）",
			ev:       parseEvidence{RawEnv: "none", URL: ""},
			wantSkip: true,
			wantHits: []string{"显式禁用"},
		},
		{
			name:     "真的能干活",
			ev:       parseEvidence{RawEnv: "http://127.0.0.1:8093", URL: "http://127.0.0.1:8093", Loopback: true, Health: healthy},
			wantOK:   true,
			wantHits: []string{"正常", "ocrd-v5-runtime-guard"},
		},
		{
			name:     "活着但没上报版本",
			ev:       parseEvidence{RawEnv: "http://127.0.0.1:8093", URL: "http://127.0.0.1:8093", Loopback: true, Health: ocrsvc.Health{Running: true, RuntimeOK: true}},
			wantOK:   true,
			wantHits: []string{"正常", "未上报版本"},
		},
		{
			name:     "指向远端却连不上",
			ev:       parseEvidence{RawEnv: "http://10.0.0.9:8093", URL: "http://10.0.0.9:8093", Loopback: false, Health: refused},
			wantOK:   false,
			wantHits: []string{"远端解析服务", "curl", "connection refused"},
		},
		{
			name: "本机没装这个单元（老版 --no-ocr 没写 off）",
			ev: parseEvidence{RawEnv: "http://127.0.0.1:8093", URL: "http://127.0.0.1:8093",
				Loopback: true, UnitName: "skillforge-ocr", UnitPath: "", Health: refused},
			wantSkip: true,
			wantHits: []string{"没有解析服务单元", "systemctl status", "不算失败"},
		},
		{
			name: "端口在听但运行时已损坏（僵尸）",
			ev: parseEvidence{RawEnv: "http://127.0.0.1:8093", URL: "http://127.0.0.1:8093",
				Loopback: true, UnitName: "skillforge-ocr", UnitPath: "/etc/systemd/system/skillforge-ocr.service", Health: zombie},
			wantOK: false,
			// 关键：必须点明「端口在听 ≠ 能干活」，并给出 restart。
			wantHits: []string{"端口在听", "systemctl restart", "journalctl"},
		},
		{
			name: "单元在但服务没在跑",
			ev: parseEvidence{RawEnv: "http://127.0.0.1:8093", URL: "http://127.0.0.1:8093",
				Loopback: true, UnitName: "skillforge-ocr", UnitPath: "/etc/systemd/system/skillforge-ocr.service", Health: refused},
			wantOK:   false,
			wantHits: []string{"本该有解析服务", "systemctl restart", "connection refused"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := judgeParseService(tc.ev)
			if got.ok != tc.wantOK {
				t.Fatalf("ok=%v，期望 %v；明细：%v", got.ok, tc.wantOK, got.detail)
			}
			if got.skip != tc.wantSkip {
				t.Fatalf("skip=%v，期望 %v；明细：%v", got.skip, tc.wantSkip, got.detail)
			}
			// 跳过的不许同时判通过（否则 N/N 分母会被跳过项灌水）。
			if got.ok && got.skip {
				t.Fatalf("不可能同时通过又跳过；明细：%v", got.detail)
			}
			if got.name != "文档解析服务" {
				t.Fatalf("检查项名字变了：%q（-selftest 输出里靠它认人）", got.name)
			}
			for _, w := range tc.wantHits {
				if !containsAny(got.detail, w) {
					t.Fatalf("明细里没有 %q（客户据此没法自查）；实际：%v", w, got.detail)
				}
			}
			for _, w := range tc.wantAvoid {
				if containsAny(got.detail, w) {
					t.Fatalf("明细里不该出现 %q（会把预期行为说成事故）；实际：%v", w, got.detail)
				}
			}
		})
	}
}

// TestJudgeParseServiceMultiLineErrorIsTrimmed 原始错误只留第一行。
//
// 为什么：底层错误常带多行 context（HTTP 响应体、堆栈），铺进 -selftest 输出会把
// 「下一步怎么办」顶出屏幕 —— 这轮用户的投诉就是「一直卡着、看不到有用信息」。
func TestJudgeParseServiceMultiLineErrorIsTrimmed(t *testing.T) {
	ev := parseEvidence{
		RawEnv: "http://127.0.0.1:8093", URL: "http://127.0.0.1:8093", Loopback: true,
		UnitName: "skillforge-ocr", UnitPath: "/etc/systemd/system/skillforge-ocr.service",
		Health: ocrsvc.Health{Err: errors.New("Get \"http://127.0.0.1:8093/health\": dial tcp 127.0.0.1:8093: connect: connection refused\n第二行不该出现\n第三行也不该出现")},
	}
	got := judgeParseService(ev)
	if !containsAny(got.detail, "connection refused") {
		t.Fatalf("第一行的关键信息丢了：%v", got.detail)
	}
	for _, bad := range []string{"第二行不该出现", "第三行也不该出现", "\n"} {
		for _, l := range got.detail {
			if strings.Contains(l, bad) {
				t.Fatalf("多行错误没被掐掉（%q）：%v", bad, got.detail)
			}
		}
	}
}

func containsAny(lines []string, needle string) bool {
	for _, l := range lines {
		if strings.Contains(l, needle) {
			return true
		}
	}
	return false
}

// ── TLS 信任库自检项 ────────────────────────────────────────────────────

// TestJudgeTrustStoreConfiguredButNotLoaded 锁住最坏的一种「静默失效」：
// 客户按文档配了 CA bundle，但一个证书都没装进来。此时必须报失败 ——
// 若报通过，客户会以为 https 已经没问题，然后去怀疑我们程序本身。
func TestJudgeTrustStoreConfiguredButNotLoaded(t *testing.T) {
	c := judgeTrustStore(trustEvidence{st: tlsconf.Status{
		BundlePath:  "/etc/skillforge/ca.pem",
		BundleCerts: 0,
		SystemOK:    true,
		SystemCerts: 120,
		Err:         errors.New("读不到 CA 文件 /x.pem：没有那个文件或目录"),
	}})
	if c.skip || c.ok {
		t.Fatalf("配了 CA 却零证书，必须报失败，实际 ok=%v skip=%v", c.ok, c.skip)
	}
	joined := strings.Join(c.detail, "\n")
	if !strings.Contains(joined, "没有那个文件或目录") {
		t.Errorf("必须把加载失败的原因原样给出，实际：%s", joined)
	}
	if !strings.Contains(joined, "CA") {
		t.Errorf("修复指引必须说清「要给签发服务端证书的 CA」，实际：%s", joined)
	}
}

// TestJudgeTrustStoreEmptySystemStoreWithHTTPStepIsFailure：机器太素（没装
// ca-certificates）、没配 CA，而且端点里有 https —— 这是 https 必失败的现场，
// 必须失败并给出离线机可执行的两条修法，且要点名是哪一个端点在等 CA。
func TestJudgeTrustStoreEmptySystemStoreWithHTTPStepIsFailure(t *testing.T) {
	c := judgeTrustStore(trustEvidence{
		st:             tlsconf.Status{SystemOK: false, SystemNote: "系统根证书库读不到"},
		httpsEndpoints: []string{"SKILLFORGE_LLM_BASE_URL（llm.corp:8443）"},
	})
	if c.ok {
		t.Fatal("没有信任来源、端点又是 https，却报通过 —— 客户会一头撞上证书错误")
	}
	joined := strings.Join(c.detail, "\n")
	for _, want := range []string{"ca-certificates", tlsconf.EnvVarCA, "llm.corp:8443"} {
		if !strings.Contains(joined, want) {
			t.Errorf("修复指引缺少 %q，实际：%s", want, joined)
		}
	}
}

// TestJudgeTrustStoreEmptySystemStoreOnPlainHTTPIsNotBlamed：全部端点都是 http 的
// 纯内网部署（很常见的离线形态）。此时没有系统 CA 不影响任何功能 ——
// 判失败会让客户去修一个跟他无关的东西，反而消耗自检的可信度。必须通过，
// 但要把「将来改 https 前先解决它」说明白。
func TestJudgeTrustStoreEmptySystemStoreOnPlainHTTPIsNotBlamed(t *testing.T) {
	c := judgeTrustStore(trustEvidence{
		st: tlsconf.Status{SystemOK: false, SystemNote: "系统根证书库读不到"},
	})
	if !c.ok {
		t.Fatalf("端点全是 http 时不该判失败，实际 detail=%v", c.detail)
	}
	joined := strings.Join(c.detail, "\n")
	if !strings.Contains(joined, "当前不受影响") || !strings.Contains(joined, "https") {
		t.Errorf("通过时也要把「为什么现在没事、什么时候会有事」说清楚，实际：%s", joined)
	}
}

// TestJudgeTrustStoreHealthyNamesTheSource：通过时必须把「信任来源」摊开，
// 让客户排障时不必再猜这次到底信了谁。
func TestJudgeTrustStoreHealthyNamesTheSource(t *testing.T) {
	c := judgeTrustStore(trustEvidence{st: tlsconf.Status{SystemOK: true, SystemCerts: 141}})
	if !c.ok {
		t.Fatalf("系统根可用应通过，实际 detail=%v", c.detail)
	}
	if !strings.Contains(strings.Join(c.detail, "\n"), "141") {
		t.Errorf("通过时也要说清来源与数量，实际：%v", c.detail)
	}

	c2 := judgeTrustStore(trustEvidence{st: tlsconf.Status{BundlePath: "/etc/skillforge/ca.pem", BundleCerts: 2}})
	if !c2.ok {
		t.Fatalf("自定义 CA 加载成功应通过，实际 %v", c2.detail)
	}
	if !strings.Contains(strings.Join(c2.detail, "\n"), "2 份") {
		t.Errorf("应报告加载了几份证书，实际：%v", c2.detail)
	}
}

// TestJudgeTrustStoreInsecureIsLoudButNotFailure：跳过校验是客户可能刻意为之的选择，
// 不算失败；但绝不能静默通过 —— 必须显眼警告「任何人都能冒充内网服务」。
func TestJudgeTrustStoreInsecureIsLoudButNotFailure(t *testing.T) {
	c := judgeTrustStore(trustEvidence{st: tlsconf.Status{SystemOK: true, SystemCerts: 100, Insecure: true}})
	if !c.ok {
		t.Fatal("Insecure 是客户的选择，不该判失败（否则自检会挡住合法用法）")
	}
	joined := strings.Join(c.detail, "\n")
	if !strings.Contains(joined, "⚠") || !strings.Contains(joined, "跳过") {
		t.Errorf("开了跳过校验必须显眼警告，实际：%s", joined)
	}
	if !strings.Contains(joined, tlsconf.EnvVarInsecure) {
		t.Errorf("警告里要指名是哪个环境变量，实际：%s", joined)
	}
}

// TestJudgeTrustStoreUnknownSystemStoreIsNotFailure：数不出系统证书数量
// ≠ 没有证书。把「未知」当「失败」会冤枉一台本来没坏的机器。
func TestJudgeTrustStoreUnknownSystemStoreIsNotFailure(t *testing.T) {
	c := judgeTrustStore(trustEvidence{st: tlsconf.Status{
		SystemOK:    true,
		SystemNote:  "系统根证书库可用，但本平台取不到证书数量",
		BundlePath:  "/etc/skillforge/ca.pem",
		BundleCerts: 1,
	}})
	if !c.ok {
		t.Fatalf("有自定义 CA 且系统根可用（数量未知）应通过，实际 %v", c.detail)
	}
	if !strings.Contains(strings.Join(c.detail, "\n"), "取不到证书数量") {
		t.Errorf("系统根状况的说明必须原样带出来，实际：%v", c.detail)
	}
}

// ── 时区 / 时间自检项 ────────────────────────────────────────────────────
//
// 这一项的失效形态是「静默差 8 小时」：没有任何报错，只有时间不对。
// 所以判定必须区分两件修法完全不同的事：时区库没了（环境问题）vs TZ 写错了（配置问题）。

// TestJudgeTimezoneMissingZoneDatabaseIsFailure：最小化安装/精简容器里没有
// /usr/share/zoneinfo，老二进制又没内嵌时区库 —— 这时时间会静默按 UTC 走。
// 必须判失败，并且给出**离线机器也能执行**的修法（装 tzdata 包 / 换内嵌版本 / 拷目录）。
func TestJudgeTimezoneMissingZoneDatabaseIsFailure(t *testing.T) {
	c := judgeTimezone(tzEvidence{tzEnv: "Asia/Shanghai", probeErr: "unknown time zone Asia/Shanghai"})
	if c.ok {
		t.Fatalf("时区库不可用必须判失败，否则客户永远不知道时间错在哪：%v", c.detail)
	}
	joined := strings.Join(c.detail, "\n")
	for _, want := range []string{"tzdata", tzProbeZone, "差 8 小时", "/usr/share/zoneinfo"} {
		if !strings.Contains(joined, want) {
			t.Errorf("修法说明里缺 %q，客户照着修不下去。实际：\n%s", want, joined)
		}
	}
}

// TestJudgeTimezoneBadTZNameIsFailure：时区库好好的，但客户把 TZ 写成了
// 解析不了的名字。这时不能报「时区库缺失」—— 那是把客户指到错的方向。
func TestJudgeTimezoneBadTZNameIsFailure(t *testing.T) {
	c := judgeTimezone(tzEvidence{
		tzEnv: "Asia/shanghai", probeOK: true, tzResolved: "", systemDirs: []string{"/usr/share/zoneinfo"},
	})
	if c.ok {
		t.Fatalf("TZ 解析不了必须判失败：%v", c.detail)
	}
	joined := strings.Join(c.detail, "\n")
	if !strings.Contains(joined, "Asia/shanghai") {
		t.Errorf("要点名客户自己写的那个值，否则他找不到改哪里：%s", joined)
	}
	if !strings.Contains(joined, "CST") {
		t.Errorf("必须警告 CST 这类歧义名的实际后果（差 14 小时），否则客户会改成 CST 再踩一次：%s", joined)
	}
	if strings.Contains(joined, "apt-get install") {
		t.Errorf("库是好的，不该让客户去装 tzdata：%s", joined)
	}
}

// TestJudgeTimezoneHealthyReportsOffset：通过时要把「现在是几点、偏移多少」摊开，
// 让客户不必再猜服务跑在哪个时区。
func TestJudgeTimezoneHealthyReportsOffset(t *testing.T) {
	c := judgeTimezone(tzEvidence{
		tzEnv: "Asia/Shanghai", tzResolved: "Asia/Shanghai", utcOffset: "+08:00",
		localNow: "2026-09-16 21:30:00", probeOK: true, systemDirs: []string{"/usr/share/zoneinfo"},
	})
	if !c.ok {
		t.Fatalf("正常配置不该判失败：%v", c.detail)
	}
	joined := strings.Join(c.detail, "\n")
	for _, want := range []string{"Asia/Shanghai", "+08:00", "2026-09-16 21:30:00"} {
		if !strings.Contains(joined, want) {
			t.Errorf("通过时也应摊开 %q，实际：%s", want, joined)
		}
	}
}

// TestJudgeTimezoneEmbeddedFallbackIsStated：机器上没有 zoneinfo、靠内嵌兜住时，
// 必须把这件事说出来。客户下次换机器/换包时会重新踩这个坑，只有这里能告诉他原因。
func TestJudgeTimezoneEmbeddedFallbackIsStated(t *testing.T) {
	c := judgeTimezone(tzEvidence{
		tzEnv: "Asia/Shanghai", tzResolved: "Asia/Shanghai", utcOffset: "+08:00",
		localNow: "2026-09-16 21:30:00", probeOK: true, systemDirs: nil,
	})
	if !c.ok {
		t.Fatalf("内嵌时区库生效时应通过：%v", c.detail)
	}
	joined := strings.Join(c.detail, "\n")
	if !strings.Contains(joined, "内嵌") {
		t.Errorf("靠内嵌兜住时必须说清来源，否则客户换包后会以为程序坏了：%s", joined)
	}
}

// TestJudgeTimezoneUnsetTZIsNotFailureButWarnsOnUTC：没配 TZ 是合法配置，
// 不能判失败；但如果是 UTC 静默生效，必须提醒差 8 小时。
// 反例同样要锁：时间本来就是 +08:00 时不该再刷这句提示（否则提示贬值成噪音）。
func TestJudgeTimezoneUnsetTZIsNotFailureButWarnsOnUTC(t *testing.T) {
	utc := judgeTimezone(tzEvidence{
		tzResolved: "Local", utcOffset: "+00:00", localNow: "2026-09-16 13:30:00", probeOK: true,
	})
	if !utc.ok {
		t.Fatalf("没配 TZ 不该判失败（这是合法配置）：%v", utc.detail)
	}
	if !strings.Contains(strings.Join(utc.detail, "\n"), "TZ=Asia/Shanghai") {
		t.Errorf("UTC 生效时必须给出可直接抄的修法：%v", utc.detail)
	}

	cst := judgeTimezone(tzEvidence{
		tzResolved: "Local", utcOffset: "+08:00", localNow: "2026-09-16 21:30:00", probeOK: true,
	})
	if strings.Contains(strings.Join(cst.detail, "\n"), "TZ 没配") {
		t.Errorf("时间已经是对的，不该再提示改时区：%v", cst.detail)
	}
}

// TestJudgeTimezoneGorootZipIsNotReportedAsEmbedded：打包机/开发机上装了 Go，
// time.LoadLocation 会命中 $GOROOT/lib/time/zoneinfo.zip 这一级 —— 客户机上没有 Go 安装目录。
// 老实现把这种「本机 OK」报成「二进制内嵌已兜住」，等于给了客户一个假的安心：
// 他换到真正的离线裸机才发现时间差 8 小时。这里锁死：来源必须按证据报，并点明这一级客户机没有。
func TestJudgeTimezoneGorootZipIsNotReportedAsEmbedded(t *testing.T) {
	zip := "/usr/local/go/lib/time/zoneinfo.zip"
	c := judgeTimezone(tzEvidence{
		tzResolved: "Asia/Shanghai", utcOffset: "+08:00", localNow: "2026-09-16 21:30:00",
		probeOK: true, systemDirs: nil, gorootZip: zip,
	})
	if !c.ok {
		t.Fatalf("时区解析成功时不该判失败：%v", c.detail)
	}
	joined := strings.Join(c.detail, "\n")
	if !strings.Contains(joined, zip) {
		t.Errorf("必须点名命中的是 GOROOT 里那份（否则客户没法判断这份在他机器上有没有）：%s", joined)
	}
	if strings.Contains(joined, "**二进制内嵌**") {
		t.Errorf("不能断言「二进制内嵌已生效」—— 这一级的出处是 Go 安装目录，客户机上没有：%s", joined)
	}
	for _, want := range []string{"装了 Go", "客户机"} {
		if !strings.Contains(joined, want) {
			t.Errorf("必须点明这一级只在装了 Go 的机器上有、客户机通常没有，缺 %q：%s", want, joined)
		}
	}
}

// TestJudgeTimezoneBareMachineReportsEmbedded：既没有系统 zoneinfo 也没有 Go 安装目录，
// 还能解析出时区 → 只有一种解释：内嵌兜住了。这时候报「内嵌」才是可信的结论。
func TestJudgeTimezoneBareMachineReportsEmbedded(t *testing.T) {
	c := judgeTimezone(tzEvidence{
		tzResolved: "Asia/Shanghai", utcOffset: "+08:00", localNow: "2026-09-16 21:30:00",
		probeOK: true, systemDirs: nil, gorootZip: "",
	})
	if !c.ok {
		t.Fatalf("内嵌兜住时不该判失败：%v", c.detail)
	}
	joined := strings.Join(c.detail, "\n")
	if !strings.Contains(joined, "**二进制内嵌**") {
		t.Errorf("裸机上解析成功只能是内嵌兜的，必须明说：%s", joined)
	}
}
