#!/usr/bin/env python3
# LIVE-LEGS: train_visible TIMEOUT_S=150
"""训练页「中间材料真的在用户视野里」线上终验：真浏览器 + 真服务 + 真登录。

盯的是用户原话：「现在速度过于慢了，中间可以流式输出思考的一些中间材料，
现在一直卡着计时，用户体验不佳」。

跟前一条腿（admin_train_progress_e2e.py）的分工，必须说清楚，别当成重复：
  · train_progress 腿跑**真训练**（真模型、二十分钟），证明「帧真的从后端流到了屏幕」；
  · 本腿**把 /api/admin/train 换成桩帧**，专证「材料真的落在用户看得见的地方」。
    为什么这条必须用桩：这是个**纯几何**问题，跟训练内容无关，用桩才可重跑、可秒级
    复现，也才能在只有 150s 的预算里拿到结论。真帧链路已由上面那条腿覆盖。

根因（2026-09-18 线上探测，数字是实测的）：
  `.modal` 自己就是滚动容器（web/css/style.css `max-height:86vh; overflow-y:auto`），
  长「写作要求」把内容撑到 1130px（可视 772px）：
    `#tr-log` 只露出 45px，材料块整体在 modal 底线**外 329px**，modal.scrollTop 停在 0。
  旧代码只做内层 `$('tr-log').scrollTop = scrollHeight`，**从没管外层 modal** ——
  所以「材料在流、计时器在跳，用户什么也看不见」，看到的就是一个不动的框。

断言（V5 是主体，其余是它的前提，缺一条都不算数）：
  V1 真凭据登录 → 技能管理 → 新建技能，训练表单可见
  V2 点击发出了 /api/admin/train 请求
  V3 材料块出现（且**只有 1 个** —— 一片一节点会把页面拖死，历史形态）
  V4 材料块文字里真带「思考/正文」标签（证明帧被解析渲染了，不是个空壳）
  V5 **主体**：材料块整体落在 #skill-new 可视区内（上缘不低于容器上缘、下缘不超过容器下缘）
  V6 内层日志贴底（材料像终端一样滚）
  V7 全程无 JS 异常
  V8 前提自证：容器**真的溢出**（scrollHeight-clientHeight ≥ 20）—— 不溢出则 V5 恒真，
     这时报 FAIL 并说明「这把尺子失去意义」，不许当通过
  V9 1.5s 后复测仍在内（防「贴底一瞬间路过」的假绿）

负向自证（可随时重跑，务必跑一次再相信这条腿）：
  把出货 web/js/admin.js 里那句 `if (on) modal.scrollTop = modal.scrollHeight;` 删掉，
  存成任意路径，然后：
    SF_MUTATE_ADMIN_JS=/tmp/admin_no_pin.js BASE=http://127.0.0.1:8092 \
      python3 web/tests/train_material_visibility_e2e.py
  期望：V5 精确转红、其余仍绿、rc=1（精确到「红的正是预期那条」）。还原后回绿。
  实测记录：2026-09-18 对**修复前已部署的**线上前端跑本脚本 → V5 红、材料块下缘在
  容器底线外 335px；注入式自证同样只红 V5。

用法：
  python3 web/tests/train_material_visibility_e2e.py
  BASE=http://127.0.0.1:8092 ADMIN_USER=… ADMIN_PASS=… python3 …   # 凭据走环境变量
  没装 playwright / 没给凭据 / 打不开页面 → SKIP 退出 0（SKIP ≠ PASS）。
"""
import json
import os
import sys
import time

BASE = os.environ.get('BASE', 'https://skillforge.open-claw.click')
# runner 会把 leg 里的 TIMEOUT_S 传进来（roster 测试要求 leg 里的键必须被脚本真读到）。
# 这里只用它给自己划页面等待预算，外层硬砍由 runner 的 timeout 负责。
BUDGET_S = float(os.environ.get('TIMEOUT_S', '150'))
SHOT = os.environ.get('SHOT_OUT', '/tmp/sf_shots/train_material_visibility.png')
# 自证用：指向一份被改坏的 admin.js，脚本会用它顶掉线上资产（只影响本次浏览器会话）。
MUTATE_ADMIN_JS = os.environ.get('SF_MUTATE_ADMIN_JS') or ''

