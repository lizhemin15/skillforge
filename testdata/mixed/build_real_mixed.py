#!/usr/bin/env python3
"""造「仿真实生产」混合 PDF：前 SCAN_N 页是扫描页（叠薄水印文本层），其余页可选中。

用途：复现用户投诉 ③——上传此类文件后生成的 skill 与素材无关。
判据来源：deploy/ocr/verify_mixed_pdf.sh 注释描述的现场（扫描软件写入 ≤20 字水印层 →
旧逻辑「文本层非空就直取」→ 真扫描页一页都不送 OCR → 整本只剩水印 → chars 非 0 全链路绿灯）。

关键实现点（踩过的坑）：
- 扫描页不能直接 insert_pdf 转存：源件里的图片流会被原样搬，产出 384MB（超上传上限）。
  正解是渲染成灰阶像素后按 JPEG 编码插入 → 每页几百 KB。
- 水印中文必须用中文字体（wqy-zenhei.ttc）。用 helv 会渲染成一串 '·' 方块字符，
  既不像真水印，也会被 ocrd 的质量规则当「乱码页」判走 OCR 通道。
"""
import os
import sys

import pymupdf
from PIL import Image
import io

VECTOR = "testdata/manual/manual-vector.pdf"   # 可选中版（文字层完好）
SCAN = "testdata/manual/manual-scan.pdf"       # 扫描版（图片，无文字层）
OUT = sys.argv[1] if len(sys.argv) > 1 else "/tmp/mixed_real.pdf"
SCAN_N = int(sys.argv[2]) if len(sys.argv) > 2 else 6
DPI = int(sys.argv[3]) if len(sys.argv) > 3 else 200
WM_FONT = "/usr/share/fonts/truetype/wqy/wqy-zenhei.ttc"

out = pymupdf.open()
src = pymupdf.open(SCAN)
vec = pymupdf.open(VECTOR)
total = min(vec.page_count, src.page_count)

for i in range(total):
    if i < SCAN_N:
        # 扫描页：把源页栅格化成灰阶 JPEG 当整页图片（模拟扫描件）
        pm = src[i].get_pixmap(dpi=DPI, colorspace=pymupdf.csGRAY)
        img = Image.frombytes("L", (pm.width, pm.height), pm.samples)
        buf = io.BytesIO()
        img.save(buf, "JPEG", quality=82, optimize=True)
        page = out.new_page(width=src[i].rect.width, height=src[i].rect.height)
        page.insert_image(page.rect, stream=buf.getvalue(), keep_proportion=False)
    else:
        out.insert_pdf(vec, from_page=i, to_page=i)   # 文字页：带完好文字层

# 给扫描页叠薄水印文本层：有字，但远薄于正文（旧逻辑会被它骗过 → 该页永不送 OCR）
for i in range(min(SCAN_N, out.page_count)):
    p = out[i]
    r = p.rect
    # fontname 必须和 fontfile 一起给：只给 fontfile 时 PyMuPDF 回落 helv，
    # 中文会渲染成一串 '·'（假水印），就不算复现真实扫描仪行为了。
    p.insert_text((r.x0 + 40, r.y0 + 30), "品牌传播部  内部资料  第%d页" % (i + 1),
                  fontname="cjk", fontfile=WM_FONT, fontsize=7, color=(0.55, 0.55, 0.55))

out.save(OUT, garbage=4, deflate=True, deflate_images=True, clean=True)
size_mb = os.path.getsize(OUT) / 1e6
print("OUT=%s pages=%d scan_pages=%d text_pages=%d size=%.1fMB"
      % (OUT, out.page_count, SCAN_N, out.page_count - SCAN_N, size_mb))

# 自检：前提必须成立，否则白测 ——「扫描页文字层只有水印那么薄」「文字页文字层够厚」
print("--- 前提自检 ---")
thin_ok = True
for i in range(min(SCAN_N, out.page_count)):
    t = out[i].get_text().strip()
    flag = "薄（旧逻辑会踩坑）" if len(t) < 40 else "厚，不像扫描页！"
    thin_ok = thin_ok and len(t) < 40
    print("  scan p%d 文字层=%d 字 %r → %s" % (i + 1, len(t), t[:26], flag))
for i in range(SCAN_N, min(SCAN_N + 2, out.page_count)):
    t = out[i].get_text().strip()
    print("  text p%d 文字层=%d 字 %r" % (i + 1, len(t), t[:34].replace("\n", "/")))
out.close()
print("前提成立" if thin_ok else "前提不成立：扫描页文字层过厚")
sys.exit(0 if thin_ok else 2)
