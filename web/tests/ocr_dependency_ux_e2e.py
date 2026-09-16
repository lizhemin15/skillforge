#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""线上取证：解析服务不可用时，用户到底看到什么（用**出货二进制**真跑，不是桩）。

现场（2026-09-16 投诉）：目标机上训练技能报
    127.0.0.1:8093/extract connection refused
用户判断不出这是环境问题，也不知道下一步敲什么命令 —— 只能来问。

这个脚本用真服务 + 真二进制 + 真 SSE 流复现并验收：
  leg A（依赖不可用）：把实例的 SKILLFORGE_OCR_URL 指到一个**死端口**，
      真发一次训练请求，断言流里出现「没有在运行」+「systemctl restart …」
      +「本次训练已中止」，且**快速失败**（不是等 20 分钟才报）。
  leg B（依赖正常）：同一条二进制、真 ocrd，断言解析这一步照样成功
      （证明改动没把正常路径弄坏），看到「解析完成」即收工，不等整轮训练。

为什么用独立实例 + 数据副本，而不是直接打线上 8092：
  训练会往 SKILLFORGE_DATA_DIR 里写技能目录。取证不能污染生产数据，
  所以把生产 data/ 拷一份到临时目录，实例指向副本；OCR 地址用环境变量覆盖。
  凭据从 /opt/skillforge/skillforge.env 读，**不打印、不落盘**。

用法：
    bash scripts/acceptance-live.sh                 # 跟着线上验收一起跑（推荐）
    ONLY=ocrdep bash scripts/acceptance-live.sh     # 只跑这一条 leg
    python3 web/tests/ocr_dependency_ux_e2e.py      # 直接跑
    BIN=/opt/skillforge/skillforge python3 web/tests/ocr_dependency_ux_e2e.py
退出码：0 = 断言全通过（前置条件不成立时打 SKIP 并退 0 —— 但 runner 默认据此判
      FAIL/未验证，要放行必须显式 ALLOW_SKIP=1）；1 = 有断言失败。

为什么文件名从 `..._live_check.py` 改成 `..._e2e.py`（2026-09-16）：
  它原来是**零引用**的：ci.yml 没有、preflight.sh 没有、docs 没有，而
  `scripts/acceptance-live.sh` 只枚举 `web/tests/*_e2e.py` —— 名字不匹配 =
  这条线上验收永远不跑，失败形态是「一切正常」。这跟 `deploy/offline/tests/`
  那个「同名黑洞」是同一种病：**尺子没接进任何闸门，红着绿着都没人知道**。
  改后缀 + 声明 `# LIVE-LEGS:` 之后，它归 runner 管，`live_e2e_roster.test.mjs`
  守它跟文件真身不许脱钩。
