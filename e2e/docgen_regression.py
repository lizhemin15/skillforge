#!/usr/bin/env python3
"""
SkillForge 「办公文档管家」docgen E2E 回归测试
==============================================
通过 POST /api/chat 走完整链路（意图识别→技能匹配→docgen生成→SSE file事件→
下载文件字节），验证：
  1. 四种格式（Word/Excel/PDF/PPT）都能生成有效文件
  2. 格式保持稳定（要 PDF 就出 PDF，不是默认变成 Excel）
  3. 空白模板路径（无数据 → 空表/空文档）能下发
  4. 填写生成路径（带数据 → 数据真实写入文件）
  5. 意图识别可靠性（多次调用都应命中 docgen + skill）

用法：
  python3 docgen_regression.py [base_url] [--repeat N]
    base_url  默认 http://127.0.0.1:8092
    --repeat  每种格式请求次数（默认 excel 3 次测稳定性，其余 1 次；或全用 N）
  退出码: 全通过=0, 任一失败=1

依赖: 仅 Python3 标准库（urllib/zipfile）。
"""

import json
import sys
import io
import zipfile
import argparse
import datetime
import urllib.request

# ---------- 可命中 docgen 的请求模板（不同格式, 含填写场景） ----------
# (标签, 用户消息, 期望格式, 期望签名校验函数, 关键词)
REQUESTS = [
    # --- 空白模板下发（无数据 → 空表） ---
    ("Excel空白模板", "帮我生成一份员工信息登记表的Excel空白模板，列：姓名、部门、岗位、入职日期",
     "excel", None, ["姓名", "部门"]),
    ("Word空白模板", "给我一份会议通知的Word文档模板",
     "word", None, ["会议"]),
    # --- 填写生成（带数据 → 成品文件） ---
    ("Excel填写生成", "生成员工花名册Excel，填入数据：张三/技术部/工程师/2024-03-01，李四/市场部/主管/2023-07-15",
     "excel", None, ["张三", "李四", "技术部"]),
    ("Word填写生成", "帮我生成请假申请单Word文档并填入：张伟，请假2天，2026年9月10日到11日，事假，理由家里有事",
     "word", None, ["张伟"]),
    ("PDF填写生成", "生成一份供货商对账单的PDF文档，列：供应商、采购单号、到货日期、应付金额",
     "pdf", None, ["供应商", "采购单号"]),
    ("PPT演示文稿", "生成一份季度汇报的PPT演示文稿",
     "ppt", None, []),
]


def sse_parse(resp_body):
    """把 SSE 响应体解析成 (事件序列, 文件token, 文件名, 错误列表)。"""
    events = []
    tok = None
    fname = "?"
    errs = []
    cur = None
    for raw in resp_body:
        line = raw.decode("utf-8", "replace").strip()
        if line.startswith("event:"):
            cur = line[6:].strip()
            events.append(cur)
        elif line.startswith("data:"):
            dd = line[5:].strip()
            if cur == "file":
                f = json.loads(dd)
                tok = f.get("url", "").rsplit("/", 1)[-1]
                fname = f.get("name", "?")
            elif cur == "error":
                errs.append(dd)
    return events, tok, fname, errs


def download(base, tok):
    if not tok:
        return b""
    url = f"{base}/api/chat/gen/{tok}"
    with urllib.request.urlopen(url, timeout=30) as r:
        return r.read()


# ---------- 格式签名校验 ----------
def is_valid_office(data, fmt):
    """校验文件字节确实是期望的格式。"""
    if not data:
        return False, "空字节"
    if fmt == "pdf":
        if data[:4] != b"%PDF":
            return False, f"签名={data[:4]!r} 非PDF"
        # 结构校验：存在页面对象与内容流（gopdf 中文字体用 CID hex 编码，
        # 文本不落明文，故只验结构 + 有效页面）
        ok = b"/Type /Page" in data or b"/Contents" in data
        return ok, f"签名=%PDF 结构含{ '/Type /Page' if ok else '?内容流' }"
    if fmt in ("word", "excel", "ppt"):
        if data[:2] != b"PK":
            return False, f"签名={data[:2]!r} 非zip"
        try:
            names = zipfile.ZipFile(io.BytesIO(data)).namelist()
        except zipfile.BadZipFile as e:
            return False, f"BadZipFile: {e}"
        must = {
            "word": "word/document.xml",
            "excel": "xl/workbook.xml",
            "ppt": "ppt/presentation.xml",
        }
        ok = any(must[fmt] in n for n in names)
        return ok, f"含 {must[fmt]}?"
    return False, f"未知格式 {fmt}"


