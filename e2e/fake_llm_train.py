#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""训练线假模型：替身 LLM，让 **训练**（Step1..Step9 + Step8.5 裁判循环）E2E 可确定性复跑。

【和 e2e/fake_llm.py 的分工】
  fake_llm.py        —— 运行时线（聊天：判类 → 执笔 → 审稿改稿 → 反问）。
  fake_llm_train.py  —— 训练线（/api/admin/train：抽特征 → 元数据 → 判类 → 生成提示词 →
                        结构抽取/审稿标准/大纲 → 生成示例 → 校验 → **8.5 裁判试用与回炉**
                        → 落盘）。本文件顶替真模型，把整条训练流水线跑完。

【为什么必须有一个「第 1 轮不合格、第 2 轮满分」的裁判】
  Step8.5 的全部价值在于「裁判真的跑过、并且交付的是最优那一轮的版本」。如果裁判永远
  给满分，循环只会跑 1 轮，「回炉」和「交付最优轮」两条路径都不会被走到——那就是一条
  只有绿灯、没有覆盖的假 E2E。所以这里的裁判按草稿里的轮次标记判：
    草稿含 TRIAL-R1 → 低分不通过（触发回炉）；含 TRIAL-R2 → 满分通过（触发交付）。
  判定用**草稿内容**而不是调用计数：重跑、乱序、并发都不影响结论。

【轮次怎么分辨】不用计数器，用提示词标记：Step4 产出的提示词含 SYS-V1-MARK，回炉产出的
  含 SYS-V2-MARK。试用请求的 system 就是被测技能自己那份 system_prompt，所以
  「system 含 V2」⇔「这一轮试用的是回炉后的版本」。这条正是 E2E 要证明的东西。

【分发规则（按 system 里的特征串，全部照抄产品源码里的提示词前缀）】
  结构抽取器         → 分类结构 JSON（anchors 直接取 fixture 里的 EX-* 行，保证能切出范文）
  独立的稿件评审裁判 → 裁判打分 JSON（jsonMode）
  修订任务           → 回炉版提示词（含 SYS-V2-MARK）
  资深写作研究员     → 写作特征 JSON
  设计元数据         → 元数据 JSON
  系统设计专家       → 技能类型 JSON（必须是 write）
  顶尖的提示词工程师 → Step4 提示词（含 SYS-V1-MARK）
  审稿标准的提炼器   → Markdown 清单
  写作大纲专家       → 大纲骨架文本
  system 含 V1/V2 且 user 含「这一类的要求写一篇稿件」→ 试用草稿
  其余              → 兜底：一段既像 JSON 又 ≥300 字的文本（**绝不 hang、绝不 500**）

