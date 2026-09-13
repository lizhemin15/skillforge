# 手册素材测试集

这批文件的用途：给「手册 → 写作 skill」流水线提供可复现的测试料，
并记录素材本身是怎么造出来的（真手册是扫描件，不入库）。

## 不入库的（.gitignore 挡住）

| 文件 | 体积 | 说明 |
| --- | --- | --- |
| `manual-vector.pdf` | 17 MB | **事故元凶**：带文本层的 vector PDF。头部和尾部是纯 ASCII，中段是 deflate 压缩流 |
| `manual-scan.pdf` | 36 MB | 真·扫描件（纯图片） |
| `manual-scan-lite.pdf` | 14 MB | 抽稀页码的扫描件，跑流水线省时间用 |
| `.scan-pages/` | ~50 MB | 扫描页导出的 jpg，中间产物 |

**为什么 `manual-vector.pdf` 是关键素材**：它曾经被 `looksBinary` 判成「文本」，
于是跳过 OCR，17 MB 原始 PDF 字节直接灌进 LLM，provider 报
「26 万 token 超限」而抽取静默降级。旧判据只统计**前 8192 字节**的控制字符——
而这个文件前 8192 字节的控制字符占比是 **0.0000%**。回归防线见
`internal/skillgen/binary_sniff_test.go`，其中合成样本在任何机器上都能复现这个结构。

## 入库的

| 文件 | 说明 |
| --- | --- |
| `manual-ocr.txt` | 扫描件跑 OCR 后的**中文正文**（156 KB）。`looksBinary` 的反向断言用它：含零星 NUL，但必须仍判为文本 |
| `build_manual.py` | 用 ReportLab 造三份测试 PDF（vector / scan / scan-lite） |
| `verify_ocr.py` | 校验 OCR 产出与 PDF 页面的数字保真（合同金额、条款编号） |
| `normalize_facts.py` | 把 OCR 文本里的数字规范化，便于比对 |
| `build-report.json` | 造料结果记录（页数、字符数、OCR 耗时） |
| `part1~3.md` / `front.md` / `tail.md` | 造 PDF 时用的分章正文源 |
| `skillforge-ocr.service` | OCR 跑批用的 systemd unit（长任务不占 SSH 会话） |

## 重新造料

```bash
cd testdata/manual
python3 build_manual.py          # 生成三份 PDF
python3 verify_ocr.py            # 校验数字保真
```

真手册 PDF 拿回来后，放到本目录即可自动被 `TestLookBinaryRealFiles` 拾取
（文件不存在时该用例跳过，不会让 CI 假红）。
