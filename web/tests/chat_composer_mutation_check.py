#!/usr/bin/env python3
"""双向自证：往出货文件里注入真实故障，看 chat_composer.test.mjs 的断言是不是真会红。

为什么必须有这个脚本（而不是"测试全绿就够了"）：
  全绿只说明**现在**没坏，不说明断言抓得住坏。一条写歪的断言（命中函数定义、
  只比子串、条件写反、骑在空集上）永远是绿的，界面坏成什么样它都不会响 ——
  这种"假绿"比没测试更危险，因为它让人以为有防线。
  本文件的断言尤其容易假绿：B 段是从出货文件里按标记切片、再 `new Function`
  抽出来跑的，**标记被删就会静默跳过**（半条都不会红）。所以第 8 条注入专门
  打这个洞。

判据（四重，缺一不可）：
  1. 注入点必须存在（找不到 = 注入无效 = 等于没测，直接算不合格）
  2. 注入后**必须出现 FAIL 行**。「红在崩溃上不算红」：rc!=0 但没有 FAIL 行，
     说明注入把代码改到跑不起来了，那种红证明不了任何断言有效。
  3. FAIL 行还必须**是预期那一条**（见每条的 expect）。红在别处不算红 ——
     改坏 A 却让 D 跳红线，等于这条断言根本没在盯它该盯的东西。
  4. 还原后必须回绿（不回绿说明注入有残留，下一次跑基线就已经脏了）

改断言之后必须重跑本脚本 —— 断言改了而自证没重跑，等于用新的假绿盖住旧的。

用法：python3 web/tests/chat_composer_mutation_check.py
"""
import os
import shutil
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
JS = os.path.join(ROOT, 'web/js/chat.js')
CSS = os.path.join(ROOT, 'web/css/style.css')
HTML = os.path.join(ROOT, 'web/index.html')
TEST = os.path.join(ROOT, 'web/tests/chat_composer.test.mjs')

TARGETS = (JS, CSS, HTML)
baks = {p: p + '.composer.bak' for p in TARGETS}
for p in TARGETS:
    shutil.copy2(p, baks[p])


def run():
    r = subprocess.run(['node', TEST], capture_output=True, text=True, cwd=ROOT)
    fails = [l.strip() for l in r.stdout.splitlines() if l.strip().startswith('FAIL')]
    err = [l for l in r.stderr.strip().splitlines() if l.strip()][-2:]
    return r.returncode, fails, err


