#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""selftest_user_raw_ruler.py —— 变异自证：证明「本轮原话必须进执笔跳」这把尺子真的会红。

背景：线上投诉「生成的 skill 跟我给的内容完全没关系」。根因是用户原话压根没进执笔跳
prompt（chat.go 在 Push 之前取 history；GenerateWithPack/GenerateWithPlan 没 userMsg）。
守卫测试在 internal/agent/user_raw_prompt_test.go。

为什么守卫测试还需要这个脚本：测试全绿有两种可能——(a) 接线真的在；(b) 测试自己写歪了，
漏断言了。本脚本逐条把接线**真的拆掉**，要求测试精确转红（不是崩溃红、不是别的用例红），
再还原要求回绿。缺任何一条判据都算这把尺子不合格。

四条判据（缺一不可）：
  ① 注入点存在：替换串必须在文件里找得到，且替换后文件内容真的变了（否则是假注入→假绿）；
  ② 注入后必须出现 FAIL 行（rc!=0 但没有 FAIL 行＝崩溃红，不算数）；
  ③ FAIL 必须是**预期那一条**用例（僵尸红不算数）；
  ④ 还原后必须回绿。

用法：python3 scripts/selftest_user_raw_ruler.py   （rc=0 全绿；rc=1 有判据不成立）
"""

import os
import re
import shutil
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
AGENT = os.path.join(ROOT, "internal/agent/agent.go")
# 注意：这里必须是「拆好的参数列表」，不能写成带单引号的一整串再 split ——
# 本脚本第一版正是那样写的，结果 `-run "'A|B|C'"` 被 Go 当成字面正则，4 条用例里
# 只有中间那条真的跑起来，其余三条「变异后全绿」全是**空跑绿**。判据①′ 就是为了
# 永久钉住这一类：基线必须看到每条预期用例都出 `--- PASS` 行。
CASES = [
    "TestWriteHopCarriesUserWordsVerbatim",
    "TestManualDraftHopCarriesUserWordsVerbatim",
    "TestUserRawBlockOmittedWhenNoUserText",
]
GO_TEST = ["go", "test", "./internal/agent/", "-run", "|".join(CASES), "-count=1", "-v"]

# 三处注入：执笔跳、构思跳、占位符禁令。每处都必须被自己的用例抓住。
MUTATIONS = [
    (
        "执笔跳不带原话",
        AGENT,
        "userRawBlock(sc, userMsg)+argBlockOf(sc, args)",
        "argBlockOf(sc, args)",
        "TestWriteHopCarriesUserWordsVerbatim",
    ),
    (
        "构思跳不带原话",
        AGENT,
        "lead = userRawBlock(sc, userMsg) + argBlockOf(sc, args)",
        "lead = argBlockOf(sc, args)",
        "TestWriteHopCarriesUserWordsVerbatim",
    ),
    (
        "注入了但没禁占位符原样输出",
        AGENT,
        '"把 {xxx} 原样写进正文视为不合格。\\n"',
        '"\\n"',
        "TestWriteHopCarriesUserWordsVerbatim",
    ),
    (
        "原话块排到了抽参块后面",
        AGENT,
        "lead = userRawBlock(sc, userMsg) + argBlockOf(sc, args)",
        "lead = argBlockOf(sc, args) + userRawBlock(sc, userMsg)",
        "TestWriteHopCarriesUserWordsVerbatim",
    ),
    (
        "空 userMsg 也硬塞原话块",
        AGENT,
        '\tm := strings.TrimSpace(userMsg)\n\tif m == "" {\n\t\treturn ""\n\t}',
        '\tm := strings.TrimSpace(userMsg)',
        "TestUserRawBlockOmittedWhenNoUserText",
    ),
]

FAIL_RE = re.compile(r"^--- FAIL: (\S+)", re.M)
PASS_RE = re.compile(r"^--- PASS: (\S+)", re.M)


def run_tests():
    p = subprocess.run(GO_TEST, cwd=ROOT, capture_output=True, text=True, timeout=900)
    out = p.stdout + p.stderr
    return p.returncode, out, FAIL_RE.findall(out), PASS_RE.findall(out)


def main():
    fails = []

    # 基线：必须先全绿，否则「转红」的对照没有意义（本来就红的尺子抓不出变异）。
    rc, out, names, passed = run_tests()
    if rc != 0:
        print("基线不绿，先修测试再谈自证：\n" + out[-3000:])
        return 1
    # 判据①′：每条预期用例都必须真的跑过并且 PASS。缺一条＝空跑绿：
    # 变异拆掉接线它也不会红，因为根本没执行。
    missing = [c for c in CASES if c not in passed]
    if missing:
        print("判据①′ 基线里这些用例根本没跑起来（空跑绿）：%s" % missing)
        print("实际 PASS：%s" % passed)
        print(out[-2000:])
        return 1
    print("[基线] %d/%d 绿 ✓（每条预期用例都有 PASS 行）" % (len(CASES), len(CASES)))

    for label, path, old, new, expect in MUTATIONS:
        backup = path + ".selftest.bak"
        shutil.copy2(path, backup)
        try:
            src = open(path, encoding="utf-8").read()
            # 判据①：注入点必须存在，且替换必须真的改动文件。
            if src.count(old) < 1:
                fails.append(f"{label}: 判据① 注入点没找到（替换串没对上）→ 假注入")
                continue
            mutated = src.replace(old, new)
            if mutated == src:
                fails.append(f"{label}: 判据① 替换后文件没变 → 假注入")
                continue
            open(path, "w", encoding="utf-8").write(mutated)

            rc, out, names, _ = run_tests()
            # 判据②：必须有 FAIL 行。rc!=0 但零 FAIL 行＝崩溃红（编译错/超时），不算数。
            if rc == 0:
                fails.append(f"{label}: 判据② 拆了接线测试还全绿 → 尺子是假的")
                continue
            if not names:
                fails.append(f"{label}: 判据② rc={rc} 但没有 FAIL 行（崩溃红不算红）:\n" + out[-1500:])
                continue
            # 判据③：红的必须是预期那条。
            if expect not in names:
                fails.append(f"{label}: 判据③ 红的是 {names}，预期 {expect} → 僵尸红")
                continue
            print(f"[变异] {label} → {expect} 精确转红 ✓")
        finally:
            shutil.move(backup, path)

    # 判据④：还原后回绿（也顺带证明上面的还原真的还原干净了）。
    rc, out, names, passed = run_tests()
    if rc != 0 or names:
        fails.append("判据④ 还原之后没回绿，工作树被自证脚本弄脏了:\n" + out[-1500:])
    rest_passed = [c for c in CASES if c in passed]
    if len(rest_passed) != len(CASES):
        fails.append("判据④ 还原后虽然 rc=0，但有用例没跑起来（空跑绿）：%s" % rest_passed)
    else:
        print("[还原] %d/%d 回绿 ✓" % (len(CASES), len(CASES)))


    if fails:
        print("\n自证不通过：")
        for f in fails:
            print(" - " + f)
        return 1
    print("\n自证通过：5/5 变异均被预期用例精确抓住，还原回绿。rc=0")
    return 0


if __name__ == "__main__":
    sys.exit(main())
