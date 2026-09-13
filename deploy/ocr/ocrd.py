#!/usr/bin/env python3
# ocrd v3 — 多格式文档提取常驻微服务（资源优化版）
# 用法: ocrd [--port 8093] [--dpi 200] [--max-cache 64]
# POST /health        → {"ok":true}
# POST /extract       → multipart, field "file"=<任意支持格式>, 表单 "name"=原文件名(可选)
#   DOC: pdf, docx, doc, xlsx, xls, pptx, ppt, txt, md, csv, html, json
#   → {"ok":true,"fmt":"pdf|docx|...","text":"...","pages":N,"chars":N,"hash":"<sha256>","from_cache":bool,"raw":bool}
# POST /convert       → 兼容旧端点, 等价于 /extract(仅PDF), 返回旧结构 {ok,pages,text,...}
#
# 多格式设计：
#   · PDF  → 既有三层优化: 文本层检测优先 / 渲染+OCR / 内存LRU缓存 / per-hash锁并发去重 / 全局推理锁
#   · OOXML(docx/xlsx/pptx) → zipfile + xml.etree 纯标准库解析, 零新依赖, 极轻量
#   · 文本类(txt/md/csv/html/json) → 直接读, 自动猜编码
#   · 老格式 doc/xls/ppt 无法纯标准库解析 → 降级返回 {ok:false,error:"unsupported_fmt_detected"} 供上层提示
# 统一经 SHA256 LRU 缓存, 重复文件秒回, 零重复算力。
import sys, os, argparse, json, time, tempfile, threading, hashlib, re, zipfile, io, gc, glob, shutil
from collections import OrderedDict
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from xml.etree import ElementTree as ET
import pymupdf  # PDF 文本层/渲染
from rapidocr_onnxruntime import RapidOCR  # 扫描页 OCR

BUF = 64 * 1024 * 1024  # 64MB 上传上限
MAX_TEXT = 4 * 1024 * 1024  # 单文件解析出文本上限 4MB (防恶意超大文本)
# ONNX Runtime 的推理内存 arena 只增不还：实测峰值每页爬升约 20MB，
# 50 页扫描件（如整本手册）在 MemoryMax=1G 下会被 cgroup OOM 杀掉。
# 对策：每 RECYCLE_EVERY 页重建一次引擎，把峰值压回基线（引擎重建 ~0.5s，模型已在磁盘缓存）。
RECYCLE_EVERY = 20
STALE_TMP_HOURS = 2       # 启动时清理崩溃残留的 /tmp/ocrd_*（进程被 kill -9 时 finally 不执行）

# ---------- OOXML 命名空间 ----------
NS = {
    "w": "http://schemas.openxmlformats.org/wordprocessingml/2006/main",
    "a": "http://schemas.openxmlformats.org/drawingml/2006/main",
    "x": "http://schemas.openxmlformats.org/spreadsheetml/2006/main",
    "p": "http://schemas.openxmlformats.org/presentationml/2006/main",
}

# ---------- 格式识别 (扩展名 + 魔数) ----------
TEXT_EXTS = {".txt", ".md", ".markdown", ".csv", ".html", ".htm", ".json", ".xml", ".log"}
OOXML = {".docx", ".xlsx", ".pptx"}
LEGACY_OFFICE = {".doc", ".xls", ".ppt", ".rtf"}
PDF = {".pdf"}

def detect_format(raw: bytes, name: str) -> str:
    """返回 'pdf'|'docx'|'xlsx'|'pptx'|'text'|'legacy_office'|'unknown'。优先魔数, 其次扩展名。"""
    ext = (name or "").lower()
    ext = ext[ext.rfind("."):] if "." in ext else ""
    if raw[:4] == b"%PDF":
        return "pdf"
    # OOXML = zip 包且包含对应 [Content_Types].xml
    if raw[:2] == b"PK":
        try:
            with zipfile.ZipFile(io.BytesIO(raw)) as z:
                names = z.namelist()
                if any(n.startswith("word/") for n in names):
                    return "docx"
                if any(n.startswith("xl/") for n in names):
                    return "xlsx"
                if any(n.startswith("ppt/") for n in names):
                    return "pptx"
        except Exception:
            pass
        return "zip_other"
    # OLE2 老格式 (doc/xls/ppt)
    if raw[:8] == b"\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1":
        if ext in (".doc", ".xls", ".ppt"):
            return "legacy_office"
        return "legacy_office"
    if ext in TEXT_EXTS:
        return "text"
    if ext == ".pdf":
        return "pdf"  # 非标准魔数但扩展名是 pdf
    # 大段可打印文本兜底判为 text
    if not raw or b"\x00" not in raw[:256]:
        printable = sum(1 for b in raw[:1024] if 9 <= b <= 13 or 32 <= b < 127)
        if printable / max(len(raw[:1024]), 1) > 0.85:
            return "text"
    return "unknown"

