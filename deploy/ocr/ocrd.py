#!/usr/bin/env python3
# ocrd v4 — 多格式文档提取常驻微服务（资源优化版 + PDF 逐页择优）
# 用法: ocrd [--port 8093] [--dpi 200] [--max-cache 64]
# POST /health        → {"ok":true,"version":...,"min_page_chars":...}
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

# 文本层判定的最小有效字数。
# 旧逻辑是 `if txt:` —— 只要该页 get_text() 拿到**任何**字符就当「有文字层，直接采用」。
# 而真实扫描件（扫描全能王/夸克/Adobe Scan 导出）几乎都在扫描图上叠了一层很薄的文字层
# 描述「扫描全能王 第 3 页」之类的页脚/水印。于是逐页判定全部命中，真扫描页**一页都不送 OCR**：
# 整本正文被静默丢掉，只留下页码和水印。
# 实测事故：前 2 页扫描 + 后 3 页可选的混合 PDF，两版都测出这种退化。带水印那版 0.08s 返回、
# 零 OCR、正文两页全无 —— 素材只剩 15%，训练出来的技能与用户给的内容毫无关系。
# 页码 + 水印通常 ≤ 20 字，真正文页远超 40 字，阈值取中间的 40（可用 OCRD_MIN_PAGE_CHARS 调）。
MIN_PAGE_CHARS = int(os.environ.get("OCRD_MIN_PAGE_CHARS") or 40)
# 「整本看起来不像正文」的下限：平均每页少于这么多字时给出 warning 让上层有机会中止。
MIN_AVG_PAGE_CHARS = 20

# 版本串只为了让运维能**从外部证明线上跑的是哪一版**。
# 事故教训：换二进制后只靠 `systemctl restart` + 进程活着，看不出新逻辑有没有生效；
# 一旦 /health 能把「逐页择优 + 阈值」报出来，部署验收就有可 curl 的证据，
# 不用去猜「是不是没重启成功」。
VERSION = "ocrd-v4-perpage"

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
    """线程安全 LRU, max_bytes 按内容大小计。key=文件 hash, value=(fmt,text,stats,bytes)。"""
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
            return v[0], v[1], v[2]
    def put(self, key, value, bytes_):
        if self.max <= 0:
            return
        with self._lock:
            old = self._d.pop(key, None)
            if old:
                self._size -= old[3]
            self._d[key] = (value[0], value[1], value[2], bytes_)
            self._size += bytes_
            while self._size > self.max and self._d:
                _, (_, _, _, b) = self._d.popitem(last=False)
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
        """全格式入口: 返回 (fmt, text, chars, from_cache, stats)。per-hash 锁并发去重。

        stats 是解析可信度诊断（PDF: pages/text_pages/ocr_pages/empty_pages），非 PDF 为 {}。
        为什么要它：**char 数分辨不出「解析出 1129 字正文」和「解析出 1129 字水印」**。
        线上事故里混合型 PDF 正好落进这个盲区——chars 非 0，全链路绿灯，训练出来的技能
        与素材无关。上层需要逐页统计才能判断素材是否可信（见 parse_warning）。
        """
        h = hashlib.sha256(raw).hexdigest()
        cached = self.cache.get(h)
        if cached is not None:
            return cached[0], cached[1], len(cached[1]), True, cached[2]
        lk = self._hash_lock(h)
        with lk:
            cached = self.cache.get(h)
            if cached is not None:
                return cached[0], cached[1], len(cached[1]), True, cached[2]
            fmt, text, stats = self._do_extract(raw, name)
            if len(text) > MAX_TEXT:
                text = text[:MAX_TEXT]
                stats = dict(stats or {})
                stats["truncated"] = True
            self.cache.put(h, (fmt, text, stats), len(raw))
            return fmt, text, len(text), False, stats

    def _do_extract(self, raw: bytes, name: str):
        fmt = detect_format(raw, name)
        if fmt == "text":
            return "text", decode_text(raw), {}
        if fmt in ("docx", "xlsx", "pptx"):
            fn = {"docx": extract_docx, "xlsx": extract_xlsx, "pptx": extract_pptx}[fmt]
            text, _ = fn(raw)
            return fmt, text, {}
        if fmt == "pdf":
            return self._pdf(raw)
        if fmt == "legacy_office":
            raise ValueError("老格式 .doc/.xls/.ppt 无法在线解析, 请另存为 docx/xlsx/pptx 后上传")
        raise ValueError("不支持的文件格式: %s" % name)

    def _ocr_page(self, page, tmpdir: str, pno: int) -> str:
        """渲染单页并 OCR，返回识别文本（失败返回 ""）。

        流式释放：渲染图落盘→识别→立刻删，避免整本 render 把内存顶爆；
        每 RECYCLE_EVERY 页重建引擎（ONNX arena 只增不还，见文件头注释）。
        """
        img = os.path.join(tmpdir, f"p{pno}.png")
        pix = page.get_pixmap(dpi=self.dpi)
        pix.save(img)
        pix = None
        res = None
        try:
            with self._infer_lock:  # 全局推理锁
                eng = self._ensure()
                res, _ = eng(img)
                self._n_since_recycle += 1
                if self._n_since_recycle >= RECYCLE_EVERY:
                    eng = None          # 丢掉局部引用，让 _recycle 真正释放
                    self._recycle()
        finally:
            try: os.remove(img)
            except OSError: pass
        lines = [ln[1] for ln in res] if res else []
        return "\n".join(lines).strip()

    def _pdf(self, raw: bytes):
        """既有三层优化 PDF 路径 (逐页文本层判定 + OCR + 流式)。返回 (fmt, text, stats)。

        逐页判定的关键在 MIN_PAGE_CHARS：文本层**够厚**才直取（可选中 PDF 全走这条，
        零 OCR，秒回）；只有水印/页码的薄文本层视为「实为扫描页」，渲染 OCR，并把
        文本层与 OCR 结果**择优保留**（谁信息多要谁），绝不静默丢页。

        stats 回传逐页统计，供上层判断素材可信度（见 parse_warning）。
        """
        tmpdir = tempfile.mkdtemp(prefix="ocrd_")
        pdf_path = os.path.join(tmpdir, "in.pdf")
        with open(pdf_path, "wb") as f:
            f.write(raw)
        pieces = []
        doc = None
        stats = {"pages": 0, "text_pages": 0, "ocr_pages": 0, "empty_pages": 0}
        try:
            doc = pymupdf.open(pdf_path)
            total = len(doc)
            stats["pages"] = total
            for pno in range(total):
                page = doc[pno]
                # ① 文本层够厚 → 直取（最省，可选中 PDF 的正常路径）
                txt = (page.get_text() or "").strip()
                if len(txt) >= MIN_PAGE_CHARS:
                    stats["text_pages"] += 1
                    pieces.append(txt)
                    continue
                # ② 文本层为空 / 只有水印页码 → 按扫描页处理，渲染 + OCR
                ocr_txt = self._ocr_page(page, tmpdir, pno)
                if ocr_txt and len(ocr_txt) >= len(txt):
                    stats["ocr_pages"] += 1
                    pieces.append(ocr_txt)
                elif ocr_txt:
                    # OCR 认得比文本层少，但两者都短：保留信息量大的那个
                    stats["ocr_pages"] += 1
                    pieces.append(ocr_txt)
                elif txt:
                    # OCR 彻底失败：宁可留薄文本层，也不要丢页
                    stats["text_pages"] += 1
                    pieces.append(txt)
                else:
                    stats["empty_pages"] += 1
            return "pdf", "\n\n".join(pieces), stats
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

