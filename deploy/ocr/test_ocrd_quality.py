#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""ocrd 文本层可读性（乱码）判据的自证型单测。

守的是什么事故：字体缺 ToUnicode 的 PDF，页面 get_text() 能吐出**够厚**的文本层，但内容
是 U+FFFD / 私用区编号的乱码。旧判据只看字数（len(txt) >= MIN_PAGE_CHARS），于是这种页
被「直取」，垃圾当正文喂给训练，chars 非 0、全链路绿灯，生成物与素材毫无关系。
本测试钉住三件事：
  1. 可选中且可读的 PDF 仍必须**零 OCR**（一次都不许调 _ocr_page）；
  2. 「够厚但乱码」的页必须回落 OCR，且乱码统计（garbled_pages/garbled_detail）如实回传；
  3. 薄文本层 + OCR 有货时仍采用 OCR（既有行为不回退）。

自证性：断言钉的是**页级 stats 数值 + 最终输出文本内容**，不是「函数被调过」。把实现里的
质量判据改成恒 True（等于退回旧逻辑）时，本文件必须变红（见文件末 __main__ 处的说明）。

OCR 全程打桩：不加载 RapidOCR 权重，也不依赖 ONNX。
运行：python3 -m pytest deploy/ocr/test_ocrd_quality.py -q   （或 python3 -m unittest）
"""
import os
import sys
import types
import unittest
from unittest import mock

import pymupdf

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

# ocrd 顶层就 import rapidocr_onnxruntime。权重打包在 PyInstaller 里时，直接跑 Python 可能
# 拿不到这个包 —— 但本测试全程打桩 _ocr_page，根本不需要推理引擎，所以缺了就注入空壳模块，
# 让「文本层判据」这一层照常被验证（断言不落在 ONNX 上）。
try:  # pragma: no cover - 环境相关
    import rapidocr_onnxruntime  # noqa: F401
except Exception:  # pragma: no cover - 环境相关
    _shim = types.ModuleType("rapidocr_onnxruntime")
    _shim.RapidOCR = object
    sys.modules["rapidocr_onnxruntime"] = _shim

import ocrd  # noqa: E402


# --------------------------------------------------------------------------
# 测试料构造（全部用 pymupdf 现造，不落任何二进制进仓库）
# --------------------------------------------------------------------------
# 乱码文本层的复现方式：给页面字体挂一个「错」的 ToUnicode 映射 —— 这正是线上
# 「字体缺/错 ToUnicode」的形态。
#   · GARBLE_FFFD: 映射到孤立代理项 U+D800，MuPDF 解码失败后吐 U+FFFD 替换符
#   · GARBLE_PUA : 映射到私用区 U+E001（真实缺 ToUnicode 的字体常只剩这类编号）
GARBLE_FFFD = ("D800", "\ufffd", "U+FFFD")
GARBLE_PUA = ("E001", "\ue001", "私用区")


def _raw_page_doc(target_hex: str, n_chars: int) -> "pymupdf.Document":
    """造一页「文本层字符由 ToUnicode 决定」的单页 PDF（用于复现乱码文本层）。"""
    def obj(n, body):
        return b"%d 0 obj\n" % n + body + b"\nendobj\n"

    content = b"BT /F1 12 Tf 40 780 Td (" + b"A" * n_chars + b") Tj ET"
    tounicode = (
        b"/CIDInit /ProcSet findresource begin\n12 dict begin\nbegincmap\n/CMapType 2 def\n"
        b"1 begincodespacerange\n<00> <FF>\nendcodespacerange\n"
        b"1 beginbfchar\n<41> <" + target_hex.encode() + b">\nendbfchar\nendcmap\n"
        b"CMapName currentdict /CMap defineresource pop\nend\nend"
    )
    objs = [
        obj(1, b"<< /Type /Catalog /Pages 2 0 R >>"),
        obj(2, b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
        obj(3, b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] "
               b"/Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>"),
        obj(4, b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica "
               b"/Encoding /WinAnsiEncoding /ToUnicode 6 0 R >>"),
        obj(5, b"<< /Length %d >>\nstream\n" % len(content) + content + b"\nendstream"),
        obj(6, b"<< /Length %d >>\nstream\n" % len(tounicode) + tounicode + b"\nendstream"),
    ]
    out = b"%PDF-1.7\n"
    offs = []
    for o in objs:
        offs.append(len(out))
        out += o
    xref = len(out)
    out += b"xref\n0 %d\n" % (len(objs) + 1) + b"0000000000 65535 f \n"
    for off in offs:
        out += b"%010d 00000 n \n" % off
    out += (b"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n"
            % (len(objs) + 1, xref))
    return pymupdf.open("pdf", out)


def _normal_page_doc(lines) -> "pymupdf.Document":
    """造一页正常中文文本页（pymupdf 内置 CJK 字体，无需外部字体文件）。"""
    doc = pymupdf.open()
    page = doc.new_page(width=595, height=842)
    y = 80
    for ln in lines:
        page.insert_text((50, y), ln, fontsize=14, fontname="china-s")
        y += 26
    return doc


def _lines(tag: str, idx: int) -> list:
    return ["%s 可选中正文第%d页 第%d行：本页有真实文本层，无需 OCR。" % (tag, idx, k)
            for k in range(1, 5)]


def _build_pdf(specs) -> bytes:
    """按顺序合并各页，返回 PDF 字节。

    specs 每项: ("text", lines) | ("garble", target_hex, 字符数)
    合并会让乱码页保留各自的 ToUnicode 映射（insert_pdf 逐页拷贝页面内容）。
    """
    out = pymupdf.open()
    for spec in specs:
        src = _normal_page_doc(spec[1]) if spec[0] == "text" else _raw_page_doc(spec[1], spec[2])
        out.insert_pdf(src, start_at=-1)  # start_at=-1 → 插到最前面，保持 specs 顺序
        src.close()
    data = out.tobytes()
    out.close()
    return data


def _page_texts(raw: bytes) -> list:
    doc = pymupdf.open("pdf", raw)
    texts = [(doc[i].get_text() or "").strip() for i in range(len(doc))]
    doc.close()
    return texts


class _OcrStub:
    """_ocr_page 的计数桩：记录被调用的页码，按页返回预设文本；绝不触碰真实权重。"""

    def __init__(self, by_page=None, default=""):
        self.by_page = dict(by_page or {})
        self.default = default
        self.calls = []

    def __call__(self, page, tmpdir, pno):
        self.calls.append(pno)
        return self.by_page.get(pno, self.default)


def _engine():
    # max_cache=0 → 不缓存，保证每条用例都是真解析（不回放上一次的 stats）
    return ocrd.OcrEngine(dpi=200, max_cache=0)


def _extract(raw, stub, name="x.pdf"):
    with mock.patch.object(ocrd.OcrEngine, "_ocr_page", stub):
        return _engine().extract(raw, name)


# --------------------------------------------------------------------------
# 判据函数本身
# --------------------------------------------------------------------------
class TestTextQuality(unittest.TestCase):
    def test_可读中文与英文页判为可读(self):
        for txt in ["第一页正文：本页有真实文本层，无需 OCR。\n可以直取。",
                    "Plain english text with numbers 12345 and\nnewlines."]:
            ok, why = ocrd._text_quality(txt)
            self.assertTrue(ok, "正常文本被误判为乱码: %r -> %r" % (txt, why))
            self.assertEqual(why, "")

    def test_空文本判为不可读(self):
        ok, why = ocrd._text_quality("")
        self.assertFalse(ok)
        self.assertIn("空", why)

    def test_替换符占比超阈值判乱码(self):
        ok, why = ocrd._text_quality("\ufffd" * 50)
        self.assertFalse(ok)
        self.assertIn("U+FFFD", why)

    def test_私用区占比超阈值判乱码(self):
        ok, why = ocrd._text_quality("\ue001\ue123\uf8ff" * 20)
        self.assertFalse(ok)
        self.assertIn("私用区", why)

    def test_控制字符占比超阈值判乱码(self):
        ok, why = ocrd._text_quality("\x01\x02\x03" * 20)
        self.assertFalse(ok)
        self.assertIn("控制字符", why)
        # \t\n\r 是正常排版字符，不该被算成控制信号
        ok2, _ = ocrd._text_quality("第一行正文内容\n第二行正文内容\t第三行\r\n第四行正文内容")
        self.assertTrue(ok2)

    def test_可读占比过低判乱码(self):
        # 符号表混杂：可读字符不到 60%
        ok, why = ocrd._text_quality("\u25a0\u25b2\u2605\u2606\u2665\u2666" * 10 + "abc")
        self.assertFalse(ok)
        self.assertIn("可读字符占比", why)

    def test_阈值留有余量不误伤(self):
        # 200 字里 3 个替换符 = 1.5%，不超 2% 阈值，不该让整页回落 OCR（偶发坏字形容忍）
        ok, why = ocrd._text_quality("正" * 197 + "\ufffd" * 3)
        self.assertTrue(ok, "1.5%% 的替换符不该判乱码: %r" % why)
        # 200 字里 6 个 = 3%，超阈值，必须判乱码（阈值是 >2%，不是「有一点就算」）
        ok2, why2 = ocrd._text_quality("正" * 194 + "\ufffd" * 6)
        self.assertFalse(ok2)
        self.assertIn("U+FFFD", why2)


# --------------------------------------------------------------------------
# 逐页判定端到端（走 public extract）
# --------------------------------------------------------------------------
class TestPdfPerPageQuality(unittest.TestCase):
    def test_a_全可读PDF零OCR(self):
        raw = _build_pdf([("text", _lines("TEXTLINE", i + 1)) for i in range(3)])
        page_texts = _page_texts(raw)
        self.assertEqual(len(page_texts), 3)
        for t in page_texts:  # 自证测试料本身够厚且可读，否则本条测的不是「直取」路径
            self.assertGreaterEqual(len(t), ocrd.MIN_PAGE_CHARS)
            self.assertTrue(ocrd._text_quality(t)[0], "测试料本身不可读: %r" % t[:40])

        stub = _OcrStub(default="桩不该被调用")
        fmt, text, chars, cached, stats = _extract(raw, stub)

        self.assertEqual(fmt, "pdf")
        self.assertEqual(stats["pages"], 3)
        self.assertEqual(stats["text_pages"], 3)
        self.assertEqual(stats["ocr_pages"], 0)
        self.assertEqual(stats["empty_pages"], 0)
        self.assertEqual(stats["garbled_pages"], 0)
        self.assertEqual(stats["garbled_detail"], [])
        # 钉住「一次都没调」而不是「调了没用」
        self.assertEqual(stub.calls, [], "可选中且可读的 PDF 不该走 OCR")
        self.assertIn("TEXTLINE 可选中正文第1页", text)
        self.assertIn("TEXTLINE 可选中正文第3页", text)
        self.assertEqual(ocrd.parse_warning(stats, chars), "")

    def test_b_够厚但乱码的页回落OCR并统计(self):
        n = ocrd.MIN_PAGE_CHARS + 20
        raw = _build_pdf([
            ("garble", GARBLE_FFFD[0], n),   # 第1页：够厚但全是 U+FFFD
            ("text", _lines("GOODPAGE", 2)),  # 第2页：正常可读
            ("garble", GARBLE_PUA[0], n),    # 第3页：够厚但全是私用区
        ])
        page_texts = _page_texts(raw)
        # 自证测试料：1、3 页确实「够厚」且确实是乱码，2 页确实可读
        self.assertGreaterEqual(len(page_texts[0]), ocrd.MIN_PAGE_CHARS)
        self.assertGreaterEqual(len(page_texts[2]), ocrd.MIN_PAGE_CHARS)
        self.assertFalse(ocrd._text_quality(page_texts[0])[0])
        self.assertFalse(ocrd._text_quality(page_texts[2])[0])
        self.assertTrue(ocrd._text_quality(page_texts[1])[0])

        ocr_p1 = "OCR 恢复第一页正文：扫描段关键句一。扫描件内容只能靠 OCR 读出。"
        ocr_p3 = "OCR 恢复第三页正文：扫描段关键句三。扫描件内容只能靠 OCR 读出。"
        stub = _OcrStub(by_page={0: ocr_p1, 2: ocr_p3})
        fmt, text, chars, cached, stats = _extract(raw, stub, "mixed_garbled.pdf")

        self.assertEqual(stats["pages"], 3)
        self.assertEqual(stats["text_pages"], 1, "只有第2页该直取文本层")
        self.assertEqual(stats["ocr_pages"], 2, "两页乱码文本层都必须回落 OCR")
        self.assertEqual(stats["garbled_pages"], 2)
        self.assertEqual(stub.calls, [0, 2])
        self.assertEqual(len(stats["garbled_detail"]), 2)
        self.assertIn("第1页", stats["garbled_detail"][0])
        self.assertIn("U+FFFD", stats["garbled_detail"][0])
        self.assertIn("第3页", stats["garbled_detail"][1])
        self.assertIn("私用区", stats["garbled_detail"][1])
        # 页数守恒：1 直取 + 2 OCR + 0 空 = 3
        self.assertEqual(stats["text_pages"] + stats["ocr_pages"] + stats["empty_pages"], 3)
        # 最终文本吃的是 OCR 结果，乱码字符一个都不许漏进输出
        self.assertIn(ocr_p1, text)
        self.assertIn(ocr_p3, text)
        self.assertIn("GOODPAGE 可选中正文第2页", text)
        self.assertNotIn("\ufffd", text)
        self.assertNotIn("\ue001", text)
        # 上层能据 warning 决策
        warn = ocrd.parse_warning(stats, chars)
        self.assertIn("2 页文本层疑似乱码", warn)
        self.assertIn("字体缺 ToUnicode", warn)
        self.assertIn("已改用 OCR 或保留原文", warn)

    def test_c_薄文本层仍采用OCR结果(self):
        raw = _build_pdf([("text", ["第 3 页"])])  # 只有 5 个字，不够厚
        self.assertLess(len(_page_texts(raw)[0]), ocrd.MIN_PAGE_CHARS)

        ocr_txt = ("扫描页正文：本条为纯图片内容，没有文字层，只能靠 OCR 读出。"
                   "条款细节说明文字，长度远超薄文本层。")
        stub = _OcrStub(by_page={0: ocr_txt})
        fmt, text, chars, cached, stats = _extract(raw, stub, "thin.pdf")

        self.assertEqual(stub.calls, [0])
        self.assertEqual(stats["pages"], 1)
        self.assertEqual(stats["ocr_pages"], 1, "薄文本层页必须走 OCR 且采用 OCR 结果")
        self.assertEqual(stats["text_pages"], 0)
        self.assertEqual(stats["garbled_pages"], 0, "薄但可读的水印不属于乱码")
        self.assertEqual(stats["garbled_detail"], [])
        self.assertIn(ocr_txt, text)
        self.assertNotIn("第 3 页", text, "薄文本层不该与 OCR 结果并存(会重复计数)")

    def test_d_OCR失败时乱码页保留原文绝不丢页(self):
        n = ocrd.MIN_PAGE_CHARS + 20
        raw = _build_pdf([("garble", GARBLE_FFFD[0], n)])
        stub = _OcrStub(default="")  # OCR 彻底失败
        fmt, text, chars, cached, stats = _extract(raw, stub, "garbled_ocrdead.pdf")

        self.assertEqual(stub.calls, [0])
        self.assertEqual(stats["text_pages"], 1, "OCR 失败也要兜住这一页，不许丢")
        self.assertEqual(stats["ocr_pages"], 0)
        self.assertEqual(stats["empty_pages"], 0)
        self.assertEqual(stats["garbled_pages"], 1)
        self.assertEqual(len(text.strip()), n, "应原样保留乱码文本层而不是空页")
        self.assertIn("U+FFFD", stats["garbled_detail"][0])

    def test_e_OCR结果同样不可读时保留文本层(self):
        n = ocrd.MIN_PAGE_CHARS + 20
        raw = _build_pdf([("garble", GARBLE_PUA[0], n)])
        stub = _OcrStub(default="\ufffd" * 60)  # OCR 也吐乱码
        fmt, text, chars, cached, stats = _extract(raw, stub, "ocr_garbled_too.pdf")

        self.assertEqual(stub.calls, [0])
        self.assertEqual(stats["text_pages"], 1, "OCR 结果不可读时退回文本层兜底")
        self.assertEqual(stats["ocr_pages"], 0)
        self.assertEqual(stats["empty_pages"], 0)
        self.assertEqual(stats["garbled_pages"], 1)


class TestServiceSurface(unittest.TestCase):
    """版本串与 /health 字段：CI/上层要能从外部断言线上跑的是这一版规则。"""

    def test_版本与判据串(self):
        self.assertEqual(ocrd.VERSION, "ocrd-v5-quality")
        self.assertEqual(ocrd.QUALITY_RULE, "fffd_or_pua>2%|readable<60%")

    def test_health_回报quality_rule(self):
        sent = {}

        class _H(ocrd.Handler):
            def __init__(self):
                self.path = "/health"

            def _send_json(self, code, obj):
                sent["code"], sent["obj"] = code, obj

        _H().do_GET()
        self.assertEqual(sent["code"], 200)
        self.assertEqual(sent["obj"]["version"], "ocrd-v5-quality")
        self.assertEqual(sent["obj"]["quality_rule"], "fffd_or_pua>2%|readable<60%")
        self.assertEqual(sent["obj"]["min_page_chars"], ocrd.MIN_PAGE_CHARS)


if __name__ == "__main__":
    unittest.main(verbosity=2)