# ---------- 编码猜测 (文本类) ----------
def decode_text(raw: bytes) -> str:
    for enc in ("utf-8", "gb18030", "big5", "latin1"):
        try:
            return raw.decode(enc)
        except UnicodeDecodeError:
            continue
    return raw.decode("utf-8", "replace")

# ---------- OOXML 解析 ----------
def _q(tag, ns):
    return "{%s}%s" % (NS[ns], tag)

def extract_docx(raw: bytes) -> tuple[str, int]:
    """返回 (text, 段落数)。解析 word/document.xml 的 w:p 段落。"""
    with zipfile.ZipFile(io.BytesIO(raw)) as z:
        try:
            xml = z.read("word/document.xml")
        except KeyError:
            raise ValueError("docx 缺少 document.xml")
    root = ET.fromstring(xml)
    paras = []
    for p in root.iter(_q("p", "w")):
        # 取段落内所有 w:t 文本节点
        texts = [t.text or "" for t in p.iter(_q("t", "w"))]
        line = "".join(texts).strip()
        # 表格内单元格段落也随段落流保留
        paras.append(line)
    return "\n".join(paras), len(paras)

def extract_xlsx(raw: bytes) -> tuple[str, int]:
    """返回 (text, sheet数)。解析所有 worksheet, 恢复 sharedStrings, 单元格按行拼接。"""
    with zipfile.ZipFile(io.BytesIO(raw)) as z:
        names = z.namelist()
        # 共享字符串表
        shared = []  # index -> str
        if "xl/sharedStrings.xml" in names:
            sroot = ET.fromstring(z.read("xl/sharedStrings.xml"))
            for si in sroot.iter(_q("si", "x")):
                text = "".join((t.text or "") for t in si.iter(_q("t", "x")))
                shared.append(text)
        # 每个 worksheet (跳过 chartSheet)
        sheets = [n for n in names if re.match(r"xl/worksheets/sheet\d+\.xml$", n)]
        sheets.sort(key=lambda s: int(re.search(r"(\d+)", s).group(1)))
        out = []
        for sn in sheets:
            root = ET.fromstring(z.read(sn))
            rows = []
            for row in root.iter(_q("row", "x")):
                cells = []
                for c in row.iter(_q("c", "x")):
                    v = c.find(_q("v", "x"))
                    t = c.get("t") or ""
                    if t == "s" and v is not None:  # shared string ref
                        idx = int(v.text or "0")
                        cells.append(shared[idx] if idx < len(shared) else "")
                    elif v is not None:
                        cells.append(v.text or "")
                if cells:
                    rows.append("\t".join(cells))
            out.append("### Sheet %d\n%s" % (len(out) + 1, "\n".join(rows)))
        return "\n\n".join(out), len(sheets)

def extract_pptx(raw: bytes) -> tuple[str, int]:
    """返回 (text, slide数)。解析每张 slide 的 a:t 文本 (文本框内)。"""
    with zipfile.ZipFile(io.BytesIO(raw)) as z:
        names = z.namelist()
        slides = [n for n in names if re.match(r"ppt/slides/slide\d+\.xml$", n)]
        slides.sort(key=lambda s: int(re.search(r"(\d+)", s).group(1)))
        out = []
        for sn in slides:
            root = ET.fromstring(z.read(sn))
            texts = []
            for t in root.iter(_q("t", "a")):
                txt = (t.text or "").strip()
                if txt:
                    texts.append(txt)
            out.append("### Slide %d\n%s" % (len(out) + 1, "\n".join(texts)))
        return "\n\n".join(out), len(slides)

# ---------- LRU 缓存 ----------
class LRUCache:
    """线程安全 LRU, max_bytes 按内容大小计。key=文件 hash, value=(fmt,text,raw,bytes)."""
    def __init__(self, max_bytes):
        self.max = max_bytes
        self._d = OrderedDict()
        self._lock = threading.Lock()
        self._size = 0
    def get(self, key):
        with self._lock:
            v = self._d.pop(key, None)
            if v is None:
                return None
            self._d[key] = v
            return v[0], v[1]
    def put(self, key, value, bytes_):
        if self.max <= 0:
            return
        with self._lock:
            old = self._d.pop(key, None)
            if old:
                self._size -= old[2]
            self._d[key] = (value[0], value[1], bytes_)
            self._size += bytes_
            while self._size > self.max and self._d:
                _, (_, _, b) = self._d.popitem(last=False)
                self._size -= b

