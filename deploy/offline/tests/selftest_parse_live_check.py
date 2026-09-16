#!/usr/bin/env python3
"""真跑 `skillforge -selftest`，验证「文档解析服务」这一项的判定在真实 HTTP 下的五种结论。

为什么不能只靠单测：
  `judgeParseService` 的单测喂的是**合成证据**，它证明的是判定逻辑本身没错。但从
  「客户敲下 -selftest」到「屏幕上出现结论」之间还有一整套真实代码：读进程/实例 env →
  URLFromEnv 归一化 → Loopback 判定 → 找 systemd 单元文件 → 真发一次 HTTP 探活 →
  解析 /health 的 JSON → 拼明细 → 汇总计数与退出码。这条链上断任何一环，单测都还是全绿，
  而客户看到的是「自检说解析服务没问题，可上传 PDF 就是抽不出字」。

  线上真发生过这种事：端口在听、HTTP 200、进程也没崩，只是运行时坏了。旧判定只看
  「端口能不能连上」，于是自检报绿、客户满世界找自己的素材问题。

判据（每组场景都要同时满足，缺一不可）：
  1. `[3/4] 文档解析服务 …… <状态>` 必须是**预期那一个**（OK / 跳过 / 失败）；
  2. 明细里必须出现客户据此能自查的关键字（修复命令 / 影响说明）；
  3. 不该出现的话术不能出现（比如把 `--no-ocr` 的正常形态说成「损坏」）；
  4. 其余三项检查的状态必须与基线**逐字相同** —— 证明红的只有该红的那一条。
     「一片红」和「全绿」一样没有信息量：改坏 A 却让 B 跳红线，等于这条断言没盯住它。

用法：
  python3 deploy/offline/tests/selftest_parse_live_check.py
  python3 deploy/offline/tests/selftest_parse_live_check.py --mutation-selfcheck

--mutation-selfcheck 会往**出货源码**里注入真故障、重建、要求本脚本转红，再还原回绿。
没这一步，本脚本自己就可能是「永远绿」的安慰剂（一条锚点写歪、判断恒真，谁也看不出来）。
"""
import os
import re
import shutil
import socket
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
FAKE = ROOT / "deploy/offline/tests/fake_http.py"

# 这里必须用**没有真服务**的高位端口：若拿 8093 去测，本机正好装着解析服务时就变成
# 「拿别人的答案来核对」，测试红绿取决于环境，是最难查的一类 flaky。
P_OK = 19093      # 健康的 ocrd 形态
P_ZOMBIE = 19094  # 端口在听、HTTP 200、自报运行时已损坏（线上事故形态）
P_IMPOSTOR = 19095  # 端口被别人占了，回的 JSON 里连 ok 字段都没有
P_DEAD = 19099    # 本机、没人听

