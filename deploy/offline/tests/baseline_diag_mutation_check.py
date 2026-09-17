#!/usr/bin/env python3
"""基座体检（-diag）与「环境给不了沙箱」这一栏的**双向真跑**自证。

为什么要这把尺子（2026-09-17 客户离线部署现场）：
  客户机器是 CentOS 7 系（glibc 2.17 / systemd 219），两个报错其实是**同一个真因**：
    · skillforge-ocr 反复启动退出，journal 只有一行
      `Failed to load Python shared library '…/_MEI…/libpython3.11.so.1.0':
       /lib64/libc.so.6: version `GLIBC_2.28' not found`
    · 运行 sf-ocr-doctor.sh 时「代码执行沙箱」这一项红，原始报错只有一句
      `systemd-run: unrecognized option '--pipe'`
  客户从这两句原文里既看不出「该换基座」还是「该改配置」，也没有任何东西告诉他
  「这台机器永远做不到」。修完之后必须钉住下面两件事，否则下一次改回去也没人知道：

  ① `-diag`：老基座上要给出「不可用/不匹配 + 修法」，正常机器上不许乱报问题（**双向**）；
  ② `-selftest`：老基座上「代码执行沙箱」必须报「不可用（环境）」——
     既不能报 OK（骗人），也**不能算进「未通过」**（那会把一台正常装好的机器判成装失败）。

怎么造「老基座」：往 PATH 最前面塞一个假 systemd-run，
  --help 输出**真实的 systemd 219 帮助文本**（internal/tools/testdata/systemd_run_help_219.txt，
  那份是本轮事故现场从 219 容器里原样取回来的），--version 输出 "systemd 219"。
  跑的是同一份出货二进制，只换环境 —— 不需要真去找一台 CentOS 7。

双向自证（缺一不可）：
  A 老基座场景必须命中结论；B 真环境场景必须**不**命中同一条结论。
  只验 A 的话，把判定写成「恒报不可用」也能过；只验 B 的话，把判定写成「恒报可用」也能过。
"""
import os
import re
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
HELP_219 = ROOT / "internal" / "tools" / "testdata" / "systemd_run_help_219.txt"

# 真实事故原文（launch 期加载器的话），做合成 ocrd 用 —— 判据要看的就是它。
REPRO_GLIBC = ("[PYI-16:ERROR] Failed to load Python shared library "
               "'/opt/skillforge/run/ocr-tmp/_MEI00247ee9m89rGw/libpython3.11.so.1.0': "
               "/lib64/libc.so.6: version `GLIBC_2.28' not found "
               "(required by /opt/skillforge/run/ocr-tmp/_MEI00247ee9m89rGw/libpython3.11.so.1.0)")

SECTION = re.compile(r"^\[(\d+)/(\d+)\]\s+(.+?)\s+……\s+(\S+)\s*$")
SUMMARY = re.compile(r"^自检结果：(.+)$", re.M)


def resolve_go():
    """找一个能满足 go.mod 要求的 go（与 selftest_parse_live_check.py 同一策略）。"""
    need = (1, 21)
    m = re.search(r"^go\s+(\d+)\.(\d+)", (ROOT / "go.mod").read_text(encoding="utf-8"), re.M)
    if m:
        need = (int(m.group(1)), int(m.group(2)))
    cands = [os.environ.get("GO_BIN"), "/usr/local/go/bin/go", "/usr/lib/go/bin/go", "go"]
    tried = []
    for c in cands:
        if not c:
            continue
        exe = c if os.path.sep in c else None
        try:
            out = subprocess.run([c, "version"], capture_output=True, text=True, timeout=60).stdout
        except Exception as e:  # noqa: BLE001
            tried.append("%s(%s)" % (c, e))
            continue
        mm = re.search(r"go(\d+)\.(\d+)", out)
        if mm and (int(mm.group(1)), int(mm.group(2))) >= need:
            return (exe or c), need, tried
        tried.append("%s(%s)" % (c, out.strip() or "版本未知"))
    return None, need, tried


def build(go_bin, out):
    ld = ("-X github.com/lizhemin15/skillforge/internal/version.Version=v-baseline-diag "
          "-X github.com/lizhemin15/skillforge/internal/version.Commit=deadbee "
          "-X github.com/lizhemin15/skillforge/internal/version.Date=2026-01-01T00:00:00Z")
    p = subprocess.run([go_bin, "build", "-ldflags", ld, "-o", str(out), "./cmd/server"],
                       cwd=ROOT, capture_output=True, text=True, timeout=900)
    return p.returncode, p.stdout + p.stderr


