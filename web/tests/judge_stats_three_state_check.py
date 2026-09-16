#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""judge_stage_streaming_stats.py 的三态判定尺子（合成料，秒级，不需要真跑训练）。

为什么需要它：A1~A6 的判定逻辑此前只在 judge_stage_streaming_e2e.py 里被执行，
而那个 e2e 要真跑一轮 20 分钟的训练、且只在 acceptance 里被调 —— 也就是说
**CI 里没有任何东西碰过这份判定逻辑**。于是下面这个真实发生过的 bug 一路绿灯：

    ok is None（被证明过的 N/A）
      → 打印循环写成 `"PASS" if ok else "FAIL"` → None 是假值 → 印成 FAIL
      → 并且被计入 bad → 一个「本轮按设计不该跑」的阶段把整条尺子判红（假红）

三态里最危险的其实不是它，是反过来的那半：把 N/A 印成 PASS —— 那就是**把「没验」
说成「验过了」**，静默、且没人会去查。所以本尺子的判据是「状态 token 必须精确」，
不是「大概意思是那回事」。

四种形态各造一份合成料（都是 20+ 帧、4s 间隔，满足 A1/A3 的前提）：
  S1 阶段在且有材料      → PASS A5 + RC=0
  S2 阶段缺席 + 有理由    → 「N/A 」精确前缀 + RC=0（既不许印 PASS，也不许计入 bad）
  S3 阶段缺席 + 没理由    → FAIL A5（理由必须是「不适用理由」那一支）+ RC=1
  S4 S1 的 --assert 退出码 → 0（参照物：确认 RC 是真的有判别力，不是恒 0/恒 1）

S2/S3 是**同一条分支的两侧**，一起验才排得掉「恒 N/A」和「恒 FAIL」两种假绿。

用法：python3 web/tests/judge_stats_three_state_check.py
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
STATS = ROOT / "web/tests/judge_stage_streaming_stats.py"

MARK = "5/9 未按手册处理"      # 判定器里登记的唯一可接受理由（COND_STAGES）
STAGE_NAMED = "8.5/9 裁判独立试用评分"
STAGE_OTHER = "3/9 检测素材（文本层直取 + 扫描页 OCR）"
PARSE_STEP = "解析：文本层直取 6/OCR 2（扫描页才走 OCR）"

results: list[tuple[str, bool, str]] = []


def check(name: str, ok: bool, detail: str = "") -> None:
    results.append((name, ok, detail))


def build(case_dir: Path, *, has_named_stage: bool, has_reason: bool) -> tuple[Path, Path]:
    """造一份最小可用的 SSE 料：20+ 帧、4s 间隔、材料 ≥400 字、解析帧合规。"""
    case_dir.mkdir(parents=True, exist_ok=True)
    evs: list[dict] = [{"type": "step", "data": STAGE_OTHER}]

    def think(stage: str, text: str) -> None:
        evs.append({"type": "delta",
                    "data": json.dumps({"kind": "think", "stage": stage, "text": text},
                                       ensure_ascii=False)})

    # 12 帧（不是 9）：A1 的门槛是「≥20 帧」，缺席 8.5/9 的那两份料会少 3 帧，
    # 9 帧时总数只有 19 —— 尺子自己的前提没给全，会造出假红（踩过一次）。
    for i in range(12):
        think(STAGE_OTHER, "第%d页：文本层直取，未走 OCR。" % (i + 1))
    # 一段长材料，确保 A4（材料累计 ≥400 字）成立
    think(STAGE_OTHER, "素材要点：" + "可选中文本层直取，不需要 OCR。" * 25)
    evs.append({"type": "step", "data": MARK if has_reason else "5/9 手册处理中"})
    for i in range(5):
        think(MARK if has_reason else "5/9 手册处理中", "手册分节解析第 %d 段。" % (i + 1))
    if has_named_stage:
        evs.append({"type": "step", "data": STAGE_NAMED})
        think(STAGE_NAMED, "试用写稿：本段是裁判独立试用评分阶段的流式材料。" * 12)
        think(STAGE_NAMED, "逐维打分：结构 8 / 语言 9 / 事实 7。")
    evs.append({"type": "step", "data": PARSE_STEP})
    evs.append({"type": "done", "data": "ok"})

    raw = case_dir / "raw.sse"
    raw.write_text("".join("data: %s\n\n" % json.dumps(e, ensure_ascii=False) for e in evs),
                   encoding="utf-8")
    frames = case_dir / "frames.log"
    frames.write_text("".join("+  %.1fs [frame/%s] %s\n" % (i * 4.0, STAGE_OTHER, "x")
                              for i in range(len(evs))), encoding="utf-8")
    return raw, frames


