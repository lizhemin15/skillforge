#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""造 E2E 素材：一份「公文」docx，里面**内嵌两个 word 模板**（word/embeddings/）。
对应真实场景：管理员上传一份通知，正文写「附件：1.报名表 2.汇总表」，
这两张表不是单独上传的文件，而是 OLE 内嵌在通知里。用户命中技能时要把它们发给用户。

产出：
  {os.environ.get('ATTACH_E2E_DIR', '/tmp/sf-attach-e2e')}/outer.docx      公文（上传给 skillforge 的源文件）
  {os.environ.get('ATTACH_E2E_DIR', '/tmp/sf-attach-e2e')}/inner1.docx     期望被拆出来的第 1 份附件（用于逐字节 md5 比对）
  {os.environ.get('ATTACH_E2E_DIR', '/tmp/sf-attach-e2e')}/inner2.docx     期望被拆出来的第 2 份附件
"""
import zipfile, os, io

OUT = "{os.environ.get('ATTACH_E2E_DIR', '/tmp/sf-attach-e2e')}"
CT = '''<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Default Extension="bin" ContentType="application/vnd.openxmlformats-officedocument.oleObject"/>
<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>
</Types>'''


def docx_bytes(paras, table_rows=None):
    """最小但合法的 docx（Word 能打开的那种结构）。"""
    body = ""
    for p in paras:
        body += f'<w:p><w:r><w:t xml:space="preserve">{p}</w:t></w:r></w:p>'
    if table_rows:
        rows = ""
        for r in table_rows:
            cells = "".join(
                f'<w:tc><w:tcPr><w:tcW w:w="2400" w:type="dxa"/></w:tcPr><w:p><w:r><w:t>{c}</w:t></w:r></w:p></w:tc>'
                for c in r
            )
            rows += f"<w:tr>{cells}</w:tr>"
        body += (
            '<w:tbl><w:tblPr><w:tblW w:w="0" w:type="auto"/></w:tblPr>'
            f"{rows}</w:tbl>"
        )
    doc = (
        '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
        '<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">'
        f"<w:body>{body}<w:sectPr/></w:body></w:document>"
    )
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w", zipfile.ZIP_DEFLATED) as z:
        z.writestr("[Content_Types].xml", CT)
        z.writestr(
            "_rels/.rels",
            '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
            '<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">'
            '<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>'
            "</Relationships>",
        )
        z.writestr("word/document.xml", doc)
    return buf.getvalue()


inner1 = docx_bytes(
    ["报名表", "（请如实填写，逐项不得空缺）"],
    [["姓名", "性别", "出生年月"], ["", "", ""], ["学历/职称", "联系电话", ""], ["", "", ""]],
)
inner2 = docx_bytes(
    ["汇总表", "（由各单位填报后统一报送）"],
    [["序号", "单位", "报名人数", "联系人"], ["1", "", "", ""], ["2", "", "", ""]],
)

open(f"{OUT}/inner1.docx", "wb").write(inner1)
open(f"{OUT}/inner2.docx", "wb").write(inner2)

# ---- 外层公文：正文里带附件清单（提取器靠它给附件起有意义的名字）----
outer_paras = [
    "关于开展2026年度业务培训工作的通知",
    "各有关单位：",
    "为进一步提升业务能力，夯实基层工作基础，现就2026年度业务培训有关事项通知如下。",
    "一、培训对象：各单位在职业务骨干，每个单位推荐不超过3人；新入职不满一年的人员不占名额。",
    "二、培训时间：2026年11月3日至11月7日，共5天，11月3日上午8:30前报到。",
    "三、培训地点：市教育培训中心（东区）综合楼三层报告厅。",
    "四、培训内容：政策法规解读、业务操作实务、典型案例分析、分组研讨与实操考核。",
    "五、费用说明：培训不收取费用，交通及食宿由派出单位按规定报销。",
    "六、报送要求：请各单位于2026年10月24日前，将报名表（附件1）填写完整并加盖公章，"
    "同时填写汇总表（附件2），将两份材料的电子版一并报送至指定邮箱，纸质材料报送至办公室。",
    "七、注意事项：参训人员须全程参加，不得无故缺勤；培训期间遵守会场纪律，手机调至静音。",
    "八、写作要求：本类通知正文须依次写明培训对象、时间、地点、内容、费用、报送要求与注意事项，"
    "语气正式、条理清晰，分条列项，不使用口语化表达，落款为发文单位与发文日期。",
    "九、参考范文：关于举办安全生产培训的通知（略）。各单位可参照上述结构撰写本单位通知。",
    "附件：1.报名表 2.汇总表",
]
outer_doc_xml = (
    '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
    '<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"'
    ' xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">'
    "<w:body>"
    + "".join(f'<w:p><w:r><w:t xml:space="preserve">{p}</w:t></w:r></w:p>' for p in outer_paras)
    + "<w:sectPr/></w:body></w:document>"
)

with zipfile.ZipFile(f"{OUT}/outer.docx", "w", zipfile.ZIP_DEFLATED) as z:
    z.writestr("[Content_Types].xml", CT)
    z.writestr(
        "_rels/.rels",
        '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
        '<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">'
        '<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>'
        "</Relationships>",
    )
    z.writestr("word/document.xml", outer_doc_xml)
    # 真正内嵌的两个附件（OLE 容器位置，载荷就是完整的 word 文件字节）
    z.writestr("word/embeddings/oleObject1.bin", inner1)
    z.writestr("word/embeddings/oleObject2.bin", inner2)
    z.writestr(
        "word/_rels/document.xml.rels",
        '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
        '<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">'
        '<Relationship Id="rId10" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/oleObject" Target="embeddings/oleObject1.bin"/>'
        '<Relationship Id="rId11" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/oleObject" Target="embeddings/oleObject2.bin"/>'
        "</Relationships>",
    )

import hashlib

for f in ("outer.docx", "inner1.docx", "inner2.docx"):
    p = f"{OUT}/{f}"
    print(f"{f}\t{os.path.getsize(p)}B\tmd5={hashlib.md5(open(p,'rb').read()).hexdigest()}")
