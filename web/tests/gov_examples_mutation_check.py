#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""gov 内置技能「数据治理任务开发」示例脚本的真机验收 + 负向自证。

为什么要有这把尺子
------------------
出货文件 `internal/store/prompts/gov_task_dev.md` 是喂给模型的提示词，里面「## 六、完整示例」
的两个脚本（示例 1：公文 Word → 结构化 Excel；示例 2：产品汇总 Word 套模板样式）**是真机跑过的**，
也是模型照抄的模板。它们一旦过期（API 改名、字段改名、模板样式读不出来），模型生成的治理任务就会
静默产出错东西——而平台侧没有任何红灯。所以这里用**真 runner + 假 AI 桩**把两个示例端到端跑一遍，
断言的是「产出物内容」，不是「脚本有没有调用某个 API」。

跑法
----
    python3 web/tests/gov_examples_machine_check.py              # 全绿才算过
    INJECT=1 python3 web/tests/gov_examples_machine_check.py     # 假 AI 返回废话 → 必须精确转红
    INJECT=2 python3 web/tests/gov_examples_machine_check.py     # 素材缺「规格」→ 必须精确转红
    KEEP=1 ...                                                   # 保留中间产物到 /tmp 便于取证

负向自证（INJECT）的判据是「红的正好是预期集合」，不是「有红就行」：
 - INJECT=1 只许 C2/C5/C6 红（AI 没给出 JSON → 0 行数据），C1/C3/C4 必须仍然绿；
 - INJECT=2 只许 C8/D1/D3/D4/D5/D6 红（抽不出产品 → 0 张表），C7/C9/D2 必须仍然绿。
「全红」等于尺子坏掉，不是发现故障——所以要集合相等，且进程必须以非零码退出。