INJECTIONS = [
    # —— A 段：布局（胶囊不许占输入框的行）——
    # ① 把胶囊行改回 flex-basis:auto（= 当年那颗 166px 的胶囊又跟 textarea 挤同行，
    #    textarea 被推到 x≈518）。用户原话的形态。
    ('1) 胶囊行退回 flex-basis:auto（输入框又被挤到右边）',
     CSS, '.ch-mrow { flex: 0 0 100%;', '.ch-mrow { flex: 0 0 auto;', '整行'),
    # ② 包一层但是名字对不上（CSS 里那条 .ch-mrow 落不到它头上）——真故障常这样长出来：
    #    HTML 改了、CSS 没跟上。
    ('2) index.html 的 .ch-mrow 类名漂移（CSS 落不到它头上）',
     HTML, 'class="ch-mrow"', 'class="ch-mrow-x"', '直系子元素'),
    # ③ ★ DOM 层对应那条：把 .ch-mrow 包裹拆掉 → 胶囊退回"和 textarea 同行"。
    #    （改前就是这个 DOM 形状：胶囊直接做 .ch-input-box 的直系子元素）
    #    ⚠️ 2026-09-19 这里曾经断过（注入点找不到就被判不合格，CI 直接红）：
    #    原来是把「.ch-mrow 开标签 … 到它自己的闭标签」整块当 old，MCP 数据源入口
    #    落进同一个包裹之后，这段就不再连续了。**不要把 MCP 那段抄进 old** ——
    #    那会让注入点跟着 MCP 的 HTML 一起脆掉，以后谁动一下 MCP 按钮就把这条自证弄红。
    #    改成只注射**包裹的开标签**：包裹没了，胶囊就成了 .ch-input-box 的直系子元素，
    #    正是改前那个"输入框被推到右边"的根因，断言（直系子元素必须 3 个）照样逮得住。
    ('3) 胶囊行包裹被拆掉（回滚修复：胶囊又和输入框同行）',
     HTML,
     '        <div class="ch-mrow">\n'
     '          <div class="ch-switch" id="ch-switch" data-mode="auto">\n',
     '        <div class="ch-switch" id="ch-switch" data-mode="auto">\n',
     '直系子元素'),
    # ④ 全局 textarea{min-height:96px} 重新漏回输入框。
    #    改前 .ch-input 显式写 min-height:0（为了压掉全局 96）；20260917 起改成显式 3 行 88px。
    #    删掉这条声明 → 有效值落回全局 96 → 默认高度漂到「4 行多一点」，正好被 88±3 的容差逮住。
    ('4) .ch-input 不再显式声明 min-height（吃全局 textarea 的 96px，默认高度漂成 96）',
     CSS, '.ch-input { min-height: 88px; }\n', '', '默认高度 = 3 行'),
    # ⑤ textarea 不再占满剩余宽度。
    ('5) .ch-input 退回 flex:none（不再占满剩余宽度）',
     CSS, '  flex: 1; min-width: 0; resize: none;', '  flex: none; min-width: 0; resize: none;', '撑满'),

    # —— B 段：贴底滚动 ——
    # ⑥ 平滑滚动回归：流式每次赋值都会重启动画 → 实测落后 994px。
    # 注入方式只能是「在文件末尾再追加一条 smooth」——同名后者覆盖，正是当年它
    # 生效的位置。（把末尾那条 auto **删掉** 不算故障：没有声明时浏览器默认就是
    # auto，实测也不会坏。注入必须是真故障，否则自家断言报红反而是误报。）
    ('6) 文件末尾追加 .ch-scroll{scroll-behavior:smooth}（后者覆盖 → 滚动追不上）',
     CSS, '.ch-scroll { scroll-behavior: auto; }', '.ch-scroll { scroll-behavior: auto; }\n.ch-scroll { scroll-behavior: smooth; }', 'smooth'),
    # ⑦ 补帧被删：赋值之后 markdown 重排又长高，视口停在半路（"差一行没到底"）。
    ('7) keepBottom 去掉双帧补正（重排之后停在中途）',
     JS,
     '    if (typeof window.requestAnimationFrame !== \'function\' || stickRaf) return;\n'
     '    stickRaf = window.requestAnimationFrame(function () {\n'
     '      stickRaf = window.requestAnimationFrame(function () {\n'
     '        stickRaf = 0;\n'
     '        scroll.scrollTop = scroll.scrollHeight;\n'
     '      });\n'
     '    });\n',
     '    return;\n', '补帧'),
    # ⑧ 连同步赋值也去掉 —— 流式期间每段文字都要等下一帧，中间是空的。
    ('8) keepBottom 连同步赋值都不要了',
     JS, '  function keepBottom() {\n    scroll.scrollTop = scroll.scrollHeight;', '  function keepBottom() {', '顶到底'),
    # ⑨ ★ 打「静默跳过」这个洞：标记删掉 → 切片取不到 → 整段 B 段自动跳过。
    #    没有这条，删掉标记 + 删掉补帧可以同时全绿。
    ('9) 删掉 composer:stick-begin 标记（B 段会静默跳过 → 必须自己报红）',
     JS, '  // --- composer:stick-begin', '  // ', '切到 keepBottom'),

    # —— C 段：高度（默认 3 行 / 上限 / 打字即增高）——
    # ⑩ ★ 回滚本次修复的根因：JS 里又把上限写死成 160（改前的样子）。
    #    这等价于「CSS 说 240、JS 只给 160」的两处漂移，正是要防的那类 bug。
    ('10) autoGrow 上限写死回 160（不再从 CSS 读 → 涨到 240 却只显示 160）',
     JS,
     "    const cs = getComputedStyle(input);\n"
     "    const cap = parseFloat(cs.maxHeight);\n"
     "    input.style.height = Math.min(input.scrollHeight, isNaN(cap) ? Infinity : cap) + 'px';\n",
     "    input.style.height = Math.min(input.scrollHeight, 160) + 'px';\n",
     '被上限夹住'),
    # ⑪ ★ 回滚最核心那条故障：autoGrow 没接 input 事件 → 打字永远不涨（改前恒定 41px）。
    ('11) 删掉 input → autoGrow 的接线（打字不再增高，= 改前的真实故障）',
     JS, '    input.addEventListener(\'input\', autoGrow);\n', '', '已挂到 input 事件'),
    # ⑫ 去掉「先归零」：内容变短后回不去（框被永久撑在上限上）。
    ('12) autoGrow 不再先把高度归零（内容删掉后回不落）',
     JS, "    input.style.height = 'auto';   // 必须先归零：不归零的话 scrollHeight 被当前高度锁住，只降不升\n",
     '', '能回落'),
    # ⑬ CSS 上限被砍回 160（改前的死配置）——A 段必须逮住。
    ('13) CSS .ch-input 上限砍回 160px（长素材只多显示 3 行）',
     CSS, 'font-size: 14.5px; line-height: 1.6; max-height: 240px;',
     'font-size: 14.5px; line-height: 1.6; max-height: 160px;', '上限 240px'),
    # ⑭ HTML 行数退回 1（首屏兜底又变窄）。
    ('14) index.html rows 退回 1（首屏兜底窄回去）',
     HTML, 'id="chat-input" class="ch-input" rows="3"', 'id="chat-input" class="ch-input" rows="1"', 'rows ≥ 3'),
]

bad = 0
try:
    print('=== 基线 ===')
    rc, fails, err = run()
    print(f'exit={rc}  {len(fails)} 条红')
    if rc != 0:
        print('基线就不绿，别往下走了')
        sys.exit(3)

    for name, path, old, new, expect in INJECTIONS:
        src = open(path, encoding='utf-8').read()
        if old not in src:
            print(f'\n!! 注入点没找到，注入无效（等于没测）：{name}')
            bad += 1
            continue
        open(path, 'w', encoding='utf-8').write(src.replace(old, new, 1))
        rc, fails, err = run()
        hit = [f for f in fails if expect in f]
        if rc != 0 and not fails:
            verdict = '❌ 假红 —— 是崩溃不是断言失败，本注入无效：' + ' | '.join(err)
            bad += 1
        elif not fails:
            verdict = '❌ 假绿！改坏了还是绿的'
            bad += 1
        elif not hit:
            verdict = f'❌ 红在别处（{len(fails)} 条红，没有一条含「{expect}」）—— 这条注入没被它该盯的断言盯住'
            bad += 1
        else:
            verdict = '见红 ✓'
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
