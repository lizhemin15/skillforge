#!/usr/bin/env python3
"""变异自证：疑点回执（首轮疑点闸门）那一批尺子。

用户投诉的原话是「用户对话给的信息可是重中之重」——贴一段话进去，AI 却回一张
通用要素清单问「请补充标题/亮点/受众」。修法是「疑点回执」：拦之前先逐字引用用户
原话挑 0~3 条真疑点，挑不出/引用对不上/超时一律不拦，带假设直写。

这批判据**全是「不许对用户做什么」的负向约束**，最容易写成永远不会红的装饰：
只要哪天有人把停问分支改回通用清单、或把关思考链的开关摘掉，测试照样全绿，用户
再被问一遍要素清单。所以这里对每一条判据做**真故障注入**，缺一即自证失败：

  ① 注入点存在（改前能在源文件里找到，且改后字节确实变了）—— 否则测的是空气
  ② 注入后 rc != 0 **且出现 `--- FAIL:` 行**（只 rc!=0 的可能是编译红/崩溃红，不算）
  ③ 红的正是预期那条判据的**预期那句话**（不是别的用例顺带红）
  ④ 还原后文件与原始字节逐字节相同，且测试回绿

为什么每条判据都要有：这轮 8 条注入里，唯一没红的那条（封顶 3 条）暴露了一把
一直在自己骗自己的尺子——被测列里有两条被别的规则顺手丢掉，于是「取消封顶」也
照样绿（详见 regression-guard-quality/references/cap-assertion-premise-collapse.md）。
「注入没红」的默认解读是先怀疑尺子。
"""
import os
import pathlib
import re
import shutil
import subprocess
import sys

# 仓库根从脚本自己的位置推，不写死 /root/skillforge（CI runner 上工作目录不同，
# 写死会红在一个跟断言毫无关系的地方，报的还是「权限」这种把人引去查权限的假象）。
REPO = pathlib.Path(__file__).resolve().parents[1]


def resolve_go():
    """挑一个够新的 go：本机 PATH 里可能是老的，CI runner 上也可能没有 /usr/local/go/bin/go。
    候选都试一遍按 go.mod 筛；一个都不合格报**环境红**（别退化成断言红把人引去修判据）。"""
    m = re.search(r"^go (\d+)\.(\d+)", (REPO / "go.mod").read_text(), re.M)
    need = (int(m.group(1)), int(m.group(2))) if m else (0, 0)
    cands = [os.environ.get("GO_BIN"), shutil.which("go"),
             "/usr/local/go/bin/go", "/usr/bin/go"]
    tried = []
    for c in cands:
        if not c or not os.path.isfile(c) or not os.access(c, os.X_OK):
            continue
        v = subprocess.run([c, "version"], capture_output=True, text=True).stdout
        vm = re.search(r"go(\d+)\.(\d+)", v)
        got = (int(vm.group(1)), int(vm.group(2))) if vm else (0, 0)
        tried.append(f"{c}(go{got[0]}.{got[1]})")
        if got >= need:
            return c
    print(f"环境红：找不到 go >= {need[0]}.{need[1]}；试过 {tried or '（无候选）'}"
          f" —— 设 GO_BIN=... 指一个（这不是断言红，先修环境）")
    sys.exit(1)


GO = resolve_go()