不依赖外部网络：假 AI 桩起在 127.0.0.1 随机端口，通过 task.json 的 api_base 注入。
gov-runner 是编译好的二进制（外部产物，不在本仓库），缺失则 SKIP（退出 0），绝不静默当成 PASS。
"""
import base64
import io
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
import zipfile
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
PROMPT_MD = REPO / "internal/store/prompts/gov_task_dev.md"
RUNNER = Path(os.environ.get("GOV_RUNNER", "/opt/datatoolbox/gov-runner"))
INJECT = int(os.environ.get("INJECT", "0") or "0")
KEEP = os.environ.get("KEEP") == "1"

# 模板表格样式指纹（示例 2 的「套用模板样式」必须把这些值从模板带进产出）
TPL_BORDER = {"style": "double", "size": 8, "color": "3E5C76"}
TPL_HEAD_FILL = "C6D9F1"
TPL_FONT = "仿宋"
TPL_FONT_PT = 10.5
TPL_COL_W = [2400, 1800, 1500, 1500]

EX1_OUT = "区市县情况-结构化(AI抽取).xlsx"
EX2_OUT = "产品汇总(模板样式).docx"
EX2_TPL = "gov_ex2_模板.docx"
EX2_DATA = "gov_ex2_产品介绍.docx"

# ==================== 素材真值（唯一真相源：既生成 fixture，也算期望产出） ====================
DIMS = ["人口", "经济", "工业", "教育"]
FI1 = {
    "province": "江源省",
    "cities": [
        {"city": "云台市", "districts": [
            {"district": "城东区", "counties": [
                {"name": "平安县", "dims": {
                    "人口": "全县常住人口 42.8 万人，其中苜蓿岭镇 3.1 万人。",
                    "经济": "全年地区生产总值 186.5 亿元，同比增长 6.2%。",
                    "工业": "规上工业企业 128 家，规上工业增加值 94.3 亿元。",
                    "教育": "普通中学 12 所，在校学生 1.86 万人。"}},
                {"name": "宁河县", "dims": {
                    "人口": "全县常住人口 31.2 万人，其中沙棘沟镇 2.4 万人。",
                    "经济": "全年地区生产总值 142.7 亿元，同比增长 5.1%。",
                    "工业": "规上工业企业 96 家，规上工业增加值 63.5 亿元。",
                    "教育": "普通中学 9 所，在校学生 1.32 万人。"}},
            ]},
            {"district": "白桦区", "counties": [
                {"name": "青松县", "dims": {
                    "人口": "全县常住人口 27.6 万人，其中白桦岭镇 2.0 万人。",
                    "经济": "全年地区生产总值 118.3 亿元，同比增长 4.8%。",
                    "工业": "规上工业企业 74 家，规上工业增加值 47.1 亿元。",
                    "教育": "普通中学 8 所，在校学生 1.05 万人。"}},
            ]},
        ]},
        {"city": "岭西市", "districts": [
            {"district": "青松区", "counties": [
                {"name": "桦南县", "dims": {
                    "人口": "全县常住人口 22.4 万人，其中桦树屯镇 1.7 万人。",
                    "经济": "全年地区生产总值 96.8 亿元，同比增长 4.2%。",
                    "工业": "规上工业企业 61 家，规上工业增加值 35.9 亿元。",
                    "教育": "普通中学 7 所，在校学生 0.89 万人。"}},
            ]},
        ]},
    ],
}

PRODUCTS = [
    ("一、江源省", None, None), ("（一）云台市", None, None), ("1. 城东区", None, None),
    ("云杉牌办公桌，规格 1400×700×750mm，参考价 1280 元，年产量 3200 张",
     "云杉牌办公桌", "1400×700×750mm"),
    ("云杉牌折叠椅，规格 常规，参考价 260 元，年产量 12000 把",
     "云杉牌折叠椅", "常规"),
    ("2. 白桦区", None, None),
    ("云杉牌书柜，规格 2000×400×1800mm，参考价 2100 元，年产量 800 组",
     "云杉牌书柜", "2000×400×1800mm"),
    ("（二）岭西市", None, None), ("1. 青松区", None, None),
    ("云杉牌文件柜，规格 1800×400×900mm，参考价 980 元，年产量 1500 组",
     "云杉牌文件柜", "1800×400×900mm"),
]
PRODUCT_GROUPS = ["江源省 / 云台市 / 城东区", "江源省 / 云台市 / 白桦区", "江源省 / 岭西市 / 青松区"]


def fi1_paragraphs():
    """公文正文段落（示例 1 输入）。"""
    out = ["江源省县域经济社会发展情况汇编", ""]
    out.append("一、" + FI1["province"])
    for c in FI1["cities"]:
        out.append("（一）" + c["city"])
        for i, d in enumerate(c["districts"], 1):
            out.append("%d. %s" % (i, d["district"]))
            for j, ct in enumerate(d["counties"], 1):
                out.append("（%d）%s" % (j, ct["name"]))
                for dim in DIMS:
                    out.append("%s：%s" % (dim, ct["dims"][dim]))
    return out


def fi2_paragraphs():
    """产品介绍段落（示例 2 输入）；INJECT=2 时抹掉「规格 」以制造真故障。"""
    out = []
    for line, _name, _spec in PRODUCTS:
        if INJECT == 2 and "，规格 " in line:
            line = line.replace("，规格 ", "，")
        out.append(line)
    return out


def expected_rows():
    """示例 1 期望的 4 行结构化结果。"""
    rows = []
    for c in FI1["cities"]:
        for d in c["districts"]:
            for ct in d["counties"]:
                r = {"所属省份": FI1["province"], "所属市": c["city"], "所属区": d["district"], "所属县": ct["name"]}
                for dim in DIMS:
                    r[dim + "情况"] = ct["dims"][dim]
                rows.append(r)
    return rows


def expected_products():
    """示例 2 期望的 4 个产品（含原文逐字）。

    注意：只依赖 PRODUCTS（真值源），**不依赖 INJECT**——注入改的是素材，
    不能同时改期望值，否则尺子会跟着故障一起变，注入就永远抓不到（D1 的历史坑）。"""
    out = []
    prov = city = dist = ""
    for line, name, spec in PRODUCTS:
        if line.startswith("一、"):
            prov = "江源省"
        elif line.startswith("（一）"):
            city = "云台市"
        elif line.startswith("（二）"):
            city = "岭西市"
        elif re.match(r"^\d+[、．.]\s*", line):
            dist = re.sub(r"^\d+[、．.]\s*", "", line)
        elif name:
            m = re.search(r"参考价\s*([^，,。；;]+)", line)
            n = re.search(r"年产量\s*([^，,。；;]+)", line)
            out.append({"province": prov, "city": city, "district": dist, "name": name, "spec": spec,
                        "price": m.group(1).strip() if m else "", "output": n.group(1).strip() if n else ""})
    return out


# ==================== fixture 落盘 ====================
def write_docx(path, paragraphs):
    import docx
    doc = docx.Document()
    for p in paragraphs:
        if p:
            doc.add_paragraph(p)
    doc.save(str(path))


def write_template_docx(path):
    """造「表格模板」Word：表格样式（双线边框/表头底色/表头加粗居中/正文字体字号/列宽）都塞进去，
    这些值必须原样出现在示例 2 的产出里，才叫「套模板」。"""
    import docx
    from docx.oxml import OxmlElement
    from docx.oxml.ns import qn
    from docx.enum.text import WD_ALIGN_PARAGRAPH
    from docx.shared import Emu, Pt

    doc = docx.Document()
    doc.add_paragraph("产品汇总表格模板（仅表格样式被使用）")
    t = doc.add_table(rows=2, cols=4)
    header = ["产品名称", "规格", "参考价", "年产量"]
    body = ["示例产品", "A1", "1 元", "1 件"]
    for ci, v in enumerate(header):
        cell = t.cell(0, ci)
        run = cell.paragraphs[0].add_run(v)
        run.bold = True
        cell.paragraphs[0].alignment = WD_ALIGN_PARAGRAPH.CENTER
        tcPr = cell._tc.get_or_add_tcPr()
        shd = OxmlElement("w:shd")
        shd.set(qn("w:val"), "clear")
        shd.set(qn("w:color"), "auto")
        shd.set(qn("w:fill"), TPL_HEAD_FILL)
        tcPr.append(shd)
    for ci, v in enumerate(body):
        cell = t.cell(1, ci)
        run = cell.paragraphs[0].add_run(v)
        run.font.name = TPL_FONT
        run.font.size = Pt(TPL_FONT_PT)
        rPr = run._element.get_or_add_rPr()
        rf = OxmlElement("w:rFonts")
        rf.set(qn("w:ascii"), TPL_FONT)
        rf.set(qn("w:eastAsia"), TPL_FONT)
        rPr.append(rf)
    for ci, w in enumerate(TPL_COL_W):
        t.columns[ci].width = Emu(int(w) * 635)
    for ci, w in enumerate(TPL_COL_W):
        for r in t.rows:
            r.cells[ci].width = Emu(int(w) * 635)
    tblPr = t._tbl.tblPr
    borders = OxmlElement("w:tblBorders")
    for side in ("top", "left", "bottom", "right", "insideH", "insideV"):
        el = OxmlElement("w:" + side)
        el.set(qn("w:val"), TPL_BORDER["style"])
        el.set(qn("w:sz"), str(TPL_BORDER["size"]))
        el.set(qn("w:space"), "0")
        el.set(qn("w:color"), TPL_BORDER["color"])
        borders.append(el)
    tblPr.append(borders)
    doc.save(str(path))


# ==================== 假 AI 桩 ====================
def stub_extract(prompt):
    """从提示词里抽【正文】并解析出县级行——按示例 1 的提示词契约来（这本身就是判据）。"""
    idx = prompt.rfind("【正文】")
    if idx < 0:
        return None
    text = prompt[idx + len("【正文】"):]
    prov = city = dist = ""
    rows = []
    for raw in text.split("\n"):
        line = raw.strip()
        if not line:
            continue
        m = re.match(r"^[一二三四五六七八九十]+、(.+)$", line)
        if m:
            prov = m.group(1).strip()
            continue
        m = re.match(r"^（[一二三四五六七八九十]+）(.+)$", line)
        if m:
            city = m.group(1).strip()
            dist = ""
            continue
        m = re.match(r"^\d+[、．.]\s*(.+?)\s*$", line)
        if m:
            dist = m.group(1).strip()
            continue
        m = re.match(r"^（\d+）(.+)$", line)
        if m:
            rows.append({"所属省份": prov, "所属市": city, "所属区": dist, "所属县": m.group(1).strip(),
                         "人口情况": "", "经济情况": "", "工业情况": "", "教育情况": ""})
            continue
        m = re.match(r"^(人口|经济|工业|教育)[：:](.*)$", line)
        if m and rows:
            rows[-1][m.group(1) + "情况"] += m.group(2).strip()
    return rows


class StubHandler(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(n)
        try:
            payload = json.loads(body.decode("utf-8"))
        except Exception:
            payload = {}
        # runner 的契约：POST {api_base}/api/v1/agent/completion  {prompt} -> {success, content}
        prompt = payload.get("prompt") or ""
        if not isinstance(prompt, str):
            prompt = json.dumps(prompt, ensure_ascii=False)
        if INJECT == 1:
            # 真故障：AI 返回废话（没有 JSON 数组）——客户真会撞上
            content = "好的，我先通读文档，理解层级后再抽取。"
        else:
            rows = stub_extract(prompt)
            if rows is None:
                content = "（桩：提示词里没找到【正文】标记）"
            else:
                content = json.dumps(rows, ensure_ascii=False)
        resp = json.dumps({"success": True, "content": content}, ensure_ascii=False).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(resp)))
        self.end_headers()
        self.wfile.write(resp)


def start_stub():
    srv = ThreadingHTTPServer(("127.0.0.1", 0), StubHandler)
    port = srv.server_address[1]
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv, port


# ==================== 抠出货文件里的示例脚本 ====================
def extract_fence(md, heading_re):
    """按行首围栏扫描取代码块（注意：示例代码内部含行内 ``` 字面量，
    用非贪婪正则会提前截断 → 必须只在「行首围栏」处收尾）。"""
    lines = md.split("\n")
    start = None
    for i, l in enumerate(lines):
        if l.strip() and re.match(heading_re, l.strip()) and not l.strip().startswith("```"):
            start = i
            break
    if start is None:
        return None
    fence = None
    for j in range(start + 1, len(lines)):
        s = lines[j].strip()
        if fence is None:
            if re.match(r"^```(javascript|js)\s*$", s):
                fence = j
            elif s and not s.startswith("#"):
                pass
        elif re.match(r"^```\s*$", s):
            return "\n".join(lines[fence + 1:j])
    return None


