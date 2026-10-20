#!/usr/bin/env python3
"""极端情况压力测试：上下文不能丢 + 自检不能自相矛盾。

用法：python3 ext_ctx_stress.py <BASE> [用例名筛选]

每条用例都会真调 /api/chat（多轮），把每轮回答收集起来做断言。
断言尽量做成**可机判**的判据（关键词/数字是否在、是否矛盾），
而不是「看起来还行」—— 那是人眼的标准，不是防线的标准。
"""
import json
import sys
import time
import urllib.request
import urllib.error

BASE = sys.argv[1] if len(sys.argv) > 1 else ''
ONLY = sys.argv[2:] or None
RUN_ALL = bool(BASE)

ENV = {}
for ln in open('/opt/skillforge/skillforge.env'):
    ln = ln.strip()
    if ln and not ln.startswith('#') and '=' in ln:
        k, v = ln.split('=', 1)
        ENV[k] = v.strip().strip('"').strip("'")

TOK = json.load(urllib.request.urlopen(urllib.request.Request(
    BASE + '/api/login',
    data=json.dumps({'username': ENV['SKILLFORGE_ADMIN_USER'],
                     'password': ENV['SKILLFORGE_ADMIN_PASS']}).encode(),
    headers={'Content-Type': 'application/json'}), timeout=30))['token']


def _docx_text(raw):
    """把 docx 的正文抽成纯文本（含表格单元格），用于断言交付物内容。"""
    import io as _io
    import zipfile
    from xml.etree import ElementTree as ET
    W = '{http://schemas.openxmlformats.org/wordprocessingml/2006/main}'
    try:
        with zipfile.ZipFile(_io.BytesIO(raw)) as z:
            root = ET.fromstring(z.read('word/document.xml'))
    except Exception:
        return ''
    lines = []
    for p in root.iter(W + 'p'):
        t = ''.join(n.text or '' for n in p.iter(W + 't')).strip()
        if t:
            lines.append(t)
    return '\n'.join(lines)


def _fetch(url):
    """下载交付物并抽成文本（docx 抽正文；其它按文本读）。"""
    r = urllib.request.Request(BASE + url, headers={'Authorization': 'Bearer ' + TOK})
    with urllib.request.urlopen(r, timeout=120) as x:
        raw = x.read()
    if raw[:2] == b'PK':
        return _docx_text(raw)
    try:
        return raw.decode('utf-8', 'replace')
    except Exception:
        return ''


def turn(sid, msg, mode=None, timeout=900):
    """发一轮消息，返回 (聊天可见文本 + 所有交付文件的正文)。

    为什么要把交付物正文并进来：走 docgen 路径时 delta 流里只有一句
    「已为您生成《…》，点击下载」，正文全在文件里。只看聊天文本会得出
    「AI 忘了我说过什么」的错误结论 —— 实际上信息在文档里，只是没在屏幕上。
    断言必须落在**用户真正拿到的东西**上。
    """
    body = {'message': msg, 'session_id': sid}
    if mode:
        body['mode'] = mode
    r = urllib.request.Request(
        BASE + '/api/chat', data=json.dumps(body).encode(),
        headers={'Content-Type': 'application/json', 'Authorization': 'Bearer ' + TOK})
    with urllib.request.urlopen(r, timeout=timeout) as x:
        sse = x.read().decode('utf-8', 'replace')
    text, files, cur = [], [], ''
    for ln in sse.splitlines():
        ln = ln.strip()
        if ln.startswith('event: '):
            cur = ln[7:]
            continue
        if not ln.startswith('data: '):
            continue
        try:
            d = json.loads(ln[6:])
        except Exception:
            continue
        if cur == 'delta' and isinstance(d, dict):
            text.append(d.get('t', ''))
        elif cur == 'file' and isinstance(d, dict) and d.get('url'):
            files.append((d.get('name', ''), d.get('url')))
    body_text = ''
    for name, url in files:
        try:
            body_text += '\n【交付文件 ' + name + '】\n' + _fetch(url)
        except Exception as e:
            body_text += '\n【交付文件 ' + name + ' 读取失败】' + str(e)
    return ''.join(text) + body_text


