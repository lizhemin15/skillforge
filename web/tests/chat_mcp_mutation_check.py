#!/usr/bin/env python3
"""双向自证：往出货文件里注入真实故障，看 chat_mcp.test.mjs 的断言是不是真会红。

为什么必须有这个脚本（而不是"测试全绿就够了"）：
  全绿只说明**现在**没坏，不说明断言抓得住坏。这个文件的断言尤其容易假绿：
    · 一半断言是从出货 JS 里抠函数出来跑的，**签名被改名就整段静默跳过**；
    · 另一半是 DOM/CSS 的结构断言，正则写歪一点就永远命中；
    · renderMCP 那几条骑在自造假 DOM 上，假 DOM 少实现一个面（比如 classList.toggle
      是个空函数）→ 断言恒真，界面坏成什么样它都不响。
  所以下面 6 条注入，前 5 条打业务逻辑，第 6 条专门打"抠函数"这个洞。

判据（四重，缺一不可）：
  1. 注入点必须存在（找不到 = 注入无效 = 等于没测，直接算不合格）
  2. 注入后**必须出现 FAIL 行**。「红在崩溃上不算红」：rc!=0 但一行 FAIL 都没有，
     说明注入把代码改到跑不起来了，那种红证明不了任何断言有效。
  3. FAIL 行还必须**是预期那一条**（见每条的 expect）。红在别处不算红 ——
     改坏 A 却让 D 跳红线，等于这条断言根本没在盯它该盯的东西。
  4. 还原后必须回绿（不回绿说明注入有残留，下一次跑基线就已经脏了）

改断言之后必须重跑本脚本 —— 断言改了而自证没重跑，等于用新的假绿盖住旧的。

用法：python3 web/tests/chat_mcp_mutation_check.py
"""
import os
import shutil
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
JS = os.path.join(ROOT, 'web/js/chat.js')
CSS = os.path.join(ROOT, 'web/css/style.css')
HTML = os.path.join(ROOT, 'web/index.html')
TEST = os.path.join(ROOT, 'web/tests/chat_mcp.test.mjs')

TARGETS = (JS, CSS, HTML)
baks = {p: p + '.mcpmut.bak' for p in TARGETS}
for p in TARGETS:
    shutil.copy2(p, baks[p])


def restore():
    for p in TARGETS:
        shutil.copy2(baks[p], p)


def run():
    r = subprocess.run(['node', TEST], capture_output=True, text=True, cwd=ROOT)
    fails = [l.strip() for l in r.stdout.splitlines() if l.strip().startswith('FAIL')]
    err = [l for l in r.stderr.strip().splitlines() if l.strip()][-2:]
    return r.returncode, fails, err


