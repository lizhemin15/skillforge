#!/usr/bin/env python3
"""fastjson 回归防线双向自证：注入破坏 → 必须见 FAIL 行；还原 → 必须回绿。

「红在崩溃上不算红」：rc!=0 但没有 `--- FAIL` 行 = 脚本/编译崩了，不能证明断言有效。
"""
import re
import shutil
import subprocess
import sys
from pathlib import Path

# 从脚本位置推仓库根：写死 /root/skillforge 的话，换台机/换 checkout 就跑不了，
# 而「跑不了」在这类脚本里最容易被当成「没红=防线有效」。
REPO = Path(__file__).resolve().parent.parent
SRC = REPO / "internal/llm/fastjson.go"
BAK = Path("/tmp/fastjson.go.bak")

# (注入名, 原文, 替换, 期望变红的用例)
INJECTIONS = [
    (
        "摘掉 reasoning_effort（astron 那族 provider 唯一的开关）",
        '\tbody["reasoning_effort"] = "none"\n',
        "",
        "TestFastJSON_SendsKnobsThatKeepItFast",
    ),
    (
        "400 重试时把 reasoning_effort 一起摘掉（重试等于白做）",
        "\tif knob == knobBoth {\n",
        "\tif false {\n",
        "TestFastJSON_RetriesWithoutKnobOn400",
    ),
    (
        "砍掉空 content 放大预算的兜底（回到「返空就认命」）",
        # 注意：不能把整条 case 换成 `case false:` —— 那样 triedBigger 只剩赋值没人读，
        # Go 直接编译不过，rc!=0 但没有 FAIL 行，是「崩溃冒充红」。
        # 这里改成恒假的 `&& mt < 0`：所有变量照旧被读到，能编译，逻辑上等于关掉兜底。
        "\t\tcase errors.Is(err, errEmptyContent) && !triedBigger && ctx.Err() == nil && mt < 4096:\n",
        "\t\tcase errors.Is(err, errEmptyContent) && !triedBigger && ctx.Err() == nil && mt < 0:\n",
        "TestFastJSON_EmptyContentRetriesWithBiggerBudget",
    ),
    (
        "放大预算不封顶（误配一次烧一笔账）",
        "\t\t\tif mt *= 4; mt > 4096 {\n\t\t\t\tmt = 4096\n\t\t\t}\n",
        "\t\t\tmt *= 4\n",
        "TestFastJSON_EmptyRetryBudgetIsCapped",
    ),
]


def run_tests() -> tuple[int, str]:
    p = subprocess.run(
        ["/usr/local/go/bin/go", "test", "./internal/llm/", "-run", "FastJSON", "-v"],
        cwd=REPO, capture_output=True, text=True,
        env={"PATH": "/usr/local/go/bin:/usr/bin:/bin", "HOME": "/root"},
    )
    return p.returncode, p.stdout + p.stderr


def main() -> int:
    shutil.copy2(SRC, BAK)
    orig = SRC.read_text()
    bad = 0
    try:
        for name, old, new, expect in INJECTIONS:
            if old not in orig:
                print(f"✗ 注入点不存在，这条自证无意义：{name}")
                bad += 1
                continue
            SRC.write_text(orig.replace(old, new, 1))
            assert SRC.read_text() != orig, "注入没落地"
            rc, out = run_tests()
            fails = re.findall(r"^--- FAIL: (\S+)", out, re.M)
            if rc == 0:
                print(f"✗ 注入了「{name}」却全绿 —— 这条防线是假的")
                bad += 1
            elif not fails:
                print(f"✗ 注入了「{name}」：rc={rc} 但**没有任何 FAIL 行** = 崩溃，不算红")
                bad += 1
            elif expect not in fails:
                print(f"✗ 注入了「{name}」：红了但红的不是它 —— 期望 {expect}，实际 {fails}")
                bad += 1
            else:
                print(f"✓ 注入「{name}」→ FAIL: {', '.join(fails)}")
            SRC.write_text(orig)
    finally:
        SRC.write_text(orig)

    rc, out = run_tests()
    if rc != 0:
        print(f"✗ 还原后没回绿（rc={rc}）—— 注入残留或源码被改坏了")
        print(out[-1500:])
        bad += 1
    else:
        n = out.count("--- PASS")
        print(f"✓ 还原 → 全绿（{n} 个用例）")

    print("\n" + ("自证通过：防线双向有效" if bad == 0 else f"自证失败：{bad} 条"))
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
