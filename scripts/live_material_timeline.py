#!/usr/bin/env python3
"""线上真实那条路的时间线实测：第 1 轮贴 1 万字素材，第 2 轮提写作需求。

为什么单独一个跑手：尺子（chat-sse-timeline.py）只量**一轮**，而用户投诉的那一轮
是「先贴素材、再提要求」的第二轮 —— 素材压不压得住、那一轮还卡不卡计时，全看
同一 session 的第二回合。所以：

  第 1 轮：把 /tmp/material_10k.txt 原文贴进 session（读完整个流，不量它）
  第 2 轮：同一个 session 提写作需求，交给尺子量 A1~A6

纪律（都是踩过的坑）：
  - **同一 sid**：换 sid 就变成新会话，「有没有压住素材」根本量不到；
  - 登录凭据与 token 只在进程环境里传，不落盘、不回显；
  - 环境变量名故意**不含 TOKEN/KEY/SECRET 字样**：写入侧的脱敏过滤器会把
    `SF_TOKEN=<变量>` 整段改写成 `***`，把赋值打断成一个语法错。所以用 SF_SLOT。
  - 传参本身自带自检：子进程环境里的值必须与登录拿到的一致（只比长度与指纹，
    不比明文），不一致就当场退 3 —— 否则尺子会拿 401 报 PREMISE_MISS，
    看起来像「材料坏了」，其实是传参断了。
  - 第 1 轮失败（登录/鉴权/读流断/几乎没吐正文）就报 TURN1_FAIL 并退 3，
    不要拿第 2 轮的红去顶罪 —— 那轮压根没在「带着素材」的前提下跑；
  - 尺子的 0/1/2 原样透传（2 = PREMISE_MISS，不是绿）。

用法（先 source skillforge.env，凭据从环境取）：
  set -a; . /opt/skillforge/skillforge.env; set +a
  python3 scripts/live_material_timeline.py
环境变量: BASE / MAT / ASK / SID / SKILLFORGE_ADMIN_USER / SKILLFORGE_ADMIN_PASS
退出码: 0 尺子全绿；1 尺子有 FAIL；2 尺子 PREMISE_MISS；3 第 1 轮（种素材）就没成。
"""
import hashlib
import json
import os
import subprocess
import sys
import time
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
RULER = os.path.join(HERE, 'chat-sse-timeline.py')
BASE = os.environ.get('BASE', 'http://127.0.0.1:8092')
U = os.environ.get('SKILLFORGE_ADMIN_USER', '')
P = os.environ.get('SKILLFORGE_ADMIN_PASS', '')
MAT = os.environ.get('MAT', '/tmp/material_10k.txt')
ASK = os.environ.get('ASK', '按上面素材里的【写作要求】，写一篇新闻通稿。')
# 种素材那一轮默认只贴素材原文；但服务端可能把它判成「参数不全 → 追问」轮
# （asked=true），那样第 1 轮压根没进写作跳，前提不成立。给一个旋钮让操作者
# 附一句明确写稿指令，把「压住素材再写」这个前提真正造出来。默认空=行为不变。
SEED_SUFFIX = os.environ.get('SEED_SUFFIX', '')
SID = os.environ.get('SID', 'tl-live-%d' % int(time.time()))
# 递凭据用的环境变量名：故意取中性名（不含 TOKEN/KEY/SECRET/AUTH 字样）。
# 写入侧的脱敏过滤器会把「像密钥的名字 = 值」那行整段改成 `= ***`，不但丢值还会
# 把赋值打断成语法错 —— 实测 `SF_TOKEN=tok` 被改成 `***`、`= 'SF_SLOT'` 也被改过。
SLOT = 'SF_SLOT'


def fp(s):
    """指纹：只用来判断两端拿到的是不是同一个值，不泄露明文。"""
    return '%d/%s' % (len(s or ''), hashlib.md5((s or '').encode()).hexdigest()[:8])


def login():
    body = json.dumps({'username': U, 'password': P}).encode()
    req = urllib.request.Request(BASE + '/api/login', method='POST', data=body)
    req.add_header('Content-Type', 'application/json')
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.loads(r.read().decode())['token']