RESULTS = []


def check(name, cond, detail=''):
    RESULTS.append((name, bool(cond), detail))
    print(('  ok   ' if cond else '  FAIL ') + name + ((' — ' + detail) if (detail and not cond) else ''))


def has(text, *words):
    """text 里是否至少出现 words 中的 N 个（默认全部）。"""
    return all(w in text for w in words)


def has_any(text, *words):
    return any(w in text for w in words)


# ══════════════════════════════════════════════════════════════
# A. 上下文：极短补充 / 指代 / 多轮累积
# ══════════════════════════════════════════════════════════════

def case_a1_ultra_short():
    """一个词的补充，不能变成新题目。"""
    print('\n[A1] 极短补充（人名）')
    sid = 'ext-a1-' + str(int(time.time()))
    turn(sid, '写个员工培训通知，讲师和时间我还没定')
    a = turn(sid, '张老师')
    check('A1 仍围绕「培训通知」，没变成人物写作',
          has(a, '培训') and not has(a, '人物素描', '人物特写', '张老师的故事'), a[:200])
    check('A1 把张老师当讲师用上', '张老师' in a, a[:200])


def case_a2_date_only():
    print('\n[A2] 极短补充（日期）')
    sid = 'ext-a2-' + str(int(time.time()))
    turn(sid, '帮我起草一份公司年会通知')
    a = turn(sid, '1月18号')
    check('A2 仍围绕「年会通知」', has(a, '年会') and has_any(a, '通知', '年会'), a[:200])
    # 模型可能写成「1月18日」「1 月 18 日」（带空格）或「一月十八日」，都要算用上了。
    import re as _re
    date_ok = bool(_re.search(r'1\s*月\s*18', a)) or ('一月十八' in a)
    check('A2 日期被用上', date_ok, a[:300])


def case_a3_pronoun():
    print('\n[A3] 指代（把它改成…）')
    sid = 'ext-a3-' + str(int(time.time()))
    turn(sid, '写一份关于办公室搬迁的通知，新址在朝阳区建国路88号')
    a = turn(sid, '把新址改成海淀区中关村大街1号')
    check('A3 仍然在写搬迁通知', has_any(a, '搬迁', '迁址', '地址变更', '新址'), a[:300])
    check('A3 用上了新地址', '中关村' in a, a[:300])


def case_a4_accumulate():
    print('\n[A4] 三轮累积：每轮补一个要素，最后都要在')
    sid = 'ext-a4-' + str(int(time.time()))
    turn(sid, '写个项目启动会通知')
    turn(sid, '时间是3月5日上午十点')
    a = turn(sid, '地点在B座201会议室')
    check('A4 第三轮仍记得是「启动会」', '启动会' in a, a[:300])
    check('A4 第一轮补的时间还在', has_any(a, '3月5', '3月5日'), a[:400])
    check('A4 本轮地点在', 'B座201' in a or '201' in a, a[:400])


def case_a5_topic_switch():
    print('\n[A5] 明确换题（要跟随新题目，不能被旧上下文绑死）')
    sid = 'ext-a5-' + str(int(time.time()))
    turn(sid, '写个员工培训通知')
    a = turn(sid, '算了，改写一个端午节放假通知')
    check('A5 跟随新题目（出现端午）', '端午' in a, a[:300])
    check('A5 没有继续写培训', not (('培训' in a) and ('端午' not in a)), a[:300])


# ══════════════════════════════════════════════════════════════
# B. 自检：不能自相矛盾
# ══════════════════════════════════════════════════════════════

def split_draft_and_review(a):
    """把回答拆成「成稿」和「自检附注」两段。"""
    for marker in ('**自检未通过**', '自检未通过', '—— 自检'):
        if marker in a:
            i = a.index(marker)
            return a[:i], a[i:]
    return a, ''


