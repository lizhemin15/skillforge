package main

import (
	"errors"
	"strings"
	"testing"
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
func TestJudgeSandboxEmptyUIDIsNotBlamedOnPrivilegeDrop(t *testing.T) {
	got := judgeSandbox(map[string]string{
		"network":    "断(OK)",
		"write:work": "可以(OK)",
	}, []string{"/opt/skillforge/skillforge.env"}, nil)

	if got.ok {
		t.Fatalf("没拿到 uid 证据却判 ok=true（假绿灯）：%v", got.detail)
	}
	joined := strings.Join(got.detail, "\n")
	if !strings.Contains(joined, "python3") {
		t.Fatalf("明细没点出真因 python3：%v", got.detail)
	}
	if strings.Contains(joined, "没有降权") {
		t.Fatalf("把「证据缺失」误诊成「没有降权」——这条误诊必须绝迹：%v", got.detail)
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

func containsAny(lines []string, needle string) bool {
	for _, l := range lines {
		if strings.Contains(l, needle) {
			return true
		}
	}
	return false
}
