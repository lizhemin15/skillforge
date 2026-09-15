#!/usr/bin/env python3
# 「停用技能还在管理端列表里」这套断言的自证脚本。
#
# 为什么必须有它：skill_disabled_admin_ui.test.mjs 全绿只证明「现在没坏」，
# 证明不了「坏了会被抓住」。而 Bug N 的修法恰好有一条**看起来也对**的错路：
# 把公开列表里的过滤直接删掉（于是停用技能出现在对话页，用户点了必 403），
# 或者把管理端改回读公开列表（等于没修）。断言写松了这两种都会溜过去。
#
# 做法：往**出货文件**（web/js/admin.js / internal/api/*.go）里逐个注入真实故障，
# 要求对应的那条断言变红；还原后必须重新变绿。哪条注入还是绿的，就说明那条
# 断言是假的，脚本 exit 1。
#
# 三个层次都要自证，缺一层就有一半是裸的：
#   ① 前端契约（node 跑 .test.mjs）—— 管理端到底打哪个接口
#   ② 后端契约（同一个 node 测试读 .go 源码）—— 路由/ListAll 有没有被改回去
#   ③ 后端行为（go test）—— 真的停用一个技能，全量列表还认不认它
#
# 用法：python3 web/tests/skill_disabled_admin_ui_mutation_check.py

import subprocess
import sys
from pathlib import Path

WEB = Path(__file__).resolve().parent.parent
REPO = WEB.parent
ADMIN_JS = WEB / "js" / "admin.js"
SKILLS_GO = REPO / "internal" / "api" / "skills.go"
ROUTER_GO = REPO / "internal" / "api" / "router.go"
NODE_TEST = WEB / "tests" / "skill_disabled_admin_ui.test.mjs"
GO_TEST = "go test ./internal/api/ -run Disabled -count=1"

failures = 0


def run(cmd, cwd=None):
    p = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True)
    return p.returncode, p.stdout + p.stderr


def read(p):
    return p.read_text(encoding="utf-8")


def write(p, s):
    p.write_text(s, encoding="utf-8")


def inject(path, old, new):
    """把 path 里的 old 换成 new。锚点命中次数必须正好 1 —— 出货文件变了就该
    当场报错，而不是让自证脚本变成永远绿的摆设。"""
    s = read(path)
    n = s.count(old)
    if n != 1:
        return f"锚点在 {path.name} 里命中 {n} 次（应为 1 次）—— 出货文件改了，请同步本脚本的锚点"
    write(path, s.replace(old, new, 1))
    return None


def case(desc, path, old, new, expect, cmd=None, cwd=None):
    """注入一个真故障，要求测试变红，且红的必须是预期那条。"""
    global failures
    err = inject(path, old, new)
    if err:
        print(f"✗ [{desc}] 注入失败：{err}")
        failures += 1
        return
    try:
        rc, out = run(cmd or ["node", str(NODE_TEST)], cwd=cwd)
        if rc == 0:
            print(f"✗ [{desc}] 注入后测试仍然全绿 —— 断言是假的（抓不住这个故障）")
            failures += 1
        elif expect not in out:
            print(f"✗ [{desc}] 测试红了，但红的不是预期那条（期望输出含「{expect}」）")
            for line in out.splitlines():
                if "FAIL" in line or line.startswith("---"):
                    print(f"      {line}")
            failures += 1
        elif "panic" in out or "build failed" in out or "cannot find" in out:
            # 编译不过 / panic 的「红」不算红：它证明不了断言有效，只证明改崩了。
            print(f"✗ [{desc}] 红在编译或崩溃上，不算断言抓住故障")
            failures += 1
        else:
            print(f"✓ [{desc}] → 「{expect}」变红")
    finally:
        restore_all()


# ---------- 备份 + 还原（每条注入后都要还原，不然注入状态会串到下一条）----------
BACKUPS = {}


def restore_all():
    for path, content in BACKUPS.items():
        write(path, content)


for _p in (ADMIN_JS, SKILLS_GO, ROUTER_GO):
    BACKUPS[_p] = read(_p)

print("基线：不注入时必须全绿")
rc, out = run(["node", str(NODE_TEST)])
if rc != 0:
    print("前端契约测试基线就是红的，先修好再来做注入自证：")
    print(out[-2000:])
    sys.exit(1)