# 三个必须带的关键字：OK 场景要看到版本号，坏场景要看到「怎么修」。
SCENARIOS = [
    {
        "name": "S1 解析服务真的能干活",
        "env": {"SKILLFORGE_OCR_URL": "http://127.0.0.1:%d" % P_OK},
        "want_status": "OK",
        "want_hits": ["正常", "ocrd-v5-runtime-guard"],
        "want_avoid": ["失败", "损坏", "systemctl restart"],
    },
    {
        "name": "S2 僵尸服务：端口在听但运行时坏了",
        "env": {"SKILLFORGE_OCR_URL": "http://127.0.0.1:%d" % P_ZOMBIE},
        "want_status": "失败",
        # 线上事故的形态。必须点明「端口在听 ≠ 能干活」，并给出 restart。
        "want_hits": ["端口在听", "systemctl restart", "journalctl"],
        "want_avoid": ["正常"],
    },
    {
        "name": "S3 冒名服务：200 但不是 ocrd",
        "env": {"SKILLFORGE_OCR_URL": "http://127.0.0.1:%d" % P_IMPOSTOR},
        "want_status": "失败",
        # 回 200 但没有 ok 字段 → 运行时未知 → 不可用。绝不能判绿：那是「别人占着端口
        # 也能骗过自检」，客户会拿着一个从没干活的服务去训练技能。
        "want_hits": ["端口在听"],
        "want_avoid": ["正常"],
    },
    {
        "name": "S4 显式禁用（--no-ocr 的正常形态）",
        "env": {"SKILLFORGE_OCR_URL": "off"},
        "want_status": "跳过",
        "want_hits": ["显式禁用", "--no-ocr", "扫描件 PDF"],
        # 跳过不是事故：出现「损坏」这种话术等于把预期行为吓成故障。
        "want_avoid": ["损坏"],
    },
    {
        "name": "S5 本机没装这个单元（老版 --no-ocr 没写 off）",
        "env": {
            "SKILLFORGE_OCR_URL": "http://127.0.0.1:%d" % P_DEAD,
            # 逼一个不存在的单元名，把「本机没有解析服务」这条路走实。
            "SKILLFORGE_SERVICE_NAME": "skillforge-selftest-noexist",
        },
        "want_status": "跳过",
        "want_hits": ["没有解析服务单元", "不算失败", "systemctl status"],
        "want_avoid": ["损坏"],
    },
    {
        "name": "S6 指向远端却连不上",
        "env": {"SKILLFORGE_OCR_URL": "http://192.0.2.1:8093"},  # TEST-NET-1，必不通
        "want_status": "失败",
        "want_hits": ["远端解析服务", "curl", "原始错误"],
        "want_avoid": ["正常"],
    },
    {
        "name": "S7 实例 env 文件里的地址被采纳",
        # 运维手敲 -selftest 时环境里什么都没有，配置在实例的 env 文件里。
        # 不读它 → 换了端口的实例明明好的却报红（最费客户时间的一种红）。
        "env": {},
        "env_file": "SKILLFORGE_OCR_URL=http://127.0.0.1:%d\n"
                    "# 下面是噪声：非白名单键、注释、空行都不该被搬进环境\n"
                    "PATH=/usr/bin:/bin\n"
                    "\n"
                    "SKILLFORGE_NOT_A_KEY=1\n" % P_OK,
        "want_status": "OK",
        # 锚住**端口**：只断言「正常」会被本机 8093 上的生产 ocrd 满足 —— 那样扫描件
        # 场景就是空跑绿。地址必须真的是 19093（假服务）才算这条过了。
        "want_hits": ["正常", "ocrd-v5-runtime-guard", "127.0.0.1:%d" % P_OK],
        "want_avoid": ["失败"],
    },
]

# 出货源码里的真故障注入（负向自证用）。每条的判据：注入后本脚本必须转红，
# 且红的正是「预期那条场景」——不是随便哪条，也不是靠崩溃退出。
MUTATIONS = [
    (
        "M1 判定退回「端口能连上就算健康」",
        "cmd/server/selftest.go",
        "case ev.Health.Healthy():",
        "case ev.Health.Running: // 注入：只看端口是否在听",
        "S2 僵尸服务：端口在听但运行时坏了",
        "线上事故原样注回：僵尸服务被自检判绿，客户拿着一个每次解析都失败的服务去训练技能，"
        "只看到「生成的技能跟我给的素材没关系」。",
    ),
    (
        "M2 显式禁用被判成失败",
        "cmd/server/selftest.go",
        "case ev.URL == \"\":\n\t\tc.skip = true",
        "case ev.URL == \"\":",
        "S4 显式禁用（--no-ocr 的正常形态）",
        "`--no-ocr` 是合法安装形态，判红等于告诉客户「你装坏了」，"
        "让他去折腾一个本来就不该跑的服务。",
    ),
    (
        "M3 不读实例 env 文件",
        "cmd/server/selftest.go",
        "if src := loadInstanceEnv(); src != \"\" {",
        "if src := \"\"; src != \"\" { // 注入：不读实例配置",
        "S7 实例 env 文件里的地址被采纳",
        "换了 OCR 端口的实例会被自检拿去探默认 8093 → 好的实例报红。"
        "这种红最贵：客户得先自证没装错，才能开始排查。",
    ),
    (
        "M4 原始错误被吞掉",
        "cmd/server/selftest.go",
        "\tif ev.Health.Err != nil && !c.skip {",
        "\tif false && ev.Health.Err != nil && !c.skip { // 注入：吞掉原始错误",
        "S6 指向远端却连不上",
        "客户只看到「连不上」，分不清 DNS 失败 / 连接被拒 / 超时 —— 三种原因三种修法，"
        "只能回头来问我们。",
    ),
]

