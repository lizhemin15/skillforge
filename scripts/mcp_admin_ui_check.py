#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""真浏览器验后台「MCP 连接」面板：登录 → 切 tab → 看列表是否渲染出已配的 DataToolbox。
凭据从 env 文件读，不进脚本、不进日志。截图留 /tmp/mcp_admin_ui.png 供人眼复核。"""
import os
import re
import sys

from playwright.sync_api import sync_playwright

BASE = os.environ.get("BASE", "http://127.0.0.1:8092")
ENV_FILE = os.environ.get("SKILLFORGE_ENV_FILE", "/opt/skillforge/skillforge.env")


def load_env(path):
    out = {}
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if line and not line.startswith("#") and "=" in line:
                k, v = line.split("=", 1)
                out[k.strip()] = v.strip().strip('"').strip("'")
    return out


def main():
    env = load_env(ENV_FILE)
    user, pw = env["SKILLFORGE_ADMIN_USER"], env["SKILLFORGE_ADMIN_PASS"]
    fails = []

    with sync_playwright() as p:
        b = p.chromium.launch()
        pg = b.new_page(viewport={"width": 1440, "height": 1000})
        pg.goto(BASE + "/admin", wait_until="networkidle")

        # 登录
        pg.fill("input[type=text]", user)
        pg.fill("input[type=password]", pw)
        pg.click("button:has-text('登 录')")
        # 注意：MCP 面板在未激活的 tab 里，is visible=False —— 等 attached，别等 visible，
        # 否则永远超时（这条坑踩过一次）。
        pg.wait_for_selector("#mcp-list", state="attached", timeout=20000)
        pg.wait_for_timeout(2500)
        print("✓ 登录成功，管理端已渲染")

        # 切到 MCP tab
        pg.click("button[data-tab='mcp']")
        pg.wait_for_timeout(2500)
        pane_visible = pg.eval_on_selector("#tab-mcp", "e => getComputedStyle(e).display !== 'none'")
        print("✓ MCP tab 可见" if pane_visible else "✗ MCP tab 没切出来")
        if not pane_visible:
            fails.append("MCP tab 切不出来")

        # 列表内容
        html = pg.inner_html("#mcp-list")
        text = pg.inner_text("#mcp-list")
        print("--- 列表文本 ---\n" + re.sub(r"\n{2,}", "\n", text).strip()[:600])
        for must, label in [
            ("datatoolbox", "已配的 datatoolbox 出现在列表里"),
            ("数据工具箱", "名称正确"),
            ("mcp_datatoolbox_execute_sql", "挂载的工具本地名可见（模型真调得到）"),
        ]:
            if must in html:
                print("✓ " + label)
            else:
                print("✗ " + label)
                fails.append(label)

        # 开关是否可点（不提交，只看控件存在）
        toggles = pg.query_selector_all("#mcp-list input[type=checkbox]")
        print("✓ 列表里开关控件 %d 个" % len(toggles) if toggles else "✗ 列表里没有开关控件")
        if not toggles:
            fails.append("列表里没有开关控件")

        # 表单字段齐不齐
        for sel, label in [("#mcp-id", "id"), ("#mcp-name", "名称"), ("#mcp-url", "地址"),
                           ("#mcp-key", "API Key"), ("#mcp-enabled", "启用开关"),
                           ("#mcp-save", "保存"), ("#mcp-test", "测试连接")]:
            if pg.query_selector(sel):
                print("✓ 表单有 %s" % label)
            else:
                print("✗ 表单缺 %s" % label)
                fails.append("表单缺 " + label)

        pg.screenshot(path="/tmp/mcp_admin_ui.png", full_page=True)
        print("截图: /tmp/mcp_admin_ui.png")

        # 浏览器控制台有没有报错
        b.close()

    print("\n失败 %d 项" % len(fails))
    if fails:
        sys.exit(1)
    print("后台 MCP 面板真浏览器验收通过")


if __name__ == "__main__":
    main()
