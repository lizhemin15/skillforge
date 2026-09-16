#!/usr/bin/env python3
# LIVE-LEGS: train_progress MIN_LINES=2
"""训练页「进度帧」线上终验：真浏览器 + 真服务 + 真模型。

盯的是用户原话那个故障：「现在一直卡着计时，用户体验不佳」。

根因（2026-09-16 定位）：后端 internal/api/admin.go 发 `status` / `step` 帧，
前端 web/js/admin.js 只 `case 'stage'` —— 于是「开始训练」+ 九个阶段的进度帧
**全被静默丢弃**：不报错、控制台干净、日志框空的、按钮不动。二十分钟屏幕上
只有一条不动的进度条，看起来就是卡死。

所以这条 leg 断言的**不是**「事件名字符串对不对」（那是
web/tests/sse_event_contract.test.mjs 在 CI 里守的），而是**真页面上有没有东西在动**：
  T1 日志框里真的出现了 ≥MIN_LINES 行
  T2 其中至少一行是阶段进度（形如 `1/9`）或「开始训练」
  T3 计时器文字真的在每秒变（≥3 个不同快照，不是静止的一块）
  T4 全程无 JS 异常

为什么必须真跑一遍：单测只能证明「前端认得 status/step 这两个名字」，
证明不了「后端真发了、SSE 真送到了、DOM 真渲染了」。这条链上任何一环断了，
单测还是全绿 —— 而用户看到的还是那个不动的框。

用法：
  python3 web/tests/admin_train_progress_e2e.py                 # 打线上默认地址
  BASE=http://127.0.0.1:8080 python3 web/tests/admin_train_progress_e2e.py
  MIN_LINES=3 ...              # 提高日志行数门槛

前置断言（血泪教训，别删）：
  「点了开始训练」之后**必须先证明请求真的发出去了**，再去判屏幕有没有动。
  2026-09-16 首次跑就栽在这：`#tr-desc`（一句话描述）带 `required`，脚本只填了
  名称和要求 —— 浏览器原生表单校验直接拦下提交，**没有 JS 异常、没有网络请求、
  按钮文案不变**，于是 T1~T5 全红、报告写的是「页面坏了」。页面是好的，
  红的是脚本少填了一个必填项。凡是「脚本动作没生效」都不许报成「被测系统坏了」。
  现在脚本会：先做 checkValidity 自查（把不合法的字段 id 直接点出来），
  再监听 /api/admin/train 请求，请求没发出就以**脚本原因**收场。

代价与副作用（必须显性说出来）：
  训练是二十分钟的长跑。本脚本只采样开头的 SAMPLE_SECONDS 秒，够拿到
  「屏幕在动」的证据就断开（浏览器关掉 = SSE 断开）。断开会让它白跑一轮，
  所以脚本结束时会打印【清理提示】和删除命令 —— 不要装作没发生。
  拿不到证据（连不上服务 / 没有 playwright / 登录不了）时 **SKIP 退出 0**，
  但 SKIP ≠ PASS，报告里必须写清是 SKIP。
"""
import json
import os
import re
import sys
import time

BASE = os.environ.get('BASE', 'https://skillforge.open-claw.click')
MIN_LINES = int(os.environ.get('MIN_LINES', '2'))
SAMPLE_SECONDS = float(os.environ.get('SAMPLE_SECONDS', '90'))
TRAIN_NAME = os.environ.get('TRAIN_NAME', '线上训练进度验收160916')

# 素材用一段真需求（不给文件也能训练）。提示词写在这里而不是 runner 的 env 里：
# 这是最该被审查的东西，藏进 scripts/ 没人看得见（roster 测试的注释专门讲过这点）。
# 一句话描述是表单必填项（#tr-desc 带 required）。不填它，浏览器原生校验会
# 静默拦下提交 —— 脚本看起来「点了」，其实什么都没发出去。
DESC = os.environ.get('TRAIN_DESC', '依据上级通知训练数据治理专项工作公文体例')