def load_examples():
    md = PROMPT_MD.read_text(encoding="utf-8")
    return {
        "ex1": extract_fence(md, r"^###\s*示例\s*1[：:].*$"),
        "ex2": extract_fence(md, r"^###\s*示例\s*2[：:].*$"),
    }


# ==================== 跑 runner ====================
def run_runner(code, files, api_base, workdir):
    task = {
        "code": code, "token": "stub-token", "database_id": "stub-db", "db_type": "sqlite",
        "databases": [], "input_text": "", "api_base": api_base,
        "files": [{"file_name": n, "file_base64": base64.b64encode(Path(p).read_bytes()).decode()} for n, p in files],
    }
    tf = Path(workdir) / "task.json"
    tf.write_text(json.dumps(task, ensure_ascii=False), encoding="utf-8")
    env = dict(os.environ, GOV_RUNNER_CLI="true")
    p = subprocess.run([str(RUNNER), str(tf)], cwd=workdir, env=env, capture_output=True, text=True, timeout=600)
    out = p.stdout or ""
    i = out.rfind('{\n  "success"')
    if i < 0:
        i = out.find('{"success"')
    if i < 0:
        return {"success": False, "error": "runner 没吐 JSON（前 400 字）：" + out[:400] + (p.stderr or "")[:400],
                "output": [], "output_files": []}
    try:
        return json.loads(out[i:])
    except Exception as e:
        return {"success": False, "error": "解析 runner stdout 失败：%s / %s" % (e, out[i:i + 300]), "output": [], "output_files": []}


