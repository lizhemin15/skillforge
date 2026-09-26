#!/usr/bin/env python3
"""模型热切换的真机 E2E：真二进制 + 真 HTTP + 真 DB + 两个会自报家门的假模型。

为什么必须跑真机、不能只靠单测：
  这条链路的判据是「请求到底打到哪个地址」。单测里可以断言引擎内部的字段换了，
  而**字段换了、请求仍走旧地址**恰恰就是线上事故的形状 —— 进程里常驻的客户端是启动
  那一刻建的，管理端三个接口（切换 / 保存 / 删除）都只改库。用户把模型从 A 切到 B、
  界面立刻显示「在用：B」，每一轮问答却仍在打已欠费的 A，持续收到 402，
  唯一能得出的结论是「这系统不支持热切换」。
  所以这里不看重内部字段，只认两件事：
    ① 哪个假模型收到了请求（计数器）；
    ② 进程 PID 有没有变（有没有偷偷重启才生效）。
  两个假模型放在不同端口，各自回自己名字的正文，请求打到谁一目了然。

跑法（BIN 必须是当前代码编出来的二进制）：
    go build -o /tmp/skillforge-hs ./cmd/server
    BIN=/tmp/skillforge-hs python3 e2e/llm_hotswap_e2e.py
负向对照（尺子自己会不会红）：
    python3 e2e/llm_hotswap_e2e.py --selfcheck
"""
import json
import os
import shutil
import socket
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

BIN = os.environ.get('BIN', '')
DATADIR = os.environ.get('E2E_DATA', '/tmp/skillforge-hotswap-e2e')
ADMIN_U, ADMIN_P = 'e2e-admin', 'e2e-pass-123'
PLACEHOLDER = 'sk-e2e-not-a-real-key'

checks = 0
fails = []


def check(name, ok, extra=''):
    global checks
    checks += 1
    print(('  ✓ ' if ok else '  ✗ ') + name + (('  [' + extra + ']') if extra else ''))
    if not ok:
        fails.append(name)


def free_port():
    s = socket.socket()
    s.bind(('127.0.0.1', 0))
    p = s.getsockname()[1]
    s.close()
    return p


class FakeLLM:
    """会自报家门的假模型：正文里带自己的 tag，并向外部计数器记账。"""

    def __init__(self, tag):
        self.tag = tag
        self.reqs = []
        outer = self

        class H(BaseHTTPRequestHandler):
            protocol_version = 'HTTP/1.1'

            def log_message(self, *a):
                pass

            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers.get('Content-Length', 0))))
                msgs = body.get('messages') or []
                system = '\n'.join(m.get('content', '') for m in msgs if m.get('role') == 'system')
                user = '\n'.join(m.get('content', '') for m in msgs if m.get('role') == 'user')
                if '多智能体管线的调度器' in system:
                    kind, text = 'classify', classify_json('')
                elif '写作技能的提示词工程师' in system:
                    kind, text = 'lite', json.dumps(lite_json(outer.tag), ensure_ascii=False)
                else:
                    kind, text = 'draft', '%s：这份纪要由 %s 这台模型产出。' % (outer.tag, outer.tag)
                outer.reqs.append({'kind': kind, 'system': system, 'user': user})
                if body.get('stream'):
                    self.send_response(200)
                    self.send_header('Content-Type', 'text/event-stream')
                    self.send_header('Connection', 'close')
                    self.end_headers()
                    for i in range(0, len(text), 40):
                        ch = {'id': 'f', 'object': 'chat.completion.chunk', 'created': 0,
                              'model': 'fake', 'choices': [{'index': 0,
                                                            'delta': {'content': text[i:i + 40]},
                                                            'finish_reason': None}]}
                        self.wfile.write(('data: ' + json.dumps(ch, ensure_ascii=False) + '\n\n').encode())
                    self.wfile.write(b'data: [DONE]\n\n')
                else:
                    payload = {'id': 'f', 'object': 'chat.completion', 'created': 0, 'model': 'fake',
                               'choices': [{'index': 0, 'finish_reason': 'stop',
                                            'message': {'role': 'assistant', 'content': text}}],
                               'usage': {'prompt_tokens': 1, 'completion_tokens': 1, 'total_tokens': 2}}
                    data = json.dumps(payload, ensure_ascii=False).encode()
                    self.send_response(200)
                    self.send_header('Content-Type', 'application/json')
                    self.send_header('Content-Length', str(len(data)))
                    self.end_headers()
                    self.wfile.write(data)

        self.srv = ThreadingHTTPServer(('127.0.0.1', 0), H)
        self.port = self.srv.server_address[1]
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def n(self):
        return len(self.reqs)


