#!/usr/bin/env python3
"""从 install.sh 里抠出**真的**解释器探测段，在遮蔽掉候选路径的 mount namespace 里跑。

为什么必须真跑：`internal/tools/python_resolve_test.go` 只能证明「install.sh 里写着
$PATH 兜底」——它是文本断言，证明不了这段代码真能把 /opt/xxx/bin/python3 认出来。
「文案说找过 $PATH、实际没找」这种故障，文本断言和真跑断言抓的是不同的东西。
这跟前端测试「从出货文件抠出真函数来跑」是同一条原则。

现场构造（不碰真机的 /usr/bin/python3 —— 只在自己的 mount namespace 里遮蔽）：
  mv mount --bind /dev/null <候选路径>  →  该路径变成「存在、但跑不起来」

用法：python3 web/tests/install_python_probe_live_check.py
需要 root + 可用的 mount namespace（unshare -m）。用不了时会明确报 SKIP 并不是「通过」。
"""

import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent.parent
INSTALL_SH = REPO / "deploy" / "offline" / "install.sh"

FAILURES = []
CHECKS = 0

# 候选路径到文本断言（python_resolve_test.go）里那份名单去取，
# 不在这里再抄一遍 —— 抄一遍就是第三个「两份会漂移的名单」。
def candidates_from_installer() -> list[str]:
    for line in INSTALL_SH.read_text(encoding="utf-8").splitlines():
        if line.startswith("PythonCandidates="):
            return line.split('"')[1].split()
    raise SystemExit("install.sh 里找不到 PythonCandidates=\"...\" 这一行")


CANDIDATES = candidates_from_installer()


def probe_script() -> str:
    """抠出从 PythonCandidates= 到下一节（0.5 服务单元占用检查）之前的整段。

    整段抠（不是摘几行）：这段里的 if/else 结构、override 分支、循环、$PATH 兜底、
    以及「没找到时给什么提示」是一体的，摘几行等于在测试里重写一遍实现 ——
    那样测的就不是出货文件了。

    结束锚点用下一节的标题，不是「# 为什么只警告不拦」：后者在警告块**之前**，
    用它当结尾会把警告块切掉，于是「没找到时提示里有没有可执行的修复办法」
    这条断言永远拿不到 stderr，变成假红或假绿。
    """
    sh = INSTALL_SH.read_text(encoding="utf-8")
    start = sh.index("PythonCandidates=")
    start = sh.rindex("\n", 0, start) + 1
    end = sh.index("# ---------- 0.5 服务单元占用检查", start)
    return sh[start:end]


# 探测段会调这些输出函数；它们只是日志，桩掉即可。
HARNESS_PREFIX = """
c_warn(){ printf 'WARN %s\\n' "$*" >&2; }
c_ok(){   printf 'OK %s\\n'   "$*" >&2; }
c_info(){ printf 'INFO %s\\n' "$*" >&2; }
"""

HARNESS_SUFFIX = """
printf 'PY_FOUND=%s\\n' "${PY_FOUND:-}"
printf 'PY_FROM_PATH=%s\\n' "${PY_FROM_PATH:-}"
"""


def fake_python(dirpath: Path, ok: bool) -> Path:
    """假解释器：(a) 正常 —— `-c pass` 退出 0；(b) 残件 —— 存在、可执行、但跑不起来。

    (b) 那个现场对应真故障：文件在、selinux 拦执行、或者 0 字节残缺，
    「只看存在」的探测会把它当可用，装完自检才红。
    """
    p = dirpath / "python3"
    body = "#!/bin/sh\nexit 0\n" if ok else "#!/bin/sh\nexit 127\n"
    p.write_text(body, encoding="utf-8")
    p.chmod(0o755)
    return p


def run_probe(script_path: Path, env_path: str, hide_candidates: bool) -> dict[str, str]:
    """在 mount namespace 里跑探测段，把 PY_FOUND / PY_FROM_PATH 读回来。"""
    hides = "\n".join(
        f'[ -e "{c}" ] && mount --bind /dev/null "{c}"' for c in CANDIDATES
    ) if hide_candidates else ":"
    inner = f"""
set -e
{hides}
export PATH={env_path}
bash {script_path}
"""
    p = subprocess.run(["unshare", "-m", "bash", "-c", inner],
                       capture_output=True, text=True)
    if p.returncode != 0:
        raise RuntimeError(f"探测段跑失败（rc={p.returncode}）：\n{p.stdout}{p.stderr}")
    out = {}
    for line in p.stdout.splitlines():
        if "=" in line:
            k, v = line.split("=", 1)
            out[k.strip()] = v.strip()
    out["_stderr"] = p.stderr
    return out


