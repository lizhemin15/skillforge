#!/usr/bin/env python3
# 「解释器探测」这套断言的自证脚本。
#
# 背景：用户报「skillforge 安装时提示 目标机没有 python3 沙箱探针执行失败」。
# 修完之后有两类**看着都对**的改法会让它悄悄退化：
#   ① 候选名单两边（exec.go / install.sh）只改一边 → 安装说没问题、装完自检红
#   ② 名单留着不用，探测仍写死单一路径 → 上面那条比对全绿，误报照旧
#   ③ 只看文件存在、不真跑解释器 → 「文件在但跑不起来」被判成可用
# 所以每条断言都要能被注入打红，否则等于没写。
#
# 用法：python3 web/tests/python_resolve_mutation_check.py

import subprocess
import sys
from pathlib import Path

WEB = Path(__file__).resolve().parent.parent
REPO = WEB.parent
EXEC_GO = REPO / "internal" / "tools" / "exec.go"
INSTALL_SH = REPO / "deploy" / "offline" / "install.sh"
GO_TEST = ["go", "test", "./internal/tools/", "-count=1",
           "-run", "ResolvePython|PythonCandidates|PythonLabel", "-v"]

failures = 0


def read(p):
    return p.read_text(encoding="utf-8")


def write(p, s):
    p.write_text(s, encoding="utf-8")


BACKUPS = {}
for _p in (EXEC_GO, INSTALL_SH):
    BACKUPS[_p] = read(_p)


def restore_all():
    for path, content in BACKUPS.items():
        write(path, content)


def run(cmd):
    p = subprocess.run(cmd, cwd=REPO, capture_output=True, text=True)
    return p.returncode, p.stdout + p.stderr


def inject(path, old, new):
    """锚点命中次数必须正好 1：出货文件改了就该当场报错，
    而不是让自证脚本变成永远绿的摆设。"""
    s = read(path)
    n = s.count(old)
    if n != 1:
        return f"锚点在 {path.name} 里命中 {n} 次（应为 1 次）—— 出货文件改了，请同步本脚本的锚点"
    write(path, s.replace(old, new, 1))
    return None


def case(desc, path, old, new, expect):
    """注入一个真故障，要求 Go 测试变红，且红的必须是预期那条。"""
    global failures
    err = inject(path, old, new)
    if err:
        print(f"✗ [{desc}] 注入失败：{err}")
        failures += 1
        restore_all()
        return
    try:
        rc, out = run(GO_TEST)
        if rc == 0:
            print(f"✗ [{desc}] 注入后测试仍然全绿 —— 断言是假的（抓不住这个故障）")
            failures += 1
        elif expect not in out:
            print(f"✗ [{desc}] 测试红了，但红的不是预期那条（期望输出含「{expect}」）")
            for line in out.splitlines():
                if "FAIL" in line or line.startswith("---"):
                    print(f"      {line}")
            failures += 1
        elif "build failed" in out or "cannot find" in out or "syntax error" in out:
            # 编译不过的红不算红：它证明不了断言有效，只证明改崩了。
            print(f"✗ [{desc}] 红在编译/语法上，不算断言抓住故障")
            failures += 1
        else:
            print(f"✓ [{desc}] → 「{expect}」变红")
    finally:
        restore_all()


print("基线：不注入时必须全绿")
rc, out = run(GO_TEST)
if rc != 0:
    print("Go 测试基线就是红的，先修好再来做注入自证：")
    print(out[-2000:])
    sys.exit(1)
print("基线：全绿 ✓\n")

print("注入自证（每条都必须变红）")

# ① 候选名单两边只改一边（Go 侧删一项）
case(
    "候选名单脱钩：exec.go 少了 /usr/local/bin/python3",
    EXEC_GO,
    '\t"/usr/local/bin/python3",\n',
    "",
    "候选名单条数不一致",
)

