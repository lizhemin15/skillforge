#!/usr/bin/env python3
"""变异自证：把 SplitNeeds 的 required 判定短路（任何缺参都拦 = 复现 2026-09-22 线上事故），
确认「可选字段不拦写作」那条测试精确转红；还原后必须回绿。

判据（缺一即自证失败）：
  M1 注入点存在且真的改了文件（改后字节变化）
  M2 注入后 rc != 0 且出现 FAIL 行（无 FAIL 行的 rc!=0 是崩溃红/编译红，不算）
  M3 红的正是预期那条（「可选字段不该拦下写作」），不是别的测试顺带红
  M4 还原后文件与原始字节逐字节相同，且测试回绿

为什么注入点选 `if must {` → `if must || true {` 而不是删掉声明表覆盖那段：
  事故回放用例里 needs 自报的 Required 本来就是 false，删掉覆盖仍然是绿的 ——
  那样注入出来的「红」跟真实故障无关。真实故障是**整段判据只问「缺没缺」**，
  所以注入必须打在最终那个 if 上。
"""
import subprocess
import sys
import pathlib

REPO = pathlib.Path("/root/skillforge")
SRC = REPO / "internal/agent/needs_gate.go"
OLD = "\t\tif must {\n"
NEW = "\t\tif must || true { // [MUTANT] 忽略 required：任何缺参都拦（复现线上事故）\n"
EXPECT = "可选字段不该拦下写作"

orig_bytes = SRC.read_bytes()
orig = orig_bytes.decode()
if orig.count(OLD) != 1:
    print(f"M1 FAIL 注入点不唯一：命中 {orig.count(OLD)} 次")
    sys.exit(1)


def run(tag):
    p = subprocess.run(["/usr/local/go/bin/go", "test", "./internal/agent/",
                        "-run", "TestSplitNeeds", "-v", "-count=1"],
                       cwd=REPO, capture_output=True, text=True, timeout=600)
    out = p.stdout + p.stderr
    has_fail = "--- FAIL" in out
    print(f"[{tag}] rc={p.returncode} FAIL行={has_fail}")
    for ln in out.splitlines():
        if "--- FAIL" in ln or "可选字段不该拦" in ln:
            print(f"    {ln.strip()[:160]}")
    return p.returncode, has_fail, out


try:
    SRC.write_text(orig.replace(OLD, NEW))
    after = SRC.read_bytes()
    if after == orig_bytes:
        print("M1 FAIL 写入后字节没变")
        sys.exit(1)
    print(f"M1 OK 注入完成，字节 {len(orig_bytes)} → {len(after)}")
    rc, has_fail, out = run("变异后")
    if not (rc != 0 and has_fail):
        print(f"M2 FAIL 要求 rc!=0 且有 FAIL 行，实际 rc={rc} FAIL={has_fail}")
        sys.exit(1)
    print("M2 OK 非零退出且是真红（有 FAIL 行）")
    if EXPECT not in out:
        print(f"M3 FAIL 红的不是预期那条（没看到「{EXPECT}」）")
        sys.exit(1)
    print("M3 OK 红的正是「可选字段不该拦下写作」这条")
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