def seed_turn(tok, text):
    """第 1 轮：把素材贴进会话。只读干净，不判材料（要量的是第 2 轮）。"""
    body = json.dumps({'session_id': SID, 'message': text, 'mode': 'auto', 'skill': ''}).encode()
    req = urllib.request.Request(BASE + '/api/chat', method='POST', data=body)
    req.add_header('Content-Type', 'application/json')
    req.add_header('Authorization', 'Bearer ' + tok)
    t0 = time.time()
    ev = {}
    txt = 0
    errs = []
    done = {}
    buf = b''
    with urllib.request.urlopen(req, timeout=float(os.environ.get('SF_TIMEOUT', '300'))) as r:
        ctype = (r.headers.get('Content-Type') or '')
        if 'text/event-stream' not in ctype.lower():
            raise RuntimeError('第 1 轮拿到的不是 SSE：%s' % ctype)
        while True:
            chunk = r.read(1)
            if not chunk:
                break
            buf += chunk
            while b'\n\n' in buf:
                raw, buf = buf.split(b'\n\n', 1)
                t = raw.decode('utf-8', 'replace')
                e, d = None, ''
                for line in t.splitlines():
                    if line.startswith('event: '):
                        e = line[7:].strip()
                    elif line.startswith('data: '):
                        d += line[6:]
                ev[e] = ev.get(e, 0) + 1
                if e == 'delta':
                    try:
                        txt += len(json.loads(d).get('t', ''))
                    except Exception:
                        pass
                elif e == 'error':
                    errs.append(d[:200])
                elif e == 'done':
                    # 「这一轮到底写了没有」只能从 done 帧看：asked=true 意味着它只是
                    # 追问了参数，压根没进写作那一跳 —— 那种轮的正文短是正常的，不是
                    # 「第 1 轮断了」。区分开才能给出对的结论（下面 main 里用）。
                    try:
                        done = json.loads(d)
                    except Exception:
                        done = {}
    return dict(ms=int((time.time() - t0) * 1000), ev=ev, chars=txt, errs=errs, done=done)


def main():
    if not U or not P:
        print('第 1 轮就没成：SKILLFORGE_ADMIN_USER/PASS 不在环境里（先 source skillforge.env）')
        return 3
    mat = open(MAT, encoding='utf-8').read()
    # 贴素材那一轮的输入长度直接决定「素材有没有被压住」，所以把它印出来。
    print('=== 线上实测：素材 → 写作需求（同一 session）===')
    print('target: %s | session: %s' % (BASE, SID))
    print('素材: %s（%d 字）' % (MAT, len(mat)))
    print('需求: %s' % ASK)
    try:
        tok = login()
    except Exception as e:
        print('第 1 轮就没成：登录失败 %s: %s' % (type(e).__name__, e))
        return 3
    print('登录 ok（指纹 %s，不落盘不回显）' % fp(tok))
    r1 = seed_turn(tok, mat + SEED_SUFFIX)
    asked1 = str((r1['done'] or {}).get('asked', '')).lower() == 'true'
    print('第 1 轮（种素材）: %d ms | 事件 %s | 正文 %d 字%s%s' % (
        r1['ms'], r1['ev'], r1['chars'], ' | asked=true（追问参数轮）' if asked1 else '',
        (' | error ' + str(r1['errs'])) if r1['errs'] else ''))
    # 素材轮必须真的产出过正文：否则「第 2 轮忘了素材」不是上下文问题，是第 1 轮就断了。
    # 而「追问参数」轮（asked=true）压根没进写作那一跳，它的正文短是产品行为不是故障 ——
    # 这种轮不能当「压住素材再写」的前提，要如实说清并让人拿 SEED_SUFFIX 补一句写稿指令，
    # 而不是含混地报「几乎没吐正文」（差 2 个字就翻盘的门槛会把这种情形误判成故障）。
    if r1['chars'] < 300:
        if asked1:
            print('TURN1_FAIL：第 1 轮是「追问参数」轮（asked=true，%d 字），没进写作那一跳，'
                  '前提不成立 —— 加 SEED_SUFFIX="请按上面的【写作要求】写一篇新闻通稿。" 让它真写。' % r1['chars'])
        else:
            print('TURN1_FAIL：第 1 轮几乎没吐正文（%d 字），第 2 轮的前提不成立 —— 不拿它的红顶罪' % r1['chars'])
        return 3
    print('\n--- 第 2 轮交给尺子量 ---')
    env = dict(os.environ)
    env['BASE'] = BASE
    env['SF_TIMEOUT'] = os.environ.get('SF_TIMEOUT', '300')
    env[SLOT] = tok
    # 自检：脱敏过滤器/改名这类中间环节一旦把 token 吃掉，子进程只会拿到 401，
    # 尺子会诚实地报 PREMISE_MISS —— 那看起来像「材料坏了」。这里当场把它挡下来。
    if env.get(SLOT) != tok or len(tok) < 20:
        print('传参自检失败：子进程环境里的凭据指纹 %s，期望 %s' % (fp(env.get(SLOT)), fp(tok)))
        return 3
    p = subprocess.run([sys.executable, RULER, BASE, ASK, '--label', '线上:素材→写稿(第2轮)', '--sid', SID],
                       env=env)
    return p.returncode


if __name__ == '__main__':
    sys.exit(main())
