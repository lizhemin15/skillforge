# MCP 门控终验取证（2026-09-19）

这条腿盯的是用户报的那个 bug：**管理员开启了 MCP 之后，聊天界面「勾了 MCP 数据源，实际却没调度」**。
它同时守住同一枚硬币的反面：**不勾 = 一条 MCP 都不许挂上**（MCP 挂在活的内网业务系统上，多给是安全问题）。

## 为什么必须真浏览器 + 真模型

`web/tests/chat_mcp_mutation_check.py` 是**静态**自证（抠出货 JS + 假 DOM），它证明不了「勾选集真的上了网线」。
那一段只有真页面 + 真服务 + 真模型量得到 —— 就是 `web/tests/chat_mcp_gate_e2e.py`。

## 结果（2026-09-19 22:30~22:50，本机 127.0.0.1:8092）

| leg | 命令 | 结果 |
| --- | --- | --- |
| 正跑 | `BASE=http://127.0.0.1:8092 REQUIRE_LIVE=1 python3 web/tests/chat_mcp_gate_e2e.py` | `--- 13/13 ok ---` rc=0 |
| 负向自证（真浏览器注入「勾了也不带上」） | 同上 + `INJECT_MCP_OFF=1` | `--- 11/14 ok ---` rc=0，打印 `✓ 负向自证成立` |
| 名册接线（真 runner，只跑这条腿） | `ONLY=mcp_gate bash scripts/acceptance-live.sh` | `PASS chat_mcp_gate_e2e.py :: mcp_gate 断言 13/13 161s`；`全部真实通过：1/1（未验证 0 条）`；RUNNER_RC=0 |
| 守卫自身双向自证 | `guard-require-live.log` | 修复态 rc=1 / 注入摘守卫 rc=0 / 名册语义 rc=0 / 还原=True |

真值源（不经本仓库的 Go 客户端，直连 MCP 端点）：远端工具 **25** 个（本地前缀 `mcp_datatoolbox_`）、
真接口标识 **59** 条。B2/B4 就是拿这个当靶子，避免「靶子没了判成门控坏了」的假红。

## 负向自证的语义（★ 别记反）

注入「勾了也不带上」之后：**B1 必须精确转红**，级联只允许 `B1/B2/B4`（都依赖 MCP 真被调度），
`A1/A2` 必须仍然绿。这时脚本打 `✓ 负向自证成立` 且 **rc=0** —— 负向 leg 成立 = 尺子会响 = 好事。

rc=1 只在三种「尺子坏了」的情况下出现：B1 竟然还绿（假绿）、红到了无关断言上（`A1/A2`，脏证据）、
红名单里有预期之外的断言（注入打到不相干的地方）。**所以「负向 leg 要 rc=1」是过期说法。**

## 两种 SKIP 语义（本轮踩过的坑）

脚本里有 `env_skip()` 一处定义、三处调用（playwright 没装 / 打不开页面 / 刷新后页面没起来）：

- **默认**：打 `SKIP` + rc=0。这是给名册 runner 的语义 —— runner 把 `^SKIP` 判成「未验证」，
  不会算 PASS；`SKIP≠PASS`。
- **`REQUIRE_LIVE=1`**：改成打 `FAIL` + rc=1。**本地闸门（`scripts/preflight.sh`）必须带这个开关**，
  因为 `selfcheck` 只认 rc —— 同一份 SKIP 在它那里会被印成 ✓，那就成了一把**没验过却挂着绿的尺子**
  （2026-09-19 实测踩到，见 `guard-require-live.log`）。
- 「前提式 SKIP」**已废弃**：库里没有启用中的 MCP、真值源连不上、库里明明有启用 MCP 而界面没渲染出按钮
  —— 这三种都改成**显式断言打红**（`A0 前提…` / `B2/B4 前提…`），不再吞成 SKIP。
  打成 SKIP 会被 `ALLOW_SKIP=1` 一路糊掉，尺子就烂了。

## 复现

```bash
cd /root/skillforge
# 正跑（约 160s，两次真模型调用）
BASE=http://127.0.0.1:8092 REQUIRE_LIVE=1 python3 web/tests/chat_mcp_gate_e2e.py
# 负向自证（期望 rc=0 + 「✓ 负向自证成立」）
BASE=http://127.0.0.1:8092 REQUIRE_LIVE=1 INJECT_MCP_OFF=1 python3 web/tests/chat_mcp_gate_e2e.py
# 名册接线
ONLY=mcp_gate bash scripts/acceptance-live.sh
```

前提：本机 8092 有活服务、`/opt/skillforge/data/skillforge.db` 里至少一台 `enabled=1` 的 MCP 服务器、
装了 playwright。（注：**公网域名在这台机器上解不出 DNS**，用回环地址。）