def check(desc: str, got, want) -> None:
    global CHECKS
    CHECKS += 1
    if got == want:
        print(f"  ok   {desc}")
    else:
        FAILURES.append(desc)
        print(f"  FAIL {desc}\n       期望 {want!r} / 实际 {got!r}")


def main() -> int:
    def skip(why: str) -> int:
        # SKIP ≠ PASS：默认判失败，只有显式 ALLOW_SKIP=1（CI runner 这类构造不出
        # mount namespace 的环境）才放行，且放行时输出里明说「未验证」。
        if os.environ.get("ALLOW_SKIP") == "1":
            print(f"SKIP（未验证，ALLOW_SKIP=1）：{why}")
            return 0
        print(f"SKIP：{why}\nSKIP 不等于通过 —— 请在部署机上以 root 重跑本脚本。"
              f"\n（确实无法构造现场的环境可设 ALLOW_SKIP=1，但那只是「未验证」，不是绿。）")
        return 1

    if shutil.which("unshare") is None:
        return skip("本机没有 unshare，构造不出遮蔽现场")
    # 先用最小的 unshare 试一下（非 root 或 seccomp 拦了 mount namespace 时提前判明）
    probe = subprocess.run(["unshare", "-m", "true"], capture_output=True)
    if probe.returncode != 0:
        return skip(f"本环境用不了 mount namespace（unshare -m true rc={probe.returncode}）")
    if os.geteuid() != 0:
        return skip("需要 root 才能在 mount namespace 里遮蔽候选路径")

    tmp = Path(tempfile.mkdtemp(prefix="pyprobe-"))
    try:
        harness = tmp / "probe.sh"
        harness.write_text(HARNESS_PREFIX + probe_script() + HARNESS_SUFFIX, encoding="utf-8")

        good_dir = tmp / "good"
        bad_dir = tmp / "bad"
        empty_dir = tmp / "empty"
        for d in (good_dir, bad_dir, empty_dir):
            d.mkdir()
        good = fake_python(good_dir, ok=True)
        bad = fake_python(bad_dir, ok=False)

        print(f"候选名单（取自 install.sh）：{CANDIDATES}")
        print(f"只有平台 python3 可用的现场：{empty_dir}（PATH 里没有任何 python3）\n")

        # ① 候选全不可用、但 $PATH 上有一个可用的 python3
        #    → 这是用户报的那类机器（自己编译 / conda 装到 /opt/xxx/bin）。
        #    修之前这里会说「没有 python3」，用户一查 PATH 明明有。
        print("① 候选全不可用 + $PATH 上有可用 python3（本轮修的那个现场）")
        r = run_probe(harness, f"{good_dir}:/usr/bin:/bin", hide_candidates=True)
        check("$PATH 上的解释器被认出来", r.get("PY_FOUND"), str(good))
        check("标记为「来自 $PATH」（要钉进配置文件）", r.get("PY_FROM_PATH"), "1")

        # ② 候选全不可用、$PATH 上也没有 → 必须报空，不能瞎猜一个
        print("\n② 候选全不可用 + $PATH 上也没有")
        r = run_probe(harness, f"{empty_dir}:/usr/bin:/bin", hide_candidates=True)
        check("老老实实报没有（不猜）", r.get("PY_FOUND"), "")
        check("不标记来自 $PATH", r.get("PY_FROM_PATH"), "0")
        # 文案与行为必须一致：它说「找过 X 以及 $PATH」，就必须真找过 ——
        # ① 已经真跑证明 $PATH 真找了；这里再确认文案给出的修复办法是可执行的。
        err = r.get("_stderr", "")
        check("给出可执行的修复路径（提示 SKILLFORGE_PYTHON）", "SKILLFORGE_PYTHON" in err, True)
        check("文案列出的候选名单与实际探测的一致",
              all(c in err for c in CANDIDATES), True)
        check("文案承诺「找过 $PATH」（① 已真跑证明）", "$PATH" in err, True)

        # ③ 对照：候选可用时不走 $PATH 兜底，也不钉进配置文件。
        #    没有这条，「无脑钉住标准位置」这种改动会一路绿 ——
        #    那种钉法会在系统升级换路径时僵住。
        print("\n③ 对照：候选命中（/usr/bin/python3），$PATH 上没有 python3")
        r = run_probe(harness, f"{empty_dir}:/usr/bin:/bin", hide_candidates=False)
        check("用候选里的标准位置", r.get("PY_FOUND"), CANDIDATES[0])
        check("标准位置不标记 from $PATH（不钉死）", r.get("PY_FROM_PATH"), "0")

        # ④ override 优先于 $PATH 兜底
        print("\n④ override（SKILLFORGE_PYTHON）优先于 $PATH 兜底")
        os.environ["SKILLFORGE_PYTHON"] = str(good)
        try:
            r = run_probe(harness, f"{empty_dir}:/usr/bin:/bin", hide_candidates=True)
            check("用 override", r.get("PY_FOUND"), str(good))
            check("override 不算「来自 $PATH」", r.get("PY_FROM_PATH"), "0")
        finally:
            os.environ.pop("SKILLFORGE_PYTHON", None)

        # ⑤ 负向自证：$PATH 兜底必须真探活。
        #    PATH 上那个「python3」存在、可执行、但跑不起来（残件/被拦执行）。
        #    只看存在的话会把它当可用 → 装完自检才红。
        print("\n⑤ 负向自证：$PATH 上是个跑不起来的残件")
        r = run_probe(harness, f"{bad_dir}:/usr/bin:/bin", hide_candidates=True)
        check("跑不起来的残件不许被判成可用", r.get("PY_FOUND"), "")
        check("不标记 from $PATH", r.get("PY_FROM_PATH"), "0")
    finally:
        shutil.rmtree(tmp, ignore_errors=True)

    print()
    if FAILURES:
        print(f"--- {CHECKS - len(FAILURES)}/{CHECKS} ok --- {len(FAILURES)} 条红")
        for f in FAILURES:
            print(f"  ✗ {f}")
        return 1
    print(f"--- {CHECKS}/{CHECKS} ok ---")
    return 0


