#!/usr/bin/env python3
"""原始思考链取证：直接问线上生效的那家 provider，把 reasoning 逐片打时间戳。

为什么单独有这支：材料过滤器把「非中文段」全挡掉了，于是线上出现「t=22s~46s 屏幕
只有墙钟计时」时，光看 SSE 是**看不出**那 24 秒里模型究竟在吐什么 —— 挡掉的东西
恰恰是要查的东西。所以绕过本服务的显示层，直接看 provider 的原始流。

只读线上配置，不外发、不落凭据。
"""
import json
import os
import sqlite3
import time
import urllib.request

DB = "/opt/skillforge/data/skillforge.db"
c = sqlite3.connect(DB)
c.row_factory = sqlite3.Row
r = dict(c.execute("select * from llm_config where is_active=1 limit 1").fetchone())
base = (r.get("base_url") or r.get("base") or "").rstrip("/")
key = r.get("api_key") or ""
model = r.get("model")
print(f"[配置] provider={r.get('provider')} model={model} base={base}")

mat = open(os.environ.get("MAT", "/tmp/material_10k.txt"), encoding="utf-8").read()
# 大素材光是 prefill 就可能吃掉一两分钟（实测 1 万字那档首片 105s 才来），
# 而这里要看的是「思考链长什么样」，跟素材长短无关 —— 所以允许用短素材取样本。
print(f"[素材] {len(mat)} 字")
prompt = ("下面是公司的背景素材（约 %d 字），请通读后按素材写一篇1500 字左右的公司新闻稿，"
          "标题自拟，写完直接给正文。\n\n素材如下：\n" % len(mat)) + mat

body = json.dumps({
    "model": model,
    "messages": [{"role": "user", "content": prompt}],
    "stream": True,
    "enable_thinking": True,
    "thinking_budget": 1024,
    "max_tokens": 4096,
}).encode()
req = urllib.request.Request(base + "/chat/completions", method="POST", data=body)
req.add_header("Content-Type", "application/json")
req.add_header("Authorization", "Bearer " + key)

t0 = time.time()
first_reason = first_content = None
n_reason = n_content = 0
n_ascii_letters = n_cjk = 0
with urllib.request.urlopen(req, timeout=600) as resp:
    for raw in resp:
        line = raw.decode("utf-8", "ignore").strip()
        if not line.startswith("data:"):
            continue
        payload = line[5:].strip()
        if payload == "[DONE]":
            break
        try:
            ev = json.loads(payload)
        except Exception:
            continue
        if ev.get("error") or ev.get("code"):
            print(f"[{time.time()-t0:6.1f}s] ⚠ provider 报错帧: {json.dumps(ev, ensure_ascii=False)[:200]}")
        ch0 = (ev.get("choices") or [{}])[0]
        if ch0.get("finish_reason"):
            print(f"[{time.time()-t0:6.1f}s] ⚑ finish_reason={ch0.get('finish_reason')}")
        d = ch0.get("delta") or {}
        now = time.time() - t0
        rc = d.get("reasoning_content") or d.get("reasoning") or ""
        ct = d.get("content") or ""
        if isinstance(rc, str) and rc:
            n_reason += len(rc)
            n_ascii_letters += sum(1 for ch in rc if ch.isascii() and ch.isalpha())
            n_cjk += sum(1 for ch in rc if "\u4e00" <= ch <= "\u9fff")
            if first_reason is None:
                first_reason = now
                print(f"[{now:6.1f}s] 🧠 首个思考片")
            if n_reason % 400 < len(rc) or now > 20:
                print(f"[{now:6.1f}s] 🧠 +{len(rc):4d} 尾: {rc[-70:]!r}")
        if isinstance(ct, str) and ct:
            n_content += len(ct)
            if first_content is None:
                first_content = now
                print(f"[{now:6.1f}s] 🧱 首个正文片 len={len(ct)}")
            if first_content is not None and now - first_content < 0.4:
                print(f"[{now:6.1f}s] 🧱 {ct[:60]!r}")

print(f"\n[合计] 思考 {n_reason} 字（拉丁字母 {n_ascii_letters} / 汉字 {n_cjk}）"
      f"，正文 {n_content} 字，首思考 {first_reason}，首正文 {first_content}")