def make_fake_systemd(tmp: Path) -> Path:
    """假 systemd-run：--help 用真实 219 帮助文本，--version 报 219。"""
    d = tmp / "fake-bin"
    d.mkdir(exist_ok=True)
    help_text = HELP_219.read_text(encoding="utf-8").split("=====VERSION=====")[0]
    script = ("#!/bin/sh\n"
              "case \"$1\" in\n"
              "  --help) cat <<'SFHELP'\n" + help_text + "SFHELP\n    exit 0 ;;\n"
              "  --version) echo 'systemd 219'; exit 0 ;;\n"
              "esac\n"
              "exit 1\n")
    p = d / "systemd-run"
    p.write_text(script, encoding="utf-8")
    p.chmod(0o755)
    return d


def env_with(prefix: Path | None, extra: dict | None = None) -> dict:
    env = dict(os.environ)
    if prefix is not None:
        env["PATH"] = str(prefix) + os.pathsep + env.get("PATH", "")
    if extra:
        env.update(extra)
    return env


def run_cmd(cmd, env, timeout=300):
    p = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout, env=env)
    return p.returncode, p.stdout + p.stderr


def parse_items(out):
    items = {}
    for line in out.splitlines():
        m = SECTION.match(line)
        if m:
            items[m.group(3)] = m.group(4)
    return items


def check_diag(binpath: Path, tmp: Path, fake: Path):
    """-diag 的四条场景：老基座 / 真环境 / 基线比本机高 / 产物归因。"""
    bad = []

    # A) 老基座：沙箱必须被判不可用，并给出结论与修法；这一步没有「要动手修的问题」。
    rc_a, out_a = run_cmd([str(binpath), "-diag"], env_with(fake, {"SKILLFORGE_OCR_URL": "off"}))
    for want in ["不可用（环境不支持）", "systemd 219", "--pipe", "PrivateNetwork", "systemd ≥232"]:
        if want not in out_a:
            bad.append("A 老基座场景缺少 %r（客户据此不知道该换基座）；输出：\n%s" % (want, out_a))
    if rc_a != 0:
        bad.append("A 老基座场景退出码 %d —— 「本机环境给不了」不该算「有问题要处理」" % rc_a)

    # B) 真环境：同一条结论**不许**出现（否则尺子恒红，等于没装）。
    rc_b, out_b = run_cmd([str(binpath), "-diag"], env_with(None, {"SKILLFORGE_OCR_URL": "off"}))
    if "不可用（环境不支持）" in out_b:
        bad.append("B 真环境场景竟然也报「不可用」—— 判定没有跟着环境走；输出：\n%s" % out_b)
    if rc_b != 0:
        bad.append("B 真环境场景退出码 %d（本机没有需要处理的问题时才该是 0）；输出：\n%s" % (rc_b, out_b))

    # C) 包内 ocrd 声明的基线比本机 glibc 高：必须判「有问题要处理」并给出换包/换基座两条路。
    # 注：-diag 是按「与主程序同前缀」找 bin/ocrd 的，所以要在 tmp/pkg 下放一份主程序再跑。
    fakebin = tmp / "pkg" / "bin"
    fakebin.mkdir(parents=True, exist_ok=True)
    rc_c, out_c = run_isolated_pkg(binpath, tmp, fakebin, "9.99")
    for want in ["低于", "换一个 ocrd 构建基线", "--no-ocr"]:
        if want not in out_c:
            bad.append("C 基线比本机高时缺少 %r；输出：\n%s" % (want, out_c))
    if rc_c != 1:
        bad.append("C 基线比本机高时退出码 %d（应为 1：这是要动手处理的）" % rc_c)

    # D) 基线匹配但产物加载失败（真实的加载器原文）：必须归因到 glibc 并把原文带出来。
    rc_d, out_d = run_isolated_pkg(binpath, tmp, fakebin, "2.17",
                                   body="cat <<'EOF'\n" + REPRO_GLIBC + "\nEOF\nexit 1")
    for want in ["启动自检: 失败", "glibc ≥2.28", "与文件在不在无关"]:
        if want not in out_d:
            bad.append("D 产物加载失败时缺少 %r；输出：\n%s" % (want, out_d))
    if rc_d != 1:
        bad.append("D 产物加载失败时退出码 %d（应为 1）" % rc_d)

    return bad


