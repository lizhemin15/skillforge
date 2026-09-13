#!/usr/bin/env python3
# 双向自证：注入 3 种破坏，看新加断言是否真的见红；还原后必须回绿。
# 只碰这三处，跑完必还原（finally 兜底）。
import subprocess, shutil, sys, os

WEB = '/root/skillforge/web'
JS, CSS, TEST = f'{WEB}/js/chat.js', f'{WEB}/css/style.css', f'{WEB}/tests/chat_modes.test.mjs'
baks = {p: p + '.selfcheck.bak' for p in (JS, CSS)}
for p in baks:
    shutil.copy2(p, baks[p])

def run():
    r = subprocess.run(['node', TEST], capture_output=True, text=True, cwd='/root/skillforge')
    fails = [l.strip() for l in r.stdout.splitlines() if 'FAIL' in l]
    # ⚠️ 「红在崩溃上不算红」：rc!=0 但没有 FAIL 行 = 脚本抛异常崩了，
    # 那种红证明不了断言有效（注入本身把代码改到跑不起来）。
    crashed = r.returncode != 0 and not fails
    return r.returncode, fails, r.stderr.strip().splitlines()[-3:]

INJECTIONS = [
    ('1) chat.js 选中类名改回错的那个（is-on → on）', JS, "const ON_CLASS = 'is-on';", "const ON_CLASS = 'on';"),
    ('2) 旧档不清除（切档后两档同时亮）', JS,
     "    auto.classList.toggle(ON_CLASS, mode === 'auto');",
     "    auto.classList.toggle(ON_CLASS, true);"),
    ('3) CSS 选择器改掉（只改一边，另一侧必须发现）', CSS,
     ".ch-switch-opt.is-on { color: var(--text); font-weight: 600; }",
     ".ch-switch-opt.on { color: var(--text); font-weight: 600; }"),
    # 4) 把「标签 = 要发出去的那句话」改回旧行为（标签 = 技能名）：
    #    界面上就并排出现「✓ 采购合同」+「采购合同」两颗同名胶囊。
    ('4) 示范 chip 标签退回技能名（与「✓」那颗撞名）', JS,
     '    a.label = shortAskLabel(a.send, sk.name || sk.slug);',
     '    a.label = sk.name || sk.slug;'),
]

try:
    print('=== 基线 ===')
    rc, fails, err = run()
    print(f'exit={rc}  {len(fails)} 条红')
    assert rc == 0, '基线就不绿，别往下走了'
    for name, path, old, new in INJECTIONS:
        src = open(path, encoding='utf-8').read()
        if old not in src:
            print(f'\n!! 注入点没找到，注入无效（等于没测）：{name}')
            sys.exit(2)
        open(path, 'w', encoding='utf-8').write(src.replace(old, new, 1))
        rc, fails, err = run()
        if rc != 0 and fails:
            verdict = '见红 ✓'
        elif rc != 0:
            verdict = '❌ 假红 —— 是崩溃不是断言失败，本注入无效：' + ' | '.join(err)
        else:
            verdict = '❌ 假绿！改坏了还是绿的'
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
        for f in fails[:6]:
            print('   ', f)