# 「写作要求」必须够长——本缺陷的前提就是「长表单把实况区顶出视野」。
# 短提示词下 modal 不溢出，V5 会变成恒真的空跑（V8 就是专门拦这个的）。
REQUIREMENT = (
    '【体例要求】\n'
    '一、标题：二号方正小标宋简体，居中，不加书名号；副标题三号楷体_GB2312，居中。\n'
    '二、正文：三号仿宋_GB2312，行距 28 磅，首行缩进 2 字符；一级标题黑体，'
    '二级标题楷体_GB2312，层级依次为「一、」「（一）」「1.」「（1）」。\n'
    '三、落款：单位全称 + 成文日期（阿拉伯数字，右空四字），加盖公章位置留白。\n'
    '四、数据与事实必须来自素材，不得编造；缺失项写「详见附件」，不要自由发挥。\n'
    '五、全文结构固定为：背景与依据 → 工作目标 → 重点任务 → 保障措施 → 时间安排。\n'
    '六、重点任务部分按「任务名称—责任部门—完成时限—成果形式」四段式逐条展开，'
    '每条不超过 200 字，条与条之间不空行。\n'
    '七、涉及数字的表述统一用阿拉伯数字；涉及单位的表述首次出现用全称，'
    '之后用简称并在此处标注「（以下简称××）」。\n'
    '八、不要出现「首先/其次/最后」这类口语化连接词，改用公文惯用序次语。\n'
    '九、结尾不用「谢谢」「此致敬礼」，直接以落款结束。\n'
    '十、输出纯文本，不要 Markdown 标记，不要加粗星号，不要在任何位置插入分隔线。\n'
    '（以上为范文体例，请严格对齐；本段刻意写长，用于复现「长表单把实况区顶出视野」。）'
)


