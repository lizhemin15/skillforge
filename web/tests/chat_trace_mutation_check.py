#!/usr/bin/env python3
"""双向自证：往出货文件 chat.js 里注入真实故障，看 chat_trace.test.mjs 的**材料断言**是不是真会红。

为什么必须有这个脚本：
  chat_trace.test.mjs 已经跑在 CI 里，它断言「有材料时渲染 .ctk-mat」。但这些断言此前
  只跑不证 —— 全绿的断言集只说明**现在**没坏，不说明它抓得住坏。一条写歪的断言
  （命中注释、只比子串、条件写反、骑在空集上）永远是绿的，界面坏成什么样都不响。
  这种假绿比没测试更危险：它让人以为这块有人看着。

材料这条线尤其值得自证，因为它的历史形态就是「后端发了、前端没渲染」：
  用户原话「现在速度过于慢了，中间可以流式输出思考的一些中间材料，现在一直卡着计时，
  用户体验不佳」。后端把材料挂上了 TraceStep.Material，只要前端那一行的三元条件被改坏
  （或转义被顺手去掉、或空材料也留空壳），用户屏幕上就退回「只有跳秒的计时器」——
  而 Go 侧的单测、SSE 侧的验收**全都是绿的**，因为后端确实发了。

判据（四重，缺一不可，沿用 chat_composer_mutation_check.py 的规矩）：
  1. 注入点必须存在（找不到 = 注入无效 = 等于没测，直接不合格）
  2. 注入后**必须出现 FAIL 行**。「红在崩溃上不算红」：rc!=0 但没有 FAIL 行，
     说明注入把文件改到跑不起来了，那种红证明不了任何断言有效。
  3. FAIL 行还必须是**预期那一条**。改坏 A 却让 B 跳红线，等于这条断言没在盯它该盯的。
  4. 还原后必须回绿（不回绿说明注入有残留，下一次跑基线就已经脏了）

用法：python3 web/tests/chat_trace_mutation_check.py
"""
import os
import shutil
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
JS = os.path.join(ROOT, 'web/js/chat.js')
TEST = os.path.join(ROOT, 'web/tests/chat_trace.test.mjs')
BAK = JS + '.trace.bak'

# 材料渲染那一行的三元条件（唯一锚点：连缩进与下一行一起锚，避免命中别处的 `s.material`）。
# matOf 现在的形状是「先判 material_log（滚动日志），没有日志才退回单行 material」，
# 所以锚点要跟着实现走 —— 锚点找不到脚本会自己报 FAIL，不会静默空跑。
COND = """    return s.material
      ? '<span class="ctk-mat">"""


def run_test():
    """跑 node 测试，返回 (rc, stdout)。"""
    p = subprocess.run(['node', TEST], cwd=ROOT, capture_output=True, text=True)
    return p.returncode, p.stdout + p.stderr


def fail_lines(out):
    return [ln.strip() for ln in out.splitlines() if ln.strip().startswith('FAIL')]


