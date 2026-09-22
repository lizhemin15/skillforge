#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""逐页流复验：证明 ocrd 是**边解析边发**，不是解析完再拆行。

用法：
  python3 scripts/verify_ocr_stream.py <ocrd-url> <某个真 .pdf> [页数下限]
  python3 scripts/verify_ocr_stream.py --selftest     # 尺子自证（假服务，不需要 ocrd）

为什么单独成文件（而不是塞进 deploy_ocrd.sh 的 heredoc）：
  `curl … | python3 - <<'PY' … PY` 里 python3 的 stdin 被 heredoc 占了，管道数据
  没人读 —— 脚本会拿程序文本当输入，看起来「跑过了」其实什么也没量。分离成文件后
  数据流只有一条，且验收腿与上线门禁能共用同一份判据（弱断言的老毛病：两边各写一份）。

判据（六条，缺一不可）：
  ① 真收到逐页行（不是整份 JSON 一把回）→ 否则屏幕上就是「一直卡着计时」；
  ② 第一页到得**远早于**汇总行 → 证明渐进，而不是先攒齐再拆；
  ③ 逐页行数与末行 stats.pages 相符 → 不丢页（丢页 = 素材悄悄少了内容）；
  ④ 必须有汇总行且 ok=true → 断流当成功会让半截素材安静地训出错技能；
  ⑤ 汇总行 from_cache 必须为假 → 缓存命中时 on_page 根本不回调，量到的是零；
  ⑥ 所有页都要带 src/chars → 前端靠它区分「直取文本层」与「扫描件 OCR」。

自证（--selftest）：六个假服务分别演「真流式 / 攒齐再吐 / 老件一把回 JSON / 缓存命中 /
  半途断流 / 接口 404」。自证与真验收**共用同一个 judge()** —— 两边各写一份判据，就是
  「尺子自己被改坏也全绿」的那类假绿。不需要 ocrd、不需要 pymupdf，CI 上也能跑。