def mutation_selfcheck() -> int:
    """负向自证：把 install.sh 的 $PATH 兜底去掉，本脚本必须**精确**变红。

    没有这一步，本脚本只是「一堆 ok」——它凭什么值得信？这跟 Go 测试的注入自证
    是同一条原则：断言必须能被真故障打红，且红的必须是预期那条。

    判据四重（缺一不可）：
      ① 注入点在文件里真的存在（replace 命中）
      ② 注入后本脚本非零退出
      ③ 输出里出现的是预期那条 FAIL（不是崩溃、不是别的红）
      ④ 还原后回绿
    """
    orig = INSTALL_SH.read_text(encoding="utf-8")
    needle = '\t\t_via_path="$(command -v python3 2>/dev/null || true)"'
    if needle not in orig:
        print("自证失败：注入点不存在（install.sh 结构改了）——本脚本的自证已失效，先修它")
        return 1
    injected = orig.replace(needle, '\t\t_via_path=""', 1)
    if injected == orig:
        print("自证失败：注入没生效")
        return 1

    expect = "$PATH 上的解释器被认出来"
    print("-- 注入：把 install.sh 的 $PATH 兜底去掉（= 用户报的那个故障形状）")
    try:
        INSTALL_SH.write_text(injected, encoding="utf-8")
        p = subprocess.run([sys.executable, __file__], capture_output=True, text=True)
        if p.returncode == 0:
            print("自证失败：去掉 $PATH 兜底后本脚本仍然全绿 —— 这些 ok 是假的")
            return 1
        if expect not in p.stdout:
            print(f"自证失败：本脚本红了，但红的不是预期那条（期望输出含「{expect}」）")
            for line in p.stdout.splitlines():
                if "FAIL" in line:
                    print("      " + line)
            return 1
        print(f"-- 注入后精确变红（rc={p.returncode}，红的是「{expect}」）✓")
    finally:
        INSTALL_SH.write_text(orig, encoding="utf-8")

    p = subprocess.run([sys.executable, __file__], capture_output=True, text=True)
    if p.returncode != 0:
        print("自证失败：还原后没有回绿 —— 自证把工作区改坏了")
        return 1
    print("-- 还原后回绿 ✓")
    print("\n本脚本的负向自证通过：它抓得住「文案说找过 $PATH、实际没找」这个故障 ✓")
    return 0


if __name__ == "__main__":
    if "--mutation-selfcheck" in sys.argv:
        sys.exit(mutation_selfcheck())
    sys.exit(main())
