package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 本文件的锚：2026-09-17 客户离线部署现场。
//
// 客户在**解包目录里**跑装前体检（`bin/skillforge -diag`）。那时可执行文件自己就在 bin/
// 下面，如果候选表里只有「Dir(exe)/bin/ocrd」，指向的就是 `<包根>/bin/bin/ocrd`（不存在），
// 于是回退到 /opt/skillforge/bin/ocrd —— 而那是**上一次安装留下的旧 ocrd**。
// 后果不是「体检报错」，而是「体检拿另一份产物的基线给客户下结论」：旧基线恰好匹配时，
// 会把「这份包在这台机器上根本装不起来」报成没问题。所以候选顺序必须被真文件布局钉住。

// TestDiagOCRCandidatesOrder —— 包内布局必须排在回退路径之前，且同目录候选必须在列。
func TestDiagOCRCandidatesOrder(t *testing.T) {
	got := diagOCRCandidates("/pkg/bin/skillforge")
	if len(got) != 3 {
		t.Fatalf("候选数 = %d，期望 3：%v", len(got), got)
	}
	// 装机布局：<前缀>/skillforge + <前缀>/bin/ocrd
	if want := filepath.Join("/pkg/bin", "bin", "ocrd"); got[0] != want {
		t.Errorf("候选[0] = %q，期望 %q（装机布局优先）", got[0], want)
	}
	// 解包布局：<包根>/bin/skillforge + <包根>/bin/ocrd（**并列**）
	if want := filepath.Join("/pkg/bin", "ocrd"); got[1] != want {
		t.Errorf("候选[1] = %q，期望 %q（同目录并列布局；缺了它就会误用旧安装的 ocrd）", got[1], want)
	}
	if got[2] != "/opt/skillforge/bin/ocrd" {
		t.Errorf("候选[2] = %q，期望回退到 /opt/skillforge/bin/ocrd", got[2])
	}
}

// TestPickOCRBinaryUnpackedLayout —— 真临时目录：包内布局下必须挑到同目录并列的那份 ocrd。
func TestPickOCRBinaryUnpackedLayout(t *testing.T) {
	pkg := t.TempDir()
	if err := os.MkdirAll(filepath.Join(pkg, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(pkg, "bin", "skillforge")
	ocrd := filepath.Join(pkg, "bin", "ocrd")
	writeFile(t, exe)
	writeFile(t, ocrd)

	if got := pickOCRBinary(exe); got != ocrd {
		t.Fatalf("pickOCRBinary(%q) = %q，期望 %q", exe, got, ocrd)
	}
}

// TestPickOCRBinaryPrefersInstalledLayout —— 装机后布局：<前缀>/bin/ocrd 优先。
func TestPickOCRBinaryPrefersInstalledLayout(t *testing.T) {
	prefix := t.TempDir()
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(prefix, "skillforge")
	installed := filepath.Join(prefix, "bin", "ocrd")
	writeFile(t, exe)
	writeFile(t, installed)
	// 同一前缀下同时存在「同目录的 ocrd」（非标准布局）时，装机布局仍必须优先。
	writeFile(t, filepath.Join(prefix, "ocrd"))

	if got := pickOCRBinary(exe); got != installed {
		t.Fatalf("pickOCRBinary(%q) = %q，期望 %q", exe, got, installed)
	}
}

// TestPickOCRBinaryIgnoresDirectory —— 目录不能当成 ocrd（包里误建了同名目录时，
// 拿它去 exec 会得到一句与真实原因无关的报错）。
//
// 注意这里**不能**断言「返回空串」：候选表最后一条是绝对回退路径 /opt/skillforge/bin/ocrd，
// 在装过 SkillForge 的机器（CI 上的构建机、客户机上）它真实存在，返回它才是对的。
// 本用例只盯一件事：目录不算文件。（第一版就是这么写错的 —— 尺子的缺陷，不是出品的缺陷。）
func TestPickOCRBinaryIgnoresDirectory(t *testing.T) {
	pkg := t.TempDir()
	dirPath := filepath.Join(pkg, "bin", "ocrd")
	if err := os.MkdirAll(dirPath, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(pkg, "bin", "skillforge")
	writeFile(t, exe)

	got := pickOCRBinary(exe)
	if got == dirPath {
		t.Fatalf("同名目录被当成了 ocrd：%q", got)
	}
	if _, err := os.Stat("/opt/skillforge/bin/ocrd"); os.IsNotExist(err) && got != "" {
		t.Fatalf("pickOCRBinary(%q) = %q，本机没有绝对回退路径时期望空串", exe, got)
	}
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}
