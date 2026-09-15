#!/usr/bin/env python3
"""「停用」必须可逆 —— 真服务上的往返验收（Bug N）。

用户报的原文：「业务技能停用了就消失了」。

现场：管理端「技能管理」列表打的是**公开**接口 /api/skills，而公开列表按设计把
停用技能过滤掉（对话页勾选层、首页卡片都不该出现停用技能）。于是点一次「停用」，
那一行就从界面上蒸发 —— 连它自己的「启用」按钮一起带走。停用变成了不可逆操作，
想恢复只能手改 sqlite。

这个脚本在**真服务**上把这个往返走一遍，重点是第 ⑦ 步：
停用之后管理端列表里必须**还在**、且 enabled=false。
（修复前那一步必红 —— 这就是它的存在意义：断言的是界面能否恢复，不是接口通不通。）

前置：需要管理员账号。按顺序取 ——
  1. 环境变量 SKILLFORGE_ADMIN_USER / SKILLFORGE_ADMIN_PASS
  2. 服务 env 文件里的同名两项（$SKILLFORGE_ENV_FILE，默认
     /opt/skillforge/skillforge.env）—— 线上验收就在这台机器上跑，不必把口令
     写进仓库。
  3. 都没有 → 打 SKIP 并 exit 0（**SKIP != PASS**，别把跳过当通过）。

用法：
  python3 web/tests/skill_toggle_roundtrip_e2e.py
  BASE=http://127.0.0.1:9999 python3 web/tests/skill_toggle_roundtrip_e2e.py

安全：脚本自己在 finally 里删掉临时技能（删除判据 = HTTP 200 且再查两次都不在）。
      名字带「验收」字样，万一脚本被杀掉，管理端一眼能认出这是残留物。
"""
# LIVE-LEGS: default
# ↑ 线上验收 leg 声明。枚举规则见 scripts/acceptance-live.sh 与
#   web/tests/live_e2e_roster.test.mjs。
import json
import os
import sys
import time
import urllib.error
import urllib.request

BASE = os.environ.get('BASE', 'http://127.0.0.1:8092').rstrip('/')
ENV_FILE = os.environ.get('SKILLFORGE_ENV_FILE', '/opt/skillforge/skillforge.env')
# 名字里必须有「验收」：脚本被强杀时，残留物要一眼认得出。
TMP_SLUG = 'yanzheng-tingyong-laihui'
TMP_NAME = '验收临时技能·停用往返'

fails = []
checks = 0


def check(name, ok, extra=''):
    global checks
    checks += 1
    if ok:
        print(f'ok   {name}')
    else:
        print(f'FAIL {name} {extra}')
        fails.append(name)
    return bool(ok)


def report(code=0):
    print(f'--- {checks - len(fails)}/{checks} ok ---')
    if fails:
        print('FAILED: ' + '; '.join(fails))
    sys.exit(1 if fails else code)


def skip(msg):
    print(f'SKIP {msg}')
    print('--- 0/0 ok ---（SKIP != PASS，这条 leg 算未验证）')
    sys.exit(0)


def creds():
    u = os.environ.get('SKILLFORGE_ADMIN_USER', '')
    p = os.environ.get('SKILLFORGE_ADMIN_PASS', '')
    if u and p:
        return u, p
    if os.path.exists(ENV_FILE):
        for line in open(ENV_FILE, encoding='utf-8', errors='replace'):
            line = line.strip()
            if line.startswith('SKILLFORGE_ADMIN_USER='):
                u = u or line.split('=', 1)[1].strip().strip('"').strip("'")
            elif line.startswith('SKILLFORGE_ADMIN_PASS='):
                p = p or line.split('=', 1)[1].strip().strip('"').strip("'")
    return u, p


