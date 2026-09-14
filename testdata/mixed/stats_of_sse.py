#!/usr/bin/env python3
"""从训练 SSE 里读出「逐页统计」那一行，并对它做严格断言。

为什么单独抽成一个文件：线上验收脚本（verify_live_train_material.sh）和它的
自检脚本（verify_stats_selfcheck.sh）必须用**同一份解析逻辑**。
如果自检脚本自己复制一份正则，那么改了解析、自检还在测旧逻辑，
自检就变成给假绿盖章。共用文件才能保证「测的就是线上跑的那段代码」。

被断言的进度行长这样（真实线上抓的原文）：

    topic.pdf 解析完成：833 字符（3.9s）共 4 页（文本层直取 2 / OCR 2）

注意这里**没有** text_pages / ocr_pages 这种字段名 —— 原始统计 map 只活在
服务端内存里，写进进度流的是 statsLine() 压出来的人话。
所以早先那条 `grep -q 'text_pages' train.sse` 的断言是永远不可能通过的假断言，
必须按人话整段解析（而不是拿一个裸子串去 grep，那样 0 命中/恒命中都会误判）。

用法：
    stats_of_sse.py <sse文件>               # 打印 "pages text_pages ocr_pages" 或 NONE
    stats_of_sse.py <sse文件> --expect 4 2 2  # 三项严格相等才算过；退出码即结论
"""
import json
import re
import sys

# 整段锚定：页数、文本层直取页数、OCR 页数三个数字必须同处一行，
# 不许跨行拼凑，也不许只匹配到其中一个数字就算数。
LINE_RE = re.compile(r"共\s*(\d+)\s*页（文本层直取\s*(\d+)\s*/\s*OCR\s*(\d+)")


def read_stats(path):
    """返回 (pages, text_pages, ocr_pages)；读不到返回 None。"""
    try:
        raw = open(path, encoding="utf-8", errors="replace").read()
    except OSError as e:
        print(f"读不到 SSE 文件 {path}：{e}")
        return None
    # SSE 逐行解析：只认 'data: ' 前缀的行，正文可能被 JSON 转义过，
    # 所以先按行取 JSON，再把内容反序列化出来拼成纯文本再匹配。
    text_parts = []
    for ln in raw.splitlines():
        ln = ln.strip()
        if not ln.startswith("data: "):
            continue
        try:
            d = json.loads(ln[6:])
        except Exception:
            continue
        part = d.get("data")
        if isinstance(part, str):
            text_parts.append(part)
    m = LINE_RE.search("\n".join(text_parts))
    if not m:
        return None
    return tuple(int(g) for g in m.groups())


def main(argv):
    if len(argv) < 2:
        print("用法：stats_of_sse.py <sse文件> [--expect pages text_pages ocr_pages]")
        return 2
    path, want, rest = argv[1], None, argv[2:]
    if rest[:1] == ["--expect"]:
        if len(rest) != 4:
            print("--expect 需要三个整数：pages text_pages ocr_pages")
            return 2
        try:
            want = tuple(int(x) for x in rest[1:4])
        except ValueError:
            print("--expect 的三个参数必须是整数")
            return 2
    got = read_stats(path)
    if want is None:
        print("NONE" if got is None else "%d %d %d" % got)
        return 0 if got is not None else 1
    if got is None:
        print("FAIL 进度流里没有「共 N 页（文本层直取 X / OCR Y）」这一行 —— 择优逻辑没跑到或没上报")
        return 1
    if got != want:
        print("FAIL 逐页统计对不上：期望 pages/text/ocr=%d/%d/%d，线上是 %d/%d/%d"
              % (want + got))
        return 1
    print("共 %d 页（文本层直取 %d / OCR %d）" % got)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
