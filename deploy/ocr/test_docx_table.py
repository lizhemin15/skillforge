#!/usr/bin/env python3
"""docx 表格还原的确定性单测（从出货文件抠真函数跑，不在测试里复写实现）。

为什么有这个文件：用户场景是「给一份业务流程文档，生成一个能照着答的 skill」。
业务流程文档的干货**几乎全在表格里**（报销标准、审批时限、岗位职责、流程节点）。
旧 extract_docx 用 root.iter(w:p) 全平铺，表格单元格里的段落被当成普通段落逐个输出、
彼此没有任何分隔符，于是 5 行 4 列的表摊成 20 行「人员级别 / 住宿标准 / … / 总监 / 800」。
行列关系一旦丢失，模型只能猜「800 是哪一列」，实测把「经理住宿 600」答成 350，
还编出原文根本没有的条款 —— 这就是用户报的「幻觉严重」。

所以这里钉的不是「有没有输出数字」（那从来都有），而是**行列关系有没有保住**：
   T1 基本表格 → 输出 markdown 表格，且同行的单元格必须还在同一行
   T2 表格前后段落顺序不乱（表格要留在原位，不能全挤到文末）
   T3 横向合并（gridSpan）→ 补空列，后面列不左移
   T4 纵向合并（vMerge 续格）→ 不产生整行空白的假数据行
   T5 单元格文本里的竖线要转义，否则会撑破 markdown 表格
   T6 无表格文档 → 行为与旧版一致（逐段一行）
   T7 段落里嵌表格 → 表格内容不得混进该段文字

用法：
  python3 deploy/ocr/test_docx_table.py           # 正绿
  python3 deploy/ocr/test_docx_table.py --break   # 变异自证：换回旧的「全平铺」实现，
                                                  # 必须出现预期的 FAIL 行。自证成功也 exit 0；
                                                  # 没抓到红（假绿）exit 1。
"""
import importlib.util
import io
import sys
import zipfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
SRC = HERE / "ocrd.py"

W = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"


class _Dummy:
    """顶替 pymupdf / rapidocr_onnxruntime，只为让模块 import 通过（本测试不用它们）。"""

    def __getattr__(self, k):
        return _Dummy()

    def __call__(self, *a, **k):
        return _Dummy()


def load_ocrd(break_it=False):
    """从出货文件加载 ocrd 模块。打桩可选依赖 → 任何机器（含 CI runner）都能跑。"""
    for name in ("pymupdf", "rapidocr_onnxruntime"):
        sys.modules.setdefault(name, _Dummy())
    spec = importlib.util.spec_from_file_location("ocrd_under_test", SRC)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    if break_it:
        # 变异：换回旧的全平铺实现 —— 只改 extract_docx 一个函数，其余保持真实现
        def old_extract_docx(raw):
            with zipfile.ZipFile(io.BytesIO(raw)) as z:
                xml = z.read("word/document.xml")
            from xml.etree import ElementTree as ET
            root = ET.fromstring(xml)
            paras = []
            for p in root.iter("{%s}p" % W):
                texts = [t.text or "" for t in p.iter("{%s}t" % W)]
                paras.append("".join(texts).strip())
            return "\n".join(paras), len(paras)
        mod.extract_docx = old_extract_docx
    return mod