每一笔请求（含回复全文）都落 FAKE_LLM_LOG（jsonl），E2E 事后核对「注入了什么」。
就绪行：FAKE_LLM_READY <port>
"""

import json
import os
import re
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOG = os.environ.get("FAKE_LLM_LOG", "/tmp/fake-llm-train.jsonl")

V1_MARK = "SYS-V1-MARK"
V2_MARK = "SYS-V2-MARK"
TRIAL_R1 = "TRIAL-R1"
TRIAL_R2 = "TRIAL-R2"

# 用来把「提示词」撑过本地硬门（validate: system_prompt.md 必须 ≥300 字符）。
PAD = (
    "执行时先判定本次需求属于手册哪一类，把该类写作要求逐条落实，事实只取用户给定素材，"
    "手册之外的事实一律不写入；结构上先亮主体、再列要件、最后落责任与时限；"
    "措辞克制正式，不使用主观评价、不使用网络流行语，不出现第一人称。"
) * 6


def log(kind, system, user, reply, stream):
    with open(LOG, "a", encoding="utf-8") as f:
        f.write(json.dumps({"kind": kind, "stream": stream, "system": system,
                            "user": user, "reply": reply},
                           ensure_ascii=False) + "\n")


# ---------- 训练期各阶段 ----------

def v1_prompt():
    """Step4 产出的 system_prompt（第 1 版）。"""
    return (
        V1_MARK + "\n"
        "身份：你是企业公文写作助手，依据写作手册的分类要求成稿。\n"
        "任务：按用户给定素材与手册该类写作要求，产出一篇可直接使用的稿件。\n"
        "执行步骤：一、判定分类；二、取该类写作要求与范文；三、起草；四、对照清单自检。\n"
        "结构规范：标题—主体—要件（责任人与完成时限）—落款。\n"
        "文风与措辞：正式、克制，不写主观评价。\n"
        "长度：600-1200 字。\n"
        "禁用项：不得使用第一人称；不得写入素材之外的事实。\n"
        "特殊要求：判不准分类时先向用户提问，不要自行猜测。\n" + PAD
    )


def v2_prompt():
    """回炉后产出的 system_prompt（第 2 版）：只针对扣分项补强。"""
    return (
        V2_MARK + "\n"
        "身份：你是企业公文写作助手，依据写作手册的分类要求成稿。\n"
        "任务：按用户给定素材与手册该类写作要求，产出一篇可直接使用的稿件。\n"
        "执行步骤：一、判定分类并说明依据；二、取该类写作要求与范文；三、起草；"
        "四、逐条对照该类要求与审稿清单自检，任一条不达标就改到达标再交付。\n"
        "结构规范：标题—主体—要件（责任人与完成时限，逐条可核对）—落款；"
        "每一条决议/措施都必须带上责任人与完成时限。\n"
        "文风与措辞：正式、克制，不写主观评价与会议气氛。\n"
        "长度：600-1200 字。\n"
        "禁用项：不得使用第一人称；不得写入素材之外的事实；不得出现推测性结论。\n"
        "特殊要求：判不准分类时先向用户提问，不要自行猜测。\n" + PAD
    )


def attrs_json():
    return json.dumps({
        "audience": "单位内部读者与对外公众",
        "style": "正式公文",
        "structure": "标题—主体—要件—落款",
        "tone": "克制、客观",
        "taboo": "不得使用第一人称", "length_guide": "600-1200 字",
        "term_note": "责任人与完成时限必须写明",
    }, ensure_ascii=False)


def meta_json():
    return json.dumps({
        "name": "公文写作助手",
        "description": "依据写作手册分类要求生成企业内部公文与对外通稿。",
        "input_params": [
            {"name": "topic", "label": "主题", "type": "text", "required": True, "placeholder": "要写什么"},
            {"name": "material", "label": "素材", "type": "textarea", "required": True, "placeholder": "事实来源"},
            {"name": "style", "label": "语气", "type": "select", "required": False, "options": ["正式", "平实"]},
        ],
    }, ensure_ascii=False)


def type_json():
    # 必须回 write：write 分支才会走「母模板 + 范文」并在 8.5 里被试用。
    return json.dumps({"type": "write", "attachment": "", "rationale": "手册是写作类，产出成稿"},
                      ensure_ascii=False)


def outline_text():
    return ("# 大纲模板\n一、标题\n二、导语（时间/地点/主体/事件）\n"
            "三、主体（决议或事实，逐条带责任人与完成时限）\n四、落款\n" + PAD)


def review_checklist():
    return ("# 审稿标准清单\n- CHK-1：首段是否交代时间、地点、主体、事件。\n"
            "- CHK-2：每条决议或措施是否写明责任人与完成时限。\n"
            "- CHK-3：是否出现第一人称或主观评价。\n- CHK-4：是否有素材之外的事实。\n")


# ---------- Step5：结构抽取（关键：anchors 必须是原文里真实存在的片段） ----------

def _anchor(line):
    """把 fixture 的 EX-* 整行拆成 start/end 两段原文片段。

    用「前 14 字 / 后 14 字」而不是整行：产品侧要求锚点在原文里唯一出现，
    首尾各取一段既能唯一定位、又不会因为行长而撞上「锚点过长」的限制。
    """
    r = line.strip()
    n = len(r)
    if n <= 34:
        return {"start": r, "end": r}
    return {"start": r[:14], "end": r[-14:]}


def structure_json(user):
    """只抽取**本次 prompt 里真的出现过**的分类（源码可能按块多次调用后合并）。

    分类名/要求/范文一律照 fixture 原文取——结构抽取器的契约就是「摘录 + 定位」，
    假模型也照这个契约来，锚点才切得出手册范文（loadPack 切不出范文会触发硬校验）。
    """
    cats = []
    cur = None
    for raw in user.splitlines():
        s = raw.strip()
        if s.startswith("## "):
            cur = {"name": s[3:].strip(), "trigger": "", "requirement": "", "anchors": []}
            cats.append(cur)
        elif cur is not None:
            if s.startswith("TRIG:"):
                cur["trigger"] = s[5:].strip()
            elif s.startswith("REQ-"):
                cur["requirement"] = s
            elif s.startswith("EX-"):
                cur["anchors"].append(_anchor(s))
    cats = [c for c in cats if c["anchors"] and c["requirement"]]
    return json.dumps({"general": "依据写作手册分类要求成稿，事实只取用户素材。", "categories": cats},
                      ensure_ascii=False)


# ---------- Step8.5：试用草稿与裁判打分 ----------

def trial_draft(system):
    if V2_MARK in system:
        return (TRIAL_R2 + "\n"
                "周例会决议：由办公室在九月二十日前完成整改并书面回执，责任人为李四；"
                "逾期由分管领导督办，办理结果于下周复核后书面反馈。\n")
    return (TRIAL_R1 + "\n"
            "本次会议开得很成功，与会人员都很满意。会上决定整改一下相关问题。\n"
            "其余事项稍后再议。\n")


def judge_json(user):
    """第 1 轮低分（不通过 → 触发回炉），第 2 轮满分（通过 → 交付）。"""
    if TRIAL_R2 in user:
        return json.dumps({
            "dims": [
                {"key": "category_routing", "score": 25, "reason": "文体与用途与本类要求一致。"},
                {"key": "requirement_compliance", "score": 25, "reason": "责任人与完成时限均已写明。"},
                {"key": "example_alignment", "score": 20, "reason": "结构与措辞与该类范文同一路数。"},
                {"key": "structure_completeness", "score": 15, "reason": "标题、主体、要件齐全。"},
                {"key": "no_hallucination", "score": 15, "reason": "事实均可在素材中找到依据。"},
            ],
            "findings": [],
        }, ensure_ascii=False)
    return json.dumps({
        "dims": [
            {"key": "category_routing", "score": 12, "reason": "稿件不像该类要求的文体。"},
            {"key": "requirement_compliance", "score": 10, "reason": "缺少责任人与完成时限。"},
            {"key": "example_alignment", "score": 8, "reason": "措辞密度与该类范文差距明显。"},
            {"key": "structure_completeness", "score": 6, "reason": "缺少要件段与落款。"},
            {"key": "no_hallucination", "score": 9, "reason": "出现了素材之外的推测性表述。"},
        ],
        "findings": ["要求依从 10/25：缺少责任人与完成时限", "结构完整 6/15：缺少要件段与落款"],
    }, ensure_ascii=False)


def fallback():
    """不认识的提示词：返回一段既像 JSON、又 ≥300 字的文本。

    为什么要这样兜底：这条流水线里既有「要 JSON」的阶段也有「要正文」的阶段，
    分发漏了一个就会 hang 或 500，把 E2E 变成假红。兜底文本两头都接得住，
    并且以 kind=fallback 落盘——E2E 断言「兜底次数为 0」，漏了分发会被点名。
    """
    return json.dumps({"note": "未识别的提示词（E2E 兜底回复）。" + PAD,
                       "reason": "fallback"}, ensure_ascii=False)


def dispatch(system, user, stream):
    """按 system 特征串分发。顺序：先窄后宽，避免宽规则吃掉窄规则。"""
    if "你是写作手册的结构抽取器" in system:
        kind, text = "structure", structure_json(user)
    elif "你是独立的稿件评审裁判" in system:
        kind, text = "judge", judge_json(user)
    elif "这是一次**修订**任务" in system:
        kind, text = "revise_prompt", v2_prompt()
    elif "资深写作研究员" in system:
        kind, text = "attrs", attrs_json()
    elif "设计元数据" in system:
        kind, text = "meta", meta_json()
    elif "你是系统设计专家" in system:
        kind, text = "type", type_json()
    elif "你是写作大纲专家" in system:
        kind, text = "outline", outline_text()
    elif "审稿标准的提炼器" in system:
        kind, text = "review_std", review_checklist()
    elif "你是一位顶尖的提示词工程师" in system:
        kind, text = "sysprompt_v1", v1_prompt()
    elif (V1_MARK in system or V2_MARK in system) and "这一类的要求写一篇稿件" in user:
        kind, text = "trial", trial_draft(system)
    else:
        kind, text = "fallback", fallback()
    log(kind, system, user, text, stream)
    return text


# ---------- HTTP ----------

class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, format, *args):  # noqa: A002 —— 逐字对齐 BaseHTTPRequestHandler 的签名（覆盖时改名会与基类关键字调用不兼容）
        pass

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(n)
        try:
            body = json.loads(raw)
        except Exception:
            self.send_error(400)
            return
        msgs = body.get("messages") or []
        system = "\n".join(m.get("content", "") for m in msgs if m.get("role") == "system")
        user = "\n".join(m.get("content", "") for m in msgs if m.get("role") == "user")
        stream = bool(body.get("stream"))
        text = dispatch(system, user, stream)

        if stream:
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Connection", "keep-alive")
            self.end_headers()
            for i in range(0, len(text), 24):
                chunk = {"id": "fake", "object": "chat.completion.chunk",
                         "created": 0, "model": body.get("model", "fake"),
                         "choices": [{"index": 0, "delta": {"content": text[i:i + 24]},
                                      "finish_reason": None}]}
                self.wfile.write(("data: " + json.dumps(chunk, ensure_ascii=False) + "\n\n").encode())
                self.wfile.flush()
            end = {"id": "fake", "object": "chat.completion.chunk", "created": 0,
                   "model": body.get("model", "fake"),
                   "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]}
            self.wfile.write(("data: " + json.dumps(end, ensure_ascii=False) + "\n\n").encode())
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
            return

        payload = {"id": "fake", "object": "chat.completion", "created": 0,
                   "model": body.get("model", "fake"),
                   "choices": [{"index": 0, "finish_reason": "stop",
                                "message": {"role": "assistant", "content": text}}],
                   "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}
        data = json.dumps(payload, ensure_ascii=False).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


def main():
    port = int(os.environ.get("FAKE_LLM_PORT", "0"))
    open(LOG, "w").close()
    srv = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    print("FAKE_LLM_READY %d" % srv.server_address[1], flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    sys.exit(main())