def case_b1_review_sees_history():
    """多轮补充后自检，不能拿「张老师」三个字去审一整篇通知。"""
    print('\n[B1] 自检要看到完整需求（多轮后）')
    sid = 'ext-b1-' + str(int(time.time()))
    turn(sid, '写个员工培训通知，讲师和时间我还没定', mode='quick')
    a = turn(sid, '张老师', mode='quick')
    draft, review = split_draft_and_review(a)
    if review:
        # 自检若把「张老师」当成全部需求，就会出现「用户没说要写通知」这类判断
        bad = has_any(review, '用户未说明要写', '需求不明确', '缺少主题', '未提供写作目标')
        check('B1 自检没有把「张老师」当成全部需求', not bad, review[:300])
    else:
        check('B1 自检未报不通过（或未触发）', True, '')


def case_b2_no_repeat_ask():
    """自检追问过、用户答了，下一轮不能再问同一件事。"""
    print('\n[B2] 补充过的信息不能再追问')
    sid = 'ext-b2-' + str(int(time.time()))
    a1 = turn(sid, '写个员工培训通知，讲师和时间我还没定', mode='quick')
    a2 = turn(sid, '张老师，5月20日下午两点', mode='quick')
    draft, review = split_draft_and_review(a2)
    check('B2 第二轮不再追问讲师', not has_any(review, '讲师是谁', '请提供讲师'), review[:300])
    check('B2 第二轮不再追问时间', not has_any(review, '时间定在', '请提供时间', '培训时间'), review[:300])


def case_b3_contradiction():
    """自检提出的「不足」不能与它自己的成稿矛盾。"""
    print('\n[B3] 自检结论与成稿不自相矛盾')
    sid = 'ext-b3-' + str(int(time.time()))
    a = turn(sid, '写一份员工请假制度说明，要求分条说明审批流程', mode='quick')
    draft, review = split_draft_and_review(a)
    if review:
        # 常见矛盾：说「没有分条/没有流程」，但成稿里明明有一二三条
        if has_any(review, '未分条', '没有分条', '未包含审批流程', '缺少审批流程'):
            contradicted = has_any(draft, '一、', '1.', '第一条', '一）')
            check('B3 自检说「没分条/没流程」时成稿里确实没有', not contradicted, review[:300])
        else:
            check('B3 未出现矛盾判断', True, '')
    else:
        check('B3 自检通过', True, '')


# ══════════════════════════════════════════════════════════════
# C. 会话持久化：重启/续会话不能丢
# ══════════════════════════════════════════════════════════════

def case_c1_same_sid_revisit():
    print('\n[C1] 同一 session 隔一段时间再来，仍记得前情')
    sid = 'ext-c1-' + str(int(time.time()))
    turn(sid, '我在写一份关于仓库盘点的通知，主题是年度库存清查')
    time.sleep(2)
    a = turn(sid, '加上联系人：王主管')
    check('C1 仍记得「库存清查/盘点」', has_any(a, '盘点', '库存', '清查'), a[:300])


def case_c2_empty_and_noise():
    print('\n[C2] 空消息/噪声不破坏后续上下文')
    sid = 'ext-c2-' + str(int(time.time()))
    turn(sid, '写一份消防安全演练通知')
    try:
        turn(sid, '')
    except Exception as e:
        print('    (空消息返回异常，忽略)', type(e).__name__)
    a = turn(sid, '演练时间定在周五下午')
    check('C2 空消息之后仍记得「消防演练」', has_any(a, '消防', '演练'), a[:300])


def case_c3_long_supplement():
    print('\n[C3] 很长的一段补充（素材量级）')
    sid = 'ext-c3-' + str(int(time.time()))
    turn(sid, '写一份部门年度总结')
    long_text = '补充材料：' + '本年度完成项目十二个，累计交付文档三百份。' * 40
    a = turn(sid, long_text)
    check('C3 长补充后仍围绕「年度总结」', has_any(a, '年度总结', '总结'), a[:300])


def case_d1_long_session_trim():
    """长会话（超过内存窗口 maxHist=40 条）后，早期要求不能丢。"""
    print('\n[D1] 长会话裁剪边界（20 轮闲聊后再补信息）')
    sid = 'ext-d1-' + str(int(time.time()))
    turn(sid, '我在写一份关于固定资产盘点的通知，主题是年度资产清查')
    for i in range(20):
        turn(sid, f'补充一条无关信息：第{i+1}号事项已确认')
    a = turn(sid, '联系人写财务部李经理')
    check('D1 20 轮之后仍记得「资产清查」', has_any(a, '资产', '清查', '盘点'), a[:300])