# ---------- 造 docx ----------
def _esc(s):
    return (s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;"))


def _p(text):
    return '<w:p><w:r><w:t xml:space="preserve">%s</w:t></w:r></w:p>' % _esc(text)


def _tc(text, span=1, vmerge=False):
    tcpr = "<w:tcPr>"
    if span > 1:
        tcpr += '<w:gridSpan w:val="%d"/>' % span
    if vmerge:
        tcpr += "<w:vMerge/>"
    tcpr += "</w:tcPr>"
    return "<w:tc>%s%s</w:tc>" % (tcpr, _p(text))


def _tr(cells):
    return "<w:tr>%s</w:tr>" % "".join(cells)


def _tbl(rows):
    return "<w:tbl>%s</w:tbl>" % "".join(rows)


def make_docx(body_xml):
    doc = ('<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
           '<w:document xmlns:w="%s"><w:body>%s</w:body></w:document>' % (W, body_xml))
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as z:
        z.writestr("[Content_Types].xml",
                   '<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">'
                   '<Default Extension="xml" ContentType="application/xml"/></Types>')
        z.writestr("word/document.xml", doc)
    return buf.getvalue()


results = []


def ok(cond, msg):
    results.append(bool(cond))
    print(("  ok   " if cond else "  FAIL ") + msg)


def rows_of(md):
    """把 markdown 表格拆成 [[cell,...],...]，忽略分隔行。"""
    out = []
    for line in md.splitlines():
        line = line.strip()
        if not line.startswith("|"):
            continue
        cells = [c.strip() for c in line.strip("|").split("|")]
        if all(set(c) <= {"-"} and c for c in cells):
            continue
        out.append(cells)
    return out


def main(break_it=False):
    mod = load_ocrd(break_it)
    ex = mod.extract_docx

    print("— T1 基本表格：行列关系必须保住 —")
    body = (_p("第二条 报销标准如下表：")
            + _tbl([_tr([_tc("人员级别"), _tc("住宿标准(元/晚)"), _tc("伙食补助(元/天)")]),
                    _tr([_tc("经理"), _tc("600"), _tc("120")]),
                    _tr([_tc("普通员工"), _tc("450"), _tc("100")])])
            + _p("第三条 超标准部分个人承担。"))
    text, n = ex(make_docx(body))
    tbl = [r for r in rows_of(text)]
    ok(len(tbl) == 3, "解析出 3 行数据（含表头），实际 %d 行：%r" % (len(tbl), tbl[:3]))
    ok(any(r[:3] == ["经理", "600", "120"] for r in tbl),
       "「经理|600|120」仍在同一行 —— 这是本用例的全部意义：600 和它的列头对得上")
    ok(not any("600" in ln and ln.strip() == "600" for ln in text.splitlines()),
       "600 不再单独占一行（旧实现每格一行，模型无从判断它属于哪列）")

    print("— T2 表格位置：必须在原位，不能挤到文末 —")
    lines = [l for l in text.splitlines() if l.strip()]
    i_before = next((i for i, l in enumerate(lines) if "如下表" in l), -1)
    i_tbl = next((i for i, l in enumerate(lines) if l.startswith("|")), -1)
    i_after = next((i for i, l in enumerate(lines) if "超标准" in l), -1)
    ok(i_before < i_tbl < i_after,
       "顺序为 前文(%d) → 表格(%d) → 后文(%d)" % (i_before, i_tbl, i_after))

    print("— T3 横向合并：补空列，后续列不左移 —")
    body = _tbl([_tr([_tc("项目", span=2), _tc("金额")]),
                 _tr([_tc("A"), _tc("B"), _tc("999")])])
    text, _ = ex(make_docx(body))
    tbl = rows_of(text)
    ok(tbl and len(tbl[-1]) == 3, "数据行列数 = 3（横向合并已展开），实际 %r" % (tbl[-1] if tbl else None))
    ok(tbl and tbl[-1][-1] == "999", "最后一列仍是 999，没有因合并而左移")

    print("— T4 纵向合并：续格留空占位，不左移也不重复 —")
    # 真实形态：财务部跨两行（第 2 行起始格有字，第 3 行同列是 vMerge 空占位），
    # 而「负责人」列每行各有各的人。若实现把续格整个丢掉，第 3 行会变成
    # 只有 ['李四'] 一个格子 → 李四串到「部门」列下 → 答「财务部负责人是谁」就会错位。
    # 注意续格本身是**空**的（内容属于上方那格），所以这里第二个参数传空串。
    body = _tbl([_tr([_tc("部门"), _tc("负责人")]),
                 _tr([_tc("财务部"), _tc("张三")]),
                 _tr([_tc("", vmerge=True), _tc("李四")])])
    text, _ = ex(make_docx(body))
    tbl = rows_of(text)
    ok(not any(all(c == "" for c in r) for r in tbl),
       "没有出现整行全空的假数据行：%r" % (tbl,))
    # 第 2、3 行各 2 列；李四必须还在第 2 列，且「财务部」不得在下一行重复出现
    ok(all(len(r) == 2 for r in tbl), "每行都是 2 列（续格留了空占位）：%r" % (tbl,))
    ok(not any("李四" in r[0] for r in tbl if r),
       "李四没被挤到第 1 列（部门列）：%r" % (tbl,))
    ok(sum(1 for r in tbl for c in r if "财务部" in c) == 1,
       "「财务部」只出现一次（续格没有把内容重复一遍）")

    print("— T5 单元格内竖线必须转义 —")
    body = _tbl([_tr([_tc("项"), _tc("值")]), _tr([_tc("A|B"), _tc("1")])])
    text, _ = ex(make_docx(body))
    tbl = rows_of(text)
    ok(any("A\\|B" in ln for ln in text.splitlines()),
       "竖线被转义成 \\|，不会把单元格劈成两列")

    print("— T6 无表格文档：行为与旧版一致 —")
    body = _p("第一条 出差前须提交申请。") + _p("第二条 报销时限十个工作日。")
    text, n = ex(make_docx(body))
    ok(text.splitlines() == ["第一条 出差前须提交申请。", "第二条 报销时限十个工作日。"],
       "逐段一行，顺序不变：%r" % text.splitlines())

    print("— T7 段落里嵌表格：不得混进该段文字 —")
    inner = _tbl([_tr([_tc("内表格")])])
    body = ('<w:p><w:r><w:t>正文前</w:t></w:r>%s<w:r><w:t>正文后</w:t></w:r></w:p>' % inner)
    text, _ = ex(make_docx(body))
    ok("正文前正文后" in text,
       "段落文字连成一句（表格内容没插进来）：%r" % [l for l in text.splitlines() if "正文" in l])

    failed = results.count(False)
    print("")
    if break_it:
        if failed == 0:
            print("变异自证失败：换回旧实现后一条都没红 —— 测试是假绿的")
            return 1
        print("变异自证成功：旧实现下有 %d 条预期 FAIL" % failed)
        return 0
    if failed:
        print("表格还原：%d 项 FAIL" % failed)
        return 1
    print("表格还原：全部通过")
    return 0


if __name__ == "__main__":
    sys.exit(main("--break" in sys.argv))
