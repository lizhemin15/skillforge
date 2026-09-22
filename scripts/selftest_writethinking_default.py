#!/usr/bin/env python3
"""变异自证：把 WriteThinkingOff() 的默认分支从「关」翻回「开」，
确认 TestPlainWriteHopDisablesThinkingByDefault 精确转红；还原后必须回绿。

判据（缺一即自证失败）：
  M1 注入点存在且真的改了文件（改后字节 + 命中数）
  M2 注入后 rc != 0 且出现 FAIL 行（无 FAIL 行的 rc!=0 是崩溃红，不算）
  M3 FAIL 必须是预期那条（enable_thinking=false 的断言），不是别的测试顺带红
  M4 还原后文件与原始字节逐字节相同，且测试回绿
"""
import os, re, shutil, subprocess, sys, pathlib

# 仓库根从脚本位置推；go 也要挑「够新的那个」。
# 2026-09-22 实测代价：这两处写死（/root/skillforge、/usr/local/go/bin/go）之后，
# 本地怎么跑都绿，CI runner 上工作目录是 /home/runner/work/skillforge/skillforge、
# go 在 setup-go 的 toolcache 里 —— 直接崩在 open() 上，红得跟断言毫无关系。
REPO = pathlib.Path(__file__).resolve().parents[1]


def resolve_go():
    """GO_BIN 钉死优先；否则按 go.mod 的要求在候选里挑第一个够新的。挑不到报环境红。"""
    m = re.search(r"^go (\d+)\.(\d+)", (REPO / "go.mod").read_text(), re.M)
    need = (int(m.group(1)), int(m.group(2))) if m else (0, 0)
    tried = []
    for c in [os.environ.get("GO_BIN"), shutil.which("go"),
              "/usr/local/go/bin/go", "/usr/bin/go"]:
        if not c or not os.path.isfile(c) or not os.access(c, os.X_OK):
            continue
        out = subprocess.run([c, "version"], capture_output=True, text=True).stdout
        vm = re.search(r"go(\d+)\.(\d+)", out)
        got = (int(vm.group(1)), int(vm.group(2))) if vm else (0, 0)
        tried.append(f"{c}(go{got[0]}.{got[1]})")
        if got >= need:
            return c
    print(f"环境红：找不到 go >= {need[0]}.{need[1]}；试过 {tried or '（无候选）'}"
          f" —— 这不是断言红，先修环境")
    sys.exit(1)


GO = resolve_go()
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
    p = subprocess.run([GO, "test", "./internal/agent/",
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
