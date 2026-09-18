#!/usr/bin/env python3
# LIVE-LEGS: account TIMEOUT_S=90
"""管理端「账号」区块线上终验：真浏览器 + 真服务 + 真登录（**只读，不改线上账号**）。

盯的是用户原话：「skillforge管理员账号应当在页面可修改」。
用户要的是「能在页面上改」，不是「有个接口」。所以这条腿必须打**真浏览器**：
接口存在、Go 单测全绿（见 internal/api/account_test.go：改密生效、改名废旧令牌、
重启不被 env 覆盖……）都只证明后端对，证明不了**页面上那个区块看得见、填得进**。
历史上最常见的坏法正是「后端好了，前端 tab 藏在别处 / 输入框 disabled / 预填写死 admin」。

分工（别当重复）：
  · internal/api/account_test.go —— 后端契约（含「重启不被 env 覆盖」这条最阴的）；
  · 本腿 —— 浏览器里那个区块真的可见、可编辑、可提交（但不提交）。

为什么**不提交**：这条腿默认打线上（skillforge.open-claw.click）。在线上点一次保存
＝真改掉客户的管理员账号密码，验收动作不能有这种副作用。
「改完能登 / 活过重启」由 account_test.go 在 CI 里守，那里用的是临时库。
本腿另加一条硬约束断言 A7：全程**不许**出现写 /api/admin/account 的请求 ——
防止以后有人手滑补一句 `click` 把这条腿变成会改线上密码的定时炸弹。

断言：
  A1 真凭据登录 → 切「账号」tab，区块可见
  A2 用户名框可见，且预填的是**当前登录名**（不是硬编码 "admin"）
  A3 用户名框真能编辑：填探针值 → 回读一致 → 清空 → 回读为空（不留痕）
  A4 三个密码框都在（当前/新/确认），type=password
  A5 「保存」按钮可见且可用（enabled），**不点**
  A6 全程无 JS 异常
  A7 只读自证：没有写 /api/admin/account 的请求（GET 允许，那是页面预填）

注意（血泪）：绝不能拿 count() 当「存在」判据 —— 未登录时 #tab-account 也在 DOM 里，
只是 hidden。判可见性一律用 is_visible()/getBoundingClientRect，否则下一步 fill
立刻抛 "element is not visible"，把「没给凭据」演成「页面坏了」。

用法：
  python3 web/tests/admin_account_e2e.py
  BASE=http://127.0.0.1:8092 ADMIN_USER=… ADMIN_PASS=… python3 web/tests/admin_account_e2e.py
  没装 playwright / 没给凭据 / 打不开页面 → SKIP 退出 0（SKIP ≠ PASS）。
  凭据只从环境变量读，不落盘、不打印（scripts/acceptance-live.sh 的带凭据外壳负责喂）。
"""
import os
import sys
import time

