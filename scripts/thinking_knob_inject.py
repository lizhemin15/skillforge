#!/usr/bin/env python3
"""变异自证：把「提速 + 中间材料」这轮每条断言各注入一个**真故障**，确认它会红，再还原确认回绿。

为什么必须有这个脚本：
  这一轮的改动横跨 Go（思考开关、材料挂载、正文/思考分离）和前端（材料渲染）。Go 侧
  `go test ./...` 全绿、SSE 侧 `chat-sse-timeline.py` 全绿，都只说明**现在**没坏，不说明
  这些断言抓得住坏。一条写歪的断言（命中注释、只比子串、条件写反、`-run` 里测试名打错
  导致根本没跑）永远是绿的 —— 这种假绿比没测试更危险，因为它让人以为这块有防线。

判据（四重，缺一不可）：
  1. 注入点必须存在且**唯一**（找不到 / 命中多处 = 注入无效 = 等于没测，直接不合格）
  2. 注入后**必须出现 FAIL 行**。「红在崩溃上不算红」—— rc!=0 但没有 FAIL 行，说明注入把
     代码改到跑不起来了，那种红证明不了任何断言有效。
  3. FAIL 必须是**预期那条**。改坏 A 却让 B 跳红线，等于这条断言没在盯它该盯的东西。
  4. 还原后必须回绿。

本脚本比同族脚本多守两件事（都是真踩过的洞）：
  * **环境红 ≠ 断言红**：go 工具链太旧 / 依赖拉不下来 / 编译失败时，rc!=0 且输出里也可能
    出现 FAIL 字样（甚至什么 FAIL 都没有），一眼看去像「注入成功让它红了」，其实一条断言
    都没跑到。这里显式识别编译/工具链失败签名，单独报「环境红」，并让整脚本以 2 退出，
    跟「断言没抓住」区分开 —— 否则你会去改断言，而真正该修的是环境。
  * **测试必须真的跑过**：还原那一步用 `-v` / 逐个断言名确认预期用例出现 `--- PASS`。
    否则 `-run` 名字打错时「还原后回绿」也是假的（压根没跑）。

只碰仓库工作区，任何路径都还原（含异常退出）。

用法：python3 scripts/thinking_knob_inject.py
"""
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

# go 用占位符，真正的可执行文件在 main() 里按 go.mod 要求现挑（见 resolve_go）。
#
# 为什么不写死绝对路径：本地这台机器 PATH 里躺着 1.18 的 /usr/bin/go（解析不了 go.mod 里的
# `go 1.25.0`，报 "must match format 1.23"），而 CI runner 上 setup-go 装在
# /opt/hostedtoolcache/... —— 根本没有 /usr/local/go/bin/go。写死哪一边都会在另一边炸：
# 本地炸成"每条注入都红"，CI 炸成 FileNotFoundError。两边看着都像测试红，其实一条断言
# 都没跑到。
GO_MARK = "@GO@"

# 编译/工具链失败的签名：出现这些就不算「断言抓住了故障」。
ENV_RED = (
    "errors parsing go.mod",
    "must match format",
    "invalid go version",
    "[build failed]",
    "cannot find package",
    "no required module provides",
    "cannot find module",
    "syntax error",
    "toolchain",
)

