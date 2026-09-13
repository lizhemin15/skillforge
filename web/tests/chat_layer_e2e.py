#!/usr/bin/env python3
"""真 DOM 兜底：用无头浏览器点一遍勾选层，验证"点开就保持打开"。

为什么非要有这个脚本（node 测试不够吗）：
  chat_modes.test.mjs 是**读源码字符串**的断言。它能检查"白名单写着 chipsWrap.contains(t)"，
  却检查不出运行时白名单**失效**。真实翻车案例（2026-09-14，线上实测发现）：

      openSkLayer()  → skLayer.hidden = false
      renderChips()  → 重建整行，被点的那颗 button 变成游离节点
      document 冒泡阶段 → chipsWrap.contains(t) === false → closeSkLayer()
      → 层 hidden=false 又立刻 =true。用户看到"点一下闪一下就没了"。

  静态断言全绿，界面是坏的。所以：凡"事件顺序 / DOM 归属 / 重渲染副作用"这三类，
  必须在真 DOM 里断言一次。

判据：
  - 每条检查打 ok/FAIL 行；任何 FAIL 都算红，退出码 1（不是崩，是断言红）。
  - 服务没起 / playwright 不可用 → 打 SKIP 并 exit 0。**SKIP != PASS**，别把跳过当通过。

用法：
  python3 web/tests/chat_layer_e2e.py            # 默认打 http://127.0.0.1:8092
  BASE=http://127.0.0.1:9999 python3 web/tests/chat_layer_e2e.py
"""
import os
import sys

BASE = os.environ.get('BASE', 'http://127.0.0.1:8092')

fails = []
checks = 0


def check(name, ok, extra=''):
    global checks
    checks += 1
    if ok:
        print(f'ok   {name}')
    else:
        print(f'FAIL {name} {extra}')
        fails.append(name)
    return bool(ok)


def report():
    print(f'--- {checks - len(fails)}/{checks} ok ---')
    if fails:
        print('FAILED: ' + '; '.join(fails))
        return 1
    return 0


def main():
    try:
        from playwright.sync_api import sync_playwright
    except Exception as e:  # pragma: no cover
        print(f'SKIP playwright 不可用：{e}')
        return 0

    with sync_playwright() as p:
        br = p.chromium.launch()
        try:
            pg = br.new_page(viewport={'width': 1280, 'height': 900})
            try:
                pg.goto(BASE, wait_until='domcontentloaded', timeout=15000)
            except Exception as e:
                print(f'SKIP 打不开 {BASE}（服务没起？）：{str(e)[:120]}')
                return 0
            # 等推荐行渲染出来 = 技能库/会话都就绪
            try:
                pg.wait_for_function(
                    "() => { const b = document.querySelector('#chips button');"
                    " return b && b.textContent.trim().length > 0; }", timeout=10000)
            except Exception:
                pass
            return run_checks(pg)
        finally:
            br.close()


