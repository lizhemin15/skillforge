#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""零材料强制门 · 真机 E2E（假模型替身 + 真二进制 + 真 SSE + 真多轮）。

【为什么必须有这一条，单测不够】
internal/api/chat_needs_gate_wiring_test.go 用的是 httptest + 假 handler：它证明的是
**函数接线**对不对。证明不了三件只有真机才成立的事：
  1. 真二进制里技能表的 input_params 是从**数据库**读出来的（declaredParams 走 store），
     单测里那份是内存塞进去的 —— 「极简创建生成的技能，声明到底有没有进库」只有真机量得到；
  2. 真 SSE 帧序（event: needs / done.asked=true）跟前端认的是不是同一套；
  3. **本轮到底花了几次模型调用** —— 内网 token 慢，这一条是硬指标，只有假模型日志量得到。

【用户投诉的回放】
一句话指令 + 零材料时，分类器一条 needs 都不报 → 旧实现里闸门形同不存在 → 写作跳直通，
产出 645 字带假日期、假参会人的会议纪要。用户读到的是「它替我编了」。

【假模型】en2e/fake 一样的分发思路（见 e2e/fake_llm.py）：按 system 里的特征串分发。
  - 「写作技能的提示词工程师」→ 极简创建的 JSON（含 2 个 required 输入项）
  - 「多智能体管线的调度器」  → 意图分类 JSON，**needs 恒为 []**（这就是缺口的样子）
  - 其余                      → 起草正文，带 FABRICATED-BODY 标记
FABRICATED-BODY 是关键证物：它在 SSE 里出现 = 写作跳真的动了 = 正文是编的。

跑法（需要一份当前代码编出来的二进制，不要拿线上那份）：
  cd /root/skillforge
  go build -o /tmp/skillforge-zg ./cmd/server
  BIN=/tmp/skillforge-zg python3 e2e/zero_material_gate_e2e.py
负向对照（不需要二进制、不需要端口，纯回放）：
  python3 e2e/zero_material_gate_e2e.py --selfcheck
