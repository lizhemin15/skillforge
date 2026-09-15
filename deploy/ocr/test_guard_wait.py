#!/usr/bin/env python3
"""运行时守卫「退出前等在飞请求走完」的确定性单测（从出货文件抠真函数跑，不在测试里复写实现）。

为什么有这个文件：verify_runtime_loss.sh 的 S4 在真二进制上抓到了缺陷 ——
守卫回完 200 之后固定 sleep 0.4s 就 os._exit(75)，而调用方（Go 侧 http.Post）此刻还在传
PDF body，于是对端拿到的是**连接被重置**（curl rc=55），而不是我们写的那句人话。
修法：先把 body 排空、把响应 flush 出去，再等「在飞请求计数」归零才退出。

真二进制上的 S4 只能覆盖「单条请求 + 本机快网」这一种时序；「另一条请求还在传」这种并发
时序靠造故障很难稳定复现（要卡准窗口）。所以这里用计数语义把行为直接钉死：
   C1 有在飞请求 → 退出必须发生在该请求释放**之后**（旧实现必挂）
   C2 无在飞请求 → 立刻退（别让自愈被无谓拖延）
   C3 在飞请求永不释放 → 到点仍然退（自愈不能被一个卡死的调用方永久堵住）

用法：
  python3 deploy/ocr/test_guard_wait.py           # 正绿
  python3 deploy/ocr/test_guard_wait.py --break   # 变异自证：换回旧的「固定 sleep 就退」，
                                                  # 必须出现预期的 FAIL 行。
                                                  # 自证成功也 exit 0；没抓到红(假绿) exit 1。
"""
import importlib.util
import os
import sys
import threading
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
SRC = HERE / "ocrd.py"

RUNTIME_LOST_MSG = "runtime_dir_lost"


class _Dummy:
    """顶替 pymupdf / rapidocr_onnxruntime，只为让模块 import 通过（本测试不用它们）。"""

    def __getattr__(self, k):
        return _Dummy()

    def __call__(self, *a, **k):
        return _Dummy()


def load_ocrd():
    """从出货文件加载 ocrd 模块。打桩可选依赖 → 任何机器（含 CI runner）都能跑。"""
    for name in ("pymupdf", "rapidocr_onnxruntime"):
        sys.modules.setdefault(name, _Dummy())
    spec = importlib.util.spec_from_file_location("ocrd_under_test", SRC)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


class ExitSpy:
    """替换 ocrd.os：拦住 os._exit（真调用会杀掉测试进程本身），其余属性透传给真的 os。"""

    def __init__(self):
        self.exited = threading.Event()
        self.code = None

    def _exit(self, code):
        self.code = code
        self.exited.set()
        raise SystemExit(code)  # 只在 _bye 线程里抛；线程内 SystemExit 等于安静结束该线程

    def __getattr__(self, k):
        return getattr(os, k)


def old_exit_for_restart(mod, delay=0.4, timeout=0.0):
    """复刻修复前的实现（固定 sleep 就退）——只用于 --break 自证，不是出货代码。"""
    def _bye():
        time.sleep(delay)
        mod.os._exit(75)

    threading.Thread(target=_bye, daemon=False).start()


def probe_exit_waits(mod, hold, timeout, delay=0.05):
    """核心探针：让 inflight=1 保持 hold 秒，看退出是否等到它释放。

    返回 (exit_after_release, elapsed, exit_code, release_seen)
    """
    spy = ExitSpy()
    mod.os = spy
    released = {}

    def holder():
        time.sleep(hold)
        mod.inflight_delta(-1)
        released["t"] = time.time()

    mod.inflight_delta(1)
    th = threading.Thread(target=holder, daemon=True)
    th.start()
    t0 = time.time()
    mod.exit_for_restart(delay=delay, timeout=timeout)
    got = spy.exited.wait(timeout + 5)
    t_exit = time.time()
    th.join(timeout=5)
    if not got or "t" not in released:
        return (False, t_exit - t0, spy.code, "t" in released)
    return (t_exit >= released["t"], t_exit - t0, spy.code, True)


