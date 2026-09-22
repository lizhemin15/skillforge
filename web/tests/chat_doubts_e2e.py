#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""线上验收：首轮疑点回执 —— 用户给的原话不许被「通用要素清单」吞掉。

用户诉求原文（2026-09-22）：
  「现在速度过于慢了，中间可以流式输出思考的一些中间材料，现在一直卡着计时，用户体验不佳」
  「有的时候似乎像是没看到我的信息一样的，还在问我要信息，要的时候也不是根据我目前提供
    的信息的基础上来进一步补充，而是直接通用的补充。」
  「用户对话给的信息可是重中之重」

被测行为（internal/api/chat.go 的停问分支 + internal/agent/doubts.go）：
  needs 闸门发现技能必填项「看起来没给」时，**不许**再照 input_params 拼一份通用要素清单
  问用户（旧实现就是这句，与用户这次说了什么完全无关 —— 投诉根源）。
  新行为只有两种合法结局：
    (a) 无疑点 / 引用对不上原文 / 这一跳超时 → 明说「无疑点，开始写」后**带假设直接起草**；
    (b) 有真疑点 → 停下问，但每条必须**逐字引用用户原文** + 带默认理解（用户回「就按你的」即可）。

判据（每条都可被真故障证伪，见 SELFCHECK）：
  Q1 旧通用清单签名不许出现（逐字取自 git 历史 b508947^ 的 needsMessage）。
  Q2 必须有可判读结局：要么流出正文，要么停问文案非空 —— 不许「既不问也不写」。
  Q3 不许「只有计时在跳」：首个有用帧（正文 / 旁白 / 停问）到达时间 ≤ TTFB_BUDGET。
  Q4a 我这条消息里的**随机锚串**必须出现在「停问引用」或「正文」之一。
      这条就是投诉的直接反面：旧行为下锚串两边都不在（没引用、也没写）。
  Q4b 文案里每个「」引用必须能在我的原话里逐字查到（防编造引用 —— 展示假引用比不展示更糟）。
  Q5（informed 腿专有）信息齐全时不许停下追问：结局必须是「写」。

负向对照（手动跑，不进 LIVE-LEGS；不需要服务、不需要模型）：
  SELFCHECK=1 python3 web/tests/chat_doubts_e2e.py
  6 个合成桩回放：老通用清单 / 编造引用 / 空结局 / 只有计时 必须精确转红，
  带引用停问 / 无疑点直写 必须全绿。任一不符合即 rc=1。
  **这是本尺子自己的前提**：没有它，"判据恒绿" 无法被排除。