# ==================== 产出物读取 ====================
def xlsx_read(path):
    import openpyxl
    wb = openpyxl.load_workbook(str(path), data_only=True)
    ws = wb[wb.sheetnames[0]]
    return ws.title, [[("" if c is None else str(c)) for c in row] for row in ws.iter_rows(values_only=True)]


def materialize(res, outdir, want_name):
    """runner 的产出不落盘，统一以 {name, content_base64} 回传 → 落盘才能验内容。"""
    for f in (res.get("output_files") or []):
        if not isinstance(f, dict):
            continue
        if f.get("name") != want_name:
            continue
        b64 = f.get("content_base64") or f.get("base64") or ""
        if not b64:
            continue
        p = Path(outdir) / want_name
        p.write_bytes(base64.b64decode(b64))
        return str(p)
    return None


def docx_xml(path):
    with zipfile.ZipFile(str(path)) as z:
        return z.read("word/document.xml").decode("utf-8", "replace")


def docx_text(xml):
    t = re.sub(r"<w:p\b[^>]*>", "\n", xml)
    t = re.sub(r"</w:p>", "\n", t)
    t = re.sub(r"<w:tab\b[^>]*/>", "\t", t)
    t = re.sub(r"<[^>]+>", "", t)
    return t.replace("&amp;", "&").replace("&lt;", "<").replace("&gt;", ">").replace("&quot;", '"')