ENV_RED = ("[build failed]", "cannot find package", "undefined:", "syntax error", "errors parsing go.mod")


def resolve_go():
    """挑一个满足 go.mod 要求的 go（本机 PATH 里可能是过旧的 1.18）。"""
    need = (1, 0)
    m = re.search(r"^go\s+(\d+)\.(\d+)", (ROOT / "go.mod").read_text(encoding="utf-8"), re.M)
    if m:
        need = (int(m.group(1)), int(m.group(2)))
    tried = []
    for c in [os.environ.get("GO_BIN"), shutil.which("go"), "/usr/local/go/bin/go", "/usr/bin/go"]:
        if not c:
            continue
        if os.sep in c and not Path(c).exists():
            tried.append("%s(不可用)" % c)
            continue
        try:
            out = subprocess.run([c, "version"], capture_output=True, text=True, timeout=60).stdout
        except (OSError, subprocess.SubprocessError):
            tried.append("%s(不可用)" % c)
            continue
        mm = re.search(r"\bgo(\d+)\.(\d+)", out)
        if mm and (int(mm.group(1)), int(mm.group(2))) >= need:
            return c, need, tried
        tried.append("%s(%s)" % (c, out.strip() or "版本未知"))
    return None, need, tried


def build(go_bin, out):
    """带版本号构建：不带的话 judgeVersion 永远失败，输出里全是噪声。"""
    ld = "-X github.com/lizhemin15/skillforge/internal/version.Version=v-selftest-check " \
         "-X github.com/lizhemin15/skillforge/internal/version.Commit=deadbee " \
         "-X github.com/lizhemin15/skillforge/internal/version.Date=2026-01-01T00:00:00Z"
    p = subprocess.run([go_bin, "build", "-ldflags", ld, "-o", str(out), "./cmd/server"],
                       cwd=ROOT, capture_output=True, text=True, timeout=900)
    return p.returncode, p.stdout + p.stderr


def port_free(port):
    """端口能不能拿来监听。

    必须带 SO_REUSEADDR：上一次跑完的假服务留下的 TIME_WAIT 套接字会让裸 bind 失败，
    于是「复用刚释放的端口」被误判成「被别人占着」，测试变成跑一次就得等一分钟。
    注意 SO_REUSEADDR 不会让你绑上**正在监听**的端口（那需要 SO_REUSEPORT），
    所以这条检查照样能拦住「拿别人的服务做验收」。
    """
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        s.bind(("127.0.0.1", port))
        return True
    except OSError:
        return False
    finally:
        s.close()


