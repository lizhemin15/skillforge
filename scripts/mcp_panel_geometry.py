#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""量后台「MCP 连接」面板的真实几何 —— 用**真选择器**，判据是数字不是感觉。

为什么要有这把尺子：Vision 看截图说「已启用 徽章折成两行」「工具名被 ellipsis 砍掉」，
但 AI 看图的描述不能当判据（它也会看错，比如把「短标识」读成「知标识」）。
这里直接问浏览器要 clientRect / scrollWidth，折行 = height/line-height ≥ 2，
截断 = scrollWidth > clientWidth。空选择器 = 0 失败 = 空跑绿，所以先断言选择器命中数。
"""
import os, sys
from playwright.sync_api import sync_playwright

BASE = os.environ.get("BASE", "http://127.0.0.1:8092")
ENV_FILE = os.environ.get("SKILLFORGE_ENV_FILE", "/opt/skillforge/skillforge.env")
env = {}
with open(ENV_FILE, encoding="utf-8") as f:
    for line in f:
        line = line.strip()
        if line and not line.startswith("#") and "=" in line:
            k, v = line.split("=", 1)
            env[k.strip()] = v.strip().strip('"').strip("'")

JS = r"""
() => {
  const q = s => [...document.querySelectorAll(s)];
  const box = e => { const r = e.getBoundingClientRect(); const cs = getComputedStyle(e);
    const lh = parseFloat(cs.lineHeight) || parseFloat(cs.fontSize) * 1.4;
    return {text: e.textContent.trim().replace(/\s+/g, ' ').slice(0, 70),
            w: Math.round(r.width), h: Math.round(r.height), lh: Math.round(lh),
            lines: Math.round(r.height / lh * 10) / 10, ws: cs.whiteSpace,
            clipped: e.scrollWidth > Math.ceil(r.width) + 1,
            sw: e.scrollWidth, cw: Math.round(r.width)};
  };
  const rows = q('#mcp-list .prov-row');
  return {
    rowCount: rows.length,
    cards: q('#mcp-list .prov-row').length,
    nm: q('#mcp-list .prov-row .nm').map(box),
    pill: q('#mcp-list .pill-active').map(box),
    dt: q('#mcp-list .prov-row .dt').map(box),
    mountLine: q('#mcp-list .prov-row .dt:has(code)').map(box),
    codeCount: q('#mcp-list .prov-row .dt code').length,
    actions: q('#mcp-list .prov-row .row-actions').map(box),
  };
}
"""

with sync_playwright() as p:
    b = p.chromium.launch()
    pg = b.new_page(viewport={"width": 1440, "height": 1000})
    pg.goto(BASE + "/admin", wait_until="networkidle")
    pg.fill("input[type=text]", env["SKILLFORGE_ADMIN_USER"])
    pg.fill("input[type=password]", env["SKILLFORGE_ADMIN_PASS"])
    pg.click("button:has-text('登 录')")
    pg.wait_for_selector("#mcp-list", state="attached", timeout=20000)
    pg.click("button[data-tab='mcp']")
    pg.wait_for_timeout(2500)
    d = pg.evaluate(JS)
    b.close()

fails, notes = [], []
print("命中：.prov-row=%d  .nm=%d  .pill-active=%d  .dt=%d  工具名 code=%d"
      % (d["rowCount"], len(d["nm"]), len(d["pill"]), len(d["dt"]), d["codeCount"]))
# 尺子自己先自证：空命中 = 空跑绿，直接判失败
if d["rowCount"] < 1: fails.append("选择器没命中 .prov-row —— 尺子坏了（不是产品好）")
if not d["pill"]:     fails.append("选择器没命中 .pill-active 徽章 —— 尺子坏了")
if not d["mountLine"]: fails.append("选择器没命中「已挂载工具」那一行 —— 尺子坏了")

for x in d["pill"]:
    print("  徽章: w=%d h=%d lh=%d 行数=%s ws=%s | %s" % (x["w"], x["h"], x["lh"], x["lines"], x["ws"], x["text"]))
    if x["lines"] > 1.2:
        fails.append("徽章「%s」折成 %s 行（h=%d，lh=%d）" % (x["text"], x["lines"], x["h"], x["lh"]))
for x in d["nm"]:
    print("  标题: h=%d 行数=%s | %s" % (x["h"], x["lines"], x["text"]))
    if x["lines"] > 1.2:
        fails.append("标题行折成 %s 行：%s" % (x["lines"], x["text"]))
for x in d["dt"]:
    tag = "挂载工具行" if "<code>" in "" or d["mountLine"] and x is d["mountLine"][0] else "信息行"
    print("  信息行: w=%d sw=%d 截断=%s | %s" % (x["w"], x["sw"], x["clipped"], x["text"][:56]))
for x in d["mountLine"]:
    print("  挂载工具行: w=%d sw=%d 截断=%s 行数=%s | %s" % (x["w"], x["sw"], x["clipped"], x["lines"], x["text"][:56]))
    if x["clipped"] and d["codeCount"] > 3:
        fails.append("挂载工具行被 ellipsis 砍成一行：25 个工具名只看得见前几个（sw=%d > w=%d）" % (x["sw"], x["w"]))

print("\n失败 %d 项" % len(fails))
for f in fails:
    print("  ✗ " + f)
sys.exit(1 if fails else 0)