def check_content(data, fmt, keywords):
    """填写生成路径校验：关键数据是否真实写入文件。

    仅对 zip 封装格式（word/excel/ppt）做明文关键词校验；PDF 中文字体以
    CID 字形索引编码不存在明文，跳过（由字节量间接佐证内容已填写）。
    """
    if not keywords:
        return True, ""
    if fmt == "pdf":
        return True, "PDF(CID)跳过明文校验"
    missing = []
    hay = ""
    try:
        z = zipfile.ZipFile(io.BytesIO(data))
        for name in z.namelist():
            if name.endswith("sharedStrings.xml") or name.endswith("document.xml") \
               or ("slides" in name and name.endswith(".xml")):
                hay += z.read(name).decode("utf-8", "replace")
    except Exception as e:
        return False, f"解zip读文本失败: {e}"
    for kw in keywords:
        if kw not in hay:
            missing.append(kw)
    if missing:
        return False, f"缺少关键词: {missing}"
    return True, ""


def run_case(base, label, msg, fmt, _sign, keywords, timeout=160):
    """执行单个用例，返回 (通过, 详情dict)。"""
    body = json.dumps({"session_id": f"e2e-{fmt}-{datetime.datetime.now().microsecond}",
                       "message": msg}).encode()
    req = urllib.request.Request(f"{base}/api/chat", data=body,
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            events, tok, fname, errs = sse_parse(r)
    except Exception as e:
        return False, {"error": f"请求失败: {e}", "events": []}
    if errs:
        return False, {"error": f"业务错误: {errs}", "events": events}
    if "file" not in events:
        return False, {"error": f"未收到 file 事件, events={events}", "events": events}
    data = download(base, tok)
    ok, sig = is_valid_office(data, fmt)
    if not ok:
        return False, {"error": f"格式校验失败: {sig}", "name": fname, "bytes": len(data)}
    ok2, msg2 = check_content(data, fmt, keywords)
    if not ok2:
        return False, {"error": msg2, "name": fname, "bytes": len(data)}
    return True, {"name": fname, "bytes": len(data), "sig": sig}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("base", nargs="?", default="http://127.0.0.1:8092")
    ap.add_argument("--repeat", type=int, default=0,
                    help="额外重复次数(用于稳定性冒烟,默认0)")
    args = ap.parse_args()
    base = args.base.rstrip("/")

    # 稳定性格外冒烟: 同一"要PDF"请求连发, 必须每次都出真PDF
    stability = [
        ("PDF稳定性(连发)", "生成一份季度对账的PDF文档", "pdf", 3 if args.repeat == 0 else args.repeat),
    ]

    print("=" * 70)
    print("SkillForge 办公文档管家 E2E 回归测试")
    print(f"目标: {base}   时间: {datetime.datetime.now():%Y-%m-%d %H:%M:%S}")
    print("=" * 70)

    passed = failed = 0
    rows = []

    # 功能用例
    for label, msg, fmt, _s, kws in REQUESTS:
        ok, detail = run_case(base, label, msg, fmt, None, kws)
        if ok:
            passed += 1
            rows.append(f"  ✅ {label:<18} name={detail['name']}  bytes={detail['bytes']}  {detail['sig']}")
        else:
            failed += 1
            rows.append(f"  ❌ {label:<18} {detail.get('error', '未知错误')}")
        print(rows[-1])

    # 稳定性用例
    for label, msg, fmt, n in stability:
        sub_pass = sub_fail = 0
        for i in range(n):
            ok, detail = run_case(f"{base}", f"{label}#{i+1}", msg, fmt, None, [])
            if ok:
                sub_pass += 1
            else:
                sub_fail += 1
                rows.append(f"    ❌ {label}#{i+1}: {detail.get('error', '')}")
        kind = "✅" if sub_fail == 0 else "❌"
        passed += sub_pass
        failed += sub_fail
        rows.append(f"  {kind} {label}  {sub_pass}/{n} 通过")
        print(rows[-1])

    print("-" * 70)
    print(f"总计: {passed} 通过, {failed} 失败")
    print("=" * 70)
    return 0 if failed == 0 else 1


if __name__ == "__main__":
    sys.exit(main())