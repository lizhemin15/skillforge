#!/usr/bin/env python3
"""内置技能「数据治理任务开发」回归防线双向自证：注入破坏 → 必须见预期 FAIL；还原 → 必须回绿。

为什么要单独有这一把（而不是「上面那 6 条用例都绿了」就完了）：
  那 6 条断言的对象是**一份提示词文本**和**一次播种行为**，它没有任何编译期约束。
  提示词里少一个 await、示例调了一个清单外的 API、播种默认开着 —— 这些都不影响
  `go build`，也不会让别的包红，只会在夜里悄悄退化。用户对这类退化的感受是
  「生成的技能跟我给的东西没关系」，是投诉项，不是理论风险。
  所以每条真故障都必须有**一条**断言精确抓到它，且这条断言必须被证明「抓得住」。

「红在崩溃上不算红」：rc!=0 但没有 `--- FAIL` 行 = 脚本/编译崩了，不能证明断言有效。
本脚本的注入全部保证**语法合法**（只改字符串内容、只删一行文档），避免用崩溃冒充红。
"""
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path

# 从脚本位置推仓库根（不写死 /root/skillforge：换 checkout 就跑不了，
# 而「跑不了」在这类脚本里最容易被当成「没红 = 防线有效」）。
REPO = Path(__file__).resolve().parent.parent
MD = REPO / "internal/store/prompts/gov_task_dev.md"
SEED = REPO / "internal/store/seed_gov_skill.go"
BAK_MD = Path("/tmp/gov_task_dev.md.bak")
BAK_SEED = Path("/tmp/seed_gov_skill.go.bak")

# 跑哪几条用例：内容自洽（TestGovTaskDev*）+ 播种行为（TestSeedGovTaskDev*）
TEST_RUN = "TestGovTaskDev|TestSeedGovTaskDev"

# 期望「全绿」时的用例数（5 条内容 + 1 条... 见 seed_gov_skill_test.go）：
# 前 3 条在 TestGovTaskDev*，后 2 条在 TestSeedGovTaskDev*。这个数字用来防
# 「-run 匹配到 0 条用例也算绿」——那是最经典的假绿。
EXPECT_PASS = 5

# (注入名, 目标文件, 原文, 替换, 期望变红的用例)
INJECTIONS = [
    (
        "示例里调了清单外的杜撰 API（模型照着抄，运行时才炸）",
        MD,
        "const aiText = await gov.callAI(prompt);",
        "const aiText = await gov.callAIXX(prompt);",
        "TestGovTaskDevPromptExamplesUseOnlyDocumentedAPIs",
    ),
    (
        "示例里漏掉 await（复制粘贴到 gov 里就是拿 Promise 当字符串用）",
        MD,
        "    const tpls = await gov.readWordTables(templateFile);",
        "    const tpls = gov.readWordTables(templateFile);",
        "TestGovTaskDevPromptExamplesUseOnlyDocumentedAPIs",
    ),
    (
        "官方清单里少一项（清单与真值脱钩，模型再也不敢用 querySQL）",
        MD,
        "- `await gov.querySQL(sql, params?)` → `[{...}]` — 对关联库执行 SELECT，返回行对象数组。参数用 `?` 占位符。\n",
        "",
        "TestGovTaskDevPromptAPISurfaceMatchesOfficial",
    ),
    (
        "播种默认开着（本该「安装即有、默认关闭」的技能偷偷对所有人上架）",
        SEED,
        "\t\tEnabled:      false,\n",
        "\t\tEnabled:      true,\n",
        "TestSeedGovTaskDevSkillDefaults",
    ),
    (
        "分类落成「通用」（业务专用能力混进通用分类，管理端里没法按场景找）",
        SEED,
        '\tgovTaskDevCategory = "业务场景"\n',
        '\tgovTaskDevCategory = "通用"\n',
        "TestSeedGovTaskDevSkillDefaults",
    ),
]


def go_bin() -> str:
    """定位 go 可执行文件；找不到给一句人话并 exit 2（工具缺失 ≠ 防线有洞）。"""
    found = shutil.which("go")
    if found:
        return found
    fallback = "/usr/local/go/bin/go"
    if os.path.exists(fallback):
        return fallback
    print("✗ 环境缺少 go 可执行文件（which go 找不到）—— 这条自证没跑，"
          "不等于防线没问题；请先装 Go 或把 Go 放进 PATH。")
    sys.exit(2)


def run_tests() -> tuple[int, str]:
    env = dict(os.environ)
    env["PATH"] = os.path.dirname(go_bin()) + os.pathsep + env.get("PATH", "")
    p = subprocess.run(
        [go_bin(), "test", "./internal/store/", "-run", TEST_RUN, "-count=1", "-v"],
        cwd=REPO, capture_output=True, text=True, env=env,
    )
    return p.returncode, p.stdout + p.stderr


def main() -> int:
    shutil.copy2(MD, BAK_MD)
    shutil.copy2(SEED, BAK_SEED)
    orig = {MD: MD.read_text(), SEED: SEED.read_text()}
    bad = 0
    try:
        for name, path, old, new, expect in INJECTIONS:
            if old not in orig[path]:
                print(f"✗ 注入点不存在，这条自证无意义：{name}")
                print(f"  目标：{path.relative_to(REPO)}")
                bad += 1
                continue
            path.write_text(orig[path].replace(old, new, 1))
            assert path.read_text() != orig[path], "注入没落地"
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
            path.write_text(orig[path])
    finally:
        MD.write_text(orig[MD])
        SEED.write_text(orig[SEED])

    rc, out = run_tests()
    if rc != 0:
        print(f"✗ 还原后没回绿（rc={rc}）—— 注入残留或源码被改坏了")
        print(out[-1500:])
        bad += 1
    else:
        n = out.count("--- PASS")
        if n != EXPECT_PASS:
            print(f"✗ 还原后跑到的用例数是 {n}，期望 {EXPECT_PASS} —— "
                  f"要么 -run 匹配漂了（跑少了会静默变绿），要么用例被删/改名了")
            bad += 1
        else:
            print(f"✓ 还原 → 全绿（{n} 个用例）")

    print("\n" + ("自证通过：防线双向有效" if bad == 0 else f"自证失败：{bad} 条"))
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
