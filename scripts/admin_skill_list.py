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
    # ⚠️ 必须同时看 id/slug 和 name：验收技能是**训练**出来的，它的 name 是模型起的标题
    # （如「数据治理专项公文起草」），标识垃圾只体现在 id/slug 上。上一版只看 name，
    # 结果「线上训练进度验收160916」被判成「没有要删的」——漏判比误删更隐蔽。
    # 收紧后的判据（2026-09-18 扩）：**slug 以「线上」开头** 且 含验收/取证/复验/leg/自证/实测 之一。
    # 为什么敢这么写：正式技能是用户自己起的名（办公文档管家 / 采购验收单 / 公积金办事…），
    # 没有一条以「线上」开头；而验收脚本建的技能一定带「线上 + 场景 + 时间戳」的签名。
    # 为什么这次要扩：这些残留不只是难看 —— 线上真发生过「把新闻稿整理成 Word」被路由到
    # 残留的 write 模式验收技能、再纠偏到 办公文档管家 的事（journal 20:53:29），
    # 等于多跑一遍决策。垃圾技能是会影响线上行为的，不是纯装饰。
    JUNK_MARK = ('验收', '取证', '复验', 'leg', '自证', '实测')
    hay = str(sid or '') + '\x00' + str(n or '')
    slug = str(sid or '')
    # 签名 = 「线上…」（验收脚本建的训练技能）或含「自证」（注入自证占位这类手工件）。
    signed = slug.startswith('线上') or ('自证' in slug) or ('实测' in slug)
    flag = ' [垃圾候选]' if (signed and any(k in hay for k in JUNK_MARK)) else ''
    if flag:
        junk.append((sid, n))
    print(f'  - {sid} | {n} | enabled={s.get("enabled")}{flag}')

if '--delete' in sys.argv:
    if not junk:
        print('没有要删的')
        sys.exit(0)
    print(f'本轮拟删 {len(junk)} 条（判据：slug 以「线上」开头且含验收/取证/复验/leg/自证/实测）')
    # 删除必须自证到磁盘：只看 HTTP 200 会漏掉「接口说删了、目录还在」这种半死状态。
    # 判据 = HTTP 200 **且** data/skills/<slug> 目录消失。任一不成立就非零退出。
    import os
    data_dir = os.environ.get('SKILLFORGE_DATA_DIR', '/opt/skillforge/data')
    bad = 0
    for sid, n in junk:
        p = os.path.join(data_dir, 'skills', str(sid))
        before = os.path.isdir(p)
        st, txt = call('DELETE', f'/api/admin/skills/{urllib.parse.quote(str(sid))}', token=tok)
        after = os.path.isdir(p)
        ok = (st == 200) and not after
        print(f'删除 {sid}（{n}）→ HTTP {st}｜磁盘目录 {before}→{after}｜{"OK" if ok else "!! 没删干净"} {txt[:60]}')
        if not ok:
            bad += 1
    print(f'删除完成：成功 {len(junk) - bad} / 失败 {bad}')
    sys.exit(1 if bad else 0)
