#!/usr/bin/env python3
"""分析训练页时间线：首帧材料延迟、帧密度、最长静默空档。

输入 /tmp/train_lines.json（由 web/tests/admin_train_progress_e2e.py 的 LINES_OUT 落盘）。
用户体感指标就是这个：屏幕上「最长多久没有任何新东西」。
"""
import json
import sys

path = sys.argv[1] if len(sys.argv) > 1 else '/tmp/train_lines.json'
d = json.load(open(path))
lines = d.get('lines', [])
ticks = d.get('ticks', [])
win = d.get('sample_seconds', 0)
stop_reason = d.get('stop_reason', 'timeout')
ended_at = d.get('ended_at')
# 窗口右端怎么取：跑完了就用「后端说训练完成的那一刻」，还在跑才用采样窗口。
# 训练结束后的那截空窗不是「屏幕卡住」，算进去会把体感指标污染成噪声
# （2026-09-17 那次 213.7s 跑完、窗口 660s，照旧算法能算出 446s「静默」）。
right = ended_at if (stop_reason == 'terminal' and ended_at) else win
tail_note = (f'训练 {ended_at:.1f}s 结束（后端终态信号），尾部 '
             f'{win - ended_at:.1f}s 不计入体感指标') if stop_reason == 'terminal' \
    else f'采样窗口内未收到训练终态信号，{win:.0f}s 时仍在跑（尾部空档按「还在跑」计）'

print(f"采样窗口 {win}s（收工 {stop_reason}@{ended_at}s） / 日志行 {len(lines)} / 计时快照 {len(ticks)}")
print(f"  {tail_note}")

mat = [(t, r) for t, r in lines if '思考：' in r or '正文：' in r or '提示：' in r]
prog = [(t, r) for t, r in lines if '思考：' not in r and '正文：' not in r and '提示：' not in r]

if mat:
    print(f"\n中间材料帧 {len(mat)} 次，首帧 {mat[0][0]:.1f}s，末帧 {mat[-1][0]:.1f}s")
    print("  抽样（首/中/末）：")
    for t, r in (mat[0], mat[len(mat) // 2], mat[-1]):
        print(f"   {t:6.1f}s  {r[:100]}")
else:
    print("\n❌ 窗口内零中间材料帧")

if prog:
    print(f"\n阶段进度行 {len(prog)} 条：")
    for t, r in prog[:12]:
        print(f"   {t:6.1f}s  {r[:100]}")

# 最长静默空档：屏幕上「没有任何新行 + 计时器文字没变」的最长时段
marks = sorted([t for t, _ in lines] + [t for t, _ in ticks])
if marks and right:
    spans = []
    prev = 0.0
    for t in marks:
        spans.append((t - prev, prev, t))
        prev = t
    spans.append((right - prev, prev, right))
    gap, a, b = max(spans)
    print(f"\n最长静默空档 {gap:.1f}s（{a:.1f}s → {b:.1f}s）")
    top = sorted(spans, reverse=True)[:5]
    print("  前五段空档：" + '，'.join(f'{g:.1f}s(@{s0:.0f})' for g, s0, _ in top))

# 材料帧到达间隔
if len(mat) > 1:
    its = [mat[i + 1][0] - mat[i][0] for i in range(len(mat) - 1)]
    its_s = sorted(its)
    p50 = its_s[len(its_s) // 2]
    print(f"\n材料帧间隔：p50 {p50:.1f}s / max {max(its):.1f}s / 总帧 {len(its) + 1}")
    big = [(i, round(v, 1)) for i, v in enumerate(its) if v > 20]
    if big:
        print(f"  >20s 的空档 {len(big)} 处：{big[:8]}")
