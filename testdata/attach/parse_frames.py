#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""按标准 SSE 口径解析 chat 流：event: 行 + 紧跟的 data: 行。
只数我们要的证据：下发文件帧（event: file）、模板短路帧（type: template）、回答文本。"""
import json, sys, os


def frames(path):
    ev = None
    for line in open(path, encoding='utf-8', errors='replace'):
        line = line.rstrip('\n')
        if line.startswith('event: '):
            ev = line[7:].strip()
        elif line.startswith('data: '):
            raw = line[6:]
            try:
                o = json.loads(raw)
            except Exception:
                continue
            yield ev, o
        elif not line.strip():
            ev = None


def collect(path):
    files, texts, tmpl = [], [], []
    for ev, o in frames(path):
        objs = o if isinstance(o, list) else [o]
        for it in objs:
            if not isinstance(it, dict):
                continue
            t = it.get('type') or ev
            # 下发附件帧：event: file，data 里带 name/url（多份时按份发帧）
            if ev == 'file' or t == 'file':
                files.append({'name': it.get('name', ''), 'url': it.get('url', ''), 'kind': it.get('kind', '')})
            if t == 'template':
                tmpl.append(it)
            if t in ('text', 'answer', 'delta'):
                texts.append(str(it.get('data') or it.get('text') or '')[:300])
    return files, texts, tmpl


if __name__ == '__main__':
    files, texts, tmpl = collect(sys.argv[1])
    mode = sys.argv[2] if len(sys.argv) > 2 else 'name'
    if mode == 'name':
        print(f'  file 帧 {len(files)} 个：{[(f["name"], f["url"][:60]) for f in files]}')
        # 断言 1：点名了就必须真发出来
        n = len(files)
        print(('  ok   ' if n >= 1 else '  FAIL ') + f'点名模板 → 收到下发帧（{n} 个）')
        # 断言 2：名字要是清单原名
        ok2 = any('报名表' in f['name'] for f in files)
        print(('  ok   ' if ok2 else '  FAIL ') + f'文件名是清单原名：{[f["name"] for f in files]}')
        # 断言 3：每份都带可点 url（下发不能只说不给）
        ok3 = bool(files) and all(f['url'].startswith('/api/chat/attachment/') for f in files)
        print(('  ok   ' if ok3 else '  FAIL ') + '每份都带可点的下载 url')
    else:
        n = len(files)
        print(('  ok   ' if n == 0 else '  FAIL ') + f'没点名 → 下发帧 {n} 个（期望 0，不预判）')
    if texts:
        print('  —— 回答片段：' + texts[-1][:160].replace('\n', ' '))