# ---------- OCR 引擎 (仅 PDF) ----------
class OcrEngine:
    def __init__(self, dpi, max_cache):
        self.dpi = dpi
        self.engine = None
        self._init_lock = threading.Lock()
        self._infer_lock = threading.Lock()
        self._n_since_recycle = 0
        self._hash_lock_guard = threading.Lock()
        self._hash_locks = {}
        self.cache = LRUCache(max_cache * 1024 * 1024)
    def _hash_lock(self, h):
        with self._hash_lock_guard:
            if len(self._hash_locks) > 2048:
                self._hash_locks.clear()
            lk = self._hash_locks.get(h)
            if lk is None:
                lk = threading.Lock()
                self._hash_locks[h] = lk
            return lk
    def _ensure(self):
        if self.engine is None:
            with self._init_lock:
                if self.engine is None:
                    self.engine = RapidOCR()
        return self.engine

    def _recycle(self):
        """释放引擎并回收内存（调用方须持有 _infer_lock）。引擎下次用 _ensure() 重建。"""
        self.engine = None
        self._n_since_recycle = 0
        gc.collect()

    def extract(self, raw: bytes, name: str):
        """全格式入口: 返回 (fmt, text, chars, from_cache)。per-hash 锁并发去重。"""
        h = hashlib.sha256(raw).hexdigest()
        cached = self.cache.get(h)
        if cached is not None:
            return cached[0], cached[1], len(cached[1]), True
        lk = self._hash_lock(h)
        with lk:
            cached = self.cache.get(h)
            if cached is not None:
                return cached[0], cached[1], len(cached[1]), True
            fmt, text = self._do_extract(raw, name)
            if len(text) > MAX_TEXT:
                text = text[:MAX_TEXT]
            self.cache.put(h, (fmt, text), len(raw))
            return fmt, text, len(text), False

    def _do_extract(self, raw: bytes, name: str):
        fmt = detect_format(raw, name)
        if fmt == "text":
            return "text", decode_text(raw)
        if fmt in ("docx", "xlsx", "pptx"):
            fn = {"docx": extract_docx, "xlsx": extract_xlsx, "pptx": extract_pptx}[fmt]
            text, _ = fn(raw)
            return fmt, text
        if fmt == "pdf":
            return self._pdf(raw)
        if fmt == "legacy_office":
            raise ValueError("老格式 .doc/.xls/.ppt 无法在线解析, 请另存为 docx/xlsx/pptx 后上传")
        raise ValueError("不支持的文件格式: %s" % name)

    def _pdf(self, raw: bytes):
        """既有三层优化 PDF 路径 (文本层优先 + OCR + 流式)。返回 (fmt, text, pages?)。
        此处返回 (fmt='pdf', text), pages 用于 /convert 兼容需单独算, 但前端不需要。"""
        tmpdir = tempfile.mkdtemp(prefix="ocrd_")
        pdf_path = os.path.join(tmpdir, "in.pdf")
        with open(pdf_path, "wb") as f:
            f.write(raw)
        pieces = []
        doc = None
        try:
            doc = pymupdf.open(pdf_path)
            total = len(doc)
            for pno in range(total):
                page = doc[pno]
                # ① 文本层检测 —— 最省
                txt = page.get_text().strip()
                if txt:
                    pieces.append(txt)
                    continue
                # ② 真扫描页 → 渲染+OCR (流式释放)
                img = os.path.join(tmpdir, f"p{pno}.png")
                pix = page.get_pixmap(dpi=self.dpi)
                pix.save(img)
                pix = None
                with self._infer_lock:  # ③ 全局推理锁
                    eng = self._ensure()
                    res, _ = eng(img)
                    self._n_since_recycle += 1
                    if self._n_since_recycle >= RECYCLE_EVERY:
                        eng = None          # 丢掉局部引用，让 _recycle 真正释放
                        self._recycle()
                lines = [ln[1] for ln in res] if res else []
                pieces.append("\n".join(lines))
                try: os.remove(img)
                except OSError: pass
            return "pdf", "\n\n".join(pieces)
        finally:
            if doc: doc.close()
            try: os.remove(pdf_path)
            except OSError: pass
            try: os.rmdir(tmpdir)
            except OSError: pass

ENGINE = None