def stub_sse_body():
    """按后端真实帧格式造一段桩 SSE（internal/api/admin.go 的 send + material relay）。

    后端实际发的是 `data: {"type":"delta","data":"{\\"kind\\":\\"think\\",…}"}` ——
    `data` 是**被转义的 JSON 字符串**（两层编码），这里必须照抄，否则就变成
    「前端解析路径没被覆盖，但尺子绿了」的假绿。
    不补 `done` 帧：前端按设计在 done/error 时把实况块收掉（怕人以为还在跑），
    而本腿要量的正是它——留着它，几何才可测。
    """
    def frame(t, data):
        return 'data: ' + json.dumps({'type': t, 'data': data}, ensure_ascii=False) + '\n\n'

    parts = [frame('status', '开始训练技能：线上材料可视性验收')]
    parts.append(frame('step', '1/9 读取素材'))
    parts.append(frame('delta', json.dumps({'kind': 'think', 'text':
        '先看清楚素材里给了哪些体例约束：标题字号、正文行距、落款位置都要逐条对齐，'
        '再把重点任务的四段式结构抽出来。'}, ensure_ascii=False)))
    parts.append(frame('step', '2/9 抽取体例'))
    for i in range(12):
        parts.append(frame('delta', json.dumps({'kind': 'text', 'text':
            f'第{i + 1}段范文骨架：背景与依据—工作目标—重点任务—保障措施—时间安排；'
            '这一段的取数是「责任部门 + 完成时限 + 成果形式」三件套，缺项写详见附件。'},
            ensure_ascii=False)))
    parts.append(frame('step', '3/9 归纳参数'))
    parts.append(frame('delta', json.dumps({'kind': 'note', 'text':
        '素材里没有明确说公文字号，按范文推断为三号仿宋，这一条要在 fidelity.md 里标出来。'},
        ensure_ascii=False)))
    return ''.join(parts)


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

    if MUTATE_ADMIN_JS and not os.path.exists(MUTATE_ADMIN_JS):
        skip(f'SF_MUTATE_ADMIN_JS 指向的文件不存在：{MUTATE_ADMIN_JS}')
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
        train_reqs = []
        page.on('request', lambda r: train_reqs.append(r.method + ' ' + r.url)
                if '/api/admin/train' in r.url else None)

        # 自证模式：顶掉线上 admin.js（只影响这个浏览器会话，不动服务端）。
        if MUTATE_ADMIN_JS:
            with open(MUTATE_ADMIN_JS, 'rb') as fh:
                mutated = fh.read()
            page.route('**/assets/js/admin.js*', lambda route: route.fulfill(
                status=200, headers={'Content-Type': 'application/javascript; charset=utf-8'},
                body=mutated))
            print(f'⚠️  自证模式：本次加载的是被注入的 admin.js（{MUTATE_ADMIN_JS}，'
                  f'{len(mutated)} 字节）—— 期望只有 V5 转红')

        # 桩掉训练请求：本腿只判几何，不跑二十分钟真训练（分工见文件头）。
        body = stub_sse_body()
        page.route('**/api/admin/train', lambda route: route.fulfill(
            status=200,
            headers={'Content-Type': 'text/event-stream; charset=utf-8',
                     'Cache-Control': 'no-cache'},
            body=body))

        try:
            page.goto(BASE + '/admin', wait_until='domcontentloaded', timeout=20000)
        except Exception as e:  # noqa: BLE001
            skip(f'打不开 {BASE}/admin（{e}）')
            browser.close()
            return 0

        user, pwd = os.environ.get('ADMIN_USER'), os.environ.get('ADMIN_PASS')
        if user and pwd:
            try:
                page.fill('#lg-user', user)
                page.fill('#lg-pass', pwd)
                page.click('#login-form button[type=submit]')
                page.click('button[data-tab="skills"]')
                page.click('button[onclick*="newSkillView"]')
                page.wait_for_selector('#train-form', timeout=15000, state='visible')
                ok('V1 真凭据登录 → 技能管理 → 新建技能，训练表单可见'
                   '（凭据走环境变量，未落盘、未打印）')
            except Exception as e:  # noqa: BLE001
                fail(f'V1 登录后打不开训练表单（{type(e).__name__}: {str(e)[:120]}）')
                browser.close()
                return 1
        if not page.locator('#train-form').is_visible():
            skip('训练表单不可见（未登录 / 不是训练页）—— SKIP 不等于 PASS；'
                 '要真跑请给 ADMIN_USER/ADMIN_PASS')
            browser.close()
            return 0

        # 三个字段都得填：少一个会被浏览器原生校验静默拦下，
        # 演成「点了没反应」的假红（历史形态，见 admin_train_progress_e2e.py 的注释）。
        page.fill('#tr-name', '线上材料可视性验收' + time.strftime('%m%d%H%M%S'))
        page.fill('#tr-desc', '复现长表单把训练实况区顶出视野的布局')
        page.fill('#tr-req', REQUIREMENT)
        bad = page.evaluate(
            "() => Array.from(document.querySelectorAll('#train-form [required]'))"
            ".filter(e => !e.checkValidity()).map(e => e.id || e.name || e.tagName)")
        if bad:
            fail(f'PROBE 训练表单原生校验未通过，提交会被静默拦下 —— 这是**脚本原因**'
                 f'（少填必填项），不是页面故障。不合法字段：{bad}')
            browser.close()
            return 1

        page.click('#tr-go')

        # V2 前提：点击真的发出了请求（否则后面所有屏幕断言都不成立）
        deadline = time.time() + min(15, BUDGET_S)
        while time.time() < deadline and not train_reqs:
            time.sleep(0.3)
        if not train_reqs:
            fail('V2 PROBE 点了开始训练，但 15s 内没有发出 /api/admin/train 请求 —— '
                 '这是**脚本动作没生效**，不是页面故障；后续屏幕断言不成立')
            browser.close()
            return 1
        ok(f'V2 点击已触发训练请求（{train_reqs[0][:60]}）—— 屏幕断言的前提成立')

        # V3 前提：材料块出现。桩帧是一次性送达的，给它 20s。
        mat_deadline = time.time() + min(20, BUDGET_S)
        while time.time() < mat_deadline:
            if page.evaluate("() => !!document.getElementById('tr-material')"):
                break
            time.sleep(0.2)

        GEO_JS = """() => {
          const box = document.getElementById('skill-new');
          const log = document.getElementById('tr-log');
          const mat = document.getElementById('tr-material');
          const br = box.getBoundingClientRect();
          const lr = log.getBoundingClientRect();
          const mr = mat ? mat.getBoundingClientRect() : null;
          const r1 = (n) => Math.round(n);
          return {
            box: {top: r1(br.top), bottom: r1(br.bottom), h: r1(br.height),
                  scrollTop: r1(box.scrollTop), scrollHeight: r1(box.scrollHeight),
                  clientHeight: r1(box.clientHeight)},
            log: {top: r1(lr.top), bottom: r1(lr.bottom),
                  scrollTop: r1(log.scrollTop), scrollHeight: r1(log.scrollHeight),
                  clientHeight: r1(log.clientHeight),
                  visiblePx: r1(Math.max(0, Math.min(lr.bottom, br.bottom) -
                                         Math.max(lr.top, br.top)))},
            mat: mr ? {top: r1(mr.top), bottom: r1(mr.bottom), h: r1(mr.height),
                       last: (mat.textContent || '').slice(-70)} : null,
            matCount: document.querySelectorAll('#tr-log .material').length,
          };
        }"""
        geo = page.evaluate(GEO_JS)

        if not geo['mat']:
            fail('V3 PROBE 桩帧发完了，页面上**没有** #tr-material 块 —— '
                 '中间材料根本没上屏（后续几何断言不成立）')
            browser.close()
            return 1
        ok(f'V3 材料块已上屏（高 {geo["mat"]["h"]}px，末尾「{geo["mat"]["last"][:40]}」）')

        # V3b 一片一节点会把长跑页面拖死（历史形态）：必须只有 1 个块
        if geo['matCount'] == 1:
            ok('V3b 实况块只有 1 个（就地更新，不是一片一个 DOM 节点）')
        else:
            fail(f'V3b 实况块有 {geo["matCount"]} 个 —— 二十分钟下来几万个节点会拖死页面')

        # V4 前提：材料文字真带标签，证明帧被解析渲染（不是个空壳）
        if '思考' in (geo['mat']['last'] or '') or '正文' in (geo['mat']['last'] or '') \
                or '提示' in (geo['mat']['last'] or ''):
            ok('V4 材料块里是真材料文字（带「思考/正文/提示」标签）')
        else:
            fail(f'V4 材料块文字里没有类别标签，末尾是「{geo["mat"]["last"][:60]}」'
                 f'—— 帧可能没被前端解析（两层 JSON 编码路径没走到）')

        # V8 前提自证：容器真的溢出。不溢出的话 V5 恒真 —— 空跑绿比红更坏。
        overflow = geo['box']['scrollHeight'] - geo['box']['clientHeight']
        if overflow >= 20:
            ok(f'V8 前提成立：容器真的溢出（内容 {geo["box"]["scrollHeight"]}px / '
               f'可视 {geo["box"]["clientHeight"]}px，可滚 {overflow}px）')
        else:
            fail(f'V8 前提不成立：容器没溢出（可滚 {overflow}px < 20px）—— '
                 f'此时「材料在视野内」恒真，这把尺子失去意义。'
                 f'要么提示词太短，要么布局改成了不滚动的容器，先修前提再谈结论')

        # V5 主体：材料块整体落在可视区内
        inside = (geo['mat']['top'] >= geo['box']['top'] - 2 and
                  geo['mat']['bottom'] <= geo['box']['bottom'] + 2)
        if inside:
            ok(f'V5 【主体】材料块整体在实况区可视范围内'
               f'（块 {geo["mat"]["top"]}~{geo["mat"]["bottom"]}，'
               f'可视 {geo["box"]["top"]}~{geo["box"]["bottom"]}）')
        else:
            below = geo['mat']['bottom'] - geo['box']['bottom']
            fail(f'V5 【主体】材料块**不在**可视范围内：块 {geo["mat"]["top"]}~'
                 f'{geo["mat"]["bottom"]}，可视 {geo["box"]["top"]}~{geo["box"]["bottom"]}'
                 f'（下缘超出底线 {below}px；日志框只露出 {geo["log"]["visiblePx"]}px）'
                 f' —— 材料在流、计时器在跳，但用户看不见，就是这样')

        # V6 内层日志贴底
        l = geo['log']
        if l['scrollHeight'] <= l['clientHeight'] or \
                l['scrollTop'] + l['clientHeight'] >= l['scrollHeight'] - 2:
            ok(f'V6 内层日志贴底（scrollTop {l["scrollTop"]} + 可视 {l["clientHeight"]} '
               f'≥ 内容 {l["scrollHeight"]}）')
        else:
            fail(f'V6 内层日志没贴底：{l["scrollTop"]}+{l["clientHeight"]} < {l["scrollHeight"]}'
                 f'，新材料会落在日志框下沿之外')

        # V9 稳定复测：1.5s 后再量一次，防「贴底瞬间路过」。
        # 条件是 V5 先成立 —— V5 已红时「1.5s 后还在不在」没有意义（两点必然都不在），
        # 再报一条红只会让人以为有两处毛病。这里显式打 N/A 并写清理由（不是静默跳过）。
        # 注意：**不能**用 `SKIP` 开头 —— scripts/acceptance-live.sh 用 `^SKIP` 判「这条腿
        # 没被验证」，本腿若在绿的情况下打一行 SKIP，整条腿会被判 FAIL（尺子把自己坑了）。
        if not inside:
            print('N/A  V9 不评：V5 已红（几何不成立时谈「1.5s 后仍在视野内」没有意义）'
                  '—— 不计入通过数，理由同 V5')
        else:
            time.sleep(1.5)
            geo2 = page.evaluate(GEO_JS)
            if geo2['mat'] and geo2['mat']['bottom'] <= geo2['box']['bottom'] + 2 \
                    and geo2['mat']['top'] >= geo2['box']['top'] - 2:
                ok(f'V9 1.5s 后复测仍在视野内（块基线 {geo2["mat"]["bottom"]} ≤ '
                   f'容器底线 {geo2["box"]["bottom"]}）')
            else:
                fail(f'V9 1.5s 后材料块掉出视野了：{geo2["mat"]} vs 容器 {geo2["box"]}'
                     f' —— 只在瞬间贴过一次，用户看到的还是空白')

        # V7 无 JS 异常
        if not errs:
            ok('V7 全程无 JS 异常')
        else:
            fail(f'V7 出现 JS 异常（{len(errs)} 条）：{errs[:2]}')

        try:
            os.makedirs(os.path.dirname(SHOT), exist_ok=True)
            page.screenshot(path=SHOT, full_page=False)
            print(f'截图 → {SHOT}')
        except Exception as e:  # noqa: BLE001
            print(f'（截图失败：{e}）')

        print('\n--- 证据 ---')
        print(f'容器：可视 {geo["box"]["top"]}~{geo["box"]["bottom"]}'
              f'（高 {geo["box"]["h"]}，内容 {geo["box"]["scrollHeight"]}，'
              f'scrollTop {geo["box"]["scrollTop"]}）')
        print(f'日志：{geo["log"]["top"]}~{geo["log"]["bottom"]}'
              f'（露出 {geo["log"]["visiblePx"]}px）')
        print(f'材料：{geo["mat"]["top"]}~{geo["mat"]["bottom"]}'
              f'（高 {geo["mat"]["h"]}）')
        browser.close()

    total = ok_cnt + fail_cnt
    if total == 0:
        print('\n--- 0/0 ok ---')
        print('FAILED: 一条断言都没跑（绿得没有断言，不算通过）')
        return 1
    print(f'\n--- {ok_cnt}/{total} ok ---')
    if fail_cnt:
        print('FAILED: 训练页中间材料可见性（真浏览器）')
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