def classify_json(slug):
    return json.dumps({
        'intent': 'write', 'action': 'write', 'skill_slug': slug, 'needs_tools': False,
        'reason': '命中技能', 'params': {}, 'needs': [],
        'steps': [{'phase': 'analyze', 'detail': 'write/write'},
                  {'phase': 'match', 'detail': '命中技能'},
                  {'phase': 'params', 'detail': '提炼用户内容'},
                  {'phase': 'generate', 'detail': '通用写作'}],
    }, ensure_ascii=False)


def lite_json(tag):
    """极简创建的回执：带 tag 是为了「哪台模型生成的技能」也能一眼看出来。"""
    return {
        'name': '热切换验证技能-%s' % tag, 'description': '验证模型热切换用',
        'system_prompt': ('你是写作助手。按用户给的素材写，素材里没有的日期、人名、数字一律不写。\n'
                          '（由 %s 产出）' % tag),
        'style': ['客观'], 'structure': ['开头', '正文'], 'forbidden': ['编造'],
        'length_hint': '200 字左右',
    }


def post_json(base, path, obj, tok=None, timeout=60):
    req = urllib.request.Request(base + path, data=json.dumps(obj).encode(),
                                 headers={'Content-Type': 'application/json'})
    if tok:
        req.add_header('Authorization', 'Bearer ' + tok)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def get_json(base, path, tok=None, timeout=30):
    req = urllib.request.Request(base + path, headers={})
    if tok:
        req.add_header('Authorization', 'Bearer ' + tok)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def post_multipart(base, path, fields, tok, timeout=300):
    boundary = '----e2ehotswap'
    parts = []
    for k, v in fields.items():
        parts.append(('--' + boundary + '\r\nContent-Disposition: form-data; name="%s"\r\n\r\n%s\r\n' % (k, v)).encode())
    parts.append(('--' + boundary + '--\r\n').encode())
    data = b''.join(parts)
    req = urllib.request.Request(base + path, data=data, headers={
        'Content-Type': 'multipart/form-data; boundary=' + boundary,
        'Authorization': 'Bearer ' + tok})
    return urllib.request.urlopen(req, timeout=timeout)


def drain(resp):
    """把 SSE 读干（读到连接自然结束）。正文片段顺手收集，用于「用户看到的字出自哪台模型」。

    这里踩过一个会伪装成「模型没被调用」的坑，必须写下来：
      trace 事件的 data 是**数组**（[{"phase":…}]），不是对象。旧写法对每个事件统一
      `ev.get(k)`，数组上抛 AttributeError，被外层 `except Exception: pass` 吞掉 ——
      drain 在第一个事件就返回，剩下的流再没人读，连接被 GC 时服务端判定「客户端挂了」，
      把这一轮所有模型调用标成 `context canceled`，假模型一次请求都记不到。
      于是「换模型到底生不生效」的判据全部变红，而真正的问题在尺子上。
      结论：① 事件体按类型分别取；② 异常不许静默，看得见才查得动。
    """
    text = []
    try:
        for raw in resp:
            line = raw.decode('utf-8', 'ignore').strip()
            if not line.startswith('data:'):
                continue
            body = line[5:].strip()
            if body == '[DONE]':
                break
            try:
                ev = json.loads(body)
            except Exception:
                continue
            pieces = []
            if isinstance(ev, dict):
                pieces = [ev.get('t'), ev.get('delta'), ev.get('text'), ev.get('content')]
            elif isinstance(ev, list):
                for it in ev:
                    if isinstance(it, dict):
                        pieces += [it.get('t'), it.get('text'), it.get('content')]
            for p in pieces:
                if isinstance(p, str):
                    text.append(p)
    except Exception as e:
        print('    ⚠ SSE 没读干（%s: %s）—— 后面的判据可能因此失真，先修尺子'
              % (type(e).__name__, e))
    return ''.join(text)


def chat(base, sid, msg):
    body = {'session_id': sid, 'message': msg, 'mode': 'manual'}
    req = urllib.request.Request(base + '/api/chat', data=json.dumps(body).encode(),
                                 headers={'Content-Type': 'application/json'})
    return urllib.request.urlopen(req, timeout=300)