# (名字, 文件, 原文, 替换, 测试命令, 预期失败的用例名, 说明)
MUTATIONS = [
    (
        "M1 材料不挂到进行中那一步", "internal/api/chat_trace.go",
        "\t\tout[i].Material = mat",
        "\t\t_ = mat // 注入：材料不挂上去",
        [GO_MARK, "test", "-v", "./internal/api/", "-run", "TestTraceMaterial", "-count=1"],
        "TestTraceMaterialAttachesToActiveStep",
        "后端发了材料但没挂到 active 那一步 → 前端拿不到，用户又只剩跳秒的计时器。",
    ),
    (
        "M2 关思考链的开关没发出去", "internal/llm/stream.go",
        '\t\tbody["enable_thinking"] = false',
        '\t\t_ = "注入：不发 enable_thinking"',
        [GO_MARK, "test", "-v", "./internal/llm/", "-run", "TestStreamChat", "-count=1"],
        "TestStreamChatSendsBothThinkKnobs",
        "开关没进请求体 → 模型继续吐几十秒思考，提速归零。",
    ),
    (
        # 锚点跟着实现走（2026-09-19 漂过一次）：分类跳的 StreamOpts 从「直接写在
        # StreamChat 调用里」改成了先建 `opts` 变量（为了给 OnContent 挂埋点），
        # 老锚点整段不存在了 → 脚本按规矩判「注入点没找到」→ CI 红。
        # 那次红是对的：锚点漂了就等于这把尺子不再量任何东西。
        # 现在锚在 `MaxTokens: classifyMaxTokens(),` 这一行上 —— 它是这一跳独有的。
        "M3 分类那一跳不再请求关思考链", "internal/agent/agent.go",
        "\t\tDisableThinking: true,\n\t\tJSONMode:        true,\n\t\tMaxTokens:       classifyMaxTokens(),\n\t\tOnReasoning:     reasoningSink(ctx),\n\t}",
        "\t\tDisableThinking: false,\n\t\tJSONMode:        true,\n\t\tMaxTokens:       classifyMaxTokens(),\n\t\tOnReasoning:     reasoningSink(ctx),\n\t}",
        [GO_MARK, "test", "-v", "./internal/agent/", "-run", "TestEvalTurn", "-count=1"],
        "TestEvalTurnAsksProviderToDisableThinking",
        "接线回归：分类那一跳又把思考链打开了 —— 单元/SSE 全绿，线上慢回 40s。",
    ),
    (
        "M4 思考链漏进正文", "internal/llm/stream.go",
        # 锚点跟着实现走：加了空正文守卫（reasonChunks 计数）之后，
        # 原来那句 `r != "" && o.OnReasoning != nil` 已经不存在了。
        # 脚本故意「锚点找不到就判失败」——它替我们发现了这次漂移，别把这条判据删了。
        '\t\t\tif r := ch.Delta.ReasoningContent; r != "" {\n\t\t\t\treasonChunks++\n\t\t\t\tif o.OnReasoning != nil {\n\t\t\t\t\to.OnReasoning(r)\n\t\t\t\t}\n\t\t\t}',
        '\t\t\tif r := ch.Delta.ReasoningContent; r != "" {\n\t\t\t\treasonChunks++\n\t\t\t\tsb.WriteString(r) // 注入：思考链混进正文\n\t\t\t\tif o.OnReasoning != nil {\n\t\t\t\t\to.OnReasoning(r)\n\t\t\t\t}\n\t\t\t}',
        [GO_MARK, "test", "-v", "./internal/llm/", "-run", "TestStreamChatSeparates", "-count=1"],
        "TestStreamChatSeparatesReasoningFromContent",
        "思考链混进正文 → 用户看到一大段自我嘀咕被当成答案写进稿子。",
    ),
    (
        "M5 前端不渲染中间材料", "web/js/chat.js",
        "          (s.material\n            ? '<span class=\"ctk-mat\"><span class=\"ctk-mat-tag\">思考中</span>' + esc(s.material) + '</span>'\n            : '') +",
        "          '' +",
        ["node", "tests/chat_trace.test.mjs"],
        "有材料时渲染 .ctk-mat",
        "后端发了、前端不渲染 —— 这轮最阴的形态：go test 和 SSE 验收全绿，只有屏幕退回跳秒。",
    ),
    (
        "M6 docgen 规格那一跳不再把正文流成材料", "internal/agent/agent.go",
        "out, err := e.llm.StreamChat(ctx, sys, argBlock.String(), llm.StreamOpts{\n"
        "\t\tDisableThinking: true,\n\t\tJSONMode:        true,\n"
        "\t\tOnReasoning:     reasoningSink(ctx),\n\t\tOnContent:       contentSink(ctx),\n\t})",
        "out, err := e.llm.StreamChat(ctx, sys, argBlock.String(), llm.StreamOpts{\n"
        "\t\tDisableThinking: true,\n\t\tJSONMode:        true,\n"
        "\t\tOnReasoning:     reasoningSink(ctx),\n\t})",
        [GO_MARK, "test", "-v", "./internal/agent/", "-run", "TestGenerateDocStreamsDocumentTextAsMaterial", "-count=1"],
        "TestGenerateDocStreamsDocumentTextAsMaterial",
        "这一跳关着思考链（astron 上思考片段恒为 0），掐了 contentSink 就等于掐掉了它唯一的"
        "材料来源：整轮最长的 16.8 秒静默又回来了，而材料链路的其它断言全绿。",
    ),
    (
        "M7 抽取器把容器归属算在压栈之后", "internal/agent/jsonpreview.go",
        "\t\towner := p.attr()\n\t\tp.kind = append(p.kind, b)\n\t\tp.own = append(p.own, owner)",
        "\t\tp.kind = append(p.kind, b)\n\t\tp.own = append(p.own, p.attr())",
        [GO_MARK, "test", "-v", "./internal/agent/", "-run", "TestJsonPreviewChunkInvariant", "-count=1"],
        "TestJsonPreviewChunkInvariant",
        "真踩过的 bug 原样注回：attr() 看的是栈顶容器，压完再算就是在问「新容器归谁」，"
        "答案永远是它自己 —— `\"rows\":[[\"a\",\"b\"]]` 一个值都露不出来，根级 `[` 还会越界 panic。",
    ),
    (
        "M8 结构化阶段又把思考链打开了", "internal/skillgen/generator.go",
        # 锚点故意用「调用行 + 紧随其后的 return \"\", err」而不是上面那几行注释：
        # 注释是给人看的，随时会被补一句（本轮就补过），锚点跟着碎掉 → 脚本报
        # 「注入点没找到」＝ 这条自证悄悄失效，而它看上去只是「跳过了一项」。
        # 带返回值的上下文既唯一，也不怕注释被改。
        # 注入体必须是锚点的「只改被测行为」的严格变异：同样 4 行，只把
        # withoutThinking(ctx) 拿掉。曾经这里只换了锚点、没换注入体，
        # 结果注入把 if err 块整段删掉 → build failed（环境红）——
        # 环境红不是断言红，等于这条自证白跑一轮。
        "out, err := g.chatWithMaterial(withoutThinking(ctx), sys, user, true)\n"
        "\tif err != nil {\n"
        "\t\treturn \"\", err\n"
        "\t}",
        "out, err := g.chatWithMaterial(ctx, sys, user, true)\n"
        "\tif err != nil {\n"
        "\t\treturn \"\", err\n"
        "\t}",
        [GO_MARK, "test", "-v", "./internal/skillgen/", "-run",
         "TestModelCallSitesDeclareThinkingMode", "-count=1"],
        "TestModelCallSitesDeclareThinkingMode",
        "用户投诉的「慢」就是这个形态：1/9 提取写作特征那一步 94.8 秒里 5026 字思考链 / 286 字正文，"
        "产出只是一段特征 JSON。摘掉开关后耗时立刻回到 94.8s 量级，而**所有别的单测、界面、验收全绿** —— "
        "只有源码守卫看得出「这个调用点没表态」。",
    ),
    (
        "M9 顺手把「撰写系统提示词」也关成一刀切", "internal/skillgen/generator.go",
        "\tuser := \"需求:\\n\" + in.Requirement + \"\\n\\n特征分析:\\n\" + attrs + \"\\n\\n母模板:\\n\" + tpl\n"
        "\tout, err := g.chatWithMaterial(ctx, sys, user)",
        "\tuser := \"需求:\\n\" + in.Requirement + \"\\n\\n特征分析:\\n\" + attrs + \"\\n\\n母模板:\\n\" + tpl\n"
        "\tout, err := g.chatWithMaterial(withoutThinking(ctx), sys, user)",
        [GO_MARK, "test", "-v", "./internal/skillgen/", "-run",
         "TestKeepThinkingAllowlistHasNoDrift", "-count=1"],
        "TestKeepThinkingAllowlistHasNoDrift",
        "反方向的坏：4/9 撰写系统提示词产出的是要给人读的提示词正文，思考链直接影响质量。"
        "关掉它整轮更快、单测全绿、界面照常，唯一的变化是技能变差 —— 「提速」漂成「降级」时"
        "没有任何东西会响。这条守卫专门盯这种无声降级。",
    ),
    (
        "M10 开关读了却没接到 provider 上", "internal/skillgen/generator.go",
        "\t\tDisableThinking: thinkingOff(ctx),",
        "\t\tDisableThinking: false,",
        [GO_MARK, "test", "-v", "./internal/skillgen/", "-run",
         "TestChatWithMaterialCarriesThinkingFlagToProvider", "-count=1"],
        "TestChatWithMaterialCarriesThinkingFlagToProvider",
        "最隐蔽的一种：调用点老老实实标了 withoutThinking、源码守卫全绿、扫描器也认，"
        "但开关在最后一跳被写死成 false —— 9 处标注全是摆设，860.5s 原样回来。"
        "行为用例是唯一能看见这一跳的尺子。",
    ),
]


