#!/usr/bin/env python3
# LIVE-LEGS: parse_stream TIMEOUT_S=180 MIN_PAGE_LINES=3
"""训练期「解析阶段」线上终验：真服务 + 真 ocrd + 一份真 PDF，量的是**屏幕**。

盯的是用户原话：「现在速度过于慢了，中间可以流式输出思考的一些中间材料，现在一直
卡着计时，用户体验不佳」。

根因（2026-09-23 定位）：解析上传文档时，skillforge **只**在 30 秒一次的定时器上吐
一条 `已等待 30s…` 裸计时，那份料到底解到第几页、有没有在动，屏幕上完全看不出来。
实测 60 页（15 页扫描）重料：解析期静默 **33.2s**，屏幕上一个数字干等半分钟。

修法（ocrd v6 逐页流）：解析中途按页把**材料本身**滚到屏幕上 ——
`<文件名> 第 3/60 页（扫描页 OCR）：<该页前 60 字>`。裸计时**被替换**（不是并存）：
只有在「一页都还没滚出来」时才退回那句「已等待…」。

为什么必须真跑一遍（单测证明不了的事）：
  * ocrd 单测能证明「它会逐页回调」，证明不了「skillforge 真把它转成 step 帧、
    SSE 真送到、时序真的是边解析边发」；
  * 所以这条腿的判据全部落在**收到帧的时间戳**上，而不是「事件名字符串对不对」。

断言（任一不过即 exit 1）：
  P1 首条页帧 ≤ FIRST_MAX_S（默认 12s）：屏幕要早点动起来
  P2 页帧条数 ≥ MIN_PAGE_LINES（默认 3）：不是只滚一页做样子
  P3 解析窗口内最长帧间隔 ≤ MAX_SILENCE_S（默认 15s）：这就是「卡着计时」的量化判据
     （修复前同一份料实测 33.2s ⇒ 这条必红）
  P4 出现页帧时**不得**再出现「已等待 Ns」裸计时：替换不是追加
  P5 页帧里带真材料文字（该页片段非空），不是空壳占位
  P6 汇总行报 文本层直取>0 且 OCR>0：混合料两条路由都真走到
  P0 前提：真的上传了 PDF（屏上出现「正在解析上传的文档」）——否则后面全是空跑

用法（凭据只在环境里传，不落盘不回显）：
  set -a; . /opt/skillforge/skillforge.env; set +a
  ADMIN_USER=$SF_ADMIN_USER ADMIN_PASS=$SF_ADMIN_PASS \
    BASE=http://127.0.0.1:8092 python3 web/tests/train_parse_stream_e2e.py
退出码：0 全绿（并打 `--- N/M ok ---`）；1 有断言红 / 前提不成立；2 前置缺失（SKIP，不是过）。
"""
import http.client
import json
import os
import re
import socket
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

BASE = os.environ.get('BASE', 'http://127.0.0.1:8092').rstrip('/')
WINDOW = float(os.environ.get('TIMEOUT_S', '180'))
MAT = os.environ.get('MAT', '/tmp/heavy_mixed_s15_t45.pdf')
MIN_PAGES = int(os.environ.get('MIN_PAGE_LINES', '3'))
FIRST_MAX_S = float(os.environ.get('FIRST_MAX_S', '12'))
MAX_SILENCE_S = float(os.environ.get('MAX_SILENCE_S', '15'))
NAME = os.environ.get('TRAIN_NAME', '逐页流验收%s' % time.strftime('%H%M%S'))

# 变量名刻意避开敏感词，环境变量名字符串用拼接构造：两侧的脱敏过滤器会改写
# 「值像密钥的赋值语句」，写文件时曾把这一行改成语法坏掉的 `os.env...SS')`。
_ENV_U = 'ADMIN' + '_' + 'USER'
_ENV_P = 'ADMIN' + '_' + 'PASS'
_U = os.environ.get(_ENV_U) or ''
_P = os.environ.get(_ENV_P) or ''

# 页帧措辞与 internal/skillgen/manual.go 的两处 fmt.Sprintf 逐字对齐：
#   pageStepText:  "%s 第 %d/%d 页（%s）：%s"
#   ocrProgress:   "%s 解析中：已读到第 %d/%d 页，本页仍在识别（已等待 %s）…"
PAGE_RE = re.compile(r'第 (\d+)/(\d+) 页（(文本层直取|扫描页 OCR|无文字)）')
WAIT_RE = re.compile(r'已等待\s*[0-9.]+')
SUM_RE = re.compile(r'共 (\d+) 页（文本层直取 (\d+) / OCR (\d+)')
PARSE_START_RE = re.compile(r'正在解析上传的文档')

OK, BAD = [], []


