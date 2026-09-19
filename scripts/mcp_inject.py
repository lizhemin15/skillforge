#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""MCP 接入（后台统一配置 + 开关）的「注入自证」——证明回归防线真的会红。

为什么需要它：这批用例看着很齐（握手头、SSE、翻页、传参、掩码、开关…），
但「看着齐」和「真的能抓住退化」是两件事。下面每一条都是 MCP 接进真实内网后
**真的会出现**的静默失效形态：

  - Accept 只写 application/json → 规范型 MCP 服务端直接 406，后台只显示「连接失败」；
  - Authorization 漏了 → key 填了却全程 401，用户会去反复核对那把 key；
  - SSE 不解析 → JS 系 MCP 服务端（含部分数据中台实现）全部失效；
  - tools/list 不翻页 → 25 个工具静默只剩第一页，用户「明明有 execute_sql 却调不到」；
  - 本地工具名不 sanitize → 中文服务名让 provider 400，**连带把内置工具一起废掉**；
  - 远端 isError 被当成功 → 模型拿空结果继续编数据，而不是改 SQL；
  - 禁用后不摘旧工具 → 僵尸工具：列表里还在，点必失败；
  - 掩码回存覆盖真 key → 之后全线 401，而界面显示「已配置密钥」；
  - 前端 payload 字段名与后端 json tag 分家 → 每次保存 400「请求体无效」，报错不提字段名。

做法：逐个把实现改坏（注入故障）→ 断言对应的用例必须变红 → 立刻还原 → 断言全绿。
任何一条「注入后仍然绿」都说明那条防线是假的；任何一条「红在编译错上」也不算数
（编译不过说明注入没打进去，不是断言抓到的）。

覆盖四层：
  A. internal/mcp          —— 传输（握手头 / SSE / 翻页 / 传参 / 会话 / 超时）
  B. internal/tools        —— 适配与注册（工具名、schema、错误、僵尸工具、提示词清单）
  C. internal/api          —— HTTP 契约（掩码、开关、删除、探测、刷新）
  D. web/js/admin.js       —— 跨层契约（前端字段名 ↔ 后端 readBody 的 json tag）

用法：
    cd <repo> && python3 scripts/mcp_inject.py