"""
import json
import os
import re
import socket
import subprocess
import sys
import threading
import time
import urllib.request
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

BIN = os.environ.get('BIN', '')
DATADIR = os.environ.get('DATADIR', '/tmp/sf-zerogate-%s' % time.strftime('%H%M%S'))
ADMIN_U, ADMIN_P = 'e2e-admin', 'e2e-pass-123'
PLACEHOLDER = 'e2e-local-dev-only'   # 本地占位串：真密钥不许进脚本
FAKE_BODY = 'FABRICATED-BODY'
# 旧版通用要素清单的逐字签名（git show b508947^:internal/api/chat.go 的 needsMessage）
OLD_LIST_SIGS = ('我需要你补充以下信息', '我需要你补充一些信息', '要开始写作，我需要你')

MSG_ZERO = '帮我写一份会议纪要'
MSG_GRANT = '就按你的'

fails, checks = [], 0


def check(name, ok, extra=''):
    global checks
    checks += 1
    print(('ok   ' if ok else 'FAIL ') + name + ('' if ok else ' ' + str(extra)))
    if not ok:
        fails.append(name)
    return bool(ok)


# ---------------------------------------------------------------- 假模型

LITE_JSON = {
    'skill_name': '会议纪要写作',
    'description': '把会议素材整理成决议清晰的会议纪要',
    'trigger': '需要把会议记录整理成正式纪要时',
    'input_params': [
        {'name': 'subject', 'label': '会议主题', 'type': 'text', 'required': True,
         'placeholder': '如：Q3 项目复盘会', 'help': '写标题与首段要用'},
        {'name': 'attendees', 'label': '参会人', 'type': 'text', 'required': True,
         'placeholder': '如：产品、研发两边同学', 'help': '决议责任人要用'},
    ],
    # 600 字以内、≥300 字（写盘时会追加本地交互协议，下限校验在追加之后）
    'style_profile_md': '## 文风\n客观、条目化，不写会议气氛。\n\n## 结构骨架\n'
                        '一、会议基本信息（主题/时间/地点/参会人）\n二、讨论要点\n三、决议事项（责任人+时限）\n\n'
                        '## 长度\n400~800 字。\n\n## 禁忌\n不得出现主观评价、不得出现没有依据的数字。\n',
    'reviewer_md': '- [ ] 是否写明会议主题、时间、参会人\n- [ ] 每条决议是否都有责任人与时限\n'
                   '- [ ] 是否出现主观评价\n- [ ] 是否出现素材里没有的数字\n',
    'system_prompt_md': '你是专精于撰写会议纪要的资深写作专家。用户给你需求与素材，你产出成稿。\n\n'
                        '## 工作方式\n先看用户这轮给的信息：如果连会议主题、参会人这类必备要素都'
                        '找不到依据，就先问一句要材料（材料给全了再动笔），不要凭想象补事实。'
                        '信息够了就直接写完。\n\n'
                        '## 硬约束\n决议事项必须写清责任人与完成时限；不得描写会议气氛；'
                        '不得出现素材里没有的日期、人名、数字。\n\n'
                        '## 交付物卫生\n只输出纪要正文本身，不要附加写作说明、核对清单或自证附录。',
}


def classify_json(slug):
    """意图分类回执：**needs 恒为 []** —— 这正是线上编造内容那次现场的样子。"""
    return json.dumps({
        'intent': 'write', 'action': 'write', 'skill_slug': slug, 'needs_tools': False,
        'reason': '命中技能', 'params': {}, 'needs': [],
        'steps': [{'phase': 'analyze', 'detail': 'write/write'},
                  {'phase': 'match', 'detail': '命中技能'},
                  {'phase': 'params', 'detail': '提炼用户内容'},
                  {'phase': 'generate', 'detail': '通用写作'}],
    }, ensure_ascii=False)


class FakeLLM:
    """替身模型：只在内存里记请求，供「本轮花了几次调用」的真机断言用。"""

    def __init__(self):
        self.reqs = []          # [{'kind':…, 'system':…, 'user':…}]
        self.slug = ''
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
                if '写作技能的提示词工程师' in system:
                    kind, text = 'lite', json.dumps(LITE_JSON, ensure_ascii=False)
                elif '多智能体管线的调度器' in system:
                    kind, text = 'classify', classify_json(outer.slug)
                else:
                    kind, text = 'draft', FAKE_BODY + '：8 月 26 日，张三主持了会议，李四等 12 人参加。\n'
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

    def mark(self):
        return len(self.reqs)

    def since(self, n):
        return self.reqs[n:]


# ---------------------------------------------------------------- HTTP 小工具

def free_port():
    s = socket.socket()
    s.bind(('127.0.0.1', 0))
    p = s.getsockname()[1]
    s.close()
    return p


def post_json(base, path, obj, tok=None, timeout=30):
    r = urllib.request.Request(base + path, data=json.dumps(obj).encode(),
                               headers={'Content-Type': 'application/json'})
    if tok:
        r.add_header('Authorization', 'Bearer ' + tok)
    with urllib.request.urlopen(r, timeout=timeout) as resp:
        return json.loads(resp.read().decode() or '{}')


def post_multipart(base, path, fields, tok, timeout=180):
    """极简创建是 multipart，这里手拼请求体，不引第三方库。"""
    boundary = '----zgboundary' + uuid.uuid4().hex
    parts = []
    for k, v in fields.items():
        parts.append(('--%s\r\nContent-Disposition: form-data; name="%s"\r\n\r\n%s\r\n'
                      % (boundary, k, v)).encode())
    parts.append(('--%s--\r\n' % boundary).encode())
    body = b''.join(parts)
    r = urllib.request.Request(base + path, data=body)
    r.add_header('Content-Type', 'multipart/form-data; boundary=' + boundary)
    r.add_header('Authorization', 'Bearer ' + tok)
    return urllib.request.urlopen(r, timeout=timeout)


def read_train_sse(resp):
    """训练端点的帧序跟 /api/chat **不一样**：没有 `event:` 行，事件名在 data 里
    （{"type":"step|delta|error|done","data":…}）。混用同一个解析器会读到 done={} ——
    第一步就假红。"""
    out = {'done': {}, 'error': '', 'steps': [], 'types': []}
    for raw in resp:
        line = raw.decode('utf-8', 'replace').rstrip('\n')
        if not line.startswith('data:'):
            continue
        try:
            obj = json.loads(line[5:].strip())
        except Exception:
            continue
        t = obj.get('type', '')
        out['types'].append(t)
        if t == 'step':
            out['steps'].append(str(obj.get('data', '')))
        elif t == 'error':
            out['error'] = str(obj.get('data', ''))
        elif t == 'done':
            try:
                out['done'] = json.loads(obj.get('data', '{}'))
            except Exception:
                out['done'] = {}
    return out


def read_sse(resp, t0=None):
    """逐帧读 SSE；返回观测值。观测与判据分离，SELFCHECK 直接喂桩。"""
    t0 = t0 or time.time()
    ev, needs, deltas, step_text, done = '', [], [], [], {}
    for raw in resp:
        line = raw.decode('utf-8', 'replace').rstrip('\n')
        if line.startswith('event:'):
            ev = line[6:].strip()
            continue
        if not line.startswith('data:'):
            continue
        try:
            data = json.loads(line[5:].strip())
        except Exception:
            continue
        if ev == 'needs':
            needs.append(data)
        elif ev == 'delta':
            deltas.append(data.get('t', ''))
        elif ev == 'step':
            step_text.append(str(data.get('text', data)))
        elif ev == 'done':
            done = data
    text = ''.join(deltas)
    return {'has_needs_frame': bool(needs), 'needs': needs, 'text': text,
            'asked': str(done.get('asked', '')) == 'true', 'done': done,
            'frames': len(step_text)}


# ---------------------------------------------------------------- 判据（纯函数）

def norm(s):
    return re.sub(r'[\s\u3000\u200b]+', '', s or '')


def judge_zero_round(obs, msg):
    """零材料轮：必须停问、必须逐字引用原话、必须没有编造正文。"""
    out = []
    out.append(('L3 零材料时停问（有 needs 帧 且 done.asked=true）',
                obs['has_needs_frame'] and obs['asked'],
                'has_needs=%s asked=%s —— 没停问就是写作跳直通，正文是编的'
                % (obs['has_needs_frame'], obs['asked'])))
    ask = obs['text'] if obs['asked'] else ''
    out.append(('L4 停问文案逐字引用用户原话「%s」' % msg,
                ('「' + msg + '」') in ask, 'ask=%r' % ask[:120]))
    out.append(('L5 停问文案点名缺的必填项（会议主题/参会人 至少一个）',
                ('会议主题' in ask) or ('参会人' in ask), 'ask=%r' % ask[:120]))
    out.append(('L6 没有编造正文（本轮 SSE 里不出现起草标记 %s）' % FAKE_BODY,
                FAKE_BODY not in obs['text'],
                '正文里出现了起草标记 —— 用户拿到的是编的事实'))
    out.append(('L8 停问文案里没有旧版通用要素清单（逐字）',
                not [s for s in OLD_LIST_SIGS if s in ask], 'ask=%r' % ask[:120]))
    return out


def judge_call_budget(round_reqs):
    """本轮模型调用预算：内网 token 慢，停问这一轮只许花 1 次（分类那一跳）。"""
    kinds = [r['kind'] for r in round_reqs]
    return [('L7 零材料停问这轮只花 1 次模型调用（实际 %d 次：%s）' % (len(kinds), kinds),
             kinds == ['classify'],
             '多出来的调用会让用户白等；出现 draft 就说明还是编了')]


def judge_grant_round(obs):
    """授权轮：出口必须通向写作，否则就是问成死循环。"""
    return [
        ('L9 回「就按你的」不再停问', not (obs['has_needs_frame'] and obs['asked']),
         'asked=%s needs=%s —— 出口闭不上会问成死循环' % (obs['asked'], obs['has_needs_frame'])),
        ('L10 授权后真的出货（SSE 里出现起草正文）', FAKE_BODY in obs['text'],
         '没出货 —— 用户被卡在停问那一格出不去'),
    ]


# ---------------------------------------------------------------- 负向对照（合成桩）

def stub(has_needs=True, asked=True, text=''):
    return {'has_needs_frame': has_needs, 'needs': [{'name': 'subject'}] if has_needs else [],
            'text': text, 'asked': asked, 'done': {'asked': 'true' if asked else 'false'},
            'frames': 3}


NEW_ASK = ('你这次说的是「帮我写一份会议纪要」，但这条消息里没有可用的成篇材料（我这边只收到 9 字指令），'
           '直接动笔就只能替你编事实。\n\n要动手还缺：**会议主题、参会人**。\n\n'
           '把这些信息贴给我，或者回一句「就按你的」，我就按通用写法先起一稿。')


def selfcheck():
    def zero(obs, reqs):
        return judge_zero_round(obs, MSG_ZERO) + judge_call_budget(reqs)

    # (名字, 判据组, 观测桩, 本轮调用, 期望是否全绿)
    cases = [
        ('零材料轮：停问 + 逐字引用 + 无编造 + 只花 1 次调用 → 全绿',
         zero, stub(text=NEW_ASK), [{'kind': 'classify'}], True),
        ('旧行为：没停问、直接出厂编造正文 → 必须红',
         zero, stub(has_needs=False, asked=False, text=FAKE_BODY + '：8月26日张三主持'),
         [{'kind': 'classify'}, {'kind': 'draft'}], False),
        ('停问但不引用原话（旧通用清单）→ 必须红',
         zero, stub(text='我需要你补充以下信息：会议主题、参会人。'), [{'kind': 'classify'}], False),
        ('停问这轮多花了一次 draft 调用 → 必须红',
         zero, stub(text=NEW_ASK), [{'kind': 'classify'}, {'kind': 'draft'}], False),
        ('授权轮：不停问且出货 → 全绿',
         judge_grant_round, stub(has_needs=False, asked=False, text=FAKE_BODY),
         [{'kind': 'classify'}, {'kind': 'draft'}], True),
        ('授权轮还在停问（问成死循环）→ 必须红',
         judge_grant_round, stub(has_needs=True, asked=True, text=NEW_ASK), [{'kind': 'classify'}], False),
    ]
    bad = 0
    for name, judge, obs, reqs, want_green in cases:
        rows = judge(obs, reqs) if judge is zero else judge(obs)
        reds = [n for n, ok, _ in rows if not ok]
        ok = (not reds) if want_green else (len(reds) > 0)
        print(('ok   ' if ok else 'FAIL ') + name + ('' if ok else ' ← 红=%s' % reds))
        bad += 0 if ok else 1
    print('--- %d/%d ok ---' % (len(cases) - bad, len(cases)))
    return 1 if bad else 0


# ---------------------------------------------------------------- 真机流程

def main():
    if '--selfcheck' in sys.argv:
        return selfcheck()
    if not BIN or not os.path.isfile(BIN) or not os.access(BIN, os.X_OK):
        print('SKIP 缺 BIN（当前代码编出来的二进制）：go build -o /tmp/skillforge-zg ./cmd/server && '
              'BIN=/tmp/skillforge-zg python3 %s' % sys.argv[0])
        return 2
    os.makedirs(DATADIR, exist_ok=True)
    fake = FakeLLM()
    app_port = free_port()
    base = 'http://127.0.0.1:%d' % app_port
    env = dict(os.environ,
               SKILLFORGE_ADDR='127.0.0.1:%d' % app_port,
               SKILLFORGE_DATA_DIR=DATADIR,
               SKILLFORGE_DB=os.path.join(DATADIR, 'skillforge.db'),
               SKILLFORGE_ADMIN_USER=ADMIN_U,
               SKILLFORGE_ADMIN_PASS=ADMIN_P,
               SKILLFORGE_JWT_SECRET=PLACEHOLDER,
               SKILLFORGE_BASE_URL=base)
    env.pop('SKILLFORGE_LLM_API_KEY', None)   # 真密钥一律不用：模型走 DB 配置里的假模型
    log = open(os.path.join(DATADIR, 'server.log'), 'w')
    srv = subprocess.Popen([BIN], env=env, stdout=log, stderr=subprocess.STDOUT)
    print('二进制：%s\n数据目录：%s\n假模型：127.0.0.1:%d  服务：%s' % (BIN, DATADIR, fake.port, base))

    def cleanup():
        srv.terminate()
        try:
            srv.wait(timeout=5)
        except Exception:
            srv.kill()
    try:
        # 就绪断言必须同时命中「本次端口」+「本次数据目录」：8099 被残留实例占着的旧事故
        # 就是靠这一行识别的（读到别人实例的产物 → 假红/假绿）。
        want = 'listening on 127.0.0.1:%d (data: %s)' % (app_port, DATADIR)
        for _ in range(60):
            if os.path.exists(os.path.join(DATADIR, 'server.log')) and want in open(
                    os.path.join(DATADIR, 'server.log')).read():
                break
            if srv.poll() is not None:
                print('✗ 服务退出，日志：')
                print(open(os.path.join(DATADIR, 'server.log')).read()[-2000:])
                return 1
            time.sleep(0.5)
        else:
            print('✗ 30s 内未确认服务归属（不接受打到别人实例上的结果）')
            return 1

        tok = post_json(base, '/api/login', {'username': ADMIN_U, 'password': ADMIN_P})['token']
        print('管理端登录 ✓')

        # 模型配置进 DB：极简创建走 store.GetActiveLLM()，不吃 env（这正是「线上核 DB」那条）
        cfg = post_json(base, '/api/admin/llms', {
            'provider': 'openai', 'model': 'fake-model',
            'base_url': 'http://127.0.0.1:%d' % fake.port,
            'api_key': PLACEHOLDER, 'is_active': True}, tok)
        cid = cfg.get('id') or (cfg.get('config') or {}).get('id')
        if cid:
            post_json(base, '/api/admin/llms/active', {'id': cid}, tok)
        print('假模型已配为当前 LLM ✓ id=%s' % cid)

        # ---- ① 极简创建：一份指南 + 两篇范文 ----
        guide = ('# 会议纪要写作指南\n\n'
                 '1. 首段写清会议主题、时间、地点、参会人。\n'
                 '2. 决议事项必须写清责任人与完成时限，缺一不可。\n'
                 '3. 不得描写会议气氛，不得出现主观评价。\n'
                 '4. 不得出现素材里没有的日期、人名和数字。\n'
                 '5. 全篇 400~800 字。\n' * 3)
        examples = ('---\n# 示例纪要一\nQ3 复盘会于 9 月 1 日在三楼会议室召开，产品与研发共 12 人参加。'
                    '会议决定：由张工在 9 月 20 日前完成接口对齐。\n'
                    '---\n# 示例纪要二\n安全例会于 9 月 8 日召开，安全组 6 人参加。'
                    '会议决定：由李四在 9 月 30 日前完成隐患排查。\n')
        n0 = fake.mark()
        resp = post_multipart(base, '/api/admin/train/lite',
                              {'guide': guide, 'examples': examples}, tok)
        lite = read_train_sse(resp)
        if not check('L1 极简创建成功（done 帧带 slug）', bool(lite['done'].get('slug')),
                     'error=%r 帧类型=%s 步骤=%s' % (lite['error'], lite['types'], lite['steps'][-3:])):
            return report()
        slug = lite['done']['slug']
        fake.slug = slug
        print('   极简创建产出技能：%s（模型调用 %d 次）' % (slug, len(fake.since(n0))))
        check('L2 极简创建这一步只花 1 次模型调用（内网 token 慢）',
              [r['kind'] for r in fake.since(n0)] == ['lite'],
              [r['kind'] for r in fake.since(n0)])

        # ---- ② 零材料一轮：一句话指令，分类器一条 needs 都不报 ----
        sid = 'zg-' + uuid.uuid4().hex[:8]
        n1 = fake.mark()
        obs = read_sse(chat(base, sid, MSG_ZERO))
        print('   零材料轮：asked=%s 正文长度=%d' % (obs['asked'], len(obs['text'])))
        print('   停问文案（用户看到的原文）：%s' % obs['text'][:220].replace('\n', ' ⏎ '))
        for name, ok, extra in judge_zero_round(obs, MSG_ZERO) + judge_call_budget(fake.since(n1)):
            check(name, ok, extra)

        # ---- ③ 授权出口一轮：必须出货，不许问成死循环 ----
        n2 = fake.mark()
        obs2 = read_sse(chat(base, sid, MSG_GRANT))
        print('   授权轮：asked=%s 正文长度=%d 本轮调用=%s'
              % (obs2['asked'], len(obs2['text']), [r['kind'] for r in fake.since(n2)]))
        for name, ok, extra in judge_grant_round(obs2):
            check(name, ok, extra)
        return report()
    finally:
        cleanup()


def chat(base, sid, msg, skill=None):
    body = {'session_id': sid, 'message': msg, 'mode': 'manual'}
    if skill:
        body['skill'] = skill
    r = urllib.request.Request(base + '/api/chat', data=json.dumps(body).encode(),
                               headers={'Content-Type': 'application/json'})
    return urllib.request.urlopen(r, timeout=300)


def report():
    print('--- %d/%d ok ---' % (checks - len(fails), checks))
    if fails:
        print('FAILED: ' + '; '.join(fails))
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