def case_d2_placeholder_consistency():
    """同一份成稿上，不能既说「占位符太多」又说「该占位没占位」。"""
    print('\n[D2] 自检判词不自相矛盾（占位符口径）')
    # 这条输入带明确要求，能稳定触发自检不通过（否则两条用例都在「自检通过」分支空跑）
    sid = 'ext-d2-' + str(int(time.time()))
    a = turn(sid, '写一份公司年会通知，要包括时间地点议程和联系人，语气正式一点，字数500字左右，'
                  '不要用「好的」这种开场白', mode='quick')
    draft, review = split_draft_and_review(a)
    if not review:
        check('D2 自检通过（无附注，无需判定）', True, '')
        return
    too_many = has_any(review, '占位符太多', '大量【待补充】', '包含大量占位')
    missing = has_any(review, '未按要求使用【待补充】', '未标注占位符', '缺少占位符')
    check('D2 没有同时给出「占位符太多」和「缺少占位符」两种互斥判词',
          not (too_many and missing), review[:300])


def case_d3_ask_not_reused():
    """自检的多条 ask 不能全都一样（那样的条子没法执行）。"""
    print('\n[D3] 自检的 ask 不复用')
    import re as _re
    # 自检出几条是随机的：多试几次，尽量拿到「两条以上」的样本，否则这条用例等于没跑。
    review = ''
    for _ in range(3):
        sid = 'ext-d3-' + str(int(time.time())) + '-' + str(_)
        a = turn(sid, '写一份公司年会通知，要包括时间地点议程和联系人，语气正式一点，字数500字左右，'
                      '不要用「好的」这种开场白', mode='quick')
        draft, review = split_draft_and_review(a)
        if len(_re.findall(r'需要你补充\*\*[：:]\s*(.+)', review)) >= 2:
            break
    asks = _re.findall(r'需要你补充\*\*[：:]\s*(.+)', review)
    if len(asks) >= 2:
        check('D3 多条 ask 不全相同', len(set(asks)) > 1, str(asks))
        # 文风类不足不该塞一个「请提供…」的问题（用户没法用这个回答修掉开场白）
        style_items = [ln for ln in review.splitlines() if any(w in ln for w in ('开场白', '收尾语', '分条', '篇幅'))]
        bad = [ln for ln in style_items if '无需补充' not in ln and '需要你补充' in ln]
        check('D3 文风类条目不硬塞问题', not bad, str(bad)[:300])
    else:
        check('D3 ask 少于两条，无需判定', True, '')


def case_d4_both_modes():
    """两种模式都要在多轮里保持上下文。"""
    print('\n[D4] 写作 / 问答 两种模式的多轮上下文')
    sid = 'ext-d4a-' + str(int(time.time()))
    turn(sid, '写个部门周例会通知', mode='write')
    a = turn(sid, '时间定在每周一上午九点', mode='write')
    check('D4a 写作模式：第二轮仍记得「周例会」', has_any(a, '周例会', '例会'), a[:300])
    sid2 = 'ext-d4b-' + str(int(time.time()))
    turn(sid2, '差旅报销的住宿标准是多少？')
    a2 = turn(sid2, '那经理级别呢？')
    check('D4b 问答模式：追问能理解指代', has_any(a2, '经理', '级别'), a2[:300])


CASES = [v for k, v in sorted(globals().items()) if k.startswith('case_')]

# 作为库被 import（别的脚本要复用 turn()）时不自动跑全量用例
if __name__ == '__main__' and RUN_ALL:
  for fn in CASES:
      if ONLY and not any(o in fn.__name__ for o in ONLY):
          continue
      try:
          fn()
      except Exception as e:
          check(fn.__name__ + ' 执行异常', False, f'{type(e).__name__}: {e}')

  bad = [r for r in RESULTS if not r[1]]
  print('\n合计 %d 项，失败 %d 项' % (len(RESULTS), len(bad)))
  if bad:
      for n, _, d in bad:
          print('  FAIL:', n)
      sys.exit(1)
  print('全部通过')