def parse_warning(stats, chars: int) -> str:
    """把「素材可能不完整」翻译成一句话交给上层，让上层有机会中止而不是硬着头皮继续。

    只看 chars 判断不了：混合型 PDF（前几页扫描 + 后几页可选）能解析出上千字符，但
    那可能全是「扫描全能王 / 第 N 页」的水印。这里用逐页统计（页数、扫描页数、空页数）
    给出「平均每页字数过低」「有整页没识别出文字」这类**可据以决策**的信号。
    """
    if not stats:
        return ""
    pages = int(stats.get("pages") or 0)
    if pages <= 0:
        return ""
    parts = []
    empty = int(stats.get("empty_pages") or 0)
    if empty:
        parts.append(f"有 {empty}/{pages} 页未识别出任何文字")
    avg = chars / pages
    if avg < MIN_AVG_PAGE_CHARS:
        parts.append(f"共 {pages} 页只解析出 {chars} 字（平均每页 {avg:.0f} 字），"
                     f"疑似只拿到了页码/水印而正文缺失")
    return "；".join(parts)


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
            # version / min_page_chars 一起报出来：部署验收可以直接 curl 出「跑的是哪一版、
            # 逐页择优的阈值是多少」，不用先进机器翻二进制（见 VERSION 处的注释）。
            self._send_json(200, {"ok": True, "service": "ocrd", "version": VERSION,
                                  "min_page_chars": MIN_PAGE_CHARS,
                                  "formats": "pdf|docx|xlsx|pptx|txt|md|csv|html", "rapidocr": True})
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
            fmt, text, chars, from_cache, stats = ENGINE.extract(raw, name or "")
            ms = round((time.time() - t0) * 1000)
            warn = parse_warning(stats, chars)
            if path == "/extract":
                return self._send_json(200, {
                    "ok": True, "fmt": fmt, "text": text, "chars": chars,
                    "name": name or "", "hash": hashlib.sha256(raw).hexdigest(),
                    "from_cache": from_cache, "total_ms": ms,
                    "stats": stats, "warning": warn,
                })
            # /convert 兼容旧结构
            return self._send_json(200, {
                "ok": True, "fmt": fmt, "text": text, "chars": chars,
                "hash": hashlib.sha256(raw).hexdigest(),
                "from_cache": from_cache, "total_ms": ms,
                "stats": stats, "warning": warn,
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
    print(f"{VERSION} listening on 127.0.0.1:{a.port} (dpi={a.dpi}, max_cache={a.max_cache}MB, recycle_every={RECYCLE_EVERY}页, min_page_chars={MIN_PAGE_CHARS}, formats=pdf/docx/xlsx/pptx/txt)", flush=True)
    srv = ThreadingHTTPServer(("127.0.0.1", a.port), Handler)
    srv.serve_forever()

if __name__ == "__main__":
    main()