# ==================== 尺子 ====================
class Ruler:
    def __init__(self):
        self.results = []          # (id, ok, detail)

    def ck(self, cid, ok, detail=""):
        self.results.append((cid, bool(ok), detail))
        print("%s %-6s %s" % ("[PASS]" if ok else "[FAIL]", cid, detail))
        return bool(ok)

    @property
    def failed(self):
        return sorted([r[0] for r in self.results if not r[1]])

    @property
    def total(self):
        return len(self.results)


def check_ex1(R, ex1_code, workdir):
    R.ck("A1", bool(ex1_code) and "COLUMNS = ['所属省份'" in ex1_code
         and "gov.writeExcel('区市县情况-结构化(AI抽取).xlsx'" in ex1_code,
         "示例 1 脚本从出货文件完整抠出（%d 字符，含首列定义与末尾 writeExcel）" % len(ex1_code or ""))
    for api in ("gov.readWord(", "gov.parseWordStructure(", "gov.callAI(", "gov.writeExcel("):
        ok = api in (ex1_code or "")
        cid = "A2." + re.sub(r"\W", "", api)[:12]
        R.results.append((cid, ok, "示例 1 用到 " + api))
        print("%s %s  示例 1 用到 %s" % ("[PASS]" if ok else "[FAIL]", cid, api))

    doc = str(Path(workdir) / "gov_ex1_公文.docx")
    write_docx(doc, fi1_paragraphs())
    srv, port = start_stub()
    try:
        res = run_runner(ex1_code, [("gov_ex1_公文.docx", doc)], "http://127.0.0.1:%d" % port, workdir)
    finally:
        srv.shutdown()

    logs = "\n".join(str(x) for x in (res.get("output") or []))
    R.ck("B1", res.get("success") is True, "runner success=%r error=%r" % (res.get("success"), res.get("error") or ""))
    n_sec = len(re.findall(r"^[一二三四五六七八九十]+、", "\n".join(fi1_paragraphs()), re.M))
    got_sec = re.search(r"已解析出标题层级 (\d+) 条", logs)
    R.ck("C1", bool(got_sec) and int(got_sec.group(1)) == 10, "标题层级日志：%s（期望 10 条）" % (got_sec.group(1) if got_sec else "缺失"))
    R.ck("C2", "AI 抽取到县级单位 4 个" in logs, "抽取计数日志：%s" % (re.search(r"AI 抽取到县级单位 \d+ 个", logs).group(0) if re.search(r"AI 抽取到县级单位 \d+ 个", logs) else "缺失"))

    files = res.get("output_files") or []
    names = [f.get("name") if isinstance(f, dict) else str(f) for f in files]
    R.ck("C3", EX1_OUT in names, "产出文件：%s" % names)
    if EX1_OUT not in names:
        return
    p = materialize(res, workdir, EX1_OUT)
    if not p:
        p = str((Path(workdir) / EX1_OUT)) if (Path(workdir) / EX1_OUT).exists() else None
    if not p:
        R.ck("C4", False, "产出文件没带 content_base64，无法验内容")
        R.ck("C5", False, "同上")
        R.ck("C6", False, "同上")
        return
    sheet, data = xlsx_read(p)
    exp = expected_rows()
    R.ck("C4", sheet == "结构化结果", "sheet 名：%r" % sheet)
    R.ck("C5", bool(data) and data[0] == ["所属省份", "所属市", "所属区", "所属县", "人口情况", "经济情况", "工业情况", "教育情况"],
         "表头：%s" % (data[0] if data else None))
    got = data[1:] if data else []
    want = [[r["所属省份"], r["所属市"], r["所属区"], r["所属县"], r["人口情况"], r["经济情况"], r["工业情况"], r["教育情况"]] for r in exp]
    R.ck("C6", got == want, "数据行 %d 行 vs 期望 %d 行（逐字比对）%s" % (len(got), len(want), "" if got == want else "\n  实得=" + json.dumps(got, ensure_ascii=False) + "\n  期望=" + json.dumps(want, ensure_ascii=False)))