def main():
    if not os.path.exists(JS):
        print(f'FAIL 找不到出货文件 {JS}')
        return 1
    if COND not in open(JS, encoding='utf-8').read():
        print('FAIL 材料渲染锚点找不到 —— chat.js 的实现变了，本脚本的自证对象已失效。')
        print('     请对照 web/js/chat.js 里 .ctk-mat 那一段更新 COND，别直接删脚本。')
        return 1

    shutil.copy2(JS, BAK)
    rc_bad = 0
    try:
        # ---- 基线必须绿：否则后面的「变红」说明不了任何事 ----
        rc, out = run_test()
        if rc != 0 or fail_lines(out):
            print('FAIL 基线不绿 —— 先修实现/断言，再来自证。')
            print(out[-1500:])
            return 1
        print('ok   基线绿（chat_trace.test.mjs 全过）')

        # 每条: (名字, 作用域, 被替换的原文, 替换成的文本, 期望转红的断言名, 说明)
        # 第 4 项必须精确 —— 只证明「改坏了会红」不够，得证明**改坏成那种故障**会红。
        # 作用域 'cond' = 只在材料那一行的三元条件里替换；'file' = 全文件替换（要求唯一命中）。
        muts = [
            ('材料整块不渲染', 'cond', 's.material', 'false', '有材料时渲染 .ctk-mat',
             '三元条件恒假 → 材料 span 根本不进 DOM。这正是「后端发了、前端没渲染」的故障形态。'),
            ('材料不转义', 'file', 'esc(s.material)', "(s.material || '')", '材料被转义（不产生真标签）',
             '去掉 esc → 模型吐出的任意文本直接当 HTML。真实注入点，且是**静默**的：不报错、不断言、只是能被 XSS。'),
            ('无材料也留空壳', 'cond', 's.material', 'true', '无材料时不留空壳',
             '三元条件恒真 → 每次心跳都多一个空的「思考中」框，把行撑高、时间线变成一堵墙。'),
            # ---- 思考日志（MaterialLog）这一组 ----
            # 上面三条守的是单行材料；用户投诉的第二个形态是「一行字在地上抖」：
            # 单行窗口只有 160 字，每帧原地替换，屏幕上除了跳秒什么都读不出来。
            # 日志窗口就是为这个加的，这三条守它。
            ('日志窗口渲染不出（退回单行抖）', 'file',
             "const log = s.material_log || '';",
             "const log = '';",
             '有 material_log 时渲染滚动日志容器',
             '日志分支恒不进 → 退回 160 字单行原地替换，正是用户说的「一行字在抖」；'
             '此时后端仍在发 material_log，Go 侧与 SSE 验收全绿，只有这条断言能抓。'),
            ('日志被截成短尾巴（整段在长退化成一行）', 'file',
             'esc(log)',
             "esc(log.slice(-40))",
             '日志整段进 DOM（不是只剩尾巴一句）',
             '日志只送 40 字 → 容器有、贴底也对，但「整段在长」没了，用户还是只看到一行在换。'),
            ('贴底跟随失效（新内容滚出视野）', 'file',
             "const follow = body.dataset.matFollow !== '0';",
             "const follow = false;",
             '贴底跟随：首帧就把日志滚到底',
             '不再跟随 → 日志越写越长但视口停在顶部，用户看到的是「卡住不动的一段旧文字」。'),
        ]

        for name, scope, old, repl, expect, why in muts:
            src = open(BAK, encoding='utf-8').read()
            if scope == 'cond':
                new = src.replace(COND, COND.replace(old, repl), 1)
            else:
                if src.count(old) != 1:
                    print(f'FAIL [{name}] 锚点不唯一（命中 {src.count(old)} 处）—— '
                          f'注错地方等于没测，请把锚点写细。')
                    rc_bad = 1
                    continue
                new = src.replace(old, repl, 1)
            if new == src:
                print(f'FAIL [{name}] 注入无效：锚点没命中，等于没测。')
                rc_bad = 1
                continue
            open(JS, 'w', encoding='utf-8').write(new)

            rc2, out2 = run_test()
            fl = fail_lines(out2)
            ok_hit = bool(fl)
            ok_pick = any(expect in ln for ln in fl)
            print(f'--- 注入：{name}（{why}）')
            if not ok_hit:
                print(f'FAIL [{name}] 注入后没有 FAIL 行（rc={rc2}）—— '
                      f'红在崩溃上不算红，这条断言抓不住它。')
                rc_bad = 1
            elif not ok_pick:
                print(f'FAIL [{name}] 红了但不是预期那条：期望含「{expect}」，实际 {fl}')
                rc_bad = 1
            else:
                print(f'ok   [{name}] 精确转红：{expect}')
                # 还原后必须回绿
                open(JS, 'w', encoding='utf-8').write(src)
                rc3, out3 = run_test()
                if rc3 != 0 or fail_lines(out3):
                    print(f'FAIL [{name}] 还原后没回绿，注入有残留。')
                    rc_bad = 1
                else:
                    print(f'ok   [{name}] 还原后回绿')
            open(JS, 'w', encoding='utf-8').write(src)
    finally:
        shutil.copy2(BAK, JS)
        os.remove(BAK)

    print('--- ' + ('全部自证通过：材料断言抓得住这 6 类故障' if not rc_bad else '自证失败'))
    return rc_bad


if __name__ == '__main__':
    sys.exit(main())