def start_fakes():
    """起假服务；端口被占就明确报错而不是继续（否则测的是别人的服务）。"""
    for p in (P_OK, P_ZOMBIE, P_IMPOSTOR):
        if not port_free(p):
            return None, "端口 %d 已被占用 —— 换端口或先停掉占用进程，不要在别人的服务上做验收" % p
    proc = subprocess.Popen(
        [sys.executable, str(FAKE), "%d=ocr-ok" % P_OK, "%d=ocr-zombie" % P_ZOMBIE, "%d=ocr-impostor" % P_IMPOSTOR],
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    if proc.stdout is None:  # 理论上不会发生（上面就是 PIPE），但别让 None 冒泡成崩溃
        proc.kill()
        return None, "假服务的输出管道没建起来"
    line = proc.stdout.readline().strip()
    if not line.startswith("ready"):
        proc.kill()
        return None, "假服务没起来：%r" % line
    return proc, None


# `[3/4] 文档解析服务 …… 失败`
# 检查项名字里可能有空格（真有一个叫「PDF 中文字体」），所以用 (.+?) 而不是 (\S+)：
# 早先用 \S+ 时那一整行都没被认出来，本脚本直接判自己「格式变了」。
# -selftest 输出的检查项名单（格式自证用），**按输出顺序**。
#
# 早先这里是个数量常量 `SELFTEST_ITEMS = 4`，注释写着「加检查项时同步改这里」。
# 结果 F3 加了「TLS 信任库」、F5 加了「时区 / 时间」之后没人想起它，脚本就烂在这儿了
# —— 更糟的是它当时还没被接进任何闸门，所以是**静默腐烂**：直到 2026-09-16 手工跑
# 才发现它红着，而屏幕上报的是「输出格式变了」（误导：格式没变，是名单变了）。
#
# 现在改成名单：数量从名单推出（不再有两处数要同步），并且报错时点名**新增/消失**的是哪一项，
# 一句话就能分清「-selftest 多了个检查项」和「输出格式真坏了」。
SELFTEST_ITEM_NAMES = (
    "版本注入",
    "时区 / 时间",
    "PDF 中文字体",
    "文档解析服务",
    "TLS 信任库",
    "代码执行沙箱",
)
SELFTEST_ITEMS = len(SELFTEST_ITEM_NAMES)
SECTION = re.compile(r"^\[(\d+)/(\d+)\]\s+(.+?)\s+……\s+(\S+)\s*$")
BLANK = re.compile(r"^\s+(.*)$")
# 汇总行不在输出开头，所以必须带 re.M，否则 ^/$ 只认整份输出（匹配永远是空的，
# 而「匹配不到」在 check_summary 里被当成「产品没打印汇总行」——假红比没断言更费时间）。
SUMMARY = re.compile(r"^自检结果：(.+)$", re.M)


def parse_sections(out):
    """把 -selftest 输出切成 {检查项名: (序号, 总数, 状态, [明细...])}，并逐项校验格式。

    为什么要自己解析而不是 grep：格式一变形（比如把「……」改成「:」），grep 只是找不到
    就判失败，看不出是「格式改了」还是「判定错了」。这里把两者分开报。
    """
    got, cur = {}, None
    for raw in out.splitlines():
        m = SECTION.match(raw)
        if m:
            cur = m.group(3)
            got[cur] = {"idx": int(m.group(1)), "total": int(m.group(2)), "status": m.group(4), "detail": []}
            continue
        m = BLANK.match(raw)
        if m and cur:
            got[cur]["detail"].append(m.group(1).strip())
    return got


def env_for(sc, env_file):
    env = dict(os.environ)
    # 先清掉所有可能影响判定的键：宿主上真装着解析服务时，它的配置会把场景搅浑。
    for k in ("SKILLFORGE_OCR_URL", "SKILLFORGE_SERVICE_NAME", "SKILLFORGE_ENV_FILE"):
        env.pop(k, None)
    env.update(sc["env"])
    if env_file:
        env["SKILLFORGE_ENV_FILE"] = env_file
        env.pop("SKILLFORGE_OCR_URL", None)  # 这条场景要证明「配置从文件里来」
    return env


def run_once(binpath, sc, tmp):
    env_file = None
    if sc.get("env_file"):
        env_file = tmp / "skillforge.env"
        env_file.write_text(sc["env_file"], encoding="utf-8")
    p = subprocess.run([str(binpath), "-selftest"], capture_output=True, text=True,
                       timeout=180, env=env_for(sc, env_file))
    return p.returncode, p.stdout + p.stderr


def check_summary(out, secs, rc):
    """汇总行与退出码必须与各项状态**算得一致**。

    为什么单拎出来查：判定对了、计数错了，客户看到的仍然是错结论。`跳过` 被算进「未通过」
    时，`--no-ocr` 的正常安装会在最后一行写「1/4 项未通过，请按上面提示修复」——
    客户于是去修一个按设计就不该跑的服务。反过来，失败被算进「通过」就更糟：自检成了安慰剂。
    """
    bad = []
    m = SUMMARY.search(out)
    if not m:
        bad.append("输出里没有「自检结果：」汇总行 —— 客户据此判断要不要动手修，不能缺")
        return bad
    line = m.group(1)
    n_ok = sum(1 for s in secs.values() if s["status"] == "OK")
    n_skip = sum(1 for s in secs.values() if s["status"] == "跳过")
    n_fail = sum(1 for s in secs.values() if s["status"] == "失败")
    total = len(secs)

    if n_fail:
        if "%d/%d 项未通过" % (n_fail, total) not in line:
            bad.append("有 %d 项失败，汇总行却是 %r（应当写明 %d/%d 项未通过）" % (n_fail, line, n_fail, total))
        if rc == 0:
            bad.append("有失败项却退出码 0 —— 脚本/CI 会把这份自检当通过，等于没做")
    else:
        if "未通过" in line:
            bad.append("没有任何失败项，汇总行却说「未通过」：%r" % line)
        if n_skip and ("另有 %d 项按本机配置跳过" % n_skip) not in line:
            bad.append("有 %d 项跳过，汇总行该说明「另有 %d 项按本机配置跳过」，实际 %r"
                       "（跳过是配置决定的，不说清客户会当成漏检）" % (n_skip, n_skip, line))
        if rc != 0:
            bad.append("全通过/跳过却退出码 %d —— 会把正常安装判成失败" % rc)
    if total and secs and len({s["total"] for s in secs.values()}) != 1:
        bad.append("各项的分母不一致：%s（格式变了，本脚本的解析需要跟着改）"
                   % {k: v["total"] for k, v in secs.items()})
    return bad


def check_scenario(binpath, sc, tmp, baseline):
    """跑一条场景，返回 (问题列表, 本次各检查项状态)。"""
    rc, out = run_once(binpath, sc, tmp)
    bad = []
    secs = parse_sections(out)

    # 格式自证：解析不出全部检查项就是输出格式变了，此时「判绿」是假的（什么都没看）。
    # 名单对不上时把「新增/消失」挑明 —— 见 SELFTEST_ITEM_NAMES 上面那段教训。
    if set(secs) != set(SELFTEST_ITEM_NAMES):
        added = sorted(set(secs) - set(SELFTEST_ITEM_NAMES))
        gone = sorted(set(SELFTEST_ITEM_NAMES) - set(secs))
        if added or gone:
            bad.append("-selftest 的检查项名单与脚本不同：新增 %s，消失 %s（本脚本期望 %s）。"
                       "若是你真加了/删了检查项，请更新 SELFTEST_ITEM_NAMES 并确认新项在各场景下的"
                       "期望状态；否则是输出格式坏了"
                       % (added or "无", gone or "无", list(SELFTEST_ITEM_NAMES)))
        else:
            bad.append("没解析出 %d 个检查项（拿到 %d 个：%s）—— -selftest 输出格式变了，"
                       "本脚本的断言已失效，别当它是绿"
                       % (SELFTEST_ITEMS, len(secs), sorted(secs)))
        return bad, secs
    # 序号与名字的顺序都要对得上：只查「是 1..N 连续编号」会漏掉「两项序号互换」这类错位。
    want_order = {name: i + 1 for i, name in enumerate(SELFTEST_ITEM_NAMES)}
    got_order = {name: info["idx"] for name, info in secs.items()}
    if got_order != want_order:
        bad.append("检查项序号不是 1..%d 的连续编号：%s（输出格式变了）"
                   % (SELFTEST_ITEMS, sorted(got_order.items())))

    # 配置来源自证：场景自带 env 文件时，输出里的「配置    : <路径>」必须正是那个文件。
    #
    # 为什么必须查：这条场景要证明的是「地址是从实例 env 文件里读出来的」。
    # 只断言「状态 OK + 端口对」还不够 —— 本机 /opt/skillforge/skillforge.env 里
    # 也可能正好是同一个地址（真机上就发生过：生产 ocrd 在 8093 上健康，
    # 于是「不读 env 文件」的注入故障照样判绿）。路径对上，才证明读的是这次的文件。
    if sc.get("env_file"):
        want_src = str(tmp / "skillforge.env")
        if want_src not in out:
            bad.append("输出里没有「配置    : %s」—— 这次判定用的地址不是从本次场景的 env 文件里读的"
                       "（很可能读到了本机别的配置）" % want_src)

    target = secs.get("文档解析服务")
    if not target:
        bad.append("输出里没有「文档解析服务」这一项，实际检查项：%s" % sorted(secs))
        return bad, secs

    if target["status"] != sc["want_status"]:
        bad.append("状态是 %r，期望 %r；明细：%s" % (target["status"], sc["want_status"], target["detail"]))
    joined = "\n".join(target["detail"])
    for w in sc["want_hits"]:
        if w not in joined:
            bad.append("明细里缺少 %r（客户据此没法自查）；实际：%s" % (w, target["detail"]))
    for w in sc["want_avoid"]:
        if w in joined:
            bad.append("明细里不该出现 %r（会把预期行为说成事故）；实际：%s" % (w, target["detail"]))

    bad.extend(check_summary(out, secs, rc))

    # 只该红这一条：其余各项的状态与基线逐字相同。
    for name, info in secs.items():
        if name == "文档解析服务":
            continue
        if baseline and name in baseline and info["status"] != baseline[name]:
            bad.append("附带伤害：%r 的状态从 %r 变成 %r（改坏 A 却让 B 跳红线 = "
                       "这条断言没盯住它该盯的东西）" % (name, baseline[name], info["status"]))
    return bad, secs


def run_all(binpath, tmp, baseline=None):
    """跑全部场景；返回 (失败列表, 基线状态字典)。"""
    fails = []
    if baseline is None:
        baseline = {}
    for sc in SCENARIOS:
        bad, secs = check_scenario(binpath, sc, tmp, baseline)
        if bad:
            print("✗ %s" % sc["name"])
            for b in bad:
                print("    %s" % b)
            fails.append(sc["name"])
        else:
            print("✓ %s（状态 %s）" % (sc["name"], secs["文档解析服务"]["status"]))
    return fails, baseline


def mutation_selfcheck(go_bin, tmp):
    """往出货源码注入真故障 → 必须转红，且红的正是预期场景 → 还原 → 必须回绿。"""
    print("=== 负向自证：注入真故障，要求本脚本转红 ===\n")
    rc_bad, rc_env = [], []

    # 先拿一份干净的基线：包括各检查项的基线状态，用于「只该红一条」的判定。
    binpath = tmp / "sf-clean"
    rc, out = build(go_bin, binpath)
    if rc != 0:
        print("✗ 环境红：干净源码就编不过\n%s" % out[-800:])
        return 2
    base_fails, baseline = run_all(binpath, tmp)
    if base_fails:
        print("✗ 干净源码下本脚本就没过（%s）—— 先修它，注入自证没有意义" % base_fails)
        return 1
    print("\n基线：%d 个场景全绿；其余三项检查状态 = %s\n" % (len(SCENARIOS), baseline))

    for name, rel, old, new, expect_scenario, why in MUTATIONS:
        path = ROOT / rel
        original = path.read_text(encoding="utf-8")
        n = original.count(old)
        if n != 1:
            print("✗ %s: 注入点命中 %d 处（要求唯一）—— selftest.go 实现变了，"
                  "请对照实现更新锚点，别删脚本" % (name, n))
            rc_bad.append(name)
            continue
        print("--- 注入：%s（%s）" % (name, why))
        mbin = tmp / "sf-mut"
        try:
            path.write_text(original.replace(old, new, 1), encoding="utf-8")
            rc, out = build(go_bin, mbin)
            if rc != 0:
                sig = [s for s in ENV_RED if s in out]
                print("✗ %s: 注入后编译不过（%s）—— 「红在崩溃上不算红」，"
                      "这证明不了任何断言有效" % (name, sig or "未知"))
                rc_env.append(name)
                continue
            fails, _ = run_all(mbin, tmp, baseline)
            if expect_scenario not in fails:
                print("✗ %s: 注入了真故障，但预期场景 %r 没转红（红的只有 %s）"
                      % (name, expect_scenario, fails or "无"))
                rc_bad.append(name)
                continue
            print("✓ %s: 精确转红（%s）" % (name, expect_scenario))
        finally:
            path.write_text(original, encoding="utf-8")

        # 还原后必须回绿，且**所有**场景都要过
        rc, out = build(go_bin, binpath)
        if rc != 0:
            print("✗ %s: 还原后编译不过，无法判定是否回绿" % name)
            rc_env.append(name)
            continue
        back, _ = run_all(binpath, tmp, baseline)
        if back:
            print("✗ %s: 还原后仍有场景红（%s）—— 「还原回绿」是假的" % (name, back))
            rc_bad.append(name)
        else:
            print("  ↳ 还原回绿 ✓")
        print()

    print()
    if rc_env:
        print("环境红（不是断言问题）：%s —— 先修工具链，此时不要动断言" % sorted(set(rc_env)))
        return 2
    if rc_bad:
        print("注入自证未通过：%s" % rc_bad)
        return 1
    print("注入自证全部通过（%d/%d：注入→精确红、还原→真绿）" % (len(MUTATIONS), len(MUTATIONS)))
    return 0


def main():
    go_bin, need, tried = resolve_go()
    if go_bin is None:
        # 没有 go 就跑不了真二进制。打印成大写的「跳过」，绝不静默当绿 ——
        # 静默跳过是最坏的一种误导：它让人以为这块有人看着。
        print("跳过：找不到满足 go.mod 要求（≥ %d.%d）的 go，无法构建真二进制。" % need)
        print("  试过：%s" % "、".join(tried))
        print("  CI 里必须跑到本脚本（runner 自带 go），本地可 GO_BIN=... 指定。")
        return 0
    print("go: %s（go.mod 要求 ≥ %d.%d）" % (go_bin, need[0], need[1]))

    fake, err = start_fakes()
    if err:
        print("✗ %s" % err)
        return 1
    try:
        with tempfile.TemporaryDirectory(prefix="sf-selftest-") as td:
            tmp = Path(td)
            if "--mutation-selfcheck" in sys.argv:
                return mutation_selfcheck(go_bin, tmp)
            binpath = tmp / "skillforge"
            rc, out = build(go_bin, binpath)
            if rc != 0:
                print("✗ 构建失败（这不算断言红，是环境问题）：\n%s" % out[-1500:])
                return 2
            print("=== 真跑 -selftest：解析服务判定的五种结论 ===\n")
            # 基线留空：首次运行时其余三项（字体/沙箱）在开发机上本就可能是失败，
            # 拿它当基线会掩盖「注入污染了别的检查项」。基线只在注入自证里用。
            fails, _ = run_all(binpath, tmp)
            print()
            if fails:
                print("✗ 未通过：%s" % fails)
                return 1
            print("✓ 全部通过（%d 个场景）" % len(SCENARIOS))
            return 0
    finally:
        if fake:
            fake.kill()


if __name__ == "__main__":
    sys.exit(main())