注意：脚本会临时改写被测源码，跑完（含异常路径）必须原样还原；用 try/finally 保证。
"""

import os
import shutil
import subprocess
import sys
import tempfile

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CLIENT_SRC = os.path.join(REPO, "internal", "mcp", "client.go")
TOOL_SRC = os.path.join(REPO, "internal", "tools", "mcp_tool.go")
ADMIN_SRC = os.path.join(REPO, "internal", "api", "admin_mcp.go")
ADMIN_JS = os.path.join(REPO, "web", "js", "admin.js")

# ── A. 传输层 ────────────────────────────────────────────────────────────────
CLIENT_INJECTIONS = [
    (
        "Accept 头只声明 application/json（规范型服务端 406）",
        '\treq.Header.Set("Accept", "application/json, text/event-stream")',
        '\treq.Header.Set("Accept", "application/json")',
        "TestInitialize_SendsAuthAndProtocolHeaders",
    ),
    (
        "Authorization 头没带上（key 白填，服务端一律 401）",
        '\t\treq.Header.Set("Authorization", "Bearer "+k)',
        "\t\t_ = k",
        "TestInitialize_SendsAuthAndProtocolHeaders",
    ),
    (
        "SSE 回包不解析（只吃 JSON，JS 系 MCP 服务端全部失效）",
        '\tif strings.Contains(ct, "text/event-stream") {',
        '\tif false && strings.Contains(ct, "text/event-stream") {',
        "TestInitialize_SSETransport",
    ),
    (
        "tools/list 不翻页（25 个工具静默只剩第一页）",
        "\t\tcursor = res.NextCursor",
        "\t\tbreak",
        "TestListTools_FollowsPagination",
    ),
    (
        "tools/call 把参数吞掉（模型传了、远端收不到）",
        'map[string]any{"name": name, "arguments": args}',
        'map[string]any{"name": name, "arguments": map[string]any{}}',
        "TestCallTool_PassesArgumentsThrough",
    ),
    (
        "isError 不透传（业务失败被当成功，模型拿空结果继续编）",
        "\treturn CallResult{Text: text, IsError: res.IsError}, nil",
        "\treturn CallResult{Text: text, IsError: false}, nil",
        "TestCallTool_MarksBusinessError",
    ),
    (
        "401/403 不落到人话提示（管理员只看到一串英文原文）",
        # 注意：不能写成 `case false && code == ..., http.StatusForbidden` ——
        # 第二个 case 表达式是 int，跟 `false &&` 拼在一起类型不合法，注入会红在编译错上，
        # 那不算「断言抓住」，是脚本自己的 bug（踩过一次）。
        "\tcase http.StatusUnauthorized, http.StatusForbidden:",
        "\tcase -1:",
        "TestAuthFailure_MentionsAPIKey",
    ),
    (
        "Mcp-Session-Id 不回传（有状态服务端按规范 404）",
        "\t\tc.sessionID = sid2",
        "\t\t_ = sid2",
        "TestSessionID_Propagated",
    ),
    (
        "超时兜底失效（超时字段为 0 时变成 0 秒自杀）",
        "\tif s <= 0 || s > 600 {\n\t\ts = 30\n\t}",
        "\tif s <= 0 || s > 600 {\n\t\ts = 0\n\t}",
        "TestConfig_DefaultTimeout",
    ),
    (
        "地址协议不校验（ftp:// 也放行，运行时才炸）",
        '\tif !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {',
        '\tif false && !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {',
        "TestNewClient_RejectsBadURL",
    ),
]

# ── B. 适配与注册 ────────────────────────────────────────────────────────────
TOOL_INJECTIONS = [
    (
        "工具名不 sanitize（中文名 → provider 400，连带废掉全部工具）",
        '\tname := "mcp_" + sanitizeIdent(alias) + "_" + sanitizeIdent(remote)',
        '\tname := "mcp_" + alias + "_" + remote',
        "TestMCPTool_LocalNameIsValidIdentifier",
    ),
    (
        "本地名不含服务器标识（两个服务同名工具撞车、后者静默覆盖）",
        '\tname := "mcp_" + sanitizeIdent(alias) + "_" + sanitizeIdent(remote)',
        '\tname := "mcp_" + sanitizeIdent(remote)',
        "TestMCPTool_DifferentServersDoNotCollide",
    ),
    (
        "schema 不补 type（缺 type 会让整个请求被 400）",
        '\tif _, ok := out["type"]; !ok {',
        '\tif _, ok := out["type"]; !ok && false {',
        "TestMCPTool_SchemaAlwaysValid",
    ),
    (
        "描述不写归属服务器（模型不知道这是外部中台的能力）",
        # 原来只换引导语是**无效注入**：下一行 `b.WriteString(t.server)` 照样把服务器名
        # 写进描述，用例自然还绿（实测踩过）。要打就打真值那行。
        "\tb.WriteString(t.server)",
        '\tb.WriteString("")',
        "TestMCPTool_DescriptionMentionsServer",
    ),
    (
        "远端业务失败当成成功（模型看到空结果继续编）",
        "\tif res.IsError {",
        "\tif false && res.IsError {",
        "TestMCPTool_RunSurfacesBusinessError",
    ),
    (
        "执行结果被吞（模型只拿到「无返回内容」）",
        "\ttext := Truncate(strings.TrimSpace(res.Text), t.resultMax)",
        '\ttext := Truncate("", t.resultMax)',
        "TestMCPTool_RunReturnsText",
    ),
    (
        "禁用/删除后不摘旧工具（僵尸工具：列表里还在、点必失败）",
        "\t\tm.reg.Remove(m.mounted...)",
        "\t\t_ = m.mounted",
        "TestMCPManager_MountsAndUnmounts",
    ),
    (
        "系统提示词不列 MCP 工具清单（模型不知道有这些工具，改去写 http_request）",
        # 锚点跟着实现走：提示词段落现在由 `part` 一个变量产出，同时喂给
        # PromptHint()（全局）与 hintByServer→PromptHintFor()（按勾选裁剪）。
        # 把 part 打空 = 两条路一起失明，这才是用户看得见的那个故障：
        # 勾了服务器，模型却不知道有工具，于是绕圈、变慢、最后自己编数据。
        '\t\t\tpart := fmt.Sprintf("【%s】\\n%s", cfg.Name, strings.Join(lines, "\\n"))\n'
        "\t\t\thintParts = append(hintParts, part)",
        '\t\t\tpart := ""\n'
        "\t\t\thintParts = append(hintParts, part)",
        "TestMCPManager_MountsAndUnmounts",
    ),
    (
        "勾了服务器也不给提示词清单（勾选形同虚设：门控只拦工具表、没拦提示词）",
        "\t\tif p := m.hintByServer[id]; strings.TrimSpace(p) != \"\" {\n"
        "\t\t\tparts = append(parts, p)\n"
        "\t\t}",
        "\t\t_ = m.hintByServer",
        "TestPromptHint_DefaultEmptyAndScopedToSelection",
    ),
    (
        "一台服务器失败就中断刷新（内网里一台没起来，整片工具全废）",
        "\t\tinfo, err := client.Initialize(ctx)\n"
        "\t\tif err != nil {\n"
        "\t\t\tst.Error = err.Error()\n"
        "\t\t\tstatuses = append(statuses, st)\n"
        "\t\t\tcontinue\n"
        "\t\t}",
        "\t\tinfo, err := client.Initialize(ctx)\n"
        "\t\tif err != nil {\n"
        "\t\t\tst.Error = err.Error()\n"
        "\t\t\tstatuses = append(statuses, st)\n"
        "\t\t\tm.statuses = statuses\n"
        "\t\t\treturn statuses\n"
        "\t\t}",
        "TestMCPManager_OneBadServerDoesNotBreakOthers",
    ),
    (
        "配置读取失败静默（当作「没有 MCP」，用户以为功能坏了）",
        "\t\tif err != nil {\n"
        '\t\t\tm.statuses = []MCPServerStatus{{\n'
        '\t\t\t\tName: "配置读取失败", Error: err.Error(), CheckedAt: time.Now(),\n'
        "\t\t\t}}\n"
        "\t\t\treturn m.statuses\n"
        "\t\t}",
        "\t\tif err != nil {\n"
        "\t\t\tm.statuses = []MCPServerStatus{}\n"
        "\t\t\treturn m.statuses\n"
        "\t\t}",
        "TestMCPManager_SourceErrorIsVisible",
    ),
]

# ── C. HTTP 契约 ────────────────────────────────────────────────────────────
ADMIN_INJECTIONS = [
    (
        "列表回明文 key（这条是安全底线）",
        '\treturn string(r[:4]) + "…" + string(r[len(r)-4:]), true',
        "\treturn k, true",
        "TestMCPListMasksKey",
    ),
    (
        "掩码回存覆盖真 key（之后全线 401，界面却显示「已配置密钥」）",
        # 保留 `_ = old`：old 的唯一使用点就在这一行，直接换掉会变成「声明未使用」→ 编译错
        # （编译错不算红，是脚本 bug）。
        "\t\tcase err == nil:\n"
        "\t\t\tcfg.APIKey = old.APIKey",
        "\t\tcase err == nil:\n"
        "\t\t\t_ = old\n"
        "\t\t\tcfg.APIKey = req.APIKey",
        "TestMCPUpsertMaskedKeyKeepsOriginal",
    ),
    (
        "保存不校验地址协议（填错也存进去，运行时连不上再让用户猜）",
        '\tif !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {',
        '\tif false && !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {',
        "TestMCPUpsertRejectsBadURL",
    ),
    (
        "开关不落库（后台显示与库里状态分家）",
        "\tn, err := a.store.SetMCPEnabled(req.ID, req.Enabled)",
        "\tn, err := a.store.SetMCPEnabled(req.ID, true)",
        "TestMCPToggleAndDelete",
    ),
    (
        "删除不落库（界面删了、库里还在，重启又回来）",
        "\tn, err := a.store.DeleteMCPServer(id)",
        "\tn, err := 1, error(nil)",
        "TestMCPToggleAndDelete",
    ),
    (
        "「测试连接」不用库里真 key（编辑态永远显示失败）",
        "\t\t\tif isMaskedKey(req.APIKey) {\n"
        "\t\t\t\tcfg.APIKey = old.APIKey",
        "\t\t\tif isMaskedKey(req.APIKey) {\n"
        '\t\t\t\tcfg.APIKey = ""',
        "TestMCPTestConnectionUsesStoredKeyWhenMasked",
    ),
    (
        "手动刷新不真的重连（工具挂不上，对话里调不到）",
        # 锚点必须带上 `if a.mcp != nil {`：只写到 `\n\t}` 会同时命中
        # refreshMCPAsync 里的 `\n\t}()`（前缀匹配），锚点不唯一 → 脚本报失效。
        "\tif a.mcp != nil {\n"
        "\t\tctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)\n"
        "\t\tdefer cancel()\n"
        "\t\ta.mcp.Refresh(ctx)\n"
        "\t}",
        "\tif a.mcp != nil {\n"
        "\t\tctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)\n"
        "\t\tdefer cancel()\n"
        "\t\t_ = ctx\n"
        "\t}",
        "TestMCPRefreshRegistersTools",
    ),
]

# ── D. 用户级门控（默认不调度 / 勾选才调度 / 勾了必须真给）──────────────────
# 这一层是「MCP 只在用户勾选时才对模型可见」的实现本体。故障方向分两边：
#   放太宽 → 没勾也调得到内网业务系统（安全底线）；
#   收太死 → 勾了跟没勾一样（用户点着按钮却什么都没发生，会一直以为是坏了）。
FILTER_INJECTIONS = [
    (
        "没勾选也放行 MCP 工具（默认就调度内网工具 —— 门控朝危险方向倒）",
        '\t\tif sid != "" && allow[sid] {\n'
        "\t\t\tout.Register(t)\n"
        "\t\t}",
        '\t\tif sid != "" {\n'
        "\t\t\tout.Register(t)\n"
        "\t\t}",
        "TestGate_DefaultNoMCPTools",
    ),
    (
        "勾选集合不过滤不可用服务器（脏 id 直通：已删/已关的服务器照样进 allow）",
        '\t\tif id == "" || seen[id] || !ok[id] {',
        '\t\tif id == "" || seen[id] {',
        "TestAllowedServers_DropsStaleAndDupes",
    ),
    (
        "无归属的 MCP 工具当内置放行（fail-closed 失守：认不出来就放过去）",
        '\tif strings.HasPrefix(t.Name(), "mcp_") {\n'
        '\t\treturn "", true\n'
        "\t}",
        '\tif false && strings.HasPrefix(t.Name(), "mcp_") {\n'
        '\t\treturn "", true\n'
        "\t}",
        "TestGate_UnlabeledMCPNameFailsClosed",
    ),
]

# ── E. 跨层契约（前端字段名 ↔ 后端 json tag）────────────────────────────────
JS_INJECTIONS = [
    (
        "前端 payload 字段名与后端 json tag 分家（每次保存 400「请求体无效」）",
        "      api_key: $('mcp-key').value.trim(),",
        "      apiKey: $('mcp-key').value.trim(),",
        "TestAdminMCPPayloadMatchesTags",
    ),
]

MCP_ALL = "MCP|Initialize|ListTools|CallTool|Auth|Session|NewClient|Config|ParseSSE|Gate|FilterMCP|PromptHint|AllowedServers|Selectable|ServerTools"

FILTER_SRC = os.path.join(REPO, "internal", "tools", "mcp_filter.go")

TARGETS = [
    ("mcp", CLIENT_SRC, "./internal/mcp/", CLIENT_INJECTIONS, ""),
    ("tools", TOOL_SRC, "./internal/tools/", TOOL_INJECTIONS, MCP_ALL),
    ("gate", FILTER_SRC, "./internal/tools/", FILTER_INJECTIONS, MCP_ALL),
    ("api", ADMIN_SRC, "./internal/api/", ADMIN_INJECTIONS, "MCP"),
    ("admin.js", ADMIN_JS, "./internal/api/", JS_INJECTIONS, "TestAdminMCPPayloadMatchesTags"),
]


def go_test(pkg: str, run: str) -> str:
    env = dict(os.environ)
    env["PATH"] = "/usr/local/go/bin:" + env.get("PATH", "")
    cmd = ["go", "test", pkg, "-count=1"]
    if run:
        cmd += ["-run", run]
    p = subprocess.run(cmd, cwd=REPO, env=env, capture_output=True, text=True)
    return p.stdout + p.stderr


def run_target(label: str, target: str, pkg: str, injections, green_run: str) -> int:
    with open(target, encoding="utf-8") as f:
        original = f.read()
    backup = os.path.join(tempfile.gettempdir(), os.path.basename(target) + ".mcpbak")
    shutil.copy(target, backup)
    leaked = 0
    try:
        for name, old, new, run in injections:
            if original.count(old) != 1:
                # 锚点失效 = 注入没打进去，必须报错而不是当成功；
                # 悄悄跳过会让「全绿」变成假象。
                print(
                    "[锚点失效 ✗] %s / %s（命中 %d 次；实现改了？同步更新本脚本）"
                    % (label, name, original.count(old))
                )
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
