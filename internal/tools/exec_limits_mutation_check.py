#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""exec_limits_mutation_check.py —— 「沙箱限额可调 + 失败说得出原因」两把尺子的负向自证。

为什么要单独一份：`internal/tools/exec_limits_test.go` 是新加的守卫，光有正向绿
证明不了它拦得住东西——如果断言写松了（比如只查 status 非空），把代码改回
「退出码 -1」它照样绿。这份脚本负责「注入真故障 → 必须红 → 红在对的地方 → 还原回绿」。

单机离线部署的现场教训（本脚本守的就是这两个）：
  · 客户拿 17MB PDF 让模型写脚本解析，写死的 MemoryMax=256M 把它 cgroup OOM 掉，
    模型只拿到「退出码 -1」，于是猜、重试、再猜 —— 界面上就是一动不动跳秒的计时器；
  · 限额对客户完全不可调，只能改我们的源码。

四重判据（缺一即判本脚本 FAIL）：
  ① 注入点真的改了（改前/改后文件哈希不同，且改后确实含注入内容）；
  ② 注入后 go test 退出码非 0；
  ③ 并且输出里有 `--- FAIL:` 行（rc!=0 但没有 FAIL 行 = 崩溃/编译错 = 假红，不算红）；
  ④ FAIL 的正是预期那条测试；
  ⑤ 还原后文件哈希回到原值、且 go test 回绿。