def req(method, path, body=None, token=None):
    """返回 (status, 解析后的 JSON 或 None)。"""
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode()
        headers['Content-Type'] = 'application/json'
    if token:
        headers['Authorization'] = 'Bearer ' + token
    r = urllib.request.Request(BASE + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(r, timeout=20) as resp:
            raw = resp.read().decode('utf-8', 'replace')
            try:
                return resp.status, json.loads(raw)
            except json.JSONDecodeError:
                return resp.status, None
    except urllib.error.HTTPError as e:
        raw = e.read().decode('utf-8', 'replace')
        try:
            return e.code, json.loads(raw)
        except json.JSONDecodeError:
            return e.code, None


def public_has(slug):
    st, j = req('GET', '/api/skills')
    if st != 200 or not isinstance(j, dict):
        return None
    return any(s.get('slug') == slug for s in j.get('skills') or [])


ADMIN_LIST_STATUS = None


def admin_entry(slug):
    """管理端列表里的这一条（没找到返回 None）；顺便记下状态码 ——
    404 说明服务端根本没这条路由（老版本二进制），和「技能不在列表里」是两回事，
    混在一起会让人误判成修复没生效。"""
    global ADMIN_LIST_STATUS
    st, j = req('GET', '/api/admin/skills', token=TOKEN)
    ADMIN_LIST_STATUS = st
    if st != 200 or not isinstance(j, dict):
        return None
    for s in j.get('skills') or []:
        if s.get('slug') == slug:
            return s
    return None


USER, PASS = creds()
if not (USER and PASS):
    skip(f'拿不到管理员账号（env 或 {ENV_FILE} 里没有 SKILLFORGE_ADMIN_USER/PASS）')

st, j = req('POST', '/api/login', {'username': USER, 'password': PASS})
if not isinstance(j, dict) or not j.get('token'):
    check('管理员登录成功', False, f'登录返回 {st}：{(j or {}).get("error", "")}')
    report()
check('管理员登录成功', st == 200)
TOKEN = (j or {}).get('token') or ''

# 万一是上次残留（脚本被强杀过），先清掉，保证下面第 ③ 步的「在」是真建出来的。
req('DELETE', f'/api/admin/skills/{TMP_SLUG}', token=TOKEN)

try:
    st, j = req('POST', '/api/admin/skills', {
        'slug': TMP_SLUG,
        'name': TMP_NAME,
        'description': '线上验收用临时技能：验证停用后可再启用（脚本自己会删掉）',
        'category': 'general',
        'input_params': [],
        'system_prompt': '这是验收临时技能，不会被真正调用。',
    }, token=TOKEN)
    check('建临时业务技能', st == 200 and (j or {}).get('slug') == TMP_SLUG, f'返回 {st} {j}')

    # ③ 反空转锚：先证明它真的在两个列表里。少了这一步，后面「停用后不见了」的
    #    断言在「技能压根没建出来」时也是绿的（假绿）。
    check('新建后：公开列表里有它', public_has(TMP_SLUG) is True)
    e = admin_entry(TMP_SLUG)
    check('新建后：管理端列表里有它且 enabled=true', bool(e) and e.get('enabled') is True)

    st, _ = req('POST', '/api/admin/skills/toggle', {'slug': TMP_SLUG, 'enabled': False}, token=TOKEN)
    check('停用接口返回 200', st == 200, f'返回 {st}')

    check('停用后：公开列表里没有它（公开口径仍然过滤）', public_has(TMP_SLUG) is False)

    # ⑦ 核心：Bug N 的判据。修复前这里是 FAIL —— 停用一次，管理端那一行连同
    #    它的「启用」按钮一起消失，只能手改 sqlite 才能恢复。
    e = admin_entry(TMP_SLUG)
    check('停用后：管理端列表里**仍然有**它（否则停用不可逆）', e is not None,
          f'—— GET /api/admin/skills 返回 {ADMIN_LIST_STATUS}；'
          '404 说明这个二进制还没有管理端全量列表（修复没部署），'
          '200 却找不到说明管理端又在读被过滤过的公开列表（Bug N 复发）')
    check('停用后：管理端看到 enabled=false（渲染得出「已停用」药丸 + 启用按钮）',
          bool(e) and e.get('enabled') is False, f'实际 {e.get("enabled") if e else None}')

    st, _ = req('POST', '/api/admin/skills/toggle', {'slug': TMP_SLUG, 'enabled': True}, token=TOKEN)
    check('再启用接口返回 200', st == 200, f'返回 {st}')
    check('再启用后：公开列表里回来了（可逆闭环）', public_has(TMP_SLUG) is True)
    e = admin_entry(TMP_SLUG)
    check('再启用后：管理端 enabled=true', bool(e) and e.get('enabled') is True)
finally:
    # 清理判据不是「接口返回 200」，而是**再查两次都不在** —— 200 只说明请求被受理。
    st, _ = req('DELETE', f'/api/admin/skills/{TMP_SLUG}', token=TOKEN)
    gone_public = public_has(TMP_SLUG) is False
    gone_admin = admin_entry(TMP_SLUG) is None
    # 磁盘目录也要看：DB 里没了但目录留着，下次同名建技能会撞车。
    disk = os.path.join('/opt/skillforge/data/skills', TMP_SLUG)
    gone_disk = not os.path.exists(disk)
    check('清理：临时技能从公开列表消失', gone_public)
    # 这里必须连状态码一起断言：管理端接口 404（老二进制 / 路由没接上）时
    # `admin_entry` 也返回 None —— 只看「不在」的话，这条会空转变绿。
    check('清理：临时技能从管理端列表消失',
          ADMIN_LIST_STATUS == 200 and gone_admin,
          f'（GET /api/admin/skills 返回 {ADMIN_LIST_STATUS}）')
    check('清理：磁盘目录也删掉了', gone_disk, f'（{disk} 还在，可能是残留）')
    if st != 200:
        print(f'note 删除接口返回 {st}（技能可能本来就不在）')

report()