INJECTIONS = [
    # —— Bug M1：不勾也带上 ——
    # ① 请求体里的兜底守卫被"顺手简化"掉（新人重构最爱干的事）。后果：
    #    调用方忘传 → mcp 字段变成 undefined → JSON 序列化时字段消失 → 后端只能靠猜。
    ('1) 请求体 mcp 的兜底守卫被删（忘传 → 字段消失，后端只能猜）',
     JS, 'mcp: Array.isArray(mcpIds) ? mcpIds : []', 'mcp: mcpIds',
     '兜底为空数组'),

    # —— Bug M2：勾选集直接照发，不跟服务端清单求交集 ——
    # ② 交集那一步退化成原样透传。后果：管理员停用/删掉某台 MCP 后，用户浏览器里
    #    还留着旧 id，照发 → 后端闸门被打开（"用户选中了 MCP"）但工具表一台都挂不上
    #    → 模型空转到轮数上限 → 编个答案给用户。这是"看不见的坏"。
    ('2) 勾选集 ∩ 清单退化成原样透传（已下架的 id 照发）',
     JS, 'return (picked || []).filter((id) => have.has(id));', 'return (picked || []).slice();',
     '已下架的剔掉'),

    # —— Bug M3：没得选还常驻按钮 ——
    # ③ HTML 上那个 hidden 被删。后果：管理员一台都没开，用户端照旧多一个按钮，
    #    点开是空面板 —— 用户会以为"这里坏了"，而不是"没有可用的数据源"。
    ('3) index.html #mcp-box 默认 hidden 被删（没得选也露按钮）',
     HTML, '<div class="ch-mcp" id="mcp-box" hidden>', '<div class="ch-mcp" id="mcp-box">',
     '#mcp-box 默认带 hidden'),

    # —— Bug M4：弹出层关不掉 ——
    # ④ 守卫那条 CSS 被删。.ch-mcp-layer 自己写了 display:flex，会盖掉浏览器默认的
    #    [hidden]{display:none}；少了守卫，JS 里设了 hidden 层还挂在屏幕上。
    #    技能层踩过一模一样的坑，所以这条单独守。
    ('4) .ch-mcp-layer[hidden] 守卫被删（层设了 hidden 也还挂在屏幕上）',
     CSS, '.ch-mcp-layer[hidden] { display: none; }', '',
     '.ch-mcp-layer[hidden]'),

    # —— Bug M5：清空清单时按钮不收回去 ——
    # ⑤ renderMCP 里"清单空 → 收按钮"那条守卫被删（只留 return）。后果：管理员把最后
    #    一台 MCP 停用后，用户**不刷新页面**就还能看到一个按钮和上一次的勾选态。
    #    这条同时证明自造的假 DOM 真在跑（否则删了代码断言照样绿）。
    ('5) renderMCP 里"清单空 → 收按钮"守卫被删（停用后按钮残留）',
     JS, 'if (!mcpServers.length) { mcpBox.hidden = true; closeMCPLayer(); return; }',
     'if (!mcpServers.length) { return; }',
     '必须收回去'),

    # —— 尺子自检：抽不到函数必须红 ——
    # ⑥ 把被抠的函数改个名。这是打"静默空跑"这个洞：如果测试只让 bind 抛异常，
    #    这里是崩溃红（rc!=0 但没有 FAIL 行），按判据不合格。
    ('6) 被抠的函数改名（尺子抽不到 → 必须报 FAIL，而不是静默空跑/崩溃红）',
     JS, 'function mcpPayloadIds(picked, servers) {', 'function mcpPayloadIdsV2(picked, servers) {',
     '出货文件里能抠到 function mcpPayloadIds'),
]


def main():
    rc0, fails0, err0 = run()
    if rc0 != 0:
        print('基线就是红的，先修好再来自证：')
        for f in fails0:
            print('  ' + f)
        restore()
        return 2
    print(f'基线绿（{len(open(TEST, encoding="utf-8").read().splitlines())} 行测试文件）\n')

    bad = 0
    for label, path, old, new, expect in INJECTIONS:
        src = open(path, encoding='utf-8').read()
        if old not in src:
            print(f'!! {label}')
            print(f'   注入点不存在：{old[:70]!r} —— 注入无效，等于没测')
            bad += 1
            continue
        open(path, 'w', encoding='utf-8').write(src.replace(old, new, 1))
        rc, fails, err = run()
        restore()

        hit = [f for f in fails if expect in f]
        ok = rc != 0 and bool(fails) and bool(hit)
        why = ''
        if rc == 0:
            why = '注入后仍然全绿 —— 这条断言压根没在盯它'
        elif not fails:
            why = f'rc={rc} 但没有 FAIL 行（崩溃红不算红）：{err}'
        elif not hit:
            why = f'FAIL 的不是预期那条（预期含 {expect!r}），实际：{fails}'
        print(('ok    ' if ok else 'MISS  ') + label)
        if ok:
            print(f'      → {hit[0][:120]}')
        else:
            print(f'      → {why}')
            bad += 1

    rc1, fails1, _ = run()
    if rc1 != 0:
        print(f'\n!! 还原后没回绿，注入有残留：{fails1}')
        bad += 1
    else:
        print('\n还原后回绿')

    for p in TARGETS:
        os.remove(baks[p])
    print(f'\n注入 {len(INJECTIONS)} 条，不合格 {bad} 条')
    return 1 if bad else 0


if __name__ == '__main__':
    sys.exit(main())
