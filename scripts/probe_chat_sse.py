#!/usr/bin/env python3
"""抓一条原始 SSE：把 /api/chat 的每一帧原样落盘，用来回答「闸门为什么还在追问」。

只干一件事：发一轮（可选：先发素材轮），把 raw frames 写到 /tmp/probe_sse.txt。
不改产品代码、不落盘凭据。
用法：
  set -a; . /opt/skillforge/skillforge.env; set +a
  python3 -u scripts/probe_chat_sse.py            # 单轮：直接提写作需求
  MAT=/tmp/material_10k.txt python3 -u scripts/probe_chat_sse.py   # 两轮：先贴素材
"""
import hashlib
import json
import os
import time
import urllib.request

BASE = os.environ.get('BASE', 'http://127.0.0.1:8092')
U = os.environ.get('SKILLFORGE_ADMIN_USER', '')
P = os.environ.get('SKILLFORGE_ADMIN_PASS', '')
MAT = os.environ.get('MAT', '')
ASK = os.environ.get('ASK', '按上面素材里的【写作要求】，写一篇新闻通稿。')
OUT = os.environ.get('OUT', '/tmp/probe_sse.txt')


def post(path, obj, tok=None):
    req = urllib.request.Request(BASE + path, data=json.dumps(obj).encode(),
                                 headers={'Content-Type': 'application/json'})
    if tok:
        req.add_header('Authorization', 'Bearer ' + tok)
    return urllib.request.urlopen(req, timeout=600)


def fp(s):
    return '%d/%s' % (len(s or ''), hashlib.md5((s or '').encode()).hexdigest()[:8])


tok = json.loads(post('/api/login', {'username': U, 'password': P}).read())['token']
print('登录 ok（指纹 %s）' % fp(tok))

sid = 'probe-%d' % int(time.time())
log = open(OUT, 'w')

MAT_RAW = open(MAT).read() if MAT else ''
turns = []
if MAT_RAW:
    turns.append(('素材轮', MAT_RAW + '\n\n请按上面的【写作要求】写一篇新闻通稿。'))
turns.append(('需求轮', ASK))

for name, msg in turns:
    t0 = time.time()
    log.write('\n########## %s (%d 字) ##########\n' % (name, len(msg)))
    r = post('/api/chat', {'session_id': sid, 'message': msg}, tok)
    for raw in r:
        line = raw.decode('utf-8', 'replace').rstrip('\n')
        if line:
            log.write('[%6.2fs] %s\n' % (time.time() - t0, line))
    log.flush()
    print('%s 完成 %.1fs' % (name, time.time() - t0))

log.close()
print('sid=%s  原始帧 → %s' % (sid, OUT))
