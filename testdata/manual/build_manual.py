#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""把 manual/*.md 排成 A4 矢量 PDF，再"扫描化"成图片版 PDF（模拟扫描件）。
两遍排版：第一遍拿到各章实际页码，回填目录页码后重排。
"""
import re, json, pathlib, shutil
import fitz
import numpy as np
from PIL import Image

D = pathlib.Path('/root/skillforge/testdata/manual')
OUT_VEC = D / 'manual-vector.pdf'
OUT_SCAN = D / 'manual-scan.pdf'
IMGDIR = D / '.scan-pages'

SONG = '/usr/share/fonts/truetype/arphic-gbsn00lp/gbsn00lp.ttf'          # 宋体（正文）
HEI = '/usr/share/fonts/truetype/wqy/wqy-zenhei.ttc'   # 文泉驿正黑：CJK+数字全覆盖（DroidSansFallback 无数字字形，会把编号渲染成方块）

W, H = 595.28, 841.89
ML, MR, MT, MB = 62.0, 62.0, 72.0, 68.0
TW = W - ML - MR
TH = H - MT - MB
FS_BODY, LEAD, PARA_GAP, INDENT_CH = 10.3, 18.15, 3.2, 2
FS_CH, FS_SEC = 15.0, 11.6
NOPRINT_LEAD = '，。、；：？！）】》」』%”·…—'

# ---------- 解析 markdown ----------
def parse(path):
    blocks, buf = [], []
    for raw in path.read_text(encoding='utf-8').splitlines():
        line = raw.rstrip()
        if re.match(r'^#{1,2}\s+第.+章', line):
            if buf: blocks.append(('p', ' '.join(buf))); buf = []
            blocks.append(('ch', re.sub(r'^#+\s+', '', line)))
        elif re.match(r'^###\s+', line):
            if buf: blocks.append(('p', ' '.join(buf))); buf = []
            blocks.append(('sec', re.sub(r'^###\s+', '', line)))
        elif re.match(r'^#{1,2}\s+', line):          # 附则等
            if buf: blocks.append(('p', ' '.join(buf))); buf = []
            blocks.append(('sec', re.sub(r'^#+\s+', '', line)))
        elif line.strip() == '---':
            if buf: blocks.append(('p', ' '.join(buf))); buf = []
            blocks.append(('pagebreak', ''))
        elif not line.strip():
            if buf: blocks.append(('p', ' '.join(buf))); buf = []
        else:
            buf.append(line.strip())
    if buf: blocks.append(('p', ' '.join(buf)))
    return blocks

font_song = fitz.Font(fontfile=SONG)
font_hei = fitz.Font(fontfile=HEI)

def wrap(text, size, first_indent):
    """按宽度折行，带最小避头尾：不在行首放收尾标点。"""
    out, cur, avail = [], '', TW - (first_indent * size)
    i = 0
    while i < len(text):
        ch = text[i]
        w = font_song.text_length(ch, fontsize=size)
        if font_song.text_length(cur, fontsize=size) + w > avail and cur:
            if ch in NOPRINT_LEAD:            # 收尾标点挤在行末
                cur += ch; i += 1
            out.append(cur); cur, avail = '', TW; continue
        cur += ch; i += 1
    if cur: out.append(cur)
    return out

class Layout:
    def __init__(self):
        self.pages = []
        self.y = MT
        self.chapter_pages = {}
        self._new_page()

    def _new_page(self):
        self.pages.append([])
        self.y = MT

    def room(self, n):
        return self.y + n * LEAD <= H - MB

    def add_line(self, s, size, x, fontname, dy=None):
        self.pages[-1].append((x, self.y + (dy or (size * 0.86)), s, fontname, size))
        self.y += LEAD

    def emit(self, blocks, toc_pages=None):
        for kind, text in blocks:
            if kind == 'pagebreak':
                self._new_page(); continue
            if kind == 'ch':
                self._new_page()
                self.chapter_pages[text] = len(self.pages)
                self.y += 26
                self.add_line(text, FS_CH, 0, 'hei', dy=FS_CH * 0.9)
                self.y += 14
                continue
            if kind == 'sec':
                need = LEAD * 2
                if not self.room(2): self._new_page()
                self.y += 9
                self.add_line(text, FS_SEC, 0, 'hei', dy=FS_SEC * 0.88)
                self.y += 3
                continue
            if kind == 'toc':
                for item, pg in text:
                    if not self.room(1): self._new_page()
                    label, num = item
                    base = label if isinstance(label, str) else label
                    dots = '.' * max(2, int((TW - 40 - font_song.text_length(base, fontsize=FS_BODY)) / (FS_BODY * 0.30)))
                    self.pages[-1].append((ML, self.y + FS_BODY * 0.86, f'{base}  {dots}  {pg}', 'song', FS_BODY))
                    self.y += LEAD
                continue
            # 段落
            lines = wrap(text, FS_BODY, INDENT_CH)
            for k, ln in enumerate(lines):
                if not self.room(1):
                    self._new_page()
                x = ML + (FS_BODY * INDENT_CH if k == 0 else 0)
                self.pages[-1].append((x, self.y + FS_BODY * 0.86, ln, 'song', FS_BODY))
                self.y += LEAD
            self.y += PARA_GAP

def render(layout, path):
    doc = fitz.open()
    for idx, ops in enumerate(layout.pages, 1):
        page = doc.new_page(width=W, height=H)
        page.insert_font(fontname='song', fontfile=SONG)
        page.insert_font(fontname='hei', fontfile=HEI)
        for x, y, s, fn, size in ops:
            page.insert_text(fitz.Point(ML + x, y), s, fontname=fn, fontsize=size, color=(0.08, 0.08, 0.09))
        if idx == 1:   # 封面不排页眉页码
            continue
        page.insert_text(fitz.Point(ML, MT - 26), '企业新闻稿写作手册（内部资料·第二版）',
                         fontname='song', fontsize=8.2, color=(0.45, 0.45, 0.47))
        pn = f'— {idx} —'
        page.insert_text(fitz.Point(W / 2 - font_song.text_length(pn, fontsize=9) / 2, H - 46),
                         pn, fontname='song', fontsize=9, color=(0.25, 0.25, 0.27))
    doc.save(str(path))
    return doc.page_count

# ---------- 封面 ----------
FRONT = D / 'front.md'
raw_front = FRONT.read_text(encoding='utf-8')
toc_start = raw_front.index('## 目　录')
toc_end = raw_front.index('# 第一章')
body_front = raw_front[raw_front.index('# 第一章'):]   # 封面/目录由程序生成，正文从第一章开始
tmp_front = D / '.front-body.md'
tmp_front.write_text(body_front, encoding='utf-8')

blocks_front = parse(tmp_front)
blocks_all = parse(D / 'part1.md') + parse(D / 'part2.md') + parse(D / 'part3.md') + parse(D / 'tail.md')

# 目录条目：章一律列出；小节只对"实义"章节（第一章/第十四章）展开，
# 其余类目章节的小节结构完全一致（适用范围/要求/模板/错误/范文），列出来是冗余。
toc_items = []
for kind, text in blocks_front + blocks_all:
    if kind == 'ch':
        toc_items.append((text, None))
    elif kind == 'sec' and re.match(r'^(1|14)\.\d+', text):
        toc_items.append(('　　' + text, None))

def build(toc_pages):
    lay = Layout()
    # 封面（居中排版，第 1 页不排页眉页码）
    cover = [('企业新闻稿写作手册', 'hei', 30.0, 250),
             ('（内部资料 · 第二版）', 'song', 14.0, 300),
             ('品牌传播部  编写', 'song', 12.0, 372),
             ('公司新闻发布审核委员会  审定', 'song', 12.0, 400),
             ('二〇二六年三月', 'song', 12.0, 470)]
    for text, fn, size, yy in cover:
        f = font_hei if fn == 'hei' else font_song
        lay.pages[-1].append(((TW - f.text_length(text, fontsize=size)) / 2, yy, text, fn, size))
    lay.chapter_pages['企业新闻稿写作手册'] = 1
    lay._new_page()
    lay.y += 40
    lay.add_line('目　　录', FS_CH, 0, 'hei', dy=FS_CH * 0.9)
    lay.y += 20
    lay.emit([('toc', list(zip(toc_items, [toc_pages.get(t[0], '') if t[1] is None else '' for t in toc_items])))])
    lay.emit(blocks_front)
    lay.emit(blocks_all)
    return lay

lay1 = build({})
n1 = render(lay1, OUT_VEC)
ch_pages = lay1.chapter_pages
lay2 = build(ch_pages)
n2 = render(lay2, OUT_VEC)
lay3 = build(lay2.chapter_pages)
n3 = render(lay3, OUT_VEC)
print(f'矢量 PDF 页数: pass1={n1} pass2={n2} pass3={n3}')

# ---------- 扫描化 ----------
if IMGDIR.exists(): shutil.rmtree(IMGDIR)
IMGDIR.mkdir()
doc = fitz.open(str(OUT_VEC))
rng = np.random.default_rng(20260913)
files = []
for i, page in enumerate(doc, 1):
    pix = page.get_pixmap(dpi=240, colorspace=fitz.csGRAY, annots=False)
    img = Image.frombytes('L', (pix.width, pix.height), pix.samples)
    arr = np.array(img).astype(np.int16)
    arr = arr + rng.normal(0, 2.0, arr.shape)                      # 扫描噪点
    arr = np.clip(arr, 0, 255).astype(np.uint8)
    out = IMGDIR / f'p{i:03d}.jpg'
    Image.fromarray(arr).save(out, 'JPEG', quality=85)
    files.append(out)
print(f'扫描页图片: {len(files)} 张，{files[0].stat().st_size//1024}KB/页 级别')

sdoc = fitz.open()
for f in files:
    pg = sdoc.new_page(width=W, height=H)
    pg.insert_image(fitz.Rect(0, 0, W, H), filename=str(f))
sdoc.save(str(OUT_SCAN))
print(f'扫描版 PDF: {OUT_SCAN}  页数={sdoc.page_count}  大小={OUT_SCAN.stat().st_size//1024}KB')
(D / 'build-report.json').write_text(json.dumps({'pages': sdoc.page_count, 'chapter_pages': ch_pages}, ensure_ascii=False, indent=1), encoding='utf-8')