def run(raw: Path, frames: Path) -> tuple[int, str]:
    env = dict(os.environ, RAW_LOG=str(raw), FRAMES_LOG=str(frames))
    p = subprocess.run([sys.executable, str(STATS), "--assert"],
                       capture_output=True, text=True, env=env)
    return p.returncode, p.stdout + p.stderr


def a5_line(out: str) -> str:
    for ln in out.splitlines():
        if "A5" in ln and ln.strip():
            return ln.strip()
    return ""


def main() -> int:
    tmp = Path(tempfile.mkdtemp(prefix="judge_three_state_"))
    rc_s1, out_s1 = run(*build(tmp / "s1", has_named_stage=True, has_reason=False))
    rc_s2, out_s2 = run(*build(tmp / "s2", has_named_stage=False, has_reason=True))
    rc_s3, out_s3 = run(*build(tmp / "s3", has_named_stage=False, has_reason=False))

    l1, l2, l3 = a5_line(out_s1), a5_line(out_s2), a5_line(out_s3)

    check("S1 阶段在且有材料 → PASS A5", l1.startswith("PASS A5") and rc_s1 == 0,
          "rc=%d 行=%s" % (rc_s1, l1))
    # 这两条合起来才是「三态」：既证明它敢说 N/A，也证明它没把 N/A 说成 PASS。
    check("S2 缺席+有理由 → 印「N/A」而不是 PASS",
          l2.startswith("N/A") and "PASS" not in l2, "行=%s" % l2)
    check("S2 N/A 不计入 bad → RC=0（否则是假红）", rc_s2 == 0, "rc=%d" % rc_s2)
    check("S3 缺席+没理由 → FAIL 且理由是「不适用理由」那一支",
          l3.startswith("FAIL A5") and "不适用理由" in l3 and rc_s3 == 1,
          "rc=%d 行=%s" % (rc_s3, l3))
    # 收紧：S3 的红必须是**只有 A5 一条**。否则「A5 红了」可能只是被别的故障捎带红的
    # （那种红的归因是错的，会把人带去查错地方）。
    n_fail_s3 = sum(1 for ln in out_s3.splitlines() if ln.strip().startswith("FAIL A"))
    check("S3 只有 A5 一条 FAIL（红得精确，不是被捎带）",
          n_fail_s3 == 1, "FAIL 行数=%d" % n_fail_s3)
    # 参照物：S1 与 S2 的 rc 都可能是 0，若判定器恒返 0 则 S3 抓不到 —— S3 的 rc=1 与之互为对照。
    check("S4 前提成立：S1/S2 除 A5 外全绿（证明 S3 的红来自 A5 本身）",
          "FAIL A1" not in out_s1 and "FAIL A1" not in out_s2
          and "FAIL A6" not in out_s1 and "FAIL A6" not in out_s2,
          "s1_has_other_fail=%s" % ("FAIL A" in out_s1.replace("FAIL A5", "")))

    print("===== 三态判定尺子（合成料）=====")
    bad = 0
    for name, ok, detail in results:
        print("  %s %s%s" % ("OK  " if ok else "BAD ", name, ("（%s）" % detail) if detail else ""))
        if not ok:
            bad += 1
    print("----")
    print("S1 原始输出节选：\n%s" % "\n".join(out_s1.splitlines()[-8:]) if bad else "", end="")
    print("\n--- %d/%d ok ---" % (len(results) - bad, len(results)))
    if bad:
        print("RC=1 三态判定回归")
        return 1
    print("RC=0")
    return 0


if __name__ == "__main__":
    sys.exit(main())