def probe_no_inflight(mod, timeout=5.0):
    spy = ExitSpy()
    mod.os = spy
    t0 = time.time()
    mod.exit_for_restart(delay=0.05, timeout=timeout)
    got = spy.exited.wait(timeout + 5)
    return (got, time.time() - t0, spy.code)


def probe_stuck_inflight(mod, timeout=0.6):
    """在飞请求永不释放 → 必须在 timeout 附近仍然退出（自愈不能被堵死）。"""
    spy = ExitSpy()
    mod.os = spy
    mod.inflight_delta(1)
    t0 = time.time()
    mod.exit_for_restart(delay=0.05, timeout=timeout)
    got = spy.exited.wait(timeout + 5)
    elapsed = time.time() - t0
    mod.inflight_delta(-1)
    return (got, elapsed, spy.code)


def main():
    brk = "--break" in sys.argv
    mod = load_ocrd()
    print(f"=== 出货文件：{SRC.name} ===")
    print(f"=== 模式：{'变异自证(--break)' if brk else '正跑'} ===")

    if brk:
        mod.exit_for_restart = (
            lambda delay=0.4, timeout=0.0: old_exit_for_restart(mod, delay, timeout))
        print("注入：exit_for_restart 换回「固定 sleep 0.4s 就退」的旧实现")

    fails = []

    # C1：退出必须发生在在飞请求释放之后
    ok, elapsed, code, seen = probe_exit_waits(mod, hold=0.9, timeout=5.0)
    if ok and code == 75 and seen:
        print(f"PASS C1 退出等到在飞请求释放后才发生（elapsed={elapsed:.2f}s, rc={code}）")
    else:
        fails.append("C1")
        print(f"FAIL C1 在飞请求还没走完就退出了：exit_after_release={ok} "
              f"seen_release={seen} elapsed={elapsed:.2f}s rc={code}")
        print("        期望：退出发生在 inflight 归零之后（修复前固定 sleep 0.4s，"
              "调用方还在传 body → 对端看到连接中断 curl rc=55）")

    # C2：没有在飞请求时不该磨蹭
    got, elapsed, code = probe_no_inflight(mod)
    if got and code == 75 and elapsed < 1.5:
        print(f"PASS C2 无在飞请求时立即退出（elapsed={elapsed:.2f}s, rc={code}）")
    else:
        fails.append("C2")
        print(f"FAIL C2 无在飞请求时的退出异常：got={got} elapsed={elapsed:.2f}s rc={code}")

    # C3：卡死的调用方不能永久堵住自愈
    got, elapsed, code = probe_stuck_inflight(mod, timeout=0.6)
    if got and code == 75 and elapsed >= 0.5:
        print(f"PASS C3 在飞请求永不释放时仍到点退出（elapsed={elapsed:.2f}s, rc={code}）")
    else:
        fails.append("C3")
        print(f"FAIL C3 卡死在飞请求时的退出异常：got={got} elapsed={elapsed:.2f}s rc={code}")

    if brk:
        # 变异自证：旧实现必须让 C1 转红，C1 不红就是这把尺子失效（假绿）
        if "C1" in fails:
            print(f"=== 变异自证成功：旧实现精确让 C1 转红（同批红项：{', '.join(fails)}）"
                  f" ===")
            return 0
        print(f"=== 变异自证失败（假绿）：注入旧实现后 C1 竟然仍绿，fails={fails} ===")
        return 1

    if fails:
        print(f"=== 单测未通过（红项：{', '.join(fails)}）===")
        return 1
    print("=== 单测全绿：退出前等在飞请求走完，且不会堵死自愈 ===")
    return 0


if __name__ == "__main__":
    sys.exit(main())
