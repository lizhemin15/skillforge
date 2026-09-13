#!/usr/bin/env python3
"""生成「扫描件」测试料 —— 用于在干净容器里验证 ocrd 真的能 OCR（而不是只起了个 HTTP 服务）。

为什么不能只测文本层：
  ocrd 对带文本层的 PDF 会走「直接抽取、零 OCR」的快路径 —— 那条路径**根本不碰 ONNX**。
  要证明 OCR 引擎（PP-OCRv4 det+rec+cls 三个 .onnx 模型）在目标机器上真能跑，
  必须喂一个**纯图像 PDF**（无文本层），逼它走推理。

只用 pymupdf 生成（构建镜像里已有），不需要字体文件、不需要 PIL：
  1. 造一页带中文+数字的文本页，用 pymupdf 内置 CJK 字体
  2. 渲染成位图，再把位图塞进一页新的空 PDF → 这一步后 PDF 里就没有文本层了
"""
import sys

import pymupdf

LINE = "离线部署自检 SKILLFORGE 2026 端口 8093"


def main(out_path: str) -> None:
    src = pymupdf.open()
    page = src.new_page(width=595, height=300)  # A4 宽
    # china-s = 简体中文内置字体；数字字形也在（旧版踩过「字体没数字字形 → OCR 出方块」的坑）
    page.insert_text((60, 90), LINE, fontsize=26, fontname="china-s")
    page.insert_text((60, 150), "OCR DEPLOY CHECK 12345", fontsize=22, fontname="helv")
    pix = page.get_pixmap(dpi=200)

    out = pymupdf.open()
    p = out.new_page(width=src[0].rect.width, height=src[0].rect.height)
    p.insert_image(src[0].rect, pixmap=pix)
    out.save(out_path)
    src.close()
    out.close()

    # 自证：产物必须**没有文本层**，否则测的就不是 OCR 路径（这行断言很关键）
    chk = pymupdf.open(out_path)
    got = chk[0].get_text().strip()
    chk.close()
    if got:
        print("FAIL: 生成的 fixture 仍带文本层，无法验证 OCR 推理路径: %r" % got[:60], file=sys.stderr)
        sys.exit(1)
    print("fixture ok -> %s (no text layer, forces OCR inference)" % out_path)


if __name__ == "__main__":
    main(sys.argv[1] if len(sys.argv) > 1 else "scan.pdf")