还原用「精确字符串反向替换 + 哈希比对」，不用 `git checkout` —— 那会连未提交的
正常改动一起冲掉，让自证本身变成现场污染源。
"""
import hashlib
import os
import re
import subprocess
import sys

REPO = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
EXEC_GO = os.path.join(REPO, "internal", "tools", "exec.go")
TEST_GO = os.path.join(REPO, "internal", "tools", "exec_limits_test.go")

SH = "bash"


def run(cmd, **kw):
    return subprocess.run(cmd, shell=True, cwd=REPO, capture_output=True, text=True, **kw)


def sha(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def go_test(test_name):
    """跑单条测试，返回 (rc, out)。"""
    r = run(f"go test ./internal/tools/ -run '{test_name}' -count=1 2>&1")
    return r.returncode, (r.stdout or "") + (r.stderr or "")


def fail_lines(out):
    """抠出 go test 的 FAIL 行，含子测试名（--- FAIL: TestX/子测试）。"""
    return [l.strip() for l in out.splitlines() if l.strip().startswith("--- FAIL:")]


def assert_green(test_name):
    rc, out = go_test(test_name)
    if rc != 0:
        print(f"   ✗ 前置不成立：{test_name} 在未注入时就不绿（rc={rc}）")
        print(out[-2000:])
        return False
    return True


def mutate(path, old, new, what):
    """精确字符串替换。返回 (ok, 原文件内容)。"""
    src = open(path, encoding="utf-8").read()
    if src.count(old) != 1:
        print(f"   ✗ 注入点定位失败（{what}）：匹配到 {src.count(old)} 处，应为 1 处")
        return False, src
    patched = src.replace(old, new, 1)
    if patched == src:
        print(f"   ✗ 注入是 no-op（{what}）：替换后内容未变")
        return False, src
    with open(path, "w", encoding="utf-8") as f:
        f.write(patched)
    if sha(path) == hashlib.sha256(src.encode()).hexdigest():
        print(f"   ✗ 注入后文件哈希未变（{what}）")
        return False, src
    return True, src


def restore(path, original):
    with open(path, "w", encoding="utf-8") as f:
        f.write(original)


CASES = [
    # (说明, 目标文件, 原串, 注入串, 预期变红的测试)
    (
        "把超时限额改回写死 30s（客户不可调）",
        EXEC_GO,
        "Timeout:     envDuration(EnvExecTimeout, 30*time.Second),",
        "Timeout:     30 * time.Second,",
        "TestExecLimitsAreOverridable",
    ),
    (
        "把内存限额改回写死 256M（17MB PDF 必被 OOM）",
        EXEC_GO,
        'MemoryMax:   envSize(EnvExecMemory, "256M"),',
        'MemoryMax:   "256M",',
        "TestExecLimitsAreOverridable",
    ),
    (
        "把失败诊断压回一句「退出码 -1」（模型只能瞎猜重试）",
        EXEC_GO,
        "status, hint := classifySandboxFailure(exitCode, out, timedOut, t.cfg)",
        "status, hint := fmt.Sprintf(\"退出码 %d\", exitCode), \"\"\n\tif timedOut {\n\t\tstatus = fmt.Sprintf(\"超时被杀（上限 %s）\", t.cfg.Timeout)\n\t}",
        # 期望的尺子是**端到端**那条：诊断函数自己没被改，改的是调用点，
        # 所以只有走 Run() 的断言才可能红（测诊断函数的用例在这种情况下依然全绿）。
        "TestRunReportsSandboxOOMToModel",
    ),
    (
        "把 OOM 判据放宽成 Contains(out, \"memory\")（普通内存字样全被误诊）",
        EXEC_GO,
        'if strings.Contains(out, n) {\n\t\t\treturn true\n\t\t}\n\t}\n\treturn false\n}\n\n// looksLikePythonMemoryError',
        # 注入串必须**能编译**：第一版写成 `Contains(strings.ToLower(out), "memory")`
        # 把循环变量 n 弄成了未使用 → 编译错 → 输出里没有 `--- FAIL:` 行。
        # 那种 rc!=0 是崩溃红、不是断言红，自证脚本会（正确地）判它「不算数」。
        'if strings.Contains(strings.ToLower(out), strings.ToLower(n)) || strings.Contains(strings.ToLower(out), "memory") {\n\t\t\treturn true\n\t\t}\n\t}\n\treturn false\n}\n\n// looksLikePythonMemoryError',
        "TestOOMGateHasNoFalsePositives",
    ),
    (
        "给 python MemoryError 去掉词边界（numpy 的 _ArrayMemoryError 被误判）",
        EXEC_GO,
        "var pyMemoryError = regexp.MustCompile(`\\bMemoryError\\b`)",
        'var pyMemoryError = regexp.MustCompile(`MemoryError`)',
        "TestOOMGateHasNoFalsePositives",
    ),
]


def main():
    # 前置：**被本自证触碰的文件**必须干净。注入靠写文件、还原靠写回，本来不碰 git；
    # 但仍先断言干净，防止有人在脏工作区上跑，把「本来就有改动」误读成注入效果。
    # 2026-09-17：原先断言「整个工作区干净」，结果 preflight 在开发中途跑（编辑了
    # web/ 或 docs/ 之类无关文件）就整条挂掉 —— 那是假红，且会逼人把这条尺子从接线里
    # 摘掉（零引用尺子的成因之一）。改为只认「本次要注入/还原的那两个文件」，
    # 归因能力不减（注入只发生在这两个文件里），也不会被无关改动误伤。
    guarded = [EXEC_GO, TEST_GO]
    st = run("git status --porcelain -- " + " ".join(guarded)).stdout.strip()
    if st:
        print("✗ 被自证覆盖的文件有未提交改动，先提交再跑（否则无法归因注入效果）：")
        print(st[:1000])
        return 2

    # 前置：所有被守的测试在未注入时必须绿，否则「红了」不能归因于注入。
    tests = sorted({c[4] for c in CASES})
    print("== 前置：未注入时应全绿 ==")
    for t in tests:
        if not assert_green(t):
            return 1
        print(f"   ✓ {t} 绿")

    failures = []
    originals = {}
    try:
        for desc, path, old, new, expect in CASES:
            print(f"\n== 注入：{desc} ==")
            ok, orig = mutate(path, old, new, desc)
            originals[path] = orig
            if not ok:
                failures.append(desc + "（注入失败）")
                restore(path, orig)
                continue

            rc, out = go_test(expect)
            # 判据 ②：必须非零退出
            if rc == 0:
                print(f"   ✗ 注入后仍然绿（rc=0）——这把尺子拦不住这个故障：{expect}")
                failures.append(desc + "（注入后没红）")
            # 判据 ③：必须真有 FAIL 行（排除编译错/崩溃造成的假红）
            fl = fail_lines(out)
            if rc != 0 and not fl:
                print(f"   ✗ rc={rc} 但没有 `--- FAIL:` 行 = 编译错/崩溃，是假红，不算数")
                print(out[-1500:])
                failures.append(desc + "（假红：无 FAIL 行）")
            # 判据 ④：红的必须是预期那条
            elif rc != 0 and not any(expect in l for l in fl):
                print(f"   ✗ 红在了别处：{fl}，预期 {expect}")
                failures.append(desc + "（红错地方）")
            elif rc != 0:
                print(f"   ✓ 已变红且红在预期处：{fl[0]}")

            restore(path, orig)
            # 判据 ⑤：还原必须回到原内容 + 回绿
            if sha(path) != hashlib.sha256(orig.encode()).hexdigest():
                print("   ✗ 还原后文件哈希不等于原值")
                failures.append(desc + "（还原不彻底）")
            rc2, out2 = go_test(expect)
            if rc2 != 0:
                print(f"   ✗ 还原后没回绿（rc={rc2}）")
                print(out2[-1500:])
                failures.append(desc + "（还原后没回绿）")
            else:
                print("   ✓ 还原后回绿")
    finally:
        # 兜底还原：任何一个 case 抛异常都要把文件写回去，别把仓库留在注入态。
        for path, orig in originals.items():
            if sha(path) != hashlib.sha256(orig.encode()).hexdigest():
                restore(path, orig)

    print("\n" + "=" * 60)
    if failures:
        print("✗ 负向自证未通过：")
        for f in failures:
            print("   - " + f)
        return 1
    print("✓ 全部注入都被拦下，且都红在预期位置；还原后全绿")
    return 0


if __name__ == "__main__":
    sys.exit(main())