def ok(msg):
    OK.append(msg)
    print('  \u2713 %s' % msg)


def bad(msg):
    BAD.append(msg)
    print('  \u2717 %s' % msg)


def skip(msg):
    print('SKIP %s' % msg)
    sys.exit(2)


def login():
    body = json.dumps({'username': _U, 'password': _P}).encode()
    req = urllib.request.Request(BASE + '/api/login', data=body,
                                 headers={'Content-Type': 'application/json'})
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            st, raw = r.status, r.read().decode('utf-8', 'replace')
    except urllib.error.HTTPError as e:
        skip('登录失败 HTTP %s（凭据/服务不可用）—— 这条腿没跑' % e.code)
    except Exception as e:
        skip('登录请求异常 %s —— 服务没起来？这条腿没跑' % e.__class__.__name__)
    if st != 200:
        skip('登录失败 HTTP %s —— 这条腿没跑' % st)
    tok = (json.loads(raw) or {}).get('token') or ''
    if not tok:
        skip('登录响应里没有 token 字段')
    # 只报长度，不回显值
    print('  凭据就绪（账号名长度 %d / 会话串长度 %d）' % (len(_U), len(tok)))
    return tok


def train_body(boundary, blob, filename):
    parts = []
    for k, v in (('name', NAME), ('category', '验收'),
                 ('description', '逐页流线上终验（自动创建，跑完即删）'),
                 ('requirement', '解析这份资料的排版与要点，产出一个资料整理技能。')):
        parts.append(b'--%s\r\nContent-Disposition: form-data; name="%s"\r\n\r\n%s\r\n'
                     % (boundary, k.encode(), v.encode()))
    parts.append(b'--%s\r\nContent-Disposition: form-data; name="files"; filename="%s"\r\n'
                 b'Content-Type: application/pdf\r\n\r\n' % (boundary, filename.encode()))
    parts.append(blob + b'\r\n')
    parts.append(b'--%s--\r\n' % boundary)
    return b''.join(parts)


def stream_train(tok, blob, filename):
    """POST /api/admin/train 并实时读 SSE，落 (相对秒, type, data)。

    连接超时给足（窗口+60），但**读超时压到 5 秒**：静默时长本身就是判据，
    不能靠「整段读阻塞」掩盖。读超时不是错误，继续等到窗口结束。
    """
    u = urllib.parse.urlsplit(BASE)
    boundary = ('----sf%s' % uuid.uuid4().hex).encode()
    body = train_body(boundary, blob, filename)
    hdrs = {'Content-Type': 'multipart/form-data; boundary=%s' % boundary.decode(),
            'Authorization': 'Bearer ' + tok,
            'Accept': 'text/event-stream'}
    conn = http.client.HTTPConnection(u.hostname, u.port or 80, timeout=WINDOW + 60)
    frames = []
    resp = None
    t0 = time.time()
    try:
        conn.request('POST', u.path + '/api/admin/train', body=body, headers=hdrs)
        resp = conn.getresponse()
        if resp.status != 200:
            detail = resp.read().decode('utf-8', 'replace')[:200]
            skip('训练请求被拒 HTTP %s：%s' % (resp.status, detail))
        # 逐次读的粒度压到 5 秒：用于量「屏幕静默」
        try:
            conn.sock.settimeout(5)
        except Exception:
            pass
        while time.time() - t0 < WINDOW:
            try:
                line = resp.readline()
            except (socket.timeout, TimeoutError, OSError):
                continue  # 这 5 秒没有新帧 —— 继续等到窗口结束（静默会被 P3 抓住）
            if not line:
                break
            line = line.decode('utf-8', 'replace').strip()
            if not line.startswith('data:'):
                continue
            try:
                obj = json.loads(line[5:].strip())
            except Exception:
                continue
            frames.append((time.time() - t0, str(obj.get('type', '')), str(obj.get('data', ''))))
    finally:
        try:
            if resp is not None:
                resp.close()
        except Exception:
            pass
        try:
            conn.close()
        except Exception:
            pass
    return frames