REQUIREMENT = (
    '根据以下材料训练出技能：单位收到上级关于开展数据治理专项工作的通知后，'
    '成立专项工作组，明确数据责任人，按季度完成数据资产盘点与质量核查，'
    '形成问题清单并限期整改。输出体例为正式公文，标题、正文、落款齐全。'
)

ok_cnt = 0
fail_cnt = 0
skips = []


def ok(msg):
    global ok_cnt
    ok_cnt += 1
    print(f'ok   {msg}')


def fail(msg):
    global fail_cnt
    fail_cnt += 1
    print(f'FAIL {msg}')


def skip(msg):
    skips.append(msg)
    print(f'SKIP {msg}')


def main():
    try:
        from playwright.sync_api import sync_playwright
    except Exception as e:  # noqa: BLE001
        skip(f'没装 playwright（{e}）—— 真浏览器终验跑不了，SKIP 不等于 PASS')
        return 0

    with sync_playwright() as p:
        try:
            browser = p.chromium.launch(args=['--no-sandbox'])
        except Exception as e:  # noqa: BLE001
            skip(f'打不开浏览器（{e}）')
            return 0

        page = browser.new_page(viewport={'width': 1440, 'height': 900})
        errs = []
        page.on('pageerror', lambda e: errs.append(str(e)))
        # 记下发往 /api/admin/train 的请求：这是「点击真的到了后端」的硬证据，
        # 光看屏幕动没动分不清「前端吞帧」和「压根没发请求」。
        train_reqs = []
        page.on('request', lambda r: train_reqs.append(r.url) if '/api/admin/train' in r.url else None)
        try:
            page.goto(BASE + '/admin', wait_until='domcontentloaded', timeout=20000)
        except Exception as e:  # noqa: BLE001
            skip(f'打不开 {BASE}/admin（{e}）')
            browser.close()
            return 0

        # 训练页在登录墙后面：凭据从环境变量走，**不进脚本、不进日志**。
        # 没给凭据就 SKIP（而不是伪造一个 token 绕过去 —— 那样验的不是用户走的路）。
        user, pwd = os.environ.get('ADMIN_USER'), os.environ.get('ADMIN_PASS')
        if user and pwd:
            try:
                page.fill('#lg-user', user)
                page.fill('#lg-pass', pwd)
                page.click('#login-form button[type=submit]')
                # 训练表单住在「新建技能」弹窗（#skill-new）里，登录完成时它还没被打开。
                # 早期版本在这里直接等 #train-form，等的是一个永远不会可见的元素，
                # 于是每轮都以「登录后训练页没出现」这种**假红**收场 —— 页面本身是好的，
                # 红的是脚本少点了一下。真终验不能留这种狼来了。
                # 登录后落在「LLM 服务」tab，训练表单在 #tab-skills 里（隐藏）。
                # 三步缺一不可：登录 → 切「技能管理」→ 开「新建技能」弹窗。
                # 早期版本只做了第一步就去等 #train-form，等的是永远不可见的元素。
                page.click('button[data-tab="skills"]')
                page.click('button[onclick*="newSkillView"]')
                page.wait_for_selector('#train-form', timeout=15000, state='visible')
                ok('已用真凭据登录 → 技能管理 → 新建技能，训练表单可见（凭据走环境变量，未落盘、未打印）')
            except Exception as e:  # noqa: BLE001
                fail(f'登录后打不开训练表单（{type(e).__name__}: {str(e)[:120]}）')
                browser.close()
                return 1

        # 判「前提在不在」必须看**可见性**，不能看 count()：#train-form 一直在 DOM 里
        # （住在新技能弹窗里），未登录时只是隐藏。拿 count() 当门槛会漏过这一层，
        # 下一行 fill 立刻抛 "element is not visible" —— 尺子前提没满足却崩成 Traceback
        # （RC=1），把「我没给凭据」演成「被测系统坏了」。SKIP 要 SKIP 干净。
        if not page.locator('#train-form').is_visible():
            skip('训练表单不可见（未登录 / 不是训练页）—— SKIP 不等于 PASS；'
                 '要真跑请给 ADMIN_USER/ADMIN_PASS')
            browser.close()
            return 0

        # 开跑（三个必填/入参都得给：少一个就可能被原生校验静默拦下）
        page.fill('#tr-name', TRAIN_NAME)
        page.fill('#tr-desc', DESC)
        page.fill('#tr-req', REQUIREMENT)

        # 前置自查：表单本身合不合法？不合法就点名是哪个字段，别让「脚本没填够」
        # 伪装成「页面坏了」。
        bad = page.evaluate(
            "() => Array.from(document.querySelectorAll('#train-form [required]'))"
            ".filter(e => !e.checkValidity()).map(e => e.id || e.name || e.tagName)")
        if bad:
            fail(f'PROBE 训练表单原生校验未通过，提交会被浏览器静默拦下 —— '
                 f'这是**脚本原因**（少填了必填项），不是页面故障。不合法字段：{bad}')
            browser.close()
            return 1

        page.click('#tr-go')

        # 前置断言：点击必须在 15s 内产出真的训练请求
        deadline = time.time() + 15
        while time.time() < deadline and not train_reqs:
            time.sleep(0.3)
        if not train_reqs:
            fail('PROBE 点了开始训练，但 15s 内没有发出 /api/admin/train 请求 —— '
                 '这是**脚本动作没生效**（点击目标/表单状态），不是页面故障；'
                 '此时报告里的屏幕静止标记为不可用，不当作界面缺陷')
            browser.close()
            return 1
        ok(f'PROBE 点击已触发训练请求（{train_reqs[0][:60]}）—— 屏幕断言的前提成立')

        t0 = time.time()
        line_seen = []          # (t, 行文本)
        tick_snaps = []         # 计时器文字的不同快照
        last_tick = None
        while time.time() - t0 < SAMPLE_SECONDS:
            try:
                rows = page.eval_on_selector_all(
                    '#tr-log .ln', "els => els.map(e => e.innerText.replace(/\\s+/g,' ').trim())")
            except Exception:  # noqa: BLE001
                rows = []
            for r in rows:
                if not any(r == x[1] for x in line_seen):
                    line_seen.append((round(time.time() - t0, 1), r))
            try:
                tick = (page.inner_text('#tr-txt') or '').strip()
            except Exception:  # noqa: BLE001
                tick = ''
            if tick and tick != last_tick:
                # 带上到达时刻：光有文字没法回答「最长静默多久」（这是用户体感的核心指标）
                tick_snaps.append((round(time.time() - t0, 1), tick))
                last_tick = tick
            time.sleep(0.4)

        # ---- T1 日志框里有真行 ----
        if len(line_seen) >= MIN_LINES:
            ok(f'T1 日志框真出现 ≥{MIN_LINES} 行（实得 {len(line_seen)} 行）')
        else:
            fail(f'T1 日志框 {SAMPLE_SECONDS:.0f}s 内只有 {len(line_seen)} 行（期望 ≥{MIN_LINES}）'
                 f'—— 这就是「不动的框」；前端把后端帧吞了就会这样。实得：{line_seen}')

        # ---- T2 其中有阶段进度 ----
        stage_rows = [r for _, r in line_seen if re.search(r'\d+\s*/\s*\d+', r) or '开始训练' in r]
        if stage_rows:
            ok(f'T2 出现阶段/开始帧：{stage_rows[0][:70]}')
        else:
            fail(f'T2 {SAMPLE_SECONDS:.0f}s 内没有阶段进度行（期望形如 `1/9`）—— '
                 f'后端在跑但进度到不了屏幕')

        # ---- T3 计时器真的在动 ----
        if len(tick_snaps) >= 3:
            ok(f'T3 计时器文字在动（{len(tick_snaps)} 个不同快照）：首次「{tick_snaps[0][1][:40]}」'
               f' 末次「{tick_snaps[-1][1][:40]}」')
        else:
            fail(f'T3 计时器只有 {len(tick_snaps)} 个快照（期望 ≥3）—— '
                 f'屏幕是静止的：{tick_snaps[:3]}')

        # ---- T3b 心跳文案真的说了「距上次进度」（这是长跑任务的活气声明）----
        hb = [(t, s) for t, s in tick_snaps if '已' in s]
        if hb:
            ok(f'T3b 心跳文案含已用时长：{hb[-1][1][:60]}')
        else:
            fail('T3b 计时器里没有「已用时长」文案（心跳没在更新）')

        # ---- T4 无 JS 异常 ----
        if not errs:
            ok('T4 全程无 JS 异常')
        else:
            fail(f'T4 有 {len(errs)} 个 JS 异常：{errs[:3]}')

        # ---- T5 中间材料真的流到屏幕上（「卡着计时」这次的核心）----
        # T1~T4 守的都是「帧有没有到屏幕」；阶段内部静默那两分钟，屏幕上有进度行、
        # 计时器也在动，T1~T4 全绿而用户照样在盯空计时器。所以必须单独守材料。
        mat_rows = [(t, r) for t, r in line_seen if '思考：' in r or '正文：' in r or '提示：' in r]
        try:
            mat_nodes = page.eval_on_selector_all('#tr-log .material', 'els => els.length')
        except Exception:  # noqa: BLE001
            mat_nodes = -1
        if mat_rows:
            ok(f'T5a {SAMPLE_SECONDS:.0f}s 内出现流式中间材料 {len(mat_rows)} 次，'
               f'首片在 {mat_rows[0][0]:.1f}s：{mat_rows[0][1][:70]}')
        else:
            fail(f'T5a {SAMPLE_SECONDS:.0f}s 内没有任何中间材料行 —— 阶段内部仍是静默，'
                 f'用户还是只能盯着一个空计时器（改动没生效，或帧被前端吞了）')
        if mat_nodes == 1:
            ok('T5b 实况块只占 1 个 DOM 节点（就地更新，不是一片一节点）')
        else:
            fail(f'T5b 实况块占 {mat_nodes} 个 DOM 节点（期望 1）—— '
                 f'一片一节点会随训练时长线性增长，二十分钟下来把页面拖死')

        # 完整时间线落盘（算最长静默空档要用全量，不是前 6 行）
        out = os.environ.get('LINES_OUT')
        if out:
            with open(out, 'w') as fh:
                json.dump({'sample_seconds': SAMPLE_SECONDS, 'lines': line_seen,
                           'ticks': tick_snaps, 'js_errors': errs,
                           'ok': ok_cnt, 'fail': fail_cnt, 'skips': skips}, fh, ensure_ascii=False)
            print(f'时间线已落盘 → {out}（{len(line_seen)} 行 / {len(tick_snaps)} 个计时快照）')

        print('\n--- 证据 ---')
        print(f'采样窗口 {SAMPLE_SECONDS:.0f}s / 日志行 {len(line_seen)} / 计时快照 {len(tick_snaps)}')
        for t, r in line_seen[:6]:
            print(f'  [{t:>6.1f}s] {r[:110]}')
        print('--- 清理提示（不装看不见）---')
        print(f'本轮训练是采样后主动断开的，后端可能仍在跑并最终生成技能「{TRAIN_NAME}」。')
        print(f'验收完请删掉它，别留在线上：')
        print(f"  curl -X DELETE -H 'Authorization: Bearer <admin-token>' \\")
        print(f"    '{BASE}/api/admin/skills/{TRAIN_NAME}'")
        browser.close()

    print(f'\n--- {ok_cnt} ok / {fail_cnt} fail ---')
    if fail_cnt:
        print('FAILED: 训练页进度帧链路（真浏览器）')
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