def run(cmd, cwd):
    p = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True, timeout=900)
    return p.returncode, p.stdout + p.stderr


def _ver(binpath):
    """拿一个 go 可执行文件的 (major, minor)；拿不到就 None。"""
    if not binpath:
        return None
    if os.sep in binpath and not Path(binpath).exists():
        return None
    try:
        _, out = run([binpath, "version"], ROOT)
    except (OSError, subprocess.SubprocessError):
        return None
    m = re.search(r"\bgo(\d+)\.(\d+)", out)
    return (int(m.group(1)), int(m.group(2))) if m else None


def required_go():
    m = re.search(r"^go\s+(\d+)\.(\d+)", (ROOT / "go.mod").read_text(encoding="utf-8"), re.M)
    return (int(m.group(1)), int(m.group(2))) if m else (1, 0)


def resolve_go():
    """挑一个**够新**的 go。

    本地这台机器 PATH 里有 1.18 的 /usr/bin/go，CI runner 上没有 /usr/local/go/bin/go ——
    所以既不能信 PATH 的第一个，也不能写死绝对路径，只能把候选都试一遍、按 go.mod 的
    要求筛。一个都不合格就报环境红，别让它退化成「每条注入都红」的假红。
    """
    need = required_go()
    tried = []
    # GO_BIN 一旦设了就**只用它**（钉死工具链，别人也可能要靠它证明「环境红」这条路）。
    # 不设时才逐个候选试。注意不能信 PATH 的第一个：本地这台机器 PATH 里 /usr/bin/go 是
    # 1.18，而 CI runner 上根本没有 /usr/local/go/bin/go。
    pin = os.environ.get("GO_BIN")
    cands = [pin] if pin else [shutil.which("go"), "/usr/local/go/bin/go", "/usr/bin/go"]
    for c in cands:
        v = _ver(c)
        if v is None:
            if c:
                tried.append(f"{c}(不可用)")
            continue
        tried.append(f"{c}(go{v[0]}.{v[1]})")
        if v >= need:
            return c, need, tried
    return None, need, tried


