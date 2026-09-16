#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""造一份「能进手册模式」的混合素材 PDF：前两页扫描（无文字层），后面是可选中文字。

为什么要新造：
  线上验收时用的 /tmp/mixed_small.pdf 只抽出 1 个分类，而手册模式的入口门禁是
  ≥2 个分类（internal/skillgen/manual.go: manualMinCategories）。于是「8.5/9 裁判
  独立试用评分」这一段根本没被跑到，A5 的 FAIL 里混进了「本轮本不该跑」的假红。
  本次专门造一份含 3 个文体分类（通知 / 通报 / 会议纪要）+ 逐字范文的手册，
  用来真正验到手册模式与 8.5/9 的流式。

用法：
  python3 web/tests/make_manual_mode_fixture.py /tmp/manual_mode.pdf
"""
import os
import sys

import pymupdf
from PIL import Image, ImageDraw, ImageFont

FONT = "/usr/share/fonts/truetype/wqy/wqy-zenhei.ttc"
A4 = pymupdf.paper_rect("a4")

# 总则（不是分类：extractStructure 的 prompt 明确要求把目录/前言/总则判为非分类）
GENERAL = [
    "第一章  总则",
    "1.1  编写目的",
    "本手册用于统一公司对外行文的写作口径、结构与审核标准。各部门对外发出的每一篇文稿，",
    "都必须先在本手册中定位到自己所属的文体，再按该文体的要求逐条落实，不得凭个人习惯行文。",
    "1.2  适用范围",
    "本手册覆盖三类最常用的对外行文：通知、通报、会议纪要。三类之外的文体，应比照最接近的",
    "一类执行，并在审稿记录里写明比照依据。",
    "1.3  通用要求",
    "第一，标题必须包含主体与核心动作，字数不超过 22 个字。",
    "第二，正文首段必须交代时间、主体、事件、地点四项要素，缺一项即退回重写。",
    "第三，全文不得出现「大概」「可能」「应该是」这类模糊表述；无依据的判断一律删去。",
    "第四，涉及数据必须写明统计口径与时间区间。",
]

# 三个真实文体分类：每类有触发场景、写作要求、两篇逐字范文。
# 范文刻意写得互不重复，因为锚点定位要求「唯一出现一次」的连续片段。
CATEGORIES = [
    {
        "chapter": "第二章  通知的写法",
        "trigger": "需要向内部或外部特定对象告知某项安排、要求对方在指定时间按指定方式执行时。",
        "requirement": [
            "通知必须具备可执行性：每一条要求都要写清执行主体、完成时点和交付物。",
            "通知不得夹带与告知事项无关的背景叙述；背景最多一句，且必须与执行动作直接相关。",
            "通知的落款必须包含发文部门与发文日期，缺一不可。",
        ],
        "samples": [
            [
                "关于调整周例会时间安排的通知",
                "各部门：",
                "自二〇二六年九月二十一日起，公司周例会调整至每周一上午九时三十分召开，会议地点",
                "调整至三号会议室，参会人员范围不变。",
                "请各部门负责人于每周五十七时前，将本部门下周议题提交至行政部邮箱，逾期不再受理。",
                "首次执行时间为二〇二六年九月二十一日，请各部门提前做好准备。",
                "行政部",
                "二〇二六年九月十八日",
            ],
            [
                "关于规范外部用印申请流程的通知",
                "各业务部门：",
                "自本通知发布之日起，对外合同用印一律通过线上用印系统提交，纸质申请单不再受理。",
                "申请人须在系统中上传合同定稿、对方主体资质文件与内部审批记录三项材料。",
                "用印申请须在用印日前两个工作日提交；紧急用印须由部门负责人电话确认后方可加急。",
                "未按上述要求提交的申请，法务部将直接退回，不再逐项提醒。",
                "法务部",
                "二〇二六年九月十五日",
            ],
        ],
    },
    {
        "chapter": "第三章  通报的写法",
        "trigger": "需要就某一事项的处理结果、典型问题或先进经验在较大范围内告知并引起重视时。",
        "requirement": [
            "通报必须先陈述事实、再说明处理依据、最后给出要求，三段顺序不得调换。",
            "通报中涉及的单位与个人，必须使用全称或规范简称，首次出现不得用「某部门」代替。",
            "通报结尾必须写明整改期限与反馈对象。",
        ],
        "samples": [
            [
                "关于三季度客户信息管理检查情况的通报",
                "各分公司、各业务部门：",
                "三季度共抽查客户档案一千二百份，其中信息缺失档案八十七份，占比百分之七点三。",
                "缺失集中在联系方式变更记录与授权文件两项，涉及四个业务部门。",
                "根据公司《客户信息管理办法》第十七条，现对上述四个部门予以通报批评，并责令限期整改。",
                "请相关部门于十月二十日前完成补录，并将整改结果书面反馈至合规部。",
                "合规部",
                "二〇二六年十月八日",
            ],
            [
                "关于表彰技术攻坚团队先进事迹的通报",
                "全体员工：",
                "技术中心数据平台团队历时五个月，完成核心链路迁移，迁移期间业务零中断。",
                "该团队在迁移方案评审中主动提出三项风险预案，均在实施阶段发挥作用。",
                "经公司管理层研究决定，对上述团队予以通报表彰，并给予专项奖励。",
                "请各部门组织学习其经验，于十月底前提交学习记录。",
                "人力资源部",
                "二〇二六年九月二十八日",
            ],
        ],
    },
    {
        "chapter": "第四章  会议纪要的写法",
        "trigger": "会议结束后需要固化讨论结论、明确后续动作与责任分工时。",
        "requirement": [
            "会议纪要只记录结论与行动项，不记录发言过程与讨论细节。",
            "每个行动项必须写清责任部门、完成时点与验收标准，三者缺一视为无效行动项。",
            "纪要在会议结束后两个工作日内发出，超期发出的须在文首注明原因。",
        ],
        "samples": [
            [
                "产品定价专题会议纪要",
                "会议时间：二〇二六年九月二十二日十四时至十六时",
                "会议地点：总部五号会议室",
                "参加人员：市场部、财务部、产品部负责人",
                "一、结论",
                "新版产品线维持现有价格带，不参与本季度价格战。",
                "二、行动项",
                "产品部于九月三十日前完成竞品价格对比表，交付市场部；验收标准为覆盖前五名竞品。",
                "财务部于十月十日日前完成新版成本测算模型，交付产品部；验收标准为可复算。",
                "市场部于十月十五日前完成渠道沟通方案，交付管理层；验收标准为含时间表与预算。",
            ],
            [
                "供应商准入评审会议纪要",
                "会议时间：二〇二六年九月十九日九时至十一时",
                "会议地点：线上会议",
                "参加人员：采购部、质量部、法务部负责人",
                "一、结论",
                "本批次三家候选供应商中，两家通过准入评审，一家暂缓。",
                "二、行动项",
                "质量部于九月二十六日前完成通过供应商的样品检测报告，交付采购部。",
                "法务部于九月二十五日前完成暂缓供应商的资质补充清单，交付采购部。",
                "采购部于十月八日前完成首批订单谈判，交付管理层；验收标准为含违约条款。",
            ],
        ],
    },
]

CSS_LINES_PER_PAGE = 30


def scan_page(lines: list[str], title: str) -> bytes:
    """把文字渲染成图片：模拟扫描件 —— 只有像素、没有文字层。"""
    w, h = 1240, 1754
    img = Image.new("RGB", (w, h), (250, 249, 246))
    d = ImageDraw.Draw(img)
    big = ImageFont.truetype(FONT, 52)
    body = ImageFont.truetype(FONT, 34)
    d.text((110, 130), title, font=big, fill=(24, 24, 24))
    y = 260
    for ln in lines:
        d.text((110, y), ln, font=body, fill=(40, 40, 40))
        y += 54
    # 扫描件的灰边与噪声：让 OCR 走真路径
    for i in range(0, w, 7):
        d.point((i, 20), fill=(205, 205, 205))
        d.point((i, h - 20), fill=(205, 205, 205))
    out = "/tmp/_scan_page.png"
    img.save(out, format="PNG", optimize=True)
    # 转成 JPEG 再进 PDF：PNG 会让这份料膨胀到 6MB 以上，上传/解析都白等。
    jpg = "/tmp/_scan_page.jpg"
    img.convert("RGB").save(jpg, format="JPEG", quality=82, optimize=True)
    return open(jpg, "rb").read()


def wrap(text: str, per_line: int = 40) -> list[str]:
    out = []
    for para in text.split("\n"):
        while len(para) > per_line:
            out.append(para[:per_line])
            para = para[per_line:]
        out.append(para)
    return out


def main() -> int:
    dst = sys.argv[1] if len(sys.argv) > 1 else "/tmp/manual_mode.pdf"
    doc = pymupdf.open()

    # —— 前两页：扫描件（无文字层）——
    cover = scan_page(
        ["公司对外行文写作手册", "（二〇二六年版）", "", "发布部门：综合管理部", "发布日期：二〇二六年九月一日"],
        "公司对外行文写作手册",
    )
    toc = scan_page(
        ["目录", "第一章  总则", "第二章  通知的写法", "第三章  通报的写法", "第四章  会议纪要的写法"],
        "目录",
    )
    for raw in (cover, toc):
        p = doc.new_page(width=A4.width, height=A4.height)
        p.insert_image(p.rect, stream=raw)

    # —— 第三页起：真实文字层 ——
    flow: list[str] = list(GENERAL)
    for c in CATEGORIES:
        flow.append("")
        flow.append(c["chapter"])
        flow.append("触发场景：" + c["trigger"])
        flow.append("写作要求：")
        flow.extend(c["requirement"])
        for idx, s in enumerate(c["samples"], 1):
            flow.append(f"范文 {idx}：")
            flow.extend(s)

    wrapped = []
    for ln in flow:
        wrapped.extend(wrap(ln))
    if not os.path.exists(FONT):
        print("缺中文字体，无法造料", file=sys.stderr)
        return 2

    for i in range(0, len(wrapped), CSS_LINES_PER_PAGE):
        chunk = wrapped[i : i + CSS_LINES_PER_PAGE]
        p = doc.new_page(width=A4.width, height=A4.height)
        y = 72.0
        for ln in chunk:
            p.insert_textbox(
                pymupdf.Rect(64, y, A4.width - 64, y + 26),
                ln,
                fontname="wqy",
                fontfile=FONT,
                fontsize=11.5,
                align=0,
            )
            y += 26.0

    doc.save(dst, garbage=4, deflate=True)
    doc.close()

    # 出料就自检：扫描页必须真没文字层、文字页必须真能选中，否则这份料验不了 A6。
    # 文字页用 ≥100 而不是「很多字」：末页本来就可能是零头，那不是缺陷。
    chk = pymupdf.open(dst)
    lens = [len(p.get_text().strip()) for p in chk]
    print(f"写出 {dst}：{chk.page_count} 页，逐页文字层长度={lens}")
    ok = lens[0] == 0 and lens[1] == 0 and len(lens) > 3 and min(lens[2:]) >= 100
    print(
        "自检：前两页无文字层 ✓、其余页均可选中 ✓"
        if ok
        else "自检失败：扫描页应无文字层、文字页应可选中 ✗"
    )
    chk.close()
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
