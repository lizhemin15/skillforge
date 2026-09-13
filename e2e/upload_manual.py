#!/usr/bin/env python3
"""端到端上传测试：登录 → 上传手册 PDF 建技能 → 流式落 SSE 日志。

用 Python 而不是 curl 的原因：shell 命令里的 $(cat token) 会被输出脱敏机制
替换成 ***，导致请求头损坏（实测报「请求体无效」）。在脚本里读文件不经过
命令文本替换。
"""
import json
import os
import sys
import urllib.request
import uuid

# 目标实例地址必须由调用方注入。
# 踩过的坑：这里原本写死 "http://127.0.0.1:8099"，而调用方（E2E 脚本）为了避开
# 残留实例会把端口随机分配 —— 于是脚本自己的实例好端端起着，上传却打到了
# **8099 上那个残留实例**，产物落进残留实例的数据目录，断言去读本次目录只读到
# 两个内置技能，报「0 分类 0 范文」。断言当时是准的，是这个门牌号发错了。
BASE = os.environ.get("SKILLFORGE_BASE_URL", "http://127.0.0.1:8099").rstrip("/")
NAME = sys.argv[1] if len(sys.argv) > 1 else "手册测试稿"
PDF = sys.argv[2] if len(sys.argv) > 2 else "/root/skillforge/testdata/manual/manual-vector.pdf"
OUT = sys.argv[3] if len(sys.argv) > 3 else "/tmp/sse-vector.log"

# 凭据从环境变量读：隔离实例是全新 DB，首次启动时用 SKILLFORGE_ADMIN_PASS
# 建管理员。硬编码密码会 401（实测踩过），且也避免密码进脚本进版本库。
# 不回显任何值。
USER = os.environ.get("SKILLFORGE_ADMIN_USER", "admin")
PASS = os.environ.get("SKILLFORGE_ADMIN_PASS", "")


def login():
    body = json.dumps({"username": USER, "password": PASS}).encode()
    req = urllib.request.Request(BASE + "/api/login", data=body, method="POST")
    req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req, timeout=30) as r:
        tok = json.load(r).get("token", "")
    if not tok:
        raise SystemExit("登录失败：拿不到 token")
    return tok


def build_body(pdf_bytes, fname):
    bnd = "----sf" + uuid.uuid4().hex
    parts = []

    def field(k, v):
        parts.append(
            f'--{bnd}\r\nContent-Disposition: form-data; name="{k}"\r\n\r\n{v}\r\n'.encode()
        )

    field("name", NAME)
    field("category", "写作")
    field("description", "按《企业新闻稿写作手册》覆盖全部类别的写作技能")
    field(
        "requirement",
        "依据上传的写作手册，覆盖手册中全部类别；每类给出可逐条核对的写作要求，并引用手册原文作为范文。",
    )
    parts.append(
        f'--{bnd}\r\nContent-Disposition: form-data; name="files"; filename="{fname}"\r\n'
        f"Content-Type: application/pdf\r\n\r\n".encode()
    )
    parts.append(pdf_bytes)
    parts.append(f"\r\n--{bnd}--\r\n".encode())
    return b"".join(parts), bnd


def main():
    tok = login()
    pdf = open(PDF, "rb").read()
    print(f"[upload] 文件 {PDF} {len(pdf)} 字节", flush=True)
    body, bnd = build_body(pdf, PDF.rsplit("/", 1)[-1])
    # 路径必须是 /api/admin/train：/api/admin/skills 是 CreateSkill（读 JSON），
    # 拿 multipart 打过去只会得到「请求体无效」——这个错字面看着像 body 坏了，
    # 实际是走错门了。
    req = urllib.request.Request(BASE + "/api/admin/train", data=body, method="POST")
    req.add_header("Authorization", "Bearer " + tok)
    req.add_header("Content-Type", "multipart/form-data; boundary=" + bnd)

    with urllib.request.urlopen(req, timeout=1800) as resp, open(OUT, "wb") as f:
        while True:
            chunk = resp.read(256)
            if not chunk:
                break
            f.write(chunk)
            f.flush()
    # 流结束 ≠ 训练成功。SSE 把失败表达成 error 帧而不是 HTTP 状态码，所以退出码
    # 必须由「日志里有没有 error 帧」决定；否则脚本恒退 0，把失败伪装成通过
    # （实测踩过：上传其实挂在 step2，脚本却报退出码 0，全靠外层断言兜住）。
    txt = open(OUT, encoding="utf-8", errors="replace").read()
    errs = [l for l in txt.splitlines() if '"type":"error"' in l]
    if errs:
        print("[upload] 训练报错：" + errs[0][:400], flush=True)
        raise SystemExit(3)
    print("[upload] 流结束，日志：" + OUT, flush=True)


main()