def run_isolated_pkg(binpath: Path, tmp: Path, fakebin: Path, baseline: str, body: str | None = None):
    """在 tmp/pkg 前缀下跑 -diag：-diag 按「与主程序同前缀」找 bin/ocrd，所以先把二进制拷过去。"""
    pkg = fakebin.parent
    exe = pkg / "skillforge"
    subprocess.run(["cp", str(binpath), str(exe)], check=True)
    (fakebin / "ocrd.baseline").write_text("glibc=%s\npython=3.11.14\n" % baseline, encoding="utf-8")
    (fakebin / "ocrd").write_text("#!/bin/sh\n" + (body or "exit 0") + "\n", encoding="utf-8")
    (fakebin / "ocrd").chmod(0o755)
    return run_cmd([str(exe), "-diag"], env_with(None, {"SKILLFORGE_OCR_URL": "off"}))


def check_selftest_state(binpath: Path, tmp: Path, fake: Path):
    """-selftest：老基座上沙箱栏必须「不可用（环境）」，且不被算进「未通过」。"""
    bad = []
    rc, out = run_cmd([str(binpath), "-selftest"], env_with(fake, {"SKILLFORGE_OCR_URL": "off"}))
    items = parse_items(out)
    if "代码执行沙箱" not in items:
        return ["-selftest 输出里没有「代码执行沙箱」这一项；输出：\n%s" % out]
    got = items["代码执行沙箱"]
    if got != "不可用（环境）":
        bad.append("老基座上沙箱栏是 %r，应为「不可用（环境）」（报 OK = 骗人；报失败 = 把正常机器判成装失败）" % got)
    m = SUMMARY.search(out)
    if not m:
        return bad + ["-selftest 没有汇总行"]
    line = m.group(1)
    # 不变量：汇总里的「N/M 项未通过」必须等于**真失败项数**，不可用项不许被算进去。
    n_fail = sum(1 for v in items.values() if v == "失败")
    mm = re.search(r"(\d+)/(\d+) 项未通过", line)
    if mm:
        if int(mm.group(1)) != n_fail:
            bad.append("汇总说 %s 项未通过，实际失败项 %d 个 —— 说明有项被算错了（多半是把"
                       "「不可用（环境）」当成了失败）" % (mm.group(1), n_fail))
    elif n_fail:
        bad.append("有 %d 个失败项，汇总行却没写「N/M 项未通过」：%r" % (n_fail, line))
    if rc != (1 if n_fail else 0):
        bad.append("退出码 %d 与失败项数 %d 不匹配（全通过时必须是 0）" % (rc, n_fail))

    # 反向：真环境下沙箱栏不许是「不可用（环境）」。
    rc2, out2 = run_cmd([str(binpath), "-selftest"], env_with(None, {"SKILLFORGE_OCR_URL": "off"}))
    items2 = parse_items(out2)
    if items2.get("代码执行沙箱") == "不可用（环境）":
        bad.append("真环境下沙箱栏也报「不可用（环境）」—— 判定没跟着环境走（尺子恒红）")
    _ = rc2
    return bad


def main():
    go_bin, need, tried = resolve_go()
    if go_bin is None:
        print("跳过：找不到满足 go.mod 要求（≥ %d.%d）的 go，无法构建真二进制。" % need)
        print("  试过：%s" % "、".join(tried))
        print("  CI 里必须跑到本脚本（runner 自带 go），本地可 GO_BIN=... 指定。")
        return 0
    if not HELP_219.exists():
        print("✗ 缺少测试料 %s（systemd 219 的真实帮助文本）—— 没有它就没法造老基座" % HELP_219)
        return 2

    with tempfile.TemporaryDirectory(prefix="sf-baseline-diag-") as td:
        tmp = Path(td)
        binpath = tmp / "skillforge"
        rc, out = build(go_bin, binpath)
        if rc != 0:
            print("✗ 构建失败（这不算断言红，是环境问题）：\n%s" % out[-1500:])
            return 2
        fake = make_fake_systemd(tmp)
        print("=== 真跑 -diag / -selftest：老基座 vs 真环境（双向）===\n")
        bad = check_diag(binpath, tmp, fake)
        bad += check_selftest_state(binpath, tmp, fake)
        if bad:
            for b in bad:
                print("✗ %s" % b)
            return 1
        print("✓ 全部通过：老基座给出结论与修法、正常机器不乱报、沙箱栏三态算账正确")
        return 0


if __name__ == "__main__":
    sys.exit(main())