def check_ex2(R, ex2_code, workdir):
    R.ck("A3", bool(ex2_code) and "const COLUMNS = ['产品名称'" in ex2_code and "doc.save('产品汇总(模板样式).docx')" in ex2_code,
         "示例 2 脚本从出货文件完整抠出（%d 字符）" % len(ex2_code or ""))
    for api in ("gov.readWordTables(", "gov.word()", "doc.tableFromTemplate(", "doc.save("):
        ok = api in (ex2_code or "")
        cid = "A4." + re.sub(r"\W", "", api)[:12]
        R.results.append((cid, ok, "示例 2 用到 " + api))
        print("%s %s  %s" % ("[PASS]" if ok else "[FAIL]", cid, "示例 2 用到 " + api))

    tpl = str(Path(workdir) / EX2_TPL)
    dat = str(Path(workdir) / EX2_DATA)
    write_template_docx(tpl)
    write_docx(dat, fi2_paragraphs())
    srv, port = start_stub()
    try:
        res = run_runner(ex2_code, [(EX2_TPL, tpl), (EX2_DATA, dat)], "http://127.0.0.1:%d" % port, workdir)
    finally:
        srv.shutdown()

    logs = "\n".join(str(x) for x in (res.get("output") or []))
    R.ck("B2", res.get("success") is True, "runner success=%r error=%r" % (res.get("success"), res.get("error") or ""))
    R.ck("C7", ("表格模板：" + EX2_TPL + "（已提取样式）") in logs,
         "模板样式提取日志：%s" % (re.search(r"表格模板：.*", logs).group(0) if re.search(r"表格模板：.*", logs) else "缺失"))
    R.ck("C8", "解析出产品 4 个" in logs,
         "产品计数日志：%s" % (re.search(r"解析出产品 \d+ 个", logs).group(0) if re.search(r"解析出产品 \d+ 个", logs) else "缺失"))

    files = res.get("output_files") or []
    names = [f.get("name") if isinstance(f, dict) else str(f) for f in files]
    R.ck("C9", EX2_OUT in names, "产出文件：%s" % names)
    p = materialize(res, workdir, EX2_OUT) if EX2_OUT in names else None
    if not p:
        cand = list(Path(workdir).rglob(EX2_OUT))
        p = str(cand[0]) if cand else None
    if not p or not Path(p).exists():
        R.ck("D1", False, "产出文件不存在，后续正文/样式判据无法评")
        return
    xml = docx_xml(p)
    text = docx_text(xml)
    prods = expected_products()
    R.ck("D1", ("共 %d 个产品" % len(prods)) in text, "正文产品数文案：%s（期望「共 %d 个产品」）"
         % (re.search(r"共 \d+ 个产品", text).group(0) if re.search(r"共 \d+ 个产品", text) else "缺失", len(prods)))
    R.ck("D2", "表格样式来自模板 Word。" in text and "未能读取模板样式" not in text,
         "模板样式来源文案：%s" % ("来自模板" if "表格样式来自模板 Word。" in text else "缺失/走了默认样式"))
    R.ck("D3", all(g in text for g in PRODUCT_GROUPS) and all(("产品：" + p0["name"]) in text for p0 in prods) and len(prods) > 0,
         "分组标题 %d 个 + 产品行 %d 个" % (len(PRODUCT_GROUPS), len(prods)))
    n_tbl = len(re.findall(r"<w:tbl(?:\s[^>]*)?>", xml))
    # 注意：len(prods)>0 是前提，否则「0 张表 == 0 个产品」是空跑绿
    R.ck("D4", len(prods) > 0 and n_tbl == len(prods), "产出表格数 %d（期望 %d 个产品）" % (n_tbl, len(prods)))
    R.ck("D5", len(prods) > 0
         and len(re.findall(r'w:fill="%s"' % TPL_HEAD_FILL, xml)) >= len(prods)
         and len(re.findall(r'w:val="%s"' % TPL_BORDER["style"], xml)) >= len(prods)
         and len(re.findall(r'w:sz="%d"' % TPL_BORDER["size"], xml)) >= len(prods)
         and len(re.findall(r'w:color="%s"' % TPL_BORDER["color"], xml)) >= len(prods)
         and len(re.findall(r'w:w="%d"' % TPL_COL_W[0], xml)) >= len(prods),
         "模板样式指纹（表头底色 %s / 双线 %s sz=%d / 列宽 %d）出现次数：fill=%d border=%d sz=%d color=%d w=%d"
         % (TPL_HEAD_FILL, TPL_BORDER["style"], TPL_BORDER["size"], TPL_COL_W[0],
            len(re.findall(r'w:fill="%s"' % TPL_HEAD_FILL, xml)),
            len(re.findall(r'w:val="%s"' % TPL_BORDER["style"], xml)),
            len(re.findall(r'w:sz="%d"' % TPL_BORDER["size"], xml)),
            len(re.findall(r'w:color="%s"' % TPL_BORDER["color"], xml)),
            len(re.findall(r'w:w="%d"' % TPL_COL_W[0], xml))))
    bad = []
    for p0 in prods:
        for v in (p0["name"], p0["spec"], p0["price"], p0["output"]):
            if v and v not in text:
                bad.append(v)
    R.ck("D6", len(prods) > 0 and not bad, "4 张表内产品名/规格/参考价/年产量逐字；缺失值：%s" % (bad or "无"))


