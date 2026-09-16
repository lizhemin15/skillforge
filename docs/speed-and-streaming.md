# 训练/聊天提速 + 中间材料流式：before / after 实测对照

用户诉求原文（2026-09-17）：

> 现在速度过于慢了，中间可以流式输出思考的一些中间材料，现在一直卡着计时，用户体验不佳

这份文档只记**实测数字**与**复现方式**。数字不是估的，是同一份解析逻辑对两轮真训练帧日志算出来的。

## 0. 为什么 before/after 必须共用一份解析逻辑

如果 before 的账是当时手算的、after 的账是另写一段脚本算的，两份数字就**不可比** ——
差别分不清是「真的变快了」还是「尺子换了」。

所以解析逻辑单独抽成 `scripts/stage_timing_of_frames.py`，两组数字都由它产出：

```
scripts/stage_timing_of_frames.py <frames.log> --json out.json --label before|after
```

帧日志来自 `web/tests/judge_stage_streaming_capture.py`（真浏览器 + 真服务 + 真模型跑一轮训练，
逐帧记到达时刻）。JSON 原件见 `docs/evidence/speed/{before,after}_stages.json`。

## 1. 总耗时

| | 版本 | 总耗时 | 帧数 |
|---|---|---|---|
| before | 01:10（全阶段思考链开着） | **860.6s** | 1566 |
| after | `v260917.0159`（结构化/机械阶段关思考链） | **549.4s** | 1143 |

**−36%**（−311.2s）。两轮都是同一份素材、同一台机、同一个模型。

## 2. 省在哪一段（这才是「该省的地方真省了」的证据）

| 阶段 | before | after | 说明 |
|---|---|---|---|
| 1/9 分析参考文件，提取写作特征 | 94.8s（think 170 帧） | **12.2s**（think 0） | 结构化摘录，产出不给人读 → 关思考链 |
| 2/9 生成技能元数据与表单参数 | 77.9s（think 131） | **8.8s**（think 0） | 同上 |
| 3/9 识别技能类型 | 65.1s（think 86） | **2.4s**（think 0） | 只回一个枚举值 |
| 4/9 撰写系统提示词 | 117.6s（think 202） | 111.3s（think 164） | **保留思考链**：产出是给模型读的成品指令 |
| 6/9 生成骨架模板 | 74.7s（think 147） | **26.7s**（think 0） | 机械套模板 |
| 8.5/9 裁判独立试用评分 | —（该轮未走到） | 54.9s（think 74） | 保留思考链：要给打分理由 |

关掉思考链的阶段，`think_frames` 一律归零；**产出成品文字的两段（4/9 提示词、8.5/9 裁判）思考帧照旧**——
这是刻意留的，不是漏了。

## 3. 「一直卡着计时」= 阶段内部静默，不是总耗时

总耗时再短，只要阶段内部长时间**屏幕上一个新字都没有**，体感就是卡死。
所以真正的判据是每阶段内的 `max_silence`（最长无新东西间隔），以及线上实况腿：

| 证据 | 现场 |
|---|---|
| `admin_train_progress_e2e.py`（线上真跑一轮训练，T5 腿） | 整轮 **213.7s**、产出中间材料 **433 帧**、屏幕最长静默 **1.3s** |
| 本文档第 2 节各阶段 `max_silence` 列 | after 侧最大 6.9s，且那 6.9s 出在**仍需思考链**的裁判段 |

结论：线上不复现「只剩一个计时器在转」——每个阶段内部都有中间材料在滚。

## 4. 复现方式

```bash
# 1) 采一轮真训练的帧日志（要凭据，凭据只从服务自己的 env 文件取，不落盘）
set -a; . /opt/skillforge/skillforge.env; set +a
export ADMIN_USER="$SKILLFORGE_ADMIN_USER" ADMIN_PASS="$SKILLFORGE_ADMIN_PASS"
python3 web/tests/judge_stage_streaming_capture.py

# 2) 同一把尺子算账
python3 scripts/stage_timing_of_frames.py /tmp/after_frames.log --json /tmp/after_stages.json --label after

# 3) 线上实况腿（含「最长静默」断言）
bash scripts/acceptance-live.sh        # ONLY=train_progress 可只跑这条
```

## 5. 提速线的尺子（防「以后又悄悄变慢/又开始白等」）

| 尺子 | 钉住什么 |
|---|---|
| `internal/agent/progress_wiring_test.go` | 结构化阶段那一跳**真的**带 `enable_thinking=false` 出门（不是只在中间层置位） |
| `scripts/thinking_knob_inject.py` | 负向自证：把开关接线摘掉 → 必须转红 |
| `scripts/stage_timing_of_frames.py` | before/after 用同一份解析，阶段耗时不可各算各的 |
| `web/tests/admin_train_progress_e2e.py` | 线上真跑：最少帧数 + **最长静默上限**（卡顿回归防线） |
| `web/tests/judge_stats_three_state_check.py` + `..._mutation_check.py` | 阶段缺席时「按设计不适用」与「真漏了」不许混为一谈（松/紧双向注入自证） |
