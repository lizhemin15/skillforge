#!/usr/bin/env python3
"""列出线上技能（只看验收/实测类垃圾），并可选删除。

用法：
  python3 scripts/admin_skill_list.py            # 只列
  python3 scripts/admin_skill_list.py --delete   # 删掉名字含 验收/实测 的（走 API）

凭据从环境变量 SKILLFORGE_ADMIN_USER / SKILLFORGE_ADMIN_PASS 走，不落盘、不打印。
"""
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

BASE = os.environ.get('BASE', 'http://127.0.0.1:8092')
U = os.environ.get('SKILLFORGE_ADMIN_USER', '')
P = os.environ.get('SKILLFORGE_ADMIN_PASS', '')


def call(method, path, body=None, token=None):
    req = urllib.request.Request(BASE + path, method=method)
    req.add_header('Content-Type', 'application/json')
    if token:
        req.add_header('Authorization', 'Bearer ' + token)
    data = json.dumps(body).encode() if body is not None else None
    try:
        with urllib.request.urlopen(req, data, timeout=30) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


st, txt = call('POST', '/api/login', {'username': U, 'password': P})
if st != 200:
    print(f'登录失败 HTTP {st}：{txt[:200]}')
    sys.exit(1)
tok = json.loads(txt).get('token') or json.loads(txt).get('access_token')
if not tok:
    print(f'登录回执里没有 token 字段，实际字段：{list(json.loads(txt).keys())}')
    sys.exit(1)
print(f'登录 OK（token {len(tok)} 字符）')

st, txt = call('GET', '/api/admin/skills', token=tok)
if st != 200:
    print(f'列技能失败 HTTP {st}：{txt[:200]}')
    sys.exit(1)
d = json.loads(txt)
items = d if isinstance(d, list) else (d.get('skills') or d.get('items') or [])
print(f'线上技能总数：{len(items)}')
junk = []
for s in items:
    n = s.get('name', '')
    sid = s.get('id') or s.get('slug')
    # 判据必须收紧到「我们自己的验收产物名」，不能用「名字含 验收/测试」这种宽匹配 ——
    # 线上真有一个用户的正式技能叫「采购验收单」，宽匹配会把真数据当垃圾删掉。
    flag = ' [垃圾候选]' if any(k in n for k in ('线上训练进度验收', '线上混合素材验收', '实测废件')) else ''
    if flag:
        junk.append((sid, n))
    print(f'  - {sid} | {n} | enabled={s.get("enabled")}{flag}')

if '--delete' in sys.argv:
    if not junk:
        print('没有要删的')
        sys.exit(0)
    for sid, n in junk:
        st, txt = call('DELETE', f'/api/admin/skills/{urllib.parse.quote(str(sid))}', token=tok)
        print(f'删除 {sid}（{n}）→ HTTP {st} {txt[:80]}')