# (说明, 文件, 原文, 注入后, -run 的用例, 期望在输出里看到的话)
MUTANTS = [
    ("判据·引用不查原文（放开：模型编的引用也留下）",
     "internal/agent/doubts.go",
     "if len([]rune(q)) < minQuoteRunes || !strings.Contains(hay, q) {",
     "if len([]rune(q)) < minQuoteRunes {",
     "TestValidateDoubtsDropsQuoteNotInHaystack",
     "引用在原文里根本不存在却留下了"),

    ("判据·泛问门槛从 6 字降到 2 字（「待定」也算有默认理解）",
     "internal/agent/doubts.go",
     "\tminInferRunes = 6",
     "\tminInferRunes = 2",
     "TestValidateDoubtsDropsThinInference",
     "太泛"),

    ("判据·去掉 ≤3 条封顶（回到填表式追问）",
     "internal/agent/doubts.go",
     "\t\tif len(out) >= maxDoubts {",
     "\t\tif len(out) >= 99 {",
     "TestValidateDoubtsCapsAtThree",
     "疑点必须封顶"),

    ("判据·把模型自己的回答也算「原文」（自证式引用）",
     "internal/agent/doubts.go",
     "\t\tif history[i].Role != \"user\" {\n\t\t\tcontinue\n\t\t}",
     "\t\tif false {\n\t\t\tcontinue\n\t\t}",
     "TestDoubtHaystackCoversPriorUserTurnsOnly",
     "模型自己以前的回答进了引用查找域"),

    ("接线·疑点跳不关思考链（慢到用户以为卡死）",
     "internal/agent/doubts.go",
     "\t\tDisableThinking: true,\n\t\tJSONMode:        true,",
     "\t\tDisableThinking: false,\n\t\tJSONMode:        true,",
     "TestRaiseDoubtsHopIsFastAndAnchoredOnRawText",
     "疑点跳必须带 enable_thinking=false"),

    ("接线·停问退回通用要素清单（用户投诉的原行为）",
     "internal/api/chat.go",
     "\t\t\t\tmsg := agent.DoubtsMessage(doubts)",
     "\t\t\t\tmsg := \"我需要你补充以下信息：\" + agent.NeedsSummary(blocking)",
     "TestNeedsGateAnchoredDoubtStillAsks",
     "没有逐字引用用户原话"),

    ("接线·坏输出加回「兜底必问」（宁可拦错不可放过）",
     "internal/api/chat.go",
     "\t\t\t\twrite(evMeta, jsonSafe(map[string]string{\n"
     "\t\t\t\t\t\"note\": \"无疑点，开始写：「\" + agent.NeedsSummary(blocking) +",
     "\t\t\t\twrite(evNeeds, jsonSafe(blocking))\n"
     "\t\t\t\twrite(evDelta, jsonSafe(map[string]string{\"t\": \"我需要你补充以下信息：\" + "
     "agent.NeedsSummary(blocking)}))\n"
     "\t\t\t\twrite(evMeta, jsonSafe(map[string]string{\n"
     "\t\t\t\t\t\"note\": \"无疑点，开始写：「\" + agent.NeedsSummary(blocking) +",
     "TestNeedsGateGarbageDoubtsStillWrites",
     "疑点输出坏了却拦下了用户"),

    ("接线·首轮原话没传进疑点跳（用户给的信息丢失）",
     "internal/api/chat.go",
     "\t\t\trep, derr := h.eng.RaiseDoubts(ctx, sc.Name, blocking, req.Message, fullHist)",
     "\t\t\trep, derr := h.eng.RaiseDoubts(ctx, sc.Name, blocking, \"\", nil)",
     "TestNeedsGateAnchoredDoubtStillAsks",
     "有真疑点时闸门没拦"),
]


def run_tests(name):
    p = subprocess.run([GO, "test", "./internal/agent/", "./internal/api/",
                        "-run", name, "-count=1"],
                       cwd=str(REPO), capture_output=True, text=True)
    return p.returncode, p.stdout + p.stderr


def main():
    fails = []
    for label, rel, old, new, test, expect in MUTANTS:
        src_path = REPO / rel
        orig = src_path.read_bytes()
        text = orig.decode()
        n = text.count(old)
        if n == 0:
            fails.append(f"{label}：判据① 注入点不存在（{rel} 里找不到那段），"
                         f"这条尺子已经和实现脱钩，先修脚本")
            continue
        src_path.write_text(text.replace(old, new, 1))
        rc, out = run_tests(test)
        red_lines = [l for l in out.splitlines() if "--- FAIL:" in l]
        hit = expect in out
        if not red_lines:
            fails.append(f"{label}：判据② 注入后没有 `--- FAIL:` 行（rc={rc}）——"
                         f"这不是「红」，是没跑到/编译不过：{out[-400:]}")
        elif not hit:
            fails.append(f"{label}：判据③ 红了但红在别的地方，没看到「{expect}」："
                         f"{red_lines[0]}")
        elif rc == 0:
            fails.append(f"{label}：判据② 有 FAIL 行却 rc=0（CI 里等于没挂）")
        src_path.write_bytes(orig)
        rc2, out2 = run_tests(test)
        if src_path.read_bytes() != orig:
            fails.append(f"{label}：判据④ 还原后字节不一致")
        if rc2 != 0 or "--- FAIL:" in out2 or "FAIL" in out2:
            fails.append(f"{label}：判据④ 还原后没回绿（rc={rc2}）：{out2[-300:]}")
        if not any(label in f for f in fails):
            print(f"  ✅ {label}")
    if fails:
        print("\n变异自证失败（这把尺子守不住它声称守的东西）：")
        for f in fails:
            print(f"  ❌ {f}")
        sys.exit(1)
    print(f"\n变异自证通过：{len(MUTANTS)}/{len(MUTANTS)} 条注入全部精确转红、还原回绿。")


if __name__ == "__main__":
    main()