def run_checks(pg):
    def trigger_text():
        return pg.eval_on_selector('#chips button', 'e => e.textContent.trim()')

    def trigger_cls():
        return pg.eval_on_selector('#chips button', 'e => e.className')

    def layer_state():
        return pg.evaluate("""() => {
          const l = document.getElementById('sk-layer');
          if (!l) return null;
          const cs = getComputedStyle(l);
          const r = l.getBoundingClientRect();
          return { hidden: l.hidden, display: cs.display, h: Math.round(r.height),
                   visible: !l.hidden && cs.display !== 'none' && r.height > 0 };
        }""")

    def msg_count():
        return pg.eval_on_selector_all('.ch-msg, .msg, [data-role]', 'els => els.length')

    # —— 1. 切到手动档 ——
    pg.click('#mode-manual')
    pg.wait_for_timeout(150)
    mode_on = pg.eval_on_selector('.ch-switch-opt.is-on', 'e => e.textContent.trim()')
    check('切到「指定技能」档（选中态类名生效）', mode_on == '指定技能', f'实际={mode_on}')
    check('推荐行是「选择技能 ▾」单入口', '选择技能' in trigger_text(), f'实际={trigger_text()}')

    # —— 2. 点一下：层必须打开，并**保持**打开 ——
    pg.click('#chips button')
    pg.wait_for_timeout(120)
    open_now = check('点一下就打开（不是要连点两下）',
                     (layer_state() or {}).get('visible'), f'{layer_state()}')
    # ★ 关键：再等一会儿，看它会不会被自己关掉（重渲染 → 游离 target → 白名单失效）
    pg.wait_for_timeout(600)
    still = check('★ 打开后不会被自己关掉（"点一下闪一下"的 bug）',
                  (layer_state() or {}).get('visible'), f'600ms 后 state={layer_state()}')
    if not (open_now and still):
        return report()      # 层是关的就别硬点，否则 Playwright 等 30s 抛异常 = 崩溃红，不算红
    check('触发器点亮显示"展开中"', 'is-on' in trigger_cls(), trigger_cls())

    rows_all = pg.eval_on_selector_all('.ch-skrow', 'els => els.length')
    check('层里铺满了技能（全量，不是 4 颗）', rows_all >= 10, f'rows={rows_all}')

    # —— 3. 搜索真过滤 ——
    pg.fill('#sk-q', '合同')
    pg.wait_for_timeout(200)
    rows_q = pg.eval_on_selector_all('.ch-skrow', 'els => els.length')
    check('搜索"合同"真的过滤（不是列全部）', 0 < rows_q < rows_all, f'{rows_q}/{rows_all}')
    pg.fill('#sk-q', '')
    pg.wait_for_timeout(200)

    # —— 4. 勾一个 → 关层 + 落盘 ——
    pg.click('.ch-skrow')
    pg.wait_for_timeout(200)
    check('勾完自动关层（下一步一定是打字）', not (layer_state() or {}).get('visible'),
          f'{layer_state()}')
    check('勾完触发器显示"已指定"', '已指定' in trigger_text(), f'实际={trigger_text()}')
    picked = pg.evaluate("() => localStorage.getItem('skillforge.chatskill')")
    check('指定技能写进 localStorage（刷新不丢）', bool(picked), f'值={picked}')

    # —— 5. 再勾一次 = 取消 ——
    pg.click('#chips button')
    pg.wait_for_timeout(200)
    if check('再点能重新打开（取消路径的入口）', (layer_state() or {}).get('visible'), ''):
        pg.click('.ch-skrow')
        pg.wait_for_timeout(200)
        check('再勾一次 = 取消指定', '已指定' not in trigger_text(), f'实际={trigger_text()}')

    # —— 6. 点层外关层 ——
    pg.click('#chips button')
    pg.wait_for_timeout(200)
    if check('重新打开（点外关闭的前置）', (layer_state() or {}).get('visible'), ''):
        pg.click('body', position={'x': 5, 'y': 5})
        pg.wait_for_timeout(200)
        check('点层外关层', not (layer_state() or {}).get('visible'), f'{layer_state()}')

    # —— 7. 没选技能点发送 → 开层且保持开着，且没发出去 ——
    pg.fill('#chat-input', '帮我写个采购合同')
    pg.click('#chat-send')
    pg.wait_for_timeout(300)
    check('★ 没选技能点发送 → 打开勾选层（不是"毫无反应"）',
          (layer_state() or {}).get('visible'), f'{layer_state()}')
    pg.wait_for_timeout(600)
    check('★ 且保持开着（没被 submit 冒泡当场关掉）',
          (layer_state() or {}).get('visible'), f'{layer_state()}')
    check('这一下没发出消息（空发送不许发）', msg_count() == 0, f'msgCount={msg_count()}')

    # —— 8. 回自动档：胶囊只填不发 ——
    pg.click('#mode-auto')
    pg.wait_for_timeout(200)
    check('切回自动档收掉层', not (layer_state() or {}).get('visible'), '')
    pg.fill('#chat-input', '')
    pg.click('#chips button')
    pg.wait_for_timeout(250)
    val = pg.eval_on_selector('#chat-input', 'e => e.value')
    check('点胶囊 = 填进输入框', len(val) > 0, f'value={val!r}')
    check('点胶囊 = 不直接发送', msg_count() == 0, f'msgCount={msg_count()}')
    check('光标落在末尾（接着就能改）', pg.evaluate(
        "() => { const i = document.getElementById('chat-input');"
        " return i.selectionStart === i.value.length && document.activeElement === i; }"), '')

    return report()


if __name__ == '__main__':
    sys.exit(main())