# ---------------------------------------------------------------- 判据
# 每条判据都单独成形，负向对照才能一条条喂给它假观察。

def judge_runtime_matches(runtime_id, want_id, phase):
    """运行期必须就是「此刻库里启用的那条」。界面显示的「在用」不算数。"""
    return [('%s 运行期就是库里启用的那条（runtime_id=%s want=%s）'
             % (phase, runtime_id, want_id), runtime_id == want_id)]


def judge_only_new_got_hit(hits_old_before, hits_old_after, hits_new_before, hits_new_after, phase):
    """改完之后：新地址必须收到请求，旧地址一次都不许再收。

    两条都要判。只判「新的收到了」会漏掉「旧的也还在收」这种更难查的形态
    （两个地址都收到，说明有两条链路各用一份客户端）。
    """
    return [
        ('%s 新地址收到了请求（%d → %d）' % (phase, hits_new_before, hits_new_after),
         hits_new_after > hits_new_before),
        ('%s 旧地址一次都没再收到（%d → %d）' % (phase, hits_old_before, hits_old_after),
         hits_old_after == hits_old_before),
    ]


def judge_reply_from(text, tag, phase):
    """用户**看到的那段字**必须出自这台模型。

    比计数器更贴近用户：计数器说明请求打到了谁，正文说明用户读到的是谁。
    线上事故里两者都没有 —— 所以两条都要判。
    """
    return [('%s 用户读到的正文出自 %s（正文含「%s」标记）' % (phase, tag, tag), tag in text)]


def judge_no_restart(pid_before, pid_after):
    """「无需重启」必须是真的：靠重启才生效就等于没修（用户配完模型不会去重启进程）。"""
    return [('进程全程未被重启（pid %s → %s）' % (pid_before, pid_after), pid_before == pid_after)]


def judge_added_config_does_not_steal(hits_new_before, hits_new_after):
    """新增一条配置（未启用）不许偷换运行中的模型：用户只是「加一条备着」。"""
    return [('新增未启用的配置没有偷换运行期（其地址收到 %d → %d 次）'
             % (hits_new_before, hits_new_after), hits_new_after == hits_new_before)]


def selfcheck():
    """负向对照：把已知故障形态喂给判据，判据必须红。

    must_red 的判据是「**不是全绿**」，不是「有任何一条绿」——
    这一条本身踩过坑：judge_only_new_got_hit 一次返回两格（新地址该收到 / 旧地址该停工），
    写成 any(ok) 之后，「新地址收到了、旧地址还在收」这种更隐蔽的坏法会被判成「抓到了」，
    负向对照于是自己变成假绿。负向对照的尺子也会骗人，所以它自己也必须双向验。
    """
    n = 0
    bad = 0

    def must_red(name, pairs):
        nonlocal n, bad
        n += 1
        if all(ok for _, ok in pairs):
            print('  ✗ 负向对照「%s」没被抓（判据是假的）' % name)
            bad += 1
        else:
            print('  ✓ 负向对照「%s」被抓' % name)

    def must_green(name, pairs):
        nonlocal n, bad
        n += 1
        if all(ok for _, ok in pairs):
            print('  ✓ 负向对照「%s」放行（判据不过敏）' % name)
        else:
            print('  ✗ 负向对照「%s」误报' % name)
            bad += 1

    # ① 只改库、运行期没动 —— 线上事故的原样
    must_red('只改库运行期不动', judge_runtime_matches(2, 3, '切换'))
    must_green('库与运行期一致', judge_runtime_matches(3, 3, '切换'))
    # ② 新地址收到、旧地址还在收（两条链路各用一份客户端）
    must_red('新旧地址都在收', judge_only_new_got_hit(1, 2, 0, 1, '切换'))
    # ③ 新旧都没收到（链路上压根没打模型，看着像绿）
    must_red('新地址一次都没收到', judge_only_new_got_hit(1, 1, 0, 0, '切换'))
    must_green('干净的切换', judge_only_new_got_hit(1, 1, 0, 1, '切换'))
    # ④ 靠重启才生效
    must_red('进程被重启', judge_no_restart(1234, 4321))
    must_green('进程没重启', judge_no_restart(1234, 1234))
    # ⑤ 只是加一条备用的，就把在用的顶掉了
    must_red('新增配置偷换了运行期', judge_added_config_does_not_steal(0, 1))
    must_green('新增配置没动运行期', judge_added_config_does_not_steal(0, 0))

    print('--- %d/%d 负向对照 ok ---' % (n - bad, n))
    return 1 if bad else 0