rc, out = run(GO_TEST.split(), cwd=REPO)
if rc != 0:
    print("Go 行为测试基线就是红的，先修好再来做注入自证：")
    print(out[-2000:])
    sys.exit(1)
print("基线：全绿 ✓\n")

print("注入自证（每条都必须变红）")

# ① 管理端最经典的回退：又去读公开列表（= Bug N 原样复发）
case(
    "管理列表退回公开 /api/skills（Bug N 原样复发）",
    ADMIN_JS,
    "const r = await fetch('/api/admin/skills', { headers: authHdr() });\n      const j = await r.json();\n      const list = (j.skills || []);",
    "const r = await fetch('/api/skills');\n      const j = await r.json();\n      const list = (j.skills || []);",
    "管理列表打 /api/admin/skills",
)

# ② 元信息预填退回公开列表（编辑已停用技能 → 表单空 → 一保存清空元信息）
case(
    "元信息预填退回公开 /api/skills（保存即清空名称）",
    ADMIN_JS,
    "fetch('/api/admin/skills', { headers: authHdr() }).then(r => r.json()).then(j => {\n      const sk = (j.skills || []).find(x => x.slug === curSkillSlug);",
    "fetch('/api/skills').then(r => r.json()).then(j => {\n      const sk = (j.skills || []).find(x => x.slug === curSkillSlug);",
    "预填也打 /api/admin/skills",
)

# ③ 那条「看起来也对」的错路：把公开列表的过滤删掉
#    （停用技能于是出现在对话页勾选层，用户点了必 403）
case(
    "公开列表不再过滤停用技能（停用技能漏进对话页）",
    SKILLS_GO,
    "\t\tif !s.Enabled && !s.IsCore {\n\t\t\tcontinue\n\t\t}\n",
    "",
    "公开 List 仍然跳过停用技能",
)

# ④ 后端把两个口径又合成一个：ListAll 里加过滤
case(
    "ListAll 也过滤停用技能（管理端又只剩一半）",
    SKILLS_GO,
    "\tif skills == nil {\n\t\tskills = []model.Skill{}\n\t}\n",
    "\tif skills == nil {\n\t\tskills = []model.Skill{}\n\t}\n"
    "\tfiltered := skills[:0]\n"
    "\tfor _, s := range skills {\n"
    "\t\tif !s.Enabled {\n"
    "\t\t\tcontinue\n"
    "\t\t}\n"
    "\t\tfiltered = append(filtered, s)\n"
    "\t}\n"
    "\tskills = filtered\n",
    "ListAll 里没有过滤用的 continue",
)

# ⑤ 路由挂错 handler（前端 404 → 列表空 → 比修之前更糟）
case(
    "路由挂回公开 List（前端 404/空列表）",
    ROUTER_GO,
    'mux.HandleFunc("GET /api/admin/skills", h.Auth.Middleware(h.Skills.ListAll))',
    'mux.HandleFunc("GET /api/admin/skills", h.Auth.Middleware(h.Skills.List))',
    "注册了 GET /api/admin/skills",
)

# ⑥ 同一条回归在 **Go 行为层** 也得被抓（证明那不是只靠读源码的文本契约）
case(
    "ListAll 过滤停用技能 → Go 行为测试必须抓到",
    SKILLS_GO,
    "\tif skills == nil {\n\t\tskills = []model.Skill{}\n\t}\n",
    "\tif skills == nil {\n\t\tskills = []model.Skill{}\n\t}\n"
    "\tfiltered := skills[:0]\n"
    "\tfor _, s := range skills {\n"
    "\t\tif !s.Enabled {\n"
    "\t\t\tcontinue\n"
    "\t\t}\n"
    "\t\tfiltered = append(filtered, s)\n"
    "\t}\n"
    "\tskills = filtered\n",
    "停用后",
    cmd=GO_TEST.split(),
    cwd=REPO,
)

print()
rc, out = run(["node", str(NODE_TEST)])
rc2, out2 = run(GO_TEST.split(), cwd=REPO)
if rc != 0 or rc2 != 0:
    print("✗ 还原后测试还是红的 —— 注入没被干净还原，出货文件可能已被改坏")
    print((out + out2)[-2000:])
    failures += 1
else:
    print("还原：全绿 ✓")

print()
if failures:
    print(f"{failures} 条注入没被抓住 —— 断言需要收紧")
else:
    print("全部注入都被抓住，断言可信")
sys.exit(1 if failures else 0)