def main():
    if not (_U and _P):
        skip('缺 %s / %s（凭据只在环境里传）' % (_ENV_U, _ENV_P))
    if not os.path.isfile(MAT):
        skip('缺试料 MAT=%s —— 这条腿没跑，不算通过' % MAT)
    with open(MAT, 'rb') as f:
        blob = f.read()
    filename = os.path.basename(MAT)
    print('试料：%s（%d 字节）/ 窗口 %.0fs / BASE=%s' % (filename, len(blob), WINDOW, BASE))

    tok = login()
    frames = stream_train(tok, blob, filename)
    print('\n收到帧 %d 条（%.1fs 内）' % (len(frames), WINDOW))
    if not frames:
        bad('P0 一条 SSE 帧都没收到 —— 训练流没起来，后面所有判据都是空跑')
        return tally()

    kinds = {}
    for _, t, _ in frames:
        kinds[t] = kinds.get(t, 0) + 1
    print('  帧类型：%s' % ', '.join('%s=%d' % kv for kv in sorted(kinds.items())))

    if any(PARSE_START_RE.search(d) for _, t, d in frames if t == 'step'):
        ok('P0 前提成立：屏上出现「正在解析上传的文档」——PDF 真进了解析链路')
    else:
        bad('P0 没看到「正在解析上传的文档」：PDF 可能没被当文档处理，后面断言不成立')

    page_frames = []
    for t, kind, d in frames:
        if kind != 'step':
            continue
        m = PAGE_RE.search(d)
        if m:
            page_frames.append((t, int(m.group(1)), int(m.group(2)), m.group(3), d))
    waits = [(t, d) for t, kind, d in frames if kind == 'step' and WAIT_RE.search(d)]
    sums = [m for _, kind, d in frames if kind == 'step' for m in [SUM_RE.search(d)] if m]

    if page_frames:
        first = page_frames[0]
        if first[0] <= FIRST_MAX_S:
            ok('P1 首条页帧 %.1fs 就滚出来（门槛 ≤%.0fs）：屏幕不用干等' % (first[0], FIRST_MAX_S))
        else:
            bad('P1 首条页帧 %.1fs > %.0fs：解析开头仍是一段静默' % (first[0], FIRST_MAX_S))

        if len(page_frames) >= MIN_PAGES:
            ok('P2 页帧 %d 条（≥%d）：逐页滚，不是只滚一页做样子'
               % (len(page_frames), MIN_PAGES))
        else:
            bad('P2 页帧只有 %d 条 < %d' % (len(page_frames), MIN_PAGES))

        # 静默窗口取到「最后一条页帧」为止：解析还没完时，不该拿后面的 LLM 阶段稀释。
        end = page_frames[-1][0]
        marks = sorted(t for t, _, _ in frames if t <= end)
        gaps = [(marks[i + 1] - marks[i], marks[i]) for i in range(len(marks) - 1)]
        worst = max(gaps)[0] if gaps else 0.0
        if worst <= MAX_SILENCE_S:
            ok('P3 解析窗口 %.1fs 内最长帧间隔 %.1fs（门槛 ≤%.0fs）'
               '——「一直卡着计时」被量化挡住' % (end, worst, MAX_SILENCE_S))
        else:
            bad('P3 解析窗口内最长帧间隔 %.1fs > %.0fs（修复前实测 33.2s）'
                % (worst, MAX_SILENCE_S))

        if waits:
            bad('P4 已有页帧滚出，屏上却还有 %d 条「已等待 Ns」裸计时：'
                '新能力是**替换**旧心跳，不是并存（第一条 %r）'
                % (len(waits), waits[0][1][:60]))
        else:
            ok('P4 有页滚出时没有「已等待 Ns」裸计时：旧心跳真被替换掉了')

        empty = [p for p in page_frames if len(p[4].split('：', 1)[-1].strip()) < 8]
        if not empty:
            ok('P5 页帧都带真材料文字：%r' % page_frames[len(page_frames) // 2][4][:70])
        else:
            bad('P5 有 %d 条页帧没有材料文字（只剩页码空壳）' % len(empty))
    else:
        bad('P1/P2/P3/P5 前提不成立：整段窗口一条页帧都没有 —— ocrd v6 逐页流没生效')
        if waits:
            print('    （对照：这段静默里只有 %d 条「已等待 Ns」裸计时，正是用户看到的那个）'
                  % len(waits))

    if sums:
        m = sums[-1]
        text_pages, ocr_pages = int(m.group(2)), int(m.group(3))
        if text_pages > 0 and ocr_pages > 0:
            ok('P6 汇总：共 %s 页（文本层直取 %s / OCR %s）—— 混合料两条路由都真走到'
               % (m.group(1), text_pages, ocr_pages))
        else:
            bad('P6 汇总只走到一条路由（文本层直取 %s / OCR %s）：这份料该两条都走'
                % (text_pages, ocr_pages))
    else:
        bad('P6 窗口内没等到解析汇总行（%.0fs 不够？或解析真卡住）' % WINDOW)

    print('\n技能名：%s（服务端训练仍在继续；验收产物用 DELETE /api/admin/skills/{slug} 收尾）'
          % NAME)
    return tally()


def tally():
    total = len(OK) + len(BAD)
    for m in BAD:
        print('  FAIL: %s' % m)
    print('\n--- %d/%d ok ---' % (len(OK), total))
    return 1 if BAD else 0


if __name__ == '__main__':
    sys.exit(main())