"""
# LIVE-LEGS: ocrdep
# ↑ 线上验收 leg 声明（单条 leg：脚本内部自己跑 leg A 依赖不可用 / leg B 依赖正常）。
#   枚举规则见 scripts/acceptance-live.sh 与 web/tests/live_e2e_roster.test.mjs。
import json

import os
import re
import shutil
import signal
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid

BIN = os.environ.get("BIN", "/opt/skillforge/skillforge")
SRC_DATA = os.environ.get("SRC_DATA", "/opt/skillforge/data")
ENV_FILE = os.environ.get("ENV_FILE", "/opt/skillforge/skillforge.env")
WORKDIR = os.environ.get("WORKDIR", "/tmp/sf-ocrdep-live")
FIXTURE = os.environ.get("FIXTURE", "testdata/ocr/scan3-rtloss.pdf")
ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

ok_n = 0
fail_n = 0
skip_n = 0


def ok(msg):
    global ok_n
    ok_n += 1
    print(f"[ok] {msg}")


def fail(msg):
    global fail_n
    fail_n += 1
    print(f"[FAIL] {msg}")


def skip(msg):
    global skip_n
    skip_n += 1
    # 行首必须是 SKIP：scripts/acceptance-live.sh 用 `grep -qE '^SKIP'` 判「踢过球
    # 但没验证」，默认判 FAIL，只有显式 ALLOW_SKIP=1 才放行。写成 [SKIP] 就漏判，
    # 于是「一条断言都没验」会被当成绿 —— 这正是 SKIP≠PASS 要防的假绿。
    print(f"SKIP {msg}")


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


def read_env(path):
    """读 KEY=VALUE；只取登录凭据，其余一律不进内存。"""
    out = {}
    try:
        with open(path, "r", encoding="utf-8", errors="replace") as f:
            for line in f:
                line = line.strip()
                if not line or line.startswith("#") or "=" not in line:
                    continue
                k, v = line.split("=", 1)
                if k.strip() in ("SKILLFORGE_ADMIN_USER", "SKILLFORGE_ADMIN_PASS"):
                    out[k.strip()] = v.strip().strip('"').strip("'")
    except OSError as e:
        print(f"SKIP 读不到 {path}：{e}")
    return out


class Instance:
    """一个用出货二进制起起来的独立实例（数据副本 + OCR 地址可覆盖）。"""

    def __init__(self, name, ocr_url):
        self.name = name
        self.dir = os.path.join(WORKDIR, name)
        self.port = free_port()
        self.ocr_url = ocr_url
        self.proc = None
        self.log = open(os.path.join(WORKDIR, name + ".log"), "wb")

    def start(self, creds):
        data = os.path.join(self.dir, "data")
        if os.path.isdir(data):
            shutil.rmtree(data)
        os.makedirs(self.dir, exist_ok=True)
        shutil.copytree(SRC_DATA, data, symlinks=True)
        env = dict(os.environ)
        env.update(
            {
                "SKILLFORGE_ADDR": f"127.0.0.1:{self.port}",
                "SKILLFORGE_DATA_DIR": data,
                "SKILLFORGE_DB": os.path.join(data, "skillforge.db"),
                "SKILLFORGE_OCR_URL": self.ocr_url,
                "SKILLFORGE_SERVICE_NAME": "skillforge",
                "SKILLFORGE_ADMIN_USER": creds.get("SKILLFORGE_ADMIN_USER", "admin"),
                "SKILLFORGE_ADMIN_PASS": creds.get("SKILLFORGE_ADMIN_PASS", ""),
            }
        )
        self.proc = subprocess.Popen(
            [BIN], cwd=self.dir, env=env, stdout=self.log, stderr=subprocess.STDOUT,
            start_new_session=True,
        )
        deadline = time.time() + 25
        while time.time() < deadline:
            if self.proc.poll() is not None:
                return False
            try:
                with urllib.request.urlopen(
                    f"http://127.0.0.1:{self.port}/api/version", timeout=2
                ) as r:
                    if r.status == 200:
                        return True
            except Exception:
                time.sleep(0.4)
        return False

    def login(self, creds):
        body = json.dumps(
            {
                "username": creds.get("SKILLFORGE_ADMIN_USER", "admin"),
                "password": creds.get("SKILLFORGE_ADMIN_PASS", ""),
            }
        ).encode()
        req = urllib.request.Request(
            f"http://127.0.0.1:{self.port}/api/login",
            data=body,
            headers={"Content-Type": "application/json"},
        )
        with urllib.request.urlopen(req, timeout=10) as r:
            data = json.loads(r.read().decode())
        for k in ("token", "access_token"):
            if data.get(k):
                return data[k]
        cookie = r.headers.get("Set-Cookie") or ""
        m = re.search(r"(\w+)=([^;]+)", cookie)
        if m:
            return f"COOKIE:{m.group(1)}={m.group(2)}"
        raise RuntimeError("登录响应里没有 token：" + json.dumps(data)[:120])

    def stop(self):
        if self.proc and self.proc.poll() is None:
            try:
                os.killpg(os.getpgid(self.proc.pid), signal.SIGTERM)
                self.proc.wait(timeout=10)
            except Exception:
                try:
                    os.killpg(os.getpgid(self.proc.pid), signal.SIGKILL)
                except Exception:
                    pass
        try:
            self.log.close()
        except Exception:
            pass


def train_stream(inst, token, fixture_bytes, fixture_name, timeout=180, stop_when=None):
    """真发一次训练请求，返回 (状态码/头部行, 全部 SSE 文本, 耗时秒)。"""
    boundary = "----sf" + uuid.uuid4().hex
    parts = []
    for k, v in (
        ("name", "OCR依赖取证" + uuid.uuid4().hex[:6]),
        ("category", ""),
        ("description", "解析服务不可用时的用户可见行为取证"),
        ("requirement", "根据参考素材生成一份写作技能"),
    ):
        parts.append(
            f'--{boundary}\r\nContent-Disposition: form-data; name="{k}"\r\n\r\n{v}\r\n'.encode()
        )
    parts.append(
        f'--{boundary}\r\nContent-Disposition: form-data; name="files"; '
        f'filename="{fixture_name}"\r\nContent-Type: application/pdf\r\n\r\n'.encode()
    )
    parts.append(fixture_bytes)
    parts.append(f"\r\n--{boundary}--\r\n".encode())
    body = b"".join(parts)

    headers = {"Content-Type": f"multipart/form-data; boundary={boundary}"}
    if token.startswith("COOKIE:"):
        headers["Cookie"] = token[len("COOKIE:"):]
    else:
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(
        f"http://127.0.0.1:{inst.port}/api/admin/train", data=body, headers=headers
    )
    t0 = time.time()
    chunks = []
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            status = r.status
            while True:
                try:
                    b = r.read(4096)
                except socket.timeout:
                    # 读超时不是「失败」：训练流在两次进度之间可以静默很久。
                    # 保留已经收到的内容，让调用方按已收到的部分判定。
                    return status, b"".join(chunks).decode("utf-8", "replace"), time.time() - t0
                if not b:
                    break
                chunks.append(b)
                # 看到终点标记就立刻收工：leg B 一旦确认解析成功就该断开，
                # 否则训练会继续跑完整轮（20 分钟 + 真实模型调用），取证不需要付这个代价。
                if stop_when and stop_when in b"".join(chunks).decode("utf-8", "replace"):
                    break
                if time.time() - t0 > timeout:
                    break
            return status, b"".join(chunks).decode("utf-8", "replace"), time.time() - t0
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace"), time.time() - t0
    except Exception as e:  # noqa: BLE001
        prefix = b"".join(chunks).decode("utf-8", "replace")
        return -1, prefix + f"\n__EXC__ {type(e).__name__}: {e}", time.time() - t0


def main():
    print("=== 解析服务依赖的「用户可见行为」线上取证 ===")
    if not os.path.exists(BIN):
        print(f"SKIP 找不到出货二进制 {BIN}")
        return 0
    if not os.path.isdir(SRC_DATA):
        print(f"SKIP 找不到数据目录 {SRC_DATA}")
        return 0
    fixture_path = os.path.join(ROOT, FIXTURE)
    if not os.path.exists(fixture_path):
        print(f"SKIP 找不到夹具 {fixture_path}")
        return 0
    with open(fixture_path, "rb") as f:
        fixture_bytes = f.read()
    fx_name = os.path.basename(fixture_path)
    print(f"  二进制：{BIN}")
    print(f"  夹具  ：{fx_name}（{len(fixture_bytes)} 字节）")
    os.makedirs(WORKDIR, exist_ok=True)
    creds = read_env(ENV_FILE)
    if not creds.get("SKILLFORGE_ADMIN_PASS"):
        print("SKIP 没读到管理端密码（环境文件缺失或权限不足）")
        return 0
    print("  凭据  ：已从环境文件读取（未打印）")

    # ---------- leg A：解析服务不可用 ----------
    dead = free_port()  # 占过再放掉 → 确实没有监听者
    print(f"\n--- leg A：OCR 指向死端口 127.0.0.1:{dead} ---")
    a = Instance("dead", f"http://127.0.0.1:{dead}")
    try:
        if not a.start(creds):
            fail("独立实例起不来（见日志 " + a.log.name + "）")
        else:
            ok("独立实例已起（出货二进制，数据为生产副本，OCR 指向死端口）")
            token = a.login(creds)
            status, text, elapsed = train_stream(a, token, fixture_bytes, fx_name, timeout=180)
            if status == -1 and text.startswith("__EXC__"):
                fail(f"训练请求异常中断：{text[:160]}")
            else:
                ok(f"训练请求已发出并收到响应（HTTP {status}，{elapsed:.1f}s）")
                # 落一份「用户到底看到什么」的原文证据：验收结论会过期，
                # 原文不会 —— 下次有人问「当时用户看到的是哪句话」有据可查。
                ev = os.path.join(WORKDIR, "legA-stream.txt")
                with open(ev, "w", encoding="utf-8") as fh:
                    fh.write(text)
                print(f"  证据：{ev}")
                if "没有在运行" in text:
                    ok("流里明确说了「解析服务没有在运行」（用户能分清：不是他的文件问题）")
                else:
                    fail(f"流里没有「没有在运行」：{text[:400]}")
                if "systemctl restart" in text:
                    ok("流里给出了可执行命令 systemctl restart …（用户能自助）")
                else:
                    fail(f"流里没有修复命令：{text[:400]}")
                if "本次训练已中止" in text:
                    ok("训练已中止（没有静默降级去生成与素材无关的技能）")
                else:
                    fail(f"没有看到中止信号：{text[:400]}")
                if "journalctl" in text or "systemctl status" in text:
                    ok("给了排障入口（status/journalctl）")
                else:
                    fail("没给排障入口")
                if elapsed < 120:
                    ok(f"快速失败：{elapsed:.1f}s 内报出结论（不是等 20 分钟才报）")
                else:
                    fail(f"报错花了 {elapsed:.1f}s，用户会被按在那里等")
                if "dial tcp" in text:
                    idx_raw = text.index("dial tcp")
                    idx_fix = text.find("systemctl restart")
                    if idx_fix >= 0 and idx_fix < idx_raw:
                        ok("底层噪音排在修复指引之后（第一眼看到的是怎么修）")
                    else:
                        fail("底层噪音顶在修复指引之前")
    finally:
        a.stop()

    # ---------- leg B：解析服务正常（确认没把正常路径弄坏） ----------
    print("\n--- leg B：OCR 指向真 ocrd（127.0.0.1:8093）---")
    b = Instance("live", "http://127.0.0.1:8093")
    try:
        if not b.start(creds):
            fail("独立实例起不来（见日志 " + b.log.name + "）")
        else:
            token = b.login(creds)
            status, text, elapsed = train_stream(b, token, fixture_bytes, fx_name, timeout=90, stop_when="解析完成")
            with open(os.path.join(WORKDIR, "legB-stream.txt"), "w", encoding="utf-8") as fh:
                fh.write(text)
            if "解析完成" in text or "解析完成：" in text:
                ok(f"素材照常解析成功（{elapsed:.1f}s 内出现「解析完成」）—— 正常路径未被弄坏")
            elif "未配置 LLM" in text or "未配置" in text:
                skip("该实例没有可用 LLM 配置，解析步之前的门禁先拦住了（leg B 未验证）")
            else:
                fail(f"没看到解析成功：{text[:400]}")
            if "systemctl restart" in text and "解析完成" not in text:
                fail("正常依赖下却给出了环境修复指引（说明判定过宽）")
            else:
                ok("正常依赖下没有误报环境问题")
    finally:
        b.stop()

    # 小结格式必须逐字是 `--- N/M ok ---`：scripts/acceptance-live.sh 用
    # grep -oE '--- [0-9]+/[0-9]+ ok ---' 抠断言数，抠不到就判「绿得没有断言」。
    # skip 单独一行、行首大写 —— 让 runner 的 SKIP 判据能看见它（否则
    # 「leg A 全绿 + leg B 跳过」会被报成 PASS，那是假绿）。
    print(f"\n--- {ok_n}/{ok_n + fail_n} ok ---")
    if skip_n:
        print(f"SKIP {skip_n} 条断言未验证（见上面的 SKIP 行）")
    if fail_n:
        print("结论：FAIL —— 修复没达到「用户能自助」的标准")
        return 1
    print("结论：全部通过")
    return 0


if __name__ == "__main__":
    sys.exit(main())
