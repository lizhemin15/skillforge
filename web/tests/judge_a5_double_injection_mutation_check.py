#!/usr/bin/env python3
"""对 A5 三态尺子做**双向**负向自证：松了要红、紧了也要红。

用法: python3 web/tests/judge_a5_double_injection_mutation_check.py

注入 1（松 → 该绿的变红）：删掉第二条登记理由 → S5(query 缺席) 必须 BAD，RC=1
注入 2（过宽 → 该红的变绿）：把第二条理由的反向否决摘掉 → S6(write 缺席) 必须 BAD，RC=1
每次注入后都校验「确实改到了文件」（md5 变了），还原后校验 md5 回到原值、并重跑回绿。
没有这两下，「COND_REASONS 加一条」这件事既可能是没生效，也可能是放宽过头。
"""
import hashlib
import re
import shutil
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
STATS = ROOT / "web" / "tests" / "judge_stage_streaming_stats.py"
HARNESS = ROOT / "web" / "tests" / "judge_stats_three_state_check.py"


def md5(p: Path) -> str:
    return hashlib.md5(p.read_bytes()).hexdigest()


def run_harness() -> tuple[int, str]:
    r = subprocess.run([sys.executable, str(HARNESS)], capture_output=True, text=True)
    return r.returncode, r.stdout + r.stderr


def syntax_ok(p: Path) -> tuple[bool, str]:
    """注入后的文件必须还是合法 Python。

    2026-09-17 踩到的坑：注入 2 用 `orig.replace(r'...\\b"', 'None   # 注释')` 摘否决，
    尾随逗号被卷进注释 → `SyntaxError: invalid syntax`。于是 harness 里那份「S6 转 BAD」
    是**崩溃红**冒充的：rc=1 是真的，但根本不是「假绿现场形状」（明细里 `行=` 为空）。
    判据「rc==1 + BAD 行存在」照样全绿 —— 只有把语法这一关单列出来才看得见。
    所以：先 py_compile，语法不过就直接判 BAD，不进后续断言。
    """
    import py_compile
    try:
        py_compile.compile(str(p), doraise=True, cfile=str(p) + ".pyc")
    except py_compile.PyCompileError as e:
        return False, str(e).strip().splitlines()[0][:120]
    finally:
        for junk in (Path(str(p) + ".pyc"), Path(str(p.parent / "__pycache__"))):
            if junk.exists():
                shutil.rmtree(junk, ignore_errors=True) if junk.is_dir() else junk.unlink()
    return True, ""


