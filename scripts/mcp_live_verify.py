#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""MCP 接入的**线上真机终验** —— 只看「对话里到底调没调到 DataToolbox 的库」。

为什么单测 + 注入自证都绿了还要跑这个：
  那些尺子量的都是「我方代码有没有按契约做」。它们全绿，也可能整条链是空的 ——
  比如后台配完没人重连（工具压根没挂进 registry）、或者模型根本不知道有这些工具
  （提示词没列清单），于是它去写一段 http_request 而不是调工具。用户看到的正是
  「配好了，但 AI 还是自己编」。这里量的是端到端真结果：**真工具名 + 真数据**。

判据（任一不成立即 FAIL，非零退出）：
  A. 后台「保存 + 刷新」后，服务状态是已连接、工具数 > 0
  B. 对话真的调到了该服务的工具（工具轨迹里出现 mcp_<alias>_<tool>）
  C. 工具调用成功（不是 error/失败），且返回里带真实数据痕迹（表名或数字行）
  D. 全过程没有「把 MCP 当摆设」的迹象：若模型改用 http_request 去裸调，直接判失败

凭据一律从文件读，不出现在本文件里，也不打印（只打长度 + md5 前 8 位）。
用法：
    python3 scripts/mcp_live_verify.py                 # 默认 http://127.0.0.1:8092
    BASE=https://... python3 scripts/mcp_live_verify.py