# ---------- multipart 解析 ----------
def _extract_file(body: bytes, content_type: str):
    """从 multipart body 提取 name='file' 的 part 原始字节。返回 (bytes, 上传文件名 or None)。"""
    m = re.search(r"boundary=([^;\s]+)", content_type or "")
    if not m:
        return None, None
    boundary = b"--" + m.group(1).encode("latin1")
    first = body.find(boundary + b"\r\n")
    if first == -1:
        return None, None
    pstart = first + len(boundary) + 2
    phead_end = body.find(b"\r\n\r\n", pstart)
    if phead_end == -1:
        return None, None
    phead = body[pstart:phead_end].decode("latin1", "ignore")
    if 'name="file"' in phead:
        end = body.find(b"\r\n" + boundary, phead_end + 4)
        if end == -1:
            return None, None
        fname = None
        mm = re.search(r'filename="([^"]*)"', phead)
        if mm:
            fname = mm.group(1)
        return body[phead_end + 4:end], fname
    return None, None

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass
    def _send_json(self, code, obj):
        body = json.dumps(obj, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def do_GET(self):
        if self.path.rstrip("/") == "/health":
            self._send_json(200, {"ok": True, "service": "ocrd", "formats": "pdf|docx|xlsx|pptx|txt|md|csv|html", "rapidocr": True})
        else:
            self._send_json(404, {"ok": False, "error": "not_found"})
    def _handle_upload(self, path):
        if self.headers.get("Expect", "").lower() == "100-continue":
            self.wfile.write(b"HTTP/1.1 100 Continue\r\n\r\n")
            self.wfile.flush()
        length = int(self.headers.get("Content-Length") or 0)
        if length > BUF:
            return self._send_json(413, {"ok": False, "error": "too_large"})
        body = self.rfile.read(length) if length else b""
        if not body:
            return self._send_json(400, {"ok": False, "error": "empty_body"})
        raw, name = _extract_file(body, self.headers.get("Content-Type", ""))
        if raw is None:
            return self._send_json(400, {"ok": False, "error": "no_file_field"})
        t0 = time.time()
        try:
            fmt, text, chars, from_cache = ENGINE.extract(raw, name or "")
            ms = round((time.time() - t0) * 1000)
            if path == "/extract":
                return self._send_json(200, {
                    "ok": True, "fmt": fmt, "text": text, "chars": chars,
                    "name": name or "", "hash": hashlib.sha256(raw).hexdigest(),
                    "from_cache": from_cache, "total_ms": ms,
                })
            # /convert 兼容旧结构
            return self._send_json(200, {
                "ok": True, "fmt": fmt, "text": text, "chars": chars,
                "hash": hashlib.sha256(raw).hexdigest(),
                "from_cache": from_cache, "total_ms": ms,
                "pages": [{"page": 1, "src": fmt, "ms": ms, "text": text}] if fmt == "pdf" else [],
            })
        except Exception as e:
            self._send_json(200, {"ok": False, "error": str(e)})  # 200 + ok:false (与 v1 兼容)
    def do_POST(self):
        p = self.path.rstrip("/")
        if p in ("/extract", "/convert"):
            return self._handle_upload(p)
        self._send_json(404, {"ok": False, "error": "not_found"})

def sweep_stale_tmp():
    """清掉崩溃残留的 /tmp/ocrd_*（进程被 OOM kill -9 时 finally 里的 rmdir 不会执行）。"""
    now, n = time.time(), 0
    for d in glob.glob(os.path.join(tempfile.gettempdir(), "ocrd_*")):
        try:
            if now - os.path.getmtime(d) > STALE_TMP_HOURS * 3600:
                shutil.rmtree(d, ignore_errors=True); n += 1
        except OSError:
            pass
    return n

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=8093)
    ap.add_argument("--dpi", type=int, default=200)
    ap.add_argument("--max-cache", type=int, default=64, help="缓存MB上限")
    a = ap.parse_args()
    n = sweep_stale_tmp()
    print(f"启动清理残留临时目录: {n} 个", flush=True)
    global ENGINE
    ENGINE = OcrEngine(a.dpi, a.max_cache)
    try: ENGINE._ensure()
    except Exception as e: print("engine init warning:", e)
    print(f"ocrd v3 listening on 127.0.0.1:{a.port} (dpi={a.dpi}, max_cache={a.max_cache}MB, recycle_every={RECYCLE_EVERY}页, formats=pdf/docx/xlsx/pptx/txt)",
          flush=True)
    srv = ThreadingHTTPServer(("127.0.0.1", a.port), Handler)
    srv.serve_forever()

if __name__ == "__main__":
    main()