def main() -> int:
    orig_md5 = md5(STATS)
    orig_text = STATS.read_text()

    results: list[tuple[str, bool, str]] = []

    def check(name: str, ok: bool, detail: str = "") -> None:
        results.append((name, ok, detail))

    # ---------- 0. 先给「语法闸门」本身验牙 ----------
    # 「闸门恒返回 True」是一种最安静的假绿：注入真的把文件弄成语法错，闸门却说没事。
    # 造一份故意写坏的文件，闸门必须判 False。
    import tempfile as _tf
    with _tf.TemporaryDirectory(prefix="a5_syntax_probe_") as _d:
        bad = Path(_d) / "bad.py"
        bad.write_text("x = (1, 2,   # 注释吃掉逗号\n     3\n", encoding="utf-8")
        gate_ok, _e = syntax_ok(bad)
    check("闸门验牙：故意写坏的文件必须被判 False（否则语法闸门是恒真假绿）", not gate_ok)

    # ---------- 注入 1：删掉第二条理由（模拟「没加这条」）----------
    # 用纯字符串手术（不用正则）：正则里 \\s、[A-Za-z_]+ 这些元字符跟源码字面量
    # 的转义层数太容易搞错 —— 第一次就是没匹配上，注入静默失效，白跑一轮。
    lines = orig_text.splitlines(keepends=True)
    keep, dropped = [], []
    for i, ln in enumerate(lines):
        if "非写作类技能，手册识别这一步按设计整段不跑" in ln:
            dropped.append(ln)
            if keep and '技能类型:' in keep[-1]:
                dropped.append(keep.pop())      # 同一条理由的第一行
            continue
        keep.append(ln)
    check("注入1 定位到 ② 的那两行（找不到就没法注入）",
          len(dropped) == 2, "dropped=%d 行" % len(dropped))
    injected = "".join(keep)
    check("注入1 真的改到了文件（②被删掉，md5 必须变）", injected != orig_text)
    STATS.write_text(injected)
    check("注入1 落地校验：文件 md5 已变", md5(STATS) != orig_md5)
    s1_ok, s1_err = syntax_ok(STATS)
    check("注入1 注入后语法有效（SyntaxError 的 rc=1 是崩溃红，不算数）", s1_ok, s1_err)
    rc, out = run_harness()
    # 「红」的判据必须是**预期那条**：S5 转 BAD，而不是崩溃/语法错。
    s5_bad = any(ln.strip().startswith("BAD") and "S5 type=query" in ln
                 for ln in out.splitlines())
    check("注入1 → RC=1（脚本非零退出，CI 里才算挂住尺子）", rc == 1, "rc=%d" % rc)
    check("注入1 → S5 精确转 BAD（该绿的变红 = 尺子松了）", s5_bad)
    check("注入1 不是「崩溃红」：harness 仍跑出总结行", "ok ---" in out)
    STATS.write_text(orig_text)
    check("注入1 还原：md5 回到原值", md5(STATS) == orig_md5)

    # ---------- 注入 2：摘掉反向否决（模拟「只认有类型行」的放宽）----------
    # ⚠️ 逗号必须一起摘：源码里是 `..., r"3/9 技能类型:\s*write\b",\n    "非写作类..."`。
    # 只替换引号串、把尾随逗号留在原处 → 逗号被卷进注释 → SyntaxError（踩过，见 syntax_ok）。
    VETO = 'r"3/9 技能类型:\\s*write\\b",'
    check("注入2 定位到反向否决字面量（含尾随逗号）", VETO in orig_text)
    over_broad = orig_text.replace(
        VETO, 'None,  # 过宽注入：不再否决 write')
    check("注入2 真的改到了文件（反向否决被摘掉）", over_broad != orig_text)
    STATS.write_text(over_broad)
    check("注入2 落地校验：文件 md5 已变", md5(STATS) != orig_md5)
    s2_ok, s2_err = syntax_ok(STATS)
    check("注入2 注入后语法有效（SyntaxError 的 rc=1 是崩溃红，不算数）", s2_ok, s2_err)
    rc2, out2 = run_harness()
    s6_bad = any(ln.strip().startswith("BAD") and "S6 type=write" in ln
                 for ln in out2.splitlines())
    check("注入2 → RC=1", rc2 == 1, "rc=%d" % rc2)
    check("注入2 → S6 精确转 BAD（write 也拿 N/A = 假绿）", s6_bad)
    check("注入2 不是「崩溃红」：harness 仍跑出总结行", "ok ---" in out2)
    # 假绿的现场形状：S6 那一轮的 A5 行印成了 N/A（不是 FAIL）。这里只看 S6 自己那条
    # BAD 行的明细（harness 全量输出里 S1 的 PASS 行会先出现，全量扫会得出反向结论）。
    s6_line = next((ln.strip() for ln in out2.splitlines()
                    if ln.strip().startswith("BAD") and "S6 type=write" in ln), "")
    check("注入2 → S6 的 A5 行确实被印成 N/A（假绿现场形状）",
          "行=N/A" in s6_line, "S6 明细=%s" % s6_line[:120])
    STATS.write_text(orig_text)
    check("注入2 还原：md5 回到原值", md5(STATS) == orig_md5)

    # ---------- 还原后回绿（注入前的绿不算数，得看还原后的）----------
    rc3, out3 = run_harness()
    check("还原后重跑：RC=0 且 11/11 ok", rc3 == 0 and "11/11 ok" in out3,
          "rc=%d tail=%s" % (rc3, out3.strip().splitlines()[-1] if out3.strip() else ""))

    print("===== A5 尺子双向注入自证 =====")
    bad = 0
    for name, ok, detail in results:
        print("  %s %s%s" % ("OK  " if ok else "BAD ", name, ("（%s）" % detail) if detail else ""))
        if not ok:
            bad += 1
    print("--- %d/%d ok ---" % (len(results) - bad, len(results)))
    if bad:
        print("RC=1 A5 双向注入自证失败")
        return 1
    print("RC=0")
    return 0


if __name__ == "__main__":
    sys.exit(main())