EXPECTED_RED = {
    1: ["C2", "C6"],
    2: ["C8", "D1", "D3", "D4", "D5", "D6"],
}


def main():
    print("=" * 78)
    print("gov 内置技能示例脚本 · 真机验收（runner=%s）" % RUNNER)
    if not RUNNER.exists() or not os.access(str(RUNNER), os.X_OK):
        # gov-runner 是 DataToolbox 编译出来的外部产物，不在本仓库，所以 CI 上必然缺席。
        # 但「缺席」这个状态必须吵：SKIP≠PASS。而且不能让缺席变成永久常态 ——
        # 本地闸门（有 runner 的机器）上用 GOV_RUNNER_REQUIRED=1 把 SKIP 直接升成 FAIL，
        # 否则一把永远在 SKIP 的尺子和没有尺子是一样的，还多了「有人在看」的错觉。
        loud = [
            "",
            "!" * 78,
            "!! SKIP —— 本项【不算 PASS】",
            "!! 原因：找不到可执行的 gov-runner：%s" % RUNNER,
            "!! gov-runner 是 DataToolbox 编译出的外部产物，本仓库里没有，CI 上必然缺席。",
            "!! 后果：本脚本守着的东西（内置「数据治理任务开发」技能的两个示例脚本",
            "!!      真能跑通、模板样式真能传到产出）在本次运行中【无人看守】。",
            "!! 要真跑：在有 DataToolbox 运行环境的机器上执行（或 GOV_RUNNER=/path/to/gov-runner）。",
            "!" * 78,
            "",
        ]
        print("\n".join(loud))
        if os.environ.get("GOV_RUNNER_REQUIRED") == "1":
            print("[FAIL] GOV_RUNNER_REQUIRED=1 但 runner 缺席 —— 这道闸门在这台机器上必须真跑。")
            return 1
        return 0
    st = RUNNER.stat()
    print("runner md5=%s size=%d mtime=%s" % (__import__("hashlib").md5(RUNNER.read_bytes()).hexdigest()[:16],
                                              st.st_size, time.strftime("%Y-%m-%d %H:%M", time.localtime(st.st_mtime))))
    print("prompt 文件：%s" % PROMPT_MD)
    if not PROMPT_MD.exists():
        print("[FAIL] 出货提示词文件缺失：%s" % PROMPT_MD)
        return 1
    # 陈年风险提示：runner 比出货提示词旧 ⇒ 示例脚本可能已被改过而 runner 里跑的还是老逻辑。
    # 不做成 FAIL（重编译 runner 是另一个仓库的事，红在这里没人能修），但必须说出来。
    if RUNNER.stat().st_mtime < PROMPT_MD.stat().st_mtime:
        print("[warn] gov-runner（%s）比出货提示词（%s）旧 —— 示例脚本若已改过，本脚本验的是老 runner 的行为。"
              % (time.strftime("%Y-%m-%d %H:%M", time.localtime(RUNNER.stat().st_mtime)),
                 time.strftime("%Y-%m-%d %H:%M", time.localtime(PROMPT_MD.stat().st_mtime))))
    ex = load_examples()
    print("INJECT=%d  KEEP=%s" % (INJECT, KEEP))
    print("-" * 78)

    workdir = tempfile.mkdtemp(prefix="gov-ex-%d-" % INJECT)
    R = Ruler()
    try:
        print("—— 示例 1：公文 Word → 结构化 Excel（AI 抽取）——")
        check_ex1(R, ex.get("ex1"), workdir)
        print("—— 示例 2：产品汇总 Word 套模板样式 ——")
        check_ex2(R, ex.get("ex2"), workdir)
    finally:
        pass

    print("-" * 78)
    failed = R.failed
    print("判据 %d 项，FAIL %d 项：%s" % (R.total, len(failed), failed or "无"))
    rc = 0
    if INJECT:
        want = EXPECTED_RED[INJECT]
        if failed == want:
            print("[OK] 负向自证：注入的真故障被精确捕获，红的正好是预期集合 %s（其余 %d 项仍绿）" % (want, R.total - len(want)))
        else:
            print("[FAIL] 负向自证不成立：预期红 %s，实际红 %s（对称差 %s）" % (want, failed, sorted(set(want) ^ set(failed))))
            rc = 1
    elif failed:
        rc = 1
    print("中间产物目录：%s（KEEP=%s）" % (workdir, KEEP))
    if KEEP or rc != 0:
        print("[keep] 保留目录：%s" % workdir)
    else:
        shutil.rmtree(workdir, ignore_errors=True)
    print("=" * 78)
    return rc


if __name__ == "__main__":
    sys.exit(main())
