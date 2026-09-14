#!/usr/bin/env python3
"""构造「混合型 PDF」测试料：前 N 页扫描（图片，无文字层），后 M 页可选中文字。

可选变体：
  --scan-watermark  在扫描页上叠加极小文字层（模拟扫描软件写入的页码/水印文本层）
用于验证 ocrd 是否「有文字层的页直取、扫描页 OCR」。
"""
import argparse, os, sys
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
    sys.exit("找不到 CJK 字体")


def render_scan_page(pdf, lines, dpi_img=200):
    """把文字渲染成位图，再当作整页图片贴进 PDF —— 等价于真实扫描页。"""
    tmp = pymupdf.open()
    w, h = 595, 842  # A4 pt
    pg = tmp.new_page(width=w, height=h)
    y = 80
    for ln in lines:
        pg.insert_text((60, y), ln, fontname="cjk", fontfile=CJK_FONT, fontsize=14)
        y += 30
    pix = pg.get_pixmap(dpi=dpi_img)
    img_bytes = pix.tobytes("jpeg")
    tmp.close()
    page = pdf.new_page(width=w, height=h)
    page.insert_image(pymupdf.Rect(0, 0, w, h), stream=img_bytes)
    return page


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("out")
    ap.add_argument("--scan", type=int, default=2, help="前 N 页为扫描页")
    ap.add_argument("--text", type=int, default=3, help="后 M 页为文字层页")
    ap.add_argument("--watermark", action="store_true", help="扫描页叠加小文本层（页码水印）")
    ap.add_argument("--tag", default="ALPHA", help="正文关键词，便于断言是否真被抽出")
    a = ap.parse_args()

    pdf = pymupdf.open()
    for i in range(a.scan):
        lines = [
            f"扫描页 第 {i+1} 页  {a.tag}扫描段关键句{i+1}",
            f"{a.tag} 规范要求：所有条款必须在扫描件里被识别出来（编号 {i+1}）",
            "本条为纯图片内容，没有文字层，只能靠 OCR 读出。",
        ]
        pg = render_scan_page(pdf, lines)
        if a.watermark:
            # 真实扫描软件常写入的微量文本层：页码 + 品牌水印
            pg.insert_text((520, 820), f"第 {i+1} 页", fontname="cjk", fontfile=CJK_FONT, fontsize=9)
            pg.insert_text((60, 820), "扫描全能王 CamScanner", fontname="cjk", fontfile=CJK_FONT, fontsize=9)
    for i in range(a.text):
        pg = pdf.new_page(width=595, height=842)
        y = 80
        body = [
            f"文字层页 第 {a.scan+i+1} 页  {a.tag}可选段关键句{i+1}",
            f"{a.tag} 条款 {i+1}：本页有真实文字层，可直接解析，无需 OCR。",
            "如果这一页被送进 OCR，说明文本层检测失效。",
        ]
        for ln in body:
            pg.insert_text((60, y), ln, fontname="cjk", fontfile=CJK_FONT, fontsize=14)
            y += 30
        # 多写一些正文，模拟真实文档
        for k in range(12):
            pg.insert_text((60, y), f"{a.tag} 正文补充行 {k+1}：条款细节说明文字。",
                           fontname="cjk", fontfile=CJK_FONT, fontsize=11)
            y += 22
    pdf.save(a.out)
    pdf.close()
    print(f"wrote {a.out} scan={a.scan} text={a.text} watermark={a.watermark} size={os.path.getsize(a.out)}")


if __name__ == "__main__":
    main()
