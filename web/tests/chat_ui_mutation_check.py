#!/usr/bin/env python3
"""双向自证：往出货文件里注入真实故障，看 chat_modes.test.mjs 的断言是不是真会红。

为什么必须有这个脚本（而不是"测试全绿就够了"）：
  全绿只说明**现在**没坏，不说明断言抓得住坏。一条写歪的断言（命中函数定义、
  只比子串、条件写反）永远是绿的，界面坏成什么样它都不会响 —— 这种"假绿"
  比没测试更危险，因为它让人以为有防线。

判据（三重，缺一不可）：
  1. 注入点必须存在（找不到 = 注入无效 = 等于没测，直接 exit 2）
  2. 注入后**必须出现 FAIL 行**。「红在崩溃上不算红」：rc!=0 但没有 FAIL 行，
     说明注入把代码改到跑不起来了，那种红证明不了任何断言有效。
  3. 还原后必须回绿（不回绿说明注入有残留，下一次跑基线就已经脏了）

改断言之后必须重跑本脚本 —— 断言改了而自证没重跑，等于用新的假绿盖住旧的。
"""
import subprocess
import shutil
import sys
import os

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
JS = os.path.join(ROOT, 'web/js/chat.js')
CSS = os.path.join(ROOT, 'web/css/style.css')
HTML = os.path.join(ROOT, 'web/index.html')
TEST = os.path.join(ROOT, 'web/tests/chat_modes.test.mjs')

TARGETS = (JS, CSS, HTML)
baks = {p: p + '.mutation.bak' for p in TARGETS}
for p in TARGETS:
    shutil.copy2(p, baks[p])


def run():
    r = subprocess.run(['node', TEST], capture_output=True, text=True, cwd=ROOT)
    fails = [l.strip() for l in r.stdout.splitlines() if 'FAIL' in l]
    return r.returncode, fails, [l for l in r.stderr.strip().splitlines() if l.strip()][-2:]


INJECTIONS = [
    # —— 上一轮留下的（档位胶囊）——
    ('1) 档位选中类名改回错的那个（is-on → on）',
     JS, "const ON_CLASS = 'is-on';", "const ON_CLASS = 'on';"),
    ('2) 切档时旧档不清除（两档同时亮）',
     JS, "    auto.classList.toggle(ON_CLASS, mode === 'auto');",
     "    auto.classList.toggle(ON_CLASS, true);"),
    ('3) CSS 选择器改掉（只改一边，另一侧必须发现）',
     CSS, ".ch-switch-opt.is-on { color: var(--text); font-weight: 600; }",
     ".ch-switch-opt.on { color: var(--text); font-weight: 600; }"),
    ('4) 示范 chip 标签退回技能名（与入口那颗撞名）',
     JS, '    a.label = shortAskLabel(a.send, sk.name || sk.slug);',
     '    a.label = sk.name || sk.slug;'),

    # —— 本轮新契约 A：点推荐胶囊 = 填进输入框，不直接发 ——
    # 退回"点了就发"：用户想改个字数就没机会了，会白烧一次模型调用。
    ('5) 点推荐胶囊退回"直接发送"（用户没机会编辑）',
     JS, "    setInput(q);\n    toEnd(input);\n    input.focus();",
     "    submit();"),
    # 退回"有草稿就不填"：点了跟没点一样，正是用户抱怨的"毫无反应"。
    ('6) 有草稿时不填（点了没反应）',
     JS, "    setInput(q);\n    toEnd(input);\n    input.focus();",
     "    if (!(input.value || '').trim()) { setInput(q); }\n    input.focus();"),

    # —— 本轮新契约 B：指定技能 = 勾选层 ——
    # 取消语义丢掉：勾了就没法取消，用户只能被锁在这个技能上。
    ('7) 勾选丢掉取消语义（再勾一次不取消）',
     JS, "    return currentSlug === slug ? '' : slug;", "    return slug;"),
    # 搜索不过滤：搜"采购"还是列全部，用户以为搜索坏了。
    ('8) 面板搜索不过滤（搜了等于没搜）',
     JS, "    return orderSkills(list).filter((sk) => skillMatches(sk, q));",
     "    return orderSkills(list);"),
    # 只搜名字：关键词常在 description 里，用户搜"合同"搜不到就以为技能没了。
    ('9) 搜索只匹配名字（丢了说明字段）',
     JS, "    const hay = [sk.name, sk.slug, sk.description]",
     "    const hay = [sk.name]"),
    # 层关不掉：display:flex 盖掉 [hidden]，JS 设了 hidden 也没用。
    ('10) CSS 的 [hidden] 守卫被破坏（层关不掉）',
     CSS, ".ch-sklayer[hidden] { display: none; }", ".ch-sklayer[hidden] { display: flex; }"),
    # 点外关闭不再排除推荐行：手动档点发送会主动开层，随即被冒泡关掉 → "点了没反应"。
    ('11) 点外关闭不再排除推荐行/输入栏（点发送看不见面板）',
     JS, "      if (chipsWrap && chipsWrap.contains(t)) return;\n", ""),
    # 被拦住后不开层：只剩抖一下，用户还是不知道去哪选技能。
    ('12) 发送被拦后不开勾选层（用户找不到出路）',
     JS, "      openSkLayer();\n      return;\n    }\n    input.value = '';", "      return;\n    }\n    input.value = '';"),
    # 层被塞回 #chips 里：renderChips() 清空 innerHTML 时会连带清掉它。
    ('13) 勾选层挪进 #chips（被 renderChips 清掉）',
     HTML, '      <div class="ch-sklayer" id="sk-layer" hidden>',
     '      <div id="chips" aria-live="polite"></div>\n      <div class="ch-sklayer-tmp" id="sk-layer" hidden>'),
]

bad = 0
try:
    print('=== 基线 ===')
    rc, fails, err = run()
    print(f'exit={rc}  {len(fails)} 条红')
    if rc != 0:
        print('基线就不绿，别往下走了')
        sys.exit(3)

    for name, path, old, new in INJECTIONS:
        src = open(path, encoding='utf-8').read()
        if old not in src:
            print(f'\n!! 注入点没找到，注入无效（等于没测）：{name}')
            bad += 1
            continue
        open(path, 'w', encoding='utf-8').write(src.replace(old, new, 1))
        rc, fails, err = run()
        if rc != 0 and fails:
            verdict = '见红 ✓'
        elif rc != 0:
            verdict = '❌ 假红 —— 是崩溃不是断言失败，本注入无效：' + ' | '.join(err)
            bad += 1
        else:
            verdict = '❌ 假绿！改坏了还是绿的'
            bad += 1
        print(f'\n=== {name} → {verdict} ===')
        for f in fails[:6]:
            print('   ', f)
        shutil.copy2(baks[path], path)
finally:
    for p, b in baks.items():
        shutil.copy2(b, p)
        os.remove(b)
    rc, fails, err = run()
    print(f'\n=== 还原后 ===\nexit={rc}  剩 {len(fails)} 条红')
    if rc != 0:
        for f in fails[:8]:
            print('   ', f)
        bad += 1

print(f'\n=== 自证结论：{len(INJECTIONS)} 个注入，{bad} 个不合格 ===')
if bad:
    sys.exit(1)
print('全部注入都见红、还原回绿 ✓')