def env_red(out):
    """输出里有没有编译/工具链失败签名。"""
    low = out.lower()
    return [s for s in ENV_RED if s.lower() in low]


def test_ran(out, expect, want):
    """预期用例是否真的跑到了 want 这个结果。

    go -v  : `--- FAIL: Name (0.00s)` / `--- PASS: Name (0.00s)`（顶格）
    node   : `  FAIL Name — ...` / `  ok   Name`（**有前导空格**，所以必须容忍空白，
             否则漏配 —— 那样「还原回绿」会被误判成没跑到）
    """
    e = re.escape(expect)
    if want == "fail":
        return re.search(r"---\s*FAIL:\s*" + e + r"\b", out) or \
               re.search(r"^\s*FAIL\s+" + e, out, re.M)
    return re.search(r"---\s*PASS:\s*" + e + r"\b", out) or \
           re.search(r"^\s*ok\s+" + e, out, re.M)


def main():
    go_bin, need, tried = resolve_go()
    if go_bin is None:
        print(f"✗ 环境红：找不到满足 go.mod 要求的 go（需要 ≥ {need[0]}.{need[1]}）")
        print("  试过：" + "、".join(tried))
        print("  这不是断言红 —— 一条断言都没跑到。先修环境（或 GO_BIN=... 指一个够新的），"
              "此时不要动断言。")
        return 2
    print(f"go: {go_bin}（go.mod 要求 ≥ {need[0]}.{need[1]}）")

    rc_bad, rc_env = [], []
    for name, rel, old, new, raw_cmd, expect, why in MUTATIONS:
        cmd = [go_bin if c == GO_MARK else c for c in raw_cmd]
        path = ROOT / rel
        if not path.exists():
            print(f"✗ {name}: 文件不存在 {rel}")
            rc_bad.append(name)
            continue
        original = path.read_text(encoding="utf-8")
        n = original.count(old)
        if n == 0:
            print(f"✗ {name}: 注入点没找到 —— chat.js/Go 的实现变了，本脚本的自证对象已失效，"
                  f"请对照实现更新锚点，别直接删脚本。")
            rc_bad.append(name)
            continue
        if n != 1:
            print(f"✗ {name}: 注入点命中 {n} 处不唯一（注错地方等于没测）")
            rc_bad.append(name)
            continue

        cwd = ROOT / "web" if cmd[0] == "node" else ROOT
        print(f"--- 注入：{name}（{why}）")
        try:
            path.write_text(original.replace(old, new, 1), encoding="utf-8")
            _, out = run(cmd, cwd)
            sig = env_red(out)
            if sig:
                print(f"✗ {name}: 环境红（{sig[0]}）—— 这不是断言红，一条断言都没跑到。"
                      f"先修环境再来自证。")
                print("\n".join(out.strip().splitlines()[-4:]))
                rc_env.append(name)
            elif test_ran(out, expect, "fail"):
                print(f"✓ {name}: 精确转红（--- FAIL: {expect}）")
            else:
                print(f"✗ {name}: 注入后预期用例没有转红（校验名 {expect}）")
                print("\n".join(out.strip().splitlines()[-6:]))
                rc_bad.append(name)
        finally:
            path.write_text(original, encoding="utf-8")

        # 还原后必须回绿，而且预期用例必须**真的跑过**
        rc2, out2 = run(cmd, cwd)
        sig = env_red(out2)
        if sig:
            print(f"✗ {name}: 还原后环境红（{sig[0]}），无法判定是否回绿")
            rc_env.append(name)
        elif not test_ran(out2, expect, "pass"):
            print(f"✗ {name}: 还原后预期用例没跑到 PASS（校验名 {expect}）—— "
                  f"「还原回绿」是假的，这条自证骑在空集上。")
            rc_bad.append(name)
        else:
            print(f"  ↳ 还原回绿 ✓（--- PASS: {expect}）")

    print()
    if rc_env:
        print(f"环境红（不是断言问题）：{sorted(set(rc_env))}")
        print("先修工具链/依赖，再重跑本脚本；此时不要动断言。")
        return 2
    if rc_bad:
        print(f"变异自证未通过：{rc_bad}")
        return 1
    print(f"变异自证全部通过（{len(MUTATIONS)}/{len(MUTATIONS)}：注入→精确红、还原→真绿）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
