"""silent_gaps 的双向自证（纯逻辑，离线毫秒级跑完）。

为什么单写这个：M7「最长静默 ≤8s」这条闸的全部价值，取决于「什么算有东西在动」。
线上真实形态是 —— **材料是截尾的滚动窗口**（M3 要求 ≤220 字，实测顶在 163 字）：
新内容一直在进，但长度一个字不涨。若把「有变化」判成「长度变了」，这种正在滚动的
材料会被当成 40 秒静默（实测误报 39.8s，差点让人去改一个根本没坏的起草跳）。
所以这条尺子必须双向自证：

  ① 滚动窗口（长度恒定、尾部在变）**不得**被判成静默 —— 否则是假红，会去修没坏的东西；
  ② 真的静止（尾部/长度/正文全不动）**必须**被抓出来 —— 否则是假绿，用户的「卡着计时」
     就永远量不出来。

跑法：python3 web/tests/silent_gaps_mutation_check.py
"""
import importlib.util
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))


def _load():
    """按路径加载 chat_material_e2e.py（脚本不是包，不能 import 名字）。"""
    path = os.path.join(HERE, 'chat_material_e2e.py')
    spec = importlib.util.spec_from_file_location('mat_e2e', path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _sample(t, mlen, tail, blen=0, active=True, done=0, label='④ 执行中'):
    """采样行布局必须与 SAMPLER_JS 一致：(t, 材料长度, 材料尾部, 正文长度, 进行中, done材料数, 步骤)。"""
    return [t, mlen, tail, blen, active, done, label]


def case_rolling_at_cap(mod):
    """① 长度顶在 163 不变、尾部每帧都在变 → 最长静默必须很小（不得算成静默）。"""
    samples = [_sample(i * 200, 163, f'thinking… 第{i}片思考内容') for i in range(200)]  # 40 秒
    gaps = mod.silent_gaps(samples)
    biggest = max(g[2] for g in gaps) if gaps else 0
    ok = biggest <= 200
    print(f"{'ok  ' if ok else 'FAIL'} ① 滚动窗口恒长 163 字（尾部在变）40s："
          f"判出的最长静默 {biggest}ms（要求 ≤200ms，即帧间隔本身）")
    return ok


def case_true_freeze(mod):
    """② 40 秒内尾部/长度/正文全不动 → 必须抓出 ~40 秒静默（真静默不能漏）。"""
    samples = [_sample(i * 200, 163, '卡住的同一段材料') for i in range(200)]
    gaps = mod.silent_gaps(samples)
    biggest = max(g[2] for g in gaps) if gaps else 0
    ok = biggest >= 39000
    print(f"{'ok  ' if ok else 'FAIL'} ② 真静止 40s（尾部一模一样）："
          f"判出的最长静默 {biggest}ms（要求 ≥39000ms）")
    return ok


def case_length_only_ruler_is_blind(mod):
    """③ 反证：把「有变化」退化成只看长度（老尺子），形态 ② 会漏成 0 静默。

    这条证明「键里带尾部原文」是**承重**的，不是装饰 —— 拿老尺子量真故障得到假绿。
    """
    samples = [_sample(i * 200, 163, '卡住的同一段材料') for i in range(200)]
    prev_key, prev_t, biggest = None, None, 0
    for s in samples:
        t, mlen, _tail, blen, active, _done, label = (list(s) + ['', ''])[:7]
        key = (label, mlen, blen, active)  # ← 老尺子：不含 tail
        if key != prev_key:
            if prev_t is not None:
                biggest = max(biggest, t - prev_t)
            prev_key, prev_t = key, t
    ok = biggest == 0
    print(f"{'ok  ' if ok else 'FAIL'} ③ 反证：只看长度的老尺子量同一条 40s 真静止，"
          f"判出最长静默 {biggest}ms（要求 0ms = 假绿；故尾部原文必须进键）")
    return ok


def case_gap_is_the_right_one(mod):
    """④ 静默段定位准确：在 10.0s→13.0s 造一段真静止，判出的段必须正好是它。"""
    samples = []
    for i in range(50):                      # 0→10s 一直在动
        samples.append(_sample(i * 200, 163, f'tail-{i}'))
    for i in range(50, 65):                  # 10→13s 静止
        samples.append(_sample(i * 200, 163, 'frozen'))
    for i in range(65, 100):                 # 13→20s 又在动
        samples.append(_sample(i * 200, 163, f'resume-{i}'))
    gaps = mod.silent_gaps(samples)
    biggest = max(gaps, key=lambda g: g[2])
    ok = 2800 <= biggest[2] <= 3200 and 9800 <= biggest[0] <= 10200
    print(f"{'ok  ' if ok else 'FAIL'} ④ 静默段定位：判出最长 {biggest[2]}ms "
          f"@ {biggest[0]}ms→{biggest[1]}ms（要求 ≈3000ms @ 10000ms）")
    return ok


def main():
    mod = _load()
    results = [
        case_rolling_at_cap(mod),
        case_true_freeze(mod),
        case_length_only_ruler_is_blind(mod),
        case_gap_is_the_right_one(mod),
    ]
    print(f'--- {sum(results)}/{len(results)} ok ---')
    return 0 if all(results) else 1


if __name__ == '__main__':
    sys.exit(main())
