#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""用生产 ocrd 服务（127.0.0.1:8093 /extract）验证扫描版 PDF 的 OCR 可读性。"""
import re, json, pathlib, urllib.request, difflib, sys

D = pathlib.Path('/root/skillforge/testdata/manual')
PDF = D / 'manual-scan.pdf'
OUT = D / 'manual-ocr.txt'
OCRURL = 'http://127.0.0.1:8093/extract'

def post_extract(path):
    boundary = '----manualscan'
    body = b''
    body += f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="{path.name}"\r\nContent-Type: application/pdf\r\n\r\n'.encode()
    body += path.read_bytes() + f'\r\n--{boundary}--\r\n'.encode()
    req = urllib.request.Request(OCRURL, data=body, headers={'Content-Type': f'multipart/form-data; boundary={boundary}'})
    with urllib.request.urlopen(req, timeout=600) as r:
        return json.loads(r.read().decode('utf-8'))

# 源文本 = 实际渲染进 PDF 的正文（front 去掉目录段）
front = (D / 'front.md').read_text(encoding='utf-8')
front = front[:front.index('## 目　录')] + front[front.index('# 第一章'):]
src = '\n'.join([front] + [(D / f).read_text(encoding='utf-8') for f in ['part1.md','part2.md','part3.md','tail.md']])

res = post_extract(PDF)
if not res.get('ok'):
    print('OCR 失败:', res.get('error')); sys.exit(1)
ocr = res['text']
OUT.write_text(ocr, encoding='utf-8')

cjk = lambda s: ''.join(re.findall(r'[\u4e00-\u9fff]', s))
a, b = cjk(src), cjk(ocr)
ratio = difflib.SequenceMatcher(None, a, b).ratio()
print(f'源汉字数 {len(a)} | OCR 汉字数 {len(b)} | 序列相似度 {ratio:.4f}')

KEY = ['企业新闻稿写作手册', '第十四章', '危机回应与澄清声明', '战略合作与签约仪式',
       '标题字数是否不超过', '关于公司简介段' if False else '关于段',
       '远航智能成立于', '星澜科技成立于', '首席技术官', '第三方物流',
       '倒金字塔', '审稿人出具结论时', '修改轮次上限']
oc = cjk(ocr)
print('== 关键内容抽查（OCR 文本内检索，忽略非汉字噪声）')
miss = []
for k in KEY:
    hit = cjk(k) in oc
    print(f'  [{"OK " if hit else "MISS"}] {k}')
    if not hit: miss.append(k)
# 章节覆盖
print('== 十二章标题覆盖')
for ch in re.findall(r'^#+\s+(第.+?章[^\n]*)', src, re.M):
    name = cjk(ch)
    print(f'  [{"OK " if name in oc else "MISS"}] {ch}')
    if name not in oc: miss.append(ch)
print('\n未命中:', miss if miss else '无')
print('OCR 文本已存:', OUT)
sys.exit(0 if ratio > 0.95 and not miss else 1)