# ② 候选名单两边只改一边（install.sh 侧删一项）
case(
    "候选名单脱钩：install.sh 少了 /usr/libexec/platform-python",
    INSTALL_SH,
    'PythonCandidates="/usr/bin/python3 /usr/local/bin/python3 /usr/libexec/platform-python"',
    'PythonCandidates="/usr/bin/python3 /usr/local/bin/python3"',
    "候选名单条数不一致",
)

# ③ 名单留着不用，探测退回写死单一路径（= 用户报的原始故障原样复发）
case(
    "探测退回写死单一路径（原始故障复发）",
    INSTALL_SH,
    # 缩进是 2 个 tab：这段候选循环现在包在 `if [ -z "$PY_FOUND" ]` 里
    # （为「包内自带解释器优先」让路）。锚点必须跟着出货文件走 —— 命中 0 次时本脚本
    # 会明确报「锚点失配」而不是静默跳过，这是故意的：静默跳过 = 这条自证失效了。
    '\t\tfor _cand in $PythonCandidates; do\n'
    '\t\t\tif [ -x "$_cand" ] && "$_cand" -c pass >/dev/null 2>&1; then\n'
    "\t\t\t\tPY_FOUND=\"$_cand\"\n"
    "\t\t\t\tbreak\n"
    "\t\t\tfi\n"
    "\t\tdone\n",
    '\t\tif [ ! -x /usr/bin/python3 ]; then\n'
    '\t\t\tc_warn "写死单路径探测（注入的自证场景）"\n'
    '\t\tfi\n',
    "没有用 $PythonCandidates 做循环探测",
)

# ④ 只看文件存在、不真跑解释器探活（「文件在但跑不起来」被判成可用）
case(
    "候选探测去掉 -c pass 探活",
    INSTALL_SH,
    'if [ -x "$_cand" ] && "$_cand" -c pass >/dev/null 2>&1; then',
    'if [ -x "$_cand" ]; then',
    "候选循环没有实际执行解释器探活",
)

# ⑤ override 不可用时悄悄回退到别的解释器（运维以为在用自己那份）
case(
    "SKILLFORGE_PYTHON 不可用时回退到候选",
    EXEC_GO,
    "\t\tabs, ok := pythonUsablePath(override)\n"
    "\t\tif !ok {\n"
    "\t\t\t// 显式指定却不可用 → 不猜别的，交给自检报错。\n"
    '\t\t\treturn ""\n'
    "\t\t}\n"
    "\t\treturn abs\n"
    "\t}\n"
    "\tfor _, c := range candidates {\n",
    "\t\tif abs, ok := pythonUsablePath(override); ok {\n"
    "\t\t\treturn abs\n"
    "\t\t}\n"
    "\t}\n"
    "\tfor _, c := range candidates {\n",
    "应当返回空（暴露问题）",
)

# ⑥ 相对路径不绝对化（systemd 起的沙箱按自己的 cwd 找解释器）
case(
    "override 相对路径不解析成绝对路径",
    EXEC_GO,
    "\tabs := p\n"
    "\tif !filepath.IsAbs(abs) {\n"
    "\t\tlp, err := exec.LookPath(abs)\n"
    "\t\tif err != nil {\n"
    '\t\t\treturn "", false\n'
    "\t\t}\n"
    "\t\tabs = lp\n"
    "\t} else if _, err := exec.LookPath(abs); err != nil {\n"
    '\t\treturn "", false\n'
    "\t}\n",
    "\tabs := p\n"
    "\tif _, err := exec.LookPath(abs); err != nil {\n"
    '\t\treturn "", false\n'
    "\t}\n",
    "应经 $PATH 解析成绝对路径",
)