def main():
    if '--selfcheck' in sys.argv:
        return selfcheck()
    if not BIN or not os.path.isfile(BIN) or not os.access(BIN, os.X_OK):
        print('SKIP 缺 BIN（当前代码编出来的二进制）：\n'
              '  go build -o /tmp/skillforge-hs ./cmd/server && '
              'BIN=/tmp/skillforge-hs python3 %s' % sys.argv[0])
        return 2

    shutil.rmtree(DATADIR, ignore_errors=True)
    os.makedirs(DATADIR, exist_ok=True)
    fa, fb, fc = FakeLLM('A'), FakeLLM('B'), FakeLLM('C')
    port = free_port()
    base = 'http://127.0.0.1:%d' % port
    env = dict(os.environ,
               SKILLFORGE_ADDR='127.0.0.1:%d' % port,
               SKILLFORGE_DATA_DIR=DATADIR,
               SKILLFORGE_DB=os.path.join(DATADIR, 'skillforge.db'),
               SKILLFORGE_ADMIN_USER=ADMIN_U,
               SKILLFORGE_ADMIN_PASS=ADMIN_P,
               SKILLFORGE_JWT_SECRET=PLACEHOLDER,
               SKILLFORGE_BASE_URL=base)
    # env 里的真密钥一律不用：模型只走 DB 配置里的假模型（也正是线上核 DB 那条规则）
    for k in ('SKILLFORGE_LLM_API_KEY', 'SKILLFORGE_LLM_BASE_URL', 'SKILLFORGE_LLM_MODEL'):
        env.pop(k, None)
    logpath = os.path.join(DATADIR, 'server.log')
    log = open(logpath, 'w')
    srv = subprocess.Popen([BIN], env=env, stdout=log, stderr=subprocess.STDOUT)
    pid0 = srv.pid
    print('二进制：%s\n数据目录：%s\n假模型：A=%d B=%d C=%d\n服务：%s\n'
          % (BIN, DATADIR, fa.port, fb.port, fc.port, base))

    def cleanup():
        srv.terminate()
        try:
            srv.wait(timeout=5)
        except Exception:
            srv.kill()
    try:
        # 就绪断言必须同时命中「本次端口」+「本次数据目录」：否则会打到残留实例上，
        # 得到的是别人实例的产物（假红/假绿都出现过）。
        want = 'listening on 127.0.0.1:%d (data: %s)' % (port, DATADIR)
        for _ in range(60):
            if os.path.exists(logpath) and want in open(logpath).read():
                break
            if srv.poll() is not None:
                print('✗ 服务退出，日志：\n' + open(logpath).read()[-2000:])
                return 1
            time.sleep(0.5)
        else:
            print('✗ 30s 内未确认服务归属（不接受打到别人实例上的结果）')
            return 1

        tok = post_json(base, '/api/login', {'username': ADMIN_U, 'password': ADMIN_P})['token']
        print('管理端登录 ✓')

        # ---- ① 起点：库里没有任何配置，界面如实说「尚未装载」 ----
        j = get_json(base, '/api/admin/llms', tok)
        check('① 无配置时 runtime_id 为 0（界面显示「尚未装载」而不是假装在用）',
              j.get('runtime_id') == 0, 'runtime_id=%s' % j.get('runtime_id'))

        # ---- ② 配 A 并启用：这正是用户「切到新模型」的动作 ----
        ca = post_json(base, '/api/admin/llms', {
            'provider': '网关A', 'model': 'model-A',
            'base_url': 'http://127.0.0.1:%d' % fa.port, 'api_key': PLACEHOLDER}, tok)
        a_id = ca.get('id') or (ca.get('config') or {}).get('id')
        post_json(base, '/api/admin/llms/active', {'id': a_id}, tok)
        j = get_json(base, '/api/admin/llms', tok)
        for name, ok in judge_runtime_matches(j.get('runtime_id'), a_id, '保存并启用 A 后'):
            check(name, ok)

        n_a0, n_b0 = fa.n(), fb.n()
        txt = drain(chat(base, 'hs-1', '帮我写一份周报，本周做了接口联调。'))
        check('② 第一轮对话真的打到了 A（%d → %d）' % (n_a0, fa.n()), fa.n() > n_a0)
        check('② B 一次都没被碰过（%d）' % fb.n(), fb.n() == 0)
        for name, ok in judge_reply_from(txt, 'A', '② 第一轮对话'):
            check(name, ok)

        # ---- ③ 再加一条 B（未启用）：不许偷换运行中的模型 ----
        cb = post_json(base, '/api/admin/llms', {
            'provider': '网关B', 'model': 'model-B',
            'base_url': 'http://127.0.0.1:%d' % fb.port, 'api_key': PLACEHOLDER}, tok)
        b_id = cb.get('id') or (cb.get('config') or {}).get('id')
        j = get_json(base, '/api/admin/llms', tok)
        for name, ok in judge_runtime_matches(j.get('runtime_id'), a_id, '新增未启用的 B 后'):
            check(name, ok)
        n_a1, n_b1 = fa.n(), fb.n()
        drain(chat(base, 'hs-2', '再写一份，本周还开了次评审会。'))
        for name, ok in judge_added_config_does_not_steal(n_b1, fb.n()):
            check(name, ok)
        check('③ 对话仍在打 A', fa.n() > n_a1)

        # ---- ④ 点「切换」：当场生效，旧地址必须停工 ----
        n_a2, n_b2 = fa.n(), fb.n()
        post_json(base, '/api/admin/llms/active', {'id': b_id}, tok)
        j = get_json(base, '/api/admin/llms', tok)
        for name, ok in judge_runtime_matches(j.get('runtime_id'), b_id, '点「切换」后'):
            check(name, ok)
        txt = drain(chat(base, 'hs-3', '换台模型继续，写一份上线通知。'))
        for name, ok in judge_only_new_got_hit(n_a2, fa.n(), n_b2, fb.n(), '点「切换」后'):
            check(name, ok)
        for name, ok in judge_reply_from(txt, 'B', '点「切换」后'):
            check(name, ok)

        # ---- ⑤ 就地编辑「已保存的服务」（前端恒发 is_active:false）：也要当场生效 ----
        # 这条是用户最可能的动作：不新增、不动启用状态，只把地址/模型名改一改。
        n_c0 = fc.n()
        edited = post_json(base, '/api/admin/llms', {
            'id': b_id, 'provider': '网关C', 'model': 'model-C',
            'base_url': 'http://127.0.0.1:%d' % fc.port, 'api_key': PLACEHOLDER,
            'is_active': False}, tok)
        _ = edited
        j = get_json(base, '/api/admin/llms', tok)
        for name, ok in judge_runtime_matches(j.get('runtime_id'), b_id, '就地编辑后（id 不变）'):
            check(name, ok)
        n_a3, n_b3, n_c1 = fa.n(), fb.n(), fc.n()
        txt = drain(chat(base, 'hs-4', '用新地址再写一份会议通知。'))
        for name, ok in judge_only_new_got_hit(n_b3, fb.n(), n_c1, fc.n(), '就地编辑后'):
            check(name, ok)
        for name, ok in judge_reply_from(txt, 'C', '就地编辑后'):
            check(name, ok)
        check('⑤ 就地编辑后 A 也早就不再被碰（%d）' % fa.n(), fa.n() == n_a3)

        # ---- ⑥ 删掉正在用的那条：运行期必须退回剩下那条，不能指向已删除的记录 ----
        req = urllib.request.Request(base + '/api/admin/llms/%d' % b_id, method='DELETE',
                                     headers={'Authorization': 'Bearer ' + tok})
        urllib.request.urlopen(req, timeout=30).read()
        j = get_json(base, '/api/admin/llms', tok)
        left = [c['id'] for c in j.get('configs') or []]
        check('⑥ 删掉在用的那条后运行期退回剩下的那条（runtime_id=%s 剩下=%s）'
              % (j.get('runtime_id'), left), j.get('runtime_id') == a_id)

        # ---- ⑦ 「无需重启」必须是真的 ----
        for name, ok in judge_no_restart(pid0, srv.pid):
            check(name, ok)

        # ---- ⑧ 日志里必须留下换血痕迹（排障时用户/我们唯一能事后看到的东西） ----
        logtxt = open(logpath).read()
        check('⑧ 日志里有「模型热切换」的换血记录', '模型热切换' in logtxt)

        print('--- %d/%d ok ---' % (checks - len(fails), checks))
        if fails:
            print('FAILED: ' + '; '.join(fails))
            return 1
        return 0
    finally:
        cleanup()


if __name__ == '__main__':
    sys.exit(main())
