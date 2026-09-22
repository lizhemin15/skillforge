#!/usr/bin/env python3
"""变异自证：把 WriteThinkingOff() 的默认分支从「关」翻回「开」，
确认 TestPlainWriteHopDisablesThinkingByDefault 精确转红；还原后必须回绿。

判据（缺一即自证失败）：
  M1 注入点存在且真的改了文件（改后字节 + 命中数）
  M2 注入后 rc != 0 且出现 FAIL 行（无 FAIL 行的 rc!=0 是崩溃红，不算）
  M3 FAIL 必须是预期那条（enable_thinking=false 的断言），不是别的测试顺带红
  M4 还原后文件与原始字节逐字节相同，且测试回绿
"""
import subprocess, sys, pathlib

REPO = pathlib.Path("/root/skillforge")
SRC = REPO / "internal/llm/stream.go"
TEST = "TestPlainWriteHopDisablesThinkingByDefault"
OLD = '\tdefault:\n\t\treturn true // 默认关：快是第一位的，且实测只掉 26% 长度\n'
NEW = '\tdefault:\n\t\treturn false // [MUTANT] 故意翻回「开着思考」\n'

orig_bytes = SRC.read_bytes()
orig = orig_bytes.decode()
if orig.count(OLD) != 1:
    print(f"M1 FAIL 注入点不唯一：命中 {orig.count(OLD)} 次")
    sys.exit(1)

def run(tag):
    p = subprocess.run(["/usr/local/go/bin/go", "test", "./internal/agent/",
                        "-run", TEST, "-v", "-count=1"],
                       cwd=REPO, capture_output=True, text=True, timeout=600)
    out = p.stdout + p.stderr
    has_fail = "\n--- FAIL" in out or "--- FAIL" in out
    print(f"[{tag}] rc={p.returncode} FAIL行={has_fail}")
    for ln in out.splitlines():
        if "enable_thinking" in ln or "--- FAIL" in ln or "thinking_budget" in ln:
            print(f"    {ln.strip()[:150]}")
    return p.returncode, has_fail, out

try:
    SRC.write_text(orig.replace(OLD, NEW))
    print(f"M1 OK 注入完成，字节 {len(orig_bytes)} → {len(SRC.read_bytes())}")
    rc, has_fail, out = run("变异后")
    if not (rc != 0 and has_fail):
        print(f"M2 FAIL 要求 rc!=0 且有 FAIL 行，实际 rc={rc} FAIL={has_fail}")
        sys.exit(1)
    print("M2 OK 非零退出且是真红（有 FAIL 行）")
    if "enable_thinking" not in out:
        print("M3 FAIL 红的不是预期那条（没看到 enable_thinking 断言）")
        sys.exit(1)
    print("M3 OK 红的正是「默认必须 enable_thinking=false」这条")
finally:
    SRC.write_bytes(orig_bytes)

same = SRC.read_bytes() == orig_bytes
print(f"M4 还原字节一致={same}（{len(orig_bytes)} B）")
rc2, has_fail2, _ = run("还原后")
if not same or rc2 != 0 or has_fail2:
    print(f"M4 FAIL 还原后没回绿：rc={rc2} FAIL={has_fail2}")
    sys.exit(1)
print("M4 OK 还原后逐字节一致且回绿")
print("MUT_SELFTEST_RC=0 四重判据全过")