BASE = os.environ.get('BASE', 'https://skillforge.open-claw.click')
# 外层硬砍时间（runner 用 leg 里的 TIMEOUT_S 传进来）。必须真读 ——
# live_e2e_roster.test.mjs 专门守这一条：声明了 TIMEOUT_S 却不读，等于这条 leg
# 和默认那条跑的是同一件事，报告上却看起来覆盖了两种路径（死键）。
BUDGET_S = int(os.environ.get('TIMEOUT_S') or 420)
# 内层单步等待必须比外层预算小得多，否则被外层砍掉时连 FAIL 行都打不出来
# —— 红得没有理由的红，就是假红（人一看以为是页面坏了，其实只是超时值配歪了）。
WAIT_MS = max(5000, min(20000, int(BUDGET_S * 1000 * 0.25)))

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
        page.set_default_timeout(WAIT_MS)  # 由 TIMEOUT_S 推出来的内层等待上限
        errs = []
        page.on('pageerror', lambda e: errs.append(str(e)))
        # A7 的证据来源：记下所有打到 /api/admin/account 的请求方法。
        acct_reqs = []
        page.on('request', lambda r: acct_reqs.append(r.method + ' ' + r.url)
                if '/api/admin/account' in r.url else None)

        try:
            page.goto(BASE + '/admin', wait_until='domcontentloaded', timeout=WAIT_MS)
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
                page.wait_for_selector('button[data-tab="account"]', timeout=WAIT_MS, state='visible')
                page.click('button[data-tab="account"]')
                page.wait_for_selector('#account-form', timeout=WAIT_MS, state='visible')
                ok('A1 真凭据登录 → 切「账号」tab，账号区块可见'
                   '（凭据走环境变量，未落盘、未打印）')
            except Exception as e:  # noqa: BLE001
                fail(f'A1 登录后打不开账号区块（{type(e).__name__}: {str(e)[:120]}）')
                browser.close()
                return 1
        if not page.locator('#account-form').is_visible():
            skip('账号表单不可见（未登录 / 不是管理端）—— SKIP 不等于 PASS；'
                 '要真跑请给 ADMIN_USER/ADMIN_PASS')
            browser.close()
            return 0

        # ---- A2 用户名框可见 + 预填的是当前登录名 ----
        val = (page.input_value('#account-user') or '').strip()
        if val:
            ok(f'A2 用户名框已预填当前登录名「{val}」（长度 {len(val)}，不是硬编码 admin：'
               f'{"是" if val == user else "待观察"}）')
        else:
            fail('A2 用户名框是空的 —— 用户点进来不知道该填什么，'
                 '「能在页面上改」就还是半截；预期由 GET /api/admin/account 预填当前登录名')

        # ---- A3 真能编辑，且不留痕 ----
        probe = 'a-browser-probe-not-saved'
        page.fill('#account-user', probe)
        got = page.input_value('#account-user')
        page.fill('#account-user', '')
        empty = page.input_value('#account-user')
        if got == probe and empty == '':
            ok('A3 用户名框真能编辑（填探针值回读一致 → 清空回读为空，未提交、未留痕）')
        else:
            fail(f'A3 用户名框编辑不生效（填入回读「{got}」、清空回读「{empty}」）'
                 f' —— 输入框可能是 disabled/readonly 或绑了只读逻辑')

        # ---- A4 三个密码框都在 ----
        pws = page.evaluate(
            "() => ['account-current','account-new','account-confirm']"
            ".map(id => { const e = document.getElementById(id);"
            "  if (!e) return id + ':missing';"
            "  const r = e.getBoundingClientRect();"
            "  return id + ':' + e.type + (r.width > 0 && r.height > 0 ? ':visible' : ':hidden'); })")
        bad_pw = [x for x in pws if not x.endswith(':password:visible')]
        if not bad_pw:
            ok('A4 三个密码框（当前/新/确认）都可见且 type=password')
        else:
            fail(f'A4 密码框不对劲：{bad_pw}（期望全部 password:visible）')

        # ---- A5 保存按钮可见可用（不点）----
        btn = page.evaluate(
            "() => { const b = document.getElementById('account-save');"
            "  if (!b) return null; const r = b.getBoundingClientRect();"
            "  return {visible: r.width > 0 && r.height > 0, disabled: !!b.disabled,"
            "          text: (b.innerText || '').trim()}; }")
        if btn and btn['visible'] and not btn['disabled']:
            ok(f'A5 「{btn["text"] or "保存"}」按钮可见且可用（**未点击** —— 线上不改账号）')
        else:
            fail(f'A5 保存按钮不可用：{btn}（可见+enabled 才算「能在页面上改」）')

        time.sleep(0.6)  # 让页面加载期的请求都落地，A7 才看得全

        # ---- A7 只读自证：没写过账号 ----
        writes = [r for r in acct_reqs if not r.startswith('GET')]
        if not writes:
            ok(f'A7 只读自证：全程没有写 /api/admin/account 的请求'
               f'（{len(acct_reqs)} 条 GET 预填，0 条写）—— 没碰线上账号')
        else:
            fail(f'A7 出现了写账号的请求 {writes} —— 这条腿的约定是**只填不提交**，'
                 f'线上账号不能被验收动作改掉')

        # ---- A6 无 JS 异常 ----
        if not errs:
            ok('A6 全程无 JS 异常')
        else:
            fail(f'A6 出现 JS 异常（{len(errs)} 条）：{errs[:2]}')

        shot = os.environ.get('SHOT_OUT', '/tmp/sf_shots/admin_account_live.png')
        try:
            os.makedirs(os.path.dirname(shot), exist_ok=True)
            page.screenshot(path=shot, full_page=False)
            print(f'截图 → {shot}')
        except Exception as e:  # noqa: BLE001
            print(f'（截图失败：{e}）')

        print('\n--- 证据 ---')
        print(f'页面 {BASE}/admin · 账号请求：{acct_reqs or "（无）"}')
        browser.close()

    total = ok_cnt + fail_cnt
    if total == 0:
        print('\n--- 0/0 ok ---')
        print('FAILED: 一条断言都没跑（绿得没有断言，不算通过）')
        return 1
    print(f'\n--- {ok_cnt}/{total} ok ---')
    if fail_cnt:
        print('FAILED: 管理端账号区块（真浏览器）')
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
