#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""admin_mcp.test.mjs 这套断言的自证脚本。

为什么必须有它：那个 .test.mjs 全绿只证明「现在没坏」，证明不了「坏了会被抓住」。
而这次改的方向恰好有一条**看起来更整齐**的错路：为了排版好看，把 25 个工具名
砍成「前 N 个 + 其余略」、或者让长名字折断、或者「折叠」其实没折上
（`display:flex` 盖掉 `[hidden]` 的默认 display:none）。这三种在屏幕上都很"整洁"，
坏得静悄悄 —— 用户以为看到的就是全部。

做法：往**出货文件**（web/js/admin.js、web/css/style.css）逐个注入真实故障，
要求对应的那条断言变红；还原后必须重新变绿。哪条注入还是绿的，就说明那条断言是假的。

用法：python3 web/tests/admin_mcp_mutation_check.py
"""
import subprocess
import sys
from pathlib import Path

WEB = Path(__file__).resolve().parent.parent
ADMIN_JS = WEB / "js" / "admin.js"
STYLE_CSS = WEB / "css" / "style.css"
NODE_TEST = WEB / "tests" / "admin_mcp.test.mjs"

failures = 0
BACKUPS = {}


def read(p):
    return p.read_text(encoding="utf-8")


def write(p, s):
    p.write_text(s, encoding="utf-8")


def run(cmd):
    p = subprocess.run(cmd, capture_output=True, text=True)
    return p.returncode, p.stdout + p.stderr


def inject(path, old, new):
    """锚点命中次数必须正好 1 —— 出货文件变了就该当场报错，而不是让自证静默变成摆设。"""
    s = read(path)
    n = s.count(old)
    if n != 1:
        return f"锚点在 {path.name} 里命中 {n} 次（应为 1 次）—— 出货文件改了，请同步本脚本的锚点"
    write(path, s.replace(old, new, 1))
    return None


def case(desc, path, old, new, expect):
    global failures
    err = inject(path, old, new)
    if err:
        print(f"✗ [{desc}] 注入失败：{err}")
        failures += 1
        return
    try:
        rc, out = run(["node", str(NODE_TEST)])
        if rc == 0:
            print(f"✗ [{desc}] 注入后测试仍然全绿 —— 断言是假的（抓不住这个故障）")
            failures += 1
        elif expect not in out:
            print(f"✗ [{desc}] 测试红了，但红的不是预期那条（期望输出含「{expect}」）")
            for line in out.splitlines():
                if "FAIL" in line or "Error" in line:
                    print(f"      {line}")
            failures += 1
        elif "SyntaxError" in out or "ReferenceError" in out or "Cannot read" in out:
            # 崩溃的「红」不算红：它证明不了断言有效，只证明改崩了。
            print(f"✗ [{desc}] 红在崩溃上（抽取器碎掉），不算断言抓住故障")
            failures += 1
        else:
            print(f"✓ [{desc}] → 「{expect}」变红")
    finally:
        for p, content in BACKUPS.items():
            write(p, content)


for _p in (ADMIN_JS, STYLE_CSS):
    BACKUPS[_p] = read(_p)

print("基线：不注入时必须全绿")
rc, out = run(["node", str(NODE_TEST)])
if rc != 0:
    print("前端回归基线就是红的，先修好再来做注入自证：")
    print(out[-2000:])
    sys.exit(1)
print("基线：全绿 ✓\n")

TCHIP = '<span class="tchip" title="${esc(n)}">${esc(short)}</span>'

print("注入自证（每条都必须变红，且红在预期那条）")

case("工具名退回老形态（空格串 <code>，又被从词中间折断）",
     ADMIN_JS,
     TCHIP,
     '${names.map(x => "<code>" + esc(x) + "</code>").join(" ")}',
     "chip 数量守恒")

case("为了好看只渲染前 12 个（D3：少给工具，用户以为那就是全部）",
     ADMIN_JS,
     "    const chips = names.map(n => {",
     "    const chips = names.slice(0, 12).map(n => {",
     "chip 数量守恒")

case("chip 不再带 title（短名成了唯一信息，全名再也核对不了）",
     ADMIN_JS,
     '<span class="tchip" title="${esc(n)}">',
     '<span class="tchip">',
     "每个 chip 的 title 都是完整工具名")

case("工具名不再转义（名字里的 HTML 直接进 innerHTML）",
     ADMIN_JS,
     ">${esc(short)}</span>",
     ">${short}</span>",
     "工具名里的 HTML 被转义")

case("默认展开（折叠默认值被改掉）",
     ADMIN_JS,
     '''<div class="mcp-tools-body"${open ? '' : ' hidden'}>${chips}</div>''',
     '''<div class="mcp-tools-body">${chips}</div>''',
     "默认收着")

case("折叠是假的：CSS 缺 [hidden] 守卫（display:flex 盖掉 display:none）",
     STYLE_CSS,
     ".mcp-tools-body[hidden] { display: none; }",
     "/* 守卫被删掉了 */",
     "CSS 补了 [hidden] 守卫")

case("chip 允许折断（长名字又被从词中间切开）",
     STYLE_CSS,
     "color:var(--text); white-space:nowrap; transition: border-color .14s, background .14s;",
     "color:var(--text); white-space:normal; word-break:break-all;",
     ".tchip 整词不折行")

case("前缀切在词中间（造出 `mcp_datatoolbox_ask` 这种不存在的名字）",
     ADMIN_JS,
     "    const cut = p.lastIndexOf('_');\n    return cut > 0 ? p.slice(0, cut + 1) : '';",
     "    return p;",
     "前缀要么为空、要么以 _ 结尾")

case("状态点被删掉（退回纯文字，和上面的 URL 行又糊在一起）",
     STYLE_CSS,
     ".pill-conn::before { content:'';",
     ".pill-conn-x::before { content:'';",
     ".pill-conn 用伪元素小圆点表状态")

print()
if failures:
    print(f"变异自证未通过：{failures} 条")
    sys.exit(1)
print("变异自证全部通过：注入→精确红、还原→真绿")