# LIVE-LEGS: doubts-informed TIMEOUT_S=300 | doubts-vague TIMEOUT_S=300
# ↑ 线上验收 leg 声明。scripts/acceptance-live.sh 只认这一行来枚举要跑几条 leg；
#   web/tests/live_e2e_roster.test.mjs 守着它跟文件真身不许脱钩。
"""
import json
import os
import re
import sys
import time
import urllib.request
import uuid

BASE = os.environ.get('BASE', 'http://127.0.0.1:8092')
U = os.environ.get('SKILLFORGE_ADMIN_USER', '')
P = os.environ.get('SKILLFORGE_ADMIN_PASS', '')
SKILL = os.environ.get('SKILL', '公司新闻通稿')
LEG = os.environ.get('LEG', 'informed')
TIMEOUT_S = int(os.environ.get('TIMEOUT_S', '240'))
# 首个有用帧的预算。本地事实旁白（clock.Thinking）实测 <0.3s，疑点跳/写稿首帧 ~秒级；
# 20s 是「用户会不会觉得卡住」的体感线，不是实现细节线（别收到 5s 去卡模型方差）。
TTFB_BUDGET = float(os.environ.get('TTFB_BUDGET', '20'))
OUTDIR = os.environ.get('DOUBTS_LIVE_OUT', '/tmp/doubts_live')

# 旧版停问文案的逐字签名（git show b508947^:internal/api/chat.go 的 needsMessage）。
# 之所以钉逐字不给正则：这条判据的唯一目的是「旧行为不许回来」，模糊匹配会把
# 「新行为恰好也说了一句『补充』」判红，也会把「旧行为换了标点」判绿。
OLD_LIST_SIGS = ('我需要你补充以下信息', '我需要你补充一些信息', '要开始写作，我需要你')

ANCHOR = os.environ.get('ANCHOR') or ('锚' + uuid.uuid4().hex[:6])

# informed：一句话把技能**四个必填项**（公司名称/发布日期/核心事件/具体数据）全给全 ——
# 信息齐了还拦人，就是投诉复现。锚串要求原样出现在正文，用来判「首轮原话到底被没被吃到」。
def msg_informed(anchor):
    """informed 腿的「我方原话」。锚串做成参数而不是直接拼模块级常数 ——
    否则 SELFCHECK 的桩没法用固定锚（随机锚一进桩，桩里的引用必然对不上原文，
    自检会红在一个假问题上，反而掩盖真判据）。"""
    return (
        f'帮我写篇新闻通稿：星禾云桥科技（编号 {anchor}）2026年9月22日发布数据中台3.0，'
        f'当天签约客户128家、覆盖城市36个、续约率91.5%。'
        f'正文第一段必须原样出现公司名「星禾云桥科技」和编号「{anchor}」（一字不许改）。'
    )


MSG_VAGUE = '帮我写个新闻稿。'
MSG_INFORMED = msg_informed(ANCHOR)

# leg 名带 `doubts-` 前缀是给 runner 的 `ONLY=doubts` 用的（一次挑中两条腿）；
# 判据里只认家族名（informed / vague），免得前缀一改判据就跟着瞎。
FAMILY = LEG.split('-')[-1] if LEG.split('-')[-1] in ('informed', 'vague') else 'informed'

MSG = {'informed': MSG_INFORMED, 'vague': MSG_VAGUE}[FAMILY]

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


def report():
    print(f'--- {checks - len(fails)}/{checks} ok ---')
    if fails:
        print('FAILED: ' + '; '.join(fails))
        return 1
    return 0


# ---------------------------------------------------------------- 判据本体

def norm(s):
    """引文/锚串比对前的归一化：剥掉所有空白（含全角空格、换行、零宽）。

    跟 Go 侧 agent.normForQuote 同一口径，且**只**剥空白、不动标点 ——
    动标点会让「引用对不上」变成「引用对一半」，那就不是逐字引用了。
    """
    return re.sub(r'[\s\u3000\u200b]+', '', s or '')


def quoted_spans(text):
    """取出文案里所有 「…」 引用。新文案形态：你说「<原文片段>」——我理解成：…"""
    return [m.strip() for m in re.findall(r'「([^」]{1,80})」', text or '')]


def judge_turn(leg, msg, asked, ask_text, body_text, notes, first_useful_s, anchor):
    """把一轮的观测值判成断言。返回 [(名字, 是否通过, 附注)] —— 纯函数，SELFCHECK 直接复用。"""
    out = []
    both = norm(ask_text) + '\x00' + norm(body_text)

    # Q1 旧通用清单不许回来
    hit = [s for s in OLD_LIST_SIGS if s in (ask_text or '')]
    out.append(('Q1 停问文案里没有旧版通用要素清单（逐字）', not hit, f'命中={hit}' if hit else ''))

    # Q2 结局可判读
    has_body = len(norm(body_text)) >= 50
    has_ask = bool(norm(ask_text))
    out.append(('Q2 有可判读结局（流出正文 或 停问文案非空）',
                has_body or has_ask,
                f'body={len(norm(body_text))} ask={len(norm(ask_text))} asked={asked}'))

    # Q3 不是「只有计时在跳」
    out.append((f'Q3 首个有用帧 ≤ {TTFB_BUDGET:.0f}s（屏幕不是只剩计时）',
                first_useful_s is not None and first_useful_s <= TTFB_BUDGET,
                f'首个有用帧={first_useful_s}s'))

    # Q4a 随机锚命中（informed 腿）：引用里或正文里，必须有一处看得到我的原话内容。
    if anchor:
        ok = norm(anchor) in both
        out.append((f'Q4a 我原话里的随机锚「{anchor}」出现在引用或正文里',
                    ok, '锚串两边都没出现 —— 这说明首轮原话被吞了（投诉复现）'))
    # Q4b 引用必须逐字来自我的原话
    bad = []
    hay = norm(msg)
    for q in quoted_spans(ask_text):
        if norm(q) not in hay:
            bad.append(q)
    out.append(('Q4b 文案里的每条「」引用都能在我的原话里逐字查到',
                not bad, f'编造的引用={bad}'))

    # Q5 informed 腿：四个必填项都给了还停下追问 = 投诉复现
    if leg == 'informed':
        out.append(('Q5 信息齐全时结局是「写」而不是追问',
                    not asked, f'asked={asked}，停问文案={norm(ask_text)[:60]}'))
    return out


# ---------------------------------------------------------------- 桩回放负向对照

def _turn(**kw):
    d = dict(leg='vague', msg=MSG_VAGUE, asked=False, ask_text='', body_text='', notes='',
             first_useful_s=1.0, anchor='')
    d.update(kw)
    return d


SC_ANCHOR = 'zz9999'
SC_MSG = msg_informed(SC_ANCHOR)
SC_CITED_ASK = ('你说「星禾云桥科技（编号 zz9999）2026年9月22日发布数据中台3.0」——'
                '我理解成：要发通稿（影响到：标题）')

SELFCHECK_CASES = [
    # (名字, 桩, 期望红的判据子串, 期望是否全绿)
    ('老通用清单（b508947^ 的 needsMessage 逐字）必须转红',
     _turn(asked=True, ask_text='要开始写作，我需要你补充以下信息：公司名称、发布日期、核心事件概述。你可以直接告诉我。'),
     'Q1', False),
    ('编造引用（原文里没有的片段）必须转红',
     _turn(msg=SC_MSG, asked=True,
           ask_text='你说「客户要求三天内交付双语版本」——我理解成：对外发布（影响到：语气）'),
     'Q4b', False),
    ('空结局（既不问也不写）必须转红',
     _turn(asked=False, body_text='', notes=''),
     'Q2', False),
    ('只有计时在跳（首个有用帧 45s）必须转红',
     _turn(asked=False, body_text='正文' * 60, first_useful_s=45.0),
     'Q3', False),
    ('informed：信息齐全却停下追问必须转红',
     _turn(leg='informed', msg=SC_MSG, asked=True, anchor=SC_ANCHOR, ask_text=SC_CITED_ASK),
     'Q5', False),
    ('带原文引用的停问是全绿（合法的 (b) 结局）',
     _turn(msg=SC_MSG, asked=True, ask_text=SC_CITED_ASK),
     '', True),
    ('无疑点直写 + 锚命中是全绿（合法的 (a) 结局）',
     _turn(leg='informed', msg=SC_MSG, asked=False, anchor=SC_ANCHOR,
           body_text='星禾云桥科技（编号 zz9999）2026年9月22日发布数据中台3.0，' + '正文' * 120),
     '', True),
    # 负向前提：informed 腿里锚串两边都没出现 —— 这正是投诉现场，必须转红
    ('informed：锚串既不在引用也不在正文（投诉现场）必须转红',
     _turn(leg='informed', msg=SC_MSG, asked=False, anchor=SC_ANCHOR,
           body_text='日前，某科技公司发布了新一代数据中台产品。' * 10),
     'Q4a', False),
]


def selfcheck():
    bad = 0
    for name, turn, want_fail, want_all_green in SELFCHECK_CASES:
        global fails, checks
        fails, checks = [], 0
        rows = judge_turn(turn['leg'], turn['msg'], turn['asked'], turn['ask_text'],
                          turn['body_text'], turn['notes'], turn['first_useful_s'], turn['anchor'])
        reds = [n for n, ok, _ in rows if not ok]
        if want_all_green:
            ok = not reds
            why = f'期望全绿，实际红={reds}'
        else:
            ok = len(reds) == 1 and want_fail in reds[0]
            why = f'期望精确红在 {want_fail}，实际红={reds}'
        # 每格都打，便于 CI/CI 外一眼看出是哪一格塌了
        print(('ok   ' if ok else 'FAIL ') + name + ('' if ok else ' ← ' + why))
        if not ok:
            bad += 1
    print(f'--- {len(SELFCHECK_CASES) - bad}/{len(SELFCHECK_CASES)} ok ---')
    if bad:
        print('FAILED: SELFCHECK —— 本尺子的判据与期望不一致，先修脚本再谈线上')
    return 1 if bad else 0


# ---------------------------------------------------------------- 线上观测

USEFUL_EVENTS = ('delta', 'meta', 'needs', 'trace', 'skill', 'file')


def post(path, obj, tok=None, timeout=30):
    req = urllib.request.Request(BASE + path, data=json.dumps(obj).encode(),
                                 headers={'Content-Type': 'application/json'})
    if tok:
        req.add_header('Authorization', 'Bearer ' + tok)
    return urllib.request.urlopen(req, timeout=timeout)


def read_turn(sid, msg, tok, raw_log):
    """发一轮，逐帧读回；返回观测值。**观测与判据分离**，方便 SELFCHECK 直接喂桩。"""
    body = {'session_id': sid, 'message': msg, 'mode': 'manual', 'skill': SKILL}
    req = urllib.request.Request(BASE + '/api/chat', data=json.dumps(body).encode(),
                                 headers={'Content-Type': 'application/json'})
    req.add_header('Authorization', 'Bearer ' + tok)
    t0 = time.time()
    resp = urllib.request.urlopen(req, timeout=TIMEOUT_S)

    ev, ask, body_text, notes, first_useful, asked, nframes = '', [], [], [], None, False, 0
    deltas = []
    for raw in resp:
        line = raw.decode('utf-8', 'replace').rstrip('\n')
        if not line:
            continue
        raw_log.write(f'[{time.time() - t0:7.2f}s] {line}\n')
        raw_log.flush()
        if line.startswith('event:'):
            ev = line[6:].strip()
            continue
        if not line.startswith('data:'):
            continue
        try:
            data = json.loads(line[5:].strip())
        except Exception:
            continue
        nframes += 1
        if first_useful is None and ev in USEFUL_EVENTS:
            first_useful = round(time.time() - t0, 2)
        if ev == 'delta':
            deltas.append(data.get('t', ''))
        elif ev == 'meta':
            if data.get('note'):
                notes.append(data['note'])
        elif ev == 'done':
            asked = str(data.get('asked', '')) == 'true'
    ask_text = ''.join(deltas) if asked else ''
    body_text = '' if asked else ''.join(deltas)
    return dict(asked=asked, ask_text=ask_text, body_text=body_text,
                notes=' | '.join(notes), first_useful_s=first_useful, nframes=nframes)


def main():
    if os.environ.get('SELFCHECK') == '1':
        return selfcheck()
    if not U or not P:
        print('SKIP 缺 SKILLFORGE_ADMIN_USER / SKILLFORGE_ADMIN_PASS（先 set -a; . /opt/skillforge/skillforge.env）')
        return 0
    try:
        tok = json.loads(post('/api/login', {'username': U, 'password': P}).read())['token']
    except Exception as e:
        print(f'SKIP 登录失败（服务没起？）：{str(e)[:140]}')
        return 0

    os.makedirs(OUTDIR, exist_ok=True)
    raw_path = os.path.join(OUTDIR, f'{LEG}.sse.txt')
    sid = f'doubts-{LEG}-{uuid.uuid4().hex[:8]}'
    print(f'线上验收 leg={LEG}  skill={SKILL}  session={sid}  锚={ANCHOR or "（无）"}')
    print(f'我方原话（{len(MSG)} 字）：{MSG}')
    t0 = time.time()
    with open(raw_path, 'w') as log:
        try:
            obs = read_turn(sid, MSG, tok, log)
        except Exception as e:
            print(f'SKIP 这一轮没能读完（服务/模型异常）：{str(e)[:160]}')
            return 0
    print(f'轮次用时 {time.time() - t0:.1f}s  帧数={obs["nframes"]}  asked={obs["asked"]}')
    print(f'附注={obs["notes"] or "（无）"}')
    print(f'{"停问文案" if obs["asked"] else "正文前 120 字"}={norm(obs["ask_text"] or obs["body_text"])[:120]}')
    print(f'原始帧落盘：{raw_path}')

    rows = judge_turn(FAMILY, MSG, obs['asked'], obs['ask_text'], obs['body_text'],
                      obs['notes'], obs['first_useful_s'], ANCHOR if FAMILY == 'informed' else '')
    for name, ok, extra in rows:
        check(name, ok, extra)
    if obs['notes']:
        # 旁白/附注是「屏幕上有没有东西在动」的正面证据，单独记一笔（不算判据）
        print(f'note（非判据，仅留证）：{obs["notes"]}')
    return report()


if __name__ == '__main__':
    sys.exit(main())