退出码：0 通过；3 未通过（带人话原因）；2 用法/试料缺失（SKIP，不计 PASS）。
"""
import http.client
import json
import os
import sys
import tempfile
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MIN_GAP = 1.0  # 首行与汇总行至少差这么久，才算「边解析边发」


class RulerFail(Exception):
    """尺子自身的失败通道：带人话原因，退出码 3（未通过），不打印栈。"""


def post_stream(url, pdf_path, sink, timeout=1800):
    """POST 整个 PDF，按行读响应（读一行 yield 一行 + 到时）。

    ctype 通过 sink 回传而不是当成一行 yield 出去：混着 yield dict 会让下面
    的 json.loads 拿到两种类型（类型检查器会叫，且真跑时容易把控制行当数据）。
    """
    host, _, path = url.split("//", 1)[1].partition("/")
    # URL 只给到端口时，路径必须落到 /extract。曾经写成 "/" → 服务端回 404 not_found
    # **立刻**收场，而大素材还在发 → 客户端只看到 Broken pipe，把「404」误判成「对端没开流」。
    path = "/" + path if path else "/extract"
    port = 80
    if ":" in host:
        host, _, p = host.partition(":")
        port = int(p)
    with open(pdf_path, "rb") as f:
        blob = f.read()
    boundary = "----sfstream" + uuid.uuid4().hex
    pre = (
        "--%s\r\nContent-Disposition: form-data; name=\"file\"; filename=\"%s\"\r\n"
        "Content-Type: application/pdf\r\n\r\n" % (boundary, os.path.basename(pdf_path))
    ).encode("utf-8")
    body = pre + blob + ("\r\n--%s--\r\n" % boundary).encode("utf-8")

    conn = http.client.HTTPConnection(host, port, timeout=timeout)
    # 对端没收完素材就关连接 → 本地 sendall 抛 EPIPE。这是**崩溃红**（RC=1、零 FAIL 行），
    # 按铁律等于「没跑」：排障时会被当成尺子坏了，而不是被测服务坏了。转成人话。
    try:
        conn.request("POST", path, body=body, headers={
            "Accept": "application/x-ndjson",
            "Content-Type": "multipart/form-data; boundary=" + boundary,
            "Content-Length": str(len(body)),
        })
        resp = conn.getresponse()
    except BrokenPipeError as e:
        # 断连时对端**往往已经把话写下来了**。不读就等于把「它到底说了什么」丢掉，
        # 排障时只剩一句 Broken pipe，只能猜（实测：服务重启后的第一个 POST 必断）。
        said = ""
        try:
            s = conn.sock
            if s is not None:
                s.settimeout(3)
                buf = b""
                while len(buf) < 8192:
                    c = s.recv(4096)
                    if not c:
                        break
                    buf += c
                said = buf.decode("utf-8", "replace").strip()
        except Exception:
            pass
        raise RulerFail("对端在收完素材（%.1fMB）之前就关闭了连接：%s。它对本次请求说的原话：%s"
                        % (len(body) / 1048576.0, e, said[:600] if said else "(什么都没读到)"))
    except OSError as e:
        raise RulerFail("连不上 %s：%s" % (url, e))
    if resp.status != 200:
        # 单独定罪：404/413/鉴权失败都不该被笼统说成「对端提前关连接」。
        try:
            conn.sock.settimeout(10)
            err_txt = resp.read(400).decode("utf-8", "replace")
        except Exception:
            err_txt = ""
        if not err_txt:
            # 服务端往往在响应头之后就把 socket 关了，http.client 的 buffered reader
            # 可能已经吃到 EOF。直接从裸 socket 再抄一次，别让「它说了什么」丢掉。
            try:
                s = conn.sock
                if s is not None:
                    s.settimeout(3)
                    err_txt = s.recv(400).decode("utf-8", "replace").strip()
            except Exception:
                pass
        conn.close()
        raise RulerFail("对端拒绝（HTTP %s）：%s" % (resp.status, err_txt[:300] or "(空)"))
    ctype = (resp.getheader("Content-Type") or "").lower()
    buf = b""
    t0 = time.time()
    # 用 readline() 而不是 read()：read() 会等服务端关闭连接才返回，
    # 那样测出的「到达时刻」全是同一秒 —— 会把缓存的、非流式的实现也测成渐进。
    while True:
        chunk = resp.readline()
        if not chunk:
            break
        buf += chunk
        while b"\n" in buf:
            line, buf = buf.split(b"\n", 1)
            if line.strip():
                yield line.decode("utf-8"), time.time() - t0
    conn.close()
    sink["ctype"] = ctype


def judge(pages, tail, bad, ctype, min_pages, label):
    """共用判定：真验收与 --selftest 都走这里。返回 (rc, 人话)。"""
    if "ndjson" not in ctype:
        # 明确区分两种退化：老件（不认这个头）与中间设备吃掉流。
        return 3, ("响应 Content-Type=%r 不是 ndjson：对端没开逐页流（老件或中间层缓冲）" % ctype)
    if tail is None:
        return 3, "没收到汇总行：连接在半途断掉。半截素材会被当成完整素材去训练"
    if tail[0].get("from_cache"):
        return 3, ("对端命中了内容缓存（from_cache=true）：本次没有真的解析，逐页流一点没量到。"
                   "这条腿必须喂全新的料（预检里给固定料追加 nonce 即可）。")
    if not tail[0].get("ok"):
        return 3, "汇总行 ok=false：%s" % tail[0].get("error")
    if bad:
        return 3, "有 %d 行不是合法 JSON" % bad
    if len(pages) < min_pages:
        return 3, "逐页行只有 %d 条（要求 ≥%d）" % (len(pages), min_pages)

    nums = [p.get("page") for p, _ in pages]
    if nums != list(range(1, len(nums) + 1)):
        return 3, "页码不连续：%s" % nums
    if not any((p.get("text") or "").strip() for p, _ in pages):
        return 3, "所有页都是空的：屏幕上滚不出材料"
    if not all("src" in p and "chars" in p for p, _ in pages):
        return 3, "逐页行缺 src/chars 字段（前端要靠它区分直取与 OCR）"

    stats = tail[0].get("stats") or {}
    if stats.get("pages") not in (None, len(pages)):
        return 3, "逐页行数 %d 与 stats.pages %s 不符（丢页）" % (len(pages), stats.get("pages"))

    first_at, last_at, tail_at = pages[0][1], pages[-1][1], tail[1]
    if tail_at < 2 * MIN_GAP:
        return 3, ("整篇只花了 %.2fs：太快了，无法区分「边解析边发」与「解析完再拆行」。"
                   "这条腿要喂含扫描页的料（每页 OCR ≳0.5s）才量得到真东西。" % tail_at)
    if not (first_at + MIN_GAP < tail_at):
        return 3, ("首行 %.2fs 与汇总行 %.2fs 差不到 %.1fs：这是「解析完再拆行」，不是边解析边发"
                   % (first_at, tail_at, MIN_GAP))
    return 0, ("ok: 逐页流生效 —— %s：%d 页逐行滚出（首行 %.1fs / 末页 %.1fs / 汇总 %.1fs），stats=%s"
               % (label, len(pages), first_at, last_at, tail_at,
                  json.dumps(stats, ensure_ascii=False)))


def run(url, pdf, min_pages):
    """跑一次并出判定：返回 (rc, 人话)。生成器体里的异常只在 next() 时抛，所以 try
    必须**罩住消费循环** —— 否则 RulerFail 逃出去就成了未捕获异常（RC=1、零 FAIL 行），
    铁律里这叫「崩溃红＝没跑」。"""
    pages, tail, bad, sink = [], None, 0, {}
    try:
        for line, at in post_stream(url, pdf, sink):
            try:
                o = json.loads(line)
            except Exception:
                bad += 1
                continue
            if o.get("done") is True:
                tail = (o, at)
            else:
                pages.append((o, at))
    except RulerFail as e:
        return 3, str(e)
    return judge(pages, tail, bad, (sink.get("ctype") or "").lower(), min_pages, os.path.basename(pdf))


# ---------------------------------------------------------------- 自证用的假 ocrd

def _pages(n, src="ocr"):
    return [{"page": i, "pages": n, "src": src, "chars": 40, "text": "第 %d 页的真材料" % i}
            for i in range(1, n + 1)]


def fake_server(mode):
    """起一个只演一场戏的假 ocrd，返回 (url, server)。"""
    class H(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.0"   # 与真 ocrd 一致：靠连接关闭定界

        def log_message(self, *a):
            pass

        def _dump(self, objs):
            for o in objs:
                self.wfile.write((json.dumps(o, ensure_ascii=False) + "\n").encode("utf-8"))
            self.wfile.flush()

        def _json(self, code, obj):
            body = json.dumps(obj, ensure_ascii=False).encode("utf-8")
            self.send_response(code)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(body)

        def do_POST(self):
            left = int(self.headers.get("Content-Length") or 0)
            while left > 0:                       # 必须真收完素材，否则对面 sendall EPIPE
                c = self.rfile.read(min(left, 65536))
                if not c:
                    break
                left -= len(c)
            if mode == "notfound":
                return self._json(404, {"ok": False, "error": "not_found"})
            if mode == "legacy":
                return self._json(200, {"ok": True, "fmt": "pdf", "text": "x" * 200,
                                        "chars": 200, "from_cache": False,
                                        "stats": {"pages": 3}})
            self.send_response(200)
            self.send_header("Content-Type", "application/x-ndjson; charset=utf-8")
            self.send_header("Connection", "close")
            self.end_headers()
            if mode == "burst":                   # 攒齐一次性吐（= 解析完再拆行）
                time.sleep(2.0)
                self._dump(_pages(3) + [{"done": True, "ok": True, "from_cache": False,
                                         "stats": {"pages": 3}}])
                return
            if mode == "truncated":               # 半途断流，没有汇总行
                self._dump(_pages(2))
                return
            if mode == "cache":                   # 命中缓存：逐页行有，但本次没真解析
                self._dump(_pages(2) + [{"done": True, "ok": True, "from_cache": True,
                                         "stats": {"pages": 2}}])
                return
            for o in _pages(3):                   # good：边解析边发（每页 1s）
                self._dump([o])
                time.sleep(1.0)
            self._dump([{"done": True, "ok": True, "from_cache": False, "stats": {"pages": 3}}])

    srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
    t = threading.Thread(target=srv.handle_request, daemon=True)
    t.start()
    return "http://127.0.0.1:%d" % srv.server_address[1], srv


SELFTEST = [
    # mode,        期望 rc, 人话里必须出现的关键词
    ("good",       0, "逐页流生效"),
    ("burst",      3, "解析完再拆行"),
    ("legacy",     3, "不是 ndjson"),
    ("cache",      3, "from_cache"),
    ("truncated",  3, "没收到汇总行"),
    ("notfound",   3, "HTTP 404"),
]


def selftest():
    """每个方向都必须打中它自己那条判据 —— 打不中就是尺子坏了（或这个方向没人守）。"""
    fd, seed = tempfile.mkstemp(suffix=".pdf", prefix="stream-selftest-")
    with os.fdopen(fd, "wb") as f:
        f.write(b"%PDF-1.4\n" + os.urandom(2048) + b"\n%%EOF\n")
    bad = 0
    for mode, want_rc, want_kw in SELFTEST:
        url, srv = fake_server(mode)
        try:
            rc, msg = run(url, seed, 1)
        finally:
            srv.server_close()
        hit = (rc == want_rc) and (want_kw in msg)
        print("  %s 方向 %-10s rc=%d（期望 %d）关键词 %r %s"
              % ("✓" if hit else "✗", mode, rc, want_rc, want_kw,
                 "" if hit else "→ 实得：%s" % msg[:200]))
        if not hit:
            bad += 1
    os.unlink(seed)
    if bad:
        print("FAIL: %d/%d 个方向没打中自己那条判据：尺子坏了，别拿它的红绿当结论"
              % (bad, len(SELFTEST)))
        return 3
    print("  ok: 尺子自证成立 —— %d/%d 方向各自精确命中（含「攒齐再吐」必须被打成红）"
          % (len(SELFTEST), len(SELFTEST)))
    return 0


def main():
    if len(sys.argv) > 1 and sys.argv[1] == "--selftest":
        return selftest()
    if len(sys.argv) < 3:
        print("用法: python3 scripts/verify_ocr_stream.py <ocrd-url> <pdf> [页数下限]")
        return 2
    url, pdf = sys.argv[1], sys.argv[2]
    min_pages = int(sys.argv[3]) if len(sys.argv) > 3 else 2
    if not os.path.isfile(pdf):
        print("SKIP: 试料不存在 %s（这条腿什么也没量到，不算过）" % pdf)
        return 2
    if os.environ.get("NONCE"):
        # ocrd 按内容 hash 缓存，且**缓存命中时 on_page 根本不回调** → 同料复跑会测得
        # 「零逐页行」的假红。PDF 尾部允许 %%EOF 之后有垃圾字节：追加一行唯一注释即可改掉
        # 内容 hash（页数与解析代价不变）—— 这是每次都量到真·冷解析的关键。
        dst = os.path.join(tempfile.gettempdir(), "probe-nonce-%s.pdf" % uuid.uuid4().hex[:8])
        with open(pdf, "rb") as f:
            data = f.read()
        with open(dst, "wb") as f:
            f.write(data + ("\n%% probe-nonce %s\n" % uuid.uuid4().hex).encode("ascii"))
        pdf = dst

    rc, msg = run(url, pdf, min_pages)
    print(msg if rc == 0 else "FAIL: " + msg)
    return rc


if __name__ == "__main__":
    sys.exit(main())
