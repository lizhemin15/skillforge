#!/usr/bin/env python3
"""本地静态靶面：把 web/ 挂成 /assets/ 前缀（和线上站点一致），根路径给 index.html。

为什么要这个：网站的静态资源是挂 `/assets/` 的，直接把 web/ 当根目录 serve，
页面里的 `/assets/js/chat.js` 就会 404 —— 那时 e2e 量的几何全是 fallback 值，
属于「靶面没起对」，比没测更糟（会给出看起来正常的错数字）。
"""
import http.server
import os
import sys

WEB = os.environ.get('WEB_DIR', '/root/skillforge/web')
PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 8099


class H(http.server.SimpleHTTPRequestHandler):
    def translate_path(self, path):
        p = path.split('?', 1)[0].split('#', 1)[0]
        if p.startswith('/assets/'):
            rel = p[len('/assets/'):]
        else:
            rel = p.lstrip('/')
        if not rel or rel.endswith('/'):
            rel += 'index.html'
        parts = [x for x in rel.split('/') if x not in ('', '.', '..')]
        return os.path.join(WEB, *parts)

    def log_message(self, *a):
        pass


if __name__ == '__main__':
    os.chdir(WEB)
    print(f'serving {WEB} at http://127.0.0.1:{PORT}/  (assets → /assets/ 前缀)')
    http.server.ThreadingHTTPServer(('127.0.0.1', PORT), H).serve_forever()