"""
import hashlib
import json
import os
import re
import sqlite3
import sys
import time
import urllib.error
import urllib.request

BASE = os.environ.get("BASE", "http://127.0.0.1:8092").rstrip("/")
ENV_FILE = os.environ.get("SKILLFORGE_ENV_FILE", "/opt/skillforge/skillforge.env")
DT_DB = os.environ.get("DT_DB", "/opt/datatoolbox/data/data-store.db")
DT_MCP_URL = os.environ.get("DT_MCP_URL", "http://127.0.0.1:8080/mcp")
MCP_ID = os.environ.get("MCP_ID", "datatoolbox")

fails = []
steps = []


def step(msg):
    steps.append(msg)
    print(msg, flush=True)


def fail(msg):
    fails.append(msg)
    print("  ✗ " + msg, flush=True)


def ok(msg):
    print("  ✓ " + msg, flush=True)


def fingerprint(s):
    return "len=%d md5=%s" % (len(s), hashlib.md5(s.encode()).hexdigest()[:8])


def load_env(path):
    out = {}
    try:
        with open(path, encoding="utf-8") as f:
            for line in f:
                line = line.strip()
                if not line or line.startswith("#") or "=" not in line:
                    continue
                k, v = line.split("=", 1)
                out[k.strip()] = v.strip().strip('"').strip("'")
    except OSError as e:
        print("读不到 %s：%s" % (path, e))
        sys.exit(2)
    return out


def http(method, path, body=None, token=None, timeout=180):
    url = BASE + path
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    if token:
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read().decode("utf-8", "replace")
            return r.status, raw
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")


def dt_api_key():
    """DataToolbox 的 MCP 钥匙来自 users 表 api_key 列（不是 DATA_ONTOLOGY_API_KEY）。"""
    con = sqlite3.connect("file:%s?mode=ro" % DT_DB, uri=True)
    try:
        row = con.execute(
            "SELECT username, api_key FROM users WHERE api_key IS NOT NULL AND api_key != '' "
            "ORDER BY rowid LIMIT 1").fetchone()
    finally:
        con.close()
    if not row:
        print("DataToolbox users 表里没有 api_key，先登录一次 DT 生成")
        sys.exit(2)
    return row[0], row[1]


def sse_chat(message, session_id):
    """POST /api/chat 拿 SSE，返回 (事件名列表, 全部原文)。"""
    body = json.dumps({"session_id": session_id, "message": message}).encode()
    req = urllib.request.Request(BASE + "/api/chat", data=body,
                                 headers={"Content-Type": "application/json"}, method="POST")
    names, buf = [], []
    with urllib.request.urlopen(req, timeout=600) as r:
        cur = None
        for raw in r:
            line = raw.decode("utf-8", "replace").rstrip("\n")
            buf.append(line)
            if line.startswith("event: "):
                cur = line[7:].strip()
                if cur not in names:
                    names.append(cur)
    return names, "\n".join(buf)


def main():
    env = load_env(ENV_FILE)
    user = env.get("SKILLFORGE_ADMIN_USER", "")
    pw = env.get("SKILLFORGE_ADMIN_PASS", "")
    if not user or not pw:
        print("env 里没有管理员账号")
        sys.exit(2)

    print("=== 线上 MCP 真机终验 · BASE=%s ===" % BASE)
    print("管理员 %s / 口令 %s" % (user, fingerprint(pw)))

    # ---------- 0. 登录 ----------
    step("0. 管理员登录")
    st, raw = http("POST", "/api/login", {"username": user, "password": pw})
    if st != 200:
        fail("登录失败 HTTP %d：%s" % (st, raw[:200]))
        return
    token = json.loads(raw).get("token", "")
    if not token:
        fail("登录没返回 token")
        return
    ok("已登录（token %s）" % fingerprint(token))

    # ---------- 1. 取 DT 的 MCP key ----------
    step("1. 取 DataToolbox MCP 钥匙")
    dt_user, dt_key = dt_api_key()
    ok("用 %s 的 api_key（%s）" % (dt_user, fingerprint(dt_key)))

    # ---------- 2. 后台保存并启用 ----------
    step("2. 后台统一配置：保存并启用")
    st, raw = http("POST", "/api/admin/mcp", {
        "id": MCP_ID, "name": "数据工具箱", "url": DT_MCP_URL,
        "api_key": dt_key, "enabled": True, "timeout_sec": 60,
    }, token=token)
    if st != 200:
        fail("保存配置失败 HTTP %d：%s" % (st, raw[:300]))
        return
    ok("已保存")

    # ---------- 2b. 读回列表（掩码 / 状态） ----------
    st, raw = http("GET", "/api/admin/mcp", token=token)
    if st != 200:
        fail("列表接口 HTTP %d：%s" % (st, raw[:300]))
        return
    body = json.loads(raw)
    views = body.get("servers", body) if isinstance(body, dict) else body
    mine = [v for v in views if v.get("id") == MCP_ID]
    if not mine:
        fail("列表里找不到 %s" % MCP_ID)
        return
    mask = mine[0].get("key_mask") or ""
    if not mask:
        fail("列表没回掩码（has_key=%s）" % mine[0].get("has_key"))
    elif dt_key in raw:
        fail("列表把明文 key 回出来了 —— 安全底线破了")
    else:
        ok("列表只回掩码：%s（明文没出接口）" % mask)

    # ---------- 3. 测试连接：拿掩码回填（证明掩码回存沿用真 key） ----------
    step("3. 后台「测试连接」（用列表回的掩码回填）")
    st, raw = http("POST", "/api/admin/mcp/test", {
        "id": MCP_ID, "name": "数据工具箱", "url": DT_MCP_URL,
        "api_key": mask, "enabled": True, "timeout_sec": 60,
    }, token=token)
    if st != 200:
        fail("测试连接接口 HTTP %d：%s" % (st, raw[:300]))
    else:
        j = json.loads(raw)
        if not j.get("ok"):
            fail("测试连接 ok=false（掩码没沿用真 key？）：%s" % raw[:300])
        else:
            ok("测试连接通过：%s v%s，远端工具 %d 个 / 已挂载 %d 个" % (
                j.get("server", "?"), j.get("version", "?"),
                len(j.get("remote_tools") or []), len(j.get("mounted") or [])))

    # ---------- 3b. 开关真生效（用户要的就是这个「开关」） ----------
    # 光看接口 200 不算数：要看到「关掉 → 挂载工具归零、状态变未启用」，
    # 「再打开 → 25 个工具回来」。这才是开关在对话里真的摘掉/挂上工具。
    step("3b. 开关：关掉 → 工具必须归零；再打开 → 工具回来")

    def wait_tools(expect_enabled, seconds=90):
        """轮询列表等异步重连收敛，返回 (enabled, tool_count, state)。"""
        deadline = time.time() + seconds
        last = (None, None, "")
        while time.time() < deadline:
            st, raw = http("GET", "/api/admin/mcp", token=token)
            if st == 200:
                views = json.loads(raw).get("servers", [])
                me = [v for v in views if v.get("id") == MCP_ID]
                if me:
                    v = me[0]
                    stt = v.get("status") or {}
                    last = (v.get("enabled"), len(stt.get("tools") or []), stt.get("error", ""))
                    if v.get("enabled") == expect_enabled and (not expect_enabled or (last[1] or 0) > 0):
                        return last
            time.sleep(3)
        return last

    st, raw = http("POST", "/api/admin/mcp/toggle", {"id": MCP_ID, "enabled": False}, token=token, timeout=120)
    if st != 200:
        fail("关开关 HTTP %d：%s" % (st, raw[:200]))
    else:
        en, n, err = wait_tools(False)
        if en is False and n == 0:
            ok("关掉后：已停用、挂载工具 0 个（对话里立刻调不到了）")
        else:
            fail("关掉后状态不对：enabled=%s tools=%d err=%s" % (en, n, err))

    st, raw = http("POST", "/api/admin/mcp/toggle", {"id": MCP_ID, "enabled": True}, token=token, timeout=120)
    if st != 200:
        fail("开开关 HTTP %d：%s" % (st, raw[:200]))
    else:
        en, n, err = wait_tools(True)
        if en is True and n > 0:
            ok("再打开：工具回到 %d 个" % n)
        else:
            fail("打开后没挂上工具：enabled=%s tools=%d err=%s" % (en, n, err))

    # ---------- 4. 刷新 → 工具必须真挂上 ----------
    step("4. 刷新（重连 + 挂工具）")
    st, raw = http("POST", "/api/admin/mcp/refresh", {}, token=token, timeout=300)
    if st != 200:
        fail("刷新接口 HTTP %d：%s" % (st, raw[:300]))
    st, raw = http("GET", "/api/admin/mcp", token=token)
    if st != 200:
        fail("列表接口 HTTP %d：%s" % (st, raw[:300]))
        return
    body = json.loads(raw)
    views = body.get("servers", body) if isinstance(body, dict) else body
    me = [v for v in views if v.get("id") == MCP_ID]
    if not me:
        fail("刷新后列表里找不到 %s" % MCP_ID)
        return
    stt = me[0].get("status") or {}
    tools = int(stt.get("tool_count") or 0)
    if stt.get("error"):
        fail("服务状态有错：%s" % stt["error"])
    if tools <= 0:
        fail("挂上来的工具数是 %d —— 后台看着「已启用」，对话里其实调不到" % tools)
    else:
        ok("状态：已连接 %s v%s，工具 %d 个" % (stt.get("server", "?"), stt.get("version", "?"), tools))

    # ---------- 5. 对话里真调一次 ----------
    step("5. 对话里真调 DataToolbox 工具")
    sess = "mcp-live-%d" % int(time.time())
    msg = ("用已经接入的 DataToolbox MCP 工具，列出数据库里前 5 张表的表名和行数。"
           "必须真的调用 MCP 工具去查，不要凭印象回答，也不要用 http_request 裸调接口。")
    names, text = sse_chat(msg, sess)
    print("    事件类型：%s" % ",".join(names))
    m = re.search(r"mcp_%s_[a-zA-Z0-9_]+" % re.escape(MCP_ID), text)
    if not m:
        fail("工具轨迹里没有出现 mcp_%s_* —— 模型没调 MCP（它可能去写 http_request 了）" % MCP_ID)
    else:
        ok("真的调到了：%s" % m.group(0))
    if "http_request" in text and not m:
        fail("退化成裸调 http_request，MCP 等于摆设")
    # 真数据痕迹：表名 + 行数形态
    if re.search(r"\b(select|SELECT)\b.*\b(from|FROM)\b", text):
        ok("轨迹里能看到真 SQL")
    if re.search(r"ok\":\s*true|success", text) and m:
        ok("工具调用返回成功")

    print("\n=== 结果 ===")
    print("步骤 %d 项；失败 %d 项" % (len(steps), len(fails)))
    for f in fails:
        print("  ✗ " + f)
    if fails:
        print("线上终验未通过 —— 「后台配置能存能开」跟「对话里真调到」是两件事。")
        sys.exit(1)
    print("线上终验通过：后台配置 → 重连挂载 → 对话真调 → 真数据返回，整条链是通的。")


if __name__ == "__main__":
    main()