# ⑦ 只看存在、不探活：不可执行的残件会被选中，装完自检才红
case(
    "候选只查存在不探活（选中不可执行的残件）",
    EXEC_GO,
    "\t} else if _, err := exec.LookPath(abs); err != nil {\n"
    '\t\treturn "", false\n'
    "\t}\n"
    "\tctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)\n"
    "\tdefer cancel()\n"
    '\tif exec.CommandContext(ctx, abs, "-c", "pass").Run() != nil {\n'
    '\t\treturn "", false\n'
    "\t}\n"
    "\treturn abs, true\n",
    "\t} else if _, err := os.Stat(abs); err != nil {\n"
    '\t\treturn "", false\n'
    "\t}\n"
    "\treturn abs, true\n",
    "第 1 项不可执行时应继续用第 2 项",
)

# ⑧ 「找过 $PATH」的兜底被删掉 —— 文案还在说找过 $PATH，实际没找。
#    现场：python3 只在 /opt/xxx/bin（自己编译/conda），用户查 PATH 明明有，
#    还是被告知「没有 python3」，怎么修都修不好。
case(
    "$PATH 兜底被删（文案说找过 $PATH、实际没找）",
    INSTALL_SH,
    '\t\t_via_path="$(command -v python3 2>/dev/null || true)"\n',
    '\t\t_via_path=""\n',
    "没有 $PATH 兜底",
)

# ⑨ 兜底只看 command -v 命中、不真跑解释器
case(
    "$PATH 兜底不探活（命中就跑不起来的也算可用）",
    INSTALL_SH,
    '\t\tif [ -n "$_via_path" ] && "$_via_path" -c pass >/dev/null 2>&1; then\n',
    '\t\tif [ -n "$_via_path" ]; then\n',
    "$PATH 兜底没有探活",
)

# ⑩ 兜底不拒绝相对路径 —— systemd 起的服务按 / 找解释器，随机失败
case(
    "$PATH 兜底接受相对路径",
    INSTALL_SH,
    '\t\tcase "$_via_path" in\n'
    "\t\t\t/*) ;;\n"
    "\t\t\t# 相对路径没有意义：命中它的是安装时的 cwd，而服务由 systemd 起，\n"
    "\t\t\t# cwd 是 /。写进配置只会变成「看着对、行为随机」。\n"
    '\t\t\t*) _via_path="" ;;\n'
    "\t\tesac\n",
    "",
    "没有拒绝相对路径",
)

# ⑪ 门控消失：候选名单里的标准位置也被钉死（系统升级换路径时僵住）
case(
    "写入解释器不受 PY_FROM_PATH 门控",
    INSTALL_SH,
    '\telif [ "$PY_FROM_PATH" = "1" ]; then\n',
    '\telif [ "$PY_FROM_PATH" != "不可能相等" ]; then\n',
    "没有受 PY_FROM_PATH 门控的分支",
)

# ⑫ 门控在、写入没了 —— 安装期认了、服务期照样找不到（假断言就是这样被抓住的）
case(
    "PY_FROM_PATH 分支里不真写 SKILLFORGE_PYTHON",
    INSTALL_SH,
    "\t\tprintf 'SKILLFORGE_PYTHON=%s\\n\\n' \"$PY_FOUND\"\n",
    "\t\tprintf '# 注入：此处不写解释器\\n'\n",
    "没有真正把命中的解释器写进配置文件",
)

# ⑬ 升级安装覆盖客户手写的配置
case(
    "生成配置不沿用上一版的 SKILLFORGE_PYTHON",
    INSTALL_SH,
    '\tif [ -n "$OLD_PY" ]; then\n',
    "\tif false; then\n",
    "没有沿用上一版配置里已有的 SKILLFORGE_PYTHON",
)

print()
rc, out = run(GO_TEST)
if rc != 0:
    print("还原后测试没有回绿 —— 自证脚本自己把工作区改坏了：")
    print(out[-1500:])
    sys.exit(1)
print("还原后：全绿 ✓")

if failures:
    print(f"\n{failures} 条注入没被抓住 —— 对应断言是假的，先修断言")
    sys.exit(1)
print("\n全部注入都被对应的断言抓住 ✓")
