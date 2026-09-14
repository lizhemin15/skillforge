#!/usr/bin/env python3
"""构造「主题型混合 PDF」测试料 —— 前 N 页扫描（纯图片，无文字层）+ 后 M 页可选中文字。

与 build_mixed_pdf.py 的区别：那份产出的正文是「条款/关键句」这类机械句子，够验
ocrd 的逐页判定，但**不足以验「训练出来的 skill 与素材有关系」**——机械句子喂给 LLM
后，产出里不会留下可断言的痕迹。

这份造的是**一份真实感的写作素材**：围绕一个不与其它任何素材重叠的独特主题词
（默认「蒲公英月报」），扫描页写「写作要点」，文字页写「范文段」。于是可以断言：

  source/<base>.txt          必须含扫描页关键词（证明扫描页真进了素材）
  system_prompt.md / template.md / meta.json
                             必须出现主题词（证明技能确实由这份素材长出来）

用法：
  python3 build_topic_pdf.py out.pdf --topic 蒲公英月报 --scan 2 --text 2 [--watermark]
"""
import argparse
import os
import sys

import pymupdf

CJK_FONT = None
for cand in [
    "/usr/share/fonts/truetype/wqy/wqy-zenhei.ttc",
    "/usr/share/fonts/truetype/wqy/wqy-microhei.ttc",
    "/root/skillforge/web/fonts/wqy-zenhei.ttc",
    "/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
]:
    if os.path.exists(cand):
        CJK_FONT = cand
        break
if not CJK_FONT:
    sys.exit("找不到 CJK 字体（标题字体必须先确认 has_glyph 覆盖数字，否则 OCR 读出方块）")

# 扫描页（图片，只能靠 OCR 读出）：写作要点
SCAN_PAGES = [
    [
        "{topic} 写作规范 第 1 节",
        "要点一：标题只写事实，不写形容词。例：{topic}三季度服务小结。",
        "要点二：正文第一段必须交代时间、地点、参与人数三个要素。",
        "要点三：结尾统一署名，落款写「{topic}编辑组」，不要写成个人。",
    ],
    [
        "{topic} 写作规范 第 2 节",
        "要点四：数据必须带单位与口径，例如「服务 1200 人次（含上门 300 人次）」。",
        "要点五：禁止使用「高度重视」「大力开展」这类空话，改成可核查的动作。",
        "落款联系人固定写 编辑组，电话 010-00000000。",
    ],
]

# 文字层页：范文片段
TEXT_PAGES = [
    [
        "{topic}范文片段（可直接参照）",
        "本季度，{topic}编辑组在三个社区开展志愿宣讲 12 场，服务 1200 人次。",
        "其中上门服务 300 人次，覆盖独居老人 86 户，全部登记在册。",
        "下一步将把宣讲对象扩展到辖区内的中小学校。",
    ],
    [
        "{topic}写作检查清单",
        "一、标题是否只有事实；二、首段是否含时间地点人数；三、数据是否带单位。",
        "四、是否出现空话套话；五、落款是否为「{topic}编辑组」。",
        "以上五条全部通过后再交稿。",
    ],
]


def render_scan_page(pdf, lines, dpi_img=240):
    """把文字渲染成位图贴进 PDF —— 等价于真实扫描页（无文字层）。"""
    tmp = pymupdf.open()
    w, h = 595, 842
    pg = tmp.new_page(width=w, height=h)
    y = 80
    for ln in lines:
        pg.insert_text((60, y), ln, fontname="cjk", fontfile=CJK_FONT, fontsize=14)
        y += 34
    pix = pg.get_pixmap(dpi=dpi_img)
    img_bytes = pix.tobytes("jpeg")
    tmp.close()
    page = pdf.new_page(width=w, height=h)
    page.insert_image(pymupdf.Rect(0, 0, w, h), stream=img_bytes)
    return page


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("out")
    ap.add_argument("--topic", default="蒲公英月报")
    ap.add_argument("--scan", type=int, default=2)
    ap.add_argument("--text", type=int, default=2)
    ap.add_argument("--watermark", action="store_true",
                    help="扫描页叠一层薄文本层（模拟扫描 App 写入的页码水印）")
    ap.add_argument("--blank", type=int, default=0,
                    help="追加 N 页「纯图片但无字」的页面 —— 用于验素材门禁硬失败路径"
                         "（解析出来等于没读到正文时必须中止训练，而不是退回通用流程）")
    a = ap.parse_args()

    pdf = pymupdf.open()
    for i in range(a.scan):
        lines = [ln.format(topic=a.topic) for ln in SCAN_PAGES[i % len(SCAN_PAGES)]]
        pg = render_scan_page(pdf, lines)
        if a.watermark:
            pg.insert_text((520, 820), f"第 {i + 1} 页",
                           fontname="cjk", fontfile=CJK_FONT, fontsize=9)
            pg.insert_text((60, 820), "扫描全能王 CamScanner",
                           fontname="cjk", fontfile=CJK_FONT, fontsize=9)
    for i in range(a.text):
        pg = pdf.new_page(width=595, height=842)
        y = 80
        for ln in TEXT_PAGES[i % len(TEXT_PAGES)]:
            pg.insert_text((60, y), ln.format(topic=a.topic),
                           fontname="cjk", fontfile=CJK_FONT, fontsize=13)
            y += 30
        for k in range(10):
            pg.insert_text((60, y), f"{a.topic} 补充说明 {k + 1}：正文细节行。",
                           fontname="cjk", fontfile=CJK_FONT, fontsize=11)
            y += 22
    # 「纯图片但无字」的页：整页只有一张浅灰底图，OCR 读不出任何正文。
    # 存在意义是构造「解析成功但等于没读到正文」的现场 —— 门禁必须硬失败。
    if a.blank:
        tmp = pymupdf.open()
        pg0 = tmp.new_page(width=595, height=842)
        pg0.draw_rect(pymupdf.Rect(0, 0, 595, 842), color=(0.85, 0.85, 0.85),
                      fill=(0.9, 0.9, 0.9))
        pix = pg0.get_pixmap(dpi=150)
        blank_img = pix.tobytes("jpeg")
        tmp.close()
        for _ in range(a.blank):
            pg = pdf.new_page(width=595, height=842)
            pg.insert_image(pymupdf.Rect(0, 0, 595, 842), stream=blank_img)
    pdf.save(a.out)
    pdf.close()
    print(f"wrote {a.out} scan={a.scan} text={a.text} watermark={a.watermark} "
          f"blank={a.blank} topic={a.topic} size={os.path.getsize(a.out)}")


if __name__ == "__main__":
    main()
