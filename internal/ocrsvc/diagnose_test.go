package ocrsvc

import (
	"strings"
	"testing"
)

// 测试料全部是**真实原文**（2026-09-17 现场事故 1:1 复现出来的），不是手编的近似句。
// 复现方式：在 glibc 2.17 / systemd 219 的容器（manylinux2014）里直接跑线上 ocrd 产物。
const (
	realGlibcRepro = `[PYI-16:ERROR] Failed to load Python shared library '/opt/skillforge/run/ocr-tmp/_MEI00247ee9m89rGw/libpython3.11.so.1.0': /lib64/libc.so.6: version ` + "`GLIBC_2.28'" + ` not found (required by /opt/skillforge/run/ocr-tmp/_MEI00247ee9m89rGw/libpython3.11.so.1.0)`
	realSystemdOpt = `systemd-run: unrecognized option '--pipe'`
)

func TestDiagnoseGlibcTooOld(t *testing.T) {
	v := DiagnoseStartupFailure(realGlibcRepro)
	if v.Class != "glibc-too-old" {
		t.Fatalf("归类错：拿到 %q，期望 glibc-too-old（原文：%s）", v.Class, realGlibcRepro)
	}
	// 「这与文件在不在无关」是这条结论的核心：客户的第一反应一定是去找文件/重装。
	for _, want := range []string{"glibc ≥2.28", "与文件在不在无关", "构建基线"} {
		if !strings.Contains(v.Summary+v.Fixes[0], want) {
			t.Errorf("结论里缺少关键信息 %q；实际 summary=%q fixes[0]=%q", want, v.Summary, v.Fixes[0])
		}
	}
	if !strings.Contains(v.Evidence, "libpython3.11.so.1.0") {
		t.Errorf("证据里必须带原始行，实际 %q", v.Evidence)
	}
}

func TestDiagnoseSystemdOption(t *testing.T) {
	v := DiagnoseStartupFailure(realSystemdOpt)
	if v.Class != "systemd-option" {
		t.Fatalf("归类错：拿到 %q，期望 systemd-option", v.Class)
	}
	if !strings.Contains(v.Summary, "--pipe") {
		t.Errorf("结论要点名是哪个参数被拒收，实际 %q", v.Summary)
	}
	if !strings.Contains(strings.Join(v.Fixes, " "), "systemd ≥232") {
		t.Errorf("修法里要给出最低 systemd 版本，实际 %v", v.Fixes)
	}
}

func TestDiagnoseOtherClasses(t *testing.T) {
	cases := []struct {
		name, log, want string
	}{
		{"缺系统库", `ocrd: error while loading shared libraries: libGL.so.1: cannot open shared object file: No such file or directory`, "missing-lib"},
		{"权限", `Failed to load Python shared library '/opt/skillforge/run/ocr-tmp/_MEI1/libpython3.11.so.1.0': Permission denied`, "denied"},
		{"架构不符", `Failed to execute /opt/skillforge/bin/ocrd: Exec format error`, "wrong-arch"},
		{"OOM", `systemd[1]: skillforge-ocr.service: A process of this unit has been killed by the OOM killer.`, "oom"},
		{"没装（ExecStart 指向的程序不在）", `skillforge-ocr.service: Failed at step EXEC spawning /opt/skillforge/bin/ocrd: No such file or directory`, "not-installed"},
		{"运行时被清", `[Errno 2] No such file or directory: '/opt/skillforge/run/ocr-tmp/_MEI1/rapidocr_onnxruntime/config.yaml'`, "runtime-lost"},
	}
	for _, c := range cases {
		if got := DiagnoseStartupFailure(c.log).Class; got != c.want {
			t.Errorf("%s：归类 %q，期望 %q（原文 %s）", c.name, got, c.want, c.log)
		}
	}
}

// 反向钉子：正常日志绝不能被归成故障类，否则自检会在健康机器上报假红。
func TestDiagnoseHealthyLogIsUnknown(t *testing.T) {
	healthy := `started with glibc 2.35 (Ubuntu 22.04)
ocrd-v5-runtime-guard listening on 127.0.0.1:8093 (dpi=200, recycle_every=20页)
GET /health 200`
	if got := DiagnoseStartupFailure(healthy).Class; got != "unknown" {
		t.Fatalf("健康日志被误判成 %q —— 这类误判会让健康机器报假红", got)
	}
}

func TestEveryClassIsDocumented(t *testing.T) {
	// 分类是对外契约：新增分类却忘了登记，上层（自检/体检/doctor）会拿到一个没人认得的串。
	logs := []string{
		realGlibcRepro, realSystemdOpt,
		`libGL.so.1: cannot open shared object file`,
		`Permission denied`,
		`Exec format error`,
		`oom-kill`,
		`No such file or directory: /opt/skillforge/bin/ocrd`,
		`config.yaml`,
		`随便什么东西`,
	}
	doc := map[string]bool{}
	for _, c := range DocumentedClasses {
		doc[c] = true
	}
	for _, l := range logs {
		if c := DiagnoseStartupFailure(l).Class; !doc[c] {
			t.Errorf("分类 %q 没登记在 DocumentedClasses 里（输入：%s）", c, l)
		}
	}
}

func TestParseGlibcVersion(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ldd (GNU libc) 2.17", "2.17"},
		{"ldd (Ubuntu GLIBC 2.35-0ubuntu3.8) 2.35\nCopyright (C) 2022", "2.35"},
		{"glibc 2.28", "2.28"},
		{"musl libc (x86_64)\nVersion 1.2.4", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := ParseGlibcVersion(c.in); got != c.want {
			t.Errorf("ParseGlibcVersion(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}
