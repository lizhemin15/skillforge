#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""分类结构管理的「注入自证」——证明回归防线真的会红，而不是一堆恒真断言。

为什么需要它：单元测试只能证明「单个函数」锚定正确，证明不了「接线」正确。
比如 `RenameCategory` 里漏了 meta.json 那一步、两张路由表只改了一张、
`renameCascadeTargets` 少列一个文件、API 层把用户的输入错误当成 500——
这些 bug 都能让用例全绿，线上却是「界面上改名成功、运行时模型还在按旧分类名找类」
或者「名字填错了却提示服务器故障」的静默失效。

做法：逐个把实现改坏（注入故障）→ 断言对应的用例必须变红 → 立刻还原 → 断言全绿。
任何一条「注入后仍然绿」都说明那条防线是假的；任何一条「红在编译错上」也不算数
（编译不过说明注入没打进去，不是断言抓到的）。

覆盖两层：
  A. internal/store  —— 级联改写的落点（6 个地方少改哪个）
  B. internal/api    —— HTTP 契约（状态码语义、请求体有没有真的传下去）

用法：
    cd <repo> && python3 scripts/category_guard_inject.py

注意：脚本会临时改写被测源码，跑完（含异常路径）必须原样还原；用 try/finally 保证。
"""
import os
import shutil
import subprocess
import sys
import tempfile

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
STORE_SRC = os.path.join(REPO, "internal", "store", "skill_categories_admin.go")
API_SRC = os.path.join(REPO, "internal", "api", "admin_categories.go")

STORE_TESTS = (
    "Category|Rewrite|DropSection|UpsertRef|RouteTable|"
    "SafeCatFileName|ValidateCategoryName|AddCategoryExample|Guard"
)
# store 层每条注入覆盖的用例（只跑相关的那些，快）
CATEGORY = "TestRenameCategory_CascadesToAllSixPlaces|TestRewritePathRefs_ExactSegmentOnly"

STORE_INJECTIONS = [
    (
        "裸子串替换：不看锚定，全量 ReplaceAll",
        "\t\t\tout, _ := rewriteAnchoredRefs(string(body), names, dirs, newName, newSafe)",
        "\t\t\tout := string(body)\n"
        "\t\t\tfor _, n := range names {\n"
        "\t\t\t\tout = strings.ReplaceAll(out, n, newName)\n"
        "\t\t\t}\n"
        "\t\t\tfor _, d := range dirs {\n"
        "\t\t\t\tout = strings.ReplaceAll(out, d, newSafe)\n"
        "\t\t\t}",
        CATEGORY,
    ),
    (
        "路径段判定退化成「全角标点也算名字的一部分」",
        "\treturn unicode.IsLetter(r) || unicode.IsDigit(r)",
        "\treturn unicode.IsLetter(r) || unicode.IsDigit(r) || r >= 0x3000",
        CATEGORY,
    ),
    (
        "改名漏掉 meta.json 清单同步（6 处只改 5 处）",
        "\tif touched, err := s.cascadeRenameMeta(slug, names, newName); err != nil {",
        "\tif touched, err := false, error(nil); err != nil {",
        CATEGORY,
    ),
    (
        "改名时目标文件少列一个（system_prompt.md / reviewer.md 留旧名）",
        "\tfor _, rel := range renameCascadeTargets(cur.File) {",
        '\tfor _, rel := range []string{"categories/_index.md", "requirement.md"} {',
        CATEGORY,
    ),
    (
        "同名改名不宽容（自己覆盖自己，误报「目录已存在」）",
        "\t\tif cur.Dir == newSafe {",
        "\t\tif false {",
        "TestRenameCategory_DisplayNameOnlyRename",
    ),
    (
        "删除时不校验 force（有范文也照删）",
        "\tif count > 0 && !force {",
        "\tif false {",
        "TestDeleteCategory_NeedsForceAndCascades",
    ),
    (
        "删除时只清 _index.md，漏 system_prompt.md（两张表只清一张）",
        'for _, rel := range []string{"categories/_index.md", "system_prompt.md"} {\n'
        "\t\ttouched, hits, err := s.cascadeFile(slug, rel, func(c string) (string, int) {\n"
        "\t\t\treturn dropRouteRow(c, names)",
        'for _, rel := range []string{"categories/_index.md"} {\n'
        "\t\ttouched, hits, err := s.cascadeFile(slug, rel, func(c string) (string, int) {\n"
        "\t\t\treturn dropRouteRow(c, names)",
        "TestDeleteCategory_NeedsForceAndCascades",
    ),
    (
        "删除时不清 meta.json 清单",
        "\tif touched, err := s.cascadeDropMeta(slug, names); err != nil {",
        "\tif touched, err := false, error(nil); err != nil {",
        "TestDeleteCategory_NeedsForceAndCascades",
    ),
]

# API 层注入：每条的意图都是「这一层唯一的职责有没有被绕过」
API_TESTS = "Category"
BAD400 = "TestCategoryAPI_BadInputIs400Not500"
API_INJECTIONS = [
    (
        "状态码不分家：用户输入错误也回 500（用户会以为是系统坏了）",
        "\tcase errors.Is(err, store.ErrCategoryBadInput),\n"
        "\t\terrors.Is(err, store.ErrCategoryInUse),\n"
        "\t\terrors.Is(err, store.ErrNotManualSkill):",
        "\tcase false && errors.Is(err, store.ErrCategoryBadInput),\n"
        "\t\tfalse && errors.Is(err, store.ErrCategoryInUse),\n"
        "\t\tfalse && errors.Is(err, store.ErrNotManualSkill):",
        BAD400,
    ),
    (
        "删除时把 force 写死为真（有范文也照删，二次确认形同虚设）",
        '\tforce := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"',
        '\tforce := true || r.URL.Query().Get("force") == "true"',
        BAD400,
    ),
    (
        "新增分类时把写作要求丢了（表单填了却不落盘）",
        "\tch, err := a.store.CreateCategory(slug, body.Name, body.Trigger, body.Requirement)",
        '\tch, err := a.store.CreateCategory(slug, body.Name, body.Trigger, "")',
        "TestCreateCategoryAPI",
    ),
    (
        "改名时新名没传下去（只当显示名改，级联全不发生）",
        "\tch, err := a.store.RenameCategory(slug, body.File, body.NewName)",
        "\tch, err := a.store.RenameCategory(slug, body.File, body.File)",
        "TestRenameCategoryAPI_CascadesAndReports",
    ),
]

TARGETS = [
    ("store", STORE_SRC, "./internal/store/", STORE_INJECTIONS, STORE_TESTS),
    ("api", API_SRC, "./internal/api/", API_INJECTIONS, API_TESTS),
]


def go_test(pkg: str, run: str) -> str:
    env = dict(os.environ)
    env["PATH"] = "/usr/local/go/bin:" + env.get("PATH", "")
    p = subprocess.run(
        ["go", "test", pkg, "-run", run, "-count=1"],
        cwd=REPO,
        env=env,
        capture_output=True,
        text=True,
    )
    return p.stdout + p.stderr


def run_target(label: str, target: str, pkg: str, injections, green_run: str) -> int:
    with open(target, encoding="utf-8") as f:
        original = f.read()
    backup = os.path.join(tempfile.gettempdir(), os.path.basename(target) + ".bak")
    shutil.copy(target, backup)
    leaked = 0
    try:
        for name, old, new, run in injections:
            if original.count(old) != 1:
                # 锚点失效 = 注入没打进去，必须报错而不是当成功；
                # 悄悄跳过会让「全绿」变成假象。
                print("[锚点失效 ✗] %s / %s（实现改了？同步更新本脚本）" % (label, name))
                leaked += 1
                continue
            with open(target, "w", encoding="utf-8") as f:
                f.write(original.replace(old, new))
            out = go_test(pkg, run)
            compile_error = "build failed" in out or "cannot use" in out
            red = ("FAIL" in out) and not compile_error
            if not red:
                leaked += 1
            print(
                "[%s] %s / %s"
                % (
                    "红 ✓"
                    if red
                    else ("编译错 ✗（不算红）" if compile_error else "仍然绿 ✗ 防线是假的"),
                    label,
                    name,
                )
            )
            with open(target, "w", encoding="utf-8") as f:
                f.write(original)
    finally:
        with open(target, "w", encoding="utf-8") as f:
            f.write(original)
        assert open(target, encoding="utf-8").read() == original

    green = go_test(pkg, green_run)
    restored_ok = "FAIL" not in green
    print("%s 还原后全绿：%s" % (label, "是 ✓" if restored_ok else "否 ✗\n" + green))
    if not restored_ok:
        leaked += 1
    return leaked


def main() -> int:
    total = 0
    leaked = 0
    for label, target, pkg, injections, green_run in TARGETS:
        total += len(injections)
        leaked += run_target(label, target, pkg, injections, green_run)
    print()
    if leaked:
        print("结论：防线有洞（%d 条注入没被抓住）" % leaked)
        return 1
    print("结论：%d 条注入全部被抓住，且还原后全绿。" % total)
    return 0


if __name__ == "__main__":
    sys.exit(main())
